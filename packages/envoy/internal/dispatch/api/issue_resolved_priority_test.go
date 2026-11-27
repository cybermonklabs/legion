// issue_resolved_priority_test.go holds Dispatch to J1 and J2 of dispatch://INFRA-5793 (check
// j-c-api). An issue with no priority of its own takes the priority of the nearest issue above it
// on its parent chain that has one, in any project, within parentDepthCap links: every read answers
// it as resolved_priority beside the issue's own priority, with priority_from naming the issue it
// comes from, and every reader shows, filters, orders and counts the issue by it. A move under
// another parent and a clear of an issue's own priority need the p0 scope where they make the issue,
// or an issue under it that takes its priority from it, go to P0, and so does a new issue with no
// priority under a parent that reads P0, unless the writer's session is that parent's architect. A
// detach never needs p0.
//
// Every row reads j-c-api's seed (seedResolvedPriorities), and each write row a fresh copy of it.
// The two inventories, TestEveryIssueRowShapeCarriesTheResolvedPriority and
// TestEveryOperationThatTakesAPriorityOrAParentRunsTheP0Rows, read every committed
// dispatch-openapi*.json, as INFRA-5637's inventory does (committedOpenAPIDocuments).
//
// The plan's decision 20 builds J1's clause that a reader who cannot see the project of the issue
// a priority comes from gets none, and with it the conditional rows of R2, R4 and R5, only where
// an issue read calls access.Resolve or access.VisibleProjectsSQL, INFRA-3862's read rule. No issue
// read calls either on this change's base (`git grep -n -e 'access.Resolve' -e VisibleProjectsSQL
// -- packages/envoy/internal/dispatch/api ':!*_test.go'` finds nothing), so this file holds none of
// those rows: whichever of this change and that read rule merges second adds them.
//
// The tests need DISPATCH_TEST_DATABASE_URL, as every test of this package that opens a store.
package api

//ci:reads packages/contracts/schemas/dispatch-openapi*.json TestEveryIssueRowShapeCarriesTheResolvedPriority and TestEveryOperationThatTakesAPriorityOrAParentRunsTheP0Rows read each committed document (committedOpenAPIDocuments)

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueNames is the name a test gives each issue it files, by the issue's key, which its failure
// lines print beside the key.
type issueNames map[string]string

// name is key with its name, as a failure line prints it.
func (n issueNames) name(key string) string {
	if name, named := n[key]; named {
		return name + " (" + key + ")"
	}
	return key
}

// keyName is a priority_from as a failure line prints it: the issue's name, or none.
func (n issueNames) keyName(key string) string {
	if key == "" {
		return "none"
	}
	return n.name(key)
}

// resolvedWant is what a read answers for one issue: its own priority, the priority it resolves
// to, and the key of the issue that one comes from, "" where it is the issue's own or there is
// none.
type resolvedWant struct {
	key           string
	own, resolved *int
	from          string
}

// reads is the priority want resolves to, as a failure line prints it.
func (n issueNames) reads(want resolvedWant) string {
	switch {
	case want.resolved == nil:
		return "none"
	case want.from == "":
		return "P" + priorityName(want.resolved) + ", its own"
	default:
		return "P" + priorityName(want.resolved) + " from " + n.name(want.from)
	}
}

// requireResolvedAs fails unless reader answers the issue want names with own as its priority,
// resolved as its resolved_priority and from as its priority_from. Each field fails on a line of
// its own that names the reader and the issue, so a reader left on the issue's own priority, or on
// its root's, fails by name.
func (n issueNames) requireResolvedAs(t *testing.T, reader string, want resolvedWant, own, resolved *int, from *string) {
	t.Helper()
	if priorityName(own) != priorityName(want.own) {
		t.Errorf("%s: %s has priority %s, want its own, %s", reader, n.name(want.key), priorityName(own), priorityName(want.own))
	}
	if priorityName(resolved) != priorityName(want.resolved) {
		t.Errorf("%s: %s has resolved_priority %s, want %s", reader, n.name(want.key), priorityName(resolved), n.reads(want))
	}
	got := ""
	if from != nil {
		got = *from
	}
	if got != want.from {
		t.Errorf("%s: %s has priority_from %s, want %s", reader, n.name(want.key), n.keyName(got), n.keyName(want.from))
	}
}

// resolvedSeed is j-c-api's seed, by the names the plan's "The test rows" gives its issues: in one
// project, a root A at P0 with a child C (none), a grandchild G under C (none), a child C0 (its own
// P0) and a middle M2 (its own P2) with a child M2c (none); a root B at P1 with a middle M0 (its own
// P0) with a child M0c (none); a root N (none) with a child Nc (none); and a child X (none) under a
// root Q at P1 of a second project.
type resolvedSeed struct {
	issueNames
	a, c, g, c0, m2, m2c, b, m0, m0c, n, nc, q, x string
}

