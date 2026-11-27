package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
)

// KeptDeliveryExpiryRetention is how long a kept_delivery_expiries row is kept after the outbox
// recorded it (recorded_at): 73 hours, the copy's hour on the role's kept lane (the listener's
// roleKeepWindow, cmd/listener/role_keep.go) plus the listener's dedupe window
// (contracts.DeliveryDuplicateWindow, 72 hours). Only the settle of an attempt whose send reported
// the copy's envelope reads the row (ReadKeptDeliveryExpiry), and a send reports it when it kept
// the copy, or when it repeats that send's key inside the dedupe window, which the listener
// answers with the copy it keeps under the key. Both are sent inside the dedupe window from when
// the copy was kept, which is before its notice was recorded, and the copy's hour on top of it is
// room for a settle still in flight at the window's end: no settle still to come reads a row older
// than the bound, and PruneKeptDeliveryExpiries deletes it.
const KeptDeliveryExpiryRetention = time.Hour + contracts.DeliveryDuplicateWindow

// KeptDeliveryExpiry names one copy of Dispatch's delivery of a post to a role that the listener
// kept for the role's first holder (kept_delivery_expiries): the copy's envelope id, which every
// attempt of the post that the copy carried records, the role it was kept for, and the post, a
// comment or a message by its kind and its id. The four are the table's key, so a record of one
// role's copy is never read as another role's, whatever envelope id and post a notice names.
type KeptDeliveryExpiry struct {
	EnvelopeID string
	Role       string
	PostKind   string
	PostID     string
}

// RecordKeptDeliveryExpiry records in tx that the copy expiry names ended with no session taking
// it, and whether the listener began to forward it and never confirmed that forward
// (forwardUnconfirmed): the role's holder may already have the message. It is the outbox's
// (internal/dispatch/outbox, settleKeptDelivery), made under the lock on the post's owner that
// ReadKeptDeliveryExpiry's caller holds too, so the two never interleave on one post. A copy
// recorded already keeps a forward_unconfirmed that was true, whatever a later notice says.
func RecordKeptDeliveryExpiry(ctx context.Context, tx pgx.Tx, expiry KeptDeliveryExpiry, forwardUnconfirmed bool) error {
	_, err := tx.Exec(ctx, `
		insert into kept_delivery_expiries (envelope_id, role, post_kind, post_id, forward_unconfirmed)
		values ($1, $2, $3, $4, $5)
		on conflict (envelope_id, role, post_kind, post_id) do update
		set forward_unconfirmed = kept_delivery_expiries.forward_unconfirmed or excluded.forward_unconfirmed
	`, expiry.EnvelopeID, expiry.Role, expiry.PostKind, expiry.PostID, forwardUnconfirmed)
	return err
}

// ReadKeptDeliveryExpiry reads in tx whether the copy expiry names was recorded as ended
// (RecordKeptDeliveryExpiry), and, when it was, whether its forward may have reached the role's
// holder. It is the API's, in the transaction that would record an attempt of the copy's post
// queued under the copy's envelope (internal/dispatch/api, settleDeliveryAttempt), made under the
// lock on the post's owner, which the outbox's record is made under as well: a record made before
// the attempt settled is found here, and one made after it finds the attempt queued and settles it.
func ReadKeptDeliveryExpiry(ctx context.Context, tx pgx.Tx, expiry KeptDeliveryExpiry) (forwardUnconfirmed, found bool, err error) {
	err = tx.QueryRow(ctx, `
		select forward_unconfirmed from kept_delivery_expiries
		where envelope_id = $1 and role = $2 and post_kind = $3 and post_id = $4
	`, expiry.EnvelopeID, expiry.Role, expiry.PostKind, expiry.PostID).Scan(&forwardUnconfirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return forwardUnconfirmed, true, nil
}

// PruneKeptDeliveryExpiries deletes in tx every kept_delivery_expiries row the outbox recorded
// more than KeptDeliveryExpiryRetention ago on the database's clock, which no settle will read,
// and answers how many it deleted. It is the outbox's, on the publisher hold's holder, in the
// expiry pass that records the rows (internal/dispatch/outbox, pruneKeptDeliveryExpiries). The
// table holds a row only for each copy whose end was recorded inside the bound, so the statement
// reads all of it rather than an index of recorded_at.
func PruneKeptDeliveryExpiries(ctx context.Context, tx pgx.Tx) (int64, error) {
	tag, err := tx.Exec(ctx, `
		delete from kept_delivery_expiries where recorded_at < now() - make_interval(secs => $1)
	`, KeptDeliveryExpiryRetention.Seconds())
	return tag.RowsAffected(), err
}
