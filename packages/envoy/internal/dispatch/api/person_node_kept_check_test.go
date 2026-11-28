package api

// person_node_kept_check_test.go holds what TestOnlyAPersonChangesAPersonsDecision (c-person-node-kept)
// reads beside the rig of person_node_kept_test.go: how a failure names what a write answered, the
// fixtures each destructive write and each rule takes of its own, and the reads of the words, the
// tree and the credentials the check's words name.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// personKeptAnswer is what a write answered, as a failure of the check names it: its status, and
// beside it the code of a refusal that carries one.
func personKeptAnswer(response *httptest.ResponseRecorder) string {
	var refusal refusalAnswer
	if json.Unmarshal(response.Body.Bytes(), &refusal) == nil && refusal.Code != "" {
		return strconv.Itoa(response.Code) + " " + refusal.Code
	}
	return strconv.Itoa(response.Code)
}

// personKeptTook fails t unless response is status, a write the check wants taken, printing
// `<what> answered <status>, want <status>` and the body, which holds the refusal it carries.
func personKeptTook(t *testing.T, what string, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("%s answered %d, want %d: %s", what, response.Code, status, response.Body.String())
	}
}

// personKeptWordsOf is the words of the decision id that markdown holds, as its sum covers them
// (refs.DecisionWords), and "" where markdown holds no decision of that id.
func personKeptWordsOf(t *testing.T, markdown, id string) string {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("read the document back: %v", err)
	}
	var block *pmdoc.Node
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type == refs.DecisionSupertag && refs.TypedBlockID(node) == id {
			block = node
		}
		return block == nil
	})
	if block == nil {
		return ""
	}
	words, err := refs.DecisionWords(block)
	if err != nil {
		t.Fatalf("render the words of %s: %v", id, err)
	}
	return words
}

// projectKey is the key of a project no other fixture of the check shares: each destructive write,
// and each rule the check holds apart, takes a fixture of its own.
func (k *personKeptRig) projectKey() string {
	key := fmt.Sprintf("PKF%c%c", 'A'+k.projects/26, 'A'+k.projects%26)
	k.projects++
	return key
}

// freshIssue is an issue fixture (personKeptRig.issue, which writes its project) in a project of
// its own.
func (k *personKeptRig) freshIssue(t *testing.T) personKeptIssue {
	t.Helper()
	return k.issue(t, k.projectKey())
}

// freshProjectDocument is alice's project document (personKeptRig.projectDocument) in a project of
// its own, which alice writes first.
func (k *personKeptRig) freshProjectDocument(t *testing.T) personKeptProject {
	t.Helper()
	project := k.projectKey()
	createReferenceAPIProject(t, k.handler, project)
	return k.projectDocument(t, project)
}

// whoami is what GET /api/v1/whoami tells caller of its credential.
func (k *personKeptRig) whoami(t *testing.T, caller personKeptCaller) whoamiAnswer {
	t.Helper()
	response := k.send(t, caller, http.MethodGet, "/api/v1/whoami", nil, "")
	succeeded(t, caller.name+"'s whoami", response)
	return decodeBody[whoamiAnswer](t, response)
}

// legionIssue writes an issue Legion runs: alice files it in a project of its own with the label
// legion and personKeptSpec, and the project's daemon publishes its role holders on it, an
// architect session and an implementer session, which marks the project (newNodeWriterRig). The
// shared token's session is neither, so R28's writer check refuses it every requirement,
// criterion and check node of the issue. alice answers q1, as on every fixture
// (personKeptRig.issue), so no ask of hers stays open beside the next fixture's.
func (k *personKeptRig) legionIssue(t *testing.T) personKeptIssue {
	t.Helper()
	project := k.projectKey()
	createReferenceAPIProject(t, k.handler, project)
	issue := created201[model.Issue](t, "alice files an issue Legion runs", dispatchRequest(t, k.handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": "An issue Legion runs", "spec": personKeptSpec, "labels": []string{"legion"}, "force": true,
	}, "alice"))
	spec := readDecisionDocument(t, k.database, issue.Key, "spec")
	k.documents = append(k.documents, spec.id)
	if published := putRoleHoldersAs(t, k.handler, issue.Key, map[string]any{
		"holders": []issueRoleHolder{{Role: "architect", Session: "ses-architect-" + strings.ToLower(project)}, {Role: "implementer", Session: "ses-implementer-" + strings.ToLower(project)}},
		"actor":   daemonSessionOf(project),
	}); published.Code != http.StatusOK {
		t.Fatalf("the daemon of %s publishes its role holders on %s: status=%d body=%s", project, issue.Key, published.Code, published.Body.String())
	}
	ask := indexedDecisionBlocks(t, k.handler, issue.Key, spec.id, map[string]string{"q1": personKeptQuestion})["q1"]
	answerDecisionBlock(t, k.handler, ask, "alice", []string{"Ledger"}, "", nil)
	awaitSettled(t, k.database, spec.id)
	fixture := personKeptIssue{key: issue.Key, spec: spec, ask: ask}
	for _, decision := range []string{"pd1", "pd2"} {
		requireDecidedBy(t, k.database, "alice's decision "+decision, fixture.node(decision), "alice")
	}
	return fixture
}

// treeNode is the one node of the tree read of key, as alice reads it, whose kind and id are given.
func (k *personKeptRig) treeNode(t *testing.T, key, kind, id string) model.IssueTreeNode {
	t.Helper()
	node, _ := treeRead(t, k.handler, key, "", "alice", false).node(t, kind, id)
	return node
}

// hideFromAgents reserves the node of the block address for alice, as the store reserves a node of
// a kind it admits so (graphstore.CheckVisibilityAdmitted), so a caller that proves no author of
// hers may not see it. The store admits no document block so: no route writes a person's decision
// a caller may not see, so the check states it by SQL, in a fixture of its own.
func (k *personKeptRig) hideFromAgents(t *testing.T, address string) {
	t.Helper()
	written, err := k.database.Pool.Exec(context.Background(), `
		update graph_nodes set visibility = $3, made_by = '{"kind":"user","id":"alice"}'::jsonb
		where kind = $1 and id = $2`, refs.BlockNodeKind, address, graphstore.VisibilityAuthor)
	if err != nil || written.RowsAffected() != 1 {
		t.Fatalf("reserve the node %s for alice: %d rows, %v", address, written.RowsAffected(), err)
	}
}

// requireNamesNone fails t unless the refusal of what names none of names.
func requireNamesNone(t *testing.T, what string, refusal refusalAnswer, names ...string) {
	t.Helper()
	for _, name := range names {
		if strings.Contains(refusal.Error, name) {
			t.Errorf("%s: the refusal %q names %s, which the caller may not see", what, refusal.Error, name)
		}
	}
}

// personKeptHolds fails t unless scopes holds want and none of refused.
func personKeptHolds(t *testing.T, what string, scopes []string, want []string, refused ...string) {
	t.Helper()
	for _, scope := range want {
		if !slices.Contains(scopes, scope) {
			t.Errorf("%s holds the scopes %v, want %s among them", what, scopes, scope)
		}
	}
	for _, scope := range refused {
		if slices.Contains(scopes, scope) {
			t.Errorf("%s holds the scopes %v, want no %s", what, scopes, scope)
		}
	}
}