// seedResolvedPriorities files the seed through handler as a person at a browser, who holds p0:
// every issue in project but Q, which is other's. The caller made both projects. Each call files
// new issues, so a write row that calls it starts from a fresh copy.
func seedResolvedPriorities(t *testing.T, handler http.Handler, project, other string) resolvedSeed {
	t.Helper()
	seed := resolvedSeed{issueNames: issueNames{}}
	file := func(name, in, parent string, priority *int) string {
		t.Helper()
		body := map[string]any{"project": in, "title": "The seed's " + name, "force": true}
		if parent != "" {
			body["parent"] = parent
		}
		if priority != nil {
			body["priority"] = *priority
		}
		key := createdIssue(t, "file the seed's "+name, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")).Key
		seed.issueNames[key] = name
		return key
	}
	seed.a = file("A", project, "", new(0))
	seed.c = file("C", project, seed.a, nil)
	seed.g = file("G", project, seed.c, nil)
	seed.c0 = file("C0", project, seed.a, new(0))
	seed.m2 = file("M2", project, seed.a, new(2))
	seed.m2c = file("M2c", project, seed.m2, nil)
	seed.b = file("B", project, "", new(1))
	seed.m0 = file("M0", project, seed.b, new(0))
	seed.m0c = file("M0c", project, seed.m0, nil)
	seed.n = file("N", project, "", nil)
	seed.nc = file("Nc", project, seed.n, nil)
	seed.q = file("Q", other, "", new(1))
	seed.x = file("X", project, seed.q, nil)
	return seed
}

// wants is what every read answers for each issue of the seed (J1): its own priority as seeded,
// and the priority of the nearest issue above it that has one, in any project.
func (s resolvedSeed) wants() []resolvedWant {
	return []resolvedWant{
		{key: s.a, own: new(0), resolved: new(0)},
		{key: s.c, resolved: new(0), from: s.a},
		{key: s.g, resolved: new(0), from: s.a},
		{key: s.c0, own: new(0), resolved: new(0)},
		{key: s.m2, own: new(2), resolved: new(2)},
		{key: s.m2c, resolved: new(2), from: s.m2},
		{key: s.b, own: new(1), resolved: new(1)},
		{key: s.m0, own: new(0), resolved: new(0)},
		{key: s.m0c, resolved: new(0), from: s.m0},
		{key: s.n},
		{key: s.nc},
		{key: s.q, own: new(1), resolved: new(1)},
		{key: s.x, resolved: new(1), from: s.q},
	}
}

// want is the seed's want for key.
func (s resolvedSeed) want(t *testing.T, key string) resolvedWant {
	t.Helper()
	for _, want := range s.wants() {
		if want.key == key {
			return want
		}
	}
	t.Fatalf("the seed holds no issue %s", key)
	return resolvedWant{}
}

// readResolved is key as GET /api/v1/issues/{key} answers a person at a browser.
func readResolved(t *testing.T, handler http.Handler, key string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues/%s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// listResolved is GET /api/v1/issues?<query> as it answers a person at a browser. A query that
// names no status lists the issues of every status.
func listResolved(t *testing.T, handler http.Handler, query string) []model.IssueSummary {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/issues?%s: status=%d body=%s", query, response.Code, response.Body.String())
	}
	return decodeBody[[]model.IssueSummary](t, response)
}

// requireReadsResolved fails unless the single read and the listing of the issue's project each
// answer what want says.
func (n issueNames) requireReadsResolved(t *testing.T, handler http.Handler, project string, want resolvedWant) {
	t.Helper()
	read := readResolved(t, handler, want.key)
	n.requireResolvedAs(t, "GET /api/v1/issues/{key}", want, read.Priority, read.ResolvedPriority, read.PriorityFrom)
	for _, row := range listResolved(t, handler, "project="+project) {
		if row.Key == want.key {
			n.requireResolvedAs(t, "GET /api/v1/issues?project="+project, want, row.Priority, row.ResolvedPriority, row.PriorityFrom)
			return
		}
	}
	t.Errorf("GET /api/v1/issues?project=%s lists no %s", project, n.name(want.key))
}

// newResolvedRig is newLegionStatusRigOn's server, with the shared token and the other credentials
// of its inventory, and legionOtherProject beside brokerTestProject, which the seed files Q in.
func newResolvedRig(t *testing.T, serving testServerOptions) *legionStatusRig {
	t.Helper()
	rig := newLegionStatusRigOn(t, serving)
	createTestProject(t, rig.handler, legionOtherProject)
	rig.otherProject = true
	return rig
}

// freshSeed files a fresh copy of the seed in brokerTestProject, Q in legionOtherProject.
func (rig *legionStatusRig) freshSeed(t *testing.T) resolvedSeed {
	t.Helper()
	return seedResolvedPriorities(t, rig.handler, brokerTestProject, legionOtherProject)
}

// inheritingPair files, as a person, an issue of brokerTestProject with no priority of its own and
// a child of it with none, the first under parent where parent is not "", and names them name and
// name's child in seed.
func (rig *legionStatusRig) inheritingPair(t *testing.T, seed resolvedSeed, name, parent string) (string, string) {
	t.Helper()
	fields := map[string]any{}
	if parent != "" {
		fields["parent"] = parent
	}
	issue := rig.personIssue(t, fields).Key
	child := rig.personIssue(t, map[string]any{"parent": issue}).Key
	seed.issueNames[issue], seed.issueNames[child] = name, name+"'s child"
	return issue, child
}

// resolvedState is what a refused move, clear or create leaves as it was on one issue: its parent,
// status, own priority and last_seq as its row holds them, and the priority it resolves to and the
// issue that one comes from as its read answers them.
type resolvedState struct {
	parent, from  *string
	status        string
	own, resolved *int
	lastSeq       int
}

func (s resolvedState) String() string {
	parent, from := "none", "none"
	if s.parent != nil {
		parent = *s.parent
	}
	if s.from != nil {
		from = *s.from
	}
	return fmt.Sprintf("parent %s, status %s, priority %s, last_seq %d, resolved_priority %s from %s",
		parent, s.status, priorityName(s.own), s.lastSeq, priorityName(s.resolved), from)
}

func (s resolvedState) equal(other resolvedState) bool {
	return stringPointersEqual(s.parent, other.parent) && s.status == other.status &&
		priorityName(s.own) == priorityName(other.own) && s.lastSeq == other.lastSeq &&
		priorityName(s.resolved) == priorityName(other.resolved) && stringPointersEqual(s.from, other.from)
}

// readResolvedStates is the state of each issue of keys.
func (r *brokerTokenRig) readResolvedStates(t *testing.T, keys ...string) map[string]resolvedState {
	t.Helper()
	states := make(map[string]resolvedState, len(keys))
	for _, key := range keys {
		var state resolvedState
		if err := r.database.Pool.QueryRow(context.Background(),
			`select parent_key, status, priority, last_seq from issues where key = $1`, key,
		).Scan(&state.parent, &state.status, &state.own, &state.lastSeq); err != nil {
			t.Fatalf("read issue %s: %v", key, err)
		}
		read := readResolved(t, r.handler, key)
		state.resolved, state.from = read.ResolvedPriority, read.PriorityFrom
		states[key] = state
	}
	return states
}

// requireResolvedStatesKept fails unless each issue of before is as it was: a refused write
// changes no parent, status, own priority or resolved priority and appends no event.
func (r *brokerTokenRig) requireResolvedStatesKept(t *testing.T, names issueNames, before map[string]resolvedState) {
	t.Helper()
	for _, key := range slices.Sorted(maps.Keys(before)) {
		if after := r.readResolvedStates(t, key)[key]; !after.equal(before[key]) {
			t.Errorf("%s after the refused write: %s; want it as it was: %s", names.name(key), after, before[key])
		}
	}
}

// requireNothingFiled fails unless a refused create left no issue titled title, in any project,
// and no near_copy_checks row of that title. A create's near_copy_checks row may be written after
// its answer (recordNearCopyCheck), so it waits out the server's detached writes first.
func requireNothingFiled(t *testing.T, rig *brokerTokenRig, title string) {
	t.Helper()
	awaitBackgroundWrites(t, rig.served)
	if made := countRows(t, rig.database, `select count(*) from issues where title = $1`, title); made != 0 {
		t.Errorf("the refused create left %d issues titled %q, want none", made, title)
	}
	if checks := countRows(t, rig.database, `select count(*) from near_copy_checks where title = $1`, title); checks != 0 {
		t.Errorf("the refused create left %d near_copy_checks rows titled %q, want none", checks, title)
	}
}

// fileChild is the create, sent by send, of a new issue of project under parent with fields and no
// priority unless fields names one, naming session as its actor where session is not "" (a bearer
// that proves no session names the one it acts as), and the title it names.
func (rig *legionStatusRig) fileChild(t *testing.T, send func(*testing.T, string, string, map[string]any) *httptest.ResponseRecorder, project, parent, session string, fields map[string]any) (string, *httptest.ResponseRecorder) {
	t.Helper()
	title := rig.title()
	body := map[string]any{"project": project, "title": title, "parent": parent, "force": true}
	maps.Copy(body, fields)
	if session != "" {
		body["actor"] = map[string]any{"kind": "session", "id": session}
	}
	return title, send(t, http.MethodPost, "/api/v1/issues", body)
}

// requireFiledAs fails unless response is a create's 201 and the issue it made reads as want says,
// want.key aside: the new issue's own key is the create's.
func (n issueNames) requireFiledAs(t *testing.T, handler http.Handler, what string, response *httptest.ResponseRecorder, want resolvedWant) {
	t.Helper()
	want.key = createdIssue(t, what, response).Key
	n[want.key] = what
	read := readResolved(t, handler, want.key)
	n.requireResolvedAs(t, "GET /api/v1/issues/{key}", want, read.Priority, read.ResolvedPriority, read.PriorityFrom)
}

// resolvedPriorityKinds are the two properties J1 adds beside an issue's own priority, each with
// the JSON type the emitter writes beside null, since both are always written and null where there
// is none.
var resolvedPriorityKinds = map[string]string{"resolved_priority": "integer", "priority_from": "string"}

// resolvedShapeExceptions are the response shapes with a property named priority that
// TestEveryIssueRowShapeCarriesTheResolvedPriority holds to fewer fields than both, each with its
// reason and the fields it holds it to. The list is closed: every other shape is held to both.
var resolvedShapeExceptions = map[string]struct {
	reason string
	fields []string
}{
	"DeliveryPR": {
		reason: "its priority is the facet label (P0 to P3) of the pull request's issue, which reads the priority the issue resolves to, so it carries priority_from alone",
		fields: []string{"priority_from"},
	},
}

// prioritiesGraphPath is the path of the Priorities graph's routes. A shape that only they answer
// is a priority's, and a property of it named priority is no issue's priority.
const prioritiesGraphPath = "/api/v1/priorities"

// resolvedShapes are the response shapes of the plan's decision 3: each carries an issue's own
// priority on main, and the inventory fails when it finds fewer.
var resolvedShapes = []string{
	"ArchitectureTreeIssue", "ClaimedIssue", "DeliveryPR", "InboxRow", "Issue", "IssueDetails",
	"IssueSummary", "LegionBoardCard", "MyReplyRow", "OpenAsk",
}

// responseShapes answers each component schema of document name that the response of one of its
// operations reaches, through $ref, allOf, anyOf, oneOf, properties, items and
// additionalProperties, with the paths of the operations that reach it.
func responseShapes(t *testing.T, name string, document map[string]any) map[string][]string {
	t.Helper()
	components, _ := document["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	paths, _ := document["paths"].(map[string]any)
	reached := map[string][]string{}
	var walk func(node any, path string, seen map[string]bool)
	walk = func(node any, path string, seen map[string]bool) {
		schema, ok := node.(map[string]any)
		if !ok {
			return
		}
		if reference, referenced := schema["$ref"].(string); referenced {
			component, local := strings.CutPrefix(reference, "#/components/schemas/")
			if !local {
				t.Fatalf("%s holds the reference %s, which this test does not follow", name, reference)
			}
			if seen[component] {
				return
			}
			seen[component] = true
			target, declared := schemas[component]
			if !declared {
				t.Fatalf("%s holds the reference %s, which names no schema it declares", name, reference)
			}
			if !slices.Contains(reached[component], path) {
				reached[component] = append(reached[component], path)
			}
			walk(target, path, seen)
		}
		for _, combinator := range []string{"allOf", "anyOf", "oneOf"} {
			branches, _ := schema[combinator].([]any)
			for _, branch := range branches {
				walk(branch, path, seen)
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for _, property := range properties {
			walk(property, path, seen)
		}
		walk(schema["items"], path, seen)
		walk(schema["additionalProperties"], path, seen)
	}
	for path, item := range paths {
		operations, _ := item.(map[string]any)
		for _, method := range openAPIMethods {
			operation, _ := operations[method].(map[string]any)
			responses, _ := operation["responses"].(map[string]any)
			seen := map[string]bool{}
			for status, response := range responses {
				answer, _ := response.(map[string]any)
				if _, referenced := answer["$ref"]; referenced {
					t.Fatalf("%s: %s %s's %s response is a reference, which this test does not follow", name, strings.ToUpper(method), path, status)
				}
				content, _ := answer["content"].(map[string]any)
				for _, media := range content {
					body, _ := media.(map[string]any)
					walk(body["schema"], path, seen)
				}
			}
		}
	}
	return reached
}

// schemaTypes is the JSON types node, a property's schema, admits: its type, one name or a list
// of them, and each of its anyOf and oneOf branches'.
func schemaTypes(node any) map[string]bool {
	types := map[string]bool{}
	schema, _ := node.(map[string]any)
	switch kind := schema["type"].(type) {
	case string:
		types[kind] = true
	case []any:
		for _, one := range kind {
			if name, named := one.(string); named {
				types[name] = true
			}
		}
	}
	for _, combinator := range []string{"anyOf", "oneOf"} {
		branches, _ := schema[combinator].([]any)
		for _, branch := range branches {
			maps.Copy(types, schemaTypes(branch))
		}
	}
	return types
}

// TestEveryIssueRowShapeCarriesTheResolvedPriority is the inventory of the plan's decision 3
// (R1). It reads every committed dispatch-openapi*.json and takes each component schema that a
// response reaches and that has a property named priority. Each must declare resolved_priority, a
// nullable integer, and priority_from, a nullable string, each required where the shape requires
// priority, since both are always written. A request body takes the issue's own priority and is
// TestEveryOperationThatTakesAPriorityOrAParentRunsTheP0Rows'. The closed list
// resolvedShapeExceptions holds a shape to fewer fields with its reason, and a shape only the
// Priorities graph's routes answer carries a priority that is no issue's. The test fails by name
// for each shape that declares a field wrong or not at all, and when the main document holds fewer
// than the shapes of decision 3.
func TestEveryIssueRowShapeCarriesTheResolvedPriority(t *testing.T) {
	t.Parallel()
	documents := committedOpenAPIDocuments(t)
	for _, documentName := range slices.Sorted(maps.Keys(documents)) {
		document := documents[documentName]
		components, _ := document["components"].(map[string]any)
		schemas, _ := components["schemas"].(map[string]any)
		reached := responseShapes(t, documentName, document)
		found := []string{}
		for _, shape := range slices.Sorted(maps.Keys(reached)) {
			schema, _ := schemas[shape].(map[string]any)
			properties, _ := schema["properties"].(map[string]any)
			if _, named := properties["priority"]; !named {
				continue
			}
			found = append(found, shape)
			outside := slices.ContainsFunc(reached[shape], func(path string) bool { return !strings.HasPrefix(path, prioritiesGraphPath) })
			if !outside {
				t.Logf("%s: %s carries a priority, and only the Priorities graph's routes answer it: it is a priority's, no issue's", documentName, shape)
				continue
			}
			fields := slices.Sorted(maps.Keys(resolvedPriorityKinds))
			if exception, listed := resolvedShapeExceptions[shape]; listed {
				t.Logf("%s: %s is held to %v alone: %s", documentName, shape, exception.fields, exception.reason)
				fields = exception.fields
			}
			required, _ := schema["required"].([]any)
			requires := func(field string) bool { return slices.Contains(required, any(field)) }
			for _, field := range fields {
				property, declared := properties[field]
				if !declared {
					t.Errorf("%s: %s, which %v answer, carries an issue's priority and declares no %s beside it", documentName, shape, reached[shape], field)
					continue
				}
				kind := resolvedPriorityKinds[field]
				if types := schemaTypes(property); !types[kind] || !types["null"] {
					t.Errorf("%s: %s's %s admits %v, want a nullable %s", documentName, shape, field, slices.Sorted(maps.Keys(types)), kind)
				}
				if requires("priority") && !requires(field) {
					t.Errorf("%s: %s requires priority and not %s, which is always written, null included", documentName, shape, field)
				}
			}
		}
		if documentName != OpenAPIDocumentPath {
			continue
		}
		for _, shape := range resolvedShapes {
			if !slices.Contains(found, shape) {
				t.Errorf("%s: no response shape %s carries a priority, and the plan's decision 3 names it: the inventory found %v", documentName, shape, found)
			}
		}
	}
}

// TestTheReadAndTheListResolveThePriorityFromTheNearestIssueAbove (R2): on the seed, GET
// /api/v1/issues/{key} and the listing of each issue's project answer each issue's own priority as
// seeded and the priority of the nearest issue above it that has one, in any project: C and G 0
// from A, C0 0 its own, M2 2 its own, M2c 2 from M2, M0c 0 from M0, Nc none, X 1 from Q. A walk of
// the root's priority alone answers M2c 0 and M0c 1, and fails those lines. In a chain made by
// direct SQL whose one priority is its root's P0, the issue 32 links below the root reads 0 from
// it, and the issue 33 links below reads none: the walk stops at parentDepthCap links. After a
// person's write of A's priority to 1, the next read of C and G answers 1 from A.
func TestTheReadAndTheListResolveThePriorityFromTheNearestIssueAbove(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createTestProject(t, handler, "ACME", "OTHER", "DEEP")
	seed := seedResolvedPriorities(t, handler, "ACME", "OTHER")
	for _, want := range seed.wants() {
		project := "ACME"
		if want.key == seed.q {
			project = "OTHER"
		}
		seed.requireReadsResolved(t, handler, project, want)
	}

	t.Run(fmt.Sprintf("a chain of %d links whose one priority is its root's", parentDepthCap+1), func(t *testing.T) {
		// DEEP-1 is the root, at P0, and DEEP-n is n-1 links below it. The deepest issue names a
		// repository of its own, so the repository walk, which refuses a chain past the cap that
		// names none (TestIssueRepoWalkStopsAtTheDepthCap), answers its read.
		deepest := parentDepthCap + 2
		if _, err := database.Pool.Exec(context.Background(), `
			insert into issues (key, project_key, number, title, parent_key, priority, repo, created_by, rank)
			select 'DEEP-' || n, 'DEEP', n, 'Deep ' || n, case when n = 1 then null else 'DEEP-' || (n - 1) end,
			       case when n = 1 then 0 end, case when n = $1 then 'acme/deep' end,
			       '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
			from generate_series(1, $1::int) n
			order by n
		`, deepest); err != nil {
			t.Fatalf("insert a %d-link chain: %v", deepest-1, err)
		}
		root, atCap, past := "DEEP-1", fmt.Sprintf("DEEP-%d", deepest-1), fmt.Sprintf("DEEP-%d", deepest)
		chain := issueNames{
			root:  "the chain's root",
			atCap: fmt.Sprintf("the issue %d links below the root", parentDepthCap),
			past:  fmt.Sprintf("the issue %d links below the root", parentDepthCap+1),
		}
		chain.requireReadsResolved(t, handler, "DEEP", resolvedWant{key: atCap, resolved: new(0), from: root})
		chain.requireReadsResolved(t, handler, "DEEP", resolvedWant{key: past})
	})

	t.Run("after a person's write of A's priority to 1", func(t *testing.T) {
		requireTaken(t, "a person's write of A's priority to 1", dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+seed.a, map[string]any{"priority": 1}, "alice"), http.StatusOK)
		for _, key := range []string{seed.c, seed.g} {
			read := readResolved(t, handler, key)
			seed.requireResolvedAs(t, "GET /api/v1/issues/{key} after A's write", resolvedWant{key: key, resolved: new(1), from: seed.a},
				read.Priority, read.ResolvedPriority, read.PriorityFrom)
		}
	})
}

// TestTheListsPriorityFilterMatchesTheResolvedPriority (R3): on the seed's project, the listing's
// priority filter matches the priority each issue resolves to. priority=0 lists A, C, G, C0, M0
// and M0c; priority=1 lists B and X; priority=2 lists M2 and M2c; priority=none lists N and Nc; and
// priority=0&priority=none lists the union. Each issue a filter lists that it should not, and each
// it leaves out, fails a line of its own: a filter by the root's priority alone lists M2c under 0
// and M0c under 1, and fails those lines.
func TestTheListsPriorityFilterMatchesTheResolvedPriority(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createTestProject(t, handler, "ACME", "OTHER")
	seed := seedResolvedPriorities(t, handler, "ACME", "OTHER")
	zero := []string{seed.a, seed.c, seed.g, seed.c0, seed.m0, seed.m0c}
	none := []string{seed.n, seed.nc}
	for _, filter := range []struct {
		query string
		want  []string
	}{
		{"priority=0", zero},
		{"priority=1", []string{seed.b, seed.x}},
		{"priority=2", []string{seed.m2, seed.m2c}},
		{"priority=none", none},
		{"priority=0&priority=none", slices.Concat(zero, none)},
	} {
		listed := []string{}
		for _, row := range listResolved(t, handler, "project=ACME&"+filter.query) {
			listed = append(listed, row.Key)
		}
		for _, key := range filter.want {
			if !slices.Contains(listed, key) {
				t.Errorf("?project=ACME&%s leaves out %s, which reads %s", filter.query, seed.name(key), seed.reads(seed.want(t, key)))
			}
		}
		for _, key := range listed {
			if !slices.Contains(filter.want, key) {
				t.Errorf("?project=ACME&%s lists %s, which reads %s", filter.query, seed.name(key), seed.reads(seed.want(t, key)))
			}
		}
	}
}

// TestEveryPriorityReaderOrdersFiltersAndCountsByTheResolvedPriority (R4): each reader of an
// issue's priority but the issue read and the listing, on a server and a seed of its own, reads
// M0c, whose own priority is none and whose middle M0 is P0 under the P1 root B, as P0 from M0: the
// Inbox orders its ask with the P0 asks, before an ask on B and one on Nc; the mark-all and the
// dismissal with priority P0 reach its thread and not Nc's; the restore with priority P0 restores
// its thread; the replies route's row of its thread, the open asks of a session, the Legion board's
// card, the architecture tree's issue and the delivery timeline's row each carry resolved_priority 0
// and priority_from M0; and the delivery facet counts its pull request under P0. Each reader is its
// own subtest, so a reader left on the own priority, or on the root's, fails by name.
func TestEveryPriorityReaderOrdersFiltersAndCountsByTheResolvedPriority(t *testing.T) {
	t.Parallel()
	// seeded is a server of its own with the seed in ACME and OTHER.
	seeded := func(t *testing.T) (http.Handler, *store.Store, resolvedSeed) {
		t.Helper()
		handler, database := newTestHandlerWithStore(t)
		createTestProject(t, handler, "ACME", "OTHER")
		return handler, database, seedResolvedPriorities(t, handler, "ACME", "OTHER")
	}
	m0c := func(seed resolvedSeed) resolvedWant {
		return resolvedWant{key: seed.m0c, resolved: new(0), from: seed.m0}
	}
	// asked opens an ask from session on each issue of keys, in that order.
	asked := func(t *testing.T, handler http.Handler, seed resolvedSeed, session string, keys ...string) {
		t.Helper()
		for _, key := range keys {
			response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/asks", map[string]any{
				"question": "Is " + seed.issueNames[key] + " ready?", "actor": map[string]any{"kind": "session", "id": session},
			})
			if response.Code != http.StatusCreated {
				t.Fatalf("open an ask on %s: status=%d body=%s", seed.name(key), response.Code, response.Body.String())
			}
		}
	}
	// threads is one unread Reply thread on each issue of keys, which a session replied in.
	threads := func(t *testing.T, handler http.Handler, keys ...string) []string {
		t.Helper()
		ids := make([]string, len(keys))
		for index, key := range keys {
			ids[index] = seedUnreadThread(t, handler, "/api/v1/issues/"+key+"/comments", "Is this ready?")
		}
		return ids
	}

	t.Run("the Inbox orders an ask on M0c with the P0 asks", func(t *testing.T) {
		handler, _, seed := seeded(t)
		asked(t, handler, seed, "session-asks-in-the-inbox", seed.m0c, seed.b, seed.nc)
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/inbox: status=%d body=%s", response.Code, response.Body.String())
		}
		order := []string{}
		for _, row := range decodeBody[[]struct {
			Issue *struct {
				Key string `json:"key"`
			} `json:"issue"`
			Priority         *int    `json:"priority"`
			ResolvedPriority *int    `json:"resolved_priority"`
			PriorityFrom     *string `json:"priority_from"`
		}](t, response) {
			if row.Issue == nil {
				continue
			}
			order = append(order, row.Issue.Key)
			if row.Issue.Key == seed.m0c {
				seed.requireResolvedAs(t, "GET /api/v1/inbox's row of the ask on M0c", m0c(seed), row.Priority, row.ResolvedPriority, row.PriorityFrom)
			}
		}
		at := slices.Index(order, seed.m0c)
		if at < 0 {
			t.Fatalf("GET /api/v1/inbox lists %v, and no ask on %s", order, seed.name(seed.m0c))
		}
		if at > slices.Index(order, seed.b) {
			t.Errorf("GET /api/v1/inbox orders the ask on %s after the ask on %s, at P1: %v, want it with the P0 asks", seed.name(seed.m0c), seed.name(seed.b), order)
		}
		if at > slices.Index(order, seed.nc) {
			t.Errorf("GET /api/v1/inbox orders the ask on %s after the ask on %s, which has no priority: %v", seed.name(seed.m0c), seed.name(seed.nc), order)
		}
	})

	t.Run("mark-all-read with priority 0 marks M0c's thread and not Nc's", func(t *testing.T) {
		handler, database, seed := seeded(t)
		ids := threads(t, handler, seed.m0c, seed.nc)
		markedThrough(t, handler, "alice", time.Now(), map[string]any{"priority": []string{"P0"}})
		if _, unread := threadMark(t, database, "alice", ids[0]); unread {
			t.Errorf("POST /api/v1/me/inbox/read with priority P0 left the thread on %s unread, which reads P0 from M0", seed.name(seed.m0c))
		}
		if _, unread := threadMark(t, database, "alice", ids[1]); !unread {
			t.Errorf("POST /api/v1/me/inbox/read with priority P0 marked the thread on %s, which has no priority", seed.name(seed.nc))
		}
	})

	t.Run("dismiss with priority 0 dismisses M0c's thread and not Nc's", func(t *testing.T) {
		handler, database, seed := seeded(t)
		ids := threads(t, handler, seed.m0c, seed.nc)
		dismissThrough(t, handler, "alice", time.Now(), map[string]any{"priority": []string{"P0"}})
		if at := threadDismissal(t, database, "alice", ids[0]); at == nil {
			t.Errorf("POST /api/v1/me/inbox/dismiss with priority P0 left the thread on %s in the Inbox, which reads P0 from M0", seed.name(seed.m0c))
		}
		if at := threadDismissal(t, database, "alice", ids[1]); at != nil {
			t.Errorf("POST /api/v1/me/inbox/dismiss with priority P0 dismissed the thread on %s, which has no priority, at %s", seed.name(seed.nc), at)
		}
	})

	t.Run("restore with priority 0 restores M0c's thread", func(t *testing.T) {
		handler, database, seed := seeded(t)
		ids := threads(t, handler, seed.m0c)
		if dismissed := dismissRows(t, handler, database, "alice", inboxDismissReplyPrefix+ids[0]); dismissed != 1 {
			t.Fatalf("dismiss the thread on %s by name: dismissed %d, want 1", seed.name(seed.m0c), dismissed)
		}
		restored := postInboxDismiss(t, handler, "alice", map[string]any{
			"dismiss_through": time.Now().UTC().Format(time.RFC3339Nano), "priority": []string{"P0"}, "restore": true,
		})
		requireTaken(t, "the restore with priority P0", restored, http.StatusOK)
		if at := threadDismissal(t, database, "alice", ids[0]); at != nil {
			t.Errorf("POST /api/v1/me/inbox/dismiss with priority P0 and restore left the thread on %s dismissed at %s, which reads P0 from M0", seed.name(seed.m0c), at)
		}
	})

	t.Run("the replies route's row of M0c's thread carries its resolved priority", func(t *testing.T) {
		handler, _, seed := seeded(t)
		ids := threads(t, handler, seed.m0c)
		row := replyRow(t, getMyReplies(t, handler, "alice", ""), ids[0])
		seed.requireResolvedAs(t, "GET /api/v1/me/replies's row of the thread on M0c", m0c(seed), row.Priority, row.ResolvedPriority, row.PriorityFrom)
	})

	t.Run("a session's open asks order M0c's ask as P0", func(t *testing.T) {
		handler, _, seed := seeded(t)
		const session = "session-asks-on-the-seed"
		// B's ask is the older, so only M0c's resolved P0 puts M0c's first: the open asks are
		// ordered by priority, then oldest first.
		asked(t, handler, seed, session, seed.b, seed.m0c)
		response := sessionRequest(t, handler, http.MethodGet, "/api/v1/asks/open?author_session="+session, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/asks/open: status=%d body=%s", response.Code, response.Body.String())
		}
		order := []string{}
		for _, ask := range decodeBody[openAsksResponse](t, response).Asks {
			if ask.Owner.Issue == nil {
				continue
			}
			order = append(order, ask.Owner.Issue.Key)
			if ask.Owner.Issue.Key == seed.m0c {
				seed.requireResolvedAs(t, "GET /api/v1/asks/open's ask on M0c", m0c(seed), ask.Priority, ask.ResolvedPriority, ask.PriorityFrom)
			}
		}
		if at := slices.Index(order, seed.m0c); at < 0 || at > slices.Index(order, seed.b) {
			t.Errorf("GET /api/v1/asks/open orders %v, want the ask on %s, at P0 from M0, before the ask on %s, at P1", order, seed.name(seed.m0c), seed.name(seed.b))
		}
	})

	t.Run("the Legion board's card of M0c", func(t *testing.T) {
		handler, database, seed := seeded(t)
		nameLegionDaemon(t, handler, "ACME", "http://legion-acme.internal:13370")
		now := time.Now().UTC()
		seedLegionObservation(t, database, "ACME", seed.m0c, legion.FieldPhase, `"`+legion.PhaseImplementing+`"`, now.Add(-time.Hour))
		seedLegionRead(t, database, "ACME", now, "")
		card := legionCard(t, legionBoardOf(t, handler, "ACME"), seed.m0c)
		seed.requireResolvedAs(t, "GET /api/v1/projects/ACME/legion/board's card", m0c(seed), card.Priority, card.ResolvedPriority, card.PriorityFrom)
	})

	t.Run("the architecture tree's issue M0c", func(t *testing.T) {
		handler, _ := newComponentTreeServer(t)
		createTestProject(t, handler, "OTHER")
		seed := seedResolvedPriorities(t, handler, "CORE", "OTHER")
		requireTaken(t, "attach M0c to the daemon component", patchComponents(t, handler, seed.m0c, map[string]any{"mode": "explicit", "ids": []string{"daemon"}}), http.StatusOK)
		component := treeComponent(t, readTree(t, handler), "daemon")
		index := slices.IndexFunc(component.Issues, func(issue model.ArchitectureTreeIssue) bool { return issue.Key == seed.m0c })
		if index < 0 {
			t.Fatalf("GET /api/v1/projects/CORE/architecture counts no %s under daemon: %+v", seed.name(seed.m0c), component.Issues)
		}
		issue := component.Issues[index]
		seed.requireResolvedAs(t, "GET /api/v1/projects/CORE/architecture's issue", m0c(seed), issue.Priority, issue.ResolvedPriority, issue.PriorityFrom)
	})

	t.Run("the delivery facet counts M0c's pull request under P0", func(t *testing.T) {
		handler, database, seed := seeded(t)
		ctx := t.Context()
		if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
			DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
			ProductionJobNames: []string{"release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
			PopulationAuthors: []string{"octocat"},
		}, model.Actor{Kind: "system", ID: "test"}); err != nil {
			t.Fatalf("seed the delivery settings: %v", err)
		}
		merged := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
		for number, key := range map[int]string{1: seed.m0c, 2: seed.nc} {
			at := merged.Add(time.Duration(number) * time.Minute)
			if err := delivery.UpsertPullRequest(ctx, database.Pool, delivery.DeliveryPullRequest{
				Repo: "acme/widgets", Number: number, Title: "feat: " + seed.issueNames[key], Author: "octocat",
				URL: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number), IssueKey: &key, MergedAt: &at, CreatedAt: &at,
			}); err != nil {
				t.Fatalf("seed the pull request of %s: %v", seed.name(key), err)
			}
		}
		timeline := getDeliveryTimeline(t, handler, "from="+merged.Add(-time.Hour).Format(time.RFC3339)+"&to="+merged.Add(time.Hour).Format(time.RFC3339))
		index := slices.IndexFunc(timeline.PRs, func(pr delivery.DeliveryPRView) bool { return pr.ID == "acme/widgets#1" })
		if index < 0 {
			t.Fatalf("GET /api/v1/delivery/timeline lists no acme/widgets#1, the pull request of %s: %+v", seed.name(seed.m0c), timeline.PRs)
		}
		pr := timeline.PRs[index]
		if pr.Priority == nil || *pr.Priority != "P0" {
			t.Errorf("GET /api/v1/delivery/timeline's row of acme/widgets#1, the pull request of %s, has priority %s, want P0 from M0", seed.name(seed.m0c), cmp.Or(deref(pr.Priority), "none"))
		}
		if pr.PriorityFrom == nil || *pr.PriorityFrom != seed.m0 {
			t.Errorf("GET /api/v1/delivery/timeline's row of acme/widgets#1, the pull request of %s, has priority_from %s, want %s", seed.name(seed.m0c), seed.keyName(deref(pr.PriorityFrom)), seed.name(seed.m0))
		}
		if counted := timeline.FacetCounts["priority"]["P0"]; counted != 1 {
			t.Errorf("GET /api/v1/delivery/timeline's priority facet counts %d pull requests under P0 (%v), want acme/widgets#1, the pull request of %s", counted, timeline.FacetCounts["priority"], seed.name(seed.m0c))
		}
	})
}

