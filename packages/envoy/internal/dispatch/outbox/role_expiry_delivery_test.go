package outbox

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// keptDeliveryKind is one kind of post whose delivery Dispatch's API publishes for a role's first
// holder (publishForRole, internal/dispatch/api): the post's kind as the copy's source event id
// names it (listenerwire.DispatchPost), and what an attempt's session_id holds while it names no
// session - null on comment_deliveries, ” on message_deliveries.
type keptDeliveryKind struct {
	post     string
	unpinned string
}

// keptDeliveryKinds is both kinds of post a role publish delivers.
var keptDeliveryKinds = []keptDeliveryKind{
	{post: listenerwire.DispatchPostComment, unpinned: sqlNull},
	{post: listenerwire.DispatchPostMessage, unpinned: ""},
}

// sqlNull is how a roleAttempt spells a column that holds SQL null.
const sqlNull = "<null>"

// columnValue is value as a statement's argument: SQL null for sqlNull.
func columnValue(value string) *string {
	if value == sqlNull {
		return nil
	}
	return &value
}

// columnText is a nullable column as a roleAttempt spells it: sqlNull for SQL null.
func columnText(value *string) string {
	if value == nil {
		return sqlNull
	}
	return *value
}

// roleAttempt is one delivery attempt of a post to role:<role>, as a test seeds it and reads it
// back: its number, its mode, the session it names, the envelope it records, its state and its
// error, each nullable column sqlNull where it holds SQL null.
type roleAttempt struct {
	attempt  int
	delivery string
	session  string
	envelope string
	state    string
	failure  string
}

// expiredAttempt is attempt as settleKeptDelivery leaves it for a notice whose copy's forward may
// not have reached a holder: failed with model.KeptDeliveryExpiredError(false) and its envelope
// cleared, the rest as it stood.
func expiredAttempt(attempt roleAttempt) roleAttempt {
	return expiredAttemptWith(attempt, model.KeptDeliveryExpiredError(false))
}

// expiredAttemptWith is attempt as settleKeptDelivery leaves it: failed with failure and its
// envelope cleared, the rest as it stood.
func expiredAttemptWith(attempt roleAttempt, failure string) roleAttempt {
	attempt.state, attempt.failure, attempt.envelope = model.DeliveryStateFailed, failure, sqlNull
	return attempt
}

