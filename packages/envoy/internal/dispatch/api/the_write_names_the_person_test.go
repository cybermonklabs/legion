package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A write that carries a question answers with the person that question reached, resolved as
// every read of it resolves (askedOfPersonExpression): the person it was handed to, then the
// assignee of its issue, then the default assignee of its project. The caller then knows whose
// Inbox its question landed in without reading the ask again, and an issue nobody holds no
// longer answers nobody. This file is the write half of dispatch://INFRA-3090's chain;
// TestEveryQuestionReachesAPerson holds that every question reaches somebody, and
// TestOneExpressionDecidesWhoWaits that every read of one question names the same person.

// openQuestion opens a question through target - an issue's ask route or a document's - and
// returns the ask the route answers with.
func openQuestion(t *testing.T, handler http.Handler, target, question string) model.Ask {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
		"question": question, "actor": sessionActor(),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("open %q on %s: status=%d body=%s", question, target, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response)
}

// approvalRequestAnswer asks a person to approve the document's latest version and returns the
// question the route answered with, nil where it answered none: 201 opens a request or hands an
// open one back, 200 answers a request that already stands as asked or one already settled.
func approvalRequestAnswer(t *testing.T, handler http.Handler, artifactID string, status int) *model.Ask {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+artifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if response.Code != status {
		t.Fatalf("request approval of %s: status=%d body=%s, want %d",
			artifactID, response.Code, response.Body.String(), status)
	}
	return decodeBody[struct {
		Ask *model.Ask `json:"ask"`
	}](t, response).Ask
}

func TestTheWriteNamesThePerson(t *testing.T) {
	handler := newTestHandler(t)
	// NAMES is created by alice, so she is the person it defaults to and holds the issue she
	// filed; the write below moves the default to bob, so the chain's last step and the step
	// above it name different people.
	held := createInteractionIssue(t, handler, "NAMES", "An issue alice holds", "A spec\n")
	putDefaultAssignee(t, handler, "NAMES", "bob")
	loose := specIssue(t, handler, "NAMES", "An issue nobody holds", "A spec\n")
	document := createProjectDocument(t, handler, "NAMES", "Design notes", "# Design notes\n")

	for _, opened := range []struct {
		// owner is what the question stands on, target the route that opens it there and
		// person whom the write must name.
		owner  string
		target string
		person string
	}{
		{"an issue a person holds", "/api/v1/issues/" + held.Key + "/asks", "alice"},
		{"an issue nobody holds", "/api/v1/issues/" + loose.Key + "/asks", "bob"},
		{"a project document", "/api/v1/artifacts/" + document.ID + "/asks", "bob"},
	} {
		question := openQuestion(t, handler, opened.target, "Which way on "+opened.owner+"?")
		if question.WaitingOnPerson != opened.person {
			t.Fatalf("the question created on %s answered waiting_on_person %q, want %q",
				opened.owner, question.WaitingOnPerson, opened.person)
		}
	}

	// The approval request answers the same way, for the document of an issue a person holds
	// and for a project document, which has no issue and rests on the project's default.
	issueRequest := approvalRequestAnswer(t, handler, held.PrimaryArtifactID, http.StatusCreated)
	if issueRequest == nil || issueRequest.WaitingOnPerson != "alice" {
		t.Fatalf("the request on the issue's document answered %#v, want a question waiting on alice", issueRequest)
	}
	documentRequest := approvalRequestAnswer(t, handler, document.ID, http.StatusCreated)
	if documentRequest == nil || documentRequest.WaitingOnPerson != "bob" {
		t.Fatalf("the request on the project document answered %#v, want a question waiting on bob", documentRequest)
	}

	// The common path: a second request against a document whose request is already open. The
	// human's reply takes the turn back to the agent, so this request hands the open question
	// back to her and answers it as the create answered - the same question, and the person it
	// still reaches.
	reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+held.Key+"/comments",
		map[string]any{"ask_id": issueRequest.ID, "body": "Say more about the rollout, please."}, "alice")
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply in the request's thread: status=%d body=%s", reply.Code, reply.Body.String())
	}
	handedBack := approvalRequestAnswer(t, handler, held.PrimaryArtifactID, http.StatusCreated)
	if handedBack == nil || handedBack.ID != issueRequest.ID || handedBack.WaitingOnPerson != "alice" {
		t.Fatalf("the hand-back answered %#v, want the open question %s waiting on alice", handedBack, issueRequest.ID)
	}

	// A request whose document the human has already approved at its current version settles:
	// it answers no question at all, and so names nobody.
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+documentRequest.ID+"/answer",
		map[string]any{"selected": []string{"Approve"}}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("approve the project document: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if settled := approvalRequestAnswer(t, handler, document.ID, http.StatusOK); settled != nil {
		t.Fatalf("the settled request answered the question %#v, want none", settled)
	}

	// The project create answers the default it took: the person creating it where the call
	// names none, and the login it names where it does.
	for _, created := range []struct {
		key    string
		body   map[string]string
		person string
	}{
		{"TOOK", map[string]string{"key": "TOOK", "name": "Took"}, "alice"},
		{"NAMED", map[string]string{"key": "NAMED", "name": "Named", "default_assignee": "Bob"}, "bob"},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", created.body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%s", created.key, response.Code, response.Body.String())
		}
		if project := decodeBody[model.Project](t, response); project.DefaultAssignee != created.person {
			t.Fatalf("creating %s answered default_assignee %q, want %q",
				created.key, project.DefaultAssignee, created.person)
		}
	}
}

