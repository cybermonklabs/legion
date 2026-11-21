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
// and role a supervise: line's claim token names, and the issues an issue waits on; an event's
// type, actor and time, and the status, blocked_by, parent and rank it shows; each merge's commit
// and time. It fails for a file a reader refuses, drops (a line read as no kind the trace reads, an
// event read without its type's part) or misreads, for a captured file this test holds no reading
// of, and for a reading of a file that is not there. Window two's ready line moves an issue into
// awaiting_merge, a phase today's workflow lacks, and is read as written, for the trace to fold.
func TestTheTraceReadsEveryCapturedShape(t *testing.T) {
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
			got, err := ParseMerges(capturedBranch, capturedFile(t, "github/"+name))
			if err != nil {
				t.Fatalf("ParseMerges refuses the page: %v", err)
			}
			if !sameReading(got, want) {
				t.Errorf("ParseMerges reads the page as\n  %s\nwant\n  %s", describeReading(got), describeReading(want))
			}
		})
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
			Issue: "ACME-21", Tree: "ACME-22", From: phase.Admitted, To: phase.Planning, Trigger: workflow.TriggerGateOpened, Status: "in_progress"},
		"phase-changed-planner-completed.json": PhaseChanged{LineHead: moved("2026-11-21T03:45:12.790505154Z"),
			Issue: "ACME-25", Tree: "ACME-25", From: phase.Planning, To: phase.Implementing, Trigger: workflow.TriggerPlannerCompleted, Status: "in_progress"},
		"phase-changed-implementation-ready.json": PhaseChanged{LineHead: moved("2026-11-21T05:06:10.041666133Z"),
			Issue: "ACME-23", Tree: "ACME-24", From: phase.Implementing, To: phase.Testing, Trigger: workflow.TriggerImplementationReady, Status: "testing"},
		"phase-changed-tester-passed.json": PhaseChanged{LineHead: moved("2026-11-21T05:01:51.066252638Z"),
			Issue: "ACME-34", Tree: "ACME-35", From: phase.Testing, To: phase.Reviewing, Trigger: workflow.TriggerTesterPassed, Status: "needs_review"},
		"phase-changed-tester-failed.json": PhaseChanged{LineHead: moved("2026-11-21T04:52:53.432820309Z"),
			Issue: "ACME-32", Tree: "ACME-33", From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerTesterFailed, Status: "in_progress"},
		"phase-changed-review-approved.json": PhaseChanged{LineHead: moved("2026-11-21T04:55:47.676168178Z"),
			Issue: "ACME-3", Tree: "ACME-3", From: phase.Reviewing, To: phase.Accepting, Trigger: workflow.TriggerReviewApproved, Status: "needs_review"},
		"phase-changed-review-rejected.json": PhaseChanged{LineHead: moved("2026-11-21T05:06:12.912626559Z"),
			Issue: "ACME-31", Tree: "ACME-18", From: phase.Reviewing, To: phase.Implementing, Trigger: workflow.TriggerReviewRejected, Status: "in_progress"},
		"phase-changed-retro-completed.json": PhaseChanged{LineHead: moved("2026-11-21T04:43:39.027997278Z"),
			Issue: "ACME-30", Tree: "ACME-30", From: phase.Retro, To: phase.Merging, Trigger: workflow.TriggerRetroCompleted, Status: "retro"},
		"phase-changed-pull-request-merged.json": PhaseChanged{LineHead: moved("2026-11-21T03:16:06.296663903Z"),
			Issue: "ACME-26", Tree: "ACME-27", From: phase.Merging, To: phase.Done, Trigger: workflow.TriggerPullRequestMerged, Status: "done"},
		"phase-changed-sign-off.json": PhaseChanged{LineHead: moved("2026-11-21T04:33:31.533872396Z"),
			Issue: "ACME-30", Tree: "ACME-30", From: phase.Accepting, To: phase.Retro, Trigger: workflow.TriggerSignOff, Status: "retro"},
		"phase-changed-backward.json": PhaseChanged{LineHead: moved("2026-11-21T05:01:32.767426961Z"),
			Issue: "ACME-15", Tree: "ACME-16", From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerBackward, Status: "in_progress"},
		"phase-changed-conflict-window-one.json": PhaseChanged{LineHead: moved("2026-11-20T20:47:28.110303594Z"),
			Issue: "ACME-19", Tree: "ACME-20", From: phase.Merging, To: phase.Implementing, Trigger: workflow.TriggerConflict, Status: "in_progress"},
		"phase-changed-ready-window-two.json": PhaseChanged{LineHead: moved("2026-11-16T22:31:54.320717187Z"),
			Issue: "ACME-28", Tree: "ACME-29", From: phase.Merging, To: PhaseAwaitingMerge, Trigger: TriggerReady, Status: "retro"},
		"phase-changed-checks-red-window-two.json": PhaseChanged{LineHead: moved("2026-11-16T22:23:47.448193314Z"),
			Issue: "ACME-17", Tree: "ACME-18", From: phase.Testing, To: phase.Implementing, Trigger: workflow.TriggerChecksRed, Status: "in_progress"},
		"admission-slot-taken.json": SlotTaken{LineHead: head("admission: slot taken", "2026-11-21T04:55:47.676326318Z"),
			Issue: "ACME-2", Slots: 157, Cap: 157},
		"admission-slot-released.json": SlotReleased{LineHead: head("admission: slot released", "2026-11-21T04:55:47.676285947Z"),
			Issue: "ACME-3", Slots: 156, Cap: 157},
		"admission-waits-on-blockers.json": WaitsOnIssues{LineHead: head("admission waits for what this issue waits on", "2026-11-21T05:08:03.917770662Z"),
			Issue: "ACME-6", Blockers: []string{"ACME-5"}},
		"admission-slot-back-work-came-back.json": SlotBackWorkReturned{LineHead: head("admission gives a slot back to an issue whose work came back", "2026-11-21T04:55:47.564647172Z"),
			Issue: "ACME-2", Phase: phase.Implementing},
		"admission-slot-back-wait-ended.json": SlotBackWaitEnded{LineHead: head("admission gives a slot back to an issue whose wait ended", "2026-11-21T03:20:50.606323131Z"),
			Issue: "ACME-1", Phase: phase.Planning},
		"admission-waits-for-children.json": WaitsForChildren{LineHead: head("admission: an issue waits for the Dispatch children under it", "2026-11-21T00:52:05.903683936Z"),
			Issue: "ACME-4", Tree: "ACME-4", Generation: 1, Children: 5},
		"root-waits-on-person.json": WaitsOnPerson{LineHead: head("workflow: root waits on a person and keeps its admission slot", "2026-11-21T02:22:11.47949729Z"),
			Issue: "ACME-36", Tree: "ACME-36", Reason: "spec version 20 is awaiting a person's review", SinceSeq: 111},
		"design-gate-opened.json": GateOpened{LineHead: head("workflow: design gate opened", "2026-11-21T03:40:14.32409794Z"),
			Issue: "ACME-8", Artifact: "d9a022d3-3fd7-4acb-846d-dabef74b983d", Event: "approved", Version: 100},
		"design-gate-closed.json": GateClosed{LineHead: head("workflow: design gate closed", "2026-11-21T03:22:07.596160896Z"),
			Issue: "ACME-7", Artifact: "a52b7798-f7e8-4fe9-a271-448cc871c5fb", Event: "version", Version: 74},
		"design-gate-registered.json": GateRegistered{LineHead: head("workflow: design gate registered", "2026-11-21T07:59:33.665464267Z"),
			Issue: "ACME-7", Artifact: "a52b7798-f7e8-4fe9-a271-448cc871c5fb", Step: record.DesignStepSpec, Version: 78, Open: false,
			ChangesRequested: false, GatedSteps: []record.DesignStep{record.DesignStepSpec}},
		"held-phase-released.json": HoldReleased{LineHead: head("workflow: a held phase is released", "2026-11-20T00:10:01.999737953Z"),
			Issue: "ACME-9", Phase: phase.Merging, Reason: "the operator retried this phase's worker"},
		"issue-left-workflow.json": LeftWorkflow{LineHead: head("workflow: issue left the workflow", "2026-11-21T04:17:17.416684718Z"),
			Issue: "ACME-10", Tree: "ACME-11", Status: "backlog"},
		"tree-lingers.json": TreeLingers{LineHead: head("workflow: tree lingers", "2026-11-20T20:38:12.539056207Z"),
			Tree: "ACME-40", Generation: 1, Until: capturedTime(t, "2026-11-23T20:38:12.260483847Z")},
		"linger-closed-tree.json": LingerClosed{LineHead: head("workflow: linger closed the tree", "2026-11-21T04:11:33.472588175Z"),
			Tree: "ACME-12", Generation: 1, Issues: 1},
		"supervise-turn-started.json": TurnStarted{LineHead: head("supervise: a turn started", "2026-11-21T05:09:23.248994112Z"),
			Claim: claimOf("legion-acme-acme-39-architect", "ACME-39", claim.RoleArchitect), Generation: 192, Delivery: "outbox:424751"},
		"supervise-turn-ended.json": TurnEnded{LineHead: head("supervise: a turn ended", "2026-11-21T05:09:40.803838475Z"),
			Claim: claimOf("legion-acme-acme-2-architect", "ACME-2", claim.RoleArchitect), Generation: 59},
		"supervise-turn-ended-outcome.json": TurnEnded{LineHead: head("supervise: a turn ended", "2026-11-21T08:06:02.659521763Z"),
			Claim: claimOf("legion-acme-acme-37-merger", "ACME-37", claim.RoleMerger), Generation: 8, Outcome: true},
		"supervise-launched.json": Launched{LineHead: head("supervise: launched", "2026-11-21T05:08:53.046995199Z"),
			Claim: claimOf("legion-acme-acme-37-tester", "ACME-37", claim.RoleTester), Generation: 3, Resumed: true},
		"supervise-suspended.json": Suspended{LineHead: head("supervise: suspended", "2026-11-21T05:08:35.479252159Z"),
			Claim: claimOf("legion-acme-acme-38-architect", "ACME-38", claim.RoleArchitect), By: "released", Reason: "the issue's pod has been idle for the release period"},
		"supervise-claim-failed.json": ClaimFailed{LineHead: head("supervise: claim failed", "2026-11-18T10:37:34.716550893Z"),
			Claim: claimOf("legion-acme-acme-58-implementer", "ACME-58", claim.RoleImplementer), Cause: "launch_refused",
			Why: "launch failures ran out: launch legion-acme-acme-58-implementer: start through its authenticated launcher: the shared public leak check could not be installed: the served check could not be run: waitid: no child processes"},
		"daemon-first-line-stale-plugin.json": ProcessStart{LineHead: head("daemon: stale-plugin relaunches install", "2026-11-21T01:54:55.881009799Z")},
		"daemon-boot-stage-prompts.json": BootStage{LineHead: head("legion workflow boot stage", "2026-11-21T01:54:55.884463509Z"),
			Stage: "prompts"},
		"daemon-started.json": DaemonStarted{LineHead: head("legion daemon started", "2026-11-21T02:01:27.894781099Z"),
			Project: "ACME", AdmissionCap: 157},
		"merge-queue-left-nothing-read.json": QueueLeftNothingRead{LineHead: head("workflow: a pull request left the merge queue where its issue does not merge at that head; nothing is read", "2026-11-21T04:45:24.375465813Z"),
			Issue: "ACME-13", PullRequest: 5367, HeadSHA: "aa593efa160f8338f94e0b8dfc358aeabefd2e3c", Why: "the issue stands in done, not merging"},
		"merge-queue-left-read-once.json": QueueLeftReadOnce{LineHead: head("workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once", "2026-11-21T03:19:56.094165169Z"),
			Issue: "ACME-14", PullRequest: 4547, HeadSHA: "c3b2e84ddc994f14b179f99243a9490806e2df1b"},
	}
}

