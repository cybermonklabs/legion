package api

// going_decision.go holds what a run of the command bar decides before it answers and offers
// once it has: the actions a caller may be offered, the one decision call and the decision frame
// it becomes, and the proposal frame the answer's arguments become. The run itself, which calls
// these in order, is startGoingRun's.

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingAlwaysAsk are the two writes the bar never runs at once, whatever the decision's
// confidence (R4): handing a request to a live agent spends that agent a turn, and a
// wrong new issue leaves a husk.
var goingAlwaysAsk = map[string]bool{"dispatch_send_to_agent": true, "dispatch_issue": true}

// goingItemFields are the catalogue arguments that name an item the search finds, by the kind
// the search lists it under: the bar prompts for one of these as an item it searches for rather
// than as text.
var goingItemFields = map[string]string{
	"issue":        "issue",
	"parent":       "issue",
	"ask":          "ask",
	"reply_to_ask": "ask",
	"comment":      "comment",
	"reply_to":     "comment",
	"artifact":     "document",
	"in_reply_to":  "message",
}

// The kinds of argument the bar can prompt for (R3): a text, a number, one of an enumeration's
// values, or an item the search finds.
const (
	goingPromptText   = "text"
	goingPromptNumber = "number"
	goingPromptEnum   = "enum"
	goingPromptItem   = "item"
)

// goingCredential is what decides which rows of the catalogue a caller may be offered: whether
// the credential is a browser's cookie, which a bearer-only row refuses, the scopes it holds,
// and the session it proved (session), which fills a row addressing the caller's own session.
type goingCredential struct {
	cookie     bool
	credential *model.Credential
}

// goingCredentialOf is the asking request's credential as goingCredential reads it.
func goingCredentialOf(actor model.Actor, cookie bool) goingCredential {
	return goingCredential{cookie: cookie, credential: actor.Credential}
}

// session is the session the credential proved, "" for none or no credential.
func (c goingCredential) session() string {
	if c.credential == nil {
		return ""
	}
	return c.credential.Session
}

// runnableBy reports whether the route table lets this caller call the row: a bearer-only row
// takes no cookie, and every scope the row needs is one the credential holds. It reads the table's
// own columns, so a row the table opens or closes later is offered or not without a name here.
func (route GoingRoute) runnableBy(caller goingCredential) bool {
	if route.auth == authBearer && caller.cookie {
		return false
	}
	for _, scope := range route.scopes {
		if !caller.credential.Holds(scope) {
			return false
		}
	}
	return true
}

// runnableBy is the catalogue as one caller may run it: each action keeps the rows
// the caller may call, and an action left with none is not offered at all, so a person is never
// offered a handoff or a reply to a delivery, which only a session's bearer may write.
func (c GoingCatalogue) runnableBy(caller goingCredential) []GoingAction {
	runnable := make([]GoingAction, 0, len(c.actions))
	for _, action := range c.actions {
		routes := make([]GoingRoute, 0, len(action.Routes))
		for _, route := range action.Routes {
			if route.runnableBy(caller) {
				routes = append(routes, route)
			}
		}
		if len(routes) == 0 {
			continue
		}
		action.Routes = routes
		runnable = append(runnable, action)
	}
	return runnable
}

// goingRunStart is what a run is started with beyond its request row: the context chip, the
// login its record is kept under ("" for a caller that is no person, whose run keeps no record),
// the actions its caller may be offered, the session its caller's credential proved, and the read
// tools its caller may call (goingReadsFor).
type goingRunStart struct {
	chip    *goingContext
	login   string
	actions []GoingAction
	session string
	reads   []string
}

// action is the runnable action of that name, and whether the caller may be offered one.
func (start goingRunStart) action(name string) (GoingAction, bool) {
	for _, action := range start.actions {
		if action.Action == name {
			return action, true
		}
	}
	return GoingAction{}, false
}

