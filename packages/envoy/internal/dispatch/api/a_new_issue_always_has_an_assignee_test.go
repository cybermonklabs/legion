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
