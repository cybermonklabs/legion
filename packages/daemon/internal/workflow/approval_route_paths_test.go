package workflow_test

// This file is the external test package so that its rows can drive admission's real
// re-admission and promotion (admit.Admission), which imports this package.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The trees of these rows are a neutral project's, ACME, as every example of this public
// repository is, and they are the trees api's approval_test.go names. ownRoot is the root of F3
// (testdata/dispatch-carried-approval.json) and ownDocument its spec; ownChild is a Dispatch child
// of it. otherRoot is a root whose tree a hand-over left, and carriedRoot the root that hand-over
// made of its work, which carries a copy of otherRoot's gate. architectSession is the session of
// ownRoot's architect, which a question its tree opens names.
const (
	pathProject      = "ACME"
	ownRoot          = "ACME-2694"
	ownDocument      = "aec8fb48-fd46-458a-99a3-a1b199a46b38"
	ownChild         = "ACME-2695"
	otherRoot        = "ACME-900"
	otherDocument    = "5b0f3c2e-7d14-4a9b-8e61-2c9a7f4d1b03"
	carriedRoot      = "ACME-950"
	architectSession = "ses-architect-2694"
)

var (
	// pathSteps is the deployment's `gates.design: root-issues`, which gates the spec step alone.
	pathSteps = config.DesignGateRootIssues.Steps()
	// pathPerson is the person whose reviews and answers the facts carry.
	pathPerson = dispatch.Actor{Kind: "user", ID: "person@example.com"}
	// reviewedFrom is the moment of Dispatch's event at sequence 0; the event at sequence n is n
	// seconds after it (pathRig.mark).
	reviewedFrom = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	// decidedAt is the moment the outbox relays a decision nobody in its tree hears (relay).
	decidedAt = time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)
	// updateEntries rewrites the entries under approvalEntries from what each path writes now.
	updateEntries = flag.Bool("update", false, "rewrite testdata/approval-entries from the entries the approval paths write")
)

// approvalEntries holds one file for each row of TestApprovalRouteAnswersAfterEveryPathThatTellsAnApproval:
// the entry that row's path gives the architect, as the daemon renders it. The capture of check
// e4b07a29 (plan task t-12) sends each file to a real Oh My Pi session, so a file holds exactly
// what reaches the session, with no line added:
//
//   - a notice (a file named notice-…): the card's summary line, `<kind> on <issue>` (the daemon's
//     noticeSummary), then a newline, then the JSON payload the notice publishes
//     (record.Notice.Published, encoded as notify.HTTPPublisher encodes it);
//   - a role message (role-message-…): the body the role's topic is sent;
//   - a task (task-…): the text the architect's start carries (record.SuperviseRequest.Task), or,
//     for a decision kept while nobody in the tree could hear it, the text the next start or
//     catch-up names it in (workflow.ParkedDecisionTask).
const approvalEntries = "testdata/approval-entries"

// approvalPath is one row of the inventory: a path by which the daemon tells an architect that a
// person approved a spec. name is the subtest and the entry file; notice is the kind of the notice
// the entry is, empty for a role message or a task; drive drives the path, holds the route's
// answer at the commit of the path's transaction, and answers the entry.
type approvalPath struct {
	name   string
	notice record.NoticeKind
	drive  func(r *pathRig) string
}

