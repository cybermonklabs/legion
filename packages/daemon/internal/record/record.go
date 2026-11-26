// Package record owns the durable Postgres-backed facts of Legion's issue workflow.
package record

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// AttemptRun is the latest check-run id the daemon observed for one check name (ID), with the
// head GitHub reported that run on (Head) and what the run came to (Conclusion). Those two are
// facts of the run rather than of the settlement that carried it, and they are kept with the id
// because the fence is merged per check name over the settlements of the head it stands at
// (classify's mergeAttemptSets): each name keeps its highest run id, so the head and the
// conclusion must be the ones of that run and never a mixture of two runs'. What they answer is
// which names are red (classify.ApplySettlement derives Failing and Cancelled from them), which
// names are green on the pull request's current head (classify.GreenContexts), and whether GitHub
// has reported a check run on that head itself (classify.ChecksConcludedOnHead). A skipped run
// answers each of them exactly as a succeeded one does (RunConclusion says why).
//
// Both are optional in the stored JSON, so a row written before dispatch://INFRA-714 added them
// still parses, and such a row carries neither. It fails closed on both readings: an empty Head
// is no head, so the run is never one on the current head, and an empty Conclusion is no
// conclusion, so it never makes its name red. The names of such a row come to what the stored
// Failing and Cancelled lists say of them, which is what they came to before.
//
// GitHubConclusion is the word GitHub gave the run (success, neutral, skipped, timed_out, or one
// GitHub adds later), as the listener's settlement carried it, and only once intake's decoder
// took it for a word (decodeChecks). It decides nothing: every decision reads Conclusion, so two
// runs that differ only in their word decide alike. It is kept so the texts the roles read name a
// run by GitHub's own word rather than by the coarser conclusion. It is optional in the stored
// JSON too: a row written before dispatch://INFRA-4135 added it carries no word and reads as it
// did, a run named by its conclusion, and a daemon of the release before this one ignores it.
type AttemptRun struct {
	Name             string        `json:"name"`
	ID               int64         `json:"id"`
	Head             string        `json:"head,omitempty"`
	Conclusion       RunConclusion `json:"conclusion,omitempty"`
	GitHubConclusion string        `json:"githubConclusion,omitempty"`
}

// RunConclusion is what one check run concluded, as the settlement reporting it said: the
// listener names a run that failed and a run that was cancelled, and a run it reports as neither
// succeeded, since it settles a commit only once every check it has seen there is terminal
// (classify.ApplySettlement, which reads a candidate's Failing and Cancelled lists for it). A run
// the settlement's skipped group names is skipped: GitHub concluded it skipped, so it ran nothing
// on that head. Every decision takes a skipped run exactly as a succeeded one
// (dispatch://INFRA-4135 R2): neither makes its name red, each is a run on the head GitHub
// reported it on, and each makes its name green there (classify.GreenContexts), since GitHub's
// branch protection takes a skipped required check as passing. Only the texts the roles read name
// it differently, so no role is told that a check which never ran passed. "" is a run recorded
// before the conclusion was kept beside the id.
type RunConclusion string

const (
	RunSucceeded RunConclusion = "succeeded"
	RunSkipped   RunConclusion = "skipped"
	RunFailed    RunConclusion = "failed"
	RunCancelled RunConclusion = "cancelled"
)

// CommitStatus is one commit status a CI settlement named for the head it was of: GitHub's
// context, the state the newest terminal status of that context came to, spelled as GitHub
// spells it (CommitStatusSuccess, CommitStatusFailure, CommitStatusError), and GitHub's
// created_at of that status. A status is an entry of its own kind, apart from the check runs, so
// a context that shares its name with a check run is never that run. CreatedAt is what orders two
// statuses of one context (classify.ClassifySettlement, ApplySettlement): a status has no id, and
// the listener numbers the settlements of a record the bucket's TTL removed and wrote again
// afresh, so its generation does not order them. It is a plain field, since every status a
// settlement names carries one (intake's decodeChecks). Head is the head whose settlement named
// the status: the pull request keeps the statuses of its checked head (PullRequest.Statuses),
// and each text names only those stamped with the head it is about (workflow's settledChecks and
// headChecks), since a daemon of an earlier release writes checked_head after a rollback and
// leaves this list as it was. A settlement's own list (Settlement.Statuses) carries no stamp: the
// settlement names its head, and the stamp is put on where the list is kept
// (classify.ApplySettlement).
type CommitStatus struct {
	Context   string            `json:"context"`
	State     CommitStatusState `json:"state"`
	CreatedAt time.Time         `json:"created_at"`
	Head      string            `json:"head,omitempty"`
}

// CommitStatusState is the state a settlement names a commit status in. GitHub's other states,
// pending and expected, are never recorded, so no settlement names one.
type CommitStatusState string

const (
	CommitStatusSuccess CommitStatusState = "success"
	CommitStatusFailure CommitStatusState = "failure"
	CommitStatusError   CommitStatusState = "error"
)

// Failed is whether the status is a failed one: in failure or in error.
func (s CommitStatus) Failed() bool {
	return s.State == CommitStatusFailure || s.State == CommitStatusError
}

// StampedWith is whether the status is one the pull request keeps for head: it carries head's
// stamp (PullRequest.Statuses). A status with no stamp, as a settlement names it, is kept for no
// head. It is the one place that test is written, and the classification of a settlement
// (classify's statusNews), the merge of its statuses (mergedStatuses) and each text that names
// them (workflow's keptStatuses) read it, so none of them can drift from another.
func (s CommitStatus) StampedWith(head string) bool {
	return s.Head != "" && s.Head == head
}

// ClassifiedPush is one branch push as the daemon records it: a fact about two commits, the head it
// replaced (Before) and the head it left (SHA), true whenever it is learned, and whose push it was.
type ClassifiedPush struct {
	SHA string `json:"sha"`
	// Before is the head the push replaced, empty when the push did not say.
	Before string `json:"before,omitempty"`
	// ByReviewApp is whether the push was the review App's (its pusher is the review App's bot login).
	ByReviewApp bool `json:"byReviewApp,omitempty"`
	// Sender is the login of the account that pushed (the webhook's sender), empty from a listener
	// that predates the field. A synchronize names its move's push by it (classify.pushOf).
	Sender string `json:"sender,omitempty"`
}

// NoCommit is the commit GitHub names for a ref that does not exist: a push webhook's `after`
// when the push deleted the branch, and its `before` when the push created it.
const NoCommit = "0000000000000000000000000000000000000000"

// Issue is one durable workflow record. Status is the issue's lifecycle status as the daemon holds
// it: set when the daemon queues a status write, and set again from each Dispatch event or boot
// listing that changes the status Dispatch shows, the daemon's own echoes included, except a
// session's write the daemon sets back (sessionStatusWrite), which leaves it. While a chain of
// writes is queued, it can briefly hold an earlier write's echo.
type Issue struct {
	Key             string
	Tree            string
	Project         string
	Title           string
	Parent          *string
	Phase           phase.Phase
	Generation      uint64
	Status          string
	Rank            string
	LingerUntil     *time.Time
	Hold            *Hold
	LastDispatchSeq int64
	// HandedOver is whether the issue carried LegionLabel when Dispatch last showed it: what hands
	// a root, or an orphan admitted as one, to Legion. A child running under its tree needs none.
	HandedOver bool
	// TakenBack is whether the issue's run has stood in a phase of its own — one that carries a run
	// (CarriesRun's rule: neither phase.Admitted nor phase.Done) — as part of the tree it now roots or
	// belongs to. It is a fact about the run, not about the issue's history. Promotion reads it
	// because the spec exempts a tree Legion takes back after it finished and lingered: both that the
	// issue ran and that the tree coming back is its own, since re-admission leaves such a root at
	// phase.Admitted carrying no run, the same shape as a waiting root that only left the line and
	// returned, or a child handed over from a finished tree as a root of its own, both of which are
	// checked (blockers.Exempt).
	//
	// The workflow's transition writes the mark as the issue moves into such a phase
	// (workflow.Engine.transition), the only way an issue first leaves phase.Admitted for one. It
	// lasts through that phase's end and its root's leave into a linger, and re-admission keeps it
	// when the root's own tree comes back, the work that earned it. Every other path that opens a run
	// starts the run clean: a child's re-entry into its tree records its new generation without it
	// (workflow.Engine.enterChild), and re-admission clears it when another tree's work reaches a root
	// of its own without its run (admit.Admission.readmit), what that child ran being its old tree's.
	// An orphan that carries its run keeps the mark with it, the run being its new tree's from then
	// on. So no issue is exempt from the blocker check unless the run it is exempt for is the run it
	// arrived with. Nothing else writes it.
	TakenBack bool
	// DispatchStatus is the status Dispatch showed at the newest event or boot listing applied to the
	// record (LastDispatchSeq's), whatever the daemon has queued since: every event carries the whole
	// issue, so only an event whose status differs from it writes a status.
	DispatchStatus string
	// Priority is the issue's own Dispatch priority (P0 to P3, or none) as the newest Dispatch
	// observation applied to the record showed it: a live event, the boot listing or a detail
	// read, each of which carries it, written wherever the record takes that observation's rank.
	// It is the issue's own and never its tree root's: the root's is read from the root's record
	// where it is wanted (PriorityOf).
	Priority Priority
	// Repo is the GitHub repository (owner/name) the issue targets, and DefaultBranch that
	// repository's default branch: Dispatch's resolved repository and GitHub's default branch when
	// the daemon first recorded the issue (a root's admission, a child's recording under its tree, an
	// orphan's admission). Neither changes after that: a carried child, a re-admission and a
	// hand-over keep them, and a later edit of the issue's repository in Dispatch is not applied.
	// This release makes one exception, once: its rename pass writes a configured rename's new
	// name, in GitHub's spelling, over its old one on Repo and GitHubRepo once GitHub shows the
	// transfer, while no pod of the issue is live, under tmux while no workspace directory of it
	// is under the old name (RenameIssueRepository), and dispatch://INFRA-3532's own change removes
	// that pass.
	// Every repository-dependent action reads them. Repo is spelled as the issue's workspace and
	// shared clone are, which can be a clone's spelling that differs from GitHub's in case.
	Repo          string
	DefaultBranch string
	// GitHubRepo is GitHub's own spelling of Repo, taken with it and changed only with it, by this
	// release's rename pass (RenameIssueRepository): what Envoy's event subjects carry, which NATS
	// matches case-sensitively, so GitHub intake hears the issue's repository by it, read from the
	// record at boot. Under a configured repository rename (`repository_renames`) whose two names
	// are Repo and the name GitHub holds the repository under, it is the pair's spelling of Repo's
	// own side instead, which GitHub publishes nothing under after the transfer: the record's two
	// repository columns must name one repository, and a record keeps the name it was recorded
	// under until the rename pass writes the pair's new name, in GitHub's spelling, on both.
	// Intake still hears the issue, because the daemon subscribes to both names of a pair
	// (heardRepositories), and every GitHub call the daemon makes for the record addresses the
	// repository GitHub serves it under now (ghrepo.Renames.Serving) rather than this field.
	GitHubRepo string
	// Ancestry is the keys of a root's Dispatch parent chain, from its parent upward to the top,
	// stopping only at a repeated key or back at the root, as the newest Dispatch observation
	// applied to the record was prepared with (intake's DispatchIssue.Ancestry), or as the
	// preparation of an event of an issue on that chain read it again (intake's
	// DispatchIssue.Reread): kept for a labelled todo root under a parent unrecorded or recorded as
	// a root, such as a child of a live tree that tree's architect has not released. Such a root
	// is held under that unreleased child (the ancestry hold) and waits for no slot while any live
	// root is on its chain (Waiting), exactly while that root's release would reach it; it is not
	// the hold on Dispatch blocked-by links (blockers.WaitsOn). Empty for every other record, for a
	// root that ran as a tree of its own (workflow's RanAsRoot), which no release adopts, and
	// cleared when adoption takes the record into a tree.
	Ancestry []string
	// HandedOverFrom is the key of the tree the issue left when it became a root of its own, while
	// the tree of its own has still to be opened: re-admission (admit.Admission.readmit) writes it
	// whenever it moves a member of another tree into a tree of its own, a child its finished tree
	// handed over (carrying its run or not) or an orphan a person admitted, and the promotion that
	// opens that tree clears it (admit's promoteHolds), because the run the record carries is this
	// tree's own from then on and the waiting line reads the mark as a tree still to be opened
	// (QueuedRoot, ran). Such a root stays its own tree whether or not the mark is still set
	// (workflow's RanAsRoot, which reads the opened lifecycle too): no admission sweep or release
	// of children adopts it, into the tree it left or any other, and no ancestry hold holds it.
	// Empty for every other record.
	HandedOverFrom string
	// ToldParent is the Dispatch parent the issue was last told it stays its own tree under: a root
	// that ran as a tree of its own, which no release of children and no admission sweep adopts
	// (workflow's RanAsRoot), that a person has made a child of a live tree. The telling is one
	// message on the issue and one notice to that tree's architect, sent once per parent value
	// (workflow's tellReparented), so every later event under the same parent repeats nothing while
	// a move under another live tree is told anew. Empty for every other record and for one never
	// told.
	ToldParent string
	// Awaiting is what a root in phase awaiting_person waits on, nil in every other phase: the two
	// change together, as Hold and held do.
	Awaiting *Awaiting
	// HandoutSeq is the highest Dispatch sequence of the issue the record held when the daemon last
	// gave the root's architect work (Store.StartHandout, Store.TakeInThrough), 0 for never. It is
	// never lowered, and only those two write it: PutIssue keeps the stored value.
	HandoutSeq int64
	// DeclaredSeq is the newest Dispatch sequence of a hand-back the architect made to a person
	// since its last handout (Store.Declare), 0 when it made none or when nothing it handed back is
	// still with the person (Store.ClearDeclared), and DeclaredAt Dispatch's time of that hand-back
	// (zero when its event gave none). Only Declare, ClearDeclared, StartHandout and TakeInThrough
	// write them: PutIssue keeps the stored values.
	DeclaredSeq int64
	DeclaredAt  time.Time
	// ReleasedForWaitAt is when the daemon took the issue's admission slot away because the issue
	// waits on another issue — a Dispatch blocker of it that is not done, or an open child its own
	// first phase waits for — and nil for every other record. It is the mark that stands an issue
	// whose row was taken away for a wait in the waiting line at the moment it lost that row
	// (Waiting). The row and the mark are written together, so no pass reads a mark beside a row:
	// only Store.ReleaseSlotForWait writes it, with the delete of the row in one statement. These
	// writes clear it (Store.ClearReleasedForWait, Store.ReleaseTreeSlots), and this is the one
	// list of them: the grant of a slot, the ordinary release of a row for good, the issue's
	// departure from the workflow, which the release's own read of the live issues no longer
	// reaches (workflow's leave), and the re-admission of its tree's root, for every issue of the
	// tree (admit's releaseTreeSlots). PutIssue keeps the stored value.
	ReleasedForWaitAt *time.Time
	// MovedAt is when the daemon last saw the issue move: the time of the newest fact it recorded
	// that meant the issue's own record changed — a Dispatch event of the issue, a GitHub event of
	// its pull request, a phase change, a recorded handoff, a start or a completion (the write
	// sites in internal/workflow). It is never zero: the column that carries it is not null, and
	// the issue is stamped at the moment it is first recorded.
	//
	// Only Store.MarkIssueMoved writes it, and it writes it forwards alone, so a fact delivered
	// late moves nothing backwards. PutIssue keeps the stored value, as it does for handout_seq
	// and the declared pair: a copy of the issue read before a movement would
	// otherwise write that movement away, and every one of the write sites holds a copy read
	// before its own fact.
	MovedAt time.Time
	// TreeStalledToldAt is the movement the daemon has already told this tree's controller the
	// tree stopped for: the newest movement of anything in the tree (TreeQuiet.MovedAt) at the
	// moment the stalled-tree notice went into the outbox, nil for a tree no stall of which has
	// been told. It is written on the tree's root alone, since the stall is the tree's and the
	// notice names its root, and a child's row never carries one.
	//
	// It is what holds the daemon to one notice per stall: the rule tells a quiet tree only
	// where its newest movement stands after this mark, so a tree that has stopped is told once
	// however many passes read it, and a tree that moves and stops again is told anew
	// (admit's tellStalledTrees). Only Store.MarkTreeStalledTold writes it, in the transaction
	// that queues the notice, and PutIssue keeps the stored value as it does for moved_at: every
	// pass that records an issue holds a copy read before its own telling.
	TreeStalledToldAt *time.Time
	// MembersOwed says that a re-admission started this root's generation and its tree has not yet
	// run in it, so the members that re-admission left mid-phase, and the members reopened at done,
	// are still to be brought back (dispatch://INFRA-5471). admit's readmit sets it. It is cleared
	// in three places. Two bring the members back in the same transaction (admit's
	// returnOwedMembers): the root's promotion from the waiting line, and the first admission pass
	// that finds its tree running before any promotion while the root is still handed to Legion
	// and its design gate has opened (admit's returnMembersOfRunningTrees, MembersReturnOwed). The
	// third is that same pass meeting a root a daemon of an earlier release promoted, which brought
	// the members back and never names the column (admit's promotedByEarlierRelease). It clears
	// that mark and starts nothing. A root whose label a person takes off while it waits stops
	// being a queued root and keeps the mark until the label returns. While the mark stands, a
	// tree whose lifecycle is not open under the workflow keeps
	// its root queued whatever the gate says (ran).
	// It is false on a child's row and on every root no re-admission started. Only Store.OweMembers
	// and Store.ClearMembersOwed write it, and PutIssue keeps the stored value, as it does for
	// released_for_wait_at: every write site that records the root holds a copy read before its own
	// fact, and a copy read before the clear would otherwise owe the members a second return.
	MembersOwed bool
	// StepRole is the role of the step at the place of the issue's task, running or stopped: read
	// with the issue from its run's plan rows (plan_tasks and plan_steps at the issue's
	// generation), "" where the run has no plan rows or its task stands at no step. It is read
	// only: PutIssue writes nothing of it, and workflow's moveTask sets it on the issue it moves.
	StepRole claim.Role
}

// Repository is the repository the record names (Repo) in the spelling the record keeps: what
// every repository-dependent action of the issue works and mints for its owner.
func (i Issue) Repository() (ghrepo.Repository, error) {
	return ghrepo.Parse("the repository of "+i.Key, i.Repo)
}

// Assignment is what one unit ran one experiment under: the arm drawn for it, the mode in force
// when it was drawn and the instant, and how the issue drawn is done where the arm says so (Plan),
// recorded once for that unit and never changed
// (Store.PutAssignments). It is the declared experiments' record, and the only one: the four
// issues.memory_* columns the single hard-coded experiment was kept in are written by nothing
// from this release on, are read by nothing this release's daemon runs but the back-fill of
// 20261111045636_experiment_assignments, and by the copy by hand an operator runs on a roll
// forward (docs/kubernetes.md), and Issue carries none of them. A project declares as many
// experiments as it likes (config.Project.ExperimentsDeclared), so each answer is a row named by
// its experiment rather than a column named after it.
//
// The unit the row belongs to is the issue it is keyed by, read as that issue's tree where Unit is
// `tree` — the key is then the tree's root — and as that issue alone where Unit is `issue`.
type Assignment struct {
	// Experiment is the name the project declares the experiment under (config.Experiment.Name).
	Experiment string
	// Unit is what the draw covered, a config.ExperimentUnit word: `tree` or `issue`. It is taken
	// as a string because internal/config imports this package itself (the design gate's steps are
	// record.DesignStep), so an import of config here would close a cycle.
	Unit string
	// Variant is the arm drawn (config.Variant.Name), "" for no variant: an experiment at mode
	// off, which gives every new unit no arm, and the answer a hand-over settles a child at for
	// an experiment the tree it left held no row for.
	Variant string
	// Mode is the experiment's mode when the draw was made (a config.ExperimentMode word), "" where
	// a hand-over settled the row and the old tree held none, so no draw was ever made under any
	// mode.
	Mode string
	// DrawnAt is when the draw was made, nil where a hand-over settled the row with no draw.
	DrawnAt *time.Time
	// Carried marks a row a hand-over wrote rather than a draw: the answer this unit took from the
	// tree it left, whether that answer was an arm or no arm at all (Store.CarryAssignments).
	Carried bool
	// Plan is how the issue drawn is done, a config.PlanGroup word taken as a string for the
	// reason Unit is: `owner` or `default`, the `plan` of the arm drawn, and "" where the arm
	// names none, the draw was made under mode off, a hand-over wrote the row, or the row was
	// drawn before the column existed. The loader refuses `owner-alone`, so no draw writes it. An
	// issue keeps it for its whole life, whatever the project's file says later
	// (internal/experiment's GroupOf reads it).
	Plan string
}

// DeclaredExperiment is one experiment a hand-over settles a child at: the name the project
// declares it under and the unit it covers, which together are the whole of what the settling
// write records (Store.CarryAssignments). A settled row carries no arm, no mode and no instant
// by construction, so there is no field here to pass one in.
type DeclaredExperiment struct {
	Experiment string
	Unit       string
}

// HandedOff is an issue whose implementer recorded its handoff, with the commit the handoff
// recorded (PhaseRow.HandoffCommit).
type HandedOff struct {
	Issue  Issue
	Commit string
}

// RepoNote is what the daemon last said on a Dispatch issue about its repository: the resolved
// repository the issue showed then ("" for none) and the note posted, so an event that resolves the
// same way posts nothing again.
type RepoNote struct {
	Issue        string
	ResolvedRepo string
	Note         string
	PostedAt     time.Time
}

// Hold is a held issue's hold: the phase it left, and why it is held when the hold has a reason.
// An issue whose phase is not held has none, so ending a hold (Issue.Hold = nil) ends its reason.
type Hold struct {
	From   phase.Phase
	Reason HoldReason
}

// HoldReason is why a held issue is held.
type HoldReason string

// HoldEscalated is a hold its architect sent to the controller.
const HoldEscalated HoldReason = "escalated"

// HoldTaskStopped is a hold whose issue's owner stopped the task it ran (workflow's taskStopped):
// the task keeps its place and the reason it was stopped for (PlanTask.StoppedReason, StopTask).
// A later version of the plan that changes the task, or a person's todo on a root, ends the hold:
// a task whose work ran starts again at its first step, and a task stopped before its first step
// (TaskPlace.Started) waits for that step again, as a task never stopped does.
const HoldTaskStopped HoldReason = "task-stopped"

// HoldForChildren is a hold that stands in place of the retro of an issue whose task's last step
// is not the owner's acceptance, while issues beneath it are still open (From is phase.Retro,
// which holds no slot): the retro starts once the last of them has left.
const HoldForChildren HoldReason = "children"

// Awaiting is what a root in awaiting_person waits on: an ask its architect opened on the root
// (Ask, the ask's id), or the closed design gate at a spec version (GateVersion) on the document
// it registered (Artifact); when a person answered it (AnsweredAt, nil until then), with what
// (AnsweredBy) and that act's own detail (AnsweredDetail: the ask acted on, who closed it, or the
// version and reason of the review); and where on Dispatch the wait began (Since): the newest
// hand-back the architect made since it was last given work (Issue.DeclaredSeq). It is a snapshot
// of the wait's subject; what else is still with the person is the root's asks (RootAsk). A
// person's act Dispatch recorded at or before Since never answers the wait. An answered root
// stands in the line's returned part (Waiting) and resumes on the slot it holds.
type Awaiting struct {
	Ask            string
	GateVersion    int
	Artifact       string
	AnsweredAt     *time.Time
	AnsweredBy     Answer
	AnsweredDetail string
	Since          EventMark
}

// Answer is the act that answered a root's wait, which the task resuming its architect names.
type Answer string

