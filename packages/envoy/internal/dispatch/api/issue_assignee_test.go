package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken"
	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

type assigneeIssue struct {
	Key      string  `json:"key"`
	Parent   *string `json:"parent"`
	Assignee *string `json:"assignee"`
}

func createAssigneeProject(t *testing.T, handler http.Handler) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
}

func expectAssignee(t *testing.T, got *string, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Fatalf("assignee = %q, want null", *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("assignee = null, want %q", want)
	}
	if *got != want {
		t.Fatalf("assignee = %q, want %q", *got, want)
	}
}

// A human who creates an issue is its assignee. Every identity names a person by lowercase
// email, so the assignee is lowercase even when the identity header that named them was not.
func TestCreateIssueByHumanAssignsCreatorLowercased(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Mine",
	}, "Alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, response).Assignee, "alice")
}

// A personal token acts for its owner: an issue it creates is assigned to that human.
func TestCreateIssueByPersonalTokenAssignsOwner(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "planner"}, "Bob")
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	token := decodeBody[agentTokenResponse](t, minted).Token
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "For Bob", "actor": sessionActor(),
	}, token)
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue with personal token: status=%d body=%s", response.Code, response.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, response).Assignee, "bob")
}

// The shared token has no owner, so a root issue it creates takes the project's default, the
// chain's last step, and a child under a parent somebody holds inherits that person. Only a
// PATCH that clears the assignee leaves an issue nobody holds (unassignIssue), and a child of
// that issue reaches the default like any other create with nobody above it
// (TestCreateIssueFallsBackToProjectDefaultAssignee). An explicit assignee always wins.
func TestCreateIssueBySharedTokenInheritsParentOrTakesTheDefault(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	root := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Root", "actor": sessionActor(),
	}, "agent-token")
	if root.Code != http.StatusCreated {
		t.Fatalf("create root: status=%d body=%s", root.Code, root.Body.String())
	}
	// CORE was created by alice, so alice is the person it defaults to.
	expectAssignee(t, decodeBody[assigneeIssue](t, root).Assignee, "alice")

	unownedIssue := unassignIssue(t, handler, createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Nobody's root",
	}).Key)
	expectAssignee(t, unownedIssue.Assignee, "")

	orphanChild := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Child of unassigned", "parent": unownedIssue.Key, "actor": sessionActor(),
	}, "agent-token")
	if orphanChild.Code != http.StatusCreated {
		t.Fatalf("create child: status=%d body=%s", orphanChild.Code, orphanChild.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, orphanChild).Assignee, "alice")

	explicit := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Explicit", "assignee": " Bob ", "actor": sessionActor(),
	}, "agent-token")
	if explicit.Code != http.StatusCreated {
		t.Fatalf("create explicit: status=%d body=%s", explicit.Code, explicit.Body.String())
	}
	explicitIssue := decodeBody[assigneeIssue](t, explicit)
	expectAssignee(t, explicitIssue.Assignee, "bob")

	child := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Child of Bob", "parent": explicitIssue.Key, "actor": sessionActor(),
	}, "agent-token")
	if child.Code != http.StatusCreated {
		t.Fatalf("create child of bob: status=%d body=%s", child.Code, child.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, child).Assignee, "bob")

	humanOverride := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Alice files for Bob", "assignee": "bob",
	}, "alice")
	if humanOverride.Code != http.StatusCreated {
		t.Fatalf("create with explicit assignee as human: status=%d body=%s", humanOverride.Code, humanOverride.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, humanOverride).Assignee, "bob")
}

