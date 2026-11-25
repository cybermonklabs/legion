package supervise

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// A launch the runtime refuses because the claim's issue pod cannot run it until that pod's update
// round or its release replaces it (runtime.AwaitsPodUpdate) is charged nothing and leaves the
// claim suspended by the daemon, running nothing, so no live claim keeps either release rule off
// the pod. The claim is the only claim of its pod here. The issue and the controller are told at
// the refusal, naming the pod; whether a start of the claim is queued is the daemon's to read from
// its outbox (workflow's podRefusedLaunch). While that pod is still the issue's pod, each resume is
// refused again with no launch and no write of the claim, so the pod comes to be quiet, and the
// telling comes again only after an hour. Once another pod stands, the next resume launches the
// claim.
//
// The control is the rule before this one: the refused claim is left launching, tried again at
// every probe interval, written at each try, and nobody is told.
func TestALaunchItsPodRefusesLeavesTheClaimSuspendedUntilAnotherPodStands(t *testing.T) {
	h := newHarness(t)
	var told []string
	h.deps.PodRefused = func(_ *Machine, _ Claim, pod, _ string) { told = append(told, pod) }
	h.restart()
	issue := h.claim().Issue
	h.rt.ScriptIssuePod(issue, "pod-1")
	refusal := runtime.AwaitsPodUpdate{Pod: "pod-1", Reason: "launch " + string(testToken) + ": the pod pod-1 of " + issue + " was built without checker, the role of its step"}
	h.rt.ScriptSpawn(fake.SpawnResult{Err: refusal})

	if err := h.handle(RequestSpawn{Claim: testToken}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("the spawn answered %v, want the runtime's refusal, which is runtime.ErrAwaitsPodUpdate", err)
	}
	if c := h.claim(); c.State != StateSuspended || c.SuspendedBy != SuspendedByDaemon || c.Budgets != (Budgets{}) || c.Locator != nil ||
		!strings.Contains(c.LastStartFailure, refusal.Reason) {
		t.Fatalf("after the refusal the claim is %s by %q with budgets %+v, locator %+v and last start failure %q; want it suspended by the daemon, charged nothing, running nothing, keeping the refusal",
			c.State, c.SuspendedBy, c.Budgets, c.Locator, c.LastStartFailure)
	}
	if len(told) != 1 || told[0] != "pod-1" {
		t.Fatalf("the refusal told %q, want one report of pod-1", told)
	}

	writes := 0
	h.store.onPut = func(Claim) { writes++ }
	for range 3 {
		h.advance(testProbe)
		if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's step"}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
			t.Fatalf("a resume while pod-1 stands answered %v, want a refusal that is runtime.ErrAwaitsPodUpdate", err)
		}
	}
	h.wantCalls("Spawn", 1)
	h.wantState(StateSuspended)
	if writes != 0 || len(told) != 1 {
		t.Fatalf("the resumes while pod-1 stands wrote the claim %d times and told %q; want no write and the one report", writes, told)
	}
	h.advance(time.Hour)
	if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's step"}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("a resume an hour later answered %v, want a refusal that is runtime.ErrAwaitsPodUpdate", err)
	}
	if len(told) != 2 || told[1] != "pod-1" || writes != 0 {
		t.Fatalf("an hour later the reports are %q and the claim was written %d times; want a second report of pod-1, and no write", told, writes)
	}

	h.rt.ScriptIssuePod(issue, "pod-2")
	if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's step"}); err != nil {
		t.Fatalf("the resume once another pod stands = %v, want the claim launched", err)
	}
	h.wantCalls("Spawn", 2)
	if c := h.claim(); c.Locator == nil {
		t.Fatalf("after the resume once another pod stands the claim records no process: %+v", c)
	}
}

