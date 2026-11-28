package asks

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The answer as an ask's node holds it, which Settle writes and every reader of an answer reads
// through this package (R3), as a statement or as a value. A statement selects NodeAnswerColumn,
// which holds each fact of the answer that stands, or picks asks by one fact of it (NodeValueOf,
// NodeStateOf and the rest of the Node...Of fragments); a reader that wants the facts of several
// asks takes them through NodeAnswerTuples. The value is NodeAnswer: ReadNodeAnswer decodes the
// column into it, NodeAnswer.Read decodes one fact, and NodeAnswer.ApplyTo lays it over the ask's
// row. A reader that reads the facts through another statement of its own reads them through
// those decoders, so a value that does not parse is refused alike by every read of an answer
// (ErrNodeAnswerUnreadable).

// ErrNodeAnswerUnreadable is the error of a node whose answer holds a value that does not parse
// (NodeAnswer.Read): an answer state outside open, answered and resolved, a selection that is no
// list of labels, a time that is none, or a resolution that is no JSON of its shape. Settle writes
// none of them, so it names a node written by hand.
var ErrNodeAnswerUnreadable = errors.New("the node holds an answer that does not parse")

// IsAnswerKind reports whether kind is one NodeAnswer.Read takes into the answer: the answer
// state, the answer's words, selection and time, the person who gave it, and the resolution.
// decided_by, the one kind of AnswerKinds Read leaves, names the person answered_by names and is
// shown by the Decisions read alone, which reads it itself.
func IsAnswerKind(kind string) bool {
	switch kind {
	case graphstore.TupleAnswerState, graphstore.TupleAnswer, graphstore.TupleSelected,
		graphstore.TupleAnsweredAt, graphstore.TupleAnsweredBy, graphstore.TupleResolution:
		return true
	}
	return false
}

// NodeFact is one value or relation of a node as a statement aggregates it: its kind, the kind of
// its far end, and that far end, the value itself for a value.
type NodeFact struct {
	Kind   string `json:"kind"`
	ToKind string `json:"to_kind"`
	ToID   string `json:"to_id"`
}

// ReadNodeAnswer decodes NodeAnswerColumn, or answers which of its facts does not parse
// (ErrNodeAnswerUnreadable). A null column is a row that names no node, whose answer the node
// does not hold (NodeAnswer.Named).
func ReadNodeAnswer(encoded []byte) (NodeAnswer, error) {
	if encoded == nil {
		return NodeAnswer{}, nil
	}
	var facts []NodeFact
	if err := json.Unmarshal(encoded, &facts); err != nil {
		return NodeAnswer{}, fmt.Errorf("the node's answer: %w", err)
	}
	node := NodeAnswer{Named: true}
	for _, fact := range facts {
		if err := node.Read(fact.Kind, fact.ToID); err != nil {
			return NodeAnswer{}, err
		}
	}
	return node, nil
}

// ReadAnswer is the answer state and answer of the ask askID as every ask read shows them
// (NodeAnswer.ApplyTo), for a read that selects the ask's row's state and answer (asks.state, and
// asks.answer, nil for none) and its node's answer (NodeAnswerColumn) rather than a whole ask.
func ReadAnswer(askID, state string, answer, nodeAnswer []byte) (model.Ask, error) {
	ask := model.Ask{ID: askID, State: state}
	if len(answer) > 0 {
		ask.Answer = new(model.AskAnswer)
		if err := json.Unmarshal(answer, ask.Answer); err != nil {
			return model.Ask{}, fmt.Errorf("decode the answer of ask %s: %w", askID, err)
		}
	}
	node, err := ReadNodeAnswer(nodeAnswer)
	if err != nil {
		return model.Ask{}, fmt.Errorf("read ask %s: %w", askID, err)
	}
	node.ApplyTo(&ask)
	return ask, nil
}

// NodeAnswerColumn is the answer state of the ask aliased ask as its node holds it: each fact of
// AnswerKinds that stands, as a JSON list of NodeFact, read through the one reading of the node's
// answer (NodeAnswerTuples). ReadNodeAnswer decodes it. It is null for a row that names no node
// (asks.node_kind): Settle writes the answer on the node only once the row names it, so the row of
// an ask a process of the release before wrote, until the boot step's backfill marks it, and of an
// ask whose relations are past the store's bound (graphstore.AskFacts) is its answer's only record.
func NodeAnswerColumn(ask string) string {
	return NodeAnswerColumnIn(`graph_tuples`, ask)
}

// NodeAnswerColumnIn is NodeAnswerColumn over table, graph_tuples under the name the calling
// statement gives it (NodeAnswerTuplesIn): the replay's check of the answers it serves reads
// public.graph_tuples (loadtest's servedAnswersQuery). table is a constant of the caller's own
// text, never a request's value.
func NodeAnswerColumnIn(table, ask string) string {
	return `case when ` + ask + `.node_kind is not null then (
		select coalesce(json_agg(json_build_object('kind', f.kind, 'to_kind', f.to_kind, 'to_id', f.to_id)), '[]'::json)
		from (` + NodeAnswerTuplesIn(table, ask+`.id::text`) + `) f
		where f.asserted) end`
}

// NodeValueOf is the value of kind that stands on the node of the ask aliased ask, null where
// none stands: the newest tuple of kind (graphstore.CurrentTupleRead) with its far end bound to a
// value, so the read walks graph_tuples_values in that index's own order and sorts nothing - the
// Inbox's reply rows hold their plans to no sort (TestMyRepliesQueriesReadTheirStatesIndex). It is
// a probe of the ask's own node that the expressions below read under a case or an or, so it ends
// in an offset 0, which keeps the planner from pulling it up into a read of the log for every ask
// at once.
func NodeValueOf(ask, kind string) string {
	return `(select f.to_id from (` + graphstore.CurrentTupleRead(`t.kind`, `t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = `+ask+`.id::text and t.kind = '`+kind+
			`' and t.to_kind = '`+graphstore.KindValue+`'`) + `) f where f.asserted offset 0)`
}

