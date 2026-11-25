package record

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

type unsupportedOutboxPayload struct{}

func (unsupportedOutboxPayload) OutboxKind() OutboxKind { return "unsupported" }

// mustNotice is the notice that carries rec, the record of a notice kind of the list.
func mustNotice(t *testing.T, rec any) Notice {
	t.Helper()
	notice, err := NoticeOf(rec)
	if err != nil {
		t.Fatalf("NoticeOf(%T): %v", rec, err)
	}
	return notice
}

// mustRoleMessage is the role message that tells the holder of role rec at urgency.
func mustRoleMessage(t *testing.T, role string, rec any, urgency texts.Urgency) RoleMessage {
	t.Helper()
	message, err := NewRoleMessage(role, rec, urgency)
	if err != nil {
		t.Fatalf("NewRoleMessage(%T): %v", rec, err)
	}
	return message
}

// samePayload says whether got is want. A role message's fields are compared as the record they
// hold, since the jsonb column keeps neither their spacing nor their key order.
func samePayload(t *testing.T, got, want OutboxPayload) bool {
	t.Helper()
	wantMessage, isMessage := want.(RoleMessage)
	gotMessage, gotIsMessage := got.(RoleMessage)
	if !isMessage || !gotIsMessage || wantMessage.Kind == "" {
		return reflect.DeepEqual(got, want)
	}
	wantRecord, err := wantMessage.Record()
	if err != nil {
		t.Fatalf("the record of the %s role message queued: %v", wantMessage.Kind, err)
	}
	gotRecord, err := gotMessage.Record()
	if err != nil {
		t.Fatalf("the record of the %s role message read back: %v", gotMessage.Kind, err)
	}
	wantMessage.Fields, gotMessage.Fields = nil, nil
	return reflect.DeepEqual(gotMessage, wantMessage) && reflect.DeepEqual(gotRecord, wantRecord)
}

// Every payload a row can carry is stored and read back as the payload queued: each notice and
// role message in the shape its kind's record declares, and the rows an older daemon queued
// beside them (a role message with its body alone, and an update ask with its text).
func TestOutboxPayloadsRoundTripThroughPostgres(t *testing.T) {
	now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	passed := 59
	head := func(kind texts.Kind) texts.NoticeHead { return texts.NoticeHead{Kind: kind, Issue: "LEGION-208"} }
	payloads := []OutboxPayload{
		OrdinaryStatusWrite("testing", "in_progress", ""),
		OrdinaryStatusWrite("done", "in_progress", "Closed by its architect: no change is needed."),
		// A workflow move that nothing else on Dispatch explains, here a worker's backward move,
		// carries its reason to post on the issue before the status (workflow's moveReason).
		OrdinaryStatusWrite("in_progress", "testing", "Legion moved this issue from testing back to implementing at the tester's request: the fix does not cover the empty list."),
		PersonHandoverStatusWrite("todo", "triage"),
		ReassertStatusWrite("in_progress", "done", 7),
		HandOverStatusWrite("todo", "in_progress", "LEGION-200", 3),
		MessagePost{Body: "Pull request checks are blocked."},
		// The row an older daemon's rescue message queued, which still decodes and round-trips.
		MessagePost{Body: "The full list is in this document: dispatch://LEGION-208/artifact/rescue-pod-9d3f0c7a",
			Artifact: &MessageArtifact{Name: "rescue-pod-9d3f0c7a", Content: "```\nbranch legion/LEGION-208-rescue\n```\n"}},
		mustNotice(t, texts.PhaseFinishedRecord{NoticeHead: head(texts.PhaseFinished), Role: claim.RoleTester, Phase: phase.Testing,
			To: phase.Reviewing, Summary: "Tests passed", Verdict: "pass", HandoffPhase: "testing", HandoffVersion: 4, HandoffCommit: "c0de5eed",
			Words: map[string]texts.WordsMeta{"summary": {By: "tester", At: now}}}),
		ControllerNotice(mustNotice(t, texts.HeldRecord{NoticeHead: head(texts.Held), Role: claim.RolePlanner, Phase: phase.Planning,
			Hold: texts.HoldEscalation, Reason: string(HoldEscalated), Escalation: "the runner cannot reach the registry",
			Words: map[string]texts.WordsMeta{"escalation": {By: "architect", At: now}}})),
		mustNotice(t, texts.WorkerDiedRecord{NoticeHead: head(texts.WorkerDied), Role: claim.RoleImplementer, Phase: phase.Implementing,
			Failure: &NoticeFailure{Cause: "died", Detail: "the implementer's process died while its task was outstanding",
				LaunchFailures: FailureBudget{Count: 1, Limit: 3}, Deaths: FailureBudget{Count: 3, Limit: 3},
				PromptFailures: FailureBudget{Limit: 3}, PromptRetires: FailureBudget{Limit: 2}}}),
		ControllerNotice(mustNotice(t, texts.TickRecord{NoticeHead: texts.NoticeHead{Kind: texts.Tick, Issue: "LEGION"},
			OpenCapabilities: []string{"secrets", "resource-limits"},
			Blocked:          []texts.BlockedIssue{{Issue: "LEGION-210", BlockedBy: []string{"LEGION-211"}}},
			Held:             []texts.HeldIssue{{Issue: "LEGION-212", Reason: string(HeldDesignGate), HeldBy: []string{"LEGION-208"}}},
			Silent:           []texts.SilentIssue{{Issue: "LEGION-213", Role: claim.RoleTester, LastStep: now.Add(-time.Hour), Silent: "1h0m0s"}}})),
		ControllerNotice(mustNotice(t, texts.UpdateStalledRecord{NoticeHead: head(texts.UpdateStalled), Holds: texts.HoldsRuntimeRefused,
			Refusal: "the runtime refused to replace the pod", Update: &UpdateStalled{Round: "pod:9d3f0c7a", Pod: "9d3f0c7a",
				Images: []string{"3f6a0b2c9d1e"}, Settings: []string{"LEGION_MODEL"},
				Waiting: []UpdateWaiting{{Claim: "legion-LEGION-208-implementer", Role: claim.RoleImplementer, State: "failed"}}}})),
		ControllerNotice(mustNotice(t, texts.ProviderErrorsRecord{NoticeHead: head(texts.ProviderErrors), Role: claim.RoleImplementer,
			Turns: 6, Model: "claude-opus-4-8", HTTPStatus: 529, RetryEvery: "10m0s"})),
		ControllerNotice(mustNotice(t, texts.TreeStalledRecord{NoticeHead: head(texts.TreeStalled), QuietFor: "7h0m0s",
			Members: []texts.TreeMember{{Key: "LEGION-208", Phase: phase.Reviewing, Turn: claim.RoleReviewer, PullRequest: "sjawhar/legion#42", PullRequestState: "open"}}})),
		ControllerNotice(mustNotice(t, texts.ArchitectStalledRecord{NoticeHead: head(texts.ArchitectStalled), Role: claim.RoleArchitect,
			Arm: string(ArchitectTookNothing), Phase: phase.Implementing, Entry: 1, QueuedAt: now.Add(-time.Hour), Interrupted: true,
			Since: now.Add(-30 * time.Minute), Bound: "30m0s"})),
		ControllerNotice(mustNotice(t, texts.MemoryGuardKillRecord{NoticeHead: texts.NoticeHead{Kind: texts.MemoryGuardKill, Issue: "LEGION"},
			Cause: texts.KillGuard, Process: "python3", PID: 4242, Size: "812.40 MiB", PodMemory: "3.13 GiB", Limit: "4.00 GiB"})),
		ControllerNotice(mustNotice(t, texts.ParkedDecisionRecord{NoticeHead: head(texts.ParkedDecision), Decision: string(DecisionAskAnswered),
			Root: "LEGION-1", Ref: "dispatch://LEGION-208/ask/a1", Actor: "user:alice", At: now, Answer: "Ship it.", Options: []string{"ship"},
			Words: map[string]texts.WordsMeta{"answer": {By: "user:alice", At: now}}})),
		mustNotice(t, texts.CatchUpRecord{NoticeHead: head(texts.CatchUp), CatchUp: &texts.CatchUpFacts{Generation: 3, Launch: 2,
			Gates:      []texts.CatchUpGate{{Step: string(DesignStepSpec), Gated: true, Artifact: "artifact-208", Version: 3, Approved: 3, Open: true}},
			Issues:     []texts.CatchUpIssue{{Key: "LEGION-209", Parent: "LEGION-208", Title: "A child", Phase: phase.Accepting, Status: "in_progress"}},
			Done:       []string{"LEGION-210"},
			Unreleased: []texts.UnreleasedChild{{Key: "LEGION-211", Parent: "LEGION-208", Status: "backlog"}},
			Accepting:  []string{"LEGION-209"},
			Kept: &texts.KeptFacts{Decisions: []texts.DecisionMark{{Ref: "dispatch://LEGION-208/ask/a1", At: now, Issue: "LEGION-208",
				Decision: string(DecisionAskAnswered), Actor: "user:alice", Options: []string{"ship"}, Answer: "Ship it."}}}},
			Relaunch: &texts.RelaunchFacts{Cause: "the pod was replaced", InTurn: true},
			Words:    map[string]texts.WordsMeta{"catch_up.kept.decisions[0].answer": {By: "user:alice", At: now}}}),
		mustNotice(t, texts.ReviewStuckRecord{NoticeHead: head(texts.ReviewStuck), Role: claim.RoleReviewer, Phase: phase.Reviewing,
			Summary: texts.StuckByMergeState, Topic: "notifications.role.legion-LEGION-208-reviewer", Cause: texts.CauseMergeState,
			PullRequest: "sjawhar/legion#42", Head: "c0de", EndsRound: texts.EndsRound("c0de"), MergeState: string(MergeStateBlocked),
			NotPassed: []texts.CheckConclusion{{Check: "unit", Conclusion: "failure"}}, Passed: &passed, ListPassed: texts.ListPassed("sjawhar/legion", 42)}),
		ControllerNotice(mustNotice(t, texts.ProjectPausedRecord{NoticeHead: texts.NoticeHead{Kind: texts.ProjectPaused, Issue: "LEGION"},
			Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator}, At: now, Reason: "the model provider is down"},
			Words: map[string]texts.WordsMeta{"pause.reason": {By: PauseByOperator, At: now}}})),
		ControllerNotice(mustNotice(t, texts.ProjectResumedRecord{NoticeHead: texts.NoticeHead{Kind: texts.ProjectResumed, Issue: "LEGION"},
			Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator}, At: now.Add(time.Hour)}})),
		ControllerNotice(mustNotice(t, GaveUpNotice("LEGION", 3, []NoticeGiveUp{
			{ID: 41, Consumer: "legion-go-LEGION-dispatch", Subject: "notifications.dispatch.issue.LEGION-208.issue.updated"},
			{ID: 42, Consumer: "legion-go-LEGION-wake", Subject: "notifications.legion.LEGION.wake"}}))),
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Task: "Write the spec."},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 3, Phase: phase.Reviewing, News: true},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 3, News: true},
		DesignNewsReconcile{},
		SuperviseRequest{Op: "pause_stop", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Pause: 7},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Task: "a wake", WakeKind: pods.WakeChecks},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, WakeKind: pods.WakePause, Pause: 7},
		GateSeed{ArtifactID: "artifact-208", Version: 4},
		LingerClose{Generation: 7},
		WorkspaceRemove{Linger: 3},
		GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{DesignStepCriteria, DesignStepSpec}},
		DesignGatesPublish{Issue: "LEGION-208", Gates: []DesignGateDocument{{Step: DesignStepCriteria, ArtifactID: "artifact-criteria", Holds: new(false)}, {Step: DesignStepSpec, ArtifactID: "artifact-208", Holds: new(true)}}},
		RoleHoldersPublish{Issue: "LEGION-208"},
		IssueBranch{Generation: 2},
		SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Reason: "LEGION-209 is done"},
		IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7},
		// A release's workspace check, and a left pod's close's, which names the roles it closes and
		// the newest turn start of the pass that queued it (IssueWorkspaceCheck.TurnStart).
		IssueWorkspaceCheck{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7, Role: claim.RoleImplementer},
		IssueWorkspaceCheck{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7, Role: claim.RolePlanner,
			CloseRoles: []claim.Role{claim.RolePlanner}, TurnStart: now.Add(-40 * time.Minute)},
		// The stop and the Sandbox suspension that close queues after a clean read carry the same
		// moment (SuperviseRequest.TurnStart, IssueSuspend.TurnStart), and the zero moment where no
		// claim had had a turn: a fence any turn passes, which is no fence at all only where the row
		// names none.
		SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RolePlanner, Generation: 3,
			Reason: "LEGION-209 left the workflow", TurnStart: new(now.Add(-40 * time.Minute))},
		SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RolePlanner, Generation: 3,
			Reason: "LEGION-209 left the workflow", TurnStart: new(time.Time{})},
		IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7, TurnStart: new(now.Add(-40 * time.Minute))},
		IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7, TurnStart: new(time.Time{})},
		SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Task: "Production check of LEGION-208", CheckLook: 2, CheckOpenedAt: now},
		CheckWakeDue{Look: 2, OpenedAt: now, Reason: "the tester's own wake"},
		CheckWakeDue{Look: 3, Reason: "a look nobody reported"},
		CheckLook{Look: 2, OpenedAt: now},
		CheckLook{Look: 1},
		CheckBound{},
		// A role message an older daemon queued, with its body alone.
		RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.implementer", Body: "Pull request #42: CI settled at c0de: unit failed."},
		mustRoleMessage(t, "legion.sjawhar-legion.LEGION-208.implementer", texts.CISettledRecord{
			RoleHead: texts.RoleHead{Kind: texts.CISettled, Issue: "LEGION-208"}, PullRequest: "sjawhar/legion#42", Head: "c0de", Phase: phase.Testing,
			NotPassed: []texts.CheckConclusion{{Check: "unit", Conclusion: "failure"}}, Passed: 59, ListPassed: texts.ListPassed("sjawhar/legion", 42)}, texts.Low),
		mustRoleMessage(t, "legion.sjawhar-legion.LEGION-208.architect", texts.GateMovedRecord{
			RoleHead: texts.RoleHead{Kind: texts.GateMoved, Issue: "LEGION-208"}, Gate: texts.GateOpened, Step: string(DesignStepSpec),
			Document: "dispatch://LEGION-208/artifact/spec@v4", Artifact: "artifact-208", Version: 4, Merge: texts.MergeReleased}, texts.Med),
		HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing,
			Count: 3, Counts: map[string]int{"testing-implementing": 1, "reviewing-implementing": 3},
			PullRequest: "sjawhar/legion#42", Prompt: "Read the reviewer's thread before you write anything."},
		HookPrompt{Hook: "slow-review", Role: claim.RoleReviewer, From: phase.Accepting, To: phase.Reviewing,
			Count: 2, Counts: map[string]int{"accepting-reviewing": 2}, Prompt: "Say what the round missed."},
		UpdateAsk{Role: claim.RoleTester, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Images: []string{"3f6a0b2c9d1e"},
			Settings: []string{"LEGION_MODEL", "LEGION_TOKEN"}, EndsAt: now.Add(time.Hour)},
		// The architect's process runs in the pod too, so its ask is a row like a worker's; this
		// round an older daemon opened records its drift as text alone.
		UpdateAsk{Role: claim.RoleArchitect, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Reasons: "image a, now b", EndsAt: now.Add(time.Hour)},
		// An update ask an older daemon queued, with the text its sweep built.
		UpdateAsk{Role: claim.RoleTester, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Text: "The daemon must replace the pod that runs this issue's roles."},
		MergeQueueRead{Number: 42, Head: "c0de"},
		MergeQueueRemoval{Number: 42, Head: "c0de"},
		MergeQueueRemoval{Number: 42, Head: "c0de", Unread: true},
		ConflictConfirm{Number: 42, Head: "c0de", FirstReadAt: now.Add(-time.Minute)},
		MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Implementing, PullRequest: 42, Why: ConflictCheckNotGreen},
		// In merging the merger is asked (cybermonklabs/legion#4979); the hold to the merger above is a
		// row a daemon before it queued, which still reads after the upgrade.
		MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Merging, PullRequest: 42, Why: ConflictMergeNext},
		MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardHold, Head: "c0de", Base: "main", Phase: phase.Merging, PullRequest: 42, Why: ConflictMergeNext},
		MergeForwardMessage{Role: claim.RoleTester, Step: MergeForwardLanded, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: ConflictRulesUnread},
		MergeForwardMessage{Role: claim.RoleReviewer, Step: MergeForwardReview, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Retro, PullRequest: 42},
		// A merge forward of a pull request recorded before the daemon kept the base: the head, no
		// base, as its message names them.
		MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Phase: phase.Reviewing, PullRequest: 42, Why: ConflictRulesUnread},
	}
	for _, payload := range payloads {
		t.Run(string(payload.OutboxKind()), func(t *testing.T) {
			ctx := context.Background()
			st := migratedStore(t)
			records := NewStore()
			row, err := NewOutboxRow("LEGION-208", payload, now)
			if err != nil {
				t.Fatalf("NewOutboxRow: %v", err)
			}
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.Enqueue(ctx, tx, row))
			})
			var claimed OutboxRow
			inTx(t, st, func(tx pgx.Tx) {
				rows, err := records.DueRows(ctx, tx, "LEGION", "", nil, now)
				must(t, err)
				if len(rows) != 1 {
					t.Fatalf("DueRows = %#v, want one row", rows)
				}
				var ok bool
				claimed, ok, err = records.ClaimRow(ctx, tx, rows[0].ID, now, time.Minute)
				must(t, err)
				if !ok {
					t.Fatalf("ClaimRow of the one due row claimed nothing")
				}
			})
			got, err := DecodeOutboxPayload(claimed)
			if err != nil {
				t.Fatalf("DecodeOutboxPayload: %v", err)
			}
			if !samePayload(t, got, payload) {
				gotJSON, _ := json.Marshal(got)
				wantJSON, _ := json.Marshal(payload)
				t.Fatalf("payload = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestExpediteOutboxReportsWhetherItMovedARow(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)
	row, err := NewOutboxRow("LEGION-208", MessagePost{Body: "A row to wake."}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("make the row: %v", err)
	}
	inTx(t, st, func(tx pgx.Tx) {
		id, err := records.EnqueueID(ctx, tx, row)
		must(t, err)
		row.ID = id
		moved, err := records.ExpediteOutbox(ctx, tx, row, now)
		must(t, err)
		if !moved {
			t.Fatalf("first expedite of a row due at %s = not moved, want moved", row.NextAt)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		moved, err := records.ExpediteOutbox(ctx, tx, row, now)
		must(t, err)
		if moved {
			t.Fatalf("second expedite of the row at %s = moved, want not moved", now)
		}
	})
}

// Every kind of status row has a constructor of its own, and no constructor leaves Kind empty:
// the kind is the one thing a row cannot be queued without (validateOutboxPayload refuses a write
// without one), so a kind nothing builds has no way into the outbox at all. The table is read
// against StatusRowKinds rather than written out, so a kind added to that list and given no
// constructor fails here.
func TestEveryStatusRowKindHasAConstructorThatSetsIt(t *testing.T) {
	built := map[StatusRowKind]StatusWrite{
		OrdinaryStatusRow:       OrdinaryStatusWrite("testing", "in_progress", ""),
		PersonHandoverStatusRow: PersonHandoverStatusWrite("todo", "triage"),
		ReassertStatusRow:       ReassertStatusWrite("in_progress", "done", 7),
		HandOverStatusRow:       HandOverStatusWrite("todo", "in_progress", "LEGION-200", 3),
		CloseStatusRow:          CloseStatusWrite("done", "in_progress", "merged", 7),
	}
	if len(built) != len(StatusRowKinds) {
		t.Fatalf("%d constructors for the %d kinds of StatusRowKinds: one of them builds a kind the list does not name",
			len(built), len(StatusRowKinds))
	}
	for _, kind := range StatusRowKinds {
		write, ok := built[kind]
		if !ok {
			t.Fatalf("status row kind %q has no constructor here: give the kind one, and this test its line", kind)
		}
		if write.Kind != kind {
			t.Fatalf("the constructor of %q built a write of kind %q, want it to set its own kind", kind, write.Kind)
		}
		if err := validateOutboxPayload("LEGION-208", write); err != nil {
			t.Fatalf("the write the constructor of %q built was refused: %v", kind, err)
		}
	}
}

// A person's handover is one dispatch_status row of kind PersonHandoverStatusRow: while it is
// pending, the issue has a person's handover pending, and no second row is queued for it. The
// workflow's other status rows suppress nothing, so a hand-over row (also a todo) on another
// issue, and an ordinary todo on the same issue, count for nothing; a finished row is deleted, so
// once the person's handover is gone nothing is pending.
func TestPersonHandoverPendingCountsOnlyMarkedStatusRows(t *testing.T) {
	now := time.Date(2026, 10, 14, 17, 0, 0, 0, time.UTC)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	enqueue := func(issue string, payload OutboxPayload) {
		t.Helper()
		row, err := NewOutboxRow(issue, payload, now)
		if err != nil {
			t.Fatalf("NewOutboxRow: %v", err)
		}
		inTx(t, st, func(tx pgx.Tx) {
			must(t, records.Enqueue(ctx, tx, row))
		})
	}
	pending := func(issue string) bool {
		t.Helper()
		var got bool
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			got, err = records.PersonHandoverPending(ctx, tx, issue)
			must(t, err)
		})
		return got
	}

	enqueue("LEGION-208", PersonHandoverStatusWrite("todo", "triage"))
	enqueue("LEGION-209", HandOverStatusWrite("todo", "backlog", "LEGION-200", 3))
	enqueue("LEGION-210", OrdinaryStatusWrite("todo", "triage", ""))
	enqueue("LEGION-210", ControllerNotice{Kind: TriageNotice})

	if !pending("LEGION-208") {
		t.Fatalf("PersonHandoverPending(LEGION-208) = false, want true while its marked row is in the outbox")
	}
	if pending("LEGION-209") {
		t.Fatalf("PersonHandoverPending(LEGION-209) = true, want false for an issue whose only pending row is a hand-over")
	}
	if pending("LEGION-210") {
		t.Fatalf("PersonHandoverPending(LEGION-210) = true, want false for an unmarked todo and a triage wake")
	}
	if pending("LEGION-211") {
		t.Fatalf("PersonHandoverPending(LEGION-211) = true, want false for an issue with no row")
	}

	inTx(t, st, func(tx pgx.Tx) {
		rows, err := records.DueRows(ctx, tx, "LEGION", "LEGION-208", nil, now)
		must(t, err)
		if len(rows) != 1 || rows[0].Kind != OutboxKindDispatchStatus {
			t.Fatalf("DueRows of LEGION-208 = %#v, want the one marked row", rows)
		}
		claimed, ok, err := records.ClaimRow(ctx, tx, rows[0].ID, now, time.Minute)
		must(t, err)
		if !ok {
			t.Fatalf("ClaimRow of the marked row claimed nothing")
		}
		must(t, records.FinishOutbox(ctx, tx, claimed.ID, claimed.LeaseToken))
	})
	if pending("LEGION-208") {
		t.Fatalf("PersonHandoverPending(LEGION-208) = true after its row finished, want false")
	}
}