// TestAMoveOrAClearThatMakesWorkP0NeedsP0 (R5): under the shared token, which holds no scope, each
// from a fresh seed, a write that makes the written issue, or an issue under it that takes its
// priority from it, go from a resolved priority that is not 0 to 0 is refused 403
// OAUTH_SCOPE_REQUIRED naming p0 (requireP0Refusal), and leaves each issue's parent, status, own
// priority, last_seq and resolved priority as they were: an own priority of 0 on C; a move of Y
// (none), whose child Yc has none, under A and under M0, which a check by the root's priority alone
// lets write; a clear of M2's own priority, which a check that skips a clear lets write. A move or a
// clear that makes no work P0 writes, and each issue reads its new resolved priority: Y under M2,
// which a check by the root alone refuses, Y and Yc reading 2 from M2; Z, with its own P2, under A,
// Zc keeping 2 from Z; a leaf L at its own P2 under A; Y from under C0, its own P0, to under A,
// every resolved priority staying 0; a clear of M0's own priority, M0 and M0c reading 1 from B; and
// a clear of C0's own P0, C0 reading 0 from A. The write reads the chain whole, whatever its writer
// can see: a credential scoped to a public project that moves an issue under a public child of a
// private project's P0 root is refused too.
func TestAMoveOrAClearThatMakesWorkP0NeedsP0(t *testing.T) {
	t.Parallel()
	rig := newResolvedRig(t, testServerOptions{})
	shared := rig.credential(t, legionSharedToken)
	// refused sends body as the shared token's PATCH of key and fails unless it is the refusal
	// naming p0 and each issue of kept is as it was.
	refused := func(t *testing.T, seed resolvedSeed, key string, body map[string]any, kept ...string) {
		t.Helper()
		before := rig.readResolvedStates(t, kept...)
		requireP0Refusal(t, shared.send(t, http.MethodPatch, "/api/v1/issues/"+key, body))
		rig.requireResolvedStatesKept(t, seed.issueNames, before)
	}
	// written sends body as the shared token's PATCH of key, which needs no scope, and fails
	// unless it writes and each issue reads as wants say.
	written := func(t *testing.T, seed resolvedSeed, key string, body map[string]any, wants ...resolvedWant) {
		t.Helper()
		requireTaken(t, "the shared token's PATCH of "+seed.name(key), shared.send(t, http.MethodPatch, "/api/v1/issues/"+key, body), http.StatusOK)
		for _, want := range wants {
			read := readResolved(t, rig.handler, want.key)
			seed.requireResolvedAs(t, "GET /api/v1/issues/{key} after the write", want, read.Priority, read.ResolvedPriority, read.PriorityFrom)
		}
	}

	t.Run("an own priority of 0 on C", func(t *testing.T) {
		seed := rig.freshSeed(t)
		refused(t, seed, seed.c, map[string]any{"priority": 0}, seed.c, seed.g)
	})
	t.Run("a move of Y, whose child Yc has none, under A", func(t *testing.T) {
		seed := rig.freshSeed(t)
		y, yc := rig.inheritingPair(t, seed, "Y", "")
		refused(t, seed, y, map[string]any{"parent": seed.a}, y, yc)
	})
	t.Run("a move of Y, whose child Yc has none, under M0", func(t *testing.T) {
		seed := rig.freshSeed(t)
		y, yc := rig.inheritingPair(t, seed, "Y", "")
		refused(t, seed, y, map[string]any{"parent": seed.m0}, y, yc)
	})
	t.Run("a move of Y, whose child Yc has none, under M2 writes", func(t *testing.T) {
		seed := rig.freshSeed(t)
		y, yc := rig.inheritingPair(t, seed, "Y", "")
		written(t, seed, y, map[string]any{"parent": seed.m2},
			resolvedWant{key: y, resolved: new(2), from: seed.m2}, resolvedWant{key: yc, resolved: new(2), from: seed.m2})
	})
	t.Run("a move under A of Z, at its own P2, whose child Zc has none, writes", func(t *testing.T) {
		seed := rig.freshSeed(t)
		z := rig.personIssue(t, map[string]any{"priority": 2}).Key
		zc := rig.personIssue(t, map[string]any{"parent": z}).Key
		seed.issueNames[z], seed.issueNames[zc] = "Z", "Zc"
		written(t, seed, z, map[string]any{"parent": seed.a},
			resolvedWant{key: z, own: new(2), resolved: new(2)}, resolvedWant{key: zc, resolved: new(2), from: z})
	})
	t.Run("a move under A of a leaf L at its own P2 writes", func(t *testing.T) {
		seed := rig.freshSeed(t)
		leaf := rig.personIssue(t, map[string]any{"priority": 2}).Key
		seed.issueNames[leaf] = "L"
		written(t, seed, leaf, map[string]any{"parent": seed.a}, resolvedWant{key: leaf, own: new(2), resolved: new(2)})
	})
	t.Run("a move of Y from under C0, at its own P0, to under A writes", func(t *testing.T) {
		seed := rig.freshSeed(t)
		y, yc := rig.inheritingPair(t, seed, "Y", seed.c0)
		written(t, seed, y, map[string]any{"parent": seed.a},
			resolvedWant{key: y, resolved: new(0), from: seed.a}, resolvedWant{key: yc, resolved: new(0), from: seed.a})
	})
	t.Run("a clear of M2's own priority", func(t *testing.T) {
		seed := rig.freshSeed(t)
		refused(t, seed, seed.m2, map[string]any{"priority": nil}, seed.m2, seed.m2c)
	})
	t.Run("a clear of M0's own priority writes", func(t *testing.T) {
		seed := rig.freshSeed(t)
		written(t, seed, seed.m0, map[string]any{"priority": nil},
			resolvedWant{key: seed.m0, resolved: new(1), from: seed.b}, resolvedWant{key: seed.m0c, resolved: new(1), from: seed.b})
	})
	t.Run("a clear of C0's own P0 writes", func(t *testing.T) {
		seed := rig.freshSeed(t)
		written(t, seed, seed.c0, map[string]any{"priority": nil}, resolvedWant{key: seed.c0, resolved: new(0), from: seed.a})
	})
	t.Run("a move under a public child of a private project's P0 root, by a credential scoped to the public project", func(t *testing.T) {
		private := newPrivateProjectRig(t, testServerOptions{})
		root := private.issue(t, privateTestProject, "A private project's P0 root", map[string]any{"priority": 0})
		child := private.issue(t, brokerTestProject, "A public child of the private P0 root", map[string]any{"parent": root})
		moved := private.issue(t, brokerTestProject, "An issue with no priority the scoped token moves", nil)
		private.makePrivate(t)
		names := issueNames{root: "the private P0 root", child: "its public child", moved: "the moved issue"}
		before := private.readResolvedStates(t, moved)
		requireP0Refusal(t, private.send(t, scopedToken, http.MethodPatch, "/api/v1/issues/"+moved, map[string]any{"parent": child}))
		private.requireResolvedStatesKept(t, names, before)
	})
}

