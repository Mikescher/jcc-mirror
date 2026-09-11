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

// RemoteView is a remote as the Remotes tab shows it: the row, the target its
// client reads, and the pairs that read from it - which is also what decides
// whether it may be removed.
type RemoteView struct {
	store.Remote
	Target string   `json:"target"`
	Error  string   `json:"error,omitempty"`
	Pairs  []string `json:"pairs"`
}

func (a *App) handleGetRemotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	remotes, err := a.store.Remotes(ctx)
	if err != nil {
		a.fail(w, r, http.StatusInternalServerError, err)
		return
	}

	out := make([]RemoteView, 0, len(remotes))
	for _, rem := range remotes {
		v := RemoteView{Remote: rem, Pairs: []string{}}
		if v.Target, err = a.remoteState(rem.ID); err != nil {
			v.Error = err.Error()
		}
		names, err := a.store.PairNamesForRemote(ctx, rem.ID)
		if err != nil {
			a.fail(w, r, http.StatusInternalServerError, err)
			return
		}
		v.Pairs = append(v.Pairs, names...)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleCreateRemote(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	var rem store.Remote
	if err := applyRemoteFields(&rem, fields); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := a.store.CreateRemote(r.Context(), &rem); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	a.remoteChanged(r.Context(), rem, "added")
	writeJSON(w, http.StatusOK, rem)
}

func (a *App) handleUpdateRemote(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	rem, err := a.remoteFromFields(r.Context(), fields)
	if err != nil {
		a.fail(w, r, remoteErrorCode(err), err)
		return
	}
	// Only what the request carried is written back, as for a pair.
	if err := applyRemoteFields(&rem, fields); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}
	if err := a.store.UpdateRemote(r.Context(), &rem); err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	a.remoteChanged(r.Context(), rem, "changed")
	writeJSON(w, http.StatusOK, rem)
}

func (a *App) handleDeleteRemote(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	rem, err := a.remoteFromFields(r.Context(), fields)
	if err != nil {
		a.fail(w, r, remoteErrorCode(err), err)
		return
	}
	if err := a.store.DeleteRemote(r.Context(), rem.ID); err != nil {
		code := http.StatusInternalServerError
		switch {
		case errors.Is(err, store.ErrRemoteInUse):
			code = http.StatusConflict
		case errors.Is(err, store.ErrNoRemote):
			code = http.StatusNotFound
		}
		a.fail(w, r, code, err)
		return
	}

	a.remoteChanged(r.Context(), rem, "removed")
	writeJSON(w, http.StatusOK, rem)
}

// remoteChanged records a change and re-applies the configuration, which is what
// builds, rebuilds or closes the remote's client.
func (a *App) remoteChanged(ctx context.Context, rem store.Remote, what string) {
	a.log.Infof("remotes: %q %s", rem.Name, what)
	a.Event(ctx, store.LevelInfo, store.KindRemoteChanged,
		fmt.Sprintf("remote %q %s from the dashboard", rem.Name, what),
		map[string]any{
			"action": what, "id": rem.ID, "name": rem.Name, "host": rem.Host,
			"share": rem.Share, "path": rem.Path, "user": rem.User, "domain": rem.Domain,
			"passwordSet": rem.PasswordSet,
		})
	a.Reload(ctx)
}

// remoteFromFields loads the remote an id in the request names.
func (a *App) remoteFromFields(ctx context.Context, fields map[string]string) (store.Remote, error) {
	raw, ok := fields["id"]
	if !ok {
		return store.Remote{}, badRequest(errors.New("no remote id in the request"))
	}
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return store.Remote{}, badRequest(fmt.Errorf("remote id %q is not a number", raw))
	}
	return a.store.RemoteByID(ctx, id)
}

// applyRemoteFields writes the fields the request carried onto a remote, leaving
// the rest as they were. A blank password leaves the stored one alone: the
// dashboard is never sent it, so an untouched field comes back empty. Removing
// one takes clearPassword.
func applyRemoteFields(rem *store.Remote, fields map[string]string) error {
	clear := false
	for key, value := range fields {
		switch key {
		case "id":
		case "name":
			rem.Name = value
		case "host":
			rem.Host = value
		case "share":
			rem.Share = value
		case "path":
			rem.Path = value
		case "user":
			rem.User = value
		case "domain":
			rem.Domain = value
		case "password":
			if strings.TrimSpace(value) != "" {
				rem.Password = value
			}
		case "clearPassword":
			b, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("clearPassword must be true or false, not %q", value)
			}
			clear = b
		default:
			return fmt.Errorf("unknown remote field %q", key)
		}
	}

	if clear {
		if strings.TrimSpace(fields["password"]) != "" {
			return errors.New("send a new password or clearPassword, not both")
		}
		rem.Password = ""
	}
	return nil
}

// requestError is a mistake in what the request sent, as opposed to a remote that
// could not be reached.
type requestError struct{ error }

func (e requestError) Unwrap() error { return e.error }

func badRequest(err error) error { return requestError{err} }

// remoteErrorCode answers an error from resolving or reaching a remote: 400 for
// a request that named it wrongly, 404 for one that does not exist, and 502 for
// the rest, which are the publisher's side.
func remoteErrorCode(err error) int {
	var bad requestError
	switch {
	case errors.As(err, &bad):
		return http.StatusBadRequest
	case errors.Is(err, store.ErrNoRemote):
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}
