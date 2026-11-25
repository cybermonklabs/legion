package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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
	t.Parallel()
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
	// is refused exactly as one inside a project is; a parent cannot cross a project, so the
	// parent rules are read inside one.
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

	for _, private := range []bool{false, true} {
		name := "a credential that names one project is refused a target outside it"
		if private {
			name += ", and a hidden one as a key nobody holds, once a project is private"
		}
		t.Run(name, func(t *testing.T) {
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

			// A full member's credential scoped to one project keeps the write scope it had before
			// projects could be private, whatever the deployment holds: a target of another project
			// it sees is refused as before (R9), naming both projects.
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
			if private {
				// A target of a private project its person does not see is refused exactly as a key
				// no issue holds, naming nothing of the project it belongs to.
				createProject("HIDE")
				hidden := createIssue("HIDE", "An issue of a project its person does not see")
				hideFromBrokerPerson(t, rig, "HIDE")
				refused := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{
					"blocked_by": []string{hidden},
				}))
				missing := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{
					"blocked_by": []string{"NOPE-1"},
				}))
				if refused.Code != http.StatusBadRequest || refused.Body.String() != strings.ReplaceAll(missing.Body.String(), "NOPE-1", hidden) {
					t.Fatalf("a scoped caller linking to a hidden issue: status=%d body=%s, want what a key nobody holds answers: status=%d body=%s",
						refused.Code, refused.Body.String(), missing.Code, missing.Body.String())
				}
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
}

// foreignChildOpenAnswer is the body of a 409 FOREIGN_CHILD_OPEN, declared here rather than taken
// from the route's own type, so the test reads the wire shape a client reads.
type foreignChildOpenAnswer struct {
	Code   string `json:"code"`
	Error  string `json:"error"`
	Issues []struct {
		Key     string `json:"key"`
		Project string `json:"project"`
		Status  string `json:"status"`
	} `json:"issues"`
}

// TestAnAcceptanceWriteWaitsForAnOpenChildOfAnotherProject: a status write of retro or done that
// carries refuse_open_foreign_children, which the Legion daemon's sign_off and close_root send, is
// refused while a child of another project is open. Open is what the daemon's record.ChildOpen
// says of a child it does not record, which a child of another project always is: any status but
// done and icebox. A write without the field, a person's, and a write over an open child of the
// issue's own project, which the parent's own daemon judges, are taken as before.
func TestAnAcceptanceWriteWaitsForAnOpenChildOfAnotherProject(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	core := createListedIssues(t, handler, "CORE", 4)
	side := createListedIssues(t, handler, "SIDE", 1)
	parent, child := core[0], side[0]
	// The routes of this build refuse a parent of another project, so the test writes the link
	// itself, as a later build's route would.
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, parent, child); err != nil {
		t.Fatalf("put %s under %s: %v", child, parent, err)
	}
	patch := func(t *testing.T, key string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, body, "alice")
	}
	setStatus := func(t *testing.T, key, status string) {
		t.Helper()
		if response := patch(t, key, map[string]any{"status": status}); response.Code != http.StatusOK {
			t.Fatalf("set %s to %s: status=%d body=%s", key, status, response.Code, response.Body.String())
		}
	}
	statusOf := func(t *testing.T, key string) string {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		return decodeBody[model.Issue](t, response).Status
	}

	// Each row is a subtest of its own, so a run against a build without the rule reports every
	// row: the refused rows fail there, and the taken rows pass, which pins what stays as it was.
	for _, row := range []struct{ child, write string }{
		{"triage", "retro"},
		{"triage", "done"},
		{"in_progress", "retro"},
	} {
		t.Run(row.write+" over a child of another project in "+row.child+" is refused", func(t *testing.T) {
			setStatus(t, parent, "in_progress")
			setStatus(t, child, row.child)
			response := patch(t, parent, map[string]any{"status": row.write, "refuse_open_foreign_children": true})
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s, want 409 FOREIGN_CHILD_OPEN", response.Code, response.Body.String())
			}
			refusal := decodeBody[foreignChildOpenAnswer](t, response)
			got := make([]string, 0, len(refusal.Issues))
			for _, issue := range refusal.Issues {
				got = append(got, issue.Key+" "+issue.Project+" "+issue.Status)
			}
			want := []string{child + " SIDE " + row.child}
			if refusal.Code != "FOREIGN_CHILD_OPEN" || !slices.Equal(got, want) || !strings.Contains(refusal.Error, child) {
				t.Fatalf("refusal %+v, want FOREIGN_CHILD_OPEN naming %v", refusal, want)
			}
			if got := statusOf(t, parent); got != "in_progress" {
				t.Fatalf("the refused write left %s in %s, want in_progress", parent, got)
			}
		})
	}

	for _, row := range []struct {
		what, child string
		field       bool
	}{
		{"a child of another project in icebox, which a parent does not count as open", "icebox", true},
		{"a done child of another project", "done", true},
		{"an open child of another project, written with no field, as a person writes", "triage", false},
	} {
		t.Run("retro over "+row.what+" is taken", func(t *testing.T) {
			setStatus(t, parent, "in_progress")
			setStatus(t, child, row.child)
			body := map[string]any{"status": "retro"}
			if row.field {
				body["refuse_open_foreign_children"] = true
			}
			if response := patch(t, parent, body); response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
			}
			if got := statusOf(t, parent); got != "retro" {
				t.Fatalf("the write left %s in %s, want retro", parent, got)
			}
		})
	}

	// An open child of the issue's own project is its own daemon's to judge (record.ChildOpen
	// over the children it records), so Dispatch takes the write.
	t.Run("retro over an open child of the issue's own project is taken", func(t *testing.T) {
		ownParent, ownChild := core[2], core[3]
		if response := patch(t, ownChild, map[string]any{"parent": ownParent}); response.Code != http.StatusOK {
			t.Fatalf("put %s under %s: status=%d body=%s", ownChild, ownParent, response.Code, response.Body.String())
		}
		setStatus(t, ownChild, "in_progress")
		if response := patch(t, ownParent, map[string]any{"status": "retro", "refuse_open_foreign_children": true}); response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200", response.Code, response.Body.String())
		}
	})
}

