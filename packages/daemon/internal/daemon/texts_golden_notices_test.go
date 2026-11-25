package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// notices adds every notice to the architect that owns an issue, in each variant its builders
// write.
func (b *goldenBuild) notices(r goldenRendering) {
	t := b.t
	arch := claim.RoleArchitect
	issue := record.Issue{Key: r.key(100), Tree: r.key(99), Title: r.words("notice/title", 30), Phase: phase.Testing}
	head := r.sha("notice/head")
	pr := record.PullRequest{Issue: issue.Key, Repo: r.repo, Number: 700 + r.n, Base: "main-" + r.words("notice/base", 5), HeadSHA: head}
	ref := texts.PullRequestRef(r.repo, pr.Number)
	now := r.at(60)

	// phase-finished: a completion with its handoff and summary, a worker's move back with its
	// words and one with none, and the gate's opening.
	{
		summary := r.words("finished/summary", 2400) + " " + goldenHostile[r.n-1][6]
		handoff := record.PhaseRow{Role: claim.RoleTester, HandoffRecord: r.id("finished/handoff"), HandoffVersion: 2 + r.n,
			HandoffCommit: r.sha("finished/commit"), Verdict: []string{"pass", "fail"}[r.n-1], Summary: summary, CompletedAt: r.at(58)}
		to := []phase.Phase{phase.Reviewing, phase.Implementing}[r.n-1]
		cut, _ := texts.Bound(summary, texts.Summary)
		b.notice(r, "handoff", arch, workflow.NoticeFor(issue.Key, workflow.TriggerTesterPassed, phase.Testing, to, handoff, "", now),
			goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", phase.Testing, "to", to, "verdict", handoff.Verdict,
				"handoff_phase", "test", "handoff_version", handoff.HandoffVersion, "handoff_commit", handoff.HandoffCommit,
				"summary", cut, "words.summary.by", claim.RoleTester, "words.summary.at", r.at(58)))
		words := r.words("finished/back", 500) + " " + goldenHostile[r.n-1][4]
		from := []phase.Phase{phase.Reviewing, phase.Testing}[r.n-1]
		b.notice(r, "backward", arch, workflow.NoticeFor(issue.Key, workflow.TriggerBackward, from, phase.Implementing, record.PhaseRow{}, words, now),
			goldenWant("issue", issue.Key, "role", phase.RoleFor(from), "phase", from, "to", phase.Implementing, "moved_back", true, "reason", words,
				"words.reason.by", phase.RoleFor(from), "words.reason.at", now))
		silent := []phase.Phase{phase.Testing, phase.Reviewing}[r.n-1]
		b.notice(r, "backward-no-words", arch, workflow.NoticeFor(issue.Key, workflow.TriggerBackward, silent, phase.Implementing, record.PhaseRow{}, "", now),
			goldenWant("issue", issue.Key, "role", phase.RoleFor(silent), "phase", silent, "to", phase.Implementing, "moved_back", true))
		b.notice(r, "gate-opened", arch, workflow.NoticeFor(r.key(99), workflow.TriggerGateOpened, phase.Admitted, phase.Planning, record.PhaseRow{}, "", now),
			goldenWant("issue", r.key(99), "phase", phase.Admitted, "to", phase.Planning))
	}

	// worker-died: a failed worker with the failure's record, an architect of a tree, with the
	// sentence and the pod end, and with the sentence and the death cut, a look of a production
	// check, and a role off the phase it works.
	failure := &record.NoticeFailure{Cause: "launch_failures", Detail: r.words("died/detail", 90),
		LaunchFailures: texts.FailureBudget{Count: 2 + r.n, Limit: 3 + r.n}, Deaths: texts.FailureBudget{Count: r.n, Limit: 5},
		PromptFailures: texts.FailureBudget{Count: 0, Limit: 3}, PromptRetires: texts.FailureBudget{Count: 1, Limit: 2},
		Death: r.words("died/death", 40), StartFailure: r.words("died/start", 30)}
	b.notice(r, "failure", arch, workflow.WorkerDied(issue.Key, claim.RoleTester, phase.Testing, failure),
		goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", phase.Testing, "failure.cause", "launch_failures",
			"failure.launchFailures.count", 2+r.n, "failure.launchFailures.limit", 3+r.n))
	tree := workflow.WorkerDied(r.key(99), arch, phase.Admitted, nil)
	tree.Detail = r.words("died/sentence", 70) + " " + goldenHostile[r.n-1][4]
	tree.PodEnd = "Evicted: " + r.words("died/pod-end", 80)
	b.notice(r, "tree-architect", claim.RoleController, tree,
		goldenWant("issue", r.key(99), "role", arch, "phase", phase.Admitted, "detail", tree.Detail, "pod_end", tree.PodEnd))
	cut := workflow.WorkerDied(r.key(99), arch, phase.Admitted, nil)
	cut.Detail, cut.DetailCut = r.words("died/cut-sentence", 400), true
	cut.Death, cut.DeathCut = "gone: "+r.words("died/first-line", 394), true
	b.notice(r, "tree-architect-death", claim.RoleController, cut,
		goldenWant("issue", r.key(99), "role", arch, "detail", cut.Detail, "detail_cut", true, "death", cut.Death, "death_cut", true))
	look := workflow.WorkerDied(r.key(101), claim.RoleTester, "", failure)
	look.Look = 3 + r.n
	b.notice(r, "look", arch, look, goldenWant("issue", r.key(101), "role", claim.RoleTester, "look", 3+r.n))
	b.notice(r, "working", arch, workflow.WorkerDiedNotWorking(issue.Key, claim.RoleImplementer, phase.Reviewing, phase.Reviewing, failure),
		goldenWant("issue", issue.Key, "role", claim.RoleImplementer, "phase", phase.Reviewing, "working", phase.Reviewing))

	// A failure whose sentence, death and start each hold line breaks, as a run of refused turns,
	// an account that ends in the role's log and a launcher's error do, and whose log holds a
	// backslash before an n: the worker-died, here the controller's copy, and the held notice
	// below carry each whole on one line with every character kept, each line break and each
	// backslash written as Go writes it in a string, so the blank line stays and the backslash
	// before an n reads apart from a line feed (workflow's failureOnOneLine).
	lines := *failure
	lines.Detail = r.words("died/lines-sentence", 60) + "\n" + r.words("died/lines-provider", 50)
	lines.Death = "gone: " + r.words("died/lines-death", 40) + "; last lines of tester:\n" + r.words("died/lines-log", 40) + "\n\n" +
		r.words("died/lines-log-2", 30) + ` C:\new`
	lines.StartFailure = r.words("died/lines-start", 40) + "\r\n" + r.words("died/lines-git", 30)
	oneLine := strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\r", `\r`).Replace
	linesWant := goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", phase.Testing, "failure.detail", oneLine(lines.Detail),
		"failure.death", oneLine(lines.Death), "failure.startFailure", oneLine(lines.StartFailure))
	b.notice(r, "failure-lines", claim.RoleController, workflow.WorkerDied(issue.Key, claim.RoleTester, phase.Testing, &lines), linesWant)

	// worker-stalled: the stall backstop's record, with the last call and with none, and a
	// replacement whose launch waits at its issue pod's start gate (supervise.EndRelaunchWaitsForPod).
	{
		claimed := supervise.Claim{Issue: issue.Key, Tree: issue.Tree, Role: claim.RoleImplementer}
		stop := supervise.StallBackstopStop{LastStep: r.at(-200), Bound: time.Duration(2+r.n) * time.Hour, End: supervise.EndRelaunched,
			Step:           supervise.StepRecord{TurnStartedAt: r.at(-260), Tool: "bash", CallID: "call-" + r.id("stall/call"), StartedAt: r.at(-210), EndedAt: r.at(-205)},
			LaunchFailures: r.n, WakeDue: r.at(10)}
		b.notice(r, "call", arch, stallBackstopRecord(claimed, stop, 200*time.Minute),
			goldenWant("issue", issue.Key, "role", claim.RoleImplementer, "last_step", r.at(-200), "end", string(supervise.EndRelaunched),
				"call_id", stop.Step.CallID, "launch_failures", r.n))
		quiet := stop
		quiet.Step, quiet.End = supervise.StepRecord{TurnStartedAt: r.at(-260)}, supervise.EndRelaunchParked
		b.notice(r, "no-call", arch, stallBackstopRecord(claimed, quiet, 300*time.Minute),
			goldenWant("issue", issue.Key, "role", claim.RoleImplementer, "last_step", r.at(-200), "end", string(supervise.EndRelaunchParked),
				"stop", texts.StopNotMade))
		gated := quiet
		gated.End, gated.WakeDue = supervise.EndRelaunchWaitsForPod, time.Time{}
		b.notice(r, "waits-for-pod", arch, stallBackstopRecord(claimed, gated, 300*time.Minute),
			goldenWant("issue", issue.Key, "role", claim.RoleImplementer, "last_step", r.at(-200), "end", string(supervise.EndRelaunchWaitsForPod),
				"stop", texts.StopStopped, "launch_failures", r.n))
	}

	// worker-silent and worker-background-stalled.
	b.notice(r, "silent", arch, workflow.WorkerSilentNotice(issue, claim.RoleTester, r.at(-90), 90*time.Minute, time.Hour, r.n == 1, 4+int64(r.n), 2+int64(r.n)),
		goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", issue.Phase, "last_step", r.at(-90), "entry", 4+int64(r.n),
			"given", 2+int64(r.n)))
	b.notice(r, "background", arch, workflow.WorkerBackgroundNotice(issue, claim.RoleTester, []string{"subagent", "async-job"}[:r.n], r.at(-400),
		400*time.Minute, time.Duration(2+r.n)*time.Hour),
		goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", issue.Phase, "reported_at", r.at(-400)))

	// held: a carried hold, a failure's, and an escalation with the architect's words.
	held := issue
	held.Phase, held.Hold = phase.Held, &record.Hold{From: phase.Testing, Reason: record.HoldEscalated}
	b.notice(r, "carried", arch, workflow.HeldNotice(held),
		goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", phase.Testing, "hold", texts.HoldCarried))
	b.notice(r, "failure", arch, workflow.HeldFailureNotice(issue.Key, claim.RoleTester, phase.Testing, failure),
		goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", phase.Testing, "hold", texts.HoldFailure, "failure.cause", "launch_failures"))
	b.notice(r, "failure-lines", arch, workflow.HeldFailureNotice(issue.Key, claim.RoleTester, phase.Testing, &lines), linesWant)
	escalation := r.words("held/escalation", 700) + " " + goldenHostile[r.n-1][5]
	b.notice(r, "escalation", arch, workflow.HeldEscalationNotice(issue.Key, phase.Testing, escalation, now),
		goldenWant("issue", issue.Key, "phase", phase.Testing, "hold", texts.HoldEscalation, "reason", string(record.HoldEscalated),
			"escalation", escalation, "words.escalation.by", arch, "words.escalation.at", now))

	// pr-blocked, pr-merged (outside merging, and a close that waits for the issues beneath),
	// pr-closed-unmerged, pull-request-missing.
	b.notice(r, "cap", arch, workflow.PRBlockedNotice(issue.Key, 3+r.n, 3+r.n, r.n),
		goldenWant("issue", issue.Key, "cap", 3+r.n, "counted", 3+r.n, "conflict_rounds", r.n))
	// A cap reached with no conflict round names its conflict rounds all the same, 0
	// (dispatch://INFRA-1442's R5).
	b.notice(r, "cap-no-conflict", arch, workflow.PRBlockedNotice(issue.Key, 5+r.n, 5+r.n, 0),
		goldenWant("issue", issue.Key, "cap", 5+r.n, "counted", 5+r.n, "conflict_rounds", 0))
	b.notice(r, "outside-merging", arch, workflow.PRMergedNotice(issue.Key, pr, phase.Reviewing, nil),
		goldenWant("issue", issue.Key, "pull_request", ref, "phase", phase.Reviewing))
	waiting := []string{r.key(102), r.key(103), r.key(104)}[:1+r.n]
	b.notice(r, "waiting", arch, workflow.PRMergedNotice(issue.Key, pr, "", waiting),
		goldenWant("issue", issue.Key, "pull_request", ref, "waiting_for", waiting))
	b.notice(r, "closed", arch, workflow.PRClosedUnmergedNotice(issue.Key, pr, phase.Reviewing),
		goldenWant("issue", issue.Key, "pull_request", ref, "phase", phase.Reviewing))
	commit := r.sha("missing/commit")
	b.notice(r, "missing", arch, workflow.PullRequestMissingNotice(issue.Key, commit, r.repo, claim.IssueBranch(issue.Key)),
		goldenWant("issue", issue.Key, "handoff_commit", commit, "repo", r.repo, "branch", claim.IssueBranch(issue.Key)))

	// message-refused: a worker's backward move whose reason went as a message of its own (the
	// move wrote no status), a status write whose reason Dispatch refused beside it, of a close and
	// of a reviewer's backward move, and a message of the daemon's own. A reason opens with the
	// daemon's sentence and goes on with another author's words, a worker's or the architect's: the
	// notice gives that sentence as a field (refused_move, refused_close) and quotes those words
	// alone in a block of their own, named by that author and the time the refused row was queued.
	// The daemon's own line is quoted whole, by the daemon. Each quoted line is past the preview's
	// bound, so the block holds its head with nothing added, and the words object counts the rest
	// of the words, the lines after the first among them, in UTF-16 units, as the length counts the
	// body.
	{
		from := []phase.Phase{phase.Implementing, phase.Merging}[r.n-1]
		to := []phase.Phase{phase.Planning, phase.Retro}[r.n-1]
		queued := r.at(59)
		// Each sample's refusal is the error the refused row's run ended with, which the notice
		// carries as the daemon stores it (storetext.FarSideError): the daemon's own wrapper around
		// Dispatch's answer, which reads as Dispatch's code and then its message (dispatch.Error).
		// A message Dispatch refused for good is one over its bound (goldenPostRefused), so each
		// such body holds a block longer than one block may be: words as long as one block may
		// hold, and the text beside them in their block.
		//
		// The refusal a status sample carries follows from whether its status stands. Where it
		// does, Dispatch refused the reason posted beside it for its bound and the status was
		// written all the same, so the run ended with the reason's refused post (outbox.status),
		// and the reason holds a block over the bound as a refused message does. Where it does not,
		// the run ended at the issue read that opens it, before the reason was posted
		// (goldenReadRefused), and the reason is of an ordinary length. statusWriteRefused reads
		// only the cause's words, so neither carries the writes that stand: the sample passes them.
		refusal := func(statusSet bool, reason string) error {
			if statusSet {
				return goldenPostRefused(t, issue.Key, reason)
			}
			return goldenReadRefused(issue.Key)
		}
		reasonWords := func(statusSet bool) int {
			if statusSet {
				return dispatchbody.BlockUnits
			}
			return 300
		}
		// workflow's backwardMoveReason: the move and the role that asked, then that role's words.
		words := goldenHostile[r.n-1][5] + " " + r.words("refused/body", dispatchbody.BlockUnits) + "\n" + r.words("refused/rest", 80)
		body := fmt.Sprintf("Legion moved this issue from %s back to %s at the %s's request: ", from, to, phase.RoleFor(from)) + words
		answer := goldenPostRefused(t, issue.Key, body)
		refused := messagePostRefused(issue.Key, record.MessagePost{Body: body}, answer)
		begins := refusedPreview(words)
		b.notice(r, "message", arch, refusedQuotedAt(refused, queued),
			goldenWant("issue", issue.Key, "refused", texts.RefusedMessage, "dispatch_answer", answer.Error(), "length", goldenUnits(body),
				"refused_move", phase.Pair(from, to), "begins", begins.Text, "words.begins.by", phase.RoleFor(from), "words.begins.at", queued,
				"words.begins.rest", begins.Rest))
		own := r.words("refused/own", dispatchbody.BlockUnits) + "\n" + r.words("refused/own-rest", 80)
		answer = goldenPostRefused(t, issue.Key, own)
		refused = messagePostRefused(issue.Key, record.MessagePost{Body: own}, answer)
		begins = refusedPreview(own)
		b.notice(r, "message-own", arch, refusedQuotedAt(refused, queued),
			goldenWant("issue", issue.Key, "refused", texts.RefusedMessage, "dispatch_answer", answer.Error(), "length", goldenUnits(own),
				"begins", begins.Text, "words.begins.by", texts.ByDaemon, "words.begins.at", queued, "words.begins.rest", begins.Rest))
		// A message of the daemon's own whose first line is empty, ended by a carriage return and a
		// line feed: begins quotes nothing, and the notice says that the first line is empty, as
		// main's said that such a message begins "".
		blank := "\r\n" + r.words("refused/blank", dispatchbody.BlockUnits+120)
		answer = goldenPostRefused(t, issue.Key, blank)
		refused = messagePostRefused(issue.Key, record.MessagePost{Body: blank}, answer)
		b.notice(r, "message-empty-first-line", arch, refusedQuotedAt(refused, queued),
			goldenWant("issue", issue.Key, "refused", texts.RefusedMessage, "dispatch_answer", answer.Error(), "length", goldenUnits(blank),
				"first_line_empty", true))
		// A close's reason, the daemon's sentence and then the architect's. In sample 1 the status
		// stands and Dispatch refused the reason. In sample 2 Dispatch refused the issue read that
		// opens the row, so neither write was made.
		status := []string{"done", "icebox"}[r.n-1]
		closeSet := r.n == 1
		reason := r.words("refused/reason", reasonWords(closeSet))
		closed := record.StatusWrite{Status: status, Reason: intake.CloseRoot{Reason: reason}.Message()}
		answer = refusal(closeSet, closed.Reason)
		refused = statusWriteRefused(issue.Key, closed, answer, statusWrites{status: closeSet})
		begins = refusedPreview(reason)
		b.notice(r, "status", arch, refusedQuotedAt(refused, queued),
			goldenWant("issue", issue.Key, "refused", texts.RefusedStatus, "dispatch_answer", answer.Error(), "status", status, "status_set", closeSet,
				"reason_posted", false, "refused_close", texts.BeforeFirstPhase, "begins", begins.Text, "words.begins.by", arch,
				"words.begins.at", queued, "words.begins.rest", begins.Rest))
		// A reviewer's backward move refused beside its status. In sample 1 Dispatch refused the
		// issue read that opens the row, so neither write was made. In sample 2 the status stands
		// and Dispatch refused the reason.
		moveSet := r.n == 2
		moved := r.words("refused/moved", reasonWords(moveSet))
		move := record.StatusWrite{Status: status, Reason: "Legion moved this issue from reviewing back to implementing at the reviewer's request: " + moved}
		answer = refusal(moveSet, move.Reason)
		refused = statusWriteRefused(issue.Key, move, answer, statusWrites{status: moveSet})
		begins = refusedPreview(moved)
		b.notice(r, "status-move", arch, refusedQuotedAt(refused, queued),
			goldenWant("issue", issue.Key, "refused", texts.RefusedStatus, "dispatch_answer", answer.Error(), "status", status, "status_set", moveSet,
				"reason_posted", false, "refused_move", phase.Pair(phase.Reviewing, phase.Implementing), "begins", begins.Text,
				"words.begins.by", claim.RoleReviewer, "words.begins.at", queued, "words.begins.rest", begins.Rest))
	}

	// design-approved, design-changes-requested, carried-gate-opened.
	{
		root := r.key(99)
		person := "user:" + r.words("gate/person", 7)
		version := 5 + r.n
		b.notice(r, "approved", arch, workflow.DesignApprovedNotice(root, record.DesignStepSpec, version, person, r.at(70)),
			goldenWant("issue", root, "version", version, "actor", person, "ref", "dispatch://"+root+"/spec", "at", r.at(70)))
		reason := r.words("gate/changes", 4500) + " " + goldenHostile[r.n-1][4]
		cut, _ := texts.Bound(reason, texts.OneAct)
		b.notice(r, "changes", arch, workflow.ChangesRequestedNotice(root, record.DesignStepSpec, version, reason, person, r.at(71)),
			goldenWant("issue", root, "version", version, "actor", person, "ref", "dispatch://"+root+"/spec", "at", r.at(71),
				"reason", cut, "words.reason.by", person, "words.reason.at", r.at(71)))
		from := r.key(98)
		b.notice(r, "carried", arch, workflow.CarriedGateOpenedNotice(root, from, record.DesignStepSpec, version, person, r.at(72)),
			goldenWant("issue", root, "version", version, "actor", person, "ref", "dispatch://"+from+"/spec", "at", r.at(72)))
	}

	// child-closed and child-status in each change. A child that left the workflow (leave) names its
	// open pull request and each issue under it that is not done; a child of a lingering tree that
	// closed at its merge (closeMerged) names nothing more.
	child := r.key(110)
	left := texts.LeftBehind{OpenPullRequest: fmt.Sprintf("%s#%d", r.repo, 300+r.n),
		UnderNotDone: []texts.NotDoneIssue{{Key: r.key(117), Status: []string{"in_progress", "todo"}[r.n-1]}, {Key: r.key(118), Status: "backlog"}}[:r.n]}
	leftWant := func(pairs ...any) map[string]string {
		want := goldenWant(append(pairs, "open_pull_request", left.OpenPullRequest, "under_not_done#", len(left.UnderNotDone))...)
		for i, issue := range left.UnderNotDone {
			want[fmt.Sprintf("under_not_done[%d].key", i)] = issue.Key
			want[fmt.Sprintf("under_not_done[%d].status", i)] = issue.Status
		}
		return want
	}
	b.notice(r, "closed", arch, workflow.ChildClosedNotice(child, left), leftWant("issue", child))
	b.notice(r, "merged", arch, workflow.ChildClosedNotice(child, texts.LeftBehind{}), goldenWant("issue", child))
	b.notice(r, "todo", arch, workflow.ChildReenteredNotice(child, r.key(99)), goldenWant("issue", child, "change", texts.ChangeTodo, "tree", r.key(99)))
	waited := []string{r.key(111), r.key(112)}[:r.n]
	first := []phase.Phase{phase.Planning, phase.Implementing}[r.n-1]
	b.notice(r, "started", arch, workflow.ChildStartedNotice(child, r.key(99), first, waited),
		goldenWant("issue", child, "change", texts.ChangeStarted, "tree", r.key(99), "phase", first, "waited_on", waited))
	b.notice(r, "beneath-accepted", arch, workflow.ChildUnderAcceptedParentNotice(child, r.key(113)),
		goldenWant("issue", child, "change", texts.ChangeBeneathAccepted, "accepted_ancestor", r.key(113)))
	b.notice(r, "joined", arch, workflow.JoinedTheTreeNotice(child, r.key(99), r.key(114)),
		goldenWant("issue", child, "change", texts.ChangeJoined, "tree", r.key(99), "from_tree", r.key(114)))
	b.notice(r, "reparented", arch, workflow.ReparentedNotice(r.key(99), r.key(115), r.key(116), []string{texts.StaysOwnRan, texts.StaysOwnOperator}[r.n-1]),
		goldenWant("issue", r.key(99), "change", texts.ChangeReparented, "child", r.key(115), "parent", r.key(116)))
	b.notice(r, "left", arch, workflow.ChildLeftNotice(child, []string{"icebox", "backlog"}[r.n-1], left),
		leftWant("issue", child, "change", texts.ChangeLeft, "status", []string{"icebox", "backlog"}[r.n-1]))

	// task-stopped: the owner's stop_task of a task (workflow's taskStopped), and a failed step that
	// no earlier step of the type implementation can take the work back from (stopUnreturned).
	{
		task := "task-" + r.words("stopped/task", 3)
		at := []phase.Phase{phase.Testing, phase.Reviewing}[r.n-1]
		by := r.token(t, r.key(99), arch)
		b.notice(r, "owner", arch, workflow.OwnerStoppedNotice(issue.Key, task, at, by),
			goldenWant("issue", issue.Key, "role", phase.RoleFor(at), "phase", at, "task", task, "cause", texts.TaskStoppedByOwner, "stoppedBy", string(by)))
		role := []claim.Role{claim.RoleTester, claim.RoleReviewer}[r.n-1]
		b.notice(r, "no-return", arch, workflow.StopUnreturnedNotice(issue.Key, task, 1+r.n, role, at),
			goldenWant("issue", issue.Key, "role", role, "phase", at, "task", task, "cause", texts.TaskStoppedNoReturn, "step", 1+r.n))
	}

	// plan-not-taken: the first step passed over the plans of earlier owners (workflow's
	// tellPassedOver), and runs an owner's version of its own or the project's default steps; and
	// the first step of an issue whose experiment's group runs the default steps passed over every
	// plan given for it (tellNotTaken), a version carried to a second generation named once.
	{
		earlier := make([]record.PlanVersion, r.n)
		for i := range earlier {
			earlier[i] = record.PlanVersion{Issue: issue.Key, Generation: uint64(r.n), Version: 2 + i + r.n, ArtifactID: "plan-" + r.id(fmt.Sprintf("plan/earlier/%d", i)),
				Source: record.PlanSourceOwner, GivenBy: r.token(t, r.key(120+i), arch)}
		}
		want := goldenWant("issue", issue.Key, "why", texts.PlanNotTakenEarlierOwner, "not_taken#", len(earlier))
		for i, version := range earlier {
			want[fmt.Sprintf("not_taken[%d].version", i)] = strconv.Itoa(version.Version)
			want[fmt.Sprintf("not_taken[%d].artifact", i)] = version.ArtifactID
			want[fmt.Sprintf("not_taken[%d].given_by", i)] = string(version.GivenBy)
		}
		runs := record.PlanVersion{Issue: issue.Key, Generation: uint64(1 + r.n), Version: 5 + r.n, ArtifactID: "plan-" + r.id("plan/runs"),
			Source: record.PlanSourceOwner, GivenBy: r.token(t, r.key(99), arch)}
		owner := maps.Clone(want)
		owner["runs"], owner["runs_version"], owner["runs_artifact"], owner["runs_given_by"] = texts.PlanRunsOwner, strconv.Itoa(runs.Version),
			runs.ArtifactID, string(runs.GivenBy)
		b.notice(r, "owner", arch, workflow.PlanNotTakenNotice(issue.Key, earlier, runs), owner)
		want["runs"] = texts.PlanRunsDefault
		b.notice(r, "default", arch, workflow.PlanNotTakenNotice(issue.Key, earlier, record.PlanVersion{Issue: issue.Key, Source: record.PlanSourceDefault}), want)
		own := runs
		own.GivenBy = r.token(t, issue.Key, arch)
		carried := own
		carried.Generation++
		passed := append(slices.Clone(earlier), own, carried)
		group := record.Assignment{Experiment: []string{"planning", "how-each-issue-is-done"}[r.n-1], Unit: "issue",
			Variant: []string{"b", "default-steps"}[r.n-1], Plan: "default"}
		grouped := goldenWant("issue", issue.Key, "why", texts.PlanNotTakenGroup, "not_taken#", len(earlier)+1, "runs", texts.PlanRunsDefault,
			"experiment", group.Experiment, "variant", group.Variant)
		for i, version := range append(slices.Clone(earlier), own) {
			grouped[fmt.Sprintf("not_taken[%d].version", i)] = strconv.Itoa(version.Version)
			grouped[fmt.Sprintf("not_taken[%d].artifact", i)] = version.ArtifactID
			grouped[fmt.Sprintf("not_taken[%d].given_by", i)] = string(version.GivenBy)
		}
		b.notice(r, "group", arch, workflow.GroupPlanNotTakenNotice(issue.Key, group, passed), grouped)
	}

	// retro-held: a parent past its last step, held before its retro while issues beneath it are
	// still in the workflow, as workflow's retroHeldNotice builds it.
	{
		beneath := []string{r.key(111), r.key(112), r.key(113)}[:1+r.n]
		b.notice(r, "waiting", arch, texts.RetroHeldRecord{NoticeHead: texts.NoticeHead{Kind: texts.RetroHeld, Issue: r.key(99)},
			Role: phase.RoleFor(phase.Retro), Phase: phase.Retro, WaitingFor: beneath, StatusNotTakenYet: record.StatusNotTakenYet,
			EndChildRule: record.EndChildRule},
			goldenWant("issue", r.key(99), "role", phase.RoleFor(phase.Retro), "phase", phase.Retro, "waiting_for", beneath))
	}

	// children-first: a parent with 30 open children.
	{
		wait := goldenChildWait(r)
		want := goldenWant("issue", wait.Parent, "children#", len(wait.Open))
		for i, open := range wait.Open {
			want[fmt.Sprintf("children[%d].key", i)] = open.Key
			want[fmt.Sprintf("children[%d].status", i)] = open.Status
		}
		b.notice(r, "open-children", arch, workflow.ChildrenFirstNotice(wait), want)
	}

	// child-release-stalled in each stall, and child-not-recorded.
	{
		parent, released := r.key(120), r.key(121)
		blocker := api.ReleaseStalledNotice(r.key(99), texts.StallUnrecordedBlocker, released, parent)
		blocker.Blocker, blocker.BlockerStatus, blocker.Repo = r.key(122), "in_progress", r.repo
		b.notice(r, "unrecorded-blocker", arch, blocker,
			goldenWant("issue", r.key(99), "stall", texts.StallUnrecordedBlocker, "child", released, "parent", parent, "blocker", r.key(122), "repo", r.repo))
		ancestor := api.ReleaseStalledNotice(r.key(99), texts.StallUnrecordedAncestor, released, parent)
		ancestor.Ancestor, ancestor.Repo = r.key(123), r.repo
		b.notice(r, "unrecorded-ancestor", arch, ancestor,
			goldenWant("issue", r.key(99), "stall", texts.StallUnrecordedAncestor, "child", released, "parent", parent, "ancestor", r.key(123)))
		spent := api.ReleaseStalledNotice(r.key(99), texts.StallAttemptsSpent, released, parent)
		spent.Attempts = 4 + r.n
		b.notice(r, "attempts-spent", arch, spent,
			goldenWant("issue", r.key(99), "stall", texts.StallAttemptsSpent, "child", released, "attempts", 4+r.n))
		unrecorded := []texts.UnrecordedChild{{Child: r.key(124), Parent: parent, ParentRecorded: r.n == 1}, {Child: r.key(125), Parent: r.key(126)}}
		b.notice(r, "recovered-unrecorded", arch, recoveredUnrecordedNotice(r.key(99), []string{r.repo}, unrecorded),
			goldenWant("issue", r.key(99), "stall", texts.StallRecoveredUnrecorded, "repos", r.repo, "unrecorded[0].child", r.key(124),
				"unrecorded[1].child", r.key(125)))
		status := []string{"backlog", "icebox"}[r.n-1]
		noTodo := []texts.LetGoChild{{Child: r.key(128), Parent: parent, ParentRecorded: true, Listed: true, Status: status, Repo: r.repo},
			{Child: r.key(129), Parent: r.key(130), Listed: true, Status: "todo", Repo: r.repo},
			{Child: r.key(132), Parent: parent, ParentRecorded: true, Repo: r.repo}}
		b.notice(r, "let-go-no-todo", arch, letGoNotice(r.key(99), texts.StallLetGoNoTodo, []string{r.repo}, noTodo),
			goldenWant("issue", r.key(99), "stall", texts.StallLetGoNoTodo, "repos", r.repo, "let_go[0].child", r.key(128), "let_go[0].listed", true,
				"let_go[0].status", status, "let_go[0].repo", r.repo, "let_go[1].child", r.key(129), "let_go[1].parent", r.key(130),
				"let_go[2].child", r.key(132), "let_go[2].listed", false))
		answered := []texts.LetGoChild{{Child: r.key(131), Parent: parent, ParentRecorded: true, Listed: true, Status: "todo", Repo: r.repo}}
		b.notice(r, "let-go-answered", arch, letGoNotice(r.key(99), texts.StallLetGoAnswered, []string{r.repo}, answered),
			goldenWant("issue", r.key(99), "stall", texts.StallLetGoAnswered, "repos", r.repo, "let_go[0].child", r.key(131),
				"let_go[0].status", "todo", "let_go[0].repo", r.repo))
		refusal := r.words("refused/repo", 120)
		resolved := "elsewhere-" + r.words("resolved", 6) + "/repo"
		b.notice(r, "refused", arch, workflow.ChildNotRecordedNotice(parent, r.key(127), resolved, refusal),
			goldenWant("issue", parent, "child", r.key(127), "resolved", resolved, "refusal", refusal))
	}

	// catch-up: the root's architect and a child's own architect.
	b.catchUps(r)

	// checks-red, review-stuck in each cause, status-reasserted, session-lost, session-store-fault.
	// checks-red, as mergeForward writes it: in a phase a worker holds (low), the implementer asked
	// in reviewing and the merger in merging, which merges the base forward itself there
	// (cybermonklabs/legion#4979); of a pull request recorded before the daemon kept the base,
	// which names the head and no base; and of an issue held from testing, by its tester's failed
	// claim and by its architect's escalation, which no worker holds (med).
	standing := func(at phase.Phase) record.Issue {
		stands := issue
		stands.Phase = at
		return stands
	}
	why := []record.ConflictWhy{record.ConflictCheckNotGreen, record.ConflictRulesUnread}[r.n-1]
	b.notice(r, "conflict", arch, workflow.ChecksRedNotice(standing(phase.Reviewing), pr, why),
		goldenWant("issue", issue.Key, "phase", phase.Reviewing, "conflicts_with", pr.Base, "head", head, "pull_request", ref, "why", why,
			"asked", claim.RoleImplementer))
	b.notice(r, "merging", arch, workflow.ChecksRedNotice(standing(phase.Merging), pr, record.ConflictMergeNext),
		goldenWant("issue", issue.Key, "phase", phase.Merging, "conflicts_with", pr.Base, "head", head, "pull_request", ref, "why", record.ConflictMergeNext,
			"asked", claim.RoleMerger))
	unbased := record.PullRequest{Issue: issue.Key, Repo: r.repo, Number: 750 + r.n, HeadSHA: r.sha("notice/no-base/head")}
	b.notice(r, "no-base", arch, workflow.ChecksRedNotice(standing(phase.Merging), unbased, record.ConflictMergeNext),
		goldenWant("issue", issue.Key, "phase", phase.Merging, "head", unbased.HeadSHA, "pull_request", texts.PullRequestRef(r.repo, unbased.Number),
			"why", record.ConflictMergeNext, "asked", claim.RoleMerger))
	heldFrom := standing(phase.Held)
	heldFrom.Hold = &record.Hold{From: phase.Testing, Reason: []record.HoldReason{"", record.HoldEscalated}[r.n-1]}
	b.notice(r, "held", arch, workflow.ChecksRedNotice(heldFrom, pr, why),
		goldenWant("issue", issue.Key, "phase", phase.Testing, "hold", []string{texts.HoldFailure, texts.HoldEscalation}[r.n-1],
			"conflicts_with", pr.Base, "head", head, "pull_request", ref, "why", why, "asked", claim.RoleImplementer))
	stuck := goldenStuckRequest(r)
	stuckRef := texts.PullRequestRef(r.repo, stuck.Number)
	topic := "notifications.role." + string(r.token(t, issue.Key, claim.RoleReviewer))
	aside := r.words("stuck/aside", 140)
	b.notice(r, "undecided", arch, workflow.ReviewStuckNotice(issue.Key, stuck, texts.CauseUndecided, []string{texts.StuckByCompletion, texts.StuckByAnswer}[r.n-1], topic, "", aside),
		goldenWant("issue", issue.Key, "role", claim.RoleReviewer, "phase", phase.Reviewing, "cause", texts.CauseUndecided, "pull_request", stuckRef,
			"head", stuck.HeadSHA, "set_aside", aside, "topic", topic, "summary", []string{texts.StuckByCompletion, texts.StuckByAnswer}[r.n-1]))
	approvedHead := r.sha("stuck/approved")
	b.notice(r, "approved-other-code", arch, workflow.ReviewStuckNotice(issue.Key, stuck, texts.CauseApprovedOtherCode,
		[]string{texts.StuckByPush, texts.StuckByOtherReview}[r.n-1], topic, approvedHead, ""),
		goldenWant("issue", issue.Key, "role", claim.RoleReviewer, "phase", phase.Reviewing, "cause", texts.CauseApprovedOtherCode,
			"pull_request", stuckRef, "head", stuck.HeadSHA, "approved_head", approvedHead))
	checks := goldenChecks(r, "stuck")
	b.notice(r, "merge-state", arch, workflow.ReviewStuckNotice(issue.Key, stuck, texts.CauseMergeState,
		[]string{texts.StuckByMergeState, texts.StuckByMerge}[r.n-1], "", "", ""),
		checks.skippedFacts(checks.statusFacts(goldenWant("issue", issue.Key, "role", claim.RoleReviewer, "phase", phase.Reviewing, "cause", texts.CauseMergeState,
			"pull_request", stuckRef, "head", stuck.HeadSHA, "merge_state", string(stuck.MergeState),
			"list_passed", texts.ListPassed(r.repo, stuck.Number)))))
	session := "session-" + r.id("reasserted")
	b.notice(r, "reasserted", arch, workflow.StatusReassertedNotice(issue.Key, session, "done", "testing"),
		goldenWant("issue", issue.Key, "session", session, "wrote", "done", "status", "testing"))
	detail := r.words("lost/detail", 120) + ` "` + goldenHostile[r.n-1][5] + `"`
	b.notice(r, "lost", arch, workflow.SessionLostNotice(issue.Key, claim.RoleTester, phase.Testing, detail),
		goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", phase.Testing, "detail", detail))
	forgotten := "session-" + r.id("forgotten") + " " + goldenHostile[r.n-1][4]
	b.notice(r, "forgotten", arch, workflow.SessionForgottenNotice(issue.Key, claim.RoleTester, phase.Testing, forgotten),
		goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", phase.Testing, "session", forgotten))
	b.notice(r, "store-fault", claim.RoleController, workflow.SessionStoreFaultNotice(issue.Key, claim.RoleTester, phase.Testing, detail),
		goldenWant("issue", issue.Key, "role", claim.RoleTester, "phase", phase.Testing, "detail", detail))

	// volume-near-full: a sweep that left the volume above its low mark, with a name a role
	// chose that holds a line break (and, in the second rendering, a backslash, which is written
	// as Go writes it too, so the two names stay two values), a size read in part, a walk that was
	// cut and lists the launcher's bound cut; and the report of a trimmer that switched itself off.
	pod := "pod-" + r.id("volume/pod")
	sweep := record.VolumeNearFull{PodUID: pod, Trimmer: r.id("volume/trimmer"), Episode: 1 + r.n,
		RaisedBy:  []string{shimwire.VolumeEpisodeFirst, shimwire.VolumeEpisodeRise}[r.n-1],
		UsedBytes: int64(40+r.n) << 30, FreeBytes: int64(3-r.n) << 30, FreedBytes: []int64{2 << 30, -(1 << 20)}[r.n-1],
		StopReason: r.words("volume/stop", 80), RefusedReads: []string{r.words("volume/refused", 30)}, RefusedReadsOmitted: r.n,
		Sizes: []record.VolumeSize{
			{Path: "scratch/" + r.words("volume/planted", 30) + "\n" + goldenHostile[r.n-1][5], Bytes: int64(10+r.n) << 30,
				BytesOverHour: int64(9+r.n) << 30, BytesOverDay: int64(r.n) << 30},
			{Path: "caches/" + r.words("volume/cache", 20), Bytes: 1 << 30, BytesOverHour: 1 << 20,
				Refused: "caches/.../" + r.words("volume/under", 20), Refusals: 2 + r.n},
		},
		SizesOmitted: r.n, SizesAt: r.at(120), SizesPartial: true,
		SizesMissing:        []record.VolumeMissing{{Path: "workspaces/" + r.key(102), Reason: "openat: " + r.words("volume/missing", 30)}},
		SizesMissingOmitted: r.n, Faults: []string{"panic: " + r.words("volume/fault", 50) + "\ngoroutine 9 [running]:"}, FaultsOmitted: r.n}
	b.notice(r, "sweep", arch, workflow.VolumeNearFullNotice(issue.Key, sweep),
		goldenWant("issue", issue.Key, "volume.podUid", pod, "volume.trimmer", sweep.Trimmer, "volume.episode", sweep.Episode,
			"volume.raisedBy", sweep.RaisedBy, "volume.usedBytes", sweep.UsedBytes, "volume.freeBytes", sweep.FreeBytes,
			"volume.freedBytes", sweep.FreedBytes, "volume.stopReason", sweep.StopReason, "volume.refusedReads", sweep.RefusedReads,
			"volume.sizes[0].path", strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(sweep.Sizes[0].Path), "volume.sizes[0].bytesOverDay", sweep.Sizes[0].BytesOverDay,
			"volume.sizes[1].refused", sweep.Sizes[1].Refused, "volume.sizesAt", sweep.SizesAt, "volume.sizesPartial", true,
			"volume.sizesMissing[0].path", sweep.SizesMissing[0].Path, "volume.sizesOmitted", r.n))
	off := record.VolumeNearFull{PodUID: pod, Trimmer: r.id("volume/off-trimmer"), Episode: 1, RaisedBy: shimwire.VolumeEpisodeFirst,
		UsedBytes: []int64{0, 45 << 30}[r.n-1], FreeBytes: []int64{0, 1 << 30}[r.n-1], StopReason: r.words("volume/off", 60),
		Faults: []string{"panic: " + r.words("volume/off-fault-1", 50) + "\ngoroutine 9 [running]:", "panic: " + r.words("volume/off-fault-2", 50),
			"panic: " + r.words("volume/off-fault-3", 50)}, SwitchedOff: true}
	b.notice(r, "switched-off", arch, workflow.VolumeNearFullNotice(issue.Key, off),
		goldenWant("issue", issue.Key, "volume.podUid", pod, "volume.switchedOff", true, "volume.usedBytes", off.UsedBytes,
			"volume.freeBytes", off.FreeBytes, "volume.stopReason", off.StopReason))
}

// goldenPostRefused is the error the outbox's post of body on issue ends with where Dispatch
// refuses the body for a block over its bound, word for word: the message executor's wrapper
// (messagePostFailed) around Dispatch's answer, 400 CAP_EXCEEDED with Dispatch's own words for the
// bound the body broke. Those words are dispatchbody.Check's, the check of the message route
// (packages/envoy/internal/dispatch/api/messages.go:validateCreateMessage, whose refusal
// server.go:blockBoundError answers with that code), made over the body as the outbox posts it,
// the message and then its row's marker (record.LongestPost: the marker is a block of its own
// after every other, so the refusal names the same block whatever the row's id), with each line
// ending written as a line feed, as the route writes the body before its check. A body Dispatch
// would take, or would refuse for something other than a bound, fails the build: each sample's
// refusal is the one Dispatch makes of that very body.
func goldenPostRefused(t *testing.T, issue, body string) error {
	t.Helper()
	over := dispatchbody.Check(goldenLineFeeds.Replace(record.LongestPost(body)))
	if over == nil || dispatchbody.IsDecisionBlock(over) {
		t.Fatalf("Dispatch refuses no bound of the body posted on %s (its check answers %v), so it does not answer its post CAP_EXCEEDED", issue, over)
	}
	return messagePostFailed(issue, &dispatch.Error{Status: http.StatusBadRequest, Code: "CAP_EXCEEDED", Message: over.Error()})
}

// goldenLineFeeds writes each CR LF, and then each lone carriage return, as a line feed, as the
// message route writes a body before it checks it (pmdoc.LineFeeds).
var goldenLineFeeds = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// goldenReadRefused is the error a status row on issue ends with where Dispatch refuses the issue
// read that opens it, word for word: the status executor's wrapper (statusReadFailed) around
// Dispatch's answer when it holds no such issue, 404 NOT_FOUND with the message "not found" (GET
// /api/v1/issues/{key}: api/issue_queries.go:readIssue answers pgx.ErrNoRows, which
// api/handler_errors.go:writeHandlerError writes so). dispatch.PermanentRefusal takes that as
// permanent, so the row is finished refused with neither write made (statusRefusalTold).
// ISSUE_CHANGED, Dispatch's refusal of a write fenced on a read the issue has moved past, is no
// such refusal: the daemon reads the issue again and writes again (writeStatus), and the row
// retries on its backoff.
func goldenReadRefused(issue string) error {
	return statusReadFailed(issue, &dispatch.Error{Status: http.StatusNotFound, Code: "NOT_FOUND", Message: "not found"})
}

// goldenChildWait is a parent whose own first phase waits for its 30 open children: 25 released
// and 5 not, the eleventh blocked by an issue above it on its own parent chain.
func goldenChildWait(r goldenRendering) record.ChildWait {
	wait := record.ChildWait{Parent: r.key(130)}
	statuses := []string{"todo", "in_progress", "testing", "needs_review", "backlog"}
	for i := range 30 {
		open := record.OpenChild{Key: r.key(131 + i), Status: statuses[(i+r.n)%len(statuses)], Released: i%6 != 5}
		if i == 10 {
			open.Cycle = wait.Parent
		}
		wait.Open = append(wait.Open, open)
	}
	return wait
}

// goldenStuckRequest is the pull request of a review round stuck at its head: its 60 check runs
// and its 30 commit statuses on that head (goldenChecks), settled there, and the merge state
// GitHub reports for it.
func goldenStuckRequest(r goldenRendering) record.PullRequest {
	checks := goldenChecks(r, "stuck")
	return record.PullRequest{Repo: r.repo, Number: 800 + r.n, HeadSHA: checks.head, CheckedHead: checks.head, CheckRuns: checks.runs,
		Statuses: checks.statuses, MergeState: []record.MergeState{record.MergeStateBlocked, record.MergeStateUnstable}[r.n-1]}
}

// controllerNotices adds every notice to the controller.
func (b *goldenBuild) controllerNotices(r goldenRendering) {
	ctl := claim.RoleController
	b.notice(r, "triage", ctl, admit.TriageNotice(r.key(200)), goldenWant("issue", r.key(200)))
	b.notice(r, "slot", ctl, admit.SlotFreeNotice(r.key(201)), goldenWant("issue", r.key(201)))
	b.notice(r, "todo", ctl, admit.TodoNotice(r.key(202)), goldenWant("issue", r.key(202)))

	// tick: its held and blocked lists and its 30 silent workers, whole.
	{
		held, blocked, silent := goldenTickLists(r)
		project := r.prefix
		b.notice(r, "tick", ctl, admit.TickNotice(project, held, blocked, silent),
			goldenWant("issue", project, "held#", len(held), "blocked#", len(blocked), "silent#", len(silent),
				"held[0].issue", held[0].Issue, "blocked[0].issue", blocked[0].Issue, "silent[0].issue", silent[0].Issue))
	}

	// unslotted: a root no tree of which runs, and one that awaits a person whose Dispatch claim a
	// person or a session outside its tree holds.
	root := record.Issue{Key: r.key(210), Tree: r.key(210), Status: "in_progress", Phase: phase.Planning}
	b.notice(r, "unslotted", ctl, admit.UnslottedNotice(root, "", ""), goldenWant("issue", root.Key, "status", "in_progress", "phase", phase.Planning))
	awaiting := root
	awaiting.Phase = phase.AwaitingPerson
	claimedBy := []string{"user:" + r.words("unslotted/holder", 8), "session:" + r.id("unslotted/holder")}[r.n-1]
	b.notice(r, "awaiting", ctl, admit.UnslottedNotice(awaiting, texts.UnresumableClaimed, claimedBy),
		goldenWant("issue", root.Key, "status", "in_progress", "phase", phase.AwaitingPerson, "unresumable", texts.UnresumableClaimed,
			"claimed_by", claimedBy))

	// awaiting-person: an ask, and a document's version.
	ask := r.id("awaiting/ask")
	b.notice(r, "ask", ctl, workflow.AwaitingPersonNotice(r.key(211), record.Awaiting{Ask: ask}, record.DesignStepSpec),
		goldenWant("issue", r.key(211), "ask", "dispatch://"+r.key(211)+"/ask/"+ask))
	b.notice(r, "document", ctl, workflow.AwaitingPersonNotice(r.key(211), record.Awaiting{GateVersion: 3 + r.n}, record.DesignStepSpec),
		goldenWant("issue", r.key(211), "document", "dispatch://"+r.key(211)+"/spec", "version", 3+r.n))

	// parked-decision: an ask's answer, a changes request with its reason, and a person's approval
	// of a parked root's spec that a later change of the root's status kept from queuing the root
	// as their handover (workflow's refuseHandover).
	{
		person := "user:" + r.words("parked/person", 6)
		answer := record.ParkedDecision{Issue: r.key(213), Ref: "dispatch://" + r.key(213) + "/ask/" + r.id("parked/ask"), Decision: record.DecisionAskAnswered,
			Actor: person, At: r.at(80), Words: &record.ActWords{Text: r.words("parked/answer", 4300), Options: []string{"yes " + r.words("parked/option", 4)},
				By: person, At: r.at(79)}}
		cut, _ := texts.Bound(answer.Words.Text, texts.OneAct)
		b.notice(r, "answer", ctl, record.ParkedDecisionNoticeOf(r.key(213), r.key(212), answer, 0),
			goldenWant("issue", r.key(213), "decision", string(record.DecisionAskAnswered), "root", r.key(212), "ref", answer.Ref, "actor", person,
				"at", r.at(79), "answer", cut, "words.answer.by", person, "words.answer.at", r.at(79)))
		changes := record.ParkedDecision{Issue: r.key(212), Ref: "dispatch://" + r.key(212) + "/spec", Decision: record.DecisionDesignChangesRequested,
			Actor: person, At: r.at(81), Words: &record.ActWords{Text: r.words("parked/reason", 900), By: person, At: r.at(81)}}
		b.notice(r, "changes", ctl, record.ParkedDecisionNoticeOf(r.key(212), r.key(212), changes, 6+r.n),
			goldenWant("issue", r.key(212), "decision", string(record.DecisionDesignChangesRequested), "root", r.key(212), "ref", changes.Ref,
				"version", 6+r.n, "reason", changes.Words.Text, "words.reason.by", person, "words.reason.at", r.at(81)))
		approval := record.ParkedDecision{Issue: r.key(214), Ref: "dispatch://" + r.key(214) + "/spec", Decision: record.DecisionDesignApproved,
			Actor: person, At: r.at(82)}
		refusal := record.HandoverRefusal{ApprovalSeq: int64(1000*r.n + 300), StatusChangedSeq: int64(1000*r.n + 310 + r.n), Status: []string{"backlog", "icebox"}[r.n-1]}
		b.notice(r, "refused-handover", ctl, record.RefusedHandoverNoticeOf(r.key(214), r.key(214), approval, 3+r.n, refusal),
			goldenWant("issue", r.key(214), "decision", string(record.DecisionDesignApproved), "root", r.key(214), "ref", approval.Ref, "actor", person,
				"at", r.at(82), "version", 3+r.n, "approval_seq", refusal.ApprovalSeq, "status_change_seq", refusal.StatusChangedSeq,
				"status", refusal.Status, "not_queued", true, "set_todo", r.key(214)))
	}

	// update-stalled: a round with a new image and 30 changed settings held by two pods, a refused
	// replacement, and each other hold: an operator's suspension of a claim of the round, which the
	// notice names apart from a claim the daemon's idle release suspended (suspended_by), no
	// process in the round's pod, no pod that any claim records, and a pod the runtime cannot read.
	{
		stall, drift := goldenUpdateStall(r)
		want := goldenWant("issue", stall.issue, "holds", stall.holds, "update.round", stall.round, "update.pod", stall.pod,
			"update.images", texts.ShortDigest(drift[0].Image), "update.settings#", 30)
		b.notice(r, "drift", ctl, updateStalledNotice(stall), want)
		refused := stall
		refused.holds, refused.refusal = texts.HoldsRuntimeRefused, r.words("stalled/refusal", 90)
		want = goldenWant("issue", stall.issue, "holds", texts.HoldsRuntimeRefused, "refusal", refused.refusal, "update.round", stall.round)
		b.notice(r, "refused", ctl, updateStalledNotice(refused), want)

		member := func(role claim.Role, state supervise.ClaimState, by supervise.SuspendSource) supervise.Claim {
			return supervise.Claim{Token: r.token(b.t, stall.issue, role), Issue: stall.issue, Role: role, Session: "ses-" + r.id("update/session/"+string(role)),
				State: state, SuspendedBy: by, UpdateRound: stall.round}
		}
		operator := stall
		operator.holds = texts.HoldsOperator
		operator.waiting = waitingOf(stall.round, noticeClaims([]supervise.Claim{
			member(claim.RoleImplementer, supervise.StateIdle, ""),
			member(claim.RoleTester, supervise.StateSuspended, supervise.SuspendedByOperator),
			member(claim.RoleReviewer, supervise.StateSuspended, supervise.SuspendedByReleased),
		}))
		b.notice(r, "operator", ctl, updateStalledNotice(operator),
			goldenWant("issue", stall.issue, "holds", texts.HoldsOperator, "update.round", stall.round, "update.pod", stall.pod,
				"update.waiting[1].role", claim.RoleTester, "update.waiting[1].suspended_by", string(supervise.SuspendedByOperator),
				"update.waiting[2].suspended_by", string(supervise.SuspendedByReleased)))
		// Every word suspended_by can say, as tellLateRound writes them (waitingOf, then
		// pauseStopped over the round's pause holds and the pause stops the store records, here
		// none): an unknown source, the operator's own suspension, an idle release, a project's
		// pause that still owes the planner a launch, the daemon's own suspension, which the merger
		// also took after its process came up in another pod during the round (left_round), and a
		// root's exit.
		marks := stall
		marks.holds, marks.round = texts.HoldsOperator, supervise.PodRoundPrefix+r.id("update/marks-round")
		marked := func(role claim.Role, by supervise.SuspendSource) supervise.Claim {
			c := member(role, supervise.StateSuspended, by)
			c.UpdateRound = marks.round
			return c
		}
		left := marked(claim.RoleMerger, supervise.SuspendedByDaemon)
		left.UpdateLeftRound = marks.round
		listed := noticeClaims([]supervise.Claim{
			marked(claim.RoleImplementer, supervise.SuspendedByUnknown),
			marked(claim.RoleTester, supervise.SuspendedByOperator),
			marked(claim.RoleReviewer, supervise.SuspendedByReleased),
			marked(claim.RolePlanner, supervise.SuspendedByReleased),
			left,
			marked(claim.RoleArchitect, supervise.SuspendedByExit),
		})
		marks.waiting = pauseStopped(waitingOf(marks.round, listed), listed,
			podHolds{{Pause: 40 + int64(r.n), Issue: stall.issue, Owed: []claim.Role{claim.RolePlanner}}}, nil)
		b.notice(r, "marks", ctl, updateStalledNotice(marks),
			goldenWant("issue", stall.issue, "holds", texts.HoldsOperator, "update.round", marks.round, "update.pod", stall.pod,
				"update.waiting[0].suspended_by", string(supervise.SuspendedByUnknown),
				"update.waiting[1].suspended_by", string(supervise.SuspendedByOperator),
				"update.waiting[2].suspended_by", string(supervise.SuspendedByReleased),
				"update.waiting[3].suspended_by", texts.SuspendedByPause,
				"update.waiting[4].suspended_by", string(supervise.SuspendedByDaemon), "update.waiting[4].left_round", true,
				"update.waiting[5].suspended_by", string(supervise.SuspendedByExit)))
		for _, hold := range []string{texts.HoldsNoProcess, texts.HoldsNoPod, texts.HoldsPodUnread} {
			held := stall
			held.holds = hold
			want := goldenWant("issue", stall.issue, "holds", hold, "update.round", stall.round, "update.pod", stall.pod)
			if hold == texts.HoldsNoPod {
				held.pod = ""
				delete(want, "update.pod")
			}
			b.notice(r, hold, ctl, updateStalledNotice(held), want)
		}
	}

	// provider-errors, tree-stalled, architect-stalled in each arm.
	b.notice(r, "errors", ctl, supervise.ProviderErrorsNotice(r.key(220), claim.RoleTester, 5+r.n, "model-"+r.words("model", 6), []int{529, 0}[r.n-1], false),
		goldenWant("issue", r.key(220), "role", claim.RoleTester, "turns", 5+r.n))
	b.notice(r, "errors-unprompted", ctl, supervise.ProviderErrorsNotice(r.key(221), claim.RoleImplementer, 5+r.n, "", []int{429, 0}[r.n-1], true),
		goldenWant("issue", r.key(221), "role", claim.RoleImplementer, "turns", 5+r.n, "last_turn_unprompted", true))
	{
		root := r.key(230)
		members := []record.Issue{
			{Key: root, Tree: root, Phase: phase.Reviewing, Status: "needs_review"},
			{Key: r.key(231), Tree: root, Phase: phase.Testing, Status: "testing"},
			{Key: r.key(232), Tree: root, Phase: phase.Admitted, Status: "todo"},
		}
		requests := map[string]record.PullRequest{root: {Repo: r.repo, Number: 900 + r.n, State: record.PullRequestMerged}}
		b.notice(r, "stopped", ctl, admit.TreeStalledNotice(root, record.TreeQuiet{Quiet: time.Duration(5+r.n) * time.Hour}, members, requests),
			goldenWant("issue", root, "members[0].key", root, "members[0].phase", phase.Reviewing, "members[0].pull_request",
				texts.PullRequestRef(r.repo, 900+r.n), "members[1].key", r.key(231), "members[1].phase", phase.Testing))
	}
	{
		root := record.Issue{Key: r.key(240), Tree: r.key(240), Phase: phase.Admitted}
		took := texts.ArchitectStalledRecord{Entry: 3 + int64(r.n), QueuedAt: r.at(-120), Interrupted: r.n == 1, Bound: workflow.StrandedReleaseWake.String()}
		if r.n == 1 {
			took.Since = r.at(-60)
		}
		b.notice(r, "took-nothing", ctl, admit.ArchitectStalledNotice(root, record.ArchitectTookNothing, took),
			goldenWant("issue", root.Key, "arm", string(record.ArchitectTookNothing), "phase", phase.Admitted, "entry", 3+int64(r.n)))
		unreachable := texts.ArchitectStalledRecord{ClaimState: string([]supervise.ClaimState{supervise.StateFailed, supervise.StateRetired}[r.n-1]),
			RelaunchFailing: r.n == 1}
		b.notice(r, "claim-unreachable", ctl, admit.ArchitectStalledNotice(root, record.ArchitectClaimUnreachable, unreachable),
			goldenWant("issue", root.Key, "arm", string(record.ArchitectClaimUnreachable), "phase", phase.Admitted, "claim_state", unreachable.ClaimState))
		slow := texts.ArchitectStalledRecord{Since: r.at(-300), Bound: workflow.ArchitectGateWake.String()}
		event := []string{texts.CountedFromPauseEnd, texts.CountedFromLaunchLetOut}[r.n-1]
		slow.CountedFrom, slow.CountedFromEvent, slow.PauseID = r.at(-90-r.n), event, 60+int64(r.n)
		b.notice(r, "slow-before-gate", ctl, admit.ArchitectStalledNotice(root, record.ArchitectSlowBeforeGate, slow),
			goldenWant("issue", root.Key, "arm", string(record.ArchitectSlowBeforeGate), "phase", phase.Admitted, "since", r.at(-300),
				"counted_from", r.at(-90-r.n), "counted_from_event", event, "pause_id", 60+int64(r.n)))
		confirmed := texts.ArchitectStalledRecord{Since: r.at(-200 - r.n), Bound: workflow.ArchitectGateWake.String(), Entry: 6 + int64(r.n)}
		confirmed.CountedFrom, confirmed.CountedFromEvent = r.at(-100-r.n), texts.CountedFromEntryConfirmed
		b.notice(r, "slow-confirmed", ctl, admit.ArchitectStalledNotice(root, record.ArchitectSlowBeforeGate, confirmed),
			goldenWant("issue", root.Key, "arm", string(record.ArchitectSlowBeforeGate), "phase", phase.Admitted, "since", r.at(-200-r.n),
				"counted_from", r.at(-100-r.n), "counted_from_event", texts.CountedFromEntryConfirmed, "entry", 6+int64(r.n)))
	}

	// memory-guard-kill, under the project key: the guard's kill of one of the controller's
	// processes, the kernel's kill of a command in a microVM controller pod, and the kernel's kill
	// of one of the processes that run the controller's session.
	for _, cause := range []shimwire.MemoryKillCause{shimwire.MemoryKillCauseGuard, shimwire.MemoryKillCauseKernel, shimwire.MemoryKillCauseKernelProtected} {
		report := shimwire.LauncherMemoryKill{PID: 5000 + r.n, Executable: "python-" + r.words("controller-kill/exe", 5), SizeBytes: uint64(1+r.n) << 30,
			ReadingBytes: uint64(5+r.n) << 30, CounterBytes: uint64(6+r.n) << 30, LimitBytes: uint64(6+2*r.n) << 30, Cause: cause}
		variant, word, pod := "guard", texts.KillGuard, 5+r.n
		switch cause {
		case shimwire.MemoryKillCauseKernel:
			variant, word, pod = "kernel", texts.KillKernel, 6+r.n
		case shimwire.MemoryKillCauseKernelProtected:
			variant, word, pod = "kernel-protected", texts.KillKernelProtected, 6+r.n
		}
		b.notice(r, variant, ctl, workflow.ControllerMemoryGuardKillNotice(r.prefix, report),
			goldenWant("issue", r.prefix, "cause", word, "process", report.Executable, "pid", report.PID,
				"size", fmt.Sprintf("%d.00 GiB", 1+r.n), "pod_memory", fmt.Sprintf("%d.00 GiB", pod), "limit", fmt.Sprintf("%d.00 GiB", 6+2*r.n)))
	}

	// project-paused, with a reason past the bound in the first rendering, and project-resumed:
	// the operator's pause and its resume, which tell the controller (the controller's own tell it
	// nothing).
	{
		reason := r.words("pause/reason", []int{4300, 120}[r.n-1]) + " " + goldenHostile[r.n-1][1]
		cut, _ := texts.Bound(reason, texts.OneAct)
		operator := store.PauseActor{Kind: store.PauseByOperator}
		p := store.Pause{ID: 40 + int64(r.n), Project: r.prefix, By: operator, Since: r.at(90), Reason: reason}
		b.notice(r, "paused", ctl, pause.PausedNotice(p),
			goldenWant("issue", r.prefix, "pause.id", p.ID, "pause.by.kind", string(store.PauseByOperator), "pause.at", r.at(90), "pause.reason", cut,
				"words.pause.reason.by", string(store.PauseByOperator), "words.pause.reason.at", r.at(90)))
		p.EndedAt, p.EndedBy = r.at(95+r.n), operator
		b.notice(r, "resumed", ctl, pause.ResumedNotice(p),
			goldenWant("issue", r.prefix, "pause.id", p.ID, "pause.by.kind", string(store.PauseByOperator), "pause.at", r.at(95+r.n)))
	}

	// intake-gave-up, under the project key: a burst of 150 give-ups that names its first 100, the
	// most one notice names, in the first rendering, and a burst of three, one of each consumer, in
	// the second. Each give-up is named by its record's id, the consumer that delivered it and its
	// subject.
	{
		count := []int{150, 3}[r.n-1]
		var giveUps []record.NoticeGiveUp
		for i := range min(count, 100) {
			consumer, subject := "legion-go-"+r.prefix+"-dispatch", "notifications.dispatch.issue."+r.key(400+i)+".issue.updated"
			switch i % 3 {
			case 1:
				consumer, subject = "legion-go-"+r.prefix+"-github", fmt.Sprintf("notifications.github.%s.pr.%d", strings.ReplaceAll(r.repo, "/", "."), 800+100*r.n+i)
			case 2:
				consumer, subject = "legion-go-"+r.prefix+"-wake", "notifications.legion."+r.prefix+".wake"
			}
			giveUps = append(giveUps, record.NoticeGiveUp{ID: int64(10000*r.n + 400 + i), Consumer: consumer, Subject: subject})
		}
		b.notice(r, "gave-up", ctl, record.GaveUpNotice(r.prefix, count, giveUps),
			goldenWant("issue", r.prefix, "count", count, "giveUps#", len(giveUps), "giveUps[0].id", giveUps[0].ID,
				"giveUps[0].consumer", giveUps[0].Consumer, "giveUps[0].subject", giveUps[0].Subject,
				"giveUps[1].consumer", giveUps[1].Consumer, "giveUps[1].subject", giveUps[1].Subject))
	}
}

// goldenTickLists is a tick's lists: 12 issues held, 173 blocked, 30 silent workers.
func goldenTickLists(r goldenRendering) ([]record.HeldIssue, []record.BlockedIssue, []record.SilentIssue) {
	var held []record.HeldIssue
	reasons := []record.HeldReason{record.HeldTreeClosed}
	for i := range 12 {
		held = append(held, record.HeldIssue{Issue: r.key(300 + i), Reason: reasons[0], HeldBy: []string{r.key(400 + i)}})
	}
	var blocked []record.BlockedIssue
	for i := range 173 {
		blocked = append(blocked, record.BlockedIssue{Issue: r.key(500 + i), BlockedBy: []string{r.key(700 + i%9)}})
	}
	var silent []record.SilentIssue
	for i := range 30 {
		silent = append(silent, record.SilentIssue{Issue: r.key(800 + i), Role: claim.RoleTester, LastStep: r.at(-30 - i), Silent: fmt.Sprintf("%dm0s", 30+i)})
	}
	return held, blocked, silent
}

// goldenUpdateStall is an update round held past its bound, opened for a new image and 30
// changed settings, and the drift its claims recorded.
func goldenUpdateStall(r goldenRendering) (updateStall, []runtime.DriftItem) {
	round := "round-" + r.id("update/round")
	drift := goldenDrift(r)
	group := []supervise.Claim{{Issue: r.key(250), Role: claim.RoleImplementer, UpdateRound: round, UpdateReasons: r.words("update/reasons", 60),
		UpdateDrift: drift}}
	return updateStall{issue: r.key(250), round: round, pod: "pod-" + r.id("update/pod"), holds: texts.HoldsTwoPods,
		waiting: []record.UpdateWaiting{{Claim: string(must(claim.NewToken(r.project, r.key(250), claim.RoleImplementer))), Role: claim.RoleImplementer,
			State: "working"}},
		group: group}, drift
}

// goldenDrift is an update round's drift: a new image and 30 changed settings.
func goldenDrift(r goldenRendering) []runtime.DriftItem {
	drift := []runtime.DriftItem{{Key: runtime.DriftImage, Image: "registry.example/legion-worker@sha256:" + r.sha("update/image") + r.sha("update/image2")[:24]}}
	for i := range 30 {
		drift = append(drift, runtime.DriftItem{Key: fmt.Sprintf("setting_%s_%02d", r.words(fmt.Sprintf("update/setting/%d", i), 6), i)})
	}
	return drift
}

// roleMessages adds every role message.
func (b *goldenBuild) roleMessages(r goldenRendering) {
	issue := r.key(260)

	// ci-settled: a head of 60 checks, 25 of which failed or were cancelled and 5 of which GitHub
	// skipped, and 30 commit statuses, 3 of which are in state failure or error, told to the worker
	// of the phase and to the architect.
	{
		checks := goldenChecks(r, "settled")
		pr := record.PullRequest{Repo: r.repo, Number: 1000 + r.n, HeadSHA: checks.head, CheckedHead: checks.head, CheckRuns: checks.runs,
			Failing: checks.failing, Cancelled: checks.cancelled, Statuses: checks.statuses}
		at := []phase.Phase{phase.Testing, phase.Reviewing}[r.n-1]
		reader := phase.RoleFor(at)
		want := checks.skippedFacts(checks.statusFacts(goldenWant("issue", issue, "pull_request", texts.PullRequestRef(r.repo, pr.Number), "head", checks.head, "phase", at,
			"list_passed", texts.ListPassed(r.repo, pr.Number), "not_passed#", len(checks.notPassed))))
		b.roleMessage(r, "settled", reader, issue, workflow.CISettledMessage(issue, pr, at),
			texts.UrgencyOf(texts.CISettled, reader, texts.Variant{Phase: at}), want)
	}

	// gate-moved: opened and closed.
	for _, open := range []bool{true, false} {
		variant, reader := "closed", claim.RoleTester
		if open {
			variant, reader = "opened", claim.RoleMerger
		}
		artifact := r.id("gate-moved/" + variant)
		gate, merge := texts.GateClosed, texts.MergeHeld
		if open {
			gate, merge = texts.GateOpened, texts.MergeReleased
		}
		b.roleMessage(r, variant, reader, issue, workflow.GateMovedMessage(issue, record.DesignStepSpec, artifact, 2+r.n, open),
			texts.UrgencyOf(texts.GateMoved, reader, texts.Variant{}),
			goldenWant("issue", issue, "gate", gate, "step", "spec", "document", "dispatch://"+issue+"/spec", "artifact", artifact,
				"version", 2+r.n, "merge", merge))
	}

	// gate-moved: an opening to a reader owed no news of the approved design, naming the version of
	// the step's document it took, since which nothing it holds changed (workflow's openingDelivery).
	{
		artifact := r.id("gate-moved/opened-unchanged")
		moved := workflow.GateMovedMessage(issue, record.DesignStepSpec, artifact, 2+r.n, true)
		moved.UnchangedSince = 1 + r.n
		b.roleMessage(r, "opened-unchanged", claim.RoleImplementer, issue, moved,
			texts.UrgencyOf(texts.GateMoved, claim.RoleImplementer, texts.Variant{}),
			goldenWant("issue", issue, "gate", texts.GateOpened, "step", "spec", "document", "dispatch://"+issue+"/spec", "artifact", artifact,
				"version", 2+r.n, "merge", texts.MergeReleased, "unchanged_since", 1+r.n))
	}

	// approved-round-stuck.
	{
		stuck := goldenStuckRequest(r)
		checks := goldenChecks(r, "stuck")
		b.roleMessage(r, "merge-state", claim.RoleReviewer, issue, workflow.ApprovedRoundStuckMessage(issue, stuck),
			texts.UrgencyOf(texts.ApprovedRoundStuck, claim.RoleReviewer, texts.Variant{}),
			checks.skippedFacts(checks.statusFacts(goldenWant("issue", issue, "pull_request", texts.PullRequestRef(r.repo, stuck.Number), "head", stuck.HeadSHA,
				"merge_state", string(stuck.MergeState), "list_passed", texts.ListPassed(r.repo, stuck.Number)))))
	}

	// memory-kill: the guard's kill, the kernel's kill of a command, and the kernel's kill of a
	// process that runs the role's session; each says where heavy work runs.
	for _, cause := range []shimwire.MemoryKillCause{shimwire.MemoryKillCauseGuard, shimwire.MemoryKillCauseKernel, shimwire.MemoryKillCauseKernelProtected} {
		report := shimwire.LauncherMemoryKill{PID: 4000 + r.n, Executable: "node-" + r.words("kill/exe", 5), SizeBytes: uint64(3+r.n) << 30,
			ReadingBytes: uint64(6+r.n) << 30, CounterBytes: uint64(7+r.n) << 30, LimitBytes: 8 << 30, Cause: cause}
		variant, word := "guard", texts.KillGuard
		switch cause {
		case shimwire.MemoryKillCauseKernel:
			variant, word = "kernel", texts.KillKernel
		case shimwire.MemoryKillCauseKernelProtected:
			variant, word = "kernel-protected", texts.KillKernelProtected
		}
		reader := []claim.Role{claim.RoleImplementer, claim.RoleArchitect}[r.n-1]
		b.roleMessage(r, variant, reader, issue, workflow.MemoryKillMessage(issue, report), texts.UrgencyOf(texts.MemoryKill, reader, texts.Variant{}),
			goldenWant("issue", issue, "cause", word, "process", report.Executable, "pid", report.PID))
	}

	// hook-prompt, as hooks.go's fireHook queues it, published as its text.
	{
		prompt := r.words("hook/prompt", 400) + "\nThe work has gone back the number of times recorded below. " + goldenHostile[r.n-1][4]
		hook := "hook-" + r.words("hook/name", 6)
		row := queuedRow(b.t, issue, record.HookPrompt{Hook: hook, Role: claim.RoleReviewer, From: phase.Reviewing, To: phase.Implementing,
			Count: 2 + r.n, Counts: map[string]int{"reviewing-implementing": 2 + r.n, "testing-implementing": r.n}, PullRequest: texts.PullRequestRef(r.repo, 1200+r.n),
			Prompt: prompt})
		text, err := row.Message(issue)
		if err != nil {
			b.t.Fatalf("the hook prompt's text: %v", err)
		}
		b.published(r, texts.HookPrompt, "fired", claim.RoleReviewer, issue, text, texts.UrgencyOf(texts.HookPrompt, claim.RoleReviewer, texts.Variant{}),
			goldenWant("issue", issue, "hook", hook, "from", phase.Reviewing, "to", phase.Implementing, "prompt", prompt, "count", 2+r.n,
				"pull_request", texts.PullRequestRef(r.repo, 1200+r.n)))
	}

	// update-ask: a round with a new image and 30 changed settings, as podupdate.go queues it from
	// supervise.UpdateTask.
	{
		round, pod := "round-"+r.id("ask/round"), "pod-"+r.id("ask/pod")
		drift := goldenDrift(r)
		endsAt := r.at(45)
		reader := []claim.Role{claim.RoleImplementer, claim.RoleArchitect}[r.n-1]
		ask := supervise.UpdateTask(issue, reader, round, pod, drift, r.words("ask/reasons", 50), endsAt)
		row := queuedRow(b.t, issue, record.UpdateAsk{Role: reader, Round: round, Pod: pod, Images: ask.Images, Settings: ask.Settings,
			Reasons: ask.Reasons, EndsAt: ask.EndsAt})
		b.published(r, texts.UpdateAsk, "drift", reader, issue, row.Record(issue), texts.UrgencyOf(texts.UpdateAsk, reader, texts.Variant{}),
			goldenWant("issue", issue, "round", round, "pod", pod, "images", texts.ShortDigest(drift[0].Image), "settings#", 30,
				"ends_at", endsAt, "push_to", claim.IssueBranch(issue)))
	}
	// update-ask to the reviewer, which creates no commit: its ask names no branch to push to, and
	// its steps of background work ask no commit or push (supervise.UpdateTask's reviewer form).
	{
		round, pod := "round-"+r.id("ask-reviewer/round"), "pod-"+r.id("ask-reviewer/pod")
		endsAt := r.at(50)
		ask := supervise.UpdateTask(issue, claim.RoleReviewer, round, pod, goldenDrift(r)[:1], "", endsAt)
		row := queuedRow(b.t, issue, record.UpdateAsk{Role: claim.RoleReviewer, Round: round, Pod: pod, Images: ask.Images, EndsAt: ask.EndsAt})
		b.published(r, texts.UpdateAsk, "reviewer", claim.RoleReviewer, issue, row.Record(issue), texts.UrgencyOf(texts.UpdateAsk, claim.RoleReviewer, texts.Variant{}),
			goldenWant("issue", issue, "round", round, "pod", pod, "images", ask.Images, "ends_at", endsAt))
	}

	// merge-queue-removal: read, and with no read of GitHub.
	for _, unread := range []bool{false, true} {
		variant := "read"
		if unread {
			variant = "unread"
		}
		head := r.sha("queue/" + variant)
		row := queuedRow(b.t, issue, record.MergeQueueRemoval{Number: 1300 + r.n, Head: head, Unread: unread})
		want := goldenWant("issue", issue, "pull_request", texts.PullRequestRef(r.repo, 1300+r.n), "head", head)
		if unread {
			want["unread"] = "true"
		}
		b.published(r, texts.MergeQueueRemoval, variant, claim.RoleMerger, issue, row.Record(issue, r.repo),
			texts.UrgencyOf(texts.MergeQueueRemoval, claim.RoleMerger, texts.Variant{}), want)
	}

	// The messages of a merge forward, as workflow's mergeForward, holdMerger and
	// mergeForwardLanded queue them, each published as its step's record: the ask to the
	// implementer (and one of a pull request recorded before the daemon kept the base, which names
	// the head and no base), the ask to the merger in merging (cybermonklabs/legion#4979), the hold
	// to the worker of the phase, the hold to a merger that entered merging while the implementer's
	// merge had not landed, the landed to that worker, and the review past the review.
	{
		base, head, landed := "main-"+r.words("forward/base", 5), r.sha("forward/head"), r.sha("forward/landed")
		number := 1400 + r.n
		ref := texts.PullRequestRef(r.repo, number)
		why := []record.ConflictWhy{record.ConflictCheckNotGreen, record.ConflictRequiresCurrent}[r.n-1]
		at := []phase.Phase{phase.Testing, phase.Reviewing}[r.n-1]
		pull := record.PullRequest{Repo: r.repo}
		ask := queuedRow(b.t, issue, record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: head, Base: base,
			Phase: at, PullRequest: number, Why: why})
		b.published(r, texts.MergeForwardAsk, "ask", claim.RoleImplementer, issue, ask.Record(issue, pull),
			texts.UrgencyOf(texts.MergeForwardAsk, claim.RoleImplementer, texts.Variant{Phase: at}),
			goldenWant("issue", issue, "pull_request", ref, "head", head, "conflicts_with", base, "phase", at, "why", why))
		unbasedHead := r.sha("forward/no-base/head")
		unbased := queuedRow(b.t, issue, record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: unbasedHead,
			Phase: at, PullRequest: number + 50, Why: record.ConflictRulesUnread})
		b.published(r, texts.MergeForwardAsk, "no-base", claim.RoleImplementer, issue, unbased.Record(issue, pull),
			texts.UrgencyOf(texts.MergeForwardAsk, claim.RoleImplementer, texts.Variant{Phase: at}),
			goldenWant("issue", issue, "pull_request", texts.PullRequestRef(r.repo, number+50), "head", unbasedHead, "phase", at,
				"why", record.ConflictRulesUnread))
		mergerAsk := queuedRow(b.t, issue, record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: head, Base: base,
			Phase: phase.Merging, PullRequest: number, Why: record.ConflictMergeNext})
		b.published(r, texts.MergeForwardAsk, "ask-merger", claim.RoleMerger, issue, mergerAsk.Record(issue, pull),
			texts.UrgencyOf(texts.MergeForwardAsk, claim.RoleMerger, texts.Variant{Phase: phase.Merging}),
			goldenWant("issue", issue, "pull_request", ref, "head", head, "conflicts_with", base, "phase", phase.Merging, "why", record.ConflictMergeNext,
				"conflict_reads", texts.ConflictReadsConfirmed))
		worker := phase.RoleFor(at)
		hold := queuedRow(b.t, issue, record.MergeForwardMessage{Role: worker, Step: record.MergeForwardHold, Head: head, Base: base,
			Phase: at, PullRequest: number, Why: why})
		b.published(r, texts.MergeForwardHold, "hold", worker, issue, hold.Record(issue, pull),
			texts.UrgencyOf(texts.MergeForwardHold, worker, texts.Variant{Phase: at}),
			goldenWant("issue", issue, "pull_request", ref, "head", head, "conflicts_with", base, "phase", at, "why", why))
		mergerHold := queuedRow(b.t, issue, record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardHold, Head: head, Base: base,
			Phase: phase.Merging, PullRequest: number, Why: why})
		b.published(r, texts.MergeForwardHold, "hold-merger", claim.RoleMerger, issue, mergerHold.Record(issue, pull),
			texts.UrgencyOf(texts.MergeForwardHold, claim.RoleMerger, texts.Variant{Phase: phase.Merging}),
			goldenWant("issue", issue, "pull_request", ref, "head", head, "conflicts_with", base, "phase", phase.Merging, "why", why,
				"asked", claim.RoleImplementer))
		told := queuedRow(b.t, issue, record.MergeForwardMessage{Role: worker, Step: record.MergeForwardLanded, Head: head, Landed: landed, Base: base,
			Phase: at, PullRequest: number, Why: why})
		b.published(r, texts.MergeForwardLanded, "landed", worker, issue, told.Record(issue, pull),
			texts.UrgencyOf(texts.MergeForwardLanded, worker, texts.Variant{Phase: at}),
			goldenWant("issue", issue, "pull_request", ref, "head", head, "landed", landed, "conflicts_with", base, "phase", at))
		past := []phase.Phase{phase.Accepting, phase.Merging}[r.n-1]
		review := queuedRow(b.t, issue, record.MergeForwardMessage{Role: claim.RoleReviewer, Step: record.MergeForwardReview, Head: head, Landed: landed,
			Base: base, Phase: past, PullRequest: number, Why: record.ConflictMergeNext})
		b.published(r, texts.MergeForwardReview, "review", claim.RoleReviewer, issue, review.Record(issue, pull),
			texts.UrgencyOf(texts.MergeForwardReview, claim.RoleReviewer, texts.Variant{Phase: past}),
			goldenWant("issue", issue, "pull_request", ref, "head", head, "landed", landed, "conflicts_with", base, "phase", past))
		// The landed to the merger in merging whose landing asked the reviewer to read the merge
		// (MergeForward.ReviewAsked): it names the new head as the approval it waits for.
		mergerLanded := queuedRow(b.t, issue, record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardLanded, Head: head, Landed: landed,
			Base: base, Phase: phase.Merging, PullRequest: number, Why: record.ConflictMergeNext})
		reviewAsked := record.PullRequest{Repo: r.repo, MergeForward: &record.MergeForward{Head: head, Base: base, Phase: phase.Merging,
			Why: record.ConflictMergeNext, Landed: landed, ReviewAsked: true}}
		b.published(r, texts.MergeForwardLanded, "landed-merger", claim.RoleMerger, issue, mergerLanded.Record(issue, reviewAsked),
			texts.UrgencyOf(texts.MergeForwardLanded, claim.RoleMerger, texts.Variant{Phase: phase.Merging}),
			goldenWant("issue", issue, "pull_request", ref, "head", head, "landed", landed, "conflicts_with", base, "phase", phase.Merging,
				"approval_owed", landed))
	}

	// idle-release-declined, as the release's workspace check queues it (declineFor, declinedRecord):
	// a workspace that holds commits origin does not hold, each named, and one whose working copy
	// holds a change beside more commits than the read names (shimwire.MaxWorkspaceWorkCommits),
	// which counts the others. The role the check read through is the open phase's worker, or the
	// architect where no worker is live.
	{
		reader := []claim.Role{claim.RoleImplementer, claim.RoleArchitect}[r.n-1]
		commits := make([]string, 2+r.n)
		for i := range commits {
			commits[i] = r.sha(fmt.Sprintf("declined/commit/%d", i))
		}
		b.roleMessage(r, "commits", reader, issue, declinedRecord(issue, runtime.WorkspaceWork{Head: r.sha("declined/head"), Count: len(commits), Commits: commits}),
			texts.UrgencyOf(texts.IdleReleaseDeclined, reader, texts.Variant{}),
			goldenWant("issue", issue, "changed", false, "commits", len(commits), "commit_ids", commits))
		named := make([]string, shimwire.MaxWorkspaceWorkCommits)
		for i := range named {
			named[i] = r.sha(fmt.Sprintf("declined/cut/%d", i))
		}
		head, count := r.sha("declined/cut/head"), shimwire.MaxWorkspaceWorkCommits+5*r.n
		b.roleMessage(r, "changed-cut", reader, issue, declinedRecord(issue, runtime.WorkspaceWork{Head: head, Changed: true, Count: count, Commits: named}),
			texts.UrgencyOf(texts.IdleReleaseDeclined, reader, texts.Variant{}),
			goldenWant("issue", issue, "changed", true, "head", head, "commits", count, "commit_ids", named, "more_commit_ids", count-len(named)))
	}
}

