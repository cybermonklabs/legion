// legion_status_scope_test.go holds Dispatch to R26 of dispatch://INFRA-3320 (check k41 of
// dispatch://INFRA-4732). With Deps.LegionStatusScope on (DISPATCH_LEGION_STATUS_SCOPE), three
// writes need a credential that holds the person scope or the legion-status scope: a write of a
// Legion issue's status, the removal of the legion label from an issue, and a move of a Legion
// issue out from under every labelled issue. A Legion issue carries the label, in any case, or
// is under an issue that carries it. The server refuses such a write from any other credential
// with 403 LEGION_STATUS_SCOPE_REQUIRED, and the issue keeps its status, labels, parent and
// events (requireLegionIssueUnchanged).
//
// The tests send their writes from the credentials of one inventory (newLegionStatusRig), so each
// test reads the same list of the credentials that R26 refuses and the credentials it lets through.
// The tests need DISPATCH_TEST_DATABASE_URL, as every test of this package that opens a store.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The names of the six credentials of the inventory that a test selects by name.
const (
	legionSharedToken           = "the shared token"
	legionUnscopedPersonalToken = "a personal token holding no scope"
	legionProjectBrokerToken    = "a project's broker token"
	legionDaemonToken           = "the daemon's broker token"
	legionBrowserSignIn         = "a browser sign-in"
	legionPersonScopedToken     = "a broker token scoped to a person"
)

// legionVerdict is what the server does with a credential's write of a Legion issue.
type legionVerdict int

const (
	// legionRefused is a credential that holds neither the person scope nor the legion-status
	// scope. R26 refuses its write.
	legionRefused legionVerdict = iota
	// legionTaken is a credential that holds one of the two scopes. R26 lets its write through.
	legionTaken
	// legionRefusedFirst is a credential that a check in front of the handler refuses for every
	// write, with a code of its own. R26 never sees its write.
	legionRefusedFirst
)

// legionCredential is one credential of the inventory.
type legionCredential struct {
	name    string
	verdict legionVerdict
	// code is the code of the check in front of the handler, for a legionRefusedFirst credential.
	code string
	// send sends one request with the credential. A bearer that proves no session of its own
	// must name one in the body's actor, so send adds the actor when the body names none.
	send func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder
}

// legionStatusRig is a server whose broker-token check asks a stub broker, the project
// brokerTestProject, and the inventory of credentials that may write in that project.
type legionStatusRig struct {
	*brokerTokenRig
	credentials []legionCredential
	// made counts the issues the rig made, so that each title is a new one.
	made int
}

// newLegionStatusRig builds the server with R26's setting on or off (newLegionStatusRigOn).
func newLegionStatusRig(t *testing.T, on bool) *legionStatusRig {
	t.Helper()
	return newLegionStatusRigOn(t, testServerOptions{legionStatusScope: on})
}

// newLegionStatusRigOn builds the server from serving, with the project and the inventory:
//
//   - R26 refuses: the shared token; the shared token whose body's actor names the daemon's
//     session, since a bearer's body can name any session and proves none; a personal token
//     that holds no scope; a project's broker token, named and acting for a person; a pod's own
//     Kubernetes token.
//   - R26 lets through: a browser sign-in; a personal token that holds person; a broker token
//     scoped to a person; the daemon's broker token, named, of the daemon's session, whose scope
//     is legion-status.
//   - A check in front of the handler refuses, each with its own code: a read-only broker token
//     (403 READ_ONLY_TOKEN), and a broker token that names a service and no person (403
//     SIGN_IN_REQUIRED).
func newLegionStatusRigOn(t *testing.T, serving testServerOptions) *legionStatusRig {
	t.Helper()
	issuer := newServiceTokenIssuer(t)
	serving.oidc = issuer.verifier(t)
	rig := &legionStatusRig{
		brokerTokenRig: newBrokerTokenRigOn(t, serving),
	}
	if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{
		"key": brokerTestProject, "name": "Broker",
	}); created.Code != http.StatusCreated {
		t.Fatalf("create the project as a person: status=%d body=%s", created.Code, created.Body.String())
	}
	caller := newBrokerCaller(t)
	broker := func(token string, answer ciphertoken.Introspection) func(*testing.T, string, string, map[string]any) *httptest.ResponseRecorder {
		rig.broker.Says(token, answer)
		return func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			return rig.serve(caller.presenting(t, method, path, token, body))
		}
	}
	bearer := func(token string, actor map[string]any) func(*testing.T, string, string, map[string]any) *httptest.ResponseRecorder {
		return func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			named := maps.Clone(body)
			if named == nil {
				named = map[string]any{}
			}
			if _, ok := named["actor"]; !ok {
				named["actor"] = actor
			}
			return rig.serve(brokerRequest(t, method, path, named, "Bearer "+token, ""))
		}
	}
	personScoped := liveNamedToken(caller.jkt)
	personScoped.ProjectScope, personScoped.Projects = ciphertoken.ProjectScopePerson, nil
	rig.credentials = []legionCredential{
		{name: legionSharedToken, verdict: legionRefused, send: bearer("agent-token", sessionActor())},
		{name: "the shared token naming the daemon's session", verdict: legionRefused,
			send: bearer("agent-token", daemonSessionOf(brokerTestProject))},
		{name: legionUnscopedPersonalToken, verdict: legionRefused,
			send: bearer(personalTokenHolding(t, rig.database), sessionActor())},
		{name: legionProjectBrokerToken, verdict: legionRefused,
			send: broker("a-project-token", liveNamedToken(caller.jkt))},
		{name: "a pod's Kubernetes token", verdict: legionRefused,
			send: bearer(issuer.mint(t, serviceTokenAudience), sessionActor())},
		{name: legionBrowserSignIn, verdict: legionTaken,
			send: func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
				return dispatchRequest(t, rig.handler, method, path, body, brokerTestPerson)
			}},
		{name: "a personal token holding person", verdict: legionTaken,
			send: bearer(personalTokenHolding(t, rig.database, model.ScopePerson), sessionActor())},
		{name: legionPersonScopedToken, verdict: legionTaken,
			send: broker("a-person-scoped-token", personScoped)},
		{name: legionDaemonToken, verdict: legionTaken, send: broker("the-daemon-token", liveDaemonToken(caller.jkt))},
		{name: "a read-only broker token", verdict: legionRefusedFirst, code: contracts.DispatchReadOnlyToken,
			send: broker("a-read-only-token", readOnlyToken(caller.jkt))},
		{name: "a broker token naming a service and no person", verdict: legionRefusedFirst, code: contracts.DispatchSignInRequired,
			send: broker("a-service-token", serviceOnlyToken(caller.jkt))},
	}
	return rig
}

