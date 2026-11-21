package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// crossProjectRaceRounds is how many times each concurrent scenario below is replayed. The
// writers race each other once the lock they meet on lets them through, so one round can miss
// the interleaving that matters; each round clears the ring and lays its committed edges again
// first.
const crossProjectRaceRounds = 8

// dependencyEdge is one `blocked_by` link, written as the issue that waits and the issue it
// waits on.
type dependencyEdge struct{ issue, target string }

// TestBlockedByCrossesAProjectAndStillRefusesACycle: a `blocked_by` target may be an issue of
// another project, so an issue that waits on work tracked elsewhere can record that wait
// (INFRA-3036). Every refusal that stood before stands across projects too — a ring, a child
// waiting on its parent, a parent waiting on its descendant and an issue waiting on itself —
// and concurrent writers that would close a ring between projects meet on the deployment-wide
// dependency lock every dependency write takes exclusively, so each ends with the write applied
// or with the cycle refusal, never with a deadlock and never with the ring committed — the pair
// whose every target is its own project's included, which shares no project lock with the other
// and has nothing but that one lock to order it. A caller whose credential names one project is
// still refused a target outside it.
func TestBlockedByCrossesAProjectAndStillRefusesACycle(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	alpha := createListedIssues(t, handler, "ALPHA", 8)
	beta := createListedIssues(t, handler, "BETA", 8)
	gamma := createListedIssues(t, handler, "GAMMA", 2)

	patchBlockers := func(key string, targets []string) *httptest.ResponseRecorder {
		t.Helper()
		if targets == nil {
			targets = []string{}
		}
		return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
			"blocked_by": targets,
		}, "alice")
	}
	setBlockers := func(what, key string, targets []string) {
		t.Helper()
		if response := patchBlockers(key, targets); response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", what, response.Code, response.Body.String())
		}
	}
	blockedBy := func(key string) []string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response).BlockedBy
	}
	eventNames := func(key, eventType, blocker string) bool {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read the events of %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		var log []struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.NewDecoder(response.Body).Decode(&log); err != nil {
			t.Fatalf("decode the events of %s: %v", key, err)
		}
		for _, event := range log {
			if event.Type != eventType {
				continue
			}
			var payload issueEventFields
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode a %s payload of %s: %v", eventType, key, err)
			}
			for _, reference := range payload.ReferencesChanged {
				if reference.Kind == "issue" && reference.ID == blocker {
					return true
				}
			}
		}
		return false
	}
	expectRefusal := func(what string, response *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("%s: status=%d body=%s, want %d %s", what, response.Code, response.Body.String(), status, code)
		}
	}

	// A link into another project round-trips: it is read back on the issue, named on the event
	// the write appended, cleared again, and made at creation as well as by a patch.
	setBlockers("wait on another project's issue", alpha[0], []string{beta[0]})
	if got := blockedBy(alpha[0]); !slices.Equal(got, []string{beta[0]}) {
		t.Fatalf("blocked_by of %s = %v, want %v", alpha[0], got, []string{beta[0]})
	}
	if !eventNames(alpha[0], "issue.updated", beta[0]) {
		t.Fatalf("no issue.updated of %s names the blocker %s it added", alpha[0], beta[0])
	}
	setBlockers("clear a wait on another project's issue", alpha[0], nil)
	if got := blockedBy(alpha[0]); len(got) != 0 {
		t.Fatalf("cleared blocked_by of %s = %v, want []", alpha[0], got)
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ALPHA", "title": "Created waiting on another project",
		"blocked_by": []string{beta[1]}, "force": true,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create waiting on another project: status=%d body=%s", created.Code, created.Body.String())
	}
	if got := decodeBody[model.Issue](t, created).BlockedBy; !slices.Equal(got, []string{beta[1]}) {
		t.Fatalf("created blocked_by = %v, want %v", got, []string{beta[1]})
	}

	// What stays refused. The cycle walk is project-blind, so a ring of two cross-project links
	// is refused exactly as one inside a project is. The parent rules are read inside one project
	// here; TestParentCrossesAProjectAndStillRefusesACycle reads them across two.
	setBlockers("the first half of a cross-project ring", alpha[1], []string{beta[2]})
	expectRefusal("a ring of two cross-project links", patchBlockers(beta[2], []string{alpha[1]}),
		http.StatusConflict, "DEPENDENCY_CYCLE")

	parent := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ALPHA", "title": "Parent of a waiting child", "force": true,
	}, "alice")
	if parent.Code != http.StatusCreated {
		t.Fatalf("create a parent: status=%d body=%s", parent.Code, parent.Body.String())
	}
	parentKey := decodeBody[model.Issue](t, parent).Key
	child := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ALPHA", "title": "Child of a waiting parent", "parent": parentKey, "force": true,
	}, "alice")
	if child.Code != http.StatusCreated {
		t.Fatalf("create a child: status=%d body=%s", child.Code, child.Body.String())
	}
	childKey := decodeBody[model.Issue](t, child).Key
	expectRefusal("a child waiting on its parent", patchBlockers(childKey, []string{parentKey}),
		http.StatusConflict, "DEPENDENCY_CYCLE")
	expectRefusal("a parent waiting on its descendant", patchBlockers(parentKey, []string{childKey}),
		http.StatusConflict, "DEPENDENCY_CYCLE")
	expectRefusal("an issue waiting on itself", patchBlockers(alpha[2], []string{alpha[2]}),
		http.StatusBadRequest, "BLOCKED_BY_INPUT")

	ringLanded := func(ring []dependencyEdge) bool {
		for _, link := range ring {
			if !slices.Contains(blockedBy(link.issue), link.target) {
				return false
			}
		}
		return true
	}
	// expectWriteOrCycle holds a racing write's answer to 200 or 409 DEPENDENCY_CYCLE: any other
	// answer is a deadlock or a lost write wearing a refusal's clothes.
	expectWriteOrCycle := func(name string, round int, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code == http.StatusOK {
			return
		}
		if response.Code == http.StatusConflict && strings.Contains(response.Body.String(), `"code":"DEPENDENCY_CYCLE"`) {
			return
		}
		t.Fatalf("%s round %d: status=%d body=%s, want 200 or 409 DEPENDENCY_CYCLE",
			name, round, response.Code, response.Body.String())
	}
	// raceOverRing lays the ring's committed edges, then runs writes at once and holds each
	// answer to what expectWriteOrCycle allows. The ring itself must never be whole afterwards.
	raceOverRing := func(name string, ring, committed, writes []dependencyEdge) {
		t.Helper()
		for round := range crossProjectRaceRounds {
			for _, link := range ring {
				setBlockers(name+": clear "+link.issue, link.issue, nil)
			}
			for _, link := range committed {
				setBlockers(name+": commit "+link.issue, link.issue, []string{link.target})
			}
			start := make(chan struct{})
			responses := make(chan *httptest.ResponseRecorder, len(writes))
			for _, link := range writes {
				go func() {
					<-start
					responses <- patchBlockers(link.issue, []string{link.target})
				}()
			}
			close(start)
			for range writes {
				expectWriteOrCycle(name, round, awaitResponse(t, responses))
			}
			if ringLanded(ring) {
				t.Fatalf("%s round %d: the ring committed whole: %v", name, round, ring)
			}
		}
	}
	// raceOverRingJoinedUnderTheLock is raceOverRing for the pair no project lock orders: two
	// writers whose every target is its own project's, in two different projects, which only the
	// committed cross-project edges join into one component. A gate transaction of the test's own
	// stands in for the cross-project writers those edges come from: it holds the deployment-wide
	// dependency lock exclusively, as such a write does, waits until both writers have queued
	// behind it — which is past every read either makes with no lock held — writes their edges,
	// and commits, so the edges and the lock are released together. Each writer therefore walks a
	// graph that gained the joining edges after it read anything outside a lock, which is the
	// ordering a protocol deciding its lock from such a read would walk into. The edges are
	// written as SQL rather than through the API because an API write of them would queue behind
	// this very gate.
	raceOverRingJoinedUnderTheLock := func(name string, ring, committed, writes []dependencyEdge) {
		t.Helper()
		for round := range crossProjectRaceRounds {
			for _, link := range ring {
				setBlockers(name+": clear "+link.issue, link.issue, nil)
			}
			func() {
				gate, err := database.Pool.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer gate.Rollback(context.Background())
				if err := lockDeploymentIssueDependencies(context.Background(), gate); err != nil {
					t.Fatal(err)
				}
				responses := make(chan *httptest.ResponseRecorder, len(writes))
				for _, link := range writes {
					go func() {
						responses <- patchBlockers(link.issue, []string{link.target})
					}()
				}
				waitForDatabaseLocks(t, gate, len(writes))
				for _, link := range committed {
					if _, err := gate.Exec(context.Background(), `
						insert into issue_links (issue_key, kind, target_key) values ($1, 'blocked_by', $2)
					`, link.issue, link.target); err != nil {
						t.Fatal(err)
					}
				}
				if err := gate.Commit(context.Background()); err != nil {
					t.Fatal(err)
				}
				for range writes {
					expectWriteOrCycle(name, round, awaitResponse(t, responses))
				}
			}()
			if ringLanded(ring) {
				t.Fatalf("%s round %d: the ring committed whole: %v", name, round, ring)
			}
		}
	}

	// Three writers closing a ring over three projects, each write crossing a boundary: the
	// deployment-wide lock is what they meet on.
	threeWay := []dependencyEdge{
		{alpha[3], beta[3]},
		{beta[3], gamma[0]},
		{gamma[0], alpha[3]},
	}
	raceOverRing("a ring over three projects", threeWay, nil, threeWay)

	// One cross-project writer and one writer whose every target is its own project's, over a
	// committed edge that closes the ring through the other project: the deployment lock orders
	// them, and so would the project lock they share.
	mixed := []dependencyEdge{
		{alpha[4], beta[4]},
		{beta[4], alpha[5]},
		{alpha[5], alpha[4]},
	}
	raceOverRing("a cross-project writer beside one inside a project", mixed,
		[]dependencyEdge{{beta[4], alpha[5]}},
		[]dependencyEdge{{alpha[4], beta[4]}, {alpha[5], alpha[4]}})

	// Two writers whose every target is its own project's, in two different projects, over
	// committed cross-project edges that join their components. Neither write crosses a boundary
	// and the two share no project, so no project lock holds them apart and the deployment lock
	// is all there is: a protocol that took it shared for a write whose own targets stay inside
	// one project would let both commit and close the ring, since the edges that join them land
	// after each has read anything it reads unlocked.
	joined := []dependencyEdge{
		{alpha[6], beta[5]},
		{beta[5], beta[6]},
		{beta[6], alpha[7]},
		{alpha[7], alpha[6]},
	}
	raceOverRingJoinedUnderTheLock("two writers inside their own projects over cross-project edges", joined,
		[]dependencyEdge{{alpha[6], beta[5]}, {beta[6], alpha[7]}},
		[]dependencyEdge{{beta[5], beta[6]}, {alpha[7], alpha[6]}})

	t.Run("a credential that names one project is refused a target outside it", func(t *testing.T) {
		rig := newBrokerTokenRig(t)
		caller := newBrokerCaller(t)
		const token = "a-token-linking-across-projects"
		rig.broker.Says(token, liveNamedToken(caller.jkt))
		createProject := func(key string) {
			t.Helper()
			response := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": key, "name": key})
			if response.Code != http.StatusCreated {
				t.Fatalf("create project %s: status=%d body=%s", key, response.Code, response.Body.String())
			}
		}
		createIssue := func(project, title string) string {
			t.Helper()
			response := rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": project, "title": title, "spec": "# Spec", "force": true,
			})
			if response.Code != http.StatusCreated {
				t.Fatalf("create %q in %s: status=%d body=%s", title, project, response.Code, response.Body.String())
			}
			return decodeBody[model.Issue](t, response).Key
		}
		createProject(brokerTestProject)
		createProject("OTH")
		own := createIssue(brokerTestProject, "Its own project's issue")
		sibling := createIssue(brokerTestProject, "Another issue of its own project")
		outside := createIssue("OTH", "An issue of another project")

		refused := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{
			"blocked_by": []string{outside},
		}))
		if refused.Code != http.StatusForbidden {
			t.Fatalf("a scoped caller linking across projects: status=%d body=%s, want 403", refused.Code, refused.Body.String())
		}
		refusal := decodeBody[refusalAnswer](t, refused)
		if refusal.Code != codeProjectScope {
			t.Fatalf("refusal = %+v, want %s", refusal, codeProjectScope)
		}
		if !strings.Contains(refusal.Error, brokerTestProject) || !strings.Contains(refusal.Error, "OTH") {
			t.Fatalf("refusal %q names neither the project the credential covers (%s) nor the target's (OTH)",
				refusal.Error, brokerTestProject)
		}
		read := rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+own, nil)
		if got := decodeBody[model.Issue](t, read).BlockedBy; len(got) != 0 {
			t.Fatalf("the refused write left blocked_by = %v, want []", got)
		}

		// The same caller still links inside its own project, so what is refused is the target
		// outside the credential's project and not the route.
		allowed := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{
			"blocked_by": []string{sibling},
		}))
		if allowed.Code != http.StatusOK {
			t.Fatalf("a scoped caller linking inside its own project: status=%d body=%s", allowed.Code, allowed.Body.String())
		}
		if got := decodeBody[model.Issue](t, allowed).BlockedBy; !slices.Equal(got, []string{sibling}) {
			t.Fatalf("blocked_by = %v, want %v", got, []string{sibling})
		}
	})
}