const (
	// AnsweredByAsk is a person's answer, resolution or reply on the ask the root waits on.
	AnsweredByAsk Answer = "ask"
	// AnsweredByRetraction is Dispatch's settlement retracting the last ask of the root's wait still
	// with the person, whose decision block left the spec: no person can answer it any more.
	AnsweredByRetraction Answer = "retracted"
	// AnsweredByClosed is a close, by anyone who is not a person, of the last ask of the root's wait
	// still with the person: the question is gone, so nobody will answer it.
	AnsweredByClosed Answer = "closed"
	// AnsweredByReply is a person's act on another ask of the root.
	AnsweredByReply Answer = "reply"
	// AnsweredByTurn is a session handing the last question of the root's wait still with the person
	// to the agent's turn: a progress note leaves no question with the person, so the architect
	// looks again.
	AnsweredByTurn Answer = "turn"
	// AnsweredByApproval is the root's design gate standing open: a person's approval of the
	// root's spec, or a registration under gates.design: off, where nobody approved it. A wait on
	// a closed gate that gates.design: off answered (workflow's leaveGateOff) is recorded as this
	// too, since the API's answeredBy has no value of its own for it; its AnsweredDetail marks it,
	// so the resume never says a person approved the spec.
	AnsweredByApproval Answer = "approved"
	// AnsweredByChanges is a person's changes request on the root's spec, which the gate records
	// as told with its reason.
	AnsweredByChanges Answer = "changes_requested"
	// AnsweredByVersion is a spec version newer than the one waited on, which hands the approval
	// request back to the architect.
	AnsweredByVersion Answer = "version"
	// AnsweredByDecline is a person's decline to answer an ask of the root, the one the wait
	// names or any other: they closed the question with no answer, so nobody will answer it, and
	// the detail is that ask. The resume prints what Dispatch wrote for the asking agent and the
	// person's own note, kept on the ask's mirror row (RootAsk.Decline).
	AnsweredByDecline Answer = "declined"
)

// ParkedDecision is a person's decision on a tree nobody in it could hear, which the daemon
// relayed to the controller (a parked-decision notice) or kept for a root that waits for its slot
// (RelayParkedDecision). It is kept under the issue it was decided on, never under a root, so the
// tree it is owed to is read from the record whenever a start or a catch-up names it: an issue
// that changes trees takes its decisions with it with nothing to move. It stands until the one
// start task or catch-up that claimed it (ClaimedBy, the outbox row whose payload names it:
// workflow.Engine.ParkedDecisionTask names only rows nothing counting claims or holds) has told
// an architect of its issue's tree, and is deleted then — by that catch-up's publish
// (Store.ForgetKeptDecisions) or by the confirmation of the delivery that holds it (HeldBy). A
// claiming row that reached no architect hands it over instead (workflow.HandOverKept): it goes
// back to KEPT, and a fresh catch-up names it at once while its tree has an architect to tell, so
// a release never leaves it kept with nobody to name it again.
type ParkedDecision struct {
	// Issue is where in Dispatch the decision was made: the issue of the ask, comment or spec it
	// was made on. It is the row's key with Ref, and what says whose debt the row is: the
	// architect of the tree this issue stands in now owes the person the answer, whatever tree it
	// stood in when the decision was made. It is empty for no row the daemon writes, since every
	// path that keeps one knows where it was made (RelayParkedDecision).
	Issue    string
	Ref      string
	Decision DecisionKind
	Actor    string
	At       time.Time
	// ClaimedBy is the outbox row claiming it, 0 when no row does; HeldBy is the claim and the
	// delivery id of the queued start task holding it, the zero value when none does.
	ClaimedBy int64
	HeldBy    DeliveryHold
}

// DeliveryHold is the claim token and delivery id of a start's task queued on an architect's
// claim, which holds what that task names until that delivery is confirmed (the rows are told),
// retired (the hold is cleared and the rows are KEPT again) or taken back under a new id (the
// hold moves to it). The zero value is no hold.
type DeliveryHold struct {
	Claim    string
	Delivery string
}

// AwaitingPerson says whether the issue is a root waiting on a person (phase awaiting_person).
func (i Issue) AwaitingPerson() bool { return i.Phase == phase.AwaitingPerson && i.Awaiting != nil }

// Answered says whether the issue is a root a person's answer released from awaiting_person, which
// waits in the line for the resume of its architect, on the row it held through the wait
// (KeepsSlot).
func (i Issue) Answered() bool { return i.AwaitingPerson() && i.Awaiting.AnsweredAt != nil }

// WorkingPhase is the phase the issue's worker works: the phase its hold left while it is held —
// the one whose worker's claim failed — and the phase it stands in otherwise. It is the one rule:
// the role that works the issue at it (RoleAt: workflow.RoleFor of it, or at step the role of the
// running step) is the role whose failure holds the issue, the role the stranded report names,
// and the role the operator's retry of a phase worker acts for (api's retryClaim), which read it
// here rather than each keeping a copy to drift from.
func (i Issue) WorkingPhase() phase.Phase {
	if i.Hold != nil {
		return i.Hold.From
	}
	return i.Phase
}

// RoleAt is the role that works the issue at p: the role of the step its task stands at
// (StepRole) for phase.Step, whose role the plan names, and phase.RoleFor(p) for every other
// phase.
func (i Issue) RoleAt(p phase.Phase) claim.Role {
	if p == phase.Step {
		return i.StepRole
	}
	return phase.RoleFor(p)
}

// StepIsOf says whether the step the issue stands at is role's to report on, where the phase alone
// cannot say: at phase.Step, every role the deployment defines runs as the one phase, so only the
// role of the running step (StepRole) works it, and a report or a task of another such role is of
// a step the task has left or not reached. Every other phase is one role's, which the caller reads
// from the phase, so it answers true there. It is the one statement of that rule: the engine's
// judgement of a completion (workflow's handoff), the completion route's fallback to a held task
// (api's heldTaskNotCurrent) and the judges of queued work (HoldsQueued) all read it.
func (i Issue) StepIsOf(role claim.Role) bool {
	return i.Phase != phase.Step || role == i.StepRole
}

// EventMark places a Dispatch event in its issue's event log: its sequence on the issue (Seq) and
// Dispatch's time of it (At), each zero when unknown. Dispatch appends an issue's events under the
// issue's row lock, so their sequence is the order they commit in. The time is the start of the
// transaction that wrote the event, so an event can carry a time before that of an event that
// committed ahead of it: only the sequence orders two events for certain.
type EventMark struct {
	Seq int64
	At  time.Time
}

// After says whether m comes after start: by sequence when both carry one, otherwise by time when
// both carry one, strictly, so an event at start's own time is not after it. An m with no sequence
// against a start with one is ordered by their times when both carry a time: such an m comes from a
// read of a Dispatch that does not give the sequence (one older than its migration 0118, which a
// daemon deployed first, a rolling update or a Dispatch rollback can meet), even for a review whose
// row has one, so the times are what orders the two, as before Dispatch kept the sequence. That
// fallback is as blind as it was then: to two transactions that began at one instant, and to one
// that began before the other and committed after it. A start with a sequence and no time is a
// registration's read (its Since), which has no time of its own to compare with, so an m without a
// sequence is never after it. When neither is known on both sides, which comes first cannot be
// told, and m counts as after start, as it did before marks were recorded.
func (m EventMark) After(start EventMark) bool {
	switch {
	case m.Seq > 0 && start.Seq > 0:
		return m.Seq > start.Seq
	case !m.At.IsZero() && !start.At.IsZero():
		return m.At.After(start.At)
	case start.Seq > 0:
		return false
	}
	return true
}

// Same says whether m and other can mark the same event: equal sequences when both carry one,
// otherwise equal times when both carry one, otherwise true. A review that a read of Dispatch shows
// with no sequence (one Dispatch recorded before it kept the sequence, or a Dispatch that predates
// it) is still recognised by its time as the one its live event carried.
func (m EventMark) Same(other EventMark) bool {
	switch {
	case m.Seq > 0 && other.Seq > 0:
		return m.Seq == other.Seq
	case !m.At.IsZero() && !other.At.IsZero():
		return m.At.Equal(other.At)
	}
	return true
}

// Merged is m with each part it lacks taken from other. Two reads of one review (Same), one of
// which gave less of its mark, are kept as the fuller one, so a sequence the daemon once knew of a
// review is never lost to a read that did not give it.
func (m EventMark) Merged(other EventMark) EventMark {
	if m.Seq == 0 {
		m.Seq = other.Seq
	}
	if m.At.IsZero() {
		m.At = other.At
	}
	return m
}

// ID is the mark as one number for a fact's event id: the sequence, else the time in Unix
// nanoseconds, else 0.
func (m EventMark) ID() int64 {
	switch {
	case m.Seq > 0:
		return m.Seq
	case !m.At.IsZero():
		return m.At.UnixNano()
	}
	return 0
}

// PhaseRow is the durable part of a role's work on an issue; live claim facts are joined for state.
type PhaseRow struct {
	Issue string
	Role  claim.Role
	Claim claim.Token
	// HandoffCommit is the commit the role's completion of its current phase reported: for the
	// planner, the implementer's implementing round, the tester and the reviewer the head their
	// handoff record names, the commit their work was done against, which they report beside that
	// record and which the implementer's plugin holds to be its pushed @- on
	// legion/<issue>@origin; and the commit retro and merging stand on.
	HandoffCommit string
	// Pass counts the moves the issue has made back to an earlier step of its task (by the task's
	// order, workflow's Walk.MovesBack), each of which runs the task's steps again from the step it
	// went back to. It tells one pass through a phase from the last, which is what the handoff
	// completion key is built on (internal/api's handoffPosition), and so it is never reset while
	// the record of the work stands: a completion at a commit an earlier pass already reported has
	// to be a fact of its own.
	Pass int
	// HandoffRecord and HandoffVersion are the Dispatch handoff record the role's completion of its
	// current phase named, for a phase with a handoff (phase.Handoff), and that record's version
	// among its phase's records: the next worker reads that version with `dispatch handoff-read
	// --version <n>`, as its task's Handoff line says. Empty and zero for a phase that writes none.
	HandoffRecord  string
	HandoffVersion int
	// Rounds is how many review rounds the issue has had while this role held it, counted on the
	// implementer's row alone (workflow's recordRound). A review that requested changes ends one,
	// and so does the fail of a step of the type review whose agent is not the owner. Nothing else
	// opens one — a tester's fail, the fail of a step of another type and a worker's backward move
	// each raise Pass. They leave this count where it stands. What review_round_cap bounds is
	// Rounds less ConflictRounds.
	Rounds int
	// ConflictRounds is how many of Rounds were opened while the daemon's record held the pull
	// request's own head as conflicting with its base (PullRequest.ConflictingHead): a round the
	// implementer answers with a forward merge, which no reviewer asked for. review_round_cap
	// compares Rounds - ConflictRounds with the cap, so such a round costs the issue nothing
	// (workflow's recordRound).
	ConflictRounds int
	// BackwardMoves is how many times the issue moved back by each kind of move, keyed
	// "<from>-<to>" as phase.MovePairs() writes the pairs: "testing-implementing",
	// "reviewing-implementing", "merging-implementing" and the rest. It is counted on the
	// implementer's row alone, beside Pass and in the same place (workflow's recordPass), and the
	// two count the same moves to different ends: Pass rises on every move back, because a phase
	// completion's idempotency key is built from it (api's handoffPosition), while this
	// map says what kind each move was, which is what a hook reads. A conflict round is the one
	// move the two part company on: it raises Pass, and Rounds and ConflictRounds where a review
	// opened the round, and no pair at all, since nobody sent the work back, so no hook fires
	// for it.
	// The map starts empty when the migration lands and is never filled in from the past, so an
	// issue already in flight reads counts that add up to less than its Pass.
	BackwardMoves map[string]int
	// CapTold is the review_round_cap value this row last posted the cap's message and its
	// pr-blocked notice for, 0 for a row that has posted none. It is what makes that telling go
	// once for each value of the cap rather than on every round after the first that reaches it:
	// the round that tells writes the cap it read here, and a later round compares the two.
	// A new generation clears it (ClearGeneration), so a tree sent back to the start tells again
	// from its own first crossing.
	CapTold int
	Verdict string
	// Summary is what the role reported with its completion, which the architect is told when the
	// phase ends.
	Summary string
	// LastHandoff is the handoff record the role last named for a file-backed phase (a commit, on a
	// row a daemon before the records last wrote, which no record id equals). Unlike HandoffRecord
	// it survives the next phase's start, so a completion naming it again is known to carry no
	// handoff written since.
	LastHandoff string
	// Decision is the review round's decision, on the reviewer's row: the newest review the
	// reviewer's own review-App session (told from any other review-App session's by its Legion
	// footer) or an account with write access to the repository submitted for the round that carried
	// one (workflow's DecidesRound), kept until the round ends, since the reviewer's completion can
	// come after the review it posted. Nil until a review decides.
	Decision *ReviewDecision
	// ReviewedHead is the commit the daemon last recorded as read by the reviewer's own deciding
	// review of the entry that stood: the head named in the Legion footer of a review that
	// approved or requested changes, submitted as the review App from the session the claims
	// record holds for the claim the daemon starts the reviewer under (workflow's review,
	// byReviewer and reviewersClaim), in the first round as in every later one. Only the
	// reviewer's row holds it, beside the row of a role of the deployment whose step is of the type
	// review, which holds the commit its step's newest run read (workflow's recordStepReview). A person's review does not move it, since the reviewer did not read
	// that commit; neither does another review-App session's review or a review that decides
	// nothing, and while the reviewer's claim records no session no review-App review decides the
	// round or moves it (workflow's RefusedWhileNoSession). Unlike Decision it outlives its round:
	// no entry into a phase clears it (workflow's clearHandoff, whose list of fields does not hold
	// it), so the task of the reviewer's next round names it (workflow's namingTheReviewRound) and
	// the reviewer reads the difference from it to the head. Only a review of the entry that stands
	// moves it: the row holds the task of an entry (EntryTask), and the review's footer names that
	// entry (Entry, intake.PullRequestReview.LegionEntry). No time is compared. A review that names
	// no entry, or another one, decides its round and leaves the commit where it stands, so the
	// next round reads more: from an older commit, or the whole change. A new generation clears it
	// (ClearGeneration), and the clear empties the entry's task until the next start raises an
	// entry, so the first review of a re-admitted root, of a child run again and of an adopted
	// member reads the whole change, and a review of an entry before the clear moves nothing,
	// however late it is applied; a carried hand-over keeps its rows, and the commit with them.
	// Empty until such a review, and on every row the release before the column wrote.
	ReviewedHead string
	// CompletedAt is when the workflow applied the role's completion of its current phase, zero until
	// then. The reviewer's orders the reviews its round receives against the completion (workflow's
	// reviewersAnswer and answerSkew), and is read only for a completed round (Completed). It can
	// be stale: a daemon older than migration 0026 writes a completion without it, and
	// ClearGeneration resets the row without it. Behind a completion an old daemon wrote, a stale time
	// at worst makes a review read as the reviewer's answer: one extra review-stuck notice.
	CompletedAt time.Time
	// Entry is the number of the phase entry this role's row is on: a transition's start of the
	// phase the issue has moved into, the start of a mid-phase worker a re-admitted tree's first run
	// brings back, and a root architect's start of work it was not on raise it, to one above the
	// highest entry of every phases row of the issue (workflow's enter), and write the three fields
	// below beside it, in their own transaction
	// (workflow's start, StartPromotedWorker, StartArchitect and ResumeAwaitedRoot). It is the
	// finer of the two numbers a completion is held to: the issue's generation is the same for
	// every entry of one run, where the entry is new at each one, which is what tells one testing
	// phase from the testing phase after a move back (api/handoff.go). A start that hands
	// the role no work it was not already on leaves it where it stands — the send of this entry's
	// own work to a claim behind it, the held phase's retry, and every start of no entry
	// (SuperviseRequest.Entry, which lists them) — and unlike HandoffCommit it is never reset:
	// ClearGeneration leaves the entry and clears the task below. Zero on a row no entry has been
	// raised on: a role the issue's phases have not reached, and every row of a daemon upgraded
	// in place that the upgrade found with no work handed out, which the completion route reads
	// as nothing to compare. The reviewer's row has a second reader: each task the reviewer is
	// given in reviewing ends naming this number (reviewround.EntryLabel), its review's
	// footer names it back, and only a review that names the number the row holds moves
	// ReviewedHead. Because the number is never reset and each entry raises it, a review of an
	// earlier entry can never name the entry that stands.
	Entry int64
	// EntryTask is the work of that entry, the task text the role is told, written beside the
	// number in the same transaction. It is what makes the work a durable fact of the record
	// rather than of the one outbox row that carried it: a claim behind the entry is sent this
	// task again, however that row ended. Empty on a row no entry has been raised on, and on one
	// ClearGeneration reset, whose re-admitted tree's promotion writes the task again.
	EntryTask string
	// EntryVerbatim says the sweep's send of a root architect's entry again sends EntryTask as
	// written (workflow's SendArchitectStart). Only the resume of a root a person answered sets
	// it (workflow's ResumeAwaitedRoot): its task names the answer and no kept decision, and the
	// answer is the work the entry owes. Every other raise clears it (workflow's enter), and
	// ClearGeneration resets it with the task. False, the default, is a task the derivation of a
	// root architect's first task wrote, or no task at all, which that send derives again from
	// the record as it stands: a kept decision is claimed by the outbox row that names it, so a
	// text composed once and sent again could name one another telling has claimed since. It is
	// a fact of the record rather than a reading of the task's text, so what wrote an entry never
	// turns on the wording of one release. A phase worker's row carries false and nothing reads
	// it there, since the send of a phase worker's entry sends its task as written in every case
	// (workflow's SendOwedEntry).
	EntryVerbatim bool
	// EntryRaisedAt is when the entry was raised, read from the transition's own clock. It is
	// what tells how long a role has been owed the work its entry names, for a reader weighing a
	// difference that has stood. Zero on a row no entry has been raised on.
	EntryRaisedAt time.Time
	// EntryMovedBack is whether the start that raised that entry was a move back to an earlier
	// phase, written beside the three fields above in the same transaction from the move the
	// transition already reads for its pass (workflow's transition, start and enter). True on
	// every row a daemon upgraded in place found, a row no entry has been raised on among them:
	// no row before the column said which move raised its entry, and true refuses a behind
	// completion as the release before did (store migration
	// 20261113090512_phase_entry_moved_back.up.sql), until the role's next entry writes what its
	// own move was. A row PutPhase writes reads what it was given: false on one this release
	// inserted before raising any entry on it, and after the start of a mid-phase worker a
	// re-admitted tree's first run brings back, which moves no phase back.
	//
	// One rule of the daemon reads it and no other: the completion route's entry fence, which
	// takes a completion of an earlier entry when the commit it names is the head the record
	// holds, and refuses it however the heads read when the entry standing now was raised by a
	// move back (api/handoff.go). Such an entry asks for the phase's work to be done again, which
	// is what the earlier entry's verdict must not stand in for, and the heads cannot say so: the
	// move back that sends work back at a head that did not move leaves the two heads equal. The
	// counts beside it cannot say so either — Pass rises on every move back and a phase is never
	// re-entered without one, so a fence on the count would refuse the case the route exists to
	// take.
	EntryMovedBack bool
	// StepTask and StepPlace are the step this role's row runs, for an issue with plan rows: the
	// plan's task (PlanTask.Task) and the step's place in it, from 1 for a step and 0 for the retro
	// on the implementer's row and for the merge on the merger's row, written by the start that
	// raises the entry (workflow's enter). A role runs at most one step of an issue, so the row's
	// handoff, completion, entry and decision above are the facts of that step's run. An issue with
	// no plan rows keeps "" and 0, and so does every row the release before the column wrote. A new
	// generation clears both with the entry's task (ClearGeneration), since the task ids belong to
	// one generation's plan; a carried hand-over keeps its rows, and the step with them.
	StepTask  string
	StepPlace int
}

// Completed says whether the role completed its current phase: its completion reported a commit
// or named a handoff record, both of which clearing the handoff at the next phase's start
// (workflow's clearHandoff) and a new generation (ClearGeneration) empty.
func (p PhaseRow) Completed() bool {
	return p.HandoffCommit != "" || p.HandoffRecord != ""
}

// PhaseRole names one row of the phases record: the issue and the role whose row it is, which
// together are that row's key. It is how a batched read of many issues' rows answers
// (Store.PhaseEntries).
type PhaseRole struct {
	Issue string
	Role  claim.Role
}

// PhaseEntry is what a batched read of many issues' phases rows answers for one of them
// (Store.PhaseEntries): the entry the row names, the moment the transition raised it and whether
// its task is one to send as written, which are the three fields of PhaseRow a pass over every
// slot holder reads, and the claim the row names with the task of the step it runs, which
// WorkerClaimOf reads. RaisedAt is zero on a row no entry has been raised on, where Entry is zero
// too.
type PhaseEntry struct {
	Entry    int64
	RaisedAt time.Time
	// Verbatim is the row's PhaseRow.EntryVerbatim: the entry was raised by the resume of a root a
	// person answered, whose task names the answer. The sweep's reach over a root's architect
	// reads it (workflow.ArchitectAtWork): such an entry, while its claim has not been given it,
	// keeps a root the answer's own pass promoted past admitted in the reach, in every phase that
	// carries a run but awaiting_person, where the answer to the new wait raises an entry of its own.
	Verbatim bool
	// Claim and StepTask are the row's PhaseRow.Claim and PhaseRow.StepTask.
	Claim    claim.Token
	StepTask string
}

// ReviewDecision is what one review decided: its state (changes_requested or approved), its body,
// handed to the next implementer, and the head it was written on, whose code an approval approves.
//
// ToldStuckHead and ToldStuckState are the daemon's own mark beside it rather than anything the
// review said: the head and GitHub's merge state an approval of this decision was last reported
// stuck on, in the review-stuck notice an approval GitHub will not merge from writes (workflow's
// settleRound, stuckMergeState), which the state build shows. ToldStuck is every finding already
// told for ToldStuckHead: a state, or what held the head beside its state (a StuckHold). Together
// they make that notice once for each head and finding rather than once for each fact or for each
// turn findings take: GitHub answers one head `blocked`, then `unknown` while it recomputes, then
// `blocked` again, and a head held on a review request is held on a null `mergeable` for a while
// after each push to its base, then on the review request again - and the mark is the only thing
// that survives the answers in between. A mark that kept only the last finding would tell each
// finding again every time another came between. A decision an earlier release marked carries no
// list, and its ToldStuckState stands as the one finding told, for the check and for the next
// append alike (workflow's toldFindings), so the upgrade forgets no finding. They live with
// the decision because the notice is of the approval this decision is: a review that decides the
// round afresh replaces the decision and the mark with it, and the next head the new approval
// sticks on is told in its turn. Empty until a notice goes out.
//
// Refused is the daemon's own mark too, and the one case where a decision row holds no decision:
// why the daemon set aside a review that would otherwise have decided the round (workflow's
// refusesRound), which is what the stuck notice of a round nothing decided names
// (workflow's undecidedReason). It is written only on a row that holds no deciding decision -
// State empty - so a refusal that follows an approval cannot erase it, and the clear every entry
// into a phase makes of the role's row (workflow's clearHandoff) takes it with the decision, so
// no round inherits the refusal of the round before it. The decision column is jsonb, so a row
// an older daemon wrote simply has none.
type ReviewDecision struct {
	State          string      `json:"state"`
	Body           string      `json:"body,omitempty"`
	Head           string      `json:"head,omitempty"`
	Refused        string      `json:"refused,omitempty"`
	ToldStuckHead  string      `json:"toldStuckHead,omitempty"`
	ToldStuckState MergeState  `json:"toldStuckState,omitempty"`
	ToldStuck      []StuckTold `json:"toldStuck,omitempty"`
}

// StuckTold is one finding a review-stuck notice told for a head (ReviewDecision.ToldStuck): what
// held the head when GitHub's merge state was beside a hold, or else the state itself.
type StuckTold struct {
	State MergeState `json:"state,omitempty"`
	Hold  StuckHold  `json:"hold,omitempty"`
}

// StuckHold is what holds a head GitHub answers `blocked` or `unknown` for, when the daemon can read
// it (classify.MergeOutcome.Hold): a finding of its own, whatever state GitHub gives beside it.
type StuckHold string

// The holds a review-stuck notice names: GitHub has not computed whether the head merges
// (`mergeable` null), or it holds a review request open on the pull request.
const (
	StuckHoldMergeableNull   StuckHold = "mergeable-null"
	StuckHoldReviewRequested StuckHold = "review-requested"
)

// ReviewOrder is where a review falls among the pull request's reviews: when it was submitted, then
// GitHub's review id. The id alone is not enough, since GitHub assigns it when a review is created
// and a draft keeps it when it is submitted later. SubmittedAt is zero for a review a listener that
// predates submitted_at carried, one whose time could not be read, and every mark recorded before
// the field. Among reviews that all carry a time the order does not depend on delivery; with an
// untimed review in play it is not transitive, so the outcome can depend on the order reviews are
// delivered in. No stored mark can recover a time it never had. The order decides a round only
// among reviews that arrive before it ends (workflow's review).
type ReviewOrder struct {
	SubmittedAt time.Time
	ID          int64
}

