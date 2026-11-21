package api

// going_catalogue.go holds the catalogue of the dispatch tools that write, which GoingCaller runs
// as its caller, and serves at GET /api/v1/going/catalogue the actions an agent may propose on
// Dispatch's graph.
//
// The catalogue holds every write tool of goingWriteTools (going_tools.go), each carried whole
// through contracts.DispatchTools from its spec in packages/contracts/src/dispatch-tools.ts: its
// own curated description, which is what a model chooses on, and its own argument schema. The
// actions it serves and the answer run's decision offers (Actions) are the twenty tools the
// table marks Proposed, which is under the count where a model's choice measurably falls apart,
// so that list is read whole: nothing searches it, and no second schema is authored per route.
// dispatch_review is no action of them, on purpose: a review records an approval or a changes
// request in a person's name, and the decision that proposes operations on Dispatch's graph must
// never propose one.
//
// Which tools write is not a field a tool spec carries. goingWriteTools names the rows of the
// server's own table each tool writes, and buildGoingCatalogue holds every one of them to the
// table it mounts: a row that is no row of that table, one the table does not class as a write
// (mutatingRoutes, the set TestEveryMutatingRouteIsClassed reads), a placeholder nothing fills,
// or a relation in neither edge family refuses to mount (register). The other direction is
// TestEveryPersonRouteHasATool's: every route a person may call is a row of a tool or a route
// goingExcludedRoutes names.
//
// Every row also carries what its route takes of a call (going_translate.go) and how a write of
// it is undone (GoingUndo, which going_undo.go runs), and the builder refuses a row missing
// either, so no row is left that cannot run or whose undo is unstated.
//
// Nothing here decides anything: the catalogue says what an action is and what it takes, and
// GoingCaller performs one already chosen. Which actions a run offers, and with what arguments,
// is going_decision.go's; running one as its caller and undoing it is going_writes.go's and
// going_undo.go's.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// goingCataloguePath is the route that serves the catalogue, named once so the refusal of an
// action the catalogue does not hold can point at the list without spelling it again.
const goingCataloguePath = "/api/v1/going/catalogue"

// goingCallerSession stands, where a row's path binding names it, for the agent session the
// caller acts as rather than for an argument of the tool: a follow records the calling session
// as the ask's follower, and a tool argument names no session at all.
const goingCallerSession = "@session"

// goingSendIssue stands, where a row's path binding names it, for the issue a send to an agent
// session is posted on (GoingCaller.sendIssue, going_messages.go) rather than for an argument of
// the tool: a send to a session that works on an issue goes on that issue's conversation. A
// caller that names no such issue fills none, and the call takes the tool's next row.
const goingSendIssue = "@issue"

// GoingUndo is how a write of one row is undone, as the catalogue serves it: the kind, and every
// row of the table the undo writes through, none for a write that is not undone.
type GoingUndo struct {
	Kind   goingUndoKind `json:"kind" enum:"reverse,write_back,close_created,not_undone"`
	Routes []string      `json:"routes"`
}

// GoingRoute is one row of the server's table an action writes, as the catalogue serves it:
// the row, and how a write of it is undone.
type GoingRoute struct {
	Method  string    `json:"method"`
	Pattern string    `json:"pattern"`
	Undo    GoingUndo `json:"undo"`

	// path and the guard are how a call reaches this row (goingToolRoute), translate what the
	// row's route takes of the call, and auth and scopes are who the table lets call it
	// (routeAuth, routeScopes), which decides whether a caller is offered it at all
	// (GoingRoute.runnableBy); they are the caller's business rather than the reader's, so none is
	// served.
	path         map[string]string
	whenArgument string
	whenValue    any
	translate    goingTranslator
	auth         routeAuth
	scopes       routeScopeSet
}

