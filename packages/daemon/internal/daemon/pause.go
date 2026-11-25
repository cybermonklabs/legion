package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pause"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon's half of a project's pause (dispatch://INFRA-3759). The pause's records and its two
// transactions are the store's (internal/store/pause.go, and the stop records' operations in
// internal/store/pause_stops.go), its flag and its acts the pause service's (internal/pause), and
// what a machine does while the project is paused the supervisor's (internal/supervise/pause.go).
// What is the daemon's is what joins them:
//
//   - the stop transaction a machine's pause suspension ends in (projectPause.stopped,
//     supervise.Deps.PauseStopped, decision 6), and the let-out moment of a paced launch
//     (projectPause.wakeLetOut, Deps.WakeLetOut, decision 14);
//   - what runs after a pause or a resume commits (projectPause.Paused and projectPause.Resumed,
//     the service's Effects, decisions 3 and 22);
//   - the boot's read of the flag before any claim is restored, and its pass over the claims a
//     restart finds live while paused (supervision.start, decision 18 and R5);
//   - the outbox's `pause_stop` executor (outbox.pauseStop, decision 4).

// pauseStopWait is how long a `pause_stop` row waits before it asks its claim again while the
// claim holds the stop for its turn or its launch (ErrSuspendHeld), or a launch whose outcome is
// uncertain stands. The wait is bounded rather than the backoff's minute, so a turn of an hour
// asks six times and logs no warning a minute (decision 4); what ends it sooner is the stop
// transaction, which makes the row due at once when it records the claim stopped
// (store.ExpeditePauseStopRowsTx) and wakes its lane.
const pauseStopWait = 10 * time.Minute

// projectPause is the daemon's half of its project's pause: the service, which holds the flag and
// makes each act, and what the service hands the daemon to do (pause.Effects). It is built only
// with the workflow (run): a daemon with no workflow has no outbox to run a pause's stops, so it
// neither pauses nor reads a pause, and its machines are handed no flag (nil Deps.Paused, never
// paused).
type projectPause struct {
	service *pause.Service
	store   *store.Store
	// records is the project's records (projectRecords), built with no wake: the stop transaction
	// wakes the lane of the start row it writes itself, in the hook of its commit (stopCommitted).
	records projectRecords
	pods    *store.PodRecords
	// project is the project's Dispatch key ("LEGION"), the spelling of project_pauses; token is
	// its claim token ("legion", claim.ProjectToken), the spelling of the claim-keyed tables.
	project string
	token   string
	// backstop is the idle release's backstop (`backstop_minutes`), which the stop of an open
	// phase's worker owed no launch arms (decision 6); zero where the deployment releases no pod,
	// and then the stop arms none, as a release then arms none.
	backstop time.Duration
	now      func() time.Time
	log      *slog.Logger
	// workflow is the runtime this half belongs to, whose outbox lanes and admission pass the
	// resume wakes and applies. supervisor is set by openSupervision before any machine exists:
	// the catch-up reads its machines and the resume posts to them. The API, the only caller of
	// the service's acts and so of the Effects, is served after both are set.
	workflow   *workflowRuntime
	supervisor *supervisor
}

// newProjectPause builds the daemon's pause over st for cfg's project. Its flag reads no pause
// until the boot loads it (supervision.start).
func newProjectPause(cfg config.Config, st *store.Store, token string, w *workflowRuntime, clock supervise.Clock, log *slog.Logger) (*projectPause, error) {
	pods, err := st.PodRecordsOf(cfg.Project)
	if err != nil {
		return nil, err
	}
	p := &projectPause{store: st, records: projectRecords{Store: record.NewStore(), project: cfg.Project}, pods: pods, project: cfg.Project,
		token: token, backstop: releaseBackstop(cfg), now: clock.Now, log: log, workflow: w}
	// The service reads the rows the pause holds and whether a controller is registered, and no
	// issue record, so it reads the store as it is (pause.Options.Records).
	if p.service, err = pause.New(pause.Options{Store: st, Records: record.NewStore(), Project: cfg.Project, Config: cfg,
		Effects: p, Log: log, Now: clock.Now}); err != nil {
		return nil, err
	}
	return p, nil
}

