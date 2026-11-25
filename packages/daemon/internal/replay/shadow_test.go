package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// answersApproval is a person answering key's approval request ask at at (ask.answered), the
// close of the request Dispatch writes beside the approval that answers it.
func (b *windowBuilder) answersApproval(at time.Time, key, ask string) {
	b.t.Helper()
	architect := b.session(key, claim.RoleArchitect)
	b.event(at, key, "ask.answered", builtPerson, map[string]any{
		"id": ask, "issue_key": key, "author": architect, "kind": record.AskApproval, "state": "answered",
		"approval": map[string]any{"artifact_id": b.artifact(key), "version": 1, "requested_version": 1},
		"answer":   map[string]any{"at": at, "user": builtPerson.ID},
	})
}

// midwayWindow is a window from 10:00 to 12:00, its inputs reaching back to 08:00, that starts
// half-way through a tree's work, and the id of its root's design document. ACME-1, a root, is
// promoted at 08:05; its architect writes version 1 of its spec at 08:15, asks a person to approve
// it at 08:17 and registers it at 08:20, which opens the gate of a deployment that gates no step,
// and ACME-1 plans from then. At 08:30 the architect files ACME-2 under it, which starts at once and
// implements from 09:00. Both stand in the workflow at the window's start, the approval request
// open. At 10:30 the person approves the version, answering the request; at 11:00 ACME-2's
// implementer's turn ends and the person sets ACME-2 done, and it leaves the workflow; at 11:30
// ACME-1 moves on to implementing.
func midwayWindow(t *testing.T) (Window, string) {
	b := newWindowBuilder(t, clock(10, 0), clock(12, 0))
	b.reaches(2 * time.Hour)
	b.boot(clock(8, 0))
	b.file(clock(8, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(8, 5), "ACME-1")
	b.specWritten(clock(8, 15), "ACME-1")
	b.asksApproval(clock(8, 17), "ACME-1", approvalAsk)
	b.rootDesigned(clock(8, 20), "ACME-1")
	b.file(clock(8, 30), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a1"})
	b.childStarts(clock(8, 30), "ACME-2")
	b.segmentEnds(clock(9, 0), "ACME-2", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.approves(clock(10, 30), "ACME-1", approvalAsk)
	b.answersApproval(clock(10, 30), "ACME-1", approvalAsk)
	b.turnEnds(clock(11, 0), "ACME-2", claim.RoleImplementer)
	b.edit(clock(11, 0), "ACME-2", setStatus(record.DoneStatus))
	b.leaves(clock(11, 0), "ACME-2", record.DoneStatus)
	b.segmentEnds(clock(11, 30), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	return b.build(), b.artifact("ACME-1")
}

// newestBefore is, by issue key, the recorded sequence of each issue's newest Dispatch event before
// w's start.
func newestBefore(t *testing.T, w Window) map[string]int64 {
	t.Helper()
	newest := map[string]int64{}
	for key, events := range w.Events {
		for _, raw := range events {
			event, err := ParseDispatchEvent(raw)
			if err != nil {
				t.Fatalf("read an event of %s: %v", key, err)
			}
			if event.At.Before(w.From) {
				newest[key] = max(newest[key], event.Seq)
			}
		}
	}
	return newest
}

// approvalRead is what one settle of a replay reads of a document's approval from its copy of
// Dispatch (shadow.approvalOf).
type approvalRead struct {
	at              time.Time
	latest          int
	ask, waitingOn  string
	approvedVersion int
	approvedBy      string
}

// A replay of a window that starts half-way starts its copy of Dispatch where the record stands at
// the start, so a Dispatch write of an issue placed at the start applies after it, and an answer
// after the start is read against the ask from before it. The test replays midwayWindow under main:
// at the start ACME-1 and ACME-2 each stand in the copy at the recorded sequence of their newest
// event before it, and the copy holds the approval request ACME-1's architect opened at 08:17, open
// on the person's turn; every settle from the start to the person's approval at 10:30 reads that
// request open on version 1 of the document, written at 08:15, and every settle from then reads the
// version approved by the person and no request open, the request from before the start being the
// one the answer closed; and the person's done of ACME-2 at 11:00 is applied, no fence dropping it,
// so ACME-2 leaves the workflow then.
func TestAPlacedIssuesDispatchWritesApplyAfterAMidwayStart(t *testing.T) {
	dsn := replayDSN(t)
	window, artifact := midwayWindow(t)
	if trace, err := BuildTrace(window); err != nil || len(trace.Unplaced) != 0 {
		t.Fatalf("the trace of midwayWindow: %v, could not place %s", err, describeReading(trace.Unplaced))
	}
	newest := newestBefore(t, window)
	for _, key := range []string{"ACME-1", "ACME-2"} {
		if newest[key] == 0 {
			t.Fatalf("the window holds no event of %s before its start, which the test needs", key)
		}
	}

	var copied *shadow
	started := map[string]int64{}
	var opened shadowAsk
	var reads []approvalRead
	out := t.TempDir()
	cfg := RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out}
	cfg.atStart = func(s *shadow) {
		copied = s
		for key, iss := range s.issues {
			started[key] = iss.seq
		}
		if iss, ok := s.issues["ACME-1"]; ok && iss.asks[approvalAsk] != nil {
			opened = *iss.asks[approvalAsk]
		}
	}
	cfg.atSettled = func(_ context.Context, _ *store.Store, at time.Time) error {
		if copied == nil {
			return nil
		}
		read := copied.approvalOf("ACME-1", artifact)
		got := approvalRead{at: at, latest: read.latest, ask: read.ask, waitingOn: read.waitingOn}
		if read.approved != nil {
			got.approvedVersion, got.approvedBy = read.approved.version, read.approved.actor.Kind+":"+read.approved.actor.ID
		}
		reads = append(reads, got)
		return nil
	}
	result, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("replay midwayWindow under main: %v", err)
	}
	if copied == nil {
		t.Fatal("the run never handed the test its copy of Dispatch at the window's start")
	}
	for _, key := range slices.Sorted(maps.Keys(newest)) {
		if started[key] != newest[key] {
			t.Errorf("at the window's start the copy of Dispatch holds %s at sequence %d, want %d, the recorded sequence of its newest event before the start",
				key, started[key], newest[key])
		}
	}
	if opened.kind != record.AskApproval || opened.artifact != artifact || !opened.open || opened.waitingOn != "human" {
		t.Errorf("at the window's start the copy of Dispatch holds ACME-1's ask %s as %+v, want the approval request of %s opened at 08:17, open on the person's turn",
			approvalAsk, opened, artifact)
	}
	person := builtPerson.Kind + ":" + builtPerson.ID
	before, after := 0, 0
	for _, read := range reads {
		switch {
		case read.at.Before(clock(10, 30)):
			before++
			if read.latest != 1 || read.ask != approvalAsk || read.waitingOn != "human" || read.approvedVersion != 0 {
				t.Errorf("at %s the copy of Dispatch reads ACME-1's document as %+v, want version 1 under the request %s from before the start, open on the person's turn, and no approval",
					stamp(read.at), read, approvalAsk)
			}
		default:
			after++
			if read.latest != 1 || read.ask != "" || read.approvedVersion != 1 || read.approvedBy != person {
				t.Errorf("at %s the copy of Dispatch reads ACME-1's document as %+v, want version 1 approved by %s and no request open", stamp(read.at), read,
					person)
			}
		}
	}
	if before == 0 || after == 0 {
		t.Errorf("the settles read ACME-1's document %d times before the approval at 10:30 and %d from it, want some of each: %+v", before, after, reads)
	}
	asks := copied.issues["ACME-1"].asks
	if len(asks) != 1 || asks[approvalAsk] == nil || asks[approvalAsk].open {
		var held []shadowAsk
		for _, id := range slices.Sorted(maps.Keys(asks)) {
			held = append(held, *asks[id])
		}
		t.Errorf("after the run the copy of Dispatch holds ACME-1's asks %+v, want the one request from before the start, closed by the answer", held)
	}

	if result.Divergence.Fenced != 0 {
		t.Errorf("a fence of the daemon's dropped %d of the replay's Dispatch events, want none", result.Divergence.Fenced)
	}
	left := linesAt(replayLog(t, out), func(line replayLine) bool {
		return line.Msg == "workflow: issue left the workflow" && line.Issue == "ACME-2"
	})
	if want := []time.Time{clock(11, 0)}; !slices.EqualFunc(left, want, time.Time.Equal) {
		t.Errorf("ACME-2 left the workflow at %s, want once, at the person's done, %s", describeTimes(left), describeTimes(want))
	}
}

// The daemon's own session writes both what its outbox writes, which a replay makes again from its
// own rows, and what its API writes for an agent, which no row makes, and a replay applies the
// second as recorded (keepsDaemonEvent), each judged against its issue's event before it in the
// record: a move to backlog, icebox or triage, and one from those or from done to todo that adds
// no legion label, root or child alike. The test reads in order each event of a window whose daemon
// session writes: ACME-1, a root a person filed in backlog with the
// label, todo at 09:10 (the controller's set_status), in_progress at 09:11 (its promotion), backlog
// at 09:20 (the controller parks it), icebox at 09:21 and icebox again at 09:22, which moves
// nothing; ACME-2, a child filed in triage, todo at 09:31 (release_children); ACME-3, a root a
// person filed in icebox with the label, todo at 09:41 (the controller's set_status); ACME-4, a
// child filed in triage, todo with the label at 09:51 (a hand-over's write); ACME-5, a root handed
// to Legion, in_progress at 10:01 and done at 10:02 (its promotion and its close); ACME-6, a child
// of ACME-5 filed todo, in_progress at 10:11, done at 10:12 (its close) and todo at 10:13
// (rerun_child); and ACME-7, a child of ACME-5 filed todo, in_progress at 10:21, done at 10:22 and
// todo with the label at 10:23 (a hand-over's write). The replay applies the writes of 09:10,
// 09:20, 09:21, 09:31, 09:41 and 10:13, and leaves every other to its outbox.
func TestADaemonWriteNoOutboxRowMakesIsAppliedAsRecorded(t *testing.T) {
	b := newWindowBuilder(t, clock(9, 0), clock(12, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "backlog", Rank: "a1", Labels: handedToLegion})
	b.daemonWrites(clock(9, 10), "ACME-1", "todo")
	b.daemonWrites(clock(9, 11), "ACME-1", "in_progress")
	b.daemonWrites(clock(9, 20), "ACME-1", "backlog")
	b.daemonWrites(clock(9, 21), "ACME-1", "icebox")
	b.daemonWrites(clock(9, 22), "ACME-1", "icebox")
	b.file(clock(9, 30), "ACME-2", filing{Title: "Write the export's header row", Parent: "ACME-1", Tree: "ACME-1", Status: "triage", Rank: "a2"})
	b.daemonWrites(clock(9, 31), "ACME-2", "todo")
	b.file(clock(9, 40), "ACME-3", filing{Title: "Count the widgets per shelf", Status: "icebox", Rank: "a3", Labels: handedToLegion})
	b.daemonWrites(clock(9, 41), "ACME-3", "todo")
	b.file(clock(9, 50), "ACME-4", filing{Title: "Name the header row's columns", Parent: "ACME-1", Tree: "ACME-1", Status: "triage", Rank: "a4"})
	b.handsOver(clock(9, 51), "ACME-4")
	b.file(clock(10, 0), "ACME-5", filing{Title: "Keep the last search on the shelf screen", Status: "todo", Rank: "a5", Labels: handedToLegion})
	b.daemonWrites(clock(10, 1), "ACME-5", "in_progress")
	b.daemonWrites(clock(10, 2), "ACME-5", record.DoneStatus)
	b.file(clock(10, 10), "ACME-6", filing{Title: "Clear the last search on sign-out", Parent: "ACME-5", Tree: "ACME-5", Status: "todo", Rank: "a6"})
	b.daemonWrites(clock(10, 11), "ACME-6", "in_progress")
	b.daemonWrites(clock(10, 12), "ACME-6", record.DoneStatus)
	b.daemonWrites(clock(10, 13), "ACME-6", "todo")
	b.file(clock(10, 20), "ACME-7", filing{Title: "Show the last search's date", Parent: "ACME-5", Tree: "ACME-5", Status: "todo", Rank: "a7"})
	b.daemonWrites(clock(10, 21), "ACME-7", "in_progress")
	b.daemonWrites(clock(10, 22), "ACME-7", record.DoneStatus)
	b.handsOver(clock(10, 23), "ACME-7")
	window := b.build()

	d := &driver{}
	var applied []string
	for _, key := range slices.Sorted(maps.Keys(window.Events)) {
		for i, raw := range window.Events[key] {
			parsed, err := ParseDispatchEvent(raw)
			if err != nil {
				t.Fatalf("read event %d of %s: %v", i+1, key, err)
			}
			env, err := readEnvelope(raw, parsed)
			if err != nil {
				t.Fatalf("read the envelope of event %d of %s: %v", i+1, key, err)
			}
			event := scriptEvent{parsed: parsed, env: env, prev: -1}
			if i > 0 {
				event.prev = len(d.script.events) - 1
			}
			d.script.events = append(d.script.events, event)
			if daemonWrote(env.actor, env.key) && d.keepsDaemonEvent(&d.script.events[len(d.script.events)-1]) {
				applied = append(applied, key+" "+parsed.At.Format("15:04"))
			}
		}
	}
	if want := []string{"ACME-1 09:10", "ACME-1 09:20", "ACME-1 09:21", "ACME-2 09:31", "ACME-3 09:41", "ACME-6 10:13"}; !slices.Equal(applied, want) {
		t.Errorf("the replay applies the daemon's writes %q as recorded, want %q", applied, want)
	}
}

// apiStatusWindow is a window from 09:00 to 13:00 in which the daemon's session writes statuses
// for a controller, as the daemon's API writes them for the controller's set_status. ACME-1, a root
// a person filed in backlog with the legion label at 09:05, is set todo by the daemon's session at
// 09:30: it enters the line, and the pass promotes it at once; at 11:00 its architect's turn ends
// and a person sets it done, and it leaves the workflow. ACME-2, a root handed to Legion at 09:10
// and promoted then, is set back to backlog by the daemon's session at 10:00, as its architect's
// turn ends, and leaves the workflow then; the linger closes its tree at 12:00.
func apiStatusWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "backlog", Rank: "a1", Labels: handedToLegion})
	b.file(clock(9, 10), "ACME-2", filing{Title: "Count the widgets per shelf", Status: "todo", Rank: "a2", Labels: handedToLegion})
	b.rootPromoted(clock(9, 10), "ACME-2")
	b.daemonWrites(clock(9, 30), "ACME-1", "todo")
	b.rootPromoted(clock(9, 30), "ACME-1")
	b.turnEnds(clock(10, 0), "ACME-2", claim.RoleArchitect)
	b.daemonWrites(clock(10, 0), "ACME-2", "backlog")
	b.leaves(clock(10, 0), "ACME-2", "backlog")
	b.turnEnds(clock(11, 0), "ACME-1", claim.RoleArchitect)
	b.edit(clock(11, 0), "ACME-1", setStatus(record.DoneStatus))
	b.leaves(clock(11, 0), "ACME-1", record.DoneStatus)
	b.lingerCloses(clock(12, 0), "ACME-2", 1)
	return b.build()
}

