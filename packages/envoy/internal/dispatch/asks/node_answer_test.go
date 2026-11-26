package asks

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every read of an answer decodes the facts of an ask's node through NodeAnswer.Read, so a value
// that does not parse is refused by all of them alike: an answer state outside open, answered and
// resolved, a selection that is no list of labels, a time that is none, and a resolution that is no
// JSON of its shape. A kind the node does not state an answer with is not Read's.
func TestReadRefusesEachValueOfTheNodeThatDoesNotParse(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ kind, value string }{
		{graphstore.TupleAnswerState, "half-answered"},
		{graphstore.TupleAnswerState, ""},
		{graphstore.TupleSelected, "Yes, No"},
		{graphstore.TupleAnsweredAt, "next Tuesday"},
		{graphstore.TupleResolution, "declined"},
	} {
		var node NodeAnswer
		held, err := node.Read(test.kind, test.value)
		if !held || !errors.Is(err, ErrNodeAnswerUnreadable) {
			t.Errorf("Read(%s, %q) = held %t, %v, want the kind held and ErrNodeAnswerUnreadable", test.kind, test.value, held, err)
		}
	}
	var node NodeAnswer
	if held, err := node.Read(graphstore.TupleDecidedBy, "alice"); held || err != nil {
		t.Errorf("Read(decided_by) = held %t, %v, want a kind it leaves to its caller", held, err)
	}
}

// ReadNodeAnswer decodes NodeAnswerColumn: null names no node, a list names the facts that stand,
// and ApplyTo lays them over the row as every ask read does.
func TestReadNodeAnswerDecodesTheColumnAndApplyToLaysItOverTheRow(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 20, 9, 0, 0, 123456000, time.UTC)
	node, err := ReadNodeAnswer(nil)
	if err != nil || node.Named {
		t.Fatalf("ReadNodeAnswer(null) = %+v, %v, want an answer that names no node", node, err)
	}
	row := model.Ask{State: StateAnswered, Answer: &model.AskAnswer{User: "bob", At: at}}
	node.ApplyTo(&row)
	if row.State != StateAnswered || row.Answer == nil || row.Answer.User != "bob" {
		t.Errorf("a row that names no node reads %+v, want it as it was", row)
	}

	node, err = ReadNodeAnswer([]byte(`[
		{"kind": "answer_state", "to_kind": "value", "to_id": "answered"},
		{"kind": "answer", "to_kind": "value", "to_id": "Ship it."},
		{"kind": "selected", "to_kind": "value", "to_id": "[\"Yes\"]"},
		{"kind": "answered_at", "to_kind": "value", "to_id": "` + AnsweredAtValue(at) + `"},
		{"kind": "answered_by", "to_kind": "person", "to_id": "alice"}]`))
	if err != nil || !node.Named {
		t.Fatalf("ReadNodeAnswer(facts) = %+v, %v, want an answer that names its node", node, err)
	}
	row = model.Ask{State: StateOpen}
	node.ApplyTo(&row)
	if row.State != StateAnswered || row.Answer == nil || row.Answer.User != "alice" || !row.Answer.At.Equal(at) ||
		row.Answer.Text == nil || *row.Answer.Text != "Ship it." || len(row.Answer.Selected) != 1 || row.Answer.Selected[0] != "Yes" {
		t.Errorf("the row reads %+v with the answer %+v, want alice's answer as the node holds it", row, row.Answer)
	}

	if _, err := ReadNodeAnswer([]byte(`[{"kind": "answer_state", "to_kind": "value", "to_id": "half-answered"}]`)); !errors.Is(err, ErrNodeAnswerUnreadable) {
		t.Errorf("ReadNodeAnswer(an answer state outside the three) = %v, want ErrNodeAnswerUnreadable", err)
	}
}
