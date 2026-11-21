package workflow

import (
	"context"
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

// A wake brings a released pod back. The release suspended every claim of the pod with the source
// `released` and deleted the pod, its disk and its Sandbox; a wake resumes one of those claims,
// which relaunches its session in a fresh pod whose launcher restores the workspace bundle the
// release saved. Seven things wake a pod, and this file holds four of them: the CI settlement, the
// role message and the agent message the Envoy listener reports, and the stored backstop. The
// fifth, the time of a wait the released worker declared, and the sixth, a move of the design gate
// over a released merger, wake through wakeReleased as they do (waitDue, wait.go; deliverGateMove,
// gate.go). The seventh — a delivery the daemon already had for a role of the pod — needs
// nothing here, because a delivery to a suspended claim already resumes it (supervise's
// deliverResuming) and the delivery is its own account of why the pod came back.
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
	// IssueReleasing is whether an idle release of issue's pod is in progress: the release's mark
	// stands (issue_releases), so its rows may still stop the pod's roles and take the pod away.
	// The design gate's move reads it (deliverGateMove): a role message to a merger whose pod is
	// being released can find no session to reach.
	IssueReleasing(ctx context.Context, tx pgx.Tx, issue string) (bool, error)
	// CancelPodBackstops drops the open backstops of issue's role in the caller's transaction:
	// the design gate's opening cancels the backstop of every merger of the tree in merging, since
	// the opening's one delivery is that merger's task for the opening (deliverGateMove).
	CancelPodBackstops(ctx context.Context, tx pgx.Tx, issue string, role claim.Role) error
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
	// brings back as a fresh pod with the workspace the release saved.
	StoppedByRelease WakeStop = iota
	// StoppedByPause: a project's pause stopped the session and kept its pod, its workspace and
	// its unpushed work as they were (decision 16 of dispatch://INFRA-3759).
	StoppedByPause
)

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
		"Your session, your workspace and any work you had not pushed are as you left them. "
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
		// "As it stands" and not "where you left it": a merger's backstop wake can reach it behind
		// the note of a closed design gate (MergerGateNote), and its phase as it stands then is to
		// merge nothing, which a call to take up its work where it left it would contradict.
		return preamble + "The wake is " + backstop + " running out: " + detail +
			". Nothing else asked for you, so go on with your phase as it stands."
	case pods.WakeWaitTime, pods.WakeDesignGate:
		// The detail is the wake's own task — the wait's (waitTimeTask) or the gate's
		// (gateMovedTask) — which says what the wake is. The gate's state goes in front of the whole
		// prompt at its send (the start's GateMessage mark, MergerGateMessage).
		return preamble + detail
	default:
		return preamble + "The wake is " + kind + ": " + detail + "."
	}
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
	_, err = e.wakeReleased(ctx, tx, released, kind, detail, fact.At)
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
	if err != nil || woken == 0 {
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
//
// A merger the design gate holds is not woken by its backstop while the gate is closed
// (dispatch://INFRA-2040 R1, D13 of its plan): its turn would come during the hold with nothing
// it may do. The hold is read in this fact's own transaction before the row is fired
// (backstopHeld), and while it holds the fact writes nothing and answers Unspent: the row stays
// due and unfired and no resume is recorded, so pod_resumes_one_open_per_role refuses no later
// wake of the merger (a CI settlement, a message), and intake.ApplyFact forgets the event id, so
// the first tick that reads the gate open applies the same pod-backstop:<id> again and takes it.
// That is the merger's one wake of an opening no gate row writes, such as `gates.design` emptied
// across a restart. The wake tick passes over such a backstop before it applies any fact
// (HeldMergers, internal/daemon's fireDueBackstops); this read decides a fact that raced the
// gate's, closed after the tick read it open.
func (e *Engine) podBackstopDue(ctx context.Context, tx pgx.Tx, fact intake.PodBackstopDue) (intake.Result, error) {
	if e.cfg.Pods == nil {
		return intake.Result{}, nil
	}
	if held, err := e.backstopHeld(ctx, tx, fact.Issue, fact.Role); err != nil || held {
		return intake.Result{Unspent: held}, err
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

// backstopHeld is whether the design gate holds role's work on key (HeldByDesignGate), read in tx
// from the issue and its tree's gate rows under the deployment's gated steps. Only a merger can be
// held, so no other role's backstop reads anything; an issue the record does not hold is held by
// nothing, and its backstop finds no claim to wake.
func (e *Engine) backstopHeld(ctx context.Context, tx pgx.Tx, key string, role claim.Role) (bool, error) {
	if role != claim.RoleMerger {
		return false, nil
	}
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return false, err
	}
	rows, err := e.store.GatesOf(ctx, tx, issue.Tree)
	if err != nil {
		return false, err
	}
	_, held := HeldByDesignGate(*issue, role, record.TreeGate{Steps: e.cfg.Steps, Rows: rows})
	return held, nil
}

// HeldMergers is, of issues, every one whose merger the design gate holds (HeldByDesignGate), with
// the closed gate's row that holds it, by issue. It is the wake tick's own filter
// (internal/daemon's fireDueBackstops), read once per tick over the issues of the mergers'
// backstops that came due, so a held merger's backstop costs that read each minute and no fact's
// transaction, as the silence sweep's filter spares a held merger (QuietByDesign). podBackstopDue
// asks the same rule again inside each fact, so a gate that closed after this read is decided
// there.
//
// It reads in batches, never per issue: the issues, then their trees' gate rows. An issue the
// record does not hold is answered nothing, and its fact finds no claim to wake.
func (e *Engine) HeldMergers(ctx context.Context, tx pgx.Tx, issues []string) (map[string]record.DesignGate, error) {
	return heldMergers(ctx, tx, e.store, e.cfg.Project, e.cfg.Steps, issues)
}

// heldMergers is HeldMergers over store, for issues of project, under steps, the steps the
// deployment gates. It is the one batched read of the hold, which the wake tick's filter and the
// state's source (HoldsNow) both ask.
func heldMergers(ctx context.Context, tx pgx.Tx, store record.Store, project string, steps []record.DesignStep, issues []string) (map[string]record.DesignGate, error) {
	held := make(map[string]record.DesignGate)
	if len(issues) == 0 {
		return held, nil
	}
	read, err := store.IssuesOf(ctx, tx, project, issues)
	if err != nil {
		return nil, err
	}
	trees := make([]string, 0, len(read))
	for _, issue := range read {
		trees = append(trees, issue.Tree)
	}
	gates, err := store.Gates(ctx, tx, trees)
	if err != nil {
		return nil, err
	}
	for _, issue := range read {
		if gate, holds := HeldByDesignGate(issue, claim.RoleMerger, record.TreeGate{Steps: steps, Rows: gates[issue.Tree]}); holds {
			held[issue.Key] = gate
		}
	}
	return held, nil
}

// HoldsNow is, of claims, every merger claim the design gate holds now (HeldByDesignGate), with
// the closed gate's row that holds it, by the claim's token. It is what the state shows on each
// claim (internal/projection's Project, which may not read this package and is handed it, as it
// is handed StandingWaitsNow's waits), so a merger carries its hold on its view exactly while the
// outbox's fence holds its phase start. Only a merger can be held, so the claims of every other
// role read nothing, and a project with no merger claim costs no read at all.
//
// It reads in batches, never per claim: the mergers' issues, then their trees' gate rows
// (heldMergers). project is the Dispatch project, and steps the steps the deployment gates. A
// claim whose issue the record of project does not hold is held by nothing.
func HoldsNow(ctx context.Context, tx pgx.Tx, store record.Store, project string, steps []record.DesignStep, claims []supervise.Claim) (map[claim.Token]record.DesignGate, error) {
	var issues []string
	for _, current := range claims {
		if current.Role == claim.RoleMerger {
			issues = append(issues, current.Issue)
		}
	}
	held, err := heldMergers(ctx, tx, store, project, steps, issues)
	if err != nil || len(held) == 0 {
		return nil, err
	}
	holds := make(map[claim.Token]record.DesignGate, len(held))
	for _, current := range claims {
		if gate, found := held[current.Issue]; found && current.Role == claim.RoleMerger {
			holds[current.Token] = gate
		}
	}
	return holds, nil
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
// resumes a claim in process instead, heals a refused wake (wakeForDelivery). It answers the id of
// that start row, and 0 where it woke nothing.
//
// A wake that takes the pod and whose cause is news — a CI settlement, a message to the role or one
// to the session — ends the woken claim's wait in the same transaction, before its start row is
// queued (endWokenWait): R3's third end, so the start row's task is the claim's new work and is
// delivered. The backstop's wake ends none.
//
// The design gate's own wake (pods.WakeDesignGate, deliverGateMove) writes its start row with the
// mark that its task opens with the gate's own message (record.SuperviseRequest.GateMessage),
// which the claim's send builds as the gate stands then, so a wake written at the close and sent
// after the opening says that the gate opened.
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
func (e *Engine) wakeReleased(ctx context.Context, tx pgx.Tx, released supervise.Claim, kind, detail string, at time.Time) (int64, error) {
	issue, err := e.store.Issue(ctx, tx, released.Issue)
	if err != nil || issue == nil {
		return 0, err
	}
	root, err := e.store.Issue(ctx, tx, issue.Tree)
	if err != nil || root == nil {
		return 0, err
	}
	if !pods.InWorkflowNotLingering(issue.Standing(), root.Standing(), record.OutOfWorkflow) {
		return 0, nil
	}
	pause, stopping, err := e.cfg.Pods.PauseOfWake(ctx, tx, issue.Key, released.Token, 0)
	if err != nil {
		return 0, err
	}
	newest, err := e.cfg.Pods.ClosePodGoneResume(ctx, tx, issue.Key, released.Role)
	if err != nil {
		return 0, err
	}
	if newest > 0 {
		e.log.Warn("wake: a pod went away under an open resume of a released role; this wake closes that resume and takes its own",
			"issue", issue.Key, "role", released.Role, "newest_release", newest, "kind", kind, "detail", detail)
	}
	taken, err := e.cfg.Pods.TakePodWake(ctx, tx, issue.Key, released.Role, kind, at, pause)
	if err != nil || !taken {
		return 0, err
	}
	if err := e.endWokenWait(ctx, tx, *issue, *root, released.Token, released.Role, kind, detail); err != nil {
		return 0, err
	}
	stop := StoppedByRelease
	if stopping != 0 {
		stop = StoppedByPause
	}
	e.log.Info("wake: a released pod is coming back", "issue", issue.Key, "role", released.Role, "kind", kind, "detail", detail,
		"pause", pause, "stopped_by_pause", stopping)
	start := wakeRow(*issue, released.Role, kind, pause, WakeTask(kind, detail, stop))
	start.GateMessage = kind == pods.WakeDesignGate
	return e.enqueueID(ctx, tx, issue.Key, start)
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
