// Package runtime is the boundary between the daemon's supervision of an agent and the thing
// that actually runs it — a pane on the daemon's private tmux server today, a pod in a cluster
// at Stage 4.
//
// The boundary carries process facts only: a runtime spawns, stops, and observes processes, and
// says `Alive`, `Gone`, `NotRecordedProcess`, or `Uncertain` about them. What the agent inside
// the process is doing — that it connected, acknowledged a prompt, started or ended a turn — is
// the worker stream's to report, a peer source beside this one. Both reach the supervisor, which
// is the only place the two kinds of fact meet.
//
// The connection contract (`Conn`, `Conns`) lives here rather than in the stream package so that
// this package and its fake depend on methods rather than on the stream's structs; the stream's
// own connection satisfies it.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Runtime is what the supervisor has instead of a process table. Every method is about one
// agent's process, addressed by the locator the runtime itself minted, except `Release`, which is
// about a claim whether or not a process runs for it, `Observe` and `ReconcileOrphans`, which are
// about all of them, and `CleanupTree`, which is about a whole tree.
//
// A locator that no longer names the claim's current process — nothing is there, or something
// else is — is a process already stopped as far as `Suspend` and `Release` are concerned: neither
// acts on it, and neither returns an error for it. `Suspend` leaves a newer incarnation the runtime
// recorded for the claim alone. What a claim's process shares with the other processes of its
// issue or tree — a sandbox's issue pod, its volume and Secrets — outlives every claim's `Suspend`
// and `Release`, and only `CleanupTree` deletes it. A state the runtime cannot verify is an error,
// never "stopped".
type Runtime interface {
	// Spawn starts an agent and returns the locator that identifies the process it started —
	// including the incarnation, captured at spawn, that later observations are fenced against.
	// On an error the caller owns stopping any previous process of the claim, and Spawn has
	// stopped any process it started itself.
	Spawn(ctx context.Context, spec SpawnSpec) (Locator, error)
	// Resume starts the same agent again from the session file the spec names, after waiting
	// for the previous incarnation to be Gone. prev is the incarnation the caller recorded, nil
	// when it recorded none, and a hint: a runtime that finds a claim's process by the claim's own
	// place, as a sandbox finds a role's launcher in its issue's pod, waits out whatever runs
	// there even when prev is nil. A claim resumes the agent it recorded or none: a fresh agent on
	// a claim that had one is the failure the same-agent refusal exists to catch. A session the
	// runtime looked for and did not find is ErrSessionFileMissing, and a session store that could
	// not be asked at all is ErrSessionStore; every runtime answers both the same way, having
	// started nothing, and the caller tells them apart by what it does next (supervise's start).
	// On an error the caller owns stopping prev (Suspend), and Resume has stopped any process it
	// started itself.
	Resume(ctx context.Context, prev *Locator, spec SpawnSpec) (Locator, error)
	// Suspend stops the process gracefully, within the runtime's stop grace, and keeps everything
	// a later Resume needs: the agent's session, and whatever the runtime holds for the claim. The
	// relaunches that replace a live process — prompt retirement and the registration deadline —
	// retire it with Suspend first; a relaunch after a death calls none, since the process is gone
	// and Spawn and Resume never start a second process on the claim.
	Suspend(ctx context.Context, loc Locator) error
	// Release ends the claim, whether or not a process runs for it. The process k.Locator records,
	// while it is still the claim's, is sent a shutdown frame over the claim's connection and given
	// the runtime's stop grace to end itself; then it is gone, and the runtime forgets the claim.
	// What the claim's process shared with its issue or tree stays for CleanupTree. k.Locator is
	// nil when no process runs.
	Release(ctx context.Context, k Known) error
	// Probe is one observation of one locator, now. `Uncertain` is a verdict, not a failure: a
	// returned error means the runtime itself could not be asked, and an error is never a
	// statement about the process.
	Probe(ctx context.Context, loc Locator) (Observation, error)
	// Observe is the periodic sweep of every locator the runtime knows about. The channel closes
	// when ctx ends.
	Observe(ctx context.Context) (<-chan Observation, error)
	// ReconcileOrphans ends what this runtime owns that belongs to no known claim — what a crash
	// between spawning a process and persisting its locator leaves behind. known is every claim the
	// daemon has not retired; the located ones also join the watch. The daemon reads known before
	// it calls this, while claims may launch, so what the runtime itself launched and has not
	// released is never an orphan, known or not. What an earlier sweep adopted protects nothing: a
	// process no sweep's known set names any more is the orphan sweep's. An entry that fails
	// Validate is an error, and nothing of its claim is ended: tmux, which knows a claim's
	// processes only by the locators it is given, then ends nothing at all. The sandbox deletes no
	// issue Sandbox of a tree whose lifecycle is open or releasing: those are CleanupTree's alone; and
	// no controller Sandbox whose claim the store holds unretired when the delete is decided.
	ReconcileOrphans(ctx context.Context, known []Known, grace time.Duration) error
	// AdoptWorkingCopy hands the agent's working copy the git identity its commits are authored
	// with, in the place the working copy actually lives (which under a sandbox is not a
	// directory the daemon can see).
	AdoptWorkingCopy(ctx context.Context, loc Locator, id GitIdentity) error
	// ProvisionsWorkspaces is whether the runtime provisions each claim's workspace where its
	// process runs (a pod's init containers, on the issue's own volume). When it does, the daemon
	// provisions and removes none on its own host.
	ProvisionsWorkspaces() bool
	// ProvisionBound is how much longer a launch may run before its agent's own process has even
	// started: zero for a runtime whose process starts the agent at once (tmux), and the Sandbox
	// runtime's own init-container budget (its two clones' and the issue's checkout's bounds
	// together) for a launch whose pod runs provisioning first, which that runtime's wait for the
	// pod's launcher allows past its boot timeout. The supervisor also adds it to the registration
	// deadline's base Boot×RegistrationIntervals bound while a claim is still StateLaunching.
	ProvisionBound() time.Duration
	// LaunchBound is the longest one launch try — one Spawn or Resume call — may run on the
	// runtime's own account before the runtime ends it with an error, so a try that never answers
	// ends at the bound, is charged to the claim's launch budget, and the next try runs. It is no
	// shorter than a provisioning pod's rightful wait (BootTimeout, ProvisionBound and the Sandbox
	// runtime's own calls), and zero for a runtime that states no bound: a tmux pane start is a
	// local command with no cluster wait, as ProvisionBound zero means no init phase. A try the
	// caller's own context ends is not the bound's: the supervisor reads its context's error to
	// tell the daemon's stop from a refusal, and charges that try nothing.
	LaunchBound() time.Duration
	// PodDrift is why the place k's process runs in no longer matches the configuration the
	// runtime would build it from now, one short phrase an operator reads per reason, and nil when
	// nothing differs. Only a runtime whose processes share a place fixed at its creation has any
	// — the Sandbox runtime's issue pod, whose image, reservation, operator volumes and mounts,
	// disk, role overlays, secrets broker enrollment and session store are all fixed for the pod's
	// life — and only for a claim that runs in one: it answers nothing for a claim with no
	// process, which records no place at all, and nothing for the project controller's own pod,
	// which has no issue workspace and nothing to push. The daemon opens an update round on a
	// non-empty answer: the round asks each role of that place to do the steps of its update
	// message (supervise.UpdateTask) and answer (api's claims/ready-for-update), and replaces the
	// place (ReplacePod) once every asked role has answered, no turn runs and no claim's activity
	// report holds it (internal/daemon's roundEnd.holding), or at the round's bound
	// (pod_update_grace_minutes) whatever the roles answered and whatever runs, an operator's
	// suspension excepted.
	//
	// ErrPodGone is the place the locator records not being there, or being ended for something
	// other than this round's own replacement, which is neither drift nor a failure to ask:
	// nothing can be read of a place that is going, so nothing says it has drifted from nothing,
	// and the round open over it ends. Any other error means the runtime itself could not be
	// asked, and is never a statement that something drifted either.
	PodDrift(ctx context.Context, k Known) ([]string, error)
	// ReplacePod ends the place k's process runs in, once its update round is over, and returns
	// when the runtime no longer has it. It creates nothing in its stead: every process it ends is
	// reported ended by the daemon's own act (Observation.Displaced), and the supervisor's resume
	// of the first of them builds the place again from the current configuration. It is a no-op
	// returning nil for a claim with no process, for a place already gone or already stopped, and
	// for a runtime with no such place to replace.
	//
	// due is the caller's own last word on whether the replacement is still due, asked under
	// whatever the runtime holds the place with and immediately before it ends anything: a
	// runtime that waits for the place's launch turn waits first and asks after, because what the
	// caller decided before the wait is exactly what the wait can outlive — a worker of the round
	// starting a turn, or a claim of the issue the caller had never read. nil from due is a
	// replacement still due, and the runtime goes on; any error is a replacement no longer due,
	// which the runtime returns having changed nothing, so the caller tells its own refusal from
	// a failure by the error it handed over. due is required: a replacement is the one call that
	// deletes an issue's disk, so every caller states what makes it still due rather than
	// leaving the question to a nil.
	ReplacePod(ctx context.Context, k Known, due func(context.Context) error) error
	// CleanupTree deletes what the runtime holds for tree beyond its claims' processes, once every
	// claim of the tree has retired and the tree's cleanup is reserved (store.CleanupReservedTree);
	// nil once nothing of it remains. A runtime that holds nothing per tree returns nil at once.
	CleanupTree(ctx context.Context, tree string) error
}

