package workflow

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The catch-up a relaunched root architect's ready writes opens with the sentence of what the last
// let-go of its process ended, which the ready carries while the claim's record of it is untold
// (intake.ClaimReady.Relaunch, the daemon's applyTerminal): the first turn of such an architect
// can be its catch-up's, and it reads that its subagent died before it reads its tree
// (dispatch://INFRA-3276 R3). The rest of the catch-up is what it always was. A ready that carries
// no sentence — the record told, or nothing recorded — composes the reason as before.
func TestARelaunchSentenceOpensTheRootArchitectsCatchUp(t *testing.T) {
	pool := migratedPool(t)
	seedAdmittedTree(t, pool, 1)
	engine := testEngine(config.DesignGateRootIssues, nil)
	sentence := supervise.RelaunchSentence(supervise.EndedWork{Cause: "its pod was replaced", Turn: true,
		Background: true, Kinds: []string{"subagent"}, Reported: true})
	const tree = "the tree of LEGION-1 at generation 1 as the daemon records it now; start or resume it as your role says"
	reasons := func() []string {
		t.Helper()
		got := []string{}
		for _, payload := range noticeRows(t, pool) {
			if notice, ok := payload.(record.Notice); ok && notice.Kind == "catch-up" {
				got = append(got, notice.Reason)
			}
		}
		return got
	}
	ready := func(launch int, relaunch string) {
		t.Helper()
		token, err := claim.NewToken("legion", "LEGION-1", claim.RoleArchitect)
		if err != nil {
			t.Fatalf("claim token: %v", err)
		}
		if _, err := intake.ApplyFact(context.Background(), pool, "supervise", fmt.Sprintf("supervise:%s:%d:ready", token, launch),
			intake.ClaimReady{Issue: "LEGION-1", Role: claim.RoleArchitect, Launch: uint64(launch), Relaunch: relaunch}, engine, admissionStub{}); err != nil {
			t.Fatalf("apply the ready of launch %d: %v", launch, err)
		}
	}

	ready(2, sentence)
	if got, want := reasons(), sentence+"\n\n"+tree; len(got) != 1 || got[0] != want {
		t.Fatalf("the relaunched architect's catch-up reasons %q, want the one %q: the sentence, then the tree as before", got, want)
	}

	ready(3, "")
	if got := reasons(); len(got) != 1 || got[0] != tree {
		t.Fatalf("a ready that carries no sentence wrote catch-up reasons %q, want the tree alone", got)
	}
}

// A root architect that holds only the daemon's own delivery of a relaunch's sentence, not yet
// taken, owes no work: that delivery carries no run, phase or entry and names no person's
// decision, and the architect stays live through the wait to read it. So it holds neither the
// root's wait on a person nor the telling of a person's act after the hand-back, where a task the
// architect has not taken holds both (owed).
func TestARelaunchSentenceDeliveryHoldsNoRootsWaitOnAPerson(t *testing.T) {
	relaunch := &supervise.Delivery{ID: "relaunch:WORD", Task: "Your previous process ended because its pod was replaced."}
	if !relaunch.Relaunch() {
		t.Fatalf("%s reads as no relaunch delivery", relaunch.ID)
	}
	for _, tc := range []struct {
		name    string
		pending *supervise.Delivery
		person  bool
		want    act
		why     string
	}{
		{"the relaunch's sentence alone", relaunch, false, actEnter, "awaiting an answer"},
		{"the relaunch's sentence and a person's act after the hand-back", relaunch, true, actTellInPlace, "already acted"},
		{"a task not yet taken, the control", &supervise.Delivery{ID: "outbox:7", Task: "Decide it."}, false, actStay, "not taken its task"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := awaitRootView()
			view.Run = record.RoleRun{Claim: &record.RoleClaim{State: supervise.StateReady, Pending: tc.pending}}
			if tc.person {
				view.Asks[0].PersonSeq = 7
			}
			if got := decideAwait(view); got.Act != tc.want || !strings.Contains(got.Why, tc.why) {
				t.Fatalf("decideAwait = %+v, want act %v because %q", got, tc.want, tc.why)
			}
		})
	}
}
