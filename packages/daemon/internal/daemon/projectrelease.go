package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// projectReleaseReread is how often a pod that waits for its asked roles has its claims read
// again, and how often the store is read for a claim every machine already holds settled
// (awaitSettled): a turn's end raises no event the release can subscribe to, so the release
// looks, and it looks at once when an answer arrives.
const projectReleaseReread = 5 * time.Second

// projectReleasePoll is how often a stop the release asked for is read for the claim being stored
// suspended. The stop itself is the machine's and takes the worker's stop grace; this is only how
// soon the release notices it landed.
const projectReleasePoll = 250 * time.Millisecond

// projectReleaseRetry is how long the release waits before it tries again a write that failed —
// its ask to a pod's roles (ask) and its finish (writeFinish) — and projectReleaseRetryMax the
// longest such wait: the wait doubles after each failed try, up to that. projectReleaseRetry is
// also the pause between two tries of its read of the claims at its end (writeClaims) and of its
// arm of a taken-over idle release's backstop (armTakenOver).
const (
	projectReleaseRetry    = time.Second
	projectReleaseRetryMax = 10 * time.Second
)

// projectReleaseTries is how many times in all the release tries a list of the project's
// Sandboxes, its read of the claims at its end and its arm of a taken-over idle release's backstop
// before it gives up at the last try's error (tryBounded), and projectReleaseListPause the pause
// between two lists (list). A release that stops short at a list leaves the mark holding every
// start of the project until the next boot with the pods it did not take standing: at the first
// list every pod of the project, and at the list once every issue pod ended the controller's claim
// and pod and the repository caches. So one timeout or one 5xx of the API server is not enough to
// stop it.
const (
	projectReleaseTries     = 4
	projectReleaseListPause = 10 * time.Second
)

// projectReleaseStartDivisor and projectReleaseStartFloor are projectReleaseStartBound's share of
// the bound and its floor; the floor leaves the start of a short bound time for the store to run
// its write again after its first wait of a second (store's transactUntilCommitted).
const (
	projectReleaseStartDivisor = 10
	projectReleaseStartFloor   = 2 * time.Second
)

// projectReleaseStartBound is how long the start of a release of bound tries to record it before
// it clears the mark and answers the store's error (Start): a tenth of bound, and at least
// projectReleaseStartFloor.
func projectReleaseStartBound(bound time.Duration) time.Duration {
	return max(bound/projectReleaseStartDivisor, projectReleaseStartFloor)
}

// releaseUnknownKind is why a Sandbox whose labels name none of the kinds a release takes is
// left, by the release and by its dry run alike.
const releaseUnknownKind = "its labels name no kind of Sandbox the release deletes"

// releaseAskUnwritten is why a pod is left when the release could not write its ask to the pod's
// running roles by the bound, followed by the last error (ask, takePod).
const releaseAskUnwritten = "the ask to its running roles could not be written"

// releaseAskAtBound is why a pod is left when the release's ask to the pod's running roles was
// written only at or after the bound, so its roles had no time to answer (ask, takePod).
const releaseAskAtBound = "the ask to its running roles was written only at the bound, which left them no time to answer"

// releaseAskUnreached is why a pod is left when the release's ask was written to the pod's
// running roles before the bound and reached no session of any of them by it: a role that said
// ready holds its Envoy topic only from its session's first turn or the first-turn bound
// (supervise.SaidReady), so nobody was asked to push the pod's work (wait, takePod).
const releaseAskUnreached = "the ask reached no session of its running roles by the bound"

// releaseNoRoleAtLastRead is the reason a pod's line gives when an earlier read reached a role
// process of it and the last read before its delete found none (writeDeleted).
const releaseNoRoleAtLastRead = "no role process ran at the last read"

// releaseLauncherRead is the reason a pod's line gives when no read through a role's shim reached
// a role process of it and the release read it through its launcher instead (launcherRead): the
// line's count is then the launcher's, the commits under a working copy or a local bookmark
// (workspace.WorkRevset), which is narrower than a shim's read of the issue's unpushed work.
const releaseLauncherRead = "no role process ran in it, so its launcher read its workspace (workspace-work), which counts only the commits under a working copy or a local bookmark, not the issue's unpushed work as a role's shim reads it"

// releaseLauncherUnread opens the reason a deleted_unread pod's line gives: no role process ran in
// it, and its launcher did not answer the read or refused it, whose error follows (launcherRead).
const releaseLauncherUnread = "no role process ran in it, and its launcher did not read its workspace (workspace-work): "

// releaseNoMachine is the refusal written for a claim the release asks a suspension of that the
// daemon supervises no machine for (suspend, hold).
const releaseNoMachine = "the daemon supervises no machine for it"

// reasonSeparator joins the reasons of a Sandbox's line, which is what the route promises of
// them (api.ReleasedSandbox): leave and takeOthers join what they build.
const reasonSeparator = "; "

// projectReleaseErrorInFull is the message of the lines that carry, in parts, the whole of a far
// side's error text the release cut (keepWhole), where the result and every other line about it
// keep only its start.
const projectReleaseErrorInFull = "project release: an error in full; the result and the lines about it keep its start"

