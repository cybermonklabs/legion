package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/cmd/writing-check/harness/claude"
	"github.com/sjawhar/envoy/cmd/writing-check/harness/llm"
)

// The variants of the added grader (--added-grader). The added judgement lists as added some
// facts that an input it read already holds. Each variant other than old is a way to list fewer
// of them.
const (
	// AddedOld is the added judgement's request as it was: the user message holds the question,
	// the two texts, the record and the writer's answer, then the session part.
	AddedOld = "old"
	// AddedInputsFirst sends lost's and added's user message with the inputs first: the
	// question, the record, the writer's answer and the session part, then the original text
	// and, last, the new text. The reply schema and added's system prompt are the old ones.
	// Lost's system prompt has one more line, which names two kinds of lost fact (LostKinds).
	AddedInputsFirst = "inputs-first"
	// AddedVerify makes the old request, then, for a grade that lists a fact, one more call
	// (grade-added-verify.md) that asks of each listed fact whether an input holds it.
	AddedVerify = "verify"
	// AddedInputsFirstVerify makes inputs-first's added request, then verify's check of the
	// facts it lists. Lost takes inputs-first's request, and its grade is an inputs-first grade.
	AddedInputsFirstVerify = "inputs-first-verify"
)

// AddedGraders are the variants, the default first.
var AddedGraders = []string{AddedOld, AddedInputsFirst, AddedVerify, AddedInputsFirstVerify}

// inputsFirstLayout is whether variant sends lost's and added's user message in inputs-first's
// order, and verifies whether it checks the facts that the added call lists.
func inputsFirstLayout(variant string) bool {
	return variant == AddedInputsFirst || variant == AddedInputsFirstVerify
}

func verifies(variant string) bool {
	return variant == AddedVerify || variant == AddedInputsFirstVerify
}

// Verification is the check of an added grade (verify, inputs-first-verify): the facts that the
// first call listed, and the check's call, which asks of each listed fact whether an input holds
// it, with the input's words. The grade's Items are the listed facts that no input holds.
type Verification struct {
	// FirstSHA256 is the input_sha256 of the first call: the added judgement's old request under
	// verify, inputs-first's request under inputs-first-verify. FirstReused says that the grade
	// of the first call is another run's (score --grades-from): its call, the grade's Call, is
	// that run's.
	FirstSHA256 string `json:"first_input_sha256"`
	FirstReused bool   `json:"first_reused,omitempty"`
	// Listed are the facts that the first call listed.
	Listed []Item `json:"listed,omitempty"`
	// Held are the listed facts that the check found an input holds, each with the input's
	// words: the grade drops them. NotFound are the facts that the check said an input holds,
	// with words that no input holds as the call gave it: the grade keeps them.
	Held     []HeldFact `json:"held,omitempty"`
	NotFound []HeldFact `json:"held_words_not_found,omitempty"`
	// Refused, Call and Reply are the check's call, as a grade holds its own; none where the
	// first call listed no fact.
	Refused []Call          `json:"refused,omitempty"`
	Call    *Call           `json:"call,omitempty"`
	Reply   json.RawMessage `json:"reply,omitempty"`
}

// HeldFact is a listed fact, with the input's words that the check gave for it.
type HeldFact struct {
	Item
	InputWords string `json:"input_words"`
}

// heldFields are the fields of an item of the check's reply, in order.
var heldFields = []string{"number", "input_words", "held"}

// checkRequest is the check's request, the user message aside: its system prompt, filled from
// the data of the added call, its reply schema and its bound.
func checkRequest(data promptData) (llm.Request, error) {
	item := object(
		property{numbered(0, heldFields[0]), stringSchema},
		property{numbered(1, heldFields[1]), stringSchema},
		property{numbered(2, heldFields[2]), booleanSchema},
	)
	req := llm.Request{Purpose: "grade:added:verify", MaxTokens: graderMaxTokens,
		Schema: schemaJSON(object(property{"facts", arrayOf(item)}))}
	var err error
	req.System, err = render("grade-added-verify", data)
	return req, err
}

// checkUser is the check's user message, the session part aside, and after, the part that
// follows the session part: the question, the original text, the record and the writer's answer,
// as far as p reads them; then, after the session part, the listed facts, each with its number.
func (in GradeInput) checkUser(p contextParts, listed []Item) (user, after string) {
	var b strings.Builder
	if p.Question {
		b.WriteString(tag("question", in.Question))
	}
	b.WriteString(tag("original_text", in.Original))
	b.WriteString(in.contextTags(p))
	var facts strings.Builder
	for i, it := range listed {
		fmt.Fprintf(&facts, "%d. Fact: %s\n   Words of the new text: %s\n", i+1, strings.TrimSpace(it.Note), strings.TrimSpace(it.Quote))
	}
	return b.String(), tag("listed_facts", facts.String())
}

