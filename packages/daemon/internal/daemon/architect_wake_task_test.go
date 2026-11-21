package daemon

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// release suspends the architect's claim with the source an idle release's role stop gives it
// (supervise.SuspendedByReleased), and does nothing else of a release: no idle pass decides it,
// and no release mark, role-stop row or pod_releases row is written, so a wake taken after it
// pairs with release 0 (store.LatestPodRelease). The claim keeps its session and the task it
// holds, and only a wake or a start brings it back.
func (r *mainPathRig) release() {
	r.t.Helper()
	machine := r.machine()
	if err := machine.Handle(r.ctx, supervise.RequestSuspend{Claim: r.token, Reason: "the issue's pod is released",
		By: supervise.SuspendedByReleased}); err != nil {
		r.t.Fatalf("release the architect's claim: %v", err)
	}
	r.waitMachine()
	if got := machine.Claim(); !got.Released() {
		r.t.Fatalf("the architect after the release = %s by %q, want it released", got.State, got.SuspendedBy)
	}
}

// wakeByRoleMessage is the Envoy listener's report of a message published to the architect's role
// topic while its claim is released (intake.ListenerWake), applied as the daemon applies it: the
// rig's own engine carries no pod tables, so the report goes through an engine handed the store's
// (store.Store.PodRecordsOf), as the daemon's engine is, which takes the wake and writes its start
// row (workflow's listenerWake, wakeReleased and wakeRow). It returns the task that row carries.
func (r *mainPathRig) wakeByRoleMessage(report string) string {
	r.t.Helper()
	tables, err := r.trees.PodRecordsOf(mainPathProject)
	if err != nil {
		r.t.Fatalf("read the pod tables: %v", err)
	}
	engine := workflow.New(r.records, workflow.Config{Project: mainPathProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Pods: tables}, quietLogger())
	result, err := intake.ApplyFact(r.ctx, r.pool, "wake", report, intake.ListenerWake{Kind: intake.ListenerWakeRole,
		Role: string(r.token), At: time.Now().UTC(), Report: report}, engine)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply the listener's report %s = %#v, %v", report, result, err)
	}
	return workflow.WakeTask(pods.WakeRoleMessage, string(r.token), workflow.StoppedByRelease)
}

