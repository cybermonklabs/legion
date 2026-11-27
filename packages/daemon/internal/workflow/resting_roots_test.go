package workflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The issues of k10 (dispatch://INFRA-3320, R8): P is LEGION-100 (basisAncestor), R is LEGION-208
// (gatedRoot), a root whose spec step rests on P's approval, S a root of its own that is a
// Dispatch child of P and opened its step on a person's approval of its own document, M a member of
// P's own tree, U a Dispatch child of P filed and never released, T the root of a tree P is a member
// of, and O a root in Legion's work outside P's chain that R is moved under.
const (
	restingOwnChild    = "LEGION-301"
	restingNeverLet    = "LEGION-303"
	restingMember      = "LEGION-101"
	restingAncestorsOf = "LEGION-50"
	restingOutside     = "LEGION-70"
)

// restingBasis is a person's approval of version 3 of P's primary document, whose requirement p2
// the checks of R serve.
func restingBasis() *record.GateBasis {
	basis := approvedBasis()
	basis.Serves = []string{"dispatch://LEGION-100/spec#b-p2@v3"}
	return basis
}

// restingReturn is the sentence of the SIGNOFF_CHILD_OPEN refusal of P (LEGION-100) that says
// where a parent in accepting goes once its open children are finished: into awaiting_children,
// and back by itself to the start it held, a held acceptance as testing (workflow's
// ReturnFromChildren; dispatch://INFRA-3320 R8 and R12).
const restingReturn = "Unless the pull request of LEGION-100 has merged, the next admission pass moves it to awaiting_children, and once no child under it is open it goes back by itself to the start it held, a held acceptance as testing; otherwise sign off again. "

// seedAcceptingAncestor records P as the root of a running tree of its own in accepting, on its
// open pull request #41 at head with the reviewer's approval of that head (seedAccepting's rows).
func seedAcceptingAncestor(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Accepting, Generation: 1, Status: "needs_review", Rank: "T",
		HandedOver: true, LastDispatchSeq: 1})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: basisAncestor, Repo: "sjawhar/legion", Number: 41,
		Branch: "legion/" + basisAncestor, HeadSHA: "head", CheckedHead: "head", CheckRuns: []record.AttemptRun{{Name: acceptingCheck, ID: 1}}, Failing: []string{}})
	seedPhase(t, pool, record.PhaseRow{Issue: basisAncestor, Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: "implement-1"})
	seedPhase(t, pool, record.PhaseRow{Issue: basisAncestor, Role: claim.RoleReviewer, Claim: "review-claim", HandoffRecord: "review-1", HandoffVersion: 1,
		Summary: "reviewed", Decision: &record.ReviewDecision{State: "approved", Head: "head"}})
}

// registerResting records R and registers its spec step on P's approval through the engine, and
// stands R in implementing, its tree at work.
func registerResting(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result) {
	t.Helper()
	seedGatedTree(t, pool)
	apply("register-on-basis", onBasis(restingBasis(), 6))
	if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; !gate.OnBasis() || !slices.Equal(gate.RestsOn, []string{basisAncestor}) {
		t.Fatalf("R's spec step = %#v, want it open on P's approval and resting on %s", gate, basisAncestor)
	}
	leave(t, pool, gatedRoot, phase.Implementing, "in_progress")
}

// closeIssue applies Dispatch's event of key moving to status (done, backlog), which takes it out
// of the workflow (the engine's leave), and mirrors it, as the daemon's handlers apply an event.
// The record then holds the status, as admission's observation of the event writes it after the
// engine's handler (admit's recordObservation), which these tests' admission stub does not.
func closeIssue(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, parent, status string, seq int64) {
	t.Helper()
	kind := "issue.updated"
	if status == "done" {
		kind = "issue.closed"
	}
	applyMirrored(t, pool, engine, key+"-"+status, intake.DispatchIssue{Key: key, Seq: seq, Type: kind, Status: status, Title: key, Parent: parent, Rank: "V"})
	if _, err := pool.Exec(t.Context(), "update issues set status = $2, dispatch_status = $2 where key = $1", key, status); err != nil {
		t.Fatalf("record the observation of %s at %s: %v", key, status, err)
	}
}

// mirrorChild writes what Dispatch shows of key, a child of parent at status, at seq, waiting on
// blockers: what seedMirrorChild writes, where a test needs a later sequence or a link.
func mirrorChild(t *testing.T, pool *pgxpool.Pool, key, parent, status string, seq int64, blockers ...string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchReading{Key: key, Project: "LEGION", Seq: seq,
			Status: record.Carry(status), Parent: record.Carry(parent), Blockers: record.Carry(blockers)})
	})
}

