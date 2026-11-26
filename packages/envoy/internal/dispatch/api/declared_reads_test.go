package api

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// This file is c-declared-reads of dispatch://INFRA-1098 (dispatch://INFRA-1098/spec#b-r-no-regression):
// no read gets a caller-chosen query, and nothing a caller cannot see becomes reachable. The
// declaration half reads the route table; the behaviour half drives each read, because a
// declaration is not a guard; and the same test compares every read that stood before this tree.

// readsBeforeThisTree are the GET routes the route table held before this tree, read off
// testdata/routes.txt at the commit of main this line merged last (bad897aa9dac, 2026-11-21):
// what main serves without any of this tree's children. A read this tree adds is a GET row of
// the table that is not here.
var readsBeforeThisTree = []string{
	"GET /api/v1",
	"GET /api/v1/agents",
	"GET /api/v1/agents/{session_id}/answers",
	"GET /api/v1/agents/{session_id}/artifacts/{slug}",
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}",
	"GET /api/v1/agents/{session_id}/messages",
	"GET /api/v1/artifacts/{id}",
	"GET /api/v1/artifacts/{id}/asks",
	"GET /api/v1/artifacts/{id}/blocks",
	"GET /api/v1/artifacts/{id}/blocks/{block_id}",
	"GET /api/v1/artifacts/{id}/comments",
	"GET /api/v1/artifacts/{id}/events",
	"GET /api/v1/artifacts/{id}/references",
	"GET /api/v1/artifacts/{id}/reviews",
	"GET /api/v1/artifacts/{id}/subscribers",
	"GET /api/v1/artifacts/{id}/text",
	"GET /api/v1/artifacts/{id}/versions/{number}",
	"GET /api/v1/asks/open",
	"GET /api/v1/asks/{id}",
	"GET /api/v1/asks/{id}/followers",
	"GET /api/v1/block-links",
	"GET /api/v1/broadcasts",
	"GET /api/v1/broadcasts/{id}",
	"GET /api/v1/ci-reconcile/passes",
	"GET /api/v1/comments/{id}",
	"GET /api/v1/comments/{id}/blocks",
	"GET /api/v1/credential-grants",
	"GET /api/v1/credential-requests",
	"GET /api/v1/credentials/capabilities",
	"GET /api/v1/dashboard/builds",
	"GET /api/v1/delivery/measures",
	"GET /api/v1/delivery/pipeline",
	"GET /api/v1/delivery/runs/{id}",
	"GET /api/v1/delivery/timeline",
	"GET /api/v1/events/log",
	"GET /api/v1/github/app-installation/{owner}/{repo}",
	"GET /api/v1/going/capability",
	"GET /api/v1/going/catalogue",
	"GET /api/v1/going/requests/{id}/stream",
	"GET /api/v1/handoffs/{id}",
	"GET /api/v1/inbox",
	"GET /api/v1/issues",
	"GET /api/v1/issues/resolve",
	"GET /api/v1/issues/{key}",
	"GET /api/v1/issues/{key}/artifacts",
	"GET /api/v1/issues/{key}/artifacts/{slug}",
	"GET /api/v1/issues/{key}/artifacts/{slug}/blocks",
	"GET /api/v1/issues/{key}/artifacts/{slug}/text",
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}",
	"GET /api/v1/issues/{key}/asks",
	"GET /api/v1/issues/{key}/collaborators",
	"GET /api/v1/issues/{key}/comments",
	"GET /api/v1/issues/{key}/coverage",
	"GET /api/v1/issues/{key}/decisions",
	"GET /api/v1/issues/{key}/events",
	"GET /api/v1/issues/{key}/handoffs",
	"GET /api/v1/issues/{key}/handoffs/{phase}/latest",
	"GET /api/v1/issues/{key}/messages",
	"GET /api/v1/issues/{key}/messages/{id}",
	"GET /api/v1/issues/{key}/references",
	"GET /api/v1/issues/{key}/results",
	"GET /api/v1/issues/{key}/role-holders",
	"GET /api/v1/issues/{key}/steps",
	"GET /api/v1/issues/{key}/subscribers",
	"GET /api/v1/issues/{key}/summary",
	"GET /api/v1/limits",
	"GET /api/v1/machine-logins",
	"GET /api/v1/me/agent-tokens",
	"GET /api/v1/me/agents/state",
	"GET /api/v1/me/answers",
	"GET /api/v1/me/documents/{id}/read",
	"GET /api/v1/me/replies",
	"GET /api/v1/me/state",
	"GET /api/v1/messages/{id}",
	"GET /api/v1/messages/{id}/blocks",
	"GET /api/v1/near-copy-checks",
	"GET /api/v1/near-copy-checks/{id}",
	"GET /api/v1/priorities",
	"GET /api/v1/priorities/{id}/history",
	"GET /api/v1/projects",
	"GET /api/v1/projects/{key}/architecture",
	"GET /api/v1/projects/{key}/architecture-source",
	"GET /api/v1/projects/{key}/architecture-sources",
	"GET /api/v1/projects/{key}/artifacts",
	"GET /api/v1/projects/{key}/artifacts/{slug}",
	"GET /api/v1/projects/{key}/artifacts/{slug}/blocks",
	"GET /api/v1/projects/{key}/artifacts/{slug}/summary",
	"GET /api/v1/projects/{key}/artifacts/{slug}/text",
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}",
	"GET /api/v1/projects/{key}/legion/board",
	"GET /api/v1/projects/{key}/legion/daemon",
	"GET /api/v1/projects/{key}/plans",
	"GET /api/v1/projects/{key}/related",
	"GET /api/v1/reference-targets",
	"GET /api/v1/references",
	"GET /api/v1/references/questions",
	"GET /api/v1/related-checks",
	"GET /api/v1/schema/blocks",
	"GET /api/v1/search",
	"GET /api/v1/search/coverage",
	"GET /api/v1/search/results",
	"GET /api/v1/settings/architecture-sources",
	"GET /api/v1/settings/delivery",
	"GET /api/v1/settings/new-issue",
	"GET /api/v1/settings/project-default-assignees",
	"GET /api/v1/settings/repo-projects",
	"GET /api/v1/signins",
	"GET /api/v1/users",
	"GET /api/v1/whoami",
}