// A status the daemon's API writes for the controller moves a replay as it moved the record: the
// replay applies the daemon session's event of it as recorded, where it leaves its outbox's own
// status writes out. The test replays apiStatusWindow under main: ACME-1, set todo from backlog by
// the daemon's session at 09:30, enters the line and takes a slot then, and leaves the workflow at
// the person's done at 11:00; ACME-2, set back to backlog by the daemon's session at 10:00, leaves
// the workflow then.
func TestAStatusTheDaemonsAPIWritesIsAppliedAsRecorded(t *testing.T) {
	dsn := replayDSN(t)
	window := apiStatusWindow(t)
	if trace, err := BuildTrace(window); err != nil || len(trace.Unplaced) != 0 {
		t.Fatalf("the trace of apiStatusWindow: %v, could not place %s", err, describeReading(trace.Unplaced))
	}
	out := t.TempDir()
	if _, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: out}); err != nil {
		t.Fatalf("replay apiStatusWindow under main: %v", err)
	}
	log := replayLog(t, out)
	for _, want := range []struct {
		key, msg string
		at       []time.Time
	}{
		{"ACME-1", "admission: slot taken", []time.Time{clock(9, 30)}},
		{"ACME-1", "workflow: issue left the workflow", []time.Time{clock(11, 0)}},
		{"ACME-2", "workflow: issue left the workflow", []time.Time{clock(10, 0)}},
	} {
		got := linesAt(log, func(line replayLine) bool { return line.Msg == want.msg && line.Issue == want.key })
		if !slices.EqualFunc(got, want.at, time.Time.Equal) {
			t.Errorf("the replay writes %q of %s at %s, want at %s", want.msg, want.key, describeTimes(got), describeTimes(want.at))
		}
	}
}

