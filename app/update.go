package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
	"blackforestbytes.com/jcc-mirror/update"
)

// confirmAfter is how long the new binary has to stay up before the update is
// trusted. Until it does, a non-zero exit puts the previous binary back - so
// this is the window the supervisor's safety net covers (DESIGN.md §5).
const confirmAfter = 60 * time.Second

// checkTimeout bounds a HEAD of the binary URL, and applyTimeout the download,
// the smoke test and the rename together. An update is never urgent, but it must not
// be able to sit on a connection for an afternoon either.
const (
	checkTimeout = 30 * time.Second
	applyTimeout = 30 * time.Minute
)

// firstCheckDelay keeps the update check out of the first seconds of a boot,
// where the network may not be up yet and a failure has nothing to do with
// updates.
const firstCheckDelay = 2 * time.Minute

// updates is what the last check found. It is in memory because it is a cache of
// one HEAD request: the durable half of the update story is the state file in the
// data volume, which the supervisor reads.
type updates struct {
	mu        sync.Mutex
	manager   update.Manager
	checkedAt time.Time
	release   *update.Release
	newer     bool
	blocked   string
	err       string
	busy      bool
}

// UpdateStatus is the Update panel: what is running, what the server has, and
// which of the two buttons makes sense right now.
type UpdateStatus struct {
	Version    string     `json:"version"`
	BuildStamp string     `json:"buildStamp,omitempty"`
	BuiltAt    *time.Time `json:"builtAt,omitempty"`

	// Binary is the executable actually running, and Managed says whether it came
	// out of the data volume rather than the image. A first install runs the
	// image's binary and has no /data/bin at all.
	Binary  string `json:"binary,omitempty"`
	Managed bool   `json:"managed"`

	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"`
	Auto       bool   `json:"auto"`
	Every      string `json:"every,omitempty"`

	CheckedAt *time.Time      `json:"checkedAt,omitempty"`
	Available *update.Release `json:"available,omitempty"`
	Newer     bool            `json:"newer"`
	Blocked   string          `json:"blocked,omitempty"`
	Busy      bool            `json:"busy,omitempty"`
	Error     string          `json:"error,omitempty"`

	State       *update.State `json:"state,omitempty"`
	CanRollBack bool          `json:"canRollBack"`
}

// The two refusals that are not failures: the server has nothing newer, or what
// it has is the binary a previous attempt had to undo. They are sentinels so the
// endpoint can answer 409 rather than 502 - a person pressing a button on a
// mirror that is already current has not hit an upstream fault.
var (
	errUpToDate = errors.New("the binary at the update URL is not newer than this one")
	errBlocked  = errors.New("that binary was rolled back once already")
)

func (a *App) buildTime() time.Time { return update.ParseBuildStamp(a.opts.BuildStamp) }

// UpdateStatus collects the panel without talking to the server: it is read on
// every page load, and a check is a request someone has to ask for.
func (a *App) UpdateStatus(ctx context.Context) UpdateStatus {
	st := UpdateStatus{Version: a.opts.Version, BuildStamp: a.opts.BuildStamp}
	if built := a.buildTime(); !built.IsZero() {
		st.BuiltAt = &built
	}
	if exe, err := os.Executable(); err == nil {
		st.Binary = exe
		st.Managed = exe == a.upd.manager.Binary()
	}

	values, err := a.store.Config(ctx)
	if err == nil {
		st.Configured = strings.TrimSpace(values.Get(store.KeyUpdateURL)) != ""
		st.Auto = values.Bool(store.KeyUpdateAuto)
		st.Every = values.Duration(store.KeyUpdateInterval).String()
		if where, err := a.updateLocation(values); err == nil {
			st.Source = where
		}
	}

	if state, ok, err := a.upd.manager.LoadState(); err == nil && ok {
		st.State = &state
	}
	// A rollback is offered whenever there is a managed binary to step off, with
	// or without a previous one: removing it falls back to the image's, which on
	// a first update is exactly the binary that was replaced.
	st.CanRollBack = a.upd.manager.Installed()

	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	if !a.upd.checkedAt.IsZero() {
		checked := a.upd.checkedAt
		st.CheckedAt = &checked
	}
	st.Available, st.Newer = a.upd.release, a.upd.newer
	st.Blocked, st.Error, st.Busy = a.upd.blocked, a.upd.err, a.upd.busy
	return st
}

