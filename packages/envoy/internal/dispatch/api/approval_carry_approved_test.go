package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// storeAsTheReleaseBeforeWroteIt rewrites the stored markdown of version number of artifactID as
// a process of the release before wrote it, whose version writes kept every server-owned attribute
// of an ask block as its live tree held it, where this build's keep only what a version keeps of an
// ask (docs' keptAskState, storableVersionTree): the opener of the ask block blockID gets
// rendered, an attribute as that release's rendering spells it, before its closing brace. A
// version that release wrote holds no version_renders row, so every read answers its stored
// markdown (docs.RenderVersion): the row this build wrote is deleted. It answers the version's
// markdown as stored.
func storeAsTheReleaseBeforeWroteIt(t *testing.T, database *store.Store, artifactID string, number int, blockID, rendered string) string {
	t.Helper()
	markdown := versionMarkdownAt(t, database, artifactID, number)
	var seeded strings.Builder
	found := false
	for line := range strings.Lines(markdown) {
		if !found && strings.HasPrefix(strings.TrimLeft(line, ":"), "ask{#"+blockID+" ") && strings.HasSuffix(line, "}\n") {
			line, found = strings.TrimSuffix(line, "}\n")+" "+rendered+"}\n", true
		}
		seeded.WriteString(line)
	}
	if !found {
		t.Fatalf("version %d of %s holds no opener of the ask block %s:\n%s", number, artifactID, blockID, markdown)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update artifact_versions set markdown = $3 where artifact_id = $1 and number = $2;
	`, artifactID, number, seeded.String()); err != nil {
		t.Fatalf("store version %d of %s as the release before wrote it: %v", number, artifactID, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `delete from version_renders where artifact_id = $1 and number = $2`, artifactID, number); err != nil {
		t.Fatalf("drop the render row of version %d of %s: %v", number, artifactID, err)
	}
	return seeded.String()
}

// Carried: the version he approved shows a server-owned attribute on the block he answered - a
// selection, an answer, or a copy's provenance other than its row's (askServerAttributeNames) -
// which a browser's socket edit put there and the agent named a version of: this build's version
// write keeps the copy's provenance as the live tree holds it, and a process of the release before
// kept the selection and the answer too, as the version stored here holds them. The human approved
// that version inside the settle window; settlement then takes the attribute off the block, as it
// takes off each of the seven a version keeps of no ask in step with its node, and the version it
// writes differs from the one he approved by that attribute alone. The two are byte-equal once
// both have had the eight attributes taken off every ask block, and the block agrees with its
// node, so his approval is carried to it with the attribute case's reason (R10: "It carries the
// approval when the new rendering equals the approved rendering after both have had those eight
// attributes removed from every ask block").
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

			setLiveBlockAttributes(t, handler, documents, issue.PrimaryArtifactID, "region", map[string]any{tc.attribute: tc.shown})
			named := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/versions", map[string]any{"summary": "Name the " + tc.attribute + " other than the row's", "actor": sessionActor()})
			if named.Code != http.StatusCreated {
				t.Fatalf("name the version showing the %s other than the row's: status=%d body=%s", tc.attribute, named.Code, named.Body.String())
			}
			waitForArtifactVersion(t, handler, issue.PrimaryArtifactID, 2)
			shown := versionMarkdown(t, handler, issue.PrimaryArtifactID, 2)
			if !strings.Contains(shown, tc.rendered) {
				shown = storeAsTheReleaseBeforeWroteIt(t, database, issue.PrimaryArtifactID, 2, "region", tc.rendered)
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