// createIssueUnder creates an issue of project titled title as alice, under parent where parent
// is not empty, and answers the response as it came.
func createIssueUnder(t *testing.T, handler http.Handler, project, title, parent string, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"project": project, "title": title, "force": true}
	if parent != "" {
		body["parent"] = parent
	}
	for name, value := range fields {
		body[name] = value
	}
	return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
}

// createdIssue is the issue a create answered 201 with, failing the test on any other answer.
func createdIssue(t *testing.T, what string, response *httptest.ResponseRecorder) model.Issue {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("%s: status=%d body=%s, want 201", what, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// patchIssueParent moves key under parent as alice.
func patchIssueParent(t *testing.T, handler http.Handler, key, parent string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"parent": parent}, "alice")
}

// TestParentCrossesAProjectAndStillRefusesACycle: an issue's parent may be an issue of another
// project, on create and on PATCH, so a root that delivers part of another project's issue can
// stand under it. The parent lists the child and hears child.added as it does for a child of its
// own project, and every refusal that stood inside one project stands across two: a ring of
// parent links, and a chain past the depth cap, whichever projects its links alternate between.
func TestParentCrossesAProjectAndStillRefusesACycle(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	alpha := createListedIssues(t, handler, "ALPHA", 3)
	beta := createListedIssues(t, handler, "BETA", 3)

	created := createdIssue(t, "a create under a parent of another project",
		createIssueUnder(t, handler, "BETA", "Delivers part of another project's issue", alpha[0], nil))
	if created.Parent == nil || *created.Parent != alpha[0] {
		t.Fatalf("created %s with parent %v, want %s", created.Key, created.Parent, alpha[0])
	}

	moved := patchIssueParent(t, handler, beta[1], alpha[1])
	if moved.Code != http.StatusOK {
		t.Fatalf("move %s under %s: status=%d body=%s, want 200", beta[1], alpha[1], moved.Code, moved.Body.String())
	}
	if parent := decodeBody[model.Issue](t, moved).Parent; parent == nil || *parent != alpha[1] {
		t.Fatalf("moved %s has parent %v, want %s", beta[1], parent, alpha[1])
	}
	listed := false
	for _, child := range readIssueChildren(t, handler, alpha[1]) {
		listed = listed || child.Key == beta[1]
	}
	if !listed {
		t.Fatalf("%s does not list %s among its children", alpha[1], beta[1])
	}
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+alpha[1]+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("read the events of %s: status=%d body=%s", alpha[1], events.Code, events.Body.String())
	}
	heard := false
	for _, event := range decodeBody[[]struct {
		Type    string `json:"type"`
		Payload struct {
			ChildKey string `json:"child_key"`
		} `json:"payload"`
	}](t, events) {
		heard = heard || (event.Type == "child.added" && event.Payload.ChildKey == beta[1])
	}
	if !heard {
		t.Fatalf("no child.added of %s names %s", alpha[1], beta[1])
	}

	// A ring through two projects: BETA-3 under ALPHA-3 stands, so ALPHA-3 under BETA-3 closes it.
	if response := patchIssueParent(t, handler, beta[2], alpha[2]); response.Code != http.StatusOK {
		t.Fatalf("move %s under %s: status=%d body=%s, want 200", beta[2], alpha[2], response.Code, response.Body.String())
	}
	ring := patchIssueParent(t, handler, alpha[2], beta[2])
	if ring.Code != http.StatusConflict || !strings.Contains(ring.Body.String(), `"code":"PARENT_INPUT"`) ||
		!strings.Contains(ring.Body.String(), "would create a cycle") {
		t.Fatalf("a ring of parents through two projects: status=%d body=%s, want 409 PARENT_INPUT would create a cycle",
			ring.Code, ring.Body.String())
	}

	// A chain of parentDepthCap links whose projects alternate takes no further link, on create
	// or by moving its root under another issue.
	chain := []string{createdIssue(t, "the chain's root",
		createIssueUnder(t, handler, "ALPHA", "The root of a chain over two projects", "", nil)).Key}
	for link := 1; link <= 32; link++ {
		project := "ALPHA"
		if link%2 == 1 {
			project = "BETA"
		}
		chain = append(chain, createdIssue(t, "link "+strconv.Itoa(link)+" of the chain",
			createIssueUnder(t, handler, project, "Link "+strconv.Itoa(link)+" of a chain over two projects", chain[link-1], nil)).Key)
	}
	deeper := createIssueUnder(t, handler, "ALPHA", "One link past the cap", chain[32], nil)
	if deeper.Code != http.StatusConflict || !strings.Contains(deeper.Body.String(), `"code":"PARENT_INPUT"`) ||
		!strings.Contains(deeper.Body.String(), "at most 32 links") {
		t.Fatalf("a create 33 links below its root: status=%d body=%s, want 409 PARENT_INPUT", deeper.Code, deeper.Body.String())
	}
	lifted := patchIssueParent(t, handler, chain[0], alpha[0])
	if lifted.Code != http.StatusConflict || !strings.Contains(lifted.Body.String(), `"code":"PARENT_INPUT"`) ||
		!strings.Contains(lifted.Body.String(), "at most 32 links") {
		t.Fatalf("moving the chain's root under %s: status=%d body=%s, want 409 PARENT_INPUT", alpha[0], lifted.Code, lifted.Body.String())
	}
}