// After is whether o was submitted after other. Submission times decide when both reviews have one
// and they differ; otherwise the ids do, so a review without a time is ordered by id against any
// other, rather than losing to every review that has one.
func (o ReviewOrder) After(other ReviewOrder) bool {
	if !o.SubmittedAt.IsZero() && !other.SubmittedAt.IsZero() && !o.SubmittedAt.Equal(other.SubmittedAt) {
		return o.SubmittedAt.After(other.SubmittedAt)
	}
	return o.ID > other.ID
}

// PullRequest is the daemon's latest GitHub observation for one issue's pull request.
type PullRequest struct {
	Issue  string
	Repo   string
	Number int
	Branch string
	// Base is the branch the pull request's head merges into, as GitHub's pull-request webhook
	// names it (`base_ref`) and the daemon's own read of the pull request names it (`base.ref`);
	// "" is a pull request recorded before the daemon kept the base, which the next read fills. A
	// push to this branch is what tells the daemon the head may have gone behind or dirty, since
	// GitHub sends no event for the pull request when its base moves (workflow's basePush).
	Base string
	// BaseMoved is the commit the latest push onto Base left that branch at, "" when no such push
	// is recorded. It says which base the mergeability and merge state recorded below were read
	// against: while it is set, GitHub may have recomputed both, so the next read of the pull
	// request is known to be needed rather than waited for.
	BaseMoved string
	HeadSHA   string
	// HeadUpdatedAt is the latest updated_at among the lifecycle observations applied (opened,
	// reopened, synchronize or closed; one with no clock never lowers it): an older one is a late
	// redelivery and changes nothing (classify.LateLifecycle).
	HeadUpdatedAt time.Time
	// CheckedHead is the head whose CI settlement Failing, Cancelled, CheckRuns, Generation,
	// Snapshot and Statuses record, empty while none is recorded: the current head, or an earlier
	// head whose settlement no longer counts, since a settlement stands only for the head it names.
	// A settlement moves no issue (dispatch://INFRA-714/spec R1): what it names is told to the
	// roles that work the issue and judged by them (workflow's checks).
	CheckedHead string
	// CheckRuns is the latest run observed of each check name, and Failing and Cancelled the
	// checks that come to failed and to cancelled. All three stand for the checked head alone: a
	// settlement of another head is applied to the fence the pull request remembers for that head
	// (SettledHeads), or to an empty one where it remembers none (classify.SettlementFor). A run's
	// own conclusion is what puts its name in Failing, in Cancelled, or in neither
	// (classify.ApplySettlement), so a name whose newest run passed is in none of them however an
	// older run of it concluded. A name the settlements report no run for at all - a check GitHub
	// never started - keeps the conclusion the lists were given (classify.EffectiveOutcome), and a
	// check with no concluded run anywhere is in none of them.
	Failing    []string
	Cancelled  []string
	CheckRuns  []AttemptRun
	Generation int64
	Snapshot   string
	// Statuses are the commit statuses the settlements applied for the checked head named, the
	// newest of each context, in context order and each stamped with that head
	// (CommitStatus.Head). A settlement's list is the whole list the head's CI record holds, which
	// can be fewer than GitHub holds: pending statuses are not recorded, a token without Commit
	// statuses names none, the record's size bound can drop them, and a listener of an earlier
	// release names none. So each applied settlement of the checked head is merged into it per
	// context, the entry with the later created_at kept (classify.ApplySettlement): a context the
	// settlement leaves out stays as it is kept, and an older status never puts back a state a
	// newer one replaced. A settlement that names a newer status and whose check runs are older
	// than the kept ones, or the same at a generation no higher than the kept one, is applied to
	// this list alone, the kept entry standing on an equal created_at, and leaves the check runs,
	// Failing, Cancelled, Generation and Snapshot as they are (classify.SettlementStatusesOnly). A
	// settlement the daemon does not apply leaves it as it is. A reopen and a new generation clear
	// it with the check runs (the reopen step of a head run, Store.ClearGeneration), and a
	// settlement of a new checked head puts back the statuses remembered for that head with its
	// check runs (classify.SettlementFor). A move of the head leaves it stamped with the head it was
	// of, so no text names it for the new head (workflow's headChecks).
	Statuses []CommitStatus
	// SettledHeads is what the pull request remembers of each of the last heads a CI settlement
	// applied on: that head's fence as the last settlement applied on it left it, the head a
	// settlement applied on last at the end, at most classify.maxSettledHeads heads. Each entry is
	// a Settlement whose fields are the pull request's fields of the same name as they stood with
	// CheckedHead at the entry's Head (Settlement says what each holds). The fence above is one
	// head's: a reopen and a new generation's clear empty it, and a settlement of another head
	// moves it there. A settlement of a head the fence does not stand at is weighed against, and
	// applied to, that head's entry here, which is the fence it would have met had nothing moved
	// or emptied it (classify.SettlementFor): each check name keeps its run of highest id and each
	// context its status of latest created_at, so an earlier settlement of the head, delivered
	// late or replayed from a message the intake gave up on, however late, is stale as it is
	// against the fence, and never puts back a run or a status a newer one replaced. A reset
	// clears the fence and never this. A head run keeps it as it stood at the run's anchor
	// (HeadRun.Settled), and a reopen and a new generation keep it, as the pull request's row is.
	//
	// The store keeps it in a table of its own, pull_request_settled_heads, one row for each head
	// (migration 20261120144241 says why), which only the three whole reads of one pull request
	// load: PullRequest, PullRequestByNumber and PullRequestByBranch. A list read and a row read
	// (PullRequestRow, PullRequestRowByNumber, PullRequestRowByBranch) load none of it and mark
	// their records (readBy), which PutPullRequest refuses.
	SettledHeads []Settlement
	// readBy names the read that made the record with none of what it remembers - the list reads
	// OpenPullRequests, OpenPullRequestsOnBase and PullRequestsOf, and the row reads
	// PullRequestRow, PullRequestRowByNumber and PullRequestRowByBranch - and is "" for a record
	// one of the three whole reads of one pull request made, or that no read made. Such a read
	// loads neither SettledHeads nor HeadRun.Settled, so a write of its record would delete every
	// remembered head: PutPullRequest refuses it, naming the read.
	readBy string
	// Pushes are the branch's pushes the daemon still reads: every push whose own move, its two
	// heads, the record has not made (Moves) - a push still on its way from the head, a link of a
	// chain still to complete, or one that returns the branch to a commit it held before
	// (classify.unspentPushes) - in the order they were delivered, two pushes of the same two heads
	// kept apart.
	Pushes []ClassifiedPush
	// Moves are the moves the record has made, newest last and at most the last 32, each pair of
	// heads once: each head's arrival from the head before it, and each push on the chain of pushes
	// it found between them. A push whose own move is among them is spent (classify.unspentPushes),
	// whenever it arrives: as the move's own push when the move has none recorded yet (Move.Pushed),
	// and otherwise queued on the move (Move.Repeats). A push for a move not made yet - one that
	// returns the branch to a commit it held before, say - is not spent.
	Moves []Move
	// Run is the head run: the pull request's judgement as it stood before the first head still
	// waiting for a push of its move, and every head, settlement and reopen applied since,
	// replayed at each step and each push (classify.ApplyPush). Nil when no head waits for a push,
	// and once a run is cut to its bound with none left waiting.
	Run *HeadRun
	// ReviewSeen is the newest deciding review (changes requested or approved, from the review App
	// or an account with write access to the repository) GitHub reported for the pull request: a
	// deciding review not after it was submitted before one already processed, and records nothing.
	// A comment, or anyone else's review, decides nothing and leaves it as it is. It lasts as long
	// as the pull request's record: across rounds, a reopen, and a new generation while the pull
	// request is open; a new generation deletes one that is not.
	ReviewSeen ReviewOrder
	State      PullRequestState
	// Mergeability is GitHub's lazily computed verdict for whether this head can be merged into
	// its base branch without a conflict, as the daemon's own read of
	// GitHub's /pulls/{number} last found it (daemon.readIssuePullRequest), decoding GitHub's
	// nullable `mergeable` field: MergeabilityUnknown while GitHub is still computing it (never a
	// transient conflict - GitHub reports no checks run on a mergeability GitHub has not computed
	// yet, either), MergeabilityMergeable once GitHub can merge the head automatically, and
	// MergeabilityConflicting once it cannot. "" means the record holds none for this head
	// (StateReadAt below says when that is).
	Mergeability Mergeability
	// MergeState is GitHub's own merge state for this head, read from `mergeable_state` on the
	// same /pulls/{number} answer Mergeability is decoded from (daemon.readIssuePullRequest), in
	// GitHub's lowercase spelling; "" means the record holds none for this head (StateReadAt below
	// says when that is). It is kept beside
	// Mergeability because the tri-state cannot tell a head GitHub holds on the repository's own
	// required checks or reviews (MergeStateBlocked) from one GitHub would merge now
	// (MergeStateClean): GitHub answers `mergeable: true` for both.
	MergeState MergeState
	// ReviewsRequested is how many review requests GitHub holds open on the pull request, people,
	// Apps and teams alike, as the read that recorded MergeState found them: a base's GraphQL batch
	// reads `reviewRequests { totalCount }` beside the merge state, a REST read of a `blocked`,
	// MERGEABLE head takes its whole answer from the same GraphQL query, and any other REST read
	// counts its `requested_reviewers` and `requested_teams`. Nil means no read has counted them:
	// a row written before migration 20261124130001, a head no read has answered for, or a GraphQL
	// answer that left the count null or named it in an error. It speaks of the read at
	// EvidenceReadAt alone. A review GitHub asked for and that is not given yet is the one cause of
	// a `blocked` head the daemon can read without a ruleset (classify.MergeOutcomeOf). GitHub does
	// not ask again after it dismisses an approval, so 0 does not say that no review is required;
	// the merger's merge names any review still required.
	ReviewsRequested *int
	// HeadChecks is whether a check or a commit status of this head was still running when the
	// read that recorded MergeState was made: GitHub's `statusCheckRollup` of the head commit,
	// counted by state, which that read carries (a base's GraphQL batch asks for it beside the
	// merge state, and a REST read that answers `blocked` and MERGEABLE takes its whole answer from
	// the same query). It is current evidence, where the head's settlement (ChecksConcludedOnHead)
	// is only the last one the daemon applied: a check re-run on the same head, a workflow queued
	// before it makes a run and a commit status set back to pending leave that settlement standing
	// while GitHub holds the head `blocked`. The unread value is a row written before migration
	// 20261124130001, or a read that gave no rollup for this head. It speaks of the read at
	// EvidenceReadAt alone.
	HeadChecks HeadChecks
	// NullMergeableReads is how many reads of this head in a row GitHub answered `mergeable` null,
	// the answer it gives while it computes whether the head merges, whatever merge state it gave
	// beside it (`blocked` or `unknown`), and NullMergeableSince is when the first of them was read
	// (its StateReadAt). The round waits quietly while the run is younger than
	// classify.NullMergeableFloor, read to read, which is routine after a push, and tells the
	// architect once a null read of the run comes at least that long after its first, or once the
	// first is classify.NullMergeableTellAfter old by the clock, whichever comes first
	// (classify.MergeOutcomeOf). A computed answer (MERGEABLE or CONFLICTING), another head, or a
	// reopen, sets both back to zero.
	NullMergeableReads int
	NullMergeableSince time.Time
	// EvidenceReadAt is the StateReadAt of the read that wrote ReviewsRequested, HeadChecks and the
	// null reads. They speak of that read alone, and classify reads them as unread unless
	// EvidenceReadAt equals StateReadAt (classify.EvidenceStands): a release before migration
	// 20261124130001 writes its own read time beside a new head or a new read of the same head and
	// leaves them where they stood, so after a rollback and a roll-forward they can be an older
	// read's. The zero time means no read wrote them.
	EvidenceReadAt time.Time
	// StateReadAt is the moment the daemon last got GitHub's answer about the merge state of the
	// head this record stands at, taken from the reader's own clock when that answer was in hand.
	// This is the one full account of the stamp; every other comment that needs part of it points
	// here.
	//
	// Who stamps it: both readers of the pull request, the per-issue read (daemon's
	// readIssuePullRequest) and the merge-state ladder (daemon's readMergeState), each through the
	// mergeability fact (workflow's mergeability). An answer about a head the record has moved past
	// is dropped with its time, and so is an answer read before the one the record holds: the
	// readers run apart, and the per-issue read dates its answer before it waits on the fact lock,
	// so an older answer can be applied after a newer one, and it would put back what GitHub has
	// answered otherwise since. So the stamp is the moment of the newest read the record holds of
	// its head. A read that only confirms what is recorded moves the stamp exactly as one that
	// changes something does: it is the age of the knowledge, not of the last change to it, and
	// that is what tells a head read a minute ago from one nothing has read for a day. No webhook
	// stamps it, and a fact with no read time behind it writes none, erases none and is never
	// dropped for its time.
	//
	// What clears it, always with MergeState and Mergeability: any fact that moves the head
	// (classify's advance: a synchronize, a reopen or a close at another head), because the stamp
	// would then be the time of a read about the head before; and a reopen at any head, which
	// builds the record afresh (workflow's pullRequestOpened).
	//
	// What carries it: a replay of the head run moves it with MergeState and Mergeability
	// (classify's replayRun and withJudgement), so a replay never pairs one read's state with
	// another read's time. withJudgement decides on MergeState and not on the stamp: wherever the
	// record holds no state, a replay puts in place of the record's pair the one the replayed run
	// ends with, which is the anchor's pair as the run's steps leave it, a head step among them
	// clearing it (advance). So a stamp held beside no state can go with no head move of the record
	// and no reopen. A run anchored before migration 20261112070503 holds a state with no stamp,
	// and a replay gives that pair back as it is.
	//
	// What the zero time means, after any of those: the record holds no read time for its head.
	// Beside a recorded MergeState it is a state whose read time the record does not hold, a row
	// written before migration 20261112070503 or a pair a replay gave back from an anchor that
	// predates it; beside MergeState's "" the record holds no read of its head at all. Neither says
	// when GitHub was last asked.
	StateReadAt time.Time
	// ConflictingHead is the head the daemon last judged conflicting with its base, written only
	// when a conflict is confirmed: a second CONFLICTING read of this head and the same base branch,
	// made at least a minute after the first read (ConflictRead below), in workflow's
	// mergeability. A read that finds the same head mergeable clears it; a read of
	// MergeabilityUnknown, GitHub's answer for a head sitting in the merge queue, neither writes
	// nor clears it. "" means no head is held conflicting.
	//
	// A backward move made while it is non-empty and equal to HeadSHA is a conflict round, which
	// review_round_cap does not count (PhaseRow.ConflictRounds): the implementer answers such a
	// round with a forward merge, which moves HeadSHA and so ends the exemption with no write of
	// its own. Mergeability beside it cannot carry the fact, because every read rewrites it from
	// GitHub's answer and that answer is UNKNOWN while the head is queued, so a move made after a
	// later read would read as no conflict.
	ConflictingHead string
	// ConflictRead is a first read of the record's head that found it conflicting with its base,
	// which no second read has confirmed or released yet (dispatch://INFRA-4374 R2), nil while the
	// record holds none. GitHub computes mergeability in a background job and can answer
	// CONFLICTING about a base it has not caught up with, so one read acts on nothing: the daemon
	// acts on a conflict only once a second CONFLICTING read of the same head and the same base
	// branch, made at least a minute after the first (ConflictRead.At), confirms it.
	//
	// Who writes and clears it: workflow's mergeability. A CONFLICTING read that finds no first
	// read of its head and base branch writes one, queues the conflict_confirm read that asks
	// GitHub again once the minute has passed, and acts on nothing: ConflictingHead above is
	// written only at the confirmation. A MERGEABLE read, from any reader, clears it, and so does
	// the confirmation, which hands the conflict to ConflictingHead and the act on it (R0). An
	// UNKNOWN read leaves it as it is, as it leaves ConflictingHead; when the conflict_confirm
	// row's last read still answers UNKNOWN, the fact that row applies
	// (intake.ConflictConfirmDone) clears it, but only while it is that row's first read, of the
	// row's head and made at the row's time (ConflictConfirm.FirstReadAt): a row of a first read
	// already released leaves a newer first read of the same head standing. After any of these
	// clears the next CONFLICTING read is a first read again. A read made before the one the record
	// holds (StateReadAt above) neither writes, confirms nor clears it: it is dropped whole with its
	// time, as one about a head the record has moved past is, so a MERGEABLE answer read before the
	// first read and applied after it leaves the first read standing.
	//
	// Who reads it: mergeability, to tell a confirming read from a first one; the conflict_confirm
	// row's executor in internal/daemon, which finishes without a read once the record no longer
	// holds the row's first read, of its head and made at its time; and the log line of the read
	// that releases it, which names both reads.
	//
	// It is GitHub's answer about a head and a base, as ConflictingHead is, not the issue's run's,
	// so a new generation keeps it (Store.ClearGeneration). It is no part of a head run's
	// judgement (Judgement), so a replay of the run leaves it as it is (classify's replayRun and
	// withJudgement), and so does a move of the head (classify's advance): it names its own head,
	// and a read of another head confirms nothing of it.
	ConflictRead *ConflictRead
	// MergeForward is the daemon's ask that a role merge the base forward onto a head whose
	// conflict was confirmed and acted on (dispatch://INFRA-4374 R1), nil while none stands. The
	// issue keeps its phase, and the ask is a message on the topic of the claim that works the
	// role's step rather than a phase start: it names the head and the base branch of the conflict,
	// the role asked, the phase the issue stood in and stays in, why the act was needed, when it was
	// asked, and, once a new head arrives, that head (MergeForward.Landed). It also keeps every
	// merge forward that landed before it in the same stretch of work (MergeForward.Earlier).
	//
	// Who writes it: Engine.mergeForward, in the transaction that queues the merge_forward rows,
	// and pullRequestSynchronized, which sets Landed on the first head that moves off
	// MergeForward.Head and ReviewAsked when that landing asks the reviewer to read the merge. An
	// ask that replaces the one the record holds - of a new head whose conflict was confirmed, or
	// of the same head asked again on a phase entry - carries that ask's Earlier, and the ask
	// itself, its head and the head that landed, once it landed.
	// What clears it: any transition into implementing, the issue leaving the workflow, and a new
	// generation (Store.ClearGeneration), since it is an ask of the run before. That ends the
	// stretch of work, Earlier with it.
	//
	// Who reads it: Engine.mergeForward, which asks once for each head, so a later confirmed
	// conflict or a transition at the head it names sends nothing (an ask made in implementing
	// covers implementing only); the merge_forward row's executor in internal/daemon, which
	// finishes undelivered once the record's ask no longer names the row's head; Engine.handoff,
	// which refuses a completion at MergeForward.Head once the record's head has moved off it, or
	// at the head of any merge forward of Earlier the record's head is not
	// (HANDOFF_HEAD_MERGED_FORWARD); and the merger's start task, which waits for the reviewer's
	// approval of the landed head when the reviewer was asked to read it.
	//
	// It is no part of a head run's judgement (Judgement), so a replay of the run and a move of
	// the head leave it as it is (classify's replayRun, withJudgement and advance): the arrival of
	// a new head is what Landed records, not a reason to drop the ask.
	MergeForward *MergeForward
	// MergeSHA is the merge commit GitHub reported for the pull request, which the production
	// check's agent looks for in production: empty until the merge, and for a pull request merged
	// before the column existed. Nothing fills it later — no compare is left to — so the check's
	// task says the commit is unknown and the agent reads it from GitHub (workflow.checkTask).
	MergeSHA string
}

// ConflictRead is a first CONFLICTING read of a pull request's head (PullRequest.ConflictRead):
// the head GitHub answered about, the base branch it answered against, and when that read was
// made. Base is the branch's name, the one the read named (intake.PullRequestMergeability.Base)
// or else the recorded PullRequest.Base, and never a commit: GitHub's base.sha does not name the
// base commit its mergeability was computed against, and a conflict with a newer base is still a
// conflict, so a push to the base between the two reads does not start the two reads again. It
// is stored as the jsonb of pull_requests.conflict_read (migration 20261119003157), null for nil.
type ConflictRead struct {
	Head string    `json:"head"`
	Base string    `json:"base"`
	At   time.Time `json:"at"`
}

// MergeForward is the daemon's ask that a role merge Base forward onto Head
// (PullRequest.MergeForward). Role is the role asked: the implementer before merging, and the
// merger in merging (MergeForwardAsker, cybermonklabs/legion#4979); Asked reads it. Phase is the phase
// the issue stood in when the daemon asked, and stays in. Why is the first of R0's reasons that
// held (dispatch://INFRA-4374 R0, ConflictWhy). AskedAt is when the ask was written. Landed is the
// first new head that arrived after the ask, "" until one does; ReviewAsked is whether that
// landing asked the reviewer to read the merge, which it does when the reviewer step of the
// issue's task already ran (workflow's mergeForwardLanded), and which the merger's task and its
// landed message read. Earlier is every merge forward that landed before this ask in the same
// stretch of work, oldest first: a later ask replaces the one the record holds, so when the head a
// merge left conflicts in its turn and is merged forward again, Earlier is what still names the
// head the first merge replaced (PullRequest.MergeForward says how long the stretch lasts). It is
// stored as the jsonb of pull_requests.merge_forward (migration 20261119003157), null for nil; the
// jsonb takes Earlier and Role with no migration of their own, and leaves each out while it is
// empty.
type MergeForward struct {
	Head        string               `json:"head"`
	Base        string               `json:"base"`
	Role        claim.Role           `json:"role,omitempty"`
	Phase       phase.Phase          `json:"phase"`
	Why         ConflictWhy          `json:"why"`
	AskedAt     time.Time            `json:"askedAt"`
	Landed      string               `json:"landed,omitempty"`
	ReviewAsked bool                 `json:"reviewAsked,omitempty"`
	Earlier     []LandedMergeForward `json:"earlier,omitempty"`
}

// Asked is the role the ask names: Role, or the implementer for an ask that names none, which a
// daemon before cybermonklabs/legion#4979 wrote, since it asked the implementer in every phase. So an
// implementer that daemon asked in merging still merges after the upgrade, and the merger is asked
// only by a new act (workflow's mergeForward).
func (m MergeForward) Asked() claim.Role {
	if m.Role == "" {
		return claim.RoleImplementer
	}
	return m.Role
}

// LandedMergeForward is a merge forward that landed before the ask the record holds
// (MergeForward.Earlier): Head is the head it was asked of, the one its merge replaced, and
// Landed the head the merge left.
type LandedMergeForward struct {
	Head   string `json:"head"`
	Landed string `json:"landed"`
}

// ConflictWhy is one of the reasons R0 acts on a confirmed conflict for (dispatch://INFRA-4374
// R0, classify.ConflictNeedsMerge, which reads them in this order), as the one word the daemon
// records the act's reason in (MergeForward.Why, MergeForwardMessage.Why) and logs it under. It is
// a string, so the JSON of a record or a row that carries one is the word itself. They are
// declared here, and classify's names are these, because record is what both the decision and
// the message that states it import.
type ConflictWhy string

const (
	// ConflictMergeNext: the issue stands in merging, or is held from it. A conflict where the
	// merge comes next before merging - retro, or accepting where no step of the task follows the
	// acceptance (classify.MergeComesNext) - waits for merging, where the merger merges the base
	// forward (cybermonklabs/legion#4979).
	ConflictMergeNext ConflictWhy = "merge-next"
	// ConflictRulesUnread: the daemon could not read the base branch's rules.
	ConflictRulesUnread ConflictWhy = "base-rules-unread"
	// ConflictRequiresCurrent: a ruleset of the base requires a pull request to be current with it.
	ConflictRequiresCurrent ConflictWhy = "base-requires-current"
	// ConflictCheckNotGreen: a check the base requires did not succeed on the head.
	ConflictCheckNotGreen ConflictWhy = "required-check-not-green"
)

// MergeForwardAsker is the role the daemon asks to merge the base forward while the issue stands
// in at (cybermonklabs/legion#4979): the merger in merging, where the merge is its own work, and
// the implementer in every earlier phase where a merge forward stands (classify.MergeForwardStands:
// the task at the implementer's place, or a place after it on the task's line). The ask's row goes to it
// (MergeForwardMessage), and the record's ask keeps it (MergeForward.Role).
func MergeForwardAsker(at phase.Phase) claim.Role {
	if at == phase.Merging {
		return claim.RoleMerger
	}
	return claim.RoleImplementer
}