// ErrSessionFileMissing is a Resume whose spec.ResumeSessionFile is held by no store the runtime
// can resume it from: no folder the tmux runtime searched holds that session, or the session table
// the Kubernetes launcher reads has no row for it. The session is gone, not unreachable, so the
// caller starts a fresh agent in its place, charged nothing and reported (supervise's start).
// Nothing was started. Both runtimes answer this for that one fact, whatever else is configured —
// the operator's takeover of another runtime's claims (`runtime.tmux.take_over`) changes which
// sessions are found, never what a session found nowhere means.
var ErrSessionFileMissing = errors.New("the recorded session file is missing")

// ErrSessionStore is a Resume whose session store could not be asked: the table does not exist, or
// the store did not answer. It says nothing about the claim's session, so it is neither a missing
// session nor the claim's own failure: the caller keeps the session, charges nothing, and starts
// again every minute until the store answers (supervise's startWaitsForStore). Nothing was
// started.
var ErrSessionStore = errors.New("the session store could not be asked")

// ErrPodGone is PodDrift asked about a place that is not there, or on its way out for an act
// that is not its round's: the Sandbox the locator records is gone, no pod of it is running, the
// pod running now is not the one the locator records, or the pod is being ended under it — the
// pod was replaced where it stood for a failure of its own, and its disk went with it.
//
// It is why PodDrift says that rather than answering no drift. The two readings are opposite
// where it matters: drift that cleared is a pod still running, which the round open over it
// replaces as it promised its workers it would, while a pod that is not there has taken whatever
// was unpushed on its disk with it, which the round's workers must be told of (internal/daemon's
// podWentWithoutItsRound). A caller that cannot tell them apart tells an issue nothing of the
// work it lost, and replaces a pod that is already gone.
//
// The round open over such a place ends at the sweep that reads this, and every sweep asks this
// before the round waits on a turn of its own, since each such wait is a wait on the pod.
// What was lost is read from the round's own record of the claims it asked and what each had
// done when it left, never from where a claim's process is now (internal/daemon's
// endRoundOverAGonePod, supervise's Claim.UpdateLeftRound), so the sweep that reads this need
// not come before the roles are relaunched into the pod that replaced this one.
var ErrPodGone = errors.New("the pod the locator records is not there")