// releaseBackstop is the idle release's backstop where the deployment releases pods
// (idleRelease), and zero where it releases none: what the stop of an open phase's worker arms
// (projectPause.backstop) and what a release arms (outbox.backstop, set by attachIdleRelease).
func releaseBackstop(cfg config.Config) time.Duration {
	release, ok := idleRelease(cfg)
	if !ok {
		return 0
	}
	return release.Backstop()
}

// stopped is the stop transaction (supervise.Deps.PauseStopped, decision 6): the claim write a
// pause's suspension ends in, with the pause's records, in one store transaction, whose context
// carries the machine's claim-write deadline. In order:
//
//   - the issue's lock (store.LockIssueForPause), so two stops of one pod's roles, the resume's
//     end of the pod's hold and a role's ready run one after another; then the row of the
//     suspension's pause for share (store.LockPauseTx), open or ended, so a resume of it waits;
//     and where that pause has ended, the project's open pause for share too
//     (store.LockOpenPauseTx), taken before anything is written so a later pause's resume waits
//     for this transaction rather than meeting it halfway;
//   - the claim, and its pending delivery where it holds one, as the plain write writes them
//     (store.PutClaimTx, store.PutClaimAndDeliveryTx);
//   - the stop's records, through the one writer a paused cancel of a release records its stops
//     by too (store.RecordPauseStopTx): the stop, and on its first record the pod's hold and the
//     backstop this transaction decides; then the claim's `pause_stop` rows made due at once
//     (store.ExpeditePauseStopRowsTx), so a stop that waited for this turn's end ends now. Once
//     the transaction commits, the lifecycle lane of each row it made due is woken, under the
//     row's own key (pause.StopKey).
//
// A claim no longer suspended — a later write of a claim whose stop is still owed (the machine's
// putStops) — stops there: its stop is recorded, and a claim that runs now needs no pod hold, no
// owed launch and no backstop. A suspended claim then gets, on the first record of its stop:
// the pod's hold — always while its pause is still open, whatever the pod's other roles do, since
// an update round past its bound or an idle rule would otherwise take a pod that holds the stopped
// role's unpushed work; and, for a stop that ends after its pause ended, only where no role of the
// pod is live then (store.IssueHasLiveRoleTx, decision 6) — and its role's open resume closed
// (store.CloseRoleOpenResumeTx); and either the launch it is owed — a held wake of the pause
// (store.PodRecords.TakePodWake with pods.WakePause, which cancels the role's own backstop)
// with its start row (workflow.PauseWakeRow) — or, owed none as the worker of the issue's open
// phase serving its generation, its backstop. A start owed once the pause has ended, with no
// pause open after it, is logged as a released held row (decision 23), and the outbox runs it at
// once through the pacing; a later open pause holds it, and that pause's resume logs it.
//
// A stop that commits after its pause has ended, with no later pause open, owes the role a launch
// whatever the stop met (afterResume): the machine decided it while the resume's transaction was
// in flight, with the flag still set, and this transaction waited on the pause's row until the
// resume committed. The role was live when the resume ended the pause, so the resume's promise
// (R4 of dispatch://INFRA-3759) is that it runs again. Such a stop is recorded owing its launch.
// A stop under a later open pause keeps the launch it was owed: that pause holds the role.
//
// A claim of an issue the daemon keeps no record of — an operator's own spawn, of the project's
// issue or of another project's — gets no start row, since the outbox's start reads the issue's
// record, and no backstop, since no workflow serves it. A record that another project's daemon
// keeps of the issue on the same database is no record of this daemon's, and the project's records
// read it as none (projectRecords, which says why). Read as this daemon's, its start row would
// also leave the claim suspended: the resume launches by rule only a claim of an issue with no
// record of its project (store.Store.OwedLaunchesWithNoIssueRecord, the same rule in SQL). The
// launch it is owed is the resume's instead: one owed while its pause stood is launched by that
// pause's resume (Resumed, resumeOwedSpawns), and one owed once its pause has ended, with no pause
// open after it, is launched by rule as soon as this transaction commits (made.resume), through
// the wake pacing as every resume of the rule is. A later open pause's resume launches one owed
// under it.
//
// It is safe to make again: the machine owes a stop whose commit's answer was lost and makes it
// again with its next claim write. store.RecordPauseStopTx answers false for a stop already
// recorded, and then nothing of the first run is written twice: no second hold, no second
// backstop, and the role's open resume — the first run's held wake — is left open, so the held
// wake taken again conflicts with it (one open resume per role) and queues no second start. A
// second resume by rule of a claim the first already launched finds it running and does nothing.
// What the first run's commit owes once it lands — the lanes of the rows it queued or made due
// woken, the released row's line and the owed spawn's resume (stopCommitted) — is that commit's
// hook, and so is the wake of the role-holder publication the claim's write queued, which the
// store's claim write registers itself (store.PutClaimTx): a stop whose commit's answer was lost
// and that the store says committed still does each once, and the run made again, which writes
// none of those rows and changes no holder, does none of them a second time. It writes no log line
// of the stop itself: the machine writes supervise.PauseRoleStoppedMessage once it lands.
func (p *projectPause) stopped(ctx context.Context, c supervise.Claim, stop supervise.PauseStop) error {
	at := p.now().UTC()
	if err := store.Transact(ctx, p.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		made, err := p.stopTx(ctx, tx, c, stop, at)
		if err != nil {
			return err
		}
		store.OnCommit(ctx, func() { p.stopCommitted(c, stop, made) })
		return nil
	}); err != nil {
		return fmt.Errorf("write %s stopped for pause %d: %w", c.Token, stop.Pause, err)
	}
	return nil
}