// credential is the credential of the inventory named name.
func (rig *legionStatusRig) credential(t *testing.T, name string) legionCredential {
	t.Helper()
	for _, credential := range rig.credentials {
		if credential.name == name {
			return credential
		}
	}
	t.Fatalf("the inventory holds no credential named %q", name)
	return legionCredential{}
}

// title is a new issue title.
func (rig *legionStatusRig) title() string {
	rig.made++
	return fmt.Sprintf("Legion status probe %d", rig.made)
}

// issue makes an issue of brokerTestProject as a person: with labels, under parent (a root when
// parent is empty), and then moved to status. An empty status keeps triage, the status of every
// new issue.
func (rig *legionStatusRig) issue(t *testing.T, labels []string, parent, status string) string {
	t.Helper()
	body := map[string]any{"project": brokerTestProject, "title": rig.title(), "force": true}
	if len(labels) > 0 {
		body["labels"] = labels
	}
	if parent != "" {
		body["parent"] = parent
	}
	created := rig.asPerson(t, http.MethodPost, "/api/v1/issues", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("make an issue as a person: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[model.Issue](t, created).Key
	if status != "" && status != model.IssueStatusTriage {
		if moved := rig.asPerson(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"status": status}); moved.Code != http.StatusOK {
			t.Fatalf("move %s to %s as a person: status=%d body=%s", key, status, moved.Code, moved.Body.String())
		}
	}
	return key
}

// legionIssueState is what R26 guards on an issue, and the sequence that every write of the
// issue moves.
type legionIssueState struct {
	status  string
	labels  []string
	parent  *string
	lastSeq int
}

func readLegionIssue(t *testing.T, database *store.Store, key string) legionIssueState {
	t.Helper()
	var state legionIssueState
	if err := database.Pool.QueryRow(context.Background(),
		`select status, labels, parent_key, last_seq from issues where key = $1`, key,
	).Scan(&state.status, &state.labels, &state.parent, &state.lastSeq); err != nil {
		t.Fatalf("read issue %s: %v", key, err)
	}
	return state
}

func (s legionIssueState) String() string {
	parent := "none"
	if s.parent != nil {
		parent = *s.parent
	}
	return fmt.Sprintf("status %s, labels %q, parent %s, last_seq %d", s.status, s.labels, parent, s.lastSeq)
}

func (s legionIssueState) equal(other legionIssueState) bool {
	return s.status == other.status && slices.Equal(s.labels, other.labels) &&
		stringPointersEqual(s.parent, other.parent) && s.lastSeq == other.lastSeq
}

// requireLegionIssueUnchanged fails unless key is as before: a refused write changes no status,
// label or parent, and appends no event.
func requireLegionIssueUnchanged(t *testing.T, database *store.Store, key string, before legionIssueState) {
	t.Helper()
	if after := readLegionIssue(t, database, key); !after.equal(before) {
		t.Errorf("%s after the refused write: %s; want it as it was: %s", key, after, before)
	}
}

// requireRefused fails unless response is a refusal with status and code.
func requireRefused(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != status || refusal.Code != code {
		t.Errorf("status=%d body=%s, want %d %s", response.Code, response.Body.String(), status, code)
	}
}

// requireLegionStatusRefusal fails unless response is R26's refusal: 403
// LEGION_STATUS_SCOPE_REQUIRED, a message that names the legion-status scope, and the challenge
// RFC 6750 gives a credential that lacks that scope.
func requireLegionStatusRefusal(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	requireRefused(t, response, http.StatusForbidden, codeLegionStatusScopeRequired)
	var refusal refusalAnswer
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if !strings.Contains(refusal.Error, "legion-status") {
		t.Errorf("the refusal's message %q names no legion-status scope", refusal.Error)
	}
	if got, want := response.Header().Get("WWW-Authenticate"), `Bearer error="insufficient_scope", scope="legion-status"`; got != want {
		t.Errorf("the refusal's challenge = %q, want %q", got, want)
	}
}

// answered fails unless response is what the server owes credential for its write of key, and
// reports whether the server took the write. judged says whether R26 judges the write: the write
// sets a Legion issue's status, removes the legion label, or moves a Legion issue out from under
// every labelled issue.
//
//   - A check in front of the handler refuses a legionRefusedFirst credential, whatever the
//     write.
//   - With the setting on, R26 refuses a judged write from a legionRefused credential.
//   - The server takes every other write, 200.
//
// A refused write leaves key as before.
func (rig *legionStatusRig) answered(t *testing.T, credential legionCredential, judged bool, key string, before legionIssueState, response *httptest.ResponseRecorder) bool {
	t.Helper()
	switch {
	case credential.verdict == legionRefusedFirst:
		requireRefused(t, response, http.StatusForbidden, credential.code)
		requireLegionIssueUnchanged(t, rig.database, key, before)
		return false
	case judged && rig.deps.LegionStatusScope && credential.verdict == legionRefused:
		requireLegionStatusRefusal(t, response)
		requireLegionIssueUnchanged(t, rig.database, key, before)
		return false
	}
	if response.Code != http.StatusOK {
		t.Errorf("status=%d body=%s, want 200: the server takes this write", response.Code, response.Body.String())
		return false
	}
	return true
}

// legionCheck checks one value a taken write left on its issue.
type legionCheck func(t *testing.T, after legionIssueState)

func hasStatus(status string) legionCheck {
	return func(t *testing.T, after legionIssueState) {
		t.Helper()
		if after.status != status {
			t.Errorf("the taken write left %s, want status %s", after, status)
		}
	}
}

func hasLabels(labels ...string) legionCheck {
	return func(t *testing.T, after legionIssueState) {
		t.Helper()
		if !slices.Equal(after.labels, labels) {
			t.Errorf("the taken write left %s, want labels %q", after, labels)
		}
	}
}

// hasParent checks the issue's parent: an empty parent is no parent.
func hasParent(parent string) legionCheck {
	return func(t *testing.T, after legionIssueState) {
		t.Helper()
		if (parent == "" && after.parent != nil) || (parent != "" && (after.parent == nil || *after.parent != parent)) {
			t.Errorf("the taken write left %s, want parent %q", after, parent)
		}
	}
}

// legionWrite is one PATCH of one issue. make makes, as a person, the issues the write needs, and
// answers the PATCH. judged says whether R26 judges the write (answered).
type legionWrite struct {
	name   string
	judged bool
	make   func(t *testing.T, rig *legionStatusRig) legionPatch
}

// legionPatch is a PATCH of key with body, and the checks of what the write leaves on key when
// the server takes it.
type legionPatch struct {
	key     string
	body    map[string]any
	written []legionCheck
}

// send sends write from credential and checks the answer (answered). For a taken write it checks
// what the write left on its issue.
func (rig *legionStatusRig) send(t *testing.T, credential legionCredential, write legionWrite) {
	t.Helper()
	patch := write.make(t, rig)
	before := readLegionIssue(t, rig.database, patch.key)
	response := credential.send(t, http.MethodPatch, "/api/v1/issues/"+patch.key, patch.body)
	if !rig.answered(t, credential, write.judged, patch.key, before, response) {
		return
	}
	after := readLegionIssue(t, rig.database, patch.key)
	for _, check := range patch.written {
		check(t, after)
	}
}

// sendFromEach sends each write from each credential of the inventory, each pair in a subtest of
// its own and on issues of its own.
func (rig *legionStatusRig) sendFromEach(t *testing.T, writes []legionWrite) {
	t.Helper()
	for _, write := range writes {
		t.Run(write.name, func(t *testing.T) {
			for _, credential := range rig.credentials {
				t.Run(credential.name, func(t *testing.T) { rig.send(t, credential, write) })
			}
		})
	}
}

// statusWrite is a write of status on key, which leaves key at status when the server takes it.
func statusWrite(key, status string) legionPatch {
	return legionPatch{key: key, body: map[string]any{"status": status}, written: []legionCheck{hasStatus(status)}}
}

// legionStatusWrites are test 1's judged writes: a status write on an issue that carries the
// label, and a write of the open status that such an issue holds already.
func legionStatusWrites() []legionWrite {
	return []legionWrite{
		{name: "a status write on a labelled issue", judged: true, make: func(t *testing.T, rig *legionStatusRig) legionPatch {
			return statusWrite(rig.issue(t, []string{"legion"}, "", ""), "todo")
		}},
		{name: "the open status a labelled issue holds", judged: true, make: func(t *testing.T, rig *legionStatusRig) legionPatch {
			return statusWrite(rig.issue(t, []string{"legion"}, "", "backlog"), "backlog")
		}},
	}
}

// legionHandOver is the daemon's hand-over of an issue nobody labelled (HandOver,
// packages/daemon/internal/dispatch/client.go): a status, the label added in the same PATCH, on
// the sequence the daemon read, as the daemon's own session.
func legionHandOver() legionWrite {
	return legionWrite{name: "the daemon's hand-over of an unlabelled issue", judged: true, make: func(t *testing.T, rig *legionStatusRig) legionPatch {
		key := rig.issue(t, nil, "", "")
		return legionPatch{key: key, body: map[string]any{
			"status":            "todo",
			"labels_add":        []string{"legion"},
			"expected_sequence": readLegionIssue(t, rig.database, key).lastSeq,
			"actor":             daemonSessionOf(brokerTestProject),
		}, written: []legionCheck{hasStatus("todo"), hasLabels("legion")}}
	}}
}

// legionLabelRemovals are test 2's judged writes: labels that leave out legion, and labels that
// leave out Legion, the label in another case.
func legionLabelRemovals() []legionWrite {
	removal := func(label string) func(*testing.T, *legionStatusRig) legionPatch {
		return func(t *testing.T, rig *legionStatusRig) legionPatch {
			return legionPatch{
				key:     rig.issue(t, []string{label, "area"}, "", ""),
				body:    map[string]any{"labels": []string{"area"}},
				written: []legionCheck{hasLabels("area")},
			}
		}
	}
	return []legionWrite{
		{name: "labels without legion", judged: true, make: removal("legion")},
		{name: "labels without Legion", judged: true, make: removal("Legion")},
	}
}

// legionBeforeAndAfterWrites are test 3's writes on an unlabelled root, and the labelled issue
// they move it under. R26 judges a status write by the labels and the chain the issue has after
// the call too: a status write that adds the label, or that moves the issue under a labelled
// issue, is judged. The label added alone is not.
func legionBeforeAndAfterWrites(t *testing.T, rig *legionStatusRig) (writes []legionWrite, labelled string) {
	t.Helper()
	labelled = rig.issue(t, []string{"legion"}, "", "")
	child := rig.issue(t, nil, labelled, "")
	onARoot := func(body map[string]any, written ...legionCheck) func(*testing.T, *legionStatusRig) legionPatch {
		return func(t *testing.T, rig *legionStatusRig) legionPatch {
			return legionPatch{key: rig.issue(t, nil, "", ""), body: body, written: written}
		}
	}
	return []legionWrite{
		{name: "a status write that adds the label", judged: true, make: onARoot(
			map[string]any{"status": "todo", "labels_add": []string{"legion"}}, hasStatus("todo"), hasLabels("legion"))},
		{name: "a status write whose labels hold the label", judged: true, make: onARoot(
			map[string]any{"status": "todo", "labels": []string{"legion"}}, hasStatus("todo"), hasLabels("legion"))},
		{name: "a status write that moves the issue under a labelled issue", judged: true, make: onARoot(
			map[string]any{"status": "todo", "parent": labelled}, hasStatus("todo"), hasParent(labelled))},
		{name: "a status write that moves the issue under an unlabelled child of a labelled issue", judged: true, make: onARoot(
			map[string]any{"status": "todo", "parent": child}, hasStatus("todo"), hasParent(child))},
		{name: "the label added alone", judged: false, make: onARoot(
			map[string]any{"labels_add": []string{"legion"}}, hasLabels("legion"))},
	}, labelled
}

// legionParentFixture is the issues test 4 moves an issue between.
type legionParentFixture struct {
	// labelled is a labelled root, and middle an unlabelled child of it: each moved issue is made
	// under middle, two levels under labelled.
	labelled, middle string
	// unlabelled is a root under no labelled issue.
	unlabelled string
	// other is another labelled root, and otherChild an unlabelled child of it.
	other, otherChild string
}

// legionParentStatuses are the three open statuses test 4 moves an issue at.
var legionParentStatuses = []string{"triage", "backlog", "in_progress"}

// legionParentMoves are test 4's moves of an unlabelled issue two levels under a labelled root.
// R26 judges a move out from under every labelled issue, under an unlabelled root or to no
// parent, at each open status. It does not judge a move under another labelled root or under an
// unlabelled child of one.
func legionParentMoves(t *testing.T, rig *legionStatusRig) ([]legionWrite, legionParentFixture) {
	t.Helper()
	fixture := legionParentFixture{labelled: rig.issue(t, []string{"legion"}, "", "")}
	fixture.middle = rig.issue(t, nil, fixture.labelled, "")
	fixture.unlabelled = rig.issue(t, nil, "", "")
	fixture.other = rig.issue(t, []string{"legion"}, "", "")
	fixture.otherChild = rig.issue(t, nil, fixture.other, "")
	move := func(status string, parent any, check legionCheck) func(*testing.T, *legionStatusRig) legionPatch {
		return func(t *testing.T, rig *legionStatusRig) legionPatch {
			return legionPatch{
				key:     rig.issue(t, nil, fixture.middle, status),
				body:    map[string]any{"parent": parent},
				written: []legionCheck{check, hasStatus(status)},
			}
		}
	}
	var writes []legionWrite
	for _, status := range legionParentStatuses {
		writes = append(writes,
			legionWrite{name: "under an unlabelled root at " + status, judged: true, make: move(status, fixture.unlabelled, hasParent(fixture.unlabelled))},
			legionWrite{name: "to no parent at " + status, judged: true, make: move(status, nil, hasParent(""))},
		)
	}
	writes = append(writes,
		legionWrite{name: "under another labelled root", judged: false, make: move(model.IssueStatusTriage, fixture.other, hasParent(fixture.other))},
		legionWrite{name: "under an unlabelled child of another labelled root", judged: false, make: move(model.IssueStatusTriage, fixture.otherChild, hasParent(fixture.otherChild))},
	)
	return writes, fixture
}

// legionLabelRemovalUnderALabelledParent is test 5's write: a labelled issue under a labelled
// parent loses its own label. It stays a Legion issue through its parent, and R26 judges the
// removal all the same.
func legionLabelRemovalUnderALabelledParent(t *testing.T, rig *legionStatusRig) legionWrite {
	t.Helper()
	parent := rig.issue(t, []string{"legion"}, "", "")
	return legionWrite{name: "the label's removal under a labelled parent", judged: true, make: func(t *testing.T, rig *legionStatusRig) legionPatch {
		return legionPatch{
			key:     rig.issue(t, []string{"legion"}, parent, ""),
			body:    map[string]any{"labels": []string{}},
			written: []legionCheck{hasLabels()},
		}
	}}
}

// legionDeepStatusWrites are test 11's judged writes: a status write on an issue 1, 16 and
// parentDepthCap links under a labelled root, and on an issue under a root labelled Legion. It
// answers the issue parentDepthCap links under the labelled root too.
func legionDeepStatusWrites(t *testing.T, rig *legionStatusRig) (writes []legionWrite, deepest string) {
	t.Helper()
	chain := createChain(t, rig.handler, brokerTestProject, rig.issue(t, []string{"legion"}, "", ""), parentDepthCap)
	for _, depth := range []int{1, 16, parentDepthCap} {
		key := chain[depth-1]
		writes = append(writes, legionWrite{name: fmt.Sprintf("%d links under a labelled root", depth), judged: true,
			make: func(*testing.T, *legionStatusRig) legionPatch { return statusWrite(key, "todo") }})
	}
	underCapital := rig.issue(t, nil, rig.issue(t, []string{"Legion"}, "", ""), "")
	writes = append(writes, legionWrite{name: "under a root labelled Legion", judged: true,
		make: func(*testing.T, *legionStatusRig) legionPatch { return statusWrite(underCapital, "todo") }})
	return writes, chain[parentDepthCap-1]
}

// legionStatusChange is one call of the status-change route. make makes, as a person, the issues
// the call needs, and answers the call. judged says whether R26 judges the call: a step of the
// call writes the status of a Legion issue, or moves a Legion issue out from under every
// labelled issue (sendStatusChange).
type legionStatusChange struct {
	name   string
	judged bool
	make   func(t *testing.T, rig *legionStatusRig) legionStatusChangeCall
}

// legionStatusChangeCall is a status change of key with body, every issue that the call can
// change, and the checks of what the call leaves on each issue when the server takes it. A dry
// run writes nothing, so its checks are not read.
type legionStatusChangeCall struct {
	key     string
	body    map[string]any
	issues  []string
	written map[string][]legionCheck
}

// legionCallState is what a status change can change on its issues: the state of each issue, and
// the number of events and of comments on the issues.
type legionCallState struct {
	issues           map[string]legionIssueState
	events, comments int64
}

func readLegionCall(t *testing.T, database *store.Store, keys []string) legionCallState {
	t.Helper()
	state := legionCallState{issues: make(map[string]legionIssueState, len(keys))}
	for _, key := range keys {
		state.issues[key] = readLegionIssue(t, database, key)
	}
	state.events = countRows(t, database, `select count(*) from events where issue_key = any($1)`, keys)
	state.comments = countRows(t, database, `select count(*) from comments where issue_key = any($1)`, keys)
	return state
}

// requireLegionCallUnchanged fails unless each issue of keys is as before, and the issues have as
// many events and as many comments as before.
func requireLegionCallUnchanged(t *testing.T, database *store.Store, keys []string, before legionCallState) {
	t.Helper()
	after := readLegionCall(t, database, keys)
	for _, key := range keys {
		if !after.issues[key].equal(before.issues[key]) {
			t.Errorf("%s after the call: %s; want it as it was: %s", key, after.issues[key], before.issues[key])
		}
	}
	if after.events != before.events || after.comments != before.comments {
		t.Errorf("the issues %v have %d events and %d comments after the call, want %d and %d",
			keys, after.events, after.comments, before.events, before.comments)
	}
}

// sendStatusChange sends change from credential, which must reach the handler. With the setting
// on, R26 refuses a judged call from a legionRefused credential: the answer is R26's refusal, its
// changed is empty, and no issue of the call changes (requireLegionCallUnchanged). The server
// takes every other call, 200. A dry run that the server takes changes nothing. Any other call
// that the server takes leaves each check of written true.
func (rig *legionStatusRig) sendStatusChange(t *testing.T, credential legionCredential, change legionStatusChange) {
	t.Helper()
	call := change.make(t, rig)
	before := readLegionCall(t, rig.database, call.issues)
	response := credential.send(t, http.MethodPost, "/api/v1/issues/"+call.key+"/status-change", call.body)
	if change.judged && rig.deps.LegionStatusScope && credential.verdict == legionRefused {
		requireLegionStatusRefusal(t, response)
		if changed := decodeBody[statusChangeRefusal](t, response).Changed; len(changed) != 0 {
			t.Errorf("the refused call changed %+v, want nothing", changed)
		}
		requireLegionCallUnchanged(t, rig.database, call.issues, before)
		return
	}
	if response.Code != http.StatusOK {
		t.Errorf("status=%d body=%s, want 200: the server takes this call", response.Code, response.Body.String())
		return
	}
	if call.body["dry_run"] == true {
		requireLegionCallUnchanged(t, rig.database, call.issues, before)
		return
	}
	for _, key := range call.issues {
		after := readLegionIssue(t, rig.database, key)
		for _, check := range call.written[key] {
			check(t, after)
		}
	}
}

// legionCascade is test 7's cascade: an unlabelled root moved to backlog with include_children,
// over a child that carries the label and is in work. So the call writes the status of the
// child, a Legion issue. With dryRun, it is the dry run of that call.
func legionCascade(dryRun bool) legionStatusChange {
	name := "a cascade over a labelled child in work"
	if dryRun {
		name = "the dry run of " + name
	}
	return legionStatusChange{name: name, judged: true, make: func(t *testing.T, rig *legionStatusRig) legionStatusChangeCall {
		root := rig.issue(t, nil, "", "")
		child := rig.issue(t, []string{"legion"}, root, "in_progress")
		body := map[string]any{"status": "backlog", "include_children": true}
		if dryRun {
			body["dry_run"] = true
		}
		return legionStatusChangeCall{key: root, body: body, issues: []string{root, child}, written: map[string][]legionCheck{
			root:  {hasStatus("backlog")},
			child: {hasStatus("backlog"), hasLabels("legion"), hasParent(root)},
		}}
	}}
}

// legionStatusChanges are test 7's judged calls of the status-change route: the cascade and its
// dry run (legionCascade); include_parent on an unlabelled child of a labelled issue, which
// writes the status of the labelled issue and of its child; unlink_children of a labelled root,
// which moves its unlabelled child out from under every labelled issue; and a status change with
// no option on a labelled issue.
func legionStatusChanges() []legionStatusChange {
	return []legionStatusChange{
		legionCascade(false),
		legionCascade(true),
		{name: "include_parent on an unlabelled child of a labelled issue", judged: true, make: func(t *testing.T, rig *legionStatusRig) legionStatusChangeCall {
			parent := rig.issue(t, []string{"legion"}, "", "backlog")
			child := rig.issue(t, nil, parent, "")
			return legionStatusChangeCall{
				key: child, body: map[string]any{"status": "in_progress", "include_parent": true}, issues: []string{parent, child},
				written: map[string][]legionCheck{
					parent: {hasStatus("in_progress")},
					child:  {hasStatus("in_progress"), hasParent(parent)},
				},
			}
		}},
		{name: "unlink_children on a labelled root", judged: true, make: func(t *testing.T, rig *legionStatusRig) legionStatusChangeCall {
			root := rig.issue(t, []string{"legion"}, "", "")
			child := rig.issue(t, nil, root, "")
			return legionStatusChangeCall{
				key: root, body: map[string]any{"status": "backlog", "unlink_children": true}, issues: []string{root, child},
				written: map[string][]legionCheck{
					root:  {hasStatus("backlog")},
					child: {hasParent(""), hasStatus(model.IssueStatusTriage)},
				},
			}
		}},
		{name: "a status change with no option on a labelled issue", judged: true, make: func(t *testing.T, rig *legionStatusRig) legionStatusChangeCall {
			key := rig.issue(t, []string{"legion"}, "", "")
			return legionStatusChangeCall{
				key: key, body: map[string]any{"status": "todo"}, issues: []string{key},
				written: map[string][]legionCheck{key: {hasStatus("todo")}},
			}
		}},
	}
}

// TestALegionIssueStatusNeedsThePersonOrLegionStatusScope: a status write on an issue that carries
// the legion label is refused from every credential that holds neither scope, a write of the open
// status the issue holds already too, and taken from every credential that holds one. A status
// write on an issue under no labelled issue is taken from every credential that reaches the
// handler. The two credentials a check in front of the handler refuses keep their own codes. The
// daemon's hand-over, a status with the label added, is taken from the daemon's broker token and
// refused from the shared token, though its body names the daemon's session.
func TestALegionIssueStatusNeedsThePersonOrLegionStatusScope(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	rig.sendFromEach(t, append(legionStatusWrites(), legionWrite{
		name: "a status write on an issue under no labelled issue", judged: false,
		make: func(t *testing.T, rig *legionStatusRig) legionPatch {
			return statusWrite(rig.issue(t, nil, "", ""), "todo")
		},
	}))
	handOver := legionHandOver()
	t.Run(handOver.name, func(t *testing.T) {
		for _, name := range []string{legionDaemonToken, legionSharedToken} {
			credential := rig.credential(t, name)
			t.Run(name, func(t *testing.T) { rig.send(t, credential, handOver) })
		}
	})
}

// TestTakingTheLegionLabelOffNeedsTheScopeToo: labels that leave out legion, in either case, are
// refused from every credential that holds neither scope and taken from every credential that
// holds one. A write that keeps the label is taken from every credential that reaches the
// handler: Legion written again as legion, a change of another label, and labels_add of a label.
func TestTakingTheLegionLabelOffNeedsTheScopeToo(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	labelsWrite := func(labels []string, body map[string]any, written ...string) func(*testing.T, *legionStatusRig) legionPatch {
		return func(t *testing.T, rig *legionStatusRig) legionPatch {
			return legionPatch{key: rig.issue(t, labels, "", ""), body: body, written: []legionCheck{hasLabels(written...)}}
		}
	}
	rig.sendFromEach(t, append(legionLabelRemovals(),
		legionWrite{name: "Legion written again as legion", judged: false,
			make: labelsWrite([]string{"Legion"}, map[string]any{"labels": []string{"legion"}}, "legion")},
		legionWrite{name: "labels that keep legion and change another label", judged: false,
			make: labelsWrite([]string{"legion", "area"}, map[string]any{"labels": []string{"legion", "ops"}}, "legion", "ops")},
		legionWrite{name: "labels_add of another label", judged: false,
			make: labelsWrite([]string{"legion"}, map[string]any{"labels_add": []string{"ops"}}, "legion", "ops")},
	))
}

// TestACallIsJudgedByTheLabelsAndChainBeforeAndAfterIt: on an unlabelled root, a status write is
// judged by what the same call leaves too. With labels_add of legion, with labels that hold
// legion, and with a parent that is a labelled issue or an unlabelled child of one, it is refused
// from every credential that holds neither scope, and nothing changes. The label added alone is
// taken, and so is an issue made under a labelled issue.
func TestACallIsJudgedByTheLabelsAndChainBeforeAndAfterIt(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	writes, labelled := legionBeforeAndAfterWrites(t, rig)
	rig.sendFromEach(t, writes)
	t.Run("an issue made under a labelled issue", func(t *testing.T) {
		for _, credential := range rig.credentials {
			t.Run(credential.name, func(t *testing.T) {
				children := func() int64 {
					return countRows(t, rig.database, `select count(*) from issues where parent_key = $1`, labelled)
				}
				before := children()
				made := credential.send(t, http.MethodPost, "/api/v1/issues", map[string]any{
					"project": brokerTestProject, "title": rig.title(), "parent": labelled, "force": true,
				})
				if credential.verdict == legionRefusedFirst {
					requireRefused(t, made, http.StatusForbidden, credential.code)
					if after := children(); after != before {
						t.Errorf("%s has %d children after the refused create, want %d", labelled, after, before)
					}
					return
				}
				if made.Code != http.StatusCreated {
					t.Fatalf("make an issue under %s: status=%d body=%s, want 201", labelled, made.Code, made.Body.String())
				}
				hasParent(labelled)(t, readLegionIssue(t, rig.database, decodeBody[model.Issue](t, made).Key))
			})
		}
	})
}

// TestAParentChangeThatLeavesNoLabelledAncestorNeedsTheScope: an unlabelled issue two levels
// under a labelled root, moved under an unlabelled root or to no parent, is refused from every
// credential that holds neither scope at each of three open statuses, and taken from every
// credential that holds one. At done, the same moves are ISSUE_CLOSED and change nothing, since
// a closed issue takes no parent. After a refused move, a status write on the issue is refused
// too: the issue is still a Legion issue, so two calls cannot do what one may not. A move under
// another labelled root, or under an unlabelled child of one, is taken. A move of an issue under
// the issue below it is PARENT_INPUT, which comes before R26, and changes nothing.
func TestAParentChangeThatLeavesNoLabelledAncestorNeedsTheScope(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	writes, fixture := legionParentMoves(t, rig)
	rig.sendFromEach(t, writes)

	t.Run("a move of a closed issue", func(t *testing.T) {
		for _, credential := range rig.credentials {
			t.Run(credential.name, func(t *testing.T) {
				for _, parent := range []any{fixture.unlabelled, nil} {
					key := rig.issue(t, nil, fixture.middle, "done")
					before := readLegionIssue(t, rig.database, key)
					response := credential.send(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"parent": parent})
					if credential.verdict == legionRefusedFirst {
						requireRefused(t, response, http.StatusForbidden, credential.code)
					} else {
						requireRefused(t, response, http.StatusConflict, "ISSUE_CLOSED")
					}
					requireLegionIssueUnchanged(t, rig.database, key, before)
				}
			})
		}
	})

	t.Run("a status write after the refused move", func(t *testing.T) {
		for _, credential := range rig.credentials {
			if credential.verdict != legionRefused {
				continue
			}
			t.Run(credential.name, func(t *testing.T) {
				key := rig.issue(t, nil, fixture.middle, "backlog")
				before := readLegionIssue(t, rig.database, key)
				moved := credential.send(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"parent": fixture.unlabelled})
				rig.answered(t, credential, true, key, before, moved)
				written := credential.send(t, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"status": "todo"})
				rig.answered(t, credential, true, key, before, written)
			})
		}
	})

	t.Run("a move under an issue below it", func(t *testing.T) {
		above := rig.issue(t, nil, fixture.labelled, "")
		below := rig.issue(t, nil, above, "")
		for _, credential := range rig.credentials {
			t.Run(credential.name, func(t *testing.T) {
				before, belowBefore := readLegionIssue(t, rig.database, above), readLegionIssue(t, rig.database, below)
				response := credential.send(t, http.MethodPatch, "/api/v1/issues/"+above, map[string]any{"parent": below})
				if credential.verdict == legionRefusedFirst {
					requireRefused(t, response, http.StatusForbidden, credential.code)
				} else {
					requireRefused(t, response, http.StatusConflict, "PARENT_INPUT")
				}
				requireLegionIssueUnchanged(t, rig.database, above, before)
				requireLegionIssueUnchanged(t, rig.database, below, belowBefore)
			})
		}
	})
}