// seedRolePost writes a post of kind on issue T-1 for role - a person's comment that mentions the
// role, or message to it - and answers it as the copy of its delivery names it.
func seedRolePost(t *testing.T, database *store.Store, kind, role string) listenerwire.DispatchPost {
	t.Helper()
	if kind == listenerwire.DispatchPostComment {
		return listenerwire.DispatchPost{Kind: kind, ID: seedComment(t, database, "T-1", model.Actor{Kind: "user", ID: "alice"}, "For the role.", nil)}
	}
	var id string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into messages (issue_key, author, body, target) values ('T-1', '{"kind":"user","id":"alice"}', 'For the role.', $1)
		returning id::text
	`, "role:"+role).Scan(&id); err != nil {
		t.Fatalf("create the message to role %s: %v", role, err)
	}
	return listenerwire.DispatchPost{Kind: kind, ID: id}
}

// deliveryTable is the table post's attempts are rows of, and the column that names the post in
// it, which is also the key its receipts' payloads name the post under.
func deliveryTable(post listenerwire.DispatchPost) (table, column string) {
	if post.Kind == listenerwire.DispatchPostMessage {
		return "message_deliveries", "message_id"
	}
	return "comment_deliveries", "comment_id"
}

// receiptTypeOf is the type of the receipt each attempt of post gets.
func receiptTypeOf(post listenerwire.DispatchPost) string {
	if post.Kind == listenerwire.DispatchPostMessage {
		return "message.delivery"
	}
	return "comment.delivery"
}

// seedRoleAttempts records attempts of post's delivery to role:<role> as the API leaves each once
// its send settled, a comment's beside the mention row its attempts are opened under.
func seedRoleAttempts(t *testing.T, database *store.Store, post listenerwire.DispatchPost, role string, attempts ...roleAttempt) {
	t.Helper()
	ctx := context.Background()
	target := "role:" + role
	for _, attempt := range attempts {
		var err error
		if post.Kind == listenerwire.DispatchPostComment {
			if _, mentionErr := database.Pool.Exec(ctx, `
				insert into comment_mentions (comment_id, target, delivery) values ($1, $2, $3)
				on conflict (comment_id, target) do nothing
			`, post.ID, target, attempt.delivery); mentionErr != nil {
				t.Fatalf("seed comment %s's mention of %s: %v", post.ID, target, mentionErr)
			}
			_, err = database.Pool.Exec(ctx, `
				insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, envelope_id, state, error)
				values ($1, $2, $3, $4, $5, $6, $7, $8)
			`, post.ID, target, attempt.attempt, attempt.delivery, columnValue(attempt.session), columnValue(attempt.envelope),
				attempt.state, columnValue(attempt.failure))
		} else {
			_, err = database.Pool.Exec(ctx, `
				insert into message_deliveries (message_id, attempt, delivery, session_id, envelope_id, state, error, requested_by)
				values ($1, $2, $3, $4, $5, $6, $7, '{"kind":"user","id":"alice"}')
			`, post.ID, attempt.attempt, attempt.delivery, columnValue(attempt.session), columnValue(attempt.envelope),
				attempt.state, columnValue(attempt.failure))
		}
		if err != nil {
			t.Fatalf("seed %s %s's attempt %+v: %v", post.Kind, post.ID, attempt, err)
		}
	}
}

// roleAttempts is every attempt of post's delivery, by attempt.
func roleAttempts(t *testing.T, database *store.Store, post listenerwire.DispatchPost) []roleAttempt {
	t.Helper()
	table, column := deliveryTable(post)
	rows, err := database.Pool.Query(context.Background(), `
		select attempt, delivery, session_id, envelope_id, state, error from `+table+` where `+column+` = $1 order by attempt
	`, post.ID)
	if err != nil {
		t.Fatalf("read %s %s's attempts: %v", post.Kind, post.ID, err)
	}
	defer rows.Close()
	var found []roleAttempt
	for rows.Next() {
		var attempt roleAttempt
		var session, envelope, failure *string
		if err := rows.Scan(&attempt.attempt, &attempt.delivery, &session, &envelope, &attempt.state, &failure); err != nil {
			t.Fatalf("scan an attempt of %s %s: %v", post.Kind, post.ID, err)
		}
		attempt.session, attempt.envelope, attempt.failure = columnText(session), columnText(envelope), columnText(failure)
		found = append(found, attempt)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s %s's attempts: %v", post.Kind, post.ID, err)
	}
	return found
}

// keepDelivery stores on role's kept lane the copy of Dispatch's delivery of post that the
// listener's publish route keeps for the role's first holder (cmd/listener/api.go, the publish
// route, and bus.KeptCopyDispatchDelivery): source dispatch, no role mark, a dedupe key under the route's
// publish. prefix, and post written into its source_event_id (bus.DispatchPostEventID). It answers
// the copy as the stream holds it, whose envelope's event id is the envelope_id publishForRole
// records on every attempt the copy carries.
func keepDelivery(t *testing.T, rig *roleRig, role string, post listenerwire.DispatchPost) bus.StoredMessage {
	t.Helper()
	item := contracts.MessageEnvelope(contracts.Message{Source: "dispatch", Text: "For the role's first holder."},
		contracts.RoleTopicPrefix+role, "publish."+uuid.NewString())
	item.SourceEventID = bus.DispatchPostEventID(post)
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := rig.client.JS().Publish(bus.KeptRoleSubject(role), data, natsgo.MsgId(bus.KeptMsgID(item.DedupeKey, role)))
	if err != nil {
		t.Fatalf("keep the copy of %s %s's delivery on role %s's kept lane: %v", post.Kind, post.ID, role, err)
	}
	stored, err := rig.client.NextStoredMessage(context.Background(), bus.KeptRoleSubject(role), ack.Sequence)
	if err != nil || stored.Sequence != ack.Sequence {
		t.Fatalf("read the kept copy at %d back: sequence %d, %v", ack.Sequence, stored.Sequence, err)
	}
	return stored
}

// deliveryReceipts is every receipt of post's attempts the event log holds, oldest first, each
// with its payload decoded as its type's own.
func deliveryReceipts(t *testing.T, database *store.Store, post listenerwire.DispatchPost) []model.Event {
	t.Helper()
	_, column := deliveryTable(post)
	rows, err := database.Pool.Query(context.Background(), `
		select id, issue_key, actor, payload, notify from events where type = $1 and payload ->> $2::text = $3 order by id
	`, receiptTypeOf(post), column, post.ID)
	if err != nil {
		t.Fatalf("read %s %s's receipts: %v", post.Kind, post.ID, err)
	}
	defer rows.Close()
	var found []model.Event
	for rows.Next() {
		event := model.Event{Type: receiptTypeOf(post)}
		var actor, payload []byte
		if err := rows.Scan(&event.ID, &event.IssueKey, &actor, &payload, &event.Notify); err != nil {
			t.Fatalf("scan a receipt of %s %s: %v", post.Kind, post.ID, err)
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			t.Fatalf("decode the actor of receipt %d: %v", event.ID, err)
		}
		var decodeErr error
		if post.Kind == listenerwire.DispatchPostMessage {
			var receipt model.MessageDeliveryEventPayload
			decodeErr = json.Unmarshal(payload, &receipt)
			event.Payload = receipt
		} else {
			var receipt model.CommentDeliveryEventPayload
			decodeErr = json.Unmarshal(payload, &receipt)
			event.Payload = receipt
		}
		if decodeErr != nil {
			t.Fatalf("decode the payload of receipt %d: %v", event.ID, decodeErr)
		}
		found = append(found, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s %s's receipts: %v", post.Kind, post.ID, err)
	}
	return found
}

// deliveryReceiptCount is how many delivery receipts of any post the event log holds.
func deliveryReceiptCount(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from events where type in ('comment.delivery', 'message.delivery')`).Scan(&count); err != nil {
		t.Fatalf("count the delivery receipts: %v", err)
	}
	return count
}

// expiredReceipt is the payload of the receipt settleKeptDelivery appends for attempt of post: the
// one the API writes for a failed attempt to role:<role> that names no session, with failure.
func expiredReceipt(post listenerwire.DispatchPost, role string, attempt roleAttempt, failure string) any {
	if post.Kind == listenerwire.DispatchPostMessage {
		return model.MessageDeliveryEventPayload{
			MessageID: post.ID, Attempt: attempt.attempt, Delivery: attempt.delivery, Target: "role:" + role,
			State: model.DeliveryStateFailed, Error: failure,
		}
	}
	return model.CommentDeliveryEventPayload{
		CommentID: post.ID, Target: "role:" + role, Attempt: attempt.attempt, Delivery: attempt.delivery,
		State: model.DeliveryStateFailed, Error: failure,
	}
}

// receiptAttempt is the attempt a delivery receipt reports.
func receiptAttempt(receipt model.Event) int {
	switch payload := receipt.Payload.(type) {
	case model.CommentDeliveryEventPayload:
		return payload.Attempt
	case model.MessageDeliveryEventPayload:
		return payload.Attempt
	}
	return 0
}

