// issue_p0_scope_test.go holds Dispatch to G1 of dispatch://INFRA-5637: a write that makes an
// issue's own priority 0 (P0) needs a credential that holds the p0 scope (model.ScopeP0). A
// priority of 0 on a new issue, or on an issue whose own priority is another or none, from any
// other credential is refused 403 OAUTH_SCOPE_REQUIRED with the challenge naming p0, before every
// refusal that reads the issue, and the write writes nothing of its request. A write that keeps P0,
// lowers it, clears it or sets 1 to 3 needs no scope. The issue's own priority decides, never the
// one it inherits.
//
// The tests send their writes from the credentials of one inventory (p0Inventory): those of
// newLegionStatusRigOn with R26's setting off, and three personal tokens. They need
// DISPATCH_TEST_DATABASE_URL, as every test of this package that opens a store.
package api

//ci:reads packages/contracts/schemas/dispatch-openapi*.json TestEveryOperationThatTakesAnIssuePriorityNeedsP0ForZero globs and reads each committed document (the path built runtime-relative ../../../../..)

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// p0Verdict is what the server does with a credential's write that makes an issue's own priority
// 0.
type p0Verdict int

const (
	// p0Refused is a credential that does not hold p0: the write is refused 403
	// OAUTH_SCOPE_REQUIRED naming p0, and writes nothing.
	p0Refused p0Verdict = iota
	// p0Written is a credential that holds p0: the write is taken.
	p0Written
	// p0RefusedFirst is a credential that a check in front of the handler refuses for every
	// write, with a code of its own (legionRefusedFirst).
	p0RefusedFirst
)

// p0Credential is one credential of the inventory and what the server does with its write of P0.
type p0Credential struct {
	legionCredential
	p0 p0Verdict
}

// The three personal tokens the inventory adds to newLegionStatusRigOn's credentials.
const (
	p0TokenAlone         = "a personal token holding p0 alone"
	p0TokenPersonApprove = "a personal token holding person and approve"
	p0TokenAll           = "a personal token holding person, approve and p0"
)

// p0Inventory is every credential of rig, which newLegionStatusRigOn built, and three personal
// tokens of the rig's person: one holding p0 alone, one holding person and approve (the shape of
// a token a person minted before p0), and one holding person, approve and p0.
//
//   - Written: a browser sign-in, the token holding p0 alone, and the token holding person,
//     approve and p0. Of rig's credentials only the browser sign-in holds p0.
//   - Refused for want of p0: every other credential that reaches the handler.
//   - Refused in front of the handler, each with its own code: the read-only broker token (403
//     READ_ONLY_TOKEN) and the broker token naming a service and no person (403
//     SIGN_IN_REQUIRED), the checks newLegionStatusRigOn marks legionRefusedFirst.
func p0Inventory(t *testing.T, rig *legionStatusRig) []p0Credential {
	t.Helper()
	inventory := make([]p0Credential, 0, len(rig.credentials)+3)
	for _, credential := range rig.credentials {
		verdict := p0Refused
		switch {
		case credential.verdict == legionRefusedFirst:
			verdict = p0RefusedFirst
		case credential.name == legionBrowserSignIn:
			verdict = p0Written
		}
		inventory = append(inventory, p0Credential{legionCredential: credential, p0: verdict})
	}
	for _, token := range []struct {
		name    string
		scopes  []string
		verdict p0Verdict
	}{
		{p0TokenAlone, []string{model.ScopeP0}, p0Written},
		{p0TokenPersonApprove, []string{model.ScopePerson, model.ScopeApprove}, p0Refused},
		{p0TokenAll, []string{model.ScopePerson, model.ScopeApprove, model.ScopeP0}, p0Written},
	} {
		inventory = append(inventory, p0Credential{
			legionCredential: legionCredential{name: token.name, send: rig.bearer(personalTokenHolding(t, rig.database, token.scopes...), sessionActor())},
			p0:               token.verdict,
		})
	}
	return inventory
}

// personIssue makes, as the rig's person at a browser, an issue of brokerTestProject with a new
// title and fields beside it (a priority, a parent, labels, a spec), and answers it as the create
// answered it.
func (rig *legionStatusRig) personIssue(t *testing.T, fields map[string]any) model.Issue {
	t.Helper()
	body := map[string]any{"project": brokerTestProject, "title": rig.title(), "force": true}
	maps.Copy(body, fields)
	created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("make an issue %v as a person: status=%d body=%s", fields, created.Code, created.Body.String())
	}
	return decodeBody[model.Issue](t, created)
}