// IssuePodReader reads which pod an issue's Sandbox holds now, asked by the issue alone: the uid
// of that pod, "" where the runtime holds no Sandbox of the issue or the Sandbox holds no pod, and
// read false where the runtime cannot say at all. It exists beside PodDrift because it needs no
// locator: an update round whose claims record no pod — every claim suspended, since a suspension
// lets the process go and its locator with it (supervise's letGo) — has no claim left to ask
// PodDrift through, and only the issue to ask by (internal/daemon's podlessRound). A pod other
// than the round's own, or none, is that round's pod gone, and the round ends; the round's own
// pod, or no answer, leaves it waiting. A runtime whose processes run in no pod implements
// nothing here, and the daemon then reads no answer, which a round never needs from it, since
// such a runtime answers no drift and so opens no pod round.
type IssuePodReader interface {
	IssuePod(issue string) (pod string, read bool)
}

// LaunchSessionNamer names the session a launch's agent starts or resumes under, where the
// runtime chose it before the agent started rather than leaving it to the agent to mint: a
// Claude Code role's, whose `--session-id` or `--resume <id>` the Sandbox runtime decides when it
// prepares the launch (claudeInputs). It answers for the incarnation loc records, and false for
// any other — an Oh My Pi role, whose id its agent mints and reports at its registration, a
// launch the runtime does not remember (a daemon restarted since), or an earlier incarnation. It
// is what lets the daemon register a Claude role's session for its pod at the shim's hello, before
// the agent registers (supervise's ensureRegistered). A runtime that names no session implements
// nothing here.
type LaunchSessionNamer interface {
	LaunchSession(loc Locator) (session string, named bool)
}

// IssueSuspender stops an issue's pod once authorize, called under the runtime's issue launch
// lock, says the issue's durable close and its complete stored role population allow it (false
// finishes the close without acting). Once the pod is stopped, the issue's Sandbox is deleted
// with everything it owns — the role Secrets holding each launcher's token and credentials — and
// the call returns once the cluster no longer has it; a Sandbox already gone is success, so a
// retried close finishes. Nothing durable is kept by keeping it: an issue pod's disk is the pod's
// own and went with the pod, so the issue's next launch makes the Sandbox again.
// Process-only runtimes have no shared issue resources to suspend.
type IssueSuspender interface {
	SuspendIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error
}

// WorkspaceBundler asks one live role's process to bundle the unpushed work of the issue
// workspace every role of the issue shares, so a release can keep it while the pod, its disk and
// the workspace on it go. The role named must be one whose process runs: the shim builds the
// bundle with the jj and git of the workspace it already works in. A runtime whose processes
// share no workspace implements nothing here, and the release of such a pod saves nothing because
// there is no such pod.
type WorkspaceBundler interface {
	BundleWorkspace(ctx context.Context, issue string, role claim.Role) (WorkspaceBundle, error)
}

