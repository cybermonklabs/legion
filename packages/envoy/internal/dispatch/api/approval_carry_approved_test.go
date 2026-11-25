package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The version a human approved is compared with a later version byte for byte once both have had
// the eight server-owned attributes taken off every ask block (the
// carry's attribute case): what the approved version's blocks showed of a decision is no part of
// what he approved, since an answer is held on the ask's node alone (R3), so a version that differs
// from his only by those attributes carries his approval, and one that differs by any other
// character does not.
//
// Each test approves inside the window a live write opens, so each guards the race it could lose:
// the review route pins the latest version, and a settlement that fires first repairs the block
// and moves the latest on. The window is pastTheRun, longer than any run of this package's test
// binary, and the test settles the document at once (docs.SettleNow) wherever it needs a
// settlement, so no settlement fires between the write and the approval however slow the machine
// is. The review's version must still be the one the test meant, and that version's markdown must
// show what the test put there, or the test fails as setup rather than passing over a version that
// never showed it.

// approveVersion approves the document as login through the review route, which pins its latest
// version, and fails t unless that is version number: the version the test meant.
func approveVersion(t *testing.T, handler http.Handler, artifactID, login string, number int) {
	t.Helper()
	review := reviewDocument(t, handler, artifactID, login, "approved", "")
	if review.Version != number {
		t.Fatalf("%s approved version %d, want version %d: settlement wrote the block back before the approval landed", login, review.Version, number)
	}
}

// attributeRemovalSentence is the reason a review carried by the attribute case records, naming
// the approval it carries forward (docs' attributeRemovalCase).
func attributeRemovalSentence(from int) string {
	return fmt.Sprintf("Carried forward from the approval of version %d: the server-owned attributes of its ask blocks, which hold no answer, were the whole change since it.", from)
}

// Carried: the version he approved shows a server-owned attribute on the block he answered - a
// selection, an answer, or a copy's provenance other than its row's (askServerAttributeNames) -
// which a browser's socket edit put there and the agent named a version of while a process of the
// release before served, the one fleet whose version writes keep the seven attributes a version can
// store: on a fleet of one build a version written before settlement takes them off
// (docs' storableVersionTree). The human approved that version inside the settle window; the
// release before's process then left, settlement takes the attribute off the block, as it takes
// each of the seven off a block once the fleet is one build, and the version it writes differs
// from the one he approved by that attribute alone. The two are byte-equal once both have had the
// eight attributes taken off every ask block, and the block agrees with its node, so his approval
// is carried to it with the attribute case's reason (R10: "It carries the approval when the new
// rendering equals the approved rendering after both have had those eight attributes removed from
// every ask block").
func TestAnApprovedVersionShowingADecisionOtherThanItsRowsCarriesToTheVersionWithoutIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		attribute string
		shown     any
		rendered  string // what version 2's markdown shows for the attribute
	}{
		{"selected", []string{"West"}, `selected="[&#x22;West&#x22;]"`},
		{"answer", "Pick West.", `answer="Pick West."`},
		{"copied_from", "00000000-0000-0000-0000-000000000000", `copied_from="00000000-0000-0000-0000-000000000000"`},
		{"copied_from_document", "dispatch://TEST/artifact/other", `copied_from_document="dispatch://TEST/artifact/other"`},
	} {
		t.Run(tc.attribute, func(t *testing.T) {
			handler, database, documents := settlingCarryDocuments(t, pastTheRun)
			issue := createInteractionIssue(t, handler, "TEST", "Approved "+tc.attribute+" other than the row's", "Context\n\n"+regionAsk)
			asks := settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
			answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)

			setLiveBlockAttributes(t, documents, issue.PrimaryArtifactID, "region", map[string]any{tc.attribute: tc.shown})
			whileTheReleaseBeforeServes(t, database, func() {
				named := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]any{"summary": "Name the " + tc.attribute + " other than the row's", "actor": sessionActor()})
				if named.Code != http.StatusCreated {
					t.Fatalf("name the version showing the %s other than the row's: status=%d body=%s", tc.attribute, named.Code, named.Body.String())
				}
			})
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
			shown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 2)
			if !strings.Contains(shown, tc.rendered) {
				t.Fatalf("version 2 = %s, want it to show the %s other than the row's on the block alice answered", shown, tc.attribute)
			}
			approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 2)

			// Settlement takes the attribute off and leaves what a version keeps of alice's answer on
			// the block - its state, who answered and when (docs' keptAskState) - which version 2
			// shows beside it.
			settledVersion(t, handler, documents, issue.PrimaryArtifactID, 3)
			restored := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3)
			if want := strings.Replace(shown, " "+tc.rendered, "", 1); restored != want || !strings.Contains(restored, ` state="answered" answered_by="alice" answered_at="`) {
				t.Fatalf("version 3 = %s, want settlement to have taken the %s off, leaving the block's kept state:\n%s", restored, tc.attribute, want)
			}
			requireNoAnswerAttributes(t, "version 3", restored)
			requireReviewCarriedWith(t, handler, issue.PrimaryArtifactID, 3, 2, attributeRemovalSentence(2), nil)
		})
	}
}