// TestTheLabelsRemovalNeedsTheScopeUnderALabelledParentToo: a labelled issue under a labelled
// parent stays a Legion issue without its own label, and the removal of that label is still
// refused from every credential that holds neither scope, and taken from every credential that
// holds one.
func TestTheLabelsRemovalNeedsTheScopeUnderALabelledParentToo(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	rig.sendFromEach(t, []legionWrite{legionLabelRemovalUnderALabelledParent(t, rig)})
}

// TestALabelledIssueLeavesItsLabelledParentWithoutTheScope: an issue that carries the label stays
// a Legion issue wherever it moves, so the shared token, which holds neither scope, moves it from
// under a labelled parent to no parent, or under an unlabelled root, and its labels and status
// stay as they were.
func TestALabelledIssueLeavesItsLabelledParentWithoutTheScope(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	shared := rig.credential(t, legionSharedToken)
	parent := rig.issue(t, []string{"legion"}, "", "")
	unlabelled := rig.issue(t, nil, "", "")
	for _, move := range []struct {
		name   string
		parent any
		want   string
	}{
		{"to no parent", nil, ""},
		{"under an unlabelled root", unlabelled, unlabelled},
	} {
		t.Run(move.name, func(t *testing.T) {
			rig.send(t, shared, legionWrite{name: move.name, judged: false, make: func(t *testing.T, rig *legionStatusRig) legionPatch {
				return legionPatch{
					key:     rig.issue(t, []string{"legion"}, parent, "backlog"),
					body:    map[string]any{"parent": move.parent},
					written: []legionCheck{hasParent(move.want), hasLabels("legion"), hasStatus("backlog")},
				}
			}})
		})
	}
}

