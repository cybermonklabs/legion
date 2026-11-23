package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestADesignGateNamingNoDocumentItsReaderSeesIsLeftOutWhateverItNames: a design gate stores its
// document's id unread (putIssueDesignGates), so the id may name a document of a private project,
// or nothing at all. A reader that does not see every object is answered only the gates whose id
// names a document it sees: one naming a document of PRIV and one naming an id no document holds
// are both left out for alice, an open member who sees PUB alone, so her issue read cannot tell
// which ids name a document she does not see. carol, who sees both projects, reads the gate naming
// PRIV's document and not the one naming nothing. With no private project alice sees every object
// and reads all three as stored, as every reader did before (R9). carol stores the three gates
// while PRIV is public, as a deployment held them before its first project went private: while one
// is private, a write of a gate naming a document its writer does not see, an id no document holds
// included, is refused (refuseUnseenGateDocuments).
//
// Control: the statement that left out only a gate naming a document its reader does not see, and
// carol, who does not see every object while PRIV is private, reads the decisions gate, whose id
// names no document.
func TestADesignGateNamingNoDocumentItsReaderSeesIsLeftOutWhateverItNames(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	primary := func(project string) string {
		t.Helper()
		created := decodeBody[struct {
			Key               string `json:"key"`
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}](t, seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": project, "title": "A gated issue of " + project, "spec": "The spec of " + project + ".",
		}, http.StatusCreated))
		if created.PrimaryArtifactID == "" {
			t.Fatalf("the issue carol made in %s has no primary document", project)
		}
		return created.PrimaryArtifactID
	}
	seen, unseen := primary(seedPublic), primary(seedPrivate)
	const nothing = "00000000-0000-4000-8000-00000000d0c5"
	issue := seed.issue[seedPublic]
	all := []issueDesignGate{
		{Step: model.GatedStepDecisions, ArtifactID: nothing},
		{Step: model.GatedStepCriteria, ArtifactID: seen},
		{Step: model.GatedStepSpec, ArtifactID: unseen},
	}
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/issues/"+issue+"/design-gates", map[string]any{"gates": all}, http.StatusOK)
	seed.setVisibility(t, seedPrivate, model.ProjectPrivate)
	stored := decodeBody[struct {
		DesignGates []issueDesignGate `json:"design_gates"`
	}](t, seed.mustRequest(t, callerCarol, http.MethodGet, "/api/v1/issues/"+issue, nil, http.StatusOK)).DesignGates
	ordered := func(gates ...issueDesignGate) []issueDesignGate {
		gates = slices.Clone(gates)
		slices.SortFunc(gates, func(left, right issueDesignGate) int {
			return slices.Index(model.GatedStepsKnown, left.Step) - slices.Index(model.GatedStepsKnown, right.Step)
		})
		return gates
	}
	read := func(caller privateCaller) []issueDesignGate {
		t.Helper()
		return decodeBody[struct {
			DesignGates []issueDesignGate `json:"design_gates"`
		}](t, seed.mustRequest(t, caller, http.MethodGet, "/api/v1/issues/"+issue, nil, http.StatusOK)).DesignGates
	}
	if want := ordered(all[1], all[2]); !slices.Equal(stored, want) {
		t.Fatalf("carol reads %s's gates as %#v, want the two naming a document she sees, %#v", issue, stored, want)
	}
	if got, want := read(callerAlice), ordered(all[1]); !slices.Equal(got, want) {
		t.Errorf("with PRIV private alice reads %s's gates as %#v, want only the one naming PUB's document, %#v: "+
			"a gate naming PRIV's document and one naming no document must read alike", issue, got, want)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if reader, err := seed.server.readerFor(t.Context(), model.BrowserActor(seedAlice)); err != nil || !seesEveryObject(reader) {
		t.Fatalf("with PRIV public alice reads as %+v (%v), want a reader that sees every object", reader.Viewer, err)
	}
	if got, want := read(callerAlice), ordered(all...); !slices.Equal(got, want) {
		t.Errorf("with no private project alice reads %s's gates as %#v, want every gate as stored, %#v (R9)", issue, got, want)
	}
}