// readsThisTreeDeletes are the reads this issue's own pull request deletes by name, each read as
// absent rather than as changed: the three inbox reads, which go once dispatch://INFRA-1152 and
// dispatch://INFRA-4948 have moved their callers to the one inbox read, and the read
// dispatch://INFRA-1253's tree read replaces once its four filters answer, the decisions read.
var readsThisTreeDeletes = map[string]string{
	"GET /api/v1/inbox":                        "an inbox read, replaced by the one inbox read (dispatch://INFRA-1152)",
	"GET /api/v1/me/replies":                   "an inbox read, replaced by the one inbox read (dispatch://INFRA-1152)",
	"GET /api/v1/agents/{session_id}/messages": "an inbox read, replaced by the one inbox read (dispatch://INFRA-1152)",
	"GET /api/v1/issues/{key}/decisions":       "the decisions read, replaced by the tree read's decisions view (dispatch://INFRA-1253)",
}

// rollFields are the fields of an answer that may differ from its declared shape while their own
// child's rolling deploy is in flight, each with that child. The list started from four -
// decisions_artifact_id, the decisions read's document, verification_artifact_id and
// plan_artifact_id - and holds none at this issue's own head, since every child's roll is over
// once this issue merges: a field still in it here fails.
var rollFields = map[string]string{}

// rolledOutFields are the four fields the roll list started from, each with the read it stood
// in. With the list empty at this head, each is absent from its read's answer, as the field of
// a read this tree deletes is.
var rolledOutFields = []struct{ route, field string }{
	{"GET /api/v1/issues/{key}", "decisions_artifact_id"},
	{"GET /api/v1/issues/{key}/decisions", "document"},
	{"GET /api/v1/issues/{key}", "verification_artifact_id"},
	{"GET /api/v1/issues/{key}", "plan_artifact_id"},
}

// callerChosenParameter matches a query parameter a caller would choose what the store is asked
// with: a node kind, a filter, a traversal, an edge or a relation to follow, a depth. A route that
// declares one takes from its caller what this tree's reads exist not to take.
var callerChosenParameter = regexp.MustCompile(`(?i)^(?:kinds?|node_kinds?|supertags?|filters?|where|query|cypher|sql|traversal|traverse|follow|edges?|relations?|tuples?|via|depth|hops|direction)$`)

// generalQueryPattern matches the path of a general query endpoint: one that answers whatever a
// caller asks of the graph rather than one read of its own.
var generalQueryPattern = regexp.MustCompile(`(?i)/(?:graph|query|queries|traverse|traversal|cypher|sql)(?:/|$)`)

// undeclaredGraphWords are the parameters the behaviour half sends each read this tree adds: a
// traversal, a kind and a depth, each with a value a walk of the graph would take.
var undeclaredGraphWords = map[string]string{"traversal": "part_of", "kind": "person", "depth": "9"}