// SavedWorkspaceBundle answers the bundle an issue's last release saved for the pod of podUID,
// created at podCreated, to restore, and what that ask did to the bundle. It is the other half
// of WorkspaceBundler: the save's own store (store.Store.TakeWorkspaceBundleForRestore, bound to
// the daemon's project where the daemon wires it), handed to the runtime so that the runtime,
// which alone knows when a replacement pod's launcher is up, can carry the saved work back into
// the new workspace before any role of that pod starts.
//
// WorkspaceBundleNone is nothing to send and nothing recorded: an issue never released, one
// whose restore the launcher confirmed, one whose release a wake cancelled, one whose bundle has
// had its one attempt already, one the daemon's project does not record, whose bundle another
// project's daemon saved, and the pod the work was taken off asking for its own workspace's
// work back. The runtime calls this at every start of every role, and a release stops the roles
// of the pod it is releasing, so a role relaunched in that pod before the teardown deletes it is
// one of those calls; the store knows that pod by the uid the save recorded
// (WorkspaceBundle.PodUID) and leaves the row untouched for the pod that replaces it.
//
// WorkspaceBundleSend is the attempt, and the call is where it is spent: the store records
// podUID against the bundle before answering the body, and answers no body for that bundle
// again. A launcher holds its record of the restore it ran in memory, so a roles container that
// restarts comes back to the pod's own disk with that record gone and the workspace hours of
// work past the save; the daemon's record is what keeps the bundle from being applied over that
// work.
//
// WorkspaceBundleRefusedOlder is that same one attempt, spent on a pod that was not created
// after the save: nothing is sent, and no later pod is sent the bundle either. Such a pod is one
// the release's teardown did not replace, or the replacement under a node clock running ahead of
// the API server's; either way its roles go on working and pushing from the workspace its own
// fetch provisioned, and a bundle the daemon kept open for the pod after it would be applied
// over that work. The saved work stays in the database for a person to recover and holds the
// issue's pod (workflow.IdleHoldBundleUnrestored).
type SavedWorkspaceBundle func(ctx context.Context, issue, podUID string, podCreated time.Time) (WorkspaceBundle, WorkspaceBundleTake, error)

// WorkspaceBundleRestored is the runtime's confirmation that the pod replacing the released one
// has put the issue's saved work back: its launcher ran the restore and reported that it
// succeeded, naming the release whose save it restored. It drops that release's saved copy and no
// other (store.Store.DeleteRestoredWorkspaceBundle), so it is called only on that report, and a
// report that arrives after a later release has saved again finds nothing of its own to delete —
// a bundle no launcher has restored, because none took it, because the restore failed, or because
// its confirmation named a release the store no longer holds, stays in the daemon's database for
// the issue's next pod.
type WorkspaceBundleRestored func(ctx context.Context, issue string, release int64) error

// MemoryGuardKilled is the daemon's reading of one process an issue pod's launcher ended with
// SIGKILL because the pod was near its memory limit, or, in a microVM pod, one that the pod's own
// kernel stopped because the pod ran out of memory (the report's Cause,
// shimwire.MemoryKillCauseKernel, or shimwire.MemoryKillCauseKernelProtected for a process that
// runs the role's session), which the launcher's guest watch names
// (shimwire.LauncherMemoryKill): the launcher sends the report on the connection of the role whose
// process it was, and the daemon tells that role why the process is gone. The session gives it the
// issue key, the claim token of the role the connection authenticated for, the uid of the pod the
// launcher runs in and the report; the three are the session's own and never the report's, so a
// launcher cannot name another role, issue or pod.
//
// It takes the wire's own frame rather than a shape of this package's: shimwire imports nothing
// of the daemon's own packages, so the import is acyclic, and a second copy of the report would
// be one more shape to keep in step for no reader that needs it.
//
// An error, a store that did not answer among them, is returned to the session, which logs it
// and drops the report: nothing acknowledges the frame, so the launcher does not send it again,
// and the guard's kill line in the pod's own log is then the one record of the kill.
type MemoryGuardKilled func(ctx context.Context, issue string, token claim.Token, podUID string, report shimwire.LauncherMemoryKill) error

// ControllerMemoryGuardKilled is the daemon's reading of one process the controller pod's launcher
// ended with SIGKILL because the pod was near its memory limit, which the memory guard of a gVisor
// controller pod does, or, in a microVM controller pod, one that the pod's own kernel stopped
// because the pod ran out of memory (the report's Cause, shimwire.MemoryKillCauseKernel, or
// shimwire.MemoryKillCauseKernelProtected for a process that runs the controller's session), which
// the launcher's guest watch names (shimwire.LauncherMemoryKill): the controller's own pod runs the
// guard or the guest watch as an issue pod in the same sandbox does, and the daemon tells the
// project's controller which of its programs ended and why. The session gives it the claim token
// its connection authenticated for, the uid of the pod the launcher runs in and the report, the
// first two the session's own and never the report's. The controller's pod is of no issue, so
// there is no issue to name, and MemoryGuardKilled, whose note is told to a role of an issue, has
// nothing to take it with.
//
// It takes the wire's own frame for the reason MemoryGuardKilled does, and an error is returned
// to the session, which logs it and drops the report, as MemoryGuardKilled's is.
type ControllerMemoryGuardKilled func(ctx context.Context, token claim.Token, podUID string, report shimwire.LauncherMemoryKill) error