// TestACascadeThatWritesALegionIssueIsRefusedWhole: R26 judges every issue that a call of the
// status-change route would change, before the call's first write. From the shared token, which
// holds neither scope, it refuses each judged call whole (legionStatusChanges): no issue of the
// call changes its status, labels, parent or last_seq, and no issue of the call gets an event or
// a comment. A labelled child that is in the call's status group already is not changed, so the
// cascade over it is taken. With DISPATCH_PARENT_STATUS_RULE on too, done on a labelled parent
// whose child is in work answers R26's code, not PARENT_STATUS_CONFLICT. A browser sign-in makes
// the cascade over a labelled child in work.
func TestACascadeThatWritesALegionIssueIsRefusedWhole(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	shared := rig.credential(t, legionSharedToken)
	for _, change := range legionStatusChanges() {
		t.Run(change.name, func(t *testing.T) { rig.sendStatusChange(t, shared, change) })
	}

	t.Run("a cascade over a labelled child in the status's group already", func(t *testing.T) {
		rig.sendStatusChange(t, shared, legionStatusChange{judged: false, make: func(t *testing.T, rig *legionStatusRig) legionStatusChangeCall {
			root := rig.issue(t, nil, "", "")
			child := rig.issue(t, []string{"legion"}, root, "icebox")
			kept := readLegionIssue(t, rig.database, child)
			return legionStatusChangeCall{
				key: root, body: map[string]any{"status": "backlog", "include_children": true}, issues: []string{root, child},
				written: map[string][]legionCheck{
					root: {hasStatus("backlog")},
					child: {func(t *testing.T, after legionIssueState) {
						t.Helper()
						if !after.equal(kept) {
							t.Errorf("the cascade changed %s to %s, want it as it was: %s", child, after, kept)
						}
					}},
				},
			}
		}})
	})

	t.Run("done on a labelled parent whose child is in work, with the parent-status rule on", func(t *testing.T) {
		ruled := newLegionStatusRigOn(t, testServerOptions{legionStatusScope: true, parentStatusRule: true})
		parent := ruled.issue(t, []string{"legion"}, "", "in_progress")
		child := ruled.issue(t, nil, parent, "in_progress")
		ruled.sendStatusChange(t, ruled.credential(t, legionSharedToken), legionStatusChange{judged: true,
			make: func(*testing.T, *legionStatusRig) legionStatusChangeCall {
				return legionStatusChangeCall{key: parent, body: map[string]any{"status": "done"}, issues: []string{parent, child}}
			}})
		// The control: R26 lets a browser sign-in through, and R24 refuses the same call. So the
		// rule is on, and R26's code came first.
		closed := ruled.credential(t, legionBrowserSignIn).send(t, http.MethodPost, "/api/v1/issues/"+parent+"/status-change",
			map[string]any{"status": "done"})
		requireRefused(t, closed, http.StatusConflict, "PARENT_STATUS_CONFLICT")
	})

	t.Run("the cascade from a browser sign-in", func(t *testing.T) {
		rig.sendStatusChange(t, rig.credential(t, legionBrowserSignIn), legionCascade(false))
	})
}