// NodeRelationOf is the far end of the relation of kind that stands on the node of the ask
// aliased ask, null where none stands: the newest tuple of each far end
// (graphstore.CurrentTupleRead), read through graph_tuples_current in its own order. Settle keeps
// one far end of each relation it writes standing and retracts the others, so the probe answers
// the first that stands. It ends in an offset 0, as NodeValueOf does.
func NodeRelationOf(ask, kind string) string {
	return `(select f.to_id from (` + graphstore.CurrentTupleRead(`t.to_kind, t.to_id`, `t.to_id, t.asserted`,
		`t.from_kind = '`+graphstore.KindAsk+`' and t.from_id = `+ask+`.id::text and t.kind = '`+kind+
			`' and t.to_kind <> '`+graphstore.KindValue+`'`) + `) f where f.asserted limit 1 offset 0)`
}

// NodeStateOf is the answer state of the ask aliased ask as NodeAnswer.ApplyTo lays it over the
// row, for a statement that picks asks by it (R3): the answer_state its node holds where the row
// names the node, open where the node holds none, and the row's own where the row names no node.
// An owner's ask lists by state and a session's open asks on one issue pick by it alone; a
// statement that reads open asks across owners picks by it beside the row's state (api's
// askOpenOf). A read that decides a transition reads the row's state instead, as Settle checks
// the state it moves from against the row, and so do the open counts.
func NodeStateOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.state else coalesce(` +
		NodeValueOf(ask, graphstore.TupleAnswerState) + `, '` + StateOpen + `') end`
}

// NodeAnswerTextOf is the words of the answer of the ask aliased ask as ApplyTo lays them over the
// row: the answer value its node holds on an ask the node holds answered, none on any other the
// node holds, and the row's answer's text where the row names no node.
func NodeAnswerTextOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.answer->>'text' when ` +
		NodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + StateAnswered + `' then ` +
		NodeValueOf(ask, graphstore.TupleAnswer) + ` end`
}

// NodeAnswererOf is the person who answered the ask aliased ask as ApplyTo lays it over the row:
// the node's answered_by on an ask the node holds answered, null on any other the node holds, and
// the row's answer's user where the row names no node. The Replies list's unread count reads an
// answer as a write in the ask's thread through it and NodeAnsweredAtOf, and an issue's ask page
// reads its answerer through it (api's askThreadPageStatement).
func NodeAnswererOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.answer->>'user' when ` +
		NodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + StateAnswered + `' then ` +
		NodeRelationOf(ask, graphstore.TupleAnsweredBy) + ` end`
}

// NodeAnsweredAtOf is when the ask aliased ask was answered, as ApplyTo lays it over the row: the
// node's answered_at on an ask the node holds answered, null on any other the node holds, and the
// row's answer's at where the row names no node.
func NodeAnsweredAtOf(ask string) string {
	return `(` + NodeAnsweredAtTextOf(ask) + `)::timestamptz`
}

// NodeAnsweredAtTextOf is NodeAnsweredAtOf before its cast: the text the time is stored in, the
// row's answer's at or the node's answered_at (AnsweredAtValue), each RFC 3339 with every
// fraction of a second the time carries. An issue's ask page reads it so, to cut the time to
// microseconds before a cast would round it (api's askThreadPageStatement).
func NodeAnsweredAtTextOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.answer->>'at' when ` +
		NodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + StateAnswered + `' then ` +
		NodeValueOf(ask, graphstore.TupleAnsweredAt) + ` end`
}

// NodeResolutionOf is the resolution of the ask aliased ask as ApplyTo lays it over the row, as
// jsonb: the resolution value its node holds on an ask the node holds resolved, none on any other
// the node holds, and the row's own where the row names no node. The write advice's declined asks
// read a decline through it (api's readDeclinedAsks), and an issue's ask page a resolution's time
// and actor (api's askThreadPageStatement).
func NodeResolutionOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.resolution when ` +
		NodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + StateResolved + `' then (` +
		NodeValueOf(ask, graphstore.TupleResolution) + `)::jsonb end`
}

// NodeAnswerJSONOf is the answer of the ask aliased ask as ApplyTo lays it over the row, as jsonb
// of asks.answer's shape without its act: the answer, selection, answerer and time its node holds
// on an ask the node holds answered - the selection the empty list where none is stated - null on
// any other the node holds, and the row's own answer where the row names no node. Its two readers
// are the search text, which reads the words of it (api's askSearchTextOf, ask_search_suffix), and
// the session answers, which read the whole of it and hold it to their window by its time
// (sessionanswers' query).
func NodeAnswerJSONOf(ask string) string {
	return `case when ` + ask + `.node_kind is null then ` + ask + `.answer when ` +
		NodeValueOf(ask, graphstore.TupleAnswerState) + ` = '` + StateAnswered + `' then jsonb_build_object(
			'user', ` + NodeRelationOf(ask, graphstore.TupleAnsweredBy) + `,
			'selected', coalesce((` + NodeValueOf(ask, graphstore.TupleSelected) + `)::jsonb, '[]'::jsonb),
			'text', ` + NodeValueOf(ask, graphstore.TupleAnswer) + `,
			'at', ` + NodeValueOf(ask, graphstore.TupleAnsweredAt) + `) end`
}