// approvalPaths is every path that tells an architect of an approval, the rows of inventory test
// I1 of the approval route's plan (task t-9), the six paths of the spec's Problem among them.
var approvalPaths = []approvalPath{
	// Path 1: a person's approval of the root's own spec opens its gate (applyArtifact).
	{name: "notice-design-approved-from-artifact-approved", notice: "design-approved", drive: func(r *pathRig) string {
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.noticeEntry(ownRoot, "design-approved")
	}},
	// Path 1: a registration whose read of Dispatch carries the person's approval of the version it
	// registers (gateRegistered).
	{name: "notice-design-approved-from-a-registration", notice: "design-approved", drive: func(r *pathRig) string {
		r.recordIssue(rootIssue(ownRoot))
		r.openTree(ownRoot)
		approved := r.next()
		r.apply(intake.GateRegistered{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, Version: 3, DispatchVersion: 3,
			ApprovedVersion: 3, ApprovedMark: r.mark(approved), ReviewedBy: pathPerson, Since: record.EventMark{Seq: r.next()}})
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.noticeEntry(ownRoot, "design-approved")
	}},
	// Path 2: a person's approval of the other root's spec opens the copy a hand-over carried, and
	// the notice names that other root's spec (applyArtifact, copyTakes).
	{name: "notice-carried-gate-opened", notice: "carried-gate-opened", drive: func(r *pathRig) string {
		r.carried(record.DesignGate{LatestVersion: 12})
		r.review(otherRoot, otherDocument, intake.DispatchArtifactApproved, 12)
		r.wantCarried(carriedRoot, otherRoot, otherDocument, 12)
		return r.noticeEntry(carriedRoot, "carried-gate-opened")
	}},
	// Path 2 and 6: the same opening of the copy a carried root holds while it is out of the
	// workflow, in the backlog, the icebox or triage. Its architect stopped with its tree, so the
	// outbox relays the notice's decision (record.SpecDecision's carried-gate-opened) and keeps it
	// for the root's next start or catch-up, which names it. The approval is of the other root's
	// document, so the carried root's status only takes it out of the workflow.
	{name: "task-carried-gate-opened-kept-for-a-carried-root-in-backlog", drive: keptForACarriedRoot("backlog")},
	{name: "task-carried-gate-opened-kept-for-a-carried-root-in-icebox", drive: keptForACarriedRoot("icebox")},
	{name: "task-carried-gate-opened-kept-for-a-carried-root-in-triage", drive: keptForACarriedRoot("triage")},
	// Path 5: the start task of the root a hand-over carried in with a copy carried open, which the
	// re-admission's promotion writes (Engine.StartArchitect, writeCarriedPhase).
	{name: "task-start-of-a-carried-root", drive: func(r *pathRig) string {
		r.carried(record.DesignGate{LatestVersion: 12, ApprovedVersion: new(12), ApprovedMark: r.mark(r.next())})
		r.wantCarried(carriedRoot, otherRoot, otherDocument, 12)
		return r.startTaskEntry(carriedRoot)
	}},
	// Path 6: the catch-up of that carried root at its architect's launch (QueueCatchUp), which
	// states the copy approved and open.
	{name: "notice-catch-up-of-a-carried-root", notice: "catch-up", drive: func(r *pathRig) string {
		r.carried(record.DesignGate{LatestVersion: 12, ApprovedVersion: new(12), ApprovedMark: r.mark(r.next())})
		r.ready(carriedRoot)
		r.wantCarried(carriedRoot, otherRoot, otherDocument, 12)
		return r.noticeEntry(carriedRoot, "catch-up")
	}},
	// Path 3: the role message that the gate opened, which the root's architect is sent last
	// (tellGateMoved).
	{name: "role-message-gate-opened", drive: func(r *pathRig) string {
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.roleMessageEntry(ownRoot)
	}},
	// Path 4: a root waiting on its gate is answered by the approval (answerAwaitedGate), and
	// admission's promotion resumes it with the design-approved wake (ResumeAwaitedRoot).
	{name: "task-resume-answer-awaited-gate", drive: func(r *pathRig) string {
		r.awaiting(record.Awaiting{GateVersion: 3, Artifact: ownDocument, Since: r.mark(r.next())})
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.startTaskEntry(ownRoot)
	}},
	// Path 4: a re-admitted root whose architect handed two questions back while it still owed its
	// start task, and a person answered one before the architect took that task: admission's pass
	// on the confirmation gives the architect the resume task where it stands (tellInPlace). The
	// approval its re-admission recorded stands throughout.
	{name: "task-resume-tell-in-place", drive: func(r *pathRig) string {
		r.closedRoot()
		r.todo(ownRoot)
		held, delivery := r.deliverStart(ownRoot)
		r.apply(intake.DispatchAskOpened{Key: ownRoot, Ask: "ask-q1", Kind: record.AskQuestion, AuthorSession: architectSession, Mark: r.mark(r.next())})
		r.apply(intake.DispatchAskOpened{Key: ownRoot, Ask: "ask-q2", Kind: record.AskQuestion, AuthorSession: architectSession, Mark: r.mark(r.next())})
		r.apply(intake.DispatchAskClosed{Key: ownRoot, Ask: "ask-q1", Kind: record.AskQuestion, State: "answered", Actor: pathPerson, Mark: r.mark(r.next())})
		if root := r.issue(ownRoot); root.DeclaredSeq == 0 || root.AwaitingPerson() {
			r.t.Fatalf("%s before its architect took its task = %+v, want its hand-back declared and no wait begun", ownRoot, root)
		}
		r.confirm(held, delivery)
		r.wantOwn(ownRoot, ownDocument, 2)
		return r.startTaskEntry(ownRoot)
	}},
	// Path 4: a root waiting on a question whose gate a person's approval opens, which Dispatch
	// recorded before the wait began and the daemon applied after it: the approval answers no wait
	// of its own, and admission's pass answers the open gate (answerOpenGates) and resumes it.
	{name: "task-resume-answer-open-gates", drive: func(r *pathRig) string {
		approved, since := r.next(), r.next()
		r.awaiting(record.Awaiting{Ask: "ask-q", Since: r.mark(since)})
		r.write(func(tx pgx.Tx) error {
			return r.records.PutRootAsk(r.ctx, tx, record.RootAsk{Issue: ownRoot, Ask: "ask-q", Kind: record.AskQuestion, Open: true,
				Seq: since, WaitingOn: "human", WaitingOnSeq: since, ByTree: true})
		})
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3, approved)
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.startTaskEntry(ownRoot)
	}},
	// Path 5: the start task of a root at admitted whose gate is open (writeGateOpen,
	// gateStanding): its first phase waits for an open Dispatch child, and the sweep raises the
	// entry of an architect whose phases row names none (Engine.StartArchitect).
	{name: "task-start-of-a-root-whose-gate-is-open", drive: func(r *pathRig) string {
		r.slotted(ownRoot)
		r.write(func(tx pgx.Tx) error {
			return r.records.PutGate(r.ctx, tx, record.DesignGate{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, LatestVersion: 3})
		})
		r.mirror(ownRoot, "in_progress", "")
		r.mirror(ownChild, "backlog", ownRoot)
		r.putClaim(r.architectClaim(ownRoot))
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		if got := r.issue(ownRoot).Phase; got != phase.Admitted {
			r.t.Fatalf("%s at its gate's opening stands in %s, want admitted while its open child goes first", ownRoot, got)
		}
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.startTaskEntry(ownRoot)
	}},
	// Path 6: the catch-up at the launch of the architect of a root whose gate is open
	// (QueueCatchUp, treeCatchUp).
	{name: "notice-catch-up-at-a-launch", notice: "catch-up", drive: func(r *pathRig) string {
		r.root(ownRoot, ownDocument, 3)
		r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 3)
		r.ready(ownRoot)
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.noticeEntry(ownRoot, "catch-up")
	}},
	// Path 6: a person's approval of the spec of a root whose tree closed as done: nobody in the
	// tree hears the design-approved notice, so the outbox relays it and keeps the decision for the
	// root's next start (record.RelayParkedDecision), which names it (KeptDecisionsTask).
	{name: "task-design-approved-kept-for-a-root-closed-as-done", drive: func(r *pathRig) string {
		r.closedRoot()
		r.relay(ownRoot, "design-approved")
		r.wantOwn(ownRoot, ownDocument, 2)
		return r.keptTask(ownRoot)
	}},
	// Path 6: the same decision kept for a root a person moved to the backlog, the icebox or
	// triage. In the backlog and the icebox a fresh approval queues a handover and keeps no
	// decision, so the decision is the one a refused handover keeps (Engine.refuseHandover); in
	// triage the gate takes the approval, as a closed root's does, and the outbox keeps it.
	{name: "task-design-approved-kept-for-a-root-in-backlog", drive: keptForARootOutOfTheWorkflow("backlog")},
	{name: "task-design-approved-kept-for-a-root-in-icebox", drive: keptForARootOutOfTheWorkflow("icebox")},
	{name: "task-design-approved-kept-for-a-root-in-triage", drive: keptForARootOutOfTheWorkflow("triage")},
	// Path 5 and 6: the root closed as done, re-admitted after a new version of its spec, whose
	// start task names the kept decision and the approval the version left below it
	// (KeptDecisionsTask). The re-admission cleared the gate row, and the route answers from the
	// record it left, before the start task is told and after its confirmation told what it named.
	{name: "task-start-of-the-readmitted-root", drive: func(r *pathRig) string {
		r.closedRoot()
		r.relay(ownRoot, "design-approved")
		r.review(ownRoot, ownDocument, intake.DispatchArtifactVersion, 3)
		r.todo(ownRoot)
		r.wantOwn(ownRoot, ownDocument, 2)
		entry := r.startTaskEntry(ownRoot)
		held, delivery := r.deliverStart(ownRoot)
		r.confirm(held, delivery)
		if kept := r.keptRows(ownRoot); kept != 0 {
			r.t.Fatalf("%s keeps %d decisions after its start task was told, want none: the confirmation tells what the task named", ownRoot, kept)
		}
		r.wantOwn(ownRoot, ownDocument, 2)
		return entry
	}},
	// Path 1: an exact undo. A person approved version 24, an edit wrote version 45 and its exact
	// undo version 46, whose text is version 24's, and Dispatch carried the approval to 46 (F3).
	{name: "notice-design-approved-after-an-exact-undo", notice: "design-approved", drive: func(r *pathRig) string {
		r.root(ownRoot, ownDocument, 24)
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactApproved, 24, 1300)
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactVersion, 45, 1440)
		r.reviewAt(ownRoot, ownDocument, intake.DispatchArtifactVersion, 46, 1456)
		r.apply(workflow.CarriedApprovalCapture(r.t))
		r.wantOwn(ownRoot, ownDocument, 46)
		return r.noticeEntry(ownRoot, "design-approved")
	}},
	// Path 1: the gate seed a registration queued reads the person's approval from Dispatch, which
	// the approval's own event never brought.
	{name: "notice-design-approved-from-the-gate-seed", notice: "design-approved", drive: func(r *pathRig) string {
		r.root(ownRoot, ownDocument, 3)
		r.seed(ownRoot, 3)
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.noticeEntry(ownRoot, "design-approved")
	}},
	// Path 1: the gate seed of a gate recorded above the version Dispatch holds resets it to that
	// version with the approval it read (gateAboveDispatch).
	{name: "notice-design-approved-from-gate-above-dispatch", notice: "design-approved", drive: func(r *pathRig) string {
		r.recordIssue(rootIssue(ownRoot))
		r.openTree(ownRoot)
		// The caller's version, registered unchecked, as a daemon that did not compare it with
		// Dispatch's recorded it.
		r.apply(intake.GateRegistered{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, Version: 5, Since: record.EventMark{Seq: r.seq}})
		r.seed(ownRoot, 4)
		r.wantOwn(ownRoot, ownDocument, 4)
		return r.noticeEntry(ownRoot, "design-approved")
	}},
	// Path 4: the boot's catch-up applies the approval a root waiting on its gate missed while the
	// daemon was down, holding the root out of admission's decision until it has applied the window,
	// and admission resumes the root on the fact that finishes the catch-up (CatchUpApplied).
	{name: "task-resume-boot-catch-up", drive: func(r *pathRig) string {
		r.awaiting(record.Awaiting{GateVersion: 3, Artifact: ownDocument, Since: r.mark(r.next())})
		r.admission.HoldForCatchUp(ownRoot)
		seq := r.next()
		fact, unread, err := intake.DecodeDispatchEvent(pathProject, ownRoot, dispatchApproval(ownRoot, ownDocument, 3, r.mark(seq)))
		if err != nil || len(unread) > 0 {
			r.t.Fatalf("decode the approval event = %v, unread %v", err, unread)
		}
		generation := r.issue(ownRoot).Generation
		r.applyAs("daemon", fmt.Sprintf("awaiting-catch-up:event:%s:%d:%d", ownRoot, generation, seq), fact)
		if _, _, found := r.startRow(ownRoot); found {
			r.t.Fatalf("%s was resumed while the boot's catch-up held it, want its resume decided once the window is applied", ownRoot)
		}
		r.admission.ReleaseCatchUp(ownRoot)
		r.applyAs("daemon", fmt.Sprintf("awaiting-catch-up:caught-up:%s:%d:%d:boot", ownRoot, generation, seq),
			intake.CatchUpApplied{Issue: ownRoot, Through: seq})
		r.wantOwn(ownRoot, ownDocument, 3)
		return r.startTaskEntry(ownRoot)
	}},
}