// declaredReadsToken is the broker's answer for a live token bound to jkt and held to project
// alone: the credential the behaviour half reads one project's rows with.
func declaredReadsToken(jkt, project string) ciphertoken.Introspection {
	answer := liveNamedToken(jkt)
	answer.Projects = []string{project}
	return answer
}

// TestGraphReadsAreDeclaredAndScoped is c-declared-reads. The declaration half reads the route
// table: there is no general query endpoint and no caller-chosen traversal, every read this tree
// adds is its own row with its own authentication mode and its own scope row, and no route
// declares a kind, a filter or a traversal taken from its caller. The behaviour half drives each
// read this tree adds with an undeclared traversal, kind and depth, which it must ignore or
// refuse; with a credential that has no standing on the row, which it must refuse; and with an
// identifier of another project, which it must refuse. It then reads every read that stood
// before this tree: each the tree deletes is absent, named by its path; each other answers the
// shape the contracts declare, field for field, an artifact's answer carrying its role; each
// answers nothing of this project to another project's credential; the roll list is empty, and
// the issue read answers no blocked_by.
func TestGraphReadsAreDeclaredAndScoped(t *testing.T) {
	table := (&server{deps: Deps{}}).routes()
	mounted := map[string]apiRoute{}
	for _, route := range table {
		mounted[route.Method+" "+route.Pattern] = route
	}
	before := map[string]bool{}
	for _, name := range readsBeforeThisTree {
		before[name] = true
	}
	var added []apiRoute
	for _, route := range table {
		if route.Method == http.MethodGet && !before[route.Method+" "+route.Pattern] {
			added = append(added, route)
		}
	}
	t.Logf("the route table holds %d rows, %d of them reads; %d reads stood before this tree, and %d are this tree's: %v",
		len(table), countReads(table), len(readsBeforeThisTree), len(added), routeNames(added))

	t.Run("the route table holds no general query and no route takes a kind, a filter or a traversal from its caller", func(t *testing.T) {
		requireNoCallerChosenQuery(t, table, before)
	})
	t.Run("every read this tree adds is its own row with its own authentication mode and its own scope row", func(t *testing.T) {
		requireEveryAddedReadDeclared(t, added)
	})

	rig := newBrokerTokenRig(t)
	owned := seedTwoProjects(t, rig.handler, rig.database)
	caller := newBrokerCaller(t)
	const ownToken, otherToken = "a-token-held-to-this-project", "a-token-held-to-the-other-project"
	rig.broker.Says(ownToken, declaredReadsToken(caller.jkt, "TEST"))
	rig.broker.Says(otherToken, declaredReadsToken(caller.jkt, owned.otherKey))
	reads := &declaredReadRig{rig: rig, owned: owned, caller: caller, ownToken: ownToken, otherToken: otherToken}

	t.Run("each read this tree adds ignores or refuses an undeclared traversal, kind and depth", func(t *testing.T) {
		reads.requireUndeclaredWordsIgnored(t, added)
	})
	t.Run("each read this tree adds refuses a credential with no standing on its row", func(t *testing.T) {
		reads.requireNoStandingRefused(t, added)
	})
	t.Run("each read this tree adds refuses an identifier of another project", func(t *testing.T) {
		reads.requireOtherProjectsIdentifierRefused(t, added)
	})
	t.Run("each read this tree deletes is absent, named by its path", func(t *testing.T) {
		requireDeletedReadsAbsent(t, mounted)
	})
	t.Run("every read that stood before this tree answers the shape the contracts declare, field for field", func(t *testing.T) {
		reads.requireDeclaredShapes(t, mounted)
	})
	t.Run("an artifact's answer carries its role, and a query node's role is the name of its query", func(t *testing.T) {
		reads.requireArtifactRoles(t)
	})
	t.Run("another project's credential reads nothing of this project from a read that stood before this tree", func(t *testing.T) {
		reads.requireNothingAcrossProjects(t, mounted)
	})
	t.Run("the roll list is empty at this head, and the issue read answers no blocked_by", func(t *testing.T) {
		reads.requireRollListEmpty(t)
	})
}

func countReads(table []apiRoute) int {
	reads := 0
	for _, route := range table {
		if route.Method == http.MethodGet {
			reads++
		}
	}
	return reads
}

