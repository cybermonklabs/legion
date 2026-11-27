package graphstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// KindDeliveryAttempt is the node kind of one attempt to reach one session or role: one row of
// comment_deliveries or message_deliveries, which is retry machinery keyed per target per
// attempt. The attempt is a node of its own, and not a value of the comment or message it
// carries, because its number, state, envelope and errors belong to no comment and to no person.
// A delivery is the TupleDeliveredTo tuple the attempt ends in, and never a node.
const KindDeliveryAttempt = "delivery_attempt"

// The relations an attempt states besides TupleDeliveredTo. TupleOwedTo points at the session or
// role the attempt is for while its send is undecided, and ends with a retraction once the attempt
// leaves pending; TupleAnsweredBy, the kind an ask's answer states too (ask.go), points at the
// reply the attempt drew. Neither is an arm of graph_edges, so neither is one of
// refs.TupleKinds().
const TupleOwedTo = "owed_to"

// The values of an attempt, as the kinds of the value tuples that state them: the store names of
// the attribute nodes migration 20261116102759 ties to the delivery_attempt supertag. Each is the
// attribute's own name but the attempt's state, which shares its name with a base attribute of
// every node and is stored as its type's name, `_` and its own name (pmdoc/schemanodes.StoreName):
// a value tuple of kind `state` out of the attempt would read as the node's own state.
const (
	AttemptNumber       = "attempt"
	AttemptState        = "delivery_attempt_state"
	AttemptEnvelopeID   = "envelope_id"
	AttemptError        = "error"
	AttemptResolveError = "resolve_error"
	AttemptDuplicate    = "duplicate"
	AttemptClaimedAt    = "claimed_at"
	AttemptAcceptedAt   = "accepted_at"
	AttemptAcceptedAs   = "accepted_as"
)

// attemptValues are the values an attempt row states, in the order its facts are appended, each
// with the SQL that reads it off the row (attempt_ends r, attemptMoved): the attempt's number as
// decimal, a flag as `true` or `false`, and a time as RFC 3339 in UTC to the microsecond Postgres
// keeps (attemptMoment). A value the row holds none of reads null, and is absent; a comment's
// attempt holds no acceptance and a message's no resolve error.
var attemptValues = []struct{ kind, value string }{
	{AttemptNumber, `r.attempt::text`},
	{AttemptState, `r.state`},
	{AttemptEnvelopeID, `r.envelope_id`},
	{AttemptError, `r.error`},
	{AttemptResolveError, `r.resolve_error`},
	{AttemptDuplicate, `r.duplicate::text`},
	{AttemptClaimedAt, attemptMoment(`r.claimed_at`)},
	{AttemptAcceptedAt, attemptMoment(`r.accepted_at`)},
	{AttemptAcceptedAs, `r.accepted_as`},
}

// attemptRelationKinds are the relations an attempt states, the other half of the kinds the read
// of what the store holds of an attempt is bounded to.
var attemptRelationKinds = []string{TupleOwedTo, TupleDeliveredTo, TupleAnsweredBy}

// attemptMoment renders a time column as Go's time.RFC3339Nano writes it in UTC: to the
// microsecond Postgres keeps, its trailing zeros trimmed and the point with them where nothing is
// left after it, so a value this statement states reads exactly as one an earlier build's Go
// writer stated, and an unmoved time is never stated again. A null time renders null.
func attemptMoment(column string) string {
	return `rtrim(rtrim(to_char(` + column + ` at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), '0'), '.') || 'Z'`
}

// AttemptKey names one attempt row by its table's primary key: (comment, target, attempt) of
// comment_deliveries, or (message, attempt) of message_deliveries. Exactly one of Comment and
// Message is set.
type AttemptKey struct {
	Comment, Target string
	Message         string
	Attempt         int
}

// CommentAttempt and MessageAttempt are the keys of a comment's attempt and a message's.
func CommentAttempt(comment, target string, attempt int) AttemptKey {
	return AttemptKey{Comment: comment, Target: target, Attempt: attempt}
}

func MessageAttempt(message string, attempt int) AttemptKey {
	return AttemptKey{Message: message, Attempt: attempt}
}

