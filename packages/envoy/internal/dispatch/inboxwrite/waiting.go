package inboxwrite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The assignee arm (the spec's R2 and R4): an open ask reaches the person it waits on, the person
// asks.AskedOfPersonExpression names - the person it was handed to, else its issue's assignee,
// else its project's default assignee - and that person takes part in its thread. The ask's
// create appends both tuples (Record, for an Opened write), and each write that changes who an
// ask waits on moves them (QueueMoveWaiting): patchIssue's assignee, putAskAskedOf,
// putProjectDefaultAssignee, the restore of the asks an issue's close withdrew and a settlement's
// restore of an ask it retracted. Neither raises a user_threads version: the old triggers moved
// no row for either, and the census counts only the deliveries a reply caused.
//
// A person never sees their own event (outbox's recipientFilter.keep), so a write that moves an
// ask onto the person who made it - who hands it to themselves, takes its issue, or makes
// themselves the default it falls through to - appends their participates_in and no delivered_to
// (waitingOnTuples). That participation is still the arm's, and no write of theirs in the thread:
// the next move away retracts it as it retracts anybody's (OwnMove, QueueMoveWaiting).
//
// The list follows the assignment only if no two of those writes interleave, so each serialises
// against the others on the locks and rows the rule reads, in the lock order written on
// api/inbox_read.go's markInboxReadThreads. An issue's assignee and an ask's asked_of are written
// under the issue's or the document's owner lock, which every create and every move of an ask on
// that owner takes first. A project's default is written under its project's owner lock, taken
// exclusive (putProjectDefaultAssignee), so a write that reads the default to decide a delivery
// takes that lock shared first (HoldDefault). And a move locks the asks rows it moves first in its
// write phase (QueueMoveWaiting), so two moves of one ask, and a move and an answer, take turns
// there, and the move reads who the ask waits on, and who its delivery stands to, only once it
// holds them, in the statement after that lock.

// waitingOnTuples is what an ask reaching the person it waits on appends, made by who at at,
// under event: participates_in from waiting to askID and delivered_to from askID to waiting. The
// asker - asker is their canonical login, "" for an ask a session or nobody asked - is reached by
// neither: their own ask draws no delivery to them, and they take part in it already as its asker
// (Record's step 1). The person who made the write, whom who names (personOf), draws no
// delivery either, since a person never sees their own event, and takes part, since the ask now
// waits on them (OwnMove). A create's writer is its asker, so only a move meets that second rule.
func waitingOnTuples(askID, waiting, asker string, at time.Time, who model.Actor, event int64) []graphstore.Tuple {
	waiting = model.CanonicalLogin(waiting)
	if waiting == "" || waiting == asker {
		return nil
	}
	tuples := make([]graphstore.Tuple, 0, 2)
	tuples = append(tuples, graphstore.Tuple{
		FromKind: graphstore.KindPerson, FromID: waiting, Kind: graphstore.TupleParticipatesIn,
		ToKind: graphstore.KindAsk, ToID: askID,
		At: at, Who: who, SourceEvent: event, Asserted: true,
	})
	if waiting == personOf(who) {
		return tuples
	}
	return append(tuples, deliveredTo(graphstore.KindAsk, askID, waiting, at, who, event))
}

// personOf is the canonical login of the person actor names, and "" for a session or nobody: the
// asker of an ask, read off its author, and the writer of a write, read off its event's actor.
func personOf(actor model.Actor) string {
	if kind, id, named := graphstore.AuthorOf(actor); named && kind == graphstore.KindPerson {
		return id
	}
	return ""
}

// OwnMove is true where person's participates_in in ask, stated under event, is the one
// waitingOnTuples states for a move person made of the ask onto themselves, beside no
// delivered_to: event is a write of that ask that decides who it waits on - its restore
// (ask.opened naming it; its create is that type too, and is made by its asker), a hand-on of it
// (ask.asked_of naming it), a change of its issue's assignee (issue.updated on that issue) or a
// move of its project's default assignee (settings.project_default_assignee.updated on that
// project) - made by person, who did not ask the ask, and the ask delivered person nothing under
// it. Such a participation is the assignee arm's and no write of person's in the thread:
// QueueMoveWaiting moves it as it moves the participation a delivery stands beside, and the census
// counts it among the participations that arm explains.
//
// Each argument is SQL text of the statement it is spliced into, and names none of the aliases
// the test reads under (own_event, own_ask, own_issue, own_document, own_delivery): person is a
// canonical login, ask an ask's id as text and event an event's id.
func OwnMove(person, ask, event string) string {
	return `(exists (
				select 1
				from events own_event
				join asks own_ask on own_ask.id = (case when pg_input_is_valid(` + ask + `, 'uuid') then ` + ask + ` end)::uuid
				left join issues own_issue on own_issue.key = own_ask.issue_key
				left join artifacts own_document on own_document.id = own_ask.artifact_id
				where own_event.id = ` + event + `
				  and case own_event.type
				      when 'ask.opened' then own_event.payload->>'id' = own_ask.id::text
				      when 'ask.asked_of' then own_event.payload->>'id' = own_ask.id::text
				      when 'issue.updated' then own_event.issue_key = own_ask.issue_key
				      when 'settings.project_default_assignee.updated'
				          then own_event.project_key = coalesce(own_issue.project_key, own_document.project_key)
				      else false end
				  and ` + graphstore.PersonOfSQL(`own_event.actor`) + ` = ` + person + `
				  and ` + graphstore.PersonOfSQL(`own_ask.author`) + ` is distinct from ` + person + `)
			and not ` + deliveredUnder(ask, person, event, `own_delivery`) + `)`
}

// deliveredUnder is whether a delivered_to from ask to person stands asserted under event, the
// alias the delivery is read under. Each argument is SQL text of the statement it is spliced
// into, event a column that is never null. It reads the relation repeat key, graph_tuples_once,
// one probe per call: that key is partial on `source_event <> 0`, which a bound event cannot
// prove, so an event other than the sentinel is read through it and the sentinel through a read
// of its own, which only a tuple the backfill stated with no event reaches. Read as one
// `source_event = event` instead, a move of an ask handed on two hundred times walked every
// delivery of the ask for each participation it read: 5.8 ms of each of the move's two statements
// on 2026-11-21, growing with the ask's history.
func deliveredUnder(ask, person, event, alias string) string {
	delivered := func(cause string) string {
		return `exists (
				select 1 from graph_tuples ` + alias + `
				where ` + alias + `.from_kind = '` + graphstore.KindAsk + `' and ` + alias + `.from_id = ` + ask + `
				  and ` + alias + `.kind = '` + graphstore.TupleDeliveredTo + `'
				  and ` + alias + `.to_kind = '` + graphstore.KindPerson + `' and ` + alias + `.to_id = ` + person + `
				  and ` + cause + ` and ` + alias + `.asserted)`
	}
	return `(` + delivered(alias+`.source_event = `+event+` and `+alias+`.source_event <> 0`) + `
			or (` + event + ` = 0 and ` + delivered(alias+`.source_event = 0`) + `))`
}

// HoldDefault takes the owner lock of the project an ask on the owner issueKey or artifactID falls
// through to, shared (store.LockOwnersShared), where it falls through to that project's default
// assignee: the owner is a project document, or an issue nobody holds. A write that is about to
// read who such an ask waits on, to deliver it - an ask's create, the restore of an issue's
// withdrawn asks, a settlement that may restore an ask it retracted - takes it in its read phase,
// after its owner lock and before the read, so a move of the default (putProjectDefaultAssignee,
// which takes that lock exclusive before it reads the default it moves) either commits before the
// write reads the default or waits for the write to commit and then finds the ask it delivered. A
// write into the thread of one open ask that falls through so - a comment or a callback reply in
// it, an edit there that names somebody new, an answer to it - takes the same lock before it reads
// who takes part in the thread, or appends a participation the move would read (HoldAskDefault,
// AskDefaultProject), so the move either committed first, and the write delivers to the default it
// left, or finds the write's own participation in the thread once it committed and leaves it.
// A shared lock conflicts with the exclusive one as `for share` of the project row conflicted with
// that route's `for no key update`, and not with another shared one, and it assigns no
// transaction id. The caller holds its owner lock, under which an issue's assignee does not move;
// an owner that does not fall through is read and nothing is locked.
func HoldDefault(ctx context.Context, tx pgx.Tx, issueKey, artifactID *string) error {
	var project string
	if err := tx.QueryRow(ctx, `
		select coalesce(i.project_key, ar.project_key, '')
		from (select 1) as one
		left join issues i on i.key = $1 and i.assignee is null
		left join artifacts ar on ar.id = $2::uuid and ar.issue_key is null
	`, issueKey, artifactID).Scan(&project); err != nil {
		return fmt.Errorf("read whether an ask falls through to its project's default: %w", err)
	}
	return HoldProjectDefault(ctx, tx, project)
}

// HoldProjectDefault is HoldDefault for a write that knows the project its asks fall through to
// as its own write phase leaves the owner - a change of an issue's assignee to nobody, whose update
// is queued and unread - and "" for one that falls through to none.
func HoldProjectDefault(ctx context.Context, tx pgx.Tx, project string) error {
	if project == "" {
		return nil
	}
	if err := store.LockOwnersShared(ctx, tx, store.ProjectOwner(project)); err != nil {
		return fmt.Errorf("hold the default assignee of project %s: %w", project, err)
	}
	return nil
}

// AskDefaultProject is SQL text over an asks row aliased a: the project whose default assignee the
// ask waits on and a move of that default reaches (DefaultMoved) - an open ask handed to nobody, on
// an issue nobody holds or on a project document, by the owner rule HoldDefault reads - and the
// empty string for an ask that waits on no default. A write into the ask's thread reads it under
// the owner lock, under which none of the rows it reads moves, beside a read of the ask it makes
// already where it has one (api's describeAsk), and hands it to HoldProjectDefault.
const AskDefaultProject = `coalesce(case when a.state = '` + asks.StateOpen + `' and a.asked_of is null then coalesce(
		(select i.project_key from issues i where i.key = a.issue_key and i.assignee is null),
		(select ar.project_key from artifacts ar where ar.id = a.artifact_id and ar.issue_key is null)) end, '')`

// HoldAskDefault is HoldDefault for a write into the thread of ask askID with no read of the ask
// of its own to read the project in (AskDefaultProject): it reads that project and holds it shared
// (HoldProjectDefault), and holds nothing for an ask a move of a default does not reach.
func HoldAskDefault(ctx context.Context, tx pgx.Tx, askID string) error {
	var project string
	if err := tx.QueryRow(ctx, `select `+AskDefaultProject+` from asks a where a.id = $1::uuid`, askID).Scan(&project); err != nil {
		return fmt.Errorf("read whether ask %s falls through to its project's default: %w", askID, err)
	}
	return HoldProjectDefault(ctx, tx, project)
}

// waitingOn is the person ask askID waits on while it is open (asks.AskedOfPersonExpression), and
// "" for an ask that is not open. A create runs it after its insert, holding its owner row and,
// where the ask falls through to its project's default, the project's row (HoldDefault), so
// nothing it reads moves before the create commits: this build's creates store an open ask, and a
// write that stores an ask already closed - the seed's - gives no person its tuples, as the
// backfill gives an older build's ask that person's tuples only while it is open. A create that
// read the person itself under those same locks hands them to Record instead (Write.WaitingOn),
// and this read is not made.
func waitingOn(ctx context.Context, tx pgx.Tx, askID string) (string, error) {
	var waiting string
	err := tx.QueryRow(ctx, `
		select coalesce(`+asks.AskedOfPersonExpression+`, '')
		from asks a`+asks.AskedOfPersonJoins+`
		where a.id = $1::uuid and a.state = $2
	`, askID, asks.StateOpen).Scan(&waiting)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the person ask %s waits on: %w", askID, err)
	}
	return waiting, nil
}