// A hand-over's write, the daemon's todo of a child of a closed tree with the legion label added,
// is its outbox's (workflow's handOver), and a replay makes it again from its own row rather than
// applying the record's event of it (keepsDaemonEvent). The test replays treeClosedWindow under
// main: the replay's own linger close hands ACME-2 over at 11:30, and its copy of Dispatch holds
// one event that puts the label on ACME-2, the echo of the replay's own write, whose id is past
// every recorded one (echoIDBase).
func TestAHandOverStaysTheReplaysOwnWrite(t *testing.T) {
	dsn := replayDSN(t)
	window := treeClosedWindow(t)
	var copied *shadow
	out := t.TempDir()
	cfg := RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out}
	cfg.atStart = func(s *shadow) { copied = s }
	if _, err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("replay treeClosedWindow under main: %v", err)
	}
	if copied == nil {
		t.Fatal("the run never handed the test its copy of Dispatch at the window's start")
	}
	handed := linesAt(replayLog(t, out), func(line replayLine) bool { return line.Msg == "admission hand-over" && line.Issue == "ACME-2" })
	if want := []time.Time{clock(11, 30)}; !slices.EqualFunc(handed, want, time.Time.Equal) {
		t.Errorf("the replay hands ACME-2 over at %s, want once, at %s", describeTimes(handed), describeTimes(want))
	}
	iss := copied.issues["ACME-2"]
	labelled := false
	var writes []string
	for i, raw := range iss.log[iss.seeded:] {
		var event struct {
			ID      int64      `json:"id"`
			Actor   eventActor `json:"actor"`
			Payload struct {
				Labels []string `json:"labels"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("read event %d of ACME-2 in the copy of Dispatch: %v", i+1, err)
		}
		carries := dispatch.CarriesLegionLabel(event.Payload.Labels)
		if carries && !labelled {
			writes = append(writes, fmt.Sprintf("%s:%s %d", event.Actor.Kind, event.Actor.ID, event.ID))
			if event.Actor.ID != dispatch.DaemonSession("ACME-2") || event.ID < echoIDBase {
				t.Errorf("the copy of Dispatch puts the label on ACME-2 by the event %d of %s:%s, want the echo of the replay's own write",
					event.ID, event.Actor.Kind, event.Actor.ID)
			}
		}
		labelled = carries
	}
	if len(writes) != 1 {
		t.Errorf("the copy of Dispatch puts the label on ACME-2 by the events %q, want one", writes)
	}
}

// heldRootTodoWindow is a window from 09:00 to 13:00 with the controller's todo on a held root.
// ACME-1, a root, is promoted at 09:05, plans from 09:20 and implements from 09:50. At 10:20 its
// implementer's claim fails, which holds it from implementing, keeping its slot. At 11:20 the
// daemon's session sets it todo, as the daemon's API writes the controller's set_status: the todo
// releases the hold, the implementer is launched again, and the daemon writes in_progress a second
// later. ACME-1 moves to testing at 11:50.
func heldRootTodoWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.boot(clock(9, 0))
	b.file(clock(9, 5), "ACME-1", filing{Title: "Export the widget list", Status: "todo", Rank: "a0", Labels: handedToLegion})
	b.rootPromoted(clock(9, 5), "ACME-1")
	b.rootDesigned(clock(9, 20), "ACME-1")
	b.segmentEnds(clock(9, 50), "ACME-1", phase.Planning, phase.Implementing, workflow.TriggerPlannerCompleted)
	b.claimFailed(clock(10, 20), "ACME-1", claim.RoleImplementer)
	b.daemonWrites(clock(11, 20), "ACME-1", "todo")
	b.info(clock(11, 20), "workflow: a held phase is released", "issue", "ACME-1", "phase", string(phase.Implementing),
		"reason", heldRootReleaseReason)
	b.launched(clock(11, 20), "ACME-1", claim.RoleImplementer, true)
	b.turnStarts(clock(11, 20), "ACME-1", claim.RoleImplementer)
	b.daemonWrites(clock(11, 20).Add(time.Second), "ACME-1", "in_progress")
	b.segmentEnds(clock(11, 50), "ACME-1", phase.Implementing, phase.Testing, workflow.TriggerImplementationReady)
	return b.build()
}

// The controller's todo on a held root releases the hold in a replay where the record's did. The
// record's release names a todo as its cause (heldRootReleaseReason), for which the replay enters
// no retry, so the hold ends only where the daemon session's event of the todo is applied as
// recorded (keepsDaemonEvent, releasesHold). The test replays heldRootTodoWindow under main: the
// daemon's own code releases ACME-1's hold at 11:20, and ACME-1 makes its recorded move to testing
// at 11:50, no move refused.
func TestTheControllersTodoOnAHeldRootReleasesTheHold(t *testing.T) {
	dsn := replayDSN(t)
	window := heldRootTodoWindow(t)
	if trace, err := BuildTrace(window); err != nil || len(trace.Unplaced) != 0 {
		t.Fatalf("the trace of heldRootTodoWindow: %v, could not place %s", err, describeReading(trace.Unplaced))
	}
	out := t.TempDir()
	result, err := Run(context.Background(), RunConfig{Window: window, Rule: mainRule(t), Branch: MainBranch, Seed: 1, Slowdown: 1,
		PGDSN: dsn, Out: out})
	if err != nil {
		t.Fatalf("replay heldRootTodoWindow under main: %v", err)
	}
	log := replayLog(t, out)
	released := linesAt(log, func(line replayLine) bool {
		return line.Msg == "workflow: a held phase is released" && line.Issue == "ACME-1"
	})
	if want := []time.Time{clock(11, 20)}; !slices.EqualFunc(released, want, time.Time.Equal) {
		t.Errorf("the replay releases ACME-1's hold at %s, want at %s", describeTimes(released), describeTimes(want))
	}
	tested := linesAt(log, func(line replayLine) bool {
		return line.Msg == phaseChangedLine && line.Issue == "ACME-1" && line.From == string(phase.Implementing) && line.To == string(phase.Testing)
	})
	if want := []time.Time{clock(11, 50)}; !slices.EqualFunc(tested, want, time.Time.Equal) {
		t.Errorf("ACME-1 moves to testing at %s in the replay, want at %s", describeTimes(tested), describeTimes(want))
	}
	if len(result.Divergence.Refused) != 0 {
		t.Errorf("the replay refused the recorded moves %+v, want none", result.Divergence.Refused)
	}
}

// outsideDaemonWindow is a window from 09:00 to 13:00 recorded under main, its inputs reaching back
// to the daemon's boot at 06:00, the controller ticking a minute after it and every hour. OTHER-7 is
// an issue of another project, which runs a daemon of its own: a person files it at 06:30, and that
// daemon's session writes it todo at 06:31, in_progress at 07:00 and done at 10:20. ACME-1, a root
// promoted at 06:05, implements from 07:00 to the window's end. ACME-2, which a person files todo
// under ACME-1 at 07:30, blocked by OTHER-7, starts at 11:01, at the first tick after its blocker's
// close.
func outsideDaemonWindow(t *testing.T) Window {
	b := newWindowBuilder(t, clock(9, 0), clock(13, 0))
	b.reaches(3 * time.Hour)
	b.boot(clock(6, 0))
	rootThrough(b, "ACME-1", "a1", clock(6, 5), clock(6, 20), moveAt(7, 0, phase.Implementing, workflow.TriggerPlannerCompleted))
	b.file(clock(6, 30), "OTHER-7", filing{Title: "Publish the shelf images", Status: "triage", Rank: "x7"})
	other := eventActor{Kind: "session", ID: dispatch.DaemonSession("OTHER-7")}
	b.changed(clock(6, 31), "OTHER-7", other, setStatus("todo"))
	b.changed(clock(7, 0), "OTHER-7", other, setStatus("in_progress"))
	b.changed(clock(10, 20), "OTHER-7", other, setStatus(record.DoneStatus))
	b.file(clock(7, 30), "ACME-2", filing{Title: "Label the shelves", Parent: "ACME-1", Tree: "ACME-1", Status: "todo", Rank: "a2",
		BlockedBy: []string{"OTHER-7"}})
	b.childStarts(clock(11, 1), "ACME-2", "OTHER-7")
	return b.build()
}

// Only the daemon of the window's own project writes what a replay makes again from its own outbox
// (keepsDaemonEvent): another project's daemon is another daemon, and its writes of its own issues,
// an outside blocker's close among them, are applied as recorded, as a person's are. The test
// replays outsideDaemonWindow under main: OTHER-7's close by its own daemon at 10:20 reaches the
// replay's copy of Dispatch, and ACME-2 starts at 11:01, the first tick after it. It fails where
// that close is taken for a write of the window's own daemon and left out, for which OTHER-7 stays
// todo and ACME-2 never starts.
func TestAnotherProjectsDaemonWritesItsIssuesAsRecorded(t *testing.T) {
	log, copied := replayCopied(t, outsideDaemonWindow(t), record.MainRule.Name)
	if closed, want := eventsApplied(t, copied, "OTHER-7", "issue.closed"), []time.Time{clock(10, 20)}; !slices.EqualFunc(closed, want, time.Time.Equal) {
		t.Errorf("OTHER-7's close was applied at %s, want at %s", describeTimes(closed), describeTimes(want))
	}
	if started, want := startedAt(log, "ACME-2"), []time.Time{clock(11, 1)}; !slices.EqualFunc(started, want, time.Time.Equal) {
		t.Errorf("ACME-2 started at %s, want at %s, the first tick after its blocker's close", describeTimes(started), describeTimes(want))
	}
}