// seedOwnApprovalChild records S as a root of its own at work in implementing, its spec step open
// on a person's approval of its own document and resting on nothing, and mirrors it as a Dispatch
// child of P.
func seedOwnApprovalChild(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: restingOwnChild, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: restingOwnChild, Project: "LEGION", Title: restingOwnChild, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "S",
		HandedOver: true, LastDispatchSeq: 1})
	seedGate(t, pool, record.DesignGate{Issue: restingOwnChild, ArtifactID: "artifact-301", LatestVersion: 2, ApprovedVersion: new(2),
		ApprovedMark: record.EventMark{Seq: 3, At: basisApprovedAt}})
	// Above the sequence seedIssue's own copy row took, so the copy holds S under P.
	mirrorChild(t, pool, restingOwnChild, basisAncestor, "in_progress", 2)
}

// refusedWith fails unless result is a 409 refusal of code whose message names each of names, and
// returns the message.
func refusedWith(t *testing.T, result intake.Result, code string, names ...string) string {
	t.Helper()
	if result.Refusal == nil || result.Refusal.Status != 409 || result.Refusal.Code != code {
		t.Fatalf("the answer = %+v, want 409 %s", result.Refusal, code)
	}
	for _, name := range names {
		if !strings.Contains(result.Refusal.Message, name) {
			t.Fatalf("the %s refusal %q names no %q", code, result.Refusal.Message, name)
		}
	}
	return result.Refusal.Message
}

// closeRootNow applies the architect's close_root of key and answers what the daemon made of it.
func closeRootNow(t *testing.T, pool *pgxpool.Pool, id, key string) intake.Result {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, "api", id, intake.CloseRoot{Issue: key, Reason: "moot"},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{})
	if err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
	return result
}

// lastCatchUpReason is the reason of the newest catch-up notice the outbox holds.
func lastCatchUpReason(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	reason := ""
	for _, payload := range noticeRows(t, pool) {
		if notice, ok := payload.(record.Notice); ok && notice.Kind == "catch-up" {
			reason = notice.Reason
		}
	}
	return reason
}

// restingNamed is how R is named by the refusals, the start task and the catch-up while it
// implements.
const restingNamed = "LEGION-208 (its spec step, in implementing)"

// The refusal of P's sign-off and close while R is in the workflow names R and says who moves it.
const restingMover = "Each is a tree of its own: its own architect, or a person, moves it."

