package api

// going_decision.go holds what a run of the command bar decides before it answers: the actions a
// caller may be offered, and the one decision call and the decision frame it becomes, with the
// line on top it gives. The run itself, which calls these in order, is startGoingRun's.

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The line on top of the bar's list (R2), as the action the decision model Jev chose gives it:
// Ask for no action, File as a new issue for the create of an issue, and Do for any other action
// of the catalogue (goingLineOnTopOf).
const (
	goingLineAsk  = "ask"
	goingLineDo   = "do"
	goingLineFile = "file"
)

// goingLineOnTopOf is the line on top action gives.
func goingLineOnTopOf(action string) string {
	switch action {
	case going.ActionNone:
		return goingLineAsk
	case goingIssueCreateTool:
		return goingLineFile
	}
	return goingLineDo
}

// goingCredential is what decides which rows of the catalogue a caller may be offered: whether
// the credential is a browser's cookie, which a bearer-only row refuses, the scopes it holds,
// the session it proved (session), which fills a row addressing the caller's own session, and
// whether it is presented with a proof bound to each request (a broker-issued access token under
// DPoP), which is no credential the command bar acts with: a run acts with a person's own
// credential (going_grant.go), and a proof is single use and bound to one URL.
type goingCredential struct {
	cookie     bool
	credential *model.Credential
	proofBound bool
}

// goingCredentialOf is the asking request's credential as goingCredential reads it.
func goingCredentialOf(r *http.Request, actor model.Actor, cookie bool) goingCredential {
	_, proofBound := ciphertoken.Presented(r)
	return goingCredential{cookie: cookie, credential: actor.Credential, proofBound: proofBound}
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
// offered a handoff or a reply to a delivery, which only a session's bearer may write. A caller
// whose token needs a proof per request is offered no action at all: the bar acts as a person's
// own credential, and a broker-issued token is not one.
func (c GoingCatalogue) runnableBy(caller goingCredential) []GoingAction {
	runnable := make([]GoingAction, 0, len(c.actions))
	if caller.proofBound {
		return runnable
	}
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

// goingRunStart is what a pause's answer is started with beyond its request row: the context chip,
// the actions its caller may be offered, which the decision chooses among, and the read tools its
// caller may call (goingReadsFor).
type goingRunStart struct {
	chip    *goingContext
	actions []GoingAction
	reads   []string
}

// goingDecision is the decision frame's data (contracts GoingDecisionEvent), which the run's
// record keeps as it was sent: the action the decision model chose, going.ActionNone among the
// choices, the line on top that action gives (goingLineOnTopOf), and what the words are about.
type goingDecision struct {
	Action  string `json:"action"`
	Line    string `json:"line" enum:"ask,do,file"`
	Context string `json:"context" enum:"page,named,none"`
}

// goingDecide makes the run's one decision call, the action asked among the actions the caller
// may run, and answers the decision it came to. It answers false - no decision, and so no
// decision frame - with no decider configured, and for a call that could not be made or answered
// nothing usable: the answer runs as it would with none, and the bar's line on top reads Ask.
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
	decision := goingDecision{Action: action.Choice, Line: goingLineOnTopOf(action.Choice), Context: going.ContextNone}
	if about, answered := result.Answers[going.QuestionContext]; answered &&
		slices.Contains([]string{going.ContextPage, going.ContextNamed, going.ContextNone}, about.Choice) {
		decision.Context = about.Choice
	}
	return decision, true
}

// goingNamedHit is the fast tier of R2: words that are an issue's key, or the very key, title or
// name of a hit the keyword search finds for them in project, whatever the case, are answered by
// that hit, and no model is asked of them. It answers whether the words are such words, with the
// hit they name where the search found one: words of a key's shape are a key whether or not an
// issue holds it. The search is the keyword statement alone (searchParameters.keywordsOnly), so
// it asks no embedding of the words either.
func (s *server) goingNamedHit(ctx context.Context, words, project string) (*going.Hit, bool, error) {
	key := issueKeyPattern.MatchString(strings.ToUpper(words))
	found, err := s.runSearch(ctx, searchParameters{text: words, project: project, limit: searchDefaultLimit, keywordsOnly: true})
	if err != nil {
		return nil, key, err
	}
	for _, result := range found.Results {
		if goingNamesHit(result, words) {
			return &going.Hit{Kind: result.Kind, ID: result.ID, Title: goingHitTitle(result), Href: result.Href}, true, nil
		}
	}
	return nil, key, nil
}

// goingNamesHit is whether words are result's own name, whatever the case: its issue's key or
// title, or its document's name, as the bar reads its hits.
func goingNamesHit(result model.SearchResult, words string) bool {
	owner := result.Owner
	if owner.Kind == "issue" {
		return strings.EqualFold(owner.Key, words) || strings.EqualFold(owner.Title, words)
	}
	return strings.EqualFold(owner.Name, words)
}

// GoingDecisionActions is actions as the decision model chooses among them: each action's name
// and the catalogue's own description of it, in the catalogue's order. The server's decision call
// and the bake-off (cmd/going-bakeoff) both put their action question through it, so the
// bake-off measures what the server asks.
func GoingDecisionActions(actions []GoingAction) []going.Action {
	choices := make([]going.Action, 0, len(actions))
	for _, action := range actions {
		choices = append(choices, going.Action{Name: action.Action, Description: action.Description})
	}
	return choices
}