// GoingAction is one action an agent may propose: a dispatch tool that writes, with the
// description and the arguments the tool spec itself states.
type GoingAction struct {
	// Action is the catalogue's key: the tool's own name, such as "dispatch_issue".
	Action string `json:"action"`
	// Description is the tool spec's own description, whole and unchanged. It is what a model
	// chooses on, and the model bake-off measured the decision on these words, so nothing here
	// shortens or rewrites them.
	Description string `json:"description"`
	// Routes are the rows of the server's table this action writes, in the order a call is
	// matched against them.
	Routes []GoingRoute `json:"routes"`
	// Relations is every graph relation this action's verbs write, over all its rows, each a
	// structural edge kind (model.GraphEdgeKinds) or a `block[]` attribute of a typed block
	// (pmdoc.LinkKinds).
	Relations []string `json:"relations,omitempty"`
	// Request is the action's arguments: the tool spec's own schema.
	Request []contracts.ToolField `json:"request"`
	// Example is a call this action's schema accepts, the tool spec's own.
	Example json.RawMessage `json:"example"`
}

// GoingCatalogue is every write tool GoingCaller runs, and the actions of them an agent may
// propose.
type GoingCatalogue struct {
	actions []GoingAction
	// byAction is the index into actions of each action's name.
	byAction map[string]int
	// proposed is the actions of the tools goingWriteTools marks Proposed.
	proposed []GoingAction
}

// Actions is the actions an agent may propose, in the order the tool specs state the tools: the
// tools goingWriteTools marks Proposed, which GET /api/v1/going/catalogue serves and the answer
// run's decision offers. Lookup holds every write tool.
func (c GoingCatalogue) Actions() []GoingAction {
	return c.proposed
}

// Lookup is the action that name holds, and whether the catalogue holds one at all.
func (c GoingCatalogue) Lookup(name string) (GoingAction, bool) {
	index, held := c.byAction[name]
	if !held {
		return GoingAction{}, false
	}
	return c.actions[index], true
}

// mutatingRoutes is every row of the table that changes something, in the shape
// TestEveryMutatingRouteIsClassed reads the table in: a row whose method is not GET, each of
// which writeClasses must class (undecidableWrites). It is the one classification of a write the
// server has, and the catalogue is held to it.
func mutatingRoutes(routes []apiRoute) map[string]bool {
	mutating := make(map[string]bool, len(routes))
	for _, route := range routes {
		if route.Method == http.MethodGet {
			continue
		}
		mutating[route.Method+" "+route.Pattern] = true
	}
	return mutating
}

// edgeFamilies is every relation name a catalogue entry may declare: the global relation names
// and the `block[]` attributes of the typed blocks, which are the two families of edge the graph
// holds and the two GET /api/v1/references?kind= accepts (references.go).
func edgeFamilies() []string {
	return append(slices.Clone(model.GraphEdgeKinds), pmdoc.LinkKinds()...)
}

// goingPlaceholders is the placeholders of a pattern, in the order it spells them.
func goingPlaceholders(pattern string) []string {
	var placeholders []string
	for rest := pattern; ; {
		opened := strings.Index(rest, "{")
		if opened < 0 {
			return placeholders
		}
		rest = rest[opened+1:]
		closed := strings.Index(rest, "}")
		if closed < 0 {
			return placeholders
		}
		placeholders = append(placeholders, rest[:closed])
		rest = rest[closed+1:]
	}
}

// BuildGoingCatalogue is the catalogue of the server's own route table, with no test hooks: what
// `envoy-dispatch` serves and what a caller outside a request builds. register builds the
// server's from the table it mounts.
func BuildGoingCatalogue() (GoingCatalogue, error) {
	return buildGoingCatalogue((&server{}).routes())
}