// TestAProjectScopedCallerIsRefusedAParentOfAnotherProject: a credential that may write one
// project is refused a parent outside it, as it is refused such a blocked_by target, though the
// issue it writes is of its own project: 403 PROJECT_SCOPE naming the parent and its project,
// with nothing written. A parent of its own project is the control.
func TestAProjectScopedCallerIsRefusedAParentOfAnotherProject(t *testing.T) {
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	for _, project := range []string{"ALPHA", "BETA"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	createIssue := func(project, title string) string {
		t.Helper()
		return createdIssue(t, "create "+title, rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": title, "spec": "# Spec", "force": true,
		})).Key
	}
	approved := createIssue("ALPHA", "Another project's approved issue")
	own := createIssue("BETA", "Its own project's issue")
	sibling := createIssue("BETA", "Another issue of its own project")
	const token = "a-token-scoped-to-beta"
	answer := liveNamedToken(caller.jkt)
	answer.Projects = []string{"BETA"}
	rig.broker.Says(token, answer)
	betaIssues := func() int {
		t.Helper()
		var count int
		if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from issues where project_key = 'BETA'`).Scan(&count); err != nil {
			t.Fatalf("count BETA's issues: %v", err)
		}
		return count
	}

	before := betaIssues()
	refused := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
		"project": "BETA", "title": "Filed under another project's issue", "spec": "# Spec", "parent": approved, "force": true,
	}))
	expectRefusal(t, "a scoped create under a parent of another project", refused, http.StatusForbidden, codeProjectScope, approved, "ALPHA", "BETA")
	if after := betaIssues(); after != before {
		t.Fatalf("the refused create took BETA from %d issues to %d, want none written", before, after)
	}

	moved := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{"parent": approved}))
	expectRefusal(t, "a scoped move under a parent of another project", moved, http.StatusForbidden, codeProjectScope, approved, "ALPHA", "BETA")
	if parent := decodeBody[model.Issue](t, rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+own, nil)).Parent; parent != nil {
		t.Fatalf("the refused move left %s under %s, want no parent", own, *parent)
	}

	allowed := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{"parent": sibling}))
	if allowed.Code != http.StatusOK {
		t.Fatalf("a scoped move under its own project's issue: status=%d body=%s, want 200", allowed.Code, allowed.Body.String())
	}
	if parent := decodeBody[model.Issue](t, allowed).Parent; parent == nil || *parent != sibling {
		t.Fatalf("%s's parent = %v, want %s", own, parent, sibling)
	}
}

// TestInheritedValuesStopAtTheProjectEdge: an issue inherits its repository, its components, its
// architecture owner and the legion label only from ancestors of its own project, because one
// project's deployment acts on each. A parent of another project passes on none of them, and
// neither does a chain that leaves the project and comes back into it: BETA's issue under
// ALPHA's issue under BETA's issue is a root of its own for every one of the four. A child and a
// grandchild of the issue's own project still inherit each, which is the control.
func TestInheritedValuesStopAtTheProjectEdge(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	for _, project := range []string{"ALPHA", "BETA"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project,
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	for repo, project := range map[string]string{"acme/alpha-default": "ALPHA", "acme/beta": "BETA"} {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/settings/repo-projects/"+repo, map[string]any{
			"project": project, "default": true,
		}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("map %s as %s's default: status=%d body=%s", repo, project, response.Code, response.Body.String())
		}
	}
	// Each project's model holds a component named web, so an attachment that crossed the edge
	// would land on the other project's web.
	for _, statement := range []string{
		`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by) values
			('ALPHA', 1, 'acme/alpha-model', 'main', 1, '{"kind":"system","id":"test"}'),
			('BETA', 1, 'acme/beta-model', 'main', 1, '{"kind":"system","id":"test"}')`,
		`insert into architecture_snapshots (id, project_key, commit, files) values (901, 'ALPHA', 'c1', '{}'), (902, 'BETA', 'c1', '{}')`,
		`insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('ALPHA', 'web', 'Web', '', null, 901, 1), ('BETA', 'web', 'Web', '', null, 902, 1)`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("seed the architecture models: %v\n%s", err, statement)
		}
	}
	everyValue := map[string]any{
		"components": map[string]any{"mode": "explicit", "ids": []string{"web"}},
		"labels":     []string{"legion"},
	}
	withRepo := func(repo string) map[string]any {
		fields := map[string]any{"repo": repo}
		for name, value := range everyValue {
			fields[name] = value
		}
		return fields
	}
	alphaRoot := createdIssue(t, "ALPHA's root", createIssueUnder(t, handler, "ALPHA", "Approved work of ALPHA", "", withRepo("acme/alpha"))).Key

	// The child of another project is filed by a session, whose create's advice answers who owns
	// the status from the row the create is about to write (legionManagedStatusOfRow).
	sessionCreate := func(project, title, parent string) (string, *testWriteAdvice) {
		t.Helper()
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": title, "parent": parent, "force": true,
			"components": map[string]any{"mode": "inherit"},
			"actor":      adviceSessionActor("session-across-projects"),
		})
		advice := adviceFromResponse(t, response.Code, response.Body.String())
		return decodeBody[model.Issue](t, response).Key, advice
	}
	betaChild, betaChildAdvice := sessionCreate("BETA", "Delivers part of ALPHA's work", alphaRoot)
	alphaChild, alphaChildAdvice := sessionCreate("ALPHA", "A child of ALPHA's own", alphaRoot)
	betaGrandchild := createdIssue(t, "BETA's grandchild of ALPHA's root", createIssueUnder(t, handler, "BETA", "Under the child of another project", betaChild, nil)).Key
	alphaGrandchild := createdIssue(t, "ALPHA's grandchild", createIssueUnder(t, handler, "ALPHA", "A grandchild of ALPHA's own", alphaChild, nil)).Key

	// The chain that comes back: BETA's issue under ALPHA's issue under BETA's top, the top
	// holding every value.
	betaBack := createdIssue(t, "BETA's issue that comes back", createIssueUnder(t, handler, "BETA", "Back in BETA", "", nil)).Key
	betaTop := createdIssue(t, "BETA's top", createIssueUnder(t, handler, "BETA", "BETA's own top", "", withRepo("acme/beta4"))).Key
	alphaBetween := createdIssue(t, "ALPHA's issue between", createIssueUnder(t, handler, "ALPHA", "ALPHA between two BETA issues", betaTop, nil)).Key
	if response := patchIssueParent(t, handler, betaBack, alphaBetween); response.Code != http.StatusOK {
		t.Fatalf("move %s under %s: status=%d body=%s, want 200", betaBack, alphaBetween, response.Code, response.Body.String())
	}

	legionManaged := func(key string) bool {
		t.Helper()
		tx, err := database.Pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		managed, err := legionManagedStatus(context.Background(), tx, key)
		if err != nil {
			t.Fatalf("read who owns the status of %s: %v", key, err)
		}
		return managed
	}
	type tree struct {
		Components []struct {
			ID     string `json:"id"`
			Issues []struct {
				Key      string `json:"key"`
				Attached string `json:"attached"`
			} `json:"issues"`
		} `json:"components"`
		Unassigned []struct {
			Key string `json:"key"`
		} `json:"unassigned"`
	}
	readTree := func(project string) tree {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/"+project+"/architecture", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s's architecture: status=%d body=%s", project, response.Code, response.Body.String())
		}
		return decodeBody[tree](t, response)
	}
	trees := map[string]tree{"ALPHA": readTree("ALPHA"), "BETA": readTree("BETA")}
	// owner is how key's project's web component counts it, "" where it does not, and whether
	// the project's tree lists key as unassigned.
	owner := func(project, key string) (string, bool) {
		t.Helper()
		attached := ""
		for _, component := range trees[project].Components {
			for _, issue := range component.Issues {
				if component.ID == "web" && issue.Key == key {
					attached = issue.Attached
				}
			}
		}
		unassigned := false
		for _, issue := range trees[project].Unassigned {
			unassigned = unassigned || issue.Key == key
		}
		return attached, unassigned
	}

	for _, want := range []struct {
		key, project, is string
		repo             string
		inheritedFrom    string
		legion           bool
		attached         string
	}{
		{betaChild, "BETA", "a child of another project's issue", "acme/beta", "", false, ""},
		{betaBack, "BETA", "an issue whose chain leaves the project and comes back", "acme/beta", "", false, ""},
		{alphaBetween, "ALPHA", "an issue under an issue of another project", "acme/alpha-default", "", false, ""},
		{betaGrandchild, "BETA", "a grandchild under the child of another project", "acme/beta", "", false, ""},
		{alphaChild, "ALPHA", "a child of its own project's issue", "acme/alpha", alphaRoot, true, "inherited"},
		{alphaGrandchild, "ALPHA", "a grandchild of its own project's issue", "acme/alpha", alphaRoot, true, "inherited"},
	} {
		issue := readTestIssue(t, handler, want.key)
		if repo := valueOrNone(issue.ResolvedRepo); repo != want.repo {
			t.Errorf("%s, %s: resolved_repo = %s, want %s", want.key, want.is, repo, want.repo)
		}
		inheritedFrom := valueOrNone(issue.Components.InheritedFrom)
		if want.inheritedFrom == "" && (issue.Components.Mode != "inherit" || inheritedFrom != "none" || len(issue.Components.IDs) != 0) {
			t.Errorf("%s, %s: components mode %s ids %v inherited from %s, want none inherited",
				want.key, want.is, issue.Components.Mode, issue.Components.IDs, inheritedFrom)
		}
		if want.inheritedFrom != "" && (inheritedFrom != want.inheritedFrom || !slices.Equal(issue.Components.IDs, []string{"web"})) {
			t.Errorf("%s, %s: components mode %s ids %v inherited from %s, want web inherited from %s",
				want.key, want.is, issue.Components.Mode, issue.Components.IDs, inheritedFrom, want.inheritedFrom)
		}
		if got := legionManaged(want.key); got != want.legion {
			t.Errorf("%s, %s: legionManagedStatus = %t, want %t", want.key, want.is, got, want.legion)
		}
		attached, unassigned := owner(want.project, want.key)
		if attached != want.attached || unassigned != (want.attached == "") {
			t.Errorf("%s, %s: %s's web counts it %q and unassigned is %t, want %q", want.key, want.is, want.project, attached, unassigned, want.attached)
		}
	}
	if advice := betaChildAdvice.LegionManagedStatus; advice == nil || *advice {
		t.Errorf("the create of %s under %s answered legion_managed_status %s, want false", betaChild, alphaRoot, answerOrNone(advice))
	}
	if advice := alphaChildAdvice.LegionManagedStatus; advice == nil || !*advice {
		t.Errorf("the create of %s under %s answered legion_managed_status %s, want true", alphaChild, alphaRoot, answerOrNone(advice))
	}

	// Once the child of another project holds values of its own, its grandchild of the same
	// project takes each of them from it, across the edge above.
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+betaChild, withRepo("acme/beta1"), "alice"); response.Code != http.StatusOK {
		t.Fatalf("give %s values of its own: status=%d body=%s", betaChild, response.Code, response.Body.String())
	}
	grandchild := readTestIssue(t, handler, betaGrandchild)
	if repo := valueOrNone(grandchild.ResolvedRepo); repo != "acme/beta1" {
		t.Errorf("%s under %s: resolved_repo = %s, want acme/beta1", betaGrandchild, betaChild, repo)
	}
	if inheritedFrom := valueOrNone(grandchild.Components.InheritedFrom); inheritedFrom != betaChild {
		t.Errorf("%s under %s: components inherited from %s, want %s", betaGrandchild, betaChild, inheritedFrom, betaChild)
	}
	if !legionManaged(betaGrandchild) {
		t.Errorf("%s under %s: legionManagedStatus = false, want true", betaGrandchild, betaChild)
	}
	trees["BETA"] = readTree("BETA")
	if attached, _ := owner("BETA", betaGrandchild); attached != "inherited" {
		t.Errorf("%s under %s: BETA's web counts it %q, want inherited", betaGrandchild, betaChild, attached)
	}
}

// valueOrNone renders an optional string of a read in a failure message.
func valueOrNone(value *string) string {
	if value == nil {
		return "none"
	}
	return *value
}

// answerOrNone renders an optional answer of a read in a failure message.
func answerOrNone(value *bool) string {
	if value == nil {
		return "none"
	}
	return strconv.FormatBool(*value)
}
