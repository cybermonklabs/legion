package workflow

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// A wake brings a released pod back. The release suspended every claim of the pod with the source
// `released` and deleted the pod, its disk and its Sandbox; a wake resumes one of those claims,
// which relaunches its session in a fresh pod, whose workspace provisioning makes again from the
// issue's pushed branch unless the pod is on a kept volume: the release took the pod only once its
// workspace held nothing that origin does not hold (StoppedByRelease). Five things wake a pod, and this file holds four of them: the CI settlement, the
// role message and the agent message the Envoy listener reports, and the stored backstop. The
// fifth — a delivery the daemon already had for a role of the pod — needs nothing here, because a
// delivery to a suspended claim already resumes it (supervise's deliverResuming) and the delivery
// is its own account of why the pod came back.
//
// Every wake acts only on a claim that is still `released`, of an issue still in the workflow
// whose tree is live, and a role comes back once while its pod is on its way: the role's one open
// resume is what says so, so one head that settles again as each of its checks finishes wakes the
// pod once.

// Pods is the daemon's record of its released pods as the workflow reads and writes it: the Envoy
// listener's liveness report, the claims a release left released, the wake that brings one back,
// and the backstop a wake fires. The daemon hands one in (Config.Pods, the store's own
// PodRecordsOf), which holds the project as the claim-keyed tables spell it, so the engine carries
// one project and reaches Postgres through an interface here as it does through record.Store.
//
// A wake is taken whole by TakePodWake: the resume recorded against the release the wake ends,
// or against the pause that holds it, which the role's one open resume refuses a second of, and
// the woken role's backstop cancelled. The store owns that order, and this package never writes a
// part of it.
type Pods interface {
	// RecordListenerReady stamps when the Envoy listener last said it is alive and reporting,
	// which is the whole of R7's gate.
	RecordListenerReady(ctx context.Context, tx pgx.Tx, at time.Time) error
	// ReleasedClaimOfRole is the released claim of issue's role, and whether there is one.
	ReleasedClaimOfRole(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (supervise.Claim, bool, error)
	// ReleasedClaimByToken and ReleasedClaimBySession are the released claim a listener's report
	// names, by the role token a message was published to and by the session one was sent to.
	// Every daemon reads the report and only the one holding a released claim of that name acts.
	ReleasedClaimByToken(ctx context.Context, tx pgx.Tx, token string) (supervise.Claim, bool, error)
	ReleasedClaimBySession(ctx context.Context, tx pgx.Tx, session string) (supervise.Claim, bool, error)
	// TakePodWake takes one wake of issue's role and answers whether this call took it; false is
	// a wake another pass already took, which is already bringing the pod back. pause is the
	// project pause that holds the wake, 0 for none: a held wake records its pause and pairs with
	// no release (decision 15 of dispatch://INFRA-3759).
	TakePodWake(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, kind string, at time.Time, pause int64) (bool, error)
	// PauseOfWake is which pause holds a wake of the claim token, issue's role, by the store's one
	// rule of it (store's PodRecords.PauseOfWake), and which pause stopped the claim. mark is the
	// pause that held the outbox row the wake is taken for, which a wake of this package, taken
	// for no row, passes as 0. The wake is a held wake of held (decision 15), and the session it
	// brings back is one stopping stopped (decision 16), in a pod that pause kept unless a later
	// release of the pod that did not stop the session took it.
	PauseOfWake(ctx context.Context, tx pgx.Tx, issue string, token claim.Token, mark int64) (held, stopping int64, err error)
	// ClosePodGoneResume closes the open resume of issue's role when the pod it was waiting for
	// has gone away: a release of the issue recorded since that wake was taken. It answers the
	// issue's newest release where it closed a row, and 0 where it closed none.
	ClosePodGoneResume(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (int64, error)
	// FirePodBackstop marks the backstop fired and answers whether this call fired it.
	FirePodBackstop(ctx context.Context, tx pgx.Tx, id int64) (bool, error)
	// VolumeNoticeCounts and RecordVolumeNotice are the bounds on what an issue's architect is
	// told about its pod's volume filling, counted in the store rather than in this process:
	// a daemon that restarts in the middle of a filling pod's episode must not start the count
	// over and tell that architect three more times (volumeNearFull). The read answers the
	// notices already written for this trimmer and episode and the newest notice of the pod
	// whatever line it was on; the write counts one against that line and answers the count it
	// now stands at. Both run inside the fact's transaction, the one that writes the notice's
	// outbox row, so a notice held for an absent architect has still spent its count.
	VolumeNoticeCounts(ctx context.Context, tx pgx.Tx, issue, podUID, trimmer string, episode int) (store.VolumeNoticeCounts, error)
	RecordVolumeNotice(ctx context.Context, tx pgx.Tx, issue, podUID, trimmer string, episode int, at time.Time) (int, error)
	// OpenPodBackstop is whether issue's role has a backstop armed and not yet fired: a release
	// that left the role with its phase open armed it, so the backstop brings the pod back. The
	// cutover's repair reads it to pass over a merger something will still wake
	// (cutoverMergerRepair).
	OpenPodBackstop(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) (bool, error)
}

// wakeRow is the outbox row a wake writes: a `start` of the woken role at the issue's current
// generation, naming no phase, carrying the task that says why the pod came back. It is the same
// row a phase start writes, which is the point — the outbox's start executor already resumes a
// suspended claim by rule (RequestRuleResume), already records the start that cancels a release
// still in flight (StartedBy, read by store.IssueReleaseStop), and already delivers the row's
// task once the relaunched agent is ready. Naming no phase keeps the row out of every phase
// fence: the issue's phase may move between the wake and the relaunch, and the wake's task is
// never the phase's work. It is queued as every worker's task is (queuedRow), so a woken reviewer
// of an issue in reviewing is told the entry of its round too (PodWake says why).
//
// The row is marked with the wake's kind, so the start finishes without acting once a park, a
// close or a moved phase has taken the role over while the row waited (decision 10 of
// dispatch://INFRA-3759), and with the pause that held the wake, 0 for none (decision 15), which
// the outbox's lines of the start name. The outbox holds every start of a paused project, marked
// or not (record's outboxPauseFree).
func wakeRow(issue record.Issue, role claim.Role, kind string, pause int64, task string) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: role, Generation: issue.Generation, Task: task,
		WakeKind: kind, Pause: pause}
}