// An ancestor's sign-off and close wait for the issues beneath it on its Dispatch chain, a root
// under it that rests on its approval among them, then for the roots that rest on its approval
// from outside its chain, then for its open Dispatch children, in R8's order; and its architect
// hears when each resting root starts to rest on it and when it closes or leaves the workflow, at
// its start when the daemon recorded it in no live tree then, and in each catch-up (k10,
// dispatch://INFRA-3320 R8).
func TestRestingRootsHoldTheAncestorSignOff(t *testing.T) {
	t.Parallel()
	t.Run("a resting root under the ancestor holds the sign-off as an issue beneath it until it is done, and the merge then closes the ancestor", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedAcceptingAncestor(t, pool)
		registerResting(t, pool, applyFacts(t, pool, engine))
		// R stands under P on its Dispatch chain and in Legion's work, so it is beneath P, which
		// holds the sign-off before its rest on P's approval does (R18 of dispatch://INFRA-3320).
		mirrorChild(t, pool, gatedRoot, basisAncestor, "in_progress", 20)

		refusedWith(t, signOffNow(t, pool, "signoff-held", basisAncestor), signOffChildInWorkflow, gatedRoot)
		if got := phaseOf(t, pool, basisAncestor); got != phase.Accepting {
			t.Fatalf("P = %s after the refused sign-off, want accepting", got)
		}

		closeIssue(t, pool, engine, gatedRoot, basisAncestor, "done", 21)
		if result := signOffNow(t, pool, "signoff-accepted", basisAncestor); result.Refusal != nil {
			t.Fatalf("the sign-off once R is done = %+v, want it accepted", result.Refusal)
		}
		if got := phaseOf(t, pool, basisAncestor); got != phase.Retro {
			t.Fatalf("P = %s after the accepted sign-off, want retro", got)
		}

		// The merged close needs no hold of its own (R8): the sign-off waited for R. It is queued at
		// the merge and ends P's run once Dispatch shows P done (R24).
		leave(t, pool, basisAncestor, phase.Merging, "retro")
		applyRefusingNothing(t, pool, engine, intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 41, MergeSHA: "merge-41"})
		if got := storedIssue(t, pool, basisAncestor).PendingClose; got == nil {
			t.Fatalf("P's close after its merge = %+v, want the merge's close queued", got)
		}
		landClose(t, pool, engine, basisAncestor)
		if got := phaseOf(t, pool, basisAncestor); got != phase.Done {
			t.Fatalf("P = %s once Dispatch shows it done, want done: the merge closes it as any merge does", got)
		}
	})

	t.Run("a resting root under the ancestor on the backlog holds nothing", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedAcceptingAncestor(t, pool)
		registerResting(t, pool, applyFacts(t, pool, engine))
		mirrorChild(t, pool, gatedRoot, basisAncestor, "in_progress", 20)

		closeIssue(t, pool, engine, gatedRoot, basisAncestor, "backlog", 21)
		if result := signOffNow(t, pool, "signoff-accepted", basisAncestor); result.Refusal != nil {
			t.Fatalf("the sign-off with R on the backlog = %+v, want it accepted", result.Refusal)
		}
		if got := phaseOf(t, pool, basisAncestor); got != phase.Retro {
			t.Fatalf("P = %s after the sign-off, want retro", got)
		}
	})

	t.Run("a Dispatch child on its own approval holds the sign-off as an issue beneath it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedAcceptingAncestor(t, pool)
		seedOwnApprovalChild(t, pool)

		// S stands under P on its Dispatch chain and in Legion's work, so it is beneath P, whatever
		// gate it works under (R18 of dispatch://INFRA-3320).
		refusedWith(t, signOffNow(t, pool, "signoff-held", basisAncestor), signOffChildInWorkflow, restingOwnChild)

		closeIssue(t, pool, engine, restingOwnChild, basisAncestor, "done", 3)
		if result := signOffNow(t, pool, "signoff-accepted", basisAncestor); result.Refusal != nil {
			t.Fatalf("the sign-off once S is done = %+v, want it accepted", result.Refusal)
		}
		if got := phaseOf(t, pool, basisAncestor); got != phase.Retro {
			t.Fatalf("P = %s after the sign-off, want retro", got)
		}
	})

	t.Run("a resting root moved under a parent in Legion's work outside the ancestor's chain holds the sign-off as a resting root", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedAcceptingAncestor(t, pool)
		registerResting(t, pool, applyFacts(t, pool, engine))
		// After its step opened on P's approval, R is moved under O, a root at work outside P's
		// chain: R is beneath O and not P, and it still rests on P's approval.
		seedIssue(t, pool, record.Issue{Key: restingOutside, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: restingOutside, Project: "LEGION", Title: "outside", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "O",
			HandedOver: true, LastDispatchSeq: 1})
		mirrorChild(t, pool, gatedRoot, restingOutside, "in_progress", 20)

		message := refusedWith(t, signOffNow(t, pool, "signoff-held", basisAncestor), signOffRestingRoot, restingNamed, restingMover)
		if want := "LEGION-100 is not accepted while these roots that rest on its approval are still in the workflow: " + restingNamed + ". " +
			"The design-gate step of each rests on an approval of LEGION-100, so LEGION-100 is accepted after them. " + restingMover + " " +
			"Sign off again once each one is done or out of the workflow"; message != want {
			t.Fatalf("the refusal = %q, want %q", message, want)
		}
		if got := phaseOf(t, pool, basisAncestor); got != phase.Accepting {
			t.Fatalf("P = %s after the refused sign-off, want accepting", got)
		}

		closeIssue(t, pool, engine, gatedRoot, restingOutside, "done", 21)
		if result := signOffNow(t, pool, "signoff-accepted", basisAncestor); result.Refusal != nil {
			t.Fatalf("the sign-off once R is done = %+v, want it accepted", result.Refusal)
		}
		if got := phaseOf(t, pool, basisAncestor); got != phase.Retro {
			t.Fatalf("P = %s after the accepted sign-off, want retro", got)
		}
	})

	t.Run("close_root waits for a resting root under the ancestor and for a child never released", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedIssue(t, pool, record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "T",
			HandedOver: true, LastDispatchSeq: 1})
		registerResting(t, pool, applyFacts(t, pool, engine))
		mirrorChild(t, pool, gatedRoot, basisAncestor, "in_progress", 20)

		refusedWith(t, closeRootNow(t, pool, "close-held", basisAncestor), signOffChildInWorkflow, gatedRoot)
		if got := phaseOf(t, pool, basisAncestor); got != phase.Admitted {
			t.Fatalf("P = %s after the refused close, want admitted", got)
		}

		closeIssue(t, pool, engine, gatedRoot, basisAncestor, "done", 21)
		seedMirrorChild(t, pool, restingNeverLet, "backlog", basisAncestor)
		message := refusedWith(t, closeRootNow(t, pool, "close-held-by-a-child", basisAncestor), signOffChildOpen, "LEGION-303 (backlog)")
		if want := "LEGION-100 is not closed while these Dispatch children of it are open: LEGION-303 (backlog). " +
			"A parent closes after its children, and a child that was never released holds the close until it is done or in icebox: close the tree again once none of them is open. " +
			"A child that runs as a tree of its own is moved by its own architect or a person. " +
			endChildrenFirst("close"); message != want {
			t.Fatalf("the refusal = %q, want %q", message, want)
		}

		// Past R8's codes the close waits for every child done (R24): one in icebox still holds it.
		mirrorChild(t, pool, restingNeverLet, basisAncestor, "icebox", 2)
		refusedWith(t, closeRootNow(t, pool, "close-held-by-an-iceboxed-child", basisAncestor), "CHILDREN_NOT_DONE", "LEGION-303 (icebox)")
		mirrorChild(t, pool, restingNeverLet, basisAncestor, "done", 3)
		if result := closeRootNow(t, pool, "close-accepted", basisAncestor); result.Refusal != nil {
			t.Fatalf("the close once R and U are done = %+v, want it accepted", result.Refusal)
		}
		if got := phaseOf(t, pool, basisAncestor); got != phase.Admitted {
			t.Fatalf("P = %s after the close, want admitted until Dispatch shows it done", got)
		}
		landClose(t, pool, engine, basisAncestor)
		if got := phaseOf(t, pool, basisAncestor); got != phase.Done {
			t.Fatalf("P = %s once Dispatch shows it done, want done", got)
		}
	})

	t.Run("a step a new version of the root's spec closed still holds the sign-off", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		apply := applyFacts(t, pool, engine)
		seedAcceptingAncestor(t, pool)
		registerResting(t, pool, apply)
		mirrorChild(t, pool, gatedRoot, basisAncestor, "in_progress", 20)

		apply("root-v5", intake.DispatchArtifact{Key: gatedRoot, ArtifactID: gateDoc, Kind: intake.DispatchArtifactVersion, Version: 5, Mark: askSeq(9)})
		if gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; record.GateOpen(gate) || gate.Basis != nil || !slices.Equal(gate.RestsOn, []string{basisAncestor}) {
			t.Fatalf("R's spec step after version 5 = %#v, want it closed with no basis, still resting on %s", gate, basisAncestor)
		}
		refusedWith(t, signOffNow(t, pool, "signoff-held", basisAncestor), signOffChildInWorkflow, gatedRoot)
	})

	t.Run("every hold at once is answered in R8's order", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedAcceptingAncestor(t, pool)
		seedBeneath(t, pool, restingMember, basisAncestor, basisAncestor, phase.Implementing, "in_progress")
		registerResting(t, pool, applyFacts(t, pool, engine))
		// U, a Dispatch child of P filed in triage and never released, holds as an open child.
		seedMirrorChild(t, pool, restingNeverLet, "triage", basisAncestor)

		message := refusedWith(t, signOffNow(t, pool, "signoff-beneath", basisAncestor), signOffChildInWorkflow, restingMember)
		if strings.Contains(message, gatedRoot) || strings.Contains(message, restingNeverLet) {
			t.Fatalf("the refusal %q names the holds that come after the first", message)
		}
		leave(t, pool, restingMember, phase.Done, "done")

		message = refusedWith(t, signOffNow(t, pool, "signoff-resting", basisAncestor), signOffRestingRoot, restingNamed)
		if strings.Contains(message, restingNeverLet) {
			t.Fatalf("the refusal %q names U, which rests on nothing", message)
		}
		closeIssue(t, pool, engine, gatedRoot, "", "done", 20)

		message = refusedWith(t, signOffNow(t, pool, "signoff-open", basisAncestor), signOffChildOpen, "LEGION-303 (triage)")
		if want := "LEGION-100 is not accepted while these Dispatch children of it are open: LEGION-303 (triage). " +
			"A parent is accepted after its children, a child that runs as a tree of its own included: run each one to done, releasing one that was never released. " +
			restingReturn +
			"A child that runs as a tree of its own is moved by its own architect or a person. " +
			endChildrenFirst("acceptance"); message != want {
			t.Fatalf("the refusal = %q, want %q", message, want)
		}
		mirrorChild(t, pool, restingNeverLet, basisAncestor, "icebox", 2)

		if result := signOffNow(t, pool, "signoff-accepted", basisAncestor); result.Refusal != nil {
			t.Fatalf("the sign-off once nothing holds it = %+v, want it accepted", result.Refusal)
		}
		if got := phaseOf(t, pool, basisAncestor); got != phase.Retro {
			t.Fatalf("P = %s after the sign-off, want retro", got)
		}
	})

	// P's merge waits in merging while R, a Dispatch child of P parked on the backlog, is not done,
	// and its close is queued once R is done or a person moves R out from under P, which the
	// decision pass reads (WriteDueClose).
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, pool *pgxpool.Pool, engine *Engine)
	}{
		{"until it is done", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			closeIssue(t, pool, engine, gatedRoot, basisAncestor, "done", 22)
		}},
		{"until a person unlinks it", func(t *testing.T, pool *pgxpool.Pool, engine *Engine) {
			applyMirrored(t, pool, engine, gatedRoot+"-unlinked", intake.DispatchIssue{Key: gatedRoot, Seq: 22, Type: "issue.updated", Status: "backlog",
				Title: gatedRoot, Rank: "V"})
			closePass(t, pool, engine)
		}},
	} {
		t.Run("a merged ancestor waits in merging for a resting child on the backlog "+tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			seedAcceptingAncestor(t, pool)
			registerResting(t, pool, applyFacts(t, pool, engine))
			// R is a Dispatch child of P, parked on the backlog before P's merge.
			mirrorChild(t, pool, gatedRoot, basisAncestor, "in_progress", 20)
			closeIssue(t, pool, engine, gatedRoot, basisAncestor, "backlog", 21)

			leave(t, pool, basisAncestor, phase.Merging, "retro")
			applyRefusingNothing(t, pool, engine, intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 41, MergeSHA: "merge-41"})
			if got, pending := phaseOf(t, pool, basisAncestor), storedIssue(t, pool, basisAncestor).PendingClose; got != phase.Merging || pending != nil {
				t.Fatalf("P after its merge = %s with close %+v, want merging with no close queued while R is on the backlog", got, pending)
			}
			if merged := noticesOfKind(t, pool, basisAncestor, "pr-merged"); len(merged) != 1 || !strings.Contains(merged[0].Reason, gatedRoot+" (backlog)") {
				t.Fatalf("P's pr-merged notices = %+v, want one naming R and its status", merged)
			}

			tc.end(t, pool, engine)
			if pending := storedIssue(t, pool, basisAncestor).PendingClose; pending == nil || pending.Kind != "merge" {
				t.Fatalf("P's close once R is no child of P that is not done = %+v, want the merge's close queued", pending)
			}
		})
	}

	t.Run("the ancestor's done with unlink_children ends a basis standing open on it, and the root's catch-up reports the step unregistered", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedAcceptingAncestor(t, pool)
		registerResting(t, pool, applyFacts(t, pool, engine))
		mirrorChild(t, pool, gatedRoot, basisAncestor, "in_progress", 20)

		// A person closes P with unlink_children while R is in the workflow: Dispatch moves R out
		// from under P at the status it stands in, and then closes P.
		applyMirrored(t, pool, engine, gatedRoot+"-unlinked", intake.DispatchIssue{Key: gatedRoot, Seq: 21, Type: "issue.updated", Status: "in_progress",
			Title: gatedRoot, Rank: "V"})
		closeIssue(t, pool, engine, basisAncestor, "", "done", 2)
		if gate, registered := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]; registered {
			t.Fatalf("R's spec step once P is done = %#v, want it unregistered: the basis it stood open on ended", gate)
		}
		ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
		if len(ended) != 1 || !strings.Contains(ended[0].Reason, "unregistered: register it again") {
			t.Fatalf("R's basis-ended notices = %+v, want one saying the step is unregistered", ended)
		}
		claimReady(t, pool, engine, gatedRoot, claim.RoleArchitect, 1)
		caught := lastCatchUp(t, pool)
		spec := slices.IndexFunc(caught.Gates, func(gate record.CatchUpGate) bool { return gate.Step == record.DesignStepSpec })
		if spec < 0 || !caught.Gates[spec].Unregistered || caught.Gates[spec].Open || caught.Gates[spec].Basis != nil {
			t.Fatalf("R's catch-up states its gates %+v, want the spec step unregistered with no basis", caught.Gates)
		}
	})

	for _, tc := range []struct {
		name, status, said string
	}{
		{"the ancestor's architect is told at the root's registration and at its close", "done", "LEGION-208 is done."},
		{"the ancestor's architect is told at the root's registration and at its move to the backlog", "backlog",
			"LEGION-208 left the workflow: its status is backlog."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := testEngine(config.DesignGateRootIssues, nil)
			seedLiveAncestor(t, pool)
			registerResting(t, pool, applyFacts(t, pool, engine))
			closeIssue(t, pool, engine, gatedRoot, "", tc.status, 20)

			resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
			if len(resting) != 2 {
				t.Fatalf("resting-root notices keyed by P = %#v, want the registration's and the leave's", resting)
			}
			for _, notice := range resting {
				for _, part := range []string{gatedRoot, "spec step", "dispatch://LEGION-100/spec#b-p2@v3"} {
					if !strings.Contains(notice.Reason, part) {
						t.Fatalf("the resting-root reason %q names no %q", notice.Reason, part)
					}
				}
				if notice.Ref != "dispatch://LEGION-100/spec" || notice.Version != 3 || notice.Role != claim.RoleArchitect {
					t.Fatalf("the resting-root notice = %#v, want it to name P's spec at version 3 to its architect", notice)
				}
			}
			want := tc.said + " Its spec step rests on the approval of LEGION-100. The sign-off and the close of LEGION-100, and its retro where its task has no acceptance step, no longer wait for LEGION-208. " +
				"The checks of LEGION-208 serve these requirements of LEGION-100: dispatch://LEGION-100/spec#b-p2@v3."
			if resting[1].Reason != want {
				t.Fatalf("the leave's resting-root reason = %q, want %q", resting[1].Reason, want)
			}
		})
	}

	// Two steps of R rest on P: the leave's notice names both, in the plural, and each requirement
	// their bases serve once; the document of P it names and the version are one step's, the
	// first whose basis names P now.
	for _, tc := range []struct {
		name     string
		criteria record.DesignGate
		ref      string
		version  int
	}{
		{"both steps open on P's approval", record.OpenOnBasis(record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria,
			ArtifactID: criteriaDoc, LatestVersion: 2}, record.GateBasis{Issue: basisAncestor, ArtifactID: "artifact-100-verification", Version: 2,
			Approval: record.EventMark{Seq: 41, At: basisApprovedAt}, Approver: "user:alice", Serves: restingBasis().Serves, RegisteredAt: basisEngineNow}),
			"dispatch://LEGION-100/artifact/verification-md", 2},
		{"the criteria step resting on P after its basis ended", record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria,
			ArtifactID: criteriaDoc, LatestVersion: 3, EverOpened: true, RestsOn: []string{basisAncestor}},
			"dispatch://LEGION-100/spec", 3},
	} {
		t.Run("two steps that rest on the ancestor, "+tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := stepsEngine(twoSteps(), nil)
			seedLiveAncestor(t, pool)
			registerResting(t, pool, applyFacts(t, pool, engine))
			seedGate(t, pool, tc.criteria)
			closeIssue(t, pool, engine, gatedRoot, "", "done", 20)

			resting := noticesOfKind(t, pool, basisAncestor, "resting-root")
			if len(resting) != 2 {
				t.Fatalf("resting-root notices keyed by P = %#v, want the registration's and the close's", resting)
			}
			want := "LEGION-208 is done. Its criteria and spec steps rest on the approval of LEGION-100. " +
				"The sign-off and the close of LEGION-100, and its retro where its task has no acceptance step, no longer wait for LEGION-208. " +
				"The checks of LEGION-208 serve these requirements of LEGION-100: dispatch://LEGION-100/spec#b-p2@v3."
			if closed := resting[1]; closed.Reason != want || closed.Ref != tc.ref || closed.Version != tc.version {
				t.Fatalf("the close's resting-root notice = %#v, want %q naming %s at version %d", closed, want, tc.ref, tc.version)
			}
		})
	}

	t.Run("an ancestor under a sub-architect is told through notices keyed by it", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedIssue(t, pool, record.Issue{Key: restingAncestorsOf, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: restingAncestorsOf, Project: "LEGION", Title: "tree root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "R",
			HandedOver: true, LastDispatchSeq: 1})
		seedBeneath(t, pool, basisAncestor, restingAncestorsOf, restingAncestorsOf, phase.Implementing, "in_progress")
		registerResting(t, pool, applyFacts(t, pool, engine))
		closeIssue(t, pool, engine, gatedRoot, "", "done", 20)

		// The outbox routes a notice keyed by P to the nearest running architect at or above P,
		// its sub-architect where one runs (the daemon's owningArchitect).
		if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 2 {
			t.Fatalf("resting-root notices keyed by P = %#v, want the registration's and the close's", got)
		}
		if got := noticesOfKind(t, pool, restingAncestorsOf, "resting-root"); len(got) != 0 {
			t.Fatalf("resting-root notices keyed by the tree's root = %#v, want none: they are keyed by P", got)
		}
	})

	t.Run("an ancestor the daemon does not record gets no notice, and its start task names the root", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		registerResting(t, pool, applyFacts(t, pool, engine))
		if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 0 {
			t.Fatalf("resting-root notices = %#v, want none for an ancestor the daemon does not record", got)
		}

		// P's admission records it, and the start its promotion queues for P's architect
		// (Engine.StartArchitect) names R before P's first act and beside no decision: the roots
		// that rest on P's approval are a fact of the record the derivation reads
		// (ArchitectStart.Resting), whatever shape P is given.
		ancestor := record.Issue{Key: basisAncestor, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: basisAncestor, Project: "LEGION", Title: "ancestor", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "T",
			HandedOver: true, LastDispatchSeq: 1}
		seedIssue(t, pool, ancestor)
		seedRecord(t, pool, func(tx pgx.Tx) error { return engine.StartArchitect(t.Context(), tx, ancestor) })
		start := architectStartOf(t, pool, basisAncestor)
		resting := "These roots rest on the approval of LEGION-100 and are still in the workflow: " + restingNamed + ". " +
			"The daemon refuses the sign-off and the close of LEGION-100, and holds its retro where its task has no acceptance step, while any of them is in the workflow (SIGNOFF_RESTING_ROOT_IN_WORKFLOW). " +
			"Each root's own architect, or a person, moves it."
		fresh := ArchitectStartTask(ArchitectStart{Root: ancestor, Gate: record.TreeGate{Steps: engine.GatedSteps()}})
		if want := resting + "\n\n" + fresh; start.Task != want || start.Kept != nil || start.Entry == 0 {
			t.Fatalf("P's start = task %q naming %#v at entry %d, want %q naming no decision, at the entry it raised", start.Task, start.Kept, start.Entry, want)
		}

		// That task held and narrowed of every decision at its send (the daemon's rootTask) is
		// written again from the record as it stands then, and still names R.
		var narrowed string
		seedRecord(t, pool, func(tx pgx.Tx) error {
			read, err := ArchitectStartOf(t.Context(), tx, engine.store, engine.GatedSteps(), ancestor)
			narrowed = NarrowRootTask(read, start.Entry, record.KeptDecisions{})
			return err
		})
		if narrowed != start.Task {
			t.Fatalf("P's held task narrowed of every decision = %q, want the task it was, %q, naming R", narrowed, start.Task)
		}
	})

	t.Run("the ancestor's catch-up names a resting root while it is in the workflow", func(t *testing.T) {
		pool := migratedPool(t)
		engine := testEngine(config.DesignGateRootIssues, nil)
		seedLiveAncestor(t, pool)
		// P's tree has no running architect: the notice row is queued all the same, for the
		// outbox to route, and the next catch-up names R.
		registerResting(t, pool, applyFacts(t, pool, engine))
		if got := noticesOfKind(t, pool, basisAncestor, "resting-root"); len(got) != 1 {
			t.Fatalf("resting-root notices = %#v, want the registration's", got)
		}

		claimReady(t, pool, engine, basisAncestor, claim.RoleArchitect, 1)
		// P registered no gate of its own, so its reason names its spec step before the roots.
		want := "the tree of LEGION-100 at generation 1 as the daemon records it now; start or resume it as your role says" +
			"; no document is registered for the spec step of LEGION-100: register the document of that step with register_gate and request the person's approval" +
			"; resting on the approval of LEGION-100 and still in the workflow: " + restingNamed +
			" (the daemon refuses the sign-off and the close of LEGION-100, and holds its retro where its task has no acceptance step, while any of them is; each root's own architect, or a person, moves it)"
		if got := lastCatchUpReason(t, pool); got != want {
			t.Fatalf("P's catch-up reason = %q, want %q", got, want)
		}

		closeIssue(t, pool, engine, gatedRoot, "", "done", 20)
		claimReady(t, pool, engine, basisAncestor, claim.RoleArchitect, 2)
		if got := lastCatchUpReason(t, pool); strings.Contains(got, "resting on the approval") {
			t.Fatalf("P's catch-up reason once R is done = %q, want no resting root named", got)
		}
	})
}