// TestADetachNeverNeedsP0 (R6): under the shared token, which holds no scope, a detach writes,
// since taking away the issues above an issue only turns a priority it took from them into none. A
// PATCH that sets no parent on M0 writes, and M0c keeps 0 from M0; the same on C writes, and C reads
// none; and POST /api/v1/issues/{key}/status-change on B with done and unlink_children writes, M0 is
// a root, and M0c keeps 0 from M0. A check by the root's priority, under which M0c goes from 1 to 0,
// refuses the first and the third.
func TestADetachNeverNeedsP0(t *testing.T) {
	t.Parallel()
	rig := newResolvedRig(t, testServerOptions{})
	shared := rig.credential(t, legionSharedToken)
	// detached fails unless key is a root and each issue reads as wants say.
	detached := func(t *testing.T, seed resolvedSeed, key string, wants ...resolvedWant) {
		t.Helper()
		if parent := readResolved(t, rig.handler, key).Parent; parent != nil {
			t.Errorf("%s after the detach is under %s, want a root", seed.name(key), seed.name(*parent))
		}
		for _, want := range wants {
			read := readResolved(t, rig.handler, want.key)
			seed.requireResolvedAs(t, "GET /api/v1/issues/{key} after the detach", want, read.Priority, read.ResolvedPriority, read.PriorityFrom)
		}
	}

	t.Run("a PATCH that sets no parent on M0", func(t *testing.T) {
		seed := rig.freshSeed(t)
		requireTaken(t, "the detach of M0", shared.send(t, http.MethodPatch, "/api/v1/issues/"+seed.m0, map[string]any{"parent": nil}), http.StatusOK)
		detached(t, seed, seed.m0, resolvedWant{key: seed.m0c, resolved: new(0), from: seed.m0})
	})
	t.Run("a PATCH that sets no parent on C", func(t *testing.T) {
		seed := rig.freshSeed(t)
		requireTaken(t, "the detach of C", shared.send(t, http.MethodPatch, "/api/v1/issues/"+seed.c, map[string]any{"parent": nil}), http.StatusOK)
		detached(t, seed, seed.c, resolvedWant{key: seed.c})
	})
	t.Run("a status change of B to done with unlink_children", func(t *testing.T) {
		seed := rig.freshSeed(t)
		requireTaken(t, "the status change of B", shared.send(t, http.MethodPost, legionStatusChangePath(seed.b), map[string]any{
			"status": "done", "unlink_children": true,
		}), http.StatusOK)
		detached(t, seed, seed.m0, resolvedWant{key: seed.m0c, resolved: new(0), from: seed.m0})
	})
}