func TestIssueAssigneeRejectsUnlistedAndMalformedLogins(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Target",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[assigneeIssue](t, created).Key

	for _, test := range []struct {
		name   string
		method string
		target string
		body   map[string]any
		code   string
	}{
		{name: "create unlisted", method: http.MethodPost, target: "/api/v1/issues", body: map[string]any{"project": "CORE", "title": "X", "assignee": "mallory"}, code: "ASSIGNEE_NOT_ALLOWED"},
		{name: "patch unlisted", method: http.MethodPatch, target: "/api/v1/issues/" + key, body: map[string]any{"assignee": "mallory"}, code: "ASSIGNEE_NOT_ALLOWED"},
		{name: "patch blank", method: http.MethodPatch, target: "/api/v1/issues/" + key, body: map[string]any{"assignee": "  "}, code: "ASSIGNEE_NOT_ALLOWED"},
		{name: "patch number", method: http.MethodPatch, target: "/api/v1/issues/" + key, body: map[string]any{"assignee": 7}, code: "INVALID_ISSUE"},
		{name: "create object", method: http.MethodPost, target: "/api/v1/issues", body: map[string]any{"project": "CORE", "title": "X", "assignee": map[string]any{"login": "bob"}}, code: "INVALID_ISSUE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, test.method, test.target, test.body, "alice")
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("status=%d body=%s, want 400 %s", response.Code, response.Body.String(), test.code)
			}
			if test.code == "ASSIGNEE_NOT_ALLOWED" && test.name != "patch blank" && !strings.Contains(response.Body.String(), "mallory") {
				t.Fatalf("refusal does not name the login: %s", response.Body.String())
			}
		})
	}
	unchanged := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	expectAssignee(t, decodeBody[assigneeIssue](t, unchanged).Assignee, "alice")
}

