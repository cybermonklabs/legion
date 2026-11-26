package contracts

import "regexp"

// StepAnswersFault is the first rule of a step record's answers that the answers break, or
// StepAnswersHold where they break none.
type StepAnswersFault int

const (
	// StepAnswersHold is answers that break no rule.
	StepAnswersHold StepAnswersFault = iota
	// StepAnswersTooMany is answers of more than StepAnswersMax entries.
	StepAnswersTooMany
	// StepAnswerModelForm is an entry whose model is outside the form of an agent's text
	// (StepAgentTextPattern).
	StepAnswerModelForm
	// StepAnswerNoCall is an entry of fewer than 1 call.
	StepAnswerNoCall
	// StepAnswerModelTwice is an entry whose model an earlier entry names.
	StepAnswerModelTwice
)

var stepAgentText = regexp.MustCompile(StepAgentTextPattern)

// CheckStepAnswers is the first rule of a step record's answers that answers break, and the index
// of the entry that breaks it. The index is -1 where the answers hold, or break the rule of their
// length. pair gives an entry's model and its calls. It is the one statement of the rule:
// Dispatch's step store refuses a write that breaks it, and the Legion daemon, through
// github.com/sjawhar/envoy/steprecord, refuses to queue one. Each caller refuses a fault with its
// own wording, naming its own fields.
func CheckStepAnswers[E any](answers []E, pair func(E) (model string, calls int64)) (StepAnswersFault, int) {
	if len(answers) > StepAnswersMax {
		return StepAnswersTooMany, -1
	}
	for i, answer := range answers {
		model, calls := pair(answer)
		switch {
		case !stepAgentText.MatchString(model):
			return StepAnswerModelForm, i
		case calls < 1:
			return StepAnswerNoCall, i
		}
		for _, earlier := range answers[:i] {
			if named, _ := pair(earlier); named == model {
				return StepAnswerModelTwice, i
			}
		}
	}
	return StepAnswersHold, -1
}
