package replay

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// replayFact is what makes a fact of this package an intake.Fact. intake.Fact's one method is
// unexported, so a type of another package is a fact only by embedding one of intake's; the
// embedded intake.EntrySweep carries nothing and names no issue, and no handler of the daemon's
// chain takes a replay fact for one, since each of them reads a fact by its own type, which is
// this package's: the mirror and the engine do nothing for it. Admission runs its pass on every
// replay fact whatever its transaction wrote (ownFact, registered with admit's UsePassFacts),
// since each stands for a change the replayed daemon made outside the facts the replay applies
// (a change of the P0 bound, a claim its supervisor settled, a mark or a refusal it kept) and
// several write no record of the project, so the pass rule would run none for them.
type replayFact struct{ intake.EntrySweep }

// replayOwn marks a fact of this package (ownFact).
func (replayFact) replayOwn() {}

// ownFact says whether fact is one of this package's own (replayFact).
func ownFact(fact intake.Fact) bool {
	_, own := fact.(interface{ replayOwn() })
	return own
}

// RecordedMove is one recorded phase move a replay enters at the end of its work segment, by
// workflow.(*Engine).ReplayTransition (MoveTransition): Issue at Generation moved From To by
// Trigger. The replay's handler, which runs before the daemon's chain, makes the move, and the
// pass of the same fact gives out whatever slot the move frees. It is named apart from Move, the
// trace's reading of a `workflow: phase changed` line, which is what it enters again.
type RecordedMove struct {
	replayFact
	Issue      string
	Generation uint64
	From, To   phase.Phase
	Trigger    workflow.TriggerKind
}

// ReadyMark is the merger's ready mark of the release at 9542c6fb inside merging (TriggerReady,
// MoveMark, assumption 20), which the replay's handler writes into the replay's own table of marks
// (replayTables) under a rule that keeps a slot in merging and gives it back at the mark
// (record.SlotRule.ReadsReadyMark), whose admission pass reads it there (ruleTables). Under every
// other rule it changes nothing.
type ReadyMark struct {
	replayFact
	Issue      string
	Generation uint64
}

// Settled is the replay's own fact once its worker stub has settled the claim of a root's
// architect whose root has handed work to a person (record.Issue.DeclaredSeq): the daemon applies
// a fact when such a claim settles (supervision's ClaimReady, TaskConfirmed), so the admission pass
// that enters the root's wait on the person (workflow's ReconcileAwaiting) runs at the moment the
// claim settled, and the replay's stub, which applies no fact of supervision's, applies this one in
// their place. It carries the root, and no handler of the chain reads it but
// admission, whose pass runs.
type Settled struct {
	replayFact
	Issue string
}

// HeldSignOff is the replay's own fact when the workflow refuses a sign-off because an issue beneath
// the parent is still in the workflow (SIGNOFF_CHILD_IN_WORKFLOW; Divergence.HeldSignOffs): the
// daemon keeps no record of the refusal, so the replay's handler writes it into a table of its own
// (replayTables) under a rule whose start may take a cover, which reads the parent's wait on the
// issues beneath it from there (ruleTables, record.LineFacts.BeneathWaits). Under every other rule
// it changes nothing. Its pass runs in the same transaction, at the moment of the refusal.
type HeldSignOff struct {
	replayFact
	Issue      string
	Generation uint64
}

// FirstBoot is the first boot of a release that runs the rule cover over a start state another
// rule left (spec v4, R9): the store migration of that release gives a slot to each issue the
// daemon started that holds none — a waiter that gave its slot back, an issue past its review, an
// issue that owes its next worker a start — and the first admission pass, which runs in the same
// transaction, works out the covers (admit's settleCovers). The replay applies it at the window's
// start under a rule whose start may take a cover, and the handler does nothing under any other.
type FirstBoot struct {
	replayFact
}

// P0BoundChanged is a change of R10's P0 bound the window records (Window.P0Bounds), applied at its
// moment: the daemon makes an admission pass at each change of the bound, so a raise starts the P0
// issues it lets in at once (cybermonklabs/legion#5488), and the replay makes that pass with this
// fact, which no handler of the chain reads but admission, whose pass runs under the bound in force
// from that moment (p0BoundAt). Under a rule that starts no P0 issue outside the cap the pass finds
// what the one before it found. At the forward merge that brings #5488 the replay applies that
// change's own fact in its place.
type P0BoundChanged struct {
	replayFact
	Bound int
}