// p0IssueState is what a refused write of P0 leaves as it was on an issue: its own priority, its
// status, title and labels, the sequence every write of it moves, and its newest event.
type p0IssueState struct {
	priority  *int
	status    string
	title     string
	labels    []string
	lastSeq   int
	lastEvent int64
}

func readP0Issue(t *testing.T, database *store.Store, key string) p0IssueState {
	t.Helper()
	var state p0IssueState
	if err := database.Pool.QueryRow(context.Background(), `
		select i.priority, i.status, i.title, i.labels, i.last_seq,
		       coalesce((select max(e.id) from events e where e.issue_key = i.key), 0)
		from issues i where i.key = $1
	`, key).Scan(&state.priority, &state.status, &state.title, &state.labels, &state.lastSeq, &state.lastEvent); err != nil {
		t.Fatalf("read issue %s: %v", key, err)
	}
	return state
}

func (s p0IssueState) String() string {
	return fmt.Sprintf("priority %s, status %s, title %q, labels %q, last_seq %d, newest event %d",
		priorityName(s.priority), s.status, s.title, s.labels, s.lastSeq, s.lastEvent)
}

func (s p0IssueState) equal(other p0IssueState) bool {
	return priorityName(s.priority) == priorityName(other.priority) && s.status == other.status && s.title == other.title &&
		slices.Equal(s.labels, other.labels) && s.lastSeq == other.lastSeq && s.lastEvent == other.lastEvent
}

// priorityName is an own priority as a failure line names it.
func priorityName(priority *int) string {
	if priority == nil {
		return "none"
	}
	return fmt.Sprint(*priority)
}

// isP0 is whether an own priority is 0.
func isP0(priority *int) bool {
	return priority != nil && *priority == 0
}

// requireP0IssueKept fails unless key is as before: a refused write of P0 changes no priority,
// status, title or label, and appends no event.
func requireP0IssueKept(t *testing.T, database *store.Store, key string, before p0IssueState) {
	t.Helper()
	after := readP0Issue(t, database, key)
	if after.equal(before) {
		return
	}
	if isP0(after.priority) && !isP0(before.priority) {
		t.Errorf("%s after the refused write: %s; want it as it was: %s: a credential without p0 wrote priority 0", key, after, before)
		return
	}
	t.Errorf("%s after the refused write: %s; want it as it was: %s", key, after, before)
}

// requireP0Refusal fails unless response is the refusal of a write of P0 from a credential that
// does not hold p0: 403 OAUTH_SCOPE_REQUIRED, the challenge RFC 6750 gives a credential that
// lacks the p0 scope, and a message that names p0. The message's words are the server's.
func requireP0Refusal(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	requireRefused(t, response, http.StatusForbidden, codeScopeRequired)
	if got, want := response.Header().Get("WWW-Authenticate"), `Bearer error="insufficient_scope", scope="`+model.ScopeP0+`"`; got != want {
		t.Errorf("the refusal's challenge = %q, want %q", got, want)
	}
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if !strings.Contains(refusal.Error, model.ScopeP0) {
		t.Errorf("the refusal's message %q names no %s scope", refusal.Error, model.ScopeP0)
	}
}

// requireNoIssueTitled fails unless brokerTestProject holds no issue titled title: a refused
// create made none.
func requireNoIssueTitled(t *testing.T, database *store.Store, title string) {
	t.Helper()
	if made := countRows(t, database, `select count(*) from issues where project_key = $1 and title = $2`, brokerTestProject, title); made != 0 {
		t.Errorf("the refused create left %d issues titled %q in %s, want none: a credential without p0 made an issue at priority 0", made, title, brokerTestProject)
	}
}

// requireP0Answer fails unless response is what the server owes credential for its write of
// priority 0 on key, whose state before the write was before: a refusal that leaves key as it
// was, or, from a credential that holds p0, 200 and key at priority 0.
func (rig *legionStatusRig) requireP0Answer(t *testing.T, credential p0Credential, key string, before p0IssueState, response *httptest.ResponseRecorder) {
	t.Helper()
	switch credential.p0 {
	case p0RefusedFirst:
		requireRefused(t, response, http.StatusForbidden, credential.code)
		requireP0IssueKept(t, rig.database, key, before)
	case p0Refused:
		requireP0Refusal(t, response)
		requireP0IssueKept(t, rig.database, key, before)
	case p0Written:
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200: the credential holds p0", response.Code, response.Body.String())
		}
		if after := readP0Issue(t, rig.database, key); !isP0(after.priority) {
			t.Errorf("%s after the write of priority 0: %s, want priority 0", key, after)
		}
	}
}

