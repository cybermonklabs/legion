package workflow

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A wake brings a released pod back. The release read the pod's workspace first and went on only
// where it held nothing that no branch or tag on origin holds; it suspended every claim of the pod
// with the source `released` and deleted the pod, its disk and its Sandbox, or, where the Sandbox
// owns a kept volume, kept that Sandbox Suspended with its volume. A wake resumes one of those
// claims, which relaunches its session in a fresh pod that provisions the workspace from the
// issue's pushed branch, or mounts the kept volume as the roles left it. Five things wake a pod,
// and this file holds four of them: the CI settlement, the role message and the agent message the
// Envoy listener reports, and the stored backstop. The fifth — a delivery the daemon already had
// for a role of the pod — needs nothing here, because a delivery to a suspended claim already
// resumes it (supervise's deliverResuming) and the delivery is its own account of why the pod came
// back.
//
// Every wake acts only on a claim that is still `released`, of an issue still in the workflow
// whose tree is live, and a role comes back once while its pod is on its way: the role's one open
// resume is what says so, so one head that settles again as each of its checks finishes wakes the
// pod once.
//
// The listener's report of a message is also a mark on the claim it names, whatever that claim's
// state (listenerWake, dispatch://INFRA-5682 R1): a wake of that claim that waits in the daemon's
// pacing, or the one this report raises, goes ahead of the wakes that carry no message, because
// Envoy keeps a role's message an hour and then drops it.

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
	// ClaimByToken and ClaimBySession are the claim a listener's report names, by the role token a
	// message was published to and by the session one was sent to, in whatever state it is. Every
	// daemon reads the report and only the one holding a claim of that name acts.
	ClaimByToken(ctx context.Context, tx pgx.Tx, token string) (supervise.Claim, bool, error)
	ClaimBySession(ctx context.Context, tx pgx.Tx, session string) (supervise.Claim, bool, error)
	// MarkMessage marks the claim token for an Envoy message that has waited since at, and
	// answers the mark it wrote, or else the mark that stands once it has: the earliest of the
	// marks the role has taken no turn since, and the zero time for a claim that takes none. A mark
	// written where none stood can be one the role has taken a turn since, which stands not at all
	// (the store's MarkClaimMessageTx).
	MarkMessage(ctx context.Context, tx pgx.Tx, token claim.Token, at time.Time) (time.Time, error)
	// TakePodWake takes one wake of issue's role and answers whether this call took it; false is
	// a wake another pass already took, which is already bringing the pod back. pause is the
	// project pause that holds the wake, 0 for none: a held wake records its pause and pairs with
	// no release (decision 15 of dispatch://INFRA-3759).
	TakePodWake(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, kind string, at time.Time, pause int64) (bool, error)
	// PauseOfWake is which pause holds a wake of the claim token, issue's role, by the store's one
	// rule of it (store's PodRecords.PauseOfWake), and which pause stopped the claim. mark is the
	// pause that held the outbox row the wake is taken for, which a wake of this package, taken
	// for no row, passes as 0. The wake is a held wake of held (decision 15), and the session it
	// brings back is one stopping stopped in a pod it kept (decision 16).
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
// of an issue in reviewing is told the entry of its round too (WakeTask says why).
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

// WakeStop is what stopped the session a wake brings back, which the wake's task names first.
type WakeStop int

const (
	// StoppedByRelease: an idle release stopped the session and took its pod, which the wake
	// brings back as a fresh pod. The release took the pod only because no working copy and no
	// bookmark of its workspace held a commit origin does not hold and no working-copy change was in
	// it (WorkspaceAfterRelease).
	StoppedByRelease WakeStop = iota
	// StoppedByPause: a project's pause stopped the session and kept its pod, its workspace and
	// its unpushed work as they were (decision 16 of dispatch://INFRA-3759).
	StoppedByPause
)

