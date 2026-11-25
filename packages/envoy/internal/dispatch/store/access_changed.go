package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AccessChangedChannel is the Postgres channel each write that can move what someone sees
// notifies inside its own transaction, so every Dispatch process hears it once the write commits
// and none hears a write that did not: a project's owner, visibility or access list, or a group
// (the access package's writes), and a person's member kind (PgPeopleStore's SignIn,
// SignInSubject and ConfirmSubject, when the kind they write is not the one the row held). It
// carries no payload: a listener reads what it needs afresh. The document service closes on it
// each socket whose person no longer reads its room (docs: Service.recheckAccess), and moves the
// access epoch on which its sockets ask their rule again (docs: accessEpoch), as it does on
// events.SessionAccessChannel, which the writes of a session's ties and projects notify; and the
// live delivery hub resolves each subscriber's reader again (api: deliveryHub.recheckStaleReaders).
// A change made without it, by SQL alone, is found on the document sockets' heartbeat instead
// (docs: Service.RunAccessRecheck).
const AccessChangedChannel = "dispatch_access_changed"

// AnnounceAccessChange notifies AccessChangedChannel in tx, which Postgres delivers if tx commits.
func AnnounceAccessChange(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select pg_notify($1, '')`, AccessChangedChannel); err != nil {
		return fmt.Errorf("announce the access change: %w", err)
	}
	return nil
}