// p0IssueWrite is one write of p0IssueWrites: a write of priority 0 that send sends from one
// credential on an issue it makes for that credential and that case. from is each own priority
// the issue holds before the write, nil being none; a create has none alone.
type p0IssueWrite struct {
	from []*int
	send func(t *testing.T, rig *legionStatusRig, credential p0Credential, from *int)
}

// p0IssueWrites are the requests of TestEveryOperationThatTakesAnIssuePriorityNeedsP0ForZero, by
// the operation, as "METHOD path", that each one sends. Each operation of a committed OpenAPI
// document whose request body takes a priority has its requests here, or its reason in
// p0PriorityFilters.
var p0IssueWrites = map[string]p0IssueWrite{
	"POST /api/v1/issues": {from: []*int{nil}, send: func(t *testing.T, rig *legionStatusRig, credential p0Credential, _ *int) {
		t.Helper()
		title := rig.title()
		response := credential.send(t, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": brokerTestProject, "title": title, "priority": 0, "force": true,
		})
		switch credential.p0 {
		case p0RefusedFirst:
			requireRefused(t, response, http.StatusForbidden, credential.code)
			requireNoIssueTitled(t, rig.database, title)
		case p0Refused:
			requireP0Refusal(t, response)
			requireNoIssueTitled(t, rig.database, title)
		case p0Written:
			if response.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s, want 201: the credential holds p0", response.Code, response.Body.String())
			}
			key := decodeBody[model.Issue](t, response).Key
			if made := readP0Issue(t, rig.database, key); !isP0(made.priority) {
				t.Errorf("%s as the create made it: %s, want priority 0", key, made)
			}
		}
	}},
	"PATCH /api/v1/issues/{key}": {from: []*int{nil, new(2)}, send: func(t *testing.T, rig *legionStatusRig, credential p0Credential, from *int) {
		t.Helper()
		fields := map[string]any{}
		if from != nil {
			fields["priority"] = *from
		}
		key := rig.personIssue(t, fields).Key
		before := readP0Issue(t, rig.database, key)
		response := credential.send(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"priority": 0})
		rig.requireP0Answer(t, credential, key, before, response)
	}},
}

// p0PriorityFilters are the operations whose request body takes a priority that is no issue's,
// each with its reason. The list is closed: an operation found outside it and outside
// p0IssueWrites fails TestEveryOperationThatTakesAnIssuePriorityNeedsP0ForZero by name.
var p0PriorityFilters = map[string]string{
	"POST /api/v1/me/inbox/dismiss": "its priority is a filter of the Inbox rows the dismissal reaches, and no issue's priority",
	"POST /api/v1/me/inbox/read":    "its priority is a filter of the Inbox rows the mark-all reaches, and no issue's priority",
}

// openAPIMethods are the keys of an OpenAPI path item that name an operation.
var openAPIMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// committedOpenAPIDocuments reads every committed dispatch-openapi*.json beside
// OpenAPIDocumentPath, by its path from the repository root, and fails unless
// OpenAPIDocumentPath is among them.
func committedOpenAPIDocuments(t *testing.T) map[string]map[string]any {
	t.Helper()
	// From this package to the repository root, which OpenAPIDocumentPath is written from.
	root := filepath.Join("..", "..", "..", "..", "..")
	paths, err := filepath.Glob(filepath.Join(root, filepath.Dir(filepath.FromSlash(OpenAPIDocumentPath)), "dispatch-openapi*.json"))
	if err != nil {
		t.Fatalf("glob the committed OpenAPI documents: %v", err)
	}
	documents := map[string]map[string]any{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var document map[string]any
		if err := json.Unmarshal(raw, &document); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatalf("name %s from the repository root: %v", path, err)
		}
		documents[filepath.ToSlash(name)] = document
	}
	if _, read := documents[OpenAPIDocumentPath]; !read {
		t.Fatalf("read %v, which does not hold %s", slices.Sorted(maps.Keys(documents)), OpenAPIDocumentPath)
	}
	return documents
}