// keptForACarriedRoot drives path 2 and 6 for a carried root that is out of the workflow in
// status (backlog, icebox or triage): a person's approval of the other root's spec opens the copy
// the carried root holds, its architect stopped with its tree, and the outbox's relay keeps the
// decision for the root's next start, whose task the row answers.
func keptForACarriedRoot(status string) func(r *pathRig) string {
	return func(r *pathRig) string {
		r.carried(record.DesignGate{LatestVersion: 12})
		r.lingering(carriedRoot, status)
		r.review(otherRoot, otherDocument, intake.DispatchArtifactApproved, 12)
		r.relay(carriedRoot, "carried-gate-opened")
		r.wantCarried(carriedRoot, otherRoot, otherDocument, 12)
		return r.keptTask(carriedRoot)
	}
}

// keptForARootOutOfTheWorkflow drives path 6 for ownRoot, a root whose tree ran and that a person
// moved to status (backlog, icebox or triage), where a person approves version 2 of its spec and
// the approval leaves one kept decision for the root's next start, whose task the row answers.
//
// In the backlog and the icebox a fresh approval queues a person's handover, which keeps no
// decision, so the decision is the one a refused handover keeps: the approval's fact carries a
// change of the root's status after it (StatusChangedSeq), so the engine hands the approval to
// record.RelayParkedDecision in the fact's own transaction (Engine.refuseHandover) and queues no
// handover. In triage the approval queues no handover (intake.DispatchArtifact.MayQueueHandover),
// so the root's gate takes it, as a closed root's gate does, and the design-approved notice it
// writes finds nobody to hear it: the outbox's relay keeps the decision.
func keptForARootOutOfTheWorkflow(status string) func(r *pathRig) string {
	return func(r *pathRig) string {
		r.ranRoot(status)
		switch status {
		case "backlog", "icebox":
			r.approvedOutOfWorkflow(status, true)
		case "triage":
			r.approvedOutOfWorkflow(status, false)
			r.relay(ownRoot, "design-approved")
		default:
			r.t.Fatalf("no path of an approval is known for the status %s", status)
		}
		if pending := r.handoverPending(ownRoot); pending {
			r.t.Fatalf("the approval of %s in %s left a person's handover pending, want none: it keeps the decision instead", ownRoot, status)
		}
		if kept := r.keptRows(ownRoot); kept != 1 {
			r.t.Fatalf("%s in %s keeps %d decisions after the approval, want its one", ownRoot, status, kept)
		}
		r.wantOwn(ownRoot, ownDocument, 2)
		return r.keptTask(ownRoot)
	}
}