// stopCommitted is what a committed stop transaction owes (pauseStopMade): each is a wake, a line
// stating what committed, or a resume the machine paces and the pause's gate holds while a later
// pause stands, so it is right however late it runs.
func (p *projectPause) stopCommitted(c supervise.Claim, stop supervise.PauseStop, made pauseStopMade) {
	if made.started {
		p.workflow.wakeOutbox(c.Issue, record.OutboxKindSupervise)
	}
	for _, key := range made.expedited {
		p.workflow.wakeOutbox(key, record.OutboxKindSupervise)
	}
	if made.released != 0 {
		p.log.Info(pause.HeldRowReleasedMessage, "pause", stop.Pause, "row", made.released, "kind", string(record.OutboxKindSupervise),
			"issue", c.Issue, "claim", string(c.Token))
	}
	if made.resume {
		p.resumeOwedSpawn(c.Token, stop.Pause)
	}
}

// pauseStopMade is what one stop transaction wrote that its caller acts on once it commits: a
// start row queued (started), the start row's id where it is a released held row (released), the
// keys of the claim's `pause_stop` rows it made due (expedited), whose lanes are woken, and whether
// the claim is an operator's spawn owed a launch now that its pause has ended, which no start row
// can carry (resume).
type pauseStopMade struct {
	started   bool
	released  int64
	expedited []string
	resume    bool
}