// buildGoingCatalogue is one entry per dispatch tool that writes, and an error naming every row
// the catalogue cannot hold: a row that is no row of this table, a row the table does not class
// as a write, a placeholder no argument of the tool fills, a relation in neither edge family, a
// row with no translation, a row whose undo is none of the four or writes through a row that is
// no write of this table, a tool whose entry and spec disagree on its rows (goingToolRows), a
// spec that binds its routes and that neither table names, and a tool name no tool spec has.
// register refuses to mount a table it answers an error for, so a catalogue entry can only ever
// name an action the server serves, and every row states how its write is undone.
func buildGoingCatalogue(routes []apiRoute) (GoingCatalogue, error) {
	mutating := mutatingRoutes(routes)
	served := make(map[string]apiRoute, len(routes))
	for _, route := range routes {
		served[route.Method+" "+route.Pattern] = route
	}
	families := edgeFamilies()
	catalogue := GoingCatalogue{
		actions:  make([]GoingAction, 0, len(goingWriteTools)),
		byAction: make(map[string]int, len(goingWriteTools)),
	}
	var refusals []string
	for _, tool := range contracts.DispatchTools {
		written, writes := goingWriteTools[tool.Name]
		if !writes {
			if _, reads := goingReadTools[tool.Name]; !reads && len(tool.Routes) > 0 {
				refusals = append(refusals, tool.Name+": its spec binds its routes, and neither goingWriteTools nor goingReadTools names it")
			}
			continue
		}
		rows, agree := goingToolRows(tool, written.Routes, written.UndoRoutes)
		if !agree {
			refusals = append(refusals, tool.Name+": its rows are stated by its entry and its spec's routes both, or by neither")
		}
		takes := make(map[string]bool, len(tool.Arguments))
		for _, argument := range tool.Arguments {
			takes[argument.Name] = true
		}
		entry := GoingAction{
			Action:      tool.Name,
			Description: tool.Description,
			Routes:      make([]GoingRoute, 0, len(rows)),
			Request:     tool.Arguments,
			Example:     tool.Example,
		}
		for _, row := range rows {
			name := row.Method + " " + row.Pattern
			mounted, isServed := served[name]
			switch {
			case !isServed:
				refusals = append(refusals, fmt.Sprintf("%s: %s is no row of the route table", tool.Name, name))
			case !mutating[name]:
				refusals = append(refusals, fmt.Sprintf("%s: %s is no write of the route table", tool.Name, name))
			}
			refusals = append(refusals, goingBindingProblems(tool.Name, row, takes)...)
			for _, relation := range row.Relations {
				if !slices.Contains(families, relation) {
					refusals = append(refusals, fmt.Sprintf("%s: relation %q is neither a structural edge kind (%s) nor a block[] attribute (%s)",
						tool.Name, relation, strings.Join(model.GraphEdgeKinds, ", "), strings.Join(pmdoc.LinkKinds(), ", ")))
				}
				if !slices.Contains(entry.Relations, relation) {
					entry.Relations = append(entry.Relations, relation)
				}
			}
			if row.Translate == nil {
				refusals = append(refusals, fmt.Sprintf("%s: %s takes no translation of the call's arguments", tool.Name, name))
			}
			undo := GoingUndo{Kind: written.Undo, Routes: row.UndoRoutes}
			if undo.Routes == nil {
				undo.Routes = []string{}
			}
			refusals = append(refusals, goingUndoProblems(tool.Name+": "+name, undo, mutating)...)
			entry.Routes = append(entry.Routes, GoingRoute{
				Method:       row.Method,
				Pattern:      row.Pattern,
				Undo:         undo,
				path:         row.Path,
				whenArgument: row.WhenArgument,
				whenValue:    row.WhenValue,
				translate:    row.Translate,
				auth:         mounted.Auth,
				scopes:       mounted.OAuthScopes,
			})
		}
		catalogue.byAction[tool.Name] = len(catalogue.actions)
		catalogue.actions = append(catalogue.actions, entry)
		if written.Proposed {
			catalogue.proposed = append(catalogue.proposed, entry)
		}
	}
	for name := range goingWriteTools {
		if _, held := catalogue.byAction[name]; !held {
			refusals = append(refusals, name+": no dispatch tool of that name (packages/contracts/src/dispatch-tools.ts)")
		}
		if _, reads := goingReadTools[name]; reads {
			refusals = append(refusals, name+": goingWriteTools and goingReadTools both name it")
		}
	}
	if len(refusals) > 0 {
		sort.Strings(refusals)
		return GoingCatalogue{}, fmt.Errorf("%d catalogue entry problem(s):\n%s", len(refusals), strings.Join(refusals, "\n"))
	}
	return catalogue, nil
}