// goingDecision is the decision frame's data (contracts GoingDecisionEvent), which the run's
// record keeps as it was sent.
type goingDecision struct {
	Actions []goingDecidedAction `json:"actions"`
	Act     bool                 `json:"act"`
	Context string               `json:"context"`
	Display goingDecidedDisplay  `json:"display"`
	// offered are the actions the answer is told to write arguments for, best first: the two
	// best that are actions, or none where the decision is sure the words ask for none.
	offered []string
}

// goingDecidedAction is one of the two best choices: Confidence is the model's own confidence in
// its choice, the number the action threshold is fitted on and compared with, and is set on the
// chosen action alone; Probability is the share the model gave the choice, nil where it gave none.
type goingDecidedAction struct {
	Action      string   `json:"action"`
	Confidence  *float64 `json:"confidence"`
	Probability *float64 `json:"probability"`
}

// goingDecidedDisplay is the display choice with its confidence, and Chart whether that choice
// draws a chart unasked: a chart at or above the measured chart-display threshold.
type goingDecidedDisplay struct {
	Choice     string  `json:"choice"`
	Confidence float64 `json:"confidence"`
	Chart      bool    `json:"chart"`
}

// goingDecide makes the run's one decision call, the action asked among the
// actions the caller may run, and answers the decision it came to. It answers false - no
// decision, and so no decision frame and no proposal - with no decider configured, and for a call
// that could not be made or answered nothing usable: the answer runs as it would with none.
func (s *server) goingDecide(ctx context.Context, words string, start goingRunStart) (goingDecision, bool) {
	decider := s.deps.Decider
	if decider == nil {
		return goingDecision{}, false
	}
	known := make(map[string]bool, len(start.actions)+1)
	for _, action := range start.actions {
		known[action.Action] = true
	}
	known[going.ActionNone] = true
	var page going.PageContext
	if start.chip != nil {
		page = going.PageContext{Path: start.chip.Path, Project: start.chip.Project, Issue: start.chip.Issue, Block: start.chip.Block}
	}
	result, err := decider.Decide(ctx, going.DecisionQuestions(GoingDecisionActions(start.actions)), going.DecisionState(words, page))
	switch {
	case err != nil:
		slog.Warn("dispatch going: the decision call could not be made, so the run answers with no decision", "error", err)
		return goingDecision{}, false
	case result.Fault != "":
		slog.Warn("dispatch going: the decision call gave no answer, so the run answers with no decision",
			"fault", result.Fault, "detail", result.Detail)
		return goingDecision{}, false
	}
	action, answered := result.Answers[going.QuestionAction]
	if !answered || !known[action.Choice] {
		slog.Warn("dispatch going: the decision named no action it was offered, so the run answers with no decision")
		return goingDecision{}, false
	}
	record := decider.Record()
	decision := goingDecision{
		Actions: goingTopTwo(action, known),
		Context: going.ContextNone,
		Display: goingDecidedDisplay{Choice: going.DisplayProse},
	}
	if about, answered := result.Answers[going.QuestionContext]; answered &&
		slices.Contains([]string{going.ContextPage, going.ContextNamed, going.ContextNone}, about.Choice) {
		decision.Context = about.Choice
	}
	if display, answered := result.Answers[going.QuestionDisplay]; answered &&
		(display.Choice == going.DisplayChart || display.Choice == going.DisplayProse) {
		decision.Display.Choice, decision.Display.Confidence = display.Choice, display.Confidence
	}
	// Like for like: each threshold was fitted on the confidence the model reports for its
	// choice, the gap between a two-way choice's two probabilities, so it is that number and no
	// probability that is compared with it.
	sure := record.ActionThreshold != nil && action.Confidence >= *record.ActionThreshold
	decision.Act = action.Choice != going.ActionNone && sure && !goingAlwaysAsk[action.Choice]
	decision.Display.Chart = decision.Display.Choice == going.DisplayChart && record.ChartThreshold != nil &&
		decision.Display.Confidence >= *record.ChartThreshold && !s.displayFitOff.Load()
	// Words the decision is sure ask for nothing to change are offered nothing; below that,
	// the two best actions are offered, as R4 offers them.
	if action.Choice != going.ActionNone || !sure {
		for _, decided := range decision.Actions {
			if decided.Action != going.ActionNone {
				decision.offered = append(decision.offered, decided.Action)
			}
		}
	}
	return decision, true
}

