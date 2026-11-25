package workflow

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A fix holds the branch of its task from its opening until its way is taken, and nothing else
// pushes there meanwhile: a confirmed conflict waits for the fix, a merge forward the implementer
// owes when the fix opens is the fix's own first work, and the way after the fix reads the conflict
// again. These tests read the merge-forward rows' kinds, steps, roles and heads, the record's ask,
// the fix's typed start facts and the steps' records, never a sentence.

// unreadRules are base rules the daemon could not read, under which R0 acts on every confirmed
// conflict (classify.ConflictNeedsMerge).
var unreadRules = &record.BaseRules{Unread: "HTTP 502"}

// ownedByFix1 names fix 1 of task a of LEGION-208's generation 1 as a merge forward's owner.
var ownedByFix1 = &record.MergeForwardFix{Generation: 1, Task: "a", Number: 1}

// conflictRecorded writes on LEGION-208's pull request that its head is held in a confirmed
// conflict with main, as two CONFLICTING reads a minute apart leave it (mergeability); green says
// whether the settlement standing on the head reads the base's required check ci green there.
func (r *fixRig) conflictRecorded(head string, green bool) {
	r.t.Helper()
	pr := recordedPR(r.t, r.pool, "LEGION-208")
	pr.Base, pr.ConflictingHead = "main", head
	if green {
		pr.CheckedHead, pr.CheckRuns = head, []record.AttemptRun{{Name: "ci", ID: 1, Head: head, Conclusion: record.RunSucceeded}}
	}
	seedPR(r.t, r.pool, pr)
}

// conflictRead is one read of LEGION-208's pull request finding head CONFLICTING with main, with
// the base's rules as the daemon read them, applied as the event id.
func (r *fixRig) conflictRead(id, head string, rules *record.BaseRules) {
	r.t.Helper()
	if result := r.apply(id, intake.PullRequestMergeability{Repo: statesRepo, Number: 42, Base: "main", HeadSHA: head,
		Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty, Rules: rules}); result.Refusal != nil {
		r.t.Fatalf("the conflicting read %s was refused: %+v", id, result.Refusal)
	}
}

// asked is the merge forward LEGION-208's pull request record holds now, nil for none.
func (r *fixRig) asked() *record.MergeForward {
	r.t.Helper()
	return recordedPR(r.t, r.pool, "LEGION-208").MergeForward
}

// undelivered is, for each merge-forward message the outbox holds, why the executor would finish
// it undelivered at its next attempt (MergeForwardAt), "" for one it would deliver.
func (r *fixRig) undelivered() []string {
	r.t.Helper()
	var whys []string
	for _, message := range mergeForwardMessages(r.t, r.pool) {
		applyInTx(r.t, r.pool, r.engine, func(ctx context.Context, tx pgx.Tx) error {
			_, _, why, err := MergeForwardAt(ctx, r.engine.store, tx, "LEGION-208", message.Role, message.Step, message.Head)
			whys = append(whys, why)
			return err
		})
	}
	return whys
}

// startFactsIn is the typed facts a start of the step of LEGION-208's task standing in at would be
// told now (startFacts).
func (r *fixRig) startFactsIn(at phase.Phase) startFacts {
	r.t.Helper()
	var facts startFacts
	onStored(r.t, r.pool, r.engine, "LEGION-208", func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		w, err := r.engine.walk(ctx, tx, issue)
		if err != nil {
			return err
		}
		issue.Phase, issue.Hold = at, nil
		facts, err = r.engine.startFacts(ctx, tx, issue, w)
		return err
	})
	return facts
}

// testerCompletesAt is the tester's completion of its step at commit, naming handoff, through the
// engine's handoff, its test record naming every state event applied so far; it answers the
// result.
func (r *fixRig) testerCompletesAt(commit, handoff string) intake.Result {
	r.t.Helper()
	return r.apply("test-done-"+handoff, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "tester-claim",
		Summary: "tested", Commit: commit, HandoffRecord: handoff, HandoffVersion: 1, StatesSerial: r.serial})
}