// TestNoMintHoldsLegionStatus: no credential mints a personal token that holds legion-status,
// alone or beside person, and no edit gives it to one, and no such call changes a token. A
// credential that may mint (a browser sign-in, a personal token holding approve) is refused 400
// TOKEN_SCOPES_INPUT. A broker token scoped to a person holds person alone and never approve
// (brokerTokenActor), so the mint route's own scope refuses it first, 403 OAUTH_SCOPE_REQUIRED.
// The table's own check refuses a row that holds it, so a write that passes the API cannot store
// one either.
func TestNoMintHoldsLegionStatus(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	personal := personalTokenHolding(t, rig.database, model.ScopePerson, model.ScopeApprove)
	signIn, personScoped := rig.credential(t, legionBrowserSignIn), rig.credential(t, legionPersonScopedToken)
	minters := []struct {
		name string
		send func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder
		// status and code are the refusal the minter gets for each mint and edit.
		status int
		code   string
	}{
		{signIn.name, signIn.send, http.StatusBadRequest, codeTokenScopesInput},
		{"a personal token holding person and approve", func(t *testing.T, method, path string, body map[string]any) *httptest.ResponseRecorder {
			return rig.serve(brokerRequest(t, method, path, body, "Bearer "+personal, ""))
		}, http.StatusBadRequest, codeTokenScopesInput},
		{personScoped.name, personScoped.send, http.StatusForbidden, codeScopeRequired},
	}
	tokens := func(t *testing.T) int64 {
		t.Helper()
		return countRows(t, rig.database, `select count(*) from agent_tokens where owner = $1`, brokerTestPerson)
	}
	minted := rig.asPerson(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "held"})
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint a token as a person: status=%d body=%s", minted.Code, minted.Body.String())
	}
	held := decodeBody[agentTokenResponse](t, minted).ID
	heldScopes := func(t *testing.T) []string {
		t.Helper()
		var scopes []string
		if err := rig.database.Pool.QueryRow(context.Background(),
			`select scopes from agent_tokens where id = $1`, held).Scan(&scopes); err != nil {
			t.Fatalf("read the scopes of token %s: %v", held, err)
		}
		return scopes
	}
	for _, minter := range minters {
		t.Run(minter.name, func(t *testing.T) {
			for _, scopes := range [][]string{{model.ScopeLegionStatus}, {model.ScopePerson, model.ScopeLegionStatus}} {
				before := tokens(t)
				response := minter.send(t, http.MethodPost, "/api/v1/me/agent-tokens", map[string]any{"name": "probe", "scopes": scopes})
				requireRefused(t, response, minter.status, minter.code)
				if after := tokens(t); after != before {
					t.Errorf("a mint of %q: the person holds %d tokens after it, want %d", scopes, after, before)
				}
			}
			before := heldScopes(t)
			edited := minter.send(t, http.MethodPatch, "/api/v1/me/agent-tokens/"+held, map[string]any{
				"scopes": []string{model.ScopePerson, model.ScopeLegionStatus},
			})
			requireRefused(t, edited, minter.status, minter.code)
			if after := heldScopes(t); !slices.Equal(after, before) {
				t.Errorf("an edit to person and legion-status left the token holding %q, want %q", after, before)
			}
		})
	}
	t.Run("a row written straight into the table", func(t *testing.T) {
		for _, scopes := range [][]string{{model.ScopeLegionStatus}, {model.ScopePerson, model.ScopeLegionStatus}} {
			plaintext, hash, err := newAgentToken()
			if err != nil {
				t.Fatalf("make a token: %v", err)
			}
			_, err = rig.database.Pool.Exec(context.Background(), `
				insert into agent_tokens (owner, name, token_hash, prefix, scopes)
				values ($1, $2, $3, $4, $5)
			`, brokerTestPerson, "straight", hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength], scopes)
			var refusal *pgconn.PgError
			if !errors.As(err, &refusal) || refusal.Code != "23514" || refusal.ConstraintName != "agent_tokens_scopes_check" {
				t.Errorf("an insert of a token holding %q: %v, want SQLSTATE 23514 naming agent_tokens_scopes_check", scopes, err)
			}
		}
	})
}