// A Move is a write that changes who open asks wait on, as QueueMoveWaiting reads it: a select of
// the ids of the asks it may move, and the moment and the actor of the event the move is recorded
// under. Each writer names its own (HandedOn, Reassigned, DefaultMoved, Restored).
type Move struct {
	// what names the write in an error.
	what string
	// candidates selects the id of each ask the write may move; args are its parameters, from $1.
	candidates string
	args       []any
	// at and who are the moment and the actor of the write's event, which every tuple of the move
	// is dated at and made by.
	at  time.Time
	who model.Actor
}

// HandedOn is putAskAskedOf's write, recorded under an event at at by who: ask askID.
func HandedOn(askID string, at time.Time, who model.Actor) Move {
	return Move{what: "the hand-on of ask " + askID, candidates: `select $1::uuid`, args: []any{askID}, at: at, who: who}
}

// Reassigned is patchIssue's change of the assignee of issueKey, recorded under an event at at by
// who: every open ask of the issue handed to nobody, which follows its issue.
func Reassigned(issueKey string, at time.Time, who model.Actor) Move {
	return Move{
		what: "the change of " + issueKey + "'s assignee",
		candidates: `
			select a.id from asks a
			where a.issue_key = $1 and a.state = '` + asks.StateOpen + `' and a.asked_of is null`,
		args: []any{issueKey},
		at:   at, who: who,
	}
}