// updateReady is the one bit of the panel the topbar wants, without the config
// read the whole of it costs.
func (a *App) updateReady() bool {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()
	return a.upd.newer && a.upd.blocked == ""
}

// updateLocation is where the binary would come from, in the form the panel
// shows it.
func (a *App) updateLocation(values store.Values) (string, error) {
	src, err := a.updateSource(values)
	if err != nil {
		return "", err
	}
	return src.Location(), nil
}

// updateSource is the configured URL, fetched directly over the host's network
// rather than the tunnel (DESIGN.md §5).
func (a *App) updateSource(values store.Values) (update.Source, error) {
	rawURL, err := update.Locate(values.Get(store.KeyUpdateURL))
	if err != nil {
		return update.Source{}, err
	}
	return update.Source{URL: rawURL}, nil
}

// CheckUpdate asks the server what it has. It is one HEAD request, and the answer
// is remembered rather than acted on: whether to install it is the auto setting's
// business, or a button's.
func (a *App) CheckUpdate(ctx context.Context) (update.Release, bool, error) {
	values, err := a.store.Config(ctx)
	if err != nil {
		return update.Release{}, false, err
	}
	src, err := a.updateSource(values)
	if err != nil {
		a.noteCheck(nil, false, "", err)
		return update.Release{}, false, err
	}

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	rel, err := src.Head(ctx)
	if err != nil {
		a.noteCheck(nil, false, "", err)
		return update.Release{}, false, err
	}

	newer := rel.ModTime.After(a.upd.manager.Baseline(a.buildTime()))
	blocked := ""
	// An update that was installed and rolled back is not offered again by
	// itself. Without this a binary that crashes on startup would be fetched,
	// installed, rolled back and fetched again on every check - the one loop a
	// self-updater must not be able to get into.
	if failed, ok := a.upd.manager.Blocked(); ok && !rel.ModTime.After(failed.RemoteModTime) {
		blocked = failed.RollbackReason
		if blocked == "" {
			blocked = "this binary was rolled back once already"
		}
	}

	if a.noteCheck(&rel, newer, blocked, nil) && newer && blocked == "" {
		a.log.Infof("update: %s has a binary from %s, newer than this one", rel.URL, rel.ModTime.Format(time.RFC3339))
		a.Event(ctx, store.LevelInfo, store.KindUpdateFound,
			"a newer binary is at the update URL, from "+rel.ModTime.Format(time.RFC3339),
			map[string]any{"url": rel.URL, "size": rel.Size, "auto": values.Bool(store.KeyUpdateAuto)})
	}
	return rel, newer && blocked == "", nil
}

// noteCheck records what the check found and reports whether it is news - a
// different binary from the one the last check saw. Everything about the update
// panel is polled, so without that test an available update would be an event
// every check interval, forever.
func (a *App) noteCheck(rel *update.Release, newer bool, blocked string, err error) bool {
	a.upd.mu.Lock()
	defer a.upd.mu.Unlock()

	changed := rel != nil && (a.upd.release == nil || !a.upd.release.ModTime.Equal(rel.ModTime))
	a.upd.checkedAt, a.upd.release, a.upd.newer, a.upd.blocked = time.Now(), rel, newer, blocked
	a.upd.err = ""
	if err != nil && !errors.Is(err, update.ErrNotConfigured) {
		a.upd.err = err.Error()
	}
	return changed
}