// Every kind the outbox stores runs on exactly one lane, and LaneOf names the lane whose kinds
// list it: a kind on neither lane would be queued and never run, one on both would run twice over.
// The kinds and the lanes named here are this test's own, the oracle, and the kinds are read from
// the outbox's own lanes (OutboxLanes): a lane added there with its kinds and not named here fails
// both the count of lanes and the count of kinds.
func TestEveryOutboxKindIsInExactlyOneLane(t *testing.T) {
	every := []OutboxKind{OutboxKindDispatchStatus, OutboxKindDispatchMessage, OutboxKindNotice, OutboxKindControllerNotice, OutboxKindSupervise,
		OutboxKindGateSeed, OutboxKindLingerClose, OutboxKindIssueSuspend, OutboxKindWorkspaceRemove, OutboxKindIssueBranch,
		OutboxKindCheckWake, OutboxKindCheckLook, OutboxKindCheckBound, OutboxKindRoleMessage, OutboxKindHookPrompt, OutboxKindUpdateAsk,
		OutboxKindIssueBundle, OutboxKindIssueRelease, OutboxKindGatedStepsPublish, OutboxKindClaimRelease, OutboxKindDesignGatesPublish,
		OutboxKindMergeQueueRead, OutboxKindMergeQueueRemoval, OutboxKindDesignChanges, OutboxKindRoleHoldersPublish, OutboxKindStepRecord,
		OutboxKindConflictConfirm, OutboxKindMergeForward, OutboxKindPlanRecord}
	oracle := []OutboxLane{LaneBoard, LaneClaim, LaneDocuments, LaneLifecycle, LaneRecord, LaneRoleMessage}
	if got := OutboxLanes(); !reflect.DeepEqual(got, oracle) {
		t.Errorf("OutboxLanes() = %v, want the lanes %v in name order", got, oracle)
	}
	lanes := map[OutboxKind][]OutboxLane{}
	for _, lane := range OutboxLanes() {
		for _, kind := range LaneKinds(lane) {
			lanes[kind] = append(lanes[kind], lane)
		}
	}
	for _, kind := range every {
		if len(lanes[kind]) != 1 {
			t.Errorf("%s is on lanes %v, want exactly one", kind, lanes[kind])
			continue
		}
		if got := LaneOf(kind); got != lanes[kind][0] {
			t.Errorf("LaneOf(%s) = %s, want %s, the lane that lists it", kind, got, lanes[kind][0])
		}
	}
	if len(lanes) != len(every) {
		t.Errorf("the lanes list %d kinds, want the %d the outbox stores", len(lanes), len(every))
	}
	if lane := LaneOf("unsupported"); lane != "" {
		t.Errorf("LaneOf of a kind that is not one = %q, want none", lane)
	}
	if kinds := LaneKinds("fast"); kinds != nil {
		t.Errorf("LaneKinds of a lane that is not one = %v, want nil", kinds)
	}
}

// Every kind the lanes run is decided here as held by a project's pause or not (decision 9 of
// dispatch://INFRA-3759), and the decision is the one list the outbox's claim reads (pauseHolds):
// a kind added to a lane, a new lane's among them (OutboxLanes), fails this test until somebody
// writes it below, held or not. "every row" is a kind the pause holds whole, "op start" the
// supervise kind, whose start alone launches a role, and "" a kind that runs through the pause: a
// stop, a write to Dispatch, a controller's notice.
func TestEveryOutboxKindIsHeldOrNotHeldByAPause(t *testing.T) {
	decided := map[OutboxKind]string{
		OutboxKindSupervise:         "op start",
		OutboxKindNotice:            "every row",
		OutboxKindHookPrompt:        "every row",
		OutboxKindRoleMessage:       "every row",
		OutboxKindUpdateAsk:         "every row",
		OutboxKindMergeQueueRemoval: "every row",
		// A merge forward's message goes to one role of the issue and wakes a released one, as a
		// hook prompt does (main's dispatch://INFRA-4374).
		OutboxKindMergeForward:       "every row",
		OutboxKindCheckWake:          "every row",
		OutboxKindCheckLook:          "every row",
		OutboxKindCheckBound:         "every row",
		OutboxKindControllerNotice:   "",
		OutboxKindDispatchStatus:     "",
		OutboxKindDispatchMessage:    "",
		OutboxKindGateSeed:           "",
		OutboxKindLingerClose:        "",
		OutboxKindIssueSuspend:       "",
		OutboxKindWorkspaceRemove:    "",
		OutboxKindIssueBranch:        "",
		OutboxKindIssueBundle:        "",
		OutboxKindIssueRelease:       "",
		OutboxKindGatedStepsPublish:  "",
		OutboxKindClaimRelease:       "",
		OutboxKindDesignGatesPublish: "",
		OutboxKindMergeQueueRead:     "",
		// A conflict's second read reads the pull request from GitHub and starts no turn; the merge
		// forward its fact queues is held.
		OutboxKindConflictConfirm: "",
		// The role-holders publication writes Dispatch the sessions that hold an issue's roles and
		// starts no turn, and a pause's stops change those holders, which Dispatch should show.
		OutboxKindRoleHoldersPublish: "",
		// A step record writes Dispatch one start or one end of a step of an issue
		// (cybermonklabs/legion#4400) and starts no turn, so the pause lets it run as it does a
		// status write.
		OutboxKindStepRecord: "",
		// A plan record writes Dispatch the plan one run of an issue carries out (main's plan
		// ratings, contract 168) and starts no turn, so the pause lets it run as a step record.
		OutboxKindPlanRecord: "",
		// The reconcile of a tree's news of its approved design reads Dispatch and writes the news
		// record, and starts no turn: the news reaches a role only through a news-only start, a
		// supervise start the pause holds (op start above), so the pause lets the reconcile run,
		// and at the resume each held start carries what it recorded.
		OutboxKindDesignChanges: "",
	}
	for _, lane := range OutboxLanes() {
		for _, kind := range LaneKinds(lane) {
			if _, ok := decided[kind]; !ok {
				t.Errorf("%s runs on the %s lane and nobody decided whether a project's pause holds it: add it to pauseHolds or to this test as not held", kind, lane)
			}
		}
	}
	listed := map[OutboxKind]string{}
	for _, hold := range pauseHolds {
		if _, twice := listed[hold.kind]; twice {
			t.Errorf("pauseHolds names %s twice", hold.kind)
		}
		listed[hold.kind] = "every row"
		if hold.op != "" {
			listed[hold.kind] = "op " + string(hold.op)
		}
	}
	for kind, want := range decided {
		if got := listed[kind]; got != want {
			t.Errorf("a project's pause holds %s as %q, want %q", kind, got, want)
		}
	}
	for kind := range listed {
		if _, ok := decided[kind]; !ok {
			t.Errorf("pauseHolds names %s, which this test decides nothing of", kind)
		}
	}
}