// catchUps adds the catch-up of a root's architect and of a child's own architect
// (cybermonklabs/legion#4203), each over a tree of 60 issues (goldenTree), queued by
// workflow.QueueCatchUp over a store that holds the tree (goldenStore).
func (b *goldenBuild) catchUps(r goldenRendering) {
	for _, variant := range []string{"root", "child"} {
		tree := goldenTreeOf(r, variant)
		store := &goldenStore{tree: tree}
		ended := supervise.EndedWork{Cause: "the process died: " + r.words("catch-up/cause", 30), Turn: true, Reported: r.n == 1}
		var relaunch *texts.RelaunchFacts
		if variant == "root" {
			facts := ended.Facts(false)
			relaunch = &facts
		}
		if err := workflow.QueueCatchUp(context.Background(), nil, store, tree.steps, tree.owner, 5+uint64(r.n), relaunch, r.at(90), time.Time{}); err != nil {
			b.t.Fatalf("queue the %s catch-up: %v", variant, err)
		}
		if len(store.queued) != 1 {
			b.t.Fatalf("the %s catch-up queued %d rows, want 1", variant, len(store.queued))
		}
		var notice record.Notice
		if err := json.Unmarshal(store.queued[0].Payload, &notice); err != nil {
			b.t.Fatalf("read the %s catch-up back: %v", variant, err)
		}
		want := goldenWant("issue", tree.owner.Key, "catch_up.generation", tree.owner.Generation, "catch_up.launch", 5+uint64(r.n),
			"catch_up.issues#", len(tree.notDone), "catch_up.done#", len(tree.done), "catch_up.unreleased#", len(tree.unreleased),
			"catch_up.accepting", tree.accepting)
		for i, key := range tree.notDone {
			want[fmt.Sprintf("catch_up.issues[%d].key", i)] = key
		}
		if variant == "root" {
			want["catch_up.gates[0].step"], want["catch_up.gates[0].version"] = "spec", fmt.Sprint(tree.gate.LatestVersion)
			want["catch_up.gates[0].open"], want["catch_up.gates[0].document"] = "false", "dispatch://"+tree.owner.Key+"/spec"
			want["relaunch.cause"] = ended.Cause
			// A turn cut off whose record names no work that stopped and no report: main's note said
			// that every other subagent and background job of the process is gone too.
			if !ended.Reported {
				want["relaunch.process_background_work"] = texts.ProcessBackgroundWorkGone
			}
			for i, decision := range tree.kept {
				want[fmt.Sprintf("catch_up.kept.decisions[%d].ref", i)] = decision.Ref
				want[fmt.Sprintf("words.catch_up.kept.decisions[%d].answer.by", i)] = decision.Actor
			}
			want["catch_up.kept.step"], want["catch_up.kept.document"] = "spec", "dispatch://"+tree.owner.Key+"/spec"
			want["catch_up.kept.approved"], want["catch_up.kept.latest"] = fmt.Sprint(tree.approval.Approved), fmt.Sprint(tree.approval.Latest)
		}
		b.queuedNotice(r, variant, claim.RoleArchitect, notice, want)
	}
}

