package replay

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// capturedDir holds samples of the daemon's, Dispatch's and GitHub's output, read once from the
// real systems and neutralized; its CAPTURED.md says how each file was captured. log/ holds one
// daemon line per file and events/ one Dispatch event per file, each a JSON array of one; github/
// holds a page of GitHub's commit listing as GitHub served it.
const capturedDir = "testdata/captured"

// capturedBranch is the base branch whose commit listing github/ holds a page of (the sha of the
// captured request).
const capturedBranch = "main"

// capturedEntry is one entry of a captured log file: a Cloud Logging entry cut to what the
// repository may hold, the time the log store took the line in and the daemon's line itself.
type capturedEntry struct {
	Timestamp   string          `json:"timestamp"`
	JSONPayload json.RawMessage `json:"jsonPayload"`
}

// The trace is built from the daemon's log lines, Dispatch's events and GitHub's commit pages
// (spec "The inputs"), so its readers are held to real samples of each rather than to shapes this
// package wrote itself: one of every line kind and event type the trace reads, captured from the
// systems that wrote them and neutralized (capturedDir). Each file is read with the trace's
// readers — ParseLogLine for a daemon line, ParseDispatchEvent for an event, ParseMerges for a
// commit page — and the reading is held to the one written out by hand from the file: a line's
// type, msg and time, its issue, tree, phases and trigger, the slots in use and the cap, the issue
// and role a supervise: line's claim token names, the issues an issue waits on, and the pull
// request and the issues a merged issue's close waits for; an event's type, actor, the claim its
// actor's session title names, and time, and the status, blocked_by, parent, rank and Dispatch
// claim it shows;
// each merge's commit and time. It fails for a file a reader refuses, drops (a line read as no kind
// the trace reads, an event read without its type's part) or misreads, for a captured file this
// test holds no reading of, and for a reading of a file that is not there. Window two's ready line
// moves an issue into awaiting_merge, a phase today's workflow lacks, and is read as written, for
// the trace to fold.
// A line of an older shape holds, for each member it lacks, the value ParseLogLine gives it: the
// spec as the step, no changes request and no gated steps for the older gate registrations, and
// the unrecorded cause for the failed claim with no cause.
func TestTheTraceReadsEveryCapturedShape(t *testing.T) {
	t.Parallel()
	lines, events, merges := capturedLineReadings(t), capturedEventReadings(t), capturedMergeReadings(t)
	var read []string
	for name := range lines {
		read = append(read, "log/"+name)
	}
	for name := range events {
		read = append(read, "events/"+name)
	}
	for name := range merges {
		read = append(read, "github/"+name)
	}
	captured := capturedFiles(t)
	for _, name := range captured {
		if !slices.Contains(read, name) {
			t.Errorf("%s/%s is captured, and this test holds no reading of it", capturedDir, name)
		}
	}
	for _, name := range read {
		if !slices.Contains(captured, name) {
			t.Errorf("this test holds a reading of %s/%s, which is not captured", capturedDir, name)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(lines)) {
		want := lines[name]
		t.Run("log/"+name, func(t *testing.T) {
			entry := capturedOne[capturedEntry](t, "log/"+name)
			got, err := ParseLogLine(entry.JSONPayload)
			if err != nil {
				t.Fatalf("ParseLogLine refuses the line: %v", err)
			}
			if got == nil {
				t.Fatalf("ParseLogLine drops the line, which the trace reads as %s", describeReading(want))
			}
			if !sameReading(got, want) {
				t.Errorf("ParseLogLine reads the line as\n  %s\nwant\n  %s", describeReading(got), describeReading(want))
			}
		})
	}
	for _, name := range slices.Sorted(maps.Keys(events)) {
		want := events[name]
		t.Run("events/"+name, func(t *testing.T) {
			got, err := ParseDispatchEvent(capturedOne[json.RawMessage](t, "events/"+name))
			if err != nil {
				t.Fatalf("ParseDispatchEvent refuses the event: %v", err)
			}
			if !sameReading(got, want) {
				t.Errorf("ParseDispatchEvent reads the event as\n  %s\nwant\n  %s", describeReading(got), describeReading(want))
			}
		})
	}
	for _, name := range slices.Sorted(maps.Keys(merges)) {
		want := merges[name]
		t.Run("github/"+name, func(t *testing.T) {
			got, _, err := ParseMerges(capturedBranch, capturedFile(t, "github/"+name))
			if err != nil {
				t.Fatalf("ParseMerges refuses the page: %v", err)
			}
			if !sameReading(got, want) {
				t.Errorf("ParseMerges reads the page as\n  %s\nwant\n  %s", describeReading(got), describeReading(want))
			}
		})
	}
}

// The trace reads, for each claim, the sessions the record shows running it: each session actor
// whose title names a role's claim of an issue, from its first event in the window, whichever
// issue that event is of, in the order of their first events, a fresh session of the same claim
// after the one it replaced. It reads none from a person, from the daemon's own session, from the
// controller's session, whose title names its project, or from a session outside Legion. At a
// moment, a claim runs the latest of its sessions whose first event is at or before it, and before
// the first, the first; a claim the record shows no session of runs none.
func TestTheTraceReadsTheSessionsThatRanEachClaim(t *testing.T) {
	t.Parallel()
	b := newWindowBuilder(t, clock(9, 0), clock(15, 0))
	b.boot(clock(9, 0))
	first := recordedSession("ACME-1", claim.RoleArchitect, "00000000-0000-4000-8000-0000000000c1")
	fresh := recordedSession("ACME-1", claim.RoleArchitect, "00000000-0000-4000-8000-0000000000c2")
	controller := eventActor{Kind: "session", ID: "00000000-0000-4000-8000-0000000000c3",
		Origin: &eventOrigin{Host: "omp", SessionTitle: "Legion controller · ACME"}}
	outside := eventActor{Kind: "session", ID: "00000000-0000-4000-8000-0000000000c4", Origin: &eventOrigin{Host: "omp", SessionTitle: "session 1"}}
	b.filedBy(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion}, controller)
	b.rootPromoted(clock(9, 10), "ACME-1")
	b.claimedBy(clock(9, 12), "ACME-1", first)
	b.filedBy(clock(9, 30), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo",
		Rank: "a1"}, first)
	b.changed(clock(10, 0), "ACME-2", outside, func(issue *builtIssue) { issue.Title = "Write the header row" })
	b.edit(clock(10, 30), "ACME-2", func(issue *builtIssue) { issue.Rank = "a2" })
	b.claimedBy(clock(11, 0), "ACME-1", fresh)
	trace, err := BuildTrace(b.build())
	if err != nil {
		t.Fatal(err)
	}
	want := []RecordedSession{{ID: first.ID, Role: claim.RoleArchitect, First: clock(9, 12)}, {ID: fresh.ID, Role: claim.RoleArchitect, First: clock(11, 0)}}
	if got := trace.Issues["ACME-1"].Sessions; !sameReading(got, want) {
		t.Errorf("the trace reads the sessions of ACME-1's claims as %s, want %s", describeReading(got), describeReading(want))
	}
	if got := trace.Issues["ACME-2"].Sessions; len(got) != 0 {
		t.Errorf("the trace reads the sessions of ACME-2's claims as %s, want none: the architect's filing of it is of ACME-1's claim",
			describeReading(got))
	}
	for _, c := range []struct {
		role claim.Role
		at   time.Time
		want string
	}{
		{claim.RoleArchitect, clock(9, 0), first.ID},
		{claim.RoleArchitect, clock(9, 12), first.ID},
		{claim.RoleArchitect, clock(10, 59), first.ID},
		{claim.RoleArchitect, clock(11, 0), fresh.ID},
		{claim.RoleArchitect, clock(14, 0), fresh.ID},
		{claim.RolePlanner, clock(10, 0), ""},
	} {
		if got := trace.Issues["ACME-1"].sessionAt(c.role, c.at); got != c.want {
			t.Errorf("ACME-1's %s claim runs the session %q at %s, want %q", c.role, got, c.at.Format("15:04"), c.want)
		}
	}
}

