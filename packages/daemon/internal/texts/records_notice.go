package texts

import (
	"encoding/json"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// This file holds the records of the notices to the architect that owns an issue. A notice stays
// one record.Notice on the wire, so the json name of each field is a key of record.Notice, and a
// key has one Go type in every notice record. A key keeps the shape its readers take. A nested
// shape (the failure, the catch-up) is declared here, and record either names it as an alias
// (record.NoticeFailure) or keeps a copy of the same shape (record.CatchUp).
//
// A field that holds another author's words is a string with the struct tag texts:"words". A
// record with such a field declares the words key, the payload's words object, which names each
// words field that is set by its path (WordsPaths) with its author and its time.

// NoticeFailure is why a claim failed, as the notices of that failure name it
// (record.NoticeFailure is this type): the one word the site that failed the claim
// chose, the sentence beside it, the four counts against their limits, and what the runtime and the
// launcher last said of the claim's death and its start.
type NoticeFailure struct {
	Cause          string        `json:"cause"`
	Detail         string        `json:"detail,omitempty"`
	LaunchFailures FailureBudget `json:"launchFailures"`
	Deaths         FailureBudget `json:"deaths"`
	PromptFailures FailureBudget `json:"promptFailures"`
	PromptRetires  FailureBudget `json:"promptRetires"`
	Death          string        `json:"death,omitempty"`
	StartFailure   string        `json:"startFailure,omitempty"`
}

// FailureBudget is one count of a failed claim beside the limit it is measured against
// (record.FailureBudget is this type).
type FailureBudget struct {
	Count int `json:"count"`
	Limit int `json:"limit"`
}

// PhaseFinishedRecord is the notice that a phase ended (workflow's NoticeFor): the role whose
// phase ended, the phase, the phase the issue moved to, the worker's summary (words, bounded at
// Summary) with the handoff's reference (its phase word, its version and its commit), and the
// tester's verdict. A worker's own move back gives its words in reason, by that role.
type PhaseFinishedRecord struct {
	NoticeHead
	Role           claim.Role           `json:"role,omitempty"`
	Phase          phase.Phase          `json:"phase,omitempty"`
	To             phase.Phase          `json:"to,omitempty"`
	Summary        string               `json:"summary,omitempty" texts:"words"`
	Verdict        string               `json:"verdict,omitempty"`
	HandoffPhase   string               `json:"handoff_phase,omitempty"`
	HandoffVersion int                  `json:"handoff_version,omitempty"`
	HandoffCommit  string               `json:"handoff_commit,omitempty"`
	Reason         string               `json:"reason,omitempty" texts:"words"`
	Words          map[string]WordsMeta `json:"words,omitempty"`
}

// WorkerDiedRecord is the notice that a role's claim failed (workflow's claimFailed): the role,
// the issue's phase, the phase the issue works where the role does not work it (Working), the
// look of a production check that lost its checking role, and why the claim failed.
//
// Failure is the failure record, where the daemon recorded the failure's cause. Detail is the
// sentence the failing site wrote, where the notice carries no failure record (the tree
// architect's own notice, and a notice whose failure recorded no cause): on one line, cut where it
// is longer than the bound of a pod end, which DetailCut marks. PodEnd is the pod end that the
// claim's process was last found to have died of (an eviction, above all), as the runtime read it
// off the pod. Death is the first line of the runtime's account of the process's death, where the
// notice carries no failure record, the claim failed because its process died, and no pod end
// was read: cut where it is longer than the same bound, which DeathCut marks.
type WorkerDiedRecord struct {
	NoticeHead
	Role      claim.Role     `json:"role,omitempty"`
	Phase     phase.Phase    `json:"phase,omitempty"`
	Working   phase.Phase    `json:"working,omitempty"`
	Look      int            `json:"look,omitempty"`
	Failure   *NoticeFailure `json:"failure,omitempty"`
	Detail    string         `json:"detail,omitempty"`
	DetailCut bool           `json:"detail_cut,omitempty"`
	PodEnd    string         `json:"pod_end,omitempty"`
	Death     string         `json:"death,omitempty"`
	DeathCut  bool           `json:"death_cut,omitempty"`
}

// The words of what became of a stalled process (WorkerStalledRecord.Stop).
const (
	StopStopped     = "stopped"
	StopUnconfirmed = "unconfirmed"
	StopNotMade     = "not-made"
)

// WorkerStalledRecord is the notice of the stall backstop (the daemon's stallBackstopRecord): the
// role, when its agent last stepped and for how long it has been silent, the bound the deployment
// gives the role (worker_stall_backstop_hours), when the turn began, the last call the daemon saw
// (its tool, its id, when it started and when it ended), how the backstop's recovery ended (a
// supervise.ProcessEnd word), when a queued launch is due, what became of the stalled process (one
// of the Stop words) and the launch failures the replacement charged.
type WorkerStalledRecord struct {
	NoticeHead
	Role           claim.Role `json:"role,omitempty"`
	LastStep       time.Time  `json:"last_step"`
	SilentFor      string     `json:"silent_for"`
	Bound          string     `json:"bound,omitempty"`
	TurnStarted    time.Time  `json:"turn_started,omitzero"`
	CallTool       string     `json:"call_tool,omitempty"`
	CallID         string     `json:"call_id,omitempty"`
	CallStarted    time.Time  `json:"call_started,omitzero"`
	CallEnded      time.Time  `json:"call_ended,omitzero"`
	End            string     `json:"end"`
	WakeDue        time.Time  `json:"wake_due,omitzero"`
	Stop           string     `json:"stop,omitempty"`
	LaunchFailures int        `json:"launch_failures,omitempty"`
}

// The words of the start a worker-silent notice says the daemon wrote (WorkerSilentRecord.Sent).
const (
	SentOwedEntry = "owed-entry"
	SentPhaseTask = "phase-task"
)

// WorkerSilentRecord is the notice of a phase worker that nothing will prompt (workflow's
// repairSilence): the role, the phase, when its agent last stepped and for how long it has been
// silent, the bound (worker_silence_minutes), the start the daemon wrote (one of the Sent words),
// the phase entry the phase row names and the entry the claim was last given.
type WorkerSilentRecord struct {
	NoticeHead
	Role      claim.Role  `json:"role,omitempty"`
	Phase     phase.Phase `json:"phase,omitempty"`
	LastStep  time.Time   `json:"last_step"`
	SilentFor string      `json:"silent_for"`
	Bound     string      `json:"bound,omitempty"`
	Sent      string      `json:"sent"`
	Entry     int64       `json:"entry,omitempty"`
	Given     int64       `json:"given,omitempty"`
}

// WorkerBackgroundStalledRecord is the notice of a phase worker that waits on background work of
// its own past its bound (workflow's workerBackgroundStalled): the role, the phase, each kind of
// work its session reported, when it reported it, how long the report has stood, and the bound
// (worker_background_hours).
type WorkerBackgroundStalledRecord struct {
	NoticeHead
	Role       claim.Role  `json:"role,omitempty"`
	Phase      phase.Phase `json:"phase,omitempty"`
	Work       []string    `json:"work,omitempty"`
	ReportedAt time.Time   `json:"reported_at"`
	StoodFor   string      `json:"stood_for"`
	Bound      string      `json:"bound,omitempty"`
}

// HeldRecord is the notice of an issue's hold (workflow's claimFailed, retryOrEscalate and
// HeldNotice): the role, the phase the hold came from, what held it (a Hold word, which the
// urgency rule reads), the hold's reason word (escalated for an escalation, which both agents'
// texts branch on), the architect's own words for its escalation, and why the claim failed.
type HeldRecord struct {
	NoticeHead
	Role       claim.Role           `json:"role,omitempty"`
	Phase      phase.Phase          `json:"phase,omitempty"`
	Hold       string               `json:"hold"`
	Reason     string               `json:"reason,omitempty"`
	Escalation string               `json:"escalation,omitempty" texts:"words"`
	Failure    *NoticeFailure       `json:"failure,omitempty"`
	Words      map[string]WordsMeta `json:"words,omitempty"`
}

// PRBlockedRecord is the notice that an issue reached the review round cap (workflow's
// recordRound): the cap, the rounds counted and the conflict rounds not counted.
type PRBlockedRecord struct {
	NoticeHead
	Cap            int `json:"cap"`
	Counted        int `json:"counted"`
	ConflictRounds int `json:"conflict_rounds,omitempty"`
}

// PRMergedRecord is the notice that an issue's pull request merged (workflow's merged and
// closeMergedTelling): the pull request, the phase the issue stood in where it was not merging,
// and each issue the close waits for.
type PRMergedRecord struct {
	NoticeHead
	PullRequest string      `json:"pull_request"`
	Phase       phase.Phase `json:"phase,omitempty"`
	WaitingFor  []string    `json:"waiting_for,omitempty"`
}

// PRClosedUnmergedRecord is the notice that an issue's pull request closed without merging
// (workflow's closed): the pull request and the issue's phase.
type PRClosedUnmergedRecord struct {
	NoticeHead
	PullRequest string      `json:"pull_request"`
	Phase       phase.Phase `json:"phase,omitempty"`
}

// PullRequestMissingRecord is the notice that the implementer completed and GitHub shows no open
// pull request on its branch (workflow's pullRequestMissing): the handoff commit, the repository
// and the branch.
type PullRequestMissingRecord struct {
	NoticeHead
	HandoffCommit string `json:"handoff_commit,omitempty"`
	Repo          string `json:"repo"`
	Branch        string `json:"branch"`
}

// The words of what Dispatch refused (MessageRefusedRecord.Refused).
const (
	RefusedMessage = "message"
	RefusedStatus  = "status"
)

// ByDaemon is the author the words object of a message-refused notice names for a first line
// that is the daemon's own text (MessageRefusedRecord.Begins).
const ByDaemon = "daemon"

// BeforeFirstPhase is what a message-refused notice's refused_close says: the refused line is the
// close its architect made of the issue before its first phase (MessageRefusedRecord.RefusedClose).
const BeforeFirstPhase = "before_first_phase"

// MessageRefusedRecord is the notice of a Dispatch write the daemon could not make (the daemon's
// messagePostRefused and statusWriteRefused): what Dispatch refused (one of the Refused words) and
// its answer. A refused message gives its length in characters and how its first line begins. A
// refused status write gives the status, whether the status is set, whether the reason posted
// beside it is on the issue, and, where it is not, the reason's length and how its first line
// begins. Two first lines open with the daemon's own sentence and go on with another author's
// words: a worker's backward move, whose move and asker the sentence states, and a close before
// the issue's first phase, whose words are the architect's. Of such a line the record gives the
// daemon's sentence as a field, RefusedMove (the move, "<from>-<to>") or RefusedClose
// (BeforeFirstPhase), and the other author's words alone in Begins. Every other first line,
// such a sentence with no words after it among them, is the daemon's own, and Begins gives it
// whole. Begins is a words field, which the words object names with its author, the role whose
// words it holds (the asker of the move, the architect of the close) or ByDaemon for the
// daemon's own line, with the time the daemon queued the write, and, for a line past the
// preview's bound, with the count of what the cut left out (rest): Begins holds the line's head
// and nothing else.
type MessageRefusedRecord struct {
	NoticeHead
	Refused        string               `json:"refused"`
	DispatchAnswer string               `json:"dispatch_answer"`
	Length         int                  `json:"length,omitempty"`
	RefusedMove    string               `json:"refused_move,omitempty"`
	RefusedClose   string               `json:"refused_close,omitempty"`
	Begins         string               `json:"begins,omitempty" texts:"words"`
	Status         string               `json:"status,omitempty"`
	StatusSet      *bool                `json:"status_set,omitempty"`
	ReasonPosted   *bool                `json:"reason_posted,omitempty"`
	Words          map[string]WordsMeta `json:"words,omitempty"`
}

// GateDecision is a person's decision on a design-gate step that a notice reports: the version
// decided, who decided (a Dispatch actor, user:<login> for a person, absent where nobody decided),
// the reference of the step's document (workflow's stepRef) and when.
type GateDecision struct {
	Version int       `json:"version,omitempty"`
	Actor   string    `json:"actor,omitempty"`
	Ref     string    `json:"ref,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

// DesignApprovedRecord is the notice that a design-gate step opened (workflow's gateRegistered
// and applyArtifact).
type DesignApprovedRecord struct {
	NoticeHead
	GateDecision
}

// DesignChangesRequestedRecord is the notice of a person's changes request on a design-gate step
// (workflow's tellChanges): the decision, and the person's reason (words, bounded at OneAct).
type DesignChangesRequestedRecord struct {
	NoticeHead
	GateDecision
	Reason string               `json:"reason,omitempty" texts:"words"`
	Words  map[string]WordsMeta `json:"words,omitempty"`
}

// CarriedGateOpenedRecord is the notice that a person's approval opened the gate a carried root
// brought with it (workflow's applyArtifact): the decision, whose reference names the issue whose
// document the person approved.
type CarriedGateOpenedRecord struct {
	NoticeHead
	GateDecision
}

// ChildClosedRecord is the notice that a child is done (workflow's leave and closeMerged). The
// issue is the child.
type ChildClosedRecord struct {
	NoticeHead
}

// The words of why a reparented root stays a tree of its own (ChildStatusRecord.StaysOwn), one for
// each reason of workflow's RanAsRoot.
const (
	StaysOwnOperator   = "operator"
	StaysOwnRan        = "ran"
	StaysOwnCarriesRun = "carries-run"
	StaysOwnHandedOver = "handed-over"
)

// ChildStatusRecord is the notice of a change of a child (workflow's leave, ChildReenteredNotice,
// ChildStartedNotice, ChildUnderAcceptedParentNotice, JoinedTheTreeNotice and ReparentedNotice):
// the change (a Change word, which the urgency rule reads), the tree it runs under, every issue it
// waited on, the root of the closed tree it came from (absent for the admission queue), the status
// it left with, the ancestor whose reviewed head the daemon accepted, and, for a reparented root,
// the root, its new Dispatch parent and why it stays a tree of its own (a StaysOwn word).
type ChildStatusRecord struct {
	NoticeHead
	Change           string   `json:"change"`
	Child            string   `json:"child,omitempty"`
	Tree             string   `json:"tree,omitempty"`
	WaitedOn         []string `json:"waited_on,omitempty"`
	FromTree         string   `json:"from_tree,omitempty"`
	Status           string   `json:"status,omitempty"`
	AcceptedAncestor string   `json:"accepted_ancestor,omitempty"`
	Parent           string   `json:"parent,omitempty"`
	StaysOwn         string   `json:"stays_own,omitempty"`
}

// ChildrenFirstRecord is the notice that a parent's own first phase waits for its open children
// (workflow's ChildrenFirstNotice): every open child.
type ChildrenFirstRecord struct {
	NoticeHead
	Children []OpenChildFacts `json:"children"`
}

// OpenChildFacts is one open child of a children-first notice: its key, the Dispatch status it
// holds its parent with, whether the daemon released it, and the issue above it on its own parent
// chain that a blocked_by link of it names (empty where there is no such cycle).
type OpenChildFacts struct {
	Key      string `json:"key"`
	Status   string `json:"status"`
	Released bool   `json:"released"`
	Cycle    string `json:"cycle"`
}

// The words of why a released child is not recorded (ChildReleaseStalledRecord.Stall).
const (
	StallUnrecordedAncestor  = "unrecorded-ancestor"
	StallUnrecordedBlocker   = "unrecorded-blocker"
	StallAttemptsSpent       = "attempts-spent"
	StallRecoveredUnrecorded = "recovered-unrecorded"
	// StallLetGoNoTodo is a repository the daemon no longer waits for GitHub to answer for,
	// because no todo issue labelled for Legion or under a parent names it now.
	StallLetGoNoTodo = "let-go-no-todo"
	// StallLetGoAnswered is a repository the daemon no longer waits for GitHub to answer for,
	// because a read GitHub answered for it at another issue's preparation answers for it now.
	StallLetGoAnswered = "let-go-answered"
)

// ChildReleaseStalledRecord is the notice that a child a release released is not recorded (api's
// tellReleaseWait and release retry, the daemon's recovery of an unread repository, and the
// daemon's letting go of one): why (a Stall word), the child, its parent, the unrecorded issue
// above it, the unrecorded issue on its parent chain that blocks it with that issue's status, the
// repository GitHub did not answer for, the releases tried, the repositories recovered or let go,
// each child still not recorded after a recovery, and each child a letting go leaves that no
// recovery will record.
type ChildReleaseStalledRecord struct {
	NoticeHead
	Stall         string            `json:"stall"`
	Child         string            `json:"child,omitempty"`
	Parent        string            `json:"parent,omitempty"`
	Ancestor      string            `json:"ancestor,omitempty"`
	Blocker       string            `json:"blocker,omitempty"`
	BlockerStatus string            `json:"blocker_status,omitempty"`
	Repo          string            `json:"repo,omitempty"`
	Attempts      int               `json:"attempts,omitempty"`
	Repos         []string          `json:"repos,omitempty"`
	Unrecorded    []UnrecordedChild `json:"unrecorded,omitempty"`
	LetGo         []LetGoChild      `json:"let_go,omitempty"`
}

// UnrecordedChild is one child a recovery left unrecorded: the child, its parent and whether the
// parent is recorded now.
type UnrecordedChild struct {
	Child          string `json:"child"`
	Parent         string `json:"parent"`
	ParentRecorded bool   `json:"parent_recorded"`
}

// LetGoChild is one child a release_children left to the recovery of a repository the daemon let
// go, which no recovery will record now: the child, its parent and whether the parent is recorded
// now, the status Dispatch's listing gives the child (empty where the listing no longer holds
// it), and the repository its wait was filed under.
type LetGoChild struct {
	Child          string `json:"child"`
	Parent         string `json:"parent"`
	ParentRecorded bool   `json:"parent_recorded"`
	Status         string `json:"status"`
	Repo           string `json:"repo"`
}

// ChildNotRecordedRecord is the notice that a child was not recorded under the tree (workflow's
// refuseChild): the child, the repository it resolved to, and the refusal.
type ChildNotRecordedRecord struct {
	NoticeHead
	Child    string `json:"child"`
	Resolved string `json:"resolved,omitempty"`
	Refusal  string `json:"refusal"`
}

// CatchUpRecord is the notice that tells an architect at a launch the issues it owns (workflow's
// QueueCatchUp): the account of the tree, and what the last let-go of the architect's process
// ended where the launch's ready carried it.
type CatchUpRecord struct {
	NoticeHead
	CatchUp  *CatchUpFacts        `json:"catch_up,omitempty"`
	Relaunch *RelaunchFacts       `json:"relaunch,omitempty"`
	Words    map[string]WordsMeta `json:"words,omitempty"`
}

// CatchUpFacts is a catch-up's account of the issues an architect owns (the shape of
// record.CatchUp, whose keys it keeps). It names each issue that is not done with its fields, each
// done issue by its key alone, each child not yet released whole, and the keys of the issues that
// wait in accepting for this architect's acceptance. Kept is what a person decided on the tree
// while nobody could hear it, with the words of each decision. Decided is the text of those
// decisions that an older daemon wrote, which this daemon writes no more.
type CatchUpFacts struct {
	Generation uint64            `json:"generation"`
	Launch     uint64            `json:"launch"`
	HandedOver bool              `json:"handed_over,omitempty"`
	Carried    bool              `json:"carried,omitempty"`
	Gates      []CatchUpGate     `json:"gates"`
	Issues     []CatchUpIssue    `json:"issues"`
	Done       []string          `json:"done,omitempty"`
	Unreleased []UnreleasedChild `json:"unreleased,omitempty"`
	Accepting  []string          `json:"accepting,omitempty"`
	Decided    string            `json:"decided,omitempty"`
	Kept       *KeptFacts        `json:"kept,omitempty"`
}

// CatchUpGate is one design-gate step as a catch-up states it (the shape of record.CatchUpGate):
// the step, whether a person must approve its document, and the document as a dispatch:// link,
// with the artifact it is registered as, its latest version and the version approved.
type CatchUpGate struct {
	Step         string `json:"step"`
	Gated        bool   `json:"gated"`
	Unregistered bool   `json:"unregistered,omitempty"`
	Document     string `json:"document,omitempty"`
	Artifact     string `json:"artifact,omitempty"`
	Version      int    `json:"version,omitempty"`
	Approved     int    `json:"approved,omitempty"`
	Open         bool   `json:"open"`
}

// CatchUpIssue is one issue of the tree as a catch-up names it (record.CatchUpIssue is this
// type): an issue that is not done.
type CatchUpIssue struct {
	Key    string      `json:"key"`
	Parent string      `json:"parent,omitempty"`
	Title  string      `json:"title"`
	Phase  phase.Phase `json:"phase"`
	Status string      `json:"status"`
}

// UnreleasedChild is one child that its architect filed and has not released: its key, its parent
// and its Dispatch status.
type UnreleasedChild struct {
	Key    string `json:"key"`
	Parent string `json:"parent"`
	Status string `json:"status"`
}

// KeptFacts is what one architect start task or catch-up names of what its tree is owed (the
// shape of record.KeptDecisions): each decision, and the approval a re-admission kept.
type KeptFacts struct {
	Decisions []DecisionMark `json:"decisions,omitempty"`
	KeptApproval
}

// KeptApproval is the approval a root's re-admission kept for its next architect start (the shape
// of record.KeptApproval): the step whose document a person approved, that document as a
// dispatch:// link, the version approved and the version the document has reached since.
type KeptApproval struct {
	Step     string `json:"step,omitempty"`
	Document string `json:"document,omitempty"`
	Approved int    `json:"approved,omitempty"`
	Latest   int    `json:"latest,omitempty"`
}

// DecisionMark is one kept decision (the shape of record.DecisionMark): its reference, the time of
// the decision, the issue it was made on, what the person did, who, the options the answer chose,
// the words of the act (bounded at OneAct where a catch-up names one decision and at
// EachOfSeveral each where it names several), when the person wrote them, and whether more follows
// than the act's event carried (Carry's more). A catch-up gives the count of the rest in its words
// object, so it leaves Rest, the record's count of what a text had cut off the words before they
// were kept, empty.
type DecisionMark struct {
	Ref      string    `json:"ref"`
	At       time.Time `json:"at"`
	Issue    string    `json:"issue,omitempty"`
	Decision string    `json:"decision,omitempty"`
	Actor    string    `json:"actor,omitempty"`
	Options  []string  `json:"options,omitempty"`
	Answer   string    `json:"answer,omitempty" texts:"words"`
	AnswerAt time.Time `json:"answer_at,omitzero"`
	More     bool      `json:"more,omitempty"`
	Rest     int       `json:"rest,omitempty"`
}

// ChecksRedRecord is the notice that the daemon asked the implementer to merge the base forward
// onto a head that conflicts with its base (workflow's mergeForward): the phase the issue stays
// in, what holds the issue where it is held (a Hold word: HoldFailure or HoldEscalation), the
// base, the head, the pull request and why the daemon acted (record.ConflictWhy's word). A held
// issue stays in the phase its hold came from, and no worker holds that phase (checksRedUrgency).
// The base is absent where the daemon has not recorded it, as for a pull request recorded before
// the daemon kept the base (record.PullRequest.Base).
type ChecksRedRecord struct {
	NoticeHead
	Phase         phase.Phase `json:"phase,omitempty"`
	Hold          string      `json:"hold,omitempty"`
	ConflictsWith string      `json:"conflicts_with,omitempty"`
	Head          string      `json:"head"`
	PullRequest   string      `json:"pull_request"`
	Why           string      `json:"why"`
}

// The summary words of a review-stuck notice: the fact that wrote it. dispatch://INFRA-3498's R4
// keeps them, and the architect's prompt reads them.
const (
	StuckByCompletion  = "the reviewer's completion"
	StuckByAnswer      = "the reviewer's review"
	StuckByOtherReview = "a review that is not the reviewer's answer"
	StuckByPush        = "a push"
	StuckByMergeState  = "GitHub's merge state for the head"
	StuckByMerge       = "the pull request's merge"
)

// The causes of a stuck review round (ReviewStuckRecord.Cause).
const (
	CauseUndecided         = "undecided"
	CauseApprovedOtherCode = "approved-other-code"
	CauseMergeState        = "merge-state"
)

// ReviewStuckRecord is the notice of a review round that nothing on its way ends (workflow's
// settleRound and tellStuckMergeState): the reviewer's role and the phase, the summary words (a
// StuckBy constant), the reviewer's role topic where the reviewer decides it, the cause (a Cause
// word), the pull request and the head, the head the reviewer approved where it is not the head,
// the review that ends the round (EndsRound, which keeps APPROVE of head <sha>), the reason the
// daemon set a review aside, and, for a head GitHub will not merge, its merge state, the checks and
// the commit statuses of that head that did not pass, the checks of that head GitHub skipped
// (skipped) with the field that says once that a skipped check ran nothing on the head
// (each_skipped_check, CheckConclusion), the count of the checks and statuses that passed and the
// command that lists them.
type ReviewStuckRecord struct {
	NoticeHead
	Role              claim.Role        `json:"role,omitempty"`
	Phase             phase.Phase       `json:"phase,omitempty"`
	Summary           string            `json:"summary,omitempty"`
	Topic             string            `json:"topic,omitempty"`
	Cause             string            `json:"cause"`
	PullRequest       string            `json:"pull_request"`
	Head              string            `json:"head"`
	ApprovedHead      string            `json:"approved_head,omitempty"`
	EndsRound         string            `json:"ends_round,omitempty"`
	SetAside          string            `json:"set_aside,omitempty"`
	MergeState        string            `json:"merge_state,omitempty"`
	NotPassed         []CheckConclusion `json:"not_passed,omitempty"`
	Skipped           []string          `json:"skipped,omitempty"`
	EachSkippedCheck  string            `json:"each_skipped_check,omitempty"`
	NotPassedStatuses []StatusState     `json:"not_passed_statuses,omitempty"`
	Passed            *int              `json:"passed,omitempty"`
	ListPassed        string            `json:"list_passed,omitempty"`
}

// StatusReassertedRecord is the notice that a session wrote a lifecycle status and the daemon set
// its own back (workflow's StatusReassertedNotice): the session, the status it wrote and the
// status the daemon set.
type StatusReassertedRecord struct {
	NoticeHead
	Session string `json:"session"`
	Wrote   string `json:"wrote"`
	Status  string `json:"status"`
}

// VolumeNearFullRecord is the notice that a sweep of the issue pod's volume left that volume above
// its low mark, or that the pod's cache trimmer switched itself off (workflow's volumeNearFull):
// the sweep as the pod's launcher reported it, with the pod it was of (VolumeSweep). Its readers'
// prompt says how to read the sweep's fields.
type VolumeNearFullRecord struct {
	NoticeHead
	Volume *VolumeSweep `json:"volume,omitempty"`
}

// VolumeSweep is one sweep of an issue pod's volume as the cache trimmer in the pod's launcher
// reported it (record.VolumeNearFull is this type): the pod; the trimmer's identifier, its
// episode and what raised the episode; the bytes used and free at the sweep's last reading and the
// bytes the sweep freed (negative where the used bytes grew while it ran); why the sweep stopped,
// in the trimmer's own words; the /proc reads the kernel refused; the sizes of the size walk
// (VolumeSize) with the time they were measured, whether a cut ended the walk early and the
// targets it did not measure (VolumeMissing); the faults the trimmer recovered from; and whether
// the trimmer switched itself off. Each Omitted count is how many entries the launcher's bound
// left out of a list. The keys are the shape main's release keeps in its outbox rows. SizesAt is
// left out where it is zero, since then no walk measured the sizes, as in the report of a trimmer
// that switched itself off: a row main's release queued, which writes the zero time, reads back as
// zero all the same.
type VolumeSweep struct {
	PodUID              string          `json:"podUid"`
	Trimmer             string          `json:"trimmer"`
	Episode             int             `json:"episode"`
	RaisedBy            string          `json:"raisedBy"`
	UsedBytes           int64           `json:"usedBytes"`
	FreeBytes           int64           `json:"freeBytes"`
	FreedBytes          int64           `json:"freedBytes"`
	StopReason          string          `json:"stopReason"`
	RefusedReads        []string        `json:"refusedReads,omitempty"`
	RefusedReadsOmitted int             `json:"refusedReadsOmitted,omitempty"`
	Sizes               []VolumeSize    `json:"sizes"`
	SizesOmitted        int             `json:"sizesOmitted,omitempty"`
	SizesAt             time.Time       `json:"sizesAt,omitzero"`
	SizesPartial        bool            `json:"sizesPartial,omitempty"`
	SizesMissing        []VolumeMissing `json:"sizesMissing,omitempty"`
	SizesMissingOmitted int             `json:"sizesMissingOmitted,omitempty"`
	Faults              []string        `json:"faults,omitempty"`
	FaultsOmitted       int             `json:"faultsOmitted,omitempty"`
	SwitchedOff         bool            `json:"switchedOff,omitempty"`
}

// MarshalJSON keeps sizes an array on the wire, empty rather than null, as UpdateRound keeps
// waiting one: the plugin's strict reader refuses a null.
func (v VolumeSweep) MarshalJSON() ([]byte, error) {
	type wire VolumeSweep
	out := wire(v)
	if out.Sizes == nil {
		out.Sizes = []VolumeSize{}
	}
	return json.Marshal(out)
}

// VolumeSize is one target of the trimmer's size walk (record.VolumeSize is this type): its path,
// the bytes of the regular files at or under it, and of those the bytes more than an hour old and
// more than a day old. Refused names the first read under the path that the walk did not make, and
// Refusals counts such reads: a size that carries them is a lower bound.
type VolumeSize struct {
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	BytesOverHour int64  `json:"bytesOverHour"`
	BytesOverDay  int64  `json:"bytesOverDay"`
	Refused       string `json:"refused,omitempty"`
	Refusals      int    `json:"refusals,omitempty"`
}

// VolumeMissing is one target the size walk measured none of, or a part of the volume it did not
// list in full (record.VolumeMissing is this type), with the kernel's answer where a read was
// refused.
type VolumeMissing struct {
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}

// SessionLostRecord is the notice that a worker's recorded session is in no session store
// (workflow's sessionLost): the role, the phase and the runtime's account of the failed resume.
type SessionLostRecord struct {
	NoticeHead
	Role   claim.Role  `json:"role,omitempty"`
	Phase  phase.Phase `json:"phase,omitempty"`
	Detail string      `json:"detail"`
}

// LookupFailed is what a session-forgotten notice's launcher_lookup says: the pod's launcher could
// not look the session up by its id (SessionForgottenRecord.LauncherLookup).
const LookupFailed = "failed"

// SessionForgottenRecord is the notice that a worker's claim forgot the session it recorded
// (workflow's sessionLost): the role, the phase, the session forgotten, and why it was forgotten,
// as dispatch://INFRA-4108's R3 has each of its texts say: the pod's launcher, older than the
// release that added the kind, could not look that session up by its id (LauncherLookup,
// LookupFailed), and Oh My Pi did not resume it by that id (ResumedByID, false on every notice of
// the kind). It is a kind of its own, and not session-lost, because the kind is the notice's
// one-line message and that session was not lost: nothing of it was deleted, and a store may hold
// it still. No earlier release decodes the kind, so the way back of migration 20261120042902
// deletes its unsent rows.
type SessionForgottenRecord struct {
	NoticeHead
	Role           claim.Role  `json:"role,omitempty"`
	Phase          phase.Phase `json:"phase,omitempty"`
	Session        string      `json:"session"`
	LauncherLookup string      `json:"launcher_lookup"`
	ResumedByID    bool        `json:"resumed_by_id"`
}

// SessionStoreFaultRecord is the notice that no worker of the issue can start because the session
// store cannot be asked (workflow's sessionStoreFault): the role, the phase, the runtime's account,
// and how often the daemon tries again.
type SessionStoreFaultRecord struct {
	NoticeHead
	Role       claim.Role  `json:"role,omitempty"`
	Phase      phase.Phase `json:"phase,omitempty"`
	Detail     string      `json:"detail"`
	RetryEvery string      `json:"retry_every"`
}
