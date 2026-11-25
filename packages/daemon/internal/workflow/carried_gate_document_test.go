package workflow

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// seedRootCarriedOnOldDocuments records what a hand-over leaves: the old root gatedRoot, done, and
// LEGION-209, a root of its own handed over from it with its gate carried, which holds a copy of
// each of the old root's gate rows, bound to the old root's documents (criteriaDoc and gateDoc),
// as admit's readmit writes them ("The carry binds this issue's steps to the old root's
// documents"). The Verification document took a new version while the old tree lingered, so that
// step's copy is closed.
func seedRootCarriedOnOldDocuments(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "the old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-209", Project: "LEGION", Title: "the carried root", Parent: new(gatedRoot), Phase: phase.Admitted, Generation: 2,
		Status: "in_progress", Rank: "V", HandedOver: true, HandedOverFrom: gatedRoot, LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-209", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	seedTreeLifecycle(t, pool, "LEGION-209")
	for _, issue := range []string{gatedRoot, "LEGION-209"} {
		seedGate(t, pool, record.DesignGate{Issue: issue, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, LatestVersion: 2, ApprovedVersion: new(1)})
		seedGate(t, pool, record.DesignGate{Issue: issue, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4)})
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutHandedOverRoot(t.Context(), tx, record.HandedOverRoot{Issue: "LEGION-209", Generation: 2, GateCarried: true})
	})
}

// Main's catch-up (workflow/catch_up.go:treeCatchUp at d46ec4e0) stated each step of a root's
// design gate by the row it holds, "stated.Artifact, stated.Version, stated.Open = recorded.ArtifactID,
// recorded.LatestVersion, classify.DesignGateOpen(recorded)", and named no document by any other
// key. The branch adds `document`, a dispatch:// link it writes as stepRef(issue.Key, step) for
// every step, so for a root whose gate was carried it names the carried root's own document
// beside the artifact, version and approval of the old root's document the copy is bound to. The
// root architect's prompt (go/architect-root.md) reads the entry as "its document as a
// `dispatch://` link (`document`) ... the `artifact`, that document's latest `version`, the
// `approved` version and whether the step is `open`", and calls the carried gate "copies on the
// old root's documents". INFRA-3686's R2: "The fields are those of the kind, ... the references
// (a `dispatch://` link for each Dispatch item), the versions and the state that the reader acts
// on"; a field that states a fact wrongly is the change's own defect.
func TestACarriedRootsCatchUpNamesTheOldRootsDocumentOfEachCarriedStep(t *testing.T) {
	pool := migratedPool(t)
	seedRootCarriedOnOldDocuments(t, pool)
	claimReady(t, pool, stepsEngine(twoSteps(), nil), "LEGION-209", claim.RoleArchitect, 1)

	caught := lastCatchUp(t, pool)
	if !caught.Carried || len(caught.Gates) != 2 {
		t.Fatalf("the catch-up says carried %t with gates %+v; want a carried root's catch-up naming both carried steps", caught.Carried, caught.Gates)
	}
	for _, gate := range caught.Gates {
		if want := stepRef(gatedRoot, gate.Step); gate.Document != want {
			t.Errorf("the carried root's catch-up names the %s step's document %s with artifact %s at version %d (approved %d, open %t), "+
				"but that artifact is the old root's document %s, which the copy is bound to and a person approves",
				gate.Step, gate.Document, gate.Artifact, gate.Version, gate.Approved, gate.Open, want)
		}
	}
}

// Main's gate-moved message (workflow/gate.go:tellGateMoved at d46ec4e0) named the document that
// moved by the artifact the row holds: "The design gate of %s opened: a person approved version %d
// of its %s document (%s)". Its carried-gate-opened notice of the same approval
// (workflow/gate.go:applyArtifact) named the old root's document: "a human approved %s's spec
// document at version %d, which opens the gate %s carried". The branch's carried-gate-opened
// notice keeps that (`ref` on the old root's document), but its gate-moved message names
// `document` as stepRef(tree, step), the carried root's own document, beside the old root's
// artifact and the version a person approved of the old root's document, so one approval is told
// on two documents in one transaction. INFRA-3686's R2: "The fields are those of the kind, ...
// the references (a `dispatch://` link for each Dispatch item), the versions and the state that
// the reader acts on"; a field that states a fact wrongly is the change's own defect.
func TestTheGateMovedMessageOfACarriedStepsOpeningNamesTheDocumentAPersonApproved(t *testing.T) {
	pool := migratedPool(t)
	seedRootCarriedOnOldDocuments(t, pool)
	apply := applyFacts(t, pool, stepsEngine(twoSteps(), nil))

	apply("approve-the-old-document", stepApproval(gatedRoot, criteriaDoc, 2, record.EventMark{Seq: 9}))

	opened := noticesOfKind(t, pool, "LEGION-209", "carried-gate-opened")
	if len(opened) != 1 || opened[0].Ref != stepRef(gatedRoot, record.DesignStepCriteria) {
		t.Fatalf("the carried root's carried-gate-opened notices = %#v, want one on the old root's Verification document", opened)
	}
	var moved []*texts.GateMovedRecord
	for _, message := range roleMessageRows(t, pool, "LEGION-209") {
		if message.Kind != texts.GateMoved {
			continue
		}
		rec, err := message.Record()
		if err != nil {
			t.Fatalf("a gate-moved message carries no record: %v", err)
		}
		moved = append(moved, rec.(*texts.GateMovedRecord))
	}
	if len(moved) != 1 || moved[0].Gate != texts.GateOpened {
		t.Fatalf("the carried root's gate-moved messages = %+v, want one that says the gate opened", moved)
	}
	if moved[0].Document != opened[0].Ref {
		t.Errorf("the gate-moved message names document %s with artifact %s at version %d, but the person approved version %d of %s "+
			"(the carried-gate-opened notice of the same approval names that document)",
			moved[0].Document, moved[0].Artifact, moved[0].Version, opened[0].Version, opened[0].Ref)
	}
}
