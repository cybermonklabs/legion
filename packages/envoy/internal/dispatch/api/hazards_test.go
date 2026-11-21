// hazards_test.go holds the red team's hazards of cybermonklabs/legion#4156 closed at their source
// (plan task t-a-34): a token is never read as a browser, every access.Resolve takes the request's
// own credential, the owner a create writes is a full member, groups are bounded and paged, and
// project.access_changed carries no access list.
package api

import (
	"bytes"
	"context"
	"encoding/json"
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
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// name, with every call the actor it resolves comes from, sorted. Each is the request's own caller
// with its credential, never an actor a write recorded: optionalActor's (resolveReader's and the
// token mint's, both through readerFor); model.BrowserActor, a cookie's (the signed-in person GET
// /auth/whoami's event head is read for, through readerFor: personEventHead's login is the one
// router.login read from the request's cookie); and a live socket's caller as its upgrade resolved
// it, credential kept (liveSocket.caller, and the subscription stream's Caller, which answers it).
// A new call is added here only once its actor is shown to come from the request's credential.
var resolveCallers = map[string][]string{
	"api.readerFor":           {"BrowserActor", "Caller", "caller", "optionalActor"},
	"docs.ServeHTTP":          {"BrowserActor"},
	"docs.readableRooms":      {"BrowserActor"},
	"routes.repositoryHidden": {"BrowserActor"},
}

// TestEveryResolveTakesTheResolvedCredential reads every non-test Go file of the module
// and holds each call of access.Resolve to resolveCallers: the function that makes it, and the
// calls its actor argument comes from (actorSources). A call from any other function, or one whose
// actor comes from anywhere else (a recorded actor, a field of the input, a literal), fails naming
// its file, line and function. Every row of the table must be found, so a scan that read nothing
// cannot pass.
func TestEveryResolveTakesTheResolvedCredential(t *testing.T) {
	var functions []declaredFunction
	for _, source := range moduleSources(t) {
		for _, declaration := range source.file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil {
				functions = append(functions, declaredFunction{source: source, decl: function})
			}
		}
	}
	var offences []string
	found := map[string]bool{}
	for _, function := range functions {
		name := function.name()
		ast.Inspect(function.decl.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !callsResolve(call, function.source.file.Name.Name) || len(call.Args) != 3 {
				return true
			}
			where := filepath.ToSlash(function.source.path) + ":" + strconv.Itoa(function.source.positions.Position(call.Pos()).Line)
			sources := actorSources(functions, function, call.Args[2], map[string]bool{})
			slices.Sort(sources)
			sources = slices.Compact(sources)
			if want, listed := resolveCallers[name]; !listed || !slices.Equal(sources, want) {
				offences = append(offences, where+": "+name+" resolves an actor from "+strings.Join(sources, ", "))
				return true
			}
			found[name] = true
			return true
		})
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

// declaredFunction is one function declaration of the scanned sources.
type declaredFunction struct {
	source moduleSource
	decl   *ast.FuncDecl
}

func (function declaredFunction) name() string {
	return function.source.file.Name.Name + "." + function.decl.Name.Name
}

// parameter answers the position of name among the function's parameters, if it is one.
func (function declaredFunction) parameter(name string) (int, bool) {
	index := 0
	for _, field := range function.decl.Type.Params.List {
		for _, parameter := range field.Names {
			if parameter.Name == name {
				return index, true
			}
			index++
		}
	}
	return 0, false
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

// actorSources names the calls argument, an expression of function, comes from: the function a
// call argument calls; for a variable, the function each of its assignments in function calls; and
// for a parameter of function, the sources of the argument each call of function in its own
// package passes there, followed as far as it goes (seen stops a cycle). Anything else is named by
// its own shape, which no row of resolveCallers holds.
func actorSources(functions []declaredFunction, function declaredFunction, argument ast.Expr, seen map[string]bool) []string {
	variable, ok := argument.(*ast.Ident)
	if !ok {
		return []string{calledName(argument)}
	}
	var sources []string
	if index, ok := function.parameter(variable.Name); ok && !seen[function.name()] {
		seen[function.name()] = true
		passed := false
		for _, caller := range functions {
			if caller.source.file.Name.Name != function.source.file.Name.Name {
				continue
			}
			ast.Inspect(caller.decl.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if ok && calledName(call) == function.decl.Name.Name && len(call.Args) > index {
					passed = true
					sources = append(sources, actorSources(functions, caller, call.Args[index], seen)...)
				}
				return true
			})
		}
		if !passed {
			sources = append(sources, "a parameter of "+function.name()+" no call passes")
		}
	}
	ast.Inspect(function.decl.Body, func(node ast.Node) bool {
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

// TestACreateCannotMakeARestrictedMemberTheOwner: a create by a credential that acts for nobody,
// the deployment's shared token, takes the default_assignee it names as the project's owner, and
// an owner is a full member, as R1's owner transfer holds one. Naming bob, a restricted member, is
// 400 OWNER_NOT_ALLOWED and writes no project; naming carol, a full member, makes her the owner.
func TestACreateCannotMakeARestrictedMemberTheOwner(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessCarol)
	markRestricted(t, database, "bob")
	create := func(key, assignee string) *httptest.ResponseRecorder {
		return agentRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]any{
			"key": key, "name": key, "default_assignee": assignee,
			"actor": map[string]string{"kind": "session", "id": "ses_creator"},
		}, "agent-token")
	}
	ownerOf := func(key string) []string {
		t.Helper()
		rows, err := database.Pool.Query(context.Background(), `select coalesce(owner, '') from projects where key = $1`, key)
		if err != nil {
			t.Fatalf("read %s's owner: %v", key, err)
		}
		defer rows.Close()
		var owners []string
		for rows.Next() {
			var owner string
			if err := rows.Scan(&owner); err != nil {
				t.Fatalf("read %s's owner: %v", key, err)
			}
			owners = append(owners, owner)
		}
		return owners
	}

	refused := create("NEW", "bob")
	if owners := ownerOf("NEW"); refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), `"code":"OWNER_NOT_ALLOWED"`) || len(owners) != 0 {
		t.Fatalf("the shared token creates NEW for bob, a restricted member: %d %s, and NEW's owner rows are %v; want 400 OWNER_NOT_ALLOWED and no project",
			refused.Code, refused.Body.String(), owners)
	}
	if created := create("NEW", accessCarol); created.Code != http.StatusCreated || !slices.Equal(ownerOf("NEW"), []string{accessCarol}) {
		t.Fatalf("the shared token creates NEW for carol: %d %s, owner %v; want 201 and carol the owner", created.Code, created.Body.String(), ownerOf("NEW"))
	}
}

