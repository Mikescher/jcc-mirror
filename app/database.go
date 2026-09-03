package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"blackforestbytes.com/jcc-mirror/engine"
	"blackforestbytes.com/jcc-mirror/store"
)

// RollbackDatabase puts a kept copy of a jcc pair's database back. It is the
// recovery of DESIGN.md S5, and it deliberately does not go through the runner:
// it moves a few MB, it asks the publisher nothing, and it must work while a sync
// is going or the tunnel is down - which is exactly when it is wanted.
func (a *App) RollbackDatabase(ctx context.Context, pairID, backupID int64, force bool, actor string) (engine.RollbackResult, error) {
	pair, err := a.store.PairByID(ctx, pairID)
	if err != nil {
		return engine.RollbackResult{}, err
	}

	eng, err := a.localEngine(ctx)
	if err != nil {
		return engine.RollbackResult{}, err
	}

	res, err := eng.RollbackDatabase(ctx, pair, backupID, force)
	if err != nil {
		return res, err
	}

	a.log.Infof("jcc: %s rolled back by %s", res.Path, actor)
	return res, nil
}

// localEngine builds an engine for the operations that touch nothing on the
// publisher. The tunnel being down is one of the reasons someone reaches for a
// rollback, so needing the remote to build one would be the wrong dependency.
func (a *App) localEngine(ctx context.Context) (*engine.Engine, error) {
	values, err := a.store.Config(ctx)
	if err != nil {
		return nil, err
	}
	opts := engine.OptionsFrom(values)
	opts.DataDir = a.opts.DataDir
	return engine.NewOffline(a.store, a.log, opts)
}

func (a *App) handleRollbackDatabase(w http.ResponseWriter, r *http.Request) {
	fields, err := readFields(w, r)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	pair, err := a.pairFromFields(r.Context(), fields)
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	// No backup named is the newest one, which is what the button on the page
	// without a list beside it means.
	var backupID int64
	if raw := strings.TrimSpace(fields["backup"]); raw != "" {
		if backupID, err = strconv.ParseInt(raw, 10, 64); err != nil {
			a.fail(w, r, http.StatusBadRequest, fmt.Errorf("backup id %q is not a number", raw))
			return
		}
	}
	force, _ := strconv.ParseBool(strings.TrimSpace(fields["force"]))

	res, err := a.RollbackDatabase(r.Context(), pair.ID, backupID, force, actorOf(r))
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNoBackup) {
			code = http.StatusNotFound
		}
		a.fail(w, r, code, err)
		return
	}

	if wantsHTML(r) {
		a.redirectHome(w, r)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