// Every path by which the daemon tells an architect that a person approved a spec leaves that
// approval standing for the architect's claim at the commit of the transaction that writes the
// path's entry: the approval route's own function (api.ClaimApprovalOf), read in a transaction of
// its own right after, answers that version in R1's words (prompts.ApprovalMessage). The plugin
// reads the route again after such an entry reaches the session, so that read sees the approval.
// Each row also writes its entry, as the daemon renders it, under approvalEntries, which the
// capture of check e4b07a29 sends to a real session; the file must equal what the path writes, so
// `-update` rewrites the files and every other run compares them.
func TestApprovalRouteAnswersAfterEveryPathThatTellsAnApproval(t *testing.T) {
	for _, path := range approvalPaths {
		t.Run(path.name, func(t *testing.T) {
			entry := path.drive(newPathRig(t))
			if path.notice != "" {
				if kind, _, _ := strings.Cut(entry, " on "); kind != string(path.notice) {
					t.Fatalf("the entry of %s is a notice of %q, want %q:\n%s", path.name, kind, path.notice, entry)
				}
			}
			holdEntry(t, path.name, entry)
		})
	}
}

// noticeClass is what noticeClasses says of one notice kind: whether a notice of it tells an
// architect that a person approved a spec, and the rows of approvalPaths that drive it.
type noticeClass struct {
	tells bool
	rows  []string
}

// noticeClasses names every notice kind an architect is told (record.NoticeKinds), with whether it
// tells an approval and, where it does, the rows that cover it.
var noticeClasses = map[record.NoticeKind]noticeClass{
	"design-approved": {tells: true, rows: []string{
		"notice-design-approved-from-artifact-approved", "notice-design-approved-from-a-registration",
		"notice-design-approved-after-an-exact-undo", "notice-design-approved-from-the-gate-seed",
		"notice-design-approved-from-gate-above-dispatch",
	}},
	"carried-gate-opened": {tells: true, rows: []string{"notice-carried-gate-opened"}},
	// A catch-up states each step of the root's gate, the version a person approved among it.
	"catch-up": {tells: true, rows: []string{"notice-catch-up-of-a-carried-root", "notice-catch-up-at-a-launch"}},
	// A changes request ends the approval it answers, so it tells none.
	"design-changes-requested": {},
	// Every other kind is the machinery's, and says nothing of a spec's approval.
	"phase-finished": {}, "worker-died": {}, "worker-stalled": {}, "worker-silent": {}, "worker-background-stalled": {}, "held": {},
	"pr-blocked": {}, "pr-merged": {}, "pr-closed-unmerged": {}, "pull-request-missing": {}, "message-refused": {},
	"child-closed": {}, "child-status": {}, "children-first": {}, "child-release-stalled": {}, "child-not-recorded": {},
	"checks-red": {}, "review-stuck": {}, "status-reasserted": {}, record.VolumeNearFullNotice: {},
	record.WorkspaceKeptNotice: {}, record.WorkspaceTakenNotice: {},
	"session-lost": {}, "session-forgotten": {}, "session-store-fault": {},
	"task-stopped": {}, "plan-not-taken": {}, "retro-held": {},
}

// Every notice kind the daemon tells an architect is classified as telling an approval or not, so
// a kind added to the daemon's list reaches this inventory: one that tells an approval is covered
// by a row of TestApprovalRouteAnswersAfterEveryPathThatTellsAnApproval whose entry is a notice of
// that kind, and no row's entry is a notice of a kind that tells none.
func TestApprovalRouteClassifiesEveryNoticeKind(t *testing.T) {
	kinds := record.NoticeKinds()
	for _, kind := range kinds {
		class, ok := noticeClasses[kind]
		if !ok {
			t.Errorf("notice kind %q is not classified: name it in noticeClasses with whether it tells an architect of an approval", kind)
			continue
		}
		if class.tells && len(class.rows) == 0 {
			t.Errorf("notice kind %q tells an approval and no row of approvalPaths covers it", kind)
		}
		for _, name := range class.rows {
			i := slices.IndexFunc(approvalPaths, func(path approvalPath) bool { return path.name == name })
			if i < 0 || approvalPaths[i].notice != kind {
				t.Errorf("notice kind %q names the row %q, which is no row of approvalPaths whose entry is a notice of that kind", kind, name)
			}
		}
		for _, path := range approvalPaths {
			if path.notice == kind && !slices.Contains(class.rows, path.name) {
				t.Errorf("the row %q writes a notice of kind %q, which noticeClasses does not name as covered by it", path.name, kind)
			}
		}
	}
	for kind := range noticeClasses {
		if !slices.Contains(kinds, kind) {
			t.Errorf("noticeClasses classifies %q, which is no notice kind the daemon tells an architect", kind)
		}
	}
}

// holdEntry holds the entry file of the row name to entry: `-update` writes it, and every other
// run fails where it differs.
func holdEntry(t *testing.T, name, entry string) {
	t.Helper()
	path := filepath.Join(approvalEntries, name+".txt")
	if *updateEntries {
		if err := os.MkdirAll(approvalEntries, 0o755); err != nil {
			t.Fatalf("create %s: %v", approvalEntries, err)
		}
		if err := os.WriteFile(path, []byte(entry), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (write it with `go test ./internal/workflow -run '^TestApprovalRoute' -update`): %v", path, err)
	}
	if string(want) != entry {
		t.Fatalf("%s is stale: the path writes\n%s\n\nthe file holds\n%s\n\n(rewrite it with -update)", path, entry, want)
	}
}

// pathRig is the daemon's own handler chain over ACME's records, in the order the daemon runs it
// (daemon/workflow.go): the Dispatch mirror, the workflow engine and admission.
type pathRig struct {
	t         *testing.T
	ctx       context.Context
	store     *store.Store
	records   *record.Postgres
	admission *admit.Admission
	handlers  []intake.Handler
	// seq is the newest Dispatch event sequence a fact has carried.
	seq int64
	// facts counts the facts applied, which names each one.
	facts int
}

// newPathRig is the rig of a deployment that gates the spec step, over a database of its own.
func newPathRig(t *testing.T) *pathRig {
	t.Helper()
	records := record.NewStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := workflow.New(records, workflow.Config{Project: pathProject, Steps: pathSteps, ReviewRoundCap: 3, Linger: time.Hour,
		Repositories: pathRepositories{}}, log)
	admission := admit.New(records, engine, 4, pathProject, log)
	return &pathRig{t: t, ctx: context.Background(), store: testpg.CloneStore(t, "legion_workflow_test"), records: records, admission: admission,
		handlers: []intake.Handler{blockers.NewMirror(records, pathProject), engine, admission}, seq: 100}
}

// pathRepositories answers acme/widgets, the repository of every issue here, at main.
type pathRepositories struct{}

func (pathRepositories) ResolveRepository(resolved string) (workflow.Resolution, error) {
	if resolved == "acme/widgets" {
		return workflow.Resolution{Repo: resolved, DefaultBranch: "main", GitHubRepo: resolved}, nil
	}
	return workflow.Resolution{Refusal: "GitHub has not been read for " + resolved + " yet", Unread: true}, nil
}

// next is the sequence of the next Dispatch event.
func (r *pathRig) next() int64 {
	r.seq++
	return r.seq
}

// mark is the place of the event at seq in its issue's event log.
func (r *pathRig) mark(seq int64) record.EventMark {
	return record.EventMark{Seq: seq, At: reviewedFrom.Add(time.Duration(seq) * time.Second)}
}

// apply runs fact through the handler chain in one transaction, as the daemon's intake does.
func (r *pathRig) apply(fact intake.Fact) {
	r.t.Helper()
	r.facts++
	r.applyAs("dispatch", fmt.Sprintf("path-fact-%d", r.facts), fact)
}

// applyAs is apply under the source and id of the daemon's own caller of the fact.
func (r *pathRig) applyAs(source, id string, fact intake.Fact) {
	r.t.Helper()
	result, err := intake.ApplyFact(r.ctx, r.store.Pool(), source, id, fact, r.handlers...)
	if err != nil {
		r.t.Fatalf("apply %T (%s): %v", fact, id, err)
	}
	if result.Refusal != nil {
		r.t.Fatalf("%T (%s) was refused: %+v", fact, id, *result.Refusal)
	}
}

// write runs write in a transaction of its own.
func (r *pathRig) write(write func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.store.Pool(), func(_ context.Context, tx pgx.Tx) error { return write(tx) }); err != nil {
		r.t.Fatalf("write the records: %v", err)
	}
}

