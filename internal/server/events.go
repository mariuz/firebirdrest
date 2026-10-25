package server

import (
	"fmt"
	"net/http"
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

type eventHub struct {
	s     *Server
	names []string

	mu   sync.Mutex
	subs map[*eventSub]struct{}
	// listening is true while the attachment is subscribed; streams opened
	// while it isn't are told so, and reconnect
	listening bool
	seq       atomic.Uint64
	done      chan struct{}
	stopped   chan struct{}
}

type eventSub struct {
	names map[string]bool
	ch    chan firebirdsql.Event
	lost  atomic.Int64 // notifications dropped because ch was full
}

func (s *Server) startEvents() {
	if len(s.cfg.DBEvents) == 0 {
		return
	}
	h := &eventHub{s: s, names: s.cfg.DBEvents, subs: map[*eventSub]struct{}{},
		done: make(chan struct{}), stopped: make(chan struct{})}
	s.events = h
	go h.run()
}

// run keeps one subscription to every exposed event, resubscribing after the
// connection to the database is lost.
func (h *eventHub) run() {
	defer close(h.stopped)
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
		}
	}
}

func (h *eventHub) setListening(on bool) {
	h.mu.Lock()
	h.listening = on
	h.mu.Unlock()
}

// broadcast hands a notification to every stream listening for its name.
func (h *eventHub) broadcast(e firebirdsql.Event) {
	h.s.m.eventReceived(e.Name)
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		if !sub.names[e.Name] {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			sub.lost.Add(1)
		}
	}
}

func (h *eventHub) subscribe(names []string) (*eventSub, bool) {
	sub := &eventSub{names: map[string]bool{}, ch: make(chan firebirdsql.Event, eventBuffer)}
	for _, n := range names {
		sub.names[n] = true
	}
	h.mu.Lock()
	h.subs[sub] = struct{}{}
	listening := h.listening
	h.mu.Unlock()
	h.s.m.eventStreams.Add(1)
	return sub, listening
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

	sub, listening := s.events.subscribe(names)
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
	// the first bytes tell the client the stream is up; a hub that is not
	// listening says so, and the client reconnects later
	if !listening {
		write("retry: " + fmt.Sprint(eventRetry.Milliseconds()) + "\nevent: error\ndata: {\"message\":\"not listening: the database is unreachable\"}\n\n")
		return nil
	}
	if !write("retry: 3000\n: listening " + strings.Join(names, ",") + "\n\n") {
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
		case e := <-sub.ch:
			s.m.eventDelivered.Add(1)
			var sb strings.Builder
			fmt.Fprintf(&sb, "id: %d\nevent: %s\ndata: {\"name\":%s,\"count\":%d}\n\n",
				s.events.seq.Add(1), e.Name, query.AppendJSON(nil, e.Name), e.Count)
			if lost := sub.lost.Swap(0); lost > 0 {
				fmt.Fprintf(&sb, ": %d notifications dropped, the stream was not read fast enough\n\n", lost)
			}
			if !write(sb.String()) {
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