// goldenTree is a tree of 60 issues an architect owns: 35 done, 20 recorded that are not, 3 of
// them in accepting, and 5 children their architects filed and have not released, which only the
// Dispatch mirror holds. variant root is the tree's root's; child is a child's own architect's,
// whose issue holds the 60 beneath it in a tree whose root is outside them.
type goldenTree struct {
	owner      record.Issue
	members    []record.Issue
	steps      []record.DesignStep
	gate       record.DesignGate
	children   map[string][]record.DispatchIssueRow
	claims     map[claim.Token]*record.RoleClaim
	kept       []record.ParkedDecision
	approval   record.KeptApproval
	notDone    []string
	done       []string
	unreleased []string
	accepting  []string
}

func goldenTreeOf(r goldenRendering, variant string) goldenTree {
	project := r.prefix
	base := 3000
	if variant == "child" {
		base = 4000
	}
	rootKey := r.key(base)
	tree := goldenTree{children: map[string][]record.DispatchIssueRow{}, claims: map[claim.Token]*record.RoleClaim{}}
	parent := func(key string) *string { return &key }
	root := record.Issue{Key: rootKey, Tree: rootKey, Project: project, Title: r.title(variant + "/root"), Phase: phase.Reviewing,
		Status: "needs_review", Generation: 7 + uint64(r.n)}
	tree.members = append(tree.members, root)
	owner := root
	if variant == "child" {
		owner = record.Issue{Key: r.key(base + 1), Tree: rootKey, Project: project, Parent: parent(rootKey), Title: r.title(variant + "/owner"),
			Phase: phase.Implementing, Status: "in_progress", Generation: 4 + uint64(r.n)}
		tree.members = append(tree.members, owner)
		tree.claims[claim.Token(must(claim.NewToken(r.project, owner.Key, claim.RoleArchitect)))] = &record.RoleClaim{State: supervise.StateWorking, Tree: rootKey}
	}
	tree.owner = owner
	tree.notDone = append(tree.notDone, owner.Key)
	phases := []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing, phase.Admitted}
	statuses := map[phase.Phase]string{phase.Planning: "in_progress", phase.Implementing: "in_progress", phase.Testing: "testing",
		phase.Reviewing: "needs_review", phase.Admitted: "todo", phase.Accepting: "needs_review", phase.Done: "done"}
	for i := range 54 {
		key := r.key(base + 10 + i)
		member := record.Issue{Key: key, Tree: rootKey, Project: project, Parent: parent(owner.Key), Title: r.title(fmt.Sprintf("%s/%d", variant, i)),
			Generation: 1}
		switch {
		case i < 35:
			member.Phase, member.Status = phase.Done, "done"
			tree.done = append(tree.done, key)
		case i < 38:
			member.Phase, member.Status = phase.Accepting, statuses[phase.Accepting]
			tree.accepting = append(tree.accepting, key)
			tree.notDone = append(tree.notDone, key)
		default:
			member.Phase = phases[i%len(phases)]
			member.Status = statuses[member.Phase]
			tree.notDone = append(tree.notDone, key)
		}
		if i%7 == 3 && i >= 38 {
			member.Parent = parent(r.key(base + 10 + 36))
		}
		tree.members = append(tree.members, member)
	}
	unreleased := map[string]bool{}
	for i := range 5 {
		key := r.key(base + 90 + i)
		under := owner.Key
		if i%2 == 1 {
			under = tree.notDone[len(tree.notDone)-1]
		}
		tree.children[under] = append(tree.children[under], record.DispatchIssueRow{Key: key, Project: project, Status: []string{"todo", "backlog", "in_progress"}[i%3],
			Parent: under})
		unreleased[key] = true
	}
	// A recorded child the mirror lists too, which is released.
	tree.children[owner.Key] = append(tree.children[owner.Key], record.DispatchIssueRow{Key: tree.notDone[1], Project: project, Status: "needs_review", Parent: owner.Key})
	// The catch-up names the children not released by parent in key order, each parent's in the
	// mirror's order (workflow's unreleasedChildren).
	for _, parent := range slices.Sorted(maps.Keys(tree.children)) {
		for _, child := range tree.children[parent] {
			if unreleased[child.Key] {
				tree.unreleased = append(tree.unreleased, child.Key)
			}
		}
	}
	slices.Sort(tree.notDone)
	slices.Sort(tree.done)
	slices.Sort(tree.accepting)
	if variant == "root" {
		tree.steps = []record.DesignStep{record.DesignStepSpec}
		tree.gate = record.DesignGate{Issue: rootKey, Step: record.DesignStepSpec, ArtifactID: r.id("catch-up/gate"), LatestVersion: 3 + r.n}
		person := "user:" + r.words("catch-up/person", 6)
		for i := range 2 {
			tree.kept = append(tree.kept, record.ParkedDecision{Issue: r.key(base + 10 + 40 + i), Ref: fmt.Sprintf("dispatch://%s/ask/%s", rootKey, r.id(fmt.Sprintf("catch-up/kept/%d", i))),
				Decision: record.DecisionAskAnswered, Actor: person, At: r.at(85 + i),
				Words: &record.ActWords{Text: r.words(fmt.Sprintf("catch-up/kept/%d", i), 300+1200*i), By: person, At: r.at(84 + i)}})
		}
		tree.approval = record.KeptApproval{Step: record.DesignStepSpec, Approved: 2 + r.n, Latest: tree.gate.LatestVersion}
	}
	return tree
}