// read runs read in a read-only transaction of its own.
func (r *pathRig) read(read func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := store.TransactWith(r.ctx, r.store.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(_ context.Context, tx pgx.Tx) error { return read(tx) }); err != nil {
		r.t.Fatalf("read the records: %v", err)
	}
}

// stored is the record of key, nil when there is none.
func (r *pathRig) stored(key string) *record.Issue {
	r.t.Helper()
	var issue *record.Issue
	r.read(func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(r.ctx, tx, key)
		return err
	})
	return issue
}

// issue is the record of key.
func (r *pathRig) issue(key string) record.Issue {
	r.t.Helper()
	issue := r.stored(key)
	if issue == nil {
		r.t.Fatalf("no record of %s", key)
	}
	return *issue
}

// rootIssue is key as the record holds an admitted root of its own tree at generation 1.
func rootIssue(key string) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: pathProject,
		Title: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", HandedOver: true,
		LastDispatchSeq: 1}
}

// recordIssue records issue as it stands.
func (r *pathRig) recordIssue(issue record.Issue) {
	r.t.Helper()
	r.write(func(tx pgx.Tx) error { return r.records.PutIssue(r.ctx, tx, issue) })
}

// openTree opens the lifecycle of tree under the workflow, as the promotion that admits its root
// does.
func (r *pathRig) openTree(tree string) {
	r.t.Helper()
	project, err := claim.ProjectToken(pathProject)
	if err != nil {
		r.t.Fatalf("project token of %s: %v", pathProject, err)
	}
	r.write(func(tx pgx.Tx) error {
		_, err := r.records.OpenTreeLifecycle(r.ctx, tx, project, tree, treelifecycle.AuthorityWorkflow)
		return err
	})
}

// slotted records key as an admitted root whose tree runs, holding one of the cap's slots.
func (r *pathRig) slotted(key string) {
	r.t.Helper()
	r.recordIssue(rootIssue(key))
	r.openTree(key)
	r.write(func(tx pgx.Tx) error {
		return r.records.PutSlot(r.ctx, tx, record.Slot{Issue: key, Index: 0, AdmittedAt: reviewedFrom})
	})
}

// mirror writes what Dispatch says of key, its status and its parent, as the mirror writes it from
// an event at sequence 1.
func (r *pathRig) mirror(key, status, parent string) {
	r.t.Helper()
	r.write(func(tx pgx.Tx) error {
		return r.records.PutDispatchIssue(r.ctx, tx, record.DispatchReading{Key: key, Project: pathProject, Seq: 1,
			Status: record.Carry(status), Parent: record.Carry(parent), Blockers: record.Carry([]string(nil))})
	})
}

// root records key as an admitted root of its own live tree whose architect registered version of
// document as its spec, as register_gate's fact carries it with no review read.
func (r *pathRig) root(key, document string, version int) {
	r.t.Helper()
	r.recordIssue(rootIssue(key))
	r.openTree(key)
	r.apply(intake.GateRegistered{Issue: key, Step: record.DesignStepSpec, ArtifactID: document, Version: version, DispatchVersion: version,
		Since: record.EventMark{Seq: r.seq}})
}

// awaiting records ownRoot as a root of a running tree waiting on a person on waiting, holding
// its slot, its spec registered at version 3 and not approved.
func (r *pathRig) awaiting(waiting record.Awaiting) {
	r.t.Helper()
	r.slotted(ownRoot)
	root := rootIssue(ownRoot)
	root.Phase, root.Awaiting = phase.AwaitingPerson, &waiting
	r.recordIssue(root)
	r.write(func(tx pgx.Tx) error {
		return r.records.PutGate(r.ctx, tx, record.DesignGate{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, LatestVersion: 3})
	})
}

// review applies a person's review or a new version of key's document, at the next sequence.
func (r *pathRig) review(key, document string, kind intake.DispatchArtifactKind, version int) {
	r.t.Helper()
	r.reviewAt(key, document, kind, version, r.next())
}

// reviewAt is review at seq.
func (r *pathRig) reviewAt(key, document string, kind intake.DispatchArtifactKind, version int, seq int64) {
	r.t.Helper()
	fact := intake.DispatchArtifact{Key: key, ArtifactID: document, Kind: kind, Version: version, Mark: r.mark(seq)}
	if kind == intake.DispatchArtifactApproved {
		fact.Actor = pathPerson
	}
	r.apply(fact)
}

// lingering records key as a root whose tree left the workflow: its phase done, its linger armed,
// in status, keeping the rest of the record key holds, or at generation 1 where it has none.
func (r *pathRig) lingering(key, status string) {
	r.t.Helper()
	until := time.Now().Add(time.Hour)
	issue := rootIssue(key)
	if stored := r.stored(key); stored != nil {
		issue = *stored
	}
	issue.Phase, issue.Status, issue.DispatchStatus, issue.LingerUntil = phase.Done, status, status, &until
	r.recordIssue(issue)
}