// capturedLineReadings is what the trace reads from each captured daemon line, by its file under
// log/, written out by hand from the file's jsonPayload: the line's type and every field of it.
func capturedLineReadings(t *testing.T) map[string]LogLine {
	t.Helper()
	head := func(msg, stamp string) LineHead { return LineHead{Msg: msg, Time: capturedTime(t, stamp)} }
	moved := func(stamp string) LineHead { return head("workflow: phase changed", stamp) }
	claimOf := func(token, issue string, role claim.Role) ClaimRef {
		return ClaimRef{Token: claim.Token(token), Issue: issue, Role: role}
	}
	return map[string]LogLine{
		"phase-changed-gate-opened.json": PhaseChanged{LineHead: moved("2026-11-21T00:19:37.742600039Z"),
			Issue: "ACME-39", Tree: "ACME-32", From: phase.Admitted, To: phase.Planning, Trigger: workflow.TriggerGateOpened, Status: "in_progress"},
		"phase-changed-planner-completed.json": PhaseChanged{LineHead: moved("2026-11-21T03:45:12.790505154Z"),
			Issue: "ACME-29", Tree: "ACME-29", From: phase.Planning, To: phase.Implementing, Trigger: workflow.TriggerPlannerCompleted, Status: "in_progress"},
		"phase-changed-implementation-ready.json": PhaseChanged{LineHead: moved("2026-11-21T05:06:10.041666133Z"),
			Issue: "ACME-28", Tree: "ACME-26", From: phase.Implementing, To: phase.Testing, Trigger: workflow.TriggerImplementationReady, Status: "testing"},
		"phase-changed-tester-passed.json": PhaseChanged{LineHead: moved("2026-11-21T05:01:51.066252638Z"),
			Issue: "ACME-48", Tree: "ACME-30", From: phase.Testing, To: phase.Reviewing, Trigger: workflow.TriggerTesterPassed, Status: "needs_review"},
		"phase-changed-tester-failed.json": PhaseChanged{LineHead: moved("2026-11-21T04:52:53.432820309Z"),
			Issue: "ACME-46", Tree: "ACME-10", From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerTesterFailed, Status: "in_progress"},
		"phase-changed-review-approved.json": PhaseChanged{LineHead: moved("2026-11-21T04:55:47.676168178Z"),
			Issue: "ACME-8", Tree: "ACME-8", From: phase.Reviewing, To: phase.Accepting, Trigger: workflow.TriggerReviewApproved, Status: "needs_review"},
		"phase-changed-review-rejected.json": PhaseChanged{LineHead: moved("2026-11-21T05:06:12.912626559Z"),
			Issue: "ACME-35", Tree: "ACME-12", From: phase.Reviewing, To: phase.Implementing, Trigger: workflow.TriggerReviewRejected, Status: "in_progress"},
		"phase-changed-retro-completed.json": PhaseChanged{LineHead: moved("2026-11-21T04:43:39.027997278Z"),
			Issue: "ACME-37", Tree: "ACME-37", From: phase.Retro, To: phase.Merging, Trigger: workflow.TriggerRetroCompleted, Status: "retro"},
		"phase-changed-pull-request-merged.json": PhaseChanged{LineHead: moved("2026-11-21T03:16:06.296663903Z"),
			Issue: "ACME-47", Tree: "ACME-43", From: phase.Merging, To: phase.Done, Trigger: workflow.TriggerPullRequestMerged, Status: "done"},
		"phase-changed-sign-off.json": PhaseChanged{LineHead: moved("2026-11-21T04:33:31.533872396Z"),
			Issue: "ACME-37", Tree: "ACME-37", From: phase.Accepting, To: phase.Retro, Trigger: workflow.TriggerSignOff, Status: "retro"},
		"phase-changed-backward.json": PhaseChanged{LineHead: moved("2026-11-21T05:01:32.767426961Z"),
			Issue: "ACME-50", Tree: "ACME-22", From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerBackward, Status: "in_progress"},
		"phase-changed-conflict-window-one.json": PhaseChanged{LineHead: moved("2026-11-20T20:47:28.110303594Z"),
			Issue: "ACME-44", Tree: "ACME-33", From: phase.Merging, To: phase.Implementing, Trigger: workflow.TriggerConflict, Status: "in_progress"},
		"phase-changed-ready-window-two.json": PhaseChanged{LineHead: moved("2026-11-16T22:31:54.320717187Z"),
			Issue: "ACME-34", Tree: "ACME-31", From: phase.Merging, To: PhaseAwaitingMerge, Trigger: TriggerReady, Status: "retro"},
		"phase-changed-checks-red-window-two.json": PhaseChanged{LineHead: moved("2026-11-16T22:23:47.448193314Z"),
			Issue: "ACME-36", Tree: "ACME-12", From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerChecksRed, Status: "in_progress"},
		"admission-slot-taken.json": SlotTaken{LineHead: head("admission: slot taken", "2026-11-21T04:55:47.676326318Z"),
			Issue: "ACME-14", Slots: 157, Cap: 157},
		"admission-slot-released.json": SlotReleased{LineHead: head("admission: slot released", "2026-11-21T04:55:47.676285947Z"),
			Issue: "ACME-8", Slots: 156, Cap: 157},
		"admission-waits-on-blockers.json": WaitsOnIssues{LineHead: head("admission waits for what this issue waits on", "2026-11-21T05:08:03.917770662Z"),
			Issue: "ACME-45", Blockers: []string{"ACME-2"}},
		"admission-slot-back-work-came-back.json": SlotBackWorkReturned{LineHead: head("admission gives a slot back to an issue whose work came back", "2026-11-21T04:55:47.564647172Z"),
			Issue: "ACME-14", Phase: phase.Implementing},
		"admission-slot-back-wait-ended.json": SlotBackWaitEnded{LineHead: head("admission gives a slot back to an issue whose wait ended", "2026-11-21T03:20:50.606323131Z"),
			Issue: "ACME-40", Phase: phase.Planning},
		"admission-waits-for-children.json": WaitsForChildren{LineHead: head("admission: an issue waits for the Dispatch children under it", "2026-11-21T00:52:05.903683936Z"),
			Issue: "ACME-56", Tree: "ACME-56", Generation: 1, Children: 5},
		"admission-reconcile-holding.json": ReconcileHolding{LineHead: head("admission reconcile: holding roots for the Dispatch consumer to reach a stream position", "2026-11-21T01:58:06.102382984Z"),
			Count: 2174, Target: 7496674, AckFloor: 7496142},
		"admission-consumer-caught-up.json": ConsumerCaughtUp{LineHead: head("admission: the Dispatch consumer has caught up; releasing held roots", "2026-11-21T02:04:04.400314433Z"),
			Count: 2174, AckFloor: 7496814, Idle: false, Admitted: 2},
		"root-waits-on-person.json": WaitsOnPerson{LineHead: head("workflow: root waits on a person and keeps its admission slot", "2026-11-21T02:22:11.47949729Z"),
			Issue: "ACME-13", Tree: "ACME-13", Reason: "spec version 20 is awaiting a person's review", SinceSeq: 111},
		"design-gate-opened.json": GateOpened{LineHead: head("workflow: design gate opened", "2026-11-21T03:40:14.32409794Z"),
			Issue: "ACME-11", Artifact: "00000000-0000-4000-8000-000000000002", Event: "approved", Version: 100},
		"design-gate-closed.json": GateClosed{LineHead: head("workflow: design gate closed", "2026-11-21T03:22:07.596160896Z"),
			Issue: "ACME-4", Artifact: "00000000-0000-4000-8000-000000000001", Event: "version", Version: 74},
		"design-gate-registered.json": GateRegistered{LineHead: head("workflow: design gate registered", "2026-11-21T07:59:33.665464267Z"),
			Issue: "ACME-4", Artifact: "00000000-0000-4000-8000-000000000001", Step: record.DesignStepSpec, Version: 78, Open: false,
			ChangesRequested: false, GatedSteps: []record.DesignStep{record.DesignStepSpec}},
		"design-gate-registered-one-step.json": GateRegistered{LineHead: head("workflow: design gate registered", "2026-11-10T12:03:11.417914384Z"),
			Issue: "ACME-5", Artifact: "00000000-0000-4000-8000-000000000002", Step: record.DesignStepSpec, Version: 7, Open: false,
			ChangesRequested: false, GatedSteps: nil},
		"design-gate-registered-before-steps.json": GateRegistered{LineHead: head("workflow: design gate registered", "2026-10-27T13:52:13.728012177Z"),
			Issue: "ACME-2", Artifact: "00000000-0000-4000-8000-000000000001", Step: record.DesignStepSpec, Version: 23, Open: false,
			ChangesRequested: false, GatedSteps: nil},
		"held-phase-released.json": HoldReleased{LineHead: head("workflow: a held phase is released", "2026-11-20T00:10:01.999737953Z"),
			Issue: "ACME-16", Phase: phase.Merging, Reason: "the operator retried this phase's worker"},
		"issue-left-workflow.json": LeftWorkflow{LineHead: head("workflow: issue left the workflow", "2026-11-21T04:17:17.416684718Z"),
			Issue: "ACME-57", Tree: "ACME-3", Status: "backlog"},
		"tree-lingers.json": TreeLingers{LineHead: head("workflow: tree lingers", "2026-11-20T20:38:12.539056207Z"),
			Tree: "ACME-18", Generation: 1, Until: capturedTime(t, "2026-11-23T20:38:12.260483847Z")},
		"linger-closed-tree.json": LingerClosed{LineHead: head("workflow: linger closed the tree", "2026-11-21T04:11:33.472588175Z"),
			Tree: "ACME-5", Generation: 1, Issues: 1},
		"root-joined-live-tree.json": RootJoined{LineHead: head("workflow: a queued root joined a live tree", "2026-11-17T09:11:21.164178694Z"),
			Issue: "ACME-6", Tree: "ACME-8", Generation: 2},
		"supervise-turn-started.json": TurnStarted{LineHead: head("supervise: a turn started", "2026-11-21T05:09:23.248994112Z"),
			Claim: claimOf("legion-acme-acme-6-architect", "ACME-6", claim.RoleArchitect), Generation: 192, Delivery: "outbox:424751"},
		"supervise-turn-ended.json": TurnEnded{LineHead: head("supervise: a turn ended", "2026-11-21T05:09:40.803838475Z"),
			Claim: claimOf("legion-acme-acme-14-architect", "ACME-14", claim.RoleArchitect), Generation: 59},
		"supervise-turn-ended-outcome.json": TurnEnded{LineHead: head("supervise: a turn ended", "2026-11-21T08:06:02.659521763Z"),
			Claim: claimOf("legion-acme-acme-51-merger", "ACME-51", claim.RoleMerger), Generation: 8, Outcome: true},
		"supervise-launched.json": Launched{LineHead: head("supervise: launched", "2026-11-21T05:08:53.046995199Z"),
			Claim: claimOf("legion-acme-acme-51-tester", "ACME-51", claim.RoleTester), Generation: 3, Resumed: true},
		"supervise-suspended.json": Suspended{LineHead: head("supervise: suspended", "2026-11-21T05:08:35.479252159Z"),
			Claim: claimOf("legion-acme-acme-7-architect", "ACME-7", claim.RoleArchitect), By: "released", Reason: "the issue's pod has been idle for the release period"},
		"supervise-claim-failed.json": ClaimFailed{LineHead: head("supervise: claim failed", "2026-11-18T10:37:34.716550893Z"),
			Claim: claimOf("legion-acme-acme-41-implementer", "ACME-41", claim.RoleImplementer), Cause: "launch_refused",
			Why: "launch failures ran out: launch legion-acme-acme-41-implementer: start through its authenticated launcher: the shared public leak check could not be installed: the served check could not be run: waitid: no child processes"},
		"supervise-claim-failed-no-cause.json": ClaimFailed{LineHead: head("supervise: claim failed", "2026-11-16T18:01:58.038220213Z"),
			Claim: claimOf("legion-acme-acme-10-tester", "ACME-10", claim.RoleTester), Cause: string(supervise.CauseUnrecorded),
			Why: "deaths with work outstanding ran out"},
		"supervise-process-died.json": ProcessEnded{LineHead: head("supervise: process died", "2026-11-21T06:18:17.149745489Z"),
			Claim: claimOf("legion-acme-acme-1-architect", "ACME-1", claim.RoleArchitect), By: TurnEndDied,
			DeathCause: "gone: pod legion-acme-acme-1 (uid 00000000-0000-4000-8000-000000000006) Failed: OutOfcpu: Pod was rejected: Node didn't have enough resource: cpu, requested: 2000, used: 7654, capacity: 7910; last lines of architect:\n(the log could not be read: container \"roles\" in pod \"legion-acme-acme-1\" is not available)"},
		"supervise-displaced-uncharged.json": ProcessEnded{LineHead: head("supervise: the process was displaced through no fault of its own, its pod replaced by the daemon or it another runtime's process this daemon takes over; relaunching it uncharged", "2026-11-19T18:00:14.314641023Z"),
			Claim: claimOf("legion-acme-acme-7-architect", "ACME-7", claim.RoleArchitect), By: TurnEndDisplaced},
		"supervise-relaunch-ended-work.json": ProcessEnded{LineHead: head("supervise: the relaunch ended work the agent had running", "2026-11-21T06:01:02.075455815Z"),
			Claim: claimOf("legion-acme-acme-4-merger", "ACME-4", claim.RoleMerger), By: TurnEndLetGo},
		"daemon-first-line-stale-plugin.json": ProcessStart{LineHead: head("daemon: stale-plugin relaunches install", "2026-11-21T01:54:55.881009799Z")},
		"daemon-boot-stage-prompts.json": BootStage{LineHead: head("legion workflow boot stage", "2026-11-21T01:54:55.884463509Z"),
			Stage: "prompts"},
		"daemon-started.json": DaemonStarted{LineHead: head("legion daemon started", "2026-11-21T02:01:27.894781099Z"),
			Project: "ACME", AdmissionCap: 157},
		"daemon-stopping.json": ProcessStop{LineHead: head("legion daemon stopping", "2026-11-21T01:54:46.606831688Z"),
			Project: "ACME"},
		"daemon-stopped.json": ProcessStop{LineHead: head("legion daemon stopped", "2026-11-21T01:54:47.064170219Z"),
			Project: "ACME"},
		"merge-queue-left-nothing-read.json": QueueLeftNothingRead{LineHead: head("workflow: a pull request left the merge queue where its issue does not merge at that head; nothing is read", "2026-11-21T04:45:24.375465813Z"),
			Issue: "ACME-49", PullRequest: 5367, HeadSHA: "aa593efa160f8338f94e0b8dfc358aeabefd2e3c", Why: "the issue stands in done, not merging"},
		"merge-queue-left-read-once.json": QueueLeftReadOnce{LineHead: head("workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once", "2026-11-21T03:19:56.094165169Z"),
			Issue: "ACME-38", PullRequest: 4547, HeadSHA: "c3b2e84ddc994f14b179f99243a9490806e2df1b"},
		"workflow-merge-forward-asked.json": MergeForwardAsked{LineHead: head("workflow: merge forward asked", "2026-11-21T19:27:15.063200483Z"),
			Issue: "ACME-2", Phase: phase.Implementing},
		"workflow-merge-forward-landed.json": MergeForwardLanded{LineHead: head("workflow: the merge forward landed", "2026-11-21T19:34:32.405321293Z"),
			Issue: "ACME-1"},
		"workflow-merged-close-waits.json": MergedCloseWaits{LineHead: head("workflow: a merged issue's close waits for the issues beneath it", "2026-11-21T19:26:26.873257581Z"),
			Issue: "ACME-1", Tree: "ACME-1", PullRequest: 2377, WaitingFor: []string{"ACME-2"}},
	}
}