// DefaultMoved is putProjectDefaultAssignee's move of project's default, recorded under an event
// at at by who: every open ask that falls through to that default, handed to nobody and on an
// issue of the project nobody holds or on a document of the project.
func DefaultMoved(project string, at time.Time, who model.Actor) Move {
	return Move{
		what: "the move of project " + project + "'s default assignee",
		candidates: `
			select a.id
			from issues i
			join asks a on a.issue_key = i.key
			where i.project_key = $1 and i.assignee is null and a.state = '` + asks.StateOpen + `' and a.asked_of is null
			union all
			select a.id
			from artifacts ar
			join asks a on a.artifact_id = ar.id
			where ar.project_key = $1 and a.issue_key is null and a.state = '` + asks.StateOpen + `' and a.asked_of is null`,
		args: []any{project},
		at:   at, who: who,
	}
}

// Restored is the restore of ask askID, which no move reached while it was closed and which is
// open again: an ask an issue's close withdrew (api's recordRestoredAsksTx), or an ask a
// settlement retracted whose block came back (docs' reconcileAskBlocks plans it, and
// settlementReconciliation.causedByEvent queues it), recorded under the ask.opened that records
// its restore, at at by who. It now reaches whoever it waits on as it is restored. A write that
// restores several asks queues the move of each right after that ask's own event.
func Restored(askID string, at time.Time, who model.Actor) Move {
	return Move{what: "the restore of ask " + askID, candidates: `select $1::uuid`, args: []any{askID}, at: at, who: who}
}