// TestTheFixIsNotRefusedWithBothSettingsOn: R25's boot fix writes as Dispatch's own system
// writer, and R26 never refuses that writer. So the fix runs with DISPATCH_LEGION_STATUS_SCOPE
// and DISPATCH_PARENT_STATUS_RULE both on. A person makes two pairs that break R24 while the rule
// is off: a labelled parent at done over an unlabelled child in todo, and a labelled parent on
// the backlog over an unlabelled child in work. The fix unlinks the first child, which keeps its
// status, and moves the second parent to in_progress. The event of each change and each comment
// of the fix name the actor parent-status-rule. Before the fix, the shared token sends the same
// unlink as a PATCH, and R26 refuses it. So the guard was on for the deps that the fix runs with.
func TestTheFixIsNotRefusedWithBothSettingsOn(t *testing.T) {
	// The rule is off for the setup, because a person cannot make a pair that breaks the rule
	// while it is on.
	rig := newLegionStatusRig(t, true)
	if !rig.deps.LegionStatusScope || rig.deps.ParentStatusRule.On() {
		t.Fatalf("the server runs with LegionStatusScope=%t and the parent-status rule on=%t, want R26 on and the rule off",
			rig.deps.LegionStatusScope, rig.deps.ParentStatusRule.On())
	}
	done := rig.issue(t, []string{"legion"}, "", "")
	open := rig.issue(t, nil, done, "todo")
	if closed := rig.asPerson(t, http.MethodPatch, "/api/v1/issues/"+done, map[string]any{"status": "done"}); closed.Code != http.StatusOK {
		t.Fatalf("move %s to done as a person: status=%d body=%s", done, closed.Code, closed.Body.String())
	}
	notStarted := rig.issue(t, []string{"legion"}, "", "backlog")
	inWork := rig.issue(t, nil, notStarted, "in_progress")

	// The shared token sends the unlink that the fix makes. R26 refuses it, and the child keeps
	// its parent. So the guard is on for these deps.
	before := readLegionIssue(t, rig.database, open)
	refused := rig.credential(t, legionSharedToken).send(t, http.MethodPatch, "/api/v1/issues/"+open, map[string]any{"parent": nil})
	requireLegionStatusRefusal(t, refused)
	requireLegionIssueUnchanged(t, rig.database, open, before)

	fix := fixRig{ruleRig: ruleRig{database: rig.database, deps: rig.deps}}
	fix.fix(t)

	unlinked := readLegionIssue(t, rig.database, open)
	hasParent("")(t, unlinked)
	hasStatus("todo")(t, unlinked)
	hasStatus("in_progress")(t, readLegionIssue(t, rig.database, notStarted))
	for field, want := range map[string][]string{"parent": {open}, "status": {notStarted}} {
		if got := fix.fixWrites(t, field); !slices.Equal(got, want) {
			t.Errorf("the issue.updated events of parent-status-rule that changed %s are on %q, want %q", field, got, want)
		}
	}
	requireComments(t, rig.database, []string{done, open, notStarted, inWork},
		fixUnlinkComment(fixUnlink{key: open, parent: done}),
		fixMoveComment(fixMove{key: notStarted, from: "backlog", inWork: inWork}))
}

