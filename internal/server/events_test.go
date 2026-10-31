package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/nakagami/firebirdsql"

	"github.com/mariuz/firebirdrest/internal/config"
)

// sseStream reads one event stream line by line.
type sseStream struct {
	resp  *http.Response
	lines chan string
}

func openStream(t *testing.T, url, auth string, headers ...string) (*sseStream, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	st := &sseStream{resp: resp, lines: make(chan string, 64)}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			st.lines <- sc.Text()
		}
		close(st.lines)
	}()
	return st, resp.StatusCode
}

// waitFor returns the first line with the prefix, or fails after 10 seconds.
func (st *sseStream) waitFor(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case l, ok := <-st.lines:
			if !ok {
				t.Fatalf("stream ended before %q", prefix)
			}
			if strings.HasPrefix(l, prefix) {
				return l
			}
		case <-deadline:
			t.Fatalf("no %q line within 10s", prefix)
		}
	}
}

func (st *sseStream) close() { st.resp.Body.Close() }

func TestEvents(t *testing.T) {
	plain := testServer(t)
	if r := do(t, plain, "GET", "/events/order_placed", ""); r.status != 404 || !strings.Contains(r.body, "FBRST135") {
		t.Errorf("events disabled: %d %s", r.status, r.body)
	}
	ts := testServerWith(t, func(c *config.Config) {
		c.DBEvents = []string{"order_placed", "stock_changed"}
		c.DBEventsRoles = []string{"web_user"}
	})
	s := serverOf(t, ts)
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})

	if r := do(t, ts, "GET", "/events/nope", "", "Authorization", user); r.status != 404 || !strings.Contains(r.body, "FBRST136") {
		t.Errorf("unknown event: %d %s", r.status, r.body)
	}
	if r := do(t, ts, "GET", "/events/order_placed", ""); r.status != 401 || !strings.Contains(r.body, "FBRST137") {
		t.Errorf("anonymous listener: %d %s", r.status, r.body)
	}
	if r := do(t, ts, "POST", "/events/order_placed", "", "Authorization", user); r.status != 405 {
		t.Errorf("POST: %d %s", r.status, r.body)
	}

	// the hub subscribes in the background: wait until a stream says it listens
	var st *sseStream
	for i := 0; ; i++ {
		var status int
		st, status = openStream(t, ts.URL+"/events/order_placed,STOCK_CHANGED", user)
		if status != 200 || !strings.HasPrefix(st.resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream: %d %v", status, st.resp.Header)
		}
		l := st.waitFor(t, "")
		if strings.HasPrefix(l, "retry: 3000") {
			st.waitFor(t, ": listening order_placed,stock_changed")
			break
		}
		st.close()
		if i == 20 {
			t.Fatal("the hub never started listening")
		}
		time.Sleep(500 * time.Millisecond)
	}
	defer st.close()
	if n := s.m.eventStreams.Load(); n != 1 {
		t.Errorf("open streams: %d", n)
	}

	// an event posted in a committed transaction arrives, with its name and count
	if r := do(t, ts, "POST", "/rpc/notify", `{"name":"stock_changed"}`, "Authorization", user); r.status != 204 {
		t.Fatalf("notify: %d %s", r.status, r.body)
	}
	st.waitFor(t, "id: ")
	if l := st.waitFor(t, "event: "); l != "event: stock_changed" {
		t.Errorf("event line %q", l)
	}
	if l := st.waitFor(t, "data: "); l != `data: {"name":"stock_changed","count":1}` {
		t.Errorf("data line %q", l)
	}
	// an event the stream didn't ask for is not delivered; a rolled-back post never fires
	do(t, ts, "POST", "/rpc/notify", `{"name":"order_placed"}`, "Authorization", user, "Prefer", "tx=rollback")
	do(t, ts, "POST", "/rpc/notify", `{"name":"order_placed"}`, "Authorization", user)
	if l := st.waitFor(t, "event: "); l != "event: order_placed" {
		t.Errorf("second event %q", l)
	}
	st.close()
	time.Sleep(200 * time.Millisecond)
	if n := s.m.eventStreams.Load(); n != 0 {
		t.Errorf("streams after close: %d", n)
	}
	if !strings.Contains(metricsOf(s), `firebirdrest_events_received_total{event="stock_changed"} 1`) {
		t.Errorf("metrics: %s", metricsOf(s))
	}
}

func metricsOf(s *Server) string {
	var sb strings.Builder
	s.writeMetrics(&sb)
	return sb.String()
}