// priorityOperations answers each operation of document, as "METHOD path", whose request body,
// under any of its media types, has a property named priority.
func priorityOperations(t *testing.T, name string, document map[string]any) []string {
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
				if takesPriority(t, name, schema["schema"], schemas, map[string]bool{}) {
					found = append(found, strings.ToUpper(method)+" "+path)
					break
				}
			}
		}
	}
	return found
}

// takesPriority reports whether node, a schema of document name, has a property named priority:
// its own, a nested object's, an array's items', or one a $ref, allOf, anyOf or oneOf reaches.
// seen holds the component schemas already walked, so a schema that refers to itself ends.
func takesPriority(t *testing.T, name string, node any, schemas map[string]any, seen map[string]bool) bool {
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
		if takesPriority(t, name, target, schemas, seen) {
			return true
		}
	}
	for _, combinator := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[combinator].([]any)
		for _, branch := range branches {
			if takesPriority(t, name, branch, schemas, seen) {
				return true
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	if _, named := properties["priority"]; named {
		return true
	}
	for _, property := range properties {
		if takesPriority(t, name, property, schemas, seen) {
			return true
		}
	}
	return takesPriority(t, name, schema["items"], schemas, seen)
}

// TestEveryOperationThatTakesAnIssuePriorityNeedsP0ForZero is the inventory of G1. It reads every
// committed dispatch-openapi*.json and takes each operation whose request body has a property
// named priority. Each such operation has its requests in p0IssueWrites or its reason in the
// closed list p0PriorityFilters, and the test fails by name for one that has neither, so an
// operation that comes to take a priority (a v2 issue write) fails it until it has its requests.
// It fails too when it finds fewer than the two v1 writes.
//
// For each write, for an issue whose own priority is none and one at 2 (a create has the first
// alone), and for each credential of p0Inventory, it writes priority 0 on an issue of its own,
// made for that credential and that case, so no earlier write makes a later one a write that
// keeps P0. A credential that holds p0 writes it, and the stored own priority is 0. Every other
// credential that reaches the handler is refused 403 OAUTH_SCOPE_REQUIRED naming p0, and the
// issue keeps its priority and last_seq, or, for a create, the project holds no issue with its
// title. The two credentials a check in front of the handler refuses keep their own codes.
func TestEveryOperationThatTakesAnIssuePriorityNeedsP0ForZero(t *testing.T) {
	t.Parallel()
	found := map[string][]string{}
	for name, document := range committedOpenAPIDocuments(t) {
		for _, operation := range priorityOperations(t, name, document) {
			found[operation] = append(found[operation], name)
		}
	}
	for _, write := range []string{"POST /api/v1/issues", "PATCH /api/v1/issues/{key}"} {
		if _, ok := found[write]; !ok {
			t.Fatalf("the committed documents take a priority in the request bodies of %v alone, which leaves out %s: the inventory found fewer than the two v1 writes", slices.Sorted(maps.Keys(found)), write)
		}
	}
	var writes []string
	for _, operation := range slices.Sorted(maps.Keys(found)) {
		if _, written := p0IssueWrites[operation]; written {
			writes = append(writes, operation)
			continue
		}
		if reason, filter := p0PriorityFilters[operation]; filter {
			t.Logf("%s takes a priority and is no write of one: %s", operation, reason)
			continue
		}
		t.Errorf("%s (%s) takes a priority in its request body, and this test sends it no write of priority 0: add its requests to p0IssueWrites, or, where its priority is no issue's, its reason to p0PriorityFilters",
			operation, strings.Join(found[operation], ", "))
	}

	rig := newLegionStatusRig(t, false)
	inventory := p0Inventory(t, rig)
	for _, operation := range writes {
		write := p0IssueWrites[operation]
		t.Run(operation, func(t *testing.T) {
			for _, from := range write.from {
				t.Run("an issue whose own priority is "+priorityName(from), func(t *testing.T) {
					for _, credential := range inventory {
						t.Run(credential.name, func(t *testing.T) { write.send(t, rig, credential, from) })
					}
				})
			}
		})
	}
}

// TestARefusedP0WriteWritesNothingElseOfItsRequest: the shared token's write of priority 0 is
// refused whole, and nothing else of its request is written. A PATCH that sends a title and
// labels_add beside it leaves the title, the labels and the issue's newest event as they were. A
// create that sends a title and labels beside it leaves no issue and no near_copy_checks row for
// that title. A PATCH that closes an open issue at priority 2 beside it, whose document holds an
// edit the close's settlement would save, leaves the issue open, its document with no new version,
// and no ask event appended. A create naming an external ref already imported is answered that
// issue as before, and the issue's priority does not change.
func TestARefusedP0WriteWritesNothingElseOfItsRequest(t *testing.T) {
	t.Parallel()
	// Settlement an hour away: the edit the close's case writes is still unsettled when the
	// close is refused, so only a settlement the close ran could save it.
	rig := newLegionStatusRigOn(t, testServerOptions{settle: time.Hour})
	shared := rig.credential(t, legionSharedToken)

	t.Run("a PATCH of priority 0 with a title and labels_add", func(t *testing.T) {
		key := rig.personIssue(t, map[string]any{"priority": 2, "labels": []string{"area"}}).Key
		before := readP0Issue(t, rig.database, key)
		refused := shared.send(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"priority": 0, "title": "A title the refused write would give", "labels_add": []string{"ops"},
		})
		requireP0Refusal(t, refused)
		requireP0IssueKept(t, rig.database, key, before)
	})

	t.Run("a create of priority 0 with a title and labels", func(t *testing.T) {
		title := rig.title()
		refused := shared.send(t, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": brokerTestProject, "title": title, "labels": []string{"area", "ops"}, "priority": 0, "force": true,
		})
		requireP0Refusal(t, refused)
		// A create's near_copy_checks row may be written after its answer (recordNearCopyCheck).
		awaitBackgroundWrites(t, rig.served)
		requireNoIssueTitled(t, rig.database, title)
		if checks := countRows(t, rig.database, `select count(*) from near_copy_checks where title = $1`, title); checks != 0 {
			t.Errorf("the refused create left %d near_copy_checks rows titled %q, want none", checks, title)
		}
	})

	t.Run("a close with priority 0 of an issue whose document holds an unsettled edit", func(t *testing.T) {
		issue := rig.personIssue(t, map[string]any{"priority": 2, "spec": "Context\n"})
		// The person types a decision block into the spec through its room, as a browser does: an
		// edit that is no version until a settlement saves it, and a block that is no ask until a
		// settlement credits it (replaceDocumentText).
		if _, err := replaceDocumentText(rig.database, rig.served.deps.Docs, issue.PrimaryArtifactID,
			"Context\n\n:::ask{#p0-close urgency=\"high\" multiple=\"false\"}\nShip it now?\n:::\n",
			model.Actor{Kind: "user", ID: brokerTestPerson}); err != nil {
			t.Fatalf("rig: a person types an ask block into %s's spec: %v", issue.Key, err)
		}
		before := readP0Issue(t, rig.database, issue.Key)
		versions := uploadedVersionCount(t, rig.database, issue.PrimaryArtifactID)
		askEvents := func() int64 {
			return countRows(t, rig.database, `select count(*) from events where issue_key = $1 and type like 'ask.%'`, issue.Key)
		}
		asked := askEvents()
		refused := shared.send(t, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"status": "done", "priority": 0})
		requireP0Refusal(t, refused)
		requireP0IssueKept(t, rig.database, issue.Key, before)
		if after := uploadedVersionCount(t, rig.database, issue.PrimaryArtifactID); after != versions {
			t.Errorf("the refused close left %s's document with %d versions, want the %d it had: it settled the document", issue.Key, after, versions)
		}
		if after := askEvents(); after != asked {
			t.Errorf("the refused close appended %d ask events on %s, want none", after-asked, issue.Key)
		}
	})

	t.Run("a create of priority 0 naming an external ref already imported", func(t *testing.T) {
		const ref = "owner/repo#5637"
		imported := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{"project": brokerTestProject, "external": ref})
		if imported.Code != http.StatusCreated {
			t.Fatalf("import %s as a person: status=%d body=%s", ref, imported.Code, imported.Body.String())
		}
		key := decodeBody[model.Issue](t, imported).Key
		before := readP0Issue(t, rig.database, key)
		answered := shared.send(t, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": brokerTestProject, "external": ref, "priority": 0,
		})
		if answered.Code != http.StatusOK {
			t.Fatalf("a create naming %s, which %s holds: status=%d body=%s, want 200 and that issue", ref, key, answered.Code, answered.Body.String())
		}
		if got := decodeBody[model.Issue](t, answered).Key; got != key {
			t.Errorf("a create naming %s answered %s, want %s, which holds it", ref, got, key)
		}
		if after := readP0Issue(t, rig.database, key); priorityName(after.priority) != priorityName(before.priority) {
			t.Errorf("%s after a create naming its ref with priority 0: %s, want its priority as it was: %s", key, after, before)
		}
	})
}