// A confirmed conflict at the head while a fix of the task holds its branch asks nobody: a
// CONFLICTING read of X, the record's confirmed conflicting head, writes no merge-forward row of
// any step, no checks-red notice and no ask on the record, with fix 1 running beside the review,
// with fix 1 reading its job, and with fix 1 ended and its way pending. The record keeps the
// conflicting head. The control, kept: the rule before (the seam mergeForwardBesideFix) asks the
// implementer at X.
func TestAConflictBesideAFixAsksNoMergeForward(t *testing.T) {
	for _, tc := range []struct {
		name  string
		given func(r *fixRig)
	}{
		{name: "fix 1 runs", given: func(r *fixRig) { r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage}) }},
		{name: "fix 1 reads its job", given: func(*fixRig) {}},
		{name: "fix 1 ended, its way pending", given: func(r *fixRig) {
			r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
			if result := r.apply("fix-done", fixCompletion(fixHead)); result.Refusal != nil {
				r.t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
			}
			if fix := r.latestFix(); fix.State != record.FixEnded || fix.Way != 0 {
				r.t.Fatalf("the fix = %+v, want it ended with its way pending while the review runs", fix)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := reviewing(t)
			r.fails(fixHead, "k-ci")
			tc.given(r)
			r.conflictRecorded(fixHead, false)
			r.conflictRead("conflict", fixHead, unreadRules)
			if messages := mergeForwardMessages(t, r.pool); len(messages) != 0 {
				t.Fatalf("merge-forward messages %+v, want none while the fix holds the branch", messages)
			}
			if told := capNotices(t, r.pool, "checks-red"); told != 0 {
				t.Fatalf("%d checks-red notices, want none while the fix holds the branch", told)
			}
			pr := recordedPR(t, r.pool, "LEGION-208")
			if pr.MergeForward != nil || pr.ConflictingHead != fixHead {
				t.Fatalf("the record holds merge forward %+v and conflicting head %q, want no ask and %s still conflicting", pr.MergeForward, pr.ConflictingHead, fixHead)
			}
		})
	}
	t.Run("control: the rule before asks beside the fix", func(t *testing.T) {
		r := reviewing(t)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		r.engine.mergeForwardBesideFix = true
		r.conflictRecorded(fixHead, false)
		r.conflictRead("conflict", fixHead, unreadRules)
		asks := 0
		for _, message := range mergeForwardMessages(t, r.pool) {
			if message.Step == record.MergeForwardAsk && message.Role == claim.RoleImplementer && message.Head == fixHead {
				asks++
			}
		}
		if asks != 1 {
			t.Fatalf("merge-forward messages %+v, want one ask of the implementer at %s under the rule before", mergeForwardMessages(t, r.pool), fixHead)
		}
	})
}

// askedOfImplementer is LEGION-208 on `implementer tester reviewer`, its task standing at the step
// whose phase moves leave, with the record's confirmed conflict at X asked of the implementer
// there: the ask, and in a phase after implementing the hold of that phase's worker.
func askedOfImplementer(t *testing.T, engine *Engine, moves ...TriggerKind) *fixRig {
	t.Helper()
	r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, moves...)
	r.conflictRecorded(fixHead, false)
	r.conflictRead("conflict", fixHead, unreadRules)
	if asked := r.asked(); asked == nil || asked.Head != fixHead || asked.Asked() != claim.RoleImplementer || asked.Landed != "" || asked.Fix != nil {
		t.Fatalf("the record's merge forward = %+v, want an ask of %s to the implementer, not landed and taken by no fix", asked, fixHead)
	}
	return r
}

// leavingCIEngine is returnEngine whose testing move leaves out the checks a CI job runs, the
// narrowed testing step a fix stands beside.
func leavingCIEngine() *Engine {
	return returnEngine()
}

