// hazards_test.go holds the red team's hazards of cybermonklabs/legion#4156 closed at their source
// (plan task t-a-34): a token is never read as a browser, every access.Resolve takes the request's
// own credential, the owner a create writes is a full member, groups are bounded and paged, and
// project.access_changed carries no access list.
package api

import (
	"context"
	"fmt"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
)

// TestATokenIsNeverReadAsABrowser: carol's dsp_ token holds the person scope and lists nothing,
// so it sees the public projects carol sees and none of her private ones. Acting for carol on a
// person-scope route, actingPerson answers carol as a user carrying the token's act; that actor,
// with the token's credential, still resolves as the token, never as carol's browser. The mint
// resolves optionalActor's actor, a session, so the token cannot give another token PRIV either,
// while carol's own browser can.
func TestATokenIsNeverReadAsABrowser(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	makePrivate(t, database, privateProject)
	token := mintListed(t, handler, accessCarol, "carol's agent", nil)
	if len(token.Projects) != 0 {
		t.Fatalf("carol's token lists %v, want none", token.Projects)
	}

	srv := directServer(deps)
	request := bearerOf(token.Token)
	caller, _, err := srv.optionalActor(request)
	if err != nil {
		t.Fatalf("resolve carol's token: %v", err)
	}
	acting, ok := srv.actingPerson(httptest.NewRecorder(), request)
	if !ok || acting.Kind != "user" || acting.Act == nil || acting.Act.TokenID != caller.Credential.TokenID {
		t.Fatalf("actingPerson = %+v, %t, want carol as a user carrying the token's act", acting, ok)
	}
	acting.Credential = caller.Credential
	viewer, err := access.Resolve(request.Context(), deps.Store.Pool, acting)
	if err != nil {
		t.Fatalf("resolve carol's token acting as carol: %v", err)
	}
	if want := []string{publicProject}; !slices.Equal(viewer.Sees, want) {
		t.Fatalf("carol's token acting as carol sees %v, want %v: a token that lists nothing sees its person's public projects, and %s is private",
			viewer.Sees, want, privateProject)
	}

	wantRefusal(t, agentRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", mintBody("given by the token", []string{privateProject}), token.Token),
		http.StatusBadRequest, codeTokenProjectsInput, "carol's token", "mint a token listing "+privateProject)
	mintListed(t, handler, accessCarol, "given at the browser", []string{privateProject})
}

// resolveCallers is every function of internal/dispatch that calls access.Resolve, by package and
// name, with the call the actor it resolves comes from: each resolves the request's own caller,
// with its credential, never an actor a write recorded. A new call is added here only once its
// actor is shown to come from the request's credential.
var resolveCallers = map[string]string{
	"api.resolveReader":       "optionalActor",
	"api.tokenProjects":       "optionalActor",
	"routes.repositoryHidden": "BrowserActor",
}

// TestEveryResolveTakesTheResolvedCredential reads every non-test Go file under internal/dispatch
// and holds each call of access.Resolve to resolveCallers: the function that makes it, and the
// call its actor argument comes from - the call itself where the argument is one, or each
// assignment of the variable it names in that function. A call from any other function, or one
// whose actor comes from anywhere else (a recorded actor, a field of the input, a literal), fails
// naming its file, line and function. Every row of the table must be found, so a scan that read
// nothing cannot pass.
func TestEveryResolveTakesTheResolvedCredential(t *testing.T) {
	var offences []string
	found := map[string]bool{}
	for _, source := range moduleSources(t, "..") {
		pkg := source.file.Name.Name
		for _, declaration := range source.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			name := pkg + "." + function.Name.Name
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !callsResolve(call, pkg) || len(call.Args) != 3 {
					return true
				}
				where := filepath.ToSlash(source.path) + ":" + strconv.Itoa(source.positions.Position(call.Pos()).Line)
				want, listed := resolveCallers[name]
				sources := actorSources(function.Body, call.Args[2])
				if !listed || len(sources) == 0 || slices.ContainsFunc(sources, func(got string) bool { return got != want }) {
					offences = append(offences, where+": "+name+" resolves an actor from "+strings.Join(sources, ", "))
					return true
				}
				found[name] = true
				return true
			})
		}
	}
	sort.Strings(offences)
	if len(offences) > 0 {
		t.Errorf("access.Resolve takes the request's own caller, with its credential (resolveCallers); these calls do not:\n\t%s",
			strings.Join(offences, "\n\t"))
	}
	for name := range resolveCallers {
		if !found[name] {
			t.Errorf("no call of access.Resolve was found in %s: the scan read nothing, or the table names a caller that is gone", name)
		}
	}
}

