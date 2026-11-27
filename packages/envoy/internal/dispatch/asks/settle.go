package asks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/failpoint"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The three answer states an ask moves between, the values of its node's answer_state
// (graphstore.TupleAnswerState) and of asks.state, which projects it.
const (
	StateOpen     = "open"
	StateAnswered = "answered"
	StateResolved = "resolved"
)

// ErrStateMoved is a Settle whose Change names the state it answers from (Change.From) over an
// ask the row no longer holds in that state: another write decided it first.
var ErrStateMoved = errors.New("the ask is no longer in the state this write answers from")

// Change is one move of an ask's answer state, which Settle writes. State is the answer state it
// leaves; Answer and Resolution are what it leaves the ask holding, nil for none, unless
// KeepAnswer or KeepResolution says the change leaves that part as the ask holds it. Answered,
// Resolved, Reopened, Restored and Recorded build the five moves there are; the caller then names
// who made it and when, and how it is caused.
type Change struct {
	State          string
	Answer         *model.AskAnswer
	KeepAnswer     bool
	Resolution     *model.AskResolution
	KeepResolution bool
	// From, when set, is the answer state the row must hold for the change to be written; a row
	// in another fails the change with ErrStateMoved and writes nothing.
	From string
	// Who made the change and At when, which every tuple it appends carries.
	Who model.Actor
	At  time.Time
	// Event is the event that caused the change, which each tuple carries as its source_event:
	// an event's id, graphstore.LatestEvent for an event the write phase appends right before
	// the change's statements, or graphstore.NoEvent. A caller whose event is appended after the
	// change - every request whose event payload or thread logins read the row this writes -
	// sets Held instead: the change's planned facts are put there (Settled.Held), and the caller
	// hands them to graphstore.QueueCaused once it has queued the event, or to graphstore.Caused
	// once the event exists where nothing queues, in the same transaction.
	Event int64
	Held  *graphstore.Uncaused
	// TuplesStored says the caller states the answer this change writes in the node and tuple
	// store in the same transaction - who answered took part in the ask's thread, and the answer
	// reached everyone else in it (inboxwrite.Record) - so the update that writes the answer sets
	// asks.tuples_stored_at to the answer's own moment (migration 20261116140001). That is the
	// marker the trigger user_threads_ask reads (20261116160001): an answer whose moment the marker
	// holds is this release's and passes it by, and any other answer is an older build's, which
	// that trigger projects and the backfill takes. Only a change that writes an answer moves it.
	TuplesStored bool
}

// Answered is the change an answer makes: the ask is answered with answer, and keeps the
// resolution it holds, which an open ask holds none of.
func Answered(answer model.AskAnswer) Change {
	return Change{State: StateAnswered, Answer: &answer, KeepResolution: true}
}

// Resolved is the change a resolution makes - a retraction, a resolution, a decline or an issue's
// withdrawal: the ask is resolved with resolution and keeps the answer it holds.
func Resolved(resolution model.AskResolution) Change {
	return Change{State: StateResolved, Resolution: &resolution, KeepAnswer: true}
}

// Reopened is the change an issue's reopen makes: the ask is open, with no answer and no
// resolution.
func Reopened() Change {
	return Change{State: StateOpen}
}

// Restored is the change settlement makes when an ask's block comes back after settlement
// retracted it: the ask gets back state and answer, the ones it held before the retraction, and
// loses the retraction.
func Restored(state string, answer *model.AskAnswer) Change {
	return Change{State: state, Answer: answer}
}

// Recorded is the whole answer state an event or a row recorded - state, answer and resolution -
// as the catch-up of the boot step reads it off an event a process of another build appended
// (docs' decision step), and as the boot step's backfill reads it off an asks row written before
// this release (AnswerFacts).
func Recorded(state string, answer *model.AskAnswer, resolution *model.AskResolution) Change {
	return Change{State: state, Answer: answer, Resolution: resolution}
}