// BaseRules is what the daemon read of the rules a pull request's base branch holds for its
// required status checks, on a read that found the head conflicting with that base
// (intake.PullRequestMergeability.Rules). It is what R0 decides a confirmed conflict on
// (dispatch://INFRA-4374 R0, classify.ConflictNeedsMerge): a base that requires a check which has
// not passed on the head, or that requires a pull request to be current with it, is a base the
// head cannot merge into until the base is merged forward, and GitHub runs no check at all on a
// head that conflicts with its base.
//
// Required is every required status-check context, each once and sorted: the contexts of each
// `required_status_checks` rule the base's rulesets apply, then a classic protection rule's
// `contexts` and `checks[].context`. Strict is whether a ruleset's rule sets
// `strict_required_status_checks_policy`; a classic rule is read as not strict, since the answer
// the daemon can read of it carries no such field, so its required contexts decide as a
// ruleset's do. Unread is "" when the read answered, and otherwise why it failed (the HTTP status
// or the error), which R0 reads as rules it cannot tell anything from. GitHub's 403 whose message
// contains `make this repository public to enable this feature` is no failure: it is the answer
// of a plan that holds neither rulesets nor classic protection, so it reads as no rules (Required
// empty, Unread "").
type BaseRules struct {
	Required []string
	Strict   bool
	Unread   string
}

// HeadRun is a pull request's head run (PullRequest.Run): Anchor, the judgement before the run's
// first step; Pushes, the anchor's pushes and every push recorded since, in delivery
// order, less those the record's bound let go; Names, the moves synchronizes that moved no head
// named while the run was open (classify.NameMove), less those an arrival spent; and Steps, in the
// order they were applied. Replaying the steps from the anchor with every push and every named
// move gives the judgement the facts would have given had every push and synchronize arrived
// before every head (classify.ApplyPush).
type HeadRun struct {
	Anchor Judgement        `json:"anchor"`
	Pushes []ClassifiedPush `json:"pushes"`
	Names  []Named          `json:"names,omitempty"`
	// Since is GitHub's clock of the anchor's head (PullRequest.HeadUpdatedAt when the run opened),
	// zero when unknown: a named move older than it is no arrival's of the run.
	Since time.Time `json:"since,omitzero"`
	// Settled is what the pull request remembered of its settled heads at the anchor
	// (PullRequest.SettledHeads), which the replay starts from: each settlement among the steps is
	// then weighed against what was settled before it, as it was when it arrived, never against a
	// later step of the same run. The pull request's own SettledHeads holds every step already, and
	// against it a step that was not stale when it arrived can be: one newer for some check names
	// and older for others than a later step, or one newer for its statuses alone. Such a step
	// shows a head again after its fence was reset (classify's settle) when it arrives, and would
	// show nothing in the replay.
	//
	// The head_run column's JSON leaves it out: the store keeps it in pull_request_settled_heads as
	// the rows of kind head_run, beside the pull request's own, and the three reads of one pull
	// request load it while a run is open (PullRequest.SettledHeads). A daemon of the release
	// before the table writes the run and never those rows, so after a roll forward an open run
	// reads the rows this release last wrote: those of the run open at the rollback, a memory no
	// newer than a later run's anchor, which fences no more than that anchor's would; or none,
	// where no run was open then, and the replay starts from nothing remembered.
	Settled []Settlement `json:"-"`
	Steps   []RunStep    `json:"steps"`
}

// Named is a move a synchronize named without moving the head: the move from Before to SHA, made by
// the push of Sender, at At by GitHub's clock (the pull request's updated_at).
type Named struct {
	Before string    `json:"before"`
	SHA    string    `json:"sha"`
	Sender string    `json:"sender"`
	At     time.Time `json:"at,omitzero"`
}

// Judgement is the part of a pull request a head run replays: its head, its CI settlement, the
// moves it has made, and GitHub's verdict on merging the head it stood at with the time that
// verdict was read.
//
// The verdict is the anchor's alone to give back, and only to a record that holds none: a move no
// push links yet takes the verdict with it (classify's advance), and the push that links the move
// replays the run, which is where a verdict the move turns out to have carried comes back. A read
// of GitHub made while the run was open answers for the head the run has reached, which is newer
// than the anchor's, so it stands and the anchor's is dropped (classify's withJudgement). A run
// recorded before these fields carries no verdict, and gives none back.
type Judgement struct {
	HeadSHA     string       `json:"headSha"`
	CheckedHead string       `json:"checkedHead,omitempty"`
	Failing     []string     `json:"failing"`
	Cancelled   []string     `json:"cancelled"`
	CheckRuns   []AttemptRun `json:"checkRuns"`
	Generation  int64        `json:"generation,omitempty"`
	Snapshot    string       `json:"snapshot,omitempty"`
	// omitempty, so a run anchored before the field reads as it did, and one anchored on a record
	// that keeps no status writes the same bytes as before it.
	Statuses     []CommitStatus `json:"statuses,omitempty"`
	Moves        []Move         `json:"moves,omitempty"`
	Mergeability Mergeability   `json:"mergeability,omitempty"`
	MergeState   MergeState     `json:"mergeState,omitempty"`
	// omitempty and omitzero, so a run anchored before any of these fields reads as it did, and a
	// nil count, no read of it, stays nil (PullRequest.ReviewsRequested, HeadChecks, the null reads
	// and EvidenceReadAt).
	ReviewsRequested   *int       `json:"reviewsRequested,omitempty"`
	HeadChecks         HeadChecks `json:"headChecks,omitempty"`
	NullMergeableReads int        `json:"nullMergeableReads,omitempty"`
	NullMergeableSince time.Time  `json:"nullMergeableSince,omitzero"`
	EvidenceReadAt     time.Time  `json:"evidenceReadAt,omitzero"`
	// omitzero, as HeadRun.Since and Named.At are: omitempty leaves a zero time in the JSON. A run
	// anchored before any read answered carries none, and so does one anchored before migration
	// 20261112070503 (PullRequest.StateReadAt says what that means).
	StateReadAt time.Time `json:"stateReadAt,omitzero"`
}

// Move is a move from the head Before to the head SHA, which a push with the same two heads makes.
// Pushed is whether that push is recorded: the move was made by it, it arrived after the move and
// the move spent it, or a chain of several recorded pushes made the move, for which GitHub sends no
// push of its own. Repeats are the pushes of the same two heads that arrived once the move was
// pushed, oldest first and at most 16: each is the push delivered again or the branch moving
// between the two heads again, which only a head's next arrival by that move tells apart.
// An arrival takes one of them for its push only through the head its synchronize says it moved
// from: its sender's when the synchronize names its sender, the oldest otherwise
// (classify.arrivalPushes). A move recorded before these fields existed reads as not
// pushed, with no repeats, so the next push of its two heads is taken as its own, as every such
// push was before.
type Move struct {
	Before  string           `json:"before"`
	SHA     string           `json:"sha"`
	Pushed  bool             `json:"pushed,omitempty"`
	Repeats []ClassifiedPush `json:"repeats,omitempty"`
}

// RunStep is one fact a head run replays: exactly one of a head's arrival (Head, with Before the
// head its synchronize says the branch moved from and Sender the account whose push moved it, each
// empty when the fact named none, and SyncedAt GitHub's clock of the synchronize), a CI settlement
// (Settlement), or a reopen, which starts the CI settlement afresh (Reopened). A step stored
// before Before, Sender or SyncedAt existed reads as naming none, and one a daemon before
// dispatch://INFRA-714 wrote for a read of what the base branch required, for the clearing of the
// head its workflow runs were read at, or for the arrival clock of a head whose red was held,
// reads as a step that replays as nothing: no required set and no held red is left to replay
// (classify.applyStep).
type RunStep struct {
	Head       string      `json:"head,omitempty"`
	Before     string      `json:"before,omitempty"`
	Sender     string      `json:"sender,omitempty"`
	SyncedAt   time.Time   `json:"syncedAt,omitzero"`
	Settlement *Settlement `json:"settlement,omitempty"`
	Reopened   bool        `json:"reopened,omitempty"`
}

// Settlement is a CI settlement of Head, in one of two uses. As a step of a head run (RunStep) it
// is one settlement as the listener reported it (classify.SettlementFor): Statuses are the commit
// statuses it named, in context order and with no stamp of their own (CommitStatus.Head);
// omitempty, so a step stored before the field reads as naming none, and a step of a settlement
// that names none is the same bytes as before it.
//
// As an entry of what a pull request remembers of its settled heads (PullRequest.SettledHeads,
// HeadRun.Settled) it is Head's check fence as the last settlement applied on it left it, the merge
// of every settlement applied on the head rather than one report of the listener's: CheckRuns is
// the run of highest id of each check name, stamped with the head and what it concluded; Failing
// and Cancelled are the names that came to failed and to cancelled, a check GitHub started no run
// of among them; Generation and Snapshot number the settlement in the listener's record; and
// Statuses is the status of latest created_at of each context, stamped with the head. CheckRuns
// is never nil on such an entry: a fence a settlement applied to holds a list
// (classify.ApplySettlement), and a nil one would read as a head never settled. The store keeps
// such an entry in a row of its own rather than as this JSON (settled_heads.go), and reads it back
// as it was put.
type Settlement struct {
	Head       string         `json:"head"`
	CheckRuns  []AttemptRun   `json:"checkRuns"`
	Generation int64          `json:"generation"`
	Snapshot   string         `json:"snapshot"`
	Failing    []string       `json:"failing"`
	Cancelled  []string       `json:"cancelled"`
	Statuses   []CommitStatus `json:"statuses,omitempty"`
}

// PullRequestState is whether a pull request is open, merged, or closed unmerged.
type PullRequestState string

const (
	PullRequestOpen   PullRequestState = "open"
	PullRequestMerged PullRequestState = "merged"
	PullRequestClosed PullRequestState = "closed"
)

// implementationMoveStates are the pull request states that let an issue's implementation move on:
// one open, whose work is still on its way, and one merged, whose work landed. Every place that
// asks the question reads it from here — LetsImplementationMove for the workflow's
// TriggerImplementationReady snapshot (workflow's transition), the notice that tells an architect
// no pull request is there (workflow's pullRequestMissing), the implementer's completion of
// implementing (api's implementationPullRequest) — and the one place that cannot call Go,
// HandedOffWithoutOpenOrMergedPullRequest's SQL, reads the same list as the parameter of its
// `state = any(...)` (implementationMoveSQLStates), so the query and the predicate cannot drift.
var implementationMoveStates = []PullRequestState{PullRequestOpen, PullRequestMerged}

// implementationMoveSQLStates is implementationMoveStates as the text array
// HandedOffWithoutOpenOrMergedPullRequest passes its query, built once from that same list.
var implementationMoveSQLStates = func() []string {
	states := make([]string, len(implementationMoveStates))
	for i, state := range implementationMoveStates {
		states[i] = string(state)
	}
	return states
}()

// LetsImplementationMove reports whether pr lets an issue's implementation move on
// (implementationMoveStates). It is nil-safe: no pull request recorded lets nothing move.
func LetsImplementationMove(pr *PullRequest) bool {
	return pr != nil && slices.Contains(implementationMoveStates, pr.State)
}

// Mergeability is GitHub's lazily computed verdict for whether a pull request's head can be merged
// into its base without a conflict, decoded from GitHub's nullable `mergeable` field
// (daemon.readIssuePullRequest). "" (the zero value) means the record holds no verdict for its
// head; PullRequest.StateReadAt's doc says when that is.
type Mergeability string

const (
	MergeabilityUnknown     Mergeability = "UNKNOWN"
	MergeabilityMergeable   Mergeability = "MERGEABLE"
	MergeabilityConflicting Mergeability = "CONFLICTING"
)

// MergeState is GitHub's merge state for a pull request's head, read from `mergeable_state` on
// GitHub's /pulls/{number} answer (daemon.readIssuePullRequest). It is kept in GitHub's own
// lowercase spelling rather than one of the daemon's: the state is GitHub's judgement, weighing the
// repository's required checks and reviews, and the daemon only records what it read. "" (the zero
// value) means the record holds no merge state for its head; PullRequest.StateReadAt's doc says
// what that means beside a read time and beside none.
type MergeState string

// The eight states GitHub spells in `mergeable_state`: clean, which GitHub would merge now;
// unstable, where a check that the repository does not require failed; has_hooks, clean on a
// repository whose pre-receive hooks run on the merge; blocked, held by a check or review the
// repository requires; dirty, conflicting with the base; behind, whose base requires the head be
// up to date and it is not; draft; and unknown, which GitHub answers while it is still computing
// the merge.
const (
	MergeStateUnread   MergeState = ""
	MergeStateClean    MergeState = "clean"
	MergeStateUnstable MergeState = "unstable"
	MergeStateHasHooks MergeState = "has_hooks"
	MergeStateBlocked  MergeState = "blocked"
	MergeStateDirty    MergeState = "dirty"
	MergeStateBehind   MergeState = "behind"
	MergeStateDraft    MergeState = "draft"
	MergeStateUnknown  MergeState = "unknown"
)

// Mergeable says whether this is a merge state the issue may advance on. The spec's R1 reads:
// "Mergeable means GitHub's merge state for the head is CLEAN, UNSTABLE (only checks the
// repository does not require failed) or HAS_HOOKS" (dispatch://INFRA-714/spec). unstable counts
// because the only thing it says failed is a check the repository does not require, which is no
// reason to hold the work; has_hooks counts because it is clean on a repository whose pre-receive
// hooks run on the merge. blocked does not count: GitHub holds the head on a check or a review
// the repository itself requires, and GitHub weighs those rules, so this judgement reads no
// rulesets of the daemon's own. The daemon reads the base's rules for its required checks only on
// a read that finds the head conflicting with its base (BaseRules), which R0 decides a confirmed
// conflict on (dispatch://INFRA-4374 R0), not whether a head may advance. dirty, behind, draft and
// unknown are not states GitHub would merge from either.
func (m MergeState) Mergeable() bool {
	return m == MergeStateClean || m == MergeStateUnstable || m == MergeStateHasHooks
}

// HeadChecks is what the read that recorded a pull request's merge state found of its head's
// checks and commit statuses, from GitHub's `statusCheckRollup` of the head commit counted by
// state (PullRequest.HeadChecks): whether any of them was still running then. Unlike the head's
// settlement, which is the last one the daemon applied, it is GitHub's answer at the moment of the
// read. "" (the zero value) means no read gave it for the head the record stands at.
type HeadChecks string

// The three values of HeadChecks: unread; running, a check run of the head QUEUED, IN_PROGRESS,
// PENDING or WAITING, or a commit status PENDING, when GitHub answered; and concluded, none of
// them, or no rollup on a commit with no check or status at all. The rollup's own state is not
// enough: GitHub answers FAILURE as soon as one context fails, while others still run. A status
// GitHub counts EXPECTED, required and never reported, is not running: nothing is under way to
// report it, and the merger's merge names it.
const (
	HeadChecksUnread    HeadChecks = ""
	HeadChecksRunning   HeadChecks = "running"
	HeadChecksConcluded HeadChecks = "concluded"
)

// ProductionCheck is an issue's production check: the work after the close in which the role the
// project's `production_check` named at the open, which Role records, drives the merged change in
// production and reports what it saw. There is one record
// per issue, opened at the merge; a later merge of the same issue — a rerun's, after a re-entry or
// a reopen inside the linger closed the first — replaces it (workflow.openCheck) and counts its
// looks from one again, so OpenedAt is the check instance's identity (ID): what tells the two
// checks' looks apart in the events their rows and reports are recorded under. It copies the
// repository, the pull request's number and the merge commit, and is fenced on itself (the issue,
// the check and the look number) rather than on the tree generation, so a re-admission that
// clears the tree's merged pull requests leaves it as it is. Looks counts the checking role's
// looks, each a wake of its session that ends with a report (pass, pending or fail) or at its
// bound; LookOpen is whether the latest look is still running and LookWakeAt when it woke. Role
// is the role that works the check, written once when the check opens (the project's
// production_check setting) and never read from the configuration again, so a setting changed
// mid-check leaves the open check with the role it started under. LastWakeKind and
// LastWakeDetail are what woke the latest look (the merge, the bound, the agent's own wake) and
// LastWakeAt when; WakeAt is when the check is next looked at — the time the checking agent
// named on a pending report, or the retry of a look nobody reported — and is nil when neither
// stands. Last marks the bound's look, after which no look starts. A closed check has ClosedAt
// and its Outcome. FailedLooks counts the looks in a row whose checking role's claim failed, in
// the look or after it ended, since the role last reported a look: what the next look's backoff
// doubles by (workflow.failedLookRetry), and 0 again once a look is reported.
type ProductionCheck struct {
	Issue   string
	Project string
	Tree    string
	Repo    string
	Number  int
	// MergeSHA is the merge commit the check's agent looks for in production, copied from the
	// pull request's record. It is empty for a pull request merged before the daemon recorded
	// merge commits, and nothing fills it later: the look's task says the commit is unknown and
	// the agent reads it from GitHub (workflow.checkTask).
	MergeSHA string
	OpenedAt time.Time
	// BoundAt is when the whole check is bound: the check's last look starts then
	// (config.Config.ProductionCheckBound after OpenedAt).
	BoundAt        time.Time
	Role           claim.Role
	Looks          int
	LookOpen       bool
	LookWakeAt     *time.Time
	LastWakeKind   string
	LastWakeDetail string
	LastWakeAt     *time.Time
	WakeAt         *time.Time
	Last           bool
	ClosedAt       *time.Time
	Outcome        CheckOutcome
	FailedLooks    int
}

// Open says whether the check is still open: no look has closed it.
func (c ProductionCheck) Open() bool { return c.ClosedAt == nil }

// ID is the check instance's identity as an event id spells it: OpenedAt as microseconds since
// the Unix epoch. OpenedAt rather than a counter because the record has no other field a later
// merge of the same issue does not reuse (the looks start at one again, the pull request may be
// the same); microseconds because that is the precision Postgres keeps a timestamptz at, so the
// value the engine opened the check with (workflow.openCheck truncates its clock to it) and the
// one every later read returns spell the same id, whatever location either carries.
func (c ProductionCheck) ID() string { return strconv.FormatInt(c.OpenedAt.UnixMicro(), 10) }

// NamedBy says whether openedAt names this check instance, as the rows and facts of a check carry
// its OpenedAt: a zero openedAt names no instance — a row queued before the rows carried one — and
// so names this one too; another instant names another check of the issue, one a later merge
// replaced, whose looks count from one again, or the one that replaced this.
func (c ProductionCheck) NamedBy(openedAt time.Time) bool {
	return openedAt.IsZero() || openedAt.Equal(c.OpenedAt)
}

// CheckOutcome is how a production check closed; "" while it is open.
type CheckOutcome string

const (
	// CheckRecorded is a look's pass: the check's own role drove the change in production and
	// recorded it on the pull request.
	CheckRecorded CheckOutcome = "recorded"
	// CheckDefect is a look's fail: the change misbehaved in production, asked about in a Dispatch ask.
	CheckDefect CheckOutcome = "defect"
	// CheckNotRun is a check that closed with no look that could see the change in production:
	// the last look reported pending, or a look could not run.
	CheckNotRun CheckOutcome = "not_run"
)

// DesignStep names one document of the root issue, and a deployment's design gate is the list of
// steps it holds the tree's work on. DesignStepDecisions names the issue's decisions document,
// DesignStepCriteria its Verification document, and DesignStepSpec its primary document. The
// record keeps one DesignGate for each step a root registers, keyed by the issue and the step:
// each step the deployment gates, and criteria where its architect registers the Verification
// document beside a deployment that gates spec alone of the two (TreeSteps).
//
// The type lives in this package rather than beside the `gates.design` list that declares it,
// because internal/classify answers the questions the steps raise and may import no package of
// this module but this one (TestClassifyImportsArePure in internal/textcheck, which reads
// internal/classify's source).
type DesignStep string

const (
	// DesignStepDecisions is the root issue's decisions document.
	DesignStepDecisions DesignStep = "decisions"
	// DesignStepCriteria is the root issue's Verification document.
	DesignStepCriteria DesignStep = "criteria"
	// DesignStepSpec is the root issue's primary document, the one document the gate held before a
	// deployment could name a list of steps.
	DesignStepSpec DesignStep = "spec"
)

// DesignSteps is every step, in the order a deployment that gates all three takes them: the
// decisions first, then the criteria they are judged by, then the spec built on both.
var DesignSteps = []DesignStep{DesignStepDecisions, DesignStepCriteria, DesignStepSpec}

// OrSpec is step itself, and DesignStepSpec when step is empty. A fact, a request or a row built
// with no step is of the spec step: spec is the step every deployment requires — `root-issues`
// resolves to it alone, and a deployment that gates nothing still holds its row — so a reader
// that defaults asks for the row the writers write (Postgres.PutGate, which defaults the same
// way). Every place that reads a step which may be empty asks this, so the rule is written once.
func (step DesignStep) OrSpec() DesignStep {
	if step == "" {
		return DesignStepSpec
	}
	return step
}

// Valid says whether step is one of the three steps. The stored column admits those three names
// alone, so a value this refuses is one no record can hold.
func (step DesignStep) Valid() bool {
	return slices.Contains(DesignSteps, step)
}

// Document is the document of its issue the step records, as a refusal or a task names it: the
// words the architect's own guide uses for the three (skill://legion-architect/references/
// design-gate.md, "Which steps are gated"). The caller supplies the possessive it needs - the
// issue's, or the architect's own.
func (step DesignStep) Document() string {
	switch step {
	case DesignStepCriteria:
		return "Verification document"
	case DesignStepDecisions:
		return "decisions document"
	}
	return "primary document"
}

// StepNames is the names of steps, in their order, for a caller that joins them into a sentence or
// hands them to Dispatch. It is never nil, so a hand-off that writes JSON writes an empty list
// rather than null.
func StepNames(steps []DesignStep) []string {
	names := make([]string, len(steps))
	for i, step := range steps {
		names[i] = string(step)
	}
	return names
}