// TestTheIssuesOwnPriorityDecidesTheP0Check: under a root that a browser made P0, the shared
// token's write of priority 0 on a child with no own priority is refused 403 naming p0, and so is
// a create of a child at priority 0: the priority the child inherits is not its own. A create of a
// child that names no priority is written, with no own priority.
func TestTheIssuesOwnPriorityDecidesTheP0Check(t *testing.T) {
	t.Parallel()
	rig := newLegionStatusRig(t, false)
	shared := rig.credential(t, legionSharedToken)
	root := rig.personIssue(t, map[string]any{"priority": 0}).Key
	if made := readP0Issue(t, rig.database, root); !isP0(made.priority) {
		t.Fatalf("rig: the root a browser made at priority 0 is %s", made)
	}

	t.Run("a PATCH of priority 0 on a child with no own priority", func(t *testing.T) {
		child := rig.personIssue(t, map[string]any{"parent": root}).Key
		before := readP0Issue(t, rig.database, child)
		refused := shared.send(t, http.MethodPatch, "/api/v1/issues/"+child, map[string]any{"priority": 0})
		requireP0Refusal(t, refused)
		requireP0IssueKept(t, rig.database, child, before)
	})

	t.Run("a create of a child at priority 0", func(t *testing.T) {
		title := rig.title()
		refused := shared.send(t, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": brokerTestProject, "title": title, "parent": root, "priority": 0, "force": true,
		})
		requireP0Refusal(t, refused)
		requireNoIssueTitled(t, rig.database, title)
	})

	t.Run("a create of a child with no priority", func(t *testing.T) {
		made := shared.send(t, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": brokerTestProject, "title": rig.title(), "parent": root, "force": true,
		})
		if made.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s, want 201: the create names no priority", made.Code, made.Body.String())
		}
		child := decodeBody[model.Issue](t, made)
		if child.Parent == nil || *child.Parent != root {
			t.Errorf("the child's parent = %v, want %s", child.Parent, root)
		}
		if stored := readP0Issue(t, rig.database, child.Key); stored.priority != nil {
			t.Errorf("%s as the create made it: %s, want no own priority", child.Key, stored)
		}
	})
}