// goingBindingProblems is what is wrong with how one row of tool binds a call's arguments: a
// placeholder its Path binds nothing to, a binding of no placeholder, a binding to an argument the
// tool does not take (takes), and a guard on such an argument.
func goingBindingProblems(tool string, row goingToolRoute, takes map[string]bool) []string {
	name := row.Method + " " + row.Pattern
	var problems []string
	for _, placeholder := range goingPlaceholders(row.Pattern) {
		if _, bound := row.Path[placeholder]; !bound {
			problems = append(problems, fmt.Sprintf("%s: %s binds nothing to {%s}", tool, name, placeholder))
		}
	}
	for placeholder, argument := range row.Path {
		if !strings.Contains(row.Pattern, "{"+placeholder+"}") {
			problems = append(problems, fmt.Sprintf("%s: %s has no placeholder {%s}", tool, name, placeholder))
		}
		if argument != goingCallerSession && argument != goingSendIssue && !takes[argument] {
			problems = append(problems, fmt.Sprintf("%s: {%s} is bound to %q, which is no argument of the tool", tool, placeholder, argument))
		}
	}
	if row.WhenArgument != "" && !takes[row.WhenArgument] {
		problems = append(problems, fmt.Sprintf("%s: %s is picked by %q, which is no argument of the tool", tool, name, row.WhenArgument))
	}
	return problems
}

// goingUndoProblems is what is wrong with one row's undo: a kind that is none of the four, a
// write that is not undone and names a route it would write, any other kind that names none,
// and a route named that is no write of the table.
func goingUndoProblems(row string, undo GoingUndo, mutating map[string]bool) []string {
	var problems []string
	switch undo.Kind {
	case goingUndoNotUndone:
		if len(undo.Routes) > 0 {
			problems = append(problems, fmt.Sprintf("%s: its write is not undone, so its undo writes nothing, and names %s", row, strings.Join(undo.Routes, ", ")))
		}
	case goingUndoReverse, goingUndoWriteBack, goingUndoCloseCreated:
		if len(undo.Routes) == 0 {
			problems = append(problems, fmt.Sprintf("%s: its undo %s names no route it writes through", row, undo.Kind))
		}
	default:
		problems = append(problems, fmt.Sprintf("%s: its undo %q is none of %s, %s, %s and %s", row, undo.Kind,
			goingUndoReverse, goingUndoWriteBack, goingUndoCloseCreated, goingUndoNotUndone))
	}
	for _, route := range undo.Routes {
		if !mutating[route] {
			problems = append(problems, fmt.Sprintf("%s: its undo writes through %s, which is no write of the route table", row, route))
		}
	}
	return problems
}

// goingCatalogueBody is what GET /api/v1/going/catalogue answers: every action, each carrying
// the description it is chosen on and the arguments it takes.
type goingCatalogueBody struct {
	Total   int           `json:"total"`
	Actions []GoingAction `json:"actions"`
}

// getGoingCatalogue serves the catalogue whole. Twenty actions is a list a reader scans, so
// there is nothing to search and nothing to page.
func (s *server) getGoingCatalogue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	actions := s.going.Actions()
	WriteJSON(w, http.StatusOK, goingCatalogueBody{Total: len(actions), Actions: actions})
}