// TestEventReplayRules checks which Last-Event-IDs can be replayed, on a hub
// fed by hand.
func TestEventReplayRules(t *testing.T) {
	old := eventReplay
	eventReplay = 4
	defer func() { eventReplay = old }()
	h := &eventHub{s: &Server{m: newMetrics()}, epoch: "e1", subs: map[*eventSub]struct{}{}}
	post := func(name string) {
		h.broadcast(firebirdsql.Event{Name: name, Count: 1})
	}
	both := map[string]bool{"a": true, "b": true}
	onlyA := map[string]bool{"a": true}
	post("a") // 1
	post("b") // 2
	post("a") // 3
	seqs := func(ns []notification) (out []uint64) {
		for _, n := range ns {
			out = append(out, n.seq)
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, reset := h.since("e1-1", both); reset != "" || fmt.Sprint(seqs(r)) != "[2 3]" {
		t.Errorf("after 1: %v %q", seqs(r), reset)
	}
	if r, reset := h.since("e1-1", onlyA); reset != "" || fmt.Sprint(seqs(r)) != "[3]" {
		t.Errorf("after 1, only a: %v %q", seqs(r), reset)
	}
	if r, reset := h.since("e1-3", both); reset != "" || len(r) != 0 {
		t.Errorf("up to date: %v %q", seqs(r), reset)
	}
	for id, want := range map[string]string{
		"e0-2": "restarted", "e1-9": "unknown", "garbage": "unknown", "e1-x": "unknown",
	} {
		if _, reset := h.since(id, both); !strings.Contains(reset, want) {
			t.Errorf("%s: reset %q, want %q", id, reset, want)
		}
	}
	h.mu.Unlock()
	post("a")
	post("a")
	post("b") // 4, 5, 6: the buffer keeps 3..6
	h.mu.Lock()
	if r, reset := h.since("e1-2", both); reset != "" || fmt.Sprint(seqs(r)) != "[3 4 5 6]" {
		t.Errorf("oldest kept: %v %q", seqs(r), reset)
	}
	if _, reset := h.since("e1-1", both); !strings.Contains(reset, "more notifications") {
		t.Errorf("beyond the buffer: %q", reset)
	}
	h.mu.Unlock()
	// a lost database connection empties the buffer: older ids reset, and
	// open streams get a reset message
	sub, _, _, _ := h.subscribe([]string{"a"}, "")
	h.gap("lost")
	if n := <-sub.ch; n.reset != "lost" {
		t.Errorf("open stream after a gap: %+v", n)
	}
	h.mu.Lock()
	if _, reset := h.since("e1-5", both); !strings.Contains(reset, "more notifications") {
		t.Errorf("after a gap: %q", reset)
	}
}

// TestEventResume reconnects a stream with Last-Event-ID against the
// database: missed notifications are replayed, and an unknown id resets.
func TestEventResume(t *testing.T) {
	ts := testServerWith(t, func(c *config.Config) { c.DBEvents = []string{"order_placed", "stock_changed"} })
	user := "Bearer " + token(t, jwt.MapClaims{"role": "web_user"})
	notify := func(name string) {
		t.Helper()
		if r := do(t, ts, "POST", "/rpc/notify", `{"name":"`+name+`"}`, "Authorization", user); r.status != 204 {
			t.Fatalf("notify: %d %s", r.status, r.body)
		}
	}
	// wait until the hub listens, as TestEvents does
	var st *sseStream
	for i := 0; ; i++ {
		st, _ = openStream(t, ts.URL+"/events/stock_changed,order_placed", user)
		if strings.HasPrefix(st.waitFor(t, ""), "retry: 3000") {
			break
		}
		st.close()
		if i == 20 {
			t.Fatal("the hub never started listening")
		}
		time.Sleep(500 * time.Millisecond)
	}
	notify("stock_changed")
	first := strings.TrimPrefix(st.waitFor(t, "id: "), "id: ")
	st.close()

	// missed while disconnected: three posts (Firebird may merge them into
	// fewer notifications, adding up their counts)
	notify("order_placed")
	notify("stock_changed")
	notify("order_placed")
	time.Sleep(500 * time.Millisecond)

	st2, _ := openStream(t, ts.URL+"/events/order_placed", user, "Last-Event-ID", first)
	defer st2.close()
	got := 0
	for got < 2 {
		l := st2.waitFor(t, "data: ")
		if !strings.Contains(l, `"order_placed"`) {
			t.Fatalf("replayed a notification the stream didn't ask for: %s", l)
		}
		var n struct{ Count int }
		json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &n)
		got += n.Count
	}
	// and live notifications follow the replay
	notify("order_placed")
	if l := st2.waitFor(t, "data: "); !strings.Contains(l, "order_placed") {
		t.Errorf("live after replay: %s", l)
	}

	// an id from another server process: reset first
	st3, _ := openStream(t, ts.URL+"/events/order_placed", user, "Last-Event-ID", "zzz-1")
	defer st3.close()
	if l := st3.waitFor(t, "event: "); l != "event: reset" {
		t.Errorf("unknown epoch: %s", l)
	}
	if l := st3.waitFor(t, "data: "); !strings.Contains(l, "restarted") {
		t.Errorf("reset reason: %s", l)
	}
}