// requireNoCallerChosenQuery reads every row of the table: no path is a general query
// endpoint's, and no read this tree adds declares a query parameter a caller chooses what the
// store is asked with (callerChosenParameter), each read printed with what it takes. A read that
// stood before this tree and declares such a word - kind on GET /api/v1/me/replies and on the two
// reference reads - declares it as its own parameter, which it answers by its own rule, as
// c-no-reach reads it (no_reach_test.go, threeBadRequests): it is printed with that reason, and
// the reads this tree adds are the ones the clause holds.
func requireNoCallerChosenQuery(t *testing.T, table []apiRoute, before map[string]bool) {
	read, added := 0, 0
	for _, route := range table {
		name := route.Method + " " + route.Pattern
		if generalQueryPattern.MatchString(route.Pattern) {
			t.Errorf("%s is a general query endpoint: it answers whatever a caller asks of the graph", name)
		}
		if route.Method != http.MethodGet {
			continue
		}
		read++
		declared := queryTagsOf(routeShapes[name].Params)
		slices.Sort(declared)
		var chosen []string
		for _, parameter := range declared {
			if callerChosenParameter.MatchString(parameter) {
				chosen = append(chosen, parameter)
			}
		}
		t.Logf("%s takes the query %v", name, declared)
		switch {
		case len(chosen) == 0:
		case before[name]:
			t.Logf("%s stood before this tree and declares %v as its own parameter, which it answers by its own rule", name, chosen)
		default:
			t.Errorf("%s declares %v, a kind, a filter or a traversal taken from its caller", name, chosen)
		}
		if !before[name] {
			added++
		}
	}
	if read == 0 || added == 0 {
		t.Fatalf("the table holds %d reads, %d of them this tree's, so nothing was read", read, added)
	}
	t.Logf("read %d reads of the table, %d of them this tree's", read, added)
}

// requireEveryAddedReadDeclared holds each read this tree adds to its declaration: a row of its
// own with a description no other row shares, an authentication mode that is not public, and a
// scope row, the row of routeScopes or readClasses that says who may read it.
func requireEveryAddedReadDeclared(t *testing.T, added []apiRoute) {
	if len(added) == 0 {
		t.Fatal("the table holds no read this tree adds, so nothing was read")
	}
	table := (&server{deps: Deps{}}).routes()
	for _, route := range added {
		name := route.Method + " " + route.Pattern
		_, scoped := routeScopes[name]
		_, listed := readClasses[name]
		t.Logf("%s: auth %s, scopes %v, read list %v", name, route.Auth, []string(route.OAuthScopes), listed)
		if strings.TrimSpace(route.Description) == "" {
			t.Errorf("%s carries no description", name)
		}
		for _, other := range table {
			if other.Method+" "+other.Pattern != name && other.Description == route.Description {
				t.Errorf("%s shares its description with %s %s, so it is no read of its own", name, other.Method, other.Pattern)
			}
		}
		if route.Auth == authPublic || route.Auth == "" {
			t.Errorf("%s is %q, which is no authentication mode of its own", name, route.Auth)
		}
		if !scoped && !listed {
			t.Errorf("%s has no scope row: neither routeScopes nor readClasses names it, so nothing in the table says whose rows it may answer", name)
		}
	}
}

// declaredReadRig is the server the behaviour half drives, the two projects seedTwoProjects
// wrote, and the broker tokens held to each.
type declaredReadRig struct {
	rig                  *brokerTokenRig
	owned                crossProject
	caller               *brokerCaller
	ownToken, otherToken string
}

// target is a read's path at this project's objects, with the query a read that requires one
// is sent (routeProbeQueries).
func (r *declaredReadRig) target(name string) string {
	_, pattern, _ := strings.Cut(name, " ")
	target := seededProbePath(pattern, r.owned)
	if query, held := routeProbeQueries(r.owned)[name]; held {
		target += "?" + query
	}
	return target
}

// otherTarget is a read's path at the other project's objects: its issue for {key} under
// /issues, its key for {key} under /projects, and its issue's spec for a query naming this
// project's spec or issue.
func (r *declaredReadRig) otherTarget(name string) string {
	target := r.target(name)
	target = strings.ReplaceAll(target, "/issues/"+r.owned.issue, "/issues/"+r.owned.otherIssue)
	target = strings.ReplaceAll(target, url.QueryEscape("dispatch://"+r.owned.issue), url.QueryEscape("dispatch://"+r.owned.otherIssue))
	target = strings.ReplaceAll(target, "/projects/TEST", "/projects/"+r.owned.otherKey)
	return target
}