// Settle is the one writer of an ask's answer state. It appends to the ask's node what the change
// states - answer_state always, and the answer's words (answer), the options it selects
// (selected) and its time (answered_at) as values, the person who gave it as the far end of
// answered_by and decided_by, and the resolution as a value - and retracts each of those the node
// holds that the change takes away. It then writes the asks row from what it appended, in the same
// transaction: state, and answer and resolution where the change states them. The node is the
// record and the row its projection.
//
// The node holds the answer only once the row names it (asks.node_kind): a row a process of the
// release before wrote names none until the boot step's backfill states the whole row on the node
// and marks it (graphcensus' asks batch), so Settle writes such a row alone and leaves the node to
// that backfill, which reads the row this writes. A marked row's node is the one its foreign key
// names, so it always exists.
//
// Settle runs the write at once, for a transaction that queues no write phase: it locks the ask's
// row first, refuses a change whose From the row no longer holds (ErrStateMoved) before it writes
// anything, plans the change as PlanSettle does, and runs its statements (Settled.Queue) as a
// batch of their own. A transaction that queues its writes plans the change in its read phase
// (PlanSettle) and queues Settled.Queue where it makes the write instead. It answers the ask's id
// and its answer state - State, Answer and Resolution - as the row now holds them; the rest of
// the row is the caller's.
func Settle(ctx context.Context, tx pgx.Tx, askID string, change Change) (model.Ask, error) {
	held, err := readHeldAnswer(ctx, tx, askID, true)
	if err != nil {
		return model.Ask{}, err
	}
	if change.From != "" && held.state != change.From {
		return model.Ask{}, fmt.Errorf("settle ask %s from %s: it is %s: %w", askID, change.From, held.state, ErrStateMoved)
	}
	settled, err := planSettle(ctx, askID, change, held)
	if err != nil {
		return model.Ask{}, err
	}
	if err := store.RunWrite(ctx, tx, settled.Queue); err != nil {
		return model.Ask{}, err
	}
	return settled.Ask, nil
}

// RowQueryer is the pgx surface PlanSettle reads through: a pgx.Tx, or a pool.
type RowQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Settled is one change of an ask's answer state as PlanSettle planned it, decided before anything
// is written, which Queue writes.
type Settled struct {
	// Ask is the ask's id and its answer state - State, Answer and Resolution - as the row stands
	// once Queue's update has run; the rest of the row is the caller's.
	Ask model.Ask
	// Held is what the change states on the ask's node, planned for its cause
	// (graphstore.PlanUncaused), the zero write for a row that names no node. Queue writes its
	// nodes at the sentinel; its facts are appended under the change's cause by Queue itself where
	// the change names that cause (Change.Event), and where the change holds them for an event the
	// caller appends later (Change.Held), by the caller's graphstore.QueueCaused once that event is
	// queued, in the same write phase.
	Held       graphstore.Uncaused
	askID      string
	change     Change
	answer     any
	resolution any
	// leaves is the row and the node as the change leaves them once written, which a later change
	// of the same ask in the same transaction is planned from (PlanSettleAfter).
	leaves heldAnswer
	// fails is whether the write is the one a test armed a failure for between the node write of
	// the answer and the asks row (failpoint.AskAnswer), decided as it was planned.
	fails bool
}

// storesAnswer is whether the change writes an answer this release's writer states in the store,
// whose update sets asks.tuples_stored_at to the answer's own moment (Change.TuplesStored).
func (c Change) storesAnswer() bool {
	return c.TuplesStored && !c.KeepAnswer && c.Answer != nil
}