// checkExpiredReceipts fails t unless receipts are one for each attempt of settled, which runs in
// attempt order: each on issue T-1, the post's owner, written by the outbox's own system actor
// (graphWriter), notifying nobody, and reporting its attempt as settleKeptDelivery settled it,
// failed with failure (expiredReceipt).
func checkExpiredReceipts(t *testing.T, receipts []model.Event, post listenerwire.DispatchPost, role string, settled []roleAttempt, failure string) {
	t.Helper()
	if len(receipts) != len(settled) {
		t.Fatalf("%s %s has %d receipts %+v, want %d, one for each attempt settled", post.Kind, post.ID, len(receipts), receipts, len(settled))
	}
	receipts = slices.Clone(receipts)
	slices.SortFunc(receipts, func(a, b model.Event) int { return receiptAttempt(a) - receiptAttempt(b) })
	for i, receipt := range receipts {
		want := expiredReceipt(post, role, settled[i], failure)
		if columnText(receipt.IssueKey) != "T-1" || !reflect.DeepEqual(receipt.Actor, graphWriter) || receipt.Notify ||
			!reflect.DeepEqual(receipt.Payload, want) {
			t.Fatalf("receipt %d is on %s by %+v, notify %t, with %+v; want one on T-1 by %+v, notifying nobody, with %+v",
				receipt.ID, columnText(receipt.IssueKey), receipt.Actor, receipt.Notify, receipt.Payload, graphWriter, want)
		}
	}
}

// receiptIDs is the ids of receipts, in their order.
func receiptIDs(receipts []model.Event) []int64 {
	ids := make([]int64, 0, len(receipts))
	for _, receipt := range receipts {
		ids = append(ids, receipt.ID)
	}
	return ids
}

// publishedReceipts is the ids of the events of receiptType subscription was handed so far, in the
// order they were published. The expiry pass publishes what a record appended once its
// transaction has committed, before the pass returns (inHolderTransaction), so a read made after
// the pass misses none of them.
func publishedReceipts(subscription <-chan model.Event, receiptType string) []int64 {
	var ids []int64
	for {
		select {
		case event, open := <-subscription:
			if !open {
				return ids
			}
			if event.Type == receiptType {
				ids = append(ids, event.ID)
			}
		default:
			return ids
		}
	}
}

// checkKeptDeliverySettled fails t unless post's attempts read want and its receipts are one for
// each attempt of settled (checkExpiredReceipts), every one of them published to subscription,
// and no other receipt of their type, for a notice whose copy's forward may not have reached a
// holder.
func checkKeptDeliverySettled(t *testing.T, rig *roleRig, subscription <-chan model.Event, post listenerwire.DispatchPost, role string, want, settled []roleAttempt) {
	t.Helper()
	checkKeptDeliverySettledWith(t, rig, subscription, post, role, want, settled, model.KeptDeliveryExpiredError(false))
}

// checkKeptDeliverySettledWith is checkKeptDeliverySettled for receipts that say failure.
func checkKeptDeliverySettledWith(t *testing.T, rig *roleRig, subscription <-chan model.Event, post listenerwire.DispatchPost, role string, want, settled []roleAttempt, failure string) {
	t.Helper()
	if got := roleAttempts(t, rig.database, post); !slices.Equal(got, want) {
		t.Fatalf("%s %s's attempts are %+v, want %+v", post.Kind, post.ID, got, want)
	}
	receipts := deliveryReceipts(t, rig.database, post)
	checkExpiredReceipts(t, receipts, post, role, settled, failure)
	if published, appended := publishedReceipts(subscription, receiptTypeOf(post)), receiptIDs(receipts); !slices.Equal(published, appended) {
		t.Fatalf("the expiry pass published %s receipts %v, want the ones it appended, %v", receiptTypeOf(post), published, appended)
	}
}

// lastNotice is the stream sequence of the last expiry notice the stream holds on subject.
func lastNotice(t *testing.T, rig *roleRig, subject string) uint64 {
	t.Helper()
	stored, err := rig.client.LastStoredMessage(context.Background(), subject)
	if err != nil {
		t.Fatalf("read the last notice on %s back: %v", subject, err)
	}
	return stored.Sequence
}