// ID is the address of the attempt's node: its row's primary key, spelled after the table it is
// a row of, `comment/<comment id>/<target>/<attempt>` or `message/<message id>/<attempt>`. The
// prefix keeps the two tables' keys apart, and a comment's target route (`role:<topic>`,
// `session:<id>`) holds no `/`, so no two rows spell one address.
func (k AttemptKey) ID() string {
	if k.Message != "" {
		return "message/" + k.Message + "/" + strconv.Itoa(k.Attempt)
	}
	return "comment/" + k.Comment + "/" + k.Target + "/" + strconv.Itoa(k.Attempt)
}

// AttemptWriter is who an attempt's facts are written by where the transaction that writes them
// appends no event: the delivery machinery itself. A claim, a release, a refresh and a renewal
// move the row on the server's own work and record no event, and what each records is how this
// server's sending went, which is nobody's act, as the outbox's record of what reached a session
// is its own (outbox.graphWriter); so does a Retry's or a sender's opening of an attempt. A write
// that appends an event beside the row - a comment's mentions, a targeted message, a broadcast's
// copies, a settle and its receipt, a reply, an acceptance - states the facts as that event's
// actor instead, under that event (QueueAttempted).
var AttemptWriter = model.Actor{Kind: "system", ID: "dispatch-delivery"}

// AttemptOpened plans, in the read phase of the write that opens an attempt, the node of the
// attempt row that write is about to insert, and answers the node columns that insert binds:
// (node_kind, node_address), which the row's foreign key names, so the node goes in first. A
// target route carries no length bound, so an address past the store's bound (AddressFits) is no
// node: the plan holds nothing and both columns are nil, which leaves the row unmarked, as every
// other writer leaves a row whose address the store cannot key. It reads and writes nothing.
//
// The node is made by who, the actor of the write that opens the attempt, at at: the
// transaction's own time as the caller's read phase read it (`select now()`, which takes no
// transaction id), the now() the row's created_at defaults to, so the row and its node carry one
// time. It goes in at the sentinel and is answered held for its cause (Uncaused), as a message's
// node is. The caller queues its write before the row's insert (QueueNodes); once the insert and
// the event the opening write appends are queued, it moves the node onto that event
// (QueueCaused) and states what the row holds under it (QueueAttempted), both with NoEvent where
// the opening appends none - a Retry's claim opens an attempt and records no event.
func AttemptOpened(key AttemptKey, who model.Actor, at time.Time) (nodeKind, nodeAddress *string, opened Uncaused) {
	id := key.ID()
	if !AddressFits(id) {
		return nil, nil, Uncaused{}
	}
	kind := KindDeliveryAttempt
	return &kind, &id, PlanUncaused([]Node{agentNode(KindDeliveryAttempt, id, who, at, NoEvent)}, nil)
}

// AttemptNode is the node of the attempt key names as the write that opened it makes it
// (AttemptOpened), made by who at `at` and caused by event: the node the backfill writes for an
// attempt row an older build inserted, which names none, before it names it on that row.
func AttemptNode(key AttemptKey, who model.Actor, at time.Time, event int64) Node {
	return agentNode(KindDeliveryAttempt, key.ID(), who, at, event)
}

// The moment an attempt's facts are dated by, as the read of its rows spells it: the
// transaction's time, which a write of the row stamps the row with, or the row's own created_at,
// which the backfill of a row an older build wrote reads.
const (
	attemptNow     = `now()`
	attemptWritten = `d.created_at`
)