// presenting sends one read under a broker token.
func (r *declaredReadRig) presenting(t *testing.T, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	return r.rig.serve(r.caller.presenting(t, http.MethodGet, target, token, nil))
}

// withQuery adds query to target.
func withQuery(target string, query url.Values) string {
	if strings.Contains(target, "?") {
		return target + "&" + query.Encode()
	}
	return target + "?" + query.Encode()
}

// arrayCounts is how many rows each array of an answer holds, by its path: a read that honoured
// a kind or a traversal answers a different count somewhere in it, where the keys of its top
// object stay what they were.
func arrayCounts(body []byte) map[string]int {
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil
	}
	counts := map[string]int{}
	var walk func(path string, value any)
	walk = func(path string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, inner := range typed {
				walk(path+"."+key, inner)
			}
		case []any:
			counts[path] += len(typed)
			for _, row := range typed {
				walk(path+"[]", row)
			}
		}
	}
	walk("", answer)
	return counts
}

// requireUndeclaredWordsIgnored sends each read this tree adds its plain request and the same
// request carrying each of undeclaredGraphWords it does not declare, and reads each one ignored,
// the same status, keys and rows, or refused, a 4xx.
func (r *declaredReadRig) requireUndeclaredWordsIgnored(t *testing.T, added []apiRoute) {
	if len(added) == 0 {
		t.Fatal("the table holds no read this tree adds, so nothing was driven")
	}
	for _, route := range added {
		name := route.Method + " " + route.Pattern
		declared := queryTagsOf(routeShapes[name].Params)
		query := url.Values{}
		for word, value := range undeclaredGraphWords {
			if !slices.Contains(declared, word) {
				query.Set(word, value)
			}
		}
		target := r.target(name)
		plain := dispatchRequest(t, r.rig.handler, http.MethodGet, target, nil, "alice")
		walkedTarget := withQuery(target, query)
		walked := dispatchRequest(t, r.rig.handler, http.MethodGet, walkedTarget, nil, "alice")
		t.Logf("%s at %s answers %d %s; with %s it answers %d %s", name, target, plain.Code, answerShape(plain.Body.Bytes()),
			query.Encode(), walked.Code, answerShape(walked.Body.Bytes()))
		if plain.Code >= 400 {
			t.Errorf("%s answers %d at %s on its own, so its undeclared parameters were sent to no row: %s", name, plain.Code, target, firstLine(plain.Body.String()))
			continue
		}
		if walked.Code >= 400 && walked.Code < 500 {
			t.Logf("%s refuses the undeclared parameters: %d %s", name, walked.Code, firstLine(walked.Body.String()))
			continue
		}
		if walked.Code != plain.Code || answerShape(plain.Body.Bytes()) != answerShape(walked.Body.Bytes()) ||
			!maps.Equal(arrayCounts(plain.Body.Bytes()), arrayCounts(walked.Body.Bytes())) {
			t.Errorf("%s honours an undeclared parameter: at %s it answers %d %s with rows %v, and with %s it answers %d %s with rows %v",
				name, target, plain.Code, answerShape(plain.Body.Bytes()), arrayCounts(plain.Body.Bytes()),
				query.Encode(), walked.Code, answerShape(walked.Body.Bytes()), arrayCounts(walked.Body.Bytes()))
		}
	}
}

// refusedRead reports whether a read's answer is a refusal: a 4xx, the 404 that tells a caller
// nothing among them.
func refusedRead(response *httptest.ResponseRecorder) bool {
	return response.Code >= 400 && response.Code < 500
}

// requireNoStandingRefused sends each read this tree adds, at this project's row, under a broker
// token held to the other project, which has no standing on that row, and reads a refusal.
func (r *declaredReadRig) requireNoStandingRefused(t *testing.T, added []apiRoute) {
	if len(added) == 0 {
		t.Fatal("the table holds no read this tree adds, so nothing was driven")
	}
	for _, route := range added {
		name := route.Method + " " + route.Pattern
		target := r.target(name)
		own := r.presenting(t, target, r.ownToken)
		answered := r.presenting(t, target, r.otherToken)
		t.Logf("%s at %s: a token held to TEST answers %d, one held to %s answers %d %s", name, target, own.Code, r.owned.otherKey, answered.Code, firstLine(answered.Body.String()))
		if !refusedRead(answered) {
			t.Errorf("%s answers %d at %s to a token held to %s, which has no standing on this project's row: %s",
				name, answered.Code, target, r.owned.otherKey, firstLine(answered.Body.String()))
		}
	}
}