// GoingDecisionActions is actions as the decision model chooses among them: each action's name
// and the catalogue's own description of it, in the catalogue's order. The server's decision call
// and the bake-off (cmd/going-bakeoff) both put their action question through it, so the
// thresholds the bake-off fits are fitted on what the server asks.
func GoingDecisionActions(actions []GoingAction) []going.Action {
	choices := make([]going.Action, 0, len(actions))
	for _, action := range actions {
		choices = append(choices, going.Action{Name: action.Action, Description: action.Description})
	}
	return choices
}

// goingTopTwo is the model's own choice, then the choice it gave the most probability among the
// rest of those it was offered, where it gave any: the two best actions R4 offers.
func goingTopTwo(action agentloop.Decision, known map[string]bool) []goingDecidedAction {
	confidence := action.Confidence
	top := []goingDecidedAction{{Action: action.Choice, Confidence: &confidence, Probability: goingProbability(action, action.Choice)}}
	runnersUp := make([]string, 0, len(action.Probabilities))
	for choice := range action.Probabilities {
		if choice != action.Choice && known[choice] {
			runnersUp = append(runnersUp, choice)
		}
	}
	if len(runnersUp) == 0 {
		return top
	}
	// Ties go to the name, so one answer always reads as one ranking.
	slices.SortFunc(runnersUp, func(left, right string) int {
		return cmp.Or(cmp.Compare(action.Probabilities[right], action.Probabilities[left]), cmp.Compare(left, right))
	})
	return append(top, goingDecidedAction{Action: runnersUp[0], Probability: goingProbability(action, runnersUp[0])})
}

// goingProbability is the probability the answer gave choice, nil where it gave none.
func goingProbability(action agentloop.Decision, choice string) *float64 {
	probability, given := action.Probabilities[choice]
	if !given {
		return nil
	}
	return &probability
}

// offer is what the answer call is told of this decision: each offered action with its own
// argument schema, the catalogue's, and the display settled on.
func (d goingDecision) offer(start goingRunStart) going.Offer {
	offer := going.Offer{Display: going.DisplayProse}
	if d.Display.Chart {
		offer.Display = going.DisplayChart
	}
	for _, name := range d.offered {
		action, runnable := start.action(name)
		if !runnable {
			continue
		}
		// A slice of ToolField always marshals.
		schema, _ := json.Marshal(action.Request)
		offer.Actions = append(offer.Actions, going.OfferedAction{Name: name, Schema: schema})
	}
	return offer
}

// goingProposal is the proposal frame's data (contracts GoingProposalEvent), which the run's
// record keeps as it was sent.
type goingProposal struct {
	Offers []goingOffer `json:"offers"`
}

// goingOffer is one offered action: one invocation for each item it changes, and Offered false
// where none survived the checks.
type goingOffer struct {
	Action      string            `json:"action"`
	Invocations []goingInvocation `json:"invocations"`
	Offered     bool              `json:"offered"`
}

// goingInvocation is one run of an action: the arguments the answer and the page filled, each
// checked against the action's own schema, and the one argument the bar still has to ask for.
type goingInvocation struct {
	Arguments map[string]any        `json:"arguments"`
	Missing   *goingMissingArgument `json:"missing"`
}

// goingMissingArgument is the one argument an invocation still needs, of a kind the bar prompts
// for: a text, a number, one of Choices, or an item of the search kind Item.
type goingMissingArgument struct {
	Field   string   `json:"field"`
	Kind    string   `json:"kind"`
	Choices []string `json:"choices,omitempty"`
	Item    string   `json:"item,omitempty"`
}