// must is v, or a panic for err.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// goldenStore is the store a catch-up reads its tree from and queues its notice in: the reads of
// workflow.QueueCatchUp (treeCatchUp, unreleasedChildren, awaitingAcceptance's owner walk,
// ParkedDecisionTask) and its writes (DropCatchUps, EnqueueNaming). Every other method is the
// embedded nil Store's, which a read this sample does not expect fails on.
type goldenStore struct {
	record.Store
	tree   goldenTree
	queued []record.OutboxRow
}

func (s *goldenStore) HandedOverRoot(context.Context, pgx.Tx, string) (*record.HandedOverRoot, error) {
	return nil, nil
}

func (s *goldenStore) GatesOf(_ context.Context, _ pgx.Tx, issue string) (map[record.DesignStep]record.DesignGate, error) {
	if issue != s.tree.gate.Issue {
		return nil, nil
	}
	return map[record.DesignStep]record.DesignGate{s.tree.gate.Step: s.tree.gate}, nil
}

// DocumentIssues answers as the store does for the tree's one gate row, the root's own
// registration: its document is the root's own.
func (s *goldenStore) DocumentIssues(_ context.Context, _ pgx.Tx, artifacts []string) (map[string]string, error) {
	issues := map[string]string{}
	if slices.Contains(artifacts, s.tree.gate.ArtifactID) {
		issues[s.tree.gate.ArtifactID] = s.tree.gate.Issue
	}
	return issues, nil
}