// projectReleaseStore is what the project release reads and writes of the daemon's store: whether
// the project's pause refuses it, its record, the stop set a dry run reads as the release's own
// transaction reads it, each claim it stops or holds as the store holds it, which is what decides
// that the claim is stored suspended (awaitSettled), every claim as the store holds it, which is
// where each claim's line of the result is read from at the end, the activity report of a
// claim's launch, which is where the background hold of an issue pod is read from (background),
// and the session names each claim of an issue holds and whether the record holds the issue's pull
// request merged, which the read of the issue's workspace carries to the shim (unpushedRead); the
// end of a deleted pod's hold streaks, which the idle pass the mark holds would otherwise end only
// after the next boot (stopAndDelete); and the standing records of the replacements of the
// project's issue pods, which the release settles before it deletes a pod, forgetting each one
// whose act was not made (settleReplacements).
type projectReleaseStore interface {
	PauseRefusesProjectRelease(ctx context.Context, project string) error
	StartProjectRelease(ctx context.Context, project string, now func() time.Time, bound time.Duration) (store.ProjectRelease, []store.TakenOverIdleRelease, []store.ProjectReleaseClaim, error)
	RecordProjectRelease(ctx context.Context, id int64, result json.RawMessage) error
	FinishProjectRelease(ctx context.Context, id int64, result json.RawMessage, at time.Time) error
	ReadProjectRelease(ctx context.Context, project string) (store.ProjectRelease, bool, error)
	ProjectReleaseStopSet(ctx context.Context, project string) ([]store.ProjectReleaseClaim, error)
	ClaimByToken(ctx context.Context, token claim.Token) (supervise.Claim, bool, error)
	Claims(ctx context.Context) ([]supervise.Claim, error)
	ClaimActivity(ctx context.Context, token claim.Token, generation uint64) (supervise.ActivityReport, bool, error)
	ClaimHolds(ctx context.Context, project string, token claim.Token) ([]supervise.Hold, error)
	PullRequestMerged(ctx context.Context, issue string) (bool, error)
	EndPodHoldStreaks(ctx context.Context, project, issue string) error
	PodReplacements(ctx context.Context, project string) ([]store.PodReplacement, error)
	ForgetPodReplacement(ctx context.Context, project, issue, operation string) error
}

// releasingStore is the daemon's store as a project release reads it: the store itself, and whether
// the record holds an issue's pull request merged, read as the per-issue read reads the pull
// request's record (issueRecord). An issue the record does not hold, and one with no pull request
// recorded, has none merged.
type releasingStore struct{ *store.Store }

func (s releasingStore) PullRequestMerged(ctx context.Context, issue string) (bool, error) {
	_, pr, err := issueRecord(ctx, s.Pool(), record.NewStore(), issue)
	return pr != nil && pr.State == record.PullRequestMerged, err
}

// projectReleaser is the daemon's project release (dispatch://INFRA-3583 R6): the operator's
// `legion project release` starts it through the API (api.ProjectRelease), and it runs as one
// goroutine of this process, since R7 ends a release at the next boot and never resumes one, so
// nothing of its acts needs a durable retry. Its record is durable; its acts are not. One release
// runs per process: a second start, a dry run's included, answers the first, whether or not a pause
// holds the project. A pause that holds the project refuses every other start, a dry run's
// included, before anything of the release acts (R13, store.ProjectPausedError).
//
// The release does its acts in this order. It sets the daemon-wide mark, from when on the daemon
// starts nothing of the project (R7), waits for a pass of the supervision's sweep that read the
// mark clear to end the acts the mark holds (projectRelease.awaitSweep), so that no pod update
// round writes the record of a replacement after the settle below has read the records, and records
// itself (Start). Its goroutine arms the backstop of each idle release it took over that had
// stopped claims (armTakenOver). It lists every Sandbox of the project, a few times over some tens
// of seconds while that list fails, and classifies each issue pod from its claims: left, when a
// claim of it is suspended by an operator; read through its launcher, when no role process runs
// there; read through a role otherwise. Each pod is then taken on a goroutine of its own: a pod
// read through a role has its workspace read through one running role's shim, and each running role
// of a pod whose workspace holds unpushed work, whose shim refused the read, that has a role in a
// turn, or that has a role whose activity report names running background work or that has sent no
// report since its last turn started (background) is asked; an ask that cannot be written is
// written again until the bound, and a pod whose ask could still not be written then, or was
// written only at the bound, is left. Each asked pod waits until each asked role answered and no
// role of it is in a turn or holds by its report, or the bound; a role counts as asked only once
// the ask reached a session of it, and a pod whose ask reached none of its roles by the bound is
// left too. Then its last read, its stop and its delete (stopAndDelete). A pod no read through a
// role's shim reached is read through its launcher before its delete (launcherRead), and the delete
// settles the standing records of the replacements of the pod first (settleReplacements). Once
// every issue pod ended, the project's Sandboxes are listed again from the API server, tried as the
// first list is, and each one the first list did not hold is taken as the first list's are
// (takeUnlisted). Then the controller's claim and pod, the repository caches and any probe go. Each
// stop is waited for until the stop timeout after the later of the stop and the bound, since a
// claim in a turn is stopped at the turn's end or at the bound (stopDeadline), and a claim counts
// as stopped only once the store holds it so (awaitSettled). A pod it leaves, the controller's
// among them, has its claims held for an operator (hold). The result grows on the record as each
// Sandbox ends, and the release finishes once each claim it held for an operator is stored
// suspended or its wait ran out; a finish whose write fails is written again until it lands
// (writeFinish). Every goroutine it starts takes a store mark of its own
// (store.ForConcurrentWorker), since its reads, stops and holds run side by side and the pool
// refuses a second connection under one mark.
type projectReleaser struct {
	// ctx is the release goroutine's lifetime, supervision's: a daemon that stops ends the release
	// where it stands, and the next boot ends its record and starts the claims it had stopped.
	ctx      context.Context
	project  string
	mark     *projectRelease
	store    projectReleaseStore
	machines *supervisor
	// pods is the runtime's part a release needs, nil on a runtime that runs no pod (tmux),
	// which refuses the release (api.ErrProjectReleaseRunsNoPod).
	pods runtime.ProjectPods
	// enqueue writes the release's asks as outbox rows (supervision.enqueue), and answers whether
	// it wrote them: it writes none for an issue of another project, whose rows only that project's
	// daemon runs. Nil in a deployment with no outbox, where no role is asked.
	enqueue func(ctx context.Context, issue string, payloads ...record.OutboxPayload) (bool, error)
	// armBackstops arms the backstop an idle release this release took over would have armed at
	// its teardown (outbox.armBackstops); nil with no workflow, which arms none.
	armBackstops func(ctx context.Context, issue string) error
	// defaultBound is pod_update_grace_minutes, the bound of a release that names none, and
	// stopTimeout is worker_stop_timeout_seconds, which bounds the wait for a stop to be stored
	// after the later of the stop and the bound (stopDeadline).
	defaultBound time.Duration
	stopTimeout  time.Duration
	reread, poll time.Duration
	// retry is the first wait before a write that failed is tried again (ask, writeFinish) and the
	// pause between two tries of the claims' read and of a backstop's arm (writeClaims,
	// armTakenOver), and listPause the pause between two lists of the project (list): the daemon
	// runs on projectReleaseRetry and projectReleaseListPause (newProjectReleaser), and a test
	// shortens them. Neither may be zero, which would make a try follow a failed one at once.
	retry, listPause time.Duration
	now              func() time.Time
	log              *slog.Logger

	mu  sync.Mutex
	run *releaseRun
}