// seedGroups makes count groups of owner, named <prefix>000 on, straight in the table: the cap
// counts rows, whoever wrote them.
func seedGroups(t *testing.T, database *store.Store, owner, prefix string, count int) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into access_groups (name, owner)
		select $2 || lpad(i::text, 3, '0'), $1 from generate_series(0, $3::int - 1) as i
	`, owner, prefix, count); err != nil {
		t.Fatalf("seed %d groups of %s: %v", count, owner, err)
	}
}

// groupsOf counts the groups owner holds.
func groupsOf(t *testing.T, database *store.Store, owner string) int64 {
	t.Helper()
	return countRows(t, database, `select count(*) from access_groups where owner = $1`, owner)
}

// putGroup writes group name as login, with body.
func putGroup(t *testing.T, handler http.Handler, login, name string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPut, "/api/v1/access-groups/"+name, body, login)
}

// TestAnOwnerHoldsAtMostMaxGroups: an owner holds at most model.MaxGroupsPerOwner groups. Each row
// names itself on failure.
//   - The cap: carol holds 100 groups, so her 101st create is 400 CAP_EXCEEDED, and a write to a
//     group she holds already is still 200.
//   - The transfer: dave holds 100 groups, so carol's PUT of her group naming dave its owner is 400
//     CAP_EXCEEDED, and the group stays carol's.
//   - The race: frank holds 99 groups and makes two more of different names at once. Each create is
//     held at its commit by a deferred trigger until both are under way, so two creates that
//     counted without the owner's lock would both find 99; with it, the second counts after the
//     first commits. Exactly one answers 200 and the other 400 CAP_EXCEEDED.
func TestAnOwnerHoldsAtMostMaxGroups(t *testing.T) {
	const frank = "frank@example.com"
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessCarol, accessDave, frank)
	empty := map[string]any{"members": []string{}}

	seedGroups(t, database, accessCarol, "carol-", model.MaxGroupsPerOwner)
	if over := putGroup(t, handler, accessCarol, "carol-more", empty); over.Code != http.StatusBadRequest || !strings.Contains(over.Body.String(), `"code":"CAP_EXCEEDED"`) {
		t.Fatalf("the cap row: carol's 101st group = %d %s, want 400 CAP_EXCEEDED", over.Code, over.Body.String())
	}
	if held := groupsOf(t, database, accessCarol); held != model.MaxGroupsPerOwner {
		t.Fatalf("the cap row: carol holds %d groups, want %d", held, model.MaxGroupsPerOwner)
	}
	if again := putGroup(t, handler, accessCarol, "carol-000", map[string]any{"members": []string{accessDave}}); again.Code != http.StatusOK {
		t.Fatalf("the cap row: carol writes her first group again = %d %s, want 200", again.Code, again.Body.String())
	}

	seedGroups(t, database, accessDave, "dave-", model.MaxGroupsPerOwner)
	if given := putGroup(t, handler, accessCarol, "carol-000", map[string]any{"members": []string{}, "owner": accessDave}); given.Code != http.StatusBadRequest ||
		!strings.Contains(given.Body.String(), `"code":"CAP_EXCEEDED"`) || groupsOf(t, database, accessDave) != model.MaxGroupsPerOwner {
		t.Fatalf("the transfer row: carol gives carol-000 to dave, who holds 100 = %d %s, dave now holds %d; want 400 CAP_EXCEEDED and dave's groups unchanged",
			given.Code, given.Body.String(), groupsOf(t, database, accessDave))
	}

	seedGroups(t, database, frank, "frank-", model.MaxGroupsPerOwner-1)
	statuses := raceTwoGroupCreates(t, handler, database, frank, empty)
	slices.Sort(statuses)
	if want := []int{http.StatusOK, http.StatusBadRequest}; !slices.Equal(statuses, want) || groupsOf(t, database, frank) != model.MaxGroupsPerOwner {
		t.Fatalf("the race row: frank's two creates at 99 answered %v and frank holds %d; want one 200, one 400 CAP_EXCEEDED, and %d groups",
			statuses, groupsOf(t, database, frank), model.MaxGroupsPerOwner)
	}
}

// raceTwoGroupCreates runs two creates of owner's groups at once, each held at its commit by a
// deferred constraint trigger on access_groups that waits for an advisory lock this test holds,
// and releases them once two writes wait on a lock: either both at their commit, which only
// creates that both counted before either committed reach, or one at its commit and the other on
// the owner's lock. It answers both statuses.
func raceTwoGroupCreates(t *testing.T, handler http.Handler, database *store.Store, owner string, body map[string]any) []int {
	t.Helper()
	ctx := context.Background()
	const hold = 34034
	if _, err := database.Pool.Exec(ctx, `
		create function hold_group_commit() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock_shared(34034);
			return null;
		end $$;
		create constraint trigger hold_group_commit after insert on access_groups
			deferrable initially deferred for each row execute function hold_group_commit();
	`); err != nil {
		t.Fatalf("install the commit hold: %v", err)
	}
	held, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the holding connection: %v", err)
	}
	defer held.Release()
	if _, err := held.Exec(ctx, `select pg_advisory_lock($1)`, hold); err != nil {
		t.Fatalf("take the commit hold: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if _, err := held.Exec(ctx, `select pg_advisory_unlock($1)`, hold); err != nil {
				t.Errorf("release the commit hold: %v", err)
			}
		}
	}
	defer release()

	answers := make(chan int, 2)
	for _, name := range []string{"race-a", "race-b"} {
		go func() {
			payload, _ := json.Marshal(body)
			request := httptest.NewRequest(http.MethodPut, "/api/v1/access-groups/"+name, bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Dispatch-User", owner)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			answers <- response.Code
		}()
	}
	deadline := time.Now().Add(20 * time.Second)
	for countRows(t, database, `
		select count(*) from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and pid <> pg_backend_pid()
	`) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the two creates never both waited on a lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	release()
	statuses := make([]int, 0, 2)
	for range 2 {
		select {
		case status := <-answers:
			statuses = append(statuses, status)
		case <-time.After(20 * time.Second):
			t.Fatal("a racing create never answered")
		}
	}
	return statuses
}

// TestTheGroupListPages: GET /api/v1/access-groups answers a page of at most limit groups after the
// name after, in name order, with next naming where the next page starts, absent on the last page.
// Five groups at limit=2 are three pages that answer the five names in order. A limit that is not
// an integer from 1 to 100, and an after that is no group name, are 400 INVALID_QUERY naming the
// parameter.
func TestTheGroupListPages(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	seedPeople(t, database, accessCarol)
	seedGroups(t, database, accessCarol, "g", 5)
	read := func(query string) model.AccessGroupPage {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/access-groups"+query, nil, accessCarol)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/access-groups%s = %d %s, want 200", query, response.Code, response.Body.String())
		}
		return decodeBody[model.AccessGroupPage](t, response)
	}
	var names []string
	query := "?limit=2"
	for page := 1; ; page++ {
		answer := read(query)
		if page < 3 && len(answer.Groups) != 2 || page == 3 && len(answer.Groups) != 1 || page > 3 {
			t.Fatalf("page %d of five groups at limit=2 (%s) holds %d groups, next %q; want 2, 2 and then 1", page, query, len(answer.Groups), answer.Next)
		}
		for _, group := range answer.Groups {
			names = append(names, group.Name)
		}
		if answer.Next == "" {
			break
		}
		query = "?limit=2&after=" + answer.Next
	}
	if want := []string{"g000", "g001", "g002", "g003", "g004"}; !slices.Equal(names, want) {
		t.Fatalf("the pages answered %v, want %v", names, want)
	}
	if whole := read(""); len(whole.Groups) != 5 || whole.Next != "" {
		t.Fatalf("the default page holds %d groups, next %q; want all five and no next", len(whole.Groups), whole.Next)
	}
	if raw := dispatchRequest(t, handler, http.MethodGet, "/api/v1/access-groups?limit=5", nil, accessCarol).Body.String(); strings.Contains(raw, `"next"`) {
		t.Fatalf("the last page = %s, want no next key", raw)
	}
	for _, refused := range []struct{ query, parameter string }{
		{"?limit=0", "limit"}, {"?limit=101", "limit"}, {"?limit=x", "limit"}, {"?after=Bad%20Name", "after"},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/access-groups"+refused.query, nil, accessCarol)
		body := decodeBody[struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}](t, response)
		if response.Code != http.StatusBadRequest || body.Code != "INVALID_QUERY" || !strings.HasPrefix(body.Error, refused.parameter+" ") {
			t.Errorf("GET /api/v1/access-groups%s = %d %+v, want 400 INVALID_QUERY naming %s", refused.query, response.Code, body, refused.parameter)
		}
	}
}

// TestTheAccessChangedEventCarriesNoAccessList: PRIV's owner, dave, adds carol to PRIV's list, and
// the event that write appends names the project and the change and nothing else, byte for byte.
// A dsp_ token that lists PRIV and holds no person scope - a caller GET .../access refuses - reads
// that event through the /ws/live events view, subscribed before the write (a resume across an
// access change is replayed nothing), and through GET /api/v1/events/log, and neither answer holds
// carol's email.
func TestTheAccessChangedEventCarriesNoAccessList(t *testing.T) {
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	seedPeople(t, rig.database, accessCarol, accessDave)
	if created := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": privateProject, "name": privateProject}, accessDave); created.Code != http.StatusCreated {
		t.Fatalf("dave creates PRIV: %d %s", created.Code, created.Body.String())
	}
	makePrivate(t, rig.database, privateProject)
	minted := dispatchRequest(t, rig.handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{
		"name": "PRIV without the person scope", "scopes": []string{}, "projects": []string{privateProject},
	}, accessDave)
	if minted.Code != http.StatusCreated {
		t.Fatalf("dave mints a PRIV token with no scope: %d %s", minted.Code, minted.Body.String())
	}
	token := decodeBody[listedToken](t, minted).Token
	if refused := agentRequest(t, rig.handler, http.MethodGet, "/api/v1/projects/"+privateProject+"/access", nil, token); refused.Code != http.StatusForbidden {
		t.Fatalf("the token reads PRIV's access = %d %s, want 403: it holds no person scope", refused.Code, refused.Body.String())
	}
	client := rig.dial(t, withBearer(token))
	subscribeEvents(t, client, "feed", nil)

	if added := dispatchRequest(t, rig.handler, http.MethodPut, "/api/v1/projects/"+privateProject+"/access/people/"+accessCarol, nil, accessDave); added.Code != http.StatusOK {
		t.Fatalf("dave adds carol to PRIV: %d %s", added.Code, added.Body.String())
	}
	// The event log answers only the events at or below the safe horizon, which a transaction
	// elsewhere on the test cluster can hold below the change just made (awaitHorizonRead).
	safeHorizon(t, rig.served)
	logged := agentRequest(t, rig.handler, http.MethodGet, "/api/v1/events/log", nil, token)
	if logged.Code != http.StatusOK || !strings.Contains(logged.Body.String(), `"type":"project.access_changed"`) {
		t.Fatalf("the token's event log = %d %.400s, want 200 holding PRIV's project.access_changed", logged.Code, logged.Body.String())
	}
	if strings.Contains(logged.Body.String(), accessCarol) {
		t.Fatalf("the token's event log holds %s: %s", accessCarol, logged.Body.String())
	}
	frame, _ := eventOfType(t, client, "feed", "project.access_changed")
	if strings.Contains(string(frame.Event), accessCarol) {
		t.Fatalf("the token's /ws/live event holds %s: %s", accessCarol, frame.Event)
	}
	var stored []byte
	if err := rig.database.Pool.QueryRow(context.Background(), `
		select payload::text from events where type = 'project.access_changed' and project_key = $1 order by id desc limit 1
	`, privateProject).Scan(&stored); err != nil {
		t.Fatalf("read the stored project.access_changed: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, stored); err != nil {
		t.Fatalf("compact %s: %v", stored, err)
	}
	if want := `{"change":"person_added","project":"PRIV"}`; compact.String() != want {
		t.Fatalf("the stored payload is %s, want %s", compact.String(), want)
	}
}