// StepWords is the three step names as a refusal spells them: each quoted, the last after "or".
// Every refusal of a value that is not one of them - the `gates.design` list a config file names,
// the step list a daemon answers the controller with - says this, so no refusal spells the three
// names of its own.
func StepWords() string {
	quoted := StepNames(DesignSteps)
	for i, name := range quoted {
		quoted[i] = "'" + name + "'"
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

// DesignGate records the current document version, the version a human approved, if any, with
// where Dispatch recorded that approval (ApprovedMark, zero when the read that carried it gave
// none), and the changes request the architect has been told of, if any: its document version, the
// reviewer's reason and the review's place in the issue's Dispatch event log (ToldChangesMark,
// zero the same way). A re-read of that same changes request, by a later registration or the gate
// seed, tells the architect nothing new; another review does, even at that version with that
// reason. An approval clears the told record and a changes request clears the approval, so the two
// marks never stand together: the one that is set is the gate's newest applied review
// (classify.LatestReview).
type DesignGate struct {
	Issue string
	// Step is the document of Issue this gate holds, and the second half of the record's key: one
	// issue holds one gate for each step its deployment gates.
	Step               DesignStep
	ArtifactID         string
	LatestVersion      int
	ApprovedVersion    *int
	ApprovedMark       EventMark
	ToldChangesVersion *int
	ToldChangesReason  string
	ToldChangesMark    EventMark
	// EverOpened records that the step has not held the tree's first release in this generation
	// of the issue: a human approved the gate's document version at some point, whether or not
	// the gate stands open now, or the step joined the tree after that release — a Verification
	// document its architect registered as criteria once the spec had opened the tree
	// (workflow's gateRegistered), which that release never waited on. Nothing else remembers
	// it: classify.DesignGateOpen is point-in-time, reading the approval against LatestVersion
	// as both stand, and a changes request clears ApprovedVersion, so a gate that opened and was
	// sent back reads exactly like one that never opened. Under the children-first rule a root
	// stays at admitted while its children run, which is the state the later rule must tell from
	// a root still waiting for its first approval. It lives on the gate row rather than on the
	// issue because ClearTreeGeneration deletes the gate when a root is re-admitted, and a
	// re-admitted root's architect writes its spec again: the mark belongs to the generation
	// whose release it records.
	EverOpened bool
}

// SpecGate is the DesignStepSpec row of gates, nil when the issue holds none. Every reader that
// still asks one question of one gate takes this entry: a deployment resolves `root-issues` to the
// single step spec, so that row is the gate those readers mean.
func SpecGate(gates map[DesignStep]DesignGate) *DesignGate {
	gate, ok := gates[DesignStepSpec]
	if !ok {
		return nil
	}
	return &gate
}

// RootAsk is the daemon's mirror of Dispatch's state of one ask of a root, written from the ask's
// own events alone. Each part has a fence of its own, the issue's event sequence of the newest
// event that gave it, so an older event changes none of that part and the same events in any order
// leave the same row: Open is fenced by Seq (0 while no event has said whether the ask is open),
// WaitingOn by WaitingOnSeq, and ClosedBy by ClosedSeq.
//
// WaitingOn is the turn Dispatch's own rule gives the ask after its newest event that names one
// ("" when none has, which on an open ask is the person's: an ask opens on it): a reply's turn, the
// person's for a reply on a closed ask (Dispatch takes the newest reply's turn, and a reply on a
// closed ask records none, so the person's), the person's for a hand-back, and the agent's for a
// version that moved an approval request past the version handed to the person. A close names no
// turn, so a closed ask keeps it, and an ask Dispatch's settlement restores reopens on it. ClosedBy
// is who made the ask's newest close, at ClosedSeq, kept when the ask reopens: a reply on a closed
// ask says it is closed but not who closed it, so a closed row whose close has not arrived yet has
// no ClosedBy, and one reopened and closed again by such a reply shows the earlier close until the
// later one arrives. Kind (AskQuestion, AskApproval, "" until an event names a kind this daemon
// knows), Artifact (the document an approval request asks a person to approve, "" for a question)
// and ByTree (a session of the root's tree opened it) are facts of the ask, kept whatever order its
// events arrive in. A design gate's approval request is an approval row whose Artifact is the
// gate's document: the mirror needs no binding to know it. PersonAnswerSeq is the newest answer of
// the ask by a person, and PersonSeq the newest other act of a person that would answer a root
// waiting on that person (workflow's personAct); PersonActSeq is the newest of the two that does.
//
// Decline is a person's decline to answer the ask, nil where nobody declined it: the resume
// prints Dispatch's own words and the person's note from it (workflow's resumeTask), so the
// daemon keeps no copy of those words and the two packages cannot drift apart. A decline is a
// fact of the ask, kept whatever order its events arrive in, and nothing takes it off the row:
// the question was closed with no answer, and no later event of that ask says otherwise, since
// Dispatch refuses every transition of a resolved ask and its settlement restores a retracted
// ask alone.
type RootAsk struct {
	Issue           string
	Ask             string
	Kind            string
	Artifact        string
	Open            bool
	Seq             int64
	WaitingOn       string
	WaitingOnSeq    int64
	ClosedBy        string
	ClosedSeq       int64
	ByTree          bool
	PersonSeq       int64
	PersonAnswerSeq int64
	Decline         *AskDecline
}

// AskDecline is a person's decline to answer an ask, which Dispatch records as the resolution
// kind `declined`: Note is the person's own note ("" where they wrote none) and Words the
// sentences Dispatch wrote beside it for the asking agent (Dispatch's model.DeclineDispatchWords),
// each as Dispatch recorded it. The two are one value because they are one act: a reader asks
// whether a decline is there, not whether three fields agree. It travels whole, from the
// Dispatch event (intake's DispatchAskClosed) through the mirror row (RootAsk.Decline) to the
// task the root's architect resumes with.
type AskDecline struct {
	Note  string
	Words string
}

// The ask kinds the mirror knows (RootAsk.Kind): an ordinary question, and the approval request
// of a document (a design gate's request among them).
const (
	AskQuestion = "question"
	AskApproval = "approval"
)

// Who closed an ask (RootAsk.ClosedBy).
const (
	ClosedByPerson     = "person"
	ClosedByTree       = "tree"
	ClosedBySettlement = "settlement"
	ClosedByOther      = "other"
)

// WithHuman says whether the ask is open and waits on the person.
func (a RootAsk) WithHuman() bool { return a.Open && a.WaitingOn != "agent" }

// GateRequestOf says whether the ask is an approval request of one of gateArtifacts, the
// documents design gates of the root are registered on. A root holds one gate for each step its
// deployment gates, so a caller that means one step passes that step's document alone.
func (a RootAsk) GateRequestOf(gateArtifacts ...string) bool {
	if a.Kind != AskApproval || a.Artifact == "" {
		return false
	}
	return slices.Contains(gateArtifacts, a.Artifact)
}

// PersonActSeq is the newest act of a person on the ask that would answer a root waiting on that
// person, 0 for none: every act PersonSeq records, and an answer of the ask unless it is an
// approval request of one of gateArtifacts, the documents the root's design gates are registered
// on (none for a root with no gate), whose answer is a review that answers through its own
// artifact.* event. It reads the row's own facts and those documents as they stand, so it gives
// the same whatever order the ask's events, the gates' registrations and their reviews arrive in.
func (a RootAsk) PersonActSeq(gateArtifacts ...string) int64 {
	if a.GateRequestOf(gateArtifacts...) {
		return a.PersonSeq
	}
	return max(a.PersonSeq, a.PersonAnswerSeq)
}

// Question says whether the ask is an ordinary question (AskQuestion): an approval request, and
// an ask no event has named a kind this daemon knows of, are not.
func (a RootAsk) Question() bool { return a.Kind == AskQuestion }

// Slot is an active admission slot.
type Slot struct {
	Issue      string
	Index      int
	AdmittedAt time.Time
}

// OutboxRow is one effect that has not yet been completed. A claimed row carries a non-empty
// lease token and its expiry; callers must present that exact token to finish or retry it.
type OutboxRow struct {
	ID   int64
	Kind OutboxKind
	// Issue is the issue the effect belongs to. A project-level row carries the bare project key
	// instead — the deployment's gated-steps publish, which belongs to no issue, and a project
	// pause's stop of a claim of another project's issue (SuperviseRequest.Issue) — and the same
	// split_part(issue, '-', 1) that scopes an issue's row to its own daemon answers that key
	// itself, since a project key holds no hyphen.
	Issue      string
	Payload    json.RawMessage
	Attempts   int
	NextAt     time.Time
	LastError  string
	CreatedAt  time.Time
	LeaseToken string
	LeaseUntil *time.Time
	// HeldByPause is the id of the project pause that held the row, and 0 for a row no pause
	// held. The resume writes it, in its own transaction, on every row the pause it ends holds
	// (Postgres.MarkPauseHeldRows), whatever the row's kind or due time, and nothing else writes it:
	// a row that runs after the resume carries it, so the wake the row takes of a released role
	// is that pause's held wake (internal/daemon's wakeForDelivery, store.Store.TakePodWake).
	HeldByPause int64
}

// Handover is a root linger's pending move of one child to an independent tree. It persists from
// the decision until the daemon-authored Dispatch event consumes or rejects it.
type Handover struct {
	Issue      string
	Tree       string
	Generation uint64
	Cancelled  bool
}

// ReleaseRetry is the bounded wait for a released todo child whose parent event has not reached the
// daemon yet. A changed Dispatch sequence begins a fresh bounded attempt.
type ReleaseRetry struct {
	Issue     string
	Sequence  int64
	Attempts  int
	NextAt    time.Time
	Exhausted bool
}

// HandedOverRoot is a root a hand-over created: the generation at which the child became a root of
// its own, carrying its phase, phase rows and pull request, and whether its old root's gate came
// with them.
type HandedOverRoot struct {
	Issue       string
	Generation  uint64
	GateCarried bool
}

// DispatchIssueRow is one row of the daemon's copy of Dispatch's issues: what the readings of
// Dispatch said about an issue of the project, whether or not the daemon records the issue itself.
// Each of its five values, Status, Parent, Blockers, Labels and Rank, is the one shown by the
// reading of the highest sequence that carried it, the first such reading where two share that
// sequence, and keeps that reading's sequence beside it in the table (PutDispatchIssue); Seq is
// the highest sequence of any reading the row took. Blockers are the keys the issue waits on, in
// Dispatch's own order, Parent the key of its Dispatch parent ("" for none), Labels its labels and
// Rank its board rank. A value no reading has written holds its column's default: "", no parent,
// or no keys or labels. ClaimedBy is who Dispatch showed holding the issue's claim, `<kind>:<id>`
// ("" for nobody), and ClaimSeq the sequence of the reading that showed it, 0 when none has. The
// claim is kept apart from the row, with a fence of its own, since a claim event says nothing of
// the issue's values and may arrive before the issue's first reading (PutDispatchClaim).
type DispatchIssueRow struct {
	Key       string
	Project   string
	Status    string
	Blockers  []string
	Seq       int64
	ClaimedBy string
	ClaimSeq  int64
	Parent    string
	Labels    []string
	Rank      string
}

// Carried is one value a reading of Dispatch carries; its zero value is a value the reading does
// not carry, which the write leaves as the copy holds it.
type Carried[T any] struct {
	Value   T
	Carried bool
}

// Carry is value, carried.
func Carry[T any](value T) Carried[T] { return Carried[T]{Value: value, Carried: true} }

// DispatchReading is one reading of a Dispatch issue at Seq, the sequence Dispatch had run to when
// it was read: the values it carries, and the claim it showed (ClaimSeq above zero). Applied says
// the reading is an issue fact the daemon applies through its handlers — an issue event, or a view
// intake let through — rather than a reading that decides nothing: a listing, the periodic turn's
// re-read, or the release's read of the children it adopts.
type DispatchReading struct {
	Key, Project string
	Seq          int64
	Status       Carried[string]
	Parent       Carried[string] // Value "" is Dispatch showing no parent
	Blockers     Carried[[]string]
	Labels       Carried[[]string]
	Rank         Carried[string]
	ClaimedBy    string
	ClaimSeq     int64
	Applied      bool
}

// Store contains only transactional operations. The caller owns each transaction so intake can
// atomically deduplicate an event, update the record, and enqueue all of its effects.
type Store interface {
	MarkProcessed(ctx context.Context, tx pgx.Tx, source, eventID string) (fresh bool, err error)
	// Processed reports whether source's event eventID is already marked (MarkProcessed), for a
	// caller that must read the mark rather than take it: a catch-up that finds itself superseded
	// reads whether it published what it named before deciding to forget it or hand it over.
	Processed(ctx context.Context, tx pgx.Tx, source, eventID string) (bool, error)
	Issue(ctx context.Context, tx pgx.Tx, key string) (*Issue, error)
	Issues(ctx context.Context, tx pgx.Tx) ([]Issue, error)
	// LiveIssues is every record of project that is not finished, by key: each not in phase done,
	// each set back to todo, and each done one that still holds a slot. It is what admission and
	// every per-fact read take, so a fact's cost does not grow with the done records a project
	// keeps; a done record is read only by key (Issue, IssuesOf) or by tree (TreeIssues).
	LiveIssues(ctx context.Context, tx pgx.Tx, project string) ([]Issue, error)
	// IssuesOf is the record of each of keys that is an issue of project, by key, leaving out a key
	// with no record; no keys read nothing.
	IssuesOf(ctx context.Context, tx pgx.Tx, project string, keys []string) ([]Issue, error)
	// AncestorsOf is the record of each of keys and of every issue above it on the chain of
	// recorded parents, by key, for the issues of project; it walks the chain in one query
	// whatever its depth. It is what the waiting line's pass reads of the issues its live read
	// leaves out, so the one chain walk (Ancestors) crosses a parent already done. No keys read
	// nothing.
	AncestorsOf(ctx context.Context, tx pgx.Tx, project string, keys []string) ([]Issue, error)
	// QueuedRootsUnder is every labelled todo root whose Ancestry holds key, by key: the roots the
	// waiting line may hold under a live root on a chain key is on (Waiting), whose Ancestry an
	// event of key reads again.
	QueuedRootsUnder(ctx context.Context, tx pgx.Tx, key string) ([]Issue, error)
	// SlottedIssues is the record of each issue of project that holds an admission slot, in slot
	// order, in one read: another project's slot in the shared table, and every issue holding no
	// slot, are left out of the read rather than of the caller's loop.
	SlottedIssues(ctx context.Context, tx pgx.Tx, project string) ([]Issue, error)
	// PutIssue writes issue, refusing one without its repository, default branch or GitHub's
	// spelling of the repository, one whose GitHub spelling names another repository, and one that
	// would give a recorded issue another of any of them. This release's rename pass writes a
	// configured rename's new name on a recorded issue through RenameIssueRepository instead, the
	// one writer that changes a recorded repository, which dispatch://INFRA-3532's own change removes.
	PutIssue(ctx context.Context, tx pgx.Tx, issue Issue) error
	// MarkIssueMoved records at as when the daemon last saw key move (Issue.MovedAt), the one
	// writer of that column: the write sites hand it the fact's own moment, and PutIssue names the
	// column nowhere, so no stale copy of an issue can write a movement away.
	//
	// It is monotone — a time at or before the recorded one writes nothing — because the facts
	// that move an issue are not delivered in the order they were made: a GitHub webhook held up
	// behind a redelivery, or a boot that applies a listing beside live events, would otherwise
	// take the record back to an older moment and make a tree read as quieter than it is. An
	// unrecorded key matches no row and writes nothing, which is what a fact of an issue the
	// daemon does not run must do.
	MarkIssueMoved(ctx context.Context, tx pgx.Tx, key string, at time.Time) error
	// MarkTreeStalledTold records at as the movement the daemon has told root's tree stopped
	// for (Issue.TreeStalledToldAt), the one writer of that column: the admission pass hands it
	// the tree's newest movement in the transaction that queues the notice, and PutIssue names
	// the column nowhere, so no stale copy of a root can write the mark away.
	//
	// It is not monotone, because it is not a movement of its own: it is the movement that was
	// told about. The rule only ever tells a tree whose newest movement stands after the mark
	// (admit's tellStalledTrees), so each write carries the row forwards anyway, and a key with
	// no record matches no row and writes nothing.
	MarkTreeStalledTold(ctx context.Context, tx pgx.Tx, root string, at time.Time) error
	// UnfilledIssues is every record of the Dispatch project key project written before the record
	// carried its repository, by key; FillIssueRepository fills one such record, once.
	UnfilledIssues(ctx context.Context, tx pgx.Tx, project string) ([]Issue, error)
	FillIssueRepository(ctx context.Context, tx pgx.Tx, key, repo, defaultBranch, githubRepo string) error
	// UnnamedIssues is every record of the Dispatch project key project that carries its repository
	// but was written before the record carried GitHub's spelling of it, by key; NameIssueRepository
	// names one such record on repo, once, with a spelling of that repository, refusing another as
	// FillIssueRepository does.
	UnnamedIssues(ctx context.Context, tx pgx.Tx, project string) ([]Issue, error)
	NameIssueRepository(ctx context.Context, tx pgx.Tx, key, repo, githubRepo string) error
	// HandedOffWithoutOpenOrMergedPullRequest is every record of the Dispatch project key project in
	// implementing, named on its repository as GitHub spells it, whose implementer has recorded its
	// handoff and which has no open or merged pull request recorded, by key, each with its
	// implementer's handoff commit: an issue whose completion waits for an open pull request record,
	// which the daemon's start reads GitHub for and tells the architect of when GitHub lists none
	// on its branch. A merged record waits for none: a completion moves on it, and its merge told
	// the architect.
	HandedOffWithoutOpenOrMergedPullRequest(ctx context.Context, tx pgx.Tx, project string) ([]HandedOff, error)
	// RepoNote is what the daemon last said on issue about its repository, nil for nothing;
	// PutRepoNote records a note it posts.
	RepoNote(ctx context.Context, tx pgx.Tx, issue string) (*RepoNote, error)
	PutRepoNote(ctx context.Context, tx pgx.Tx, note RepoNote) error
	// PutDispatchIssue writes reading into the copy of Dispatch's issues. A reading writes only the
	// values it carries, and each value keeps the sequence of the reading that wrote it: a reading
	// changes a value only at a higher sequence than that value's own, or where no reading has
	// written it (sequence 0), so a reading at a value's own sequence replaces and removes nothing,
	// and an older one changes nothing at all. A value the reading does not carry stays as the copy
	// holds it, and on a new row takes its column's default at sequence 0, written by no reading.
	// A parent of "" is Dispatch showing no parent, a value written like any other. The row's Seq
	// becomes the highest sequence of any reading it took. An applied reading (Applied) also raises
	// the row's applied sequence to its own where that stands lower, whatever its values did, and
	// any other reading leaves it (DispatchIssueApplied). The claim is fenced apart, on ClaimSeq,
	// whatever the values did: a reading carrying a claim (ClaimSeq above zero) writes it only over
	// an older claim, and one carrying none leaves the stored claim as it stands. A reading whose
	// Seq is zero — a fact the daemon minted from its own record, which Dispatch never said — writes
	// nothing at all, and one that carries no value is refused, as a fault of its caller.
	PutDispatchIssue(ctx context.Context, tx pgx.Tx, reading DispatchReading) error
	// TakeDispatchRefill takes project's refill mark (a dispatch_issue_refills row whose
	// refilled_at is null): it stamps refilled_at, deletes every dispatch_issues row of project, and
	// answers the keys it deleted and true. With no mark it changes nothing and answers false.
	TakeDispatchRefill(ctx context.Context, tx pgx.Tx, project string) (removed []string, refilled bool, err error)
	// PutDispatchClaim writes holder (`<kind>:<id>`, "" for nobody) as key's claim at seq, over an
	// older claim only, and touches nothing of the status or the blockers. It is kept whether or not
	// the mirror has a row of key yet: an event carries the claim as of its own sequence, so a claim
	// event applied before an older issue event (one nak'd and redelivered) must outlast that
	// event's older claim. A claim of a key with no row is read by nothing until the key's row
	// arrives (DispatchIssues lists rows alone), so admission never sees an issue without its
	// status and blockers. A seq of zero writes nothing.
	PutDispatchClaim(ctx context.Context, tx pgx.Tx, key, holder string, seq int64) error
	// DispatchIssues is every mirrored row of the Dispatch project key project, by key.
	DispatchIssues(ctx context.Context, tx pgx.Tx, project string) ([]DispatchIssueRow, error)
	// DispatchIssuesOf is the mirrored row of each of keys and the row of each issue those rows
	// say they wait on, by key, read by primary key: what a judgement of keys reads
	// (blockers.Load), whatever the number of rows the project's mirror holds. No keys read
	// nothing. It names no project: a key is read whatever project it is in, since an issue may
	// wait on an issue of another project and a blocker whose row the judgement cannot read
	// counts as unfinished for ever.
	DispatchIssuesOf(ctx context.Context, tx pgx.Tx, keys []string) ([]DispatchIssueRow, error)
	// DispatchChildren is every mirrored issue of the Dispatch project key project whose parent is
	// one of parents, grouped by parent and in key order within each group; a parent with no
	// children has no entry, and no parents reads nothing. A later step reads it to decide whether
	// a parent's own first phase waits for a child of its own that is still open.
	DispatchChildren(ctx context.Context, tx pgx.Tx, project string, parents []string) (map[string][]DispatchIssueRow, error)
	// DispatchClaim is who the claims mirror shows holding key's claim (`<kind>:<id>`, "" for
	// nobody), "" when it holds no claim of key. It reads the claim alone (PutDispatchClaim), with
	// or without a mirrored row of the issue: a claim event applied before the issue's first row is
	// the newest claim Dispatch showed.
	DispatchClaim(ctx context.Context, tx pgx.Tx, key string) (string, error)
	// DispatchIssueApplied is the highest sequence of an issue fact of key the daemon applied (a
	// reading PutDispatchIssue took as Applied), 0 when it applied none or the copy holds no row of
	// key: blockers.Mirror writes every issue fact the daemon applies first, in the fact's own
	// transaction, whether or not the daemon records the issue, and no listing or turn's re-read
	// moves it.
	DispatchIssueApplied(ctx context.Context, tx pgx.Tx, key string) (int64, error)
	Phases(ctx context.Context, tx pgx.Tx, issue string) ([]PhaseRow, error)
	// PhaseEntries is the entry each phases row of issues names, the moment that entry was raised
	// and whether its task is one to send as written (PhaseRow.Entry, PhaseRow.EntryRaisedAt,
	// PhaseRow.EntryVerbatim), by issue and role, read in one statement. Admission's sweep weighs
	// every role it holds against that number (admit's owedEntryRoles), reads the mark to tell
	// which roots past admitted its architect reach keeps (workflow.ArchitectAtWork), and reads the
	// moment beside them to tell how long a role has been owed the work the number names (admit's
	// architectStalls), and a read an issue would cost one round trip a slot on every pass of the
	// project (the pass rule on admit's Admission.Apply), which a project running hundreds of trees
	// pays hundreds of times over. A row the record does not hold is absent from the answer, which
	// reads as the zero entry: a role no transition has ever handed work on.
	PhaseEntries(ctx context.Context, tx pgx.Tx, issues []string) (map[PhaseRole]PhaseEntry, error)
	PutPhase(ctx context.Context, tx pgx.Tx, phase PhaseRow) error
	// PullRequest, PullRequestByBranch and PullRequestByNumber read one pull request whole, what it
	// remembers of its settled heads with it, for a fact's handling that may write the record back
	// or weighs a settlement against that memory.
	PullRequest(ctx context.Context, tx pgx.Tx, issue string) (*PullRequest, error)
	PullRequestByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*PullRequest, error)
	PullRequestByNumber(ctx context.Context, tx pgx.Tx, repo string, number int) (*PullRequest, error)
	// PullRequestRow, PullRequestRowByBranch and PullRequestRowByNumber read what PullRequest,
	// PullRequestByBranch and PullRequestByNumber read, by the same query, as the pull request's
	// row alone: none of what it remembers of its settled heads, and a record PutPullRequest
	// refuses, naming the read. PullRequestRow is the read of one pull request by its issue for
	// every reader that never writes the record back.
	PullRequestRow(ctx context.Context, tx pgx.Tx, issue string) (*PullRequest, error)
	PullRequestRowByBranch(ctx context.Context, tx pgx.Tx, repo, branch string) (*PullRequest, error)
	PullRequestRowByNumber(ctx context.Context, tx pgx.Tx, repo string, number int) (*PullRequest, error)
	// ClearedPullRequestClock is the clock (updated_at) of the newest lifecycle event pull request
	// number of repo had applied when a new generation's clear deleted its row (ClearGeneration),
	// repo compared without case as PullRequestByNumber compares it, and the zero time when no clear
	// deleted it or its row held no clock.
	ClearedPullRequestClock(ctx context.Context, tx pgx.Tx, repo string, number int) (time.Time, error)
	// OpenPullRequests is every open pull request of the Dispatch project key project's issues.
	OpenPullRequests(ctx context.Context, tx pgx.Tx, project string) ([]PullRequest, error)
	// OpenPullRequestsOnBase is every open pull request of repo recorded on the base branch base,
	// by issue: the pull requests one push onto that branch may have taken behind or dirty.
	OpenPullRequestsOnBase(ctx context.Context, tx pgx.Tx, repo, base string) ([]PullRequest, error)
	// PullRequestsOf is the pull request of each of issues, by issue key; an issue the record
	// holds no pull request for has no entry, and no issues reads nothing. It is one query
	// whatever the number of issues, for a caller that must name the pull request of every
	// issue of a tree and may make no read per issue (admit's tellStalledTrees). It answers a
	// merged and a closed pull request as well as an open one, which OpenPullRequests cannot:
	// a tree that has stopped with its work merged is exactly the tree such a caller reports. It
	// loads no settled heads, and PutPullRequest refuses its records.
	PullRequestsOf(ctx context.Context, tx pgx.Tx, issues []string) (map[string]PullRequest, error)
	// PutPullRequest writes pr whole, what it remembers of its settled heads with it, and refuses a
	// record that a list read or a row read made, which loads none of that memory
	// (PullRequest.SettledHeads says why).
	PutPullRequest(ctx context.Context, tx pgx.Tx, pr PullRequest) error
	// SetPullRequestBaseMoved writes issue's pull request's BaseMoved, after, and no other field:
	// the one write a list read's record calls for (workflow's basePush), which PutPullRequest
	// refuses.
	SetPullRequestBaseMoved(ctx context.Context, tx pgx.Tx, issue, after string) error
	// ProductionCheck is issue's production check, nil for none; PutProductionCheck writes it,
	// one per issue.
	ProductionCheck(ctx context.Context, tx pgx.Tx, issue string) (*ProductionCheck, error)
	PutProductionCheck(ctx context.Context, tx pgx.Tx, check ProductionCheck) error
	// OpenProductionChecks is every open check, oldest opened first, then by issue;
	// OpenProductionChecksOfTree those of tree's issues, in the same order.
	OpenProductionChecks(ctx context.Context, tx pgx.Tx) ([]ProductionCheck, error)
	OpenProductionChecksOfTree(ctx context.Context, tx pgx.Tx, tree string) ([]ProductionCheck, error)
	// SessionClaimsTree says whether the agent session holds a claim in the tree.
	SessionClaimsTree(ctx context.Context, tx pgx.Tx, tree, session string) (bool, error)
	// ArchitectIssueOfSession is the issue of the architect claim that the agent session holds in
	// the tree, and whether it holds one. It reads the claim in any state, as SessionClaimsTree
	// does: a suspended architect's session is still the session of its claim, and a write in
	// Dispatch names that session.
	ArchitectIssueOfSession(ctx context.Context, tx pgx.Tx, tree, session string) (string, bool, error)
	// SessionClaimsAnywhere says whether the agent session holds a claim in any tree the daemon
	// records. It is what tells a holder the daemon records in another tree from one it records
	// nowhere, which SessionClaimsTree's one boolean cannot.
	SessionClaimsAnywhere(ctx context.Context, tx pgx.Tx, session string) (bool, error)
	// ProjectClaims is every claim of the project that stands on an issue, as the quiet measure
	// reads one (TreeClaim): the issue, the claim's state and its two step times, in no order. It
	// is what the admission pass's own measure reads (admit's quietTrees) — one read of what the
	// agents of every tree are doing, for a pass that judges every tree of the project and may
	// make no read per tree.
	//
	// It exists because no read of this shape does. The claims of a whole project are read in
	// exactly one other place, the claim store's ClaimsTx (internal/store), which reads every
	// claim of the database with no `where` clause at all and decodes every column of each,
	// locators included — the state report's read, shared with it by the daemon alone. Every
	// other claim read of this record is by token (RoleClaims) or by issue, and a pass that asked
	// one of them per issue would make a query per issue of the project.
	//
	// project is the claim project token (claim.ProjectToken): `claims.project` holds the token,
	// lowercased and alphanumeric, not the Dispatch project key the issue records carry (the
	// claim store's IssueRunsElsewhere reads the same column the same way), so the caller
	// converts. A claim on no issue — the project's own controller holds one — is left out by the
	// query, since it stands in no tree and the measure could place it nowhere.
	ProjectClaims(ctx context.Context, tx pgx.Tx, project string) ([]TreeClaim, error)
	// AddParkedDecision keeps a relayed decision under the issue it was decided on, for the next
	// start or catch-up of the tree that issue stands in. A ref that issue already keeps keeps its
	// newest decision: a later one, by At, replaces it, KEPT again, so a changes request the
	// person has since followed with an approval of the same spec is not what the start names.
	AddParkedDecision(ctx context.Context, tx pgx.Tx, decision ParkedDecision) error
	// ParkedDecisions is every decision owed to tree — every row whose issue stands in tree now,
	// in any state — newest first, ties by ref. It is what the close of a tree reads: the tree
	// ends owing each of them (workflow's warnKeptDecisionsOfAClosedTree).
	ParkedDecisions(ctx context.Context, tx pgx.Tx, tree string) ([]ParkedDecision, error)
	// UnclaimedKept is what a start or catch-up of tree may name: every row whose issue stands in
	// tree now and that no counting claim or hold holds — a claim counts only while the claiming
	// outbox row's issue stands in the same tree, and a hold only while the holding claim's tree
	// is that tree — newest first, ties by ref, and tree's own readmitted approval on the same
	// rule, the zero KeptApproval when there is none to name
	// (workflow.Engine.ParkedDecisionTask).
	UnclaimedKept(ctx context.Context, tx pgx.Tx, tree string) ([]ParkedDecision, KeptApproval, error)
	// ClaimKeptDecisions claims for outbox row what kept names of what tree is owed and nothing
	// counting claims or holds yet, in the transaction that enqueues row: from then on no other
	// start or catch-up of that tree names it until that row settles what it named. A published
	// catch-up tells it (ForgetKeptDecisions); a start whose task the claim queued leaves it to
	// that delivery, which holds it until its confirmation tells it; a start or catch-up that
	// reached no architect hands it over (ReleaseKeptClaims, workflow.HandOverKept), and a row's
	// deletion releases it as a last resort (claimed_by is set null with the row), for the next
	// start or catch-up to name.
	ClaimKeptDecisions(ctx context.Context, tx pgx.Tx, tree string, kept KeptDecisions, row int64) error
	// MoveKeptClaims moves every claim of outbox row from to row to: the re-hold's copy of a catch-up
	// carries the claims of the row it copies, which then finishes.
	MoveKeptClaims(ctx context.Context, tx pgx.Tx, from, to int64) error
	// ReleaseKeptClaims unclaims every parked decision and readmitted approval outbox row holds,
	// and reports the tree each released row is owed to now, by key: the hand-over's first step,
	// since the row told no architect what it named, and what says which trees' architects the
	// hand-over must write a fresh catch-up to (workflow.HandOverKept).
	ReleaseKeptClaims(ctx context.Context, tx pgx.Tx, row int64) ([]string, error)
	// ReleaseHeldDecisions clears the hold the delivery of claim token holds on the rows marks
	// names, and reports the tree each released row is owed to now, by key: the send-time drop of
	// what a held task names of another tree leaves those rows KEPT for that tree to name
	// (workflow's RootTask). A retired delivery's holds go the same way, in the write that retires
	// it (the claim store's RetireDelivery).
	ReleaseHeldDecisions(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string, marks []DecisionMark) ([]string, error)
	// HeldKeptRefs is the reference of every decision the delivery of claim token still holds: what
	// a held task may still name when it is about to be sent (workflow's RootTask). A reference it
	// named that is gone from here was told by something else, or replaced by a newer decision on
	// the same reference, and this task names it no longer.
	HeldKeptRefs(ctx context.Context, tx pgx.Tx, token claim.Token, delivery string) ([]string, error)
	// ArchitectLaunches is the launch each tree's root architect claim stands at now, by tree, for
	// the trees that have one: what a catch-up queued to another tree's architect is written for
	// (record.CatchUp.Launch), so a failed forward of an older one is not queued again behind it.
	ArchitectLaunches(ctx context.Context, tx pgx.Tx, trees []string) (map[string]uint64, error)
	// ArchitectServed is what the root architect claim of tree has served: the issue generation
	// of the last confirmed delivery carrying one that the claim retired, and the phase entry of
	// the last confirmed delivery carrying one (supervise.Claim.ServingGeneration and
	// ServedEntry), each zero where the tree has no architect claim or its claim has retired no
	// confirmed task carrying one. It is what tells a tree whose architect ran before
	// (workflow.ArchitectStartTask's fourth rule). The launch ArchitectLaunches reads cannot tell
	// it: claims.generation counts the claim's launches, and the claim is kept from one run to
	// the next.
	ArchitectServed(ctx context.Context, tx pgx.Tx, tree string) (uint64, int64, error)
	// TreeMembersRun is whether an issue of tree other than its root stands past
	// phase.Admitted in the workflow (InWorkflow): its status is none of OutOfWorkflowStatuses
	// and its phase is neither admitted nor done. Those are the members a re-admitted tree's first
	// run starts again, its promotion or the admission pass that finds it running first (admit's
	// startMidPhaseMembers, which passes over a
	// member at admitted and one out of the workflow), and a member a person set todo once its
	// own first phase has started. It is what says a root architect's tree has work under way
	// whatever its claim has served (workflow.ArchitectStart.MembersRun). It answers one bool
	// from an exists over the tree's own issues (issues_tree), where TreeIssues would read every
	// member's whole record, done ones included.
	TreeMembersRun(ctx context.Context, tx pgx.Tx, tree string) (bool, error)
	// KeepReadmittedApproval keeps, for root's next architect start, the step whose document a
	// person approved, the version they approved and that document's latest version, which its
	// re-admission found above the approved one before it cleared the old generation's gate; a
	// later keep replaces it, KEPT again. The step is what the start task names the document by,
	// so an architect told to request approval again is sent to the document the person reviewed.
	KeepReadmittedApproval(ctx context.Context, tx pgx.Tx, root string, approval KeptApproval) error
	// DropReadmittedApproval deletes the pair root keeps, if any: a re-admission whose gate holds no
	// stale approval, or under gates.design off, leaves nothing an earlier one kept.
	DropReadmittedApproval(ctx context.Context, tx pgx.Tx, root string) error
	// ReadmittedApproval is the pair root keeps, in any state, zeros when it keeps none.
	ReadmittedApproval(ctx context.Context, tx pgx.Tx, root string) (approved, latest int, err error)
	// TellChildrenFirst marks that the architect that owns issue has been told, for the run
	// generation of tree, that the issue's own first phase waits for the Dispatch children under
	// it, and reports whether this call is the telling. That architect is the root's own for a
	// root. For a child it is the architect above it, since the child's own starts only with that
	// first phase, unless a person set the child back to todo while it ran or was held: that
	// re-entry starts and stops nothing (workflow's ReenterChild), so a child's own architect that
	// held its role still holds it and is the one told. The wait itself is never stored, so that a
	// stale mirror row costs one pass; the telling is, so that the next pass (the pass rule on
	// admit's Admission.Apply, and the boot's) does not tell it again. A re-admission opens a new
	// generation of the tree and so tells it anew, and so does a hand-over, which gives the issue a
	// tree of its own.
	TellChildrenFirst(ctx context.Context, tx pgx.Tx, issue, tree string, generation uint64, at time.Time) (bool, error)
	// TellArchitectsStalled marks that the project's controller has been told each of stalls,
	// for the run its generation names of its tree, and answers, at each index of stalls,
	// whether this call is the telling of that one: a stall already marked, or one an earlier
	// index of the same call marked, answers false, so the one row decides who tells. Each arm has
	// a mark of its own, so a root two arms read wakes the controller twice and one arm hides no
	// other. Each arm is told once, because the pass that raises them runs again and again (the
	// pass rule on admit's Admission.Apply), and a wake that pass repeats would bury the
	// controller's topic. A re-admission opens a new generation and tells it anew, as
	// TellChildrenFirst does, and the tree is in the key for the same reason: a generation names a
	// run within one tree alone. It is one statement however many stalls it marks, and none for no
	// stall, since every pass of the project calls it
	// (admit's TestAFactAsksAsManyStatementsWhateverNumberOfSlotsIsHeld).
	TellArchitectsStalled(ctx context.Context, tx pgx.Tx, stalls []ArchitectStall) ([]bool, error)
	// ArchitectStallsUntold says whether any of stalls holds no mark yet, which is whether the
	// pass TellArchitectsStalled is called from would tell the controller anything. It writes
	// nothing, so the entry sweep's gate asks it in its read-only transaction (admit's
	// OwesEntryWork), in one statement, and in none for no stall.
	ArchitectStallsUntold(ctx context.Context, tx pgx.Tx, stalls []ArchitectStall) (bool, error)
	// ForgetKeptDecisions tells what outbox row's published catch-up named of what tree is owed
	// (KeptDecisions), of the rows row claims or nothing claims: every parked decision still
	// standing at the reference and time it named — a newer decision on the same reference, kept
	// since and unclaimed, survives — and the readmitted approval still standing at the pair of
	// versions it named. One whose issue has left tree since the row was composed is told nothing:
	// its claim and hold are cleared instead, so the tree it stands in now names it. A row whose
	// task reached no architect tells nothing and hands over instead (ReleaseKeptClaims).
	ForgetKeptDecisions(ctx context.Context, tx pgx.Tx, tree string, kept KeptDecisions, row int64) error
	// ClaimSession is the session claims records for token, "" and no error when the token holds
	// no row. The workflow reads it for the claim of the worker on the reviewer's step (workflow's
	// reviewersClaim), in the reviewer's first round as in every later one, to tell the reviewer's
	// own review from another review-App session's by the Legion footer each carries
	// (PullRequestReview.LegionSession). The session is recorded there when the reviewer's agent
	// registers; the claim the outbox makes holds none until then.
	ClaimSession(ctx context.Context, tx pgx.Tx, token claim.Token) (string, error)
	// ClaimLaunchedSince is whether claims holds token at a generation above generation: the
	// claim's generation when a launch of it was refused, so a claim held at a later one has
	// launched since (each try of a launch writes the claim at the next generation first, supervise's
	// writeTry). It locks the claim's row for the rest of tx (for update), so a launch whose first
	// write comes after this read waits for tx's commit. False where the token holds no row, and
	// for generation 0, which names no refused launch.
	ClaimLaunchedSince(ctx context.Context, tx pgx.Tx, token claim.Token, generation uint64) (bool, error)
	// ClearGeneration drops the facts one generation of an issue owns: a merged or closed pull
	// request, the last generation's reading of a still-open one (the next generation runs on the
	// same branch and pull request), its design gate with what its holders took of it
	// (DesignTold), and each role's handoff, review rounds, verdict and summary. Each role keeps
	// its claim and its last handoff, so a
	// commit an earlier generation reported is never new again. An issue whose gate rows it
	// deletes has the empty set of design-gate documents queued for it, due at now, since the set
	// Dispatch holds for that issue is the one those rows recorded.
	ClearGeneration(ctx context.Context, tx pgx.Tx, issue string, now time.Time) error
	ClearTreeGeneration(ctx context.Context, tx pgx.Tx, tree string, now time.Time) error
	// RaiseStepStarts counts one more start of place on issue, marks its run open, and answers
	// the write of that start's record, which run builds from the count with that start in it:
	// the start of a step's record raises it in its own transaction (workflow's startStep), and
	// the row keeps the agent, the moment and the identity that write carries. ClearGeneration
	// and ClearTreeGeneration leave the count: the generation is part of the record's id already,
	// and the count only rises.
	RaiseStepStarts(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, run func(starts int64) StepRecord) (StepRecord, error)
	// StepStarts is the count of starts of place on issue with the agent, the moment and the run
	// of the latest, unchanged: the zero StepStart where the place has no row.
	StepStarts(ctx context.Context, tx pgx.Tx, issue string, place StepPlace) (StepStart, error)
	// OpenStep is the open run of a step of issue whose start wrote the latest moment, of the
	// several an issue can have open (Postgres.OpenStep), and the zero StepEntry where none is open.
	OpenStep(ctx context.Context, tx pgx.Tx, issue string) (StepEntry, error)
	// EndStepStart marks the open run of place on issue ended and answers its start and true, in
	// the transaction that writes the run's end (workflow's endRun); false where that run has its
	// end already, so one run gets one end.
	EndStepStart(ctx context.Context, tx pgx.Tx, issue string, place StepPlace) (StepStart, bool, error)
	// OpenUncountedStep marks open, at the daemon's boot, the row of a run at place that no start
	// of this release counted — at the count 0 for a place with no row, the next count for one
	// that reads RunUncounted — keeping the write run builds from that count as a start keeps its
	// own, and answers true; it queues nothing (workflow's OpenUncountedRuns). False, and nothing
	// written, where the place's row is open or ended.
	OpenUncountedStep(ctx context.Context, tx pgx.Tx, issue string, place StepPlace, run func(starts int64) StepRecord) (bool, error)
	// StepRuns is every row of starts of each of issues, by issue and place, in one read: what the
	// end of a step's run reads of the issues one fact touched (workflow's settleRuns).
	StepRuns(ctx context.Context, tx pgx.Tx, issues []string) ([]IssueStepRun, error)
	// PutPlan stores version v of the plan of v's issue at v's generation, with the tasks it
	// gives and their steps. The same version given twice is refused, and so is a step of a task
	// it does not give. A task the generation holds already takes the version's text and
	// attributes and keeps where it stands (MoveTask, StopTask, PutReturnCounts); the steps of
	// each task it gives are its steps alone.
	PutPlan(ctx context.Context, tx pgx.Tx, v PlanVersion, tasks []PlanTask, steps []PlanStep) error
	// Plan is the plan of issue at generation: its highest version, every task and every step.
	// nil where the generation has no plan rows: an issue that ran before plans, and a generation
	// no plan reached yet.
	Plan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) (*StoredPlan, error)
	// PlanStepsOf is every plan_steps row of issue, of every generation, each with the name of its
	// agent, ordered by generation, task and place: the earlier rows NameAgents names a version's
	// agents from.
	PlanStepsOf(ctx context.Context, tx pgx.Tx, issue string) ([]PlanStep, error)
	// KeyNames is every name a key's agent held on issue (plan_key_names), the lasting record that
	// no deletion of a plan removes: NameAgents reserves each for its key beside the plan_steps rows.
	KeyNames(ctx context.Context, tx pgx.Tx, issue string) ([]KeyName, error)
	// IssueClaims is every stored claim of issue in project (the claims' project token), in any
	// state, with its stored role, ordered by token: what a walk over every claim of an issue
	// reads, so each row it writes names one stored claim.
	IssueClaims(ctx context.Context, tx pgx.Tx, project, issue string) ([]IssueClaim, error)
	// OwnersNewestPlan is the newest plan in force, of any generation, generation 0 included, that
	// an architect of issue's tree now gave (ArchitectOfTree); nil where no owner gave one in force.
	OwnersNewestPlan(ctx context.Context, tx pgx.Tx, issue Issue) (*StoredPlan, error)
	// PlansOfEarlierOwners is the version in force of each generation of issue whose giver is no
	// architect of the issue's tree now (ArchitectOfTree): the plans its first step passes over.
	PlansOfEarlierOwners(ctx context.Context, tx pgx.Tx, issue Issue) ([]PlanVersion, error)
	// PlansOfOwners is the version in force of each generation of issue, generation 0 included,
	// whose giver is an architect of the issue's tree now (ArchitectOfTree), each carried copy
	// among them: the plans a first step of an issue whose group takes none passes over.
	PlansOfOwners(ctx context.Context, tx pgx.Tx, issue Issue) ([]PlanVersion, error)
	// CarryPlan copies the plan of issue at from to to: with carried, each task where it stands;
	// without, each task waiting at no act with its counts at 0. It writes nothing where from has
	// no plan rows.
	CarryPlan(ctx context.Context, tx pgx.Tx, issue string, from, to uint64, carried bool) error
	// DeletePlan deletes the plan of issue at generation from its three tables.
	DeletePlan(ctx context.Context, tx pgx.Tx, issue string, generation uint64) error
	// DeleteTasks deletes each of tasks, waiting or stopped, with its steps.
	DeleteTasks(ctx context.Context, tx pgx.Tx, issue string, generation uint64, tasks []string) error
	// MoveTask writes where task stands and its state, which is not stopped (StopTask), and
	// clears a stop's reason.
	MoveTask(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, place TaskPlace, state TaskState) error
	// StopTask writes task stopped for reason, at the place it stands.
	StopTask(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task, reason string) error
	// ReturnCounts is the counts of returns of task: on the task for a generation with plan rows,
	// on the implementer's phases row for one with none.
	ReturnCounts(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string) (ReturnCounts, error)
	// PutReturnCounts writes the counts of returns of task where ReturnCounts reads them, and
	// nothing else of that row.
	PutReturnCounts(ctx context.Context, tx pgx.Tx, issue string, generation uint64, task string, c ReturnCounts) error
	// PlanTasksOf is every task of the plan of each issue of the Dispatch project at the issue's
	// current generation, in one read; an issue with no plan rows at that generation has none.
	PlanTasksOf(ctx context.Context, tx pgx.Tx, project string) ([]PlanTask, error)
	// GatesOf is every design gate issue holds, by step: one entry for each step the issue's
	// architect has registered. An issue with no gate at all gives an empty map, and a step with
	// no row has no entry.
	GatesOf(ctx context.Context, tx pgx.Tx, issue string) (map[DesignStep]DesignGate, error)
	// Gates is the design gates of each of issues, by issue and then step, in one read: the
	// gates one look over many trees judges from (ReadLineFacts), rather than a read per tree
	// under the fact lock. An issue with no gate has no entry.
	Gates(ctx context.Context, tx pgx.Tx, issues []string) (map[string]map[DesignStep]DesignGate, error)
	// GatedIssues is the key of each issue of project that holds at least one design gate and that
	// Dispatch does not show as done, in key order: the issues a pass over every recorded gate
	// publishes (the daemon's boot publication), read in one query rather than one per record. An
	// issue with no gate, and one Dispatch shows done, is left out.
	GatedIssues(ctx context.Context, tx pgx.Tx, project string) ([]string, error)
	// GatesForArtifact is every gate registered on artifactID, in issue order: the document owner's
	// gate and each copy a hand-over carried to a child of its tree. Each row carries its own step.
	GatesForArtifact(ctx context.Context, tx pgx.Tx, artifactID string) ([]DesignGate, error)
	// PutGate writes one gate row, keyed by its issue and its step. A gate built with no step is
	// written as DesignStepSpec (Postgres.PutGate).
	PutGate(ctx context.Context, tx pgx.Tx, gate DesignGate) error
	// TreeTold is every told row of the issues of tree (DesignTold), by issue and then audience,
	// as each audience took it, the credit of every confirmed delivery not yet final counted in
	// (Postgres.TreeTold).
	TreeTold(ctx context.Context, tx pgx.Tx, tree string) ([]DesignTold, error)
	// BaselineTold writes issue's audience row at the approved versions root's gate rows hold in
	// this transaction, when issue holds no such row: the version work starts from
	// (Postgres.BaselineTold).
	BaselineTold(ctx context.Context, tx pgx.Tx, issue string, audience DesignAudience, root string, at time.Time) error
	// RaiseTold raises issue's audience row past each of items, from its From (nil for none) to its
	// To, as a final credit that reaches only a row that took its From, writing the row when there
	// is none; a step items do not name keeps what the row holds (Postgres.RaiseTold).
	RaiseTold(ctx context.Context, tx pgx.Tx, issue string, audience DesignAudience, items []NewsItem, at time.Time) error
	// SettleOpening settles the gate's opening on issue's audience row as it reaches the audience
	// now: owed, for the task that carries its news to lead with, or told by the opened role
	// message; the row takes its next opening number either way (Postgres.SettleOpening).
	SettleOpening(ctx context.Context, tx pgx.Tx, issue string, audience DesignAudience, owed bool) error
	// CloseOpenings settles the gate's close on every told row of tree's issues: none owes an
	// opening, and each takes its next opening number (Postgres.CloseOpenings).
	CloseOpenings(ctx context.Context, tx pgx.Tx, tree string) error
	// FillTold writes each step of versions on issue's audience row where the row knows no
	// version of that step, and leaves every step it knows as it stands (Postgres.FillTold).
	FillTold(ctx context.Context, tx pgx.Tx, issue string, audience DesignAudience, versions map[DesignStep]int, at time.Time) error
	// DesignNewsPending says whether a reconcile of tree's news (DesignNewsReconcile) is queued and
	// has not begun to run at now (Postgres.DesignNewsPending).
	DesignNewsPending(ctx context.Context, tx pgx.Tx, tree string, now time.Time) (bool, error)
	// DesignChangesOf is the recorded change set of each of pairs that has one, by pair.
	DesignChangesOf(ctx context.Context, tx pgx.Tx, pairs []VersionPair) (map[VersionPair]DesignChanges, error)
	// PutDesignChanges records the change set of one pair; a pair already recorded read keeps
	// its row (Postgres.PutDesignChanges).
	PutDesignChanges(ctx context.Context, tx pgx.Tx, changes DesignChanges) error
	// DesignHistoryUnreadOf is the recorded unread review history of each of questions that has
	// one, by question, as the rows stand; a row counts only while no reconcile of the tree that
	// asks it is pending, which its caller weighs (DesignHistoryUnread).
	DesignHistoryUnreadOf(ctx context.Context, tx pgx.Tx, questions []DesignHistoryQuestion) (map[DesignHistoryQuestion]DesignHistoryUnread, error)
	// PutDesignHistoryUnread records that the review history could not answer one question,
	// replacing the reason and time an earlier reconcile recorded of it.
	PutDesignHistoryUnread(ctx context.Context, tx pgx.Tx, unread DesignHistoryUnread) error
	// ClearDesignHistoryUnread deletes what the record holds of question's unread review history,
	// once the history has answered it.
	ClearDesignHistoryUnread(ctx context.Context, tx pgx.Tx, question DesignHistoryQuestion) error
	// RootAsk is the mirror row of ask on issue, nil when it has none.
	RootAsk(ctx context.Context, tx pgx.Tx, issue, ask string) (*RootAsk, error)
	// PutRootAsk writes ask's mirror row, unless any of the stored row's fences (Seq, WaitingOnSeq,
	// ClosedSeq) or person's acts is newer than ask's: the caller merges an event into the row it
	// read, and a row it did not read is never rolled back.
	PutRootAsk(ctx context.Context, tx pgx.Tx, ask RootAsk) error
	// RootAsks is the mirror rows of each of issues, by issue and then ask, in one read.
	RootAsks(ctx context.Context, tx pgx.Tx, issues []string) (map[string][]RootAsk, error)
	// StartHandout records that the daemon gives issue's architect work now: HandoutSeq rises to
	// the highest Dispatch sequence the record holds of the issue (its last observation, the claim
	// its Dispatch mirror shows, its wait's start, every fence and person's act of its mirrored
	// asks, its gate's reviews), DeclaredSeq clears, and an issue with no catch-up cursor (recorded
	// before it existed) gets one at 0, so its next catch-up reads from this handout.
	StartHandout(ctx context.Context, tx pgx.Tx, issue string) error
	// TakeInThrough records work the daemon gives issue's architect about an event at seq (a
	// design-changes notice): HandoutSeq rises to seq, and DeclaredSeq clears only when it is at or
	// below seq, so a hand-back newer than that event, applied first, still stands.
	TakeInThrough(ctx context.Context, tx pgx.Tx, issue string, seq int64) error
	// Declare records a hand-back to a person at mark: DeclaredSeq becomes the greater of itself and
	// mark.Seq, and DeclaredAt the newest hand-back's time.
	Declare(ctx context.Context, tx pgx.Tx, issue string, mark EventMark) error
	// ClearDeclared clears DeclaredSeq: nothing the architect handed back is still with the person.
	ClearDeclared(ctx context.Context, tx pgx.Tx, issue string) error
	// CaughtUpSeq is the Dispatch sequence the boot catch-up last applied issue's event log
	// through, and whether the issue has a cursor at all: an issue recorded before the cursor
	// existed and given no work by this release since has none (its first catch-up replays
	// nothing); one recorded since, or handed work since (StartHandout), starts at 0.
	CaughtUpSeq(ctx context.Context, tx pgx.Tx, issue string) (int64, bool, error)
	// SetCaughtUp raises issue's catch-up cursor to seq; it is never lowered.
	SetCaughtUp(ctx context.Context, tx pgx.Tx, issue string, seq int64) error
	// RestartCaughtUp sets issue's catch-up cursor back to 0, so its next catch-up reads from its
	// last handout: a re-admission's new generation, whose old generation's events are not its own.
	RestartCaughtUp(ctx context.Context, tx pgx.Tx, issue string) error
	// PutAssignments records rows, the answers drawn for issue, and reports how many rows it
	// wrote. Each is written once: an experiment issue already holds a row for keeps that row
	// whatever is passed for it now, so a later pass under another mode leaves a unit on the arm
	// it has been running under, and nothing ever updates a row. Writing nothing is no error, and
	// an issue the daemon holds no record of — an operator's spawn — takes no row at all.
	PutAssignments(ctx context.Context, tx pgx.Tx, issue string, rows []Assignment) (int, error)
	// CarryAssignments settles the assignments of key, a child handed over as a tree of its own,
	// from the tree it left: every tree-unit assignment of from's root copied verbatim — its arm,
	// its mode and the instant it was drawn — and then each experiment of settled, the experiments
	// declared at the hand-over, written at no variant, no mode and no instant for the experiments
	// the copy left unwritten. Every row is marked carried, and every write is the write-once one,
	// so an experiment the old tree held a tree row for keeps the copy, one it held none for is
	// settled at no variant and is never drawn for later, and a row key already holds stands. It
	// reports how many rows it wrote.
	//
	// An issue-unit row of the old root is left where it is: it is that one issue's own answer,
	// shown to no other issue of the tree and read back under its own key alone, so the child
	// ran in no arm of that experiment before the hand-over and is settled at none by it.
	//
	// project is the normalized runtime token tree_lifecycles is keyed by, whose row for from is
	// the fact that the old tree was opened at all: a child out of a tree the daemon never opened
	// carries nothing, and its own first draw answers for it.
	CarryAssignments(ctx context.Context, tx pgx.Tx, project, key, from string, settled []DeclaredExperiment) (int, error)
	// AssignmentsFor is the assignments of each of issues, by issue and then experiment, in one
	// read; an issue with no assignment has no entry, and an empty list reads nothing. It is the
	// read of the one or two keys a launch asks for.
	AssignmentsFor(ctx context.Context, tx pgx.Tx, issues []string) (map[string][]Assignment, error)
	// Assignments is every assignment the record holds for the issues of the Dispatch project,
	// by issue and then experiment, in one read: what the state's projection asks, which shows
	// every recorded issue of its own daemon's project its own.
	Assignments(ctx context.Context, tx pgx.Tx, project string) (map[string][]Assignment, error)
	Slots(ctx context.Context, tx pgx.Tx) ([]Slot, error)
	PutSlot(ctx context.Context, tx pgx.Tx, slot Slot) error
	ReleaseSlot(ctx context.Context, tx pgx.Tx, issue string) error
	// ReleaseSlotForWait takes issue's slot away because it waits on another issue and records
	// that it did, in one statement: the row is deleted and Issue.ReleasedForWaitAt set to at, so
	// no pass can ever read a row gone with no mark, which would say the issue never started, or
	// a mark beside a row, which would say it waits while it holds its place against the cap.
	ReleaseSlotForWait(ctx context.Context, tx pgx.Tx, issue string, at time.Time) error
	// ClearReleasedForWait sets Issue.ReleasedForWaitAt back to null: the issue holds a slot
	// again, or holds none for a reason that is not a wait.
	ClearReleasedForWait(ctx context.Context, tx pgx.Tx, issue string) error
	// ReleaseTreeSlots frees the slot of every issue of tree and clears Issue.ReleasedForWaitAt
	// from every issue of it, and answers the issues whose slot it freed, in slot order: the
	// re-admission of the tree's own root, which ends the generation those rows and marks were of
	// (admit's releaseTreeSlots).
	ReleaseTreeSlots(ctx context.Context, tx pgx.Tx, tree string) ([]string, error)
	// OweMembers sets Issue.MembersOwed on the root key: its re-admission has started a generation
	// whose members are still to be brought back (admit's readmit).
	OweMembers(ctx context.Context, tx pgx.Tx, key string) error
	// ClearMembersOwed sets Issue.MembersOwed back to false on the root key: its members came back
	// with its promotion, or with the pass that found its tree running first.
	ClearMembersOwed(ctx context.Context, tx pgx.Tx, key string) error
	// OpenTreeLifecycle binds an admission fact's root start rows to one durable tree epoch before
	// the fact commits. A reserved cleanup returns treelifecycle.ErrCleanupReserved without
	// admitting work.
	OpenTreeLifecycle(ctx context.Context, tx pgx.Tx, project, tree string, authority treelifecycle.Authority) (treelifecycle.Lifecycle, error)
	// TreeLifecycle is the tree's lifecycle under the project token, and whether it has one, read
	// without opening anything (treelifecycle.Read).
	TreeLifecycle(ctx context.Context, tx pgx.Tx, project, tree string) (treelifecycle.Lifecycle, bool, error)
	// TreeLifecycles is the lifecycle of each of trees, by tree, in one read, opening nothing: the
	// lifecycles a single look over many trees needs, rather than a read per tree under the fact
	// lock. A tree with no row has no entry, and no trees reads nothing. project is the claim
	// project token the tree_lifecycles table is keyed by, the value OpenTreeLifecycle takes, not
	// the Dispatch project key.
	TreeLifecycles(ctx context.Context, tx pgx.Tx, project string, trees []string) (map[string]treelifecycle.Lifecycle, error)
	// ControllerRegistered says whether a session holds the current controller registration of the
	// Dispatch project key project (controller.Record.Registered's rule), reading the controllers
	// table under the project token (claim.ProjectToken) `legion controller start` mints under.
	ControllerRegistered(ctx context.Context, tx pgx.Tx, project string) (bool, error)
	// ControllerNoticePending says whether the outbox of the Dispatch project key project still
	// holds an unpublished controller notice of kind, so a wake is never queued behind an
	// identical one. Issue "" reads every issue of the project, which is what a wake about the
	// project alone asks; an issue narrows the read to that issue's rows, and, for a kind whose
	// notice stands per claim (NoticePerClaim), a claim narrows it to the rows naming that claim
	// (ControllerNotice.Claim), or, where the notice names none, a role to that role's rows. A
	// claim and a role of "" narrow nothing further, so an issue alone reads every row of that
	// issue.
	ControllerNoticePending(ctx context.Context, tx pgx.Tx, project string, kind NoticeKind, issue string, role claim.Role, token claim.Token) (bool, error)
	// ControllerNoticesPending is that same read made of many issues at once: the issues among
	// issues whose own unpublished controller notice of kind the outbox of the Dispatch project
	// key project still holds, as a set keyed by issue. It is the one statement above with the
	// issue term taken as a list, in the caller's own transaction, so every issue's answer here is
	// the answer ControllerNoticePending gives it; role narrows each issue's rows to that role
	// exactly as it does there, and a role of "" narrows nothing. No issues at all reads nothing —
	// the query is not made — and a wake about the project alone, which names no issue, is the
	// single read's ask and not this one's.
	//
	// It is for the caller that must ask this of every root of one pass and may make no read per
	// root: the stalled-tree rule asks it of the trees it is about to tell (admit's
	// tellStalledTrees), on every pass (the pass rule on admit's Admission.Apply) and every boot.
	ControllerNoticesPending(ctx context.Context, tx pgx.Tx, project string, kind NoticeKind, issues []string, role claim.Role) (map[string]struct{}, error)

	// PersonHandoverPending says whether outbox still holds a dispatch_status row of issue whose
	// kind is PersonHandoverStatusRow: a finished row is deleted, so any such row
	// is one still to run or retrying. Admission and the workflow queue no second such row, and no
	// triage wake, for the issue while one is pending; the issue's other status rows, a hand-over
	// row among them, are not counted.
	PersonHandoverPending(ctx context.Context, tx pgx.Tx, issue string) (bool, error)
	// EnqueueGatedSteps writes the one gated_steps_publish row of publish's Dispatch project, due
	// at nextAt, with the project key as its issue. The boot publishes its configured list through
	// the outbox and keeps no record of what it last published, so the row the outbox holds is
	// replaced rather than left standing: a pending row can carry the list of the boot before this
	// one, and that is the list Dispatch would be shown. The table holds one such row per project
	// (20261106041346_gated_steps_publish), so a restart queues no second row, and a row that
	// already says this boot's list is left as it is.
	EnqueueGatedSteps(ctx context.Context, tx pgx.Tx, publish GatedStepsPublish, nextAt time.Time) error
	// EnqueueDesignGates writes the one design_gates_publish row of publish's issue, due at
	// nextAt, with the issue key as its issue. The row the outbox holds is replaced rather than
	// left standing: every change to a gate of that issue writes the whole set again, and a
	// pending row carries the set of the change before this one, which is the set Dispatch would
	// be shown. The table holds one such row per issue (20261110235320_design_gates_publish), so
	// a run of changes queues no second row, a row that already says this set is left as it is,
	// and a replacement takes the row from a lane holding it, whose send carries the older set.
	EnqueueDesignGates(ctx context.Context, tx pgx.Tx, publish DesignGatesPublish, nextAt time.Time) error
	// LiveRoleHolders is the session of each live claim of issue, by role, that the claim holds
	// (claim_sessions holds every name of it for that claim): the set a role-holder publication sends.
	LiveRoleHolders(ctx context.Context, tx pgx.Tx, issue string) ([]RoleHolder, error)
	// RecentClaimIssues is every issue of the Dispatch project with a live claim, or a claim that
	// changed at or after since, in key order: the issues the boot publishes.
	RecentClaimIssues(ctx context.Context, tx pgx.Tx, dispatchProject string, since time.Time) ([]string, error)
	Enqueue(ctx context.Context, tx pgx.Tx, row OutboxRow) error
	// EnqueueID enqueues row as Enqueue does and returns its id, for a row that claims what it names
	// (ClaimKeptDecisions). It refuses a ready_merge row, whose insert may update another.
	EnqueueID(ctx context.Context, tx pgx.Tx, row OutboxRow) (int64, error)
	// EnqueueStart writes a phase worker's start as a row of its own, taking out the issue's
	// oldest pending start of a phase worker — the supervise start that names a phase and
	// carries its task — that waits for what row waits for, the issue's admission slot
	// (SuperviseRequest.NeedsSlot) or nothing, and that no live lease holds, so however many
	// starts are written before the worker runs the issue holds one per wait, the later one, in
	// the place in the waiting line the first one took: the row written carries the earlier
	// row's creation time, its attempts, when it is next due and the error it last recorded, and
	// its own values where the issue owed no such row. A start that waits for a slot takes out
	// no start that waits for none, nor the other way round: the place in the line is the
	// waiting row's own, and a start claimable at once may not pass what its lane has queued
	// behind that row. It is never the earlier row rewritten — one outbox row is one task, which
	// is what a claim's record of the rows it has taken a task from rests on. A row a lane holds
	// now is not touched, including one it takes while the call waits on its row lock, and the
	// caller's row is written beside it. A lapsed lease holds nothing and is taken out like any
	// other.
	EnqueueStart(ctx context.Context, tx pgx.Tx, row OutboxRow) error
	// OwedStarts is every pending worker start of the Dispatch project key project that waits for
	// its issue's admission slot and that no live lease holds, a row whose lease has lapsed among
	// them, oldest first by creation time and then id — the waiting line's own ordering key for
	// the issues whose work came back (Waiting). An issue holding two of them appears twice,
	// oldest first.
	OwedStarts(ctx context.Context, tx pgx.Tx, project string) ([]OwedStart, error)
	// DueRows is every row of project a lane may run at now — due, under no live lease, in order
	// behind none of its issue's older rows, and, for a lifecycle row, with no other lifecycle row
	// of its issue under a live lease — oldest first, with no lease taken. Issue "" is every issue
	// of project, kinds nil every kind.
	DueRows(ctx context.Context, tx pgx.Tx, project, issue string, kinds []OutboxKind, now time.Time) ([]OutboxRow, error)
	// ClaimRow leases row id for leaseFor from now if a lane may still run it at now, in one
	// conditional update: false is another holder's lease, a lifecycle row of its issue since
	// leased, or a row no longer due, in order, or in the outbox. leaseFor must be positive.
	ClaimRow(ctx context.Context, tx pgx.Tx, id int64, now time.Time, leaseFor time.Duration) (OutboxRow, bool, error)
	// RenewOutboxLease moves the lease on row id to until while leaseToken holds it; renewed false
	// is a lease no longer the caller's, and gone is a row finished rather than leased by another.
	RenewOutboxLease(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, until time.Time) (renewed, gone bool, err error)
	// NextDue is the earliest next_at after now among issue's rows of kinds no live lease holds;
	// false is none. Kinds nil is every kind.
	NextDue(ctx context.Context, tx pgx.Tx, issue string, kinds []OutboxKind, now time.Time) (time.Time, bool, error)
	// BranchQueued says whether the outbox still holds an issue_branch row of issue, whatever its
	// lease or its next_at: the branch the row creates may not exist on GitHub yet.
	BranchQueued(ctx context.Context, tx pgx.Tx, issue string) (bool, error)
	// UnfinishedLanes is each (issue, lane) with a row of project still in the outbox, by issue
	// then lane.
	UnfinishedLanes(ctx context.Context, tx pgx.Tx, project string) ([]LaneKey, error)
	// HeldRows is every due, unleased lifecycle row of project held at now behind a lifecycle row
	// of its issue under a live lease — the oldest such; for a start fenced by its issue_branch
	// row, that row while it runs — by next_at then id. A start behind a branch row no one is
	// running is not listed. Issue "" is every issue of project.
	HeldRows(ctx context.Context, tx pgx.Tx, project, issue string, now time.Time) ([]HeldRow, error)
	// StartsWaitingForPod is, for each issue of project, the roles of the start rows that wait
	// because the issue's pod lacks the slot of the start's claim or the role of its step (its last
	// error begins with pods.StartWaitsForPodPrefix), distinct and in the order of their rows.
	StartsWaitingForPod(ctx context.Context, tx pgx.Tx, project string) (map[string][]claim.Role, error)
	// OpenIntakeGiveUps is what the state shows of project's open give-ups: each consumer's count
	// and newest stream sequence, and the newest records, at most newest, without their headers
	// and payload (intake_failures.go).
	OpenIntakeGiveUps(ctx context.Context, tx pgx.Tx, project string, newest int) (IntakeGiveUps, error)
	// FinishOutbox deletes row id while leaseToken holds it; a stale lease's finish is a no-op, so
	// a worker whose lease expired never removes the row its replacement now holds.
	FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error
	// FinishLeasedOutbox is FinishOutbox for a transaction that writes another effect with the
	// finish: a stale lease is ErrOutboxLeaseLost, so the caller rolls that effect back rather than
	// committing it beside the replacement worker's own.
	FinishLeasedOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error
	RetryOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string, nextAt time.Time, lastErr string) error
	PendingStatusWrites(ctx context.Context, tx pgx.Tx, project string) ([]OutboxRow, error)
	Handover(ctx context.Context, tx pgx.Tx, issue string) (*Handover, error)
	PutHandover(ctx context.Context, tx pgx.Tx, handover Handover) error
	CancelTreeHandovers(ctx context.Context, tx pgx.Tx, tree string, generation uint64) error
	DeleteHandover(ctx context.Context, tx pgx.Tx, issue string) error
	// DeleteHandoverAt deletes issue's fence only while it is the one tree and linger generation
	// name: a newer linger's fence on the same issue belongs to its own outbox row.
	DeleteHandoverAt(ctx context.Context, tx pgx.Tx, issue, tree string, generation uint64) error
	ReleaseRetry(ctx context.Context, tx pgx.Tx, issue string) (*ReleaseRetry, error)
	PutReleaseRetry(ctx context.Context, tx pgx.Tx, retry ReleaseRetry) error
	DeleteReleaseRetry(ctx context.Context, tx pgx.Tx, issue string) error
	// HandedOverRoot is issue's record as a root a hand-over created, nil for none;
	// PutHandedOverRoot records it.
	HandedOverRoot(ctx context.Context, tx pgx.Tx, issue string) (*HandedOverRoot, error)
	PutHandedOverRoot(ctx context.Context, tx pgx.Tx, root HandedOverRoot) error
	// WaitingNotices is every notice row of project's issues not yet due at now, oldest first.
	WaitingNotices(ctx context.Context, tx pgx.Tx, project string, now time.Time) ([]OutboxRow, error)
	// ExpediteOutbox makes row due at now if it was due later, returning whether it moved the row.
	// It takes the row, not its id, so a caller that wakes the row's lane has its issue and kind.
	ExpediteOutbox(ctx context.Context, tx pgx.Tx, row OutboxRow, now time.Time) (bool, error)
	// DropCatchUps deletes every catch-up notice row of issue the outbox still holds, a re-held copy
	// and a row the runner has leased included, but for two a drop would lose a person's decision
	// from. The first is a row whose publish told the decisions it named (KeptToldSource) and whose
	// forget has not committed yet: that row still owes them that forget, which its next run makes,
	// and dropping it would release them for the fresh catch-up to tell a second time. The second is
	// a row that carries kept decisions under a live lease (lease_until after now, which is the
	// caller's clock, the one a claim's lease is written from: ClaimRow): its lane may be publishing
	// it this moment, outside every transaction, and the drop would release what it named for the
	// fresh catch-up, written in the same transaction, to name again — telling the architect that
	// already has the publish in its hands a person's decision twice. Such a row is left for its own
	// ending to settle what it claims: its publish forgets exactly what it named
	// (ForgetKeptDecisions), a newer catch-up that supersedes it hands those rows over
	// (workflow.HandOverKept), and every other ending leaves them to the row's deletion, for the
	// architect's next ready to name. A claim of one of the issue's catch-ups and this drop are
	// serialized by an advisory lock keyed by the issue, which ClaimRow takes too: a claim either
	// commits before the drop reads the leases, and is seen, or it waits for the drop and claims
	// nothing when the drop deleted its row (TestDroppingCatchUpsLeavesTheRowALaneIsLeasing,
	// TestDroppingCatchUpsWaitsForTheClaimOfACopyAReholdQueued). The drop locks no outbox row before
	// its delete, since a lane finishing a catch-up waits on the decisions its caller holds
	// (TestDroppingCatchUpsWaitsOnNoRowAFinishIsDeleting). A leased row with no decisions is
	// dropped as before: the
	// runner executes it from memory, and the notice executor publishes it only while OutboxLeased
	// still finds it, so a dropped row is delivered only when the drop lands during its publish, a
	// duplicate the fresh catch-up follows. A dropped catch-up's decisions are released with its row
	// and named again by the fresh catch-up, written in the same transaction as the drop: only a
	// published catch-up forgets them.
	DropCatchUps(ctx context.Context, tx pgx.Tx, issue string, now time.Time) error
	// DropStatusWrites deletes every Dispatch status write of issue the outbox still holds, a row
	// the runner has leased included; one mid-attempt can still reach Dispatch, and its finish then
	// finds nothing to delete.
	DropStatusWrites(ctx context.Context, tx pgx.Tx, issue string) error
	// DropUnfencedCheckWakes deletes every check_wake row of issue the outbox still holds that
	// carries no wake of its own (CheckWakeDue.WakeAt): a row a release before that field queued,
	// which checkWakeDue cannot fence by the wake the check records. The workflow drops them where it
	// queues the row that replaces the check's wake (failedBetweenLooks). A row the runner has leased
	// is dropped too; its fact is the one applying, and its finish then finds nothing to delete.
	DropUnfencedCheckWakes(ctx context.Context, tx pgx.Tx, issue string) error
	// OutboxLeased reports whether row id is still in the outbox under leaseToken: false once
	// another transaction deleted it while the runner held it (DropCatchUps).
	OutboxLeased(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) (bool, error)
	// TreeIssues is every issue of tree, the root included, by key.
	TreeIssues(ctx context.Context, tx pgx.Tx, tree string) ([]Issue, error)
	// EarlierNotices is every unfinished notice row of tree's issues written before row id, oldest
	// first: the rows the fence reads, from which the daemon keeps those that go to the same
	// architect as the later one (its earlierNoticeFor), since a child's own architect and the
	// root's each take notices of the one tree.
	// Each row comes whole, its next_at among it, so the holder can wait for the moment the row it
	// waits behind is due rather than climb a backoff to it (the daemon's deliverNotice, whose
	// fence waits with wait.Until for a catch-up the wake pacing holds).
	EarlierNotices(ctx context.Context, tx pgx.Tx, tree string, id int64) ([]OutboxRow, error)
	// TreeCloses is every tree-close row (SuperviseRequest's "tree_close") queued for tree on an
	// issue that still stands in tree, oldest first.
	TreeCloses(ctx context.Context, tx pgx.Tx, tree string) ([]OutboxRow, error)
	// RoleRun reads one role's run of an issue generation: its queued supervise rows and the claim
	// with token.
	RoleRun(ctx context.Context, tx pgx.Tx, token claim.Token, issue string, role claim.Role, generation uint64) (RoleRun, error)
	// RoleRuns is RoleRun for a bounded batch, keyed by token. Each batch reads its queued starts
	// and claims from the caller's transaction snapshot rather than one query pair per role.
	RoleRuns(ctx context.Context, tx pgx.Tx, keys []RoleRunKey) (map[claim.Token]RoleRun, error)
	// ClaimQueued is every supervise row of issue at generation that reaches token when it runs,
	// whatever role its payload names, oldest first: a row that names the claim, and a row that
	// names none whose role's worker is token (WorkerClaimOf over the issue's phases rows, as the
	// outbox derives it). A keyed agent's claim holds the role of each of its steps in turn, so the
	// start of its next step reaches it as much as a start of the role it holds now, which RoleRuns,
	// keyed by one role, does not read.
	ClaimQueued(ctx context.Context, tx pgx.Tx, token claim.Token, issue string, generation uint64) ([]QueuedSupervise, error)
	// RoleClaims is the claims of tokens alone, keyed by token: RoleRuns' last statement without
	// the two before it, for a caller that judges a run by its claim and reads no queued start
	// beside it. A token the store holds no claim for is absent, the nil claim RoleRuns reports.
	RoleClaims(ctx context.Context, tx pgx.Tx, tokens []claim.Token) (map[claim.Token]*RoleClaim, error)
}

