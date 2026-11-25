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
// tester's verdict. A worker's own move back says so (MovedBack), since its words may be none and
// a completion that moves the issue to an earlier phase (a tester's fail, a review's request for
// changes) names the same phases; it gives its words, where it gave any, in reason, by that role.
type PhaseFinishedRecord struct {
	NoticeHead
	Role           claim.Role           `json:"role,omitempty"`
	Phase          phase.Phase          `json:"phase,omitempty"`
	To             phase.Phase          `json:"to,omitempty"`
	MovedBack      bool                 `json:"moved_back,omitempty"`
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
// A role that does not work the issue's phase also says that nothing is held (NothingHeld): its
// failure holds no phase, so the phase runs on and nothing waits on the architect's
// retry_or_escalate. dispatch://INFRA-2009's R5 keeps the reason main's notice built, which said so
// ("the %s does not work %s's phase %s; nothing is held"), and R8 of dispatch://INFRA-3686 keeps
// it as a field.
//
// Failure is the failure record, where the daemon recorded the failure's cause. Detail is the
// sentence the failing site wrote, where the notice carries no failure record (the tree
// architect's own notice, and a notice whose failure recorded no cause): on one line, cut where it
// is longer than the bound of a pod end, which DetailCut marks. PodEnd is the pod end that the
// claim's process was last found to have died of (an eviction, above all), as the runtime read it
// off the pod. Death is the first line of the runtime's account of the process's death, where the
// notice carries no failure record, the claim failed because its process died, and no pod end
// was read: cut where it is longer than the same bound, which DeathCut marks.
// Its builder is workflow's WorkerDied.
type WorkerDiedRecord struct {
	NoticeHead
	Role        claim.Role     `json:"role,omitempty"`
	Phase       phase.Phase    `json:"phase,omitempty"`
	Working     phase.Phase    `json:"working,omitempty"`
	NothingHeld bool           `json:"nothing_held,omitempty"`
	Look        int            `json:"look,omitempty"`
	Failure     *NoticeFailure `json:"failure,omitempty"`
	Detail      string         `json:"detail,omitempty"`
	DetailCut   bool           `json:"detail_cut,omitempty"`
	PodEnd      string         `json:"pod_end,omitempty"`
	Death       string         `json:"death,omitempty"`
	DeathCut    bool           `json:"death_cut,omitempty"`
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
// Its builder is workflow's WorkerSilentNotice.
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
// Its builder is workflow's WorkerBackgroundNotice.
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
// Its builders are workflow's HeldFailureNotice and HeldEscalationNotice.
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
// recordRound): the cap, the rounds counted and the conflict rounds not counted, 0 included, since
// dispatch://INFRA-1442's R5 has the notice name how many conflict rounds were not counted, as
// main's cap message did on every cap ("%d conflict rounds not counted").
// Its builder is workflow's PRBlockedNotice.
type PRBlockedRecord struct {
	NoticeHead
	Cap            int `json:"cap"`
	Counted        int `json:"counted"`
	ConflictRounds int `json:"conflict_rounds"`
}

// PRMergedRecord is the notice that an issue's pull request merged (workflow's merged and
// closeMergedTelling): the pull request, the phase the issue stood in where it was not merging,
// and each issue the close waits for. A notice whose close waits (WaitingFor) tells the architect
// to end a child, so it keeps, word for word as main's notice gave them, the two texts
// dispatch://INFRA-3178's R6 has such a notice say, which R8 of dispatch://INFRA-3686 keeps:
// record.StatusNotTakenYet (StatusNotTakenYet) and the eight things of record.EndChildRule
// (EndChildRule). A notice of a merge that closed the issue or found it out of merging tells the
// architect to end nothing and carries neither.
// Its builder is workflow's PRMergedNotice.
type PRMergedRecord struct {
	NoticeHead
	PullRequest       string      `json:"pull_request"`
	Phase             phase.Phase `json:"phase,omitempty"`
	WaitingFor        []string    `json:"waiting_for,omitempty"`
	StatusNotTakenYet string      `json:"status_not_taken_yet,omitempty"`
	EndChildRule      string      `json:"end_child_rule,omitempty"`
}

// PRClosedUnmergedRecord is the notice that an issue's pull request closed without merging
// (workflow's closed): the pull request and the issue's phase.
// Its builder is workflow's PRClosedUnmergedNotice.
type PRClosedUnmergedRecord struct {
	NoticeHead
	PullRequest string      `json:"pull_request"`
	Phase       phase.Phase `json:"phase,omitempty"`
}

// PullRequestMissingRecord is the notice that the implementer completed and GitHub shows no open
// pull request on its branch (workflow's pullRequestMissing): the handoff commit, the repository
// and the branch.
// Its builder is workflow's PullRequestMissingNotice.
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

// ByDaemon is the author a text names for words that are the daemon's own: the words object of a
// message-refused notice, for a first line that is the daemon's own text
// (MessageRefusedRecord.Begins), and the reason block of an item of news, the daemon's sentence of
// a read of Dispatch it gave up on, which quotes Dispatch's error (workflow's dispatchReason).
const ByDaemon = "daemon"

// BeforeFirstPhase is what a message-refused notice's refused_close says: the refused line is the
// close its architect made of the issue before its first phase (MessageRefusedRecord.RefusedClose).
const BeforeFirstPhase = "before_first_phase"

// MessageRefusedRecord is the notice of a Dispatch write the daemon could not make (the daemon's
// messagePostRefused and statusWriteRefused): what Dispatch refused (one of the Refused words) and
// its answer. A refused message gives its length and how its first line begins. A refused status
// write gives the status, whether the status is set, whether the reason posted beside it is on the
// issue, and, where it is not, the reason's length and how its first line begins. The length, like
// every count of carried words (Carry), is in UTF-16 units, as Dispatch counts a body. A line ends
// at a line feed, or at a carriage return and a line feed, and neither is part of it. Two bodies
// open with the daemon's own sentence and go on with another author's words: a worker's backward
// move, whose move and asker the sentence states, and a close before the issue's first phase,
// whose words are the architect's. Of such a body the record gives the daemon's sentence as a
// field, RefusedMove (the move, "<from>-<to>") or RefusedClose (BeforeFirstPhase), and the first
// line of the other author's words alone in Begins: those words begin at their first line that
// holds a character other than white space, since the lines of white space alone they open with,
// as the line endings they end with, are no words.
// Every other first line, such a sentence with no words after it among them, is the daemon's own,
// and Begins gives it whole. A first line that is empty gives no Begins: FirstLineEmpty says so,
// as main's notice said the body begins "". Begins is a words field, which the words object names
// with its author, the role whose words it holds (the asker of the move, the architect of the
// close) or ByDaemon for the daemon's own text, with the time the daemon queued the write, and,
// where the words go on past what Begins holds, with the count of the rest (rest): every unit of
// those words after Begins, the lines after the first among them, so the count is 0 only where
// Begins holds the words whole. The line endings the words end with are no words, and the count
// leaves them out. Begins holds the first line's head and nothing else, cut where it parts none of
// these as its writer wrote them (the daemon's wholeCharacters): a letter and the combining marks
// after it, by Go's own Unicode tables, an emoji and its skin-tone modifier, the characters a zero
// width joiner joins, the two regional indicators of a flag, the jamo of one Hangul syllable,
// unless the line's first character alone runs past the bound. Two characters UAX #29 keeps whole
// it can part: a prepended concatenation mark or a reph and the character after it (GB9b), and
// the consonants of an Indic conjunct with no joiner between them (GB9c).
type MessageRefusedRecord struct {
	NoticeHead
	Refused        string               `json:"refused"`
	DispatchAnswer string               `json:"dispatch_answer"`
	Length         int                  `json:"length,omitempty"`
	RefusedMove    string               `json:"refused_move,omitempty"`
	RefusedClose   string               `json:"refused_close,omitempty"`
	FirstLineEmpty bool                 `json:"first_line_empty,omitempty"`
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
// Its builder is workflow's DesignApprovedNotice.
type DesignApprovedRecord struct {
	NoticeHead
	GateDecision
}

// DesignChangesRequestedRecord is the notice of a person's changes request on a design-gate step
// (workflow's tellChanges): the decision, and the person's reason (words, bounded at OneAct).
// Its builder is workflow's ChangesRequestedNotice.
type DesignChangesRequestedRecord struct {
	NoticeHead
	GateDecision
	Reason string               `json:"reason,omitempty" texts:"words"`
	Words  map[string]WordsMeta `json:"words,omitempty"`
}

// CarriedGateOpenedRecord is the notice that a person's approval opened the gate a carried root
// brought with it (workflow's applyArtifact): the decision, whose reference names the issue whose
// document the person approved.
// Its builder is workflow's CarriedGateOpenedNotice.
type CarriedGateOpenedRecord struct {
	NoticeHead
	GateDecision
}

// ChildClosedRecord is the notice that a child is done (workflow's leave and closeMerged). The
// issue is the child. A child that left the workflow done (leave) names what it leaves behind
// (LeftBehind); a child of a lingering tree that closed at its merge (closeMerged) names nothing
// more.
// Its builder is workflow's ChildClosedNotice.
type ChildClosedRecord struct {
	NoticeHead
	LeftBehind
}

// LeftBehind is what a child's move out of the workflow leaves for the architect above it to act
// on (workflow's leave, dispatch://INFRA-3178/spec R4): the child's pull request while it is open,
// as owner/name#n, since the daemon closes no pull request and a merge of it later closes nothing,
// and each recorded issue under the child that is not done, with its status, in key order, since
// the move ends the child alone: an issue under it in the workflow runs on, and one out of the
// workflow stays out. The record holds no state that says a pull request is in the merge queue
// (the daemon hears only GitHub's removal from it), so the notice cannot say so.
type LeftBehind struct {
	OpenPullRequest string         `json:"open_pull_request,omitempty"`
	UnderNotDone    []NotDoneIssue `json:"under_not_done,omitempty"`
}

// NotDoneIssue is one issue under a child that left the workflow, which is not done: its key and
// its status.
type NotDoneIssue struct {
	Key    string `json:"key"`
	Status string `json:"status"`
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
// the change (a Change word, which the urgency rule reads), the tree it runs under, the phase a
// child that starts starts in (the first step of its plan: planning on today's default steps),
// every issue it waited on, the root of the closed tree it came from (absent for the admission
// queue), the status it left with and what that move leaves behind (LeftBehind), the ancestor
// whose reviewed head the daemon accepted, and, for a reparented root, the root, its new Dispatch
// parent and why it stays a tree of its own (a StaysOwn word).
// The notice of a child that left the workflow is built by workflow's ChildLeftNotice.
type ChildStatusRecord struct {
	NoticeHead
	Change   string      `json:"change"`
	Child    string      `json:"child,omitempty"`
	Tree     string      `json:"tree,omitempty"`
	Phase    phase.Phase `json:"phase,omitempty"`
	WaitedOn []string    `json:"waited_on,omitempty"`
	FromTree string      `json:"from_tree,omitempty"`
	Status   string      `json:"status,omitempty"`
	LeftBehind
	AcceptedAncestor string `json:"accepted_ancestor,omitempty"`
	Parent           string `json:"parent,omitempty"`
	StaysOwn         string `json:"stays_own,omitempty"`
}

// ChildrenFirstRecord is the notice that a parent's own first phase waits for its open children
// (workflow's ChildrenFirstNotice): every open child, and, since the notice tells the architect
// how a child leaves, the eight things of record.EndChildRule word for word as main's notice gave
// them (EndChildRule), which dispatch://INFRA-3178's R6 has such a notice say and R8 of
// dispatch://INFRA-3686 keeps.
type ChildrenFirstRecord struct {
	NoticeHead
	Children     []OpenChildFacts `json:"children"`
	EndChildRule string           `json:"end_child_rule"`
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
// now, whether Dispatch's listing still holds the child (Listed) with the status it gives the child
// (empty where the listing no longer holds it), and the repository its wait was filed under.
type LetGoChild struct {
	Child          string `json:"child"`
	Parent         string `json:"parent"`
	ParentRecorded bool   `json:"parent_recorded"`
	Listed         bool   `json:"listed"`
	Status         string `json:"status"`
	Repo           string `json:"repo"`
}

// ChildNotRecordedRecord is the notice that a child was not recorded under the tree (workflow's
// refuseChild): the child, the repository it resolved to, and the refusal.
// Its builder is workflow's ChildNotRecordedNotice.
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
// or as the bare artifact reference (ArtifactReference) where neither the daemon's record nor
// Dispatch, asked when the daemon published the catch-up, named the issue that holds it, with the
// artifact it is registered as, its latest version and the version approved.
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

// ChecksRedRecord is the notice that the daemon asked a role to merge the base forward onto a
// head that conflicts with its base (workflow's mergeForward): the phase the issue stays in, what
// holds the issue where it is held (a Hold word: HoldFailure, HoldEscalation, HoldStoppedTask or
// HoldChildren; main's notice said such an issue "stays held from" that phase), the base, the
// head, the pull request, why the daemon acted (record.ConflictWhy's word) and the role asked
// (Asked, record.MergeForwardAsker): the implementer before merging, and in merging the merger,
// which main's notice named in its reason ("the %s is asked to merge %s forward",
// cybermonklabs/legion#4979). A held issue stays in the phase its hold came from, and no worker
// holds that phase (checksRedUrgency). The base is absent where the daemon has not recorded it, as
// for a pull request recorded before the daemon kept the base (record.PullRequest.Base).
// Its builder is workflow's ChecksRedNotice.
type ChecksRedRecord struct {
	NoticeHead
	Phase         phase.Phase `json:"phase,omitempty"`
	Hold          string      `json:"hold,omitempty"`
	ConflictsWith string      `json:"conflicts_with,omitempty"`
	Head          string      `json:"head"`
	PullRequest   string      `json:"pull_request"`
	Why           string      `json:"why"`
	Asked         claim.Role  `json:"asked"`
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
// (each_skipped_check, CheckConclusion), the count of the checks that passed (passed), the count
// of the commit statuses that passed (passed_statuses, absent where none did) and the command that
// lists them.
// Its builder is workflow's ReviewStuckNotice.
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
	PassedStatuses    int               `json:"passed_statuses,omitempty"`
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
// Its builder is workflow's VolumeNearFullNotice.
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
// Its builder is workflow's SessionLostNotice.
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
// Its builder is workflow's SessionForgottenNotice.
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
// Its builder is workflow's SessionStoreFaultNotice.
type SessionStoreFaultRecord struct {
	NoticeHead
	Role       claim.Role  `json:"role,omitempty"`
	Phase      phase.Phase `json:"phase,omitempty"`
	Detail     string      `json:"detail"`
	RetryEvery string      `json:"retry_every"`
}

// What stopped the task a task-stopped notice names (TaskStoppedRecord.Cause): its owner's
// stop_task, or a failed step that no step before it of the type implementation can take the work
// back from, so the task stops where a return would have been.
const (
	TaskStoppedByOwner  = "owner"
	TaskStoppedNoReturn = "no_return"
)

// TaskStoppedRecord is the notice that a task of the issue stopped where it stands, its issue held
// for it (record.HoldTaskStopped), told to the architect that owns the issue (workflow's
// taskStopped and stopUnreturned): the role whose phase the hold is from, that phase, the task, and
// what stopped it (a TaskStopped word). A stop by the owner's stop_task names the architect claim
// that made the call (StoppedBy, the one notice that carries it), and the daemon's notice executor
// publishes it only where the architect the issue's notices reach is another claim, since the call
// answers its own caller. A stop by a failed step names that step by its place in the task (Step),
// its role being Role. stop_task carries no words of its caller, so the notice carries none.
// Its builders are workflow's StopUnreturnedNotice and OwnerStoppedNotice.
type TaskStoppedRecord struct {
	NoticeHead
	Role      claim.Role  `json:"role,omitempty"`
	Phase     phase.Phase `json:"phase,omitempty"`
	Task      string      `json:"task"`
	Cause     string      `json:"cause"`
	Step      int         `json:"step,omitempty"`
	StoppedBy claim.Token `json:"stoppedBy,omitempty"`
}

// The words of the plan the first step of an issue runs, which a plan-not-taken notice names
// (PlanNotTakenRecord.Runs): a version an owner of the issue's tree gave, or the project's default
// steps (record.PlanSourceOwner, record.PlanSourceDefault).
const (
	PlanRunsOwner   = "owner"
	PlanRunsDefault = "default"
)

// The words of why the first step of an issue took no plan given for it, which a plan-not-taken
// notice names (PlanNotTakenRecord.Why): the plan was an earlier owner's, whose giver is no
// architect of the issue's tree now (workflow's tellPassedOver), or the issue's experiment drew
// it into a group whose issues run the project's default steps, which takes no plan from the
// issue's owner (workflow's tellNotTaken, takesNoPlan).
const (
	PlanNotTakenEarlierOwner = "earlier_owner"
	PlanNotTakenGroup        = "group"
)

// PlanNotTakenRecord is the notice that the first step of the issue took no plan given for it, in
// one of two meanings, which Why names (a PlanNotTaken word). Either way the first step deletes
// the rows of each plan it passed over and tells the architect that owns the issue once, whatever
// number of versions it passed over, and it names each version once (workflow's namedOnce): a
// rerun carries a plan to its new generation, so the plans it passed over can hold a copy of one
// version for each generation that version ran. Each such version is named with its document's
// artifact and its giver (NotTaken), and the plan the first step runs instead is named too (Runs,
// a PlanRuns word), with its version, its artifact and its giver where it is an owner's.
//
// PlanNotTakenEarlierOwner is a plan an earlier owner gave (workflow's passOverEarlierOwners,
// tellPassedOver, PlanNotTakenNotice): its giver is no architect of the issue's tree now
// (record.ArchitectOfTree), as after an orphan is admitted again as a root of its own or an
// adoption moves a member to another tree, and the plan the first step runs is an owner's, of the
// issue's generation or carried to it, or the default steps. The architect may give such a plan
// again with register_plan as its own, which its prompt says.
//
// PlanNotTakenGroup is every plan its owner or an earlier owner gave for an issue whose
// experiment's group takes no plan from its owner (workflow's passOverOwners, tellNotTaken,
// GroupPlanNotTakenNotice): the experiment and the variant the issue was drawn into, which name
// that group (intake's GroupClause), and the run takes the project's default steps, so Runs is
// PlanRunsDefault. No plan given for such an issue is taken: register_plan on it is answered
// PLAN_NOT_TAKEN, and the architect's prompt says to give it none.
type PlanNotTakenRecord struct {
	NoticeHead
	Why          string             `json:"why"`
	NotTaken     []PlanVersionFacts `json:"not_taken"`
	Runs         string             `json:"runs"`
	RunsVersion  int                `json:"runs_version,omitempty"`
	RunsArtifact string             `json:"runs_artifact,omitempty"`
	RunsGivenBy  string             `json:"runs_given_by,omitempty"`
	Experiment   string             `json:"experiment,omitempty"`
	Variant      string             `json:"variant,omitempty"`
}

// PlanVersionFacts is one version of an owner's plan as a notice names it: its version, the
// Dispatch artifact of its document, and the architect claim that gave it (record.PlanVersion).
type PlanVersionFacts struct {
	Version  int    `json:"version"`
	Artifact string `json:"artifact"`
	GivenBy  string `json:"given_by"`
}

// RetroHeldRecord is the notice that the issue, a parent whose task's last step is not the owner's
// acceptance, passed its last step and is held before its retro while issues beneath it are still
// in the workflow (workflow's retroHeldNotice, record.HoldForChildren): the retro and the
// implementer whose work the hold keeps (Phase, Role), each issue beneath it still in the workflow,
// which the retro waits for (WaitingFor), and, since the notice tells the architect to end a
// child, the two texts dispatch://INFRA-3178's R6 has such a notice say, word for word as main's
// notice gave them, which R8 of dispatch://INFRA-3686 keeps: record.StatusNotTakenYet
// (StatusNotTakenYet) and the eight things of record.EndChildRule (EndChildRule). The daemon
// starts the retro by itself when the last of them leaves the workflow (workflow's
// releaseHeldParents).
type RetroHeldRecord struct {
	NoticeHead
	Role              claim.Role  `json:"role,omitempty"`
	Phase             phase.Phase `json:"phase,omitempty"`
	WaitingFor        []string    `json:"waiting_for"`
	StatusNotTakenYet string      `json:"status_not_taken_yet"`
	EndChildRule      string      `json:"end_child_rule"`
}