// A hold refuses the sign-off and the close of a root whatever its code (acceptanceHold.refusal):
// each hold acceptanceHeld answers has its texts in the one table (acceptanceHolds), a refusal for
// both acts, the words naming its issues and what its retro-held notice tells the owner, a hold the
// table does not name refuses both with its own code and the issues it names rather than letting
// either through, and no hold refuses nothing.
func TestEveryAcceptanceHoldRefusesBothActs(t *testing.T) {
	t.Parallel()
	acts := []architectAct{actSignOff, actCloseRoot}
	for _, code := range []string{signOffChildInWorkflow, signOffRestingRoot, signOffChildOpen} {
		if _, named := acceptanceHolds[code]; !named {
			t.Errorf("the hold %s has no texts in acceptanceHolds", code)
		}
	}
	for code, texts := range acceptanceHolds {
		for _, act := range acts {
			if texts.refusals[act] == "" {
				t.Errorf("the hold %s has no refusal text for %s", code, act)
			}
		}
		if texts.holding == "" || texts.retro == "" {
			t.Errorf("the hold %s has the words %q naming its issues and %q for its retro-held notice, want both", code, texts.holding, texts.retro)
		}
	}
	unnamed := acceptanceHold{code: "SIGNOFF_A_HOLD_NOT_IN_THE_TABLE", names: "LEGION-9"}
	for _, act := range acts {
		result, refuses := unnamed.refusal(act, "LEGION-8", "")
		if !refuses || result.Refusal == nil || result.Refusal.Code != unnamed.code || !strings.Contains(result.Refusal.Message, "LEGION-9") {
			t.Errorf("%s under a hold the table does not name = %t, %+v; want it refused with %s, naming LEGION-9", act, refuses, result.Refusal, unnamed.code)
		}
	}
	if result, refuses := (acceptanceHold{}).refusal(actSignOff, "LEGION-8", ""); refuses || result.Refusal != nil {
		t.Errorf("the sign-off under no hold = %t, %+v; want it not refused", refuses, result.Refusal)
	}
}

