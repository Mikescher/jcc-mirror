package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"blackforestbytes.com/jcc-mirror/store"
)

// feedInterval is how often the stream looks for something to send. A second is
// as fast as any of it changes: the transfer engine reports bytes continuously
// but a person reading a progress bar cannot tell a second from a tenth.
const feedInterval = time.Second

// idleStatusEvery is how many ticks pass between status frames when nothing is
// running. The status frame is the largest thing on the stream and an idle
// daemon's is the same every time.
const idleStatusEvery = 5

// logPreload is how many remembered log lines a stream replays on connect: the
// last screenful, not the whole ring.
const logPreload = 50

// streamBuffer is how far behind a client may fall before it is dropped. A
// browser that has been asleep in a background tab is reconnected rather than
// waited for: EventSource retries by itself, and the views refetch on connect.
const streamBuffer = 64

// frame is one server-sent event: a name the client can listen for and a body
// already encoded, so the same bytes go to every subscriber.
type frame struct {
	name string
	data []byte
}

type subscriber struct {
	ch chan frame
}

// stream is the SSE fan-out and the watermarks it sends from. Live data is one
// way and reconnects for free, which is the whole reason it is SSE rather than a
// socket (DESIGN.md §4).
type stream struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}

	// Where the feed has read up to. They are re-seeded whenever the first
	// subscriber arrives, so a dashboard opened after a week of syncing is not
	// sent the week.
	lastEvent  int64
	lastChange int64
	lastLog    int64
	seeded     bool

	ticks int
}

func (s *stream) subscribe() *subscriber {
	sub := &subscriber{ch: make(chan frame, streamBuffer)}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs == nil {
		s.subs = map[*subscriber]struct{}{}
	}
	s.subs[sub] = struct{}{}
	return sub
}

func (s *stream) unsubscribe(sub *subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop(sub)
}

// drop removes a subscriber and closes its channel. Membership in the map is
// what makes it happen exactly once. The caller holds the lock.
func (s *stream) drop(sub *subscriber) {
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	close(sub.ch)
}

// closeAll drops every subscriber. It is what a shutdown needs: an event stream
// is an open request that never ends by itself, so http.Server.Shutdown would
// otherwise wait out its whole timeout with a dashboard open somewhere.
func (s *stream) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		s.drop(sub)
	}
}

func (s *stream) subscribers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// publish sends one frame to every subscriber. One whose buffer is full is
// dropped rather than waited for - a slow reader must not hold up the daemon.
func (s *stream) publish(name string, v any) { s.send(frame{name: name}, v) }

func (s *stream) send(f frame, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	f.data = data

	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case sub.ch <- f:
		default:
			s.drop(sub)
		}
	}
}

// StreamState is the frame the "now" view is drawn from: everything that changes
// while something is running, in one message.
type StreamState struct {
	Status Status   `json:"status"`
	Runs   RunState `json:"runs"`
}

// handleStream is the live half of the dashboard. Everything it carries, the log
// tail included, is readable from the REST views as well; the stream exists so a
// page does not have to poll for it (DESIGN.md §4).
func (a *App) handleStream(w http.ResponseWriter, r *http.Request) {
	rc, ok := w.(http.Flusher)
	if !ok {
		a.fail(w, r, http.StatusInternalServerError, fmt.Errorf("this server cannot stream"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	// Nothing of ours buffers, but a reverse proxy in front of the LAN listener
	// might, and a buffered event stream is a dashboard that never updates.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := a.stream.subscribe()
	defer a.stream.unsubscribe(sub)

	// The first frame is the whole state, so a page that has just loaded has
	// something to draw before anything happens.
	if err := writeFrame(w, frame{name: "state", data: mustJSON(a.streamState(r.Context()))}); err != nil {
		return
	}
	rc.Flush()

	// A comment line every half minute keeps an idle connection from being
	// collected by whatever is between us and the browser.
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.closing:
			// The daemon is stopping or re-execing. The browser reconnects by
			// itself, to this process or to the one that replaces it.
			return
		case f, ok := <-sub.ch:
			if !ok {
				return // dropped for falling behind; EventSource will reconnect
			}
			if err := writeFrame(w, f); err != nil {
				return
			}
			rc.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			rc.Flush()
		}
	}
}

func writeFrame(w http.ResponseWriter, f frame) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.name, f.data)
	return err
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return data
}