// TestAWriteThatKeepsOrLowersP0OrSetsAnotherPriorityNeedsNoScope: the shared token, which holds no
// scope, writes priority 0 on an issue at P0, moves an issue from P0 to 1 and to none, moves one
// from 2 to 1, and creates issues at priority 1, at 3 and with none. None of them makes an issue's
// own priority 0.
func TestAWriteThatKeepsOrLowersP0OrSetsAnotherPriorityNeedsNoScope(t *testing.T) {
	t.Parallel()
	rig := newLegionStatusRig(t, false)
	shared := rig.credential(t, legionSharedToken)
	for _, patch := range []struct {
		name     string
		from, to *int
	}{
		{"a PATCH of priority 0 on an issue at P0", new(0), new(0)},
		{"a PATCH from P0 to 1", new(0), new(1)},
		{"a PATCH from P0 to none", new(0), nil},
		{"a PATCH from 2 to 1", new(2), new(1)},
	} {
		t.Run(patch.name, func(t *testing.T) {
			key := rig.personIssue(t, map[string]any{"priority": *patch.from}).Key
			var to any
			if patch.to != nil {
				to = *patch.to
			}
			written := shared.send(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"priority": to})
			if written.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", written.Code, written.Body.String())
			}
			if after := readP0Issue(t, rig.database, key); priorityName(after.priority) != priorityName(patch.to) {
				t.Errorf("%s after the write: %s, want priority %s", key, after, priorityName(patch.to))
			}
		})
	}
	for _, create := range []struct {
		name     string
		priority *int
	}{
		{"a create at priority 1", new(1)},
		{"a create at priority 3", new(3)},
		{"a create with no priority", nil},
	} {
		t.Run(create.name, func(t *testing.T) {
			body := map[string]any{"project": brokerTestProject, "title": rig.title(), "force": true}
			if create.priority != nil {
				body["priority"] = *create.priority
			}
			made := shared.send(t, http.MethodPost, "/api/v1/issues", body)
			if made.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s, want 201", made.Code, made.Body.String())
			}
			key := decodeBody[model.Issue](t, made).Key
			if stored := readP0Issue(t, rig.database, key); priorityName(stored.priority) != priorityName(create.priority) {
				t.Errorf("%s as the create made it: %s, want priority %s", key, stored, priorityName(create.priority))
			}
		})
	}
}