// requireOtherProjectsIdentifierRefused sends each read this tree adds an identifier of the other
// project under a broker token held to this one, and reads a refusal. A read whose path and query
// name no object of a project - the catalogue of lenses with no node, a person's own page - takes
// no identifier of a project, which is printed.
func (r *declaredReadRig) requireOtherProjectsIdentifierRefused(t *testing.T, added []apiRoute) {
	driven := 0
	for _, route := range added {
		name := route.Method + " " + route.Pattern
		own, other := r.target(name), r.otherTarget(name)
		if own == other {
			t.Logf("%s at %s names no object of a project, so no identifier of another one can be sent it", name, own)
			continue
		}
		driven++
		answered := r.presenting(t, other, r.ownToken)
		t.Logf("%s at %s, under a token held to TEST: %d %s", name, other, answered.Code, firstLine(answered.Body.String()))
		if !refusedRead(answered) {
			t.Errorf("%s answers %d at %s, an identifier of %s, to a token held to TEST: %s",
				name, answered.Code, other, r.owned.otherKey, firstLine(answered.Body.String()))
		}
	}
	if driven == 0 {
		t.Fatal("no read this tree adds names an object of a project, so nothing was driven")
	}
	t.Logf("%d reads this tree adds were sent an identifier of another project", driven)
}

// requireDeletedReadsAbsent reads each read this tree deletes as absent from the table, named by
// its path: one that still stands fails, whatever it answers.
func requireDeletedReadsAbsent(t *testing.T, mounted map[string]apiRoute) {
	for _, name := range slices.Sorted(maps.Keys(readsThisTreeDeletes)) {
		if _, stands := mounted[name]; stands {
			t.Errorf("%s still stands in the route table: %s, which this tree deletes", name, readsThisTreeDeletes[name])
			continue
		}
		t.Logf("%s is absent: %s", name, readsThisTreeDeletes[name])
	}
}

// requireDeclaredShapes reads every read that stood before this tree and that it does not delete,
// at this project's objects, and holds each answer to the shape the committed document declares
// for its 200 and the contracts module declares for each component, field for field
// (answerFieldPaths). A read that answers no JSON success is no comparison and fails, but the
// reads no seed of seedTwoProjects answers (routesNoSeedAnswers), each printed with what it would
// need, and the server-sent stream, which answers no JSON.
func (r *declaredReadRig) requireDeclaredShapes(t *testing.T, mounted map[string]apiRoute) {
	root := repositoryRoot(t)
	document := committedDocument(t, root)
	declared := schemasOfGeneratedModule(t, readRepositoryFile(t, root, contractsGenerated))
	compared := 0
	for _, name := range readsBeforeThisTree {
		if _, deleted := readsThisTreeDeletes[name]; deleted {
			continue
		}
		if _, stands := mounted[name]; !stands {
			t.Errorf("%s stood before this tree, which does not delete it, and the table no longer holds it", name)
			continue
		}
		if reason, exempt := routesNoSeedAnswers[name]; exempt {
			t.Logf("%s is not compared: no seed of this test answers it a row (%s)", name, reason)
			continue
		}
		t.Run(name, func(t *testing.T) {
			target := r.target(name)
			answered := dispatchRequest(t, r.rig.handler, http.MethodGet, target, nil, "alice")
			if answered.Code != http.StatusOK || !strings.HasPrefix(answered.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("%s answers %d %s at %s, so its shape was compared with nothing: %s",
					name, answered.Code, answered.Header().Get("Content-Type"), target, firstLine(answered.Body.String()))
			}
			compared++
			paths := answerFieldPaths(t, document, declared, name, answered.Body.Bytes())
			t.Logf("%s at %s answers %d fields", name, target, len(paths))
		})
	}
	if compared == 0 {
		t.Fatal("no read was compared with its declared shape")
	}
	t.Logf("%d reads that stood before this tree were compared with the shapes the contracts declare", compared)
}