// VolumeReported is the daemon's reading of one sweep of an issue pod's volume, as the cache
// trimmer in that pod's launcher process reported it: the launcher sends at most one such frame
// per sweep that ended with the volume still above its low mark, none for a sweep the settings
// stopped, and one when the trimmer has switched itself off after three faults
// (shimwire.LauncherVolumeReport). The session gives it the issue key, the uid of the pod the
// launcher runs in and the report; the trimmer's own identifier, which is what makes a restarted
// roles container a new counting line for the daemon's notice bounds, is inside the report.
//
// It takes the wire's own frame rather than a shape of this package's: shimwire imports nothing
// of the daemon's own packages, so this import is acyclic, and a third near-copy of the report —
// the wire's, the runtime's and the record's — would be three shapes to keep in step where two
// are already the price of a durable notice.
//
// A store that does not answer is logged and the report is dropped there. The launcher cannot
// hold it again once the daemon has read it, and it is not asked to: the trimmer sweeps on, and
// the next sweep that ends above the low mark reports the volume again, under the same trimmer
// and the episode it is then in, so the state a dropped report would have told reaches the
// architect from a later one, as far as the notice bounds let that one through (workflow's
// volumeNoticeHeldBy; internal/runtime/sandbox's volumeReported, as restoreReported's own
// comment says of its delete). The report that the trimmer switched itself off is its last, and
// one dropped here is not sent again.
type VolumeReported func(ctx context.Context, issue, podUID string, report shimwire.LauncherVolumeReport) error

// RegistrationDeadline is the registration deadline's base Boot×RegistrationIntervals bound plus
// grace: the supervisor's armRegistration calls it with ProvisionBound before a claim's hello and
// with zero after, so the one computation is the deadline's.
func RegistrationDeadline(boot time.Duration, intervals int, grace time.Duration) time.Duration {
	return boot*time.Duration(intervals) + grace
}

// Known is one claim as a runtime is told of it — each entry of the orphan sweep's known set, and
// the claim Release ends: the claim, and its process when one runs.
type Known struct {
	Claim claim.Token
	// Locator is the claim's process; nil when none runs — suspended, failed, or never launched.
	Locator *Locator
}

// Validate is the agreement every runtime checks before it acts on a Known: a claim, and, when
// there is a locator, a valid one of that same claim. A pair that disagreed would have a runtime
// end one claim's objects while it stopped another claim's process.
func (k Known) Validate() error {
	if k.Claim == "" {
		return errors.New("known claim: no claim token")
	}
	if k.Locator == nil {
		return nil
	}
	if err := k.Locator.Validate(); err != nil {
		return fmt.Errorf("known claim %s: %w", k.Claim, err)
	}
	if k.Locator.Claim != k.Claim {
		return fmt.Errorf("known claim %s: its locator is claim %s's process", k.Claim, k.Locator.Claim)
	}
	return nil
}

// SpawnSpec is everything a runtime needs to start one agent: which claim it is, what it is
// working on, and the environment, secrets, and prompt it starts with. The claim token travels
// with the process because the runtime addresses the agent's connection by it — `Suspend` and
// `Release` send a shutdown frame over `Conns.Conn(loc.Claim)` — and because a locator without it
// could not be matched to the claim it belongs to.
//
// Nothing in it is a place on one runtime's disk. Repository is the issue's repository, the zero
// Repository for a configuration with none, and each runtime locates the issue's
// workspace under its own root. DefaultBranch is that repository's default branch as GitHub names
// it, a branch name used verbatim (release/v7.1): the base each runtime provisions an issue with no
// branch at; "" exactly when Repository is zero. Env is the agent's plain variables; Secrets never travel as
// values — each is written to a 0600 file and reaches the process as a `<NAME>_FILE` pointer — and
// carries only what is the claim's to carry: a credential every agent of the deployment shares,
// like the Dispatch bearer, is a runtime option. ResumeSessionFile is set only by `Resume`, and it
// names the transcript the same agent continues from.
// Plugins, set only under Kubernetes (specs.SpawnSpec, LEGION-164), carries the plugin pair a
// stale-plugin relaunch installs into the kept pod before Oh My Pi starts, in place of the
// image's own copies; nil for every launch that installs nothing — every tmux launch, and every
// Kubernetes launch that is not replacing a process on another plugin contract. The tmux runtime
// ignores it: a pane always resumes on the plugin installed on the daemon's host, which the boot
// gate already holds to the daemon's contract. Variants are the arms of the project's declared
// experiments this launch's unit was drawn into, in the order the project's file declares the
// experiments (internal/daemon's specs reads them): such a launch names each variant's overlays
// last in PI_CONFIG_FILES, in that order, and ends its system prompt with each variant's
// instructions in that same order; empty for a launch in no experiment and for the controller,
// which runs in no tree. IssueTrailer is the issue key this pane stamps into its
// commits, as the `Dispatch: <KEY>` trailer the agent's jj overlay writes (LEGION_ISSUE_TRAILER,
// which each runtime sets itself): the claim's own issue when the project names its repository in
// `commit_issue_trailer_repos` (which internal/daemon's specs reads); "" for a repository the
// project does not name, for a launch with no repository, and for the controller, which works
// neither. Backend is the harness the process runs (`worker_backends`,
// Backends); "" is Oh My Pi (Of).
type SpawnSpec struct {
	Claim             claim.Token
	Project           string
	Tree              string
	Issue             string
	Role              claim.Role
	Generation        uint64
	BootToken         string
	Env               map[string]string
	Secrets           map[string]string
	Prompt            PromptParts
	Repository        ghrepo.Repository
	DefaultBranch     string
	ResumeSessionFile string
	Plugins           *PluginInstall
	Variants          []Variant
	IssueTrailer      string
	Backend           Backend
}