// copyExpiryCount is how many outbox_role_copy_expiries rows the outbox recorded, of any event.
func copyExpiryCount(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from outbox_role_copy_expiries`).Scan(&count); err != nil {
		t.Fatalf("count the recorded copy expiries: %v", err)
	}
	return count
}

// passExpiries runs outbox's expiry pass and fails t when the pass reports a failure: every notice
// these tests store is one a pass records or passes over.
func passExpiries(t *testing.T, outbox *Outbox) {
	t.Helper()
	if _, failed := outbox.passKeptExpiries(context.Background(), unfenced{}); failed {
		t.Fatal("the expiry pass reported a failure, want every notice recorded or passed over")
	}
}

// A copy the listener's publish route kept of Dispatch's delivery of a post for a role's first
// holder, whose expiry notice says it ended with no session taking it, settles every attempt of
// the post that copy carried - queued under the copy's envelope, naming no session - failed with
// model.KeptDeliveryExpiredError and its envelope cleared, and appends for each the comment.delivery or
// message.delivery receipt the API writes for a failed attempt, on the post's owner, published
// once the record has committed. A second attempt that repeated the first's key rode the same
// copy, and is settled with a receipt of its own. The cursor moves past the notice, nothing is
// recorded in outbox_role_copy_expiries and nothing is kept again. With the delivery branch taken
// away, the notice's source event id is no number, it is passed over, and the attempts stay
// queued.
func TestAKeptDeliverysExpirySettlesEveryAttemptItsCopyCarried(t *testing.T) {
	for _, kind := range keptDeliveryKinds {
		for _, tc := range []struct {
			name     string
			attempts int
		}{
			{"one attempt", 1},
			{"a second attempt that repeated the key and rode the same copy", 2},
		} {
			t.Run(kind.post+", "+tc.name, func(t *testing.T) {
				rig := newRoleRig(t)
				role := "kept-delivery-" + kind.post
				rig.silent(t, contracts.RoleTopicPrefix+role)
				seedIssue(t, rig.database, "T-1", nil)
				post := seedRolePost(t, rig.database, kind.post, role)
				kept := keepDelivery(t, rig, role, post)
				var carried, settled []roleAttempt
				for attempt := 1; attempt <= tc.attempts; attempt++ {
					queued := roleAttempt{
						attempt: attempt, delivery: "aside", session: kind.unpinned, envelope: envelopeOf(t, kept).EventID,
						state: model.DeliveryStateQueued, failure: sqlNull,
					}
					carried, settled = append(carried, queued), append(settled, expiredAttempt(queued))
				}
				seedRoleAttempts(t, rig.database, post, role, carried...)
				outbox := holding(t, rig.deps())
				subscription, unsubscribe := rig.broker.Subscribe()
				defer unsubscribe()
				expire(t, rig.client, role, kept)
				notice := lastNotice(t, rig, bus.KeptExpiredSubject(role))

				passExpiries(t, outbox)

				checkKeptDeliverySettled(t, rig, subscription, post, role, settled, carried)
				if cursor := expiryCursor(t, rig.database); cursor != int64(notice) {
					t.Fatalf("the expiry cursor is %d, want it at the notice's sequence %d", cursor, notice)
				}
				if expiries := copyExpiryCount(t, rig.database); expiries != 0 {
					t.Fatalf("the outbox recorded %d copy expiries, want none for a delivery's copy", expiries)
				}
				if lane := keptLane(t, rig.client, role); len(lane) != 1 {
					t.Fatalf("role %s's kept lane holds %d messages, want the delivery's one copy and nothing kept again", role, len(lane))
				}
			})
		}
	}
}

// A kept delivery's notice settles only the attempts its copy carried that nobody answered. Beside
// the attempt it settles, four attempts of the same post are left as they are, with no receipt,
// each the settled one's in every column but the one that tells it apart: one already sent; one
// still queued with a session pinned, which a holder answered or took; one queued under another
// copy's envelope; and one already failed. The cursor moves past the notice. With any one of the
// update's guards taken away - the state, the session, the envelope - its attempt is settled too
// and given a receipt.
func TestAKeptDeliverysExpiryLeavesAttemptsItsCopyDidNotCarryAsTheyAre(t *testing.T) {
	for _, kind := range keptDeliveryKinds {
		t.Run(kind.post, func(t *testing.T) {
			rig := newRoleRig(t)
			role := "kept-delivery-answered-" + kind.post
			seedIssue(t, rig.database, "T-1", nil)
			post := seedRolePost(t, rig.database, kind.post, role)
			kept := keepDelivery(t, rig, role, post)
			envelope := envelopeOf(t, kept).EventID
			carried := roleAttempt{
				attempt: 1, delivery: "steer", session: kind.unpinned, envelope: envelope,
				state: model.DeliveryStateQueued, failure: sqlNull,
			}
			untouched := []roleAttempt{
				// Sent: the copy reached a holder.
				{attempt: 2, delivery: "steer", session: kind.unpinned, envelope: envelope, state: model.DeliveryStateSent, failure: sqlNull},
				// Queued with a session pinned: a holder answered it or took it.
				{attempt: 3, delivery: "steer", session: "ses_kept_holder", envelope: envelope, state: model.DeliveryStateQueued, failure: sqlNull},
				// Queued under the envelope of another copy, which this notice does not name.
				{attempt: 4, delivery: "steer", session: kind.unpinned, envelope: "another-copy", state: model.DeliveryStateQueued, failure: sqlNull},
				// Failed already.
				{attempt: 5, delivery: "steer", session: kind.unpinned, envelope: envelope, state: model.DeliveryStateFailed, failure: contracts.NoLiveSession},
			}
			seedRoleAttempts(t, rig.database, post, role, append([]roleAttempt{carried}, untouched...)...)
			outbox := holding(t, rig.deps())
			subscription, unsubscribe := rig.broker.Subscribe()
			defer unsubscribe()
			expire(t, rig.client, role, kept)
			notice := lastNotice(t, rig, bus.KeptExpiredSubject(role))

			passExpiries(t, outbox)

			checkKeptDeliverySettled(t, rig, subscription, post, role,
				append([]roleAttempt{expiredAttempt(carried)}, untouched...), []roleAttempt{carried})
			if cursor := expiryCursor(t, rig.database); cursor != int64(notice) {
				t.Fatalf("the expiry cursor is %d, want it at the notice's sequence %d", cursor, notice)
			}
		})
	}
}

// One kept delivery's notice read more than once settles the copy's attempt once, with one
// receipt: two stores of it that fell outside the stream's duplicate window, here under two
// message ids, read in one pass; a store under the copy's own message id (bus.KeptExpiryMsgID) by
// a walk that took the first one's claim over, which the stream already holds; and both notices
// read again from the stream's start once the cursor names a stream created again since
// (keptExpiryStart). Each later read finds the attempt failed with its envelope cleared, and
// settles nothing and appends nothing.
func TestAKeptDeliverysExpiryNoticeReadAgainSettlesNothingMore(t *testing.T) {
	for _, kind := range keptDeliveryKinds {
		t.Run(kind.post, func(t *testing.T) {
			ctx := context.Background()
			rig := newRoleRig(t)
			role := "kept-delivery-repeat-" + kind.post
			seedIssue(t, rig.database, "T-1", nil)
			post := seedRolePost(t, rig.database, kind.post, role)
			kept := keepDelivery(t, rig, role, post)
			carried := roleAttempt{
				attempt: 1, delivery: "aside", session: kind.unpinned, envelope: envelopeOf(t, kept).EventID,
				state: model.DeliveryStateQueued, failure: sqlNull,
			}
			seedRoleAttempts(t, rig.database, post, role, carried)
			outbox := holding(t, rig.deps())
			subscription, unsubscribe := rig.broker.Subscribe()
			defer unsubscribe()
			notice := expire(t, rig.client, role, kept)
			data, err := json.Marshal(notice)
			if err != nil {
				t.Fatal(err)
			}
			second, err := rig.client.JS().Publish(bus.KeptExpiredSubject(role), data, natsgo.MsgId("another listener's store"))
			if err != nil {
				t.Fatalf("store the same notice under a second message id: %v", err)
			}
			if notices := storedCopies(t, rig.client, bus.KeptExpiredSubject(role)); notices != 2 {
				t.Fatalf("the stream holds %d notices of the copy, want the two the test stored", notices)
			}

			passExpiries(t, outbox)
			expire(t, rig.client, role, kept)
			if notices := storedCopies(t, rig.client, bus.KeptExpiredSubject(role)); notices != 2 {
				t.Fatalf("the stream holds %d notices of the copy after the walk that took the claim over stored it, want still two", notices)
			}
			passExpiries(t, outbox)
			if _, err := rig.database.Pool.Exec(ctx, `update outbox_kept_expiry_cursor set stream_id = 'a-stream-before-this-one'`); err != nil {
				t.Fatalf("name another stream in the cursor: %v", err)
			}
			passExpiries(t, outbox)

			checkKeptDeliverySettled(t, rig, subscription, post, role, []roleAttempt{expiredAttempt(carried)}, []roleAttempt{carried})
			streamID, err := rig.client.StreamID(ctx)
			if err != nil {
				t.Fatalf("read the stream's identity: %v", err)
			}
			var cursor int64
			var cursorStream string
			if err := rig.database.Pool.QueryRow(ctx, `select sequence, stream_id from outbox_kept_expiry_cursor`).Scan(&cursor, &cursorStream); err != nil {
				t.Fatalf("read the cursor: %v", err)
			}
			if cursor != int64(second.Sequence) || cursorStream != streamID {
				t.Fatalf("the cursor is %d in stream %q, want %d in stream %q: both notices read again from this stream's start",
					cursor, cursorStream, second.Sequence, streamID)
			}
			if expiries := copyExpiryCount(t, rig.database); expiries != 0 {
				t.Fatalf("the outbox recorded %d copy expiries, want none for a delivery's copy", expiries)
			}
		})
	}
}

// A kept delivery's notice that names a post the store does not hold - a comment or a message no
// row has, under an id in the canonical form a post's takes - is passed over: the pass does not
// fail, the cursor moves past the notice, no receipt is appended, nothing is recorded, and an
// attempt of another post recorded under the copy's envelope id is left as it is. With the
// missing post read as a failure, the pass fails and the cursor stands before the notice, which
// every later pass reads again.
func TestAKeptDeliverysExpiryOfAPostTheStoreDoesNotHoldIsPassedOver(t *testing.T) {
	for _, kind := range keptDeliveryKinds {
		t.Run(kind.post, func(t *testing.T) {
			rig := newRoleRig(t)
			role := "kept-delivery-unheld-" + kind.post
			seedIssue(t, rig.database, "T-1", nil)
			held := seedRolePost(t, rig.database, kind.post, role)
			missing := listenerwire.DispatchPost{Kind: kind.post, ID: uuid.NewString()}
			kept := keepDelivery(t, rig, role, missing)
			other := roleAttempt{
				attempt: 1, delivery: "aside", session: kind.unpinned, envelope: envelopeOf(t, kept).EventID,
				state: model.DeliveryStateQueued, failure: sqlNull,
			}
			seedRoleAttempts(t, rig.database, held, role, other)
			outbox := holding(t, rig.deps())
			subscription, unsubscribe := rig.broker.Subscribe()
			defer unsubscribe()
			expire(t, rig.client, role, kept)
			notice := lastNotice(t, rig, bus.KeptExpiredSubject(role))

			passExpiries(t, outbox)

			checkKeptDeliverySettled(t, rig, subscription, held, role, []roleAttempt{other}, nil)
			if receipts := deliveryReceiptCount(t, rig.database); receipts != 0 {
				t.Fatalf("the event log holds %d delivery receipts, want none", receipts)
			}
			if cursor := expiryCursor(t, rig.database); cursor != int64(notice) {
				t.Fatalf("the expiry cursor is %d, want it at the notice's sequence %d", cursor, notice)
			}
			if expiries := copyExpiryCount(t, rig.database); expiries != 0 {
				t.Fatalf("the outbox recorded %d copy expiries, want none", expiries)
			}
		})
	}
}

// A kept delivery's notice is passed over by the checks it shares with an outbox copy's notice,
// with only the cursor moved and the attempt its copy carried left queued with no receipt and no
// end recorded: one whose role is no role name (roles.Valid), stored on that role's own expiry
// subject and naming the copy that role's kept lane holds; one stored on another role's expiry
// subject than its own role's; one naming a sequence where its role's kept lane holds the copy of
// another post's delivery (noticedCopy); and one whose dedupe key is the outbox's while its source
// event id names a post, a copy that is neither kind of copy Dispatch answers for
// (bus.ClassifyKeptCopy), which no reader may take for an outbox copy's and a delivery's alike.
// With any one of the checks taken away, its notice settles the attempt.
func TestAKeptDeliverysNoticeThatNamesNoCopyOfItIsPassedOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		role string
		// forge stores the notice of kept, the copy of the post's delivery on role's kept lane,
		// that the outbox must pass over, and answers the notice's stream sequence.
		forge func(t *testing.T, rig *roleRig, role string, kept bus.StoredMessage) uint64
	}{
		{"its role is no role name", "Not-A-Role", func(t *testing.T, rig *roleRig, role string, kept bus.StoredMessage) uint64 {
			expire(t, rig.client, role, kept)
			return lastNotice(t, rig, bus.KeptExpiredSubject(role))
		}},
		{"stored on another role's expiry subject", "kept-delivery-misfiled", func(t *testing.T, rig *roleRig, role string, kept bus.StoredMessage) uint64 {
			data, err := json.Marshal(expiryNoticeOf(t, role, kept))
			if err != nil {
				t.Fatal(err)
			}
			ack, err := rig.client.JS().Publish(bus.KeptExpiredSubject("kept-delivery-elsewhere"), data)
			if err != nil {
				t.Fatalf("store the notice on another role's expiry subject: %v", err)
			}
			return ack.Sequence
		}},
		{"its lane holds another post's copy at the sequence it names", "kept-delivery-other-copy", func(t *testing.T, rig *roleRig, role string, kept bus.StoredMessage) uint64 {
			other := keepDelivery(t, rig, role, seedRolePost(t, rig.database, listenerwire.DispatchPostComment, role))
			notice := expiryNoticeOf(t, role, kept)
			notice.Sequence = other.Sequence
			if err := rig.client.PublishKeptCopyExpiry(context.Background(), notice); err != nil {
				t.Fatalf("store the notice naming the other post's copy: %v", err)
			}
			return lastNotice(t, rig, bus.KeptExpiredSubject(role))
		}},
		{"its key is the outbox's while its source event id names a post", "kept-delivery-crossed", func(t *testing.T, rig *roleRig, role string, kept bus.StoredMessage) uint64 {
			item := envelopeOf(t, kept)
			item.DedupeKey = "dispatch-" + uuid.NewString()
			data, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			ack, err := rig.client.JS().Publish(bus.KeptRoleSubject(role), data, natsgo.MsgId(bus.KeptMsgID(item.DedupeKey, role)))
			if err != nil {
				t.Fatalf("keep the crossed copy on role %s's kept lane: %v", role, err)
			}
			crossed, err := rig.client.NextStoredMessage(context.Background(), bus.KeptRoleSubject(role), ack.Sequence)
			if err != nil || crossed.Sequence != ack.Sequence {
				t.Fatalf("read the crossed copy at %d back: sequence %d, %v", ack.Sequence, crossed.Sequence, err)
			}
			expire(t, rig.client, role, crossed)
			return lastNotice(t, rig, bus.KeptExpiredSubject(role))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRoleRig(t)
			seedIssue(t, rig.database, "T-1", nil)
			post := seedRolePost(t, rig.database, listenerwire.DispatchPostComment, tc.role)
			kept := keepDelivery(t, rig, tc.role, post)
			carried := roleAttempt{
				attempt: 1, delivery: "aside", session: sqlNull, envelope: envelopeOf(t, kept).EventID,
				state: model.DeliveryStateQueued, failure: sqlNull,
			}
			seedRoleAttempts(t, rig.database, post, tc.role, carried)
			outbox := holding(t, rig.deps())
			subscription, unsubscribe := rig.broker.Subscribe()
			defer unsubscribe()
			notice := tc.forge(t, rig, tc.role, kept)

			passExpiries(t, outbox)

			checkKeptDeliverySettled(t, rig, subscription, post, tc.role, []roleAttempt{carried}, nil)
			if cursor := expiryCursor(t, rig.database); cursor != int64(notice) {
				t.Fatalf("the expiry cursor is %d, want it at the notice's sequence %d", cursor, notice)
			}
			if expiries := copyExpiryCount(t, rig.database); expiries != 0 {
				t.Fatalf("the outbox recorded %d copy expiries, want none", expiries)
			}
			if ends := keptDeliveryExpiryRows(t, rig.database); len(ends) != 0 {
				t.Fatalf("the outbox recorded the ends %+v of copies, want none for a notice it passed over", ends)
			}
		})
	}
}

// keptDeliveryExpiryRow is one kept_delivery_expiries row as a test reads it: the copy's envelope
// id, the role it was kept for, the post it delivers by its kind and id, and whether its forward
// may have reached the role's holder.
type keptDeliveryExpiryRow struct {
	envelope string
	role     string
	kind     string
	post     string
	forward  bool
}

// keptDeliveryExpiryRows is every row the outbox recorded in kept_delivery_expiries.
func keptDeliveryExpiryRows(t *testing.T, database *store.Store) []keptDeliveryExpiryRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select envelope_id, role, post_kind, post_id::text, forward_unconfirmed from kept_delivery_expiries
		order by envelope_id, role, post_kind, post_id
	`)
	if err != nil {
		t.Fatalf("read the recorded ends of kept deliveries: %v", err)
	}
	defer rows.Close()
	var found []keptDeliveryExpiryRow
	for rows.Next() {
		var row keptDeliveryExpiryRow
		if err := rows.Scan(&row.envelope, &row.role, &row.kind, &row.post, &row.forward); err != nil {
			t.Fatalf("scan a recorded end of a kept delivery: %v", err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the recorded ends of kept deliveries: %v", err)
	}
	return found
}

// A kept delivery's notice says whether its copy's forward may have reached the role's holder
// (bus.KeptCopyExpiry.ForwardUnconfirmed), and the attempts it settles say so too, in their error
// and in their receipts, so a person who presses Retry knows a second copy may follow; a notice
// that says no such thing settles them with the plain error. Either way every attempt ends
// settled, and the end of the copy is recorded with the qualifier. With the qualifier taken away,
// the two rows' attempts read alike, and the second fails its error.
func TestAKeptDeliverysNoticeSaysWhetherItsForwardMayHaveReachedAHolder(t *testing.T) {
	if model.KeptDeliveryExpiredError(true) == model.KeptDeliveryExpiredError(false) {
		t.Fatalf("the error of a copy whose forward may have reached its holder reads as the plain one: %q", model.KeptDeliveryExpiredError(true))
	}
	for _, kind := range keptDeliveryKinds {
		for _, tc := range []struct {
			name    string
			forward bool
		}{
			{"the forward may have reached the holder", true},
			{"no forward was claimed", false},
		} {
			t.Run(kind.post+", "+tc.name, func(t *testing.T) {
				rig := newRoleRig(t)
				role := "kept-delivery-forward-" + kind.post
				seedIssue(t, rig.database, "T-1", nil)
				post := seedRolePost(t, rig.database, kind.post, role)
				kept := keepDelivery(t, rig, role, post)
				carried := roleAttempt{
					attempt: 1, delivery: "aside", session: kind.unpinned, envelope: envelopeOf(t, kept).EventID,
					state: model.DeliveryStateQueued, failure: sqlNull,
				}
				seedRoleAttempts(t, rig.database, post, role, carried)
				outbox := holding(t, rig.deps())
				subscription, unsubscribe := rig.broker.Subscribe()
				defer unsubscribe()
				notice := expiryNoticeOf(t, role, kept)
				notice.ForwardUnconfirmed = tc.forward
				if err := rig.client.PublishKeptCopyExpiry(context.Background(), notice); err != nil {
					t.Fatalf("store the expiry notice: %v", err)
				}

				passExpiries(t, outbox)

				failure := model.KeptDeliveryExpiredError(tc.forward)
				checkKeptDeliverySettledWith(t, rig, subscription, post, role,
					[]roleAttempt{expiredAttemptWith(carried, failure)}, []roleAttempt{carried}, failure)
				want := []keptDeliveryExpiryRow{{envelope: carried.envelope, role: role, kind: kind.post, post: post.ID, forward: tc.forward}}
				if got := keptDeliveryExpiryRows(t, rig.database); !slices.Equal(got, want) {
					t.Fatalf("the recorded ends of kept deliveries are %+v, want %+v", got, want)
				}
			})
		}
	}
}

// A kept delivery's notice read before its attempt is recorded queued settles nothing, since the
// attempt is still pending, and holds nothing back: it records the end of the copy, which the
// API's transaction reads when it records the attempt (store.ReadKeptDeliveryExpiry) and settles
// the attempt failed instead of queued, and the cursor moves past it, so the notice behind it,
// of another post's queued attempt, is settled in the same pass. The record names the role: a
// read for another role's attempt finds none. With the record taken away, the attempt the notice
// found pending is recorded queued afterwards and stays queued for good; with the cursor held at
// the first notice, the second is not settled.
func TestAKeptDeliverysNoticeReadBeforeItsAttemptIsQueuedIsRecordedAndHoldsNothingBack(t *testing.T) {
	for _, kind := range keptDeliveryKinds {
		t.Run(kind.post, func(t *testing.T) {
			rig := newRoleRig(t)
			role := "kept-delivery-early-" + kind.post
			seedIssue(t, rig.database, "T-1", nil)
			early := seedRolePost(t, rig.database, kind.post, role)
			earlyCopy := keepDelivery(t, rig, role, early)
			pending := roleAttempt{
				attempt: 1, delivery: "aside", session: kind.unpinned, envelope: sqlNull,
				state: model.DeliveryStatePending, failure: sqlNull,
			}
			seedRoleAttempts(t, rig.database, early, role, pending)
			later := seedRolePost(t, rig.database, kind.post, role)
			laterCopy := keepDelivery(t, rig, role, later)
			queued := roleAttempt{
				attempt: 1, delivery: "aside", session: kind.unpinned, envelope: envelopeOf(t, laterCopy).EventID,
				state: model.DeliveryStateQueued, failure: sqlNull,
			}
			seedRoleAttempts(t, rig.database, later, role, queued)
			outbox := holding(t, rig.deps())
			subscription, unsubscribe := rig.broker.Subscribe()
			defer unsubscribe()
			notice := expiryNoticeOf(t, role, earlyCopy)
			notice.ForwardUnconfirmed = true
			if err := rig.client.PublishKeptCopyExpiry(context.Background(), notice); err != nil {
				t.Fatalf("store the early notice: %v", err)
			}
			expire(t, rig.client, role, laterCopy)
			last := lastNotice(t, rig, bus.KeptExpiredSubject(role))

			passExpiries(t, outbox)

			if got := roleAttempts(t, rig.database, early); !slices.Equal(got, []roleAttempt{pending}) {
				t.Fatalf("the attempt still pending is %+v, want it as it was", got)
			}
			if receipts := deliveryReceipts(t, rig.database, early); len(receipts) != 0 {
				t.Fatalf("the attempt still pending has %d receipts %+v, want none", len(receipts), receipts)
			}
			checkKeptDeliverySettled(t, rig, subscription, later, role, []roleAttempt{expiredAttempt(queued)}, []roleAttempt{queued})
			if cursor := expiryCursor(t, rig.database); cursor != int64(last) {
				t.Fatalf("the expiry cursor is %d, want it at the last notice's sequence %d: the pending attempt held nothing back", cursor, last)
			}
			earlyEnvelope := envelopeOf(t, earlyCopy).EventID
			ends := map[string]keptDeliveryExpiryRow{}
			for _, end := range keptDeliveryExpiryRows(t, rig.database) {
				ends[end.envelope] = end
			}
			if len(ends) != 2 ||
				ends[earlyEnvelope] != (keptDeliveryExpiryRow{envelope: earlyEnvelope, role: role, kind: kind.post, post: early.ID, forward: true}) ||
				ends[queued.envelope] != (keptDeliveryExpiryRow{envelope: queued.envelope, role: role, kind: kind.post, post: later.ID, forward: false}) {
				t.Fatalf("the recorded ends of kept deliveries are %+v, want the early copy's with its forward unconfirmed and the later copy's", ends)
			}

			// What the API's settle reads under the post's owner lock.
			ctx := context.Background()
			tx, err := rig.database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the API's settle: %v", err)
			}
			defer tx.Rollback(ctx)
			ended := store.KeptDeliveryExpiry{EnvelopeID: earlyEnvelope, Role: role, PostKind: kind.post, PostID: early.ID}
			if forward, found, err := store.ReadKeptDeliveryExpiry(ctx, tx, ended); err != nil || !found || !forward {
				t.Fatalf("the settle reads the early copy's end as forward %t, found %t, %v; want found, with its forward unconfirmed", forward, found, err)
			}
			ended.Role = "kept-delivery-another-role"
			if forward, found, err := store.ReadKeptDeliveryExpiry(ctx, tx, ended); err != nil || found {
				t.Fatalf("the settle of another role's attempt reads the early copy's end as forward %t, found %t, %v; want none", forward, found, err)
			}
		})
	}
}