// stopTx is stopped's transaction.
func (p *projectPause) stopTx(ctx context.Context, tx pgx.Tx, c supervise.Claim, stop supervise.PauseStop, at time.Time) (pauseStopMade, error) {
	var made pauseStopMade
	if err := store.LockIssueForPause(ctx, tx, c.Issue); err != nil {
		return made, err
	}
	held, err := store.LockPauseTx(ctx, tx, stop.Pause)
	if err != nil {
		return made, err
	}
	later := false
	if held.Ended() {
		if _, later, err = store.LockOpenPauseTx(ctx, tx, held.Project); err != nil {
			return made, err
		}
	}
	if c.Pending != nil {
		err = p.store.PutClaimAndDeliveryTx(ctx, tx, supervise.StoredClaim(c), *c.Pending)
	} else {
		err = p.store.PutClaimTx(ctx, tx, c)
	}
	if err != nil {
		return made, err
	}
	afterResume := c.Released() && held.Ended() && !later
	owed := stop.LaunchOwed || afterResume
	rec := store.PauseStopRecord{Pause: held, Stop: pauseStopClaimOf(c), At: at, LaunchOwed: owed}
	var issue *record.Issue
	if c.State == supervise.StateSuspended {
		rec.HoldPod = !held.Ended()
		if !rec.HoldPod {
			live, err := store.IssueHasLiveRoleTx(ctx, tx, p.token, c.Issue)
			if err != nil {
				return made, err
			}
			rec.HoldPod = !live
		}
		if issue, err = p.records.Issue(ctx, tx, c.Issue); err != nil {
			return made, err
		}
		if issue != nil && p.backstop > 0 && c.Role == workflow.OpenPhaseRole(issue.Phase) && c.ServingGeneration == issue.Generation {
			rec.Backstop = &store.PodWakeBackstop{Project: p.token, Issue: c.Issue, Role: c.Role, DueAt: at.Add(p.backstop)}
		}
	}
	first, err := store.RecordPauseStopTx(ctx, tx, rec)
	if err != nil {
		return made, err
	}
	if made.expedited, err = store.ExpeditePauseStopRowsTx(ctx, tx, p.project, c.Issue, c.Role, at); err != nil {
		return made, err
	}
	if c.State != supervise.StateSuspended {
		return made, nil
	}
	if first {
		if _, err := store.CloseRoleOpenResumeTx(ctx, tx, p.token, c.Issue, c.Role, at); err != nil {
			return made, err
		}
	}
	if issue == nil {
		made.resume = afterResume
		return made, nil
	}
	if !owed {
		return made, nil
	}
	taken, err := p.pods.TakePodWake(ctx, tx, c.Issue, c.Role, pods.WakePause, at, held.ID)
	if err != nil || !taken {
		return made, err
	}
	row, err := record.NewOutboxRow(c.Issue, workflow.PauseWakeRow(*issue, c.Role, held.ID), at)
	if err != nil {
		return made, err
	}
	id, err := p.records.EnqueueID(ctx, tx, row)
	if err != nil {
		return made, err
	}
	made.started = true
	if held.Ended() && !later {
		made.released = id
	}
	return made, nil
}

// wakeLetOut is supervise.Deps.WakeLetOut (decision 14): the moment the wake pacing lets a launch
// of the claim token out becomes the stuck clock of its role's open resume
// (store.Store.SetResumeClock, which never moves a clock back), so a launch that waited its turn
// in a long queue is not warned stuck for the wait. The machine calls it inside its decision, so
// the write runs on a goroutine of its own, as podResume's stamp does, under a hold mark of its
// own; a failed write is logged and the clock keeps the moment it had.
func (p *projectPause) wakeLetOut(token claim.Token, issue string, role claim.Role, at time.Time) {
	if issue == "" || role == claim.RoleController {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(store.ForConcurrentWorker(context.Background()), readyReleaseTimeout)
		defer cancel()
		if _, err := p.store.SetResumeClock(ctx, p.token, issue, role, at); err != nil {
			p.log.Warn("wake: could not start a resume's stuck clock at its launch's let-out moment",
				"claim", token, "at", at, "error", err)
		}
	}()
}

