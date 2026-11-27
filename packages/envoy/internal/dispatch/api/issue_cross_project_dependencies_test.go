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
// and has nothing but that one lock to order it. A caller whose credential names one project
// links a target of another project as a caller whose credential names none does (INFRA-4799),
// and a link to an issue that does not exist is refused it as it is refused every caller.
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

	t.Run("a credential that names one project links a target of another project on create, on a PATCH, and on a PATCH that keeps it", func(t *testing.T) {
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
		readBlockedBy := func(t *testing.T, key string) []string {
			t.Helper()
			read := rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+key, nil)
			if read.Code != http.StatusOK {
				t.Fatalf("read %s: status=%d body=%s", key, read.Code, read.Body.String())
			}
			return decodeBody[model.Issue](t, read).BlockedBy
		}
		patchBlockedBy := func(t *testing.T, key string, targets []string) *httptest.ResponseRecorder {
			t.Helper()
			return rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+key, token, map[string]any{
				"blocked_by": targets,
			}))
		}
		createProject(brokerTestProject)
		createProject("OTH")
		own := createIssue(brokerTestProject, "Its own project's issue")
		kept := createIssue(brokerTestProject, "An issue already waiting on another project")
		sibling := createIssue(brokerTestProject, "Another issue of its own project")
		outside := createIssue("OTH", "An issue of another project")
		// The link the third row keeps is laid by a person, whose credential names no project, so
		// that row starts from it whatever the rows before it answered.
		if laid := rig.asPerson(t, http.MethodPatch, "/api/v1/issues/"+kept, map[string]any{
			"blocked_by": []string{outside},
		}); laid.Code != http.StatusOK {
			t.Fatalf("a person links %s to %s: status=%d body=%s", kept, outside, laid.Code, laid.Body.String())
		}

		t.Run("an issue made with the link is 201", func(t *testing.T) {
			created := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
				"project": brokerTestProject, "title": "Made waiting on another project", "spec": "# Spec",
				"blocked_by": []string{outside}, "force": true,
			}))
			if created.Code != http.StatusCreated {
				t.Fatalf("a scoped caller making an issue that waits on %s: status=%d body=%s, want 201",
					outside, created.Code, created.Body.String())
			}
			made := decodeBody[model.Issue](t, created).Key
			if got := readBlockedBy(t, made); !slices.Equal(got, []string{outside}) {
				t.Fatalf("blocked_by of %s read back = %v, want %v", made, got, []string{outside})
			}
		})

		t.Run("a PATCH that sets the link is 200", func(t *testing.T) {
			if set := patchBlockedBy(t, own, []string{outside}); set.Code != http.StatusOK {
				t.Fatalf("a scoped caller linking %s to %s: status=%d body=%s, want 200", own, outside, set.Code, set.Body.String())
			}
			if got := readBlockedBy(t, own); !slices.Equal(got, []string{outside}) {
				t.Fatalf("blocked_by of %s read back = %v, want %v", own, got, []string{outside})
			}
		})

		t.Run("a PATCH that writes again a list keeping the link, with one more target, is 200", func(t *testing.T) {
			want := []string{outside, sibling}
			slices.Sort(want)
			if written := patchBlockedBy(t, kept, want); written.Code != http.StatusOK {
				t.Fatalf("a scoped caller writing %v again on %s: status=%d body=%s, want 200", want, kept, written.Code, written.Body.String())
			}
			if got := readBlockedBy(t, kept); !slices.Equal(got, want) {
				t.Fatalf("blocked_by of %s read back = %v, want %v", kept, got, want)
			}
		})

		t.Run("a link to an issue that does not exist is 400 BLOCKED_BY_INPUT", func(t *testing.T) {
			const missing = "OTH-999"
			refused := patchBlockedBy(t, sibling, []string{missing})
			if code, text := refusalOf(t, refused); refused.Code != http.StatusBadRequest || code != "BLOCKED_BY_INPUT" || !strings.Contains(text, missing) {
				t.Fatalf("a scoped caller linking %s to %s: status=%d body=%s, want 400 BLOCKED_BY_INPUT naming %s",
					sibling, missing, refused.Code, refused.Body.String(), missing)
			}
			if got := readBlockedBy(t, sibling); len(got) != 0 {
				t.Fatalf("the refused write left blocked_by of %s = %v, want []", sibling, got)
			}
		})

		// A target of a private project its person does not see is refused exactly as a key no
		// issue holds, naming nothing of the project it belongs to. It runs last: the project it
		// hides is one the rows above never read.
		t.Run("a link to an issue of a project its person does not see is answered as a key nobody holds", func(t *testing.T) {
			createProject("HIDE")
			hidden := createIssue("HIDE", "An issue of a project its person does not see")
			hideFromBrokerPerson(t, rig, "HIDE")
			refused := patchBlockedBy(t, sibling, []string{hidden})
			missing := patchBlockedBy(t, sibling, []string{"NOPE-1"})
			if refused.Code != http.StatusBadRequest || refused.Body.String() != strings.ReplaceAll(missing.Body.String(), "NOPE-1", hidden) {
				t.Fatalf("a scoped caller linking to a hidden issue: status=%d body=%s, want what a key nobody holds answers: status=%d body=%s",
					refused.Code, refused.Body.String(), missing.Code, missing.Body.String())
			}
			if got := readBlockedBy(t, sibling); len(got) != 0 {
				t.Fatalf("the refused write left blocked_by of %s = %v, want []", sibling, got)
			}
		})
	})
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
	// The test writes the link itself rather than through the route, so it reads the same at a
	// build whose routes still refuse a parent of another project.
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