// capturedEventReadings is what the trace reads from each captured Dispatch event, by its file
// under events/, written out by hand from the event: its envelope, the claim its actor's session
// title names (`Legion <role> · <KEY>`; the controller's title, `Legion controller · ACME`, and a
// session outside Legion's, `session 1`, name none), and the part of its payload its type fills.
func capturedEventReadings(t *testing.T) map[string]DispatchEvent {
	t.Helper()
	person := dispatch.Actor{Kind: "user", ID: "person-1@example.invalid"}
	daemon := dispatch.Actor{Kind: "session", ID: "legion-daemon:ACME"}
	session := func(id string) dispatch.Actor { return dispatch.Actor{Kind: "session", ID: id} }
	runs := func(key string, role claim.Role) *SessionClaim { return &SessionClaim{Issue: key, Role: role} }
	// Each captured rank is a run of z's and a short tail, Dispatch's fractional keys at the end of
	// a long project's order.
	rank := func(zs int, tail string) string { return strings.Repeat("z", zs) + tail }
	const spec = "00000000-0000-4000-8000-000000000009"
	return map[string]DispatchEvent{
		"issue-created.json": {ID: 410494, Seq: 1, Key: "ACME-29", Project: "ACME", Type: "issue.created",
			Actor: session("00000000-0000-4000-8000-000000000011"), At: capturedTime(t, "2026-11-12T02:33:45.794899Z"),
			Issue: &DispatchIssueState{Changed: []string{"title", "status", "rank", "priority", "assignee", "labels", "repo"},
				Status: "triage", Rank: rank(95, "k"), Labels: []string{"legion"}, Priority: record.P2}, ClaimShown: true},
		"issue-updated-status.json": {ID: 411144, Seq: 2, Key: "ACME-29", Project: "ACME", Type: "issue.updated",
			Actor: daemon, At: capturedTime(t, "2026-11-12T02:48:18.305772Z"),
			Issue: &DispatchIssueState{Changed: []string{"status"}, Status: "todo", Rank: rank(95, "k"), Labels: []string{"legion"},
				Priority: record.P2},
			ClaimShown: true},
		"issue-updated-blocked-by.json": {ID: 891022, Seq: 3, Key: "ACME-58", Project: "ACME", Type: "issue.updated",
			Actor: session("00000000-0000-4000-8000-000000000015"), ActorClaim: runs("ACME-15", claim.RoleArchitect),
			At: capturedTime(t, "2026-11-21T01:54:00.399322Z"),
			Issue: &DispatchIssueState{Changed: []string{"components", "blocked_by"}, Status: "triage",
				BlockedBy: []string{"ACME-42", "ACME-52", "ACME-53"}, Parent: "ACME-15", Rank: rank(145, "Z")}, ClaimShown: true},
		"issue-updated-parent.json": {ID: 868069, Seq: 3, Key: "ACME-55", Project: "ACME", Type: "issue.updated",
			Actor: session("00000000-0000-4000-8000-000000000013"), At: capturedTime(t, "2026-11-20T17:44:47.764822Z"),
			Issue: &DispatchIssueState{Changed: []string{"status", "parent", "external_links"}, Status: "in_progress",
				Parent: "ACME-54", Rank: rank(142, "j")}, Claim: "session:00000000-0000-4000-8000-000000000013", ClaimShown: true},
		"issue-updated-rank.json": {ID: 701064, Seq: 11, Key: "ACME-29", Project: "ACME", Type: "issue.updated",
			Actor: session("00000000-0000-4000-8000-000000000012"), At: capturedTime(t, "2026-11-18T00:12:10.182106Z"),
			Issue: &DispatchIssueState{Changed: []string{"rank"}, Status: "todo", Rank: rank(113, "fzzU"), Labels: []string{"legion"},
				Priority: record.P2},
			ClaimShown: true},
		"issue-closed.json": {ID: 502249, Seq: 99, Key: "ACME-27", Project: "ACME", Type: "issue.closed",
			Actor: daemon, At: capturedTime(t, "2026-11-13T21:26:48.67625Z"),
			Issue:      &DispatchIssueState{Changed: []string{"status", "closed_at"}, Status: "done", Parent: "ACME-24", Rank: rank(93, "r")},
			ClaimShown: true},
		"artifact-approved.json": {ID: 886533, Seq: 58, Key: "ACME-29", Project: "ACME", Type: "artifact.approved",
			Actor: person, At: capturedTime(t, "2026-11-21T00:17:12.362358Z"),
			Verdict: &DispatchVerdict{Artifact: spec, Name: "spec.md", Version: 5}},
		"artifact-changes-requested.json": {ID: 880496, Seq: 41, Key: "ACME-29", Project: "ACME", Type: "artifact.changes_requested",
			Actor: person, At: capturedTime(t, "2026-11-20T22:07:57.233851Z"),
			Verdict: &DispatchVerdict{Artifact: spec, Name: "spec.md", Version: 4, Reason: "text 3"}},
		"ask-opened.json": {ID: 877110, Seq: 23, Key: "ACME-29", Project: "ACME", Type: "ask.opened",
			Actor: session("00000000-0000-4000-8000-00000000000c"), ActorClaim: runs("ACME-29", claim.RoleArchitect),
			At:  capturedTime(t, "2026-11-20T20:47:49.424239Z"),
			Ask: &DispatchAsk{ID: "00000000-0000-4000-8000-00000000000e", Kind: "question", State: "open"}},
		"ask-answered.json": {ID: 880495, Seq: 40, Key: "ACME-29", Project: "ACME", Type: "ask.answered",
			Actor: person, At: capturedTime(t, "2026-11-20T22:07:57.233851Z"),
			Ask: &DispatchAsk{ID: "00000000-0000-4000-8000-00000000000a", Kind: "approval", State: "answered", Artifact: spec}},
		"handoff-recorded.json": {ID: 890146, Seq: 70, Key: "ACME-29", Project: "ACME", Type: "handoff.recorded",
			Actor: session("00000000-0000-4000-8000-00000000000f"), ActorClaim: runs("ACME-29", claim.RolePlanner),
			At:      capturedTime(t, "2026-11-21T01:35:18.01727Z"),
			Handoff: &DispatchHandoff{ID: "00000000-0000-4000-8000-000000000010", Phase: "plan", Version: 1}},
	}
}

