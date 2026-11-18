package asks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	// Event is the event that caused the change, which each tuple carries as its source_event.
	// A caller whose event is appended after the change - every request, since the ask's parts
	// and its event payload read the row this writes - sets Held instead: the change's facts are
	// added there, and the caller hands it to graphstore.Caused once the event exists, in the same
	// transaction.
	Event int64
	Held  *graphstore.Uncaused
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

// Recorded is the whole answer state an event recorded - state, answer and resolution - as the
// catch-up of the boot step reads it off an event a process of another build appended
// (docs' decision step).
func Recorded(state string, answer *model.AskAnswer, resolution *model.AskResolution) Change {
	return Change{State: state, Answer: answer, Resolution: resolution}
}

// Settle is the one writer of an ask's answer state. It appends to the ask's node what the change
// states - answer_state always, and the answer's words (answer), the options it selects
// (selected) and its time (answered_at) as values, the person who gave it as the far end of
// answered_by and decided_by, and the resolution as a value - and retracts each of those the node
// holds that the change takes away. It then writes the asks row from what it appended, in the same
// transaction: state, and answer and resolution where the change states them. The node is the
// record and the row its projection, which every reader of an ask's state but a reading's check
// of its ask (docs' askAnswers) and the decisions step's count still reads.
//
// The node holds the answer only once the row names it (asks.node_kind): a row a process of the
// release before wrote names none until the boot step's backfill states the whole row on the node
// and marks it (graphcensus' asks batch), so Settle writes such a row alone and leaves the node to
// that backfill, which reads the row this writes. A marked row's node is the one its foreign key
// names, so it always exists.
//
// Every fact it states goes out of the ask's node and changes it (graphstore.Tuple's Changes), so
// a settle made for a run of the command bar - an answer, a resolution, a decline, an issue's
// close or reopen - names the ask as changed by that run (graphstore.WithRun); the person node of
// whoever answered is never marked.
//
// It locks the ask's row first, which every caller holds already but the approval carry, and
// answers the ask's id and its answer state - State, Answer and Resolution - as the row now holds
// them; the rest of the row is the caller's.
func Settle(ctx context.Context, tx pgx.Tx, askID string, change Change) (model.Ask, error) {
	held, err := readHeldAnswer(ctx, tx, askID)
	if err != nil {
		return model.Ask{}, err
	}
	if change.From != "" && held.state != change.From {
		return model.Ask{}, fmt.Errorf("settle ask %s from %s: it is %s: %w", askID, change.From, held.state, ErrStateMoved)
	}
	if held.marked {
		nodes, tuples, err := answerFacts(askID, change, held)
		if err != nil {
			return model.Ask{}, err
		}
		for index := range tuples {
			tuples[index].Changes = true
		}
		if change.Held != nil {
			written, err := graphstore.WriteUncaused(ctx, tx, nodes, tuples)
			if err != nil {
				return model.Ask{}, fmt.Errorf("state the answer of ask %s: %w", askID, err)
			}
			change.Held.Nodes = append(change.Held.Nodes, written.Nodes...)
			change.Held.Tuples = append(change.Held.Tuples, written.Tuples...)
		} else if err := graphstore.Write(ctx, tx, nodes, tuples); err != nil {
			return model.Ask{}, fmt.Errorf("state the answer of ask %s: %w", askID, err)
		}
	}
	answer, err := encodeAnswer(change.Answer)
	if err != nil {
		return model.Ask{}, err
	}
	var resolution any
	if change.Resolution != nil {
		encoded, err := json.Marshal(change.Resolution)
		if err != nil {
			return model.Ask{}, fmt.Errorf("encode the resolution of ask %s: %w", askID, err)
		}
		resolution = encoded
	}
	settled := model.Ask{ID: askID}
	var storedAnswer, storedResolution []byte
	if err := tx.QueryRow(ctx, `
		update asks
		set state = $2,
		    answer = case when $3 then answer else $4::jsonb end,
		    resolution = case when $5 then resolution else $6::jsonb end
		where id = $1::uuid
		returning state, answer, resolution
	`, askID, change.State, change.KeepAnswer, answer, change.KeepResolution, resolution).Scan(&settled.State, &storedAnswer, &storedResolution); err != nil {
		return model.Ask{}, fmt.Errorf("write the answer state of ask %s: %w", askID, err)
	}
	if len(storedAnswer) > 0 {
		settled.Answer = &model.AskAnswer{}
		if err := json.Unmarshal(storedAnswer, settled.Answer); err != nil {
			return model.Ask{}, fmt.Errorf("decode the answer of ask %s: %w", askID, err)
		}
	}
	if len(storedResolution) > 0 {
		settled.Resolution = &model.AskResolution{}
		if err := json.Unmarshal(storedResolution, settled.Resolution); err != nil {
			return model.Ask{}, fmt.Errorf("decode the resolution of ask %s: %w", askID, err)
		}
	}
	return settled, nil
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
// state the row holds, and what the node holds now of the answer - each value by its kind, and
// each far end of answered_by and decided_by.
type heldAnswer struct {
	marked    bool
	state     string
	values    map[string]string
	relations map[string][]string
}

// readHeldAnswer reads, in one statement, the ask's row - locked as an update of its answer
// state would lock it - and what its node holds of the answer: the newest value of each value
// kind while it asserts, and every current far end of the two relations.
func readHeldAnswer(ctx context.Context, tx pgx.Tx, askID string) (heldAnswer, error) {
	held := heldAnswer{values: map[string]string{}, relations: map[string][]string{}}
	var values, relations []byte
	err := tx.QueryRow(ctx, `
		select k.node_kind is not null, k.state,
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
		for no key update of k
	`, askID, graphstore.KindAsk, graphstore.KindValue, answerValueKinds[:], answerRelationKinds[:]).Scan(&held.marked, &held.state, &values, &relations)
	if errors.Is(err, pgx.ErrNoRows) {
		return heldAnswer{}, fmt.Errorf("settle ask %s: no such ask", askID)
	}
	if err != nil {
		return heldAnswer{}, fmt.Errorf("read what ask %s holds of its answer: %w", askID, err)
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