// ranRoot is ownRoot as a root whose tree ran and then left the workflow in status (backlog,
// icebox, triage or done): its run stood in a phase of its own (record.Issue.TakenBack, which its
// re-admission keeps), and its spec row holds version 2 as its run registered it.
func (r *pathRig) ranRoot(status string) {
	r.t.Helper()
	ran := rootIssue(ownRoot)
	ran.TakenBack = true
	r.recordIssue(ran)
	r.lingering(ownRoot, status)
	r.write(func(tx pgx.Tx) error {
		return r.records.PutGate(r.ctx, tx, record.DesignGate{Issue: ownRoot, Step: record.DesignStepSpec, ArtifactID: ownDocument, LatestVersion: 2})
	})
}

// closedRoot is ownRoot as a root whose tree ran and closed as done (ranRoot). Then a person
// approves version 2: the gate of a closed tree takes it (a root closed as done is in neither the
// backlog nor the icebox, so the approval queues no handover), and the design-approved notice it
// writes finds no architect to hear it.
func (r *pathRig) closedRoot() {
	r.t.Helper()
	r.ranRoot("done")
	r.review(ownRoot, ownDocument, intake.DispatchArtifactApproved, 2)
}

// approvedOutOfWorkflow is a person's approval of version 2 of ownRoot's spec while the root is out
// of the workflow in status, prepared with Dispatch's view of the root as the daemon reads it for a
// person's approval. statusChanged says that the root's status changed after the approval, by the
// next Dispatch event (intake.DispatchArtifact.StatusChangedSeq): the approval is then delivered
// late, and a root in the backlog or the icebox is not queued by it.
func (r *pathRig) approvedOutOfWorkflow(status string, statusChanged bool) {
	r.t.Helper()
	fact := intake.DispatchArtifact{Key: ownRoot, ArtifactID: ownDocument, Kind: intake.DispatchArtifactApproved, Version: 2,
		Mark: r.mark(r.next()), Actor: pathPerson, Issue: &dispatch.Issue{Key: ownRoot, Project: pathProject, Title: ownRoot,
			Status: status, Labels: []string{dispatch.LegionLabel}, HandedOver: true, PrimaryArtifactID: ownDocument}}
	if statusChanged {
		fact.StatusChangedSeq = r.next()
	}
	r.apply(fact)
}

// handoverPending says whether a person's handover of key waits in the outbox.
func (r *pathRig) handoverPending(key string) bool {
	r.t.Helper()
	var pending bool
	r.read(func(tx pgx.Tx) error {
		var err error
		pending, err = r.records.PersonHandoverPending(r.ctx, tx, key)
		return err
	})
	return pending
}

// todo is key's move into todo with the legion label, which re-admits a root that left the
// workflow as a new generation (admit's readmit), and promotes it.
func (r *pathRig) todo(key string) {
	r.t.Helper()
	before := r.issue(key).Generation
	r.apply(intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: r.next(), Type: "issue.updated", Status: "todo",
		Title: key, Rank: "A", HandedOver: true})
	if got := r.issue(key); got.Generation != before+1 || got.Phase != phase.Admitted {
		r.t.Fatalf("%s after its todo = generation %d at %s, want it re-admitted at generation %d", key, got.Generation, got.Phase, before+1)
	}
}

// handOver records child as a member of the lingering oldRoot's tree that started its phase, and
// has the daemon hand it over as a root of its own: admission's re-admission with carry, which
// copies oldRoot's gate rows to it, and the promotion that opens its tree.
func (r *pathRig) handOver(oldRoot, child string) {
	r.t.Helper()
	old := r.issue(oldRoot)
	parent := oldRoot
	r.recordIssue(record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: oldRoot, Project: pathProject,
		Title: child, Parent: &parent, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "B",
		LastDispatchSeq: 1})
	r.write(func(tx pgx.Tx) error {
		return r.records.PutHandover(r.ctx, tx, record.Handover{Issue: child, Tree: oldRoot, Generation: old.Generation})
	})
	r.apply(intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: r.next(), Type: "issue.updated", Status: "todo",
		Title: child, Parent: oldRoot, Rank: "B", HandedOver: true, DaemonActor: true})
	if got := r.issue(child); got.Tree != child || got.Generation != 2 {
		r.t.Fatalf("handed-over %s = tree %s at generation %d, want a root of its own at generation 2", child, got.Tree, got.Generation)
	}
}

// carried is carriedRoot handed over out of otherRoot's closed tree, whose spec row was gate.
func (r *pathRig) carried(gate record.DesignGate) {
	r.t.Helper()
	r.lingering(otherRoot, "done")
	gate.Issue, gate.Step, gate.ArtifactID = otherRoot, record.DesignStepSpec, otherDocument
	r.write(func(tx pgx.Tx) error { return r.records.PutGate(r.ctx, tx, gate) })
	r.handOver(otherRoot, carriedRoot)
}

// ready is the ready of the first launch of key's architect claim (intake.ClaimReady), which the
// daemon applies once that launch's agent says it is ready.
func (r *pathRig) ready(key string) {
	r.t.Helper()
	r.applyAs("supervise", "ready:"+key+":1", intake.ClaimReady{Issue: key, Role: claim.RoleArchitect, Launch: 1})
}

// seed runs the gate seed key's registration queued, as the outbox's seedGate runs it over a read
// of Dispatch that finds version latest of the document approved by the person at the next
// sequence: the approval is applied as a review of its own or, where Dispatch holds the document
// below the version the seed was queued at, as the reset of that gate to Dispatch's version
// (intake.GateAboveDispatch).
func (r *pathRig) seed(key string, latest int) {
	r.t.Helper()
	_, payload, found := r.newestRow("kind = $1 and issue = $2", string(record.OutboxKindGateSeed), key)
	if !found {
		r.t.Fatalf("the registration of %s queued no gate seed", key)
	}
	seed := payload.(record.GateSeed)
	read := intake.DispatchArtifact{Key: key, ArtifactID: seed.ArtifactID, Kind: intake.DispatchArtifactApproved, Version: latest,
		Mark: r.mark(r.next()), Actor: pathPerson}
	id := fmt.Sprintf("%s:%d:%s:%s:%d:%s:%d:%d", key, seed.Generation, seed.Step, seed.ArtifactID, seed.Version, read.Kind, read.Version, read.Mark.ID())
	if latest < seed.Version {
		r.applyAs("outbox", "gate-above:"+id, intake.GateAboveDispatch{Generation: seed.Generation, Above: seed.Version, Read: read})
		return
	}
	r.applyAs("outbox", "gate-seed:"+id, read)
}