func (s *goldenStore) TreeIssues(context.Context, pgx.Tx, string) ([]record.Issue, error) {
	return slices.Clone(s.tree.members), nil
}

func (s *goldenStore) DispatchChildren(_ context.Context, _ pgx.Tx, _ string, parents []string) (map[string][]record.DispatchIssueRow, error) {
	children := map[string][]record.DispatchIssueRow{}
	for _, parent := range parents {
		if rows := s.tree.children[parent]; len(rows) > 0 {
			children[parent] = rows
		}
	}
	return children, nil
}

func (s *goldenStore) IssuesOf(_ context.Context, _ pgx.Tx, _ string, keys []string) ([]record.Issue, error) {
	var found []record.Issue
	for _, member := range s.tree.members {
		if slices.Contains(keys, member.Key) {
			found = append(found, member)
		}
	}
	return found, nil
}

func (s *goldenStore) RoleClaims(_ context.Context, _ pgx.Tx, tokens []claim.Token) (map[claim.Token]*record.RoleClaim, error) {
	held := map[claim.Token]*record.RoleClaim{}
	for _, token := range tokens {
		if c, ok := s.tree.claims[token]; ok {
			held[token] = c
		}
	}
	return held, nil
}

func (s *goldenStore) UnclaimedKept(context.Context, pgx.Tx, string) ([]record.ParkedDecision, record.KeptApproval, error) {
	return s.tree.kept, s.tree.approval, nil
}

func (s *goldenStore) DropCatchUps(context.Context, pgx.Tx, string, time.Time) error { return nil }

func (s *goldenStore) Enqueue(_ context.Context, _ pgx.Tx, row record.OutboxRow) error {
	s.queued = append(s.queued, row)
	return nil
}

func (s *goldenStore) EnqueueID(_ context.Context, _ pgx.Tx, row record.OutboxRow) (int64, error) {
	s.queued = append(s.queued, row)
	return int64(len(s.queued)), nil
}

func (s *goldenStore) ClaimKeptDecisions(context.Context, pgx.Tx, string, record.KeptDecisions, int64) error {
	return nil
}
