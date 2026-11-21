package reviewer

import (
	"context"
)

// Answer is the writer session's answer to Dispatch's one side question (changes 5 and 6): the
// question Dispatch sent (RewriteQuestion, DefineQuestion) and the text the agent wrote for a
// person in answer. It is no session text. The zero Answer is none: the session was not running,
// its host answers no side question, or no answer came in time, and the call reads the record
// alone.
type Answer struct {
	Question, Text string
}

// Rewritten is what one rewrite answered.
type Rewritten struct {
	// Text is the rewrite.
	Text Text
	// NotApplied is the rule of what a rewrite keeps that Text breaks (Preserve), nil where it
	// keeps them all. A rewrite that breaks one is not applied, and NotApplied is its reason.
	NotApplied *NotApplied
	// Calls are the model calls the rewrite made, the detector's first.
	Calls []Call
}

// Rewrite writes record's text again for its reader by rules, from the record and, where answer
// holds one, the writer's answer: change 4 with no answer, change 5 and a person's Rewrite with
// it. A model call that gives no usable answer is a *ModelError, and the Rewritten then still
// carries the calls made.
func Rewrite(ctx context.Context, m Model, rules []RuleSet, record Record, answer Answer) (Rewritten, error) {
	p, err := prepare(ctx, m, PurposeRewrite, rules, record, extra{question: answer.Question, answer: answer.Text})
	if err != nil {
		return Rewritten{}, err
	}
	rewritten := Rewritten{Calls: p.calls}
	reply, c, err := call[Text](ctx, m, PurposeRewrite, p.system, p.input, rewriteSchema(record.Text.Kind), nil)
	rewritten.Calls = append(rewritten.Calls, c)
	if err != nil {
		return rewritten, err
	}
	rewritten.Text = ofKind(reply, record.Text.Kind)
	rewritten.NotApplied = Preserve(record, answer.Text, rewritten.Text)
	return rewritten, nil
}

// questionData is what a side question is filled from.
type questionData struct {
	ID, Kind string
	Ask      bool
	Text     string
	Words    []string
}

// RewriteQuestion is the side question Dispatch sends the writer session of text, whose id is id,
// before a person's Rewrite of it (INFRA-3552's R2): what the reader needs to answer it, or to
// understand it, without the session's conversation.
func RewriteQuestion(id string, text Text) (string, error) {
	return render("rewrite-question", questionData{ID: id, Kind: text.Kind.spoken(), Ask: text.Kind.asks(), Text: spokenText(text)})
}
