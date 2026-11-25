package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestASupersededApprovalNamesNoGateOfAProjectItsWriterDoesNotSee: a design gate stores the id of
// the document it records unread, so a gate of an issue of PRIV can record a document of PUB. A
// write that supersedes the approval of that document tells its writer the gates it closed
// (supersededApprovalGates), and alice, an open member who sees PUB alone, is told none of
// PRIV's: her edit of PUB's document reports the approval it superseded and no gate, as for a
// document no gate records. carol, who sees PRIV, makes the same edit and is told PRIV's gate.
//
// Control: the gate read without the writer's viewer (the access.ProjectClause line taken out of
// supersededApprovalGates) names PRIV's issue to alice.
func TestASupersededApprovalNamesNoGateOfAProjectItsWriterDoesNotSee(t *testing.T) {
	seed := seedPrivateProject(t)
	created := func(project, title string) struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	} {
		t.Helper()
		return decodeBody[struct {
			Key               string `json:"key"`
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": project, "title": title, "spec": "# Design\n\nThe design of " + project + ", as approved.\n",
		}, http.StatusCreated))
	}
	document := created(seedPublic, "A document PRIV's gate records").PrimaryArtifactID
	gated := created(seedPrivate, "PRIV's gated issue").Key
	// The gate holds a READY back only on an issue in the workflow, and the row says it holds its
	// tree, so the advice would name it to anyone who sees it.
	seed.mustRequest(t, callerCarol, http.MethodPatch, "/api/v1/issues/"+gated, map[string]string{"status": "in_progress"}, http.StatusOK)
	holds := true
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/issues/"+gated+"/design-gates", map[string]any{
		"gates": []issueDesignGate{{Step: model.GatedStepSpec, ArtifactID: document, Holds: &holds}},
	}, http.StatusOK)

	supersede := func(caller privateCaller, line string) *supersededApproval {
		t.Helper()
		approved := decodeBody[struct {
			Version int `json:"version"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/artifacts/"+document+"/reviews",
			map[string]any{"state": "approved"}, http.StatusCreated)).Version
		edited := seed.request(t, caller, http.MethodPost, "/api/v1/artifacts/"+document+"/edits", map[string]any{
			"ops": []map[string]any{{"op": "insert", "after": "end", "markdown": line}},
		})
		superseded := adviceFromResponse(t, edited.Code, edited.Body.String()).SupersededApproval
		if superseded == nil || superseded.ApprovedVersion != approved || superseded.Version != approved+1 {
			t.Fatalf("%s's edit reports superseded_approval %#v, want version %d superseding the approval of %d",
				caller, superseded, approved+1, approved)
		}
		return superseded
	}

	if got := supersede(callerAlice, "A sentence alice wrote.").DesignGates; len(got) != 0 {
		t.Errorf("alice's edit of %s's document names the gates %#v; she does not see %s, so its issue %s must be named nowhere",
			seedPublic, got, seedPrivate, gated)
	}
	want := []supersededApprovalGate{{Issue: gated, Step: model.GatedStepSpec}}
	if got := supersede(callerCarol, "A sentence carol wrote.").DesignGates; !slices.Equal(got, want) {
		t.Errorf("carol's edit of %s's document names the gates %#v, want %#v: she sees %s", seedPublic, got, want, seedPrivate)
	}
}
