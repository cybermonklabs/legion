package harness

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The input modes of a judgement. Each judgement reads its subject (the reader's answer, the
// text or texts, the definition) and, beside it, the context its mode gives: the person's
// question, Dispatch's record, the writer's session part and the writer's answer to a side
// question, as far as the case has them.
const (
	// InputsFull gives every judgement all of the context the case has.
	InputsFull = "full"
	// InputsTrimmed gives each judgement only the context it needs (trimmedContext).
	InputsTrimmed = "trimmed"
)

// contextParts are the parts of a case's context one call reads. Text is the text the person
// sees, read beside a subject that is not the text (textIsContext).
type contextParts struct {
	Question, Text, Record, Session, WriterAnswer bool
}

var fullContext = contextParts{Question: true, Record: true, Session: true, WriterAnswer: true}

// trimmedContext is the context each judgement needs. The answer judgements and Define need the
// question and the record, to know what was asked and what is true; lost and added need the
// writer's session, to know the facts of the work (added also every other input of the change,
// to tell a fact the inputs hold from one they do not); phrases and undefined need the record,
// which can define a word or name what a phrase points at; harder compares the two texts and
// needs nothing else; answer_start needs the question alone.
var trimmedContext = map[Judgement]contextParts{
	JAnswered:       {Question: true, Record: true},
	JFirstParagraph: {Question: true, Record: true},
	JLost:           {},
	JAdded:          {Record: true, Session: true, WriterAnswer: true},
	JPhrases:        {Record: true},
	JUndefined:      {Record: true},
	JAnswerStart:    {Question: true},
	JHarder:         {},
	JDefine:         {Question: true, Record: true, WriterAnswer: true},
}

// questionIsSubject are the judgements that judge something against the person's question,
// so they read it in either mode.
var questionIsSubject = []Judgement{JAnswered, JFirstParagraph, JAnswerStart, JDefine}

// textIsContext are the judgements that read the text the person sees beside their subject, in
// either mode: answered, whose subject is the fresh reader's answer to the person's question from
// that text. Without the text the grader cannot tell a reader that missed what the text says
// from a text that does not say it, nor see which of the answer's words the text explains; and
// as the record ends before the text was written, it took a reply's new facts for errors against
// the older record (INFRA-3231, the calibration of answered against what the owner did after an
// explanation). first_paragraph's fresh reader read only the first paragraph, so its grader does
// not read the whole text.
var textIsContext = []Judgement{JAnswered}

// GraderInputs is the input mode of each judgement.
type GraderInputs map[Judgement]string

// FinalInputs is the mode each judgement takes where a run names none. A judgement is trimmed
// where its agreement with the full-input grader on a development sample was no worse than the
// full grader's agreement with itself, less 2 points, or where the planted outputs of grader-test
// show the full inputs make errors that the trimmed inputs do not; it is full otherwise, also
// where change 1 runs no call of it (lost, added, define). Measured on 312 development cases,
// 2026-11-15, agreement with the full grader against its own: answered 93.9% against 96.5%,
// first_paragraph 94.1% against 94.7%, undefined 79.8% against 86.9%, answer_start 88.2% against
// 95.9%, harder 100% against 100%. answered and answer_start are trimmed by grader-test: with full
// inputs answered marked a wrong planted output right in 2 of 4 runs and answer_start in 4 of 4,
// with trimmed inputs answered in 1 of 6 runs and answer_start in none. phrases reads its
// candidate sentences alone.
var FinalInputs = GraderInputs{
	JAnswered: InputsTrimmed, JFirstParagraph: InputsTrimmed, JLost: InputsFull, JAdded: InputsFull,
	JPhrases: InputsTrimmed, JUndefined: InputsFull, JAnswerStart: InputsTrimmed, JHarder: InputsTrimmed,
	JDefine: InputsFull,
}

// ParseGraderInputs reads --grader-inputs: "final", "full" or "trimmed", then any number of
// ",<judgement>=<mode>" that set one judgement's mode.
func ParseGraderInputs(s string) (GraderInputs, error) {
	parts := strings.Split(s, ",")
	out := GraderInputs{}
	switch base := strings.TrimSpace(parts[0]); base {
	case "final", "":
		maps.Copy(out, FinalInputs)
	case InputsFull, InputsTrimmed:
		for _, j := range Judgements {
			out[j] = base
		}
	default:
		return nil, fmt.Errorf("--grader-inputs starts with final, full or trimmed, not %q", base)
	}
	for _, p := range parts[1:] {
		name, mode, ok := strings.Cut(strings.TrimSpace(p), "=")
		j := Judgement(name)
		if !ok || !slices.Contains(Judgements, j) || (mode != InputsFull && mode != InputsTrimmed) {
			return nil, fmt.Errorf("--grader-inputs %q: each part after the first is <judgement>=full or <judgement>=trimmed", p)
		}
		out[j] = mode
	}
	return out, nil
}

// context is the context a judgement reads under the modes.
func (gi GraderInputs) context(j Judgement) contextParts {
	c := trimmedContext[j]
	if gi[j] == InputsFull {
		c = fullContext
	}
	if slices.Contains(questionIsSubject, j) {
		c.Question = true
	}
	if slices.Contains(textIsContext, j) {
		c.Text = true
	}
	return c
}

// String is the modes as --grader-inputs takes them: "full" or "trimmed" when every judgement
// takes it, else "trimmed" and each judgement that takes full.
func (gi GraderInputs) String() string {
	full := 0
	for _, j := range Judgements {
		if gi[j] == InputsFull {
			full++
		}
	}
	if full == len(Judgements) {
		return InputsFull
	}
	var b strings.Builder
	b.WriteString(InputsTrimmed)
	for _, j := range Judgements {
		if gi[j] == InputsFull {
			b.WriteString("," + string(j) + "=" + InputsFull)
		}
	}
	return b.String()
}