// GoingDoer is the one thing a caller needs of an HTTP client, so a test watches what was sent
// and whether anything was sent at all.
type GoingDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// GoingCaller performs an action an agent already chose. It holds the catalogue to the proposal
// before it builds anything: an action the catalogue does not hold, a call the action's schema
// refuses, a call no row of the action takes, and a call the bar can never send as written
// (goingUnsendable), each answer an error of their own with no request made. What the chosen
// row's route takes is that row's translation's to say (goingToolRoute.Translate), and every read
// the translation makes to say it - a document's id from its slug, a session's id from its title -
// goes through the same Doer as the write.
type GoingCaller struct {
	Catalogue GoingCatalogue
	// BaseURL is the Dispatch server the action is performed against, without a trailing slash;
	// empty for the server's own route table called in-process (server.goingDoer).
	BaseURL string
	// Session is the agent session the caller acts as, which fills the rows addressing it: a
	// follow records this session as the ask's follower. A caller that is no session leaves it
	// empty, and the rows needing one are passed over.
	Session string
	Doer    GoingDoer

	// resumeAfter says what of a call's first requests an earlier try of its call landed
	// (goingLandedBefore), which a translation's own refusal words for what of the write already
	// stands (goingCallInput.resumes); nil for none.
	resumeAfter func(specs []goingSend) []goingWriteRequest
	// knownSession is the listener's own lookup of one session, running or not
	// (server.goingKnownSession), which a send to an agent resolves its recipient by
	// (goingReader.recipient); nil for a call to another server's table, which reads the roster
	// alone.
	knownSession func(ctx context.Context, id string) (bool, error)
	// sendIssue is the issue a send to an agent session is posted on, which fills the rows that
	// bind goingSendIssue: the issue a row of a run's plan names (going_messages.go). Empty for a
	// call that names none, which takes the tool's row that sends to the session itself.
	sendIssue string
}

// GoingUnknownActionError is the refusal of an action the catalogue does not hold. It is the
// caller's own error, raised before any request: there is nothing to send, since nothing names
// what to send it to.
type GoingUnknownActionError struct {
	Action string
}

func (e GoingUnknownActionError) Error() string {
	return fmt.Sprintf("%q is no action of the catalogue: GET %s lists every action", e.Action, goingCataloguePath)
}

// GoingArgumentError is the refusal of a call the action's own schema does not accept, that no row
// of the action takes, or that the bar can never send as written (goingUnsendable, a translation's
// goingUntranslatable), raised before any request for the same reason. Reason is the refusal
// alone, which the bar's panel shows as it stands; Error names the action before it, for a caller
// that holds no line naming the action.
type GoingArgumentError struct {
	Action string
	Reason string
}

func (e GoingArgumentError) Error() string {
	return fmt.Sprintf("%s: %s", e.Action, e.Reason)
}

// goingPrepared is one call made ready to send: the action, the row it performs, the arguments
// it was given and what the row's translation made of them.
type goingPrepared struct {
	action    string
	route     GoingRoute
	arguments map[string]any
	call      goingCall
}

// Do performs one catalogue action with the arguments given: the action's first row the call
// fills (chooseGoingRoute), with what that row's translation makes of the arguments. Nothing is
// sent until the catalogue holds the action and the tool's own schema accepts the call; a write
// that must land first (a reasoned close's reason) is sent first, and a refusal of it is the
// answer, with the row's own write not sent.
func (c GoingCaller) Do(ctx context.Context, action string, arguments map[string]any) (*http.Response, error) {
	prepared, err := c.prepare(ctx, action, arguments)
	if err != nil {
		return nil, err
	}
	response, _, err := c.send(ctx, prepared, nil)
	return response, err
}

// prepare holds a call to the catalogue and the action's schema, chooses its row, refuses a call
// the bar can never send (goingUnsendable) and translates it, reading what the translation needs
// through the Doer, and sends no write.
func (c GoingCaller) prepare(ctx context.Context, action string, arguments map[string]any) (goingPrepared, error) {
	entry, held := c.Catalogue.Lookup(action)
	if !held {
		return goingPrepared{}, GoingUnknownActionError{Action: action}
	}
	if reason := checkGoingArguments(entry.Request, arguments); reason != "" {
		return goingPrepared{}, GoingArgumentError{Action: action, Reason: reason}
	}
	choosing := arguments
	if c.sendIssue != "" {
		// The issue fills its binding alone: the tool's schema took no argument of the name.
		choosing = maps.Clone(arguments)
		choosing[goingSendIssue] = c.sendIssue
	}
	route, values, chosen := chooseGoingRoute(entry, choosing, c.Session)
	if !chosen {
		return goingPrepared{}, GoingArgumentError{Action: action, Reason: goingNoRouteReason(entry)}
	}
	if why := goingUnsendable(action, arguments); why != "" {
		return goingPrepared{}, GoingArgumentError{Action: action, Reason: why}
	}
	call, err := route.translate(ctx, c.reader(), goingCallInput{arguments: arguments, path: values, session: c.Session, resumed: c.resumeAfter})
	if err != nil {
		var untranslatable goingUntranslatable
		if errors.As(err, &untranslatable) {
			return goingPrepared{}, GoingArgumentError{Action: action, Reason: string(untranslatable)}
		}
		return goingPrepared{}, err
	}
	return goingPrepared{action: action, route: route, arguments: arguments, call: call}, nil
}