// RoleRunKey identifies one role's run of an issue generation.
type RoleRunKey struct {
	Token      claim.Token
	Issue      string
	Role       claim.Role
	Generation uint64
}

// RoleRun is what the store holds of one claim's run of a role of an issue generation: the role's
// supervise rows for the generation still queued that reach that claim, each a row that names its
// token, or one that names no claim where that claim is the worker on the role's step
// (WorkerClaim), which is the claim such a row reaches when it runs, oldest first (finishing
// deletes a row, so each is one the outbox has not run), and this daemon's claim of that token,
// nil when it has none. Two claims of one role are two runs: a row of the role is the run of one.
type RoleRun struct {
	Queued []QueuedSupervise
	Claim  *RoleClaim
}

// QueuedSupervise is one queued supervise row: its id, which orders it against the others and
// against the newest start run on the claim, its request, how many of its runs ended without
// success, whether a lane holds it under a live lease, and how long ago it was queued.
//
// Attempts is the row's attempts. The outbox adds one each time a run of the row ends without
// success and leaves the row queued for its backoff, a failure or a wait alike (wait.ErrWaiting:
// internal/daemon's outbox settle, then Store.RetryOutbox), and a run that succeeds deletes the
// row.
//
// Running is whether a lane holds a live lease on the row: its lease_token is set and its
// lease_until is after now(), so the outbox is running the row now. A lapsed lease holds nothing.
// A lane that dies mid-run records neither the row's finish nor its retry, so the row keeps that
// lane's token and a lease_until in the past until the outbox's sweep wakes its lane and the lane
// runs it again (internal/daemon's outbox_lanes.go); the store's own due predicate reads such a
// row as free (outboxUnleasedDue), and Running reads it as a row between runs, by the live-lease
// term the store already writes in Postgres.DropCatchUps: a token that is set and a lease_until
// after now.
// lease_until is the daemon's clock at the claim, or at the lane's latest renewal, plus the
// outbox's lease (internal/daemon's outboxLease), and it is compared here with the store's now(),
// as Age is; the two clocks agree on a deployment to far less than that lease. The
// other ends of a run leave no lease at all: Store.RetryOutbox clears it, and a finished row is
// deleted.
//
// Age is now(), the start of the reading transaction, less created_at, both in the store's own
// clock. A row a transaction that began later wrote reads a negative age, which is inside any
// bound. It is read from created_at because a row the outbox runs again in place keeps it, where
// next_at moves with every backoff. It is read in the store's clock because created_at is the
// database's now() at the start of the transaction that wrote the row, and subtracting it from
// the daemon's clock would read the skew between the two clocks as age (the same reason
// Notice.PacedFrom gives).
type QueuedSupervise struct {
	ID       int64
	Request  SuperviseRequest
	Attempts int
	Running  bool
	Age      time.Duration
}

