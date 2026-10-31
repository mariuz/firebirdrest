package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nakagami/firebirdsql"

	"github.com/mariuz/firebirdrest/internal/query"
)

// Realtime: GET /events/<name>[,<name>...] streams Firebird events to the
// client as Server-Sent Events. A trigger or procedure posts an event with
// POST_EVENT 'order_placed'; Firebird delivers it when the transaction
// commits, coalescing the posts of a transaction into one notification with
// a count. Events carry no data and Firebird checks no privilege for them,
// so the server exposes only the names listed in db-events, to the roles in
// db-events-roles (any authenticated role when empty); a client that hears
// an event fetches what changed through the API, under its own role.
//
// One attachment listens for all exposed events (eventHub) and fans them out
// to the streams in process, so a thousand clients cost the database one
// connection. A stream that cannot keep up loses notifications, never
// blocks the hub; Firebird's own delivery is already a count, not a queue.

const (
	eventKeepalive = 15 * time.Second
	eventRetry     = 30 * time.Second // between attempts to reach the database
	eventBuffer    = 64               // notifications a stream may have unread
)

// eventReplay is how many recent notifications the hub keeps for clients
// that reconnect with Last-Event-ID (a variable for tests).
var eventReplay = 1000

// Every notification gets an id, "<epoch>-<seq>": epoch identifies this
// server process (it changes on restart and differs between instances) and
// seq counts the notifications it received. A client that reconnects with
// the last id it saw gets the notifications it missed from the replay
// buffer. When that can't be done (another epoch, an id older than the
// buffer, or a gap while the database connection was lost) the stream
// starts with "event: reset": the client must re-read what it shows,
// because notifications may have been missed. Nothing is missed silently.

// notification is one received event, or a reset for a live stream.
type notification struct {
	seq   uint64
	ev    firebirdsql.Event
	reset string // why the stream may have missed notifications
}

type eventHub struct {
	s     *Server
	names []string
	epoch string

	mu   sync.Mutex
	subs map[*eventSub]struct{}
	// listening is true while the attachment is subscribed; streams opened
	// while it isn't are told so, and reconnect
	listening bool
	seq       uint64         // the last notification's number
	recent    []notification // the last eventReplay notifications, oldest first
	done      chan struct{}
	stopped   chan struct{}
}

type eventSub struct {
	names map[string]bool
	ch    chan notification
	lost  atomic.Int64 // notifications dropped because ch was full
}

func (s *Server) startEvents() {
	if len(s.cfg.DBEvents) == 0 {
		return
	}
	h := &eventHub{s: s, names: s.cfg.DBEvents, subs: map[*eventSub]struct{}{},
		epoch: strconv.FormatInt(time.Now().UnixNano(), 36),
		done:  make(chan struct{}), stopped: make(chan struct{})}
	s.events = h
	go h.run()
}

// run keeps one subscription to every exposed event, resubscribing after the
// connection to the database is lost.
func (h *eventHub) run() {
	defer close(h.stopped)
	lostBefore := false
	for {
		ev, err := firebirdsql.NewFBEvent(h.s.cfg.DBURI)
		var sub *firebirdsql.Subscription
		if err == nil {
			sub, err = ev.Subscribe(h.names, h.broadcast)
		}
		if err != nil {
			if ev != nil {
				ev.Close()
			}
			h.s.log.Warn("cannot listen for events, retrying", "err", err, "in", eventRetry)
			select {
			case <-h.done:
				return
			case <-time.After(eventRetry):
				continue
			}
		}
		h.s.log.Info("listening for events", "events", strings.Join(h.names, ","))
		closed := make(chan error, 1)
		sub.NotifyClose(closed)
		if lostBefore {
			// events posted while nobody listened are gone: tell the open
			// streams, and make older ids unreplayable
			h.gap("the server lost its connection to the database; notifications may have been missed")
		}
		h.setListening(true)
		select {
		case <-h.done:
			h.setListening(false)
			ev.Close()
			return
		case err := <-closed:
			h.setListening(false)
			ev.Close()
			h.s.log.Warn("event subscription lost, resubscribing", "err", err)
			h.s.m.eventErrors.Add(1)
			lostBefore = true
		}
	}
}

