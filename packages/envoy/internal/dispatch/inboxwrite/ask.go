package inboxwrite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Ask is an ask as its answer writer reads it before the update that writes the answer: what the
// answer's write is recorded under (Answered), and what the ask's marker still owes the store
// (asks.tuples_stored_at, migration 20261116140001), which the answer's own update then covers.
type Ask struct {
	id                   string
	issueKey, artifactID *string
	project              string
	author               model.Actor
	createdAt            time.Time
	state                string
	answer               *model.AskAnswer
	// createOwed is a marker that is null: an ask an older build created, whose create stated no
	// participation and no citation. answerOwed is an answer the ask holds whose moment is
	// distinct from the marker: an older build's, whose writer stated none of its tuples.
	createOwed, answerOwed bool
	// waitingOn is the person an open ask waits on (asks.AskedOfPersonExpression).
	waitingOn string
	// node is who made the ask's node, nil where the store holds none: an item the store holds no
	// node of states no citation (refs' replaceRefsOf).
	node *model.Actor
	// opened is the ask's own create event, ask.opened, and answered the ask.answered event of the
	// older answer it holds, each graphstore.NoEvent where it owes nothing or the log holds none.
	opened, answered int64
}

// askEventTypes is the type list of the partial index events_ask_payload_id (migration 0030),
// which a read of an ask's events by payload->>'id' repeats beside the one type it reads, so the
// planner can prove the index's predicate and read it (0134's lookup takes it the same way).
const askEventTypes = `'ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited'`

// ReadAsk reads askID as an answer writer must before its answer's update moves the ask's marker
// (decision 16): the marker test of the ask's create and of the answer it holds, read off the
// ask's own row, which passes the test while the marker is null; the person an open ask waits on,
// over the issue, document and project rows that rule reads, which carry no marker of this
// child's; the ask's node; and the event each owed write was recorded under. It is one statement,
// made in the answer's read phase and taking no row lock: every answer writer holds the owner lock
// every writer of the ask takes first (store.LockOwner), the approval carry's included, under which
// nothing it reads moves before the answer is written, and the answer's own update takes the row
// in the write phase (asks.Settled.Queue).
func ReadAsk(ctx context.Context, tx pgx.Tx, askID string) (Ask, error) {
	ask := Ask{id: askID}
	var author, answer, nodeMadeBy []byte
	var opened, answered *int64
	if err := tx.QueryRow(ctx, `
		select a.issue_key, a.artifact_id::text, coalesce(i.project_key, ar.project_key, ''),
		       a.author, a.created_at, a.state, a.answer,
		       a.tuples_stored_at is null,
		       a.answer->>'user' is not null and a.tuples_stored_at is distinct from (a.answer->>'at')::timestamptz,
		       coalesce(`+asks.AskedOfPersonExpression+`, ''),
		       n.made_by,
		       case when a.tuples_stored_at is null then (
		           select e.id from events e
		           where e.type in (`+askEventTypes+`) and e.type = 'ask.opened' and e.payload->>'id' = a.id::text
		           order by e.id limit 1
		       ) end,
		       case when a.answer->>'user' is not null and a.tuples_stored_at is distinct from (a.answer->>'at')::timestamptz then (
		           select e.id from events e
		           where e.type in (`+askEventTypes+`) and e.type = 'ask.answered' and e.payload->>'id' = a.id::text
		             and (e.payload->'answer'->>'at')::timestamptz = (a.answer->>'at')::timestamptz
		           order by e.id limit 1
		       ) end
		from asks a`+asks.AskedOfPersonJoins+`
		left join graph_nodes n on n.kind = $2 and n.id = a.id::text
		where a.id = $1::uuid
	`, askID, graphstore.KindAsk).Scan(&ask.issueKey, &ask.artifactID, &ask.project,
		&author, &ask.createdAt, &ask.state, &answer, &ask.createOwed, &ask.answerOwed, &ask.waitingOn,
		&nodeMadeBy, &opened, &answered); err != nil {
		return Ask{}, fmt.Errorf("read ask %s before its answer: %w", askID, err)
	}
	if err := json.Unmarshal(author, &ask.author); err != nil {
		return Ask{}, fmt.Errorf("decode the author of ask %s: %w", askID, err)
	}
	if len(answer) > 0 {
		ask.answer = &model.AskAnswer{}
		if err := json.Unmarshal(answer, ask.answer); err != nil {
			return Ask{}, fmt.Errorf("decode the answer of ask %s: %w", askID, err)
		}
	}
	if len(nodeMadeBy) > 0 {
		ask.node = &model.Actor{}
		if err := json.Unmarshal(nodeMadeBy, ask.node); err != nil {
			return Ask{}, fmt.Errorf("decode who made the node of ask %s: %w", askID, err)
		}
	}
	if opened != nil {
		ask.opened = *opened
	}
	if answered != nil {
		ask.answered = *answered
	}
	return ask, nil
}