// newProjectReleaser is the release of the daemon serve builds: over supervision's machines and
// runtime, the store, and the workflow's outbox where the daemon runs one. A runtime that runs no
// pod (tmux) gives it no pods, and every start it is asked is refused (api.ErrProjectReleaseRunsNoPod).
func newProjectReleaser(s *supervision, st *store.Store, w *workflowRuntime) *projectReleaser {
	r := &projectReleaser{
		ctx: s.supervisor.ctx, project: s.plan.project, mark: s.plan.release, store: releasingStore{st}, machines: s.supervisor,
		// The workflow set supervision's enqueue as it attached, which the boot does before it serves.
		enqueue:      s.enqueue,
		defaultBound: s.cfg.PodUpdateGrace, stopTimeout: s.cfg.WorkerStopTimeout,
		reread: projectReleaseReread, poll: projectReleasePoll, retry: projectReleaseRetry, listPause: projectReleaseListPause,
		now: time.Now, log: s.log,
	}
	if pods, ok := s.runtime.(runtime.ProjectPods); ok {
		r.pods = pods
	}
	if w != nil && w.outbox != nil {
		runner := w.outbox
		r.armBackstops = func(ctx context.Context, key string) error {
			issue, err := runner.issue(ctx, key)
			if err != nil {
				return err
			}
			return runner.armBackstops(ctx, issue)
		}
	}
	return r
}

// releaseRun is the release of this process, once its record committed.
type releaseRun struct {
	id    int64
	bound time.Time
	// stop is the stop set StartProjectRelease read in the transaction that recorded the release:
	// that set, and no later reading, is the set the release stops.
	stop []store.ProjectReleaseClaim

	mu     sync.Mutex
	result api.ProjectReleaseResult
	// asked is each claim the release asked, and where its answer is recorded.
	asked map[claim.Token]releaseAsk
	// held is each claim the release asked an operator's suspension of, in a pod it left (an
	// issue pod or the controller's), whose storage the release waits for before it finishes, and
	// heldRefused where the machines' refusals of those suspensions are written.
	held        []claim.Token
	heldRefused *refusals
	// detail is why a claim the release stopped with no pod of the project was not stored
	// suspended in time (stopDeadline), written on the claim's line at the end.
	detail map[claim.Token]string

	// recording serializes the writes of the result, so a later write is never overtaken by an
	// earlier snapshot.
	recording sync.Mutex
}

// releaseAsk is where one asked claim's answer goes: its pod's line, which names the claim's role
// (role) once, since an issue has one claim per role and a pod is asked once, and the pod's nudge,
// which wakes the pod's wait at once. A claim is in run.asked exactly while its role is on that
// line. reached is whether the outbox's publish of the ask reached a session of the claim's role
// before the bound (reach): only such a role, or one that answered, stays on the line at the bound
// (passBound).
type releaseAsk struct {
	line    int
	role    claim.Role
	nudge   chan struct{}
	reached bool
}

// podClass is how a release takes an issue pod (classifyReleasePod).
type podClass int

const (
	releaseReads podClass = iota
	// releaseByLauncher is a pod in which no role process runs, so no shim reads it: the release
	// reads it through its launcher before its delete (launcherRead), and a dry run reads it so at
	// once (readAll), and asks none of its roles.
	releaseByLauncher
	releaseLeaves
)

// releasePod is one issue pod the release takes: its line of the result, its Sandbox, the claims
// of the stop set in it, how it is taken, and the nudge an answer to its ask sends.
type releasePod struct {
	line    int
	sandbox runtime.ProjectSandbox
	stop    []store.ProjectReleaseClaim
	class   podClass
	why     string
	nudge   chan struct{}
}

// releaseRead is one read of a pod's workspace: whether it was made, what it found, and why it was
// not. A read through one running role's shim (read) is made where a role process was there to
// read it, and its refusal is then the shim's, or the daemon's where it could not ask the shim. A
// read through the pod's launcher (launcherRead, launcher set) is made of a pod no read through a
// role's shim reached; it counts the commits under a working copy or a local bookmark
// (workspace.WorkRevset), a narrower set than the shim counts (internal/shim's unpushedRevset), and
// its refusal is why it was not made, with read false.
type releaseRead struct {
	read     bool
	launcher bool
	work     runtime.UnpushedWork
	refusal  string
}