// goingPropose is the proposal of a decision once the answer has written: for each action the
// decision offered, the answer's arguments for it, each element held to that
// action's own schema, a target the words left out filled from the chip where the decision read
// the words as about the page, and every invocation that still needs more than one argument, or
// one the bar cannot prompt for, dropped. An action the answer wrote arguments for that the
// decision did not offer - one outside the catalogue among them - is no part of the proposal and
// so never runs. With no structured reply (final nil), or no arguments for an
// action, the action is offered on what the page and one prompted argument fill.
func goingPropose(decision goingDecision, final *going.Final, start goingRunStart) goingProposal {
	written := map[string]json.RawMessage{}
	if final != nil && final.Arguments != "" {
		if err := json.Unmarshal([]byte(final.Arguments), &written); err != nil {
			// Text that is no object of lists offers nothing written: each action is offered
			// on what the page fills, as with no structured reply.
			written = map[string]json.RawMessage{}
		}
	}
	var chip *goingContext
	if decision.Context == going.ContextPage {
		chip = start.chip
	}
	proposal := goingProposal{Offers: []goingOffer{}}
	for _, name := range decision.offered {
		action, runnable := start.action(name)
		if !runnable {
			continue
		}
		var elements []json.RawMessage
		if raw, wrote := written[name]; wrote {
			if err := json.Unmarshal(raw, &elements); err != nil {
				elements = nil
			}
		}
		calls := make([]map[string]any, 0, max(len(elements), 1))
		for _, element := range elements {
			var arguments map[string]any
			if err := json.Unmarshal(element, &arguments); err != nil || arguments == nil {
				continue
			}
			calls = append(calls, arguments)
		}
		if len(elements) == 0 {
			calls = append(calls, map[string]any{})
		}
		offer := goingOffer{Action: name, Invocations: []goingInvocation{}}
		for _, arguments := range calls {
			if invocation, kept := goingInvocationOf(action, arguments, chip, start.session); kept {
				offer.Invocations = append(offer.Invocations, invocation)
			}
		}
		offer.Offered = len(offer.Invocations) > 0
		proposal.Offers = append(proposal.Offers, offer)
	}
	return proposal
}

// goingInvocationOf holds one element of the answer's arguments to its action's schema and fills
// what it can: every argument it names must be one the action takes and hold what that argument
// holds, or the element is dropped; then each argument still missing that the chip names is
// filled from the chip; then what is left missing - a required argument, or what the closest row
// of the action needs in its path - is at most one argument the bar can prompt for, or the
// element is dropped.
func goingInvocationOf(action GoingAction, arguments map[string]any, chip *goingContext, session string) (goingInvocation, bool) {
	fields := make(map[string]contracts.ToolField, len(action.Request))
	for _, field := range action.Request {
		fields[field.Name] = field
	}
	for name, value := range arguments {
		field, takes := fields[name]
		if !takes || checkGoingValue(field, value) != "" {
			return goingInvocation{}, false
		}
	}
	missing, routed := goingMissingArguments(action, arguments, session)
	if !routed {
		return goingInvocation{}, false
	}
	if chip != nil {
		for _, name := range missing {
			if value := goingChipValue(chip, name); value != "" {
				arguments[name] = value
			}
		}
		missing, routed = goingMissingArguments(action, arguments, session)
		if !routed {
			return goingInvocation{}, false
		}
	}
	switch len(missing) {
	case 0:
		if checkGoingArguments(action.Request, arguments) != "" {
			return goingInvocation{}, false
		}
		return goingInvocation{Arguments: arguments}, true
	case 1:
		prompt, promptable := goingPromptFor(fields, missing[0])
		if !promptable {
			return goingInvocation{}, false
		}
		return goingInvocation{Arguments: arguments, Missing: &prompt}, true
	}
	return goingInvocation{}, false
}