// callsResolve reports whether call is access.Resolve: qualified from another package, or bare
// inside package access itself.
func callsResolve(call *ast.CallExpr, pkg string) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		qualifier, ok := fun.X.(*ast.Ident)
		return ok && qualifier.Name == "access" && fun.Sel.Name == "Resolve"
	case *ast.Ident:
		return pkg == "access" && fun.Name == "Resolve"
	}
	return false
}

// actorSources names where argument comes from: the function a call argument calls, or, for a
// variable, the function each of its assignments in body calls (one name per assignment). Anything
// else is named by its own shape, which no row of resolveCallers holds.
func actorSources(body *ast.BlockStmt, argument ast.Expr) []string {
	variable, ok := argument.(*ast.Ident)
	if !ok {
		return []string{calledName(argument)}
	}
	var sources []string
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for index, target := range assign.Lhs {
			if named, ok := target.(*ast.Ident); !ok || named.Name != variable.Name {
				continue
			}
			value := assign.Rhs[0]
			if len(assign.Rhs) == len(assign.Lhs) {
				value = assign.Rhs[index]
			}
			sources = append(sources, calledName(value))
		}
		return true
	})
	return sources
}

// calledName is the name of the function expression calls, or the expression's own shape where it
// calls none.
func calledName(expression ast.Expr) string {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return shapeOf(expression)
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	}
	return shapeOf(call.Fun)
}

// shapeOf renders an expression that is not a call well enough for a failure to name it.
func shapeOf(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.SelectorExpr:
		return shapeOf(value.X) + "." + value.Sel.Name
	case *ast.Ident:
		return value.Name
	case *ast.CompositeLit:
		return "a literal " + shapeOf(value.Type)
	case *ast.UnaryExpr:
		return value.Op.String() + shapeOf(value.X)
	}
	return fmt.Sprintf("a %T", expression)
}

// TestARelatedCheckKeepsTheTokensList: a dsp_ token that lists only PRIV creates a PRIV issue whose
// title matches PRIV's ask, answered and then retracted by a document's settlement, so its answer
// is kept only in its ask.answered event, which a reader reads under its viewer. The check reads
// ask states as the handler's own reader, the token with its list, which sees PRIV, so the item
// carries the answer. The writer's recorded actor carries no credential: resolved as a credential
// of carol listing nothing it would see PUB alone and lose the answer (R3), and Resolve now
// refuses it, which would leave the item no ask state at all.
func TestARelatedCheckKeepsTheTokensList(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedCredentialProjects(t, handler, database)
	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": privateProject, "title": "Storage planning", "force": true,
	}, accessCarol)
	if asked.Code != http.StatusCreated {
		t.Fatalf("carol files PRIV's issue: status=%d body=%s", asked.Code, asked.Body.String())
	}
	issue := decodeBody[struct {
		Key string `json:"key"`
	}](t, asked).Key
	opened := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"question": "Which zeppelin hangar should store the blimps?",
		"options":  []map[string]string{{"label": "Brass"}, {"label": "Steel"}},
	}, accessCarol)
	if opened.Code != http.StatusCreated {
		t.Fatalf("carol opens PRIV's ask: status=%d body=%s", opened.Code, opened.Body.String())
	}
	ask := decodeBody[testAskWithSuggestions](t, opened).ID
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", map[string]any{
		"selected": []string{"Steel"}, "text": "The steel hangar.",
	}, accessCarol); answered.Code != http.StatusOK {
		t.Fatalf("carol answers PRIV's ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update asks
		set state = 'resolved', answer = null,
		    resolution = jsonb_build_object(
		      'kind', 'retracted', 'reason', 'removed from the document in version 2',
		      'actor', jsonb_build_object('kind', 'system', 'id', 'document-settlement'), 'at', now())
		where id = $1
	`, ask); err != nil {
		t.Fatalf("retract PRIV's answered ask: %v", err)
	}
	makePrivate(t, database, privateProject)
	token := mintListed(t, handler, accessCarol, "carol's PRIV agent", []string{privateProject})

	created := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": privateProject, "title": "Which zeppelin hangar stores the blimps now?",
		"actor": map[string]string{"kind": "session", "id": "ses_priv_agent"},
	}, token.Token)
	if created.Code != http.StatusCreated {
		t.Fatalf("the PRIV token files a PRIV issue: status=%d body=%s", created.Code, created.Body.String())
	}
	item := relatedAsk(t, issueSuggestions(t, "the PRIV token's issue", created), ask)
	if item.Ask == nil || item.Ask.State != "resolved" || item.Ask.Answer == nil || item.Ask.Answer.Text == nil || *item.Ask.Answer.Text != "The steel hangar." {
		t.Fatalf("PRIV's ask %s on the check of the PRIV token's issue carries %+v, want its retracted answer, which the token, listing PRIV, may read", ask, item.Ask)
	}
}