// Narrowed is m held to those of its candidate asks of which test holds, a predicate over the asks
// row named narrowed that binds no parameter of its own. Each of the move's two statements reads
// it again (moveLock, moveHeld), so the second reads it once the first holds the rows. It is
// graphcensus' move of who an ask waits on for a write an older build made, which no move of this
// release's writer reached: held to the asks whose create the store holds the tuples of already,
// and to those of them no later write of that build moves again (writeQueuedInboxEvents).
func (m Move) Narrowed(test string) Move {
	m.candidates = `select narrowed.id from asks narrowed where narrowed.id in (` + m.candidates + `) and (` + test + `)`
	return m
}

// standing is whether the relation of kind from fromKind/fromID to toKind/toID stands: its newest
// tuple asserts it (graphstore.CurrentTupleRead). Each end is SQL text of the statement it is
// spliced into, the kinds this package's own constants.
func standing(fromKind, fromID, kind, toKind, toID string) string {
	return `coalesce((` + graphstore.CurrentTupleRead(`t.kind`, `t.asserted`,
		`t.from_kind = '`+fromKind+`' and t.from_id = `+fromID+` and t.kind = '`+kind+`'
				and t.to_kind = '`+toKind+`' and t.to_id = `+toID) + `), false)`
}

// moveLock is QueueMoveWaiting's first statement over candidates, a select of ask ids: the open
// asks the move may move, locked `for no key update` in ascending id, the order every write
// locking several asks rows takes them in.
func moveLock(candidates string) string {
	return `
		select a.id::text from asks a
		where a.id in (` + candidates + `) and a.state = '` + asks.StateOpen + `'
		order by a.id
		for no key update of a`
}

