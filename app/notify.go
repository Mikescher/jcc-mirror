package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/notify"
	"blackforestbytes.com/jcc-mirror/store"
)

// sendTimeout bounds a notification. It is telemetry, so it must not be able to
// hold anything up, and a push that took longer than this has missed the moment
// anyway. Shorter than the notify client's own timeout, so this is the bound
// that actually applies.
const sendTimeout = 10 * time.Second

// The priorities of DESIGN.md §4.1. They are compiled in rather than configured:
// a person choosing per-message priorities is a person configuring the wrong
// thing, and what they actually want - "do not send me this" - is the toggle.
var notifyPriority = map[string]int{
	store.NotifySyncFailed:    1,
	store.NotifySyncOK:        0,
	store.NotifyDeleteBlocked: 2,
	store.NotifySpaceLow:      2,
	store.NotifyLockStale:     1,
	store.NotifyTunnelDown:    1,
	store.NotifyDBReplaced:    0,
}

// Notify sends one push through SCN to every enabled target that wants that
// kind. Everything about it is best-effort: a failure is recorded as an event
// and swallowed, because a notification is telemetry and never a step of the
// operation that produced it (DESIGN.md §4.1).
//
// Coalescing is the caller's job, not this function's: nothing here may be
// called per file.
func (a *App) Notify(ctx context.Context, kind string, pairID int64, title, content string) {
	if _, ok := store.NotifyTopicFor(kind); !ok {
		return
	}

	targets, err := a.store.NotifyTargets(ctx)
	if err != nil {
		a.log.Errorf("notify: %v", err)
		return
	}

	day := time.Now().Format(time.DateOnly)
	for _, target := range targets {
		if !target.Enabled || !target.Wants(kind) {
			continue
		}

		msg := notify.Message{
			Title:    title,
			Content:  content,
			Priority: notifyPriority[kind],
			// The idempotency key of DESIGN.md §4.1: with the day in it a retry
			// after a network blip costs nothing and a condition that recurs all
			// day collapses to one message on the server side. The target is in
			// it so that two targets on one account are still two messages.
			MsgID: notify.MsgID(kind, fmt.Sprint(pairID), day, fmt.Sprint(target.ID)),
		}

		// Detached from the caller: a sync that is being cancelled still gets to
		// say that it failed. Counted, so a shutdown waits for the event it may
		// write.
		a.bg.Add(1)
		go func() {
			defer a.bg.Done()
			a.send(target, msg, kind, pairID)
		}()
	}
}

func notifyConfig(t store.NotifyTarget) notify.Config {
	return notify.Config{UserID: t.UserID, UserKey: t.UserKey, Channel: t.Channel, Sender: t.Sender}
}

func (a *App) send(target store.NotifyTarget, msg notify.Message, kind string, pairID int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.runContext()), sendTimeout)
	defer cancel()

	err := a.notifier.Send(ctx, notifyConfig(target), msg)
	if err == nil {
		a.log.Debugf("notify: sent %q to %q", msg.Title, target.Name)
		return
	}

	level := store.LevelWarn
	message := fmt.Sprintf("notification to %q could not be sent: %v", target.Name, err)
	if errors.Is(err, notify.ErrQuota) {
		// Not a fault to fix, and the reason everything here is coalesced: the
		// quota is a finite resource and it has run out for today.
		message = fmt.Sprintf("notification to %q not sent: the daily SCN quota is exhausted", target.Name)
	}

	id := pairID
	pair := &id
	if pairID == 0 {
		pair = nil
	}
	a.eventFor(ctx, pair, level, store.KindError, message,
		map[string]any{"notification": kind, "title": msg.Title, "target": target.ID, "targetName": target.Name})
}

// handleTestNotification sends one message to the target the request names. It
// is the only path that ignores the target's topics and its enabled switch,
// because an operator pressing it has said what they want - and without it a
// mistyped user key means notifications silently never arrive, which is the
// exact failure the push channel exists to prevent.
func (a *App) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	target, err := a.notifyTargetFromFields(r.Context(), fields)
	if err != nil {
		a.fail(w, r, notifyTargetErrorCode(err), err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), sendTimeout)
	defer cancel()

	// No msg_id: two tests in one day are two questions, and collapsing them on
	// the server would make the second one look like it worked.
	err = a.notifier.Send(ctx, notifyConfig(target), notify.Message{
		Title:   "jcc-mirror is configured",
		Content: "A test notification from " + a.opts.Version + ".",
	})
	switch {
	case errors.Is(err, notify.ErrDisabled):
		a.fail(w, r, http.StatusBadRequest,
			fmt.Errorf("notification target %q has no SCN user id and user key", target.Name))
	case err != nil:
		a.fail(w, r, http.StatusBadGateway, err)
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
	}
}