// A sign-off's refusal over children says where a held acceptance goes back once no child is open:
// the first step of the issue's walk after the implementer's (heldAcceptanceBack,
// record.ReturnStep). Today's line keeps its words, testing; a walk of an implementer, a reviewer
// and the owner's acceptance goes back to reviewing, and one of an implementer and the owner's
// acceptance alone to accepting, the start it held, with no step named.
func TestASignOffRefusalNamesTheStepAHeldAcceptanceGoesBackTo(t *testing.T) {
	t.Parallel()
	held := acceptanceHold{code: signOffChildOpen, names: "LEGION-209 (todo)"}
	for _, tc := range []struct {
		name string
		w    Walk
		says string
	}{
		{name: "today's line", w: todaysWalk(), says: "goes back by itself to the start it held, a held acceptance as testing; otherwise sign off again."},
		{name: "implementer reviewer acceptance", w: Walk{Line: []phase.Phase{phase.Implementing, phase.Reviewing, phase.Accepting, phase.Retro, phase.Merging}},
			says: "goes back by itself to the start it held, a held acceptance as reviewing; otherwise sign off again."},
		{name: "implementer acceptance", w: Walk{Line: []phase.Phase{phase.Implementing, phase.Accepting, phase.Retro, phase.Merging}},
			says: "goes back by itself to the start it held; otherwise sign off again."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, refuses := held.refusal(actSignOff, "LEGION-208", heldAcceptanceBack(tc.w))
			if !refuses || result.Refusal == nil || !strings.Contains(result.Refusal.Message, tc.says) {
				t.Fatalf("the sign-off's refusal = %t, %+v; want it to say %q", refuses, result.Refusal, tc.says)
			}
		})
	}
}