// queueReleaseResume queues the resume a cancelled idle release writes for the architect's claim:
// the row the daemon's own builder makes (outbox.releaseResume) at the root's generation, for a
// role whose wake the cancel did not drop, inserted with the statement store.CancelIssueRelease
// uses. No release is marked and none is cancelled: the row is written alone, outside the
// cancel's transaction. It returns that row as the outbox holds it.
func (r *mainPathRig) queueReleaseResume() entryStartRow {
	r.t.Helper()
	row, err := r.runner.releaseResume(r.key, r.key)(claim.RoleArchitect, r.issue().Generation, "", "")
	if err != nil {
		r.t.Fatalf("build the release's resume: %v", err)
	}
	var id int64
	if err := r.pool.QueryRow(r.ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ($1, $2, $3, 0, now(), '', '') returning id`, row.Kind, r.key, string(row.Payload)).Scan(&id); err != nil {
		r.t.Fatalf("queue the release's resume: %v", err)
	}
	for _, start := range entryStartRows(r.t, r.pool, r.key) {
		if start.row.ID == id {
			if got := start.request; got.Task != "" || got.Entry != 0 || got.Phase != "" {
				r.t.Fatalf("the release's resume = %+v, want a start of no task, no entry and no phase", got)
			}
			return start
		}
	}
	r.t.Fatalf("the release's resume %d is not in the outbox", id)
	return entryStartRow{}
}

// architectStartPending is what the sweep reads of the architect's start now
// (workflow.ArchitectStartPending): the root as the record holds it, the entry its architect's
// phases row names, and the run the store reads (record.Store.RoleRun).
func (r *mainPathRig) architectStartPending() (string, bool) {
	r.t.Helper()
	root := r.issue()
	entry := rolePhaseRow(r.t, r.pool, r.key, claim.RoleArchitect).Entry
	var reason string
	var pending bool
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		run, err := r.records.RoleRun(r.ctx, tx, r.token, r.key, claim.RoleArchitect, root.Generation)
		reason, pending = workflow.ArchitectStartPending(root, entry, run)
		return err
	}); err != nil {
		r.t.Fatalf("read the architect's run: %v", err)
	}
	return reason, pending
}

// A wake's task reaches the architect even when the resume of a person's answer raises its entry
// while that task is still on its claim: a task of no entry is superseded by nothing
// (supervise.Delivery.SupersededBy). Before dispatch://INFRA-2614 no held task was superseded,
// and its R2 keeps that for every task the change does not name. These steps put the two on one
// claim in the order the daemon gives them:
//
//  1. the root runs with a child in a phase; its architect takes the root's first task and hands
//     a question to a person, and the child's phase keeps the root from waiting on them. The root
//     is admitted and started by the daemon's own pass; the child is put in its phase directly;
//  2. the claim is suspended as released. This step is a fixture (release): the role stop's
//     suspend alone, with no idle pass, release mark, role-stop row or pod_releases row. The idle
//     rule's one hold of a root's own, a root that waits on a person (IdleHoldAwaitingPerson),
//     does not hold here, since the root does not wait yet;
//  3. the child leaves the workflow, and that fact's pass puts the root in awaiting_person: the
//     released claim holds no task and no start is queued, so its architect owes nothing;
//  4. a message is published to the architect's role topic, and the listener's report wakes the
//     released pod: the wake's start row carries the wake's task and no entry. With no
//     pod_releases row, the wake's resume pairs with release 0;
//  5. that row resumes the claim and queues the wake's task, and the relaunch is not ready yet;
//  6. the person answers, and the pass resumes the root: it raises the architect's entry with the
//     answer's task and queues that entry's start (workflow.Engine.ResumeAwaitedRoot);
//  7. that start reaches the claim before the relaunched agent is ready.
//
// The wake's task is the relaunched agent's first prompt. The answer's task waits behind it, its
// row left for the lane to run again, and is sent once the wake's turn is over. Read as superseded
// by the entry's task, the wake's task was retired at the seventh step, never taken by a turn.
func TestAWakesTaskIsSentBeforeTheAnswerThatRaisedTheArchitectsEntry(t *testing.T) {
	const child, ask = "LEGION-452", "ask-a"
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	parent := r.key
	r.put(record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: mainPathProject, Tree: r.key, Parent: &parent, Title: "child", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "B", LastDispatchSeq: 5})
	r.admitPass()
	r.runStarts()
	r.bringUp()
	r.takeTheTask()
	r.recordClaim()
	r.apply("opened:"+ask, intake.DispatchAskOpened{Key: r.key, Ask: ask, Kind: record.AskQuestion,
		AuthorSession: "ses-architect", Mark: record.EventMark{Seq: r.nextSeq()}})
	if got := r.issue(); got.DeclaredSeq == 0 || got.AwaitingPerson() {
		t.Fatalf("the root while a child of its tree is in a phase = %+v, want it admitted with its hand-back declared", got)
	}

	r.release()
	r.apply("child-backlog", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 6, Type: "issue.updated",
		Status: "backlog", Title: "child", Parent: r.key, Rank: "B"})
	if got := r.issue(); got.Phase != phase.AwaitingPerson || got.Awaiting == nil || got.Awaiting.Ask != ask {
		t.Fatalf("the root once its child left = %+v, want it waiting on %s", got, ask)
	}

	wake := r.wakeByRoleMessage("role-message-1")
	r.runStarts()
	held := r.machine().Claim()
	if held.Pending == nil || held.Pending.Task != wake || held.Pending.Entry != 0 || held.Pending.Generation != 1 ||
		!held.Pending.ConfirmedAt.IsZero() || supervise.SaidReady(held.State) {
		t.Fatalf("the architect after the wake's start = %s holding %#v, want the wake's task of no entry held by a claim not ready yet",
			held.State, held.Pending)
	}
	wakeID := held.Pending.ID
	sent := len(r.prompts())

	r.apply("ask-answered:"+ask, intake.DispatchAskClosed{Key: r.key, Ask: ask, Kind: record.AskQuestion, State: "answered",
		Actor: alice, Mark: record.EventMark{Seq: r.nextSeq()}})
	if got := r.issue(); got.Phase != phase.Admitted || got.Awaiting != nil {
		t.Fatalf("the answered root = %+v, want it admitted again on the resume's handout", got)
	}
	if row := rolePhaseRow(t, r.pool, r.key, claim.RoleArchitect); row.Entry != 2 {
		t.Fatalf("the architect's phases row after the answer names entry %d, want the 2 the resume raised", row.Entry)
	}
	r.runStarts()
	if p := r.machine().Claim().Pending; p == nil || p.ID != wakeID {
		t.Fatalf("the architect holds %#v after the resume's start, want the wake's task %s still pending", p, wakeID)
	}

	r.bringUp()
	prompts := r.prompts()[sent:]
	if len(prompts) != 1 || prompts[0].DeliveryID != wakeID || !strings.Contains(prompts[0].Message, wake) {
		t.Fatalf("prompts once the relaunched architect came up = %+v, want the wake's task %s alone", prompts, wakeID)
	}

	r.takeTheTask()
	r.runStarts()
	resume := fmt.Sprintf("A person answered your ask %s on %s", ask, r.key)
	prompts = r.prompts()[sent:]
	if len(prompts) != 2 || !strings.Contains(prompts[1].Message, resume) {
		t.Fatalf("prompts once the wake's turn ended and the resume's row ran again = %+v, want the resume's task after the wake's", prompts)
	}
	if p := r.machine().Claim().Pending; p == nil || p.Entry != 2 || p.Generation != 1 {
		t.Fatalf("the architect holds %#v, want the resume's task of entry 2", p)
	}
}

// The resume of a cancelled idle release (the outbox's releaseResume) is a start that carries no
// task and no entry. The start executor builds no delivery for a row with no task and hands the
// claim's StartedBy nil, so the row brings the claim back and takes nothing's place: run against a
// root architect's claim that holds the task of an entry unconfirmed, it leaves that task pending,
// under its own delivery id, and the task is the relaunched agent's first prompt.
//
// Two steps are fixtures: the claim is suspended by the role stop's suspend alone (release), and
// the resume is queued with no release marked or cancelled (queueReleaseResume). No idle pass is
// run, so whether one releases a claim that holds its entry's task unconfirmed is not shown here.
func TestAReleasesResumeLeavesTheTaskOfAnEntryTheClaimHolds(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	held := r.machine().Claim().Pending
	if held == nil || held.Entry != 1 || held.Generation != 1 || !held.ConfirmedAt.IsZero() {
		t.Fatalf("the architect's first task = %#v, want entry 1 of generation 1, sent and unconfirmed", held)
	}
	r.release()
	r.queueReleaseResume()
	sent := len(r.prompts())

	r.runStarts()
	after := r.machine().Claim()
	if p := after.Pending; p == nil || p.ID != held.ID || p.Entry != 1 || !p.ConfirmedAt.IsZero() {
		t.Fatalf("the architect holds %#v after the release's resume, want the task of entry 1 (%s) still pending", p, held.ID)
	}
	if after.GivenEntry != 1 || after.Released() {
		t.Fatalf("the architect after the release's resume = %s by %q given entry %d, want it brought back, given entry 1",
			after.State, after.SuspendedBy, after.GivenEntry)
	}

	r.bringUp()
	prompts := r.prompts()[sent:]
	if len(prompts) != 1 || prompts[0].DeliveryID != held.ID {
		t.Fatalf("prompts once the resumed architect came up = %+v, want the task of entry 1 (%s) alone", prompts, held.ID)
	}
}

// The sweep that would send a root architect the entry its phases row names holds that start back
// while the resume of a cancelled idle release is queued for the claim: the resume is a start of
// the role at the root's generation, so workflow.ArchitectStartPending answers "has a start still
// queued" although it carries no task and no entry. No pass takes that row out, rewrites it or
// retires it. It runs, brings the claim back and delivers nothing, and the pass after it sends the
// entry's work.
//
// The root is one whose resume's start was lost after a person answered it (dispatch://INFRA-2614
// R4), which is what leaves the sweep a start to write: its architect took entry 1's task, a
// person answered its question, the resume raised entry 2, and that start is lost before any
// lane runs it. Four steps are fixtures: the root's record is put in its wait directly, as
// answeredResumePath puts it; the start is lost by deleting it (dropArchitectStarts); the claim is
// suspended by the role stop's suspend alone (release); and the resume is queued with no release
// marked or cancelled (queueReleaseResume).
func TestTheSweepWaitsForAReleasesQueuedResumeAndRetiresNothingOfIt(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	r.takeTheTask()
	root := r.issue()
	root.Phase = phase.AwaitingPerson
	root.Awaiting = &record.Awaiting{Ask: mainPathAsk, Since: record.EventMark{Seq: r.nextSeq()}}
	r.put(root)
	r.recordClaim()
	r.apply("ask-answered", intake.DispatchAskClosed{Key: r.key, Ask: mainPathAsk, Kind: record.AskQuestion, State: "answered",
		Actor: alice, Mark: record.EventMark{Seq: r.nextSeq()}})
	dropArchitectStarts(t, r)
	if row := rolePhaseRow(t, r.pool, r.key, claim.RoleArchitect); row.Entry != 2 || r.machine().Claim().GivenEntry != 1 {
		t.Fatalf("the architect's row names entry %d and its claim was given %d, want 2 and 1: the case's premise",
			row.Entry, r.machine().Claim().GivenEntry)
	}
	r.release()
	resume := r.queueReleaseResume()

	if reason, pending := r.architectStartPending(); !pending || reason != "has a start still queued" {
		t.Fatalf("ArchitectStartPending beside the release's resume = %q, %t, want the start still queued", reason, pending)
	}
	for range 2 {
		r.admitPass()
	}
	starts := roleStartRows(t, r.pool, r.key, claim.RoleArchitect)
	if len(starts) != 1 || starts[0].row.ID != resume.row.ID || !reflect.DeepEqual(starts[0].request, resume.request) ||
		string(starts[0].row.Payload) != string(resume.row.Payload) {
		t.Fatalf("the architect starts after two passes = %+v, want the release's resume %d alone, as it was queued", starts, resume.row.ID)
	}

	sent := len(r.prompts())
	r.runStarts()
	if p := r.machine().Claim().Pending; p != nil {
		t.Fatalf("the architect holds %#v after the release's resume, want nothing: the resume carries no task", p)
	}
	// The relaunched agent's ready gives a claim that holds nothing the daemon's own delivery of
	// what the release's let-go ended (supervise's queueRelaunch), which is no entry's work; the
	// agent takes it, and nothing else reaches it before the sweep sends the entry.
	r.bringUp()
	answered := fmt.Sprintf("A person answered your ask %s on %s", mainPathAsk, r.key)
	for _, prompt := range r.prompts()[sent:] {
		if !strings.HasPrefix(prompt.DeliveryID, relaunchDeliveryPrefix) || strings.Contains(prompt.Message, answered) {
			t.Fatalf("the resumed architect was prompted %+v, want nothing but the record of the release's let-go before the sweep sends its entry",
				r.prompts()[sent:])
		}
	}
	r.takeTheTask()
	sent = len(r.prompts())

	r.admitPass()
	starts = roleStartRows(t, r.pool, r.key, claim.RoleArchitect)
	if len(starts) != 1 || starts[0].request.Entry != 2 || starts[0].request.Task == "" {
		t.Fatalf("the architect starts after the resume ran = %+v, want the one start of entry 2 the sweep writes", starts)
	}
	r.runStarts()
	if prompts := r.prompts()[sent:]; len(prompts) != 1 || !strings.Contains(prompts[0].Message, answered) {
		t.Fatalf("prompts once the sweep sent entry 2 = %+v, want the resume's task alone", prompts)
	}
}
