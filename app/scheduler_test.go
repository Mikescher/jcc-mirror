package app

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"blackforestbytes.com/jcc-mirror/schedule"
	"blackforestbytes.com/jcc-mirror/store"
)

// configure writes settings the way the dashboard does, so the reload path is
// the one under test rather than a shortcut around it.
func (m *mirror) configure(t *testing.T, values url.Values) {
	t.Helper()
	if rec := postForm(t, m.h, "/api/config", m.token, values); rec.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", rec.Code, rec.Body)
	}
}

// tick runs one scheduler pass and waits for whatever it started. It reports the
// kind of run, or "" when the scheduler decided there was nothing to do - which
// it can be asked immediately, since a run is registered before it is started.
func (m *mirror) tick(t *testing.T, now time.Time) string {
	t.Helper()
	ctx := context.Background()

	before := len(m.app.Runs(ctx).History)
	m.app.schedulerTick(ctx, now)

	state := m.app.Runs(ctx)
	if state.Current == nil && len(state.History) == before {
		return ""
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		state = m.app.Runs(ctx)
		if state.Current == nil && len(state.History) > before {
			run := state.History[0]
			if run.Error != "" {
				t.Fatalf("scheduled %s of %q: %s", run.Kind, run.PairName, run.Error)
			}
			return run.Kind
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the scheduled run did not finish")
	return ""
}

// TestSchedulerMirrorsUnattended is what the scheduler is for: with the switch
// on and the window open, the walk and the transfer happen with nobody pressing
// anything.
func TestSchedulerMirrorsUnattended(t *testing.T) {
	m := newMirror(t)
	want := m.write(t, "Filme/Grüße.mkv", 16*1024)
	m.configure(t, url.Values{store.KeyAutomatic: {"true"}})

	now := time.Now()
	if kind := m.tick(t, now); kind != RunScan {
		t.Fatalf("the first tick ran %q, want a scan: nothing can be planned without a manifest", kind)
	}
	if kind := m.tick(t, now); kind != RunSync {
		t.Fatalf("the second tick ran %q, want a sync", kind)
	}

	got, err := os.ReadFile(filepath.Join(m.dst, filepath.FromSlash("Filme/Grüße.mkv")))
	if err != nil {
		t.Fatalf("read the mirrored file: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the file is %d bytes, want %d", len(got), len(want))
	}

	// And then it settles: a pair with a fresh manifest and an empty queue is not
	// re-planned every half minute.
	if kind := m.tick(t, now); kind != "" {
		t.Errorf("the third tick ran %q, want nothing left to do", kind)
	}
}

// TestSchedulerWaitsForItsWindow is the gate. The same tick that would have
// started a run does nothing at all outside the hours the grid opens.
func TestSchedulerWaitsForItsWindow(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/a.mkv", 2048)
	m.configure(t, url.Values{
		store.KeyAutomatic:    {"true"},
		store.KeySchedule:     {"* * = off"},
		store.KeyScanSchedule: {"* * = off"},
	})

	if kind := m.tick(t, time.Now()); kind != "" {
		t.Fatalf("a closed window ran %q", kind)
	}

	// Opening only the scan window gets the manifest and nothing more: the two
	// halves of the grid are separate answers.
	m.configure(t, url.Values{store.KeyScanSchedule: {"* * = on"}})
	if kind := m.tick(t, time.Now()); kind != RunScan {
		t.Fatalf("with the scan window open the tick ran %q, want a scan", kind)
	}
	if kind := m.tick(t, time.Now()); kind != "" {
		t.Fatalf("with the transfer window still closed the tick ran %q", kind)
	}
}

// TestSwitchedOffNothingRuns is the default an install starts on: the grid is
// wide open, and still nothing happens until someone turns the scheduler on.
func TestSwitchedOffNothingRuns(t *testing.T) {
	m := newMirror(t)
	m.write(t, "Filme/a.mkv", 2048)

	if kind := m.tick(t, time.Now()); kind != "" {
		t.Fatalf("the scheduler ran %q while switched off", kind)
	}
}

// TestTheCapFollowsTheGrid is the other half of the schedule: the number in the
// current cell is on the limiter, whether or not anything is scheduled to run.
func TestTheCapFollowsTheGrid(t *testing.T) {
	m := newMirror(t)

	m.configure(t, url.Values{store.KeySchedule: {"* * = 5MiB"}})
	m.app.schedulerTick(context.Background(), time.Now())
	if got := m.app.limiter.Limit(); got != 5<<20 {
		t.Errorf("the cap is %d, want 5 MiB/s", got)
	}

	m.configure(t, url.Values{store.KeySchedule: {"* * = full"}})
	m.app.schedulerTick(context.Background(), time.Now())
	if got := m.app.limiter.Limit(); got != 0 {
		t.Errorf("the cap is %d, want none", got)
	}

	// The change is recorded on the edge and not on every tick, which is the
	// discipline the notifications need in M6: a condition that persists is
	// reported once.
	windows := func() int {
		t.Helper()
		events, err := m.app.store.Events(context.Background(), store.EventFilter{Kinds: []string{store.KindWindow}})
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		return len(events)
	}

	before := windows()
	m.app.schedulerTick(context.Background(), time.Now())
	m.app.schedulerTick(context.Background(), time.Now())
	if after := windows(); after != before {
		t.Errorf("%d window events after two ticks that changed nothing, want the %d there were", after, before)
	}
}

// TestOnlyItsOwnRunsAreStopped: a window closing ends a scheduled transfer,
// because stopping one costs nothing. It does not end one a person started -
// that is someone at the console who wants bytes moved now.
func TestOnlyItsOwnRunsAreStopped(t *testing.T) {
	closed := schedule.Window{}
	open := schedule.Window{Open: true}

	for _, tc := range []struct {
		name string
		auto bool
		want bool
	}{
		{"scheduled", true, true},
		{"by hand", false, false},
	} {
		a, _, _ := newApp(t)

		// Planted under the runner's lock and atomically, because the run is a
		// fixture rather than a real one: the daemon's own scheduler and its
		// shutdown reach for the same fields.
		var stopped atomic.Bool
		a.runs.mu.Lock()
		a.runs.current = &Run{Kind: RunSync, Auto: tc.auto, PairName: "media", StartedAt: time.Now()}
		a.runs.cancel = func() { stopped.Store(true) }
		a.runs.mu.Unlock()

		if busy := a.tendRun(closed, open); !busy {
			t.Errorf("%s: tendRun said idle while a run was in flight", tc.name)
		}
		if stopped.Load() != tc.want {
			t.Errorf("%s: stopped = %v, want %v", tc.name, stopped.Load(), tc.want)
		}

		a.runs.mu.Lock()
		a.runs.current, a.runs.cancel = nil, nil
		a.runs.mu.Unlock()
	}
}

// TestUntilNextSleepsToTheBoundary keeps the caps punctual without a busy loop:
// the wait is to the next change, capped by the ordinary interval.
func TestUntilNextSleepsToTheBoundary(t *testing.T) {
	now := time.Date(2026, 6, 3, 7, 59, 50, 0, time.UTC)
	soon := now.Add(10 * time.Second)
	far := now.Add(6 * time.Hour)

	if d := untilNext(now, soon, far); d < 10*time.Second || d > 11*time.Second {
		t.Errorf("untilNext = %s, want just past the boundary", d)
	}
	if d := untilNext(now, far); d != schedulerInterval {
		t.Errorf("untilNext = %s, want the interval when the boundary is hours away", d)
	}
	if d := untilNext(now, now); d != time.Second {
		t.Errorf("untilNext = %s on a boundary that has just passed, want a second", d)
	}
}