// RoleClaim is a claim as RoleRun reads it: its state, the tree it is in, who suspended it while it
// is suspended, the newest start the outbox ran against it (last_start_row), the update round it is
// in (update_round), the write mark its retirement took (retired_at_write_mark), the phase entry
// it was last given work of (given_entry), the moment its agent last said it was ready
// (last_ready), its handshake of the last entry it was queued a task of (handshake_entry,
// handshake_generation, handshake_queued_at, handshake_confirmed_at), the moment its agent last
// stepped (step_at, turn_started_at), what its own session last reported it was doing
// (activity_at, activity_background, activity_next_reminder_at), and the task it holds, nil for
// none, with the task's delivery id, generation, phase, entry, the moments it was queued and
// confirmed, and whether a let-go took it back from a turn (interrupted). The id is what says the
// task is the daemon's own delivery of a relaunch's record (supervise.Delivery.Relaunch), which a
// reader of a run reads as no work of the claim's (supervise.HoldsTask: workflow's owed and
// readyWorkerTask).
type RoleClaim struct {
	State        supervise.ClaimState
	Tree         string
	SuspendedBy  supervise.SuspendSource
	LastStartRow int64
	// UpdateRound is the round the claim is in, "" when none is open: a pod round over its pod
	// or its own plugin round (supervise.Claim.UpdateRound). A rule over a run reads it to tell
	// a worker the round itself prompts, whose replacement is what starts it again, from one
	// nothing will prompt (workflow.SilentPhaseWorker).
	UpdateRound string
	// Step is the claim's record of its agent's last step as far as the two columns this read
	// takes it: At from step_at and TurnStartedAt from turn_started_at, both of them the zero
	// time where the column is null, and none of the call's own fields (supervise.StepRecord,
	// whose other columns say which call was open and are no rule's to read). It is the whole
	// record rather than one moment so that a reader asks it the one question the record
	// answers, supervise.StepRecord.LastStep, as the daemon's stall backstop asks it, and so no
	// second reading of "the last step" is written anywhere.
	//
	// What it is for: a rule over a run that must tell a worker still moving from one nothing
	// will prompt (workflow.SilentPhaseWorker, whose lastStep this is) needs the moment from
	// the record, because the one reader that has the live claim at hand is the sweep over the
	// supervisor, and admission has no claim of that supervisor at all (admit's silentWorkers).
	Step supervise.StepRecord
	// ActivityAt, ActivityBackground and ActivityNextReminderAt are the activity report of the
	// process this claim runs now (POST /legion/v1/claims/activity, store's PutClaimActivity):
	// when that session read what it was doing, zero for one that has reported nothing; whether
	// that reading found background work of the session's own running; and when the phase
	// reminder armed in that session is due, zero when none was live. The kinds that report
	// named (activity_kinds) are no part of this read: a rule over a run asks whether such work
	// is running, never what it is, where the idle release names the kinds in the hold it
	// records.
	//
	// A report an earlier process of this claim made is no report, and these three read as the
	// zero values that say nothing has been reported: the launch that replaced that process
	// cleared it (store's putClaim, which clears the four activity columns on every write that
	// moves the claim's generation). So a worker whose pod was replaced while a background task
	// of its own ran reads here as what it is — a session that has said nothing yet — rather
	// than as one still waiting on work its replacement killed.
	//
	// What they are for: a worker that waits on its own async job or subagent, and one whose own
	// phase reminder is still ahead of it, take no step and are no workers nothing will prompt
	// (workflow.SilentPhaseWorker). The idle release holds a pod on these same two readings
	// (workflow's IdleRelease, IdleHoldBackgroundWork and IdleHoldReminder).
	ActivityAt             time.Time
	ActivityBackground     bool
	ActivityNextReminderAt time.Time
	// RetiredAtWriteMark is the number outbox_write_mark_seq handed the write that recorded this
	// claim's retirement (store's putClaim, whose doc holds what that sequence orders and what it
	// does not), and 0 for a claim that has never retired and for one an earlier release retired,
	// which recorded the moment nowhere. What it is for: its one reader weighs the write mark of a
	// start row the issue owes against it, to tell a row the workflow wrote after the stop from one
	// already written when the stop was recorded (admit's stopOutlivedStart). It says nothing about
	// a claim that is not retired now, which is the only state that reader looks at.
	RetiredAtWriteMark int64
	// GivenEntry is the phase entry of the last task this claim was queued
	// (supervise.Claim.GivenEntry), and 0 for a claim that has been queued no task carrying one.
	// Its reader is the admission pass, which tells a claim behind the entry its role's phases
	// row names — one owed that entry's work — from one already holding it.
	GivenEntry int64
	// LastReady is when the claim's agent last said it could be prompted (supervise.Claim.LastReady,
	// last_ready), and the zero time for a claim whose agent never has. The ready of every launch
	// writes it, a relaunch's included (supervise's ready). Its readers are the first stall arm
	// (admit's architectTookNothing): a task waits for an agent that is up to take it, so while
	// the claim is ready, working or idle the arm counts the task from the later of its entry's
	// first queueing and this moment; and a production check's look bound after a project's
	// pause (internal/daemon's lookBoundAfterPause), which counts from no earlier than this moment
	// where it is after the pause's end.
	LastReady time.Time
	// Handshake is the claim's handshake of the last entry it was queued a task of
	// (supervise.Claim.Handshake): that entry, the run its task was of, when a task of it was
	// first queued to the claim and when a turn first confirmed one, the zero value where the
	// claim was queued no task carrying an entry. The claim's machine writes the queued moment
	// at the first queue of an entry and keeps it through every later task of the same entry and
	// run, a re-send after an unconfirmed retirement among them (supervise's EntryHandshake
	// queued), where the task's own QueuedAt is that later task's, and it keeps the first
	// confirmation through every later relaunch, but for one a refusal or a let-go says no turn
	// of the worker made (supervise's EntryHandshake unconfirmed). Its readers are the first
	// stall arm (admit's architectTookNothing), which counts the task of an entry from when that
	// entry was first queued, as R7 says (dispatch://INFRA-2614), and the third (admit's
	// architectSlowBeforeGate), which counts its bound from no earlier than that first
	// confirmation where there is one.
	Handshake supervise.EntryHandshake
	Pending   *supervise.Delivery
}