// WorkspaceAfterRelease is what a role woken after an idle release is told of its workspace, in
// the daemon's wake task (WakeTask) and in the notice the role's plugin writes when its launcher
// finds the workspace made again (WORKSPACE_AFTER_RELEASE in
// packages/pi-shared/src/workspace-recreated.ts). The two say it in the same words, so a woken
// role reads one account of its workspace, and what they say is what the daemon knows: the
// release's check keeps a pod whose workspace holds a commit on a working copy or a local bookmark
// that no branch or tag on origin holds, or a working-copy change (internal/daemon's
// checkIssueWorkspace, workspace.WorkRevset), and a pod whose disk went with it comes
// back with a workspace that provisioning made from the issue's pushed branch. The read does not
// count a commit that no working copy and no bookmark reaches, a side head a role left with
// `jj new <base>`, so the release can take one, and the text says so: it never says that nothing
// unpushed was lost, nor that unpushed work was kept, since the daemon keeps none. A kept volume is
// the one disk that does not go, which is why the second sentence is conditional. The text is the data file
// workspace-after-release.txt beside this one, embedded here and read by the plugin's test
// (packages/pi-shared/src/workspace-recreated.test.ts), which holds its copy to it byte for byte;
// the file's one line ends in a newline, which the text does not carry.
var WorkspaceAfterRelease = strings.TrimSuffix(workspaceAfterReleaseFile, "\n")

//go:embed workspace-after-release.txt
var workspaceAfterReleaseFile string

// WakeTask is the one task a woken role is given, the sentence its first turn reads before
// anything else: what stopped its session, which wake brought it back and what that wake was
// about. A message Envoy kept for the role can reach the woken session as a card before this task
// does, so the role message sentence says so. R6 asks for exactly one such task or message per
// wake, and the outbox row that carries it is written in the wake's own transaction, so a wake
// that commits has its task and one that does not has neither.
//
// The first sentences name what stopped the session: an idle release, which took the pod, or a
// project's pause, which kept it (stop, decision 16 of dispatch://INFRA-3759).
//
// The wake's task is never the phase's work, and for the reviewer it still ends with the daemon's
// two sentences of the review round (reviewround.Sentences), which the queue appends to it as to
// every task of the reviewer (queuedRow, namingTheReviewRound): in reviewing, while the row holds
// the task of an entry, they name that entry. The reason is the reviewer's normal path: one
// waiting for CI it cannot judge yet is released after the idle period and woken by the CI
// settlement, so the wake is its newest task, and the review it then submits moves the commit it
// read only when its footer names the entry that stands. The text here names no entry itself.
func WakeTask(kind, detail string, stop WakeStop) string {
	preamble := "Your issue's pod had been released while it was idle, and this wake brought it back. " +
		WorkspaceAfterRelease + " Your session is as you left it. "
	away, backstop := "while your pod was gone", "the release's backstop"
	if stop == StoppedByPause {
		preamble = "Your project was paused, and its pause stopped your session. This wake started it again. " +
			"Your pod, your workspace and any work you had not pushed are as you left them. "
		away, backstop = "while your session was stopped", "your backstop"
	}
	switch kind {
	case pods.WakeChecks:
		return preamble + "The wake is a CI settlement on your pull request's head: " + detail +
			". Read the checks before you decide anything."
	case pods.WakeRoleMessage:
		return preamble + "The wake is a message somebody published to your role topic (" + detail +
			") " + away + ". Envoy keeps a role message for one hour: a message kept for your role " +
			"reaches this session as an Envoy card that shows kept_at, possibly before this task, unless its " +
			"hour ended before you came back, and Envoy tells its sender when it drops one. " +
			"A message sent with keep: false is not kept, and its sender sends it again."
	case pods.WakeAgentMessage:
		return preamble + "The wake is a message somebody sent to your session (" + detail +
			") " + away + ". A message that Envoy or Dispatch stored for your session reaches you in this session. " +
			"A direct Envoy message that Envoy refused reaches you when its sender sends it again."
	case pods.WakeBackstop:
		return preamble + "The wake is " + backstop + " running out: " + detail +
			". Nothing else asked for you, so take up your phase where you left it."
	default:
		return preamble + "The wake is " + kind + ": " + detail + "."
	}
}