// TestRacingReparentsAcrossProjectsCommitNoRing: two writes that would close a ring of parents
// through two projects, an ALPHA issue moved under a BETA issue and that BETA issue moved under
// the ALPHA one, sent at once. Both take the deployment-wide dependency lock, and each locks the
// other's project for its parent, so they run one after the other: each round ends with exactly
// one taken and the other refused as a cycle, never with both committed, never with a deadlock.
// The race is replayed crossProjectRaceRounds times, each round on the pair cleared of parents.
func TestRacingReparentsAcrossProjectsCommitNoRing(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	alpha := createListedIssues(t, handler, "ALPHA", 1)[0]
	beta := createListedIssues(t, handler, "BETA", 1)[0]
	parentOf := func(key string) string {
		t.Helper()
		return valueOrNone(readTestIssue(t, handler, key).Parent)
	}
	clearParent := func(key string) {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{"parent": nil}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("clear %s's parent: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	for round := range crossProjectRaceRounds {
		clearParent(alpha)
		clearParent(beta)
		start := make(chan struct{})
		responses := make(chan *httptest.ResponseRecorder, 2)
		for _, move := range []struct{ key, parent string }{{alpha, beta}, {beta, alpha}} {
			go func() {
				<-start
				responses <- patchIssueParent(t, handler, move.key, move.parent)
			}()
		}
		close(start)
		taken, refused := 0, 0
		for range 2 {
			response := awaitResponse(t, responses)
			switch {
			case response.Code == http.StatusOK:
				taken++
			case response.Code == http.StatusConflict && (strings.Contains(response.Body.String(), `"code":"PARENT_INPUT"`) ||
				strings.Contains(response.Body.String(), `"code":"DEPENDENCY_CYCLE"`)):
				refused++
			default:
				t.Fatalf("round %d: status=%d body=%s, want 200 or a 409 cycle refusal", round, response.Code, response.Body.String())
			}
		}
		if taken != 1 || refused != 1 {
			t.Fatalf("round %d: %d reparents taken and %d refused, want one of each", round, taken, refused)
		}
		if parentOf(alpha) == beta && parentOf(beta) == alpha {
			t.Fatalf("round %d: the ring %s → %s → %s committed", round, alpha, beta, alpha)
		}
	}
}

// TestAParentWriteTakesTheParentsProjectLock: a create with a parent and a reparent take the
// dependency lock of the parent's project, not only of the issue's own, so a writer that holds
// that lock - an earlier build's dependency write of that project, which takes no other - fences
// both. With ALPHA's lock held, a BETA create under ALPHA's issue and a BETA reparent under it
// wait; once it is released, both land with that parent.
func TestAParentWriteTakesTheParentsProjectLock(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	alpha := createListedIssues(t, handler, "ALPHA", 1)[0]
	beta := createListedIssues(t, handler, "BETA", 1)[0]
	gate, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(context.Background())
	if err := lockProjectIssueDependencies(context.Background(), gate, "ALPHA"); err != nil {
		t.Fatal(err)
	}
	waiting := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		waiting <- patchIssueParent(t, handler, beta, alpha)
	}()
	go func() {
		waiting <- createIssueUnder(t, handler, "BETA", "Created under ALPHA's issue", alpha, nil)
	}()
	waitForDatabaseLocks(t, gate, 2)
	if parent := readTestIssue(t, handler, beta).Parent; parent != nil {
		t.Fatalf("the reparent of %s landed under %s while ALPHA's dependency lock was held", beta, *parent)
	}
	if err := gate.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := awaitResponse(t, waiting)
		if response.Code != http.StatusOK && response.Code != http.StatusCreated {
			t.Fatalf("a parent write after the lock's release: status=%d body=%s", response.Code, response.Body.String())
		}
		if parent := decodeBody[model.Issue](t, response).Parent; parent == nil || *parent != alpha {
			t.Fatalf("a parent write after the lock's release left the parent %s, want %s", valueOrNone(parent), alpha)
		}
	}
}