// Start is api.ProjectRelease's start: the release's record, or the record this process's release
// already holds, or a dry run's report.
func (r *projectReleaser) Start(ctx context.Context, bound time.Duration, dryRun bool) (api.ProjectReleaseResponse, bool, error) {
	if r.pods == nil {
		return api.ProjectReleaseResponse{}, false, api.ErrProjectReleaseRunsNoPod
	}
	if bound <= 0 {
		bound = r.defaultBound
	}
	r.mu.Lock()
	if r.run != nil {
		r.mu.Unlock()
		view, _, err := r.Read(ctx)
		return view, false, err
	}
	// A pause that holds the project refuses a new release and a dry run, before the mark, a list,
	// a read or a record (dispatch://INFRA-3583 R13): the project paused, or a pod a pause stopped
	// still held, which no role process runs in, so the release would read it only through its
	// launcher (launcherRead), whose count is narrower than the release's own, before it deleted
	// it. The release this process ran, running or finished, is answered before it, since that
	// answer starts nothing: no pause commits while that release runs
	// (store.ProjectReleasingError), so a pause that holds the project then was taken once it
	// finished, and a refusal would say that nothing was stopped or deleted of a release that did
	// both. The read is made under r.mu, as the record below is, so no release of this process
	// starts between it and the answer. StartProjectRelease asks it again in the release's own
	// transaction, which a pause cannot commit across, and refuses a pause that committed between
	// the two reads; the mark is then cleared below.
	if err := r.store.PauseRefusesProjectRelease(ctx, r.project); err != nil {
		r.mu.Unlock()
		return api.ProjectReleaseResponse{}, false, err
	}
	if dryRun {
		r.mu.Unlock()
		view, err := r.dryRun(ctx, bound)
		return view, false, err
	}
	defer r.mu.Unlock()
	// The mark is set before the record commits and before that transaction reads the stop set,
	// and every role start reads the mark once more after its claim's `launching` write, just
	// before the launcher starts the role (the Sandbox runtime's launch), so a launch either sees
	// the mark or is in the stop set. A mark already held is a release this process ran, which a
	// run would have recorded above.
	if !r.mark.mark() {
		return api.ProjectReleaseResponse{}, false, errors.New("the project release's mark is already held, with no release of this process recorded")
	}
	// A pass of the supervision's sweep that read the mark clear before it was set may still run
	// the acts the mark holds, the pod update sweep among them, whose round could write the record
	// of a replacement after the settle has read the records of its pod, and then end as a
	// replacement made over the pod the release deleted. So the release waits here for that pass to
	// end those acts, before it records, reads, asks or deletes anything; every pass after reads
	// the mark set and skips them (projectRelease.sweep). A request that ends first ends the wait:
	// nothing of the release ran, so the mark is cleared, as a record that fails clears it below,
	// and the start answers that end. A dry run sets no mark and waits for no pass.
	if err := r.mark.awaitSweep(ctx); err != nil {
		r.mark.unmark()
		return api.ProjectReleaseResponse{}, false, err
	}
	// The store runs the record's write again until one run commits or its context ends, and the
	// request's context ends only when the command stops waiting, long after the bound. A run whose
	// reply was lost keeps the start it wrote while the store asks whether it committed, and all
	// that time the mark holds every start of the project with no release running. So the start
	// records under a deadline of its own (projectReleaseStartBound), with at most
	// store.CommitSettleBound more for a commit already sent. An error clears the mark below
	// (unmark), so the operator's next run gets a whole bound; a run whose outcome is still unknown
	// at the deadline answers store.ErrCommitOutcomeUnknown, which unmark's doc follows.
	recording, cancel := context.WithTimeout(ctx, projectReleaseStartBound(bound))
	release, takenOver, stop, err := r.store.StartProjectRelease(recording, r.project, r.now, bound)
	cancel()
	if err != nil {
		r.mark.unmark()
		return api.ProjectReleaseResponse{}, false, err
	}
	run := &releaseRun{id: release.ID, bound: release.BoundAt, stop: stop, asked: map[claim.Token]releaseAsk{},
		heldRefused: newRefusals()}
	r.mark.recorded(releaseAskHook{id: release.ID, bound: run.bound, reached: func(token claim.Token) { run.reach(token, r.now()) }})
	r.run = run
	r.log.Info("project release: recorded; the daemon starts nothing of the project until its next boot",
		"release", release.ID, "bound", bound.String(), "boundAt", release.BoundAt.Format(time.RFC3339),
		"claims", len(stop), "takenOver", len(takenOver))
	go r.release(run, takenOver)
	view, err := api.ProjectReleaseResponseOf(release)
	return view, true, err
}

// armTakenOver arms, once the take-over has committed, the backstop each idle release the project
// release took over would have armed at its teardown, for the claims it had already stopped:
// those claims stay `released` and wait for their role's wake, which comes after the next boot. A
// release that had stopped no claim — the release of a pod no role can use never stops one — arms
// nothing. A backstop not armed leaves those claims to wait for a delivery alone, so an arm that
// fails is made again, retry later, up to projectReleaseTries tries in all (tryBounded), and
// one that fails each time is logged. It runs on the release's goroutine (release), so the pause
// between two tries holds no start's answer.
func (r *projectReleaser) armTakenOver(ctx context.Context, run *releaseRun, takenOver []store.TakenOverIdleRelease) {
	for _, idle := range takenOver {
		if len(idle.Suspended) == 0 {
			continue
		}
		if r.armBackstops == nil {
			r.log.Info("project release: took over an idle release that had stopped claims; no workflow arms its backstop",
				"release", run.id, "issue", idle.Issue, "roles", idle.Suspended)
			continue
		}
		err := r.tryBounded(ctx, r.retry, "project release: arm the backstop of an idle release it took over; arming it again",
			func() error { return r.armBackstops(ctx, idle.Issue) }, "release", run.id, "issue", idle.Issue)
		if err != nil {
			r.log.Error("project release: arm the backstop of an idle release it took over", "release", run.id, "issue", idle.Issue, "error", err)
		}
	}
}

// Read is api.ProjectRelease's read: the newest release of the project as its record holds it.
func (r *projectReleaser) Read(ctx context.Context) (api.ProjectReleaseResponse, bool, error) {
	release, found, err := r.store.ReadProjectRelease(ctx, r.project)
	if err != nil || !found {
		return api.ProjectReleaseResponse{}, found, err
	}
	view, err := api.ProjectReleaseResponseOf(release)
	return view, true, err
}