// Paused is the catch-up once a pause has committed and the flag is set (pause.Effects, decision
// 3): every live role of the project's machines but the controller's that the pause's
// transaction did not name — a launch that passed the gate before the flag was set, a claim the
// store held suspended while its machine was already bringing it back — is asked to stop through
// the store, a stop row and a `pause_stop` row each (store.Store.AskPauseStops), so a restart
// keeps the ask. Then the lifecycle lane of every stop's row is woken, under the row's own key
// (pause.StopKey), so the stops run now rather than at the next sweep. The catch-up's line and the
// wakes of the rows it queued are the hook of its commit. A failed ask is logged: the gate still
// holds every launch, and the next boot's pass asks what this one could not.
//
// It reads each claim's published view (supervise.Machine.View), never Machine.Claim, which would
// hold the catch-up for a launch's whole start in the runtime. A launch that passed the gate before
// the flag was set and whose first write was published before this read is read launching and
// asked to stop. One whose first write was not published yet is read as the claim stood before it.
// A claim that stood suspended, failed or retired reads gone and is asked nothing: its launch
// checks the pause again after its first write, which comes after this read and so after the flag
// was set, and the pause's gate holds it there (supervise's launchTries). A claim that stood
// queued (a spawn) or live (a relaunch) reads not gone and is asked to stop, as any live role is.
func (p *projectPause) Paused(ctx context.Context, held store.Pause, asked []store.PauseStopClaim) {
	named := make(map[claim.Token]bool, len(asked))
	lanes := map[string]bool{}
	for _, stop := range asked {
		named[stop.Claim], lanes[pause.StopKey(held, stop.Issue)] = true, true
	}
	var missed []store.PauseStopClaim
	for _, m := range p.supervisor.supervised() {
		c := m.View().Claim
		if named[c.Token] || !pausable(c) {
			continue
		}
		missed = append(missed, pauseStopClaimOf(c))
	}
	if len(missed) > 0 {
		if err := store.Transact(ctx, p.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			caught, err := p.store.AskPauseStops(ctx, held.ID, missed, p.now().UTC(), pause.StopRow)
			if err != nil || len(caught) == 0 {
				return err
			}
			store.OnCommit(ctx, func() {
				p.log.Info("pause: the catch-up asked the stop of live roles the pause's transaction did not name",
					"pause", held.ID, "claims", stopTokens(caught))
				for _, stop := range caught {
					p.workflow.wakeOutbox(pause.StopKey(held, stop.Issue), record.OutboxKindSupervise)
				}
			})
			return nil
		}); err != nil {
			p.log.Error("pause: could not ask the stop of the live roles the pause's transaction did not name; the gate holds their launches, and the next boot asks them",
				"pause", held.ID, "claims", stopTokens(missed), "error", err)
		}
	}
	for key := range lanes {
		p.workflow.wakeOutbox(key, record.OutboxKindSupervise)
	}
}

// Resumed is what follows a resume's commit once the flag is cleared (pause.Effects, decision 22),
// in its order: every machine of the project is posted the end of the pause
// (supervise.ResumePause), which drops a hold of that pause, lets a launch its gate held into the
// wake pacing and sends the task the claim holds where it may now go; each operator's spawn the
// pause stopped owing a launch is resumed by rule (resumeOwedSpawns); the hold of each pod with a
// live role ends (endLiveHolds); every outbox lane of the project with a row is woken, so the rows
// the pause held run now (outbox.wakeUnfinished); and one admission pass is applied
// (applyResumePass). Every launch goes through the pacing: each held row launches its role when
// it runs, and an operator's spawn, whose launch no row can carry, is launched by the rule's
// resume, which the pacing paces as it paces every resume of the rule.
func (p *projectPause) Resumed(ctx context.Context, ended store.Pause) {
	for _, m := range p.supervisor.supervised() {
		token := m.View().Claim.Token
		p.supervisor.post(token, supervise.ResumePause{Claim: token, Pause: ended.ID})
	}
	p.resumeOwedSpawns(ctx, ended)
	p.endLiveHolds(ctx, ended)
	if r := p.workflow.outboxWake.Load(); r != nil {
		r.wakeUnfinished(ctx)
	}
	p.applyResumePass(ctx, ended)
}

// resumeOwedSpawns resumes by rule each operator's spawn whose stop ended `stopped` owing a launch
// while ended stood, and which that stop still holds suspended
// (store.Store.OwedLaunchesWithNoIssueRecord): a claim of an issue the daemon keeps no record of,
// so no start row could carry its launch (the outbox's start reads the issue's record) and no wake
// reaches it. Without this the claim would stay suspended, holding what the pause held for it,
// until an operator resumed it. A failed read is logged, and those claims stay suspended until an
// operator resumes them.
func (p *projectPause) resumeOwedSpawns(ctx context.Context, ended store.Pause) {
	owed, err := p.store.OwedLaunchesWithNoIssueRecord(ctx, ended)
	if err != nil {
		p.log.Error("pause: the resume could not read the operator's spawns the pause stopped owing a launch; each stays suspended until the operator resumes it",
			"pause", ended.ID, "error", err)
		return
	}
	for _, token := range owed {
		p.resumeOwedSpawn(token, ended.ID)
	}
}

