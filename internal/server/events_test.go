package server

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mariuz/firebirdrest/internal/config"
)

// sseStream reads one event stream line by line.
type sseStream struct {
	resp  *http.Response
	lines chan string
}

func openStream(t *testing.T, url, auth string) (*sseStream, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
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
