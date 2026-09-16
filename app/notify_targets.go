package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"blackforestbytes.com/jcc-mirror/store"
)

func (a *App) handleGetNotifyTopics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, store.NotifyTopics)
}

func (a *App) handleGetNotifyTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := a.store.NotifyTargets(r.Context())
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, targets)
}

func (a *App) handleCreateNotifyTarget(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	target := store.NotifyTarget{Enabled: true, Events: store.DefaultNotifyEvents()}
	if err := applyNotifyTargetFields(&target, fields); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := a.store.CreateNotifyTarget(r.Context(), &target); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	a.notifyTargetChanged(r.Context(), target, "added")
	writeJSON(w, http.StatusOK, target)
}

func (a *App) handleUpdateNotifyTarget(w http.ResponseWriter, r *http.Request) {
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
	if err := applyNotifyTargetFields(&target, fields); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := a.store.UpdateNotifyTarget(r.Context(), &target); err != nil {
		a.fail(w, r, notifyTargetErrorCode(err), err)
		return
	}

	a.notifyTargetChanged(r.Context(), target, "changed")
	writeJSON(w, http.StatusOK, target)
}

func (a *App) handleDeleteNotifyTarget(w http.ResponseWriter, r *http.Request) {
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
	if err := a.store.DeleteNotifyTarget(r.Context(), target.ID); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrNoNotifyTarget) {
			code = http.StatusNotFound
		}
		a.fail(w, r, code, err)
		return
	}

	a.notifyTargetChanged(r.Context(), target, "removed")
	writeJSON(w, http.StatusOK, target)
}

func (a *App) notifyTargetChanged(ctx context.Context, t store.NotifyTarget, what string) {
	a.log.Infof("notify: target %q %s", t.Name, what)
	a.Event(ctx, store.LevelInfo, store.KindNotifyChanged,
		fmt.Sprintf("notification target %q %s from the dashboard", t.Name, what),
		map[string]any{
			"action": what, "id": t.ID, "name": t.Name, "userId": t.UserID,
			"userKeySet": t.UserKeySet, "channel": t.Channel, "sender": t.Sender,
			"enabled": t.Enabled, "events": t.Events,
		})
}

func (a *App) notifyTargetFromFields(ctx context.Context, fields map[string]string) (store.NotifyTarget, error) {
	raw, ok := fields["id"]
	if !ok || strings.TrimSpace(raw) == "" {
		return store.NotifyTarget{}, badRequest(errors.New("no notification target id in the request"))
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return store.NotifyTarget{}, badRequest(fmt.Errorf("notification target id %q is not a number", raw))
	}
	return a.store.NotifyTargetByID(ctx, id)
}

// applyNotifyTargetFields writes the fields the request carried onto a target,
// leaving the rest as they were. A blank user key leaves the stored one alone:
// the dashboard is never sent it, so an untouched field comes back empty. An
// empty events field is no topics at all.
func applyNotifyTargetFields(t *store.NotifyTarget, fields map[string]string) error {
	for key, value := range fields {
		switch key {
		case "id":
		case "name":
			t.Name = value
		case "userId":
			t.UserID = value
		case "userKey":
			if strings.TrimSpace(value) != "" {
				t.UserKey = value
			}
		case "channel":
			t.Channel = value
		case "sender":
			t.Sender = value
		case "enabled":
			b, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("enabled must be true or false, not %q", value)
			}
			t.Enabled = b
		case "events":
			t.Events = store.ParseNotifyEvents(value)
		default:
			return fmt.Errorf("unknown notification target field %q", key)
		}
	}
	return nil
}

// notifyTargetErrorCode answers an error from resolving or writing a target: 404
// for one that does not exist, 400 for everything the request got wrong.
func notifyTargetErrorCode(err error) int {
	if errors.Is(err, store.ErrNoNotifyTarget) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}