// Each payload the validation refuses is refused before its row is queued, with the rule it
// breaks. Which notice kind goes in which payload is the list's: every kind the list marks a
// controller notice is refused in an architect's notice, and every kind of another form (a task,
// a role message) is refused in both notice payloads, as a notice or task kind is in a role
// message.
func TestNewOutboxRowRefusesInvalidPayloads(t *testing.T) {
	now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	ci := mustRoleMessage(t, "legion.sjawhar-legion.LEGION-208.tester", texts.CISettledRecord{
		RoleHead: texts.RoleHead{Kind: texts.CISettled, Issue: "LEGION-208"}, PullRequest: "sjawhar/legion#42", Head: "c0de",
		Phase: phase.Testing, Passed: 3, ListPassed: texts.ListPassed("sjawhar/legion", 42)}, texts.Low)
	roleMessage := func(change func(*RoleMessage)) RoleMessage {
		message := ci
		change(&message)
		return message
	}
	type refusal struct {
		name    string
		payload OutboxPayload
		reason  string
	}
	refusals := []refusal{
		{name: "a status write of no kind", payload: StatusWrite{Status: "testing", ObservedStatus: "in_progress"}, reason: `unknown kind ""`},
		{name: "a status write of a kind nobody gave a rule", payload: StatusWrite{Kind: "escalation", Status: "testing", ObservedStatus: "in_progress"}, reason: `unknown kind "escalation"`},
		{name: "empty status", payload: StatusWrite{Kind: OrdinaryStatusRow}, reason: "empty status"},
		{name: "a reason on a re-assertion", payload: StatusWrite{Kind: ReassertStatusRow, Status: "in_progress", ObservedStatus: "done", Reason: "set back"}, reason: "only a done write or the workflow's own move does"},
		{name: "a reason on a hand-over", payload: StatusWrite{Kind: HandOverStatusRow, Status: "todo", ObservedStatus: "in_progress", HandOverTree: "LEGION-200", HandOverLinger: 3, Reason: "handed over"}, reason: "only a done write or the workflow's own move does"},
		{name: "an architect-stalled notice naming no arm", payload: ControllerNotice{Kind: ArchitectStalledNotice, Role: claim.RoleArchitect}, reason: "names the arm that read it"},
		{name: "an architect-stalled notice naming an arm the wake has not", payload: ControllerNotice{Kind: ArchitectStalledNotice, Role: claim.RoleArchitect, Arm: "took-everything"}, reason: `names the arm "took-everything", which is no arm of the wake`},
		{name: "an actor on a phase-finished notice", payload: Notice{Kind: texts.PhaseFinished, Actor: "user:a"}, reason: "only a spec decision's does"},
		{name: "a ref on a child-status notice", payload: Notice{Kind: texts.ChildStatus, Ref: "dispatch://LEGION-1/spec"}, reason: "only a spec decision's does"},
		{name: "a decision on an architect's notice", payload: Notice{Kind: texts.DesignApproved, Decision: DecisionDesignApproved}, reason: "only a parked-decision controller notice does"},
		{name: "a root on an architect's notice", payload: Notice{Kind: texts.DesignApproved, Root: "LEGION-1"}, reason: "only a parked-decision controller notice does"},
		{name: "a catch-up notice with no account of the tree", payload: Notice{Kind: texts.CatchUp}, reason: "carries a catch-up only when it is one"},
		{name: "an account of the tree on a held notice", payload: Notice{Kind: texts.Held, CatchUp: &CatchUp{Generation: 3, Launch: 1}}, reason: "carries a catch-up only when it is one"},
		{name: "a parked decision with no root", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskAnswered, Ref: "dispatch://LEGION-1/ask/a", Actor: "user:a"}, reason: "a parked-decision notice needs"},
		{name: "a parked decision with no ref", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskAnswered, Root: "LEGION-1", Actor: "user:a"}, reason: "a parked-decision notice needs"},
		{name: "a parked decision by no person", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskAnswered, Root: "LEGION-1", Ref: "dispatch://LEGION-1/ask/a", Actor: "session:s"}, reason: "a parked-decision notice needs"},
		{name: "a parked decision by an empty user", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskAnswered, Root: "LEGION-1", Ref: "dispatch://LEGION-1/ask/a", Actor: "user:"}, reason: "a parked-decision notice needs"},
		{name: "an unknown decision", payload: ControllerNotice{Kind: ParkedDecisionNotice, Decision: "ask-ignored", Root: "LEGION-1", Ref: "dispatch://LEGION-1", Actor: "user:a"}, reason: "a parked-decision notice needs"},
		{name: "a parked decision with no decision", payload: ControllerNotice{Kind: ParkedDecisionNotice, Root: "LEGION-1", Ref: "dispatch://LEGION-1/ask/a", Actor: "user:a"}, reason: "a parked-decision notice needs"},
		{name: "a decision on a slot-free wake", payload: ControllerNotice{Kind: SlotFreeNotice, Decision: DecisionAskAnswered}, reason: "only a parked-decision does"},
		{name: "an actor on a tick", payload: ControllerNotice{Kind: TickNotice, Actor: "user:a"}, reason: "only a parked-decision does"},
		{name: "a root on a held notice to the controller", payload: ControllerNotice{Kind: texts.Held, Root: "LEGION-1"}, reason: "only a parked-decision does"},
		{name: "an architect's notice carrying an update round", payload: Notice{Kind: texts.Held, Update: &UpdateStalled{Round: "pod:9d3f0c7a"}}, reason: "only the controller's update-stalled notice does"},
		{name: "a failure record on a phase-finished notice", payload: Notice{Kind: texts.PhaseFinished, Failure: &NoticeFailure{Cause: "died"}}, reason: "only a held or a worker-died notice does"},
		{name: "a failure record on a tick", payload: ControllerNotice{Kind: TickNotice, Failure: &NoticeFailure{Cause: "died"}}, reason: "only a held or a worker-died notice does"},
		{name: "a failure record on a held notice to the controller naming no cause", payload: ControllerNotice{Kind: texts.Held, Failure: &NoticeFailure{Detail: "the pod did not start"}}, reason: "names the cause it failed under"},
		{name: "a failure record naming no cause", payload: Notice{Kind: texts.WorkerDied, Failure: &NoticeFailure{Detail: "the pod did not start"}}, reason: "names the cause it failed under"},
		{name: "an escalation's words on a worker-died notice", payload: Notice{Kind: texts.WorkerDied, Escalation: "a person should look at the runner"}, reason: "only a held notice of an escalation does"},
		{name: "an escalation's words on a held notice of no escalation", payload: Notice{Kind: texts.Held, Escalation: "a person should look at the runner"}, reason: "only a held notice of an escalation does"},
		{name: "an escalation's words on a held notice to the controller of no escalation", payload: ControllerNotice{Kind: texts.Held, Escalation: "a person should look at the runner"}, reason: "only a held notice of an escalation does"},
		{name: "a stopper on a held notice", payload: Notice{Kind: texts.Held, StoppedBy: "legion-legion-legion-208-architect"}, reason: "only a task-stopped notice does"},
		{name: "a stopper on a held notice to the controller", payload: ControllerNotice{Kind: texts.Held, StoppedBy: "legion-legion-legion-208-architect"}, reason: "only a task-stopped notice does"},
		{name: "a tick notice carrying an update round", payload: ControllerNotice{Kind: TickNotice, Update: &UpdateStalled{Round: "pod:9d3f0c7a"}}, reason: "carries an update round only when it is one"},
		{name: "an update-stalled notice carrying no update round", payload: ControllerNotice{Kind: UpdateStalledNotice}, reason: "carries an update round only when it is one"},
		{name: "an update-stalled notice with no claims", payload: ControllerNotice{Kind: UpdateStalledNotice, Update: &UpdateStalled{Round: "pod:9d3f0c7a"}}, reason: "requires its round and the claims it waits on"},
		{name: "an update-stalled notice with no round", payload: ControllerNotice{Kind: UpdateStalledNotice, Update: &UpdateStalled{Waiting: []UpdateWaiting{{Claim: "legion-LEGION-208-tester", State: "working"}}}}, reason: "requires its round and the claims it waits on"},
		{name: "a slot-free wake carrying held issues", payload: ControllerNotice{Kind: SlotFreeNotice, Held: []HeldIssue{{Issue: "LEGION-209", Reason: HeldDesignGate, HeldBy: []string{"LEGION-208"}}}}, reason: "carries held issues only when it is a tick"},
		{name: "a triage notice carrying held issues", payload: ControllerNotice{Kind: TriageNotice, Held: []HeldIssue{{Issue: "LEGION-209", Reason: HeldUnclassified}}}, reason: "carries held issues only when it is a tick"},
		{name: "an architect's notice carrying a project pause", payload: Notice{Kind: "held", Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator}, At: now, Reason: "down"}}, reason: "only the controller's project-paused and project-resumed notices do"},
		{name: "a project-paused notice carrying no pause", payload: ControllerNotice{Kind: ProjectPausedNotice}, reason: "carries a project pause only when it is"},
		{name: "a project-resumed notice carrying no pause", payload: ControllerNotice{Kind: ProjectResumedNotice}, reason: "carries a project pause only when it is"},
		{name: "a tick carrying a project pause", payload: ControllerNotice{Kind: TickNotice, Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator}, At: now}}, reason: "carries a project pause only when it is"},
		{name: "a project-paused notice of no pause", payload: ControllerNotice{Kind: ProjectPausedNotice, Pause: &ProjectPause{By: PauseActor{Kind: PauseByOperator}, At: now, Reason: "down"}}, reason: "names its pause and when it was acted on"},
		{name: "a project-paused notice of no moment", payload: ControllerNotice{Kind: ProjectPausedNotice, Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator}, Reason: "down"}}, reason: "names its pause and when it was acted on"},
		{name: "a project-paused notice with no reason", payload: ControllerNotice{Kind: ProjectPausedNotice, Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator}, At: now, Reason: " \n"}}, reason: "names the reason the project was paused"},
		{name: "a project-resumed notice with a reason", payload: ControllerNotice{Kind: ProjectResumedNotice, Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator}, At: now, Reason: "fixed"}}, reason: "carries no reason; only a project-paused notice does"},
		{name: "a project-paused notice with its reason beside its pause", payload: ControllerNotice{Kind: ProjectPausedNotice, Reason: "down", Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator}, At: now, Reason: "down"}}, reason: "carries its reason in its pause"},
		{name: "a pause by the controller with no session", payload: ControllerNotice{Kind: ProjectPausedNotice, Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByController}, At: now, Reason: "down"}}, reason: "names the controller's session"},
		{name: "a pause by the operator with a session", payload: ControllerNotice{Kind: ProjectResumedNotice, Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: PauseByOperator, Session: "session-1"}, At: now}}, reason: "names no session"},
		{name: "a pause by someone else", payload: ControllerNotice{Kind: ProjectPausedNotice, Pause: &ProjectPause{ID: 7, By: PauseActor{Kind: "reviewer"}, At: now, Reason: "down"}}, reason: "is by the controller or the operator"},
		{name: "a pause's stop of no pause", payload: SuperviseRequest{Op: "pause_stop", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3}, reason: "names its pause and the role it stops"},
		{name: "a pause's stop of no role", payload: SuperviseRequest{Op: "pause_stop", Tree: "LEGION-208", Generation: 3, Pause: 7}, reason: "names its pause and the role it stops"},
		{name: "a pause's stop of the controller", payload: SuperviseRequest{Op: "pause_stop", Role: claim.RoleController, Pause: 7}, reason: "names its pause and the role it stops"},
		{name: "a suspend naming a pause", payload: SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Pause: 7}, reason: "only a pause's stop and a start name one"},
		{name: "a start naming a negative pause", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Pause: -7}, reason: "only a pause's stop and a start name one"},
		{name: "a suspend naming a wake kind", payload: SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, WakeKind: pods.WakeChecks}, reason: "only a start names one"},
		{name: "a start fenced on a turn start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, TurnStart: new(now)}, reason: "only a left pod's close's suspend names one"},
		{name: "a release's stop fenced on a turn start", payload: SuperviseRequest{Op: "release", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, TurnStart: new(now)}, reason: "only a left pod's close's suspend names one"},
		{name: "a start naming no wake kind of the pods", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, WakeKind: "listener_ready"}, reason: "only a start names one"},
		{name: "a pause's launch naming no pause", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, WakeKind: pods.WakePause}, reason: "names the pause that owed it"},
		{name: "an intake-gave-up notice counting none", payload: ControllerNotice{Kind: IntakeGaveUpNotice, GiveUps: noticeGiveUps(1)}, reason: "counts one give-up or more"},
		{name: "an intake-gave-up notice naming none", payload: ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1}, reason: "names between 1 and 1 of them, not 0"},
		{name: "an intake-gave-up notice naming 101", payload: ControllerNotice{Kind: IntakeGaveUpNotice, Count: 150, GiveUps: noticeGiveUps(101)}, reason: "names between 1 and 100 of them, not 101"},
		{name: "an intake-gave-up notice naming more than it counts", payload: ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: noticeGiveUps(2)}, reason: "names between 1 and 1 of them, not 2"},
		{name: "an intake-gave-up notice naming a give-up with no id", payload: ControllerNotice{Kind: IntakeGaveUpNotice, Count: 1, GiveUps: []NoticeGiveUp{{Consumer: "legion-go-LEGION-dispatch"}}}, reason: "by its positive record id"},
		{name: "an intake-gave-up notice to a role", payload: ControllerNotice{Kind: IntakeGaveUpNotice, Role: claim.RoleArchitect, Count: 1, GiveUps: noticeGiveUps(1)}, reason: "carries no role"},
		{name: "give-ups named on a tick", payload: ControllerNotice{Kind: TickNotice, GiveUps: noticeGiveUps(1)}, reason: "only an intake-gave-up notice does"},
		{name: "a count on a tick", payload: ControllerNotice{Kind: TickNotice, Count: 1}, reason: "only an intake-gave-up notice does"},
		{name: "a count on an architect's notice", payload: Notice{Kind: "phase-finished", Count: 1}, reason: "only an intake-gave-up notice does"},
		{name: "unknown supervise operation", payload: SuperviseRequest{Op: "unknown"}, reason: "unknown supervise operation"},
		{name: "a stop that is not a tree close", payload: SuperviseRequest{Op: "stop", Tree: "LEGION-208", Role: claim.RoleArchitect}, reason: "unknown supervise operation"},
		{name: "an issue close naming a linger", payload: SuperviseRequest{Op: "issue_close", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 3, Linger: 7}, reason: "only a tree close"},
		{name: "start without tree", payload: SuperviseRequest{Op: "start", Role: claim.RoleArchitect}, reason: "start requires tree"},
		{name: "start without role", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208"}, reason: "start requires role"},
		{name: "a role message without a role", payload: RoleMessage{Body: "Pull request #42 settled."}, reason: "role message requires role"},
		{name: "a role message without a body", payload: RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.architect"}, reason: "role message requires body"},
		{name: "a role message of no kind with an urgency", payload: RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.architect", Body: "CI settled.", Urgency: texts.Low}, reason: "carries its body alone"},
		{name: "a role message of no kind with fields", payload: RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.architect", Body: "CI settled.", Fields: ci.Fields}, reason: "carries its body alone"},
		{name: "a role message of a kind with a body", payload: roleMessage(func(m *RoleMessage) { m.Body = "CI settled." }), reason: "carries its record's fields and no body"},
		{name: "a role message of a kind with no urgency", payload: roleMessage(func(m *RoleMessage) { m.Urgency = "" }), reason: "carries the urgency its builder decided"},
		{name: "a role message of a kind with an urgency off the list", payload: roleMessage(func(m *RoleMessage) { m.Urgency = "high" }), reason: "carries the urgency its builder decided"},
		{name: "a role message of a kind with no fields", payload: roleMessage(func(m *RoleMessage) { m.Fields = nil }), reason: "carries no fields"},
		{name: "a role message whose fields hold a key its record does not declare", payload: roleMessage(func(m *RoleMessage) {
			m.Fields = json.RawMessage(`{"kind":"ci-settled","issue":"LEGION-208","pull_request":"sjawhar/legion#42","head":"c0de","phase":"testing","passed":3,"list_passed":"gh pr checks 42 --repo sjawhar/legion","summary":"CI settled."}`)
		}), reason: `unknown field "summary"`},
		{name: "a role message whose fields name another kind", payload: roleMessage(func(m *RoleMessage) {
			m.Fields = json.RawMessage(`{"kind":"gate-moved","issue":"LEGION-208","pull_request":"sjawhar/legion#42","head":"c0de","phase":"testing","passed":3,"list_passed":"gh pr checks 42 --repo sjawhar/legion"}`)
		}), reason: "do not name that kind"},
		{name: "a gated-steps publish without a project", payload: GatedStepsPublish{Steps: []DesignStep{DesignStepSpec}}, reason: "requires the project whose row it writes"},
		{name: "a gated-steps publish naming no step of the gate", payload: GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{"verification"}}, reason: `names "verification", which is no design-gate step`},
		{name: "a check look on a suspend", payload: SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, CheckLook: 1}, reason: checkLookRefusal},
		{name: "a check look on a phase start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, Phase: phase.Implementing, CheckLook: 1}, reason: checkLookRefusal},
		{name: "a check look on an architect's start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 3, CheckLook: 1}, reason: checkLookRefusal},
		{name: "a check look on a reviewer's start", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 3, CheckLook: 1}, reason: checkLookRefusal},
		{name: "a negative check look", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 3, CheckLook: -1}, reason: checkLookRefusal},
		{name: "a news-only start with a task", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 3, Phase: phase.Reviewing, Task: "Review it.", News: true}, reason: newsOnlyRefusal},
		{name: "a news-only start of an entry", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 3, Phase: phase.Reviewing, Entry: 2, News: true}, reason: newsOnlyRefusal},
		{name: "a news-only start of a worker naming no phase", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 3, News: true}, reason: newsOnlyRefusal},
		{name: "a news-only start of an architect naming a phase", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 3, Phase: phase.Accepting, News: true}, reason: newsOnlyRefusal},
		{name: "a news-only suspend", payload: SuperviseRequest{Op: "suspend", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 3, Phase: phase.Reviewing, News: true}, reason: newsOnlyRefusal},
		{name: "a news-only boot resume", payload: SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 3, Phase: phase.Reviewing, RuleResume: true, News: true}, reason: newsOnlyRefusal},
		{name: "a check wake without a look", payload: CheckWakeDue{Reason: "the tester's own wake"}, reason: "check wake requires the look it was named at"},
		{name: "a check wake without a reason", payload: CheckWakeDue{Look: 1}, reason: "check wake requires the reason it wakes the check"},
		{name: "a hook prompt with no hook", payload: HookPrompt{Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 1, Prompt: "Read the thread."}, reason: "carries the name of the hook that fired it"},
		{name: "a hook prompt to an architect", payload: HookPrompt{Hook: "back-to-implementing", Role: claim.RoleArchitect, From: phase.Reviewing, To: phase.Implementing, Count: 1, Prompt: "Read the thread."}, reason: "never to an architect"},
		{name: "a hook prompt to no role", payload: HookPrompt{Hook: "back-to-implementing", From: phase.Reviewing, To: phase.Implementing, Count: 1, Prompt: "Read the thread."}, reason: "never to an architect"},
		{name: "a hook prompt to the controller", payload: HookPrompt{Hook: "back-to-implementing", Role: claim.RoleController, From: phase.Reviewing, To: phase.Implementing, Count: 1, Prompt: "Read the thread."}, reason: "never to an architect"},
		{name: "a hook prompt of a phase to itself", payload: HookPrompt{Hook: "back-to-implementing", Role: claim.RoleReviewer, From: phase.Reviewing, To: phase.Reviewing, Count: 1, Prompt: "Read the thread."}, reason: `a hook prompt names the move back that fired it; "reviewing" to "reviewing" is no move back`},
		{name: "a hook prompt on a phase outside the forward line", payload: HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Held, To: phase.Implementing, Count: 1, Prompt: "Read the thread."}, reason: "is no move back"},
		{name: "a hook prompt that counted nothing", payload: HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Prompt: "Read the thread."}, reason: "its count is one or more"},
		{name: "a hook prompt counting backward", payload: HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: -1, Prompt: "Read the thread."}, reason: "its count is one or more"},
		{name: "a hook prompt with no words", payload: HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 1}, reason: "carries the hook's own words"},
		{name: "an update ask to no role", payload: UpdateAsk{Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Text: "Push your work."}, reason: `"" is no role`},
		{name: "an update ask to the controller", payload: UpdateAsk{Role: claim.RoleController, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Text: "Push your work."}, reason: `"controller" is no role`},
		{name: "an update ask with no round", payload: UpdateAsk{Role: claim.RoleImplementer, Pod: "9d3f0c7a", Text: "Push your work."}, reason: "names the round it asks for"},
		{name: "an update ask with neither its round's end nor a text", payload: UpdateAsk{Role: claim.RoleImplementer, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Images: []string{"3f6a0b2c9d1e"}}, reason: "names the moment its round ends"},
		{name: "an update ask with its round's end and a text", payload: UpdateAsk{Role: claim.RoleImplementer, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", EndsAt: now, Text: "Push your work."}, reason: "never both"},
		{name: "an update ask with its round's drift and a text", payload: UpdateAsk{Role: claim.RoleImplementer, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Settings: []string{"LEGION_MODEL"}, Text: "Push your work."}, reason: "never both"},
		{name: "a merge-queue read of no pull request", payload: MergeQueueRead{Head: "c0de"}, reason: "a merge-queue read names the pull request it reads and the head"},
		{name: "a merge-queue read of a negative pull request", payload: MergeQueueRead{Number: -42, Head: "c0de"}, reason: "a merge-queue read names the pull request it reads and the head"},
		{name: "a merge-queue read with no head", payload: MergeQueueRead{Number: 42}, reason: "a merge-queue read names the pull request it reads and the head"},
		{name: "a merge-queue removal of no pull request", payload: MergeQueueRemoval{Head: "c0de"}, reason: "a merge-queue removal names the pull request that left the queue and the head"},
		{name: "a merge-queue removal with no head", payload: MergeQueueRemoval{Number: 42, Unread: true}, reason: "a merge-queue removal names the pull request that left the queue and the head"},
		{name: "a conflict confirm of no pull request", payload: ConflictConfirm{Head: "c0de", FirstReadAt: now}, reason: "a conflict confirm names the pull request it reads again and the head"},
		{name: "a conflict confirm with no head", payload: ConflictConfirm{Number: 42, FirstReadAt: now}, reason: "a conflict confirm names the pull request it reads again and the head"},
		{name: "a conflict confirm with no time of the first read", payload: ConflictConfirm{Number: 42, Head: "c0de"}, reason: "a conflict confirm names when the first read it reads again was made"},
		{name: "a merge-forward message of no step", payload: MergeForwardMessage{Role: claim.RoleImplementer, Head: "c0de", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: ConflictCheckNotGreen}, reason: "is an ask, a hold, a landed or a review"},
		{name: "a merge-forward ask to the tester", payload: MergeForwardMessage{Role: claim.RoleTester, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: ConflictCheckNotGreen}, reason: "a merge-forward ask goes to the implementer"},
		{name: "a merge-forward ask to the merger in accepting", payload: MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Accepting, PullRequest: 42, Why: ConflictMergeNext}, reason: "a merge-forward ask goes to the merger only while the issue stands in merging"},
		{name: "a merge-forward ask to the merger in retro", payload: MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Retro, PullRequest: 42, Why: ConflictMergeNext}, reason: "a merge-forward ask goes to the merger only while the issue stands in merging"},
		{name: "a merge-forward hold to the implementer", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardHold, Head: "c0de", Base: "main", Phase: phase.Retro, PullRequest: 42, Why: ConflictMergeNext}, reason: "a merge-forward ask goes to the implementer"},
		{name: "a merge-forward hold to the architect", payload: MergeForwardMessage{Role: claim.RoleArchitect, Step: MergeForwardHold, Head: "c0de", Base: "main", Phase: phase.Accepting, PullRequest: 42, Why: ConflictMergeNext}, reason: "a merge-forward ask goes to the implementer"},
		{name: "a merge-forward landed to the architect", payload: MergeForwardMessage{Role: claim.RoleArchitect, Step: MergeForwardLanded, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Accepting, PullRequest: 42}, reason: "a merge-forward ask goes to the implementer"},
		{name: "a merge-forward review to the merger", payload: MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardReview, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Merging, PullRequest: 42}, reason: "a merge-forward ask goes to the implementer"},
		{name: "a merge-forward ask with no head", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Base: "main", Phase: phase.Testing, PullRequest: 42, Why: ConflictCheckNotGreen}, reason: "names the head, the phase the issue stays in and the pull request"},
		{name: "a merge-forward ask in no phase", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Base: "main", PullRequest: 42, Why: ConflictCheckNotGreen}, reason: "names the head, the phase the issue stays in and the pull request"},
		{name: "a merge-forward ask of no pull request", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Testing, Why: ConflictCheckNotGreen}, reason: "names the head, the phase the issue stays in and the pull request"},
		{name: "a merge-forward ask with no why", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Testing, PullRequest: 42}, reason: "a merge-forward ask or hold names why the daemon acted"},
		{name: "a merge-forward hold with a why that is no reason of R0", payload: MergeForwardMessage{Role: claim.RoleTester, Step: MergeForwardHold, Head: "c0de", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: "dirty"}, reason: "a merge-forward ask or hold names why the daemon acted"},
		{name: "a merge-forward review with a why that is no reason of R0", payload: MergeForwardMessage{Role: claim.RoleReviewer, Step: MergeForwardReview, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Accepting, PullRequest: 42, Why: "dirty"}, reason: "a landed or a review names one of them or none"},
		{name: "a merge-forward ask naming a head that landed", payload: MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: ConflictCheckNotGreen}, reason: "a merge-forward landed or review names the head that landed"},
		{name: "a merge-forward landed with no head that landed", payload: MergeForwardMessage{Role: claim.RoleTester, Step: MergeForwardLanded, Head: "c0de", Base: "main", Phase: phase.Testing, PullRequest: 42}, reason: "a merge-forward landed or review names the head that landed"},
		{name: "a merge-forward review with no head that landed", payload: MergeForwardMessage{Role: claim.RoleReviewer, Step: MergeForwardReview, Head: "c0de", Base: "main", Phase: phase.Accepting, PullRequest: 42}, reason: "a merge-forward landed or review names the head that landed"},
		{name: "a check look bound without a look", payload: CheckLook{}, reason: "check look bound requires the look it bounds"},
		{name: "unsupported payload", payload: unsupportedOutboxPayload{}, reason: "unknown outbox payload"},
	}
	for _, entry := range texts.Entries() {
		switch entry.Form {
		case texts.ControllerNotice:
			refusals = append(refusals, refusal{name: "a " + string(entry.Kind) + " notice for an architect",
				payload: Notice{Kind: entry.Kind}, reason: "is the controller's alone"})
		case texts.Task, texts.RoleMessage:
			refusals = append(refusals,
				refusal{name: "a notice of the " + string(entry.Form) + " kind " + string(entry.Kind),
					payload: Notice{Kind: entry.Kind}, reason: "no notice kind on the list"},
				refusal{name: "a controller notice of the " + string(entry.Form) + " kind " + string(entry.Kind),
					payload: ControllerNotice{Kind: entry.Kind}, reason: "no notice kind on the list"})
		}
		if entry.Form != texts.RoleMessage {
			refusals = append(refusals, refusal{name: "a role message of the " + string(entry.Form) + " kind " + string(entry.Kind),
				payload: roleMessage(func(m *RoleMessage) { m.Kind = entry.Kind }), reason: "no role-message kind on the list"})
		}
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOutboxRow("LEGION-208", tc.payload, now)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("NewOutboxRow error = %v, want %q", err, tc.reason)
			}
		})
	}
}