// goingMissingArguments is what a call with these arguments still needs: every required argument
// it does not name, and what the row it comes closest to reaching needs in its path besides,
// goingCallerSession standing for the caller's own session where the credential proved none. It
// answers false where no row of the action takes the call at all, whatever is filled: a guard
// such as a claim's release picks no row the caller may call.
func goingMissingArguments(action GoingAction, arguments map[string]any, session string) ([]string, bool) {
	var required []string
	for _, field := range action.Request {
		if _, named := arguments[field.Name]; field.Required && !named {
			required = append(required, field.Name)
		}
	}
	var closest []string
	routed := false
	for _, route := range action.Routes {
		if !route.takes(arguments) {
			continue
		}
		needs := slices.Clone(required)
		for _, argument := range route.path {
			if argument == goingCallerSession {
				if session == "" && !slices.Contains(needs, goingCallerSession) {
					needs = append(needs, goingCallerSession)
				}
				continue
			}
			if text, isText := arguments[argument].(string); (!isText || text == "") && !slices.Contains(needs, argument) {
				needs = append(needs, argument)
			}
		}
		if !routed || len(needs) < len(closest) {
			closest, routed = needs, true
		}
	}
	slices.Sort(closest)
	return closest, routed
}

// goingChipValue is the value the context chip holds for one argument: the issue the page names
// fills an issue argument, and its project a project argument. Nothing else is read off a page.
func goingChipValue(chip *goingContext, argument string) string {
	switch argument {
	case "issue":
		return chip.Issue
	case "project":
		return chip.Project
	}
	return ""
}

// goingPromptFor is how the bar asks for one missing argument, and false for one it cannot ask
// for: a boolean, a list or an object. A session the caller's own credential did not prove is
// asked for by the name the person knows it by, which the write resolves.
func goingPromptFor(fields map[string]contracts.ToolField, name string) (goingMissingArgument, bool) {
	if name == goingCallerSession {
		return goingMissingArgument{Field: name, Kind: goingPromptText}, true
	}
	field, takes := fields[name]
	if !takes {
		return goingMissingArgument{}, false
	}
	switch field.Kind {
	case contracts.ToolFieldString:
		if kind, item := goingItemFields[name]; item {
			return goingMissingArgument{Field: name, Kind: goingPromptItem, Item: kind}, true
		}
		return goingMissingArgument{Field: name, Kind: goingPromptText}, true
	case contracts.ToolFieldNumber:
		return goingMissingArgument{Field: name, Kind: goingPromptNumber}, true
	case contracts.ToolFieldEnum:
		return goingMissingArgument{Field: name, Kind: goingPromptEnum, Choices: field.Values}, true
	}
	return goingMissingArgument{}, false
}

// goingDisplayFit is the display-fit test hook's state: whether the server treats the configured
// decision model as having no display fit.
type goingDisplayFit struct {
	Off bool `json:"off"`
}

// goingDisplayFitInput is POST /api/v1/going/display-fit/_test's body, a type of its own for the
// reason newIssueSettingsInput is one: a request's required field and a response's are reflected
// apart.
type goingDisplayFitInput struct {
	Off *bool `json:"off" required:"true"`
}

// setGoingDisplayFit is POST /api/v1/going/display-fit/_test, {off}: with off true this process
// treats the configured decision model as having no display fit, so no decision draws a chart
// unasked, which is "the chart-display threshold unset" of a deployment whose model has none, and
// with off false it reads the record's threshold again. A body without off is 400 INVALID_STATE,
// as the settings route answers one without confirm_writes. It removes a measured number and adds
// none, and acts on the process the call lands on, as the new-issue settings hook does.
// Test-only: mounted only under Deps.TestHooksEnabled.
func (s *server) setGoingDisplayFit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input goingDisplayFitInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Off == nil {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "off is required: true or false")
		return
	}
	s.displayFitOff.Store(*input.Off)
	WriteJSON(w, http.StatusOK, goingDisplayFit{Off: *input.Off})
}