// PlanSettle plans change on the ask askID, as Settle writes it, for a transaction that queues
// its writes: it reads the ask's row and what its node holds of the answer, and decides the facts
// and the row the change leaves (Settled), writing nothing. It takes no row lock: the caller holds
// the owner lock every writer of the ask's owner takes first (store.LockOwner), under which no
// other transaction moves the row. What it reads is the stored row and node, so a transaction that
// has already queued a settle of the same ask plans the next one from that settle instead
// (PlanSettleAfter), and the state the change answers from (Change.From) is held to the row by
// Queue's update, which runs after every write queued before it. Where the change holds its facts
// for a later event (Change.Held), the planned write is put there (assigned, never added to
// another: each planned node write carries a mark of its own), and the caller hands it to
// graphstore.QueueCaused after that event.
func PlanSettle(ctx context.Context, q RowQueryer, askID string, change Change) (Settled, error) {
	held, err := readHeldAnswer(ctx, q, askID, false)
	if err != nil {
		return Settled{}, err
	}
	return planSettle(ctx, askID, change, held)
}

// failsBetween is whether a test armed a failure for this change between the node write of the
// answer and the asks row (failpoint.AskAnswer): only a change that writes an answer this
// release's writer states in the store has that point, so no other change disarms it.
func failsBetween(ctx context.Context, change Change) bool {
	return change.storesAnswer() && failpoint.Planned(ctx, failpoint.AskAnswer)
}

// PlanSettleAfter plans change on the ask earlier settled, as earlier leaves the row and the node
// once written, for a transaction that has queued earlier and not yet run it: the reopen's restore
// of an approval request its follow then answers, a settlement's retraction of an ask whose block
// returns. It reads nothing: what the second change keeps of the answer and the resolution, and
// what it retracts on the node, is what the first leaves, which no read of the tables answers
// before the write phase has run. It decides, as PlanSettle does, whether a test armed the failure
// between the node write of the answer and the asks row for it (failpoint.AskAnswer), so every
// answer path carries that point.
func PlanSettleAfter(ctx context.Context, earlier Settled, change Change) (Settled, error) {
	return planSettle(ctx, earlier.askID, change, earlier.leaves)
}

// planSettle is PlanSettle over held, the row and the node as they were read, or as an earlier
// change of this transaction leaves them. Once the change is planned it decides whether a test
// armed the failure for it (failsBetween), so a change whose plan fails disarms nothing.
func planSettle(ctx context.Context, askID string, change Change, held heldAnswer) (Settled, error) {
	settled := Settled{
		Ask:    model.Ask{ID: askID, State: change.State, Answer: change.Answer, Resolution: change.Resolution},
		askID:  askID,
		change: change,
	}
	if change.KeepAnswer {
		settled.Ask.Answer = held.answer
	}
	if change.KeepResolution {
		settled.Ask.Resolution = held.resolution
	}
	var tuples []graphstore.Tuple
	if held.marked {
		var nodes []graphstore.Node
		var err error
		if nodes, tuples, err = answerFacts(askID, change, held); err != nil {
			return Settled{}, err
		}
		settled.Held = graphstore.PlanUncaused(nodes, tuples)
	}
	settled.leaves = leftBy(held, settled.Ask, tuples)
	if change.Held != nil {
		*change.Held = settled.Held
	}
	var err error
	if settled.answer, err = encodeAnswer(change.Answer); err != nil {
		return Settled{}, err
	}
	if change.Resolution != nil {
		encoded, err := json.Marshal(change.Resolution)
		if err != nil {
			return Settled{}, fmt.Errorf("encode the resolution of ask %s: %w", askID, err)
		}
		settled.resolution = encoded
	}
	settled.fails = failsBetween(ctx, change)
	return settled, nil
}