// TreeLingers says whether tree lingers after its close: its root is recorded with a linger
// deadline, which re-admission clears. Linger holds every member where it stood, so nothing in such
// a tree transitions, starts a worker, or records a completion.
func TreeLingers(ctx context.Context, store Store, tx pgx.Tx, tree string) (bool, error) {
	root, err := store.Issue(ctx, tx, tree)
	return root != nil && root.Lingers(), err
}

// ArchitectOfTree says whether architect is an architect of issue's tree now: the architect of the
// tree's root (issue.Tree), or of a member the record holds in that tree, a sub-architect. It is
// the one rule of who owns an issue, and both readers of that hold to it: an architect's call on a
// recorded issue (api's architectForIssue, register_plan, and rerun_child's childOfTree over the
// tree it read, ArchitectOfTreeIn) and the giver of a stored plan at the issue's first step
// (PlansOfEarlierOwners, which the workflow's gateOpened reads). So an
// architect whose issue moved to another tree, or whose tree the issue left, is no owner of it any
// longer, whatever tree its claim was registered in. A claim whose agent is not the architect is
// none; a member whose key yields no claim token holds no claim, so it matches nothing. The tree
// root's architect, the owner of most issues, is answered from issue alone; only another
// architect costs the read of the tree.
func ArchitectOfTree(ctx context.Context, store Store, tx pgx.Tx, architect claim.Token, issue Issue) (bool, error) {
	return architectOfTree(architect, issue, func() ([]Issue, error) { return store.TreeIssues(ctx, tx, issue.Tree) })
}

// ArchitectOfTreeIn is ArchitectOfTree over members, the records of issue's tree that the caller
// read already (Store.TreeIssues), so a request that judges more of the tree than its owner reads
// the tree once (api's childOfTree).
func ArchitectOfTreeIn(architect claim.Token, issue Issue, members []Issue) (bool, error) {
	return architectOfTree(architect, issue, func() ([]Issue, error) { return members, nil })
}

// architectOfTree is the rule of ArchitectOfTree, with members the records of issue's tree, read
// only where the architect is not the tree root's.
func architectOfTree(architect claim.Token, issue Issue, members func() ([]Issue, error)) (bool, error) {
	if _, agent, ok := architect.Cut(); !ok || agent != claim.Agent(claim.RoleArchitect) {
		return false, nil
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return false, fmt.Errorf("the architects of the tree of %s: %w", issue.Key, err)
	}
	architectOf := func(key string) bool {
		token, err := claim.NewToken(project, key, claim.RoleArchitect)
		return err == nil && token == architect
	}
	if architectOf(issue.Tree) {
		return true, nil
	}
	tree, err := members()
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(tree, func(member Issue) bool { return architectOf(member.Key) }), nil
}

// OwnerRule is which architects own an issue for an architect's request on it, the rule the
// request's route judges and its fact judges again under the fact lock (workflow's Engine.Apply):
// OwnerOfTree, every architect of the issue's tree (ArchitectOfTree), for register_plan and
// stop_task; or OwnerAbove, of those the issue's own architect or one above it in its tree
// (ArchitectAbove), for sign_off, request_backward_move, retry_or_escalate, close_root and
// register_gate.
type OwnerRule int

const (
	OwnerOfTree OwnerRule = iota
	OwnerAbove
)

// ArchitectAbove says whether architect owns issue for a request that only the issue's own
// architect or one above it in its tree makes: architect is the architect of issue, or of an issue
// above it through its recorded parents in its tree, and the tree root's architect owns every
// issue of the tree. So a sub-architect, which holds a grant for the whole tree and can name any
// issue of it, owns its own issue and those under it, never the root or a sibling. A chain that
// leaves the tree before it reaches architect's issue (no parent on a child, a parent not recorded
// or recorded in another tree, a cycle) ends at the tree root, whose architect owns the issue: a
// human's re-parent in Dispatch leaves an issue's tree as it was (admit's recordObservation),
// except a root that never ran, which joins its parent's live tree (workflow/adopt.go), and the
// notice router ends the same walk at the same place (OwningArchitect), so the architect the
// reviewer's phase-finished notice reaches is the one that may accept the issue. An architect this
// answers true for is an architect of the issue's tree (ArchitectOfTree), since the
// walk stays in that tree. The route judges it on its own read and the fact again under the lock,
// so the two never judge different rules.
func ArchitectAbove(ctx context.Context, store Store, tx pgx.Tx, architect claim.Token, issue Issue) (bool, error) {
	if _, agent, ok := architect.Cut(); !ok || agent != claim.Agent(claim.RoleArchitect) {
		return false, nil
	}
	project, err := claim.ProjectToken(issue.Project)
	if err != nil {
		return false, fmt.Errorf("the architects above %s: %w", issue.Key, err)
	}
	architectOf := func(key string) bool {
		token, err := claim.NewToken(project, key, claim.RoleArchitect)
		return err == nil && token == architect
	}
	seen := map[string]bool{}
	for current := &issue; ; {
		if architectOf(current.Key) {
			return true, nil
		}
		if current.Parent == nil || claim.IsTreeRoot(current.Key, current.Tree) || seen[current.Key] {
			return architectOf(issue.Tree), nil
		}
		seen[current.Key] = true
		parent, err := store.Issue(ctx, tx, *current.Parent)
		if err != nil {
			return false, err
		}
		if parent == nil || parent.Tree != issue.Tree {
			return architectOf(issue.Tree), nil
		}
		current = parent
	}
}

// PhaseRowOf is role's row among rows, and the zero row where the record holds no row of that
// role: a role no transition has ever handed work on, which stands on no entry, names no handoff
// and holds no claim. Every reader of one role's row among an issue's picks it here — the state
// the projection writes (projection.Project), the entry the admission pass weighs a claim against
// (admit's resumeSlottedWorkers), the outbox's fence on a start whose entry its role has left
// (internal/daemon's outbox), the completion route's position (api's handoffPosition), the claim
// of a role's worker (WorkerClaim) and the engine's own read (workflow's phaseRow) — so that rule
// is stated once and read the same way by all of them.
func PhaseRowOf(rows []PhaseRow, role claim.Role) PhaseRow {
	for _, row := range rows {
		if row.Role == role {
			return row
		}
	}
	return PhaseRow{}
}

// WorkerClaim is the claim of the agent that works role's step of issue now, read from the record:
// the claim role's row names (PhaseRow.Claim) while that row runs a step of the issue's plan
// (StepTask set), which the step's entry writes. A step's agent can be the agent of a key, whose
// claim is the one it started on an earlier step of another role, so a role's own token does not
// name it. Every other row gives that token, claim.NewToken(project, issue, role): an issue with no
// plan rows, a row a new generation cleared (ClearGeneration clears StepTask and keeps Claim), a
// row no entry was raised on, and a row the release before wrote. project is the project's token,
// as claim.NewToken takes it, and a role that is no role of Legion yields its refusal.
//
// Each derivation of the claim of a role's worker from an issue and a role calls it, so a row that
// names a role reaches the agent that works that role's step when the row runs.
func WorkerClaim(project, issue string, role claim.Role, rows []PhaseRow) (claim.Token, error) {
	if row := PhaseRowOf(rows, role); row.StepTask != "" && row.Claim != "" {
		return row.Claim, nil
	}
	return claim.NewToken(project, issue, role)
}

// WorkerClaimOf is WorkerClaim over entries, a batched read of many issues' phases rows
// (Store.PhaseEntries), for a pass over many issues that reads their rows in one statement.
func WorkerClaimOf(project, issue string, role claim.Role, entries map[PhaseRole]PhaseEntry) (claim.Token, error) {
	entry := entries[PhaseRole{Issue: issue, Role: role}]
	return WorkerClaim(project, issue, role, []PhaseRow{{Issue: issue, Role: role, Claim: entry.Claim, StepTask: entry.StepTask}})
}

// PhaseReader reads an issue's phases rows (Store.Phases), what ReadWorkerClaim needs of a store.
type PhaseReader interface {
	Phases(ctx context.Context, tx pgx.Tx, issue string) ([]PhaseRow, error)
}

// ReadWorkerClaim is WorkerClaim over issue's phases rows as tx reads them: the claim of the agent
// that works role's step of issue now, read in the caller's transaction. It is the one reader of
// that claim for a caller that holds no rows of its own — the engine (Engine.workerClaim), the
// open phase's worker (workflow.OpenWorker), the operator's routes (internal/api's workerToken) and
// the outbox's executors (internal/daemon's workerToken and lookBoundAfterPause) — so each reads
// the rows and the rule the same way.
func ReadWorkerClaim(ctx context.Context, phases PhaseReader, tx pgx.Tx, project, issue string, role claim.Role) (claim.Token, error) {
	rows, err := phases.Phases(ctx, tx, issue)
	if err != nil {
		return "", fmt.Errorf("read the phases rows of %s: %w", issue, err)
	}
	return WorkerClaim(project, issue, role, rows)
}

// Lingers says whether the root lingers after its close: its linger deadline is set, and
// re-admission clears it.
func (i Issue) Lingers() bool { return i.LingerUntil != nil }

// Lingering says whether the root's linger is still running at now: its deadline is set and now
// is before it. It is not Lingers, which answers whether a deadline was ever armed and stays
// true for ever after: nothing clears LingerUntil but a re-admission (admit's readmit), so a
// tree whose linger expired long ago, and a root a person parked, each still answer Lingers.
// Where a rule asks whether the tree is being torn down now, rather than whether it ever left
// the workflow, it asks this: the held list's tree_closed word (HeldReasonOf), which reads a
// parked root's member as tree_parked once that root's linger has run out, and the route that
// takes a closed tree's member into a live tree (workflow's AdoptionRefusal), which C4 of
// dispatch://INFRA-3107 opens only once no linger is left to run.
//
// The zero moment reads every armed linger as still running, which is the conservative answer: a
// reader with no clock of its own leaves the member where it stands rather than reading its tree
// as torn down already.
func Lingering(issue Issue, now time.Time) bool {
	return issue.LingerUntil != nil && now.Before(*issue.LingerUntil)
}

// LingersAt says whether the root lingers after the close of its generation: it lingers, and
// re-admission has not moved it to a later generation. A row a linger's expiry wrote acts in that
// linger only, not in the tree's next run nor in a later linger of it.
func (i Issue) LingersAt(generation uint64) bool {
	return i.Lingers() && i.Generation == generation
}

// Readmittable says whether a todo on this record starts a new generation of its tree: it is a
// root whose tree lingers after its close (Lingers), or a root that is done. A child's done is
// only the child's.
//
// It is the one definition of that question, because two readers decide opposite ways on it and
// neither may fire where the other does: admission re-admits such a todo at a new generation
// (admit's readmit), and the workflow releases a held root's hold on a todo that is not this one
// (workflow's releaseHeldRoot, whose branch reads `!issue.Readmittable()`). A case added to one
// expression and not to the other would let one todo do both.
func (i Issue) Readmittable() bool {
	return claim.IsTreeRoot(i.Key, i.Tree) && (i.Lingers() || i.Phase == phase.Done)
}

// Standing is where the issue stands for the gate every path that acts on an idle or released
// pod reads (pods.InWorkflowNotLingering).
func (i Issue) Standing() pods.Standing {
	return pods.Standing{Phase: i.Phase, Status: i.Status, Lingers: i.Lingers()}
}

// CarriesRun says whether the issue carries a run: it started a phase, and its tree neither left
// it admitted nor finished or parked it (phase done). It is one reading with two uses, and the
// only definition of it.
//
// A child moving to a root of its own carries its run there when it has one: a child still
// admitted never started under its old tree's gate, and a finished one (set back to todo during
// its tree's linger, say) has no run to carry — carried at done, its new root would take a slot
// and start an architect with nothing to run. Every path that moves a child to a tree of its own
// asks it: a hand-over's event, the outbox's direct consumption, and a person's labelling of an
// orphan. And the promotion never holds a candidate that carries a run by its links, whatever
// they say (blockers.Exempt); once it holds a slot, blockers.waitsOf reads its links like any
// other issue's.
func (i Issue) CarriesRun() bool { return i.Phase != phase.Admitted && i.Phase != phase.Done }

// HoldsQueued says whether work of role queued for phase queuedFor — a start, an owed start, or the
// task a start delivers — is the work of the place the issue stands at now: the issue stands in
// queuedFor and, at the phase step, at a step of role (StepIsOf). Every role a deployment defines
// runs as step, so a start or a task queued for the step of one role names a phase that still
// reads the same while the task stands at the step of another; the role is what tells the two
// apart. At step the role names the step: a role runs at most one step of a task, since a plan, an
// owner's or a project's default steps, that names a role at two steps is refused (plan.Supported),
// as the phases row of a role assumes too (PhaseRow.StepPlace). Away from step the phase alone is
// compared: one role works several phases (the implementer implementing and retro), and a phase
// names its one role. It is the one definition of that question, which the four judges of queued
// work ask, each on its own read: the outbox before it runs a start (daemon's supervise), the
// admission pass before it lets an owed start take a slot (admit's finishFencedStarts), the
// supervisor before it sends a queued task (PhaseHolds), and the supervisor again before it
// brings a claim back for a start (supervise.StartWork, through PhaseHolds).
func (i Issue) HoldsQueued(role claim.Role, queuedFor phase.Phase) bool {
	return i.Phase == queuedFor && i.StepIsOf(role)
}

// HoldsStart says whether the outbox's start row start is the work of the place the issue stands
// at now. A start that hands its role work (HoldsQueued) is held to the role of the step at the
// phase step; one that brings a role back and hands it nothing (SuperviseRequest.BringsBack) is
// held to the phase it names alone, as every start was before the phase step: a role of the
// member that ran another step of the task, or the member's own architect, comes back and is told
// nothing wherever the member stands. The outbox asks it before it runs a start (daemon's
// supervise); the supervisor's own judge of the same start (supervise.StartWork) is given none of
// that start's phase at step, since PhaseHolds would refuse it again by the role.
func (i Issue) HoldsStart(start SuperviseRequest) bool {
	if start.BringsBack() {
		return i.Phase == start.Phase
	}
	return i.HoldsQueued(start.Role, start.Phase)
}

// PhaseHolds says whether a task queued for role at phase queuedFor on run generation is still
// the work of issue, the record of the issue its claim works: the issue holds the work
// (HoldsQueued) and, for a task of a run (generation above 0), is on that run. An issue the record
// does not hold (nil) holds every task, since only a claim the workflow never made has none. It is
// the one definition of that question: the daemon answers supervise's Deps.PhaseHolds with it
// (daemon's workflowRuntime.phaseHolds, which says why each half is compared), and a test that
// wires the answer itself calls it too, so the rule it exercises is the one production runs.
func PhaseHolds(issue *Issue, role claim.Role, queuedFor phase.Phase, generation uint64) bool {
	return issue == nil || (issue.HoldsQueued(role, queuedFor) && (generation == 0 || generation == issue.Generation))
}

// ParentOf is an observed parent key as a record holds it: nil for none. Dispatch says "no parent"
// with an empty string, and the record says it with a nil pointer.
func ParentOf(key string) *string {
	if key == "" {
		return nil
	}
	return &key
}
