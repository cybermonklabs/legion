package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
)

// KeptDeliveryExpiryRetention is the retention of a kept_delivery_expiries row, counted from
// recorded_at. A row is kept for 73 hours after the outbox recorded it. That is the copy's hour on
// the role's kept lane, plus a margin of 72 hours for a Dispatch outage. The margin is set equal
// to contracts.DeliveryDuplicateWindow. The row has one reader: Dispatch's write of queued after
// the listener's 202 kept answer (ReadKeptDeliveryExpiry). Dispatch can be down between the
// listener's notice and that write. The long bound keeps the row through such an outage. The rows
// are small.
//
// The hour is the listener's roleKeepWindow (cmd/listener/role_keep.go), and a test there holds
// the two equal (TestTheKeptDeliveryRetentionsHourIsTheKeepWindow). PruneKeptDeliveryExpiries
// deletes a row past the bound.
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

// PruneKeptDeliveryExpiries deletes every kept_delivery_expiries row the outbox recorded more than
// KeptDeliveryExpiryRetention ago on the database's clock, and answers how many it deleted. It is
// the outbox's, in the expiry pass that records the rows (internal/dispatch/outbox,
// pruneKeptDeliveryExpiries), and it is one statement, fenced in its own SQL on the duty hold the
// outbox runs under, as the outbox's own statements are (expiryRecordQuery): it deletes nothing
// unless duty_holds names holder live for hold on the database's clock. The table holds a row only
// for each copy whose end was recorded inside the bound, so the statement reads all of it rather
// than an index of recorded_at.
func PruneKeptDeliveryExpiries(ctx context.Context, pool *Pool, hold string, holder uuid.UUID) (int64, error) {
	tag, err := pool.Exec(ctx, `
		delete from kept_delivery_expiries
		where recorded_at < now() - make_interval(secs => $1)
		  and exists (select 1 from duty_holds where name = $2 and holder = $3 and expires_at > now())
	`, KeptDeliveryExpiryRetention.Seconds(), hold, holder)
	return tag.RowsAffected(), err
}