// A pause's stop of a claim of another project's issue is keyed by the project, which the outbox
// of the project's own daemon serves, and names the claim's issue in its payload: it round-trips
// through Postgres and that daemon lists it. A row may carry that issue only so: on a pause's stop
// keyed by its project, never beside a row of an issue key, never naming something that is no
// issue key, and never on another op; and a pause's stop keyed by its project must carry it.
func TestAPauseStopKeyedByItsProjectNamesItsClaimsIssue(t *testing.T) {
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	stop := SuperviseRequest{Op: "pause_stop", Tree: "OTHER-5", Role: claim.RoleArchitect, Generation: 1, Pause: 7, Issue: "OTHER-5"}
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	row, err := NewOutboxRow("LEGION", stop, now)
	if err != nil {
		t.Fatalf("NewOutboxRow of the stop keyed by LEGION: %v", err)
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.Enqueue(ctx, tx, row))
	})
	var due []OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		due, err = records.DueRows(ctx, tx, "LEGION", "", nil, now)
		must(t, err)
	})
	if len(due) != 1 || due[0].Issue != "LEGION" {
		t.Fatalf("LEGION's due rows = %+v, want the stop keyed by LEGION", due)
	}
	got, err := DecodeOutboxPayload(due[0])
	if err != nil || !reflect.DeepEqual(got, stop) {
		t.Fatalf("the stop decodes as %#v, %v; want %#v", got, err, stop)
	}

	for _, tc := range []struct {
		name    string
		issue   string
		payload SuperviseRequest
	}{
		{"beside a row of an issue key", "LEGION-208", stop},
		{"naming no issue key", "LEGION", SuperviseRequest{Op: "pause_stop", Role: claim.RoleArchitect, Pause: 7, Issue: "other-5"}},
		{"on a start", "LEGION", SuperviseRequest{Op: "start", Tree: "OTHER-5", Role: claim.RoleArchitect, Issue: "OTHER-5"}},
		{"missing from a stop keyed by the project", "LEGION", SuperviseRequest{Op: "pause_stop", Tree: "OTHER-5", Role: claim.RoleArchitect, Pause: 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewOutboxRow(tc.issue, tc.payload, now); err == nil || !strings.Contains(err.Error(), "issue") {
				t.Fatalf("NewOutboxRow(%q, %+v) = %v, want it refused for the issue it names", tc.issue, tc.payload, err)
			}
			encoded, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			stored := OutboxRow{Kind: OutboxKindSupervise, Issue: tc.issue, Payload: encoded}
			if _, err := DecodeOutboxPayload(stored); err == nil {
				t.Fatalf("the decode of a stored %+v keyed by %q passed, want it refused", tc.payload, tc.issue)
			}
		})
	}
}

// A message's artifact is refused before its row is queued wherever Dispatch would refuse the
// upload on every run, or would file it under a slug other than its name, which is the slug the
// message's link names: a name that is no slug, content that is empty or blank, and content past
// Dispatch's bound on one document. The decode of a stored row holds it to the same rules.
func TestAMessageArtifactIsRefusedUnlessDispatchWouldFileItUnderItsName(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	post := func(name, content string) MessagePost {
		return MessagePost{Body: "dispatch://LEGION-208/artifact/" + name, Artifact: &MessageArtifact{Name: name, Content: content}}
	}
	for _, tc := range []struct {
		name    string
		payload MessagePost
		reason  string
	}{
		{name: "no name", payload: post("", "the list"), reason: "which is no Dispatch slug"},
		{name: "an uppercase letter", payload: post("rescue-Pod-9d3f0c7a", "the list"), reason: "which is no Dispatch slug"},
		{name: "a colon", payload: post("rescue-pod:9d3f0c7a", "the list"), reason: "which is no Dispatch slug"},
		{name: "an underscore", payload: post("rescue_pod", "the list"), reason: "which is no Dispatch slug"},
		{name: "two dashes in a row", payload: post("rescue--pod", "the list"), reason: "which is no Dispatch slug"},
		{name: "a dash at the start", payload: post("-rescue-pod", "the list"), reason: "which is no Dispatch slug"},
		{name: "a dash at the end", payload: post("rescue-pod-", "the list"), reason: "which is no Dispatch slug"},
		{name: "a letter outside ASCII", payload: post("rescue-pöd", "the list"), reason: "which is no Dispatch slug"},
		{name: "empty content", payload: post("rescue-pod", ""), reason: "holds no content"},
		{name: "blank content", payload: post("rescue-pod", " \n\t\n"), reason: "holds no content"},
		{name: "content past one document", payload: post("rescue-pod", strings.Repeat("x", MaxMessageArtifactBytes+1)), reason: "more than the 1048576 of one Dispatch document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewOutboxRow("LEGION-208", tc.payload, now); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("NewOutboxRow error = %v, want %q", err, tc.reason)
			}
			encoded, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			row := OutboxRow{ID: 7, Kind: OutboxKindDispatchMessage, Issue: "LEGION-208", Payload: encoded}
			if _, err := DecodeOutboxPayload(row); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("DecodeOutboxPayload error = %v, want %q", err, tc.reason)
			}
		})
	}
	for _, payload := range []MessagePost{
		post("rescue-pod-9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22", "the list"),
		post("r", strings.Repeat("x", MaxMessageArtifactBytes)),
		{Body: "a message that links no document"},
	} {
		if _, err := NewOutboxRow("LEGION-208", payload, now); err != nil {
			t.Errorf("NewOutboxRow of a message whose artifact Dispatch takes as named = %v, want it queued", err)
		}
	}
}

func TestDecodeOutboxPayloadRefusesInvalidRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		row    OutboxRow
		reason string
	}{
		{name: "unknown kind", row: OutboxRow{ID: 31, Kind: "unknown", Payload: json.RawMessage(`{}`)}, reason: "unknown kind"},
		{name: "invalid JSON", row: OutboxRow{ID: 32, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{`)}, reason: "unexpected EOF"},
		{name: "unknown field", row: OutboxRow{ID: 33, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{"status":"testing","observedStatus":"todo","surprise":true}`)}, reason: "unknown field"},
		{name: "kind payload mismatch", row: OutboxRow{ID: 34, Kind: OutboxKindDispatchMessage, Payload: json.RawMessage(`{}`)}, reason: "no body"},
		{name: "empty status", row: OutboxRow{ID: 35, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{"kind":"ordinary","observedStatus":"todo"}`)}, reason: "empty status"},
		{name: "a stored status row of no kind", row: OutboxRow{ID: 51, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{"status":"testing","observedStatus":"in_progress"}`)}, reason: `unknown kind ""`},
		{name: "a stored status row of a kind nobody gave a rule", row: OutboxRow{ID: 52, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{"kind":"escalation","status":"testing","observedStatus":"in_progress"}`)}, reason: `unknown kind "escalation"`},
		{name: "a stored status row still carrying an old mark", row: OutboxRow{ID: 53, Kind: OutboxKindDispatchStatus, Payload: json.RawMessage(`{"kind":"person-handover","status":"todo","observedStatus":"triage","personHandover":true}`)}, reason: "unknown field"},
		{name: "a notice of a kind off the list", row: OutboxRow{ID: 36, Kind: OutboxKindNotice, Payload: json.RawMessage(`{"kind":"unknown"}`)}, reason: "no notice kind on the list"},
		{name: "a notice with a key no record declares", row: OutboxRow{ID: 44, Kind: OutboxKindNotice, Payload: json.RawMessage(`{"kind":"phase-finished","role":"tester","phase":"testing","urgency":"low"}`)}, reason: "unknown field"},
		{name: "a role message of a kind off the list", row: OutboxRow{ID: 45, Kind: OutboxKindRoleMessage, Payload: json.RawMessage(`{"role":"legion.sjawhar-legion.LEGION-208.tester","kind":"ci-passed","urgency":"low","fields":{"kind":"ci-passed","issue":"LEGION-208"}}`)}, reason: "no role-message kind on the list"},
		{name: "unknown supervise operation", row: OutboxRow{ID: 37, Kind: OutboxKindSupervise, Payload: json.RawMessage(`{"op":"unknown","tree":"LEGION-208","role":"architect"}`)}, reason: "unknown supervise operation"},
		{name: "start without tree", row: OutboxRow{ID: 38, Kind: OutboxKindSupervise, Payload: json.RawMessage(`{"op":"start","role":"architect"}`)}, reason: "start requires tree"},
		{name: "start without role", row: OutboxRow{ID: 39, Kind: OutboxKindSupervise, Payload: json.RawMessage(`{"op":"start","tree":"LEGION-208"}`)}, reason: "start requires role"},
		{name: "a check wake without a look", row: OutboxRow{ID: 40, Kind: OutboxKindCheckWake, Payload: json.RawMessage(`{"reason":"the tester's own wake"}`)}, reason: "check wake requires the look it was named at"},
		{name: "a check wake without a reason", row: OutboxRow{ID: 42, Kind: OutboxKindCheckWake, Payload: json.RawMessage(`{"look":1}`)}, reason: "check wake requires the reason it wakes the check"},
		{name: "a check wake with an unknown field", row: OutboxRow{ID: 43, Kind: OutboxKindCheckWake, Payload: json.RawMessage(`{"look":1,"reason":"the tester's own wake","build":"b00f"}`)}, reason: "unknown field"},
		{name: "a check bound with a field", row: OutboxRow{ID: 41, Kind: OutboxKindCheckBound, Payload: json.RawMessage(`{"look":1}`)}, reason: "unknown field"},
		{name: "a role message with no body", row: OutboxRow{ID: 48, Kind: OutboxKindRoleMessage, Payload: json.RawMessage(`{"role":"legion.sjawhar-legion.LEGION-208.architect"}`)}, reason: "role message requires body"},
		{name: "a hook prompt to an architect", row: OutboxRow{ID: 55, Kind: OutboxKindHookPrompt, Payload: json.RawMessage(`{"hook":"back-to-implementing","role":"architect","from":"reviewing","to":"implementing","count":1,"counts":{"reviewing-implementing":1},"prompt":"Read the thread."}`)}, reason: "never to an architect"},
		{name: "a hook prompt of a phase to itself", row: OutboxRow{ID: 56, Kind: OutboxKindHookPrompt, Payload: json.RawMessage(`{"hook":"back-to-implementing","role":"reviewer","from":"reviewing","to":"reviewing","count":1,"counts":{},"prompt":"Read the thread."}`)}, reason: `a hook prompt names the move back that fired it; "reviewing" to "reviewing" is no move back`},
		{name: "a hook prompt with an unknown field", row: OutboxRow{ID: 57, Kind: OutboxKindHookPrompt, Payload: json.RawMessage(`{"hook":"back-to-implementing","role":"implementer","from":"reviewing","to":"implementing","count":1,"counts":{"reviewing-implementing":1},"prompt":"Read the thread.","cap":2}`)}, reason: "unknown field"},
		{name: "an update ask with no round", row: OutboxRow{ID: 58, Kind: OutboxKindUpdateAsk, Payload: json.RawMessage(`{"role":"tester","pod":"9d3f0c7a","text":"Push your work."}`)}, reason: "names the round it asks for"},
		{name: "an update ask with an unknown field", row: OutboxRow{ID: 59, Kind: OutboxKindUpdateAsk, Payload: json.RawMessage(`{"role":"tester","round":"pod:9d3f0c7a","pod":"9d3f0c7a","text":"Push your work.","drift":"image a, now b"}`)}, reason: "unknown field"},
		{name: "a merge-queue read with no head", row: OutboxRow{ID: 58, Kind: OutboxKindMergeQueueRead, Payload: json.RawMessage(`{"number":42}`)}, reason: "a merge-queue read names the pull request it reads and the head"},
		{name: "a merge-queue read of no pull request", row: OutboxRow{ID: 59, Kind: OutboxKindMergeQueueRead, Payload: json.RawMessage(`{"number":0,"head":"c0de"}`)}, reason: "a merge-queue read names the pull request it reads and the head"},
		{name: "a merge-queue removal of no pull request", row: OutboxRow{ID: 60, Kind: OutboxKindMergeQueueRemoval, Payload: json.RawMessage(`{"head":"c0de"}`)}, reason: "a merge-queue removal names the pull request that left the queue and the head"},
		{name: "a merge-queue removal naming a role", row: OutboxRow{ID: 61, Kind: OutboxKindMergeQueueRemoval, Payload: json.RawMessage(`{"role":"merger","number":42,"head":"c0de"}`)}, reason: "unknown field"},
		{name: "a conflict confirm with no head", row: OutboxRow{ID: 62, Kind: OutboxKindConflictConfirm, Payload: json.RawMessage(`{"number":42,"firstReadAt":"2026-09-23T00:59:00Z"}`)}, reason: "a conflict confirm names the pull request it reads again and the head"},
		{name: "a conflict confirm with no time of the first read", row: OutboxRow{ID: 66, Kind: OutboxKindConflictConfirm, Payload: json.RawMessage(`{"number":42,"head":"c0de"}`)}, reason: "a conflict confirm names when the first read it reads again was made"},
		{name: "a conflict confirm with an unknown field", row: OutboxRow{ID: 63, Kind: OutboxKindConflictConfirm, Payload: json.RawMessage(`{"number":42,"head":"c0de","firstReadAt":"2026-09-23T00:59:00Z","base":"main"}`)}, reason: "unknown field"},
		{name: "a merge-forward message of an unknown step", row: OutboxRow{ID: 64, Kind: OutboxKindMergeForward, Payload: json.RawMessage(`{"role":"implementer","step":"merge","head":"c0de","base":"main","phase":"testing","pullRequest":42,"why":"merge-next"}`)}, reason: "is an ask, a hold, a landed or a review"},
		{name: "a merge-forward message with an unknown field", row: OutboxRow{ID: 65, Kind: OutboxKindMergeForward, Payload: json.RawMessage(`{"role":"implementer","step":"ask","head":"c0de","base":"main","phase":"testing","pullRequest":42,"why":"merge-next","task":"Merge main."}`)}, reason: "unknown field"},
		{name: "a claim release with a field", row: OutboxRow{ID: 54, Kind: OutboxKindClaimRelease, Payload: json.RawMessage(`{"issue":"LEGION-208"}`)}, reason: "unknown field"},
		{name: "a retro's stored reason without its rating", row: OutboxRow{ID: 62, Kind: OutboxKindStepRecord, Payload: json.RawMessage(`{"id":"` + stepRecordID +
			`","task":"default","step":0,"kind":"retro","role":"implementer","type":"","agent":"legion-leg-leg-1-implementer","generation":1,"rating_reason":"The plan put the tests after the review."}`)},
			reason: "its reason or neither"},
		{name: "a retro's stored rating without its reason", row: OutboxRow{ID: 63, Kind: OutboxKindStepRecord, Payload: json.RawMessage(`{"id":"` + stepRecordID +
			`","task":"default","step":0,"kind":"retro","role":"implementer","type":"","agent":"legion-leg-leg-1-implementer","generation":1,"rating":3}`)},
			reason: "its reason or neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeOutboxPayload(tc.row)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("row %d", tc.row.ID)) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("DecodeOutboxPayload error = %v, want row %d and %q", err, tc.row.ID, tc.reason)
			}
		})
	}
}

// checkLookRefusal is the validation's words for a supervise row whose check look is not a start
// of the implementer or the tester with no phase.
const checkLookRefusal = "a supervise check look names a start of the implementer or the tester with no phase"

// newsOnlyRefusal is the validation's words for a supervise row marked a news-only start that is
// not a start of no task and no entry naming the phase of the holder it tells, or none for an
// architect.
const newsOnlyRefusal = "a supervise start for news alone carries no task and no entry"