// TestAnIssueUnderALabelledIssueIsALegionIssue: R26 reads the whole chain above an issue. A status
// write from the shared token is refused on an issue 1, 16 and parentDepthCap links under a
// labelled root, and on an issue under a root labelled Legion, and taken on the same issues of an
// unlabelled chain. On the issue parentDepthCap links under the labelled root, the same write is
// taken from every credential that holds a scope. The negative control of the plan's proof reads
// this test: a guard that read the issue's own labels alone fails it.
func TestAnIssueUnderALabelledIssueIsALegionIssue(t *testing.T) {
	rig := newLegionStatusRig(t, true)
	shared := rig.credential(t, legionSharedToken)
	writes, deepest := legionDeepStatusWrites(t, rig)
	for _, write := range writes {
		t.Run(write.name, func(t *testing.T) { rig.send(t, shared, write) })
	}
	unlabelled := createChain(t, rig.handler, brokerTestProject, rig.issue(t, nil, "", ""), parentDepthCap)
	for _, depth := range []int{1, 16, parentDepthCap} {
		key := unlabelled[depth-1]
		t.Run(fmt.Sprintf("%d links under an unlabelled root", depth), func(t *testing.T) {
			rig.send(t, shared, legionWrite{judged: false, make: func(*testing.T, *legionStatusRig) legionPatch { return statusWrite(key, "todo") }})
		})
	}
	t.Run(fmt.Sprintf("%d links under a labelled root, from a credential that holds a scope", parentDepthCap), func(t *testing.T) {
		statuses := []string{"backlog", "icebox", "in_progress", "todo"}
		taken := 0
		for _, credential := range rig.credentials {
			if credential.verdict != legionTaken {
				continue
			}
			status := statuses[taken%len(statuses)]
			taken++
			t.Run(credential.name, func(t *testing.T) {
				rig.send(t, credential, legionWrite{judged: true, make: func(*testing.T, *legionStatusRig) legionPatch { return statusWrite(deepest, status) }})
			})
		}
	})
}