// leftBy is held, the row and the node before a change, as the change leaves them: the row's
// state, answer and resolution as ask, the row the change writes, holds them, and the node's
// values and relations with each of tuples, the facts the change states, applied in order - an
// asserted value replacing the one its kind held and a retracted one taking it away, an asserted
// far end joining its relation and a retracted one leaving it.
func leftBy(held heldAnswer, ask model.Ask, tuples []graphstore.Tuple) heldAnswer {
	left := heldAnswer{
		marked: held.marked, state: ask.State, answer: ask.Answer, resolution: ask.Resolution,
		values:    make(map[string]string, len(held.values)),
		relations: make(map[string][]string, len(held.relations)),
	}
	maps.Copy(left.values, held.values)
	for kind, ends := range held.relations {
		left.relations[kind] = slices.Clone(ends)
	}
	for _, tuple := range tuples {
		if tuple.ToKind == graphstore.KindValue {
			if tuple.Asserted {
				left.values[tuple.Kind] = tuple.ToID
			} else {
				delete(left.values, tuple.Kind)
			}
			continue
		}
		ends := slices.DeleteFunc(left.relations[tuple.Kind], func(end string) bool { return end == tuple.ToID })
		if tuple.Asserted {
			ends = append(ends, tuple.ToID)
		}
		left.relations[tuple.Kind] = ends
	}
	return left
}

// Queue puts the change's statements on b (store.WriteBatch), in Settle's order: the node write
// of what it states on the ask's node, then, where the change names its cause rather than holding
// its facts for a later event, those facts under that cause, then the asks row. The row's update
// names the state the change answers from where it names one (Change.From), and a row no longer
// in it is refused with ErrStateMoved as the batch reads the update's result, which stops the
// write phase there.
func (s Settled) Queue(b *store.WriteBatch) error {
	graphstore.QueueNodes(b, s.Held)
	if s.change.Held == nil {
		graphstore.QueueCaused(b, s.Held, s.change.Event)
	}
	// The point a test injects a failure through between the node write of the answer and the
	// asks row (failpoint.AskAnswer), for an answer this release's writer writes: every such
	// writer holds the answer's tuples for its later event (Change.Held), so they come after it.
	failpoint.Queue(b, s.fails)
	b.Queue("write the answer state of ask "+s.askID, `
		update asks
		set state = $2,
		    answer = case when $3 then answer else $4::jsonb end,
		    resolution = case when $5 then resolution else $6::jsonb end,
		    tuples_stored_at = case when $8 then ($4::jsonb->>'at')::timestamptz else tuples_stored_at end
		where id = $1::uuid and ($7::text = '' or state = $7::text)
	`, s.askID, s.change.State, s.change.KeepAnswer, s.answer, s.change.KeepResolution, s.resolution, s.change.From,
		s.change.storesAnswer()).Exec(func(tag pgconn.CommandTag) error {
		if tag.RowsAffected() == 1 {
			return nil
		}
		if s.change.From != "" {
			return fmt.Errorf("settle ask %s from %s: %w", s.askID, s.change.From, ErrStateMoved)
		}
		return fmt.Errorf("settle ask %s: no such ask", s.askID)
	})
	return nil
}

// encodeAnswer is the asks.answer value of answer, nil for none. An answer that selects nothing
// stores an empty list, never null, as every answer has since that rule.
func encodeAnswer(answer *model.AskAnswer) (any, error) {
	if answer == nil {
		return nil, nil
	}
	stored := *answer
	if stored.Selected == nil {
		stored.Selected = []string{}
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode an ask's answer: %w", err)
	}
	return encoded, nil
}

// heldAnswer is what Settle reads before it writes: whether the asks row names its node, the
// state, the answer and the resolution the row holds, and what the node holds now of the answer -
// each value by its kind, and each far end of answered_by and decided_by.
type heldAnswer struct {
	marked     bool
	state      string
	answer     *model.AskAnswer
	resolution *model.AskResolution
	values     map[string]string
	relations  map[string][]string
}