// A kept delivery's notice stored on one role's expiry subject settles no attempt of another role,
// whatever post and envelope it names: a forged frame an edge publisher stores on its own role's
// lane, carrying the post and the envelope id of another role's queued attempt, reads as a
// delivery's copy of that post, and its notice settles nothing and appends no receipt. The store
// holds no delivery of a message to the forging role, so it records no end either; a comment that
// mentions the forging role too is a delivery to it, whose end is recorded under the forging
// role's own name, which no read for the other role finds, and whose attempt, queued under a copy
// of its own, is not the envelope the notice names. The real notice of the other role is then
// recorded and settled as ever: the forged one blocked nothing. With the target check taken away,
// the real attempt is settled by the forged notice, and with the record keyed by the envelope
// alone, the real notice records nothing.
func TestAKeptDeliverysNoticeOfAnotherRoleSettlesNothingAndBlocksNothing(t *testing.T) {
	for _, kind := range keptDeliveryKinds {
		t.Run(kind.post, func(t *testing.T) {
			rig := newRoleRig(t)
			home, forger := "kept-delivery-project-a-"+kind.post, "kept-delivery-project-b-"+kind.post
			seedIssue(t, rig.database, "T-1", nil)
			post := seedRolePost(t, rig.database, kind.post, home)
			homeCopy := keepDelivery(t, rig, home, post)
			carried := roleAttempt{
				attempt: 1, delivery: "aside", session: kind.unpinned, envelope: envelopeOf(t, homeCopy).EventID,
				state: model.DeliveryStateQueued, failure: sqlNull,
			}
			seedRoleAttempts(t, rig.database, post, home, carried)
			forgedCopy := keepDelivery(t, rig, forger, post)
			standing := []roleAttempt{carried}
			var forgedEnds []keptDeliveryExpiryRow
			if kind.post == listenerwire.DispatchPostComment {
				mentioned := roleAttempt{
					attempt: 2, delivery: "aside", session: kind.unpinned, envelope: envelopeOf(t, forgedCopy).EventID,
					state: model.DeliveryStateQueued, failure: sqlNull,
				}
				seedRoleAttempts(t, rig.database, post, forger, mentioned)
				standing = append(standing, mentioned)
				forgedEnds = []keptDeliveryExpiryRow{{envelope: carried.envelope, role: forger, kind: kind.post, post: post.ID}}
			}
			outbox := holding(t, rig.deps())
			subscription, unsubscribe := rig.broker.Subscribe()
			defer unsubscribe()
			forged := expiryNoticeOf(t, forger, forgedCopy)
			forged.EventID = carried.envelope
			if err := rig.client.PublishKeptCopyExpiry(context.Background(), forged); err != nil {
				t.Fatalf("store the forged notice: %v", err)
			}
			forgery := lastNotice(t, rig, bus.KeptExpiredSubject(forger))

			passExpiries(t, outbox)

			if got := roleAttempts(t, rig.database, post); !slices.Equal(got, standing) {
				t.Fatalf("the attempts after the forged notice are %+v, want them as they were %+v", got, standing)
			}
			if receipts := deliveryReceiptCount(t, rig.database); receipts != 0 {
				t.Fatalf("the forged notice appended %d delivery receipts, want none", receipts)
			}
			if ends := keptDeliveryExpiryRows(t, rig.database); !slices.Equal(ends, forgedEnds) {
				t.Fatalf("the forged notice left the recorded ends %+v of copies, want %+v", ends, forgedEnds)
			}
			if cursor := expiryCursor(t, rig.database); cursor != int64(forgery) {
				t.Fatalf("the expiry cursor is %d, want it at the forged notice's sequence %d", cursor, forgery)
			}

			expire(t, rig.client, home, homeCopy)
			passExpiries(t, outbox)

			want := append([]roleAttempt{expiredAttempt(carried)}, standing[1:]...)
			checkKeptDeliverySettled(t, rig, subscription, post, home, want, []roleAttempt{carried})
			wantEnds := append([]keptDeliveryExpiryRow{{envelope: carried.envelope, role: home, kind: kind.post, post: post.ID}}, forgedEnds...)
			if ends := keptDeliveryExpiryRows(t, rig.database); !slices.Equal(ends, wantEnds) {
				t.Fatalf("the recorded ends of kept deliveries are %+v, want the real notice's and no other role's settled, %+v", ends, wantEnds)
			}
		})
	}
}