// owed is what an answer's catch-up appends (owedBy): the participation and the citations that
// name a person, which go through graphstore.ToPerson, those that name none, and the deliveries,
// which Record appends once it holds the thread's rows; cites is whether the ask's refs rows are
// marked beside its citations (queueMarkCitations).
type owed struct {
	people, others, deliveries []graphstore.Tuple
	cites                      bool
}

// owedBy is every tuple of ask that the marker test still owes (decision 16, P11), which the
// answer written to it appends before its own, because the update that writes the answer marks
// the ask and the backfill then passes it by. ask is nil for a write that answers nothing.
//
// Where the marker is null the ask's create owes its tuples, all under the ask's own create event,
// ask.opened, and dated at its created_at, as the backfill dates them: the asker's
// participates_in; for an ask that was open until this answer, the participates_in and the
// delivered_to of the person it waits on, as this build's create appends them (waitingOnTuples,
// which reaches no asker by their own ask); and the ask's citations, taken from the ask's own refs
// rows as they are stored rather than by indexing the question again - a fresh index resolves
// bare keys against the database as it stands (refs' Reader), and an answer rewrites no question
// - each row marked tuples_stored as its mentions tuple is stated (readCitations). An ask whose
// log holds no ask.opened still owes them: its row is the record that it was created, by whom and
// when, and the answer's update marks it, so a create left unstated here would be stated by
// nothing. Those tuples go under the store's sentinel (graphstore.NoEvent), the event the
// backfill states the same create under where the log holds none (askOpenedOf), so the two
// writers of one create agree on it whichever runs first. Where the ask holds an answer whose
// moment the marker does not hold, that older build's answer owes, under its own ask.answered
// event and at its own moment, its answerer's participates_in and one delivered_to to each person
// it reached (reachedBefore); an answer the log holds no ask.answered of is one the backfill
// states nothing of either, since it reads an older answer from its event alone, so neither
// writer states it. Each tuple carries the event of the write it belongs to, so the repeat key
// keeps it once beside the backfill's, which writes the same tuple under the same event.
func owedBy(ctx context.Context, tx pgx.Tx, ask *Ask) (owed, error) {
	var tuples owed
	if ask == nil {
		return tuples, nil
	}
	takesPart := func(kind, id string, at time.Time, who model.Actor, event int64) {
		tuple := graphstore.Tuple{
			FromKind: kind, FromID: id, Kind: graphstore.TupleParticipatesIn,
			ToKind: graphstore.KindAsk, ToID: ask.id,
			At: at, Who: who, SourceEvent: event, Asserted: true,
		}
		if kind == graphstore.KindPerson {
			tuples.people = append(tuples.people, tuple)
		} else {
			tuples.others = append(tuples.others, tuple)
		}
	}
	delivered := func(login string, at time.Time, who model.Actor, event int64) {
		tuples.deliveries = append(tuples.deliveries, graphstore.Tuple{
			FromKind: graphstore.KindAsk, FromID: ask.id, Kind: graphstore.TupleDeliveredTo,
			ToKind: graphstore.KindPerson, ToID: login,
			At: at, Who: who, SourceEvent: event, Asserted: true,
		})
	}
	if ask.createOwed {
		if kind, id, named := graphstore.AuthorOf(ask.author); named && graphstore.AddressFits(id) {
			takesPart(kind, id, ask.createdAt, ask.author, ask.opened)
		}
		if ask.state == asks.StateOpen {
			for _, tuple := range waitingOnTuples(ask.id, ask.waitingOn, personOf(ask.author), ask.createdAt, ask.author, ask.opened) {
				if tuple.Kind == graphstore.TupleDeliveredTo {
					tuples.deliveries = append(tuples.deliveries, tuple)
				} else {
					tuples.people = append(tuples.people, tuple)
				}
			}
		}
		if ask.node != nil {
			tuples.cites = true
			cited, err := readCitations(ctx, tx, ask.id, *ask.node, ask.opened)
			if err != nil {
				return owed{}, err
			}
			for _, tuple := range cited {
				if tuple.ToKind == graphstore.KindPerson {
					tuples.people = append(tuples.people, tuple)
				} else {
					tuples.others = append(tuples.others, tuple)
				}
			}
		}
	}
	if ask.answerOwed && ask.answer != nil && ask.answered != graphstore.NoEvent {
		answerer := model.CanonicalLogin(ask.answer.User)
		who := model.Actor{Kind: "user", ID: ask.answer.User}
		takesPart(graphstore.KindPerson, answerer, ask.answer.At, who, ask.answered)
		reached, err := reachedBefore(ctx, tx, thread{kind: graphstore.KindAsk, id: ask.id}, ask.answer.At, answerer)
		if err != nil {
			return owed{}, err
		}
		for _, login := range reached {
			delivered(login, ask.answer.At, who, ask.answered)
		}
	}
	return tuples, nil
}