// TestTheLegionStatusRefusalWaitsForItsSetting: with Deps.LegionStatusScope off, every write that
// tests 1 to 5 and 11 send from the shared token, and that R26 refuses with the setting on, is
// taken. So is every call of the status-change route that test 7 sends from the shared token and
// that R26 refuses with the setting on (legionStatusChanges), with DISPATCH_PARENT_STATUS_RULE off
// too. The dry run among them is taken and writes nothing.
func TestTheLegionStatusRefusalWaitsForItsSetting(t *testing.T) {
	rig := newLegionStatusRig(t, false)
	shared := rig.credential(t, legionSharedToken)
	beforeAndAfter, _ := legionBeforeAndAfterWrites(t, rig)
	moves, _ := legionParentMoves(t, rig)
	deep, _ := legionDeepStatusWrites(t, rig)
	writes := slices.Concat(
		legionStatusWrites(),
		[]legionWrite{legionHandOver()},
		legionLabelRemovals(),
		beforeAndAfter,
		moves,
		[]legionWrite{legionLabelRemovalUnderALabelledParent(t, rig)},
		deep,
	)
	judged := 0
	for _, write := range writes {
		if !write.judged {
			continue
		}
		judged++
		t.Run(write.name, func(t *testing.T) { rig.send(t, shared, write) })
	}
	for _, change := range legionStatusChanges() {
		judged++
		t.Run(change.name, func(t *testing.T) { rig.sendStatusChange(t, shared, change) })
	}
	if judged == 0 {
		t.Fatal("no write R26 judges was sent, so the setting's absence proved nothing")
	}
}