// resumeOwedSpawn posts the rule's resume (supervise.RequestRuleResume) of token, an operator's
// spawn that pause stopped owing a launch, which no start row can carry: the resume of the
// pause's own (resumeOwedSpawns) or a stop that committed once that pause had ended (stopped).
// The machine paces it (supervise's exemptWake exempts no rule's resume), finds a claim that
// already runs nothing to do, and never resumes a claim the operator suspended, nor one the work
// hold stopped (supervise.SuspendedByHold): a rule's resume is a door the hold holds, answered
// supervise.ErrHeldByWorkHold and logged as the wait it is (supervisor.decisionFailed), and only
// the hold's opening starts such a claim. The post only queues the event (supervisor.post), so a
// stop transaction, which runs inside its machine's own decision, may make it.
func (p *projectPause) resumeOwedSpawn(token claim.Token, pause int64) {
	p.supervisor.post(token, supervise.RequestRuleResume{Claim: token,
		Reason: fmt.Sprintf("the project's resume: pause %d stopped it with a launch owed, and its issue has no record a start row could carry", pause)})
}

// applyResumePass applies the resume's one admission pass as a fact of its own
// (intake.ProjectResumed), keyed on the ended pause, through the workflow's handlers: admission
// promotes on it as on every fact that is not a Dispatch issue, and nothing else acts on it. It is
// not the controller's tick, which would also queue the controller a tick notice: the resume
// wakes nobody by itself. A failed pass is logged; the next fact or tick promotes the line.
func (p *projectPause) applyResumePass(ctx context.Context, ended store.Pause) {
	w := p.workflow
	eventID := fmt.Sprintf("project-resumed:%s:%d", p.project, ended.ID)
	if _, err := intake.ApplyFact(ctx, w.pool, "pause", eventID, intake.ProjectResumed{}, w.handlers...); err != nil {
		p.log.Error("pause: the resume's admission pass failed; the next fact or tick promotes the line",
			"pause", ended.ID, "error", err)
	}
}

// endLiveHolds ends the hold of each pod of the project in which a role is live (decision 13): that
// pod is already in the state a released pod's wake leaves it in. Each hold is ended in a short
// transaction of its own under its issue's lock, and the live role is read under that lock, so a
// stop transaction of a role of the same pod commits first and the read sees it. A hold whose pod
// has no live role stays until a role of it is ready again or the pod is deleted. A failed end is
// logged and the hold stays.
func (p *projectPause) endLiveHolds(ctx context.Context, ended store.Pause) {
	holds, err := p.store.OpenPausePodHolds(ctx, p.project)
	if err != nil {
		p.log.Error("pause: the resume could not read the pods a pause holds; their holds stay until a role of each is ready",
			"pause", ended.ID, "error", err)
		return
	}
	for _, hold := range holds {
		released, err := p.store.EndLivePausePodHold(ctx, p.token, hold.Issue, store.PausePodHoldResumed, p.now().UTC())
		switch {
		case err != nil:
			p.log.Error("pause: the resume could not end a pod's hold; it stays until a role of the pod is ready",
				"pause", ended.ID, "issue", hold.Issue, "held_by", hold.Pause, "error", err)
		case released:
			p.log.Info("pause: the resume ended a pod's hold; a role of the pod is live", "pause", ended.ID,
				"issue", hold.Issue, "held_by", hold.Pause)
		}
	}
}

// askBootStops is the boot's pass while the project is paused (decision 18 and R5): once every
// claim is restored, each live claim of the project but the controller's that the open pause has
// asked no stop of — a role an earlier daemon launched before the pause reached it, or whose stop
// it never queued — is asked to stop (store.Store.AskLivePauseStops). Its rows run once the
// outbox's boot sweep starts the lanes, and the relaunches and the stale-plugin pass of this boot
// meet the gate meanwhile, so the boot launches nothing but the controller.
//
// The stops the pause still waits on are asked again at once as well (store.Store.ExpeditePauseStops):
// a stop its claim held for a turn waits on its row's bounded retry, up to pauseStopWait out, and
// the hold itself was in the stopped daemon's memory. Left to that wait, the restarted machine
// holds no suspension, so a turn that ends inside it leaves the role idle and unstopped until the
// row comes due (decision 4: a restart holds the stop again).
//
// Each of the two writes logs what it did as the hook of its own commit.
func (p *projectPause) askBootStops(ctx context.Context, open store.Pause) error {
	at := p.now().UTC()
	if err := store.Transact(ctx, p.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		asked, err := p.store.AskLivePauseStops(ctx, open.ID, at, pause.StopRow)
		if err != nil || len(asked) == 0 {
			return err
		}
		store.OnCommit(ctx, func() {
			p.log.Info("pause: the boot asked the stop of the live roles the project's pause has not stopped",
				"pause", open.ID, "claims", stopTokens(asked))
		})
		return nil
	}); err != nil {
		return err
	}
	return store.Transact(ctx, p.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		again, err := p.store.ExpeditePauseStops(ctx, open.ID, at)
		if err != nil || again == 0 {
			return err
		}
		store.OnCommit(ctx, func() {
			p.log.Info("pause: the boot asks again, at once, the stops the project's pause still waits on", "pause", open.ID, "stops", again)
		})
		return nil
	})
}