// readHeldAnswer reads, in one statement, the ask's row - locked as an update of its answer
// state would lock it, where lock says so (Settle, which runs at once) - and what its node holds
// of the answer: the newest value of each value kind while it asserts, and every current far end
// of the two relations.
func readHeldAnswer(ctx context.Context, q RowQueryer, askID string, lock bool) (heldAnswer, error) {
	held := heldAnswer{values: map[string]string{}, relations: map[string][]string{}}
	var values, relations, storedAnswer, storedResolution []byte
	locking := ""
	if lock {
		locking = "for no key update of k"
	}
	err := q.QueryRow(ctx, `
		select k.node_kind is not null, k.state, k.answer, k.resolution,
		       coalesce((select jsonb_object_agg(v.kind, v.to_id) from (
		           select distinct on (t.kind) t.kind, t.to_id, t.asserted
		           from graph_tuples t
		           where t.from_kind = $2 and t.from_id = $1::uuid::text and t.to_kind = $3 and t.kind = any($4)
		           order by t.kind, t.seq desc) v
		         where v.asserted), '{}'),
		       coalesce((select jsonb_agg(jsonb_build_array(r.kind, r.to_id)) from (
		           `+graphstore.CurrentTupleRead(`t.kind, t.to_kind, t.to_id`, `t.kind, t.to_id, t.asserted`,
		`t.from_kind = $2 and t.from_id = $1::uuid::text and t.to_kind <> $3 and t.kind = any($5)`)+`) r
		         where r.asserted), '[]')
		from asks k
		where k.id = $1::uuid
		`+locking+`
	`, askID, graphstore.KindAsk, graphstore.KindValue, answerValueKinds[:], answerRelationKinds[:]).Scan(
		&held.marked, &held.state, &storedAnswer, &storedResolution, &values, &relations)
	if errors.Is(err, pgx.ErrNoRows) {
		return heldAnswer{}, fmt.Errorf("settle ask %s: no such ask", askID)
	}
	if err != nil {
		return heldAnswer{}, fmt.Errorf("read what ask %s holds of its answer: %w", askID, err)
	}
	if len(storedAnswer) > 0 {
		held.answer = &model.AskAnswer{}
		if err := json.Unmarshal(storedAnswer, held.answer); err != nil {
			return heldAnswer{}, fmt.Errorf("decode the answer of ask %s: %w", askID, err)
		}
	}
	if len(storedResolution) > 0 {
		held.resolution = &model.AskResolution{}
		if err := json.Unmarshal(storedResolution, held.resolution); err != nil {
			return heldAnswer{}, fmt.Errorf("decode the resolution of ask %s: %w", askID, err)
		}
	}
	if err := json.Unmarshal(values, &held.values); err != nil {
		return heldAnswer{}, fmt.Errorf("read the answer values of ask %s: %w", askID, err)
	}
	var pairs [][2]string
	if err := json.Unmarshal(relations, &pairs); err != nil {
		return heldAnswer{}, fmt.Errorf("read the answer relations of ask %s: %w", askID, err)
	}
	for _, pair := range pairs {
		held.relations[pair[0]] = append(held.relations[pair[0]], pair[1])
	}
	return held, nil
}

// answerValueKinds and answerRelationKinds are the kinds Settle states: the answer state, the
// answer's three values and the resolution, and the two relations to the person who answered.
var (
	answerValueKinds = [...]string{
		graphstore.TupleAnswerState, graphstore.TupleAnswer, graphstore.TupleSelected,
		graphstore.TupleAnsweredAt, graphstore.TupleResolution,
	}
	answerRelationKinds = [...]string{graphstore.TupleAnsweredBy, graphstore.TupleDecidedBy}
)

// AnswerFacts is what the node of an ask holds of the answer state an asks row records, as the
// boot step's backfill states it for a row written before this release: the answer state, the
// answer and its person where the row holds one, and the resolution where it holds one. The node
// of such a row holds none of them yet, so nothing is retracted. at is when the row's state was
// written and event the event that wrote it, or graphstore.NoEvent where none is found.
func AnswerFacts(askID, state string, answer *model.AskAnswer, resolution *model.AskResolution, who model.Actor, at time.Time, event int64) ([]graphstore.Node, []graphstore.Tuple, error) {
	change := Recorded(state, answer, resolution)
	change.Who, change.At, change.Event = who, at, event
	return answerFacts(askID, change, heldAnswer{})
}