// Stale: the version he approved shows the block he answered under wording other than its row was
// decided on - the one way an agent reaches this over HTTP alone. The human answered the block,
// whose state the answer writes into its block (R3); the agent reworded its question through the edits route, which
// versions in the route's own transaction; the human approved that version inside the settle
// window, and settlement then wrote the decided wording back. That version differs from the one he
// approved in the question's words, which no carry case takes out, so nothing is carried (R10: "A
// write that changes one further character supersedes the approval"). The agent adds a second
// block and the human answers it, which writes one version of its block's state alone, so the approval stays stale.
func TestAnApprovedVersionShowingADecidedBlockUnderOtherWordingCarriesNothing(t *testing.T) {
	t.Parallel()
	handler, database, documents := settlingCarryDocuments(t, pastTheRun)
	issue := createInteractionIssue(t, handler, "TEST", "Approved under other wording", "Context\n\n"+regionAsk)
	asks := settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID, map[string]string{"region": "Which region goes first?"})
	answerDecisionBlock(t, handler, asks["region"], "alice", []string{"East"}, "", nil)

	reviseDocument(t, handler, issue.PrimaryArtifactID, "Which region goes first?", "Which region goes last?", 2)
	if reworded := versionMarkdown(t, handler, issue.PrimaryArtifactID, 2); !strings.Contains(reworded, "Which region goes last?") {
		t.Fatalf("version 2 = %s, want it to hold the agent's reword of the block alice answered", reworded)
	}
	approveVersion(t, handler, issue.PrimaryArtifactID, "alice", 2)

	settledVersion(t, handler, documents, issue.PrimaryArtifactID, 3)
	restored := versionMarkdown(t, handler, issue.PrimaryArtifactID, 3)
	if !strings.Contains(restored, "Which region goes first?") || strings.Contains(restored, "goes last?") {
		t.Fatalf("version 3 = %s, want settlement to have written the decided wording back", restored)
	}
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
	insertDecisionMarkdown(t, handler, issue.PrimaryArtifactID, waveAsk, 4)
	for id, ask := range settledDecisionBlocks(t, handler, documents, issue.Key, issue.PrimaryArtifactID, map[string]string{"wave": "How many regions take the first wave?"}) {
		asks[id] = ask
	}

	answerDecisionBlock(t, handler, asks["wave"], "alice", []string{"Two"}, "", nil)
	documents.SettleNow(issue.PrimaryArtifactID)
	requireStateVersionsPast(t, handler, database, issue.PrimaryArtifactID, 4, 1)
	requireNoCarry(t, handler, issue.PrimaryArtifactID, 1, "stale")
}