// PauseWakeRow is the start a project's pause owes a role it stopped with a launch owed: the row
// the stop transaction writes beside its held wake (pods.WakePause, decision 6 of
// dispatch://INFRA-3759), in wakeRow's shape, marked with the wake's kind and the pause, so it
// finishes without acting once a park, a close or a moved phase has taken the role over (decision
// 10). The outbox holds it while a pause stands, as it holds every start of a paused project. It
// carries no task: the task the role holds pending, if any, is what its launch delivers.
func PauseWakeRow(issue record.Issue, role claim.Role, pause int64) record.SuperviseRequest {
	return wakeRow(issue, role, pods.WakePause, pause, "")
}

// WakeStop is what stopped the session a wake brings back, which the wake's record names
// (texts.PodWakeRecord.StoppedBy).
type WakeStop int

const (
	// StoppedByRelease: an idle release stopped the session and took its pod, which the wake
	// brings back as a fresh pod. The release took the pod only because no working copy and no
	// bookmark of its workspace held a commit origin does not hold and no working-copy change was in
	// it (internal/daemon's checkIssueWorkspace, the shim's workspaceWorkRevset). The wake's record
	// names the stop and carries no sentence of the workspace: the role's prompt does, in its
	// passage on a `pod-wake` task with `stopped_by: release` and, in the same words, in its passage
	// on the notice its plugin writes when its launcher finds the workspace made again (`notice:
	// workspace recreated`), in internal/prompts' go/worker-common.md and go/architect-common.md.
	// Those words say what the release keeps, what a workspace made again holds, and that a commit
	// no working copy and no bookmark reaches, a side head a role left with `jj new <base>`, is not
	// kept: never that nothing unpushed was lost, nor that unpushed work was kept, since the daemon
	// keeps none.
	StoppedByRelease WakeStop = iota
	// StoppedByPause: a project's pause stopped the session and kept its pod, its workspace and
	// its unpushed work as they were (decision 16 of dispatch://INFRA-3759), unless a later release
	// of the pod that did not stop the session, one that suspended other roles alone or none, took
	// the pod, which the wake then brings back as a fresh pod.
	StoppedByPause
)