// The sessions of TestANewChildOfAP0IssueNeedsP0UnlessItsParentsArchitectFilesIt's role holders,
// which the Legion daemon of the seed's project publishes (publishSeedRoles), and a session that
// holds no role.
const (
	architectOfA    = "ses-architect-of-a"
	architectOfM0   = "ses-architect-of-m0"
	implementerOfM0 = "ses-implementer-of-m0"
	architectOfB    = "ses-architect-of-b"
	claimantOfM0    = "ses-claimant-of-m0"
	holdsNoRole     = "ses-holds-no-role"
)

// publishSeedRoles publishes the role holders of seed's A, M0 and B from the daemon's own broker
// token, as the daemon of brokerTestProject: an architect for A, an architect and an implementer
// for M0, and an architect for B. Then a fifth session claims M0.
func (rig *legionStatusRig) publishSeedRoles(t *testing.T, seed resolvedSeed) {
	t.Helper()
	rig.publishRoles(t, seed.a, architectOf(architectOfA))
	rig.publishRoles(t, seed.m0, architectOf(architectOfM0), issueRoleHolder{Role: "implementer", Session: implementerOfM0})
	rig.publishRoles(t, seed.b, architectOf(architectOfB))
	requireTaken(t, "the claim of M0", rig.credential(t, legionSharedToken).send(t, http.MethodPost, "/api/v1/issues/"+seed.m0+"/claim", map[string]any{
		"actor": claimActorBody(claimantOfM0, "Implementer"),
	}), http.StatusOK)
}

