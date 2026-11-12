package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// startTaskNow is the text an architect start of key delivers: a resume carries the wait it
// answers and no text (record.SuperviseRequest.Awaiting), and its send writes it from the record
// (the daemon's rootTask, workflow.ResumeTask), so it is written here from the record as tx reads
// it, under steps; any other start carries its own text.
func startTaskNow(ctx context.Context, tx pgx.Tx, records record.Store, key string, steps []record.DesignStep, start record.SuperviseRequest) (string, error) {
	if start.Awaiting == nil {
		return start.Task, nil
	}
	gates, err := records.GatesOf(ctx, tx, key)
	if err != nil {
		return "", err
	}
	asks, err := records.RootAsks(ctx, tx, []string{key})
	if err != nil {
		return "", err
	}
	return workflow.ResumeTask(key, *start.Awaiting, steps, gates, asks[key]), nil
}

// resumeDocument is the primary document of mainPathRoot these tests register its gate on.
const resumeDocument = "artifact-451"

// putSpecGate writes the root's spec gate as the record holds it.
func (r *mainPathRig) putSpecGate(gate record.DesignGate) {
	r.t.Helper()
	gate.Issue, gate.Step, gate.ArtifactID = r.key, record.DesignStepSpec, resumeDocument
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error { return r.records.PutGate(r.ctx, tx, gate) }); err != nil {
		r.t.Fatalf("register the spec gate: %v", err)
	}
}

// review is a person's review or a new version of the root's primary document, as Dispatch's
// artifact event reaches the engine.
func (r *mainPathRig) review(kind intake.DispatchArtifactKind, version int, reason string) {
	r.t.Helper()
	seq := r.nextSeq()
	r.apply(fmt.Sprintf("artifact-%s-%d", kind, seq), intake.DispatchArtifact{Key: r.key, ArtifactID: resumeDocument, Kind: kind,
		Version: version, Reason: reason, Actor: alice, Mark: record.EventMark{Seq: seq}})
}

// waitOn puts the root in awaiting_person on waiting, its architect live and recorded.
func (r *mainPathRig) waitOn(waiting record.Awaiting) {
	r.t.Helper()
	root := r.waitingRoot()
	root.Status, root.Phase, root.Awaiting = "in_progress", phase.AwaitingPerson, &waiting
	r.put(root)
	r.launchArchitect()
	r.recordClaim()
}

// resumed is the architect start row the resume of the root queued, which must be the newest
// architect start of the root, and the wait it carries.
func (r *mainPathRig) resumed() (int64, record.SuperviseRequest) {
	r.t.Helper()
	if got := r.issue(); got.Phase != phase.Admitted || got.Awaiting != nil {
		r.t.Fatalf("the root after the answer = %+v, want it resumed at admitted", got)
	}
	var row record.OutboxRow
	if err := r.pool.QueryRow(r.ctx, `select id, kind, issue, payload from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' = 'start' and payload->>'role' = 'architect' order by id desc limit 1`, r.key).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
		r.t.Fatalf("read the resume's start row: %v", err)
	}
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.t.Fatalf("decode the resume's start row: %v", err)
	}
	return row.ID, payload.(record.SuperviseRequest)
}

// runStart runs the one architect start row id as the supervise lane runs it.
func (r *mainPathRig) runStart(id int64) {
	r.t.Helper()
	var row record.OutboxRow
	if err := r.pool.QueryRow(r.ctx, `select id, kind, issue, payload from outbox where id = $1`, id).Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
		r.t.Fatalf("read start row %d: %v", id, err)
	}
	row.LeaseToken = leaseOutboxRow(r.t, r.pool, row.ID)
	if err := r.runner.execute(r.ctx, row); err != nil {
		r.t.Fatalf("run start row %d: %v", row.ID, err)
	}
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error { return r.records.FinishOutbox(r.ctx, tx, row.ID, row.LeaseToken) }); err != nil {
		r.t.Fatalf("finish start row %d: %v", row.ID, err)
	}
	r.waitMachine()
}

// prompted is the one prompt the architect was sent.
func (r *mainPathRig) prompted() string {
	r.t.Helper()
	prompts := r.prompts()
	if len(prompts) != 1 {
		r.t.Fatalf("the architect was prompted %d times (%+v), want the one resume", len(prompts), prompts)
	}
	return prompts[0].Message
}

func mustName(t *testing.T, what, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Fatalf("%s %q, want it to name %q", what, text, w)
		}
	}
}

func mustNotName(t *testing.T, what, text string, not ...string) {
	t.Helper()
	for _, n := range not {
		if strings.Contains(text, n) {
			t.Fatalf("%s %q, want no %q", what, text, n)
		}
	}
}

// answeredByChanges is the replay's first half: the root waits on its gate at version 13, and the
// person's changes request of version 13 answers that wait, so admission resumes the root and
// queues its architect's resume. It returns that start row.
func answeredByChanges(r *mainPathRig) int64 {
	r.t.Helper()
	r.waitOn(record.Awaiting{GateVersion: 13, Artifact: resumeDocument, Since: record.EventMark{Seq: 10}})
	r.putSpecGate(record.DesignGate{LatestVersion: 13})
	r.review(intake.DispatchArtifactChangesRequested, 13, "split it")
	id, start := r.resumed()
	if start.Awaiting == nil || start.Task != "" || start.Awaiting.AnsweredBy != record.AnsweredByChanges {
		r.t.Fatalf("the resume's start = %+v, want the changes request's wait and no text", start)
	}
	return id
}