// A hook prompt's message is three parts with a blank line between them: its first line, which
// R8 keeps as main writes it because a worker knows a hook by it; the hook's words exactly as the
// deployment wrote them; and the facts as key: value lines, the form the renderer writes. The
// facts stand below the words, because the shipped hook's words point at them. They are main's
// facts: the count that fired the hook, every move count of the issue in the order
// phase.MovePairs() gives (neither the map's own order nor alphabetical), and the pull
// request only for an issue with one. The words of the hook come through whole: a prompt that
// spells a word in angle brackets is published with it, since the daemon substitutes nothing
// into the hook's own text.
func TestAHookPromptStatesTheFactsAndLeavesTheHooksWordsAlone(t *testing.T) {
	const prompt = "This is the third time the work came back. Say what the round missed;\n<count> is no placeholder the daemon fills."
	const firstLine = "Legion hook back-to-implementing on LEGION-208: work went back from reviewing to implementing."
	fired := HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing,
		Count: 3, Counts: map[string]int{"reviewing-implementing": 3, "testing-implementing": 1, "retro-implementing": 2},
		PullRequest: "sjawhar/legion#42", Prompt: prompt}
	inOrder := []texts.MoveCount{{Move: "testing-implementing", Count: 1}, {Move: "reviewing-implementing", Count: 3}, {Move: "retro-implementing", Count: 2}}

	noPull := fired
	noPull.PullRequest = ""
	// The counts cannot normally be empty, since the firing counted the move that fired it, but a
	// row written any other way says so with an empty list.
	counted := noPull
	counted.Counts = nil
	for _, tc := range []struct {
		name  string
		fired HookPrompt
		facts texts.HookFacts
	}{
		{"a firing with a pull request", fired, texts.HookFacts{Count: 3, Counts: inOrder, PullRequest: "sjawhar/legion#42"}},
		{"a firing with no pull request", noPull, texts.HookFacts{Count: 3, Counts: inOrder}},
		{"a firing that counts nothing", counted, texts.HookFacts{Count: 3, Counts: []texts.MoveCount{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := tc.fired.Record("LEGION-208")
			if !reflect.DeepEqual(hook.HookFacts, tc.facts) {
				t.Fatalf("the facts of the firing are %+v, want %+v", hook.HookFacts, tc.facts)
			}
			got, err := tc.fired.Message("LEGION-208")
			if err != nil {
				t.Fatalf("Message: %v", err)
			}
			t.Logf("the role is told:\n%s", got)
			words, facts, found := strings.Cut(got, "\n\n"+prompt+"\n\n")
			if !found || words != firstLine {
				t.Fatalf("Message =\n%s\nwant the first line %q, then the hook's words whole, each part after a blank line", got, firstLine)
			}
			want, err := texts.Render(tc.facts)
			if err != nil {
				t.Fatal(err)
			}
			if facts != want {
				t.Fatalf("the facts below the words are\n%s\nwant\n%s", facts, want)
			}
			if hasPull := strings.Contains(facts, "pull_request: "); hasPull != (tc.fired.PullRequest != "") {
				t.Fatalf("the facts name a pull request: %t, want %t", hasPull, tc.fired.PullRequest != "")
			}
		})
	}
}

// The release field is gone: an issue's close deletes its Sandbox in every case, so a suspension
// carries nothing to say which. A row the previous release queued with the field is stripped of
// it at the upgrade (migration 0051), since the payload decodes strictly and the key would stall
// its issue's lane; one written without it has always read as this row does, and nothing encodes
// the key again.
func TestAnIssueSuspensionCarriesNoReleaseField(t *testing.T) {
	row := OutboxRow{ID: 40, Kind: OutboxKindIssueSuspend, Payload: json.RawMessage(`{"tree":"LEGION-208","generation":3,"treeGeneration":7}`)}
	got, err := DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("DecodeOutboxPayload: %v", err)
	}
	want := IssueSuspend{Tree: "LEGION-208", Generation: 3, TreeGeneration: 7}
	if got != want {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "release") {
		t.Fatalf("a suspension encodes %s, want no release field", encoded)
	}
	stale := OutboxRow{ID: 41, Kind: OutboxKindIssueSuspend, Payload: json.RawMessage(`{"tree":"LEGION-208","generation":3,"treeGeneration":7,"release":true}`)}
	if _, err := DecodeOutboxPayload(stale); err == nil || !strings.Contains(err.Error(), `unknown field "release"`) {
		t.Fatalf("decoding a row the previous release wrote = %v, want the strict refusal migration 0051 clears", err)
	}
}

// A start the release before this one wrote still decodes. Nothing writes SuperviseRequest's
// Relaunch mark now — a phase worker's start is one row that carries its task
// (workflow.StartOrOwe) — but the previous release wrote the task-less half of a pair with the
// mark on it, and such a row can still be in the outbox when this release deploys. The outbox
// decodes strictly (DisallowUnknownFields) and a refused payload is a row that retries for
// ever, so the field and the validation rule that reads it are kept for one release: the row
// decodes, and the executor acts on the mark nowhere.
func TestAStartTheReleaseBeforeWroteWithTheRelaunchMarkStillDecodes(t *testing.T) {
	row := OutboxRow{ID: 42, Kind: OutboxKindSupervise, Issue: "LEGION-208",
		Payload: json.RawMessage(`{"op":"start","tree":"LEGION-208","role":"implementer","generation":1,"phase":"implementing","relaunch":true}`)}
	got, err := DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decoding the start the previous release wrote = %v, want it accepted: a refused payload retries for ever", err)
	}
	want := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1,
		Phase: phase.Implementing, Relaunch: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %#v, want %#v", got, want)
	}
}

// The start a role was owed when the release before this one stopped still decodes and still
// carries its task. That release wrote the token it minted for the start onto the row
// (SuperviseRequest.Assignment, which the phase entry replaced), and such a row is in the outbox
// at the moment this release boots: the upgrade's own fill reads exactly that row's task as the
// work of entry 1 (migrations/20261111225552_phase_entry.up.sql). The outbox decodes strictly
// (DisallowUnknownFields) and record.Postgres.RoleRuns decodes every supervise row of the issues
// it reads, so a refused payload is both a row that retries for ever and an admission pass that
// fails on every fact — which is why the field is kept for one release, written by nothing and
// read by nothing.
func TestAnOwedStartTheReleaseBeforeWroteWithItsAssignmentStillDecodes(t *testing.T) {
	const task = "Test the change. Issue: LEGION-208. Phase: testing."
	row := OutboxRow{ID: 43, Kind: OutboxKindSupervise, Issue: "LEGION-208",
		Payload: json.RawMessage(`{"op":"start","tree":"LEGION-208","role":"tester","generation":1,"phase":"testing",` +
			`"assignment":"A1","needsSlot":true,"task":"` + task + `"}`)}
	got, err := DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decoding the start the previous release owed a tester = %v, want it accepted: a refused payload retries for ever and fails the admission pass that reads it", err)
	}
	want := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Generation: 1,
		Phase: phase.Testing, Assignment: "A1", NeedsSlot: true, Task: task}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %#v, want %#v: the row carries the task the upgrade reads as the work of entry 1", got, want)
	}
	// Nothing writes the field: a start of this release carries its entry and no assignment.
	encoded, err := json.Marshal(SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester,
		Generation: 1, Phase: phase.Testing, Entry: 2, NeedsSlot: true, Task: task})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "assignment") {
		t.Fatalf("a start of this release encodes %s, want no assignment field: the entry replaced it", encoded)
	}
}

// A tick notice carries the names of the deployment capabilities with no decision through the
// outbox row and back, and a notice with none carries no such field: the controller's payload
// reads `openCapabilities` only when there is a gap to name.
func TestATickNoticeCarriesItsOpenCapabilitiesThroughTheRow(t *testing.T) {
	now := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		payload ControllerNotice
		field   bool
	}{
		{"a tick naming two gaps", ControllerNotice{Kind: TickNotice, OpenCapabilities: []string{"secrets", "resource-limits"}}, true},
		{"a tick with no gap", ControllerNotice{Kind: TickNotice}, false},
		{"a slot-free wake", ControllerNotice{Kind: SlotFreeNotice}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := NewOutboxRow("LEGION", tc.payload, now)
			if err != nil {
				t.Fatalf("NewOutboxRow: %v", err)
			}
			if got := strings.Contains(string(row.Payload), `"openCapabilities"`); got != tc.field {
				t.Fatalf("payload %s carries openCapabilities: %t, want %t", row.Payload, got, tc.field)
			}
			got, err := DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("DecodeOutboxPayload: %v", err)
			}
			if !reflect.DeepEqual(got, tc.payload) {
				t.Fatalf("payload = %#v, want %#v", got, tc.payload)
			}
		})
	}
}

// A parked-decision notice keeps every field through its outbox row, and a spec decision's
// architect notice keeps who made it and where.
func TestAParkedDecisionKeepsItsDecisionThroughTheRow(t *testing.T) {
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	for _, payload := range []OutboxPayload{
		ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionDesignChangesRequested, Root: "LEGION-1", Ref: "dispatch://LEGION-1/spec",
			Actor: "user:alice", Version: 3, Reason: "name the rollback path"},
		ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionAskReply, Root: "LEGION-1", Ref: "dispatch://LEGION-2/comment/c-1", Actor: "user:alice"},
		Notice{Kind: "design-approved", Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-1/spec"},
	} {
		row, err := NewOutboxRow("LEGION-1", payload, now)
		if err != nil {
			t.Fatalf("NewOutboxRow(%#v): %v", payload, err)
		}
		got, err := DecodeOutboxPayload(row)
		if err != nil || !reflect.DeepEqual(got, payload) {
			t.Fatalf("decoded %#v, %v; want %#v", got, err, payload)
		}
	}
}

// SpecDecision names the three notice kinds a person's spec decision writes, and no other.
func TestSpecDecisionNamesTheThreeKindsAPersonsSpecDecisionWrites(t *testing.T) {
	for kind, want := range map[NoticeKind]DecisionKind{
		"design-approved":          DecisionDesignApproved,
		"design-changes-requested": DecisionDesignChangesRequested,
		"carried-gate-opened":      DecisionCarriedGateOpened,
		"child-status":             "",
		"worker-died":              "",
		ParkedDecisionNotice:       "",
	} {
		got, ok := SpecDecision(kind)
		if got != want || ok != (want != "") {
			t.Errorf("SpecDecision(%q) = %q, %t; want %q, %t", kind, got, ok, want, want != "")
		}
	}
	for actor, want := range map[string]bool{"user:alice": true, "user:": false, "session:s": false, "": false, "agent:user:alice": false} {
		if got := PersonActor(actor); got != want {
			t.Errorf("PersonActor(%q) = %t, want %t", actor, got, want)
		}
	}
}

// The boot's publish of the deployment's design-gate steps is one outbox row addressed to the
// bare project key rather than to an issue. The record's part is three things: the row is due to
// the daemon of that project and to no other, which the same split_part scope answers for a key
// with no hyphen; a boot writes the project's one row of the kind, replacing what a pending row
// carries, so the list waiting for Dispatch is always the list the booting daemon holds; and a
// finished row leaves nothing behind, so the boot after it queues its own.
func TestGatedStepsPublishAtBoot(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	// boot is what openWorkflow does with the list it was configured with: write the project's
	// one publish.
	boot := func(project string, steps []DesignStep) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			must(t, records.EnqueueGatedSteps(ctx, tx, GatedStepsPublish{Project: project, Steps: steps}, now))
		})
	}
	// queued is the one publish the outbox holds for project, by row id and by what it carries.
	queued := func(project string) (int64, GatedStepsPublish) {
		t.Helper()
		var held []OutboxRow
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			held, err = records.DueRows(ctx, tx, project, "", nil, now)
			must(t, err)
		})
		if len(held) != 1 || held[0].Issue != project {
			t.Fatalf("DueRows of %s = %#v, want the one publish addressed to the project key", project, held)
		}
		payload, err := DecodeOutboxPayload(held[0])
		must(t, err)
		publish, ok := payload.(GatedStepsPublish)
		if !ok {
			t.Fatalf("the row of %s decodes as %#v, want a gated-steps publish", project, payload)
		}
		return held[0].ID, publish
	}

	boot("LEGION", []DesignStep{DesignStepCriteria, DesignStepSpec})
	first, publish := queued("LEGION")
	want := GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{DesignStepCriteria, DesignStepSpec}}
	if !reflect.DeepEqual(publish, want) {
		t.Fatalf("the publish carries %#v, want %#v, the configured list in the configured order", publish, want)
	}

	// A second boot on the same list leaves the row it finds where it is, so a restart loop
	// writes no second publish and takes nothing off the backoff the lane has set on this one.
	boot("LEGION", []DesignStep{DesignStepCriteria, DesignStepSpec})
	if id, again := queued("LEGION"); id != first || !reflect.DeepEqual(again, publish) {
		t.Fatalf("a boot on the unchanged list left row %d carrying %#v, want row %d carrying %#v untouched", id, again, first, publish)
	}

	// A boot on a changed list while that row is still pending replaces what it carries: an
	// operator who changes gates.design and restarts while Dispatch has yet to take the publish
	// gets the list this daemon booted on, not the one the boot before it held.
	boot("LEGION", []DesignStep{DesignStepSpec})
	changedRow, changed := queued("LEGION")
	if changedRow != first {
		t.Fatalf("the boot on the changed list wrote row %d beside %d, want the pending row carrying the new list", changedRow, first)
	}
	if want := (GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{DesignStepSpec}}); !reflect.DeepEqual(changed, want) {
		t.Fatalf("the pending publish carries %#v after the boot on a changed list, want %#v", changed, want)
	}

	inTx(t, st, func(tx pgx.Tx) {
		rows, err := records.DueRows(ctx, tx, "INFRA", "", nil, now)
		must(t, err)
		if len(rows) != 0 {
			t.Fatalf("DueRows of INFRA = %#v, want nothing: LEGION's publish is LEGION's daemon's", rows)
		}
	})

	var claimed OutboxRow
	inTx(t, st, func(tx pgx.Tx) {
		var ok bool
		var err error
		claimed, ok, err = records.ClaimRow(ctx, tx, changedRow, now, time.Minute)
		must(t, err)
		if !ok {
			t.Fatal("ClaimRow of the publish claimed nothing")
		}
		must(t, records.FinishOutbox(ctx, tx, claimed.ID, claimed.LeaseToken))
	})

	// The run deleted the row, so the next boot publishes whatever it is configured with now: a
	// deployment that gates nothing publishes the empty list rather than nothing at all.
	boot("LEGION", []DesignStep{})
	ran, published := queued("LEGION")
	if ran == changedRow {
		t.Fatalf("the boot after the publish ran reused row %d, want a row of its own", ran)
	}
	if want := (GatedStepsPublish{Project: "LEGION", Steps: []DesignStep{}}); !reflect.DeepEqual(published, want) {
		t.Fatalf("the next boot published %#v, want the empty list of a deployment that gates nothing", published)
	}
}

// LongestPost is what a caller checks before a message is recorded, and Posted is what the runner
// posts, so no row's post may pass a bound the text its caller checked clears: the post of the
// smallest row id and of the largest are the same text but for the marker's digits, and the
// marker of math.MaxInt64 is the longest there is. Each body here stands on a bound — the body
// one block or one unit longer makes a post Dispatch refuses, which the case asserts — so a post
// of some row that measured above LongestPost is refused below rather than passing unnoticed.
// The shapes are the ones where the marker changes the measure: a body ending in a paragraph,
// where the marker is a block of its own, and one ending in an open code fence, where it is code
// inside the last block.
func TestNoRowsPostMeasuresAboveLongestPost(t *testing.T) {
	// The blank line and the longest marker LongestPost puts after a body, which the fenced body
	// below leaves room for inside its last block.
	marker := len(LongestPost(""))
	const fence = "see log\n\n```\n"
	for _, tc := range []struct{ name, body, longer string }{
		{"a body of paragraphs one block short of the bound on blocks",
			strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks-1), "\n\n"),
			strings.TrimSuffix(strings.Repeat("y\n\n", dispatchbody.ItemBlocks), "\n\n")},
		{"a last paragraph on the bound on units, which the marker stands beside",
			strings.Repeat("y", dispatchbody.BlockUnits),
			strings.Repeat("y", dispatchbody.BlockUnits+1)},
		{"an open code fence the marker falls inside, on the bound on units with it",
			fence + strings.Repeat("y", dispatchbody.BlockUnits-len("```\n")-marker),
			fence + strings.Repeat("y", dispatchbody.BlockUnits-len("```\n")-marker+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if over := dispatchbody.Check(LongestPost(tc.body)); over != nil {
				t.Fatalf("the longest post of this body = %v, want one Dispatch takes", over)
			}
			if dispatchbody.Check(LongestPost(tc.longer)) == nil {
				t.Fatalf("the longest post of this body one block or one unit longer is taken too, so the body is not on the bound and no post below could measure above it")
			}
			for _, id := range []int64{1, 9, 10, 99, 100, 1_000_000, math.MaxInt64 - 1, math.MaxInt64} {
				if over := dispatchbody.Check((MessagePost{Body: tc.body}).Posted(id)); over != nil {
					t.Errorf("the post of row %d = %v while the text its caller checked passes, so a body checked here is refused at the post", id, over)
				}
			}
		})
	}
}