func (h *eventHub) setListening(on bool) {
	h.mu.Lock()
	h.listening = on
	h.mu.Unlock()
}

// broadcast numbers a notification, keeps it for replay and hands it to
// every stream listening for its name.
func (h *eventHub) broadcast(e firebirdsql.Event) {
	h.s.m.eventReceived(e.Name)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	n := notification{seq: h.seq, ev: e}
	h.recent = append(h.recent, n)
	if over := len(h.recent) - eventReplay; over > 0 {
		h.recent = append(h.recent[:0:0], h.recent[over:]...)
	}
	for sub := range h.subs {
		if sub.names[e.Name] {
			sub.send(n)
		}
	}
}

// gap empties the replay buffer and sends a reset to every open stream.
func (h *eventHub) gap(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recent = nil
	for sub := range h.subs {
		sub.send(notification{reset: reason})
	}
}

func (sub *eventSub) send(n notification) {
	select {
	case sub.ch <- n:
	default:
		sub.lost.Add(1)
	}
}

// subscribe registers a stream. With the client's Last-Event-ID it also
// returns the notifications to replay, or why they can't be (reset), taken
// under the same lock as the registration so nothing falls in between.
func (h *eventHub) subscribe(names []string, lastID string) (sub *eventSub, listening bool, replay []notification, reset string) {
	sub = &eventSub{names: map[string]bool{}, ch: make(chan notification, eventBuffer)}
	for _, n := range names {
		sub.names[n] = true
	}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	listening = h.listening
	if lastID != "" {
		replay, reset = h.since(lastID, sub.names)
	}
	h.mu.Unlock()
	h.s.m.eventStreams.Add(1)
	if reset != "" {
		h.s.m.eventResets.Add(1)
	}
	return sub, listening, replay, reset
}

// since returns the kept notifications after lastID for names, or why
// they can't be known. Called with h.mu held.
func (h *eventHub) since(lastID string, names map[string]bool) ([]notification, string) {
	epoch, seqText, ok := strings.Cut(lastID, "-")
	last, err := strconv.ParseUint(seqText, 10, 64)
	switch {
	case !ok || err != nil:
		return nil, "unknown Last-Event-ID"
	case epoch != h.epoch:
		return nil, "the server restarted, or this is another instance; notifications may have been missed"
	case last > h.seq:
		return nil, "unknown Last-Event-ID"
	case last == h.seq:
		return nil, ""
	}
	if len(h.recent) == 0 || h.recent[0].seq > last+1 {
		return nil, "more notifications were posted than the server keeps for replay; some may have been missed"
	}
	var out []notification
	for _, n := range h.recent {
		if n.seq > last && names[n.ev.Name] {
			out = append(out, n)
		}
	}
	return out, ""
}

func (h *eventHub) id(n notification) string {
	return h.epoch + "-" + strconv.FormatUint(n.seq, 10)
}

func (h *eventHub) unsubscribe(sub *eventSub) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
	h.s.m.eventStreams.Add(-1)
}

func (h *eventHub) close() {
	close(h.done)
	<-h.stopped
}

// eventName finds an exposed event by name (as given, or Firebird's upper
// case form of an unquoted name) and returns it as configured.
func (h *eventHub) eventName(name string) string {
	for _, n := range h.names {
		if n == name || strings.EqualFold(n, name) {
			return n
		}
	}
	return ""
}