// word is the stop as the pod-wake record names it (texts.StoppedByRelease,
// texts.StoppedByPause).
func (s WakeStop) word() string {
	if s == StoppedByPause {
		return texts.StoppedByPause
	}
	return texts.StoppedByRelease
}

// roleMessageKeep is how long Envoy keeps a message published to a role topic while no session
// holds the role (dispatch://INFRA-2846's R7), which a role-message wake names.
const roleMessageKeep = time.Hour

// PodWake is the one task a woken role is given, as the record of its kind: which wake brought
// its pod back to issue (kind, the pods.Wake words) and what that wake was about, other than a CI
// settlement (SettlementWake). detail is the role token a message was published to
// (pods.WakeRoleMessage), named as its topic with the facts dispatch://INFRA-2846's R7 requires: how
// long Envoy keeps a role message, that a kept message reaches the session as a card that shows
// kept_at, possibly before this task, unless its hour ended before the session came back, that
// Envoy tells its sender when it drops one, and that a message sent with keep: false is not kept
// and its sender sends it again. It is the session a message was sent to (pods.WakeAgentMessage),
// named with the facts dispatch://INFRA-3360's R3 puts in this task: a message that Envoy or
// Dispatch stored for the session reaches it in this session, and a direct Envoy message that
// Envoy refused reaches it when its sender sends it again. It is the role a backstop was armed
// for (pods.WakeBackstop), and what any other wake was about. R6 asks for exactly one such task
// or message per wake, and the outbox row that carries it is written in the wake's own
// transaction, so a wake that commits has its task and one that does not has neither.
//
// What stopped the session (stopped_by) is the wake's to name once it has read it
// (wakeReleased, decision 16 of dispatch://INFRA-3759): an idle release, which took the pod, or a
// project's pause, which kept it unless a later release of the pod that did not stop the session
// took it. The record PodWake builds leaves it for that.
//
// The wake's task is never the phase's work, and for the reviewer it still ends with the daemon's
// two sentences of the review round (reviewround.Ending), which the queue appends to it as to
// every task of the reviewer (queuedRow, namingTheReviewRound): in reviewing, while the row holds
// the task of an entry, they name that entry. The reason is the reviewer's normal path: one
// waiting for CI it cannot judge yet is released after the idle period and woken by the CI
// settlement, so the wake is its newest task, and the review it then submits moves the commit it
// read only when its footer names the entry that stands. The record here names no entry itself.
func PodWake(issue, kind, detail string) texts.PodWakeRecord {
	wake := texts.PodWakeRecord{TaskHead: texts.TaskHead{Kind: texts.PodWake, Issue: issue}, Wake: kind}
	switch kind {
	case pods.WakeRoleMessage:
		wake.Topic, wake.KeptFor, wake.KeptCardField = notify.RoleTopicPrefix+detail, roleMessageKeep.String(), "kept_at"
		wake.KeptCardMayComeFirst, wake.ExpiredDoesNotReach = true, true
		wake.SenderToldOfDrop, wake.NotKeptWith, wake.SenderSendsAgain = true, "keep: false", true
	case pods.WakeAgentMessage:
		wake.Session, wake.StoredReachesSession, wake.RefusedSenderSendsAgain = detail, true, true
	case pods.WakeBackstop:
		wake.ArmedFor = detail
	default:
		wake.Detail = detail
	}
	return wake
}

// WakeTask is PodWake's record as the woken role reads it, naming stop as what stopped its
// session.
func WakeTask(issue, kind, detail string, stop WakeStop) string {
	wake := PodWake(issue, kind, detail)
	wake.StoppedBy = stop.word()
	return taskText(wake)
}

// listenerWake applies one wake report of the Envoy listener (intake.ListenerWake). A
// `listener_ready` report is the positive liveness signal R7's gate reads and wakes nothing. A
// role or agent report names a role token or a session, and this daemon acts only when one of its
// own claims is released under that name: every daemon consumes the subject, because the listener
// does not know which daemon owns a session and must not learn.
func (e *Engine) listenerWake(ctx context.Context, tx pgx.Tx, fact intake.ListenerWake) (intake.Result, error) {
	if e.cfg.Pods == nil {
		return intake.Result{}, nil
	}
	if fact.Kind == intake.ListenerWakeReady {
		return intake.Result{}, e.cfg.Pods.RecordListenerReady(ctx, tx, fact.At)
	}
	var released supervise.Claim
	var found bool
	var err error
	kind, detail := pods.WakeRoleMessage, fact.Role
	if fact.Kind == intake.ListenerWakeAgent {
		kind, detail = pods.WakeAgentMessage, fact.Session
		released, found, err = e.cfg.Pods.ReleasedClaimBySession(ctx, tx, fact.Session)
	} else {
		released, found, err = e.cfg.Pods.ReleasedClaimByToken(ctx, tx, fact.Role)
	}
	if err != nil || !found {
		return intake.Result{}, err
	}
	_, err = e.wakeReleased(ctx, tx, released, PodWake(released.Issue, kind, detail), detail, fact.At)
	return intake.Result{}, err
}