// TestAProjectScopedCallerNamesAParentOrABlockedByTargetOfAnotherProject: a credential that names
// one project names a parent or a blocked_by target of another project as a credential that names
// no project does (INFRA-4799). For each field, a create that names it is made, an unforced near
// copy that names it reaches the near-copy gate, which refuses the copy and records the refusal, a
// PATCH that sets it is taken, and a close that also sets it settles the issue's documents first
// and closes the issue.
func TestAProjectScopedCallerNamesAParentOrABlockedByTargetOfAnotherProject(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"parent", "blocked_by"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			rig := newBrokerTokenRigOn(t, testServerOptions{duplicateBound: nearCopyVerdictBound(t), settle: time.Hour})
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
			const token = "a-token-scoped-to-beta"
			answer := liveNamedToken(caller.jkt)
			answer.Projects = []string{"BETA"}
			rig.broker.Says(token, answer)
			// value is what a write sends in field, and named reads the field back as a list: the
			// parent's key, or none, for a parent, and the list itself for blocked_by. An issue that
			// names approved there reads as want.
			var value any = approved
			want := []string{approved}
			named := func(issue model.Issue) []string {
				if issue.Parent == nil {
					return nil
				}
				return []string{*issue.Parent}
			}
			if field == "blocked_by" {
				value = []string{approved}
				named = func(issue model.Issue) []string { return issue.BlockedBy }
			}
			nearCopyChecks := func() int {
				t.Helper()
				var count int
				if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from near_copy_checks`).Scan(&count); err != nil {
					t.Fatalf("count the near-copy checks: %v", err)
				}
				return count
			}

			filed := createdIssue(t, "a scoped create that names "+field+" "+approved, rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
				"project": "BETA", "title": "Filed against another project's issue", "spec": "# Spec", field: value, "force": true,
			})))
			if filed.Project != "BETA" || !slices.Equal(named(filed), want) {
				t.Fatalf("the scoped create made %s of project %s with %s %v, want an issue of BETA with %s %v", filed.Key, filed.Project, field, named(filed), field, want)
			}
			// An unforced copy of an open BETA issue that names a parent or a blocked_by target of
			// another project reaches the near-copy gate, which refuses it and records the refusal.
			createdIssue(t, "the issue a near copy copies", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "BETA", "title": "Astrolabe calibration drifts after a reseat", "spec": duplicateSpecAstrolabe,
			}))
			checksBefore := nearCopyChecks()
			nearCopy := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues", token, map[string]any{
				"project": "BETA", "title": "Astrolabe calibration drifts after a reseat", "spec": duplicateSpecAstrolabe, field: value,
			}))
			requireDuplicateRefusal(t, nearCopy)
			if after := nearCopyChecks(); after != checksBefore+1 {
				t.Fatalf("the gate's refusal took near_copy_checks from %d rows to %d, want one more", checksBefore, after)
			}

			set := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+own, token, map[string]any{field: value}))
			if set.Code != http.StatusOK {
				t.Fatalf("a scoped PATCH that sets %s %v: status=%d body=%s, want 200", field, want, set.Code, set.Body.String())
			}
			if got := named(decodeBody[model.Issue](t, rig.asPerson(t, http.MethodGet, "/api/v1/issues/"+own, nil))); !slices.Equal(got, want) {
				t.Fatalf("the scoped PATCH left %s with %s %v, want %v", own, field, got, want)
			}
			// A close that also sets the field settles the issue's documents first
			// (settleIssueBeforeClose), which opens the spec's decision block's ask, and closes the
			// issue. The rig settles a document only after an hour, so the spec still owes that
			// settlement when the close arrives.
			closer := createdIssue(t, "create the issue a scoped caller closes", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "BETA", "title": "An issue a scoped caller closes", "spec": closeDecision, "force": true,
			}))
			asks := func() int {
				t.Helper()
				var count int
				if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from asks where issue_key = $1`, closer.Key).Scan(&count); err != nil {
					t.Fatalf("count %s's asks: %v", closer.Key, err)
				}
				return count
			}
			asksBefore := asks()
			closed := rig.serve(caller.presenting(t, http.MethodPatch, "/api/v1/issues/"+closer.Key, token, map[string]any{
				field: value, "status": "done",
			}))
			if closed.Code != http.StatusOK {
				t.Fatalf("a scoped close that sets %s %v: status=%d body=%s, want 200", field, want, closed.Code, closed.Body.String())
			}
			if issue := decodeBody[model.Issue](t, closed); issue.Status != "done" || !slices.Equal(named(issue), want) {
				t.Fatalf("the close left %s %s with %s %v, want done with %s %v", closer.Key, issue.Status, field, named(issue), field, want)
			}
			if after := asks(); after != asksBefore+1 {
				t.Fatalf("the close left %s with %d asks, want the %d its settlement opens", closer.Key, after, asksBefore+1)
			}
		})
	}
}