// capturedEventReadings is what the trace reads from each captured Dispatch event, by its file
// under events/, written out by hand from the event: its envelope, and the part of its payload its
// type fills.
func capturedEventReadings(t *testing.T) map[string]DispatchEvent {
	t.Helper()
	person := dispatch.Actor{Kind: "user", ID: "person@example.invalid"}
	daemon := dispatch.Actor{Kind: "session", ID: "legion-daemon:ACME"}
	session := func(id string) dispatch.Actor { return dispatch.Actor{Kind: "session", ID: id} }
	// Each captured rank is a run of z's and a short tail, Dispatch's fractional keys at the end of
	// a long project's order.
	rank := func(zs int, tail string) string { return strings.Repeat("z", zs) + tail }
	const spec = "1dcfb072-140d-4494-bd05-732b86c0a8ed"
	return map[string]DispatchEvent{
		"issue-created.json": {ID: 410494, Seq: 1, Key: "ACME-25", Project: "ACME", Type: "issue.created",
			Actor: session("3e65c2b4-30dd-495f-be87-749a6829a93a"), At: capturedTime(t, "2026-11-12T02:33:45.794899Z"),
			Issue: &DispatchIssueState{Changed: []string{"title", "status", "rank", "priority", "assignee", "labels", "repo"},
				Status: "triage", Rank: rank(95, "k"), Labels: []string{"legion"}}},
		"issue-updated-status.json": {ID: 411144, Seq: 2, Key: "ACME-25", Project: "ACME", Type: "issue.updated",
			Actor: daemon, At: capturedTime(t, "2026-11-12T02:48:18.305772Z"),
			Issue: &DispatchIssueState{Changed: []string{"status"}, Status: "todo", Rank: rank(95, "k"), Labels: []string{"legion"}}},
		"issue-updated-blocked-by.json": {ID: 891022, Seq: 3, Key: "ACME-51", Project: "ACME", Type: "issue.updated",
			Actor: session("82a7b4ac-d20f-46fe-b38b-d918b160098c"), At: capturedTime(t, "2026-11-21T01:54:00.399322Z"),
			Issue: &DispatchIssueState{Changed: []string{"components", "blocked_by"}, Status: "triage",
				BlockedBy: []string{"ACME-54", "ACME-53", "ACME-55"}, Parent: "ACME-52", Rank: rank(145, "Z")}},
		"issue-updated-parent.json": {ID: 868069, Seq: 3, Key: "ACME-56", Project: "ACME", Type: "issue.updated",
			Actor: session("ae866d8a-8702-4a73-8fdf-e63db56ed57a"), At: capturedTime(t, "2026-11-20T17:44:47.764822Z"),
			Issue: &DispatchIssueState{Changed: []string{"status", "parent", "external_links"}, Status: "in_progress",
				Parent: "ACME-57", Rank: rank(142, "j")}},
		"issue-updated-rank.json": {ID: 701064, Seq: 11, Key: "ACME-25", Project: "ACME", Type: "issue.updated",
			Actor: session("e19a8cca-b02c-4c79-acee-17834b59b140"), At: capturedTime(t, "2026-11-18T00:12:10.182106Z"),
			Issue: &DispatchIssueState{Changed: []string{"rank"}, Status: "todo", Rank: rank(113, "fzzU"), Labels: []string{"legion"}}},
		"issue-closed.json": {ID: 502249, Seq: 99, Key: "ACME-43", Project: "ACME", Type: "issue.closed",
			Actor: daemon, At: capturedTime(t, "2026-11-13T21:26:48.67625Z"),
			Issue: &DispatchIssueState{Changed: []string{"status", "closed_at"}, Status: "done", Parent: "ACME-44", Rank: rank(93, "r")}},
		"artifact-approved.json": {ID: 886533, Seq: 58, Key: "ACME-25", Project: "ACME", Type: "artifact.approved",
			Actor: person, At: capturedTime(t, "2026-11-21T00:17:12.362358Z"),
			Verdict: &DispatchVerdict{Artifact: spec, Name: "spec.md", Version: 5}},
		"artifact-changes-requested.json": {ID: 880496, Seq: 41, Key: "ACME-25", Project: "ACME", Type: "artifact.changes_requested",
			Actor: person, At: capturedTime(t, "2026-11-20T22:07:57.233851Z"),
			Verdict: &DispatchVerdict{Artifact: spec, Name: "spec.md", Version: 4, Reason: "Captured change request reason"}},
		"ask-opened.json": {ID: 877110, Seq: 23, Key: "ACME-25", Project: "ACME", Type: "ask.opened",
			Actor: session("f612746a-5eb6-4863-905a-1fe5650c0de1"), At: capturedTime(t, "2026-11-20T20:47:49.424239Z"),
			Ask: &DispatchAsk{ID: "64ffbfe0-c4da-4f4f-9abb-5fbe1c415b93", Kind: "question", State: "open"}},
		"ask-answered.json": {ID: 880495, Seq: 40, Key: "ACME-25", Project: "ACME", Type: "ask.answered",
			Actor: person, At: capturedTime(t, "2026-11-20T22:07:57.233851Z"),
			Ask: &DispatchAsk{ID: "03aa53cb-6f22-4d35-832f-58fd4ee4b472", Kind: "approval", State: "answered", Artifact: spec}},
		"handoff-recorded.json": {ID: 890146, Seq: 70, Key: "ACME-25", Project: "ACME", Type: "handoff.recorded",
			Actor: session("c8a5d8a2-261d-4d0e-9511-74e57527faa9"), At: capturedTime(t, "2026-11-21T01:35:18.01727Z"),
			Handoff: &DispatchHandoff{ID: "e4c8e189-fb0c-4627-8f7c-792217db3dcc", Phase: "plan", Version: 1}},
	}
}

// capturedMergeReadings is the first-parent commits the trace reads from each captured page of
// capturedBranch's commit listing, by its file under github/, oldest first, written out by hand
// from the page: five commits listed newest first, each the first parent of the one before it, the
// oldest's first parent on no page.
func capturedMergeReadings(t *testing.T) map[string][]Merge {
	t.Helper()
	merge := func(sha, stamp string) Merge {
		return Merge{SHA: sha, At: capturedTime(t, stamp), Branch: capturedBranch}
	}
	return map[string][]Merge{
		"commits.json": {
			merge("282a49f20ac5b24d9c30925646c9d0438905fd0e", "2026-11-20T04:56:30Z"),
			merge("6d50ef31bf220fb231e46d9264eddd77ef921078", "2026-11-20T05:15:42Z"),
			merge("0d1bfb2da6a03ee24cd7f9d6a9bd6e2ad56bb79a", "2026-11-20T05:29:02Z"),
			merge("e967604ded2c6bcd03d676ad64e449f7275991c0", "2026-11-20T05:34:11Z"),
			merge("651db8d54c55d5e78e092ece50a047c09108ebd7", "2026-11-20T05:36:33Z"),
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