// ApplyUpdate does DESIGN.md §5 end to end: check, download, verify, smoke-test,
// install, and ask for the re-exec. force installs a binary that is not newer, or
// one that a previous attempt rolled back - both are things only a person at the
// dashboard should be able to say.
func (a *App) ApplyUpdate(ctx context.Context, force bool) (update.State, error) {
	values, err := a.store.Config(ctx)
	if err != nil {
		return update.State{}, err
	}
	src, err := a.updateSource(values)
	if err != nil {
		return update.State{}, err
	}

	a.upd.mu.Lock()
	if a.upd.busy {
		a.upd.mu.Unlock()
		return update.State{}, errors.New("an update is already being installed")
	}
	a.upd.busy = true
	a.upd.mu.Unlock()
	defer func() {
		a.upd.mu.Lock()
		a.upd.busy = false
		a.upd.mu.Unlock()
	}()

	// Re-checked rather than taken from the panel: the panel may be hours old, and
	// what is about to be installed should be what the server has now.
	rel, newer, err := a.CheckUpdate(ctx)
	if err != nil {
		return update.State{}, err
	}
	if !newer && !force {
		a.upd.mu.Lock()
		blocked := a.upd.blocked
		a.upd.mu.Unlock()
		if blocked != "" {
			return update.State{}, fmt.Errorf("%w (%s); install it anyway with force", errBlocked, blocked)
		}
		return update.State{}, errUpToDate
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), applyTimeout)
	defer cancel()

	a.log.Infof("update: fetching %s (%s)", rel.URL, format.Bytes(rel.Size))
	res, err := a.upd.manager.Apply(ctx, src, rel, a.opts.Version)
	if err != nil {
		a.log.Errorf("update: %v", err)
		a.Event(ctx, store.LevelError, store.KindUpdateFailed, "update failed: "+err.Error(),
			map[string]any{"url": rel.URL})
		return update.State{}, err
	}

	a.log.Infof("update: %s installed, restarting into it", res.State.ToVersion)
	a.Event(ctx, store.LevelInfo, store.KindUpdateApplied,
		fmt.Sprintf("update applied: %s replaces %s, restarting", res.State.ToVersion, res.State.FromVersion),
		map[string]any{"url": rel.URL, "from": res.State.FromVersion, "to": res.State.ToVersion, "build": res.State.ToBuild})
	a.Notify(ctx, store.NotifyUpdateApplied, 0,
		"jcc-mirror updated to "+res.State.ToVersion,
		fmt.Sprintf("%s replaces %s. If it does not come back up, the previous binary is put back automatically.",
			res.State.ToVersion, res.State.FromVersion))

	a.requestRestart(res.Exec)
	return res.State, nil
}

// RollbackUpdate steps back onto the previous binary, or onto the image's when
// the update replaced that. It is the button for the case the supervisor cannot
// see: a new binary that starts perfectly well and then does the wrong thing.
func (a *App) RollbackUpdate(ctx context.Context, reason string) (update.State, error) {
	res, err := a.upd.manager.Rollback(reason)
	if err != nil {
		return update.State{}, err
	}

	a.log.Warnf("update: rolled back, restarting")
	a.Event(ctx, store.LevelWarn, store.KindUpdateBack, "update rolled back: "+reason,
		map[string]any{"from": res.State.ToVersion, "to": res.State.FromVersion})

	// Recorded as already announced: the person who pressed the button knows, and
	// without this the daemon that comes back would report it a second time. What
	// reportUpdate exists for is the supervisor's rollbacks, which nobody watched.
	if err := a.upd.manager.MarkReported(); err != nil {
		a.log.Errorf("update: %v", err)
	}

	// The check cache describes a binary that is no longer the one running.
	a.noteCheck(nil, false, "", nil)
	a.requestRestart(res.Exec)
	return res.State, nil
}

// Restart is what the daemon's own main loop waits on beside its context. The
// value is the binary to exec into, empty when it is the one in the image and
// therefore not something this process can name.
func (a *App) Restart() <-chan string { return a.restart }

func (a *App) requestRestart(binary string) {
	// A run in flight is stopped rather than left to be killed by the exec.
	// Nothing is lost: a scan stays resumable and every transfer keeps its
	// watermark, which is what makes "just stop" the quiesce of DESIGN.md §5.
	_ = a.CancelRun("stopped: restarting into a new binary")

	select {
	case a.restart <- binary:
	default: // a restart is already pending; the first one wins
	}
}