// capturedMergeReadings is the first-parent commits the trace reads from each captured page of
// capturedBranch's commit listing, by its file under github/, oldest first, written out by hand
// from the page: five commits listed newest first, each the first parent of the one before it, the
// oldest's first parent on no page, each the squash merge of the pull request its subject's
// suffix names.
func capturedMergeReadings(t *testing.T) map[string][]Merge {
	t.Helper()
	merge := func(sha, stamp string, pr int) Merge {
		return Merge{SHA: sha, At: capturedTime(t, stamp), Branch: capturedBranch, PR: pr}
	}
	return map[string][]Merge{
		"commits.json": {
			merge("282a49f20ac5b24d9c30925646c9d0438905fd0e", "2026-11-20T04:56:30Z", 4577),
			merge("6d50ef31bf220fb231e46d9264eddd77ef921078", "2026-11-20T05:15:42Z", 4541),
			merge("0d1bfb2da6a03ee24cd7f9d6a9bd6e2ad56bb79a", "2026-11-20T05:29:02Z", 4552),
			merge("e967604ded2c6bcd03d676ad64e449f7275991c0", "2026-11-20T05:34:11Z", 4551),
			merge("651db8d54c55d5e78e092ece50a047c09108ebd7", "2026-11-20T05:36:33Z", 4516),
		},
	}
}