// TestANewChildOfAP0IssueNeedsP0UnlessItsParentsArchitectFilesIt (R7): a create with no priority of
// its own under a parent that reads P0 needs the p0 scope, unless the writer's session is the
// session of the parent's own architect row, in whatever project the parent is. On credentials
// without p0, with the seed's role holders published: under A by A's architect, and under M0 by
// M0's architect, the create writes and reads 0 from the parent, which a rule that ignores the role
// holders refuses. It is refused 403 naming p0, and writes no issue and no near_copy_checks row,
// under C by a session that is no architect of C, which a rule reading only the parent's own
// priority lets write; under M0 by a session that holds no role, by M0's implementer, by the holder
// of M0's claim, and by B's architect, which a rule that counts another role, the claim or an
// architect further up lets write. A new issue of the second project under M0 writes by M0's
// architect and is refused by another session. With the project's proven-sessions setting on, M0's
// architect named on the shared token is refused, and a broker token that proves that session
// writes. A create with its own priority 1 under M0 writes and reads 1, and a credential that holds
// p0 and no role writes. Under a parent in a private project the credential does not name, the
// create answers what it answers today, a missing parent's 400 PARENT_INPUT; under a public child of
// a private project's P0 root, the broker token that names the public project alone is refused 403
// naming p0, since the check reads the chain whole.
func TestANewChildOfAP0IssueNeedsP0UnlessItsParentsArchitectFilesIt(t *testing.T) {
	t.Parallel()
	rig := newResolvedRig(t, testServerOptions{})
	shared := rig.credential(t, legionSharedToken)
	// roled is a fresh seed with its role holders published.
	roled := func(t *testing.T) resolvedSeed {
		t.Helper()
		seed := rig.freshSeed(t)
		rig.publishSeedRoles(t, seed)
		return seed
	}
	// refused fails unless the create from session under parent, a new issue of project, is the
	// refusal naming p0 and files nothing.
	refused := func(t *testing.T, project, parent, session string) {
		t.Helper()
		title, response := rig.fileChild(t, shared.send, project, parent, session, nil)
		requireP0Refusal(t, response)
		requireNothingFiled(t, rig.brokerTokenRig, title)
	}
	// filed fails unless session's create of a new issue of project with fields, under parent, an
	// issue of seed, writes, and the new issue reads as want says.
	filed := func(t *testing.T, seed resolvedSeed, project, parent, session string, fields map[string]any, want resolvedWant) {
		t.Helper()
		_, response := rig.fileChild(t, shared.send, project, parent, session, fields)
		seed.requireFiledAs(t, rig.handler, "the new child of "+seed.issueNames[parent], response, want)
	}

	t.Run("under A by A's architect", func(t *testing.T) {
		seed := roled(t)
		filed(t, seed, brokerTestProject, seed.a, architectOfA, nil, resolvedWant{resolved: new(0), from: seed.a})
	})
	t.Run("under M0 by M0's architect", func(t *testing.T) {
		seed := roled(t)
		filed(t, seed, brokerTestProject, seed.m0, architectOfM0, nil, resolvedWant{resolved: new(0), from: seed.m0})
	})
	t.Run("under C by a session that is no architect of C", func(t *testing.T) {
		refused(t, brokerTestProject, roled(t).c, holdsNoRole)
	})
	t.Run("under M0 by a session that holds no role", func(t *testing.T) {
		refused(t, brokerTestProject, roled(t).m0, holdsNoRole)
	})
	t.Run("under M0 by M0's implementer", func(t *testing.T) {
		refused(t, brokerTestProject, roled(t).m0, implementerOfM0)
	})
	t.Run("under M0 by the holder of M0's claim", func(t *testing.T) {
		refused(t, brokerTestProject, roled(t).m0, claimantOfM0)
	})
	t.Run("under M0 by B's architect", func(t *testing.T) {
		refused(t, brokerTestProject, roled(t).m0, architectOfB)
	})
	t.Run("a new issue of the second project under M0 by M0's architect", func(t *testing.T) {
		seed := roled(t)
		filed(t, seed, legionOtherProject, seed.m0, architectOfM0, nil, resolvedWant{resolved: new(0), from: seed.m0})
	})
	t.Run("a new issue of the second project under M0 by a session that holds no role", func(t *testing.T) {
		refused(t, legionOtherProject, roled(t).m0, holdsNoRole)
	})
	t.Run("a create with its own priority 1 under M0 by a session that holds no role", func(t *testing.T) {
		seed := roled(t)
		filed(t, seed, brokerTestProject, seed.m0, holdsNoRole, map[string]any{"priority": 1}, resolvedWant{own: new(1), resolved: new(1)})
	})
	t.Run("a create under M0 by a credential that holds p0 and no role", func(t *testing.T) {
		seed := roled(t)
		holder := rig.bearer(personalTokenHolding(t, rig.database, model.ScopeP0), sessionActor())
		_, response := rig.fileChild(t, holder, brokerTestProject, seed.m0, "", nil)
		seed.requireFiledAs(t, rig.handler, "the p0 holder's new child of M0", response, resolvedWant{resolved: new(0), from: seed.m0})
	})

	t.Run("with the project's proven-sessions setting on", func(t *testing.T) {
		proven := newResolvedRig(t, testServerOptions{})
		seed := proven.freshSeed(t)
		proven.publishSeedRoles(t, seed)
		requireTaken(t, "a person turns the proven-sessions setting on", proven.asPerson(t, http.MethodPut, "/api/v1/projects/"+brokerTestProject+"/proven-sessions", map[string]any{}), http.StatusOK)
		t.Run("M0's architect named on the shared token", func(t *testing.T) {
			title, response := proven.fileChild(t, proven.credential(t, legionSharedToken).send, brokerTestProject, seed.m0, architectOfM0, nil)
			requireP0Refusal(t, response)
			requireNothingFiled(t, proven.brokerTokenRig, title)
		})
		t.Run("a broker token that proves M0's architect", func(t *testing.T) {
			_, response := proven.fileChild(t, proven.provenSession(t, architectOfM0).send, brokerTestProject, seed.m0, "", nil)
			seed.requireFiledAs(t, proven.handler, "M0's proven architect's new child of M0", response, resolvedWant{resolved: new(0), from: seed.m0})
		})
	})

	t.Run("on a private project's P0 root", func(t *testing.T) {
		private := newPrivateProjectRig(t, testServerOptions{})
		root := private.issue(t, privateTestProject, "A private project's P0 root", map[string]any{"priority": 0})
		child := private.issue(t, brokerTestProject, "A public child of the private P0 root", map[string]any{"parent": root})
		private.makePrivate(t)
		create := func(parent, title string) func() *httptest.ResponseRecorder {
			return func() *httptest.ResponseRecorder {
				return private.send(t, scopedToken, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": brokerTestProject, "title": title, "parent": parent, "force": true,
				})
			}
		}
		t.Run("a create under the private root, which the credential does not name", func(t *testing.T) {
			private.refusedAsMissing(t, []string{root}, http.StatusBadRequest, "PARENT_INPUT", root, missingIssue,
				create(root, "Filed under the private P0 root"), create(missingIssue, "Filed under the private P0 root"))
		})
		t.Run("a create under the root's public child", func(t *testing.T) {
			const title = "Filed under the public child of the private P0 root"
			requireP0Refusal(t, create(child, title)())
			requireNothingFiled(t, private.brokerTokenRig, title)
		})
	})
}