// A runtime that cannot read which pod the issue's Sandbox holds now (runtime.IssuePodReader's
// read false) has not said the refusing pod is gone, so a resume while it cannot read is refused
// as one while that pod stands: nothing is launched and nothing of the claim written, and the
// telling comes again only after an hour, where forgetting the pod would launch into it at each
// try, be refused there again, write the claim and tell the issue and the controller each time.
// Once the runtime reads another pod, the next resume launches the claim.
func TestAResumeWhileTheRuntimeCannotReadTheIssuesPodIsRefusedAsWhileThePodStands(t *testing.T) {
	h := newHarness(t)
	var told []string
	h.deps.PodRefused = func(_ *Machine, _ Claim, pod, _ string) { told = append(told, pod) }
	h.restart()
	issue := h.claim().Issue
	h.rt.ScriptIssuePod(issue, "pod-1")
	refusal := runtime.AwaitsPodUpdate{Pod: "pod-1", Reason: "launch " + string(testToken) + ": the pod pod-1 of " + issue + " was built without checker, the role of its step"}
	h.rt.ScriptSpawn(fake.SpawnResult{Err: refusal})
	if err := h.handle(RequestSpawn{Claim: testToken}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("the spawn answered %v, want the runtime's refusal", err)
	}

	h.rt.ScriptIssuePodUnread(issue)
	writes := 0
	h.store.onPut = func(Claim) { writes++ }
	for range 3 {
		h.advance(testProbe)
		if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's step"}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
			t.Fatalf("a resume while the runtime cannot read the issue's pod answered %v, want a refusal that is runtime.ErrAwaitsPodUpdate", err)
		}
	}
	h.wantCalls("Spawn", 1)
	h.wantState(StateSuspended)
	if writes != 0 || len(told) != 1 {
		t.Fatalf("the resumes while the runtime cannot read the pod wrote the claim %d times and told %q; want no write and the one report", writes, told)
	}
	h.advance(time.Hour)
	if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's step"}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("a resume an hour later answered %v, want a refusal that is runtime.ErrAwaitsPodUpdate", err)
	}
	if len(told) != 2 || told[1] != "pod-1" || writes != 0 {
		t.Fatalf("an hour later the reports are %q and the claim was written %d times; want a second report of pod-1 and no write", told, writes)
	}

	h.rt.ScriptIssuePod(issue, "pod-2")
	if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "it owns its issue's step"}); err != nil {
		t.Fatalf("the resume once another pod stands = %v, want the claim launched", err)
	}
	h.wantCalls("Spawn", 2)
}

// Each fact of a refused launch that the store does not take has a mark of its own on the claim,
// keyed by the fact's event id, and the claim shows the oldest (Claim.UnrecordedRefusal). Here the
// refusal's report fails twice, then a re-check at the end of a start row fails once and is taken:
// the report's mark stands, with when its first try failed and both its tries, since that fact is
// still not taken. Once the machine drops the refusal both facts name — another pod stands — every
// mark goes at once, not at a fact's next try, which may be an hour off; and a failed try of a fact
// about a refusal the machine no longer reports marks nothing.
func TestEachUnrecordedRefusalFactKeepsItsOwnMarkUntilItsRefusalGoes(t *testing.T) {
	h := newHarness(t)
	issue := h.claim().Issue
	h.rt.ScriptIssuePod(issue, "pod-1")
	h.rt.ScriptSpawn(fake.SpawnResult{Err: runtime.AwaitsPodUpdate{Pod: "pod-1", Reason: "launch " + string(testToken) + ": the pod pod-1 of " + issue + " was built without checker"}})
	if err := h.handle(RequestSpawn{Claim: testToken}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("the spawn answered %v, want the runtime's refusal", err)
	}
	const report, recheck = "pod-refused:tester:1", "refused-start-ended:tester:7"
	first := h.clock.Now()
	h.m.RefusalNotRecorded(report, "pod-1", errors.New("the database did not answer"))
	h.clock.Advance(2 * time.Second)
	h.m.RefusalNotRecorded(recheck, "pod-1", errors.New("the serializer was busy"))
	h.clock.Advance(2 * time.Second)
	h.m.RefusalNotRecorded(report, "pod-1", errors.New("the database did not answer again"))
	h.m.RefusalRecorded(recheck)

	want := &UnrecordedRefusal{Since: first, Attempts: 2, Error: "the database did not answer again"}
	if got := h.m.View().UnrecordedRefusal; got == nil || *got != *want {
		t.Fatalf("the view shows %+v, want the report's mark %+v: the re-check taken clears its own mark alone", got, *want)
	}
	if got := h.m.Claim().UnrecordedRefusal; got == nil || *got != *want {
		t.Fatalf("the claim shows %+v, want the report's mark %+v", got, *want)
	}

	h.m.RefusalNotRecorded(recheck, "pod-1", errors.New("the serializer was busy"))
	h.rt.ScriptIssuePod(issue, "pod-2")
	if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "pod-2 stands"}); err != nil {
		t.Fatalf("the resume once pod-2 stands answered %v, want the claim brought back", err)
	}
	if pod := h.m.RefusedPod(); pod != "" {
		t.Fatalf("once pod-2 stands the claim is refused by %q, want the refusal dropped", pod)
	}
	if got := h.m.View().UnrecordedRefusal; got != nil {
		t.Fatalf("once the refusal is dropped the view shows %+v, want no mark", *got)
	}
	h.m.RefusalNotRecorded(report, "pod-1", errors.New("the database did not answer"))
	if got := h.m.View().UnrecordedRefusal; got != nil {
		t.Fatalf("a failed try of a fact about pod-1's dropped refusal marked %+v, want nothing", *got)
	}
}