// answerFacts is the nodes and tuples change states on the node of askID, which holds held: the
// answer state; the answer's values and its person, asserted where the change leaves an answer and
// retracted where it takes one away; and the resolution likewise. A part the change keeps states
// nothing. The person who answered is a node of the store from this write on, as an author is.
func answerFacts(askID string, change Change, held heldAnswer) ([]graphstore.Node, []graphstore.Tuple, error) {
	var nodes []graphstore.Node
	var tuples []graphstore.Tuple
	tuple := func(kind, toKind, toID string, asserted bool) {
		tuples = append(tuples, graphstore.Tuple{
			FromKind: graphstore.KindAsk, FromID: askID, Kind: kind,
			ToKind: toKind, ToID: toID,
			At: change.At, Who: change.Who, SourceEvent: change.Event, Asserted: asserted,
		})
	}
	value := func(kind, to string) { tuple(kind, graphstore.KindValue, to, true) }
	retract := func(kind string) {
		if current, holds := held.values[kind]; holds {
			tuple(kind, graphstore.KindValue, current, false)
		}
	}
	relate := func(kind, login string) {
		for _, current := range held.relations[kind] {
			if current != login {
				tuple(kind, graphstore.KindPerson, current, false)
			}
		}
		if login != "" {
			tuple(kind, graphstore.KindPerson, login, true)
		}
	}
	value(graphstore.TupleAnswerState, change.State)
	if !change.KeepAnswer {
		if answer := change.Answer; answer != nil {
			if answer.Text != nil {
				value(graphstore.TupleAnswer, *answer.Text)
			} else {
				retract(graphstore.TupleAnswer)
			}
			value(graphstore.TupleSelected, SelectedValue(answer.Selected))
			value(graphstore.TupleAnsweredAt, AnsweredAtValue(answer.At))
			login := AnswererOf(*answer)
			if login != "" {
				nodes = append(nodes, graphstore.PersonNode(login, change.Who, change.At, change.Event))
			}
			relate(graphstore.TupleAnsweredBy, login)
			relate(graphstore.TupleDecidedBy, login)
		} else {
			retract(graphstore.TupleAnswer)
			retract(graphstore.TupleSelected)
			retract(graphstore.TupleAnsweredAt)
			relate(graphstore.TupleAnsweredBy, "")
			relate(graphstore.TupleDecidedBy, "")
		}
	}
	if !change.KeepResolution {
		if change.Resolution != nil {
			resolution, err := ResolutionValue(*change.Resolution)
			if err != nil {
				return nil, nil, err
			}
			value(graphstore.TupleResolution, resolution)
		} else {
			retract(graphstore.TupleResolution)
		}
	}
	return nodes, tuples, nil
}

// SelectedValue is the value selected holds for an answer's selection: the labels it selects, in
// its order, as a JSON array, the empty array for an answer that selects none.
func SelectedValue(selected []string) string {
	if selected == nil {
		selected = []string{}
	}
	encoded, _ := json.Marshal(selected)
	return string(encoded)
}