// Anyone who has signed in may reassign; the update event carries the new assignee and PATCH
// null clears it. A PATCH that does not mention assignee (the daemon's status writes) leaves it.
// An assignee move names both logins in thread_logins, since the Inbox lists an open ask on an
// open issue only to the person it is assigned to; a write that moves neither the status nor the
// assignee carries no thread_logins at all.
func TestPatchIssueReassignsClearsAndLeavesAssigneeAlone(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	createAssigneeProject(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Handoff",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[assigneeIssue](t, created).Key

	reassigned := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"assignee": "Bob",
	}, "bob")
	if reassigned.Code != http.StatusOK {
		t.Fatalf("reassign: status=%d body=%s", reassigned.Code, reassigned.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, reassigned).Assignee, "bob")

	statusOnly := agentRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"status": "in_progress", "actor": sessionActor(),
	}, "agent-token")
	if statusOnly.Code != http.StatusOK {
		t.Fatalf("status patch: status=%d body=%s", statusOnly.Code, statusOnly.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, statusOnly).Assignee, "bob")
	titleOnly := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"title": "Handoff, renamed",
	}, "alice")
	if titleOnly.Code != http.StatusOK {
		t.Fatalf("title patch: status=%d body=%s", titleOnly.Code, titleOnly.Body.String())
	}

	eventsResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
	if eventsResponse.Code != http.StatusOK {
		t.Fatalf("list events: status=%d body=%s", eventsResponse.Code, eventsResponse.Body.String())
	}
	var events []struct {
		Type    string `json:"type"`
		Actor   model.Actor
		Payload struct {
			Assignee     *string  `json:"assignee"`
			Changed      []string `json:"changed"`
			ThreadLogins []string `json:"thread_logins"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&events); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	sawReassign, sawStatus, sawTitle := false, false, false
	for _, event := range events {
		if event.Type == "issue.updated" && event.Actor.Kind == "user" && event.Actor.ID == "bob" {
			sawReassign = true
			expectAssignee(t, event.Payload.Assignee, "bob")
			// The issue moved from alice to bob, so an open ask on it leaves alice's Replies
			// list and enters bob's; the event names both, sorted.
			if !slices.Equal(event.Payload.Changed, []string{"assignee"}) || !slices.Equal(event.Payload.ThreadLogins, []string{"alice", "bob"}) {
				t.Fatalf("reassignment's issue.updated changed = %#v, thread_logins %#v; want [assignee] and [alice bob]", event.Payload.Changed, event.Payload.ThreadLogins)
			}
		}
		if event.Type == "issue.updated" && event.Actor.Kind == "user" && event.Actor.ID == "alice" {
			sawTitle = true
			if !slices.Equal(event.Payload.Changed, []string{"title"}) || event.Payload.ThreadLogins != nil {
				t.Fatalf("retitle's issue.updated changed = %#v, thread_logins %#v; want [title] and none", event.Payload.Changed, event.Payload.ThreadLogins)
			}
		}
		if event.Type == "issue.updated" && event.Actor.Kind == "session" {
			sawStatus = true
			// Nobody has written in the issue's threads, so the status move names nobody, as [].
			if !slices.Equal(event.Payload.Changed, []string{"status"}) || event.Payload.ThreadLogins == nil || len(event.Payload.ThreadLogins) != 0 {
				t.Fatalf("status write's issue.updated changed = %#v, thread_logins %#v; want [status] and []", event.Payload.Changed, event.Payload.ThreadLogins)
			}
		}
		if event.Type == "issue.created" {
			expectAssignee(t, event.Payload.Assignee, "alice")
			if !slices.Contains(event.Payload.Changed, "assignee") {
				t.Fatalf("issue.created changed = %#v, want the defaulted assignee among them", event.Payload.Changed)
			}
		}
	}
	if !sawStatus {
		t.Fatalf("the status write did not emit issue.updated by its session: %#v", events)
	}
	if !sawReassign {
		t.Fatalf("reassignment did not emit issue.updated by bob: %#v", events)
	}
	if !sawTitle {
		t.Fatalf("the retitle did not emit issue.updated by alice: %#v", events)
	}

	cleared := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"assignee": nil,
	}, "alice")
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, cleared).Assignee, "")
	// A clear takes the issue from bob and leaves it with nobody, which is not nobody: its
	// questions fall to CORE's default, alice, so the write names both people.
	clearedPayload := newestEventPayload[issueEventFields](t, database, "issue.updated", key)
	if !slices.Equal(clearedPayload.ThreadLogins, []string{"alice", "bob"}) {
		t.Fatalf("the clear's thread_logins = %#v, want [alice bob]", clearedPayload.ThreadLogins)
	}
}

// One PATCH that moves the status and the assignee together names the union of both sets,
// sorted. Through a request the status half is empty whenever an assignee rides with it: only a
// close or a reopen names anybody, a close withdraws the issue's open asks before this read, and
// a closed issue refuses an assignee, so a reopen carries none. The union of two sets that both
// have members is pinned on the read the event's own insert makes (events.IssueLoginsOf, the
// assignees the write moved unioned into the logins of the issue's open asks), over an issue
// whose open ask alice holds a Replies row on: alice is in both sets and is named once.
func TestPatchMovingStatusAndAssigneeNamesBothMovesLogins(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Handed over", "A spec")
	ask := createAnswerListAsk(t, handler, issue.Key, "Still needed?")
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "Is this still needed?", "ask_id": ask.ID}, "alice")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"status": "in_progress", "assignee": "bob",
	}, "bob"); response.Code != http.StatusOK {
		t.Fatalf("start and reassign: status=%d body=%s", response.Code, response.Body.String())
	}
	handed := newestEventPayload[issueEventFields](t, database, "issue.updated", issue.Key)
	if want := []string{"status", "assignee"}; !slices.Equal(handed.Changed, want) {
		t.Fatalf("the write's changed = %#v, want %#v", handed.Changed, want)
	}
	if want := []string{"alice", "bob"}; !slices.Equal(handed.ThreadLogins, want) {
		t.Fatalf("the write's thread_logins = %#v, want %#v: the status move names nobody and the issue went from alice to bob", handed.ThreadLogins, want)
	}

	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	query, args := events.IssueLoginsOf(issue.Key, movedLogins(nil, "alice", "carol")...).SQL(1)
	logins := []string{}
	if err := tx.QueryRow(context.Background(), query, args...).Scan(&logins); err != nil {
		t.Fatalf("read the write's logins: %v", err)
	}
	if want := []string{"alice", "carol"}; !slices.Equal(logins, want) {
		t.Fatalf("a close that left the ask open, handing the issue from alice to carol, names %#v; want %#v, alice once", logins, want)
	}
}

// An issue nobody holds is not an issue whose questions sit in nobody's Inbox: they reach the
// default assignee of its project, the chain's last step (askedOfPersonExpression). A write
// that gives such an issue an assignee takes those rows out of that person's list, so the event
// names them beside the new assignee; naming only the new one would leave the default's open
// Inbox holding a row the server no longer gives them.
func TestAssigningAnIssueNobodyHeldNamesTheProjectDefault(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	// CORE is created by alice, so she is the person its unowned work lands on.
	createAssigneeProject(t, handler)
	unowned := unassignIssue(t, handler, createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Nobody's",
	}).Key)
	expectAssignee(t, unowned.Assignee, "")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+unowned.Key,
		map[string]any{"assignee": "bob"}, "bob"); response.Code != http.StatusOK {
		t.Fatalf("hand the issue to bob: status=%d body=%s", response.Code, response.Body.String())
	}
	assigned := newestEventPayload[issueEventFields](t, database, "issue.updated", unowned.Key)
	if want := []string{"alice", "bob"}; !slices.Equal(assigned.ThreadLogins, want) {
		t.Fatalf("the write's thread_logins = %#v, want %#v: the project's default loses the issue's questions and bob gains them",
			assigned.ThreadLogins, want)
	}
}

// A summary carries the issue's assignee: the person who filed it, and null for an issue nobody
// holds, which only a PATCH clearing its assignee leaves (a create always lands on a person).
func TestListIssuesSummariesCarryAssignee(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Listed",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", response.Code, response.Body.String())
	}
	unassignIssue(t, handler, createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Nobody's",
	}).Key)
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	summaries := decodeBody[[]struct {
		Title    string  `json:"title"`
		Assignee *string `json:"assignee"`
	}](t, listed)
	byTitle := map[string]*string{}
	for _, summary := range summaries {
		byTitle[summary.Title] = summary.Assignee
	}
	expectAssignee(t, byTitle["Listed"], "alice")
	if _, present := byTitle["Nobody's"]; !present {
		t.Fatalf("unassigned issue missing from listing: %#v", summaries)
	}
	expectAssignee(t, byTitle["Nobody's"], "")
}

// GET /users is the picker's option list: everyone who has signed in (`people`), sorted. It needs
// the person scope, so the deployment's shared token is turned away and a person's own token
// reads the same list their browser reads.
func TestListUsersReturnsSignedInPeopleToThePersonScope(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/users", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list users: status=%d body=%s", response.Code, response.Body.String())
	}
	users := decodeBody[struct {
		Users []struct {
			Login string `json:"login"`
		} `json:"users"`
	}](t, response)
	if len(users.Users) != 2 || users.Users[0].Login != "alice" || users.Users[1].Login != "bob" {
		t.Fatalf("users = %#v, want alice then bob", users)
	}
	denied := agentRequest(t, handler, http.MethodGet, "/api/v1/users", nil, "agent-token")
	if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), `"code":"`+codeScopeRequired+`"`) {
		t.Fatalf("the shared token lists users: status=%d body=%s, want 403 %s", denied.Code, denied.Body.String(), codeScopeRequired)
	}
	own := agentRequest(t, handler, http.MethodGet, "/api/v1/users", nil,
		personalTokenHolding(t, database, model.ScopePerson))
	if own.Code != http.StatusOK || !strings.Contains(own.Body.String(), `"login":"alice"`) {
		t.Fatalf("a person's own token lists users: status=%d body=%s", own.Code, own.Body.String())
	}
}

// With nobody in `people`, GET /users is an empty list, never null: the picker maps over it.
func TestListUsersAnswersAnEmptyListWhenNobodyHasSignedIn(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	t.Cleanup(Register(mux, t.Context(), Deps{
		Store:    storetest.Open(t),
		Identity: identity.HeaderIdentity{People: unrecordedPeople{}},
	}).Wait)
	response := dispatchRequest(t, mux, http.MethodGet, "/api/v1/users", nil, "alice")
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"users":[]}` {
		t.Fatalf("list users with nobody signed in: status=%d body=%s", response.Code, response.Body.String())
	}
}

// GET /whoami tells a caller who the server takes it for: a human by lowercase email, whatever
// casing the identity header carried, a personal token by its owner, the shared token by a null
// owner.
func TestWhoamiNamesHumanPersonalTokenOwnerAndSharedToken(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	type whoami struct {
		Kind  string  `json:"kind"`
		Login string  `json:"login"`
		Owner *string `json:"owner"`
	}
	human := dispatchRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "Alice")
	if human.Code != http.StatusOK {
		t.Fatalf("human whoami: status=%d body=%s", human.Code, human.Body.String())
	}
	if got := decodeBody[whoami](t, human); got.Kind != "user" || got.Login != "alice" {
		t.Fatalf("human whoami = %#v", got)
	}
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens", map[string]string{"name": "planner"}, "Bob")
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	personal := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, decodeBody[agentTokenResponse](t, minted).Token)
	if personal.Code != http.StatusOK {
		t.Fatalf("personal whoami: status=%d body=%s", personal.Code, personal.Body.String())
	}
	if got := decodeBody[whoami](t, personal); got.Kind != "agent" || got.Owner == nil || *got.Owner != "bob" {
		t.Fatalf("personal-token whoami = %#v, want agent owned by bob", got)
	}
	shared := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "agent-token")
	if shared.Code != http.StatusOK {
		t.Fatalf("shared whoami: status=%d body=%s", shared.Code, shared.Body.String())
	}
	if got := decodeBody[whoami](t, shared); got.Kind != "agent" || got.Owner != nil {
		t.Fatalf("shared-token whoami = %#v, want agent with null owner", got)
	}
	if anonymous := agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "wrong"); anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("unknown bearer whoami: status=%d body=%s", anonymous.Code, anonymous.Body.String())
	}
}