// NotifyEdge is Notify for a condition that stays true: it sends when the
// condition starts and again when it clears, and says nothing in between. A
// tunnel that has been down since Tuesday must not spend the quota by itself
// (DESIGN.md §4.1).
func (a *App) NotifyEdge(ctx context.Context, kind string, pairID int64, active bool, onset, cleared string) {
	// The state that is recorded is the state that is true, not the one that
	// would be announced if it were: the Notifications page reads it back, and a
	// healthy tunnel described as down is worse than no description.
	title := onset
	if !active {
		title = cleared
	}

	edge, err := a.store.NotifyTransition(ctx, kind, pairID, active, title)
	if err != nil {
		a.log.Errorf("notify: %v", err)
		return
	}
	if edge {
		a.Notify(ctx, kind, pairID, title, "")
	}
}

// notifyRun is the one message a finished sync is allowed: "412 files, 2 failed",
// never one per failure.
func (a *App) notifyRun(ctx context.Context, run *Run, out outcome) {
	if run.Kind != RunSync {
		return
	}

	stopped := errors.Is(out.err, context.Canceled)
	switch {
	case out.err != nil && !stopped:
		a.Notify(ctx, store.NotifySyncFailed,
			run.PairID, fmt.Sprintf("Sync of %q failed", run.PairName), out.err.Error())
	case out.err == nil && out.sync != nil && out.sync.Failed > 0:
		a.Notify(ctx, store.NotifySyncFailed, run.PairID,
			fmt.Sprintf("Sync of %q finished with failures", run.PairName), out.summary)
	case out.err == nil:
		a.Notify(ctx, store.NotifySyncOK, run.PairID,
			fmt.Sprintf("Sync of %q finished", run.PairName), out.summary)
	}
}

// notifyOutcome raises the conditions a run turned up. They are edges rather than
// one-per-run messages because each of them persists until someone acts: a
// deletion waits for approval, a full volume stays full, a lock stays held.
func (a *App) notifyOutcome(ctx context.Context, run *Run, out outcome) {
	if held := out.reap != nil && out.reap.Blocked != nil; out.reap != nil {
		onset := ""
		if held {
			onset = fmt.Sprintf("Deletion from %q needs approval — %s file(s), %s: %s",
				run.PairName, format.Comma(out.reap.Blocked.Files),
				format.Bytes(out.reap.Blocked.Bytes), out.reap.Blocked.Reason)
		}
		a.NotifyEdge(ctx, store.NotifyDeleteBlocked, run.PairID, held, onset,
			fmt.Sprintf("Deletion from %q is no longer waiting", run.PairName))
	}

	// Only a sync looks at free space, so only a sync may clear the condition. A
	// scan that succeeded says nothing about whether the volume is still full.
	if run.Kind == RunSync {
		var noSpace *engine.SpaceError
		low := errors.As(out.err, &noSpace)
		onset := ""
		if low {
			onset = fmt.Sprintf("%q is out of room — %s", run.PairName, noSpace)
		}
		a.NotifyEdge(ctx, store.NotifySpaceLow, run.PairID, low, onset,
			fmt.Sprintf("%q has room again", run.PairName))
	}

	if out.database != nil {
		a.notifyDatabase(ctx, run, *out.database)
	}
}

// notifyDatabase reports the two things the lock gate can produce that someone
// would want to know: the database was replaced, and the publisher's lock has
// been sitting there long enough that it never will be.
func (a *App) notifyDatabase(ctx context.Context, run *Run, res engine.DBResult) {
	if res.Replaced {
		a.Notify(ctx, store.NotifyDBReplaced, run.PairID,
			fmt.Sprintf("%s replaced on %q", res.Path, run.PairName),
			fmt.Sprintf("%s, the previous copy kept", format.Bytes(res.Bytes)))
	}

	stale, onset := false, ""
	for _, lock := range res.Locks {
		if lock.Held && lock.Stale {
			stale = true
			onset = fmt.Sprintf("%q: the database is not syncing — %s", run.PairName, lock)
			break
		}
	}
	a.NotifyEdge(ctx, store.NotifyLockStale, run.PairID, stale, onset,
		fmt.Sprintf("%q: the stale lock is gone and the database can sync again", run.PairName))
}