// reportUpdate is what a process started by the supervisor says about the one
// before it. The supervisor has no configuration and no database, so a rollback
// it performed is written down and announced here.
func (a *App) reportUpdate(ctx context.Context) {
	state, ok, err := a.upd.manager.LoadState()
	if err != nil {
		a.log.Errorf("update: %v", err)
		return
	}
	if !ok {
		return
	}

	switch {
	case state.State == update.StateRolledBack && !state.Reported:
		a.log.Warnf("update: %s was put back — %s", a.opts.Version, state.RollbackReason)
		a.Event(ctx, store.LevelWarn, store.KindUpdateBack,
			fmt.Sprintf("the update to %s was rolled back: %s", state.ToVersion, state.RollbackReason),
			map[string]any{"from": state.ToVersion, "to": a.opts.Version})
		a.Notify(ctx, store.NotifyUpdateRolledBack, 0,
			"jcc-mirror rolled an update back",
			fmt.Sprintf("%s would not run and %s is back. It will not be installed again on its own.",
				state.ToVersion, a.opts.Version))
		if err := a.upd.manager.MarkReported(); err != nil {
			a.log.Errorf("update: %v", err)
		}

	case state.State == update.StateApplied:
		// The window the supervisor's safety net covers. Confirming closes it, so
		// a crash next week is an ordinary crash rather than a rollback.
		go a.confirmUpdate(ctx, state)
	}
}

func (a *App) confirmUpdate(ctx context.Context, state update.State) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(confirmAfter):
	}

	confirmed, err := a.upd.manager.Confirm(state)
	switch {
	case err != nil:
		a.log.Errorf("update: %v", err)
	case confirmed:
		a.log.Infof("update: %s has been up for %s, keeping it", state.ToVersion, format.Duration(confirmAfter))
	}
}

// updateLoop asks the server for a newer binary on the configured interval, and
// installs it when it was told to. The interval is re-read every round, so a
// change in the dashboard takes effect without a restart.
func (a *App) updateLoop(ctx context.Context) {
	wait := firstCheckDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		values, err := a.store.Config(ctx)
		if err != nil {
			a.log.Errorf("update: %v", err)
			wait = time.Hour
			continue
		}
		wait = values.Duration(store.KeyUpdateInterval)

		if strings.TrimSpace(values.Get(store.KeyUpdateURL)) == "" {
			continue
		}
		_, newer, err := a.CheckUpdate(ctx)
		switch {
		case err != nil && errors.Is(err, update.ErrNotConfigured):
		case err != nil:
			a.log.Warnf("update: %v", err)
		case newer && values.Bool(store.KeyUpdateAuto):
			if _, err := a.ApplyUpdate(ctx, false); err != nil {
				a.log.Errorf("update: %v", err)
			}
		}
	}
}

// ---- the dashboard's three buttons -------------------------------------

func (a *App) handleGetUpdate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.UpdateStatus(r.Context()))
}

func (a *App) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	if _, _, err := a.CheckUpdate(r.Context()); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, update.ErrNotConfigured) {
			code = http.StatusBadRequest
		}
		a.fail(w, r, code, err)
		return
	}
	writeJSON(w, http.StatusOK, a.UpdateStatus(r.Context()))
}

func (a *App) handleApplyUpdate(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	force, _ := strconv.ParseBool(strings.TrimSpace(fields["force"]))

	state, err := a.ApplyUpdate(r.Context(), force)
	if err != nil {
		code := http.StatusBadGateway
		switch {
		case errors.Is(err, update.ErrNotConfigured):
			code = http.StatusBadRequest
		case errors.Is(err, errUpToDate), errors.Is(err, errBlocked):
			code = http.StatusConflict
		}
		a.fail(w, r, code, err)
		return
	}
	// Answered before the exec, which happens as soon as the daemon's own loop
	// sees the request: the browser is reconnecting to the same address in a
	// second either way.
	writeJSON(w, http.StatusAccepted, state)
}

func (a *App) handleRollbackUpdate(w http.ResponseWriter, r *http.Request) {
	state, err := a.RollbackUpdate(r.Context(), "rolled back from the dashboard by "+actorOf(r))
	if err != nil {
		a.fail(w, r, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, state)
}