// GET /whoami answers every caller the OAuth scopes its own credential holds, which is what
// decides the routes it may call (gateRouteScopes): every scope of a person's own credential for a
// person at a browser, p0 among them, the set its own row names for a personal token, p0 where its
// maker gave it, none at all for the two credentials a deployment configures rather than a person,
// and legion-status alone for a Legion daemon's own broker token, whose broker granted it. No
// broker token, shared token or service-account token holds p0. A named broker token that names a
// person and no project holds none, even when its answer holds legion-status: Dispatch reads that
// scope only on a token scoped to one project. A credential holding none is answered an empty
// array and never null, so a reader need not tell "this credential holds no scope" from "this
// build does not say". A personal token holding the person scope is still kind `agent`, since the
// kind names the credential and the owner names the person it acts for. Only an access token the
// secrets broker issued is answered a project_scope, its own: the two broker rows answer named,
// and every other row answers none, which the dispatch command reads as the credential carrying
// none. The server reads broker tokens through a stub broker (newBrokerTokenRigOn), so one server
// answers every row.
func TestWhoamiAnswersTheScopesEachCredentialHolds(t *testing.T) {
	t.Parallel()
	issuer := newServiceTokenIssuer(t)
	rig := newBrokerTokenRigOn(t, testServerOptions{oidc: issuer.verifier(t)})
	handler, database := rig.handler, rig.database
	for _, test := range []struct {
		name   string
		call   func() *httptest.ResponseRecorder
		kind   string
		scopes []string
		// projectScope is the project_scope the answer carries, empty for none.
		projectScope string
	}{
		{
			name: "a person signed in at a browser",
			call: func() *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "alice")
			},
			kind:   "user",
			scopes: []string{model.ScopePerson, model.ScopeApprove, model.ScopeP0},
		},
		{
			name: "a personal token holding both scopes",
			call: func() *httptest.ResponseRecorder {
				token := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove)
				return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, token)
			},
			kind:   "agent",
			scopes: []string{model.ScopePerson, model.ScopeApprove},
		},
		{
			name: "a personal token holding the person scope alone",
			call: func() *httptest.ResponseRecorder {
				token := personalTokenHolding(t, database, model.ScopePerson)
				return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, token)
			},
			kind:   "agent",
			scopes: []string{model.ScopePerson},
		},
		{
			name: "a personal token holding p0 alone",
			call: func() *httptest.ResponseRecorder {
				token := personalTokenHolding(t, database, model.ScopeP0)
				return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, token)
			},
			kind:   "agent",
			scopes: []string{model.ScopeP0},
		},
		{
			name: "a personal token holding person, approve and p0",
			call: func() *httptest.ResponseRecorder {
				token := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove, model.ScopeP0)
				return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, token)
			},
			kind:   "agent",
			scopes: []string{model.ScopePerson, model.ScopeApprove, model.ScopeP0},
		},
		{
			name: "the deployment's shared token",
			call: func() *httptest.ResponseRecorder {
				return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, "agent-token")
			},
			kind:   "agent",
			scopes: []string{},
		},
		{
			name: "a verified service-account token",
			call: func() *httptest.ResponseRecorder {
				return agentRequest(t, handler, http.MethodGet, "/api/v1/whoami", nil, issuer.mint(t, serviceTokenAudience))
			},
			kind:   "agent",
			scopes: []string{},
		},
		{
			name: "a Legion daemon's own broker token, named, whose broker granted legion-status",
			call: func() *httptest.ResponseRecorder {
				caller := newBrokerCaller(t)
				rig.broker.Says("the-daemon-token", liveDaemonToken(caller.jkt))
				return rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", "the-daemon-token", nil))
			},
			kind:         "agent",
			scopes:       []string{"legion-status"},
			projectScope: ciphertoken.ProjectScopeNamed,
		},
		{
			name: "a named broker token whose answer holds legion-status and names a person and no project",
			call: func() *httptest.ResponseRecorder {
				caller := newBrokerCaller(t)
				token := liveDaemonToken(caller.jkt)
				token.Projects = nil
				rig.broker.Says("the-projectless-token", token)
				return rig.serve(caller.presenting(t, http.MethodGet, "/api/v1/whoami", "the-projectless-token", nil))
			},
			kind:         "agent",
			scopes:       []string{},
			projectScope: ciphertoken.ProjectScopeNamed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := test.call()
			if response.Code != http.StatusOK {
				t.Fatalf("whoami as %s: status=%d body=%s", test.name, response.Code, response.Body.String())
			}
			got := decodeBody[struct {
				Kind string `json:"kind"`
				// A pointer, so a null reads differently from the empty array the answer owes a
				// credential that holds no scope.
				Scopes       *[]string `json:"scopes"`
				ProjectScope *string   `json:"project_scope"`
			}](t, response)
			if got.Kind != test.kind {
				t.Fatalf("whoami as %s = kind %q, want %q: %s", test.name, got.Kind, test.kind, response.Body.String())
			}
			if got.Scopes == nil {
				t.Fatalf("whoami as %s answered no scopes array: %s", test.name, response.Body.String())
			}
			if !slices.Equal(*got.Scopes, test.scopes) {
				t.Fatalf("whoami as %s = scopes %v, want %v", test.name, *got.Scopes, test.scopes)
			}
			switch {
			case test.projectScope == "" && got.ProjectScope != nil:
				t.Fatalf("whoami as %s = project_scope %q, want none: only a broker-issued token carries one: %s", test.name, *got.ProjectScope, response.Body.String())
			case test.projectScope != "" && (got.ProjectScope == nil || *got.ProjectScope != test.projectScope):
				t.Fatalf("whoami as %s = project_scope %v, want %q: %s", test.name, got.ProjectScope, test.projectScope, response.Body.String())
			}
		})
	}
}