// TestARefusedMoveClearOrCreateWritesNothingElseOfItsRequest (R8): under the shared token, a write
// refused for want of p0 writes nothing else of its request. A PATCH that closes M2 and clears its
// own priority, and one that closes an issue with no priority and moves it under A, each on an
// issue whose document holds an edit the close's settlement would save, are refused naming p0
// before the settlement: the issue stays open, its document gains no version, and no ask event is
// appended. A create under M0 with a title and labels by a session that holds no role leaves no
// issue and no near_copy_checks row for that title.
func TestARefusedMoveClearOrCreateWritesNothingElseOfItsRequest(t *testing.T) {
	t.Parallel()
	// Settlement an hour away: the edit each close's case writes is still unsettled when the close
	// is refused, so only a settlement the close ran could save it.
	rig := newResolvedRig(t, testServerOptions{settle: time.Hour})
	shared := rig.credential(t, legionSharedToken)
	// refusedClose types a decision block into key's spec, as a person at a browser does, and fails
	// unless the shared token's close of key with body is the refusal naming p0, each issue of kept
	// is as it was, the spec has no new version and key has no new ask event.
	refusedClose := func(t *testing.T, seed resolvedSeed, key string, body map[string]any, kept ...string) {
		t.Helper()
		spec := readResolved(t, rig.handler, key).PrimaryArtifactID
		if _, err := replaceDocumentText(rig.database, rig.served.deps.Docs, spec,
			"Context\n\n:::ask{#p0-close urgency=\"high\" multiple=\"false\"}\nShip it now?\n:::\n",
			model.Actor{Kind: "user", ID: brokerTestPerson}); err != nil {
			t.Fatalf("rig: a person types a decision block into %s's spec: %v", seed.name(key), err)
		}
		before := rig.readResolvedStates(t, kept...)
		versions := specVersionCount(t, rig.database, spec)
		askEvents := func() int64 {
			return countRows(t, rig.database, `select count(*) from events where issue_key = $1 and type like 'ask.%'`, key)
		}
		asked := askEvents()
		requireP0Refusal(t, shared.send(t, http.MethodPatch, "/api/v1/issues/"+key, body))
		rig.requireResolvedStatesKept(t, seed.issueNames, before)
		if after := specVersionCount(t, rig.database, spec); after != versions {
			t.Errorf("the refused close left %s's document with %d versions, want the %d it had: it settled the document", seed.name(key), after, versions)
		}
		if after := askEvents(); after != asked {
			t.Errorf("the refused close appended %d ask events on %s (ask.withdrawn among them), want none", after-asked, seed.name(key))
		}
	}

	t.Run("a close that clears M2's own priority", func(t *testing.T) {
		seed := rig.freshSeed(t)
		refusedClose(t, seed, seed.m2, map[string]any{"status": "done", "priority": nil}, seed.m2, seed.m2c)
	})
	t.Run("a close that moves an issue with no priority under A", func(t *testing.T) {
		seed := rig.freshSeed(t)
		closed := rig.personIssue(t, nil).Key
		seed.issueNames[closed] = "the closed issue"
		refusedClose(t, seed, closed, map[string]any{"status": "done", "parent": seed.a}, closed)
	})
	t.Run("a create under M0 with a title and labels by a session that holds no role", func(t *testing.T) {
		seed := rig.freshSeed(t)
		title, response := rig.fileChild(t, shared.send, brokerTestProject, seed.m0, holdsNoRole, map[string]any{"labels": []string{"area", "ops"}})
		requireP0Refusal(t, response)
		requireNothingFiled(t, rig.brokerTokenRig, title)
	})
}

// The words TestTheRouteIndexSaysWhenAMoveAClearOrANewChildNeedsP0 reads in a sentence of a route's
// description.
var (
	movesWord     = regexp.MustCompile(`(?i)\bmov(e|es|ing)\b`)
	parentWord    = regexp.MustCompile(`(?i)\bparent\b`)
	clearWord     = regexp.MustCompile(`(?i)\bclear(s|ing|ed)?\b`)
	needsP0       = regexp.MustCompile(`(?i)\bneeds? (the )?` + model.ScopeP0 + `\b`)
	noParent      = regexp.MustCompile(`(?i)\bno parent\b|\bparent (of |to )?null\b|\bdetach`)
	needsNone     = regexp.MustCompile(`(?i)\bneeds? (none|no)\b`)
	noPriority    = regexp.MustCompile(`(?i)\bno priority\b`)
	readsP0       = regexp.MustCompile(`\bP0\b|(?i:\bpriority (of )?0\b)`)
	architectWord = regexp.MustCompile(`(?i)\barchitect\b`)
	unlinkWord    = regexp.MustCompile(`\bunlink_children\b`)
)