// TestAProjectScopedCallerRecordsAndRetractsOnAnotherProjectsCheck: a credential that names one
// project records a result on a check of another project's issue, and retracts it, as a
// credential that names no project does (INFRA-4799). The results routes reach such a check
// through a parent of another project (resolveCheck reads the Verification documents of the
// issue and every issue above it, across projects), and a result and its retraction land on the
// check's issue; a result recorded on that issue itself reaches it through its address.
func TestAProjectScopedCallerRecordsAndRetractsOnAnotherProjectsCheck(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	for _, project := range []string{"ALPHA", "BETA"} {
		if created := rig.asPerson(t, http.MethodPost, "/api/v1/projects", map[string]any{"key": project, "name": project}); created.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
		}
	}
	approved := createdIssue(t, "create another project's approved issue", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "ALPHA", "title": "Another project's approved issue", "spec": "# Spec", "force": true,
	})).Key
	uploadDocument(t, rig.handler, "/api/v1/issues/"+approved, "verification.md", twoChecks)
	underAlpha := createdIssue(t, "create a BETA issue under ALPHA's", rig.asPerson(t, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "BETA", "title": "Records on another project's check", "parent": approved, "force": true,
	})).Key
	const token = "a-token-scoped-to-beta"
	answer := liveNamedToken(caller.jkt)
	answer.Projects = []string{"BETA"}
	rig.broker.Says(token, answer)
	resultsOn := func(issue string) int {
		t.Helper()
		var count int
		if err := rig.database.Pool.QueryRow(context.Background(),
			`select count(*) from check_results where check_issue_key = $1`, issue).Scan(&count); err != nil {
			t.Fatalf("count the results on %s's checks: %v", issue, err)
		}
		return count
	}
	record := func(what, issue string) model.CheckResult {
		t.Helper()
		before := resultsOn(approved)
		recorded := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/issues/"+issue+"/results", token,
			resultBody("k1", "pass", newHeadCommit, nil)))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s, want 201", what, recorded.Code, recorded.Body.String())
		}
		if after := resultsOn(approved); after != before+1 {
			t.Fatalf("%s took %s's checks from %d results to %d, want one more", what, approved, before, after)
		}
		return decodeBody[recordedResult](t, recorded).Result
	}

	// Through the BETA issue under ALPHA's first: the route's address is BETA's, so only the
	// result route's own read of the check's issue judges ALPHA's check.
	result := record("a scoped result on another project's check, through its own project's issue", underAlpha)
	retracted := rig.serve(caller.presenting(t, http.MethodPost, "/api/v1/results/"+result.ID+"/retract", token, map[string]any{
		"reason": "The run was against the wrong commit.",
	}))
	if retracted.Code != http.StatusCreated {
		t.Fatalf("a scoped retraction on another project's check: status=%d body=%s, want 201", retracted.Code, retracted.Body.String())
	}
	if _, held := storedRetraction(t, rig.database, result.ID); !held {
		t.Fatalf("the scoped retraction left no retraction of %s", result.ID)
	}
	if events := retractionEventsOn(t, rig.database, approved); events != 1 {
		t.Fatalf("the scoped retraction appended %d result.retracted events on %s, want one", events, approved)
	}
	record("a scoped result recorded on another project's issue itself", approved)
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
	for project, repo := range map[string]string{"ALPHA": "acme/alpha-default", "BETA": "acme/beta"} {
		setDefaultRepository(t, handler, project, repo)
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