// GET /api/v1/asks/open answers two people per row, and they are not the same one: the issue's
// own assignee, under the owner the row names, and the person the question reaches. For a
// question on an issue nobody holds the first is null and the second is the project's default,
// so an author reading the row can tell "nobody holds this issue" from "nobody is waiting on
// this question", which is now never true.
func TestOpenQuestionsAnswerTheAssignee(t *testing.T) {
	handler := newTestHandler(t)
	// Every question below is opened by this one session, the author the read answers for.
	const author = "session-0123456789abcdef"
	held := createInteractionIssue(t, handler, "OPEN", "An issue alice holds", "A spec\n")
	putDefaultAssignee(t, handler, "OPEN", "bob")
	loose := specIssue(t, handler, "OPEN", "An issue nobody holds", "A spec\n")
	heldQuestion := createIssueAsk(t, handler, held.Key, "Which way on the issue alice holds?")
	looseQuestion := createIssueAsk(t, handler, loose.Key, "Which way on the issue nobody holds?")

	rows := authorOpenAsks(t, handler, author)
	for _, want := range []struct {
		owner string
		askID string
		// assignee is the issue's own assignee, "" where the issue holds nobody, and person
		// is whom the question reaches.
		assignee string
		person   string
	}{
		{"an issue alice holds", heldQuestion, "alice", "alice"},
		{"an issue nobody holds", looseQuestion, "", "bob"},
	} {
		row, listed := rows[want.askID]
		if !listed {
			t.Fatalf("the author's open questions do not list the question on %s", want.owner)
		}
		if row.Owner.Issue == nil {
			t.Fatalf("the row of the question on %s names no issue: %#v", want.owner, row.Owner)
		}
		assignee := ""
		if row.Owner.Issue.Assignee != nil {
			assignee = *row.Owner.Issue.Assignee
		}
		if assignee != want.assignee {
			t.Fatalf("the row of the question on %s says owner.issue.assignee %q, want %q",
				want.owner, assignee, want.assignee)
		}
		if row.WaitingOnPerson != want.person {
			t.Fatalf("the row of the question on %s says waiting_on_person %q, want %q",
				want.owner, row.WaitingOnPerson, want.person)
		}
	}
}