// TestTheP0RefusalComesBeforeTheClosedAndChangedRefusals: the p0 check is the first refusal of a
// PATCH. On a closed issue with no priority, the shared token's write of priority 0 is refused 403
// naming p0, where a browser's, which holds p0, is 409 ISSUE_CLOSED. On an open issue, the same
// write with a stale expected_sequence is refused 403 naming p0 from the shared token, and 409
// ISSUE_CHANGED from a browser. No refusal changes the issue.
func TestTheP0RefusalComesBeforeTheClosedAndChangedRefusals(t *testing.T) {
	t.Parallel()
	rig := newLegionStatusRig(t, false)
	shared, browser := rig.credential(t, legionSharedToken), rig.credential(t, legionBrowserSignIn)
	refusals := []struct {
		credential legionCredential
		require    func(t *testing.T, response *httptest.ResponseRecorder, conflict string)
	}{
		{shared, func(t *testing.T, response *httptest.ResponseRecorder, _ string) {
			t.Helper()
			requireP0Refusal(t, response)
		}},
		{browser, func(t *testing.T, response *httptest.ResponseRecorder, conflict string) {
			t.Helper()
			requireRefused(t, response, http.StatusConflict, conflict)
		}},
	}

	t.Run("on a closed issue with no priority", func(t *testing.T) {
		for _, refusal := range refusals {
			t.Run(refusal.credential.name, func(t *testing.T) {
				key := rig.issue(t, nil, "", "done")
				before := readP0Issue(t, rig.database, key)
				response := refusal.credential.send(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"priority": 0})
				refusal.require(t, response, "ISSUE_CLOSED")
				requireP0IssueKept(t, rig.database, key, before)
			})
		}
	})

	t.Run("with a stale expected_sequence", func(t *testing.T) {
		for _, refusal := range refusals {
			t.Run(refusal.credential.name, func(t *testing.T) {
				key := rig.issue(t, nil, "", "")
				stale := readP0Issue(t, rig.database, key).lastSeq
				if moved := rig.asPerson(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"labels_add": []string{"moved"}}); moved.Code != http.StatusOK {
					t.Fatalf("move %s on as a person: status=%d body=%s", key, moved.Code, moved.Body.String())
				}
				before := readP0Issue(t, rig.database, key)
				response := refusal.credential.send(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
					"priority": 0, "expected_sequence": stale,
				})
				refusal.require(t, response, "ISSUE_CHANGED")
				requireP0IssueKept(t, rig.database, key, before)
			})
		}
	})
}

// p0Sentence matches a sentence that names a priority of 0, and p0Scope one that names the p0
// scope.
var (
	p0Sentence = regexp.MustCompile("(?i)\\bpriority (of )?`?0\\b")
	p0Scope    = regexp.MustCompile(`\b` + model.ScopeP0 + `\b`)
)

// sentenceEnd splits a description into its sentences.
var sentenceEnd = regexp.MustCompile(`[.!?](\s+|$)`)

// TestTheRouteIndexSaysAPriorityOf0NeedsP0: GET /api/v1, the route index every agent reads, says
// on each of the two issue writes that a priority of 0 needs p0: the entries of POST
// /api/v1/issues and PATCH /api/v1/issues/{key} each hold a sentence that names a priority of 0
// and p0. The sentence's other words are the table's own.
func TestTheRouteIndexSaysAPriorityOf0NeedsP0(t *testing.T) {
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
	for _, route := range []string{"POST /api/v1/issues", "PATCH /api/v1/issues/{key}"} {
		entry, listed := entries[route]
		if !listed {
			t.Errorf("GET /api/v1 lists no %s", route)
			continue
		}
		says := slices.ContainsFunc(sentenceEnd.Split(entry.Description, -1), func(sentence string) bool {
			return p0Sentence.MatchString(sentence) && p0Scope.MatchString(sentence)
		})
		if !says {
			t.Errorf("GET /api/v1's entry of %s has no sentence that names a priority of 0 and %s: %s", route, model.ScopeP0, entry.Description)
		}
	}
}