// A fix that opens while the implementer owes the merge forward of the head takes that merge as its
// own first work: the fix opens at once, its job read stands, and its failure row names it; the
// record's ask names the fix (record.MergeForward.Fix), so each message of that ask still queued
// ends undelivered, the relaunch of a shared-copy role included, which at this branch is the
// delivery of those queued rows; the fix's start facts hold the ask's head and base; and the
// synchronize of the fix's one end push records the ask landed there, asking no review and
// starting no second writer of the branch.
//
// The controls, kept: under waitForOwedMerge (the rule that withheld the fix while a merge is owed)
// no fix opens; under mergeOutsideFix the ask is not taken, its queued ask is still delivered, and
// the fix's start facts hold no merge; and a fix that opens with no owed merge holds none either.
func TestAFixTakesTheMergeItsAgentOwes(t *testing.T) {
	t.Run("the takeover, in reviewing", func(t *testing.T) {
		r := askedOfImplementer(t, returnEngine(), TriggerImplementationReady, TriggerTesterPassed)
		queued := len(mergeForwardMessages(t, r.pool))
		if queued != 2 {
			t.Fatalf("merge-forward messages %+v, want the ask to the implementer and the hold to the reviewer", mergeForwardMessages(t, r.pool))
		}
		implementerStarts := len(r.startsOf(claim.RoleImplementer))
		r.fails(fixHead, "k-ci")
		assertStrings(t, "the fixes", r.fixes(), []string{fixOf(1, "reading", "k-ci")})
		if reads := r.jobReads(); len(reads) != 1 {
			t.Fatalf("job reads %+v, want one", reads)
		}
		assertStrings(t, "the failures", r.failures(), []string{failureOf("k-ci", "fix", 1)})
		if asked := r.asked(); asked == nil || asked.Fix == nil || *asked.Fix != *ownedByFix1 || asked.Landed != "" {
			t.Fatalf("the record's merge forward = %+v, want the ask taken by fix 1, not landed", asked)
		}
		for i, why := range r.undelivered() {
			if why == "" {
				t.Fatalf("merge-forward message %d of %+v would be delivered, want every message of the taken ask finished undelivered", i, mergeForwardMessages(t, r.pool))
			}
		}

		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		if facts := r.startFactsIn(phase.Implementing); !facts.Own || facts.Fix != 1 || facts.MergeHead != fixHead || facts.MergeBase != "main" {
			t.Fatalf("the fix's start facts = %+v, want fix 1's own, owing the merge of main at %s", facts, fixHead)
		}
		if starts := r.fixStarts(); len(starts) != 1 {
			t.Fatalf("fix starts %+v, want one", starts)
		}

		r.fixPushes(headTwo)
		if asked := r.asked(); asked == nil || asked.Landed != headTwo || asked.ReviewAsked || asked.Fix == nil || *asked.Fix != *ownedByFix1 {
			t.Fatalf("the record's merge forward = %+v, want fix 1's merge landed at %s with no review asked", asked, headTwo)
		}
		if result := r.apply("fix-done", fixCompletion(headTwo)); result.Refusal != nil {
			t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
		}
		if got := len(mergeForwardMessages(t, r.pool)); got != queued {
			t.Fatalf("merge-forward messages %+v, want only the %d queued before the fix took the ask", mergeForwardMessages(t, r.pool), queued)
		}
		if starts, fixStarts := r.startsOf(claim.RoleImplementer), r.fixStarts(); len(starts) != implementerStarts || len(fixStarts) != 1 {
			t.Fatalf("implementer starts %+v and fix starts %+v, want no start of the implementer beside fix 1's: no second writer of the branch", starts, fixStarts)
		}
	})

	t.Run("control: a fix with no owed merge holds none", func(t *testing.T) {
		r := reviewing(t)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		if facts := r.startFactsIn(phase.Implementing); !facts.Own || facts.Fix != 1 || facts.MergeHead != "" || facts.MergeBase != "" {
			t.Fatalf("the fix's start facts = %+v, want fix 1's own with no merge owed", facts)
		}
	})

	t.Run("control: the withheld start opens no fix", func(t *testing.T) {
		engine := returnEngine()
		engine.waitForOwedMerge = true
		r := askedOfImplementer(t, engine, TriggerImplementationReady, TriggerTesterPassed)
		r.fails(fixHead, "k-ci")
		assertStrings(t, "the fixes", r.fixes(), nil)
		if reads := r.jobReads(); len(reads) != 0 {
			t.Fatalf("job reads %+v, want none while the start is withheld", reads)
		}
	})

	t.Run("control: a merge outside the fix keeps its own ask", func(t *testing.T) {
		engine := returnEngine()
		engine.mergeOutsideFix = true
		r := askedOfImplementer(t, engine, TriggerImplementationReady, TriggerTesterPassed)
		r.fails(fixHead, "k-ci")
		if asked := r.asked(); asked == nil || asked.Fix != nil {
			t.Fatalf("the record's merge forward = %+v, want the ask taken by no fix", asked)
		}
		messages, whys := mergeForwardMessages(t, r.pool), r.undelivered()
		for i, message := range messages {
			if message.Step == record.MergeForwardAsk && whys[i] != "" {
				t.Fatalf("the queued ask %+v would end undelivered (%s), want it still delivered beside the fix", message, whys[i])
			}
		}
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		if facts := r.startFactsIn(phase.Implementing); facts.MergeHead != "" || facts.MergeBase != "" {
			t.Fatalf("the fix's start facts = %+v, want no merge in them", facts)
		}
	})

	// In testing the ask holds the tester as well. After the takeover no message of the ask reaches
	// either role of the shared working copy; the fix's end push lands the merge at Y; the check no
	// CI job runs passes at X; and the tester's completion at X, through the engine's handoff, is
	// recorded with its report and its end, and the way after the fix starts the tester once more,
	// at Y, with one return. So again after a restart of the daemon, and after the fix's landing
	// moved into the record's earlier landings. The control, kept: with the ask left a merge no fix
	// took (ordinaryMergeAfterTakeover), the tester's completion at X is refused as one at a head a
	// merge forward replaced.
	for _, tc := range []struct {
		name    string
		seam    func(engine *Engine)
		before  func(r *fixRig)
		refused bool
	}{
		{name: "the tester's report at X stands"},
		{name: "the tester's report at X stands after a restart", before: func(r *fixRig) { r.engine = leavingCIEngine() }},
		{name: "the tester's report at X stands once the landing is an earlier one", before: func(r *fixRig) {
			pr := recordedPR(r.t, r.pool, "LEGION-208")
			pr.MergeForward = &record.MergeForward{Head: headTwo, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing,
				Why: record.ConflictRulesUnread, AskedAt: stepClock, Earlier: []record.LandedMergeForward{{Head: fixHead, Landed: headTwo, Fix: ownedByFix1}}}
			seedPR(r.t, r.pool, pr)
		}},
		{name: "control: an ordinary merge after the takeover refuses X", seam: func(engine *Engine) { engine.ordinaryMergeAfterTakeover = true }, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := leavingCIEngine()
			if tc.seam != nil {
				tc.seam(engine)
			}
			r := askedOfImplementer(t, engine, TriggerImplementationReady)
			assertPhase(t, r.pool, phase.Testing)
			r.fails(fixHead, "k-ci")
			for i, why := range r.undelivered() {
				if (why == "") != tc.refused {
					t.Fatalf("merge-forward message %d of %+v answers %q at its next attempt; want it delivered only for a merge no fix took", i, mergeForwardMessages(t, r.pool), why)
				}
			}
			r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
			r.fixPushes(headTwo)
			if result := r.apply("fix-done", fixCompletion(headTwo)); result.Refusal != nil {
				t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
			}
			r.at(fixHead, stateOf("k-unit", stateYes, "", "tester"), ciPassing("k-ci2"))
			if tc.before != nil {
				tc.before(r)
			}
			// A pending start of a phase takes the place of the one before it, so the start the way
			// makes is read by the tester's entry it raises.
			entry := roleRowOf(t, r.pool, r.engine, "LEGION-208", claim.RoleTester).Entry

			result := r.testerCompletesAt(fixHead, "test-at-x")
			if tc.refused {
				if code := refusalCode(result); code != "HANDOFF_HEAD_MERGED_FORWARD" {
					t.Fatalf("the tester's completion at %s was answered %q (%+v), want HANDOFF_HEAD_MERGED_FORWARD", fixHead, code, result.Refusal)
				}
				return
			}
			if result.Refusal != nil {
				t.Fatalf("the tester's completion at %s was refused: %+v", fixHead, result.Refusal)
			}
			end := r.runEnd(2, 1)
			if end == nil || end.Result == nil || *end.Result != string(ResultPassed) || end.Report == nil || *end.Report != "test-at-x" ||
				end.EndCommit == nil || *end.EndCommit != fixHead {
				t.Fatalf("the tester's run end = %+v, want it passed at %s with its report test-at-x", end, fixHead)
			}
			var rerun []record.SuperviseRequest
			for _, start := range r.startsOf(claim.RoleTester) {
				if start.Entry > entry {
					rerun = append(rerun, start)
				}
			}
			if len(rerun) != 1 || rerun[0].Entry != entry+1 || rerun[0].MoveTo != headTwo {
				t.Fatalf("tester starts after entry %d: %+v, want one, at entry %d, moving its working copy to %s", entry, rerun, entry+1, headTwo)
			}
			if counts := r.counts(); counts.Pass != 1 {
				t.Fatalf("the task counts %+v, want one return", counts)
			}
			if fix := r.latestFix(); fix.Way != wayAfterFix {
				t.Fatalf("the fix = %+v, want way %d taken", fix, wayAfterFix)
			}
		})
	}

	// The fence still stands for a merge no fix took: with a landing of X at Y that no fix took
	// kept before a later landing that fix 1 took, the tester's completion at X is refused.
	t.Run("control: an ordinary landing beside the fix's own history refuses X", func(t *testing.T) {
		r := newFixRig(t, leavingCIEngine(), []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady)
		pr := recordedPR(t, r.pool, "LEGION-208")
		pr.HeadSHA, pr.Base = headThree, "main"
		pr.MergeForward = &record.MergeForward{Head: headThree, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing,
			Why: record.ConflictRulesUnread, AskedAt: stepClock, Earlier: []record.LandedMergeForward{
				{Head: fixHead, Landed: headTwo}, {Head: headTwo, Landed: headThree, Fix: ownedByFix1}}}
		seedPR(t, r.pool, pr)
		if code := refusalCode(r.testerCompletesAt(fixHead, "test-at-x")); code != "HANDOFF_HEAD_MERGED_FORWARD" {
			t.Fatalf("the tester's completion at %s was answered %q, want HANDOFF_HEAD_MERGED_FORWARD", fixHead, code)
		}
		if code := refusalCode(r.testerCompletesAt(headTwo, "test-at-y")); code != "" {
			t.Fatalf("the tester's completion at %s, the head the fix's own merge replaced, was answered %q, want it past the fence", headTwo, code)
		}
	})
}

