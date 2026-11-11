package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// publishDesignGatesResponse publishes on one issue the way the deployment's daemon does: over the
// credential the deployment configured, which acts for a machine and holds no personal scope. The
// body is given whole, so a row can send what a daemon never would.
func publishDesignGatesResponse(t *testing.T, handler http.Handler, issueKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return agentRequest(t, handler, http.MethodPut, "/api/v1/issues/"+issueKey+"/design-gates", body, "agent-token")
}

// publishDesignGates publishes an issue's whole set of gates and answers what the route stored.
func publishDesignGates(t *testing.T, handler http.Handler, issueKey string, gates []issueDesignGate) issueDesignGates {
	t.Helper()
	response := publishDesignGatesResponse(t, handler, issueKey, map[string]any{"gates": gates, "actor": sessionActor()})
	if response.Code != http.StatusOK {
		t.Fatalf("publish %v on %s: status=%d body=%s", gates, issueKey, response.Code, response.Body.String())
	}
	return decodeBody[issueDesignGates](t, response)
}

// recordedDesignGates reads an issue's stored set back off GET /api/v1/issues/{key}, which is where
// a reader of Dispatch finds what each of the issue's gated steps records, rather than off the
// answer the publish gave back.
func recordedDesignGates(t *testing.T, handler http.Handler, issueKey string) []issueDesignGate {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issueKey, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read %s: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		DesignGates []issueDesignGate `json:"design_gates"`
	}](t, response).DesignGates
}

// setIssueStatus moves an issue to one status of the lifecycle, as a person moves it in the
// dashboard; closeIssue is the `done` case of it, with the answer's own check beside it.
func setIssueStatus(t *testing.T, handler http.Handler, issueKey, status string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issueKey,
		map[string]string{"status": status}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("move %s to %s: status=%d body=%s", issueKey, status, response.Code, response.Body.String())
	}
}

// createWorkingIssue is createAdviceIssue with the issue moved into the workflow, which is where
// a tree whose gate holds a READY back stands. An issue Dispatch still shows in `triage` is out
// of the workflow, so its gates hold nothing back and the advice names none of them: a row that
// left its issue there would pass whatever the record said.
func createWorkingIssue(t *testing.T, handler http.Handler, project, title string, spec *string) struct {
	Key               string           `json:"key"`
	PrimaryArtifactID string           `json:"primary_artifact_id"`
	Advice            *testWriteAdvice `json:"advice"`
} {
	t.Helper()
	issue := createAdviceIssue(t, handler, project, title, spec)
	setIssueStatus(t, handler, issue.Key, "in_progress")
	return issue
}

// issueEventTypes is the issue's activity as a reader of it sees it, by event type, so a publish
// that moved that feed can be told from one that left it where it was.
func issueEventTypes(t *testing.T, handler http.Handler, issueKey string) []string {
	t.Helper()
	types := []string{}
	for _, event := range issueEvents(t, handler, issueKey) {
		types = append(types, event.Type)
	}
	return types
}

// createDesignGateChild creates a child issue with a spec of its own, the shape a hand-over leaves:
// the root's gate recorded the root's own document, and the child carries the work on.
func createDesignGateChild(t *testing.T, handler http.Handler, project, parent, title string) struct {
	Key               string `json:"key"`
	PrimaryArtifactID string `json:"primary_artifact_id"`
} {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": title, "parent": parent, "force": true,
		"spec": "# Child\n\nThe child's own design, as approved.\n",
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create the child of %s: status=%d body=%s", parent, response.Code, response.Body.String())
	}
	return decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, response)
}

// supersededByEdit edits the document as a session, the way an agent's `dispatch doc-edit` reaches
// that route, and answers the approval that edit superseded. Every row below is about what rides
// that report, so a write that superseded nothing fails here rather than further down.
func supersededByEdit(t *testing.T, handler http.Handler, artifactID, markdown string, approved int) *supersededApproval {
	t.Helper()
	superseded := editAdviceDocument(t, handler, artifactID, markdown).SupersededApproval
	if superseded == nil || superseded.ApprovedVersion != approved || superseded.Version != approved+1 {
		t.Fatalf("superseded_approval = %#v, want version %d superseding the approval of %d",
			superseded, approved+1, approved)
	}
	return superseded
}