// replayHandler is the handler a replay puts before the daemon's chain (admit.Handlers) for the
// replay's own facts: a RecordedMove through the engine's ReplayTransition, refused with its reason
// where the issue does not stand where the record says; a ReadyMark and a HeldSignOff into the
// replay's tables under a rule that reads them (rule); and a FirstBoot's migration of the start
// state, over records, the store of project. Every other fact it leaves to the chain.
type replayHandler struct {
	engine  *workflow.Engine
	clock   func() time.Time
	rule    record.SlotRule
	records record.Store
	project string
}

// refusedMove is the code of a replay's refused move, whose message is ReplayTransition's reason.
const refusedMove = "REPLAY_MOVE_REFUSED"

// Apply makes a replay fact's change in the fact's own transaction. A ready mark is kept with the
// merger's phase entry it falls in, and a held sign-off with the architect's (record.PhaseRow.Entry,
// zero where the role has none), so each reads only while the issue stands in that visit to the
// phase.
func (h replayHandler) Apply(ctx context.Context, tx pgx.Tx, fact intake.Fact) (intake.Result, error) {
	switch fact := fact.(type) {
	case RecordedMove:
		reason, err := h.engine.ReplayTransition(ctx, tx, fact.Issue, fact.Generation, fact.From, fact.Trigger, fact.To)
		if err != nil || reason == "" {
			return intake.Result{}, err
		}
		return intake.Result{Refusal: &intake.Refusal{Status: 409, Code: refusedMove, Message: reason}}, nil
	case ReadyMark:
		if !h.rule.ReadsReadyMark() {
			return intake.Result{}, nil
		}
		_, err := tx.Exec(ctx, `insert into replay_ready_marks (issue, generation, entry, marked_at)
			select $1::text, $2::bigint, coalesce((select entry from phases where issue = $1::text and role = $3::text), 0), $4::timestamptz
			on conflict (issue, generation, entry) do nothing`, fact.Issue, int64(fact.Generation), string(claim.RoleMerger), h.clock())
		return intake.Result{}, err
	case HeldSignOff:
		if h.rule.OrMain().Take != record.TakeCover {
			return intake.Result{}, nil
		}
		_, err := tx.Exec(ctx, `insert into replay_held_signoffs (issue, generation, entry, held_at)
			select $1::text, $2::bigint, coalesce((select entry from phases where issue = $1::text and role = $3::text), 0), $4::timestamptz
			on conflict (issue, generation, entry) do nothing`, fact.Issue, int64(fact.Generation), string(claim.RoleArchitect), h.clock())
		return intake.Result{}, err
	case FirstBoot:
		if h.rule.OrMain().Take != record.TakeCover {
			return intake.Result{}, nil
		}
		return intake.Result{}, h.firstBoot(ctx, tx)
	}
	return intake.Result{}, nil
}

// firstBoot is the store migration of a FirstBoot: each issue of the project in the workflow that
// the rule keeps a slot for where it stands (record.LineFacts.NeedsSlot), holds none, and that the
// daemon has started — it carries the mark of a release for a wait (record.Issue.ReleasedForWaitAt),
// owes its next worker a start (record.LineFacts.Owed), or stands in accepting, retro or merging
// (record.InSlotlessPhase) — takes a slot, its mark cleared, at the clock's moment. The count may stand
// above the cap after it, R5's second case; the pass that follows starts nothing R4 does not start
// until it is below.
func (h replayHandler) firstBoot(ctx context.Context, tx pgx.Tx) error {
	issues, err := h.records.LiveIssues(ctx, tx, h.project)
	if err != nil {
		return fmt.Errorf("replay: list the issues of %s at the first boot: %w", h.project, err)
	}
	slots, err := h.records.Slots(ctx, tx)
	if err != nil {
		return fmt.Errorf("replay: list the slots at the first boot: %w", err)
	}
	facts, err := record.ReadLineFactsUnder(ctx, tx, h.records, h.project, issues, h.engine.GatedSteps(), h.rule)
	if err != nil {
		return err
	}
	holds := make(map[string]bool, len(slots))
	used := make(map[int]bool, len(slots))
	for _, slot := range slots {
		holds[slot.Issue], used[slot.Index] = true, true
	}
	byKey := make(map[string]record.Issue, len(issues))
	for _, issue := range issues {
		byKey[issue.Key] = issue
	}
	live := record.LiveRoots(issues, facts)
	index := 0
	for _, issue := range issues {
		_, running := live[issue.Tree]
		_, owes := facts.Owed[issue.Key]
		started := issue.ReleasedForWaitAt != nil || owes || record.InSlotlessPhase(issue)
		if holds[issue.Key] || !started || !facts.NeedsSlot(issue, byKey[issue.Tree], running) {
			continue
		}
		for used[index] {
			index++
		}
		used[index] = true
		if err := h.records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, Index: index, AdmittedAt: h.clock()}); err != nil {
			return fmt.Errorf("replay: give %s its slot at the first boot: %w", issue.Key, err)
		}
		if issue.ReleasedForWaitAt != nil {
			if err := h.records.ClearReleasedForWait(ctx, tx, issue.Key); err != nil {
				return fmt.Errorf("replay: clear the slot release of %s at the first boot: %w", issue.Key, err)
			}
		}
	}
	return nil
}

