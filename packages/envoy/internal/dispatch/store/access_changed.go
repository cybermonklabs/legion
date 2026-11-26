package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AccessChangedChannel is the Postgres channel each write that can move what someone sees
// notifies inside its own transaction, so every Dispatch process hears it once the write commits
// and none hears a write that did not: a project's owner, visibility or access list, or a group
// (the access package's writes); a later slice adds a person's member kind. It carries no payload:
// a listener reads what it needs afresh. Later slices add its listeners: the document service,
// which closes each socket whose person no longer reads its room and moves the access epoch on
// which its sockets ask their rule again, as it does on events.SessionAccessChannel, and the live
// delivery hub, which resolves each subscriber's reader again.
const AccessChangedChannel = "dispatch_access_changed"

// AnnounceAccessChange notifies AccessChangedChannel in tx, which Postgres delivers if tx commits.
func AnnounceAccessChange(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select pg_notify($1, '')`, AccessChangedChannel); err != nil {
		return fmt.Errorf("announce the access change: %w", err)
	}
	return nil
}