// pausable is whether a pause stops c: a live claim — not gone (supervise.Claim.Gone: suspended,
// failed or retired) — that is not the controller's, which the pause never stops (store's
// liveClaimsOf reads the same rule).
func pausable(c supervise.Claim) bool {
	return !c.Gone() && !claim.IsController(c.Role, c.Issue, c.Tree)
}

// stopTokens is the claims of stops, for a log line.
func stopTokens(stops []store.PauseStopClaim) []string {
	tokens := make([]string, len(stops))
	for i, stop := range stops {
		tokens[i] = string(stop.Claim)
	}
	return tokens
}

// pauseStopClaimOf is c as a pause's stop names it (store.PauseStopClaim).
func pauseStopClaimOf(c supervise.Claim) store.PauseStopClaim {
	return store.PauseStopClaim{Claim: c.Token, Tree: c.Tree, Issue: c.Issue, Role: c.Role, Generation: c.Generation}
}

// pauseStop is the `pause_stop` executor (decision 4): one role's stop for one pause. A row whose
// pause has ended finishes without acting, since the resume ended what it asked. Otherwise the
// claim's machine is asked to suspend for the pause (RequestSuspend, By released, the pause's id,
// no deadline), and the row ends the pause's stop row with what the claim answers:
//
//   - suspended: the claim is stopped. A stop the pause's suspension made has ended its stop row
//     already, through the stop transaction (projectPause.stopped); a stop row still open is ended
//     `taken_over`, since something else stopped the claim first (pauseStopAnswered);
//   - the pause has ended (supervise.ErrPauseEnded): the resume committed after this row read its
//     pause open and before the machine decided the stop, so nothing was stopped, and the row
//     finishes without acting, as a row that reads its pause ended does;
//   - held for the agent's turn or its launch (supervise.ErrSuspendHeld): the row asks again after
//     pauseStopWait, or as soon as the stop transaction records the claim stopped and makes the
//     row due;
//   - refused as not registered: a queued claim runs nothing and has no launch under way, and the
//     pause holds the start row that would launch it, so its stop row ends `stopped` with no
//     launch owed; a launch whose outcome is uncertain asks again after the same wait, since the
//     boot's relaunch of it meets the pause's gate, whose stop ends the stop row through the stop
//     transaction;
//   - failed or retired, or no machine at all: the stop row ends `failed`.
//
// The claim is the one of the role on the row's issue, or, on a row keyed by the project, of the
// issue its payload names (record.SuperviseRequest.Issue, pause.StopKey): a claim of another
// project's issue. Each line the executor writes names the claim's issue.
func (r *outbox) pauseStop(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) error {
	if r.trees == nil {
		return errors.New("pause stop executor has no store")
	}
	issue := cmp.Or(payload.Issue, row.Issue)
	held, err := r.trees.PauseByID(ctx, payload.Pause)
	if err != nil {
		return err
	}
	if held.Ended() {
		r.pauseStopEnded(row, issue, payload.Role, held.ID)
		return nil
	}
	token, err := claim.NewToken(r.project, issue, payload.Role)
	if err != nil {
		return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	machine, found := r.supervisor.Machine(token)
	if !found {
		return r.endPauseStop(ctx, row, issue, held, token, store.PauseStopFailed, "the daemon supervises no claim of the role")
	}
	// The read before the stop is claimToStop's. The read after it is the published view
	// (supervise.Machine.View), which never waits: what the stop's own decision left.
	if _, stoppable := claimToStop(machine); !stoppable {
		return r.endPauseStop(ctx, row, issue, held, token, store.PauseStopFailed, "the claim has failed or retired")
	}
	err = machine.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByReleased, Pause: held.ID, Reason: payload.Reason})
	var refused *supervise.RefusedError
	switch {
	case err == nil:
		return r.pauseStopAnswered(ctx, row, held, machine.View().Claim)
	case errors.Is(err, supervise.ErrPauseEnded):
		r.pauseStopEnded(row, issue, payload.Role, held.ID)
		return nil
	case errors.Is(err, supervise.ErrSuspendHeld):
		return wait.Until(r.now().UTC().Add(pauseStopWait),
			fmt.Sprintf("pause %d's stop of %s is held for the claim's turn or its launch", held.ID, token))
	case errors.As(err, &refused) && refused.State == supervise.StateQueued:
		return r.endPauseStop(ctx, row, issue, held, token, store.PauseStopStopped,
			"the claim is queued: no process runs, no launch is under way, and the pause holds the start that would launch it")
	case errors.As(err, &refused) && refused.State == supervise.StateLaunchUncertain:
		return wait.Until(r.now().UTC().Add(pauseStopWait),
			fmt.Sprintf("pause %d's stop of %s waits for the relaunch of its uncertain launch, which the pause's gate stops", held.ID, token))
	default:
		return fmt.Errorf("stop claim %s for pause %d: %w", token, held.ID, err)
	}
}

