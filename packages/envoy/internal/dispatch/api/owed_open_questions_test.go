package api

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// owedAsk is an open ask block a spec version adds, ahead of a list that holds the version above
// the number of blocks a write indexes in its own request.
const owedAsk = ":::ask{#q-owed urgency=\"med\" multiple=\"false\"}\nWhich store holds the queue?\n\n- Postgres: The one we run\n- Redis: The faster one\n:::\n"

// openQuestionIDs is the id of each open question GET /api/v1/issues/{key}/decisions answers for
// the issue key, in its order.
func openQuestionIDs(t *testing.T, handler http.Handler, key string) []string {
	t.Helper()
	ids := []string{}
	for _, question := range readIssueDecisions(t, handler, key).OpenQuestions {
		ids = append(ids, question.ID)
	}
	return ids
}

// TestTheOpenQuestionsOfASpecThatOwesItsIndexHoldItsNewAsk: a spec version written through the
// upload route past the bound (docs' deferBlockIndexBlocks) adds an open ask block, and leaves its
// blocks to the block-index reconciliation. The settlement after the upload opens the block's ask
// row and finds the room at the upload's text, so it writes no version and the spec still owes its
// block index: no document_blocks row holds the ask's block yet. While the spec owes that index,
// the open questions of the Decisions read still hold the ask, read from its ask row: a question a
// person can see is never dropped for an index that is owed. Once the reconciliation has indexed
// the spec, they hold it exactly once.
func TestTheOpenQuestionsOfASpecThatOwesItsIndexHoldItsNewAsk(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	documents := deps.Docs.(*docs.Service)
	createReferenceAPIProject(t, handler, "OWED")
	issue := createSpecIssue(t, handler, "OWED", "A spec.\n")
	key, spec := issue.Key, issue.PrimaryArtifactID
	reconcileUntilNothingOwed(t, handler, deps, key)
	if questions := openQuestionIDs(t, handler, key); len(questions) != 0 {
		t.Fatalf("the open questions of %s before the spec asks anything are %v, want none", key, questions)
	}

	written := uploadDocument(t, handler, "/api/v1/issues/"+key, "spec.md",
		"A spec.\n\n"+owedAsk+"\n"+deferralList(itemsOverTheBlockBound, "end"))
	if written.Artifact.ID != spec {
		t.Fatalf("the upload of spec.md to %s wrote the document %s, want its spec %s", key, written.Artifact.ID, spec)
	}
	version := written.Version.Number
	if owed := readCoverage(t, handler, key).OwedDocuments; !slices.Contains(owed, model.OwedDocument{ArtifactID: spec, Version: version}) {
		t.Fatalf("owed_documents of %s does not name the spec at version %d: %+v", key, version, owed)
	}
	settleIssue(t, documents, key)
	if latest := latestVersion(t, database, spec).Number; latest != version {
		t.Fatalf("the settlement after the upload left the spec of %s at version %d, want the upload's version %d", key, latest, version)
	}
	if owed := readCoverage(t, handler, key).OwedDocuments; !slices.Contains(owed, model.OwedDocument{ArtifactID: spec, Version: version}) {
		t.Fatalf("once settled, owed_documents of %s does not name the spec at version %d: %+v", key, version, owed)
	}
	if indexed := blockIndexVersion(t, database, spec); indexed != nil {
		t.Fatalf("the spec of %s holds its block index at version %d, want it owed at version %d", key, *indexed, version)
	}
	ask := blockAskID(t, handler, key, "q-owed")
	if state := readBlockAsk(t, handler, ask).State; state != "open" {
		t.Fatalf("the ask %s of block q-owed is %s, want it open", ask, state)
	}

	if questions := openQuestionIDs(t, handler, key); slices.Index(questions, ask) < 0 {
		t.Errorf("while the spec of %s owes its block index at version %d, its open questions are %v, want the ask %s of block q-owed among them",
			key, version, questions, ask)
	}

	reconcileUntilNothingOwed(t, handler, deps, key)
	requireNothingOwed(t, handler, key, "once the index caught up")
	questions := openQuestionIDs(t, handler, key)
	held := 0
	for _, question := range questions {
		if question == ask {
			held++
		}
	}
	if held != 1 {
		t.Errorf("once the spec of %s holds its block index, its open questions are %v, holding the ask %s of block q-owed %d times, want once",
			key, questions, ask, held)
	}
}