// requireNoDesignGates fails unless the report names no gate. An absent list and an empty one say
// the one thing a reader acts on - Dispatch's record names no gate for this document - so the row
// is written over the list's length and not over which of the two the write sent.
func requireNoDesignGates(t *testing.T, superseded *supersededApproval, what string) {
	t.Helper()
	if len(superseded.DesignGates) != 0 {
		t.Fatalf("%s reports design_gates %#v, want none", what, superseded.DesignGates)
	}
}

// designGateEditRequest builds the one insert at a document's end that editAdviceDocument sends, as
// a request its caller serves itself. The row that locks the gate read out runs the edit on a
// goroutine of its own, and a request built in there would have to fail the test from it.
func designGateEditRequest(t *testing.T, artifactID, markdown string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"actor": adviceSessionActor("session-editor"),
		"ops":   []map[string]any{{"op": "insert", "markdown": markdown, "after": "end"}},
	})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	return request
}

// What a Legion deployment's gate records on an issue is what decides the design-gate clause of
// Dispatch's `superseded_approval` advice. The daemon publishes the document each gated step
// records (PUT /api/v1/issues/{key}/design-gates), the issue read answers that record, and a write
// that supersedes a person's approval names the live issues whose gates record the document it
// rewrote, on a step that deployment gates, and stays quiet about every other document. The rows
// below are that whole path, driven over the routes a daemon, a person and an agent each reach,
// and never over the table itself.
func TestDesignGateRecordDecidesTheSupersededApprovalAdvice(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createAdviceProject(t, handler, "DGATE")
	// The deployment serving DGATE gates all three steps, so the rows below turn on the record
	// alone; the two rows at the end are the ones that move this list.
	publishGatedSteps(t, handler, "DGATE", []string{model.GatedStepDecisions, model.GatedStepCriteria, model.GatedStepSpec})
	spec := "# Design\n\nThe design, as approved.\n"

	t.Run("a publish replaces the issue's whole set", func(t *testing.T) {
		issue := createAdviceIssue(t, handler, "DGATE", "Replaced set", &spec)
		decisions := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "decisions.md", "# Decisions\n\nOne.\n")
		criteria := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne.\n")

		both := []issueDesignGate{
			{Step: model.GatedStepDecisions, ArtifactID: decisions.Artifact.ID},
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
		}
		publishDesignGates(t, handler, issue.Key, both)
		if got := recordedDesignGates(t, handler, issue.Key); !slices.Equal(got, both) {
			t.Fatalf("%s records %#v, want %#v", issue.Key, got, both)
		}

		// The daemon publishes the issue's whole set, so a publish that names the Verification
		// document alone is the set the issue has: the two rows above are gone, not merged with.
		criteriaOnly := []issueDesignGate{{Step: model.GatedStepCriteria, ArtifactID: criteria.Artifact.ID}}
		publishDesignGates(t, handler, issue.Key, criteriaOnly)
		if got := recordedDesignGates(t, handler, issue.Key); !slices.Equal(got, criteriaOnly) {
			t.Fatalf("%s records %#v after the replacing publish, want %#v", issue.Key, got, criteriaOnly)
		}
	})

	t.Run("an empty list removes every row", func(t *testing.T) {
		issue := createAdviceIssue(t, handler, "DGATE", "Cleared set", &spec)
		gated := []issueDesignGate{{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID}}
		publishDesignGates(t, handler, issue.Key, gated)
		if got := recordedDesignGates(t, handler, issue.Key); !slices.Equal(got, gated) {
			t.Fatalf("%s records %#v, want %#v", issue.Key, got, gated)
		}

		// A deployment whose gate no longer records anything here says so with `[]`, which is the
		// one publish that clears an issue's record.
		publishDesignGates(t, handler, issue.Key, []issueDesignGate{})
		if got := recordedDesignGates(t, handler, issue.Key); len(got) != 0 {
			t.Fatalf("%s records %#v after the empty publish, want none", issue.Key, got)
		}
	})

	t.Run("a document of another issue is stored as given", func(t *testing.T) {
		root := createAdviceIssue(t, handler, "DGATE", "Handed-over root", &spec)
		child := createDesignGateChild(t, handler, "DGATE", root.Key, "Hand-over child")

		// A hand-over records the root's own document on the child, so the document a gate names
		// need not be one of the issue that records it, and Dispatch stores the id it was given.
		handed := []issueDesignGate{{Step: model.GatedStepSpec, ArtifactID: root.PrimaryArtifactID}}
		publishDesignGates(t, handler, child.Key, handed)
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+child.Key, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s: status=%d body=%s", child.Key, response.Code, response.Body.String())
		}
		read := decodeBody[struct {
			DesignGates []issueDesignGate `json:"design_gates"`
			Artifacts   []struct {
				ID string `json:"id"`
			} `json:"artifacts"`
		}](t, response)
		if !slices.Equal(read.DesignGates, handed) {
			t.Fatalf("%s records %#v, want the root's document %#v", child.Key, read.DesignGates, handed)
		}
		for _, document := range read.Artifacts {
			if document.ID == root.PrimaryArtifactID {
				t.Fatalf("%s holds the root's document itself, so the row proves nothing about a document of another issue", child.Key)
			}
		}
	})

	t.Run("an issue Dispatch does not know is refused ISSUE_NOT_FOUND", func(t *testing.T) {
		// The daemon ends a publication on this code alone, and a Dispatch serving no such route
		// answers the router's own 404 under the code NOT_FOUND, which it retries; so the code,
		// and not the status, is the whole of what it reads here.
		response := publishDesignGatesResponse(t, handler, "DGATE-9999", map[string]any{
			"gates": []issueDesignGate{}, "actor": sessionActor(),
		})
		code, _ := refusalOf(t, response)
		if response.Code != http.StatusNotFound || code != "ISSUE_NOT_FOUND" {
			t.Fatalf("publish on an unknown issue: status=%d body=%s, want 404 ISSUE_NOT_FOUND",
				response.Code, response.Body.String())
		}
	})

	t.Run("no publish appends an issue event", func(t *testing.T) {
		issue := createAdviceIssue(t, handler, "DGATE", "Quiet publish", &spec)
		// The creation's own document settlement is the only other writer of this issue's log;
		// waiting it out here leaves the publishes below as the only thing that could move it.
		if err := deps.Docs.Quiesce(context.Background()); err != nil {
			t.Fatalf("settle the issue's documents: %v", err)
		}
		before := issueEventTypes(t, handler, issue.Key)

		gated := []issueDesignGate{{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID}}
		publishDesignGates(t, handler, issue.Key, gated)
		if after := issueEventTypes(t, handler, issue.Key); !slices.Equal(after, before) {
			t.Fatalf("a publish that changed the set left %v, want the %v already there: the record is the daemon's own fact about its own gate", after, before)
		}
		// A daemon republishes at every boot, and a boot must leave every gated issue's feed where
		// it was.
		publishDesignGates(t, handler, issue.Key, gated)
		if after := issueEventTypes(t, handler, issue.Key); !slices.Equal(after, before) {
			t.Fatalf("a republish of the same set left %v, want the %v already there", after, before)
		}
	})

	t.Run("the credential the deployment configured publishes", func(t *testing.T) {
		issue := createAdviceIssue(t, handler, "DGATE", "Machine publish", &spec)
		// Such a credential holds no scope at all, so a row the person scope opens refuses it.
		if inbox := agentRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "agent-token"); inbox.Code != http.StatusForbidden {
			t.Fatalf("the deployment's credential on the inbox: status=%d body=%s, want 403", inbox.Code, inbox.Body.String())
		}
		// This row declares no personal scope, as the project's gated-steps row does, because what
		// it writes is what a machine's gate did and not a choice a person makes here.
		published := publishDesignGates(t, handler, issue.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
		})
		if published.Issue != issue.Key || len(published.Gates) != 1 || published.Gates[0].Step != model.GatedStepSpec {
			t.Fatalf("the publish answered %#v, want %s gating its spec", published, issue.Key)
		}
	})

	t.Run("the issue read answers the stored pairs in step order", func(t *testing.T) {
		issue := createAdviceIssue(t, handler, "DGATE", "Read in step order", &spec)
		if none := recordedDesignGates(t, handler, issue.Key); none == nil || len(none) != 0 {
			t.Fatalf("%s, whose gates record nothing, reads design_gates %#v, want []: a missing list would say the read never asked", issue.Key, none)
		}

		decisions := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "decisions.md", "# Decisions\n\nOne.\n")
		publishDesignGates(t, handler, issue.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
			{Step: model.GatedStepDecisions, ArtifactID: decisions.Artifact.ID},
		})
		// Published spec first, read back in the order a gate runs the steps.
		want := []issueDesignGate{
			{Step: model.GatedStepDecisions, ArtifactID: decisions.Artifact.ID},
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
		}
		if got := recordedDesignGates(t, handler, issue.Key); !slices.Equal(got, want) {
			t.Fatalf("%s records %#v, want %#v", issue.Key, got, want)
		}
	})

	t.Run("a Verification document no gate records", func(t *testing.T) {
		issue := createWorkingIssue(t, handler, "DGATE", "Approved verification", &spec)
		verification := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", "# Verification\n\nOne check.\n")
		approved := approveAdviceDocument(t, handler, verification.Artifact.ID)

		superseded := supersededByEdit(t, handler, verification.Artifact.ID, "One more check.", approved)
		requireNoDesignGates(t, superseded, "a Verification document no gate records")
	})

	t.Run("a plan a plan review approved, which no gate records", func(t *testing.T) {
		issue := createWorkingIssue(t, handler, "DGATE", "Approved plan", &spec)
		plan := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "plan.md", "# Plan\n\nOne task.\n")
		recorded := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+plan.Artifact.ID+"/plan-reviews",
			map[string]any{"actor": sessionActor(), "version": plan.Version.Number})
		if recorded.Code != http.StatusCreated {
			t.Fatalf("record the plan review: status=%d body=%s", recorded.Code, recorded.Body.String())
		}

		superseded := supersededByEdit(t, handler, plan.Artifact.ID, "One more task.", plan.Version.Number)
		requireNoDesignGates(t, superseded, "a plan a plan review approved")
	})

	t.Run("a child issue's primary document no gate records", func(t *testing.T) {
		parent := createWorkingIssue(t, handler, "DGATE", "Parent of the child", &spec)
		child := createDesignGateChild(t, handler, "DGATE", parent.Key, "Child with its own spec")
		approved := approveAdviceDocument(t, handler, child.PrimaryArtifactID)

		superseded := supersededByEdit(t, handler, child.PrimaryArtifactID, "A sentence the child gained.", approved)
		requireNoDesignGates(t, superseded, "a child issue's primary document")
	})

	t.Run("a live issue's gate names that issue and its step", func(t *testing.T) {
		issue := createWorkingIssue(t, handler, "DGATE", "Gated spec", &spec)
		publishDesignGates(t, handler, issue.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
		})
		approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

		superseded := supersededByEdit(t, handler, issue.PrimaryArtifactID, "A sentence the gate had not seen.", approved)
		want := []supersededApprovalGate{{Issue: issue.Key, Step: model.GatedStepSpec}}
		if !slices.Equal(superseded.DesignGates, want) {
			t.Fatalf("design_gates = %#v, want %#v: the write reopened that issue's spec gate", superseded.DesignGates, want)
		}
	})

	t.Run("two live issues' gates name both, each with its step", func(t *testing.T) {
		// Two projects, so the two keys order the list as plainly as a reader reads it: the gates
		// come back by issue key, and GATEA-1 stands before GATEB-1.
		createAdviceProject(t, handler, "GATEA")
		createAdviceProject(t, handler, "GATEB")
		publishGatedSteps(t, handler, "GATEA", []string{model.GatedStepSpec})
		publishGatedSteps(t, handler, "GATEB", []string{model.GatedStepCriteria})
		owner := createWorkingIssue(t, handler, "GATEA", "The issue the document belongs to", &spec)
		other := createWorkingIssue(t, handler, "GATEB", "The issue handed that same document", &spec)
		publishDesignGates(t, handler, owner.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: owner.PrimaryArtifactID},
		})
		publishDesignGates(t, handler, other.Key, []issueDesignGate{
			{Step: model.GatedStepCriteria, ArtifactID: owner.PrimaryArtifactID},
		})
		approved := approveAdviceDocument(t, handler, owner.PrimaryArtifactID)

		superseded := supersededByEdit(t, handler, owner.PrimaryArtifactID, "A sentence neither gate had seen.", approved)
		want := []supersededApprovalGate{
			{Issue: owner.Key, Step: model.GatedStepSpec},
			{Issue: other.Key, Step: model.GatedStepCriteria},
		}
		if !slices.Equal(superseded.DesignGates, want) {
			t.Fatalf("design_gates = %#v, want %#v: the write reopened both gates", superseded.DesignGates, want)
		}
	})

	t.Run("a done issue's gate beside a live one leaves the live one alone", func(t *testing.T) {
		live := createWorkingIssue(t, handler, "DGATE", "The gate still open", &spec)
		closed := createWorkingIssue(t, handler, "DGATE", "The gate that closed with its issue", &spec)
		publishDesignGates(t, handler, live.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: live.PrimaryArtifactID},
		})
		publishDesignGates(t, handler, closed.Key, []issueDesignGate{
			{Step: model.GatedStepCriteria, ArtifactID: live.PrimaryArtifactID},
		})
		closeIssue(t, handler, closed.Key)
		approved := approveAdviceDocument(t, handler, live.PrimaryArtifactID)

		superseded := supersededByEdit(t, handler, live.PrimaryArtifactID, "A sentence the open gate had not seen.", approved)
		want := []supersededApprovalGate{{Issue: live.Key, Step: model.GatedStepSpec}}
		if !slices.Equal(superseded.DesignGates, want) {
			t.Fatalf("design_gates = %#v, want %#v: a gate on a done issue holds no READY back", superseded.DesignGates, want)
		}
	})

	t.Run("a gate on an issue parked out of the workflow names none", func(t *testing.T) {
		// Nothing clears a parked issue's gate rows: the daemon suspends its claims and deletes
		// none, its boot republishes them for as long as it stays parked, and only the
		// re-admission that brings the tree back clears them. Every status that takes an issue
		// out of the workflow reads the same way here, done among them.
		for _, status := range model.IssueStatusesOutOfWorkflow {
			t.Run(status, func(t *testing.T) {
				live := createWorkingIssue(t, handler, "DGATE", "The gate still open", &spec)
				parked := createWorkingIssue(t, handler, "DGATE", "The gate of a tree that runs nothing", &spec)
				publishDesignGates(t, handler, live.Key, []issueDesignGate{
					{Step: model.GatedStepSpec, ArtifactID: live.PrimaryArtifactID},
				})
				publishDesignGates(t, handler, parked.Key, []issueDesignGate{
					{Step: model.GatedStepCriteria, ArtifactID: live.PrimaryArtifactID},
				})
				setIssueStatus(t, handler, parked.Key, status)
				approved := approveAdviceDocument(t, handler, live.PrimaryArtifactID)

				superseded := supersededByEdit(t, handler, live.PrimaryArtifactID, "A sentence only the open gate waits on.", approved)
				want := []supersededApprovalGate{{Issue: live.Key, Step: model.GatedStepSpec}}
				if !slices.Equal(superseded.DesignGates, want) {
					t.Fatalf("design_gates = %#v, want %#v: a gate on an issue in %s holds no READY back",
						superseded.DesignGates, want, status)
				}
			})
		}
	})

	t.Run("a gate only a done issue records names none", func(t *testing.T) {
		live := createWorkingIssue(t, handler, "DGATE", "The issue the document belongs to", &spec)
		closed := createWorkingIssue(t, handler, "DGATE", "The only issue whose gate records it", &spec)
		publishDesignGates(t, handler, closed.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: live.PrimaryArtifactID},
		})
		closeIssue(t, handler, closed.Key)
		approved := approveAdviceDocument(t, handler, live.PrimaryArtifactID)

		superseded := supersededByEdit(t, handler, live.PrimaryArtifactID, "A sentence no open gate waits on.", approved)
		requireNoDesignGates(t, superseded, "a document only a done issue's gate records")
	})

	t.Run("a cleared record names none", func(t *testing.T) {
		issue := createWorkingIssue(t, handler, "DGATE", "The gate the daemon withdrew", &spec)
		publishDesignGates(t, handler, issue.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
		})
		publishDesignGates(t, handler, issue.Key, []issueDesignGate{})
		approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

		superseded := supersededByEdit(t, handler, issue.PrimaryArtifactID, "A sentence written after the gate went.", approved)
		requireNoDesignGates(t, superseded, "a document whose gate record the daemon cleared")
	})

	t.Run("a gate on a step the deployment does not gate names none", func(t *testing.T) {
		// A daemon records a gate row for a step it does not gate - under `gates.design: off` the
		// registration records the spec step and approves it, and no later event ever moves it -
		// and such a gate refuses no READY. The project's own list is what says so.
		createAdviceProject(t, handler, "GATEONE")
		publishGatedSteps(t, handler, "GATEONE", []string{model.GatedStepSpec})
		issue := createWorkingIssue(t, handler, "GATEONE", "Gated on its spec alone", &spec)
		publishDesignGates(t, handler, issue.Key, []issueDesignGate{
			{Step: model.GatedStepCriteria, ArtifactID: issue.PrimaryArtifactID},
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
		})
		approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

		superseded := supersededByEdit(t, handler, issue.PrimaryArtifactID, "A sentence the spec gate had not seen.", approved)
		want := []supersededApprovalGate{{Issue: issue.Key, Step: model.GatedStepSpec}}
		if !slices.Equal(superseded.DesignGates, want) {
			t.Fatalf("design_gates = %#v, want %#v: the criteria gate of a deployment that gates the spec alone holds no READY back",
				superseded.DesignGates, want)
		}
	})

	t.Run("a project whose deployment gates nothing names none", func(t *testing.T) {
		// The empty list is what a deployment with `gates.design: off` publishes, and it is also
		// what a project reads as before its daemon has published anything at all.
		createAdviceProject(t, handler, "GATENONE")
		issue := createWorkingIssue(t, handler, "GATENONE", "Recorded by a daemon that gates nothing", &spec)
		publishDesignGates(t, handler, issue.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
		})
		approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

		superseded := supersededByEdit(t, handler, issue.PrimaryArtifactID, "A sentence no gate waits on.", approved)
		requireNoDesignGates(t, superseded, "a document a gate of a deployment that gates nothing records")
	})

	t.Run("a gate read that never answers costs the gate list and never the write", func(t *testing.T) {
		issue := createWorkingIssue(t, handler, "DGATE", "Locked-out gate read", &spec)
		publishDesignGates(t, handler, issue.Key, []issueDesignGate{
			{Step: model.GatedStepSpec, ArtifactID: issue.PrimaryArtifactID},
		})
		approved := approveAdviceDocument(t, handler, issue.PrimaryArtifactID)

		// The gate read is one statement on the advice savepoint, under the 500 ms
		// statement_timeout that savepoint sets. A lock no part of the write path takes holds that
		// one statement off until the bound ends it, which is the failure a stuck or slow read
		// would be, with nothing in the production path arranged for the test.
		lockTx, err := database.Pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin the lock's transaction: %v", err)
		}
		defer func() { _ = lockTx.Rollback(context.Background()) }()
		if _, err := lockTx.Exec(context.Background(), "lock table design_gates in access exclusive mode"); err != nil {
			t.Fatalf("lock design_gates: %v", err)
		}
		// The edit runs on a goroutine of its own so the lock's holder can watch the gate read
		// queue behind it while the write is still open.
		request := designGateEditRequest(t, issue.PrimaryArtifactID, "A sentence written while the gate read is locked out.")
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			answered <- response
		}()
		waitForDatabaseLocks(t, lockTx, 1)
		response := <-answered

		superseded := adviceFromResponse(t, response.Code, response.Body.String()).SupersededApproval
		if superseded == nil || superseded.ApprovedVersion != approved || superseded.Version != approved+1 {
			t.Fatalf("superseded_approval = %#v, want version %d superseding the approval of %d: the approval is true whatever Dispatch holds about gates",
				superseded, approved+1, approved)
		}
		requireNoDesignGates(t, superseded, "a write whose gate read was locked out")
		// The write itself commits: the version is stored, and the approval it superseded reads as
		// stale on the document a person opens next.
		if read := readApproval(t, handler, issue.PrimaryArtifactID); read.Approval == nil ||
			read.Approval.LatestVersion != approved+1 || read.Approval.State != "stale" {
			t.Fatalf("the document's approval = %#v, want version %d stored and that approval stale",
				read.Approval, approved+1)
		}
	})
}