// pauseStopEnded logs a `pause_stop` row that finishes without acting because its pause has ended:
// read ended by the row, or found ended by the machine that decided the stop. issue is the
// claim's.
func (r *outbox) pauseStopEnded(row record.OutboxRow, issue string, role claim.Role, pause int64) {
	r.log.Info("outbox pause stop finished without acting: its pause has ended", "row", row.ID, "issue", issue,
		"role", role, "pause", pause)
}

// pauseStopAnswered ends the pause's stop row of a claim that answered its stop with nil, by what
// the claim holds now. A suspended claim's stop row is ended `taken_over` where it is still open:
// one this pause's own suspension stopped was ended by its stop transaction, so what is left open
// was stopped first by a park, a close, the operator, an earlier pause's late stop or an idle
// release — or by this pause's suspension whose write the machine still owes, whose stop
// transaction then records the stop in a row of its own (store.RecordPauseStopTx), with its
// hold, its owed launch or its backstop, when it lands.
func (r *outbox) pauseStopAnswered(ctx context.Context, row record.OutboxRow, held store.Pause, c supervise.Claim) error {
	switch {
	case supervise.FailedOrRetired(c.State):
		return r.endPauseStop(ctx, row, c.Issue, held, c.Token, store.PauseStopFailed, "the claim has failed or retired")
	case c.State != supervise.StateSuspended:
		return wait.Until(r.now().UTC().Add(pauseStopWait),
			fmt.Sprintf("pause %d's stop of %s was answered while the claim is %s", held.ID, c.Token, c.State))
	default:
		return r.endPauseStop(ctx, row, c.Issue, held, c.Token, store.PauseStopTakenOver,
			fmt.Sprintf("a suspension by %s stopped the claim first", c.SuspendedBy))
	}
}

// endPauseStop ends held's open stop of token, a claim of issue, as end, logging why where one was
// open, as the hook of the end's commit: none is open once the stop transaction recorded the stop,
// or once a resume cancelled it.
func (r *outbox) endPauseStop(ctx context.Context, row record.OutboxRow, issue string, held store.Pause, token claim.Token, end store.PauseStopEnd, why string) error {
	return store.Transact(ctx, r.trees.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		ended, err := r.trees.EndPauseStop(ctx, held.ID, token, end, r.now().UTC())
		if err != nil || !ended {
			return err
		}
		store.OnCommit(ctx, func() {
			r.log.Info("outbox pause stop ended its stop row", "row", row.ID, "issue", issue, "claim", string(token),
				"pause", held.ID, "end", string(end), "why", why)
		})
		return nil
	})
}