// send sends a prepared call: each write that must land first, then the row's own, and answers
// the answer of the request that ended it - the row's own, or one sent first that did not land -
// beside how each request it sent went (goingWriteRequest). A request landed where it was
// answered below 300. landed is the requests an earlier write of the same invocation already
// landed that the call starts with: each the call still starts with, by method, path and body
// (goingSameSend), is not sent again and stands in the record as it was, and the call resumes at
// the first after them. A call every one of whose requests landed before sends nothing and answers
// no response. A request that could not be built or sent ends the call with its error and the
// requests before it.
func (c GoingCaller) send(ctx context.Context, prepared goingPrepared, landed []goingWriteRequest) (*http.Response, []goingWriteRequest, error) {
	requests, err := prepared.call.requests(prepared.route)
	if err != nil {
		return nil, nil, fmt.Errorf("encode %s: %w", prepared.action, err)
	}
	sent := make([]goingWriteRequest, 0, len(requests))
	resuming := true
	for index, spec := range requests {
		resuming = resuming && index < len(landed) && goingSameSend(landed[index], spec)
		if resuming {
			sent = append(sent, landed[index])
			continue
		}
		request, err := spec.build(ctx, c.BaseURL)
		if err != nil {
			return nil, sent, err
		}
		response, err := c.Doer.Do(request)
		if err != nil {
			return nil, sent, err
		}
		record := goingWriteRequest{Method: spec.method, Path: spec.path, BodySHA256: spec.digest(),
			Outcome: goingWriteOutcome{OK: response.StatusCode < http.StatusMultipleChoices, Status: response.StatusCode}}
		if index == len(requests)-1 || !record.Outcome.OK {
			return response, append(sent, record), nil
		}
		answer, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		record.Made = goingMadeID(answer)
		sent = append(sent, record)
	}
	return nil, sent, nil
}

// reader is the reads a translation makes, through the caller's own Doer, and the listener's own
// lookup of a send's recipient where the caller carries one.
func (c GoingCaller) reader() goingReader {
	return goingReader{doer: c.Doer, base: c.BaseURL, knownSession: c.knownSession}
}

// chooseGoingRoute is the row a call performs and the value of each placeholder of its path: the
// action's first row whose guard the call satisfies and every one of whose placeholders the call
// fills, with a text or a number (goingArgumentText: a document's version), so a document write
// naming its issue takes the issue's row and the same write naming the document alone takes the
// document's.
func chooseGoingRoute(entry GoingAction, arguments map[string]any, session string) (GoingRoute, map[string]string, bool) {
	for _, route := range entry.Routes {
		if !route.takes(arguments) {
			continue
		}
		values, filled := make(map[string]string, len(route.path)), true
		for placeholder, argument := range route.path {
			value := session
			if argument != goingCallerSession {
				named, held := arguments[argument]
				_, isBoolean := named.(bool)
				text, isText := goingArgumentText(named)
				if !held || !isText || isBoolean {
					filled = false
					break
				}
				value = text
			}
			if value == "" {
				filled = false
				break
			}
			values[placeholder] = value
		}
		if filled {
			return route, values, true
		}
	}
	return GoingRoute{}, nil, false
}

// takes reports whether a call's arguments satisfy the row's guard: a row nothing guards takes
// every call, and a guarded row only the calls naming its guard's value.
func (route GoingRoute) takes(arguments map[string]any) bool {
	return route.whenArgument == "" || arguments[route.whenArgument] == route.whenValue
}