// listenerWake applies one wake report of the Envoy listener (intake.ListenerWake). A
// `listener_ready` report is the positive liveness signal R7's gate reads and wakes nothing. A
// role or agent report names a role token or a session, and this daemon acts only when one of its
// own claims has that name: every daemon consumes the subject, because the listener does not know
// which daemon owns a session and must not learn.
//
// The report marks that claim for the message it carried, in this transaction and whatever the
// claim's state (Pods.MarkMessage, dispatch://INFRA-5682 R1), and once the fact commits the
// claim's machine is told the mark the store holds (Config.MessageMarked), so a wake of the claim
// that already waits in the pacing moves to the message queue. A claim that is released is woken
// as well, and the start that wake queues finds the claim marked.
func (e *Engine) listenerWake(ctx context.Context, tx pgx.Tx, fact intake.ListenerWake) (intake.Result, error) {
	if e.cfg.Pods == nil {
		return intake.Result{}, nil
	}
	if fact.Kind == intake.ListenerWakeReady {
		return intake.Result{}, e.cfg.Pods.RecordListenerReady(ctx, tx, fact.At)
	}
	var named supervise.Claim
	var found bool
	var err error
	kind, detail := pods.WakeRoleMessage, fact.Role
	if fact.Kind == intake.ListenerWakeAgent {
		kind, detail = pods.WakeAgentMessage, fact.Session
		named, found, err = e.cfg.Pods.ClaimBySession(ctx, tx, fact.Session)
	} else {
		named, found, err = e.cfg.Pods.ClaimByToken(ctx, tx, fact.Role)
	}
	if err != nil || !found {
		return intake.Result{}, err
	}
	mark, err := e.cfg.Pods.MarkMessage(ctx, tx, named.Token, fact.At)
	if err != nil {
		return intake.Result{}, err
	}
	if notify := e.cfg.MessageMarked; notify != nil && !mark.IsZero() {
		store.OnCommit(ctx, func() { notify(named.Token, mark) })
	}
	if named.Released() {
		_, err = e.wakeReleased(ctx, tx, named, kind, detail, fact.At)
	}
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
// settlement changed the judgement. The answer is read and dropped here: checks applies the
// settlement and stores it in this same transaction. So a settlement of another head wakes
// nobody, whether an old head's or one of a head the record has not reached yet (its synchronize
// has not arrived), and checks' drop line names it (logUnappliedSettlement); and a redelivery of a
// settlement already applied wakes nobody either, since it applies nothing (dispatch://INFRA-4135
// R4; dispatch://INFRA-945 R3 item 2 wakes on a settlement the daemon records for the head of the
// issue's pull request).
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
	if settled, applied := classify.ApplyCheckSettlement(*pr, settlementCandidate(fact)); !applied ||
		settled.CheckedHead != fact.HeadSHA || settled.HeadSHA != fact.HeadSHA {
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
	// A list of commit statuses the decoder could not read names none, and the pull request keeps
	// the statuses it holds for the head when the settlement is applied, since a context a
	// settlement leaves out stays as it is kept (classify.ApplySettlement), so the task names the
	// failed ones of those, as the CI message does.
	if fact.StatusesUnread {
		fact.Statuses = keptStatuses(*pr, fact.HeadSHA)
	}
	woken, err := e.wakeReleased(ctx, tx, released, pods.WakeChecks, settlementDetail(fact), e.now())
	if err != nil || !woken {
		return "", err
	}
	return role, nil
}

// settlementDetail names the head and what its settlement came to, which is what the woken role's
// task carries: the checks the settlement names failed, then the ones it names cancelled, and
// `passed every check` when it names neither. After either of the first two it names each failed
// commit status of fact.Statuses (record.CommitStatus.Failed), in context order: the
// settlement's own, or, where the decoder could not read its list, the ones the pull request keeps
// for the head (wakeOnSettlement). A settlement that names a failed status and neither a failed
// nor a cancelled check run says the head passed every check run and then names the statuses,
// since the head did not pass every check. With no failed status the text is the one it was before
// the daemon read statuses.
//
// A check whose run GitHub skipped is named skipped, in name order, with the sentence that a
// skipped check ran nothing on this head, after the failed or the cancelled checks and before the
// failed statuses. A run of fact.CheckRuns that neither Failing nor Cancelled names is skipped
// when its conclusion is skipped (record.RunSkipped: the settlement's skipped group named it) or
// when GitHub's word for it is skipped (record.AttemptRun.GitHubConclusion): a settlement whose
// skipped group the decoder could not read carries the word and no skipped conclusion, and the CI
// message the roles are told names such a run by its word (settledChecks, shownAs), so the task
// names it skipped too and the two texts agree. Where the settlement names no check failed or
// cancelled the text then says the head had none, and never that it passed every check or every
// check run: a skipped check ran nothing, and a woken role that read it as a pass would believe a
// suite held back for the merge queue passed on a head where it never ran (dispatch://INFRA-4135
// R1).
//
// Each skipped name is quoted as a Go string (strconv.Quote), as the CI message quotes every
// check name: a check run's name is whoever wrote the workflow's text, and this task is read into
// an agent's prompt, so the quotes bound it, and a quote or a newline inside it is escaped rather
// than closing the one around it. The failed and cancelled names stay unquoted, as they were
// before the daemon read the skipped group, so a settlement with no skipped run gives the task it
// gave before, word for word (dispatch://INFRA-4135 R3), whatever other word GitHub gave each
// run: the word of a check that passed (success or neutral, which R1 counts as passed) adds
// nothing to `passed every check`, and the CI message is where each check is named by its word.
func settlementDetail(fact intake.PullRequestChecks) string {
	head := fact.HeadSHA
	if len(head) > 12 {
		head = head[:12]
	}
	var failed []string
	for _, status := range fact.Statuses {
		if status.Failed() {
			failed = append(failed, status.Context)
		}
	}
	slices.Sort(failed)
	var skipped []string
	for _, run := range fact.CheckRuns {
		ranNothing := run.Conclusion == record.RunSkipped || run.GitHubConclusion == string(record.RunSkipped)
		if ranNothing && !slices.Contains(fact.Failing, run.Name) && !slices.Contains(fact.Cancelled, run.Name) {
			skipped = append(skipped, run.Name)
		}
	}
	slices.Sort(skipped)
	for i, name := range skipped {
		skipped[i] = strconv.Quote(name)
	}
	var detail string
	switch {
	case len(fact.Failing) > 0:
		detail = head + " failed " + strings.Join(fact.Failing, ", ")
	case len(fact.Cancelled) > 0:
		detail = head + " had " + strings.Join(fact.Cancelled, ", ") + " cancelled"
	case len(skipped) > 0:
		detail = head + " had no failed or cancelled check"
	case len(failed) > 0:
		detail = head + " passed every check run"
	default:
		return head + " passed every check"
	}
	if len(skipped) > 0 {
		detail += "; skipped " + strings.Join(skipped, ", ") + ": " + skippedRanNothing
	}
	switch len(failed) {
	case 0:
		return detail
	case 1:
		return detail + "; failed commit status " + failed[0]
	default:
		return detail + "; failed commit statuses " + strings.Join(failed, ", ")
	}
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
	detail := fmt.Sprintf("the backstop armed for %s's %s came due", fact.Issue, fact.Role)
	_, err = e.wakeReleased(ctx, tx, released, pods.WakeBackstop, detail, e.now())
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
// names as what stopped the session (WakeTask, decision 16): the pause where one stopped it, and
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
func (e *Engine) wakeReleased(ctx context.Context, tx pgx.Tx, released supervise.Claim, kind, detail string, at time.Time) (bool, error) {
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
	e.logOnCommit(ctx, "wake: a released pod is coming back", "issue", issue.Key, "role", released.Role, "kind", kind, "detail", detail,
		"pause", pause, "stopped_by_pause", stopping)
	return true, e.enqueue(ctx, tx, issue.Key, wakeRow(*issue, released.Role, kind, pause, WakeTask(kind, detail, stop)))
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