// QueueAttempted queues on b the statement that states each named attempt as its row stands where
// the batch reaches it, after the statements queued before it that wrote that row: its values as
// value tuples, `owed_to` its target while it is pending, `delivered_to` its session once the
// listener took it (model.DeliveryTaken), and `answered_by` its reply once one is recorded. The
// statement reads those rows, under the row locks the statements that wrote them hold, and what
// the store holds of each node, and appends only what moved against it (attemptMoved): a value
// the row changed is stated again, a value it cleared is retracted, an `owed_to` is retracted once
// the attempt leaves pending, and a `delivered_to` or `answered_by` the node already states is not
// stated twice - a queued attempt answered later appends its `answered_by` and no second delivery.
// So a write that moved nothing appends nothing. Every fact is dated at the transaction's time,
// written by who and caused by event: the event the write appends - LatestEvent for one the batch
// appends right before - or NoEvent for a transition that records none.
//
// All of it is derived in that one statement's SQL, so no read of the row runs in Go between the
// statement that wrote it and the one that states it, and a write phase that queues both holds its
// transaction id for its statements alone.
//
// A row that names no node is passed over: an older build wrote it during a roll, or its address
// is past the store's bound (AttemptOpened), and its old reading answers it until the backfill
// takes it once. The node is not written again: its row's foreign key holds it, and appending a
// tuple locks no row, so this adds no step to its caller's lock order. A writer that cannot be
// encoded fails Send rather than this call, which answers nothing (QueueWrite).
func QueueAttempted(b *store.WriteBatch, who model.Actor, event int64, keys ...AttemptKey) {
	moved, args := attemptMoved(keys, attemptNow)
	if moved == "" {
		return
	}
	whoParam, eventParam := "$"+strconv.Itoa(len(args)+1), "$"+strconv.Itoa(len(args)+2)
	b.QueueBuilt("state the delivery attempts", `
		with `+moved+`
		insert into graph_tuples (`+tupleWriteColumns+`)
		select '`+KindDeliveryAttempt+`', moved.id, moved.kind, moved.to_kind, moved.to_id, moved.at,
		       `+whoParam+`::jsonb, `+causeOf(eventParam+`::bigint`)+`, null::bigint, moved.asserted
		from attempt_moved moved
		order by `+attemptMovedOrder+`
		on conflict do nothing
	`, func() ([]any, error) {
		writer, err := json.Marshal(who.Recorded())
		if err != nil {
			return nil, fmt.Errorf("encode the writer of the delivery attempts: %w", err)
		}
		return append(args, writer, event), nil
	})
}