// current is this process's release while it runs, nil before and once it finished.
func (r *projectReleaser) current() *releaseRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil || r.mark.finished() {
		return nil
	}
	return r.run
}

// AskRound is api.ProjectRelease's round of the running release's asks: the mark's
// (projectRelease.askRound), so the route hands an answer to the release exactly while the outbox
// publishes the asks of that round without the claim-round read, and neither once it finished.
func (r *projectReleaser) AskRound() string {
	return r.mark.askRound()
}

// Answer is api.ProjectRelease's record of an asked role's answer. The claim is read from its
// machine's published view, which waits on no decision in flight, and held to the session and the
// generation the answer names, as the machine holds every agent request (fenceRequest). The answer
// is recorded on the release, never on the claim, and wakes its pod's wait at once. An answer that
// comes once its pod's bound has passed is accepted and not recorded: the line keeps the bound,
// which is what decided the pod. A role the bound took off its pod's line, since the ask reached no
// session of it by then and it had not answered (passBound), is one the release did not ask, and
// its answer is refused as any such claim's is.
func (r *projectReleaser) Answer(ctx context.Context, token claim.Token, session string, generation uint64, head string) error {
	run := r.current()
	if run == nil {
		return &supervise.RefusedError{Request: "ready_for_update", Reason: "no project release runs now"}
	}
	m, ok := r.machines.Machine(token)
	if !ok {
		return &supervise.RefusedError{Request: "ready_for_update", Reason: "the daemon supervises no such claim"}
	}
	held := m.View()
	if generation != held.Generation {
		return fmt.Errorf("ready_for_update for %s at generation %d, the claim is at %d: %w", token, generation, held.Generation, claim.StaleGeneration)
	}
	if held.Session != "" && session != held.Session {
		return fmt.Errorf("ready_for_update for %s as session %q, the claim resumes %q: %w", token, session, held.Session, claim.SameAgentRefusal)
	}
	run.mu.Lock()
	asked, ok := run.asked[token]
	entry := -1
	if ok {
		entry = slices.IndexFunc(run.result.Sandboxes[asked.line].Asked, func(ask api.ReleasedAsk) bool { return ask.Role == held.Role })
	}
	if entry < 0 {
		run.mu.Unlock()
		return &supervise.RefusedError{State: held.State, Request: "ready_for_update",
			Reason: "the project release " + projectReleaseRound(run.id) + " did not ask this claim; it holds no answer from it"}
	}
	ask := &run.result.Sandboxes[asked.line].Asked[entry]
	if ask.Bound {
		run.mu.Unlock()
		r.log.Info("project release: an asked role answered after its pod's bound; the release keeps the bound",
			"release", run.id, "claim", token, "head", head)
		return nil
	}
	if ask.AnsweredAt == nil {
		at := r.now().UTC()
		ask.AnsweredAt = &at
	}
	run.mu.Unlock()
	r.log.Info("project release: an asked role answered", "release", run.id, "claim", token, "head", head)
	r.record(ctx, run)
	nudge(asked.nudge)
	return nil
}

// release is the goroutine of one release, from the backstops of the idle releases it took over
// (armTakenOver) and its inventory to its finish.
func (r *projectReleaser) release(run *releaseRun, takenOver []store.TakenOverIdleRelease) {
	ctx := store.ForConcurrentWorker(r.ctx)
	r.armTakenOver(ctx, run, takenOver)
	sandboxes, err := r.list(ctx, run, r.pods.ProjectSandboxes, "project release: list the project's Sandboxes; trying the list once more")
	// The stop set's lines are written whatever the listing answered, so a release that stops
	// short at it still names each claim of the stop set, as its record holds it at the end.
	pods, others := r.inventory(run, sandboxes)
	if err != nil {
		r.fail(ctx, run, fmt.Sprintf("list the project's Sandboxes: %v", err))
		return
	}
	r.record(ctx, run)

	// Each pod is read and taken on a goroutine of its own, so its ask goes out as soon as its
	// own read is done and one pod's slow shim delays no other pod's ask.
	var wg sync.WaitGroup
	r.takePods(ctx, run, pods, &wg)
	// A claim of the stop set whose issue has no Sandbox of the project is starting where no pod
	// stands yet (a paced wake parked over no process): it is stopped as the others are, so the
	// boot starts it, and it has no pod to delete. One not stored suspended in time has that
	// written on its line.
	if podless := podlessStops(run.stop, pods); len(podless) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pending := r.stopClaims(store.ForConcurrentWorker(ctx), run, podless, "the project release stops it; it has no pod of the project")
			if len(pending) == 0 {
				return
			}
			r.log.Warn("project release: claims with no pod of the project were not stored suspended in time",
				"release", run.id, "timeout", r.stopTimeout.String(), "bound", run.bound.Format(time.RFC3339), "claims", pendingWords(pending))
			run.mu.Lock()
			if run.detail == nil {
				run.detail = map[claim.Token]string{}
			}
			for token, why := range pending {
				run.detail[token] = r.stopWaitRanOut() + ": " + why
			}
			run.mu.Unlock()
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	unlisted, err := r.takeUnlisted(ctx, run, sandboxes)
	if err != nil {
		r.fail(ctx, run, fmt.Sprintf("list the project's Sandboxes again: %v", err))
		return
	}
	if ctx.Err() != nil {
		return
	}
	r.takeOthers(ctx, run, slices.Concat(others, unlisted))
	r.finish(ctx, run)
}

// takePods takes each of pods to its end (takePod) on a goroutine of its own, under a store mark
// of its own, each counted in wg.
func (r *projectReleaser) takePods(ctx context.Context, run *releaseRun, pods []*releasePod, wg *sync.WaitGroup) {
	for _, pod := range pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.takePod(store.ForConcurrentWorker(ctx), run, pod)
		}()
	}
}