// TestTheRouteIndexSaysWhenAMoveAClearOrANewChildNeedsP0 (R9): GET /api/v1, the route index every
// agent reads, says when a move, a clear or a new child of a P0 issue needs p0. The entry of PATCH
// /api/v1/issues/{key} holds a sentence that names a move under a parent and says it needs p0, one
// that names a clear and says it needs p0, and one that says a move that sets no parent needs none;
// the entry of POST /api/v1/issues one that names a new issue with no priority under a parent that
// reads P0, says it needs p0 and names the parent's architect; and the entry of the status-change
// route one that says unlink_children needs no p0. A sentence is the description cut at each full
// stop, and its other words are the table's own.
func TestTheRouteIndexSaysWhenAMoveAClearOrANewChildNeedsP0(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	Register(mux, t.Context(), Deps{AgentTokens: sharedAgentTokens(t, "agent-token")})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/v1: status=%d body=%s", response.Code, response.Body.String())
	}
	entries := map[string]routeIndexEntry{}
	for _, entry := range decodeBody[routeIndex](t, response).Routes {
		entries[entry.Method+" "+entry.Path] = entry
	}
	for _, rule := range []struct {
		route, says string
		words       []*regexp.Regexp
	}{
		{"PATCH /api/v1/issues/{key}", "that a move under a parent needs p0", []*regexp.Regexp{movesWord, parentWord, needsP0}},
		{"PATCH /api/v1/issues/{key}", "that a clear needs p0", []*regexp.Regexp{clearWord, needsP0}},
		{"PATCH /api/v1/issues/{key}", "that a move that sets no parent needs none", []*regexp.Regexp{movesWord, noParent, needsNone}},
		{"POST /api/v1/issues", "that a new issue with no priority under a P0 parent needs p0 unless the parent's architect files it",
			[]*regexp.Regexp{noPriority, parentWord, readsP0, needsP0, architectWord}},
		{"POST /api/v1/issues/{key}/status-change", "that unlink_children needs no p0", []*regexp.Regexp{unlinkWord, p0Scope, needsNone}},
	} {
		entry, listed := entries[rule.route]
		if !listed {
			t.Errorf("GET /api/v1 lists no %s", rule.route)
			continue
		}
		says := slices.ContainsFunc(sentenceEnd.Split(entry.Description, -1), func(sentence string) bool {
			return !slices.ContainsFunc(rule.words, func(word *regexp.Regexp) bool { return !word.MatchString(sentence) })
		})
		if !says {
			t.Errorf("GET /api/v1's entry of %s has no sentence that says %s (one matching each of %v): %s", rule.route, rule.says, rule.words, entry.Description)
		}
	}
}

// resolvedP0Requests are the requests of TestEveryOperationThatTakesAPriorityOrAParentRunsTheP0Rows,
// by the operation, as "METHOD path", that each one sends under the shared token on a fresh seed.
// Each operation of a committed OpenAPI document whose request body takes a priority or a parent
// has its requests here, or its reason in resolvedP0Filters.
var resolvedP0Requests = map[string]func(t *testing.T, rig *legionStatusRig){
	"POST /api/v1/issues": func(t *testing.T, rig *legionStatusRig) {
		t.Helper()
		seed := rig.freshSeed(t)
		title, response := rig.fileChild(t, rig.credential(t, legionSharedToken).send, brokerTestProject, seed.a, holdsNoRole, nil)
		requireP0Refusal(t, response)
		requireNothingFiled(t, rig.brokerTokenRig, title)
	},
	"PATCH /api/v1/issues/{key}": func(t *testing.T, rig *legionStatusRig) {
		t.Helper()
		shared := rig.credential(t, legionSharedToken)
		t.Run("a move of an issue with no priority under A", func(t *testing.T) {
			seed := rig.freshSeed(t)
			y, yc := rig.inheritingPair(t, seed, "Y", "")
			before := rig.readResolvedStates(t, y, yc)
			requireP0Refusal(t, shared.send(t, http.MethodPatch, "/api/v1/issues/"+y, map[string]any{"parent": seed.a}))
			rig.requireResolvedStatesKept(t, seed.issueNames, before)
		})
		t.Run("a clear of M2's own priority", func(t *testing.T) {
			seed := rig.freshSeed(t)
			before := rig.readResolvedStates(t, seed.m2, seed.m2c)
			requireP0Refusal(t, shared.send(t, http.MethodPatch, "/api/v1/issues/"+seed.m2, map[string]any{"priority": nil}))
			rig.requireResolvedStatesKept(t, seed.issueNames, before)
		})
	},
}

// resolvedP0Filters are the operations whose request body takes a priority or a parent that is no
// issue's, each with its reason. The list is closed: an operation found outside it and outside
// resolvedP0Requests fails TestEveryOperationThatTakesAPriorityOrAParentRunsTheP0Rows by name.
var resolvedP0Filters = map[string]string{
	"POST /api/v1/me/inbox/dismiss": "its priority is a filter of the Inbox rows the dismissal reaches, and it takes no parent",
	"POST /api/v1/me/inbox/read":    "its priority is a filter of the Inbox rows the mark-all reaches, and it takes no parent",
}

// priorityOrParentOperations answers each operation of document, as "METHOD path", whose request
// body, under any of its media types, has a property named priority or parent.
func priorityOrParentOperations(t *testing.T, name string, document map[string]any) []string {
	t.Helper()
	components, _ := document["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	paths, _ := document["paths"].(map[string]any)
	var found []string
	for path, item := range paths {
		operations, _ := item.(map[string]any)
		for _, method := range openAPIMethods {
			operation, _ := operations[method].(map[string]any)
			body, _ := operation["requestBody"].(map[string]any)
			if _, referenced := body["$ref"]; referenced {
				t.Fatalf("%s: %s %s's request body is a reference, which this test does not follow", name, strings.ToUpper(method), path)
			}
			content, _ := body["content"].(map[string]any)
			for _, media := range content {
				schema, _ := media.(map[string]any)
				if takesAnyProperty(t, name, schema["schema"], schemas, map[string]bool{}, "priority", "parent") {
					found = append(found, strings.ToUpper(method)+" "+path)
					break
				}
			}
		}
	}
	return found
}

// takesAnyProperty is takesPriority's walk for any of names: whether node, a schema of document
// name, has a property named one of them, its own, a nested object's, an array's items', or one a
// $ref, allOf, anyOf or oneOf reaches. seen holds the component schemas already walked, so a schema
// that refers to itself ends.
func takesAnyProperty(t *testing.T, name string, node any, schemas map[string]any, seen map[string]bool, names ...string) bool {
	t.Helper()
	schema, ok := node.(map[string]any)
	if !ok {
		return false
	}
	if reference, referenced := schema["$ref"].(string); referenced {
		component, local := strings.CutPrefix(reference, "#/components/schemas/")
		if !local {
			t.Fatalf("%s holds the reference %s, which this test does not follow", name, reference)
		}
		if seen[component] {
			return false
		}
		seen[component] = true
		target, declared := schemas[component]
		if !declared {
			t.Fatalf("%s holds the reference %s, which names no schema it declares", name, reference)
		}
		if takesAnyProperty(t, name, target, schemas, seen, names...) {
			return true
		}
	}
	for _, combinator := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[combinator].([]any)
		for _, branch := range branches {
			if takesAnyProperty(t, name, branch, schemas, seen, names...) {
				return true
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, property := range names {
		if _, named := properties[property]; named {
			return true
		}
	}
	for _, property := range properties {
		if takesAnyProperty(t, name, property, schemas, seen, names...) {
			return true
		}
	}
	return takesAnyProperty(t, name, schema["items"], schemas, seen, names...)
}

// TestEveryOperationThatTakesAPriorityOrAParentRunsTheP0Rows is the inventory of J2's last clause
// (R10), beside INFRA-5637's TestEveryOperationThatTakesAnIssuePriorityNeedsP0ForZero. It reads
// every committed dispatch-openapi*.json and takes each operation whose request body has a property
// named priority or parent. Each such operation has its requests in resolvedP0Requests or its
// reason in the closed list resolvedP0Filters, and the test fails by name for one that has neither,
// so a v2 issue write of cybermonklabs/legion#4274 fails it in its own CI until it runs the rows. It
// fails too when it finds fewer than the two v1 writes. Under the shared token, on a fresh seed
// each: the create with no priority under A by a session that is not A's architect, the PATCH that
// moves an issue with no priority under A, and the PATCH that clears M2's own priority are each
// refused 403 naming p0 and write nothing.
func TestEveryOperationThatTakesAPriorityOrAParentRunsTheP0Rows(t *testing.T) {
	t.Parallel()
	found := map[string][]string{}
	for name, document := range committedOpenAPIDocuments(t) {
		for _, operation := range priorityOrParentOperations(t, name, document) {
			found[operation] = append(found[operation], name)
		}
	}
	for _, write := range []string{"POST /api/v1/issues", "PATCH /api/v1/issues/{key}"} {
		if _, ok := found[write]; !ok {
			t.Fatalf("the committed documents take a priority or a parent in the request bodies of %v alone, which leaves out %s: the inventory found fewer than the two v1 writes", slices.Sorted(maps.Keys(found)), write)
		}
	}
	var writes []string
	for _, operation := range slices.Sorted(maps.Keys(found)) {
		if _, requested := resolvedP0Requests[operation]; requested {
			writes = append(writes, operation)
			continue
		}
		if reason, filter := resolvedP0Filters[operation]; filter {
			t.Logf("%s takes a priority or a parent and is no write of an issue's: %s", operation, reason)
			continue
		}
		t.Errorf("%s (%s) takes a priority or a parent in its request body, and this test sends it no move, clear or create that makes work P0: add its requests to resolvedP0Requests, or, where neither is an issue's, its reason to resolvedP0Filters",
			operation, strings.Join(found[operation], ", "))
	}

	rig := newResolvedRig(t, testServerOptions{})
	for _, operation := range writes {
		t.Run(operation, func(t *testing.T) { resolvedP0Requests[operation](t, rig) })
	}
}