// The way after a fix reads the conflict that waited for it (Task 5g, row 4): the review requests
// changes while fix 1 runs, X being the record's confirmed conflicting head, and fix 1 ends with no
// commit. Way 2 starts the tester at X, writes no merge-forward row in its transaction, and queues
// one read of GitHub's merge state, whose answer R0 judges at X: CONFLICTING with every check the
// base requires green and no current-branch rule asks nobody; the same answer with the required
// check not green asks the implementer once, with one hold of the tester, and a second answer asks
// nothing more. Where the fix's end push moved the head to Y (row 3), the way reads nothing for X.
// The controls, kept: asking at the way with no rules read (askAtWayWithNoRules) asks the
// implementer in case one, and taking the read out (noAskAtWay) asks nothing in case two.
func TestAConflictBesideAFixIsAskedAtItsWay(t *testing.T) {
	ciRequired := &record.BaseRules{Required: []string{"ci"}}
	beside := func(t *testing.T, engine *Engine, green bool, end string) (*fixRig, *[]string) {
		t.Helper()
		var reads []string
		engine.cfg.ReadMergeState = func(issue string) { reads = append(reads, issue) }
		r := newFixRig(t, engine, []string{"implementer", "tester", "reviewer"}, TriggerImplementationReady, TriggerTesterPassed)
		r.fails(fixHead, "k-ci")
		r.readJob(intake.FixJobRead{Name: "unit", URL: fixPage})
		r.conflictRecorded(fixHead, green)
		r.conflictRead("conflict-beside", fixHead, ciRequired)
		if messages := mergeForwardMessages(t, r.pool); len(messages) != 0 {
			t.Fatalf("merge-forward messages %+v while the fix runs, want none", messages)
		}
		r.reviewedX()
		r.move(TriggerReviewRejected, "", reviewReport("the review's report"))
		if end != fixHead {
			r.fixPushes(end)
		}
		// The reads the fix's own push owes are Y's; only the reads from the fix's end on count here.
		reads = nil
		if result := r.apply("fix-done", fixCompletion(end)); result.Refusal != nil {
			t.Fatalf("the fix's completion was refused: %+v", result.Refusal)
		}
		return r, &reads
	}
	asks := func(r *fixRig) []record.MergeForwardMessage { return mergeForwardMessages(r.t, r.pool) }

	t.Run("case one: a conflict R0 leaves alone asks nobody", func(t *testing.T) {
		r, reads := beside(t, returnEngine(), true, fixHead)
		// At a head held in a confirmed conflict the return is a conflict round's, counted apart from
		// the pairs (record.ReturnCounts.ConflictRounds), so only the one return is read here.
		r.assertWay(wayAfterFix, phase.Testing, claim.RoleTester, fixHead, 1, "")
		if messages := asks(r); len(messages) != 0 {
			t.Fatalf("merge-forward messages %+v in the way's transaction, want none", messages)
		}
		if len(*reads) != 1 || (*reads)[0] != "LEGION-208" {
			t.Fatalf("merge-state reads asked %v, want one of LEGION-208", *reads)
		}
		r.conflictRead("answer-1", fixHead, ciRequired)
		if messages := asks(r); len(messages) != 0 {
			t.Fatalf("merge-forward messages %+v after the answer, want none: R0 leaves the conflict alone", messages)
		}
	})

	t.Run("case two: a conflict R0 acts on is asked once at the answer", func(t *testing.T) {
		r, reads := beside(t, returnEngine(), false, fixHead)
		if messages := asks(r); len(messages) != 0 || len(*reads) != 1 {
			t.Fatalf("merge-forward messages %+v and reads %v after the way, want none and one read", messages, *reads)
		}
		r.conflictRead("answer-1", fixHead, ciRequired)
		want := []record.MergeForwardMessage{
			{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: fixHead, Base: "main", Phase: phase.Testing, PullRequest: 42, Why: record.ConflictCheckNotGreen},
			{Role: claim.RoleTester, Step: record.MergeForwardHold, Head: fixHead, Base: "main", Phase: phase.Testing, PullRequest: 42, Why: record.ConflictCheckNotGreen},
		}
		if got := asks(r); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("merge-forward messages %+v, want %+v", got, want)
		}
		r.conflictRead("answer-2", fixHead, ciRequired)
		if got := asks(r); len(got) != len(want) {
			t.Fatalf("merge-forward messages %+v after a second answer, want no second ask", got)
		}
	})

	t.Run("row 3: a head the fix moved reads nothing for X", func(t *testing.T) {
		r, reads := beside(t, returnEngine(), false, headTwo)
		r.assertWay(wayAfterFix, phase.Testing, claim.RoleTester, headTwo, 1, "")
		if messages := asks(r); len(messages) != 0 || len(*reads) != 0 {
			t.Fatalf("merge-forward messages %+v and reads %v, want none: Y's own reads decide", messages, *reads)
		}
	})

	t.Run("control: the way that asks with no rules read asks in case one", func(t *testing.T) {
		engine := returnEngine()
		engine.askAtWayWithNoRules = true
		r, _ := beside(t, engine, true, fixHead)
		got := asks(r)
		if len(got) == 0 || got[0].Step != record.MergeForwardAsk || got[0].Role != claim.RoleImplementer || got[0].Head != fixHead {
			t.Fatalf("merge-forward messages %+v, want the implementer asked at the way", got)
		}
	})

	t.Run("control: with no read at the way case two asks nothing", func(t *testing.T) {
		engine := returnEngine()
		engine.noAskAtWay = true
		r, reads := beside(t, engine, false, fixHead)
		if messages := asks(r); len(messages) != 0 || len(*reads) != 0 {
			t.Fatalf("merge-forward messages %+v and reads %v, want none with the read taken out", messages, *reads)
		}
	})
}
