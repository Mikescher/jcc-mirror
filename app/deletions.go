package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"blackforestbytes.com/jcc-mirror/format"
	"blackforestbytes.com/jcc-mirror/store"
)

// DecideDeletion records a person's answer to a deletion the guards stopped. It
// grants nothing by itself: the next run consumes the approval, and only for the
// walk and the count it was granted for (DESIGN.md §2.5).
func (a *App) DecideDeletion(ctx context.Context, pairID int64, state, actor string) (store.DeleteApproval, error) {
	pair, err := a.store.PairByID(ctx, pairID)
	if err != nil {
		return store.DeleteApproval{}, err
	}

	approval, err := a.store.DecideDeletion(ctx, pairID, state, actor)
	if err != nil {
		return store.DeleteApproval{}, err
	}

	verb := "approved"
	if state == store.ApprovalRejected {
		verb = "rejected"
	}
	msg := fmt.Sprintf("deletion of %s file(s) (%s) from %q %s by %s",
		format.Comma(approval.Files), format.Bytes(approval.Bytes), pair.Name, verb, actor)

	a.log.Infof("delete: %s", msg)
	id := pair.ID
	a.eventFor(ctx, &id, store.LevelWarn, store.KindDeleteDecided, msg, map[string]any{
		"decision": state, "files": approval.Files, "bytes": approval.Bytes,
		"reason": approval.Reason, "scanId": approval.ScanID, "approvalId": approval.ID,
	})
	return approval, nil
}

// handleDecideDeletion is the one-click half of the guard: a threshold that
// stopped a deletion is worth nothing if approving it means a shell on the NAS.
func (a *App) handleDecideDeletion(w http.ResponseWriter, r *http.Request) {
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

	state := store.ApprovalApproved
	if strings.TrimSpace(fields["decision"]) == store.ApprovalRejected {
		state = store.ApprovalRejected
	}

	approval, err := a.DecideDeletion(r.Context(), pair.ID, state, actorOf(r))
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, err)
		return
	}

	if wantsHTML(r) {
		a.redirectHome(w, r)
		return
	}
	writeJSON(w, http.StatusOK, approval)
}