// readCitations is the citations of ask askID that its refs rows hold and the store does not:
// each row whose tuples_stored is false - one an older build wrote - and whose far end fits the
// store's address bound, as the mentions tuple of its own kind out of the ask's node to its far
// end, dated by the row's created_at, carrying the row's source_seq, made by who made the ask's
// node, and caused by event, the ask's create (refs.PlanItemReplace states a citation the same
// way). It is read in the answer's read phase, under the owner lock every writer of the ask's refs
// rows takes first, so the rows it reads are the ones the write phase marks (queueMarkCitations).
// The tuples are the caller's to append, in this transaction.
func readCitations(ctx context.Context, tx pgx.Tx, askID string, maker model.Actor, event int64) ([]graphstore.Tuple, error) {
	rows, err := tx.Query(ctx, `
		select kind, to_kind, to_id, created_at, source_seq from refs
		where from_kind = $1 and from_id = $2 and not tuples_stored and octet_length(to_id) <= $3
	`, graphstore.KindAsk, askID, graphstore.MaxAddressBytes)
	if err != nil {
		return nil, fmt.Errorf("read the citations of ask %s: %w", askID, err)
	}
	defer rows.Close()
	var cited []graphstore.Tuple
	for rows.Next() {
		tuple := graphstore.Tuple{FromKind: graphstore.KindAsk, FromID: askID, Who: maker, SourceEvent: event, Asserted: true}
		if err := rows.Scan(&tuple.Kind, &tuple.ToKind, &tuple.ToID, &tuple.At, &tuple.LegacySourceSeq); err != nil {
			return nil, fmt.Errorf("read a citation of ask %s: %w", askID, err)
		}
		cited = append(cited, tuple)
	}
	return cited, rows.Err()
}

// queueMarkCitations queues the mark of each row readCitations read: tuples_stored true, so
// graph_edges answers it from the tuple the write states beside it, rather than from the refs arm,
// which reads only the rows that hold false.
func queueMarkCitations(b *store.WriteBatch, askID string) {
	b.Queue(fmt.Sprintf("state the citations of ask %s", askID), `
		update refs set tuples_stored = true
		where from_kind = $1 and from_id = $2 and not tuples_stored and octet_length(to_id) <= $3
	`, graphstore.KindAsk, askID, graphstore.MaxAddressBytes)
}