// wakeOnSettlement wakes a released pod on a CI settlement the daemon records for the head of its
// issue's pull request, which is R2. It runs before the settlement is applied, so the woken role
// is told of the settlement the same transaction records.
//
// A settlement wakes only where it is the settlement checks applies for the record's head: the
// wake builds the candidate checks applies (settlementCandidate) and asks the apply step itself
// (classify.ApplyCheckSettlement), and wakes only when that applies it and leaves the record with
// the settlement's head as its head and as the head its checks stand at. The second half is not
// implied by the first while a head run is open, where applied says only that the replay with the
// settlement changed the judgement. The record the apply step returns is not stored here: checks
// applies the settlement and stores it in this same transaction, and the wake reads only its
// commit statuses (below). So a settlement of another head wakes nobody, whether an old head's or
// one of a head the record has not reached yet (its synchronize has not arrived), and checks' drop
// line names it (logUnappliedSettlement); and a redelivery of a settlement already applied wakes
// nobody either, since it applies nothing (dispatch://INFRA-4135 R4; dispatch://INFRA-945 R3 item 2
// wakes on a settlement the daemon records for the head of the issue's pull request).
//
// Only the worker of the pod's open phase is woken: the settlement is that worker's business and
// no one else's, and an architect reads it from its own notices. A head that settles again as each
// of its checks finishes wakes the pod no second time, because the role's resume is still open.
//
// Asking the apply step also keeps a settlement delivered late, or replayed from a message the
// intake gave up on, however late, from waking anyone where the record will not take it for the
// pull request's head now (dispatch://INFRA-1575 R1): one of a head the pull request has left, or
// one of its head older than what the record already took. The apply step weighs it against the
// fence the record would apply it to (classify.SettlementFor), which is the one the record
// remembers for the head when its fence was reset, so it is stale exactly where it is with the
// fence standing, and the woken worker's task would otherwise name a verdict that is no longer
// the head's.
func (e *Engine) wakeOnSettlement(ctx context.Context, tx pgx.Tx, fact intake.PullRequestChecks) (claim.Role, error) {
	if e.cfg.Pods == nil || fact.HeadSHA == "" {
		return "", nil
	}
	pr, err := e.pullRequest(ctx, tx, fact.Repo, fact.Number)
	if err != nil || pr == nil {
		return "", err
	}
	settled, applied := classify.ApplyCheckSettlement(*pr, settlementCandidate(fact))
	if !applied || settled.CheckedHead != fact.HeadSHA || settled.HeadSHA != fact.HeadSHA {
		return "", nil
	}
	issue, err := e.store.Issue(ctx, tx, pr.Issue)
	if err != nil || issue == nil {
		return "", err
	}
	role := OpenPhaseRole(issue.Phase)
	if role == "" {
		return "", nil
	}
	released, found, err := e.cfg.Pods.ReleasedClaimOfRole(ctx, tx, issue.Key, role)
	if err != nil || !found || released.ServingGeneration != issue.Generation {
		return "", err
	}
	// The role is the settlement's to tell only when this call is what woke it: a wake another
	// pass took is already bringing the pod back with a task of its own, and a claim this
	// settlement did not wake is told as any live role is (tellChecks).
	//
	// The task names the commit statuses the pull request keeps for the head once this settlement
	// is applied, as the CI message of the same settlement does: a settlement's statuses are
	// merged into the kept ones per context, so a context it leaves out, a failed one included,
	// stays as it is kept (classify.ApplySettlement), whether the listener's record dropped it or
	// the decoder could not read the list (intake.PullRequestChecks.StatusesUnread).
	fact.Statuses = keptStatuses(settled, fact.HeadSHA)
	woken, err := e.wakeReleased(ctx, tx, released, SettlementWake(issue.Key, issue.Phase, pr.Repo, fact), fact.HeadSHA, e.now())
	if err != nil || !woken {
		return "", err
	}
	return role, nil
}