// Variant is one arm of one declared experiment a launch runs in (SpawnSpec.Variants): the
// experiment's name and the arm's, as the draw recorded them (config.Experiment.Name,
// config.Variant.Name), the Oh My Pi settings overlays the arm names (config.Variant.ConfigFiles)
// and the key the operator wrote those overlays under (config.Variant.ConfigFilesField). An arm
// that names no overlay adds nothing to PI_CONFIG_FILES, so a launch carrying only such arms names
// the overlays a unit in no experiment gets; its prompt still ends with the copy of any
// instructions those arms name (PromptParts.VariantInstructionsPaths). Only an arm that names
// neither file is the control the others are measured against, and a launch carrying such arms
// alone is the launch a unit in no experiment gets.
//
// The instructions an arm names are no field of this: no runtime reads them off an arm, and what
// a launch's prompt ends with is the daemon's own copy of each of them
// (PromptParts.VariantInstructionsPaths, which internal/experiment's Launch answers beside these
// arms).
//
// ConfigFilesField is what a refusal and a pod's drift name the arm's overlays by, so both name
// the key an operator can find in the file rather than a path each reader works out for itself:
// `projects.<KEY>.memory.config_files` for the arm the `memory` block declares, which a drift
// names `memory.config_files` instead (MemoryBlock). The
// declared list sets it on every arm that may carry overlays (config.Project.ExperimentsDeclared):
// each arm of an `experiments` key, and the `memory` block's own arm whether or not the block
// names `config_files`. It is read only where the arm names overlays, so the block's control arm,
// which names none and stands under no key of its own, leaves it empty.
//
// MemoryBlock is whether the arm is one the project's `memory` block declares
// (config.Variant.MemoryBlock), which config.Project.ExperimentsDeclared decides and this carries.
// It is for a pod's drift report (internal/runtime/sandbox's movedRoleOverlays), which names a
// moved overlay of the block's own arm `memory.config_files <file>`, the spelling it had before
// any experiment was declared, and every other arm's by ConfigFilesField.
//
// Roles is the issue roles the arm's experiment reaches (config.Experiment.Roles), nil for every
// issue role: a role it leaves out never starts on the arm's overlays, so the readers of every
// tail a role may start on pass the arm over for that role (Reaches, RoleOverlayValues).
type Variant struct {
	Experiment       string
	Name             string
	ConfigFiles      []string
	ConfigFilesField string
	MemoryBlock      bool
	Roles            []claim.Role
}

// Reaches is whether a launch of role may start on this arm's overlays: every issue role where
// the arm's experiment names no roles, and only the roles it names where it names some.
func (variant Variant) Reaches(role claim.Role) bool {
	return variant.Roles == nil || slices.Contains(variant.Roles, role)
}

// VariantOverlays are the Oh My Pi settings overlays of variants as one PI_CONFIG_FILES tail:
// each variant's own files in its own order, the variants in theirs, which for a launch's
// variants is the order the project's file declares the experiments. Both runtimes name it after
// the operator's overlays and the role's own, so a variant's file outranks both; a list of
// variants that names no file at all answers none, so a launch under a project whose declared
// variants name no overlay is told the value it was told before any experiment was declared.
func VariantOverlays(variants []Variant) []string {
	var files []string
	for _, variant := range variants {
		files = append(files, variant.ConfigFiles...)
	}
	return files
}

// RoleOverlayValue is one whole PI_CONFIG_FILES tail a role may start on, after the operator's
// own overlays: Files is the role's own overlays and then the overlays of one arm it may be
// drawn into, and Variant is that arm, nil where the value is the role's own files alone.
type RoleOverlayValue struct {
	Variant *Variant
	Files   []string
}