// The rule's resume of a claim whose last launch its pod refused is refused by that pod while it
// stands, before the claim's session is asked about, as the operator's resume is (resume). A claim
// that records a session it does not hold — another claim recorded it first — is refused the
// rule's resume for that session too, but that refusal answers nil, and the outbox would then end
// the start that asked while the pod still stands: the end of a start row of a refused claim asks
// the workflow for its start again (internal/daemon's startRowEnded), and a start that ends the same
// way asks again, at the database's speed. The pod's refusal answers runtime.ErrAwaitsPodUpdate,
// which the start waits on. Nothing is launched and nothing of the claim is written. Once another
// pod stands, the session is asked about, and the resume is refused for it as before.
func TestARuleResumeOfARefusedClaimWaitsForItsPodWhateverItsSession(t *testing.T) {
	const other = claim.Token("legion-legion-LEGION-209-tester")
	const session, file = "ses_other", "/legion/state/sessions/ses_other.jsonl"
	refused := queuedClaim()
	refused.Generation, refused.State, refused.SuspendedBy = 1, StateSuspended, SuspendedByDaemon
	refused.Session, refused.SessionFile = session, file
	h := newHarnessOf(t, refused)
	if holders, err := h.store.HoldSessions(h.ctx, "legion", other, RecordedHolds(session, file)); err != nil || slices.ContainsFunc(holders, func(c claim.Token) bool { return c != other }) {
		t.Fatalf("another claim's hold of the session = %v, %v; want every name its own", holders, err)
	}
	h.rt.ScriptIssuePod(refused.Issue, "pod-1")
	h.m.mu.Lock()
	h.m.refusedPod = "pod-1"
	h.m.mu.Unlock()
	writes := 0
	h.store.onPut = func(Claim) { writes++ }

	if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "start row 7 of LEGION-209"}); !errors.Is(err, runtime.ErrAwaitsPodUpdate) {
		t.Fatalf("the rule's resume while pod-1 stands answered %v, want the pod's refusal, which is runtime.ErrAwaitsPodUpdate", err)
	}
	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
	if writes != 0 {
		t.Fatalf("the refused resume wrote the claim %d times, want none", writes)
	}

	h.rt.ScriptIssuePod(refused.Issue, "pod-2")
	if err := h.handle(RequestRuleResume{Claim: testToken, Reason: "start row 7 of LEGION-209"}); err != nil {
		t.Fatalf("the rule's resume once pod-2 stands answered %v, want nil: the rule leaves a claim recording a session it does not hold suspended", err)
	}
	h.wantState(StateSuspended)
	h.wantCalls("Resume", 0)
	if pod := h.m.RefusedPod(); pod != "" {
		t.Fatalf("once pod-2 stands the claim is still refused by %q, want the refusal dropped", pod)
	}
}