// AnsweredAtValue is the value answered_at holds for an answer given at at: RFC 3339 in UTC with
// every fraction of a second the time carries, as the answer's own `at` is written.
func AnsweredAtValue(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// ResolutionValue is the value resolution holds for a resolution: the resolution as the asks row
// stores it, kind, reason, Dispatch's own words, who and when, as JSON.
func ResolutionValue(resolution model.AskResolution) (string, error) {
	encoded, err := json.Marshal(resolution)
	if err != nil {
		return "", fmt.Errorf("encode an ask's resolution: %w", err)
	}
	return string(encoded), nil
}

// AnswererOf is the person node an answer's answered_by and decided_by point at: the canonical
// login of the person who gave it, or "" where it names nobody the store can key.
func AnswererOf(answer model.AskAnswer) string {
	login := model.CanonicalLogin(answer.User)
	if !graphstore.AddressFits(login) {
		return ""
	}
	return login
}

// AnswerKinds is every kind Settle states on an ask's node, values and relations alike, sorted:
// what a reader of the node's answer reads (TestEveryWriterOfAnAnswerWritesItOnTheNode).
func AnswerKinds() []string {
	kinds := append(slices.Clone(answerValueKinds[:]), answerRelationKinds[:]...)
	slices.Sort(kinds)
	return kinds
}

// NodeAnswerTuples is the read of the answer the node of an ask holds as it stands now, as SQL a
// statement splices in: the newest tuple of each fact of AnswerKinds out of the node of the ask
// whose id, as text, the SQL expression id names, a value by its kind and a relation by its far
// end (graphstore.CurrentTupleRead), as t.kind, t.to_kind, t.to_id and t.asserted, which the
// caller's `where asserted` cuts to the facts that stand. A fact of a kind a reader does not read
// is left by it, as Read leaves one. It is the one reading of the node's answer as a list of facts
// in SQL, which every statement that reads the answer so shares: every read of NodeAnswerColumn
// (api's ask reads among them), docs' text routes and writes (askNodeAnswers), and the replay's
// check of the answers it serves, through NodeAnswerTuplesIn (loadtest's servedAnswersQuery). A
// statement that reads the answer as values of its own reads it through the Node...Of fragments
// of node_answer.go instead, as the session answers and the search text read NodeAnswerJSONOf.
func NodeAnswerTuples(id string) string {
	return NodeAnswerTuplesIn(`graph_tuples`, id)
}

// NodeAnswerTuplesIn is NodeAnswerTuples over table, graph_tuples under the name the calling
// statement gives it (graphstore.CurrentTupleReadIn): the replay's check of the answers it serves
// reads public.graph_tuples, since every statement of loadtest names the schema of each table it
// reads (loadtest's servedAnswersQuery). table is a constant of the caller's own text, never a
// request's value, as id is.
func NodeAnswerTuplesIn(table, id string) string {
	return graphstore.CurrentTupleReadIn(table,
		`t.kind, t.to_kind, case when t.to_kind = '`+graphstore.KindValue+`' then '' else t.to_id end`,
		`t.kind, t.to_kind, t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = `+id+` and t.kind in (`+answerKindsList+`)`)
}

// answerKindsList is AnswerKinds as the SQL list NodeAnswerTuplesIn reads, each quoted, built once:
// every ask read that selects NodeAnswerColumn splices it in.
var answerKindsList = `'` + strings.Join(AnswerKinds(), `', '`) + `'`

// NodeAnswer is what an ask's node holds of its answer state, read off the facts of AnswerKinds
// that stand on it (Read): the answer state, "" where the node holds none; the answer's words,
// the options it selects, its time and the person who gave it (answered_by), each nil or "" where
// the node holds none; and the resolution, nil for none. decided_by names the person answered_by
// names (answerFacts), so it is read as nothing more. It is the one decoder of the node's answer,
// which every reader of it in Go shares (R3).
//
// Named says the ask's row names the node (asks.node_kind), which its reader reads beside the
// facts. Settle states an answer on the node only once the row names it, so the row of an ask a
// process of the release before wrote, until the boot step's backfill marks it, and of an ask
// whose relations are past the store's bound (graphstore.AskFacts) is its answer's only record,
// and every read of such an ask reads its answer off the row (ApplyTo).
type NodeAnswer struct {
	Named      bool
	State      string
	Answer     *string
	Selected   []string
	AnsweredAt *time.Time
	AnsweredBy string
	Resolution *model.AskResolution
}

// Read takes value, the far end of one fact of kind that stands on the node - the value itself
// for a value kind, the person for a relation - into the answer, and refuses one that does not
// decode as Settle encodes it, with ErrNodeAnswerUnreadable: an answer state none of the three, a
// selection that is no list of labels (SelectedValue), an answered_at that is no time
// (AnsweredAtValue), or a resolution that is no resolution (ResolutionValue). A fact of a kind the
// answer does not read (IsAnswerKind) is left.
func (n *NodeAnswer) Read(kind, value string) error {
	switch kind {
	case graphstore.TupleAnswerState:
		switch value {
		case StateOpen, StateAnswered, StateResolved:
			n.State = value
		default:
			return fmt.Errorf("%w: answer_state holds %q, which is none of open, answered and resolved", ErrNodeAnswerUnreadable, value)
		}
	case graphstore.TupleAnswer:
		n.Answer = &value
	case graphstore.TupleSelected:
		selected := []string{}
		if err := json.Unmarshal([]byte(value), &selected); err != nil {
			return fmt.Errorf("%w: selected holds %q, which is no list of labels: %w", ErrNodeAnswerUnreadable, value, err)
		}
		n.Selected = selected
	case graphstore.TupleAnsweredAt:
		at, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return fmt.Errorf("%w: answered_at holds %q, which is no time: %w", ErrNodeAnswerUnreadable, value, err)
		}
		n.AnsweredAt = &at
	case graphstore.TupleAnsweredBy:
		n.AnsweredBy = value
	case graphstore.TupleResolution:
		var resolution model.AskResolution
		if err := json.Unmarshal([]byte(value), &resolution); err != nil {
			return fmt.Errorf("%w: resolution holds %q, which is no resolution: %w", ErrNodeAnswerUnreadable, value, err)
		}
		n.Resolution = &resolution
	}
	return nil
}