// The replay of dispatch://INFRA-3111. The root's wait was answered by a changes request of
// version 13, and the person wrote version 19 and approved it before the resume was delivered. The
// task the architect receives is written at its send, from the gate as it stands then, and the
// changes request the approval followed is not named (classify.ReviewSuperseded): it names the
// approval, and asks for no revision and no registration of version 13.
func TestAResumeAnsweredByAChangesRequestAnApprovalSupersededNamesTheApproval(t *testing.T) {
	r := newMainPathRig(t, true)
	id := answeredByChanges(r)
	r.review(intake.DispatchArtifactVersion, 19, "")
	r.review(intake.DispatchArtifactApproved, 19, "")
	r.runStart(id)
	task := r.prompted()
	mustName(t, "the resume task", task, "approved at version 19", "asks for no revision now", "design gate is open")
	mustNotName(t, "the resume task", task, "version 13", "revise", "register_gate")
	held := r.machine().Claim().Pending
	if held == nil || len(held.Resume) == 0 || held.Task == "" || !strings.Contains(task, held.Task) {
		t.Fatalf("the delivery after its send = %+v, want it to keep the wait it is written from and the text it was sent as", held)
	}
}

// The control of the replay: with no approval after it, the changes request is the person's
// newest review, and the resume task written at its send names it and asks for the revision.
func TestAResumeAnsweredByAChangesRequestStillStandingNamesIt(t *testing.T) {
	r := newMainPathRig(t, true)
	id := answeredByChanges(r)
	r.runStart(id)
	task := r.prompted()
	mustName(t, "the resume task", task, "requested changes to your primary document on LEGION-451 at version 13", "revise the primary document as they ask",
		"not approved at version 13")
}

// The replay of dispatch://INFRA-2964. Two approval requests were made for version 13: the first
// is still open on the person's turn in the ask mirror, the person approved the second. The resume
// a person's answer of another question queued while the gate was closed is written at its send,
// and names neither the request the approval answered nor an unapproved version.
func TestAResumeAfterTheSecondOfTwoRequestsOfOneVersionIsApprovedNamesNeitherStaleRequestNorUnapprovedVersion(t *testing.T) {
	r := newMainPathRig(t, true)
	r.waitOn(record.Awaiting{Ask: mainPathAsk, Since: record.EventMark{Seq: 10}})
	r.putSpecGate(record.DesignGate{LatestVersion: 13})
	const first, second = "ask-request-first", "ask-request-second"
	r.apply("opened:"+first, intake.DispatchAskOpened{Key: r.key, Ask: first, Kind: record.AskApproval, Artifact: resumeDocument,
		AuthorSession: "ses-architect", Mark: record.EventMark{Seq: r.nextSeq()}})
	r.apply("answered:"+mainPathAsk, intake.DispatchAskClosed{Key: r.key, Ask: mainPathAsk, Kind: record.AskQuestion, State: "answered",
		Actor: alice, Mark: record.EventMark{Seq: r.nextSeq()}})
	id, start := r.resumed()
	if start.Awaiting == nil || start.Task != "" {
		t.Fatalf("the resume's start = %+v, want the answered wait and no text", start)
	}
	r.apply("opened:"+second, intake.DispatchAskOpened{Key: r.key, Ask: second, Kind: record.AskApproval, Artifact: resumeDocument,
		AuthorSession: "ses-architect", Mark: record.EventMark{Seq: r.nextSeq()}})
	r.apply("answered:"+second, intake.DispatchAskClosed{Key: r.key, Ask: second, Kind: record.AskApproval, Artifact: resumeDocument, State: "answered",
		Actor: alice, Mark: record.EventMark{Seq: r.nextSeq()}})
	r.review(intake.DispatchArtifactApproved, 13, "")
	r.runStart(id)
	task := r.prompted()
	mustName(t, "the resume task", task, "A person answered your ask "+mainPathAsk, "approved at version 13")
	mustNotName(t, "the resume task", task, first, second, "not approved", "approval request", "register_gate")
}