// AttemptFactsAsWritten is what each named attempt holds against what the store holds of its node
// now, for the backfill of an attempt row an older build wrote, whose node it has just written and
// named on the row: the facts QueueAttempted states, read in one statement of the same SQL
// (attemptMoved) rather than appended, dated at the row's own created_at rather than the
// transaction's time, written by who, at the sentinel for the caller to cause. It reads, and
// appends nothing: the backfill is a writer of its own transaction with no write phase, which
// causes each fact by the event of the attempt it is out of.
func AttemptFactsAsWritten(ctx context.Context, tx pgx.Tx, who model.Actor, keys ...AttemptKey) ([]Tuple, error) {
	moved, args := attemptMoved(keys, attemptWritten)
	if moved == "" {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		with `+moved+`
		select moved.id, moved.kind, moved.to_kind, moved.to_id, moved.at, moved.asserted
		from attempt_moved moved
		order by `+attemptMovedOrder, args...)
	if err != nil {
		return nil, fmt.Errorf("read what the delivery attempts hold: %w", err)
	}
	tuples, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Tuple, error) {
		tuple := Tuple{FromKind: KindDeliveryAttempt, Who: who, SourceEvent: NoEvent}
		err := row.Scan(&tuple.FromID, &tuple.Kind, &tuple.ToKind, &tuple.ToID, &tuple.At, &tuple.Asserted)
		return tuple, err
	})
	if err != nil {
		return nil, fmt.Errorf("read what the delivery attempts hold: %w", err)
	}
	return tuples, nil
}

// attemptMovedOrder is the order attemptMoved's facts are appended in: the relations of every
// attempt before the values of any, as every append of this store takes them (queueAppend), each
// attempt's in turn, its owed_to retractions first and its values in attemptValues' order.
const attemptMovedOrder = `moved.major, moved.id, moved.minor, moved.to_kind, moved.to_id`

// attemptMoved is the common-table expressions, for a statement's `with` list, that answer as
// attempt_moved (id, at, kind, to_kind, to_id, asserted, major, minor) the tuples that bring what
// the store holds of each named attempt's node to what its row says as the statement reads it,
// each dated at moment (attemptNow or attemptWritten), and the arguments they bind from $1; ""
// where keys names no attempt.
//
//   - attempt_rows reads the named rows that name their node, each named key once (a key named
//     twice is one attempt, stated once): a comment's attempt is owed to the route its row targets,
//     and a message's to the route its message targets (messages.target), the conversation every
//     attempt of it belongs to, its row naming the session it resolved, or the empty string before
//     it resolved one.
//   - attempt_ends reads the far end of each relation the row states now: owed_to the session or
//     the role its route names, as model.ParseRoute reads it, while the attempt is pending;
//     delivered_to the session it resolved once the listener took it; answered_by its reply. An
//     end past the store's address bound is none.
//   - held_values and held_relations read, in one pass over graph_tuples each, what the store
//     holds of every node attempt_rows names: the newest tuple of each value kind, which says
//     whether the node carries that value now and which, and each relation the node currently
//     asserts (CurrentTupleRead), both bounded to the kinds an attempt states. Each index they
//     walk is led by the node's own address.
//   - attempt_moved is what moved: a value the row carries that the node does not hold now
//     asserted, a value the node holds that the row carries no longer retracted, every owed_to
//     the node asserts but the row's retracted, and each relation of the row the node does not
//     assert yet asserted. A delivery that happened and a reply that was drawn stay stated: a
//     later settle of the row takes neither back.
//
// The attempt numbers are bound as they are, never narrowed in Go: pgx encodes each into the
// `int[]` the statement names and refuses one outside int4's range, so an attempt no row can hold
// fails the write instead of naming another attempt.
func attemptMoved(keys []AttemptKey, moment string) (string, []any) {
	var comments, targets, messages []string
	var commentAttempts, messageAttempts []int
	for _, key := range keys {
		if key.Message != "" {
			messages, messageAttempts = append(messages, key.Message), append(messageAttempts, key.Attempt)
			continue
		}
		comments, targets = append(comments, key.Comment), append(targets, key.Target)
		commentAttempts = append(commentAttempts, key.Attempt)
	}
	var args []any
	param := func(value any, cast string) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args)) + "::" + cast
	}
	var arms []string
	if len(comments) > 0 {
		arms = append(arms, `
			select d.node_address, `+moment+`, d.attempt, d.state, d.envelope_id, d.error,
			       d.resolve_error, d.duplicate, d.claimed_at, null::timestamptz,
			       null::text, d.target, d.session_id,
			       '`+KindComment+`'::text, d.reply_id::text
			from (select distinct * from unnest(`+param(comments, "uuid[]")+`, `+param(targets, "text[]")+`, `+param(commentAttempts, "int[]")+`))
				as named (comment_id, target, attempt)
			join comment_deliveries d
			  on d.comment_id = named.comment_id and d.target = named.target and d.attempt = named.attempt
			where d.node_kind = '`+KindDeliveryAttempt+`'`)
	}
	if len(messages) > 0 {
		arms = append(arms, `
			select d.node_address, `+moment+`, d.attempt, d.state, d.envelope_id, d.error,
			       null::text, d.duplicate, d.claimed_at, d.accepted_at,
			       d.accepted_as, coalesce(m.target, ''), nullif(d.session_id, ''),
			       '`+KindMessage+`'::text, d.reply_id::text
			from (select distinct * from unnest(`+param(messages, "uuid[]")+`, `+param(messageAttempts, "int[]")+`))
				as named (message_id, attempt)
			join message_deliveries d on d.message_id = named.message_id and d.attempt = named.attempt
			join messages m on m.id = d.message_id
			where d.node_kind = '`+KindDeliveryAttempt+`'`)
	}
	if len(arms) == 0 {
		return "", nil
	}
	pending := `r.state = '` + model.DeliveryStatePending + `'`
	role, session := RouteSQL(`r.route`)
	named := `t.from_kind = '` + KindDeliveryAttempt + `' and t.from_id = any(array(select id from attempt_rows))`
	return `attempt_rows (id, at, attempt, state, envelope_id, error, resolve_error, duplicate, claimed_at,
			accepted_at, accepted_as, route, resolved, reply_kind, reply) as (` + strings.Join(arms, `
			union all`) + `
		),
		attempt_ends as (
			select r.id, r.at, r.attempt, r.state, r.envelope_id, r.error, r.resolve_error, r.duplicate,
			       r.claimed_at, r.accepted_at, r.accepted_as, r.reply_kind, r.reply,
			       case when ` + pending + ` and ` + AddressFitsSQL(`route.role_id`) + ` then '` + KindRole + `'
			            when ` + pending + ` and ` + AddressFitsSQL(`route.session_id`) + ` then '` + KindSession + `' end as owed_kind,
			       case when ` + pending + ` and ` + AddressFitsSQL(`route.role_id`) + ` then route.role_id
			            when ` + pending + ` and ` + AddressFitsSQL(`route.session_id`) + ` then route.session_id end as owed_id,
			       case when r.state in ` + model.DeliveryTakenStatesSQL + ` and coalesce(r.resolved, '') <> ''
			             and ` + AddressFitsSQL(`r.resolved`) + ` then r.resolved end as delivered_id
			from attempt_rows r
			cross join lateral (
				select ` + role + ` as role_id,
				       ` + session + ` as session_id
			) route
		),
		held_values as (
			` + CurrentTupleRead(`t.from_id, t.kind`, `t.from_id, t.kind, t.to_id, t.asserted`,
		named+` and t.to_kind = '`+KindValue+`' and t.kind in (`+attemptValueKinds+`)`) + `
		),
		held_relations as (
			select current.from_id, current.kind, current.to_kind, current.to_id
			from (
				` + CurrentTupleRead(`t.from_id, t.kind, t.to_kind, t.to_id`, `t.from_id, t.kind, t.to_kind, t.to_id, t.asserted`,
		named+` and t.to_kind <> '`+KindValue+`' and t.kind in (`+attemptRelationKindList+`)`) + `
			) current
			where current.asserted
		),
		attempt_moved (id, at, kind, to_kind, to_id, asserted, major, minor) as (
			select r.id, r.at, carried.kind, '` + KindValue + `'::text, coalesce(carried.value, held.to_id),
			       carried.value is not null, 1, carried.ordinal
			from attempt_ends r
			cross join lateral (values ` + attemptCarried + `) as carried (ordinal, kind, value)
			left join held_values held on held.from_id = r.id and held.kind = carried.kind
			where (carried.value is not null and (held.asserted is not true or held.to_id <> carried.value))
			   or (carried.value is null and held.asserted)
			union all
			select r.id, r.at, held.kind, held.to_kind, held.to_id, false, 0, 0
			from attempt_ends r
			join held_relations held on held.from_id = r.id and held.kind = '` + TupleOwedTo + `'
			where (held.to_kind, held.to_id) is distinct from (r.owed_kind, r.owed_id)
			union all
			select r.id, r.at, stated.kind, stated.to_kind, stated.to_id, true, 0, stated.ordinal
			from attempt_ends r
			cross join lateral (values
				(1, '` + TupleOwedTo + `', r.owed_kind, r.owed_id),
				(2, '` + TupleDeliveredTo + `', '` + KindSession + `', r.delivered_id),
				(3, '` + TupleAnsweredBy + `', r.reply_kind, r.reply)
			) as stated (ordinal, kind, to_kind, to_id)
			where stated.to_id is not null and not exists (
				select 1 from held_relations held
				where held.from_id = r.id and held.kind = stated.kind
				  and held.to_kind = stated.to_kind and held.to_id = stated.to_id
			)
		)`, args
}

// attemptCarried, attemptValueKinds and attemptRelationKindList are attemptMoved's fixed lists,
// built once: attemptValues as the values list attempt_moved reads them from, each with its
// ordinal, and as the kinds held_values reads, and attemptRelationKinds as the kinds held_relations
// reads.
var attemptCarried, attemptValueKinds, attemptRelationKindList = func() (string, string, string) {
	carried := make([]string, len(attemptValues))
	valueKinds := make([]string, len(attemptValues))
	for index, value := range attemptValues {
		carried[index] = `(` + strconv.Itoa(index) + `, '` + value.kind + `', ` + value.value + `)`
		valueKinds[index] = `'` + value.kind + `'`
	}
	return strings.Join(carried, `, `), strings.Join(valueKinds, `, `), `'` + strings.Join(attemptRelationKinds, `', '`) + `'`
}()