// SettlementWake is the task of the role of issue's phase p whose released pod a CI settlement on
// its pull request's head woke (pods.WakeChecks), as the record of its kind: the pull request (of
// repo), the head the settlement is for, the phase, each check that did not pass with what it came
// to, each check GitHub skipped with the field that says once that such a check ran nothing
// (texts.EachSkippedCheck), each commit status of fact.Statuses in state failure or error with
// its state, the count of the checks that passed, the count of the statuses that passed, and the
// command that lists every check (tallyChecks). It reads the settlement's checks as settledChecks
// reads the record's (shownChecks): a check the settlement names failed or cancelled is that, one
// whose run GitHub skipped - its conclusion skipped, or GitHub's word for it skipped where the
// decoder could not read the skipped group - is named skipped, so the task and the CI message of
// the same settlement agree, and the woken role never reads a skipped check as a pass
// (dispatch://INFRA-4135 R1). The settlement this one wake names is the fact's own, since the wake
// runs before the fact is applied (wakeOnSettlement). Its statuses are the ones the pull request
// keeps for the head once the settlement is applied, which wakeOnSettlement puts in fact.Statuses.
func SettlementWake(issue string, p phase.Phase, repo string, fact intake.PullRequestChecks) texts.PodWakeRecord {
	tally := tallyChecks(shownChecks(fact.CheckRuns, fact.Cancelled, fact.Failing), fact.Statuses)
	return texts.PodWakeRecord{TaskHead: texts.TaskHead{Kind: texts.PodWake, Issue: issue}, Wake: pods.WakeChecks,
		PullRequest: texts.PullRequestRef(repo, fact.Number), Head: fact.HeadSHA, Phase: p, NotPassed: tally.notPassed,
		Skipped: tally.skipped, EachSkippedCheck: texts.EachSkippedCheck(tally.skipped), NotPassedStatuses: tally.notPassedStatuses,
		Passed: &tally.passed, PassedStatuses: tally.passedStatuses, ListPassed: texts.ListPassed(repo, fact.Number)}
}

// podBackstopDue wakes a released pod on its backstop, which is R5: backstop_minutes after the
// release, whatever else did or did not happen. The row is fired under a conditional update, so a
// backstop two passes read fires once, and the row survives a restart, so one that came due while
// the daemon was down fires at the first pass after it starts.
func (e *Engine) podBackstopDue(ctx context.Context, tx pgx.Tx, fact intake.PodBackstopDue) (intake.Result, error) {
	if e.cfg.Pods == nil {
		return intake.Result{}, nil
	}
	fired, err := e.cfg.Pods.FirePodBackstop(ctx, tx, fact.ID)
	if err != nil || !fired {
		return intake.Result{}, err
	}
	released, found, err := e.cfg.Pods.ReleasedClaimOfRole(ctx, tx, fact.Issue, fact.Role)
	if err != nil || !found {
		return intake.Result{}, err
	}
	_, err = e.wakeReleased(ctx, tx, released, PodWake(fact.Issue, pods.WakeBackstop, string(fact.Role)), string(fact.Role), e.now())
	return intake.Result{}, err
}