// goingPath is pattern with each placeholder replaced by its value, escaped for a path.
func goingPath(pattern string, values map[string]string) string {
	path := pattern
	for placeholder, value := range values {
		path = strings.ReplaceAll(path, "{"+placeholder+"}", url.PathEscape(value))
	}
	return path
}

// goingNoRouteReason says what each of the action's rows wanted and the call did not name, so a
// proposal missing the one argument that chooses a row is told which.
func goingNoRouteReason(entry GoingAction) string {
	wants := make([]string, 0, len(entry.Routes))
	for _, route := range entry.Routes {
		named := make([]string, 0, len(route.path))
		for _, argument := range route.path {
			switch argument {
			case goingCallerSession:
				argument = "the calling session"
			case goingSendIssue:
				argument = "an issue the session works on"
			}
			named = append(named, argument)
		}
		sort.Strings(named)
		want := route.Method + " " + route.Pattern
		if len(named) > 0 {
			want += " needs " + strings.Join(named, ", ")
		}
		if route.whenArgument != "" {
			want += fmt.Sprintf(" with %s %v", route.whenArgument, route.whenValue)
		}
		wants = append(wants, want)
	}
	return "these arguments name no route of this action: " + strings.Join(wants, "; ")
}

// checkGoingArguments holds a proposed call to one action's schema: every required argument is
// named, every named argument is one the action takes, and each value is what its field holds.
// The answer is the refusal, empty for a call the schema accepts.
func checkGoingArguments(fields []contracts.ToolField, arguments map[string]any) string {
	// The names first, whole: the "takes no argument" check below reads every field, not the
	// ones a refusal did not stop at.
	declared := make(map[string]contracts.ToolField, len(fields))
	for _, field := range fields {
		declared[field.Name] = field
	}
	for _, field := range fields {
		value, named := arguments[field.Name]
		if !named {
			if field.Required {
				return "argument " + strconv.Quote(field.Name) + " is required"
			}
			continue
		}
		if reason := checkGoingValue(field, value); reason != "" {
			return "argument " + strconv.Quote(field.Name) + " " + reason
		}
	}
	names := make([]string, 0, len(arguments))
	for name := range arguments {
		if _, takes := declared[name]; !takes {
			names = append(names, strconv.Quote(name))
		}
	}
	if len(names) > 0 {
		sort.Strings(names)
		return "this action takes no argument " + strings.Join(names, ", ")
	}
	return ""
}

// checkGoingValue answers why value is not what field holds, empty where it is.
func checkGoingValue(field contracts.ToolField, value any) string {
	if value == nil {
		if field.Nullable {
			return ""
		}
		return "is null, which it may not be"
	}
	switch field.Kind {
	case contracts.ToolFieldString:
		if _, ok := value.(string); !ok {
			return "must be a string"
		}
	case contracts.ToolFieldEnum:
		text, ok := value.(string)
		if !ok || !slices.Contains(field.Values, text) {
			return "must be one of " + strings.Join(field.Values, ", ")
		}
	case contracts.ToolFieldNumber:
		switch value.(type) {
		case float64, float32, int, int64, json.Number:
		default:
			return "must be a number"
		}
	case contracts.ToolFieldBoolean:
		if _, ok := value.(bool); !ok {
			return "must be true or false"
		}
	case contracts.ToolFieldArray:
		items, ok := value.([]any)
		if !ok {
			return "must be an array"
		}
		for index, item := range items {
			if reason := checkGoingValue(*field.Item, item); reason != "" {
				return fmt.Sprintf("item %d %s", index, reason)
			}
		}
	case contracts.ToolFieldObject:
		nested, ok := value.(map[string]any)
		if !ok {
			return "must be an object"
		}
		if reason := checkGoingArguments(field.Fields, nested); reason != "" {
			return "holds no valid value: " + reason
		}
	case contracts.ToolFieldAny:
	}
	return ""
}