// A resume and the catch-up of the same launch, both queued while the gate stood closed at version
// 13 and both delivered after the person wrote version 19 and approved it, state the same version:
// each is composed where it is delivered.
func TestAResumeAndTheCatchUpOfItsLaunchStateOneGateVersion(t *testing.T) {
	r := newMainPathRig(t, true)
	id := answeredByChanges(r)
	now := time.Now()
	keptCatchUp(t, r.pool, r.records, r.key, r.machine().Claim().Generation, now)
	r.review(intake.DispatchArtifactVersion, 19, "")
	r.review(intake.DispatchArtifactApproved, 19, "")
	r.runStart(id)
	task := r.prompted()
	publisher := &hookPublisher{}
	runner := &outbox{log: quietLogger(), pool: r.pool, dispatchProject: mainPathProject, records: r.records, notices: publisher,
		supervisor: r.sup, project: "legion", gatedSteps: config.DesignGateRootIssues.Steps(), now: func() time.Time { return now }}
	catchUps := catchUpRows(t, r.pool, r.key)
	if len(catchUps) != 1 {
		t.Fatalf("%d catch-ups of %s in the outbox, want the one of the launch", len(catchUps), r.key)
	}
	leased := catchUps[0]
	// The notices the answer and the reviews queued before the catch-up hold it behind them on its
	// lane; they are no part of what this reads, so they go.
	if _, err := r.pool.Exec(r.ctx, `delete from outbox where issue = $1 and kind = $2 and id < $3`, r.key, string(record.OutboxKindNotice), leased.ID); err != nil {
		t.Fatalf("clear the notices ahead of the catch-up: %v", err)
	}
	leased.LeaseToken = leaseOutboxRow(t, r.pool, leased.ID)
	decoded, err := record.DecodeOutboxPayload(leased)
	if err != nil {
		t.Fatalf("decode the catch-up: %v", err)
	}
	queued := decoded.(record.Notice)
	if err := runner.notice(r.ctx, leased, queued); err != nil {
		t.Fatalf("deliver the catch-up: %v", err)
	}
	published := publisher.published()
	if len(published) != 1 {
		t.Fatalf("the catch-up's delivery published %d notices, want the one catch-up", len(published))
	}
	gates := published[0].payload.(record.Notice).CatchUp.Gates
	if len(gates) != 1 || gates[0].Version != 19 || gates[0].Approved != 19 || !gates[0].Open {
		t.Fatalf("the delivered catch-up states the gates %+v, want the spec at version 19, approved at 19", gates)
	}
	mustName(t, "the resume task of the same launch", task, "approved at version 19")
	mustNotName(t, "the resume task of the same launch", task, "version 13")
}

// The kept-approval sentence a start names (workflow.KeptDecisionsTask: approved at 2, now at 3,
// request approval of 3) is asked again where the held task is sent: once the gate records the
// person's approval of version 3, the record no longer supports it, and the task is sent with the
// person's other decision and without it. With the gate as the re-admission left it, the sentence
// is sent.
func TestAKeptApprovalALaterApprovalSupersededIsDroppedFromTheHeldTask(t *testing.T) {
	const sentence = "request approval of version 3"
	for _, approved := range []bool{true, false} {
		t.Run(fmt.Sprintf("approved since %t", approved), func(t *testing.T) {
			r := newMainPathRig(t, true)
			r.put(r.waitingRoot())
			if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
				return r.records.KeepReadmittedApproval(r.ctx, tx, r.key, record.KeptApproval{Step: record.DesignStepSpec, Approved: 2, Latest: 3})
			}); err != nil {
				t.Fatalf("keep the re-admission's approval: %v", err)
			}
			ref := r.decide(mainPathAsk)
			r.admitPass()
			r.runStarts()
			if pending := r.machine().Claim().Pending; pending == nil || !strings.Contains(pending.Task, sentence) {
				t.Fatalf("the architect's pending task = %#v, want the start's task naming the kept approval", pending)
			}
			gate := record.DesignGate{LatestVersion: 3, ApprovedVersion: new(2)}
			if approved {
				gate.ApprovedVersion, gate.ApprovedMark = new(3), record.EventMark{Seq: r.nextSeq()}
			}
			r.putSpecGate(gate)
			r.bringUp()
			task := r.prompted()
			mustName(t, "the held task", task, ref)
			if strings.Contains(task, sentence) == approved {
				t.Fatalf("the held task %q names %q = %t, want %t", task, sentence, approved, !approved)
			}
		})
	}
}

// A held task a daemon of an earlier release queued carries no resume mark
// (supervise.Delivery.Resume): it is sent as the text it holds, whatever the gate says now.
func TestAHeldTaskWithNoResumeMarkIsSentAsItsText(t *testing.T) {
	r := newMainPathRig(t, true)
	root := r.waitingRoot()
	root.Status = "in_progress"
	r.put(root)
	r.putSpecGate(record.DesignGate{LatestVersion: 13, ApprovedVersion: new(13), ApprovedMark: record.EventMark{Seq: 20}})
	r.launchArchitect()
	const text = "Your primary document is not approved at version 13. Continue."
	if err := r.machine().Handle(r.ctx, supervise.RequestDeliver{Claim: r.token, Task: text, Generation: 1, ID: "outbox:7", Row: 7}); err != nil {
		t.Fatalf("queue the earlier release's task: %v", err)
	}
	r.waitMachine()
	if got := r.prompted(); got != text {
		t.Fatalf("the earlier release's held task was sent as %q, want its own text %q", got, text)
	}
	var marked bool
	if err := r.pool.QueryRow(context.Background(), `select resume is not null from pending_task_deliveries where claim_token = $1`, string(r.token)).Scan(&marked); err != nil || marked {
		t.Fatalf("the stored delivery carries a resume mark = %t (%v), want none", marked, err)
	}
}