// streamEvents serves GET /events/<names>.
func (s *Server) streamEvents(rc *reqCtx, spec string) error {
	if s.events == nil {
		return &query.Error{Status: 404, Code: "FBRST135", Message: "events are not enabled",
			Hint: "list the events to expose in db-events"}
	}
	if rc.r.Method != http.MethodGet {
		return &query.Error{Status: 405, Code: "FBRST117", Message: "events are streamed with GET"}
	}
	var names []string
	for _, n := range strings.Split(spec, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if ev := s.events.eventName(n); ev == "" {
			return &query.Error{Status: 404, Code: "FBRST136",
				Message: fmt.Sprintf("event %q is not exposed", n),
				Hint:    "exposed events: " + strings.Join(s.events.names, ", ")}
		} else {
			names = append(names, ev)
		}
	}
	if len(names) == 0 {
		return &query.Error{Status: 404, Code: "FBRST136", Message: "no event named",
			Hint: "GET /events/<name>[,<name>...]; exposed events: " + strings.Join(s.events.names, ", ")}
	}
	// the role's permission: it must be grantable to the database user (as
	// for any request) and, when db-events-roles is set, listed there
	if _, err := s.pool(rc.r.Context(), rc.role); err != nil {
		return err
	}
	if len(s.cfg.DBEventsRoles) > 0 {
		allowed := false
		for _, r := range s.cfg.DBEventsRoles {
			if strings.EqualFold(r, rc.role) {
				allowed = true
			}
		}
		if !allowed {
			status := 403
			if rc.anon {
				status = 401 // as for any permission error of an anonymous request
			}
			return &query.Error{Status: status, Code: "FBRST137",
				Message: fmt.Sprintf("role %q may not listen for events", rc.role),
				Hint:    "roles allowed by db-events-roles: " + strings.Join(s.cfg.DBEventsRoles, ", ")}
		}
	}

	sub, listening, replay, reset := s.events.subscribe(names, rc.r.Header.Get("Last-Event-ID"))
	defer s.events.unsubscribe(sub)
	w := rc.w
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	ctrl := http.NewResponseController(w)
	ctrl.SetWriteDeadline(time.Time{}) // the stream outlives the server's write timeout
	w.WriteHeader(http.StatusOK)
	write := func(msg string) bool {
		if _, err := fmt.Fprint(w, msg); err != nil {
			return false
		}
		return ctrl.Flush() == nil
	}
	resetMsg := func(reason string) string {
		return "event: reset\ndata: {\"reason\":" + string(query.AppendJSON(nil, reason)) + "}\n\n"
	}
	eventMsg := func(n notification) string {
		var sb strings.Builder
		fmt.Fprintf(&sb, "id: %s\nevent: %s\ndata: {\"name\":%s,\"count\":%d}\n\n",
			s.events.id(n), n.ev.Name, query.AppendJSON(nil, n.ev.Name), n.ev.Count)
		return sb.String()
	}
	// the first bytes tell the client the stream is up; a hub that is not
	// listening says so, and the client reconnects later
	if !listening {
		write("retry: " + fmt.Sprint(eventRetry.Milliseconds()) + "\nevent: error\ndata: {\"message\":\"not listening: the database is unreachable\"}\n\n")
		return nil
	}
	head := "retry: 3000\n: listening " + strings.Join(names, ",") + "\n\n"
	if reset != "" {
		head += resetMsg(reset)
	}
	for _, n := range replay {
		head += eventMsg(n)
	}
	s.m.eventDelivered.Add(uint64(len(replay)))
	if !write(head) {
		return nil
	}
	ctx := rc.r.Context()
	keep := time.NewTicker(eventKeepalive)
	defer keep.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.events.done:
			write("event: error\ndata: {\"message\":\"server shutting down\"}\n\n")
			return nil
		case <-keep.C:
			if !write(": keepalive\n\n") {
				return nil
			}
		case n := <-sub.ch:
			var msg string
			if n.reset != "" {
				s.m.eventResets.Add(1)
				msg = resetMsg(n.reset)
			} else {
				s.m.eventDelivered.Add(1)
				msg = eventMsg(n)
			}
			if lost := sub.lost.Swap(0); lost > 0 {
				// the stream fell behind: what was dropped is gone for it
				s.m.eventResets.Add(1)
				msg += resetMsg(fmt.Sprintf("%d notifications were dropped because the stream was not read fast enough", lost))
			}
			if !write(msg) {
				return nil
			}
		}
	}
}

// isEventStream reports whether the request opens an event stream, which
// is not subject to the request timeout.
func isEventStream(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/events/")
}