// inputsHold is whether an input that p reads holds words, each in quote form: the original
// text, the record, the writer's answer and the session part. The question is context and the
// new text is the subject, so neither of them holds a fact. The record is looked up as its texts
// hold them, with RecordPart's escape undone, as the words are (Verification.read).
func (in GradeInput) inputsHold(p contextParts) func(words string) bool {
	inputs := []string{claude.QuoteForm(in.Original)}
	if p.Record {
		inputs = append(inputs, claude.QuoteForm(unembedded(in.Record)))
	}
	if p.WriterAnswer {
		inputs = append(inputs, claude.QuoteForm(in.WriterAnswer))
	}
	return func(words string) bool {
		q := claude.QuoteForm(words)
		if q == "" {
			return false
		}
		for _, input := range inputs {
			if strings.Contains(input, q) {
				return true
			}
		}
		return p.Session && in.Session.Holds(words)
	}
}

// gradeVerified is the added judgement under --added-grader verify or inputs-first-verify. The
// first call is the old request (verify) or inputs-first's (inputs-first-verify), or the grade of
// done with no error whose inputs were that request's. Where it lists a fact, the check asks of
// each listed fact whether an input holds it, and the grade keeps the facts that no input holds. A
// fact is held only where the check gives the words of an input that hold it and that input holds
// those words. The check is the same under both variants.
func (g *Grader) gradeVerified(ctx context.Context, spec judgementSpec, in GradeInput, first gradeRequest, check llm.Request, done []*Grade) Grade {
	var grade Grade
	reused := false
	for _, d := range done {
		if d != nil && d.Error == "" && d.InputSHA256 == first.digest {
			grade, reused = *d, true
			break
		}
	}
	if !reused {
		grade = g.gradeOnce(ctx, spec, in, first)
	}
	v := &Verification{FirstSHA256: first.digest, FirstReused: reused, Listed: grade.Items}
	grade.Verify, grade.Reused = v, false
	if grade.Error != "" || len(v.Listed) == 0 {
		// No check is made: a grade whose first call is another run's makes no call in this run.
		grade.Reused = reused
		return grade
	}
	r := gradeRequest{req: check, parts: first.parts, session: first.session}
	r.req.User, r.after = in.checkUser(first.parts, v.Listed)
	resp, refused, err := g.call(ctx, r)
	c := callOf(resp)
	v.Refused, v.Call, v.Reply = refused, &c, resp.Value
	if err != nil {
		grade.Error = "the added check: " + err.Error()
		return grade
	}
	kept, err := v.read(resp.Value, in.inputsHold(first.parts))
	if err != nil {
		grade.Error = fmt.Sprintf("the added check's reply: %v", err)
		return grade
	}
	grade.Items, grade.Count = kept, len(kept)
	return grade
}

// read sorts the listed facts by the check's reply: a fact for which the reply's first item of
// its number says an input holds it, with words that holds answers, is held; a fact with words
// that no input holds is kept, and so is a fact the reply says no input holds or gives no item
// for. It answers the facts kept.
func (v *Verification) read(value json.RawMessage, holds func(string) bool) ([]Item, error) {
	var reply struct {
		Facts []map[string]json.RawMessage `json:"facts"`
	}
	if err := json.Unmarshal(value, &reply); err != nil {
		return nil, err
	}
	type answer struct {
		held  bool
		words string
	}
	answers := map[int]answer{}
	for _, f := range reply.Facts {
		var number string
		_ = json.Unmarshal(f[key(heldFields, "number")], &number)
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(number), "."))
		if _, seen := answers[n]; err != nil || n < 1 || n > len(v.Listed) || seen {
			continue
		}
		held, err := boolField(f, key(heldFields, "held"))
		if err != nil {
			return nil, err
		}
		var words string
		_ = json.Unmarshal(f[key(heldFields, "input_words")], &words)
		// The words are an input's, which the message held embedded.
		words = unembedded(words)
		answers[n] = answer{held: held, words: words}
	}
	var kept []Item
	for i, it := range v.Listed {
		a := answers[i+1]
		switch {
		case !a.held:
			kept = append(kept, it)
		case holds(a.words):
			v.Held = append(v.Held, HeldFact{Item: it, InputWords: a.words})
		default:
			v.NotFound = append(v.NotFound, HeldFact{Item: it, InputWords: a.words})
			kept = append(kept, it)
		}
	}
	return kept, nil
}