// relay is the outbox's relay of key's newest notice of kind, whose architect it found stopped
// with its finished tree (the daemon's deliverNotice and parkDecision): the one rule for a
// decision nobody in its tree hears (record.RelayParkedDecision), with the notice's decision, ref,
// author, version and reason, in a transaction of its own. The decision is relayed to the
// controller and kept for the next start of the tree's root.
func (r *pathRig) relay(key string, kind record.NoticeKind) {
	r.t.Helper()
	row, notice := r.notice(key, kind)
	decision, ok := record.SpecDecision(notice.Kind)
	if !ok {
		r.t.Fatalf("a notice of kind %s carries no spec decision", notice.Kind)
	}
	tree := r.issue(row.Issue).Tree
	r.write(func(tx pgx.Tx) error {
		root, err := r.records.Issue(r.ctx, tx, tree)
		if err != nil || root == nil {
			return errors.Join(err, fmt.Errorf("no record of the root %s", tree))
		}
		outcome, err := record.RelayParkedDecision(r.ctx, tx, r.records, row.Issue, *root, true,
			record.ParkedDecision{Ref: notice.Ref, Decision: decision, Actor: notice.Actor, At: decidedAt}, notice.Version, notice.Reason, pathSteps)
		if err == nil && outcome != record.RelaySent {
			err = fmt.Errorf("the relay of %s's %s = outcome %d, want it sent to the controller and kept", key, kind, outcome)
		}
		return err
	})
}

// architectToken is the token of key's own architect claim.
func architectToken(t *testing.T, key string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(pathProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", pathProject, err)
	}
	token, err := claim.NewToken(project, key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("the architect token of %s: %v", key, err)
	}
	return token
}

// architectClaim is the claim of key's own architect whose agent is booted and holds nothing, its
// session architectSession.
func (r *pathRig) architectClaim(key string) supervise.Claim {
	r.t.Helper()
	project, err := claim.ProjectToken(pathProject)
	if err != nil {
		r.t.Fatalf("project token of %s: %v", pathProject, err)
	}
	return supervise.Claim{Token: architectToken(r.t, key), Project: project, Tree: key, Issue: key, Role: claim.RoleArchitect, Generation: 1,
		Session: architectSession, SessionFile: "/sessions/" + architectSession + ".jsonl", State: supervise.StateReady}
}

// putClaim writes held as the claim's machine writes it.
func (r *pathRig) putClaim(held supervise.Claim) {
	r.t.Helper()
	if err := r.store.PutClaim(r.ctx, held); err != nil {
		r.t.Fatalf("write the claim %s: %v", held.Token, err)
	}
}

// deliverStart runs key's newest architect start as the outbox runs a start whose task its claim
// takes. The start waits on the issue's branch (record's outboxInOrder), so the branch's lane runs
// its row first, its work GitHub's. Then the start's lane leases the row (record.Store.ClaimRow),
// the claim queues the task with the decisions it names, which the delivery holds
// (store.Store.PutClaimAndDelivery), and the lane finishes the row. It answers the claim and the
// delivery, which the agent has not yet confirmed.
func (r *pathRig) deliverStart(key string) (supervise.Claim, supervise.Delivery) {
	r.t.Helper()
	row, start, found := r.startRow(key)
	if !found {
		r.t.Fatalf("no architect start of %s carries a task", key)
	}
	r.write(func(tx pgx.Tx) error {
		due, err := r.records.DueRows(r.ctx, tx, pathProject, key, []record.OutboxKind{record.OutboxKindIssueBranch}, time.Now())
		if err != nil {
			return err
		}
		for _, branch := range due {
			leased, _, err := r.records.ClaimRow(r.ctx, tx, branch.ID, time.Now(), time.Minute)
			if err != nil {
				return err
			}
			if err := r.records.FinishLeasedOutbox(r.ctx, tx, branch.ID, leased.LeaseToken); err != nil {
				return err
			}
		}
		return nil
	})
	var lease string
	r.write(func(tx pgx.Tx) error {
		leased, ok, err := r.records.ClaimRow(r.ctx, tx, row.ID, time.Now(), time.Minute)
		if err == nil && !ok {
			err = fmt.Errorf("the start row %d of %s is not the outbox's to run", row.ID, key)
		}
		lease = leased.LeaseToken
		return err
	})
	held := r.architectClaim(key)
	held.GivenEntry, held.DeliveredRows = start.Entry, []int64{row.ID}
	delivery := supervise.Delivery{ID: fmt.Sprintf("start-%d", row.ID), Task: start.Task, Generation: start.Generation, Entry: start.Entry,
		QueuedAt: time.Now().UTC()}
	if start.Kept != nil {
		kept, err := json.Marshal(start.Kept)
		if err != nil {
			r.t.Fatalf("encode what the start of %s names: %v", key, err)
		}
		delivery.Kept = kept
	}
	if err := r.store.PutClaimAndDelivery(r.ctx, held, delivery); err != nil {
		r.t.Fatalf("queue the start task of %s on its claim: %v", key, err)
	}
	r.write(func(tx pgx.Tx) error { return r.records.FinishLeasedOutbox(r.ctx, tx, row.ID, lease) })
	return held, delivery
}

// confirm is the turn of held's agent that takes delivery: the claim's write records the
// confirmation and tells what the task named in the same transaction
// (store.Store.PutClaimAndDelivery), and the daemon applies the confirmation, on which admission
// decides the root (intake.TaskConfirmed).
func (r *pathRig) confirm(held supervise.Claim, delivery supervise.Delivery) {
	r.t.Helper()
	held.State, delivery.ConfirmedAt = supervise.StateWorking, time.Now().UTC()
	if err := r.store.PutClaimAndDelivery(r.ctx, held, delivery); err != nil {
		r.t.Fatalf("confirm the task of %s: %v", held.Token, err)
	}
	r.applyAs("supervise", "task-confirmed:"+delivery.ID, intake.TaskConfirmed{Issue: held.Issue, Role: held.Role,
		Generation: r.issue(held.Issue).Generation})
}

// keptRows is how many decisions and kept approvals the record holds for key's tree, told or not.
func (r *pathRig) keptRows(key string) int {
	r.t.Helper()
	var kept int
	if err := r.store.Pool().QueryRow(r.ctx, `select (select count(*) from parked_decisions where issue = $1)
		+ (select count(*) from readmitted_approvals where issue = $1)`, key).Scan(&kept); err != nil {
		r.t.Fatalf("count what %s keeps: %v", key, err)
	}
	return kept
}

// wantOwn holds the route's answer for key's architect to a person's approval of version of
// key's own spec, document.
func (r *pathRig) wantOwn(key, document string, version int) {
	r.t.Helper()
	r.wantApproval(prompts.Approval{Tree: key, Spec: key, Document: document, Version: version})
}