// takeUnlisted lists the project's Sandboxes again, from the API server rather than the runtime's
// store (list), once every issue pod of the first list ended and each claim with no pod was
// stopped. A create whose check of the mark passed just before the mark was set can land after the
// first list, or reach the runtime's store after it, and a release that missed that Sandbox would
// answer that it left none while the Sandbox stands (R6). The list comes after the stops of the
// claims with no pod, since such a claim's stop waits for a launch under way on it to let the
// claim go. Each Sandbox the first list did not hold gets a line after the first list's: an
// issue's pod is classified from its claims as they stand now, which the stop of the claims with
// no pod has stopped, and taken as the first list's pods are; the line of every other Sandbox is
// answered, for takeOthers to take with the first list's.
func (r *projectReleaser) takeUnlisted(ctx context.Context, run *releaseRun, listed []runtime.ProjectSandbox) ([]int, error) {
	again, err := r.list(ctx, run, r.pods.ProjectSandboxesFromAPI,
		"project release: list the project's Sandboxes again from the API server; trying the list once more")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(listed))
	for _, s := range listed {
		seen[s.Name] = true
	}
	var unlisted []runtime.ProjectSandbox
	var names []string
	for _, s := range again {
		if !seen[s.Name] {
			unlisted = append(unlisted, s)
			names = append(names, s.Name)
		}
	}
	if len(unlisted) == 0 {
		return nil, nil
	}
	r.log.Warn("project release: the API server holds Sandboxes of the project its first list did not; it takes them",
		"release", run.id, "sandboxes", strings.Join(names, ", "))
	pods, others := r.listSandboxes(run, unlisted)
	r.record(ctx, run)
	var wg sync.WaitGroup
	r.takePods(ctx, run, pods, &wg)
	wg.Wait()
	return others, nil
}

// list is the project's Sandboxes as from lists them: the runtime's store at the release's first
// list (ProjectSandboxes), the API server once every issue pod ended (ProjectSandboxesFromAPI). A
// list that fails is tried again listPause later, up to projectReleaseTries tries in all, each
// failed try before the last logged under the message retrying, and the last try's error is
// answered with how many tries were made and how far apart; a release whose context ends meanwhile
// lists no more (tryBounded).
func (r *projectReleaser) list(ctx context.Context, run *releaseRun, from func(context.Context) ([]runtime.ProjectSandbox, error),
	retrying string) ([]runtime.ProjectSandbox, error) {
	var sandboxes []runtime.ProjectSandbox
	err := r.tryBounded(ctx, r.listPause, retrying, func() error {
		var err error
		sandboxes, err = from(ctx)
		return err
	}, "release", run.id)
	if err != nil {
		return nil, err
	}
	return sandboxes, nil
}

// tryBounded calls try until it answers nil, up to projectReleaseTries calls in all, pause apart,
// and answers the last call's error with how many calls were made and how far apart. Each call
// that failed before the last is logged as a warning under the message retrying, with attrs, the
// try's number and the call's error. A ctx that ends meanwhile ends the calls: a call it cut
// answers its own error, and a pause it cut answers ctx's.
func (r *projectReleaser) tryBounded(ctx context.Context, pause time.Duration, retrying string, try func() error, attrs ...any) error {
	for n := 1; ; n++ {
		err := try()
		if err == nil || ctx.Err() != nil {
			return err
		}
		if n == projectReleaseTries {
			return fmt.Errorf("after %d tries %s apart: %w", n, pause, err)
		}
		r.log.Warn(retrying, slices.Concat(attrs, []any{"try", n, "tries", projectReleaseTries, "retryIn", pause.String(), "error", err})...)
		if err := backoff.Sleep(ctx, pause); err != nil {
			return err
		}
	}
}

// inventory writes the result's first lines — one per Sandbox, each waiting (listSandboxes), and
// one per claim of the stop set — and answers each issue pod, classified, and the lines of the
// other Sandboxes.
func (r *projectReleaser) inventory(run *releaseRun, sandboxes []runtime.ProjectSandbox) ([]*releasePod, []int) {
	pods, others := r.listSandboxes(run, sandboxes)
	run.mu.Lock()
	defer run.mu.Unlock()
	for _, c := range run.stop {
		run.result.Claims = append(run.result.Claims, api.ReleasedClaim{Token: c.Token, Issue: c.Issue, Role: c.Role, Session: c.Session})
	}
	return pods, others
}