// A published notice of every kind the validation takes is still a payload NewOutboxRow accepts,
// so every kind can be re-held. The daemon builds a re-held copy out of the payload the publish
// carried — the listener's report decoded back into a notice (the daemon's exceptionNotice,
// reholdNotice), with the two paced moments put back by hand on the one path that keeps them
// (noticeAnswered) — and validates that copy before queueing it, so a field Published drops that
// the kind's own validation requires makes every re-hold of that kind refused and its architect
// never told, with nothing else to say so.
//
// The table holds one notice of each kind the list marks an architect's notice, each built from
// its kind's record with its fields set, and the coverage assertion holds it against the list in
// both directions, so a kind added to the list tomorrow is a row missing here rather than a kind
// nobody checked, and a kind the table names that is no architect's notice on the list is named
// too. Each row is judged by the validation itself rather than against a list of field names, so
// a field a kind comes to require is covered the day it is required: Published dropping it fails
// this test without anybody remembering to name it here. Each row then holds the publish to being
// the notice the daemon wrote, field for field and kind by kind, the fields the records added
// among them, rather than to validating: the validation refuses a field that is missing, and a
// field the publish changes instead of dropping is one it takes.
func TestAPublishedNoticeIsStillAValidOutboxPayload(t *testing.T) {
	answered := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	paced := answered.Add(time.Minute)
	head := func(kind texts.Kind) texts.NoticeHead { return texts.NoticeHead{Kind: kind, Issue: "LEGION-208"} }
	unset := false
	// One notice per kind, each as its own kind's requirements have it: a catch-up carries its
	// account of the tree, the three spec decisions the person who decided and the reference,
	// and no other kind carries either of those.
	// The pacing's two moments are a catch-up's alone (Notice.PacedAt), so that row alone carries
	// them; the re-hold's restore of them below is a no-op on the rest, as it is in the daemon.
	catchUp := mustNotice(t, texts.CatchUpRecord{NoticeHead: head(texts.CatchUp), CatchUp: &texts.CatchUpFacts{Generation: 3, Launch: 2,
		Gates:      []texts.CatchUpGate{{Step: string(DesignStepSpec), Gated: true, Artifact: "artifact-208", Version: 3, Approved: 3, Open: true}},
		Issues:     []texts.CatchUpIssue{{Key: "LEGION-209", Parent: "LEGION-208", Title: "The outbox's notices", Phase: phase.Accepting, Status: "in_progress"}},
		Done:       []string{"LEGION-210"},
		Unreleased: []texts.UnreleasedChild{{Key: "LEGION-211", Parent: "LEGION-208", Status: "backlog"}},
		Accepting:  []string{"LEGION-209"},
		Kept: &texts.KeptFacts{Decisions: []texts.DecisionMark{{Ref: "dispatch://LEGION-208/ask/a1", At: answered, Issue: "LEGION-208",
			Decision: string(DecisionAskAnswered), Actor: "user:alice", Options: []string{"ship"}, Answer: "Ship it."}}}},
		Relaunch: &texts.RelaunchFacts{Cause: "the pod was replaced", InTurn: true, Task: true},
		Words:    map[string]texts.WordsMeta{"catch_up.kept.decisions[0].answer": {By: "user:alice", At: answered}}})
	catchUp.PacedAt, catchUp.PacedFrom = paced, answered
	// A field every row leaves at zero is compared zero to zero, and proves nothing of the field:
	// the comparison below would pass on a publish that dropped it. So the pr-blocked row carries
	// a Resends of 1, since any non-zero value puts the field in the comparison, and it stays
	// there rather than beside the three set aside. The daemon reads this count off the published
	// payload and nowhere else: it is what stops the
	// re-holds at the cap, and what picks the next copy's delay (the daemon's reholdNotice,
	// notice_exceptions.go). A Published that zeroed it would re-hold every architect notice for
	// ever, and every check above would stay green.
	//
	// The worker-died row carries the failure record the two notices of a claim's failure name
	// (NoticeFailure), for the same reason: the held row is kept for the escalation, which
	// reports a hold rather than a failure and names none, and the record is one field, which a
	// Published that dropped it would drop for both kinds alike.
	every := []Notice{
		mustNotice(t, texts.PhaseFinishedRecord{NoticeHead: head(texts.PhaseFinished), Role: claim.RoleTester, Phase: phase.Testing,
			To: phase.Reviewing, Summary: "Tests passed", Verdict: "pass", HandoffPhase: "testing", HandoffVersion: 2, HandoffCommit: "c0de5eed",
			Words: map[string]texts.WordsMeta{"summary": {By: "tester", At: answered}}}),
		mustNotice(t, texts.WorkerDiedRecord{NoticeHead: head(texts.WorkerDied), Role: claim.RoleImplementer, Phase: phase.Implementing,
			Failure: &NoticeFailure{Cause: "launch_refused", Detail: "the pod did not start",
				LaunchFailures: FailureBudget{Count: 3, Limit: 3}, Deaths: FailureBudget{Count: 1, Limit: 3},
				PromptFailures: FailureBudget{Limit: 3}, PromptRetires: FailureBudget{Limit: 2}}}),
		mustNotice(t, texts.WorkerStalledRecord{NoticeHead: head(texts.WorkerStalled), Role: claim.RoleImplementer,
			LastStep: answered.Add(-time.Hour), SilentFor: "1h0m0s", Bound: "45m0s", TurnStarted: answered.Add(-2 * time.Hour),
			CallTool: "bash", CallID: "call_1", CallStarted: answered.Add(-70 * time.Minute), End: "ended", Stop: texts.StopStopped, LaunchFailures: 1}),
		mustNotice(t, texts.WorkerSilentRecord{NoticeHead: head(texts.WorkerSilent), Role: claim.RoleTester, Phase: phase.Testing,
			LastStep: answered.Add(-time.Hour), SilentFor: "1h0m0s", Bound: "30m0s", Sent: texts.SentPhaseTask, Entry: 2, Given: 2}),
		mustNotice(t, texts.WorkerBackgroundStalledRecord{NoticeHead: head(texts.WorkerBackgroundStalled), Role: claim.RoleTester, Phase: phase.Testing,
			Work: []string{"async-job"}, ReportedAt: answered.Add(-time.Hour), StoodFor: "1h0m0s", Bound: "45m0s"}),
		mustNotice(t, texts.HeldRecord{NoticeHead: head(texts.Held), Role: claim.RolePlanner, Phase: phase.Planning, Hold: texts.HoldEscalation,
			Reason: string(HoldEscalated), Escalation: "a person should look at the runner",
			Words: map[string]texts.WordsMeta{"escalation": {By: "architect", At: answered}}}),
		mustNotice(t, texts.PRBlockedRecord{NoticeHead: texts.NoticeHead{Kind: texts.PRBlocked, Issue: "LEGION-208", Resends: 1}, Cap: 5, Counted: 5, ConflictRounds: 1}),
		mustNotice(t, texts.PRMergedRecord{NoticeHead: head(texts.PRMerged), PullRequest: "sjawhar/legion#42", Phase: phase.Reviewing, WaitingFor: []string{"LEGION-209"}}),
		mustNotice(t, texts.PRClosedUnmergedRecord{NoticeHead: head(texts.PRClosedUnmerged), PullRequest: "sjawhar/legion#42", Phase: phase.Reviewing}),
		mustNotice(t, texts.PullRequestMissingRecord{NoticeHead: head(texts.PullRequestMissing), HandoffCommit: "c0de5eed", Repo: "sjawhar/legion", Branch: "legion/LEGION-208"}),
		mustNotice(t, texts.MessageRefusedRecord{NoticeHead: head(texts.MessageRefused), Refused: texts.RefusedStatus,
			DispatchAnswer: "read Dispatch issue LEGION-208 before status write: NOT_FOUND: not found", Length: 5000,
			Begins: "Closed by its architect", Status: "done", StatusSet: &unset, ReasonPosted: &unset}),
		mustNotice(t, texts.DesignApprovedRecord{NoticeHead: head(texts.DesignApproved),
			GateDecision: texts.GateDecision{Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-208/spec", At: answered}}),
		mustNotice(t, texts.DesignChangesRequestedRecord{NoticeHead: head(texts.DesignChangesRequested),
			GateDecision: texts.GateDecision{Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-208/spec", At: answered},
			Reason:       "Name the rollback path.", Words: map[string]texts.WordsMeta{"reason": {By: "user:alice", At: answered}}}),
		mustNotice(t, texts.CarriedGateOpenedRecord{NoticeHead: head(texts.CarriedGateOpened),
			GateDecision: texts.GateDecision{Version: 3, Actor: "user:alice", Ref: "dispatch://LEGION-200/spec", At: answered}}),
		mustNotice(t, texts.ChildClosedRecord{NoticeHead: texts.NoticeHead{Kind: texts.ChildClosed, Issue: "LEGION-209"}}),
		mustNotice(t, texts.ChildStatusRecord{NoticeHead: texts.NoticeHead{Kind: texts.ChildStatus, Issue: "LEGION-209"}, Change: texts.ChangeJoined,
			Tree: "LEGION-208", WaitedOn: []string{"LEGION-210"}, FromTree: "LEGION-200", Status: "todo"}),
		mustNotice(t, texts.ChildrenFirstRecord{NoticeHead: head(texts.ChildrenFirst), Children: []texts.OpenChildFacts{
			{Key: "LEGION-209", Status: "todo", Released: true}, {Key: "LEGION-210", Status: "backlog", Cycle: "LEGION-208"}}}),
		mustNotice(t, texts.ChildReleaseStalledRecord{NoticeHead: head(texts.ChildReleaseStalled), Stall: texts.StallRecoveredUnrecorded,
			Repos: []string{"sjawhar/legion"}, Unrecorded: []texts.UnrecordedChild{{Child: "LEGION-209", Parent: "LEGION-208", ParentRecorded: true}}}),
		mustNotice(t, texts.ChildNotRecordedRecord{NoticeHead: head(texts.ChildNotRecorded), Child: "LEGION-209", Resolved: "acme/gadgets",
			Refusal: "the review App is not installed on acme/gadgets"}),
		catchUp,
		mustNotice(t, texts.ChecksRedRecord{NoticeHead: head(texts.ChecksRed), Phase: phase.Testing, ConflictsWith: "main", Head: "c0de", PullRequest: "sjawhar/legion#42"}),
		mustNotice(t, texts.ReviewStuckRecord{NoticeHead: head(texts.ReviewStuck), Role: claim.RoleReviewer, Phase: phase.Reviewing,
			Summary: texts.StuckByAnswer, Topic: "notifications.role.legion-LEGION-208-reviewer", Cause: texts.CauseApprovedOtherCode,
			PullRequest: "sjawhar/legion#42", Head: "c0de", ApprovedHead: "beef", EndsRound: texts.EndsRound("c0de")}),
		mustNotice(t, texts.StatusReassertedRecord{NoticeHead: head(texts.StatusReasserted), Session: "ses_1", Wrote: "todo", Status: "in_progress"}),
		mustNotice(t, texts.TaskStoppedRecord{NoticeHead: head(texts.TaskStopped), Role: claim.RoleReviewer, Phase: phase.Reviewing,
			Task: "a", Cause: texts.TaskStoppedByOwner, StoppedBy: "legion-legion-legion-208-architect"}),
		mustNotice(t, texts.PlanNotTakenRecord{NoticeHead: texts.NoticeHead{Kind: texts.PlanNotTaken, Issue: "LEGION-209"}, Why: texts.PlanNotTakenEarlierOwner,
			NotTaken: []texts.PlanVersionFacts{{Version: 1, Artifact: "6a1f0c3e-2b7d-4e5a-9c81-3d4f5e6a7b80", GivenBy: "legion-legion-legion-208-architect"}},
			Runs:     texts.PlanRunsOwner, RunsVersion: 2, RunsArtifact: "7b2e1d4f-3c8e-4f6b-8d92-4e5f6a7b8c91", RunsGivenBy: "legion-legion-legion-209-architect"}),
		mustNotice(t, texts.RetroHeldRecord{NoticeHead: head(texts.RetroHeld), Role: claim.RoleImplementer, Phase: phase.Retro,
			WaitingFor: []string{"LEGION-209"}, StatusNotTakenYet: StatusNotTakenYet, EndChildRule: EndChildRule}),
		mustNotice(t, texts.SessionLostRecord{NoticeHead: head(texts.SessionLost), Role: claim.RoleImplementer, Phase: phase.Implementing,
			Detail: "no session store holds ses_1"}),
		mustNotice(t, texts.SessionForgottenRecord{NoticeHead: head(texts.SessionForgotten), Role: claim.RoleImplementer, Phase: phase.Implementing,
			Session: "ses_1"}),
		mustNotice(t, texts.SessionStoreFaultRecord{NoticeHead: head(texts.SessionStoreFault), Role: claim.RoleImplementer, Phase: phase.Implementing,
			Detail: "the session store did not answer", RetryEvery: "1m0s"}),
		mustNotice(t, texts.VolumeNearFullRecord{NoticeHead: head(texts.VolumeNearFull),
			Volume: &VolumeNearFull{PodUID: "pod-uid", Trimmer: "trim-1", Episode: 2, RaisedBy: "rise",
				UsedBytes: 48 << 30, FreeBytes: 2 << 30, FreedBytes: 1 << 30,
				StopReason: "nothing left older than the minimum age", RefusedReads: []string{"pid 42"},
				Sizes: []VolumeSize{{Path: "caches/uv", Bytes: 12 << 30, BytesOverHour: 11 << 30, BytesOverDay: 9 << 30,
					Refused: "caches/uv/.../locked (depth 2)", Refusals: 3}},
				SizesAt: answered, SizesPartial: true, SizesMissing: []VolumeMissing{{Path: "workspaces/LEGION-209", Reason: "openat: permission denied"}},
				Faults: []string{"read the volume: panic: runtime error\nmain.go:1"}}}),
	}

	byKind := make(map[NoticeKind]Notice, len(every))
	for _, notice := range every {
		if _, twice := byKind[notice.Kind]; twice {
			t.Fatalf("the table holds two %s notices, want one of each kind", notice.Kind)
		}
		byKind[notice.Kind] = notice
	}
	for _, entry := range texts.Entries() {
		if _, named := byKind[entry.Kind]; entry.Form == texts.Notice && !named {
			t.Errorf("the list marks %s an architect's notice and the table holds none: add one, or a publish that drops what that kind requires passes here unseen", entry.Kind)
		}
	}
	for kind := range byKind {
		if entry, _ := texts.Lookup(kind); entry.Form != texts.Notice {
			t.Errorf("the table holds a %s notice, which the list does not mark an architect's notice", kind)
		}
	}

	for _, notice := range every {
		t.Run(string(notice.Kind), func(t *testing.T) {
			if _, err := NewOutboxRow("LEGION-208", notice, paced); err != nil {
				t.Fatalf("the notice the daemon writes is refused before any publish: %v", err)
			}
			published, _ := notice.Published(41)
			// The copy the re-hold builds is this payload, with the two paced moments put back by
			// hand at the site that builds it; what Published dropped of the kind's own fields is
			// not put back anywhere, so the copy is refused for want of it.
			copied := published
			copied.PacedAt, copied.PacedFrom = notice.PacedAt, notice.PacedFrom
			if _, err := NewOutboxRow("LEGION-208", copied, paced); err != nil {
				t.Fatalf("the re-held copy of a published %s notice is refused, so its architect is never told: %v", notice.Kind, err)
			}

			// Valid is not the same as unchanged. The validation refuses only a field a kind
			// requires and does not hold — a catch-up without its account of the tree — so a
			// publish that moved a field rather than dropping it would pass every check above
			// and leave the re-held copy carrying something its architect was never owed. The
			// comparison below runs over the wire form the re-hold reads the copy back from,
			// decoded the way the daemon's exceptionNotice decodes the listener's report, with
			// unknown fields refused, so a field the publish renames or invents is caught here
			// too.
			wire, err := json.Marshal(published)
			if err != nil {
				t.Fatalf("the published %s notice does not marshal, so no listener could carry it: %v", notice.Kind, err)
			}
			var back Notice
			decoder := json.NewDecoder(strings.NewReader(string(wire)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&back); err != nil {
				t.Fatalf("the published %s notice does not read back as a notice, so no re-hold of it can be built: %v", notice.Kind, err)
			}
			// Set aside on both sides are the three the publish is meant to change, the daemon's
			// own bookkeeping of the row, and nothing else: everything a kind carries is the
			// architect's and must cross the publish untouched. Resends is bookkeeping too and is
			// not among them, because the re-hold reads it back off the publish: the pr-blocked
			// row above carries one, so the publish is held to carrying it on.
			want := notice
			want.PacedAt, want.PacedFrom, want.ResendOf = time.Time{}, time.Time{}, 0
			back.PacedAt, back.PacedFrom, back.ResendOf = time.Time{}, time.Time{}, 0
			if !reflect.DeepEqual(back, want) {
				// Both are printed as the wire carries them: %#v prints a pointer field's
				// address rather than the wait or the account behind it, which is where the
				// kinds that carry one differ. Neither Marshal can fail, the one above having
				// just marshalled these very fields.
				gotWire, _ := json.Marshal(back)
				wantWire, _ := json.Marshal(want)
				t.Fatalf("the published %s notice is %s, want %s: a published notice that is not the notice the daemon wrote means the re-held copy is not the one its architect was owed", notice.Kind, gotWire, wantWire)
			}
		})
	}
}

// A notice row and a role-message row of a kind that is not on the list are each refused before
// they are queued, as a controller notice of such a kind is. A role-message row with no kind and
// a body, as an older daemon writes it, is taken, and reads back as it was queued.
func TestAnOutboxRowOfAKindOffTheListIsRefused(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	const offList texts.Kind = "phase-started"
	if _, listed := texts.Lookup(offList); listed {
		t.Fatalf("the list holds %s, so it is no kind off the list", offList)
	}
	for _, tc := range []struct {
		name    string
		payload OutboxPayload
		reason  string
	}{
		{"a notice", Notice{Kind: offList, Role: claim.RoleTester, Phase: phase.Testing, Issue: "LEGION-208"}, "no notice kind on the list"},
		{"a controller notice", ControllerNotice{Kind: offList, Issue: "LEGION-208"}, "no notice kind on the list"},
		{"a role message", RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.tester", Kind: offList, Urgency: texts.Med,
			Fields: json.RawMessage(`{"kind":"phase-started","issue":"LEGION-208"}`)}, "no role-message kind on the list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewOutboxRow("LEGION-208", tc.payload, now)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("NewOutboxRow of %s of kind %s = %v, want it refused with %q", tc.name, offList, err, tc.reason)
			}
		})
	}
	older := RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.tester", Body: "CI settled at head c0de: \"build\" succeeded, \"unit\" failed"}
	row, err := NewOutboxRow("LEGION-208", older, now)
	if err != nil {
		t.Fatalf("a role message with no kind and a body is refused: %v", err)
	}
	if got, err := DecodeOutboxPayload(row); err != nil || !reflect.DeepEqual(got, older) {
		t.Fatalf("the role message with no kind reads back as %#v, %v; want %#v", got, err, older)
	}
}

// Each row below is the payload main's daemon queued, as its JSON stands in the outbox: a
// phase-finished notice whose reason holds the move back's prose, a worker-stalled notice, an
// architect-stalled wake and a memory-guard-kill notice whose facts are a sentence in the reason,
// an intake-gave-up notice that names no issue, a catch-up with the text of the decisions it
// kept, a held notice with the architect's words, the tick's lists, a role message of no kind with
// its body, an update ask with its text, a merge forward's ask and its landed, and a start with
// the decisions it kept. This daemon decodes and validates each, for one release, into the
// payload main wrote.
func TestARowAnOlderDaemonQueuedStillDecodes(t *testing.T) {
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	kept := &KeptDecisions{Decisions: []DecisionMark{{Ref: "dispatch://LEGION-208/ask/a1", At: at, Issue: "LEGION-208", Decision: DecisionAskAnswered, Actor: "user:alice"}}}
	for _, tc := range []struct {
		name    string
		kind    OutboxKind
		payload string
		want    OutboxPayload
	}{
		{"a phase-finished notice of a move back", OutboxKindNotice,
			`{"kind":"phase-finished","role":"tester","phase":"testing","reason":"moved back to implementing by the tester: the suite fails against main at c0de"}`,
			Notice{Kind: texts.PhaseFinished, Role: claim.RoleTester, Phase: phase.Testing, Reason: "moved back to implementing by the tester: the suite fails against main at c0de"}},
		{"a phase-finished notice of a completion", OutboxKindNotice,
			`{"kind":"phase-finished","role":"implementer","phase":"implementing","summary":"Implemented the change; tests pass."}`,
			Notice{Kind: texts.PhaseFinished, Role: claim.RoleImplementer, Phase: phase.Implementing, Summary: "Implemented the change; tests pass."}},
		{"a worker-stalled notice", OutboxKindNotice,
			`{"kind":"worker-stalled","role":"implementer","reason":"the daemon ended the implementer's turn and started a replacement in its place: its agent last stepped at 2026-11-08T08:00:00Z, past the bound this deployment gives the role, and its session was brought back on the replacement the daemon started"}`,
			Notice{Kind: texts.WorkerStalled, Role: claim.RoleImplementer, Reason: "the daemon ended the implementer's turn and started a replacement in its place: its agent last stepped at 2026-11-08T08:00:00Z, past the bound this deployment gives the role, and its session was brought back on the replacement the daemon started"}},
		{"a catch-up notice", OutboxKindNotice,
			`{"kind":"catch-up","role":"architect","reason":"the tree of LEGION-208 at generation 3 as the daemon records it now; start or resume it as your role says; in accepting, waiting for your acceptance, which no worker gives: LEGION-209 (read the evidence, then sign_off or request_backward_move)","paced_at":"2026-11-08T09:01:00Z","paced_from":"2026-11-08T09:00:00Z","catch_up":{"generation":3,"launch":2,"gates":[{"step":"spec","gated":true,"artifact":"artifact-208","version":3,"approved":3,"open":true}],"issues":[{"key":"LEGION-208","title":"The outbox's notices","phase":"implementing","status":"in_progress"},{"key":"LEGION-209","parent":"LEGION-208","title":"A child","phase":"accepting","status":"in_progress"}],"decided":"A person answered dispatch://LEGION-208/ask/a1 (user:alice) while nobody in the tree could hear it.","kept":{"decisions":[{"ref":"dispatch://LEGION-208/ask/a1","at":"2026-11-08T09:00:00Z","issue":"LEGION-208","decision":"ask-answered","actor":"user:alice"}]}}}`,
			Notice{Kind: texts.CatchUp, Role: claim.RoleArchitect, PacedAt: at.Add(time.Minute), PacedFrom: at,
				Reason: "the tree of LEGION-208 at generation 3 as the daemon records it now; start or resume it as your role says; in accepting, waiting for your acceptance, which no worker gives: LEGION-209 (read the evidence, then sign_off or request_backward_move)",
				CatchUp: &CatchUp{Generation: 3, Launch: 2,
					Gates: []CatchUpGate{{Step: DesignStepSpec, Gated: true, Artifact: "artifact-208", Version: 3, Approved: 3, Open: true}},
					Issues: []CatchUpIssue{{Key: "LEGION-208", Title: "The outbox's notices", Phase: phase.Implementing, Status: "in_progress"},
						{Key: "LEGION-209", Parent: "LEGION-208", Title: "A child", Phase: phase.Accepting, Status: "in_progress"}},
					Decided: "A person answered dispatch://LEGION-208/ask/a1 (user:alice) while nobody in the tree could hear it.",
					Kept:    kept}}},
		{"a held notice of an escalation to the controller", OutboxKindControllerNotice,
			`{"kind":"held","role":"implementer","phase":"implementing","reason":"escalated","escalation":"the runner cannot reach the registry"}`,
			ControllerNotice{Kind: texts.Held, Role: claim.RoleImplementer, Phase: phase.Implementing, Reason: string(HoldEscalated), Escalation: "the runner cannot reach the registry"}},
		{"a held notice of a failure to the controller", OutboxKindControllerNotice,
			`{"kind":"held","role":"implementer","phase":"implementing","failure":{"cause":"launch_refused","detail":"the pod did not start","launchFailures":{"count":3,"limit":3},"deaths":{"count":1,"limit":3},"promptFailures":{"count":0,"limit":3},"promptRetires":{"count":0,"limit":2}}}`,
			ControllerNotice{Kind: texts.Held, Role: claim.RoleImplementer, Phase: phase.Implementing, Failure: &NoticeFailure{
				Cause: "launch_refused", Detail: "the pod did not start", LaunchFailures: FailureBudget{Count: 3, Limit: 3}, Deaths: FailureBudget{Count: 1, Limit: 3},
				PromptFailures: FailureBudget{Limit: 3}, PromptRetires: FailureBudget{Limit: 2}}}},
		{"an architect-stalled wake", OutboxKindControllerNotice,
			`{"kind":"architect-stalled","role":"architect","reason":"the architect took nothing: the task of phase entry 3 was first queued to the architect's claim of LEGION-208 at 2026-11-08T08:00:00Z"}`,
			ControllerNotice{Kind: ArchitectStalledNotice, Role: claim.RoleArchitect, Reason: "the architect took nothing: the task of phase entry 3 was first queued to the architect's claim of LEGION-208 at 2026-11-08T08:00:00Z"}},
		{"a memory-guard-kill notice", OutboxKindControllerNotice,
			"{\"kind\":\"memory-guard-kill\",\"role\":\"controller\",\"reason\":\"The launcher's memory guard stopped one of your processes with SIGKILL because your pod was near its memory limit: `python3` (pid 4242)\"}",
			ControllerNotice{Kind: MemoryGuardKillNotice, Role: claim.RoleController,
				Reason: "The launcher's memory guard stopped one of your processes with SIGKILL because your pod was near its memory limit: `python3` (pid 4242)"}},
		{"an intake-gave-up notice", OutboxKindControllerNotice,
			`{"kind":"intake-gave-up","count":3,"giveUps":[{"id":41,"consumer":"legion-go-LEGION-dispatch","subject":"notifications.dispatch.issue.LEGION-208.issue.updated"}]}`,
			ControllerNotice{Kind: IntakeGaveUpNotice, Count: 3, GiveUps: []NoticeGiveUp{
				{ID: 41, Consumer: "legion-go-LEGION-dispatch", Subject: "notifications.dispatch.issue.LEGION-208.issue.updated"}}}},
		{"an update-stalled notice", OutboxKindControllerNotice,
			`{"kind":"update-stalled","reason":"the round has waited 4h0m0s","update":{"round":"pod:9d3f0c7a","pod":"9d3f0c7a","reasons":"image ghcr.io/cybermonklabs/legion-worker:v1, now ghcr.io/cybermonklabs/legion-worker:v2","waiting":[{"claim":"legion-LEGION-208-implementer","role":"implementer","state":"failed"}]}}`,
			ControllerNotice{Kind: UpdateStalledNotice, Reason: "the round has waited 4h0m0s", Update: &UpdateStalled{Round: "pod:9d3f0c7a", Pod: "9d3f0c7a",
				Reasons: "image ghcr.io/cybermonklabs/legion-worker:v1, now ghcr.io/cybermonklabs/legion-worker:v2",
				Waiting: []UpdateWaiting{{Claim: "legion-LEGION-208-implementer", Role: claim.RoleImplementer, State: "failed"}}}}},
		{"a tick", OutboxKindControllerNotice,
			`{"kind":"tick","openCapabilities":["secrets"],"blocked":[{"issue":"LEGION-210","blockedBy":["LEGION-211"]}],"held":[{"issue":"LEGION-212","reason":"design_gate","heldBy":["LEGION-208"]}],"silent":[{"issue":"LEGION-213","role":"tester","lastStep":"2026-11-08T09:00:00Z","silent":"1h0m0s"}]}`,
			ControllerNotice{Kind: TickNotice, OpenCapabilities: []string{"secrets"},
				Blocked: []BlockedIssue{{Issue: "LEGION-210", BlockedBy: []string{"LEGION-211"}}},
				Held:    []HeldIssue{{Issue: "LEGION-212", Reason: HeldDesignGate, HeldBy: []string{"LEGION-208"}}},
				Silent:  []SilentIssue{{Issue: "LEGION-213", Role: claim.RoleTester, LastStep: at, Silent: "1h0m0s"}}}},
		{"a parked decision", OutboxKindControllerNotice,
			`{"kind":"parked-decision","version":3,"reason":"name the rollback path","actor":"user:alice","ref":"dispatch://LEGION-1/spec","decision":"design-changes-requested","root":"LEGION-1"}`,
			ControllerNotice{Kind: ParkedDecisionNotice, Decision: DecisionDesignChangesRequested, Root: "LEGION-1", Ref: "dispatch://LEGION-1/spec",
				Actor: "user:alice", Version: 3, Reason: "name the rollback path"}},
		{"a role message of no kind", OutboxKindRoleMessage,
			`{"role":"legion.sjawhar-legion.LEGION-208.implementer","body":"CI settled at head c0de: \"build\" succeeded, \"unit\" failed"}`,
			RoleMessage{Role: "legion.sjawhar-legion.LEGION-208.implementer", Body: "CI settled at head c0de: \"build\" succeeded, \"unit\" failed"}},
		{"a merge-forward ask", OutboxKindMergeForward,
			`{"role":"implementer","step":"ask","head":"c0de","base":"main","phase":"reviewing","pullRequest":42,"why":"base-rules-unread"}`,
			MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Reviewing, PullRequest: 42, Why: ConflictRulesUnread}},
		{"a merge-forward landed", OutboxKindMergeForward,
			`{"role":"merger","step":"landed","head":"c0de","landed":"d00d","base":"main","phase":"merging","pullRequest":42,"why":"merge-next"}`,
			MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardLanded, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Merging, PullRequest: 42, Why: ConflictMergeNext}},
		{"an update ask", OutboxKindUpdateAsk,
			`{"role":"tester","round":"pod:9d3f0c7a","pod":"9d3f0c7a","text":"The daemon must replace the pod that runs this issue's roles."}`,
			UpdateAsk{Role: claim.RoleTester, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Text: "The daemon must replace the pod that runs this issue's roles."}},
		{"a hook prompt", OutboxKindHookPrompt,
			`{"hook":"back-to-implementing","role":"implementer","from":"reviewing","to":"implementing","count":3,"counts":{"reviewing-implementing":3,"testing-implementing":1},"pullRequest":"sjawhar/legion#42","prompt":"Read the reviewer's thread before you write anything."}`,
			HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Reviewing, To: phase.Implementing, Count: 3,
				Counts: map[string]int{"reviewing-implementing": 3, "testing-implementing": 1}, PullRequest: "sjawhar/legion#42",
				Prompt: "Read the reviewer's thread before you write anything."}},
		{"a merge-queue removal", OutboxKindMergeQueueRemoval, `{"number":42,"head":"c0de","unread":true}`,
			MergeQueueRemoval{Number: 42, Head: "c0de", Unread: true}},
		{"a start with the decisions it kept", OutboxKindSupervise,
			`{"op":"start","tree":"LEGION-208","role":"architect","task":"Write the spec.","kept":{"decisions":[{"ref":"dispatch://LEGION-208/ask/a1","at":"2026-11-08T09:00:00Z","issue":"LEGION-208","decision":"ask-answered","actor":"user:alice"}],"step":"spec","approved":2,"latest":3},"generation":3}`,
			SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 3, Task: "Write the spec.",
				Kept: &KeptDecisions{Decisions: kept.Decisions, KeptApproval: KeptApproval{Step: DesignStepSpec, Approved: 2, Latest: 3}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeOutboxPayload(OutboxRow{ID: 77, Kind: tc.kind, Issue: "LEGION-208", Payload: json.RawMessage(tc.payload)})
			if err != nil {
				t.Fatalf("the row main's daemon queued does not decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				gotJSON, _ := json.Marshal(got)
				t.Fatalf("the row main's daemon queued decodes as %s, want the payload it wrote %s", gotJSON, tc.payload)
			}
		})
	}
}