// The assumptions a fact a replay leaves out rests on (FactSources), and a row its worker stub
// finishes without acting (OutboxHandling): the tool's own, after the plan's, each named where a
// run left out such a fact or finished such a row.
const (
	assumeNoticesChangeNothing      = 26
	assumePublicationsDecideNothing = 27
	assumeWorkspaceInSegment        = 28
	assumeNoGitHub                  = 29
	assumeNoProductionCheck         = 30
	assumeSupervisionSettles        = 31
	assumeCompletionPassed          = 32
	assumeBootRepairsNothing        = 33
)

// FactSources is how a replay comes by each kind of fact the daemon applies, by the name of its
// type in package intake: every type intake.Fact is, the ones Dispatch events decode to and the
// ones the daemon's other producers make (its API, ticks, boots, outbox executors, supervision and
// GitHub intake). A fact type with no entry here has no source in the replay, and the inventory
// test (TestEveryFactANonDispatchProducerAppliesHasASource) names it.
var FactSources = map[string]FactSource{
	// The facts Dispatch events decode to: each recorded event of the project's issues through
	// intake.DecodeDispatchEvent, at its recorded time or offset, carrying the replay's copy of
	// Dispatch (the shadow), with the preparation the daemon's consumer makes stood in for.
	"DispatchIssue":         {Way: FactInjected, From: "each recorded issue event of the project, through intake.DecodeDispatchEvent and the preparation's stand-in, the echo of each status write the stub makes, and each move of a tree root's rank under FirstStarts recorded"},
	"DispatchArtifact":      {Way: FactInjected, From: "each recorded artifact.approved, artifact.changes_requested and artifact.version event, and the gate seed the stub executes"},
	"DispatchAskOpened":     {Way: FactInjected, From: "each recorded ask.opened event"},
	"DispatchAskRestored":   {Way: FactInjected, From: "each recorded ask event of a restored decision block"},
	"DispatchAskClosed":     {Way: FactInjected, From: "each recorded ask.answered and ask.resolved event"},
	"DispatchAskReply":      {Way: FactInjected, From: "each recorded comment on an ask and ask.handed_back event"},
	"DispatchApprovalMoved": {Way: FactInjected, From: "each recorded ask.edited event of an approval request a version moved past"},
	"DispatchClaim":         {Way: FactInjected, From: "each recorded issue.claimed and issue.released event"},

	// The API's facts, rebuilt from the record's lines at the end of their segment or their
	// recorded offset in it.
	"SignOff":         {Way: FactInjected, From: "a recorded move by sign_off, at the end of its accepting segment, and again at each leave of a member beneath while it is refused"},
	"BackwardMove":    {Way: FactInjected, From: "a recorded move by backward, at the end of its segment"},
	"CloseRoot":       {Way: FactInjected, From: "a recorded leave of a root at done with no person's close and no move to done, at its offset in the architect's segment"},
	"RetryOrEscalate": {Way: FactInjected, From: "each recorded `workflow: a held phase is released` of an architect's retry, each end of a hold the record shows going on with no release line (a relaunch of the held role's claim, or the issue's next move), and each `workflow: a phase is held` of an escalation, at its offset from the hold"},
	"RestartPhase":    {Way: FactInjected, From: "each recorded `workflow: a held phase is released` of the operator's retry, at its offset from the hold"},
	"ChildAdoption":   {Way: FactInjected, From: "each recorded `workflow: a queued root joined a live tree`, the release of a queued root into a tree, at its offset in the releasing architect's segment"},
	"GateRegistered":  {Way: FactInjected, From: "each recorded `workflow: design gate registered`, with what register_gate reads of the document rebuilt from the shadow, at its offset in the architect's segment"},
	// Death and start failure observations date claim evidence in the trace. They are not
	// ClaimFailed facts: a process can die or fail to start without exhausting the claim's budget.
	"ClaimFailed":       {Way: FactInjected, From: "each recorded `supervise: claim failed`, at its offset in the segment of the failed claim's issue"},
	"ProjectResumed":    {Way: FactInjected, From: "each recorded `pause: the project is resumed`, after the replay's own resume of its pause"},
	"PullRequestMerged": {Way: FactInjected, From: "a recorded move by pull_request_merged, at the end of its merging segment, or, where the close waited for the issues beneath it, the visit's first recorded `workflow: a merged issue's close waits for the issues beneath it`, at the end of its merging segment, which ends there; on the replay's own pull request record of the issue"},

	// The API's facts that no recorded window holds, each with the act that makes it.
	"PlanGiven": {Way: FactUnrecorded, From: "the API act register_plan, an owner's plan of one task " +
		"(cybermonklabs/legion#4611), which arrived after every window this tool has recorded, so no record holds a line of it"},
	"TaskStopped": {Way: FactUnrecorded, From: "the API act stop_task, an owner's stop of a task " +
		"(cybermonklabs/legion#4611), which arrived after every window this tool has recorded, so no record holds a line of it"},
	"SignalDeadline": {Way: FactUnrecorded, From: "the moment a signal that a rule admits by would cross its limit with no event " +
		"(intake.SignalDeadline, admission's own timer fact): no daemon release emits it and no recorded window holds a line of it, " +
		"so the replay injects none until it supplies the signals to the pass and emits the fact at each deadline of a signal rule"},

	// The daemon's own producers, made by the replay as the daemon makes them.
	"ControllerTick":           {Way: FactProduced, From: "the replay's ticks, a minute after each boot and every tick interval, observing from the shadow the keys blockers.WaitedOn selects"},
	"DaemonBooted":             {Way: FactProduced, From: "the replay's boot at each recorded process start, after its reconcile of the shadow's listing"},
	"DispatchConsumerPosition": {Way: FactProduced, From: "the release of a boot's hold on its listing at the recorded `admission: the Dispatch consumer has caught up; releasing held roots`"},
	"CatchUpApplied":           {Way: FactProduced, From: "the end of a boot's catch-up of a root, where the record shows it applied"},
	"LingerExpired":            {Way: FactProduced, From: "the stub's execution of a linger_close row at its next_at"},
	"Handover":                 {Way: FactProduced, From: "the stub's execution of a hand-over status write whose issue the shadow shows handed over already"},
	"GateAboveDispatch":        {Way: FactProduced, From: "the stub's execution of a gate_seed row that reads the document below the version it was queued at in the shadow"},
	"PullRequestMergeability":  {Way: FactProduced, From: "a conflict a stretch draws (spec R2): the replay's own pull request record read as conflicting at its head, the conflict confirmed"},
	"PullRequestSynchronized":  {Way: FactProduced, From: "the end of a drawn conflict's merge-forward segment: the head the merge forward left, on the replay's own pull request record"},

	// Left out, each under the assumption it rests on.
	"HandoffComplete":           {Way: FactLeftOut, Assumption: assumeCompletionPassed},
	"ClaimReady":                {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"TaskConfirmed":             {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"SessionLost":               {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"SessionStoreFault":         {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"WorkerSilent":              {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"WorkerBackgroundStalled":   {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"EntrySweep":                {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"MemoryGuardKill":           {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"ControllerMemoryGuardKill": {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"VolumeNearFull":            {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"ListenerWake":              {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"PodBackstopDue":            {Way: FactLeftOut, Assumption: assumeSupervisionSettles},
	"CutoverMergerRepair":       {Way: FactLeftOut, Assumption: assumeBootRepairsNothing},
	"GateRequestFound":          {Way: FactLeftOut, Assumption: assumeBootRepairsNothing},
	"CheckReport":               {Way: FactLeftOut, Assumption: assumeNoProductionCheck},
	"CheckLookEnded":            {Way: FactLeftOut, Assumption: assumeNoProductionCheck},
	"CheckWakeDue":              {Way: FactLeftOut, Assumption: assumeNoProductionCheck}, // Includes the WakeAt fence on postponed checks.
	"CheckBound":                {Way: FactLeftOut, Assumption: assumeNoProductionCheck},
	"PullRequestOpened":         {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"PullRequestReview":         {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"PullRequestChecks":         {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"PullRequestClosed":         {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"PullRequestDequeued":       {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"PullRequestMissing":        {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"Push":                      {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"BasePush":                  {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"MergeQueueReadDone":        {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"ConflictConfirmDone":       {Way: FactLeftOut, Assumption: assumeNoGitHub},

	// A review request GitHub opened or withdrew on a pull request, and the daemon's own fact that a
	// read of GitHub's merge state failed while the record holds a `blocked` answer: the replay reads
	// no GitHub, so neither comes to it.
	"PullRequestReviewRequests":   {Way: FactLeftOut, Assumption: assumeNoGitHub},
	"PullRequestMergeStateUnread": {Way: FactLeftOut, Assumption: assumeNoGitHub},
}
