package going

// decide.go holds the one decision call the command bar makes before it answers: the three
// questions put to the decision model and the state they are asked about. The server and the
// bake-off (cmd/going-bakeoff) both build their call from here, so the thresholds the bake-off
// fits are fitted on exactly what the server asks.

import (
	"encoding/json"

	"github.com/sjawhar/envoy/agentloop"
)

// The names the three questions are asked under, and so the names their answers come back
// under.
const (
	// QuestionAction asks which catalogue action the words ask for, or ActionNone.
	QuestionAction = "action"
	// QuestionContext asks whether the words point at the page's item, at items they name,
	// or at nothing.
	QuestionContext = "context"
	// QuestionDisplay asks whether the answer is best drawn as a chart or written as prose.
	QuestionDisplay = "display"
)

// ActionNone is the action choice for words that ask for nothing to change: the bar answers and
// acts on nothing. No catalogue action carries the name, since every one is a dispatch tool's.
const ActionNone = "none"

// The choices of the context question.
const (
	ContextPage  = "page"
	ContextNamed = "named"
	ContextNone  = "none"
)

// The choices of the display question.
const (
	DisplayChart = "chart"
	DisplayProse = "prose"
)

// Action is one action the decision model may choose: a catalogue action's name and the
// catalogue's own description of it, which is what the model chooses on. It is a plain pair
// rather than the catalogue's own type because the catalogue's package imports this one.
type Action struct {
	Name        string
	Description string
}

// The guidance the decision model is given beside the catalogue's descriptions. R5 corrects
// this text, and never a case's label, where a threshold does not fit.
const (
	actionInstructions = "The state holds the words a person typed into Dispatch's command bar and the page they typed them on. " +
		"Choose the one action the words ask Dispatch to perform, judged by the change the person wants made. " +
		"Choose none when the words ask a question or ask to find, read, list, count, summarise or show something, and ask for nothing to change."
	actionNoneCriterion = "The words ask a question, or ask to find, read, list, count, summarise or show something, and ask Dispatch to change nothing."

	contextInstructions = "The state holds the words a person typed into Dispatch's command bar and the page they typed them on. " +
		"Choose what the words are about."
	contextPageCriterion  = "The words are about the item of the page they were typed on, referred to as this, it, here or this issue, without naming another item."
	contextNamedCriterion = "The words name the items they are about themselves, such as an issue key like DEMO-12, a project key, a title, a person or a session."
	contextNoneCriterion  = "The words are about no particular item."

	displayInstructions = "The state holds the words a person typed into Dispatch's command bar and the page they typed them on. " +
		"Choose how the answer to the words is best shown."
	displayChartCriterion = "The answer is several numbers over one dimension, such as counts per week, per day, per project, per person or per status, " +
		"which a reader compares best drawn as a chart."
	displayProseCriterion = "The answer is a fact, a status, a single number, a list, an explanation or a change to make, which a reader takes best as words and links. " +
		"Every request to change something is answered in prose."
)

// DecisionQuestions are the three questions of one decision: action, a choice among actions and
// ActionNone whose criteria are each action's own description; context, a choice of the page's
// item, items the words name, or none; and display, a choice of chart or prose. Each answer
// carries its own confidence.
func DecisionQuestions(actions []Action) map[string]agentloop.Question {
	criteria := make(map[string]string, len(actions)+1)
	for _, action := range actions {
		criteria[action.Name] = action.Description
	}
	criteria[ActionNone] = actionNoneCriterion
	return map[string]agentloop.Question{
		QuestionAction: {Type: "choice", Instructions: actionInstructions, Criteria: choiceCriteria(criteria)},
		QuestionContext: {Type: "choice", Instructions: contextInstructions, Criteria: choiceCriteria(map[string]string{
			ContextPage:  contextPageCriterion,
			ContextNamed: contextNamedCriterion,
			ContextNone:  contextNoneCriterion,
		})},
		QuestionDisplay: {Type: "choice", Instructions: displayInstructions, Criteria: choiceCriteria(map[string]string{
			DisplayChart: displayChartCriterion,
			DisplayProse: displayProseCriterion,
		})},
	}
}

// choiceCriteria is a choice question's criteria: each choice's name and what it means. A map of
// strings always marshals, so the error is never set.
func choiceCriteria(choices map[string]string) json.RawMessage {
	raw, _ := json.Marshal(choices)
	return raw
}

// PageContext is where the words were typed: the page's path and the project, issue and block
// the bar's context chip names there. A field the page names nothing for is empty.
type PageContext struct {
	Path    string `json:"path"`
	Project string `json:"project,omitempty"`
	Issue   string `json:"issue,omitempty"`
	Block   string `json:"block,omitempty"`
}

// decisionState is the state of one decision as it rides the wire.
type decisionState struct {
	Words string      `json:"words"`
	Page  PageContext `json:"page"`
}

// DecisionState is the state the three questions are asked about: the words as typed and the
// page they were typed on. Strings always marshal, so it is always a JSON object.
func DecisionState(words string, page PageContext) json.RawMessage {
	raw, _ := json.Marshal(decisionState{Words: words, Page: page})
	return raw
}