// wantCarried holds the route's answer for key's architect to a person's approval of version of
// from's spec, document, whose work key's tree carries.
func (r *pathRig) wantCarried(key, from, document string, version int) {
	r.t.Helper()
	r.wantApproval(prompts.Approval{Tree: key, Spec: from, Document: document, Version: version, Carried: true})
}

// wantApproval reads what the approval route answers the architect of want.Tree, in a read-only
// transaction of its own once the path's transaction has committed (api.ClaimApprovalOf), and holds
// it to want's version and document in R1's words.
func (r *pathRig) wantApproval(want prompts.Approval) {
	r.t.Helper()
	var got api.ClaimApprovalResponse
	r.read(func(tx pgx.Tx) error {
		var err error
		got, err = api.ClaimApprovalOf(r.ctx, tx, r.records, pathSteps, want.Tree, claim.RoleArchitect)
		return err
	})
	message, err := prompts.ApprovalMessage(want)
	if err != nil {
		r.t.Fatalf("the approval message of %+v: %v", want, err)
	}
	wanted := api.ClaimApproval{Document: "dispatch://" + want.Spec + "/spec", Version: want.Version, Message: message}
	if got.Approval == nil {
		r.t.Fatalf("the approval route answers %s none at the path's commit, want version %d of %s", want.Tree, want.Version, wanted.Document)
	}
	if *got.Approval != wanted {
		r.t.Fatalf("the approval route answers %s %+v at the path's commit,\nwant %+v", want.Tree, *got.Approval, wanted)
	}
}

// newestRow is the newest outbox row where selects, decoded as the outbox decodes it
// (record.DecodeOutboxPayload), and whether there is one.
func (r *pathRig) newestRow(where string, args ...any) (record.OutboxRow, record.OutboxPayload, bool) {
	r.t.Helper()
	var row record.OutboxRow
	var kind string
	var payload []byte
	err := r.store.Pool().QueryRow(r.ctx, `select id, kind, issue, payload from outbox where `+where+` order by id desc limit 1`, args...).
		Scan(&row.ID, &kind, &row.Issue, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return record.OutboxRow{}, nil, false
	}
	if err != nil {
		r.t.Fatalf("read the outbox: %v", err)
	}
	row.Kind, row.Payload = record.OutboxKind(kind), payload
	decoded, err := record.DecodeOutboxPayload(row)
	if err != nil {
		r.t.Fatalf("decode outbox row %d: %v", row.ID, err)
	}
	return row, decoded, true
}

// notice is the newest notice of kind queued on key, and its row.
func (r *pathRig) notice(key string, kind record.NoticeKind) (record.OutboxRow, record.Notice) {
	r.t.Helper()
	row, payload, found := r.newestRow("kind = $1 and issue = $2 and payload->>'kind' = $3", string(record.OutboxKindNotice), key, string(kind))
	if !found {
		r.t.Fatalf("no notice of %s is queued on %s", kind, key)
	}
	return row, payload.(record.Notice)
}

// noticeEntry is the card the newest notice of kind on key publishes to its architect's topic: the
// summary line the daemon writes (`<kind> on <issue>`) and the JSON payload of the notice as it
// publishes it (record.Notice.Published).
func (r *pathRig) noticeEntry(key string, kind record.NoticeKind) string {
	r.t.Helper()
	row, notice := r.notice(key, kind)
	published, _ := notice.Published(row.ID)
	encoded, err := json.Marshal(published)
	if err != nil {
		r.t.Fatalf("encode the notice of row %d: %v", row.ID, err)
	}
	return fmt.Sprintf("%s on %s\n%s", notice.Kind, row.Issue, encoded)
}

// roleMessageEntry is the body of the newest role message to the architect of key's tree.
func (r *pathRig) roleMessageEntry(key string) string {
	r.t.Helper()
	_, payload, found := r.newestRow("kind = $1 and payload->>'role' = $2", string(record.OutboxKindRoleMessage), string(architectToken(r.t, key)))
	if !found {
		r.t.Fatalf("no role message is queued for the architect of %s", key)
	}
	return payload.(record.RoleMessage).Body
}

// startRow is the newest start of key's architect that carries a task, and whether there is one.
func (r *pathRig) startRow(key string) (record.OutboxRow, record.SuperviseRequest, bool) {
	r.t.Helper()
	row, payload, found := r.newestRow(`kind = $1 and issue = $2 and payload->>'op' = 'start' and payload->>'role' = $3
		and coalesce(payload->>'task', '') <> ''`, string(record.OutboxKindSupervise), key, string(claim.RoleArchitect))
	if !found {
		return record.OutboxRow{}, record.SuperviseRequest{}, false
	}
	return row, payload.(record.SuperviseRequest), true
}

// startTaskEntry is the task of the newest start of key's architect.
func (r *pathRig) startTaskEntry(key string) string {
	r.t.Helper()
	_, start, found := r.startRow(key)
	if !found {
		r.t.Fatalf("no architect start of %s carries a task", key)
	}
	return start.Task
}

// keptTask is what the next start or catch-up of key's tree names of the decisions kept while
// nobody in it could hear them (workflow.ParkedDecisionTask). Its read locks the rows it names, so
// it runs in a transaction that may write, and writes nothing.
func (r *pathRig) keptTask(key string) string {
	r.t.Helper()
	var task string
	r.write(func(tx pgx.Tx) error {
		root, err := r.records.Issue(r.ctx, tx, key)
		if err != nil || root == nil {
			return errors.Join(err, fmt.Errorf("no record of %s", key))
		}
		task, _, err = workflow.ParkedDecisionTask(r.ctx, tx, r.records, pathSteps, *root)
		return err
	})
	if task == "" {
		r.t.Fatalf("the next start of %s names no kept decision", key)
	}
	return task
}

// dispatchApproval is the artifact.approved event Dispatch's events route serves for the person's
// approval of version of document on key at mark, in the shape F3 records
// (testdata/dispatch-carried-approval.json), with no carried reason.
func dispatchApproval(key, document string, version int, mark record.EventMark) []byte {
	person := map[string]any{"kind": pathPerson.Kind, "id": pathPerson.ID}
	event, err := json.Marshal(map[string]any{"id": mark.Seq, "issue_key": key, "artifact_id": nil, "project": pathProject,
		"seq": mark.Seq, "type": "artifact.approved", "actor": person, "notify": true, "created_at": mark.At.Format(time.RFC3339Nano),
		"payload": map[string]any{"kind": "person", "name": "spec.md", "actor": person, "ask_id": nil, "reason": nil,
			"version": version, "artifact_id": document}})
	if err != nil {
		panic(err)
	}
	return event
}
