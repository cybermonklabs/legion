package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every issue a create writes has somebody on it. The chain defaultAssignee walks
// (api/issue_assignee.go) ends at the project's default assignee, which every project names
// (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), so a create that
// names nobody and runs for nobody still lands on a person: the step that used to leave the
// issue unassigned is gone. This file is the create half of dispatch://INFRA-3090; the question
// a read resolves is TestEveryQuestionReachesAPerson's.
//
// Each step of the chain is driven through POST /api/v1/issues and the issue is then read back,
// since the create's answer and the stored row must name the same person.
func TestANewIssueAlwaysHasAnAssignee(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	// carol is nobody's creator and holds no token here, so an issue reaching her reached the
	// project's default and nothing else.
	seedPeople(t, database, "carol")
	createAssigneeProject(t, handler)
	putDefaultAssignee(t, handler, "CORE", "carol")
	minted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/me/agent-tokens",
		map[string]string{"name": "planner"}, "bob")
	if minted.Code != http.StatusCreated {
		t.Fatalf("mint bob's token: status=%d body=%s", minted.Code, minted.Body.String())
	}
	bobsToken := decodeBody[agentTokenResponse](t, minted).Token
	parent := createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "A parent alice holds", "assignee": "alice",
	})

	for _, step := range []struct {
		// step is the step of the chain the create rests on, person whom it must reach.
		step   string
		person string
		create func() assigneeIssue
	}{
		{"the assignee the create names", "bob", func() assigneeIssue {
			return createSharedTokenIssue(t, handler, map[string]any{
				"project": "CORE", "title": "Named on the create", "assignee": "bob",
			})
		}},
		{"the person who filed it", "alice", func() assigneeIssue {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "CORE", "title": "Filed by a person",
			}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("create as a person: status=%d body=%s", response.Code, response.Body.String())
			}
			return decodeBody[assigneeIssue](t, response)
		}},
		{"the person the session acts for", "bob", func() assigneeIssue {
			response := agentRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "CORE", "title": "Filed by bob's session", "actor": sessionActor(),
			}, bobsToken)
			if response.Code != http.StatusCreated {
				t.Fatalf("create with bob's token: status=%d body=%s", response.Code, response.Body.String())
			}
			return decodeBody[assigneeIssue](t, response)
		}},
		{"the parent's assignee", "alice", func() assigneeIssue {
			return createSharedTokenIssue(t, handler, map[string]any{
				"project": "CORE", "title": "Under a parent", "parent": parent.Key,
			})
		}},
		{"the project's default", "carol", func() assigneeIssue {
			return createSharedTokenIssue(t, handler, map[string]any{
				"project": "CORE", "title": "Filed for nobody, under nobody",
			})
		}},
	} {
		t.Run(step.step, func(t *testing.T) {
			created := step.create()
			expectAssignee(t, created.Assignee, step.person)
			read := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet,
				"/api/v1/issues/"+created.Key, nil, "alice"))
			expectAssignee(t, read.Assignee, step.person)
		})
	}
}

// A create that sends assignee as an explicit JSON null names nobody, as one that sends no
// assignee does, so it walks the same chain (defaultAssignee) and lands on a person: the person
// the credential acts for, else the parent's assignee, else the project's default. Only a PATCH
// clears an issue's assignee (unassignIssue), and a parent left so is where the child's chain
// passes to the project's default.
//
// Each issue is read back, and the project's listing, which every issue summary carries its
// assignee on (GET /api/v1/issues has no assignee filter), names a person for every one of them.
func TestACreateNamingNobodyTakesTheDefault(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	// carol is nobody's creator and holds no token here, so an issue reaching her reached the
	// project's default and nothing else.
	seedPeople(t, database, "carol")
	createAssigneeProject(t, handler)
	putDefaultAssignee(t, handler, "CORE", "carol")
	unheldParent := unassignIssue(t, handler, createSharedTokenIssue(t, handler, map[string]any{
		"project": "CORE", "title": "Quarterly roadmap",
	}).Key)
	expectAssignee(t, unheldParent.Assignee, "")

	created := map[string]string{}
	for _, step := range []struct {
		// step is who sends the null, person whom the issue must reach.
		step   string
		person string
		create func() assigneeIssue
	}{
		{"a session acting for nobody, on a root issue", "carol", func() assigneeIssue {
			return createSharedTokenIssue(t, handler, map[string]any{
				"project": "CORE", "title": "Rotate the signing keys", "assignee": nil,
			})
		}},
		{"a person", "alice", func() assigneeIssue {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "CORE", "title": "Billing export format", "assignee": nil,
			}, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("create as a person: status=%d body=%s", response.Code, response.Body.String())
			}
			return decodeBody[assigneeIssue](t, response)
		}},
		{"a session acting for nobody, under a parent nobody holds", "carol", func() assigneeIssue {
			return createSharedTokenIssue(t, handler, map[string]any{
				"project": "CORE", "title": "Hire a second designer", "assignee": nil, "parent": unheldParent.Key,
			})
		}},
	} {
		t.Run(step.step, func(t *testing.T) {
			issue := step.create()
			expectAssignee(t, issue.Assignee, step.person)
			read := decodeBody[model.Issue](t, dispatchRequest(t, handler, http.MethodGet,
				"/api/v1/issues/"+issue.Key, nil, "alice"))
			expectAssignee(t, read.Assignee, step.person)
			created[issue.Key] = step.person
		})
	}

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CORE", nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", listed.Code, listed.Body.String())
	}
	listedAssignees := map[string]*string{}
	for _, summary := range decodeBody[[]assigneeIssue](t, listed) {
		listedAssignees[summary.Key] = summary.Assignee
	}
	if len(created) != 3 {
		t.Fatalf("created %d issues, want 3: %v", len(created), created)
	}
	for key, person := range created {
		assignee, present := listedAssignees[key]
		if !present {
			t.Fatalf("the listing leaves out %s: %v", key, listedAssignees)
		}
		expectAssignee(t, assignee, person)
	}
}