func (a *App) streamState(ctx context.Context) StreamState {
	return StreamState{Status: a.Status(ctx), Runs: a.Runs(ctx)}
}

// feedLoop is the one poller behind every open stream. Polling rather than
// hooking the writers is deliberate: the engine writes its own events and
// changes straight to sqlite, so a hook would have to reach into it, and a query
// on an indexed id costs nothing at this rate.
func (a *App) feedLoop(ctx context.Context) {
	tick := time.NewTicker(feedInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			a.stream.closeAll()
			return
		case <-tick.C:
			a.feedTick(ctx)
		}
	}
}

func (a *App) feedTick(ctx context.Context) {
	if a.stream.subscribers() == 0 {
		// Nobody is watching, so the watermarks mean nothing: the next dashboard
		// to open gets what has happened since it opened, not since the daemon
		// started.
		a.stream.mu.Lock()
		a.stream.seeded = false
		a.stream.mu.Unlock()
		return
	}

	a.stream.mu.Lock()
	seeded := a.stream.seeded
	lastEvent, lastChange, lastLog := a.stream.lastEvent, a.stream.lastChange, a.stream.lastLog
	a.stream.ticks++
	ticks := a.stream.ticks
	a.stream.mu.Unlock()

	if !seeded {
		var err error
		if lastEvent, lastChange, lastLog, err = a.seedWatermarks(ctx); err != nil {
			// Leaving the watermarks at zero and calling it seeded would make the
			// next tick read the log forward from the beginning and push a year of
			// it at 200 rows a second. Try again on the next tick instead.
			a.log.Errorf("stream: %v", err)
			return
		}
		a.stream.mu.Lock()
		a.stream.lastEvent, a.stream.lastChange, a.stream.lastLog = lastEvent, lastChange, lastLog
		a.stream.seeded = true
		a.stream.mu.Unlock()
	}

	if events, err := a.store.Events(ctx, store.EventFilter{Forward: true, AfterID: lastEvent, Limit: 200}); err == nil && len(events) > 0 {
		a.stream.publish("events", events)
		a.setWatermark(&a.stream.lastEvent, events[len(events)-1].ID)
	}

	if changes, err := a.store.Changes(ctx, store.ChangeFilter{Forward: true, AfterID: lastChange, Limit: 200}); err == nil && len(changes) > 0 {
		a.stream.publish("changes", changes)
		a.setWatermark(&a.stream.lastChange, changes[len(changes)-1].ID)
	}

	if lines, newest := a.log.Tail(lastLog); len(lines) > 0 {
		a.stream.publish("log", lines)
		a.setWatermark(&a.stream.lastLog, newest)
	}

	// The state frame carries the progress bar, so it goes out every tick while
	// something is moving and rarely when nothing is. Built inside the check
	// rather than outside it: collecting one is four database round trips, and an
	// idle daemon would otherwise throw four fifths of them away.
	if ticks%idleStatusEvery == 0 || a.busy() {
		a.stream.publish("state", a.streamState(ctx))
	}
}

// seedWatermarks starts a fresh set of subscribers from now rather than from the
// beginning of the log.
func (a *App) seedWatermarks(ctx context.Context) (event, change, logSeq int64, err error) {
	events, err := a.store.Events(ctx, store.EventFilter{Limit: 1})
	if err != nil {
		return 0, 0, 0, err
	}
	if len(events) > 0 {
		event = events[0].ID
	}

	changes, err := a.store.Changes(ctx, store.ChangeFilter{Limit: 1})
	if err != nil {
		return 0, 0, 0, err
	}
	if len(changes) > 0 {
		change = changes[0].ID
	}
	// The log is the exception: a dashboard that has just opened wants the last
	// screenful of it, because what went wrong was said before anyone looked.
	_, newest := a.log.Tail(0)
	if logSeq = newest - logPreload; logSeq < 0 {
		logSeq = 0
	}
	return event, change, logSeq, nil
}

func (a *App) setWatermark(field *int64, value int64) {
	a.stream.mu.Lock()
	defer a.stream.mu.Unlock()
	if value > *field {
		*field = value
	}
}