// RoleOverlayValues are every distinct PI_CONFIG_FILES tail a role starts on, in declared order:
// own, the role's own overlays (`role_config_files`), followed by the overlays of each of
// variants, the arms of every experiment the project declares, that names any — one value per
// arm, never one value holding two arms' files, since the union of two arms is a configuration
// no launch produces. An arm that names no overlay adds no value of its own, because its roles
// start on the role's own files, which the role's own value already is, and neither does an arm
// whose experiment leaves the role out of its `roles` (Variant.Reaches). A role no arm reaches
// answers that one value, which is the value every role was told before any experiment existed.
//
// The controller runs in no tree, so it is in no arm and answers its own files alone whatever
// the project declares. A value two arms produce alike is answered once, and a value holding no
// file at all is answered not at all: there is nothing for a reader of it to read.
//
// Both readers of a whole value ask this: the boot gate, which starts the image's Oh My Pi on
// each (internal/daemon's checkPaneRoleOverlays), and the image probe's manifest, which passes
// each to the probe pod (runtime/sandbox's probeManifest). What a launch is told is another
// question — it names the arms its own unit was drawn into, not every declared one — and
// VariantOverlays answers that one.
func RoleOverlayValues(role claim.Role, own []string, variants []Variant) []RoleOverlayValue {
	var values []RoleOverlayValue
	if role != claim.RoleController {
		for i, variant := range variants {
			if len(variant.ConfigFiles) > 0 && variant.Reaches(role) {
				values = append(values, RoleOverlayValue{Variant: &variants[i], Files: slices.Concat(own, variant.ConfigFiles)})
			}
		}
	}
	if len(values) == 0 {
		values = []RoleOverlayValue{{Files: own}}
	}
	read := make(map[string]struct{}, len(values))
	distinct := values[:0]
	for _, value := range values {
		joined := strings.Join(value.Files, ":")
		if _, done := read[joined]; done || len(value.Files) == 0 {
			continue
		}
		read[joined] = struct{}{}
		distinct = append(distinct, value)
	}
	return distinct
}

// PluginInstall is the plugin pair a Kubernetes launch installs before Oh My Pi starts, and the
// contract the installed pi-legion must declare. Each of PiLegion and PiEnvoy is the daemon's own
// resolution of `runtime.kubernetes.plugins` (an exact version, an absolute tarball path, or an
// http(s) URL the pod can read) or, when the file names none, the binary's linked pair — never an
// npm dist-tag or a range. Contract is this daemon's api.DaemonAPIVersion: the install's own check
// refuses to start the agent when the installed pi-legion declares another number.
type PluginInstall struct {
	PiLegion, PiEnvoy string
	Contract          int
}

// PromptParts are the pieces of the agent's system prompt, in the one order the runtime
// concatenates them into exactly one text (ComposePrompt), which the agent reads from the one
// file its one `--append-system-prompt` names — one, because a second occurrence of the flag
// replaces the first rather than adding to it: the role prompt files, the
// sentence that tells the agent which claim it is, the operator's deployment instructions, and
// the instructions of each variant the launch runs in. VariantInstructionsPaths are the daemon's
// own copies of those files, one per variant that names one, in the order the project's file
// declares the experiments (SpawnSpec.Variants); empty for every launch whose variants name none
// and for every launch in no experiment, whose prompt ends at the deployment's own.
type PromptParts struct {
	RolePromptPaths            []string
	Addressing                 string
	DeploymentInstructionsPath string
	VariantInstructionsPaths   []string
}

// ObservationKind is what a runtime can say about a process.
type ObservationKind string

const (
	// Alive: the process the locator recorded is running — its identity re-checked, not merely
	// its slot occupied.
	Alive ObservationKind = "alive"
	// Gone: the process the locator recorded is not running. Nothing is there.
	Gone ObservationKind = "gone"
	// NotRecordedProcess: something is there, and it is not the process the locator recorded —
	// a reissued pane id, a reissued pid, a locator that outlived its tmux server.
	NotRecordedProcess ObservationKind = "not_recorded_process"
	// Uncertain: the runtime could not learn anything that proves the process alive or gone. It
	// is a fourth verdict precisely so that it is never read as either: the supervisor re-arms
	// the probe and counts the streak, and changes no state on it.
	Uncertain ObservationKind = "uncertain"
	// StaleAddress: the process the locator recorded is running, but holds an address a process
	// launched now is not handed — one of the addresses a runtime hands every new process from the
	// daemon's configuration (the worker stream it dials, the daemon's API, NATS, Envoy, Dispatch,
	// the secrets broker) moved since this one was launched, as each does when the daemon restarts
	// with its key changed (the worker stream's: `advertise_host`, `bind` when no `advertise_host` is
	// set, `worker_stream_port`). Only the Sandbox runtime reports it, since a pod's argv and
	// environment are fixed at its launch. The detail names each address that moved. The
	// supervisor's response is its own (supervise's repoint). Distinct from a "stale" event (Handle's
	// own fence vocabulary, an observation of an incarnation the claim no longer holds): this one
	// names the claim's current, live incarnation.
	StaleAddress ObservationKind = "stale_address"
)