// GET /whoami names read among the scopes of every read-only credential, read off the
// credential's read-only mark and never stored among the scopes it holds, and answers the cut it
// reads at, null for none: the read entry's token, a token narrowed from each kind of trade the
// broker narrows (the golden file the broker's own test holds its narrowed answers to), and among
// them the machine login's narrowed token with a cut, and a read-only personal token of each set
// such a token may hold (readOnlyPersonalTokens). A read-person token that names its person, and a
// personal token holding person beside read, are told person beside read, and no read-only token
// is told approve. No read-only token is told that a person must sign in (sign_in_required
// false), though several name a service and no person as a machine nobody signed in on does: no
// sign-in lets a read-only token write. The control is a token that writes, told neither read nor
// a cut.
//
// Each token is sent through the mounted handler, as a client sends it: the read-only gate lets
// every read-only token through to whoami, a token that carries a cut included, and the request
// resolves its token once (decideBrokerToken), so the sign-in decision whoami answers reads the
// same caller its scopes and cut are read from. A handler called with no such request would
// resolve the token a second time on a spent proof and read no caller there at all.
func TestWhoamiNamesReadAndTheCutOfEveryReadOnlyToken(t *testing.T) {
	t.Parallel()
	rig := newBrokerTokenRig(t)
	caller := newBrokerCaller(t)
	const session = "ses_narrowed"
	answers := ciphertokentest.NarrowedAnswers(t, caller.jkt, session, time.Now().Add(time.Hour).Unix())
	read := []string{model.ScopeRead}
	personAndRead := []string{model.ScopePerson, model.ScopeRead}
	wantScopes := map[string][]string{
		"pod-entry":                     read,
		"pod-entry-no-person":           read,
		"read-entry":                    read,
		"workflow-entry":                read,
		"google-identity-no-person":     read,
		"google-identity-with-a-person": read,
		"person-sign-in":                personAndRead,
		"machine-login":                 personAndRead,
		"machine-login-with-a-cut":      personAndRead,
	}
	if kinds := slices.Sorted(maps.Keys(answers)); !slices.Equal(kinds, slices.Sorted(maps.Keys(wantScopes))) {
		t.Fatalf("the golden file answers for %v, and this test names the scopes of %v", kinds, slices.Sorted(maps.Keys(wantScopes)))
	}
	type row struct {
		name   string
		answer ciphertoken.Introspection
		// bearer is a personal token, presented as a bearer, and empty for a token the stub
		// broker answers for with answer.
		bearer string
		scopes []string
		// cut is the cut whoami answers, empty for null.
		cut string
	}
	rows := []row{
		{name: "the read entry's token", answer: readOnlyToken(caller.jkt), scopes: read},
		{name: "a token that writes", answer: liveNamedToken(caller.jkt), scopes: []string{}},
	}
	for _, kind := range slices.Sorted(maps.Keys(answers)) {
		rows = append(rows, row{name: "a token narrowed from a " + kind, answer: answers[kind], scopes: wantScopes[kind], cut: answers[kind].AsOf})
	}
	if !slices.ContainsFunc(rows, func(r row) bool { return r.cut != "" }) {
		t.Fatal("no row names a cut, and one must")
	}
	for index, personal := range readOnlyPersonalTokens(t, rig) {
		rows = append(rows, row{name: personal.name, bearer: personal.token, scopes: readOnlyPersonalTokenSets[index].whoami})
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			request := brokerRequest(t, http.MethodGet, "/api/v1/whoami", nil, "Bearer "+row.bearer, "")
			if row.bearer == "" {
				token := "a-token-of-" + strings.ReplaceAll(row.name, " ", "-")
				rig.broker.Says(token, row.answer)
				request = caller.presenting(t, http.MethodGet, "/api/v1/whoami", token, nil)
			}
			response := rig.serve(request)
			if response.Code != http.StatusOK {
				t.Fatalf("whoami: status=%d body=%s", response.Code, response.Body.String())
			}
			var members map[string]json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &members); err != nil {
				t.Fatalf("decode whoami: %v: %s", err, response.Body.String())
			}
			var got struct {
				Kind   string    `json:"kind"`
				Scopes *[]string `json:"scopes"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode whoami: %v: %s", err, response.Body.String())
			}
			if got.Kind != "agent" || got.Scopes == nil || !slices.Equal(*got.Scopes, row.scopes) {
				t.Fatalf("whoami = %s, want kind agent and scopes %v", response.Body.String(), row.scopes)
			}
			wantCut := "null"
			if row.cut != "" {
				wantCut = `"` + row.cut + `"`
			}
			if answered, present := members["cut"]; !present || string(answered) != wantCut {
				t.Fatalf("whoami cut = %s (present %v), want %s: %s", answered, present, wantCut, response.Body.String())
			}
			if answered, present := members["sign_in_required"]; !present || string(answered) != "false" {
				t.Fatalf("whoami sign_in_required = %s (present %v), want false: %s", answered, present, response.Body.String())
			}
		})
	}
}

// createSharedTokenIssue creates an issue as the shared token, which acts for nobody: the chain
// then rests on the parent and the project's default.
func createSharedTokenIssue(t *testing.T, handler http.Handler, body map[string]any) assigneeIssue {
	t.Helper()
	body["actor"] = sessionActor()
	response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "agent-token")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %v: status=%d body=%s", body["title"], response.Code, response.Body.String())
	}
	return decodeBody[assigneeIssue](t, response)
}

// unassignIssue leaves the issue key names held by nobody, through PATCH /api/v1/issues/{key}
// with a null assignee, the one write that does: every create lands on a person, a create that
// sends a null assignee included (defaultAssignee, api/issue_assignee.go), so a test that needs
// an issue nobody holds creates one and then clears it. It answers the issue the PATCH wrote.
func unassignIssue(t *testing.T, handler http.Handler, key string) model.Issue {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key,
		map[string]any{"assignee": nil}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("clear the assignee of %s: status=%d body=%s", key, response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// The project's default is the chain's last step: an issue the shared token creates with no
// assignee, under no parent, goes to it. A parent whose assignee was cleared has nobody on it, so
// its child reaches the default too.
func TestCreateIssueFallsBackToProjectDefaultAssignee(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	putDefaultAssignee(t, handler, "CORE", "Bob")

	root := createSharedTokenIssue(t, handler, map[string]any{"project": "CORE", "title": "Root"})
	expectAssignee(t, root.Assignee, "bob")

	unowned := unassignIssue(t, handler, createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Nobody's parent",
	}).Key)
	expectAssignee(t, unowned.Assignee, "")
	child := createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Child of nobody", "parent": unowned.Key,
	})
	expectAssignee(t, child.Assignee, "bob")
}

// Every earlier step of the chain wins over the project's default, in order, and a create that
// sends a null assignee names nobody, so it reaches the default as one that sends none does.
func TestProjectDefaultAssigneeYieldsToEveryEarlierChainStep(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	createAssigneeProject(t, handler)
	putDefaultAssignee(t, handler, "CORE", "bob")

	explicit := createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Naming someone wins", "assignee": "alice",
	})
	expectAssignee(t, explicit.Assignee, "alice")

	byHuman := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "CORE", "title": "Filing it yourself",
	}, "alice")
	if byHuman.Code != http.StatusCreated {
		t.Fatalf("create as human: status=%d body=%s", byHuman.Code, byHuman.Body.String())
	}
	expectAssignee(t, decodeBody[assigneeIssue](t, byHuman).Assignee, "alice")

	parent := createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "A parent with somebody", "assignee": "alice",
	})
	child := createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Inheriting from above", "parent": parent.Key,
	})
	expectAssignee(t, child.Assignee, "alice")

	nobody := createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Nobody at all", "assignee": nil,
	})
	expectAssignee(t, nobody.Assignee, "bob")
}