// A trimmer that switched itself off made its report with no size walk, so the row this daemon
// queues for it holds no sizesAt, where main's release wrote the zero time there. Both rows decode
// into the same sweep, which states no time of a walk.
func TestAVolumeRowOfNoWalkHoldsNoWalkTimeAndMainsRowStillDecodes(t *testing.T) {
	off := &VolumeNearFull{PodUID: "pod-uid", Trimmer: "trim-1", Episode: 1, RaisedBy: "first", UsedBytes: 45 << 30, FreeBytes: 1 << 30,
		StopReason: "the trimmer switched itself off after 3 panics in a row of sweeps", Sizes: []VolumeSize{},
		Faults: []string{"panic: a", "panic: b", "panic: c"}, SwitchedOff: true}
	notice, err := NoticeOf(texts.VolumeNearFullRecord{NoticeHead: texts.NoticeHead{Kind: texts.VolumeNearFull, Issue: "LEGION-208"}, Volume: off})
	if err != nil {
		t.Fatalf("NoticeOf the switched-off trimmer's sweep: %v", err)
	}
	row, err := NewOutboxRow("LEGION-208", notice, time.Now())
	if err != nil {
		t.Fatalf("queue the switched-off trimmer's notice: %v", err)
	}
	if strings.Contains(string(row.Payload), "sizesAt") {
		t.Errorf("the row of a sweep no walk measured states a time of a walk: %s", row.Payload)
	}
	mains := `{"kind":"volume-near-full","volume":{"podUid":"pod-uid","trimmer":"trim-1","episode":1,"raisedBy":"first",` +
		`"usedBytes":48318382080,"freeBytes":1073741824,"freedBytes":0,"stopReason":"the trimmer switched itself off after 3 panics in a row of sweeps",` +
		`"sizes":[],"sizesAt":"0001-01-01T00:00:00Z","faults":["panic: a","panic: b","panic: c"],"switchedOff":true}}`
	for _, tc := range []struct {
		name    string
		payload json.RawMessage
	}{{"this daemon's row", row.Payload}, {"main's row", json.RawMessage(mains)}} {
		got, err := DecodeOutboxPayload(OutboxRow{ID: 77, Kind: OutboxKindNotice, Issue: "LEGION-208", Payload: tc.payload})
		if err != nil {
			t.Fatalf("%s does not decode: %v", tc.name, err)
		}
		decoded, ok := got.(Notice)
		if !ok || decoded.Kind != texts.VolumeNearFull || !reflect.DeepEqual(decoded.Volume, off) {
			gotJSON, _ := json.Marshal(got)
			t.Errorf("%s decodes as %s, want the switched-off trimmer's sweep with no time of a walk", tc.name, gotJSON)
		}
	}
}

// recordSampleAt is the time every filled sample's times count from.
var recordSampleAt = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// filledRecord is a pointer to a record of entry's kind with every field set (fillValue), its kind
// the entry's, its issue LEGION-208, and, where it declares the words key, each words path of it
// named there with an author and a time.
func filledRecord(entry texts.Entry) any {
	rec := reflect.New(reflect.TypeOf(entry.Record))
	count := 0
	fillValue(rec.Elem(), "", &count)
	if kind := rec.Elem().FieldByName("Kind"); kind.IsValid() {
		kind.Set(reflect.ValueOf(entry.Kind))
	}
	if issue := rec.Elem().FieldByName("Issue"); issue.IsValid() {
		issue.SetString("LEGION-208")
	}
	if words := rec.Elem().FieldByName("Words"); words.IsValid() && words.Kind() == reflect.Map {
		meta := map[string]texts.WordsMeta{}
		for _, path := range texts.WordsPaths(rec.Interface()) {
			meta[path] = texts.WordsMeta{By: "user:alice", At: recordSampleAt}
		}
		words.Set(reflect.ValueOf(meta))
	}
	return rec.Interface()
}

// fillValue sets every field of v, as the samples of texts' own tests do: a string names its
// field and its place, a bool is true, a number counts, a time is a minute apart per field, a
// pointer points at a filled value and a list holds two filled items. A map is left out.
func fillValue(v reflect.Value, name string, count *int) {
	*count++
	n := *count
	if v.Type() == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(recordSampleAt.Add(time.Duration(n) * time.Minute)))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprintf("%s-%d", strings.ToLower(name), n))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(n))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(n))
	case reflect.Pointer:
		target := reflect.New(v.Type().Elem())
		fillValue(target.Elem(), name, count)
		v.Set(target)
	case reflect.Slice:
		items := reflect.MakeSlice(v.Type(), 2, 2)
		for i := range items.Len() {
			fillValue(items.Index(i), fmt.Sprintf("%s%d", name, i), count)
		}
		v.Set(items)
	case reflect.Struct:
		for i := range v.NumField() {
			if field := v.Type().Field(i); field.IsExported() && field.Type.Kind() != reflect.Map {
				fillValue(v.Field(i), field.Name, count)
			}
		}
	}
}

// topKeys is the keys of the JSON object data, in the order data holds them.
func topKeys(t *testing.T, data []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if open, err := decoder.Token(); err != nil || open != json.Delim('{') {
		t.Fatalf("%s is no JSON object: %v", data, err)
	}
	var keys []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatalf("read a key of %s: %v", data, err)
		}
		keys = append(keys, key.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("read the value of %s in %s: %v", key, data, err)
		}
	}
	return keys
}