// requireArtifactRoles reads each read that answers this project's artifacts and holds each
// artifact object of a document that plays a role, or of a query node, to its role: the
// Verification document the test uploads answers role verification, and each query node of the
// issue answers the name of its query (R20) - but the spec, the issue's primary document, which
// says what it is by primary true and answers no role.
func (r *declaredReadRig) requireArtifactRoles(t *testing.T) {
	ctx := context.Background()
	verification := uploadDocument(t, r.rig.handler, "/api/v1/issues/"+r.owned.issue, "verification.md", "# Verification\n\nIt is checked.\n")
	want := map[string]string{verification.Artifact.ID: "verification"}
	rows, err := r.rig.database.Pool.Query(ctx, `
		select a.id::text, n.attributes->>'`+graphstore.AttributeQuery+`'
		from artifacts a join graph_nodes n on n.kind = '`+graphstore.KindArtifact+`' and n.id = a.ref_key
		where a.issue_key = $1 and n.supertag = '`+graphstore.SupertagQuery+`'`, r.owned.issue)
	if err != nil {
		t.Fatalf("read the issue's query nodes: %v", err)
	}
	for rows.Next() {
		var id, query string
		if err := rows.Scan(&id, &query); err != nil {
			t.Fatalf("read a query node: %v", err)
		}
		want[id] = query
		if query == specQuery {
			want[id] = ""
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the issue's query nodes: %v", err)
	}
	t.Logf("the issue %s holds %d documents whose role the answer must carry: %v", r.owned.issue, len(want), want)
	read := 0
	for id, role := range want {
		for _, target := range []string{"/api/v1/artifacts/" + id, "/api/v1/issues/" + r.owned.issue + "/artifacts", "/api/v1/issues/" + r.owned.issue} {
			answered := dispatchRequest(t, r.rig.handler, http.MethodGet, target, nil, "alice")
			if answered.Code != http.StatusOK {
				t.Errorf("GET %s answers %d: %s", target, answered.Code, firstLine(answered.Body.String()))
				continue
			}
			found := artifactObjects(answered.Body.Bytes(), id)
			if len(found) == 0 {
				t.Logf("GET %s answers no artifact object of %s", target, id)
				continue
			}
			for _, object := range found {
				read++
				got, carried := object["role"]
				primary := object["primary"]
				if role == "" {
					t.Logf("GET %s answers the spec %s with primary %v and role %v (carried %v), want primary true and no role", target, id, primary, got, carried)
					if carried || primary != true {
						t.Errorf("GET %s answers the spec %s with primary %v and role %v (carried %v), want primary true and no role", target, id, primary, got, carried)
					}
					continue
				}
				t.Logf("GET %s answers %s with role %v (carried %v), want %q", target, id, got, carried, role)
				if !carried || got != role {
					t.Errorf("GET %s answers the artifact %s with role %v (carried %v), want %q", target, id, got, carried, role)
				}
			}
		}
	}
	if read == 0 {
		t.Fatal("no answer held an artifact object of a document that plays a role, so no role was read")
	}
}

// specQuery is the query a spec's node is the node of: the issue's primary document, which answers
// primary true and no role.
const specQuery = "spec"

// artifactObjects is every object of an answer, at any depth, whose id is id and which carries a
// slug: an artifact as a read answers one.
func artifactObjects(body []byte, id string) []map[string]any {
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil
	}
	var found []map[string]any
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if typed["id"] == id {
				if _, slugged := typed["slug"]; slugged {
					found = append(found, typed)
				}
			}
			for _, inner := range typed {
				walk(inner)
			}
		case []any:
			for _, row := range typed {
				walk(row)
			}
		}
	}
	walk(answer)
	return found
}

// thisProjectsObjects are this project's objects an answer must not name to another project's
// credential, each id with what it is.
func (r *declaredReadRig) thisProjectsObjects() map[string]string {
	return map[string]string{
		r.owned.issue: "issue " + r.owned.issue, r.owned.comment: "comment " + r.owned.comment, r.owned.ask: "ask " + r.owned.ask,
		r.owned.message: "message " + r.owned.message, r.owned.document: "project document " + r.owned.document,
		r.owned.artifact: "spec " + r.owned.artifact,
	}
}

// namedObjects is every place of an answer that names one of objects: the path of the field, as
// answerFieldPaths names one, and the object it names.
func namedObjects(body []byte, objects map[string]string) []string {
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil
	}
	var named []string
	seen := map[string]bool{}
	var walk func(path string, value any)
	walk = func(path string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			for _, key := range slices.Sorted(maps.Keys(typed)) {
				next := key
				if path != "" {
					next = path + "." + key
				}
				walk(next, typed[key])
			}
		case []any:
			for _, row := range typed {
				walk(path+"[]", row)
			}
		case string:
			for id, what := range objects {
				if id != "" && strings.Contains(typed, id) && !seen[path+" "+what] {
					seen[path+" "+what] = true
					named = append(named, cmp.Or(path, "the answer")+" = "+what)
				}
			}
		}
	}
	walk("", answer)
	slices.Sort(named)
	return named
}