// wakeReleased is the one act every wake of this file performs, and the one place the gates are
// read: the claim is still released, its issue is one this daemon records, and its tree is live —
// neither the issue nor its root has left the workflow and neither lingers
// (pods.InWorkflowNotLingering). A pod in any other state is a close's to stop or an operator's to
// resume, never a wake's.
//
// A claim of another project's issue, which the operator's spawn makes under this daemon's token
// and a project's pause stops as released, finds no record here, even where the database it shares
// with that project's daemon holds that daemon's record of the issue: the daemon's store reads no
// record of another project (internal/daemon's projectRecords, which says why). Read as this
// daemon's, that record would also leave the resume this wake took open, refusing every later wake
// of the role. So such a wake is refused as the wake of every operator's spawn of an issue with no
// record is: it takes nothing and queues nothing. What brings such a claim back is what brings back
// every operator's spawn: the resume of the pause that stopped it owing a launch
// (internal/daemon's resumeOwedSpawns), or the operator's own resume or delivery
// (`legion claims resume`, `legion claims deliver`).
//
// A wake of a role whose resume is already open wakes nothing a second time: that pod is coming
// back, and the start row of the wake that took it carries the task. A wake that takes it records
// itself against the release it ends and cancels the woken role's backstop (Pods.TakePodWake), and
// writes the one outbox row that resumes the role and gives it the task naming the wake. Both are
// this transaction's, so a wake this daemon took is a wake whose start row is queued: a take that
// committed without one is what wedges a role, which is why the outbox's own delivery wake, which
// resumes a claim in process instead, heals a refused wake (wakeForDelivery).
//
// Which pause holds the wake is the store's one rule of it (Pods.PauseOfWake, decision 15 of
// dispatch://INFRA-3759), read on this fact's own transaction. A wake of this file is taken for no
// outbox row, so it passes no row's mark. A held wake is recorded on the resume, which pairs with
// no release, and on the start row, which the outbox holds while a pause stands: what kept the
// role down was the pause, not a release. The pause that stopped the role is also what the task
// names as what stopped the session (stopped_by, decision 16): the pause where one stopped it, and
// the release otherwise.
//
// One row would refuse a wake wrongly, and it is closed before the take rather than read out of a
// refusal: the resume of a pod that has gone away. A release of a pod no claim of the issue can
// use ends no wake and so closes no resume, and it still takes the pod, leaving a row that would
// otherwise refuse every later wake of that role — a CI settlement queueing nothing, and R5's
// backstop spent on a refusal, since podBackstopDue fires the row before it calls this. So each
// wake first closes a resume of this role written before the issue's newest release
// (Pods.ClosePodGoneResume), which matches nothing while a pod is on its way back, since such a
// resume was itself written after every release there is.
func (e *Engine) wakeReleased(ctx context.Context, tx pgx.Tx, released supervise.Claim, wake texts.PodWakeRecord, detail string, at time.Time) (bool, error) {
	kind := wake.Wake
	issue, err := e.store.Issue(ctx, tx, released.Issue)
	if err != nil || issue == nil {
		return false, err
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil || root == nil {
		return false, err
	}
	if !pods.InWorkflowNotLingering(issue.Standing(), root.Standing(), record.OutOfWorkflow) {
		return false, nil
	}
	pause, stopping, err := e.cfg.Pods.PauseOfWake(ctx, tx, issue.Key, released.Token, 0)
	if err != nil {
		return false, err
	}
	newest, err := e.cfg.Pods.ClosePodGoneResume(ctx, tx, issue.Key, released.Role)
	if err != nil {
		return false, err
	}
	if newest > 0 {
		e.warnOnCommit(ctx, "wake: a pod went away under an open resume of a released role; this wake closes that resume and takes its own",
			"issue", issue.Key, "role", released.Role, "newest_release", newest, "kind", kind, "detail", detail)
	}
	taken, err := e.cfg.Pods.TakePodWake(ctx, tx, issue.Key, released.Role, kind, at, pause)
	if err != nil || !taken {
		return false, err
	}
	stop := StoppedByRelease
	if stopping != 0 {
		stop = StoppedByPause
	}
	wake.StoppedBy = stop.word()
	e.logOnCommit(ctx, "wake: a released pod is coming back", "issue", issue.Key, "role", released.Role, "kind", kind, "detail", detail,
		"pause", pause, "stopped_by_pause", stopping)
	return true, e.enqueue(ctx, tx, issue.Key, wakeRow(*issue, released.Role, kind, pause, taskText(wake)))
}

// OpenPhaseRole is the phase worker of an open phase, and "" where there is none: a phase no role
// works, and the architect, whose phases (admitted and accepting) are the architect's own turn
// rather than a worker's. It is the one statement of "the worker of the open phase" the release
// reads: which role a release arms a backstop for — an architect gets none, because nothing of
// its own is due and a sub-architect woken for nothing would hold a pod open for the backstop
// period — which role a CI settlement wakes, and which live claim of a pod has a phase reminder
// armed in it.
func OpenPhaseRole(p phase.Phase) claim.Role {
	if role := phase.RoleFor(p); role != claim.RoleArchitect {
		return role
	}
	return ""
}
