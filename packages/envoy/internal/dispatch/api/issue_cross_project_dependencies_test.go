package api

import (
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
// writes race on an unlocked probe read, so one round can miss the interleaving that matters;
// each round clears the ring and lays its committed edges again first.
const crossProjectRaceRounds = 8

// dependencyEdge is one `blocked_by` link, written as the issue that waits and the issue it
// waits on.
type dependencyEdge struct{ issue, target string }

// TestBlockedByCrossesAProjectAndStillRefusesACycle: a `blocked_by` target may be an issue of
// another project, so an issue that waits on work tracked elsewhere can record that wait
// (INFRA-3036). Every refusal that stood before stands across projects too — a ring, a child
// waiting on its parent, a parent waiting on its descendant and an issue waiting on itself —
// and concurrent writers that would close a ring between projects meet on the deployment-wide
// dependency lock, so each ends with the write applied or with the cycle refusal, never with a
// deadlock and never with the ring committed. A caller whose credential names one project is
// still refused a target outside it.
func TestBlockedByCrossesAProjectAndStillRefusesACycle(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
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
	// raceOverRing lays the ring's committed edges, then runs writes at once and holds each
	// answer to 200 or 409 DEPENDENCY_CYCLE: any other answer is a deadlock or a lost write
	// wearing a refusal's clothes. The ring itself must never be whole afterwards.
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
				response := awaitResponse(t, responses)
				if response.Code == http.StatusOK {
					continue
				}
				if response.Code == http.StatusConflict && strings.Contains(response.Body.String(), `"code":"DEPENDENCY_CYCLE"`) {
					continue
				}
				t.Fatalf("%s round %d: status=%d body=%s, want 200 or 409 DEPENDENCY_CYCLE",
					name, round, response.Code, response.Body.String())
			}
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

	// One cross-project writer and one writer whose every target is its own project's, which
	// the deployment-wide lock alone would not serialize: the second takes the shared lock, and
	// the project lock they share is what orders them. The second writer's probe sees only its
	// own project until the first writer commits, so this is the stale probe the protocol rests
	// on.
	mixed := []dependencyEdge{
		{alpha[4], beta[4]},
		{beta[4], alpha[5]},
		{alpha[5], alpha[4]},
	}
	raceOverRing("a cross-project writer beside one inside a project", mixed,
		[]dependencyEdge{{beta[4], alpha[5]}},
		[]dependencyEdge{{alpha[4], beta[4]}, {alpha[5], alpha[4]}})

	// Two writers whose every target is its own project's, in two different projects, over
	// committed cross-project edges that join their components. Neither write crosses a
	// boundary and the two share no project, so only the probe puts them on one lock.
	joined := []dependencyEdge{
		{alpha[6], beta[5]},
		{beta[5], beta[6]},
		{beta[6], alpha[7]},
		{alpha[7], alpha[6]},
	}
	raceOverRing("two writers inside their own projects over cross-project edges", joined,
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