// requireNothingAcrossProjects drives every read that stood before this tree, at this project's
// objects, under a broker token held to the other project, and reads nothing of this project in
// the answer: a refusal, or an answer that names none of its objects.
func (r *declaredReadRig) requireNothingAcrossProjects(t *testing.T, mounted map[string]apiRoute) {
	driven, leaked := 0, 0
	for _, name := range readsBeforeThisTree {
		if _, stands := mounted[name]; !stands {
			continue
		}
		if name == "GET /api/v1/going/requests/{id}/stream" {
			t.Logf("%s is a server-sent stream of a run no seed holds, so no answer of it is read", name)
			continue
		}
		driven++
		target := r.target(name)
		answered := r.presenting(t, target, r.otherToken)
		if refusedRead(answered) {
			t.Logf("%s at %s refuses a token held to %s: %d %s", name, target, r.owned.otherKey, answered.Code, firstLine(answered.Body.String()))
			continue
		}
		named := namedObjects(answered.Body.Bytes(), r.thisProjectsObjects())
		if len(named) == 0 {
			t.Logf("%s at %s answers a token held to %s %d, naming nothing of TEST", name, target, r.owned.otherKey, answered.Code)
			continue
		}
		leaked++
		t.Errorf("%s at %s answers a token held to %s %d, naming TEST's objects:\n  %s", name, target, r.owned.otherKey, answered.Code, strings.Join(named, "\n  "))
	}
	if driven == 0 {
		t.Fatal("no read was driven under the other project's credential")
	}
	t.Logf("%d reads were driven under a token held to %s; %d named something of TEST", driven, r.owned.otherKey, leaked)
}

// requireRollListEmpty reads the roll list, which must be empty at this head, and each field it
// started from, which must be absent from its read's answer and from the shape the document
// declares for it; and the issue read's blocked_by, which this issue's own pull request deletes.
func (r *declaredReadRig) requireRollListEmpty(t *testing.T) {
	for field, child := range rollFields {
		t.Errorf("the roll list still holds %s (%s) at this issue's own head, where every child's roll is over", field, child)
	}
	t.Logf("the roll list holds %d fields", len(rollFields))
	document := committedDocument(t, repositoryRoot(t))
	read := func(route string) map[string]any {
		t.Helper()
		_, pattern, _ := strings.Cut(route, " ")
		target := seededProbePath(pattern, r.owned)
		answered := dispatchRequest(t, r.rig.handler, http.MethodGet, target, nil, "alice")
		if answered.Code == http.StatusNotFound {
			return nil
		}
		if answered.Code != http.StatusOK {
			t.Errorf("%s at %s answers %d: %s", route, target, answered.Code, firstLine(answered.Body.String()))
			return nil
		}
		var object map[string]any
		if err := json.Unmarshal(answered.Body.Bytes(), &object); err != nil {
			t.Errorf("%s at %s answers no object: %v", route, target, err)
		}
		return object
	}
	checked := slices.Concat(rolledOutFields, []struct{ route, field string }{{"GET /api/v1/issues/{key}", "blocked_by"}})
	for _, held := range checked {
		answer := read(held.route)
		value, answered := answer[held.field]
		declares := declaredResponseField(t, document, held.route, held.field)
		t.Logf("%s: answered %v (%v), declared %v", held.route+" "+held.field, answered, value, declares)
		if answered {
			t.Errorf("%s answers %s (%v) at this head, a field the roll list no longer holds", held.route, held.field, value)
		}
		if declares {
			t.Errorf("the committed document still declares %s on %s", held.field, held.route)
		}
	}
}

// declaredResponseField reports whether the committed document declares field on the object
// route's 200 answers, through its reference.
func declaredResponseField(t *testing.T, document committedOpenAPIShapes, route, field string) bool {
	t.Helper()
	method, pattern, _ := strings.Cut(route, " ")
	operation, served := document.Paths[pattern][strings.ToLower(method)]
	if !served {
		return false
	}
	var declaration struct {
		Responses map[string]struct {
			Content map[string]struct {
				Schema openAPISchema `json:"schema"`
			} `json:"content"`
		} `json:"responses"`
	}
	if err := json.Unmarshal(operation, &declaration); err != nil {
		t.Fatalf("decode the document's %s: %v", route, err)
	}
	schema := declaration.Responses["200"].Content["application/json"].Schema
	for range 8 {
		name, ok := strings.CutPrefix(schema.Ref, componentSchemaRef)
		if !ok {
			break
		}
		schema = document.Components.Schemas[name]
	}
	_, declared := schema.Properties[field]
	return declared
}