// Observation is one runtime fact about one process, with the moment it was observed and, for a
// verdict that needs one, the detail an operator reads.
type Observation struct {
	Locator Locator
	Kind    ObservationKind
	At      time.Time
	Detail  string
	// Displaced is a Gone or NotRecordedProcess of a process that ended through no fault of its
	// own. The Sandbox runtime reports it when the daemon itself replaced the process's pod while
	// the process ran, to launch another role there — a replacement of a live issue or controller
	// pod on another image or running as another ServiceAccount, one holding a worker stream,
	// secrets broker enrollment or session database URL key a pod created now is not handed, or one
	// whose role launcher Secret no longer binds it. The tmux runtime reports it, under
	// `runtime.tmux.take_over`, for a process another runtime recorded, which this daemon takes over
	// and cannot see. The process did nothing wrong, so the supervisor resumes its session at once,
	// charging nothing, rather than count a death. It is a field rather than a shape of Detail,
	// which is prose for an operator and nothing else.
	Displaced bool
	// OutOfMemory is a Gone whose pod's one container — the container every role of the issue runs
	// in — the kubelet ended for running out of memory. The end was the pod's, not the role's work:
	// the turn it cut off was stopped by the pod running out of memory, whatever that turn was
	// doing, and the work that filled the pod may have been another role's. The supervisor sends
	// the role's task again behind a note saying so. Only the Sandbox runtime reports it, since a
	// pod is the only thing the kubelet ends this way. It is a field rather than a shape of Detail
	// for the reason Displaced is: Detail is prose for an operator and nothing else.
	OutOfMemory bool
	// PodEnd is the pod's own reason for the end and the kubelet's message with it, as
	// `<reason>: <message>`, the reason alone where there is no message, and empty where the pod
	// carries no reason: `Evicted: The node was low on resource: ephemeral-storage. ...` is the
	// one that costs an issue its work. It is a field rather than a shape of Detail for the
	// reason Displaced is: Detail is prose for an operator and nothing else. The Detail the
	// Sandbox runtime writes beside a pod end holds this same reason and message, and after them
	// a tail of the role's log and the container's, over many lines and the role's own in words
	// a role chose: it reads a container's own log to its logTailBytes and the issue pod's one
	// container's, which every role writes to, to its sharedLogBytes, and quotes at most
	// logTailLines of the role's lines and as many of the container's own from what it read.
	//
	// An eviction ends every role of the pod, and a sibling's launch can replace the ended pod
	// before a role's next check reads it. Where that launch finds the Sandbox not yet
	// suspended, the Sandbox runtime records the ended pod's end on its Sandbox, keyed by the
	// pod's uid, in the write that suspends it, so a role recorded in that pod reads the same
	// PodEnd once the pod is gone or another has taken its name, a restarted daemon included
	// (sandbox's endedPodAnnotation), until the end of a later pod is recorded in its place: the
	// Sandbox holds one. A launch that finds the Sandbox already suspended records nothing, and a
	// Sandbox a migrating launch deletes and creates again does not carry the record over.
	//
	// Detail is kept whole on the claim, behind its verdict (supervise.Claim.LastDeathCause), and
	// reaches a notice two ways: whole, as the `death` of the failure record a claim that failed
	// with a cause carries (workflow's noticeFailure), and as its first line alone, quoted, cut
	// at PodEndRunes and without the header of the log tail, in the tree architect's worker-died
	// reason, which carries no failure record, for a claim that failed because its process died
	// and holds no pod end (workflow's deathReason). Where there is one, that reason names this
	// field in Detail's place: the pod's cause once, on one line, and nothing of the log.
	//
	// It is bounded at PodEndRunes, which is a kubelet message with room to spare: the eviction
	// message above is 120 runes, and the longest the kubelet writes names each resource it
	// reclaimed. A longer one is cut at that bound with an ellipsis.
	PodEnd string
}

// PodEndRunes bounds Observation.PodEnd: a few hundred runes, which is a kubelet's reason and
// message whole and nothing of a log. The bound exists because the field is what a worker-died's
// reason names whole, on the one line the architect reads (workflow's deathReason), beside the
// supervisor's sentence and, where it names one, the first line of the runtime's account of the
// death, each cut to this same bound before it is quoted (workflow's quoteBounded), so what the
// reason carries of each is bounded by this number rather than by the size a node's message or a
// provider's happened to have. The claim keeps the runtime's whole account beside it
// (supervise.Claim.LastDeathCause); this field is the part of it a reason carries.
const PodEndRunes = 400

// PodEnd is reason and message as Observation.PodEnd holds them, bounded at PodEndRunes: the
// reason alone when there is no message, "" when there is no reason, and a value cut at the
// bound with an ellipsis when the two together pass it. Both the runtime that observes a pod and
// any test that asserts on the field build the value here, so one rule decides what a claim and
// a notice carry.
func PodEnd(reason, message string) string {
	if reason == "" {
		return ""
	}
	end := reason
	if message != "" {
		end += ": " + message
	}
	runes := []rune(end)
	if len(runes) <= PodEndRunes {
		return end
	}
	return string(runes[:PodEndRunes-1]) + "…"
}

// GitIdentity is the author a worker's commits carry.
type GitIdentity struct {
	Name  string
	Email string
}

// Env is the six variables that make a process commit as id: JJ_USER/JJ_EMAIL, which jj reads
// over every config scope, and the Git author and committer pairs for plain git.
func (id GitIdentity) Env() map[string]string {
	return map[string]string{
		"JJ_USER": id.Name, "JJ_EMAIL": id.Email,
		"GIT_AUTHOR_NAME": id.Name, "GIT_AUTHOR_EMAIL": id.Email,
		"GIT_COMMITTER_NAME": id.Name, "GIT_COMMITTER_EMAIL": id.Email,
	}
}
