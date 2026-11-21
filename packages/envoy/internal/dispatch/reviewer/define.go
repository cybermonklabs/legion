package reviewer

import (
	"context"
	"errors"
	"strings"
)

// Definition is what words mean where the text uses them: the words as the person selected them,
// their meaning, and the links of the record to the place that defines them.
type Definition struct {
	Words   string   `json:"words"`
	Meaning string   `json:"meaning"`
	Links   []string `json:"links"`
}

// Defined is what one Define answered.
type Defined struct {
	// Definitions are one definition for each item of the words, in their order.
	Definitions []Definition
	// Calls are the model calls Define made, the detector's first.
	Calls []Call
}

// defineReply is a definition's reply as its schema has it.
type defineReply struct {
	Definitions []Definition `json:"definitions"`
}

// ErrNoWords is a Define of no words, which makes no call.
var ErrNoWords = errors.New("reviewer: define names no words")

// Define says what words mean where record's text uses them (Decision 24, change 6), by rules,
// from the record and, where answer holds one, the writer's answer. Each item of words is one
// selection of the person's. A model call that gives no usable answer is a *ModelError, and the
// Defined then still carries the calls made.
func Define(ctx context.Context, m Model, rules []RuleSet, record Record, words []string, answer Answer) (Defined, error) {
	selected := make([]string, 0, len(words))
	for _, word := range words {
		if word = strings.TrimSpace(word); word != "" {
			selected = append(selected, word)
		}
	}
	if len(selected) == 0 {
		return Defined{}, ErrNoWords
	}
	p, err := prepare(ctx, m, PurposeDefine, rules, record, extra{question: answer.Question, answer: answer.Text, words: selected})
	if err != nil {
		return Defined{}, err
	}
	defined := Defined{Calls: p.calls}
	reply, c, err := call(ctx, m, PurposeDefine, p.system, p.input, defineSchema, func(reply defineReply) string {
		if len(reply.Definitions) == 0 {
			return "the reply defines none of the words"
		}
		return ""
	})
	defined.Calls = append(defined.Calls, c)
	if err != nil {
		return defined, err
	}
	for i := range reply.Definitions {
		definition := &reply.Definitions[i]
		definition.Words, definition.Meaning = unembedded(definition.Words), unembedded(definition.Meaning)
		if definition.Links == nil {
			definition.Links = []string{}
		}
	}
	defined.Definitions = reply.Definitions
	return defined, nil
}

// DefineQuestion is the side question Dispatch sends the writer session of a text of kind, whose
// id is id, for a person's Define of words in it (INFRA-3552's R4): what the session means by
// those words.
func DefineQuestion(id string, kind Kind, words []string) (string, error) {
	return render("define-question", questionData{ID: id, Kind: kind.spoken(), Words: words})
}
