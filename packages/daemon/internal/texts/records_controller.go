package texts

import (
	"encoding/json"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// This file holds the records of the notices to the controller. Each is one record.Notice on the
// wire, as a notice to an architect is, so the rules of records_notice.go hold here too.

// TriageRecord is the notice of an issue in triage whose root no architect owns (admit's triage
// wake). The issue is the whole of it.
type TriageRecord struct {
	NoticeHead
}

// SlotFreeRecord is the notice that admission released an issue's slot and no waiting issue took
// it (admit's slot-free wake). The issue is the whole of it.
type SlotFreeRecord struct {
	NoticeHead
}

// TodoRecord is the notice of an issue not handed to Legion that changed in todo while a slot
// stood free (admit's todo wake). The issue is the whole of it.
type TodoRecord struct {
	NoticeHead
}

// TickRecord is the daemon's periodic wake of the controller (admit's tick), whose issue is the
// project key. It names its lists whole, since the controller's daily report reads them there:
// every open row of the capability report, every issue blocked by another, every issue the
// daemon's record holds that stands in neither the active set nor the waiting line, and every
// phase worker nothing will prompt. Each key keeps the shape the controller's readers take.
type TickRecord struct {
	NoticeHead
	OpenCapabilities []string       `json:"openCapabilities,omitempty"`
	Blocked          []BlockedIssue `json:"blocked,omitempty"`
	Held             []HeldIssue    `json:"held,omitempty"`
	Silent           []SilentIssue  `json:"silent,omitempty"`
}

// BlockedIssue is one issue Legion waits on another issue for, and the issues it waits on that
// are not done (record.BlockedIssue is this type).
type BlockedIssue struct {
	Issue     string   `json:"issue"`
	BlockedBy []string `json:"blockedBy"`
}

// MarshalJSON keeps blockedBy an array on the wire, empty rather than null: the plugin's reader
// refuses a null.
func (b BlockedIssue) MarshalJSON() ([]byte, error) {
	type wire BlockedIssue
	out := wire(b)
	if out.BlockedBy == nil {
		out.BlockedBy = []string{}
	}
	return json.Marshal(out)
}

// HeldIssue is one issue the daemon's record holds that stands in neither the active set nor the
// waiting line: its key, the one word for why (a record.HeldReason) and the issues that hold it,
// as record.HeldIssue names them.
type HeldIssue struct {
	Issue  string   `json:"issue"`
	Reason string   `json:"reason"`
	HeldBy []string `json:"heldBy,omitempty"`
}

// SilentIssue is one issue whose phase worker nothing will prompt (record.SilentIssue is this
// type): the issue, the role, when its agent last stepped, how long it has been quiet, and, for a
// worker that waits on its own background work, the moment of that report. The last step and the
// time quiet are left out where the claim's record holds no step of its agent, which only a
// background stall, judged by its report, can name (admit's silentWorkers): a zero time stated
// would be a step at year 1. A row an older daemon queued, which wrote them always, reads back the
// same.
type SilentIssue struct {
	Issue           string     `json:"issue"`
	Role            claim.Role `json:"role"`
	LastStep        time.Time  `json:"lastStep,omitzero"`
	Silent          string     `json:"silent,omitempty"`
	BackgroundSince time.Time  `json:"backgroundSince,omitzero"`
}

// The words of why a root that awaits a person cannot resume (UnslottedRecord.Unresumable,
// workflow's Unresumable): its legion label was removed, it left the workflow, or a person or a
// session outside its tree holds its Dispatch claim, which ClaimedBy names.
const (
	UnresumableLabelRemoved = "label-removed"
	UnresumableLeftWorkflow = "left-workflow"
	UnresumableClaimed      = "claimed"
)

// UnslottedRecord is the notice that admission found a root with neither a slot nor a place in
// the waiting line and set its phase to done (admit's releaseUnslotted): the status the root stood
// at, its phase, and, for a root that awaits a person, why it cannot resume (an Unresumable word)
// with, for a Dispatch claim, who holds it as Dispatch names the holder (`<kind>:<id>`).
type UnslottedRecord struct {
	NoticeHead
	Status      string      `json:"status"`
	Phase       phase.Phase `json:"phase,omitempty"`
	Unresumable string      `json:"unresumable,omitempty"`
	ClaimedBy   string      `json:"claimed_by,omitempty"`
}

// AwaitingPersonRecord is the notice that a root waits on a person and keeps its slot (workflow's
// enterAwaiting): the ask it waits on (a dispatch:// link), or the document whose version waits
// on a person's review (a dispatch:// link) and that version.
type AwaitingPersonRecord struct {
	NoticeHead
	Ask      string `json:"ask,omitempty"`
	Document string `json:"document,omitempty"`
	Version  int    `json:"version,omitempty"`
}

// ParkedDecisionRecord is the notice of a person's decision on a tree Legion is not running
// (record's RelayParkedDecision, the daemon's parkDecision): what the person did (a
// record.DecisionKind word), the tree's root, the decision's reference, who and when, the version
// of a spec decision, the reason of a changes request (words), the answer of an ask (words) with
// the options it chose. Each words field is bounded at OneAct.
//
// A person's approval of a parked root's primary document that Legion did not queue as their
// handover of the root, because the root's status changed after the approval (record's
// RelayRefusedHandover, workflow's refuseHandover), says so as fields: the Dispatch event of the
// approval (ApprovalSeq), the first later event that changed the root's status
// (StatusChangedSeq), the status Dispatch showed the root at when the daemon read the approval,
// after that change (Status), that the approval queued nothing (NotQueued), and the issue whose
// todo brings the root back (SetTodo). Main's notice (40b01992) said each in its reason ("the
// approval, at Dispatch event %d, came before a change of %s's status: its status changed after
// the approval, by event %d, so Legion did not queue %s as the person's handover; set %s to todo
// to bring it back"), and R8 of dispatch://INFRA-3686 keeps each as a field; the step of setting
// the root to todo is the controller's prompt's (roles/controller-root.md), and the status is a
// fact the daemon read that main's sentence did not name.
type ParkedDecisionRecord struct {
	NoticeHead
	Decision         string               `json:"decision,omitempty"`
	Root             string               `json:"root,omitempty"`
	Ref              string               `json:"ref,omitempty"`
	Actor            string               `json:"actor,omitempty"`
	At               time.Time            `json:"at,omitzero"`
	Version          int                  `json:"version,omitempty"`
	ApprovalSeq      int64                `json:"approval_seq,omitempty"`
	StatusChangedSeq int64                `json:"status_change_seq,omitempty"`
	Status           string               `json:"status,omitempty"`
	NotQueued        bool                 `json:"not_queued,omitempty"`
	SetTodo          string               `json:"set_todo,omitempty"`
	Reason           string               `json:"reason,omitempty" texts:"words"`
	Answer           string               `json:"answer,omitempty" texts:"words"`
	Options          []string             `json:"options,omitempty"`
	Words            map[string]WordsMeta `json:"words,omitempty"`
}

// The words of what holds an update round past its bound (UpdateStalledRecord.Holds). No process
// in the round's pod (HoldsNoProcess) holds a round past its bound only where the runtime replaces
// no pod by issue (runtime.IssuePodReplacer): where it does, the daemon replaces the pod once the
// bound has come, and only a refusal of that replacement is told (HoldsRuntimeRefused). A pod the
// runtime cannot read (HoldsPodUnread) decides nothing, so the round waits for a read of it that
// succeeds.
const (
	HoldsNoProcess      = "no-process"
	HoldsTwoPods        = "two-pods"
	HoldsOperator       = "operator"
	HoldsRuntimeRefused = "runtime-refused"
	HoldsNoPod          = "no-pod"
	HoldsPodUnread      = "pod-unread"
)

// UpdateStalledRecord is the notice that an update round is held past its bound (the daemon's
// tellUpdateStalled): what holds it (a Holds word), the runtime's refusal of the replacement, and
// the round.
type UpdateStalledRecord struct {
	NoticeHead
	Holds   string       `json:"holds"`
	Refusal string       `json:"refusal,omitempty"`
	Update  *UpdateRound `json:"update,omitempty"`
}

// UpdateRound is the update round an update-stalled notice names (record.UpdateStalled is this
// type): the round, the pod it was opened over, each changed image by its short digest and each
// changed setting by its key, and each claim of the issue with a session that has not failed,
// those the round passes over among them. Reasons is the text of the drift that a round an older
// daemon opened records, and empty for a round this daemon opened, which names its drift by those
// items alone: an empty reasons is left out, as the update ask leaves out its own
// (UpdateAskRecord.Reasons), so no notice states `reasons: ""`.
type UpdateRound struct {
	Round    string          `json:"round"`
	Pod      string          `json:"pod,omitempty"`
	Images   []string        `json:"images,omitempty"`
	Settings []string        `json:"settings,omitempty"`
	Reasons  string          `json:"reasons,omitempty"`
	Waiting  []UpdateWaiting `json:"waiting"`
}

// MarshalJSON keeps waiting an array on the wire, empty rather than null: the plugin's strict
// reader refuses a null.
func (u UpdateRound) MarshalJSON() ([]byte, error) {
	type wire UpdateRound
	out := wire(u)
	if out.Waiting == nil {
		out.Waiting = []UpdateWaiting{}
	}
	return json.Marshal(out)
}

// SuspendedByPause is what an update-stalled notice's suspended_by says of a claim that a
// project's pause stopped, whether or not the claim answered the round: the pause, and no idle
// release of the claim's pod, suspended it. It names two cases. In the first the pause still owes
// the claim a launch (the daemon's podHolds.owedLaunch): the claim's role has not run in the pod
// since the pause stopped it, and before the round's bound the round asks such a claim once its
// agent is ready and waits for it while it has not answered (podHolds.owes). In the second the
// pause stopped the claim while it held no task and met no launch, owing it none, and neither a
// release of the pod that suspended the claim's role nor a ready of the claim came after that stop
// (the daemon's stoppedByAPause, store.StoppingPause): a release that suspended other roles alone
// left the claim as the stop left it. The notice is told only past the bound, where no
// answer holds the round, so the word names who suspended the claim and nothing the round waits
// for. The claim's own source is released (supervise.SuspendedByReleased), as an idle release's
// is, so this word is the notice's, read from the pause's hold of the pod and its record of the
// pause's stops.
const SuspendedByPause = "pause"

// UpdateWaiting is one claim an update-stalled notice lists (record.UpdateWaiting is this type):
// its token, its role, its state, whether it answered, and, for a suspended claim, who suspended
// it: a supervise.SuspendSource word (operator for the operator's suspension, which only the
// operator's resume ends, released for the daemon's idle release of its pod, and daemon, exit or
// unknown for a suspension of the daemon's own), or SuspendedByPause for a claim a project's pause
// stopped, owing it a launch or not. A claim not suspended yet names operator too where its machine
// holds the operator's suspension for its agent's turn or launch (supervise.Claim.SuspensionHeld).
// LeftRound says that the claim left the round's pod (supervise.Claim.LeftPodRound): its process
// came up in another pod while the round was open, so the round asks it nothing, and an operator's
// suspension of it does not hold the round's pod.
type UpdateWaiting struct {
	Claim       string     `json:"claim"`
	Role        claim.Role `json:"role,omitempty"`
	State       string     `json:"state"`
	Answered    bool       `json:"answered,omitempty"`
	SuspendedBy string     `json:"suspended_by,omitempty"`
	LeftRound   bool       `json:"left_round,omitempty"`
}

// ProviderErrorsRecord is the notice that a role ended turns in a row in a provider error
// (supervise's tellControllerOfProviderErrors): the role, the count of those turns, the model, the
// HTTP status the provider answered, and what the daemon does about the last of those turns.
// Where that turn ran a prompt of the daemon's, the daemon keeps its task and sends it again, and
// RetryEvery names how often at most. Where it ran none — a phase reminder, an Envoy message or a
// background job's result — LastTurnUnprompted says so, and the notice names no RetryEvery: the
// daemon has no task to send again for that turn. Main's notice (1e54a54b) said each in its reason
// ("%d turns in a row%s ended in %s; %s", the last part either "the daemon keeps the task and sends
// it again every %s at most" or "the last of them ran no prompt of the daemon's (a phase reminder,
// an Envoy message or a background job's result), so the daemon sends no task again for it"), and
// R8 of dispatch://INFRA-3686 keeps each fact as a field; what the daemon does for each is the
// controller's prompt's (roles/controller-root.md).
type ProviderErrorsRecord struct {
	NoticeHead
	Role               claim.Role `json:"role,omitempty"`
	Turns              int        `json:"turns"`
	Model              string     `json:"model,omitempty"`
	HTTPStatus         int        `json:"http_status,omitempty"`
	RetryEvery         string     `json:"retry_every,omitempty"`
	LastTurnUnprompted bool       `json:"last_turn_unprompted,omitempty"`
}

// TreeStalledRecord is the notice that a tree has stopped (admit's tellStalledTrees): how long the
// tree has been quiet, each issue the workflow still runs, the step the controller takes first
// (First, TreeStalledFirst: read the tree) and what it decides among once it has read the tree
// (Decide, TreeStalledChoices): ask a role what it waits on, or park the tree. dispatch://INFRA-2560's
// R5 has the notice tell the controller to read the tree and decide, and main's notice says it
// ("Read the tree and decide what it needs. You can ask a role what it waits on. You can park the
// tree."), which since cybermonklabs/legion#4622 names no move of an issue back a phase: no operation
// of the controller's `legion` tool makes that move. R8 of dispatch://INFRA-3686 keeps that step as
// fields: the read in First, the decision in Decide.
type TreeStalledRecord struct {
	NoticeHead
	QuietFor string       `json:"quiet_for"`
	Members  []TreeMember `json:"members"`
	First    string       `json:"first"`
	Decide   []string     `json:"decide"`
}

// TreeStalledFirst is the step a tree-stalled notice has the controller take first, before it
// decides among TreeStalledChoices: read the tree (TreeStalledRecord.First).
const TreeStalledFirst = "read_tree"

// The choices a tree-stalled notice names for the controller once it has read the tree
// (TreeStalledRecord.Decide).
const (
	DecideAskRole = "ask_role_what_it_waits_on"
	DecidePark    = "park_tree"
)

// TreeStalledChoices is what a tree-stalled notice decides among, in main's order: ask a role what
// it waits on, park the tree. Each call is a list of its own, so no notice holds a list another can
// write.
func TreeStalledChoices() []string { return []string{DecideAskRole, DecidePark} }

// TreeMember is one issue of a stopped tree: its key, its phase, the role whose turn it is (empty
// where no role works the phase), and its pull request with that pull request's state (each empty
// where the record holds none).
type TreeMember struct {
	Key              string      `json:"key"`
	Phase            phase.Phase `json:"phase"`
	Turn             claim.Role  `json:"turn"`
	PullRequest      string      `json:"pull_request"`
	PullRequestState string      `json:"pull_request_state"`
}

// ArchitectStalledRecord is the notice that a root's own architect has stalled (admit's
// architectStalls): the architect's role, the arm that read it (a record.ArchitectStallArm word)
// and the root's phase. Took nothing: the phase entry of the task, when it was first queued,
// whether a let-go took it back from a turn, and the moment the arm counts from where that is
// later than the first queueing (the relaunched agent's ready, or the agent's last ready). Claim
// unreachable: the claim's state, whether its relaunch is failing, whether the daemon wrote the
// start of its architect again, and whether the root waits on a person; for a root with no claim,
// the entry whose work has stood and since when. Slow before the gate: the later of the root's
// admission and its entry's last raise. Each arm names its bound, apart from a claim-unreachable
// arm whose relaunch is failing, which waits on no bound.
//
// Where an arm counted its bound from a moment later than the one it names, the notice states
// that moment (CountedFrom), what it is (a CountedFrom word: the end of the project's latest pause
// that held its architect, the let-out of the architect's launch that pause held, or the first
// confirmation of the entry's task by a turn of the architect, which a slow-before-gate arm counts
// from whether or not a pause came first) and the pause it is of (PauseID, none for a
// confirmation). A slow-before-gate arm that counted from a confirmation names the entry
// confirmed (Entry).
//
// Reason is the arm in the words skills/legion-controller/references/wakes.md quotes it by ("the
// architect took nothing", "the architect's claim cannot be reached", "the root is slow before its
// gate", record.ArchitectStallArm.Words), word for word as main's reason began: that skill reads
// the notice's reason as beginning with its arm, and R8 of dispatch://INFRA-3686 keeps a sentence
// a skill names by its words.
type ArchitectStalledRecord struct {
	NoticeHead
	Role             claim.Role  `json:"role,omitempty"`
	Arm              string      `json:"arm"`
	Reason           string      `json:"reason,omitempty"`
	Phase            phase.Phase `json:"phase,omitempty"`
	Entry            int64       `json:"entry,omitempty"`
	QueuedAt         time.Time   `json:"queued_at,omitzero"`
	Interrupted      bool        `json:"interrupted,omitempty"`
	Since            time.Time   `json:"since,omitzero"`
	ClaimState       string      `json:"claim_state,omitempty"`
	RelaunchFailing  bool        `json:"relaunch_failing,omitempty"`
	StartWritten     bool        `json:"start_written,omitempty"`
	AwaitsPerson     bool        `json:"awaits_person,omitempty"`
	Bound            string      `json:"bound,omitempty"`
	CountedFrom      time.Time   `json:"counted_from,omitzero"`
	CountedFromEvent string      `json:"counted_from_event,omitempty"`
	PauseID          int64       `json:"pause_id,omitempty"`
}

// The words of what an architect-stalled arm counted its bound from, where that is later than the
// moment the arm names (ArchitectStalledRecord.CountedFromEvent).
const (
	CountedFromPauseEnd       = "pause-end"
	CountedFromLaunchLetOut   = "launch-let-out"
	CountedFromEntryConfirmed = "entry-confirmed"
)

// MemoryGuardKillRecord is the notice that one of the controller's own processes was stopped for
// memory (workflow's ControllerMemoryGuardKillNotice), whose issue is the project key, as a tick's
// is: what stopped it (a Kill word: the memory guard of the controller's pod, or the microVM pod's
// own kernel, of a command or of one of the processes that run the controller's session), the
// process by its executable as the launcher sent it, with none of its arguments, its pid, how much
// it held, the pod's memory reading and the pod's limit, each size in binary units, as a
// memory-kill message names them (MemoryKillRecord). The pod's reading is its anonymous memory for
// the guard and its memory counter for the kernel. It names no role: the process is the
// controller's on every cause, the controller is the notice's one reader, and a card for a daemon
// notice carries no role of its reader (dispatch://INFRA-3686's R4), so main's `role: controller`
// is no field, as main's `role: architect` is none of the checks-red notice.
// dispatch://INFRA-3531's R2, which the approved R1 of its parent dispatch://INFRA-3368 covers, has
// the notice carry the pid, the executable's base name, the size, the pod's reading and the limit,
// and no argument; R8 of dispatch://INFRA-3686 keeps each as a field. What main's notice said
// beside them (that the controller's session and the pod's other processes go on, what to run in
// place of the same program, and what the end of one of the session's own processes does) is the
// controller's prompt's (roles/controller-root.md): no approved requirement fixes those words.
type MemoryGuardKillRecord struct {
	NoticeHead
	Cause     string `json:"cause"`
	Process   string `json:"process"`
	PID       int    `json:"pid"`
	Size      string `json:"size"`
	PodMemory string `json:"pod_memory"`
	Limit     string `json:"limit"`
}

// ProjectPausedRecord is the notice that the operator paused the project (the pause package's
// pausedNotice, decision 24 of dispatch://INFRA-3759), whose issue is the project key: the pause
// the act began, who paused, when, and the reason they gave. The reason is their words
// (pause.reason), bounded at OneAct and named in the words object with who paused and when.
type ProjectPausedRecord struct {
	NoticeHead
	Pause *ProjectPause        `json:"pause,omitempty"`
	Words map[string]WordsMeta `json:"words,omitempty"`
}

// ProjectResumedRecord is the notice that the operator resumed the project (the pause package's
// resumedNotice), whose issue is the project key: the pause the act ended, which is its why, who
// resumed, and when. It carries no reason, so it names no words; it declares the words object
// because the key pause has one type in every notice record, and that type holds a reason.
type ProjectResumedRecord struct {
	NoticeHead
	Pause *ProjectPause        `json:"pause,omitempty"`
	Words map[string]WordsMeta `json:"words,omitempty"`
}

// ProjectPause is the act a project-paused or a project-resumed notice tells (record.ProjectPause
// is this type): the pause begun or ended (its id), who acted, when, and, on a project-paused
// notice alone, the reason the pause was given, which is the words of who paused.
type ProjectPause struct {
	ID     int64      `json:"id"`
	By     PauseActor `json:"by"`
	At     time.Time  `json:"at"`
	Reason string     `json:"reason,omitempty" texts:"words"`
}

// PauseActor is who paused a project or ended its pause (record.PauseActor is this type): the
// kind controller with the controller's session id, or the kind operator with no session.
type PauseActor struct {
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
}

// IntakeGaveUpRecord is the notice that the intake gave up messages whose handling kept failing
// (record's GaveUpNotice, which Postgres.countIntakeGiveUp opens and joins), whose issue is the
// project key, as a tick's is: how many it gave up while the notice waited for its publish
// (Count), and the first of them, at most a hundred (record's giveUpNoticeNames), each by the id
// of its record, the consumer that delivered it and its subject (GiveUps); the controller reads
// the rest in the state, which counts each consumer's open give-ups. It names no role: the message
// is the intake's, of no issue an architect runs, and the controller is the notice's one reader,
// which replays or discards each give-up (dispatch://INFRA-1575). Main's notice (40b01992) carried
// its kind, the count and the give-ups under these keys and nothing else, so it gave its reader no
// sentence and no step; R8 of dispatch://INFRA-3686 keeps each as a field, and the record adds the
// project key as its issue. Both keys keep main's shape, omitempty, though the outbox refuses a
// notice of this kind that counts or names none.
//
// It has no urgency rule (rules.go), so it is Med for the controller, its one reader. Each
// message it names stays given up, its fact unapplied, until a replay or a discard closes it, and
// the controller must replay or discard it (replay_give_up, discard_give_up): it is no text to
// leave for a later turn.
type IntakeGaveUpRecord struct {
	NoticeHead
	Count   int      `json:"count,omitempty"`
	GiveUps []GiveUp `json:"giveUps,omitempty"`
}

// GiveUp is one message an intake-gave-up notice names (record.NoticeGiveUp is this type): the id
// of its record in intake_failures, which a replay or a discard names, the consumer that delivered
// it, and its subject as the store keeps text.
type GiveUp struct {
	ID       int64  `json:"id"`
	Consumer string `json:"consumer"`
	Subject  string `json:"subject"`
}