// TestAnAcceptanceWriteCountsOnlyTheChildrenItsWriterSees: the open-child check reads a child as
// its writer reads it (refuseOpenForeignChildren, access.ProjectClause), since a private project
// answers as missing to a caller its list does not let in. PUB's issue has an open child in PRIV,
// carol's project, made private. carol, who sees PRIV, is refused the acceptance write, and the
// 409 names the child. alice, a full member PRIV's list does not name, does not see the child: her
// same write is taken, as over an issue with no such child.
//
// Control: the check's read with its project clause taken out, as main writes it, fails the second
// case: alice is refused, and the 409 names PRIV's child.
func TestAnAcceptanceWriteCountsOnlyTheChildrenItsWriterSees(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	seedCredentialProjects(t, handler, database)
	parent := issueIn(t, handler, accessCarol, publicProject)
	child := issueIn(t, handler, accessCarol, privateProject)
	// The routes of this build refuse a parent of another project, so the test writes the link
	// itself, as TestAnAcceptanceWriteWaitsForAnOpenChildOfAnotherProject does.
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $1 where key = $2`, parent, child); err != nil {
		t.Fatalf("put %s under %s: %v", child, parent, err)
	}
	makePrivate(t, database, privateProject)
	patch := func(t *testing.T, login string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+parent, body, login)
	}
	if response := patch(t, accessCarol, map[string]any{"status": "in_progress"}); response.Code != http.StatusOK {
		t.Fatalf("set %s to in_progress: status=%d body=%s", parent, response.Code, response.Body.String())
	}
	acceptance := map[string]any{"status": "retro", "refuse_open_foreign_children": true}

	refused := patch(t, accessCarol, acceptance)
	if refused.Code != http.StatusConflict {
		t.Fatalf("carol's acceptance write: status=%d body=%s, want 409 FOREIGN_CHILD_OPEN", refused.Code, refused.Body.String())
	}
	refusal := decodeBody[foreignChildOpenAnswer](t, refused)
	if len(refusal.Issues) != 1 || refusal.Issues[0].Key != child || refusal.Issues[0].Project != privateProject {
		t.Fatalf("carol's refusal %+v, want FOREIGN_CHILD_OPEN naming %s of %s", refusal, child, privateProject)
	}

	taken := patch(t, "alice", acceptance)
	if taken.Code != http.StatusOK {
		t.Fatalf("alice's acceptance write: status=%d body=%s, want 200: the open child is of a project she does not see", taken.Code, taken.Body.String())
	}
	if strings.Contains(taken.Body.String(), child) {
		t.Fatalf("alice's answer names %s, a child of a project she does not see: %s", child, taken.Body.String())
	}
}

// TestAnAcceptanceFieldGoesOnlyWithAnAcceptanceStatus: refuse_open_foreign_children asks for the
// check of an acceptance write, so it is refused beside any other status or none.
func TestAnAcceptanceFieldGoesOnlyWithAnAcceptanceStatus(t *testing.T) {
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	core := createListedIssues(t, handler, "CORE", 1)
	for _, body := range []map[string]any{
		{"status": "in_progress", "refuse_open_foreign_children": true},
		{"title": "Retitled", "refuse_open_foreign_children": true},
	} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[0], body, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_ISSUE"`) {
			t.Fatalf("%v: status=%d body=%s, want 400 INVALID_ISSUE", body, response.Code, response.Body.String())
		}
	}
}

// TestAnAcceptanceWriteTakesTheDependencyLock: the acceptance check and its write run under the
// deployment dependency lock that every parent write takes, so no child is filed between the two.
// A status write without the field takes no dependency lock, as before.
func TestAnAcceptanceWriteTakesTheDependencyLock(t *testing.T) {
	t.Parallel()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	core := createListedIssues(t, handler, "CORE", 2)
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if err := lockDeploymentIssueDependencies(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	plain := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		plain <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[1], map[string]any{"status": "retro"}, "alice")
	}()
	if response := awaitResponse(t, plain); response.Code != http.StatusOK {
		t.Fatalf("a status write with no field while the dependency lock is held: status=%d body=%s", response.Code, response.Body.String())
	}
	waiting := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		waiting <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+core[0], map[string]any{
			"status": "retro", "refuse_open_foreign_children": true,
		}, "alice")
	}()
	waitForDatabaseLocks(t, gate, 1)
	select {
	case response := <-waiting:
		t.Fatalf("the acceptance write was answered while the dependency lock was held: status=%d body=%s", response.Code, response.Body.String())
	default:
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if response := awaitResponse(t, waiting); response.Code != http.StatusOK {
		t.Fatalf("the acceptance write after the lock's release: status=%d body=%s", response.Code, response.Body.String())
	}
}