// moveHeld is the common-table expressions of a move's statement over candidates, a select of
// ask ids its parameters bind from $1: held, each candidate ask still open on an open issue or on
// a project document, with who it waits on now (asks.AskedOfPersonExpression, canonical) and who
// asked it (graphstore.PersonOfSQL); and reached, each person the arm of such an ask stands to,
// with whether their participates_in stands and whether anything else holds them in the thread.
// Each is read as the statement runs, after the write phase's lock of the asks (moveLock) and
// after every write the batch queued before it - an ask the write itself reopens is read open - so
// nothing is read back in Go between the lock and the tuples.
//
// The arm stands to a person whose newest arm tuple asserts it and whom no mention in the ask
// holds. A person's arm tuples are the delivered_to tuples from the ask to them and the
// participates_in tuples that their own moves of the ask onto themselves stated (OwnMove). A
// delivered_to asserts the arm where it was appended beside that person's participates_in under
// one event, as only this arm appends the two; a participation an own move stated asserts it by
// itself; and the retraction of a delivered_to ends it. What else holds a person in the thread is
// a participates_in of theirs appended under an event that delivered them nothing from the ask and
// was no own move of theirs, or a mention of them in the thread (Record).
//
// held reads the candidates as one array, `a.id = any(array(candidates))`, and not as `a.id in
// (candidates)`: an array subquery is an initplan, whose value the planner does not see, so it
// estimates the asks held as many with the write's arguments as without them, and the plan it makes
// for the arguments of one execution (a custom plan) costs what the plan for none (the generic plan)
// costs, but for the initplan's own read of the candidates. pgx sends the statement as a prepared
// statement of its connection, and from its sixth execution Postgres keeps the generic plan only
// where that plan is estimated no dearer than the custom plans with their planning, which for this
// statement takes 3 to 6 ms. Read as a semi-join, a change of an issue's assignee had the generic
// plan estimate the open asks of an average issue - 19 where c-p99's corpus holds one issue of 201
// open asks, a plan of cost 2,420 - against the one its own issue holds, a plan of cost 212. Where a
// run's statistics left the generic plan that much dearer, Postgres planned the statement afresh on
// every execution, and patchIssue's p50 in c-p99 went from 3.5 ms to 6.5 ms: two runs in eight
// (2026-11-22).
func moveHeld(candidates string) string {
	return `
		held as (
			select a.id::text as ask, user_threads_login(` + asks.AskedOfPersonExpression + `) as waiting,
			       ` + graphstore.PersonOfSQL(`a.author`) + ` as asker
			from asks a` + asks.AskedOfPersonJoins + `
			where a.id = any(array(` + candidates + `)) and a.state = '` + asks.StateOpen + `'
			  and (a.issue_key is null or i.closed_at is null)
		),
		reached as (
			select h.ask, arm.person,
			       ` + standing(graphstore.KindPerson, `arm.person`, graphstore.TupleParticipatesIn, graphstore.KindAsk, `h.ask`) + ` as takes_part,
			       exists (
			           select 1 from graph_tuples p
			           where p.from_kind = '` + graphstore.KindPerson + `' and p.from_id = arm.person
			             and p.kind = '` + graphstore.TupleParticipatesIn + `'
			             and p.to_kind = '` + graphstore.KindAsk + `' and p.to_id = h.ask and p.asserted
			             and not ` + deliveredUnder(`h.ask`, `arm.person`, `p.source_event`, `o`) + `
			             and not ` + OwnMove(`arm.person`, `h.ask`, `p.source_event`) + `
			       ) as held
			from held h
			cross join lateral (
				select distinct on (arm_tuple.person) arm_tuple.person, arm_tuple.kind, arm_tuple.asserted, arm_tuple.source_event
				from (
					select d.to_id as person, d.kind, d.asserted, d.source_event, d.seq
					from graph_tuples d
					where d.from_kind = '` + graphstore.KindAsk + `' and d.from_id = h.ask
					  and d.kind = '` + graphstore.TupleDeliveredTo + `' and d.to_kind = '` + graphstore.KindPerson + `'
					union all
					select own.from_id, own.kind, own.asserted, own.source_event, own.seq
					from graph_tuples own
					where own.from_kind = '` + graphstore.KindPerson + `' and own.kind = '` + graphstore.TupleParticipatesIn + `'
					  and own.to_kind = '` + graphstore.KindAsk + `' and own.to_id = h.ask and own.asserted
					  and ` + OwnMove(`own.from_id`, `h.ask`, `own.source_event`) + `
				) arm_tuple
				order by arm_tuple.person, arm_tuple.seq desc
			) arm
			where arm.asserted
			  and (arm.kind = '` + graphstore.TupleParticipatesIn + `' or exists (
			      select 1 from graph_tuples p
			      where p.from_kind = '` + graphstore.KindPerson + `' and p.from_id = arm.person
			        and p.kind = '` + graphstore.TupleParticipatesIn + `'
			        and p.to_kind = '` + graphstore.KindAsk + `' and p.to_id = h.ask
			        and p.source_event = arm.source_event and p.asserted))
			  and not ` + standing(graphstore.KindAsk, `h.ask`, graphstore.TupleMentions, graphstore.KindPerson, `arm.person`) + `
		)`
}