// listSandboxes writes a waiting line for each of sandboxes after the lines the result holds, and
// classifies each issue pod from its claims as the machines hold them now (classifyReleasePod),
// with the claims of the stop set on its issue. The other Sandboxes are answered by their lines.
func (r *projectReleaser) listSandboxes(run *releaseRun, sandboxes []runtime.ProjectSandbox) ([]*releasePod, []int) {
	views := r.viewsByIssue()
	stopByIssue := map[string][]store.ProjectReleaseClaim{}
	for _, c := range run.stop {
		stopByIssue[c.Issue] = append(stopByIssue[c.Issue], c)
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	var pods []*releasePod
	var others []int
	for _, s := range sandboxes {
		i := len(run.result.Sandboxes)
		line := api.ReleasedSandbox{Name: s.Name, Kind: s.Kind, Issue: s.Issue, Outcome: api.ReleaseWaiting,
			OtherImage: s.Kind == runtime.ProjectSandboxIssue && s.PodUID != "" && !s.ConfiguredImage}
		run.result.Sandboxes = append(run.result.Sandboxes, line)
		if s.Kind != runtime.ProjectSandboxIssue {
			others = append(others, i)
			continue
		}
		pod := &releasePod{line: i, sandbox: s, stop: stopByIssue[s.Issue], nudge: make(chan struct{}, 1)}
		pod.class, pod.why = classifyReleasePod(views[s.Issue])
		pods = append(pods, pod)
	}
	return pods, others
}

// classifyReleasePod is how the release takes an issue pod, from its claims: left when a
// suspension that holds is over one of them, the operator's or the project's controller's
// (heldBy), with the roles and their holder named; read through its launcher (launcherRead) when
// no role process of it has said it is ready — none ever started, every claim failed, retired or
// suspended, or its role is starting again — and read through a role otherwise.
func classifyReleasePod(claims []supervise.ClaimView) (podClass, string) {
	if why := heldBy(claims); why != "" {
		return releaseLeaves, why
	}
	if len(runningClaims(claims)) == 0 {
		return releaseByLauncher, ""
	}
	return releaseReads, ""
}

// heldBy says which of claims a suspension that holds is over (Claim.RuleHolds: suspended by an
// operator or by the project's controller, or such a suspension held for its turn), and whose it
// is: "an operator suspended its <roles>", "the controller suspended its <roles>", or both, "" when
// none. The controller's holds against every rule as the operator's does (supervise's
// SuspendSource.Holds), so the release leaves such a pod as it leaves one an operator holds, and
// holds its claims for an operator (hold).
func heldBy(claims []supervise.ClaimView) string {
	var byOperator, byController []string
	for _, c := range claims {
		switch {
		case !c.RuleHolds():
		case c.Holder() == supervise.SuspendedByController:
			byController = append(byController, string(c.Role))
		default:
			byOperator = append(byOperator, string(c.Role))
		}
	}
	var why []string
	if len(byOperator) > 0 {
		why = append(why, "an operator suspended its "+strings.Join(byOperator, ", "))
	}
	if len(byController) > 0 {
		why = append(why, "the controller suspended its "+strings.Join(byController, ", "))
	}
	return strings.Join(why, ", and ")
}

// runningClaims is every claim of claims whose role process runs and has said it is ready, the
// roles a release reads through and asks.
func runningClaims(claims []supervise.ClaimView) []supervise.ClaimView {
	var running []supervise.ClaimView
	for _, c := range claims {
		if supervise.SaidReady(c.State) && c.Locator != nil {
			running = append(running, c)
		}
	}
	return running
}

// inTurn is whether any of claims is in a turn (workingClaims).
func inTurn(claims []supervise.ClaimView) bool {
	return slices.ContainsFunc(claims, func(c supervise.ClaimView) bool { return c.State == supervise.StateWorking })
}

// viewsByIssue is every claim the daemon supervises on an issue, as its machine last published
// it, by issue and in token order: the inventory's one reading of every pod's claims.
func (r *projectReleaser) viewsByIssue() map[string][]supervise.ClaimView {
	byIssue := map[string][]supervise.ClaimView{}
	for _, m := range r.machines.supervised() {
		v := m.View()
		if v.Issue != "" {
			byIssue[v.Issue] = append(byIssue[v.Issue], v)
		}
	}
	for _, claims := range byIssue {
		slices.SortFunc(claims, byViewToken)
	}
	return byIssue
}

// issueViews is every claim the daemon supervises on issue, as its machine last published it, in
// token order, read now. It keeps only issue's claims of the machines' lock-free snapshot, as
// supervisor.claimInTurn does, since it runs at each pod's re-read and inside the runtime's pod
// lock (stopAndDelete's authorize).
func (r *projectReleaser) issueViews(issue string) []supervise.ClaimView {
	if issue == "" {
		return nil
	}
	var views []supervise.ClaimView
	for _, m := range r.machines.supervised() {
		if v := m.View(); v.Issue == issue {
			views = append(views, v)
		}
	}
	slices.SortFunc(views, byViewToken)
	return views
}

// byViewToken orders claim views by token.
func byViewToken(a, b supervise.ClaimView) int {
	return strings.Compare(string(a.Token), string(b.Token))
}

// takeOthers ends every Sandbox that is no issue's, once every issue pod has ended, those of both
// lists of the project (others, the first list's lines and then takeUnlisted's): the controller's
// claim is stopped and waited for as the issue pods' claims were, so a controller in a turn is
// waited for until the turn's end or the bound (stopDeadline), and its pod deleted through the
// runtime's release of the claim (ReleaseProjectController); each repository cache and image probe
// is deleted through the runtime's fenced delete. A delete the volume guard refuses, or one the
// fence finds written since it was decided, leaves that Sandbox, named with the refusal, as does a
// Sandbox of a kind the release does not know. A controller's pod left — its claim not stored
// suspended in time, or its delete refused — has the controller's claim held for an operator, as a
// left issue pod's claims are (hold), so no boot starts the controller while its pod stands. A
// left Sandbox's reason, the hold's and the log line's, keeps the start of the error's text, and
// the log the whole of it (keepWhole).
func (r *projectReleaser) takeOthers(ctx context.Context, run *releaseRun, others []int) {
	var controllers []claim.Token
	for _, c := range run.stop {
		if claim.IsController(c.Role, c.Issue, c.Tree) {
			controllers = append(controllers, c.Token)
		}
	}
	pending := r.stopClaims(ctx, run, controllers, "the project release stops it to delete its pod")
	controller := claim.ControllerToken(r.project)
	for _, i := range others {
		run.mu.Lock()
		line := run.result.Sandboxes[i]
		run.mu.Unlock()
		var err error
		switch line.Kind {
		case runtime.ProjectSandboxController:
			if why, waiting := pending[controller]; waiting {
				err = fmt.Errorf("the controller's claim was %s: %s", r.stopWaitRanOut(), why)
			} else {
				err = r.pods.ReleaseProjectController(ctx, controller)
			}
		case runtime.ProjectSandboxCache, runtime.ProjectSandboxProbe:
			err = r.pods.DeleteProjectSandbox(ctx, line.Name)
		default:
			err = errors.New(releaseUnknownKind)
		}
		if err == nil {
			run.mu.Lock()
			run.result.Sandboxes[i].Outcome = api.ReleaseDeleted
			run.mu.Unlock()
			r.log.Info("project release: deleted a Sandbox", "release", run.id, "sandbox", line.Name, "kind", line.Kind)
			r.record(ctx, run)
			continue
		}
		reason := r.keepWhole(err.Error(), "release", run.id, "sandbox", line.Name, "kind", line.Kind)
		if line.Kind == runtime.ProjectSandboxController {
			if unended := r.hold(ctx, run, controllers, nil, "the project release leaves the controller's pod: "+reason); len(unended) > 0 {
				reason = strings.Join(append([]string{reason}, unended...), reasonSeparator)
			}
		}
		run.mu.Lock()
		at := &run.result.Sandboxes[i]
		at.Outcome, at.Reason = api.ReleaseLeft, reason
		run.mu.Unlock()
		r.log.Warn("project release: left a Sandbox", "release", run.id, "sandbox", line.Name, "kind", line.Kind, "reason", reason)
		r.record(ctx, run)
	}
}

// finish waits for each claim the release held for an operator in a pod it left (run.held) to be
// stored suspended, on a wall-clock bound of twice the stop timeout from the start of this wait
// (the hold's own deadline, then the runtime's stop grace before the claim is stored), writes each
// claim's line from its record as it stands, and finishes the release, which ends its ask round on
// the mark (projectRelease.finish) while the mark stays held. An operator's hold lives in memory
// only, so a release reported finished leaves no left pod's claim stored working for a restart to
// adopt.
func (r *projectReleaser) finish(ctx context.Context, run *releaseRun) {
	run.mu.Lock()
	held := slices.Clone(run.held)
	run.mu.Unlock()
	within := 2 * r.stopTimeout
	pending := r.awaitSettled(ctx, held, true, time.Now().Add(within), run.heldRefused)
	if ctx.Err() != nil {
		return
	}
	if len(pending) > 0 {
		for token, why := range pending {
			pending[token] = fmt.Sprintf("not stored suspended by %s within %s: %s", supervise.SuspendedByOperator, within, why)
		}
		r.log.Warn("project release: claims it held for an operator in a left pod were not stored suspended in time",
			"release", run.id, "claims", pendingWords(pending))
	}
	r.writeClaims(ctx, run, pending)
	// The finish's result is read under the lock every record takes, and the mark is finished under
	// it, so an answer recorded just before it is in it and no record is written after it.
	run.recording.Lock()
	r.mark.finish()
	result := run.snapshot()
	run.recording.Unlock()
	r.writeFinish(ctx, run, result, r.now().UTC())
}

// writeFinish records the release finished at at with result, its last. Once the mark is
// finished nothing else writes the record (record), and a record left unfinished reads as a
// release that goes on until the next boot, which then says that a boot ended it before it
// finished. So a write that fails is made again, retry after the first and up to
// projectReleaseRetryMax apart, until one lands or ctx ends: ctx is the release goroutine's, which
// lives as long as the releaser's own (r.ctx). The write is fenced on a record not yet finished
// (store.FinishProjectRelease), and a refusal of that fence ends the tries too, since no try
// changes it: a try made after one that landed with its answer lost meets it.
func (r *projectReleaser) writeFinish(ctx context.Context, run *releaseRun, result json.RawMessage, at time.Time) {
	wait := r.retry
	for {
		err := r.store.FinishProjectRelease(ctx, run.id, result, at)
		switch {
		case err == nil:
			r.log.Info("project release: finished; the daemon's next boot starts each claim it stopped", "release", run.id)
			return
		case ctx.Err() != nil:
			return
		case errors.Is(err, store.ErrNoUnfinishedProjectRelease):
			r.log.Error("project release: record the release finished; the store holds no unfinished release of its id, so the finish is not written again",
				"release", run.id, "error", err)
			return
		}
		r.log.Error("project release: record the release finished; writing it again", "release", run.id, "retryIn", wait.String(), "error", err)
		if backoff.Sleep(ctx, wait) != nil {
			return
		}
		wait = min(2*wait, projectReleaseRetryMax)
	}
}

// fail records a release that could not go on with what stopped it, and finishes it: nothing more
// of it acts, and the boot ends it as it ends any release. Why can carry a far side's error text,
// such as a list of the project's Sandboxes the API server refused, so the result and the line
// that logs it keep its start, and the log the whole of it (keepWhole).
func (r *projectReleaser) fail(ctx context.Context, run *releaseRun, why string) {
	kept := r.keepWhole(why, "release", run.id)
	r.log.Error("project release: stopped short of its acts", "release", run.id, "reason", kept)
	run.mu.Lock()
	run.result.Failure = kept
	run.mu.Unlock()
	r.finish(ctx, run)
}

// keepWhole is text, a far side's error text, as the release keeps it in its result, in a hold's
// reason and in its lines: its start, as the store keeps such a text (storetext.FarSideError),
// since a runtime's error names the request's URL, with its query, and a relayed command's can
// carry its whole output. Where that cuts the text, keepWhole first logs the whole of it, in parts
// under projectReleaseErrorInFull with args (storetext.LogWhole), so the parts come before the
// line that keeps the start.
func (r *projectReleaser) keepWhole(text string, args ...any) string {
	storetext.LogWhole(r.log, projectReleaseErrorInFull, text, args...)
	return storetext.FarSideError(text)
}

// record writes the result as it stands on the release's record. A write that fails is logged;
// the next one carries everything it would have.
func (r *projectReleaser) record(ctx context.Context, run *releaseRun) {
	run.recording.Lock()
	defer run.recording.Unlock()
	if r.mark.finished() {
		return
	}
	if err := r.store.RecordProjectRelease(ctx, run.id, run.snapshot()); err != nil && ctx.Err() == nil {
		r.log.Error("project release: record its result", "release", run.id, "error", err)
	}
}

// snapshot is the result as it stands, as the record keeps it.
func (run *releaseRun) snapshot() json.RawMessage {
	run.mu.Lock()
	defer run.mu.Unlock()
	encoded, err := json.Marshal(run.result.Listed())
	if err != nil {
		// Every field of the result is a plain value; it always encodes.
		panic(fmt.Sprintf("encode the project release's result: %v", err))
	}
	return encoded
}
