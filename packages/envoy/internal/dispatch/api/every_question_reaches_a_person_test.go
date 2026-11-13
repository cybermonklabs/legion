package api

import (
	"net/http"
	"testing"
)

// Every question Dispatch opens reaches a person, whatever write opened it and whatever it
// stands on. The chain askedOfPersonExpression resolves as a question is read ends at the
// default assignee of its project, and every project names one
// (store/migrations/20261111210103_projects_default_assignee_not_null.up.sql), so no write can
// leave a question nobody waits on. This file is the Go half of dispatch://INFRA-3090's
// c-one-chain that drives the writes; TestOneExpressionDecidesWhoWaits beside it drives the
// reads, holding that every read names the same person for one question.
//
// It reads each question back through the Inbox alone, which is the read that lists a question
// under the person it reaches, and asks that read for the questions nobody waits on, which is
// now none.
func TestEveryQuestionReachesAPerson(t *testing.T) {
	handler := newTestHandler(t)
	// REACH is created by alice (createInteractionIssue), so the person it defaults to is
	// alice: a question nobody was handed, on an issue nobody holds or on a project document,
	// reaches her. An issue bob holds names bob instead, one step earlier.
	held := createInteractionIssue(t, handler, "REACH", "An issue bob holds", "Context\n")
	if moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+held.Key,
		map[string]any{"assignee": "bob"}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("assign %s to bob: status=%d body=%s", held.Key, moved.Code, moved.Body.String())
	}
	loose := specIssue(t, handler, "REACH", "An issue nobody holds", "Context\n")
	document := createProjectDocument(t, handler, "REACH", "Design notes", "# Design notes\n")

	owners := []struct {
		// name is what the question stands on, person whom that makes it reach, issueKey the
		// issue that owns it (empty for the project document) and document the document each
		// document-addressed write is sent to.
		name     string
		person   string
		issueKey string
		document string
		block    string
	}{
		{"an issue bob holds", "bob", held.Key, held.PrimaryArtifactID, "chain-held"},
		{"an issue nobody holds", "alice", loose.Key, loose.PrimaryArtifactID, "chain-loose"},
		{"a project document", "alice", "", document.ID, "chain-document"},
	}

	// Each question with the write that opened it, so a failure names the write to fix.
	type question struct {
		id     string
		opened string
		owner  string
		person string
	}
	var questions []question
	open := func(owner int, id, opened string) {
		questions = append(questions, question{
			id: id, opened: opened, owner: owners[owner].name, person: owners[owner].person,
		})
	}

	// The three writes that open a question: the issue route, the project document's route -
	// which refuses an issue's own document, ARTIFACT_LINKED, so each question lands on the
	// owner that route serves - and the `:::ask` block a document write settles into an ask
	// (docs/ask_blocks.go). The blocks are written before the approval requests below, so no
	// request opens against a version a later edit moves past.
	for index, owner := range owners {
		if owner.issueKey != "" {
			open(index, createIssueAsk(t, handler, owner.issueKey, "A question on "+owner.name),
				"POST /api/v1/issues/{key}/asks")
		} else {
			open(index, createSnoozeAsk(t, handler, "/api/v1/artifacts/"+owner.document+"/asks",
				"A question on "+owner.name), "POST /api/v1/artifacts/{id}/asks")
		}
		blockQuestion := "Which chain reaches a person on " + owner.name + "?"
		written := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+owner.document+"/edits", map[string]any{
			"actor": sessionActor(),
			"ops": []map[string]string{{
				"op": "insert", "after": "end",
				"markdown": ":::ask{#" + owner.block + " urgency=\"med\" multiple=\"false\"}\n" + blockQuestion + "\n:::\n",
			}},
		})
		if written.Code != http.StatusOK {
			t.Fatalf("write the %q ask block: status=%d body=%s", owner.block, written.Code, written.Body.String())
		}
		awaitIndexedAskBlock(t, handler, owner.document, owner.block, blockQuestion)
		open(index, blockAskOf(t, handler, owner.document, owner.block), "a `:::ask` block of a document write")
	}
	// The fourth write: the request that asks a person to approve a document's latest version.
	for index, owner := range owners {
		open(index, requestApproval(t, handler, owner.document).ID,
			"POST /api/v1/artifacts/{id}/approval-requests")
	}

	rows := map[string]inboxAsk{}
	for _, row := range inboxFor(t, handler, "alice", "") {
		rows[row.ID] = row
	}
	for _, opened := range questions {
		row, listed := rows[opened.id]
		if !listed {
			t.Fatalf("the Inbox does not list the question %s opened on %s", opened.opened, opened.owner)
		}
		if row.WaitingOnPerson != opened.person {
			t.Fatalf("the question %s opened on %s reaches %q, want %q",
				opened.opened, opened.owner, row.WaitingOnPerson, opened.person)
		}
	}
	if len(questions) != len(rows) {
		t.Fatalf("the Inbox lists %d questions, want the %d these writes opened", len(rows), len(questions))
	}
	if nobodys := inboxFor(t, handler, "alice", "unassigned"); len(nobodys) != 0 {
		t.Fatalf("?assignee=unassigned holds %d questions, want none: %#v", len(nobodys), nobodys)
	}
}