// moveTuples is the common-table expression moved over moveHeld's two, answering
// graphstore.ToPersonRows: what the move appends, ask by ask in id order. For each other person
// the arm stands to, by login, the retraction of their delivered_to - for a person whom their own
// move reached, this ends a delivery that never stood: it is the record that the arm left them -
// and of their participates_in where it stands and nothing else holds them in the thread and they
// did not ask the ask; then, for an ask whose arm does not already stand to the person it waits
// on, that person's participates_in and, unless they made this write (the writer at writerParam),
// their delivered_to (waitingOnTuples).
func moveTuples(writerParam string) string {
	return `
		moved as (
			select row_number() over (order by stated.ask, stated.grp, stated.person, stated.step) as position,
			       stated.from_kind, stated.from_id, stated.kind, stated.to_kind, stated.to_id, stated.asserted
			from (
				select r.ask, 0 as grp, r.person, 0 as step,
				       '` + graphstore.KindAsk + `' as from_kind, r.ask as from_id, '` + graphstore.TupleDeliveredTo + `' as kind,
				       '` + graphstore.KindPerson + `' as to_kind, r.person as to_id, false as asserted
				from reached r join held h on h.ask = r.ask
				where r.person is distinct from h.waiting
				union all
				select r.ask, 0, r.person, 1,
				       '` + graphstore.KindPerson + `', r.person, '` + graphstore.TupleParticipatesIn + `',
				       '` + graphstore.KindAsk + `', r.ask, false
				from reached r join held h on h.ask = r.ask
				where r.person is distinct from h.waiting and r.takes_part and not r.held and r.person is distinct from h.asker
				union all
				select h.ask, 1, '', 0,
				       '` + graphstore.KindPerson + `', h.waiting, '` + graphstore.TupleParticipatesIn + `',
				       '` + graphstore.KindAsk + `', h.ask, true
				from held h
				where coalesce(h.waiting, '') <> '' and h.waiting is distinct from h.asker
				  and not exists (select 1 from reached r where r.ask = h.ask and r.person = h.waiting)
				union all
				select h.ask, 1, '', 1,
				       '` + graphstore.KindAsk + `', h.ask, '` + graphstore.TupleDeliveredTo + `',
				       '` + graphstore.KindPerson + `', h.waiting, true
				from held h
				where coalesce(h.waiting, '') <> '' and h.waiting is distinct from h.asker
				  and not exists (select 1 from reached r where r.ask = h.ask and r.person = h.waiting)
				  and h.waiting is distinct from nullif(` + writerParam + `::text, '')
			) stated
		)`
}

// QueueMoveWaiting queues the move of R2 for the write move names on a write phase's batch, right
// after the write's event, whose id every tuple carries as event (graphstore.LatestEvent for the
// event the batch appends right before, which nothing between them may follow with another): each
// ask it moves now reaches the person it waits on, and stops reaching whoever the arm stood to
// before. Every tuple is dated at the write's moment and made by its actor (Move.at, Move.who).
//
// It takes two statements and reads nothing back in Go between them: the lock of the asks the
// write may move, `for no key update` in ascending id (moveLock), so a second move of one of them,
// and an answer, waits for this write to commit, and this write waits for theirs; then, in a
// statement of its own and so as every write it waited for left them (moveHeld), the append of the
// tuples (moveTuples, graphstore.ToPersonArms), whose own query is the read that refuses a tuple a
// node reserved for its author forbids (graphstore.ReservedForAuthorRead), as ToPerson refuses
// one: the statement's data-modifying arms run to their end whatever that read answers, and its
// refusal ends the transaction, which commits nothing of the move. An ask on a closed issue is not
// read: a closed issue retracts nothing, and the record stays what the write stored until the
// issue's reopen restores the ask (Restored).
func QueueMoveWaiting(b *store.WriteBatch, move Move, event int64) error {
	writer := len(move.args) + 1
	b.Queue(fmt.Sprintf("lock the asks %s may move", move.what), moveLock(move.candidates), move.args...)
	args := append(slices.Clone(move.args), personOf(move.who))
	arms, params, err := graphstore.ToPersonArms(writer+1, "moved", move.at, move.who, event)
	if err != nil {
		return fmt.Errorf("move the person the asks wait on, for %s: %w", move.what, err)
	}
	b.Queue(fmt.Sprintf("move the person the asks wait on, for %s", move.what),
		`with`+moveHeld(move.candidates)+`,`+moveTuples("$"+strconv.Itoa(writer))+`,
		`+arms+graphstore.ReservedForAuthorRead("moved"), append(args, params...)...).QueryRow(graphstore.RefuseReservedForAuthor)
	return nil
}