// Notice holds every key each notice and controller-notice record of the list declares, with
// that record's JSON shape: the record of every such kind, with every field set, converts with
// NoticeOf into a notice of its kind whose JSON holds the record's keys with the record's values.
// NoticeKeys names those keys in the record's own order, and the notice opens with its kind, its
// role and its phase where the record has them, then its issue. A kind of any other form has no
// notice keys.
func TestEveryNoticeRecordOnTheListConvertsToANotice(t *testing.T) {
	for _, entry := range texts.Entries() {
		if entry.Form != texts.Notice && entry.Form != texts.ControllerNotice {
			if keys := NoticeKeys(entry.Kind); keys != nil {
				t.Errorf("NoticeKeys of the %s %s = %v, want none", entry.Form, entry.Kind, keys)
			}
			continue
		}
		t.Run(string(entry.Kind), func(t *testing.T) {
			rec := filledRecord(entry)
			notice, err := NoticeOf(rec)
			if err != nil {
				t.Fatalf("NoticeOf: %v", err)
			}
			if notice.Kind != entry.Kind {
				t.Fatalf("the notice's kind is %q, want the record's %q", notice.Kind, entry.Kind)
			}
			recordJSON, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			noticeJSON, err := json.Marshal(notice)
			if err != nil {
				t.Fatal(err)
			}
			var fromRecord, fromNotice map[string]any
			if err := json.Unmarshal(recordJSON, &fromRecord); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(noticeJSON, &fromNotice); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fromNotice, fromRecord) {
				t.Fatalf("the notice of the record is\n%s\nwant the record's keys and values\n%s", noticeJSON, recordJSON)
			}
			keys := NoticeKeys(entry.Kind)
			if got := topKeys(t, recordJSON); !slices.Equal(keys, got) {
				t.Fatalf("NoticeKeys = %v, want the keys the record marshals, in its order: %v", keys, got)
			}
			opening := []string{"kind"}
			for _, key := range []string{"role", "phase"} {
				if slices.Contains(keys, key) {
					opening = append(opening, key)
				}
			}
			opening = append(opening, "issue")
			if got := topKeys(t, noticeJSON); !slices.Equal(got[:len(opening)], opening) {
				t.Fatalf("the notice opens with %v, want %v", got[:len(opening)], opening)
			}
		})
	}
}

// A count of zero and an empty list are values a notice record can hold — no launch failure
// charged, no conflict round, no kind of work reported — and Notice leaves each of them out of
// the wire. The record leaves them out too, so the keys its JSON holds are the keys the notice
// carries, and a reader of the record's declaration learns that the key may be absent.
func TestARecordLeavesOutTheEmptyValuesItsNoticeLeavesOut(t *testing.T) {
	head := func(kind texts.Kind) texts.NoticeHead { return texts.NoticeHead{Kind: kind, Issue: "LEGION-208"} }
	for _, rec := range []any{
		texts.WorkerStalledRecord{NoticeHead: head(texts.WorkerStalled), LastStep: recordSampleAt, SilentFor: "3h0m0s",
			End: "stopped", LaunchFailures: 0},
		texts.WorkerBackgroundStalledRecord{NoticeHead: head(texts.WorkerBackgroundStalled), ReportedAt: recordSampleAt,
			StoodFor: "2h0m0s", Work: []string{}},
		texts.PRBlockedRecord{NoticeHead: head(texts.PRBlocked), Cap: 3, Counted: 3, ConflictRounds: 0},
	} {
		notice, err := NoticeOf(rec)
		if err != nil {
			t.Fatalf("NoticeOf(%T): %v", rec, err)
		}
		recordJSON, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal %T: %v", rec, err)
		}
		noticeJSON, err := json.Marshal(notice)
		if err != nil {
			t.Fatalf("marshal the notice of %T: %v", rec, err)
		}
		got, want := topKeys(t, noticeJSON), topKeys(t, recordJSON)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("the notice of %T carries the keys %v, want the record's %v", rec, got, want)
		}
	}
}

// NoticeOf converts only the record of a notice kind, of the type its kind's entry names.
func TestNoticeOfRefusesARecordThatIsNoNotice(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rec    any
		reason string
	}{
		{"a role message's record", texts.CISettledRecord{RoleHead: texts.RoleHead{Kind: texts.CISettled, Issue: "LEGION-208"}}, "not a notice"},
		{"a task's record", texts.PhaseResumeRecord{TaskHead: texts.TaskHead{Kind: texts.PhaseResume, Issue: "LEGION-208"}}, "not a notice"},
		{"a record of another kind's type", texts.PhaseFinishedRecord{NoticeHead: texts.NoticeHead{Kind: texts.Held, Issue: "LEGION-208"}}, "not a texts.PhaseFinishedRecord"},
		{"a record of a kind off the list", texts.PhaseFinishedRecord{NoticeHead: texts.NoticeHead{Kind: "phase-started", Issue: "LEGION-208"}}, "not on the list"},
		{"a record that is no object", "phase-finished", "no object of fields"},
		{"no record", nil, "not on the list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NoticeOf(tc.rec); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("NoticeOf = %v, want it refused with %q", err, tc.reason)
			}
		})
	}
}

// The nested shapes record keeps as its own types, so that a step stays a DesignStep, a decision
// a DecisionKind and a held reason a HeldReason, are the shapes the records of the list declare:
// each, filled in every field, marshals exactly as the record's type with the same values does,
// key for key and in the same order, and each reads the other's JSON with no unknown key. Empty,
// both leave out the same keys.
func TestTheShapesRecordKeepsAreTheShapesTheRecordsDeclare(t *testing.T) {
	for _, pair := range []struct{ ours, theirs reflect.Type }{
		{reflect.TypeFor[CatchUp](), reflect.TypeFor[texts.CatchUpFacts]()},
		{reflect.TypeFor[CatchUpGate](), reflect.TypeFor[texts.CatchUpGate]()},
		{reflect.TypeFor[KeptDecisions](), reflect.TypeFor[texts.KeptFacts]()},
		{reflect.TypeFor[KeptApproval](), reflect.TypeFor[texts.KeptApproval]()},
		{reflect.TypeFor[DecisionMark](), reflect.TypeFor[texts.DecisionMark]()},
		{reflect.TypeFor[HeldIssue](), reflect.TypeFor[texts.HeldIssue]()},
	} {
		t.Run(pair.ours.Name(), func(t *testing.T) {
			for _, way := range [][2]reflect.Type{{pair.theirs, pair.ours}, {pair.ours, pair.theirs}} {
				from := reflect.New(way[0])
				count := 0
				fillValue(from.Elem(), "", &count)
				encoded, err := json.Marshal(from.Interface())
				if err != nil {
					t.Fatal(err)
				}
				into := reflect.New(way[1])
				if err := decodeStrictly(encoded, into.Interface()); err != nil {
					t.Fatalf("a %s does not read the JSON of a %s: %v", way[1], way[0], err)
				}
				again, err := json.Marshal(into.Interface())
				if err != nil {
					t.Fatal(err)
				}
				if string(again) != string(encoded) {
					t.Fatalf("a %s marshals as\n%s\nand the %s it reads as\n%s", way[0], encoded, way[1], again)
				}
			}
			ours, err := json.Marshal(reflect.New(pair.ours).Interface())
			if err != nil {
				t.Fatalf("marshal an empty %s: %v", pair.ours, err)
			}
			theirs, err := json.Marshal(reflect.New(pair.theirs).Interface())
			if err != nil {
				t.Fatalf("marshal an empty %s: %v", pair.theirs, err)
			}
			if string(ours) != string(theirs) {
				t.Fatalf("an empty %s marshals as %s, and an empty %s as %s", pair.ours, ours, pair.theirs, theirs)
			}
		})
	}
}

// A role message of a kind carries its kind's record as its fields, at the urgency its builder
// gave it: every role-message record of the list that has a kind, with every field set, is a row
// NewOutboxRow takes, and the row's record is the record it was built from, marshalled in that
// record's own order. The hook prompt's record names no kind, since it is published as its text
// from a row of its own (HookPrompt), and no role message carries it. A record of another form,
// or of a type that is not its kind's, is no role message.
func TestARoleMessageOfAKindCarriesItsRecord(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	for _, entry := range texts.Entries() {
		if entry.Form != texts.RoleMessage {
			continue
		}
		t.Run(string(entry.Kind), func(t *testing.T) {
			rec := filledRecord(entry)
			message, err := NewRoleMessage("legion.sjawhar-legion.LEGION-208.implementer", rec, texts.Low)
			if entry.Kind == texts.HookPrompt {
				if err == nil {
					t.Fatalf("a role message carries the hook prompt's record, which names no kind")
				}
				return
			}
			if err != nil {
				t.Fatalf("NewRoleMessage: %v", err)
			}
			if message.Kind != entry.Kind || message.Urgency != texts.Low || message.Body != "" {
				t.Fatalf("the role message is %#v, want kind %s at low with no body", message, entry.Kind)
			}
			row, err := NewOutboxRow("LEGION-208", message, now)
			if err != nil {
				t.Fatalf("the role message is refused: %v", err)
			}
			decoded, err := DecodeOutboxPayload(row)
			if err != nil {
				t.Fatalf("the role message's row does not decode: %v", err)
			}
			back, err := decoded.(RoleMessage).Record()
			if err != nil {
				t.Fatalf("the row's record: %v", err)
			}
			if !reflect.DeepEqual(back, rec) {
				t.Fatalf("the row's record is %#v, want %#v", back, rec)
			}
			published, err := json.Marshal(back)
			if err != nil {
				t.Fatalf("marshal the row's record: %v", err)
			}
			built, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal the record: %v", err)
			}
			if string(published) != string(built) {
				t.Fatalf("the row's record marshals as %s, want the record's own %s", published, built)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		rec    any
		reason string
	}{
		{"a notice's record", texts.ChecksRedRecord{NoticeHead: texts.NoticeHead{Kind: texts.ChecksRed, Issue: "LEGION-208"}}, "not a role message"},
		{"a record of another kind's type", texts.CISettledRecord{RoleHead: texts.RoleHead{Kind: texts.GateMoved, Issue: "LEGION-208"}}, "not a texts.CISettledRecord"},
	} {
		if _, err := NewRoleMessage("legion.sjawhar-legion.LEGION-208.implementer", tc.rec, texts.Med); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("NewRoleMessage of %s = %v, want it refused with %q", tc.name, err, tc.reason)
		}
	}
}

// The role-message rows of their own outbox kinds build their kind's record from the row's
// fields: an update round's ask with the round's drift and the facts every ask gives, a removal
// from the merge queue with the pull request in the repository the record holds it under, and,
// read or unread, that the event names no reason and that the merger reads GitHub, and each message
// of a merge forward as its step's kind, the base left out where the row names none, the ask
// stating the conflict and the issue branch it pushes to.
func TestTheRoleMessageRowsBuildTheirKindsRecords(t *testing.T) {
	ends := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		message MergeForwardMessage
		want    any
	}{
		{MergeForwardMessage{Role: claim.RoleImplementer, Step: MergeForwardAsk, Head: "c0de", Base: "main", Phase: phase.Reviewing, PullRequest: 42, Why: ConflictCheckNotGreen},
			texts.MergeForwardAskRecord{RoleHead: texts.RoleHead{Kind: texts.MergeForwardAsk, Issue: "LEGION-208"}, PullRequest: "sjawhar/legion#42",
				Head: "c0de", ConflictsWith: "main", HeadConflictsWithBase: true, ConflictReads: texts.ConflictReadsConfirmed, Phase: phase.Reviewing,
				Why: string(ConflictCheckNotGreen), ChecksOnHead: texts.ChecksOnHeadNone, PushTo: claim.IssueBranch("LEGION-208")}},
		{MergeForwardMessage{Role: claim.RoleMerger, Step: MergeForwardHold, Head: "c0de", Phase: phase.Merging, PullRequest: 42, Why: ConflictMergeNext},
			texts.MergeForwardHoldRecord{RoleHead: texts.RoleHead{Kind: texts.MergeForwardHold, Issue: "LEGION-208"}, PullRequest: "sjawhar/legion#42",
				Head: "c0de", Phase: phase.Merging, Why: string(ConflictMergeNext), Asked: claim.RoleImplementer}},
		{MergeForwardMessage{Role: claim.RoleTester, Step: MergeForwardLanded, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Testing, PullRequest: 42, Why: ConflictRulesUnread},
			texts.MergeForwardLandedRecord{RoleHead: texts.RoleHead{Kind: texts.MergeForwardLanded, Issue: "LEGION-208"}, PullRequest: "sjawhar/legion#42",
				Head: "c0de", Landed: "d00d", ConflictsWith: "main", Phase: phase.Testing}},
		{MergeForwardMessage{Role: claim.RoleReviewer, Step: MergeForwardReview, Head: "c0de", Landed: "d00d", Base: "main", Phase: phase.Retro, PullRequest: 42},
			texts.MergeForwardReviewRecord{RoleHead: texts.RoleHead{Kind: texts.MergeForwardReview, Issue: "LEGION-208"}, PullRequest: "sjawhar/legion#42",
				Head: "c0de", Landed: "d00d", ConflictsWith: "main", Phase: phase.Retro, ReviewAsked: texts.MergeForwardReviewAsked}},
	} {
		if got := tc.message.Record("LEGION-208", PullRequest{Repo: "sjawhar/legion"}); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("the merge-forward %s's record is %+v, want %+v", tc.message.Step, got, tc.want)
		}
	}
	ask := UpdateAsk{Role: claim.RoleTester, Round: "pod:9d3f0c7a", Pod: "9d3f0c7a", Images: []string{"3f6a0b2c9d1e"},
		Settings: []string{"LEGION_MODEL"}, Reasons: "image a, now b", EndsAt: ends}
	want := texts.NewUpdateAsk("LEGION-208", "pod:9d3f0c7a", "9d3f0c7a", []string{"3f6a0b2c9d1e"}, []string{"LEGION_MODEL"}, ends)
	want.Reasons = "image a, now b"
	if got := ask.Record("LEGION-208"); !reflect.DeepEqual(got, want) {
		t.Errorf("the update ask's record is %+v, want %+v", got, want)
	}
	removal := MergeQueueRemoval{Number: 42, Head: "c0de", Unread: true}
	if got, want := removal.Record("LEGION-208", "sjawhar/legion"), (texts.MergeQueueRemovalRecord{
		RoleHead: texts.RoleHead{Kind: texts.MergeQueueRemoval, Issue: "LEGION-208"}, PullRequest: "sjawhar/legion#42", Head: "c0de", Unread: true,
		EventReason: texts.QueueEventReasonNone, ReadOn: texts.QueueReadOnGitHub}); !reflect.DeepEqual(got, want) {
		t.Errorf("the merge-queue removal's record is %+v, want %+v", got, want)
	}
}

// R1: above the bound a text carries "the first part, the count of the rest (or that more
// follows, where the event did not carry the whole text) and the item's reference"
// (dispatch://INFRA-3686/spec). A person answers a question of a parked tree, and the answer's
// Dispatch event reaches the daemon as a payload_omitted stand-in: the intake keeps its words as
// none with more to follow (ActWords{More: true}). The controller's parked-decision notice must say
// that more follows than it carries, as the kept-decisions task of the same decision does.
func TestAParkedDecisionOfAStandInSaysMoreFollows(t *testing.T) {
	at := time.Date(2026, 11, 14, 22, 44, 10, 0, time.UTC)
	decision := ParkedDecision{Ref: "dispatch://LEGION-208/ask/ask-q", Decision: DecisionAskAnswered, Actor: "user:alice", At: at,
		Words: &ActWords{By: "user:alice", At: at, More: true}}
	notice, err := ControllerNoticeOf(ParkedDecisionNoticeOf("LEGION-208", "LEGION-208", decision, 0))
	if err != nil {
		t.Fatalf("build the parked-decision notice: %v", err)
	}
	payload, err := json.Marshal(Notice(notice))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"more":true`) {
		t.Errorf("the notice of an answer whose event was a payload_omitted stand-in says nothing of the words that did not come: %s", payload)
	}
}

// A row's backoff after a failed run is one second, doubled at each attempt before it, and stops
// doubling at 64 seconds, RetryBackoffMost; a count below zero, which RetryOutbox never writes,
// waits as no attempt does. The daemon's outbox and the store's tests read this one rule.
func TestARowsBackoffDoublesFromOneSecondUpToSixtyFourSeconds(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		-1: time.Second, 0: time.Second, 1: 2 * time.Second, 2: 4 * time.Second, 5: 32 * time.Second,
		6: 64 * time.Second, 7: 64 * time.Second, 40: 64 * time.Second,
	} {
		if got := RetryBackoff(attempts); got != want {
			t.Errorf("RetryBackoff(%d) = %s, want %s", attempts, got, want)
		}
	}
	if RetryBackoffMost != 64*time.Second {
		t.Errorf("RetryBackoffMost = %s, want 64s, the backoff of every count past six", RetryBackoffMost)
	}
}

// The review a landed merge forward asks of the reviewer names the phase the issue stands in
// (`phase`), and the reviewer's prompt (prompts/go/reviewer.md) has it report what it decided to
// the architect and to the worker of that phase, which can act on a request for changes there. A
// reviewer step that ran before a tester step is asked while the issue stands in testing, so it
// tells the tester too. In accepting the architect holds the phase, so it alone is told.
func TestAMergeForwardReviewTellsTheArchitectAndTheWorkerOfThePhase(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "prompts", "go", "reviewer.md"))
	if err != nil {
		t.Fatalf("read the reviewer's prompt: %v", err)
	}
	var rule string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.HasPrefix(line, "A `merge-forward-review` message") {
			rule = line
		}
	}
	if !strings.Contains(rule, "Then tell the architect what you decided") {
		t.Fatalf("the reviewer's prompt for a merge-forward-review does not tell it to report its decision to the architect: %q", rule)
	}
	pr := PullRequest{Repo: "sjawhar/legion"}
	for at, worker := range map[phase.Phase]claim.Role{
		phase.Testing:   claim.RoleTester,
		phase.Accepting: "",
		phase.Retro:     claim.RoleImplementer,
		phase.Merging:   claim.RoleMerger,
	} {
		review := MergeForwardMessage{Role: claim.RoleReviewer, Step: MergeForwardReview, Head: "c0de", Landed: "d00d", Base: "main",
			Phase: at, PullRequest: 42}
		if got, ok := review.Record("LEGION-208", pr).(texts.MergeForwardReviewRecord); !ok || got.Phase != at {
			t.Errorf("the review in %s is %+v, want a merge-forward-review that names the phase %s", at, review.Record("LEGION-208", pr), at)
		}
		tells := "in `" + string(at) + "` the " + string(worker)
		switch {
		case worker != "" && !strings.Contains(rule, tells):
			t.Errorf("the reviewer's prompt for a merge-forward-review does not say %q: %q", tells, rule)
		case worker == "" && strings.Contains(rule, "in `"+string(at)+"` the "):
			t.Errorf("the reviewer's prompt tells a worker of %s, which the architect holds: %q", at, rule)
		}
	}
}