// capturedFiles is every captured file by its slash-separated path under capturedDir, but the
// directory's CAPTURED.md, which says how they were captured.
func capturedFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(capturedDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, err := filepath.Rel(capturedDir, path)
		if err != nil {
			return err
		}
		if name = filepath.ToSlash(name); name != "CAPTURED.md" {
			files = append(files, name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("list %s: %v", capturedDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("%s holds no captured file", capturedDir)
	}
	return files
}

// capturedFile is the bytes of the captured file name, a slash-separated path under capturedDir.
func capturedFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(capturedDir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read %s/%s: %v", capturedDir, name, err)
	}
	return data
}

// capturedOne is the one entry of the captured file name: a log or an event file holds a JSON
// array of one, the shape CAPTURED.md gives.
func capturedOne[T any](t *testing.T, name string) T {
	t.Helper()
	var entries []T
	if err := json.Unmarshal(capturedFile(t, name), &entries); err != nil {
		t.Fatalf("%s/%s is not a JSON array of entries: %v", capturedDir, name, err)
	}
	if len(entries) != 1 {
		t.Fatalf("%s/%s holds %d entries, want 1", capturedDir, name, len(entries))
	}
	return entries[0]
}

// capturedTime is the time stamp writes in RFC 3339, with its fraction of a second.
func capturedTime(t *testing.T, stamp string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("the time %q of a reading: %v", stamp, err)
	}
	return at
}

// sameReading says whether two readings say the same thing: deeply equal once each is canonical
// (canonicalReading).
func sameReading(got, want any) bool {
	return reflect.DeepEqual(canonicalReading(reflect.ValueOf(got)).Interface(), canonicalReading(reflect.ValueOf(want)).Interface())
}

// canonicalReading is v with every time in UTC and every empty slice nil, through pointers,
// slices and structs, so the way a reader built a value does not tell two readings apart: a
// reader may decode an empty JSON array to an empty slice or to none, and a time in any location.
func canonicalReading(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(canonicalReading(v.Elem()))
		return out
	case reflect.Slice:
		if v.Len() == 0 {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(canonicalReading(v.Index(i)))
		}
		return out
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			return reflect.ValueOf(v.Interface().(time.Time).UTC())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if field := out.Field(i); field.CanSet() {
				field.Set(canonicalReading(v.Field(i)))
			}
		}
		return out
	}
	return v
}

// describeReading is a reading as a failure prints it: its type and its fields as JSON, so a
// reading's nested parts print as values rather than as addresses.
func describeReading(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%T %+v", v, v)
	}
	return fmt.Sprintf("%T %s", v, body)
}