// ApplyTo lays n over ask, read from its row, as every read of an ask shows its answer state and
// answer (R3), in the shape the row projects them in (migration 0007's asks_answer_state_check
// and asks_resolution_state_check): the answer state the node holds, open where it holds none - an
// ask no settlement has written (Settle); its answer on an answered ask alone, with its words,
// selection, time and person; and its resolution on a resolved ask alone. The node holds no act
// (model.AskAnswer.Act), so the answer carries the row's where the node holds the row's answer
// (Gave), and none otherwise. A row that names no node (Named) is left as it reads, its answer's
// only record. api's ask reads and docs' text routes both read an ask's answer through it.
func (n NodeAnswer) ApplyTo(ask *model.Ask) {
	if !n.Named {
		return
	}
	row := ask.Answer
	ask.State, ask.Answer, ask.Resolution = StateOpen, nil, nil
	if n.State != "" {
		ask.State = n.State
	}
	switch ask.State {
	case StateAnswered:
		answer := &model.AskAnswer{Selected: append([]string{}, n.Selected...), Text: n.Answer, User: n.AnsweredBy}
		if n.AnsweredAt != nil {
			answer.At = *n.AnsweredAt
		}
		if row != nil && n.Gave(*row) {
			answer.Act = row.Act
		}
		ask.Answer = answer
	case StateResolved:
		ask.Resolution = n.Resolution
	}
}

// Gave reports whether the node holds answer, an answer an ask's row holds: the node holds an
// answered time equal to answer's, and its answered_by names the person node Settle points
// answered_by at for answer (AnswererOf), none for a person the store cannot key. It is the one
// test of whether the node holds a row's answer, so every reader that asks it applies one rule to
// the login: ApplyTo, which carries the row's act onto the node's answer where it holds; the
// answers a session was delivered, which carry it so on the node's answer their statement decodes
// (sessionanswers' scan); and docs' writes on a fleet of one build, which keep the row where an
// answered row's node does not hold its answer (keptFromItsNode).
func (n NodeAnswer) Gave(answer model.AskAnswer) bool {
	return n.AnsweredAt != nil && n.AnsweredAt.Equal(answer.At) && n.AnsweredBy == AnswererOf(answer)
}
