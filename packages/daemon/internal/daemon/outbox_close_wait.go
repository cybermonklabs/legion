package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The lines the close_wait_publish executor logs: a version of close-wait.md the daemon's write
// replaced, which a person's edit made since the daemon read the document's token, and a write
// Dispatch refused the same way on every attempt, which finished the row.
const (
	closeWaitEditReplaced = "outbox close-wait.md changed since its token was read; the daemon's version replaced it"
	closeWaitRefused      = "outbox close-wait.md write refused by Dispatch; the row finishes and the record keeps the wait"
	closeWaitNoneStored   = "outbox close-wait.md row found no stored wait to write; the row finishes"
)

// closeWaitPublish writes the stored wait of the row's issue onto its Dispatch document
// close-wait.md (record.CloseWaitPublish). It renders the wait the issue's row holds when the row
// runs (record.CloseWaitDocument), not the one it held when the row was queued, so one row that
// stands while the wait changes writes the newest wait. The withdrawals that wait holds
// (record.StoredWait.Withdrawn) are said once: the workflow keeps each in the wait until a version
// holding it is written, and once Dispatch takes this version the row takes the withdrawals it
// showed out of the wait (record.Store.ClearWaitWithdrawals), under the lock the workflow writes
// the wait under, keeping any the wait gained while the row ran.
//
// Each write names the document's whole-document token, read just before (ArtifactToken), so it
// replaces exactly the version it read. The first write names none, since no document stands. A
// token the document moved past since the read, a person's edit say, is refused with the token
// that stands (dispatch.StaleDocument). The row then writes once more over that token, so the
// daemon's version replaces the edit, and logs that it did. A refusal Dispatch makes the same way
// on every attempt (dispatch.PermanentRefusal), a closed issue's ISSUE_CLOSED among them, is logged
// and finishes the row: the record keeps the wait, and `legion state` shows it. Every other
// failure fails the run, and the row runs again on its backoff.
func (r *outbox) closeWaitPublish(ctx context.Context, row record.OutboxRow, payload record.CloseWaitPublish) error {
	if r.dispatch == nil {
		return errors.New("close-wait.md executor has no Dispatch client")
	}
	var issue *record.Issue
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, row.Issue)
		return err
	}); err != nil {
		return fmt.Errorf("read the stored wait of %s: %w", row.Issue, err)
	}
	if issue == nil || len(issue.CloseWait) == 0 {
		r.log.Warn(closeWaitNoneStored, "row", row.ID, "issue", row.Issue, "generation", payload.Generation)
		return nil
	}
	content, err := record.CloseWaitDocument(*issue)
	if err != nil {
		return err
	}
	var shown record.StoredWait
	if err := json.Unmarshal(issue.CloseWait, &shown); err != nil {
		return fmt.Errorf("read the stored wait of %s: %w", row.Issue, err)
	}
	token := ""
	read, err := r.dispatch.ArtifactToken(ctx, row.Issue, record.CloseWaitDocumentSlug)
	switch {
	case errors.Is(err, dispatch.ErrDocumentNotFound):
	case err != nil:
		return r.closeWaitFailed(row, fmt.Errorf("read the token of %s of %s: %w", record.CloseWaitDocumentName, row.Issue, err))
	default:
		token = read.Token
	}
	err = r.uploadCloseWait(ctx, row, content, token)
	var stale *dispatch.StaleDocument
	if errors.As(err, &stale) && stale.Current != "" {
		r.log.Warn(closeWaitEditReplaced, "row", row.ID, "issue", row.Issue, "read", token, "standing", stale.Current)
		err = r.uploadCloseWait(ctx, row, content, stale.Current)
	}
	if err != nil {
		return r.closeWaitFailed(row, err)
	}
	if len(shown.Withdrawn) == 0 {
		return nil
	}
	// A failure here fails the run, and the row writes the same version again before it clears.
	if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		return r.records.ClearWaitWithdrawals(ctx, tx, row.Issue, shown.Withdrawn)
	}); err != nil {
		return fmt.Errorf("take the withdrawals close-wait.md of %s showed out of its stored wait: %w", row.Issue, err)
	}
	return nil
}

// uploadCloseWait writes content as close-wait.md of the row's issue over the version token names
// ("" for none). An upload Dispatch filed under another slug than close-wait.md's is an error,
// since every later write reads the token by that slug.
func (r *outbox) uploadCloseWait(ctx context.Context, row record.OutboxRow, content, token string) error {
	slug, err := r.dispatch.UploadArtifactOver(ctx, row.Issue, record.CloseWaitDocumentName, content, token)
	if err != nil {
		return fmt.Errorf("write %s of %s: %w", record.CloseWaitDocumentName, row.Issue, err)
	}
	if slug != record.CloseWaitDocumentSlug {
		return fmt.Errorf("write %s of %s: Dispatch filed it under %s, not %s", record.CloseWaitDocumentName, row.Issue, slug, record.CloseWaitDocumentSlug)
	}
	return nil
}

// closeWaitFailed answers a failed read or write of close-wait.md: a refusal Dispatch makes the
// same way on every attempt finishes the row with one log line, and every other failure is the
// run's, which the row's backoff retries. A token refused twice is no such refusal: the document
// moved again between the two writes, and the next run reads it afresh.
func (r *outbox) closeWaitFailed(row record.OutboxRow, err error) error {
	var stale *dispatch.StaleDocument
	if errors.As(err, &stale) {
		return err
	}
	refusal, permanent := dispatch.PermanentRefusal(err)
	if !permanent {
		return err
	}
	r.log.Error(closeWaitRefused, "row", row.ID, "issue", row.Issue, "status", refusal.Status, "code", refusal.Code,
		"error", storetext.FarSideError(err.Error()))
	return nil
}
