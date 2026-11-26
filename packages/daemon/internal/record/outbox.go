package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
)

// OutboxKind names one durable side effect.
type OutboxKind string

const (
	OutboxKindDispatchStatus   OutboxKind = "dispatch_status"
	OutboxKindDispatchMessage  OutboxKind = "dispatch_message"
	OutboxKindNotice           OutboxKind = "notice"
	OutboxKindControllerNotice OutboxKind = "controller_notice"
	OutboxKindSupervise        OutboxKind = "supervise"
	OutboxKindGateSeed         OutboxKind = "gate_seed"
	OutboxKindLingerClose      OutboxKind = "linger_close"
	OutboxKindIssueSuspend     OutboxKind = "issue_suspend"
	OutboxKindWorkspaceRemove  OutboxKind = "workspace_remove"
	OutboxKindIssueBranch      OutboxKind = "issue_branch"
	OutboxKindCheckWake        OutboxKind = "check_wake"
	OutboxKindCheckLook        OutboxKind = "check_look"
	OutboxKindCheckBound       OutboxKind = "check_bound"
	OutboxKindRoleMessage      OutboxKind = "role_message"
	// OutboxKindPhaseEnded tells the worker of a parent the admission pass moved into
	// awaiting_children that its phase ended (PhaseEnded).
	OutboxKindPhaseEnded OutboxKind = "phase_ended"
	OutboxKindHookPrompt OutboxKind = "hook_prompt"
	OutboxKindUpdateAsk  OutboxKind = "update_ask"
	// OutboxKindIssueBundle is the kind of the idle release's workspace check
	// (IssueWorkspaceCheck). The check replaced the release's save of a bundle, and its kind keeps
	// the save's stored name, which this identifier spells: a row either release queued reads as
	// the first act of a release, so a row an earlier daemon queued before an upgrade runs as this
	// check, and a row this daemon queued before a rollback runs as that release's save, with no
	// migration of the outbox's kind check and no row left that one release cannot run.
	OutboxKindIssueBundle  OutboxKind = "issue_bundle"
	OutboxKindIssueRelease OutboxKind = "issue_release"
	// OutboxKindGatedStepsPublish is the one kind whose row belongs to no issue: its issue column
	// holds the bare project key (GatedStepsPublish).
	OutboxKindGatedStepsPublish OutboxKind = "gated_steps_publish"
	OutboxKindClaimRelease      OutboxKind = "claim_release"
	// OutboxKindDesignGatesPublish publishes one issue's design-gate documents onto its Dispatch
	// row (DesignGatesPublish). Unlike the gated_steps_publish above it, this row's issue column
	// is an issue key like every other kind's.
	OutboxKindDesignGatesPublish OutboxKind = "design_gates_publish"
	// OutboxKindMergeQueueRead reads a pull request once after GitHub reported that it left the
	// merge queue (MergeQueueRead), and OutboxKindMergeQueueRemoval tells the issue's merger that it
	// left (MergeQueueRemoval).
	OutboxKindMergeQueueRead    OutboxKind = "merge_queue_read"
	OutboxKindMergeQueueRemoval OutboxKind = "merge_queue_removal"
	// OutboxKindPendingBasisSeed reads the ancestor's approval request a root's step waits on
	// (PendingBasisSeed), and OutboxKindReparentSeed the new Dispatch parent of a root that ran as
	// a tree of its own (ReparentSeed). Each applies what it read as a fact, as the gate seed
	// does.
	OutboxKindPendingBasisSeed OutboxKind = "pending_basis_seed"
	OutboxKindReparentSeed     OutboxKind = "reparent_seed"
	// OutboxKindDesignChanges reconciles one tree's news of its approved design
	// (DesignNewsReconcile): it reads what changed between the versions its holders took and the
	// versions approved now, and queues the starts that tell them. Its issue column is the tree's
	// root.
	OutboxKindDesignChanges OutboxKind = "design_changes"
	// OutboxKindRoleHoldersPublish publishes the live role sessions of one issue onto its Dispatch
	// row (RoleHoldersPublish).
	OutboxKindRoleHoldersPublish OutboxKind = "role_holders_publish"
	// OutboxKindMergeHoldsChanged tells the merger of an issue in merging that the project's merge
	// holds document changed (MergeHoldsChanged).
	OutboxKindMergeHoldsChanged OutboxKind = "merge_holds_changed"
	// OutboxKindStepRecord writes one start or one end of a step of the issue onto Dispatch's
	// record of that step, or the rating a retro gave the plan onto the retro's record
	// (StepRecord).
	OutboxKindStepRecord OutboxKind = "step_record"
	// OutboxKindConflictConfirm reads a pull request's head again a minute after a first read
	// found it conflicting with its base (ConflictConfirm), and OutboxKindMergeForward carries one
	// message of a merge forward, the daemon's act on a confirmed conflict, to one role of the
	// issue (MergeForwardMessage).
	OutboxKindConflictConfirm OutboxKind = "conflict_confirm"
	OutboxKindMergeForward    OutboxKind = "merge_forward"
	// OutboxKindPlanRecord writes the plan one run of the issue carries out onto Dispatch
	// (PlanRecord).
	OutboxKindPlanRecord OutboxKind = "plan_record"
)

// OutboxLane is which executor of an issue runs a kind. An issue's rows run on six lanes side by
// side. The lifecycle lane runs the rows that start, stop and tidy the issue's roles and their
// resources, one at a time in the order they were queued: each assumes the state its predecessor
// leaves (a start after the branch it waits for, a workspace removal after the suspend that
// stopped the roles). It also carries the production check's rows
// (check_wake, check_look, check_bound), which start and end the checking role's looks in order
// with the starts and suspends they queue. The board lane runs the issue's Dispatch writes and
// notice publishes beside them, so a status write or a notice never waits behind a start pulling
// an image, and a slow publish never holds a stop back. It carries the deployment's gated-steps
// publish too, the one row addressed to the bare project key rather than to an issue, so that
// write runs on a board lane of the project's own, behind none of any issue's rows and holding
// none of them back. The role-message lane runs the kinds the daemon addresses to one role of the
// issue on that role's own topic: the end of a parent's phase that its open children stopped, which
// goes to that phase's worker (phase_ended), a hook's words, which go to a phase worker
// (hook_prompt), the messages that tell the merger its pull request left the merge queue
// (merge_queue_removal) and that the project's merge holds changed (merge_holds_changed), the
// messages of a merge forward, which go to the role asked to merge (MergeForwardAsker) and to the
// worker of the phase the issue stays in (merge_forward), and an update round's ask, which goes to
// any role whose process runs in the issue's pod, its architect included (update_ask). It also runs
// the two reads of GitHub that come before a message: the read before a merge-queue removal's
// message (merge_queue_read), and the second read of a head a first read found conflicting, whose
// confirmation is what writes a merge forward's messages (conflict_confirm). It is a lane of its
// own because the board lane's architect notices are ordered among themselves and one of them waits
// for as long as its architect holds no role (the daemon's errNoticeWaits): a worker told that its
// open children ended its phase, a role given the words a hook fired on a move back, told that its
// pull request left the merge queue or that the merge holds changed, asked to merge the base
// forward or to hold while that merge runs, or asked to get ready for the replacement of its pod
// must hear it while that is news, not after whatever the architect that owns its issue is owed.
// The claim lane runs the release of an issue's Dispatch claim (claim_release) on a lane of its
// own, because that row may wait up to an hour on a holder that has not stopped, and an hour of
// waiting on one issue's claim must delay no status write and no notice of that same issue, which
// is what a place on the board lane would cost it.
// The documents lane runs the reconcile of a tree's news of its approved design (design_changes),
// which reads Dispatch for what changed between two versions of a gated document, on a lane of its
// own: on the lifecycle lane that read would hold every start, stop and suspend of the tree's
// root behind it, and on the board lane it would wait behind a notice that waits as long as its
// architect holds no role, while the news it sends is owed to every holder of the tree.
// The record lane runs the writes of the records of the issue's work — each step's record and the
// rating a retro gave the plan (step_record), and the plan of each run (plan_record) — on a lane of
// their own, so a Dispatch that is slow to take a record, or one that backs a record off until it
// has the route, delays no status write, notice or start of that issue: a record is history, and
// the work it records never waits for it.
type OutboxLane string

const (
	LaneLifecycle   OutboxLane = "lifecycle"
	LaneBoard       OutboxLane = "board"
	LaneRoleMessage OutboxLane = "role_message"
	LaneClaim       OutboxLane = "claim"
	LaneDocuments   OutboxLane = "documents"
	LaneRecord      OutboxLane = "record"
)

// laneKinds is each lane's kinds in the order LaneKinds lists them. Every OutboxKind is in exactly
// one lane (TestEveryOutboxKindIsInExactlyOneLane). The three seeds — the gate's, a pending
// basis's and a re-parented root's — each read Dispatch and apply what they read as a fact, and
// run on the lifecycle lane together.
var laneKinds = map[OutboxLane][]OutboxKind{
	LaneLifecycle:   {OutboxKindSupervise, OutboxKindIssueSuspend, OutboxKindIssueBundle, OutboxKindIssueRelease, OutboxKindLingerClose, OutboxKindWorkspaceRemove, OutboxKindGateSeed, OutboxKindPendingBasisSeed, OutboxKindReparentSeed, OutboxKindIssueBranch, OutboxKindCheckWake, OutboxKindCheckLook, OutboxKindCheckBound},
	LaneBoard:       {OutboxKindDispatchStatus, OutboxKindDispatchMessage, OutboxKindNotice, OutboxKindControllerNotice, OutboxKindRoleMessage, OutboxKindGatedStepsPublish, OutboxKindDesignGatesPublish, OutboxKindRoleHoldersPublish},
	LaneRoleMessage: {OutboxKindPhaseEnded, OutboxKindHookPrompt, OutboxKindUpdateAsk, OutboxKindMergeQueueRead, OutboxKindMergeQueueRemoval, OutboxKindMergeHoldsChanged, OutboxKindConflictConfirm, OutboxKindMergeForward},
	LaneClaim:       {OutboxKindClaimRelease},
	LaneDocuments:   {OutboxKindDesignChanges},
	LaneRecord:      {OutboxKindStepRecord, OutboxKindPlanRecord},
}

// LaneOf is the lane that runs kind.
func LaneOf(kind OutboxKind) OutboxLane {
	for lane, kinds := range laneKinds {
		if slices.Contains(kinds, kind) {
			return lane
		}
	}
	return ""
}

// LaneKinds is the kinds lane runs, in a fixed order; nil for a lane that is not one.
func LaneKinds(lane OutboxLane) []OutboxKind {
	return slices.Clone(laneKinds[lane])
}

// OutboxLanes is every lane an issue's rows run on, in name order: the lanes laneKinds holds, so
// a reader that needs every lane, or every kind, takes them from here rather than from a list of
// its own, which a new lane would leave short.
func OutboxLanes() []OutboxLane {
	return slices.Sorted(maps.Keys(laneKinds))
}

// LaneKey names one lane of one issue.
type LaneKey struct {
	Issue string
	Lane  OutboxLane
}

// HeldRow is a due lifecycle row its lane cannot run yet: WaitsOn is the id of the row of its issue
// under the live lease it waits behind (Store.HeldRows).
type HeldRow struct {
	OutboxRow
	WaitsOn int64
}

// pauseHold is one shape of outbox row a project's pause holds: every row of kind, or, where op
// is set, the rows of kind whose supervise op is op.
type pauseHold struct {
	kind OutboxKind
	op   SuperviseOp
}

// pauseHolds is the one list of the rows a project's pause holds (decision 9 of
// dispatch://INFRA-3759): every row that launches a role, hands it a task or a message, or starts
// or ends a production check's look. Those are a supervise row whose op is `start`, an architect's
// notice (which resumes a released architect, the daemon's wakeForDelivery), the five messages to
// one role (phase_ended, hook_prompt, role_message, update_ask, merge_forward), the merger's
// merge-queue message and its message that the project's merge holds changed
// (merge_holds_changed, which resumes a released merger as the merge-queue message does), and the
// three rows of a production check (check_wake, check_look, check_bound). Every other row runs
// while the project is paused: the stops (a supervise row of any other op, issue_suspend,
// issue_release), a supervise row that takes back a stopped phase's task (`retire_task`), the
// reads and writes of Dispatch (the gate, pending-basis and re-parent seeds among them), the
// reconcile of a tree's news of its approved design (design_changes), which reads Dispatch and
// writes the news record and tells a role only by a news-only start, which is a start this list
// holds, and the controller's notices (controller_notice), since the pause never holds the
// controller (spec Design item 8).
//
// The outbox's claim reads the list as SQL literals (store.go's outboxPauseFree, built from it),
// so DueRows, ClaimRow, HeldRows and PauseHeldRows hold exactly these rows, and the resume marks
// exactly these rows with the pause it ends (MarkPauseHeldRows).
// TestEveryOutboxKindIsHeldOrNotHeldByAPause names every kind the lanes run as held or not held,
// so a kind added to a lane fails it until somebody decides which it is.
var pauseHolds = []pauseHold{
	{kind: OutboxKindSupervise, op: "start"},
	{kind: OutboxKindNotice},
	{kind: OutboxKindPhaseEnded},
	{kind: OutboxKindHookPrompt},
	{kind: OutboxKindRoleMessage},
	{kind: OutboxKindUpdateAsk},
	{kind: OutboxKindMergeForward},
	{kind: OutboxKindMergeQueueRemoval},
	{kind: OutboxKindMergeHoldsChanged},
	{kind: OutboxKindCheckWake},
	{kind: OutboxKindCheckLook},
	{kind: OutboxKindCheckBound},
}

// PauseHeldRow is one row a project's pause holds, as the state lists it and the resume logs it
// (Postgres.PauseHeldRows, Postgres.MarkPauseHeldRows). Claim is the claim the row is for where
// the row names one, and empty where it names none: an architect's notice goes to the architect
// that owns its issue when it runs (the daemon's readNoticeRoute walks the tree for it), and a
// production check's rows name no role. WaitedSeconds counts from the moment the row came due
// (next_at), as api.HeldOutboxRow's do in the state.
type PauseHeldRow struct {
	OutboxRow
	Claim         claim.Token
	WaitedSeconds int64
}

// OwedStart is one pending worker start that waits for its issue's admission slot
// (SuperviseRequest.NeedsSlot) and that no live lease holds, as the waiting line orders it: the
// row's own creation time and its id. That pair is the line's ordering key because it is durable
// — the transition wrote the row in its own transaction, and a restart finds it exactly where it
// was — and because it is already the order the issue's work came back in, which is the order the
// line gives those issues their slots back (Waiting). The role, phase and generation the row
// serves travel with it so that the one read also answers admission's repair of a row that has
// outlived the fences the outbox would have run it under, and LeaseToken with them: a row whose
// lease has lapsed is still owed, and that repair must name the token the dead lane left to
// finish it (Store.FinishOutbox).
type OwedStart struct {
	Issue     string
	CreatedAt time.Time
	ID        int64
	// WriteMark is the number outbox_write_mark_seq handed the write that last put this payload
	// in the row: the insert that queued it, which takes one through the column's own default,
	// whether that insert is an issue's first start or the one written in place of the start it
	// already owed (Store.EnqueueStart). It is the row's side of the stop rule, which weighs it
	// against the mark a claim's retirement took (store's putClaim) to tell a start written
	// after the stop from one the stop outlived (admit's stopOutlivedStart). The created_at
	// cannot do that work: a start written in place of an owed one carries that row's
	// created_at, which is how the issue keeps its place in the line.
	WriteMark  int64
	Role       claim.Role
	Phase      phase.Phase
	Generation uint64
	LeaseToken string
}

// OutboxPayload is the sealed vocabulary of payloads a workflow may enqueue.
type OutboxPayload interface{ OutboxKind() OutboxKind }

// ClaimRelease releases the issue's Dispatch claim: the row's issue column is the whole of what
// it carries. The holder and the issue's record are read when the row runs, not when it is
// queued, so a re-admission, a second park or a person who claims the issue meanwhile each end
// the row on their own, with no generation or holder to fence it.
type ClaimRelease struct{}

func (ClaimRelease) OutboxKind() OutboxKind { return OutboxKindClaimRelease }

// MessagePost is a Dispatch message body. The runner posts it as Posted: the body, then its outbox
// marker. Artifact, when set, is a document the body links (dispatch://<issue>/artifact/<name>):
// the runner puts it on the issue before it posts the body, so the link never comes before its
// document. No message of this release sets Artifact: its one producer was the pod rescue's
// message, removed with the rescue (INFRA-2848). The field stays because the outbox decodes a
// payload strictly and refuses an unknown field, so a row an older daemon queued with an artifact
// must still decode and still put its document up. It, MessageArtifact and the outbox's upload
// can go once no daemon of a release before this one shares the database and no unsent
// dispatch_message row holds an artifact.
type MessagePost struct {
	Body     string           `json:"body"`
	Artifact *MessageArtifact `json:"artifact,omitempty"`
}

// MessageArtifact is the markdown document a message links. Name is the slug Dispatch files it
// under, so the message can link it before it exists: a name that is no slug is refused, since
// Dispatch would file it under another (its artifactSlug). Content is the markdown, at most
// MaxMessageArtifactBytes.
type MessageArtifact struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// MaxMessageArtifactBytes is the most bytes a message's artifact may hold: Dispatch's own bound on
// one markdown document (pmdoc.MaxDocumentBytes), past which it refuses the upload.
const MaxMessageArtifactBytes = 1 << 20

// artifactSlug is the grammar of a Dispatch slug, which every reader of a dispatch:// link spells.
var artifactSlug = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func (MessagePost) OutboxKind() OutboxKind { return OutboxKindDispatchMessage }

// The outbox marker closes every message the runner posts. It names the row, so a retried row
// finds the post it already made instead of posting twice.
const (
	messageSeparator   = "\n\n"
	messageMarkerOpen  = "<!-- legion-outbox:"
	messageMarkerClose = " -->"
)

// MessageMarker is the marker of outbox row id.
func MessageMarker(id int64) string {
	return messageMarkerOpen + strconv.FormatInt(id, 10) + messageMarkerClose
}

// outboxKeyPrefix begins the dedupe key the runner publishes a row under.
const outboxKeyPrefix = "legion-outbox:"

// OutboxKey is the dedupe key the runner publishes outbox row id under, so the listener and the
// plugin recognise a retried publish of the row as the same message.
func OutboxKey(id int64) string {
	return outboxKeyPrefix + strconv.FormatInt(id, 10)
}

// ParseOutboxKey is the row id OutboxKey names, and whether key is a key OutboxKey writes.
func ParseOutboxKey(key string) (int64, bool) {
	digits, found := strings.CutPrefix(key, outboxKeyPrefix)
	id, err := strconv.ParseInt(digits, 10, 64)
	return id, found && err == nil && id > 0 && OutboxKey(id) == key
}

// Posted is the message the runner posts for row id: the body, then the row's marker.
func (m MessagePost) Posted(id int64) string {
	return m.Body + messageSeparator + MessageMarker(id)
}

// LongestPost is the longest text the runner could ever post for body: the post Posted composes,
// for the largest row id a row can carry (outbox ids are a bigserial, so math.MaxInt64 is the
// longest marker there is). A caller that must know, before anything is recorded, whether
// Dispatch would take the post holds this text to Dispatch's bounds rather than the body alone
// (api's closeRoot and the merger's READY completion): the marker is a block of its own after a
// body that ends in a paragraph, and it is code inside a body that ends in an open fence, so a
// body that passes a bound can be a post that does not. It is built by Posted, the function the
// runner itself posts with, so the text checked and the text posted cannot be composed by two
// different rules.
func LongestPost(body string) string {
	return MessagePost{Body: body}.Posted(math.MaxInt64)
}

// NoticeKind identifies one persisted notice delivery.
type NoticeKind string

// Notice is a role-facing workflow observation. A phase-finished notice carries the finishing
// worker's own summary and, beside it, the verdict it gave (the tester's pass or fail). A
// review-stuck notice's summary names the fact that wrote it, and its topic is the reviewer's role
// topic, which the architect asks for the decision.
type Notice struct {
	Kind    NoticeKind  `json:"kind"`
	Role    claim.Role  `json:"role,omitempty"`
	Phase   phase.Phase `json:"phase,omitempty"`
	Summary string      `json:"summary,omitempty"`
	Verdict string      `json:"verdict,omitempty"`
	Version int         `json:"version,omitempty"`
	Reason  string      `json:"reason,omitempty"`
	Topic   string      `json:"topic,omitempty"`
	// Failure is why the claim failed, carried by the notices of a claim's failure and by no
	// other kind (validateOutboxPayload): the hold a phase worker's failed claim puts its issue
	// in and the worker-died that names the death (workflow's claimFailed). It is absent where
	// the daemon recorded no cause for the failure, and absent from the two held notices that
	// report no failure at all: the escalation's (workflow's retryOrEscalate) and the
	// re-admission's (workflow's HeldNotice).
	//
	// One nested record rather than seven fields beside Reason, because a count means nothing
	// apart from the limit it ran out against and the four counts do not have four limits:
	// deaths are measured against the launch failure limit, as launch failures are. So each
	// count travels inside the pair that names its own limit, where no reader has to pair them
	// up, and a notice of anything else costs one absent key rather than seven. Reason is left
	// to its one value `escalated`, since both agents' texts branch on whether it is there
	// (skill://legion-controller/references/wakes.md,
	// skill://legion-architect/references/wake-routing.md); the failure's own sentence is
	// Failure.Detail.
	Failure *NoticeFailure `json:"failure,omitempty"`
	// Escalation is the architect's own words for the escalation it made, carried by the held
	// notice of that escalation and by no other kind or reason (validateOutboxPayload): a
	// controller woken by an escalated hold acts on it, and the words that say why the
	// architect escalated are what it acts on. It is absent for an escalation whose
	// retry_or_escalate named no reason, which is every escalation until an architect names one.
	//
	// It is a field of its own rather than Reason, which keeps its one value `escalated`
	// because both agents' texts branch on whether Reason is there at all
	// (skill://legion-controller/references/wakes.md,
	// skill://legion-architect/references/wake-routing.md).
	Escalation string `json:"escalation,omitempty"`
	// StoppedBy is the claim of the architect whose stop_task stopped the task a task-stopped
	// notice reports (workflow's taskStopped), carried by no other kind (validateOutboxPayload),
	// and absent from the task-stopped notice of a task that no architect stopped (workflow's
	// stopUnreturned). The architect that made the call is answered by its own call, so the
	// daemon's notice executor publishes the notice only when the architect the issue's notices
	// reach is another claim (the daemon's deliverNotice). Published keeps it, so a re-held copy
	// is judged the same way.
	StoppedBy claim.Token `json:"stoppedBy,omitempty"`
	// Resends counts the times the notice was queued again after the listener could not forward
	// it to its architect's session (the daemon's rehold), which stops at a cap.
	Resends int `json:"resends,omitempty"`
	// PacedAt is the moment the daemon's wake pacing let out a catch-up it held to its turn: the
	// catch-up the outbox writes in place of a launch when a start row finds an architect already
	// running (the outbox's start, workflow's claimReady). Reading that catch-up is a wake of the
	// architect, and what the organization's per-minute cap counts is the context it reads, so the
	// row waits its turn in the pacing's second queue as a launch does (dispatch://INFRA-5682 R1).
	// It is CatchUpHeldAt while the row waits there, the let-out's moment once the pacer has let
	// the row out (ReleaseHeldCatchUp), and the zero value on every other notice.
	//
	// The moment is kept here rather than read back from the row's next_at because the two part
	// company: a failed attempt rewrites next_at to its backoff (RetryOutbox), while the turn the
	// pacing gave this wake never moves. The outbox writes that wake's one line from it, on
	// whichever attempt publishes (the daemon's wakeRan), and a ready that releases the
	// architect's waiting notices leaves the row where it stands only while the moment is still
	// ahead (the daemon's releaseWaiting), so a catch-up still held stays held, and one whose turn
	// has come is released like any other notice rather than waiting out a backoff the pacing
	// never asked for. It is the daemon's own bookkeeping, so the publish drops it (Published) as
	// it drops ResendOf; a re-held copy keeps it for that one line alone, its next_at being the
	// re-send delay (the daemon's noticeAnswered).
	PacedAt time.Time `json:"paced_at,omitzero"`
	// PacedFrom is the moment the engine wrote the row held, on the clock the let-out is read on,
	// and the two travel together: PacedAt less PacedFrom is how long the pacing held this wake,
	// which is what the line it writes reports (the daemon's wakeRan). The wait is read from this
	// pair rather than from PacedAt less the row's created_at, which is two clocks subtracted from
	// each other: created_at is the database's now() at the start of the fact's transaction
	// (store/migrations/0004_record.up.sql), while the row is held from the engine's clock later
	// in that same transaction (workflow's claimReady). A row the pacer let out at its join waited
	// no turn, and its let-out writes PacedAt as PacedFrom (ReleaseHeldCatchUp), so the commit
	// that came between the write and the join is not read as a wait.
	PacedFrom time.Time `json:"paced_from,omitzero"`
	// PacedTurns is how many turns the pacing let out between the row's join of its queue and its
	// own let-out (wakepace.Let.Turns), which the line its wake writes names beside the queue. The
	// let-out writes it with PacedAt (ReleaseHeldCatchUp), and it goes where PacedAt goes: dropped
	// at the publish, kept on a re-held copy.
	PacedTurns int `json:"paced_turns,omitempty"`
	// ResendOf is the outbox row a re-held copy copies. The copy is published under that row's
	// dedupe key (OutboxKey), so the listener's report of a failed forward names the original row,
	// whichever copy failed (exceptionNotice), and the listener's role lane drops a copy that
	// reaches the holder the original reached within its delivery caches
	// (packages/envoy/cmd/listener/delivery.go). The stream and the hosts treat the key as no dedupe
	// key (dedupeKeyNamesItsEvent, packages/contracts/src/envelope.ts). The row is the daemon's own
	// bookkeeping: the executor publishes the notice without it.
	ResendOf int64 `json:"resend_of,omitempty"`
	// CatchUp is a catch-up notice's account of the tree, and absent from every other kind.
	CatchUp *CatchUp `json:"catch_up,omitempty"`
	// OpenCapabilities names every open row of the deployment's capability report
	// (capabilities.Deployment.Open) when the daemon reports them: a deployment row with no
	// decision, or a live row a live session's check failed (LEGION-663). Only a tick controller
	// notice carries it (admit.Admission.ReportCapabilities), and a tick with no open row carries
	// none. The controller reads each row's detail in `legion state`, and on a deployment row alone
	// the legion.yaml line that records a decision; a gap is the operator's to close or decide, a
	// live row's the session's environment to fix, and neither is the controller's.
	OpenCapabilities []string `json:"openCapabilities,omitempty"`
	// Blocked is every issue Legion waits on a Dispatch issue nobody has finished for (INFRA-225),
	// one it has still to start or one it started that gave its admission slot back for the wait,
	// with what each still waits on. Only a tick controller
	// notice carries it, as OpenCapabilities does: the tick is the turn that posts the day's
	// report, so the controller names each there and takes nothing for it. A tick with nothing
	// blocked carries none.
	Blocked []BlockedIssue `json:"blocked,omitempty"`
	// Held is every issue of the project the daemon's own record holds in the workflow that
	// stands in neither the active set nor the waiting line, with the one word for why
	// (C3 of dispatch://INFRA-3107), in the shape the state carries them (api.Admission.Held,
	// an alias of this row). Only a tick controller notice carries it, as OpenCapabilities and
	// Blocked are: the tick is the turn that posts the day's report, so the controller reads
	// there what nothing starts beside what waits on a Dispatch issue. A tick with nothing held
	// carries none.
	Held []HeldIssue `json:"held,omitempty"`
	// Silent is every issue of the project whose phase worker is one nothing will prompt
	// (workflow.SilentPhaseWorker, dispatch://INFRA-3163), and every one whose worker's own
	// report of background work has stood past its bound, which the daemon never prompts
	// (workflow.BackgroundStalledWorker, SilentIssue.BackgroundSince), as admission derives the
	// list (admit.Admission.silentWorkers). Only a tick controller notice carries it, as Blocked
	// does: the tick is the turn that posts the day's report, and a tick with nothing silent
	// carries none. The architect that owns the issue is told of each of these by a `worker-silent`
	// or `worker-background-stalled` notice of its own, judged by the same rules, with the two
	// differences admit.Admission.silentWorkers sets out: a handler also refuses a fact whose run
	// is not the issue's current generation, a term the tick does not weigh, since it reads the
	// worker's claim by its token alone and that claim carries no serving run; and the sweep that
	// writes those notices runs once a minute, so a tick can name a worker up to a minute before
	// its architect is told. The controller is the second reader, because the notice can reach
	// nobody: a child's architect that does not hold its role is passed over for the one above
	// it, but a notice whose walk ends at a tree root whose architect claim failed, retired or is
	// gone ends undelivered. The controller holds `fresh_session` and reads the whole project.
	Silent []SilentIssue `json:"silent,omitempty"`
	// Actor is who made the spec decision a design-approved, design-changes-requested or
	// carried-gate-opened notice reports, or the decision a parked-decision notice relays: the
	// Dispatch actor as `<kind>:<id>`, a person `user:<login>`. Empty where no person decided:
	// gates.design off opened the gate. A basis notice (basisNoticeKinds) may carry the person
	// whose act on the ancestor's document it reports.
	Actor string `json:"actor,omitempty"`
	// Ref is that decision's Dispatch reference: the document of the design-gate step a design
	// decision is of (workflow's stepRef), `dispatch://<KEY>/spec` for the spec step,
	// `dispatch://<KEY>/ask/<id>` for an answer or a resolution, `dispatch://<KEY>/comment/<id>`
	// for a reply. A basis notice may carry the reference of the ancestor's document it is about.
	Ref string `json:"ref,omitempty"`
	// Decision is what a parked-decision notice relays, and Root the root of the tree it was made
	// on. Every other kind carries neither.
	Decision DecisionKind `json:"decision,omitempty"`
	Root     string       `json:"root,omitempty"`
	// Update is an update-stalled notice's account of the round that is not finishing, and is
	// absent from every other kind. Only an UpdateStalledNotice carries one.
	Update *UpdateStalled `json:"update,omitempty"`
	// Pause is a project-paused or project-resumed notice's account of the act, and is absent
	// from every other kind. Only those two kinds carry one.
	Pause *ProjectPause `json:"pause,omitempty"`
	// Volume is what a volume-near-full notice carries: one sweep of the issue pod's volume as
	// the cache trimmer in that pod's launcher reported it. Only that kind carries one.
	Volume *VolumeNearFull `json:"volume,omitempty"`
	// Count is how many messages the intake gave up that an intake-gave-up notice counts, and
	// GiveUps names the first of them, at most giveUpNoticeNames (Postgres.countIntakeGiveUp). Only
	// that kind carries either: the controller reads the rest in the state.
	Count   int            `json:"count,omitempty"`
	GiveUps []NoticeGiveUp `json:"giveUps,omitempty"`
}

// NoticeGiveUp names one message the intake gave up, in an intake-gave-up notice: its record's id
// in intake_failures, which a replay or a discard names, the consumer that delivered it and its
// subject.
type NoticeGiveUp struct {
	ID       int64  `json:"id"`
	Consumer string `json:"consumer"`
	Subject  string `json:"subject"`
}

// ProjectPause is what the controller is told when the operator pauses or resumes its project
// (decision 24 of dispatch://INFRA-3759): who acted, when, and why. A project-paused notice names
// the pause the act began and the reason it was given; a project-resumed notice names the pause
// the act ended, which is its why, and carries no reason of its own.
type ProjectPause struct {
	// ID is the pause: the one begun, or the one ended (store's Pause, its id).
	ID int64 `json:"id"`
	// By is who acted: who paused, or who resumed.
	By PauseActor `json:"by"`
	// At is when the act was done.
	At time.Time `json:"at"`
	// Reason is why the project was paused, on a project-paused notice alone.
	Reason string `json:"reason,omitempty"`
}

// PauseActor is who paused a project or ended its pause: the kind `controller` with the
// controller's session id, or the kind `operator` with no session. The store records the same two
// shapes (store.PauseActor, the checks of project_pauses).
type PauseActor struct {
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
}

// The two kinds of PauseActor.
const (
	PauseByController = "controller"
	PauseByOperator   = "operator"
)

// validate holds the account to its kind's fields: an id, an actor of one of the two shapes, a
// moment, and a reason with words in it on a project-paused notice and none on a
// project-resumed notice.
func (p ProjectPause) validate(kind NoticeKind) error {
	if p.ID <= 0 || p.At.IsZero() {
		return fmt.Errorf("a %s notice names its pause and when it was acted on", kind)
	}
	switch p.By.Kind {
	case PauseByController:
		if p.By.Session == "" {
			return fmt.Errorf("a %s notice by the controller names the controller's session", kind)
		}
	case PauseByOperator:
		if p.By.Session != "" {
			return fmt.Errorf("a %s notice by the operator names no session; it names %q", kind, p.By.Session)
		}
	default:
		return fmt.Errorf("a %s notice is by the controller or the operator, not %q", kind, p.By.Kind)
	}
	if kind == ProjectPausedNotice && strings.TrimSpace(p.Reason) == "" {
		return fmt.Errorf("a %s notice names the reason the project was paused", kind)
	}
	if kind != ProjectPausedNotice && p.Reason != "" {
		return fmt.Errorf("a %s notice carries no reason; only a project-paused notice does", kind)
	}
	return nil
}

// VolumeNearFull is one sweep of an issue pod's volume as the trimmer in its launcher process
// reported it (shimwire.LauncherVolumeReport), plus the pod it was of, which the report does not
// carry: the launcher is authenticated for one pod and the session names it. It is what a
// volume-near-full notice tells the issue's architect, and the one thing in Legion that sees the
// volume the roles of an issue share fill under them (INFRA-2009).
//
// It holds everything the report holds. Trimmer is the identifier the trimmer minted for itself
// when the launcher process started, which is what counts a restarted roles container apart from
// the first episode, Episode the trimmer's own episode number and RaisedBy what raised it
// (shimwire.VolumeEpisodeFirst, VolumeEpisodeRecovery, VolumeEpisodeRise). UsedBytes and
// FreeBytes are the volume as the sweep's last reading saw it and FreedBytes what the sweep
// freed, read from the volume and never counted from the bytes unlinked: the sweep's first
// reading's used bytes less its last's, negative where the volume's used bytes grew while the
// sweep ran. StopReason is why the sweep stopped, in the trimmer's own words, and RefusedReads
// the /proc reads the kernel refused that no report had carried yet, of this sweep and of earlier
// ones that sent no report: one per process the trimmer could not read, or one for /proc itself
// when /proc could not be listed.
//
// Sizes are the size walk's targets with their two age splits (VolumeSize), SizesAt when they
// were measured — a report made between walks carries the last walk's numbers with that walk's
// time — SizesPartial a walk its budget or the container's stop cut before it reached every
// target, which is the one way a walk ends early, and SizesMissing every target the walk did not
// measure at all, and the volume root or an immediate child of it where the walk did not list
// one in full, each with its reason where a read was refused (VolumeMissing): a directory it
// could not open or list, a file whose lstat was refused, and, of a walk that was cut, every
// target it had not measured when it was cut, as far as the report's cap and counted past it
// (SizesMissingOmitted) — where the cut came while it listed them, every one it had listed, the
// child of the volume root whose names it was reading, whole, and every child it had not yet
// listed, or the volume root alone where the cut came inside the root's own listing
// (shimwire.LauncherVolumeReport). A directory the walk could read only in part is a
// size that names the first read under it the walk did not make, one the kernel refused or a
// directory deeper than the walk descends (VolumeSize). Faults are the panics the trimmer
// recovered from, each with its stack, and SwitchedOff the report no sweep produces: the trimmer
// has stopped itself after three faults in a row, and the daemon sends that one whatever its
// bounds stand at, since the protection this notice exists for has stopped on exactly the pod
// that has been reporting.
//
// The four Omitted counts are how many entries the launcher's own bound left out of each list,
// which it cuts because a path two levels under /legion is a name a role of the pod chose and a
// pod may hold thousands of them (internal/launcher's bound, and its walk's own for the sizes,
// which keeps the largest): a reader of a notice is never shown a cut list as the whole of what
// the trimmer measured.
//
// It is a shape of this package rather than the wire's because it is a durable payload: an
// outbox row's JSON is read back by a daemon of a later release (the re-hold's decode, Notice's
// own), so the shape a row carries must be one this package owns and can keep.
type VolumeNearFull struct {
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
	SizesAt             time.Time       `json:"sizesAt"`
	SizesPartial        bool            `json:"sizesPartial,omitempty"`
	SizesMissing        []VolumeMissing `json:"sizesMissing,omitempty"`
	SizesMissingOmitted int             `json:"sizesMissingOmitted,omitempty"`
	Faults              []string        `json:"faults,omitempty"`
	FaultsOmitted       int             `json:"faultsOmitted,omitempty"`
	SwitchedOff         bool            `json:"switchedOff,omitempty"`
}

// VolumeSize is one target of the trimmer's size walk — a directory two levels under the pod's
// volume root, a regular file beside such directories in an immediate child of the root, or an
// immediate child of the root that holds no directory of its own — as the walk measured it: the
// bytes of the regular files under it, or of the file itself, and of those the bytes of the files
// whose modification time is more than an hour old and more than a day old, which are what tell
// a reader a working set from cold bytes. Refused names the first read under Path the walk did
// not make — one the kernel refused, or a directory deeper below Path than the walk descends,
// which it does not open — as Path/<name> for a name in it and Path/.../<name> (depth N) for a
// name N levels below it, and Refusals is how many such reads there were in all: a size that
// carries them is a lower bound of the directory's own, never the whole of it. A target the walk
// read nothing of is no size but one of SizesMissing (VolumeMissing, shimwire.LauncherVolumeSize).
type VolumeSize struct {
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	BytesOverHour int64  `json:"bytesOverHour"`
	BytesOverDay  int64  `json:"bytesOverDay"`
	Refused       string `json:"refused,omitempty"`
	Refusals      int    `json:"refusals,omitempty"`
}

// VolumeMissing is one target the trimmer's size walk measured none of, or the volume root or an
// immediate child of it that the walk did not list in full, and why where a read was refused: the
// read and the kernel's answer, as `openat: permission denied`, for a directory the walk could
// not open or list or a file whose lstat was refused, and nothing for one the cut of a walk left
// unmeasured, which VolumeNearFull.SizesPartial says (shimwire.LauncherVolumeMissing).
type VolumeMissing struct {
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}

// MarshalJSON keeps `sizes` an array on the wire, empty rather than `null`, exactly as
// UpdateStalled.MarshalJSON keeps `waiting` one and for the same reason: the plugin's strict
// reader refuses a null. The `wire` alias is what keeps the method from re-entering itself.
func (v VolumeNearFull) MarshalJSON() ([]byte, error) {
	type wire VolumeNearFull
	out := wire(v)
	if out.Sizes == nil {
		out.Sizes = []VolumeSize{}
	}
	return json.Marshal(out)
}

// NoticeFailure is why a claim failed, as the notices of that failure name it: Cause is the one
// word the site that failed the claim chose, Detail the sentence that site wrote beside the word
// (supervise.FailureCause and supervise.Claim.Failure), and the four counts the failure left,
// each beside the limit it is measured against. The cause is a string here, as UpdateWaiting's
// state is, so the record package stays below the supervisor it reports on; the two words are
// the ones an agent reads of the same failure over the API (api.ClaimFailureView), so one
// failure reads the same on both surfaces.
//
// Every count is named, zero or not: `deaths 0 of 3` is what tells the reader the deaths are not
// what ran out, which an absent key would leave it to guess.
type NoticeFailure struct {
	Cause  string `json:"cause"`
	Detail string `json:"detail,omitempty"`
	// The four counts of supervise.Budgets, each against the limit of supervise.Limits it runs
	// out at: LaunchFailures and Deaths both run out at the launch failure limit, and the
	// prompt counts at one of their own.
	LaunchFailures FailureBudget `json:"launchFailures"`
	Deaths         FailureBudget `json:"deaths"`
	PromptFailures FailureBudget `json:"promptFailures"`
	PromptRetires  FailureBudget `json:"promptRetires"`
	// Death is why the claim's process was last found gone and StartFailure why its last start
	// produced no running agent, each as the claim recorded it and each absent where it recorded
	// none (dispatch://INFRA-714 R6). They are two answers to two questions and are never merged
	// into one sentence: a claim that cannot start and a claim that keeps dying are different
	// faults, and an agent deciding between a retry and an escalation reads them apart. Cause
	// above says which of them ended this claim; these say what the runtime and the launcher
	// last said, which no other surface tells the architect or the controller.
	Death        string `json:"death,omitempty"`
	StartFailure string `json:"startFailure,omitempty"`
}

// FailureBudget is one of a failed claim's counts beside the limit it is measured against, both
// as the failure left them.
type FailureBudget struct {
	Count int `json:"count"`
	Limit int `json:"limit"`
}

// UpdateStalled is what an update-stalled controller notice carries: the update round the daemon
// opened over an issue's pod, and why something holds it past its bound (the daemon's
// tellUpdateStalled). Round is the round's identity (supervise.Claim.UpdateRound), Pod the pod it
// was opened over, and Reasons the drift the round was opened for, as the ask states it. Waiting is
// every claim of the issue with a session that has not failed (the daemon's noticeClaims), each
// with its role, its state and whether it answered. A claim the daemon suspended is listed too,
// although the round neither asks it nor waits for it, unless a project's pause stopped its agent
// and still owes that role a launch (internal/daemon's podHolds.owes). So the controller finds the
// claims in the round, and which of them are suspended, without reading the whole claims list;
// the notice's Reason names who holds each held agent and who may resume it. What each entry says
// is UpdateWaiting's. The notice's issue is the outbox row's own.
//
// Reasons is one string, never a list: a single reason carries ", " inside it already — a moved
// address reads `<where> <held>, now <handed>`, a pod-shape drift `image <was>, now <now>`, and a
// plugin round's reason, which only an earlier daemon's round carries, names both contracts — so
// a list built by splitting what the claim
// records (supervise.Claim.UpdateReasons, one string) would cut each reason into pieces that read
// as reasons of their own. It is the drift exactly as the round's ask states it to each role.
type UpdateStalled struct {
	Round   string          `json:"round"`
	Pod     string          `json:"pod,omitempty"`
	Reasons string          `json:"reasons"`
	Waiting []UpdateWaiting `json:"waiting"`
}

// UpdateWaiting is one claim the notice lists (UpdateStalled.Waiting): its token, the role it
// holds, the state it is in (supervise.ClaimState — the agent of a claim under a suspension that
// holds waits for a resume by whoever may lift that suspension, which the notice's Reason names, and the round
// waits for no other suspended claim but one whose agent a project's pause stopped while the pause still owes
// that role a launch, internal/daemon's podHolds.owes; a pod round lists no `failed` claim, whose
// machine records no round), and Answered for a claim that has already answered this round.
//
// The contract the claim's plugin records is not here. A plugin with no ready_for_update op is
// asked like any other, on its role's own topic (UpdateAsk), and the round ends at its bound
// whether or not it answers, so such a claim is one the round is waiting on like any other, and
// the reader of the notice has nothing of its own to do about it.
//
// Answered tells the reader what each role said. It is no list of roles to prompt: the round
// ends at its bound whether or not a role answers, and the notice is told only for what holds the
// round past that bound.
type UpdateWaiting struct {
	Claim    string     `json:"claim"`
	Role     claim.Role `json:"role,omitempty"`
	State    string     `json:"state"`
	Answered bool       `json:"answered,omitempty"`
}

// MarshalJSON keeps `waiting` an array on the wire, empty rather than `null`: the plugin's strict
// reader refuses a null.
func (u UpdateStalled) MarshalJSON() ([]byte, error) {
	type wire UpdateStalled
	out := wire(u)
	if out.Waiting == nil {
		out.Waiting = []UpdateWaiting{}
	}
	return json.Marshal(out)
}

// BlockedIssue is one issue Legion waits on another issue for and the issues it waits on that are
// not done, in the order Dispatch lists them (blockers.Unfinished): a root admission skips, a
// child the workflow holds at `admitted`, or a started issue that gave its admission slot back
// for the wait. It lives here, below both readers, so the tick notice
// above and the state's `blocked` (api.BlockedIssue, an alias of this type) carry one shape
// rather than two that could drift.
type BlockedIssue struct {
	Issue     string   `json:"issue"`
	BlockedBy []string `json:"blockedBy"`
}

// MarshalJSON keeps `blockedBy` an array on the wire, empty rather than `null`: the plugin's
// reader refuses a null.
func (b BlockedIssue) MarshalJSON() ([]byte, error) {
	type wire BlockedIssue
	out := wire(b)
	if out.BlockedBy == nil {
		out.BlockedBy = []string{}
	}
	return json.Marshal(out)
}

// SilentIssue is one issue whose phase worker nothing will prompt, as the tick notice above
// carries it: the issue, the role whose claim is silent, the moment that claim's agent last
// stepped (supervise.StepRecord.LastStep), and how long it has been quiet. For a plain silence
// the last step is never the zero time, which workflow.SilentPhaseWorker refuses to judge from.
// It lives here beside BlockedIssue, below its reader, for the same reason that one does: one
// shape of the fact, named where every reader of the notice can see it.
//
// BackgroundSince is set on an entry of the other kind alone: a worker whose own session's
// report of background work has stood past `worker_background_hours`
// (workflow.BackgroundStalledWorker), which the daemon tells of and never prompts. It is the
// moment of that report (RoleClaim.ActivityAt), and absent from a plain silence's entry.
//
// Silent is text rather than a number of seconds or a time.Duration, and it is
// time.Duration.String's own text rounded to the minute ("2h30m0s"): the controller reads this
// list in the day's report, where a reader needs the length at a glance, and the `worker-silent`
// notice the issue's architect was sent states the same silence the same way (workflow's
// workerSilentRecord). The minute is the smallest unit any of this is measured in — the sweep
// that finds a silence runs every minute, and the bound is `worker_silence_minutes` — so no
// reader is owed the seconds.
type SilentIssue struct {
	Issue           string     `json:"issue"`
	Role            claim.Role `json:"role"`
	LastStep        time.Time  `json:"lastStep"`
	Silent          string     `json:"silent"`
	BackgroundSince time.Time  `json:"backgroundSince,omitzero"`
}

// CatchUp is what an architect is told of the issues it owns when its claim is ready at a launch
// (a catch-up notice). It is a child's own architect's first instruction, since a child's first
// phase start launches that architect with no task; a tree's root architect is launched with the
// task its record derives (workflow.ArchitectStartTask), or with none where it derives none, and
// is told its tree here beside it. It is those issues as the daemon records them then: the
// generation of the architect's own issue, the launch of the architect's claim it was written
// for, whether a hand-over created the root at this generation with its child's phase
// and pull request (HandedOver) and whether its old root's gate came with them (Carried), the
// design gate step by step (Gates), and every issue the architect owns — a root's whole tree, a
// child's own issue and every issue under it. A child registers no gate and no hand-over creates
// one, so a child's catch-up states no step and is neither handed over nor carried. A handed-over
// root's workers already run their phases, so its architect decomposes nothing anew; one that
// carried no gate registers its own spec. Decided names what a person decided on the tree while
// nobody in it could hear it and the start did not name (a decision made while the architect's
// claim had ended, before this relaunch), in the words of workflow.Engine.ParkedDecisionTask;
// empty when there is none, and on every child's catch-up, since what is kept for a tree is its
// root architect's to hear. Kept is what Decided named, which a published catch-up forgets
// (Store.ForgetKeptDecisions): a catch-up a newer one supersedes hands it over to a fresh catch-up
// instead (workflow.HandOverKept), and one that finishes undelivered any other way leaves it all
// for the architect's next ready to name again.
type CatchUp struct {
	Generation uint64         `json:"generation"`
	Launch     uint64         `json:"launch"`
	HandedOver bool           `json:"handed_over,omitempty"`
	Carried    bool           `json:"carried,omitempty"`
	Gates      []CatchUpGate  `json:"gates"`
	Issues     []CatchUpIssue `json:"issues"`
	Decided    string         `json:"decided,omitempty"`
	Kept       *KeptDecisions `json:"kept,omitempty"`
}

// CatchUpGate is one design-gate step as a catch-up states it: the step, whether a person must
// approve its document (Gated) and, once the architect has registered it, the document, its
// latest version, the version a person approved, if any, and whether the step is open.
// Unregistered says the root holds no record of the step at all, which is the step its architect
// has still to register.
//
// A step can also stand open on an ancestor's approval, or wait, closed, on an ancestor's open
// approval request (dispatch://INFRA-3320, R1 and R2): Basis names that approval or that request,
// and Pending says the step waits on the request. A step open on a basis reads Open with no
// Approved, since no person approved the version the root registered, so a relaunched architect
// reads the step as open and registered on its ancestor's approval rather than as one a person
// approved, or as one it must ask a person to approve. A pending step reads Open false, Pending,
// and the version a person approved of the root's own document, if any. A step whose basis has
// ended carries neither, and reads as registered on the root's own document.
//
// A catch-up carries one entry for each step the root must hold open before its tree works
// (TreeGate.Required: the steps the tree is gated on, or spec alone where none), so a deployment
// that gates nothing carries the one spec entry with Gated false: that registration opens the
// step at once and asks no person, which is what the policy word `off` said. The list replaced
// that word, because one word cannot name a list and each entry now says its own step's part of
// it. A criteria row the architect registered beside a deployment that gates spec alone of the two
// is an entry with Gated true, since that row holds the tree as the spec's does.
type CatchUpGate struct {
	Step         DesignStep    `json:"step"`
	Gated        bool          `json:"gated"`
	Unregistered bool          `json:"unregistered,omitempty"`
	Artifact     string        `json:"artifact,omitempty"`
	Version      int           `json:"version,omitempty"`
	Approved     int           `json:"approved,omitempty"`
	Open         bool          `json:"open"`
	Pending      bool          `json:"pending,omitempty"`
	Basis        *CatchUpBasis `json:"basis,omitempty"`
}

// CatchUpBasis is the ancestor's approval a step stands open on, or the ancestor's open approval
// request it waits on, as a catch-up names it (GateBasis): the ancestor, its document and
// the version a person approved or is asked to approve; for an approval, who gave it and
// Dispatch's time of it; for a request, its ask id.
type CatchUpBasis struct {
	Issue      string    `json:"issue"`
	Artifact   string    `json:"artifact"`
	Version    int       `json:"version"`
	Approver   string    `json:"approver,omitempty"`
	ApprovedAt time.Time `json:"approved_at,omitzero"`
	Ask        string    `json:"ask,omitempty"`
}

// CatchUpBasisOf is basis as a catch-up names it.
func CatchUpBasisOf(basis GateBasis) *CatchUpBasis {
	stated := &CatchUpBasis{Issue: basis.Issue, Artifact: basis.ArtifactID, Version: basis.Version,
		Approver: basis.Approver, ApprovedAt: basis.Approval.At}
	if basis.Pending != nil {
		stated.Ask = basis.Pending.Ask
	}
	return stated
}

// CatchUpIssue is one issue of the tree as a catch-up lists it.
type CatchUpIssue struct {
	Key    string      `json:"key"`
	Parent string      `json:"parent,omitempty"`
	Title  string      `json:"title"`
	Phase  phase.Phase `json:"phase"`
	Status string      `json:"status"`
}

func (Notice) OutboxKind() OutboxKind { return OutboxKindNotice }

// Published is the notice the runner publishes for outbox row id, and the dedupe key it goes under:
// a re-held copy goes under the key of the row it copies, and without ResendOf. The reserved
// moment and the moment it was reserved from go out with neither: all three are the daemon's own
// bookkeeping of the row, and what the pacing did says nothing to the architect that reads the
// notice (the daemon's wakeRan writes it to the log instead). A re-held copy carries only what
// the publish carried: the daemon's exceptionNotice decodes the listener's report into a notice
// and puts nothing back, and only the receipt-timeout path restores the paced moments and turns
// by hand (the daemon's noticeAnswered). Both then validate the copy before queueing it
// (reholdNotice, through NewOutboxRow), so a field validateOutboxPayload requires for its kind,
// as it requires CatchUp of a catch-up notice, is stripped here only together with a restore of
// its own there, or every re-hold of that kind is refused for want of it
// (record's TestAPublishedNoticeIsStillAValidOutboxPayload).
func (n Notice) Published(id int64) (Notice, string) {
	n.PacedAt, n.PacedFrom, n.PacedTurns = time.Time{}, time.Time{}, 0
	if n.ResendOf == 0 {
		return n, OutboxKey(id)
	}
	copied := n.ResendOf
	n.ResendOf = 0
	return n, OutboxKey(copied)
}

// CatchUpHeldAt is the moment a catch-up the wake pacing holds is written at, in its row's
// next_at and in its notice's PacedAt: a moment no clock of this daemon reaches, which is the row
// having no due moment until the pacer lets it out (ReleaseHeldCatchUp). A place in the pacing's
// queue has no moment of its own until then (dispatch://INFRA-5682 R1), and the row has to stand
// in the outbox meanwhile, so that a restart finds it and joins it to the queue again (the
// daemon's catchUpPacing). The outbox runs a row only once it is due, so no lane takes a held row
// up, and a reader of a notice's due moment that meets this one (CatchUpIsHeld) reads the pacer's
// estimate for the row's place instead (the daemon's pacedCatchUps and deliverNotice).
var CatchUpHeldAt = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

// CatchUpIsHeld is whether at, a row's next_at or a notice's PacedAt, is CatchUpHeldAt: the
// moment of a catch-up the wake pacing still holds in its queue, which is no due moment. The
// daemon's earlierNoticeFor, catchUpPacing.restore and pacedCatchUps, where it names a row's moment
// in the state's wakesWaiting, test the moment with it, so none of them takes it for a deadline.
// Two statements of the store compare next_at with CatchUpHeldAt itself (ReleaseHeldCatchUp,
// CatchUpHeld), and the other readers rely on the held moment sorting after any now: the outbox
// runs a row only once its next_at has come, and the daemon's releaseWaiting and pacedCatchUps
// tell a held notice by a PacedAt still ahead of now.
func CatchUpIsHeld(at time.Time) bool {
	return at.Equal(CatchUpHeldAt)
}

// ControllerNotice is a Notice for the project's controller topic (notify.ControllerTopic) alone,
// in an outbox row of its own, so its publish retries apart from the architect's notice row. It is
// published as the Notice it is.
type ControllerNotice Notice

func (ControllerNotice) OutboxKind() OutboxKind { return OutboxKindControllerNotice }

// The controller notices, each of a kind this comment names and each in a row of its own
// (#4065). A triage notice's root is unrecorded, so no architect owns it; a slot-free notice
// names the issue whose slot admission released while no waiting issue took it. A todo notice
// names an issue not handed to Legion that changed in `todo` while a slot stood free. On either
// of the two the controller runs its prioritization pass and its deployment's intake. A tick is
// the daemon's periodic wake, whose issue is the project key. An update-stalled notice names an
// issue whose update round has passed its bound (`pod_update_grace_minutes` after it opened)
// while something still holds it: a holding suspension of a role the round asks, a pod
// replacement the runtime refused (ReplacePod, or ReplaceIssuePod for a pod with no role process
// in it), no claim recording a process in the round's pod (or in any pod), claims recording two
// pods, or a pod the runtime cannot read. No process in the round's pod holds the round past its
// bound only where a suspension that holds (an operator's or the controller's) holds a role of
// it, where the runtime cannot say which pod the issue's Sandbox holds, or where the replacement
// by issue is refused or not offered (runtime.IssuePodReplacer): otherwise the daemon replaces
// that pod by issue at the bound. It is told once per round (the daemon's reconcilePodUpdates).
// A provider-errors notice names an issue whose role has ended six turns in a row in a provider
// error: its Role is that role, and its Reason the model and the HTTP status the provider
// answered, where it answered one, in words built from structured fields alone (supervise's
// tellOfProviderErrors). It is told once for each run of such turns (supervise.Claim's
// ProviderErrorRun says what ends a run); nothing is held and nothing is bounded, because a
// chain's bottom refusing deterministically costs one request every ten minutes. It is the one
// kind of these that an architect is told as well, in a notice row of its own (validNoticeKinds;
// the daemon's providerErrorRunReached says which architect, and for which run none). A
// tree-stalled notice names a root of the daemon's own whose tree has stopped: nothing of the tree
// has moved for longer than `stalled_tree_notice_hours`, no claim of it is at work, and it does
// not wait by design (admit's tellStalledTrees says which trees are told). Its issue is that root,
// and its Reason names the tree's quiet hours, each issue the workflow still runs with its phase,
// the role whose turn it is and the state of its pull request, and what the controller is to
// decide. It is told once for each stall: the daemon marks the root with the movement the notice
// was told for, and tells again only once the tree moves past that mark. When admission sends
// each is its rule (admit.Admission.wakeController and its callers,
// admit.Admission.TellController).
//
// A parked-decision notice names a person's decision on a tree Legion is not running, which
// nobody inside the tree will hear: a spec decision the notice executor could not deliver to the
// tree's architect (it stopped with its finished tree, or its claim ended), or a person's answer,
// resolution or reply (one that leaves the turn with the agent, or one under a closed ask) on an ask
// of an issue whose tree lingers or has closed. Its issue is where the decision was made, Root the
// tree's root, Decision what the person did, Ref where in Dispatch, Actor who; a spec decision
// carries its Version, a changes request its Reason. The controller decides whether the tree runs
// again (skill://legion-controller/references/parked-decisions.md).
//
// The daemon tells the controller no notice its skill says to do nothing on (#4065).
// So a root that waits on a person (workflow's enterAwaiting) and a root admission took out
// of the workflow (admit's releaseUnslotted) are told to nobody: the controller does nothing for
// the first, and reads the second from the state, a root at `phase: done` whose status is one
// only a running tree has. Their two kinds, `awaiting-person` and `unslotted`, went with their
// notices, and the migration 20261115121838_drop_controller_wakes_for_nothing deletes the rows an
// earlier release left queued.
//
// An architect-stalled notice names a root whose own architect has stalled, in one of the three
// readings ArchitectStallArm names below. Its Role is the architect, and its
// Reason names the arm, the root and what the record says. One root raises one notice per arm,
// per run of its tree (Store.TellArchitectsStalled, admit's architectStalls), so a root that
// two arms read raises two and neither hides the other. Nobody inside the tree is told: the
// architect is the role that would be, and each arm is a reading that it did not take, cannot be
// reached, or has not moved (dispatch://INFRA-2614).
//
// A memory-guard-kill notice names one of the controller's own programs that the memory guard of
// the controller's pod ended with SIGKILL because the pod was near its memory limit. Its issue is
// the project key, as a tick's is, its Role is the controller, and its Reason names the process,
// its pid, its size, the pod's reading and the limit, with none of the process's arguments, and
// says what to run instead (workflow's controllerMemoryGuardKill). Each kill is a row of its own,
// written by the workflow in the kill's own fact and never through admit.Admission.wakeController,
// whose rule keeps one unsent notice per kind: two kills are two programs ended, and the second
// must not be dropped behind the first.
//
// A project-paused notice tells the controller that the operator paused its project, and a
// project-resumed notice that the operator resumed it (decision 24 of dispatch://INFRA-3759). Its
// issue is the project key, and its Pause names who acted, when, and why: the pause's reason, or
// the pause the resume ended (ProjectPause). Each is queued in the pause's or the resume's own
// transaction (store's Pause and Resume); the controller's own pause or resume tells it nothing.
//
// An intake-gave-up notice counts the messages the intake gave up while it waited for its
// publish, and names the first giveUpNoticeNames of them by their records' ids (Count, GiveUps).
// It carries no role, and its issue is the project key, as a tick's is. The give-up writes it,
// and only while a controller is registered (Postgres.GiveUpIntakeMessage, countIntakeGiveUp):
// the first give-up of a burst opens it, due giveUpNoticeDelay later, each later give-up joins it
// while no publish has taken it, and one that finds it leased or given back to retry opens the
// next. Nobody inside a tree is told: the message is the intake's, of no issue an architect runs,
// and the controller replays or discards it (dispatch://INFRA-1575).
const (
	TriageNotice           NoticeKind = "triage"
	SlotFreeNotice         NoticeKind = "slot-free"
	TodoNotice             NoticeKind = "todo"
	TickNotice             NoticeKind = "tick"
	ParkedDecisionNotice   NoticeKind = "parked-decision"
	UpdateStalledNotice    NoticeKind = "update-stalled"
	ProviderErrorsNotice   NoticeKind = "provider-errors"
	TreeStalledNotice      NoticeKind = "tree-stalled"
	ArchitectStalledNotice NoticeKind = "architect-stalled"
	MemoryGuardKillNotice  NoticeKind = "memory-guard-kill"
	ProjectPausedNotice    NoticeKind = "project-paused"
	ProjectResumedNotice   NoticeKind = "project-resumed"
	IntakeGaveUpNotice     NoticeKind = "intake-gave-up"
)

// ArchitectStallArm names which reading raised an architect-stalled notice. It is part of the
// mark that keeps one wake to one root, one run and one arm (ArchitectStall,
// Store.TellArchitectsStalled), and the first words of the notice's reason, so a controller that
// reads two wakes of one root reads two different facts:
//
//   - ArchitectTookNothing: the task of the entry the architect's phases row names stands queued
//     to its claim, no turn of that claim confirmed it, and more than
//     workflow.StrandedReleaseWake has passed since the later of the first queueing of that entry,
//     which the claim's handshake keeps (supervise.Claim.Handshake) however many times the daemon
//     queued that entry's task again, and the last ready of the claim's agent. The task is counted
//     only while the claim is ready, working or idle, so a launch the wake pacing holds back counts
//     nothing; a task no let-go took back, held by a claim that is suspended, failed or retired, is
//     counted from that first queueing. The daemon writes no second start of that entry while
//     its task stands (workflow.OwedEntry), so the wake is the one thing left. A task the root's
//     own record has superseded — one that carries an entry, of a run before its generation or of
//     an entry the row has moved past — is no such task: the sweep writes the start of the row's
//     entry and that start's task takes its place (workflow.ArchitectStartPending,
//     supervise.Delivery.SupersededBy). Nor is a task of no entry, such as a wake's: no start takes
//     its place, and the sweep holds the entry's start back until a turn takes it.
//   - ArchitectClaimUnreachable: the architect's claim is failed or retired, or the root has no
//     architect claim at all and the work of its entry has stood that same bound, and in either
//     case no start of the root's run is the daemon's next move for it, which
//     workflow.ArchitectRelaunchOf decides. A root waiting on a person is read as any root is,
//     since the daemon queues the next start of its architect only when that wait is answered.
//   - ArchitectSlowBeforeGate: the root stands at phase.Admitted with no design-gate step
//     registered 90 minutes after it was admitted, or after its architect's entry was last
//     raised, whichever is later, counted from no earlier than the moment the wake pacing let
//     out the launch of the architect that the latest pause held, and, once a turn of the
//     architect has confirmed a task of that entry, than the moment it first did. While no turn
//     has confirmed one, the pacing's queue counts against the bound, and a healthy architect
//     confirms its entry's task long before the bound.
type ArchitectStallArm string

const (
	ArchitectTookNothing      ArchitectStallArm = "took-nothing"
	ArchitectClaimUnreachable ArchitectStallArm = "claim-unreachable"
	ArchitectSlowBeforeGate   ArchitectStallArm = "slow-before-gate"
)

// Words is the arm in the words an architect-stalled notice's reason begins with, before what
// the record says (admit's architectStalls). The words and the mark's arm are written from
// one value, so the wake a controller reads and the row that keeps it to one cannot name two
// different readings. An arm nothing here names reads as its own value, which no caller writes.
func (a ArchitectStallArm) Words() string {
	switch a {
	case ArchitectTookNothing:
		return "the architect took nothing"
	case ArchitectClaimUnreachable:
		return "the architect's claim cannot be reached"
	case ArchitectSlowBeforeGate:
		return "the root is slow before its gate"
	}
	return string(a)
}

// ArchitectStall is one arm an admission pass read over one root's own architect: the root, the
// tree it stands in, the generation of that tree's run, the arm, and the moment the pass read it.
// The first four are the key of the mark that keeps one wake to one root, one run and one arm
// (Store.TellArchitectsStalled), and At is kept on the mark for a reader of the table alone.
type ArchitectStall struct {
	Issue      string
	Tree       string
	Generation uint64
	Arm        ArchitectStallArm
	At         time.Time
}

// NoticePerClaim is whether one unpublished controller notice of kind stands per claim — the
// issue and the role that names it — rather than one per project
// (admit.Admission.wakeController reads it before it queues a wake).
//
// Two kinds answer true. A provider-errors notice names the claim whose turns keep ending in a
// provider error, and in an outage every claim of the project has a notice of its own to send.
// It is told at exactly the sixth turn in a row and once for each run (supervise.Claim's
// ProviderErrorRun says what ends a run), so a second claim's notice dropped behind the first
// claim's would never be sent at all. Two claims of one issue are two such claims — an issue's
// architect and its worker fail their turns in the same outage — so the issue alone is not the
// key; the role is the rest of it.
//
// A tree-stalled notice names one root whose tree has stopped, and carries no role: the stall
// is the whole tree's and not one agent's. So its key is the issue alone, which is what an
// empty role gives it — the role is a term the pending read leaves out when the caller names
// none (Store.ControllerNoticePending) — and the issue is the tree's root. It is keyed that way
// because a project can have two trees stopped at once, which is the very shape its controller
// most needs to read: keyed per project, the second tree's notice would be dropped for as long
// as the first tree's waited in the outbox, and no pass could tell the controller of it until
// that first notice was published.
//
// Every other kind that wakeController queues says something about the project — a free slot, a
// todo issue, the day's tick — which one wake carries however many facts find it pending. A
// memory-guard-kill notice never passes through that rule: the workflow writes one row for each
// kill.
func NoticePerClaim(kind NoticeKind) bool {
	return kind == ProviderErrorsNotice || kind == TreeStalledNotice
}

// SuperviseOp identifies a worker-session operation: "start" starts, resumes, or retries the role's
// claim; "suspend" stops its process and keeps its session; "release" stops its process and keeps
// its session too, as one role stop of an idle pod's release, and is its own op rather than a
// second "suspend" because the open-issue invariant reads the op to say which act it is reading —
// a suspend stops a role of an issue the workflow is taking out, a release stops a role of an
// issue that stays in the workflow and comes back at the next wake; "step_stop" stops the process
// of the step a stopped task ran and keeps its session, as a suspend does and from the same source
// (the daemon), and is its own op for the same reason a release is: the stopped task's issue stays
// in the workflow, held, until a later version of its plan starts the task again (workflow's
// stopTask); "tree_close" is the tree's
// close — the request that ends the claim, the tree's root claim included; "issue_close" is a
// child issue's close as done, which ends the claim as a tree close does and keeps its session, so
// the issue's next run, should a person set it todo again, resumes each claim's session rather
// than starting fresh (supervise's RequestIssueClose); "retire_task" takes back the task the
// role's claim holds for a phase the workflow stopped under its worker (Phase, at the generation
// and no later than Entry), and stops nothing: the worker keeps its process, its turn and its
// session (workflow's StopForChildren, supervise.Machine.RetireStoppedTask); "pause_stop" is a
// project pause's stop of one live role (store's Pause), which stops its process and keeps its
// session, phase and task as an idle release's stop does, and is its own op because it is bound to
// its pause (Pause below) and does nothing once that pause has ended.
// There is no plain stop: only a close ends a claim from the workflow, so the op states which.
type SuperviseOp string

// SuperviseRequest describes the session operation the outbox runner performs.
type SuperviseRequest struct {
	Op   SuperviseOp `json:"op"`
	Tree string      `json:"tree"`
	Role claim.Role  `json:"role"`
	Task string      `json:"task,omitempty"`
	// Kept is what an architect start's Task named of the decisions its root keeps
	// (workflow.Engine.ParkedDecisionTask). The start settles them once it has run (the outbox's
	// settleKept): a task the root architect's claim queued forgets them
	// (Store.ForgetKeptDecisions), and a start that reached no claim hands them over to a fresh
	// catch-up (workflow.HandOverKept). Every other request carries none.
	Kept *KeptDecisions `json:"kept,omitempty"`
	// Generation is the issue generation the request serves. A request of an earlier generation
	// finishes without acting: it must never suspend, stop, or start the next generation's worker.
	// Three requests are not held to it. A production check's look start (CheckLook) is fenced on
	// its check instead (the daemon outbox's checkLookHolds). A tree close of the issue's own tree
	// whose cleanup is reserved still retires its claim and drives that cleanup, however stale its
	// generation (reservedTreeClose). And a suspend or a tree close of a tree the issue has since
	// left (Tree is not the issue's tree) still stops a claim that stands in that tree, which no
	// worker of the issue's new tree is until that tree's start re-points it (stopInLeftTree).
	Generation uint64 `json:"generation"`
	// Phase is the phase a phase worker's start serves; a start for a phase the issue has left
	// finishes without acting. It is empty for a root's architect, which serves every phase, for a
	// child's own architect's plain start (workflow's childArchitectStart), and for a suspend or a
	// tree's close, which must still act after the issue moves on. The start a re-admission writes
	// to bring back a mid-phase child's own architect names the phase the child stands in (admit's
	// startMidPhaseMembers), and finishes without acting once the child has left it.
	Phase phase.Phase `json:"phase,omitempty"`
	// Entry is the phase entry this start's task is of, as the transition raised it on the role's
	// phases row in its own transaction (record.PhaseRow.Entry). It travels to the claim's
	// delivery and, once a confirmed delivery is retired, onto the claim, so a completion can be
	// held to the entry the turn reporting it was given (api/handoff.go) rather than to the run
	// alone, whose value every phase entry of one run shares. Three starts raise the entry they
	// carry: a transition's start of the phase the issue has moved into, the promotion's start of
	// a mid-phase worker a re-admitted tree brings back (workflow's start and
	// StartPromotedWorker), and a root architect's start that hands it work it was not on —
	// admission's promotion of its root and the sweep's raise for an architect whose row names no
	// entry, where the derivation serves it a task (workflow.Engine.StartArchitect), and the
	// resume of a root a person answered (workflow.Engine.ResumeAwaitedRoot). The
	// architect's start names no phase (Phase above), so the outbox holds it to the generation
	// and the claim's given entry alone. Three more carry the entry the role's row already names
	// without raising it: the send of an owed entry (workflow's SendOwedEntry), the sweep's start
	// of a root architect behind its entry (workflow's SendArchitectStart) and the held phase's
	// retry, which marks it with ResendEntry below. It is zero for every other
	// operation, and for every start that hands no entry's work at all. Those starts, in full, so
	// that no other comment need list them: the promotion's architect start of a root the
	// derivation serves no task (admit's promotion), a child's own architect's plain start
	// (workflow's gateOpened, which a child's first phase start runs, and reviveArchitect, which
	// each phase start of a child runs), the roles admission brings back beside a re-admitted
	// member's phase worker (admit's startMidPhaseMembers), the boot's resume by rule
	// (workflow's booted), the boot's one-time start of a merger the `awaiting_merge` cutover left
	// released (workflow's cutoverMergerRepair), the phase's resume task a ready worker level with
	// the entry its phases row names is given (workflow's readyWorkerTask), the silence repair's
	// resume task to a silent worker level with that entry (workflow's workerSilent), a production
	// check's look (workflow's startLook), the pod wake (workflow's wakeRow), the resume of a
	// cancelled idle release (the daemon's outbox) and a start queued for news of the approved
	// design alone (News below, the daemon's design_changes reconcile). Each hands its role work
	// it was already on, or work no phase entry numbers at all.
	Entry int64 `json:"entry,omitempty"`
	// News marks a start queued for news of the approved design alone: the reconcile of a tree's
	// news (the daemon's design_changes executor) queues it as a row of its own (Store.Enqueue),
	// never through workflow.StartOrOwe, which would take the issue's owed phase start out in its
	// place, and it waits for no admission slot.
	// It carries no task and no entry of its own: its executor reads, when it runs, what the record
	// owes the claim it reaches and hands that as the task, and finishes without acting where the
	// record owes it nothing then. A holder's carries the phase of the member it tells (Phase), so
	// it finishes once that member has left the phase; an architect's carries none, as every
	// architect start, and is queued on the architect's own issue: the tree root's for its
	// summary, and the owning architect's for the holder form of a member in accepting.
	News bool `json:"news,omitempty"`
	// ResendEntry marks a start that hands the role the work of the entry its phases row already
	// names rather than a new entry, so the claim's given entry does not decline the task
	// (supervise.Claim.GivenEntry, the queue's entry test). The held phase's retry, by any route
	// into it, is that start (workflow.Engine.restart): it raises no entry, and a claim whose
	// worker confirmed that entry's last task and whose turn then ended holds it as its given one
	// with no delivery pending, so the entry test alone would drop the retry's own task and
	// relaunch the worker with no prompt. The outbox row-id guard still holds
	// (supervise.Claim.DeliveredRows), so a retry of the row itself gives that task no second
	// time. It says nothing of who asked for the start (Decided below): the phase-entry upgrade
	// stamped it on each owed start whose claim already held an entry
	// (migrations/20261111225552_phase_entry.up.sql), whoever had queued that start.
	ResendEntry bool `json:"resendEntry,omitempty"`
	// Decided marks a start a person, the controller or the issue's architect decided. Two starts
	// carry it: the held phase's retry (workflow.Engine.restart), by any route into it — the
	// architect's retry_or_escalate, the operator's `legion claims retry` of a phase worker, a
	// person's todo on a held root — and admission's start of a promoted root's architect
	// (workflow.Engine.StartPromotedArchitect), which a todo on the root brings, the controller's
	// restart of a tree among them. It is the one start that launches again the agent of a claim
	// held for OpenAI's cyber-policy refusal: the outbox hands it to the claim's retry
	// (supervise.RequestRetry's Decided, which supervise.Claim.RetryHeld reads), and that retry
	// starts the agent in a new session (supervise's freshSession). Every other start of such a
	// claim is one the daemon makes by itself, which the machine refuses, whatever other mark it
	// carries. A row a release before this field wrote carries no Decided, so its start of a held
	// claim is refused too; where that start is of the issue's phase the refusal holds the issue
	// for its architect (the daemon outbox's heldStartRefused), and the architect's retry then
	// writes a row that carries it. A release before this field refuses a row that carries it
	// (DisallowUnknownFields), so the way back takes the key off every supervise row, with every
	// daemon of the database stopped, before the earlier image boots (docs/kubernetes.md, under
	// 20261125223008_claim_held_for).
	Decided bool `json:"decided,omitempty"`
	// ResumeTask marks Task as the phase's resume task (workflow's Engine.ResumeTask), the one a
	// re-admitted tree's promotion gives a mid-phase child: a claim that already holds a task for
	// the same generation and phase is given it once it is ready, so the start delivers none.
	ResumeTask bool `json:"resumeTask,omitempty"`
	// RoundNamed is the typed fact that Task ends with the daemon's two sentences of the reviewer's
	// review round (reviewround.Sentences). The queue sets it on each start of the reviewer that
	// carries a task, once it has named that task (workflow's queuedRow, namingTheReviewRound), and
	// three starts set it before the queue, since what they send is the task enter stored with the
	// sentences already in it (workflow's startMarks.stored), so the queue adds none. The outbox's
	// start carries it into the claim's delivery (supervise.RequestDeliver.RoundNamed), and the one
	// place each prompt of a delivery is composed reads it there (supervise's Delivery.message): a
	// reviewer's task with no such fact, which only a row the release before this one queued can
	// be, is given the daemon's sentences naming no commit and no entry at its end, so no text a
	// task of the release before ends with is taken for the daemon's. A row the release before
	// wrote decodes with it false. The release before decodes strictly (DisallowUnknownFields), so
	// the way back of 20261116234034_phase_reviewed_head deletes the key from each row before that
	// release boots.
	RoundNamed bool `json:"roundNamed,omitempty"`
	// RuleResume marks the boot's resume-by-rule start, and the boot's one-time start of a merger
	// the `awaiting_merge` cutover left released (workflow's cutoverMergerRepair). If its resumed
	// claim spends its launch budget, the runner finishes this row rather than retrying it as
	// RequestRetry with fresh budgets: the failed claim stays failed for the boot report to name.
	RuleResume bool `json:"ruleResume,omitempty"`
	// NeedsSlot marks a start whose phase holds one of the cap's slots (PhaseKeepsSlot) as one
	// that waits for its issue's admission slot. The cap counts the issues the daemon has
	// started whose change is still to write, so what it must bound is the start of a phase that
	// holds a row; everything else the transition writes — the phase, the Dispatch status, the
	// entry and its task on the role's phases row, the notices — is written in the transition's
	// own transaction and waits for nothing. A start into accepting, retro or merging carries no
	// mark, since an issue in those phases holds no row for such a start to wait for, and it is
	// claimable the moment it commits. workflow.StartOrOwe is the
	// one writer of the mark, which it reads from the start's own phase, so no caller decides
	// it. The hold is on this row's claimability alone (store.go's outboxInOrder): a row that is
	// merely unclaimable takes no lease, so none of its issue's later rows is held behind it.
	// The row carries the worker's task whether or not it is marked, since a start is one row
	// either way; what the mark decides is when the worker comes back with it. While the hold
	// stands, the daemon's store reads the same rule (its slotWaitingStart, held to this one by
	// TestTheIdleRuleAndTheOutboxAgreeOnAStartWaitingForItsSlot): the idle release does not keep
	// the issue's pod for the start (idleWaitingRow). What such a start means for a backstop is
	// the store's PodRecords.WaitsForItsSlot's to say.
	NeedsSlot bool `json:"needsSlot,omitempty"`
	// Relaunch is written by nothing. It marked the task-less half of the two rows a phase
	// worker's start used to be, before a start became one row that carries its task
	// (workflow.StartOrOwe); the outbox decodes it now and acts on it nowhere.
	//
	// It is kept for one release because the outbox decodes strictly
	// (DecodeOutboxPayload, DisallowUnknownFields) and a row the previous release wrote may
	// still be in the table when this one deploys: a payload the decoder refuses is a row that
	// retries for ever. The validation rule below is kept with it, so such a row still passes
	// the check the decode makes. The release after this one may delete both.
	Relaunch bool `json:"relaunch,omitempty"`
	// Assignment is written by nothing. It carried the token the release before the phase entry
	// minted for a start and wrote on the role's phases row, which Entry above replaced
	// (INFRA-2800); the outbox decodes it now and acts on it nowhere.
	//
	// It is kept for one release for the reason Relaunch above is, and for the one the
	// migration's own header gives (migrations/20261111225552_phase_entry.up.sql): a rollback is
	// followed by a roll-forward, and the release before this one mints that token again at each
	// role's next start, so rows carrying assignment are written after any strip of the key has
	// run and are read again by this release. A decoder that refuses the field leaves such a row
	// retrying for ever and fails every admission pass of its issue besides, since
	// record.Postgres.RoleRuns strictly decodes every supervise row of the issues it reads. No
	// validation rule names it: the start it rides on passes the checks above as it stands. The
	// release after this one may delete it, and the strip of the key belongs there beside it.
	Assignment string `json:"assignment,omitempty"`
	// FreshSession marks a start that must not resume the session its claim recorded: the
	// operator's `legion claims retry --fresh-session` (api's retryClaim). The runner passes it
	// to the claim's retry, which forgets the session inside that one decision (supervise's
	// RequestRetry), so a start that never ran leaves the session where it was.
	FreshSession bool `json:"freshSession,omitempty"`
	// Linger is the root generation whose linger a tree close expires. A member keeps its
	// generation across re-admission, so the close acts only while its tree lingers at that root
	// generation: never in the tree's next run, nor in a later linger of it.
	Linger uint64 `json:"linger,omitempty"`
	// Reason is why a suspend stops the claim, for the line the suspended claim logs.
	Reason string `json:"reason,omitempty"`
	// TurnStart fences the `suspend` of a left pod's close on a turn: it is the newest turn start
	// of the issue's claims in the view the pass queued the close from, as the close's workspace
	// check carried it (IssueWorkspaceCheck.TurnStart), the zero moment where none had had a turn.
	// The suspend of a claim whose last turn started after it finishes without acting (the daemon
	// outbox's suspend), since that turn may have made work the check's read did not see, as a
	// release's role stop is cancelled by a turn after its mark (issue_releases.turn_start). The
	// claim stays live, the close's Sandbox suspension carries the same fence and finishes without
	// acting too (IssueSuspend.TurnStart), and the next idle pass decides the close again. Every
	// other request carries none: nil is no
	// fence, and the zero moment is a fence that any turn passes. A release before this field
	// refuses a row that carries it (DisallowUnknownFields), so a rollback waits for such rows to
	// run (docs/kubernetes.md, "Rolling back the release that keeps no saved workspace").
	TurnStart *time.Time `json:"turnStart,omitempty"`
	// CheckLook is the look of the issue's production check a start starts, 0 for a phase start.
	// A check start runs after the close, where the issue has no phase and its tree may linger, so
	// the outbox fences it on the open check and its look (the row acts only while the check is
	// open at that look with the look open) instead of on the generation and the lingering tree.
	CheckLook int `json:"checkLook,omitempty"`
	// CheckOpenedAt is the check instance the look belongs to (ProductionCheck.OpenedAt, its ID):
	// a later merge of the same issue replaces the check and counts its looks from one again, so a
	// start row of the replaced check's look 1 would otherwise act on the new check's. The outbox
	// finishes a start without acting when the issue's check is not the one it names; zero names
	// no instance (a row queued before the field) and is not fenced.
	CheckOpenedAt time.Time `json:"checkOpenedAt,omitzero"`
	// Pause is the project pause a row serves (store's Pause, its id). A `pause_stop` row carries
	// it always: its executor stops the claim for that pause, and finishes without acting once
	// the pause has ended (decision 4 of dispatch://INFRA-3759). A start may carry the pause that
	// held it, and the launch a pause owed a role it stopped always does: the start the stop
	// transaction writes beside the held wake (WakeKind pods.WakePause). Every other request
	// carries none.
	Pause int64 `json:"pause,omitempty"`
	// Issue is the issue of the claim a `pause_stop` row stops where the row is keyed by the
	// project rather than by that issue (pause.StopKey): a claim of another project's issue, which
	// the operator's spawn makes under the daemon's own project token. Such a row is keyed by the
	// project because the outbox runs only the rows of its own project's keys (DueRows), and its
	// executor reads the claim from this issue. It is an issue key, and every other row carries
	// none, a `pause_stop` row keyed by its claim's issue among them. A release before this key
	// refuses a row that carries it (DisallowUnknownFields); the rollback deletes every
	// `pause_stop` row (migrations/20261115194357_project_pause.up.sql).
	Issue string `json:"issue,omitempty"`
	// WakeKind marks a start that brings a released role back for a wake: the wake's kind, one of
	// the pods wake kinds (validWakeKind), as the resume the wake took records it. The start
	// finishes without acting when its claim is suspended by the daemon, since a park, a close or
	// a moved phase took the role over while the row waited (decision 10).
	// A start that is no wake, and every request of another op, carries none.
	WakeKind string `json:"wakeKind,omitempty"`
}

func (SuperviseRequest) OutboxKind() OutboxKind { return OutboxKindSupervise }

// GateSeed records the design document version that starts a gate, and the step whose row it
// starts: the seed reads that document from Dispatch and applies what it finds there.
type GateSeed struct {
	ArtifactID string `json:"artifactId"`
	// Step is the design-gate step the registered document is that of. A row written before a
	// deployment could name a list carries none, and the facts its seed builds name no step.
	Step    DesignStep `json:"step,omitempty"`
	Version int        `json:"version"`
	// Generation is the issue generation that registered the gate; the seed's fact is that
	// generation's.
	Generation uint64 `json:"generation"`
}

func (GateSeed) OutboxKind() OutboxKind { return OutboxKindGateSeed }

// PendingBasisSeed reads, from Dispatch, the ancestor's document and issue a root's step waits on
// through the ancestor's approval request (DesignGate.Basis.Pending) and applies what it finds, as
// a fact of its own rather than the gate seed's: a person's approval of the version opens the
// step, a changes request, a newer version, the request's close with no approval or the
// ancestor's leave ends the wait, and a request still waiting changes nothing. The row's issue is
// the root, and RegisteredAt names which registration of the step it reads for (normalized once,
// by PendingBasisSeedOf, to UTC microsecond precision, matching the stored row), so a second wait
// registered again on the same request is a fact of its own. Its read is the one that opens or
// ends such a wait, and it is queued whenever the daemon must learn what Dispatch shows of it:
// when the step is registered on the request, for every such step of the project at the daemon's
// boot, whatever phase its root stands in, and on every event that is a cue for the step
// (workflow's pendingBasisCue), which itself opens and ends nothing.
type PendingBasisSeed struct {
	// Step and ArtifactID are the root's step and its own document the step was registered on.
	Step       DesignStep `json:"step"`
	ArtifactID string     `json:"artifactId"`
	// BasisArtifactID is the ancestor's document, and BasisVersion the version of it the request
	// asks a person to approve.
	BasisArtifactID string `json:"basisArtifactId"`
	BasisVersion    int    `json:"basisVersion"`
	// Ask is the approval request the step waits on (PendingBasis.Ask).
	Ask string `json:"ask"`
	// Generation is the root generation that registered the step; the seed's fact is that
	// generation's.
	Generation uint64 `json:"generation"`
	// RegisteredAt is the daemon's own time of the registration the seed reads for
	// (GateBasis.RegisteredAt, which PendingBasisSeedOf normalizes once to UTC microseconds as the
	// store keeps it), so each registration of the step on the same request is a wait of its own.
	RegisteredAt time.Time `json:"registeredAt"`
}

func (PendingBasisSeed) OutboxKind() OutboxKind { return OutboxKindPendingBasisSeed }

// PendingBasisSeedOf is the seed of gate, a step that waits on an ancestor's approval request
// (DesignGate.PendingOnBasis), for the root generation that registered it: the one row every
// queue of such a read writes, so each reads the same ancestor, version, request and registration.
// RegisteredAt is normalized here, and nowhere else, to UTC at microsecond precision, the precision
// Postgres stores and reads back, so a seed queued from an in-memory gate names the same wait as
// one queued from the stored row.
func PendingBasisSeedOf(gate DesignGate, generation uint64) PendingBasisSeed {
	return PendingBasisSeed{Step: gate.Step.OrSpec(), ArtifactID: gate.ArtifactID, BasisArtifactID: gate.Basis.ArtifactID,
		BasisVersion: gate.Basis.Version, Ask: gate.Basis.Pending.Ask, Generation: generation,
		RegisteredAt: gate.Basis.RegisteredAt.UTC().Truncate(time.Microsecond)}
}

// ReparentSeed reads the new Dispatch parent of a root that ran as a tree of its own and that a
// person has made a child of a live tree (workflow's tellReparented), and applies what it finds
// as a fact that posts the message on the row's issue, the re-parented root, and queues the
// child-status notice to the architect of the parent's tree. The engine does no I/O, so the read
// of whether the parent carries the legion label, holds a person's approval of its primary
// document's latest version and marks requirement blocks in it, which decides whether the words
// name the path of a view on that parent, waits on this row.
type ReparentSeed struct {
	// Parent is the root's new Dispatch parent, whose labels and primary document the row reads.
	Parent string `json:"parent"`
	// Tree is the root of the live tree Parent belongs to, whose architect the notice goes to.
	Tree string `json:"tree"`
	// Ran is why the root stays a tree of its own (workflow's RanAsRoot), which the words name.
	Ran string `json:"ran"`
}

func (ReparentSeed) OutboxKind() OutboxKind { return OutboxKindReparentSeed }

// LingerClose records the issue generation whose linger should close.
type LingerClose struct {
	Generation uint64 `json:"generation"`
}

func (LingerClose) OutboxKind() OutboxKind { return OutboxKindLingerClose }

// IssueSuspend stops an issue's pod after its close has stopped every role, and deletes the
// Sandbox the roles ran in (runtime.IssueSuspender). Both workflow generations and this row's ID
// fence a later re-admission or start. The issue keeps nothing: an ephemeral disk is its pod's own
// and goes with the pod, and a kept volume goes with the Sandbox, so a later todo re-enters the
// issue fresh on a Sandbox its next launch makes.
//
// TurnStart, on the suspension of a left pod's close that stops a role, is the turn start its
// workspace check read, as each of that close's `suspend` rows carries it
// (SuperviseRequest.TurnStart): the suspension finishes without acting where a claim of the issue
// started a turn after it (store.IssueSuspension), as the stop of that claim's role did, so
// nothing of the close outlives a stop its fence held and the next idle pass decides the close
// again with a read of its own. Every other suspension carries none: nil is no fence, and the
// zero moment is a fence that any turn passes. A release before this field refuses a row that
// carries it (DisallowUnknownFields), so a rollback waits for such rows to run
// (docs/kubernetes.md, "Rolling back the release that keeps no saved workspace").
//
// PodUID, on the suspension a left pod's close queues after its launcher's clean read, is the pod
// that read found clean (IssueWorkspaceCheck.PodUID): the suspension takes that pod or nothing,
// read under the runtime's pod lock (internal/daemon's suspendIssue). "" binds no pod. A release
// before this field refuses a row that carries it (DisallowUnknownFields), so a rollback waits for
// such rows to run too (docs/kubernetes.md, "Rolling back the release that keeps no saved
// workspace").
type IssueSuspend struct {
	Tree           string     `json:"tree"`
	Generation     uint64     `json:"generation"`
	TreeGeneration uint64     `json:"treeGeneration"`
	TurnStart      *time.Time `json:"turnStart,omitempty"`
	PodUID         string     `json:"podUid,omitempty"`
}

func (IssueSuspend) OutboxKind() OutboxKind { return OutboxKindIssueSuspend }

// IssueWorkspaceCheck is the first act of an idle pod's release: it asks the shim of Role — a role
// whose process is still running in the pod, because this row runs before the `release` rows that
// stop them — to read whether the workspace every role of the issue shares holds work that no
// branch or tag on origin holds (runtime.WorkspaceWorkReader). Only a shim can read it: it runs the
// `jj` of the workspace it already works in, and it dies with its role's process, so the read has
// to happen while that process is there. A read that finds work, or that fails, declines the
// release: the pod stays with the work, and Role is told once that the release waits for its push
// (internal/daemon's checkIssueWorkspace). The daemon pushes nothing for a role.
//
// It stops nothing, so a release cancelled here costs only the read, and it sits before the
// release's boundary (the first `release` row), so a delivery queued while it runs still cancels
// the whole release (outbox.cancelRelease). The teardown waits for it (store.IssueRelease).
//
// CloseRoles, where it names any role, makes the row the first act of another act: the close of a
// pod whose issue left the workflow while its tree runs on (workflow.LeftPodClose), which marks no
// release. Those are the live roles the close stops. The check reads the same way and declines the
// same way, and on a read that finds nothing origin does not hold it queues the close's stops and
// the Sandbox suspension itself (internal/daemon's checkLeftPodWorkspace). A row with no
// CloseRoles is a release's, and its JSON is the one an earlier release wrote.
//
// TurnStart, on a close's row, is the newest turn start of the issue's claims in the view the pass
// queued the close from (pods.IdleView.NewestTurnStart), zero where none had had a turn: the close
// stops no role after a turn that started since, as a release's mark holds the same moment for its
// own stops (issue_releases.turn_start). The check holds the close on a turn before its decision
// (workflow.LeftPodCloseAfterCheck), and each `suspend` it queues carries the moment on
// (SuperviseRequest.TurnStart), so a turn between the decision and the stop holds that stop. A
// release's row leaves it zero, since its mark holds it.
//
// Launcher makes the pod's launcher read the workspace rather than Role's shim (R6 of
// #5285, runtime.PodWorkspaceReader): no role process runs in the pod, so no shim
// is there to read through. It is the check of the release of a pod no claim owns
// (workflow.UnownedPodRelease), inside that release's mark, and, with Close, of the close of a left
// pod with no live claim, which marks no release and stops no role: its clean read queues the
// Sandbox suspension alone, fenced on TurnStart. Such a row names no Role, since no role's shim
// reads it; a decline it records tells the issue's architect (workspace-kept, WorkspaceKeptNotice).
// A release before these two fields refuses a row that carries either (DisallowUnknownFields), so
// a rollback waits for such rows to run (docs/kubernetes.md, "Rolling back the release that keeps
// no saved workspace").
//
// PodUID, on a launcher's row, is the pod the pass read the runtime holding when it queued the
// check (pods.IdleView.PodUID), "" where it could not say: a read the launcher made of another pod
// is evidence of that pod alone, and is discarded (internal/daemon's checkIssueWorkspace and
// checkLeftPodWorkspace), and the teardown and the suspension that a clean read lets through take
// that pod or nothing (IssueRelease.PodUID, IssueSuspend.PodUID).
type IssueWorkspaceCheck struct {
	Tree           string       `json:"tree"`
	Generation     uint64       `json:"generation"`
	TreeGeneration uint64       `json:"treeGeneration"`
	Role           claim.Role   `json:"role"`
	CloseRoles     []claim.Role `json:"closeRoles,omitempty"`
	TurnStart      time.Time    `json:"turnStart,omitzero"`
	Launcher       bool         `json:"launcher,omitempty"`
	Close          bool         `json:"close,omitempty"`
	PodUID         string       `json:"podUid,omitempty"`
}

// LeftPodClose is whether the check is the first act of a left pod's close (CloseRoles, or Close
// for a close that stops no role) rather than of a release.
func (c IssueWorkspaceCheck) LeftPodClose() bool { return len(c.CloseRoles) > 0 || c.Close }

func (IssueWorkspaceCheck) OutboxKind() OutboxKind { return OutboxKindIssueBundle }

// IssueRelease is the last act of an idle pod's release, queued behind the workspace check
// (IssueWorkspaceCheck) and the per-role `release` supervise rows that stopped the pod's role
// processes: it asks the runtime's release (runtime.IssueReleaser), which suspends the issue's
// Sandbox and waits its pod out, keeps a Sandbox that owns the issue's kept volume and deletes
// every other Sandbox with the role Secrets it owns, and records the release. Both workflow
// generations and this row's ID fence a start or a re-admission that came after, as an
// IssueSuspend's do.
//
// Unlike a close, the issue stays in the workflow: the claims keep their phase and their pending
// task, and the next wake for one of the roles brings a new pod, whose workspace provisioning makes
// again from what was pushed to the issue's branch. The check took the pod only because the
// workspace held nothing that was not pushed. A new pod of a kept Sandbox mounts the volume the
// last pod used.
//
// PodUID, on the teardown of a release whose launcher reads the workspace first, is the pod that
// check reads (IssueWorkspaceCheck.PodUID): the teardown takes that pod or nothing, read under the
// runtime's pod lock, and a pod that took its place since cancels the release (internal/daemon's
// releaseIssue). "" binds no pod. A release before this field refuses a row that carries it
// (DisallowUnknownFields), so a rollback waits for such rows to run, an issue_release row behind
// its check among them (docs/kubernetes.md, "Rolling back the release that keeps no saved
// workspace").
type IssueRelease struct {
	Tree           string `json:"tree"`
	Generation     uint64 `json:"generation"`
	TreeGeneration uint64 `json:"treeGeneration"`
	PodUID         string `json:"podUid,omitempty"`
}

func (IssueRelease) OutboxKind() OutboxKind { return OutboxKindIssueRelease }

// WorkspaceRemove removes the workspace named by the row's issue.
type WorkspaceRemove struct {
	// Linger is the root generation whose linger the removal expires. Like a tree close, it acts
	// only while the tree lingers at that root generation: once re-admission moves the root on, the
	// workspace there is the tree's next run's, whatever generation the issue itself keeps.
	Linger uint64 `json:"linger"`
	// Tree is the tree whose linger queued the removal. An issue adopted into another tree since
	// (workflow.Engine.Adopt) works there now, so the removal finishes without acting. A row queued
	// by an earlier release has none, and is fenced by Linger against the issue's current tree.
	Tree string `json:"tree,omitempty"`
}

func (WorkspaceRemove) OutboxKind() OutboxKind { return OutboxKindWorkspaceRemove }

// RoleMessage is one message the daemon sends a role holder, published to that role's topic: Role
// is the claim token, which the runner addresses as a role topic, and Body is the whole of what the
// holder is told. It is how the daemon reaches a worker with something to read rather than a task
// to do - a settled head's check results, which move nothing (workflow's tellChecks,
// dispatch://INFRA-714/spec R1). A role with no live holder is told nothing: the row finishes,
// since the message is of a moment that has passed by the time anyone claims the role, and every
// recipient can read the pull request's checks on GitHub for itself.
//
// A role whose claim is released is told nothing either, and is not woken for the message: the
// row finishes without a publish. The message's three senders in the workflow - tellChecks (a CI
// settlement), tellGateMoved (a design gate's move) and tellStuckMergeState (an approved round
// stuck on its merge state) - only inform; before a release stopped its session cleanly, such a
// message to a released role was dropped all the same once that session expired; and a wake for
// each would start a pod for every CI settlement and every gate move, once per released role.
// Published, the message would meet a released role as one nobody holds: the listener answers
// holder_lapsed, or unclaimed once the reaper has freed the claim, and raises the role's wake
// beside either, so the pod would come back for a message the row had already dropped. The one
// exception is a row a project's pause held for a role a pause stopped: without the pause the role
// would have had the message, so the row is published to the role after the resume, and that
// publish wakes it (internal/daemon's outbox.roleMessage, R4 of dispatch://INFRA-3759).
type RoleMessage struct {
	Role string `json:"role"`
	Body string `json:"body"`
}

func (RoleMessage) OutboxKind() OutboxKind { return OutboxKindRoleMessage }

// GatedStepsPublish is the design-gate steps the deployment gates, written onto the Dispatch
// project row so a person reading Dispatch sees what holds a tree's work without reading the
// deployment's configuration. Project is the Dispatch project key, and Steps the configured list
// in the order the deployment gates it, empty for a deployment that gates nothing.
//
// The row belongs to no issue: its issue column holds Project itself, the bare key, which the
// same split_part every reader scopes a row by answers. The daemon publishes at every boot and
// keeps no record of what it last published, so the route is what decides whether anything
// changed; a boot whose list matches the stored one writes no row there and appends no event.
type GatedStepsPublish struct {
	Project string       `json:"project"`
	Steps   []DesignStep `json:"steps"`
}

func (GatedStepsPublish) OutboxKind() OutboxKind { return OutboxKindGatedStepsPublish }

// DesignGatesPublish is the document each of an issue's design-gate steps records, written onto
// the issue's Dispatch row (PUT /api/v1/issues/{key}/design-gates) so Dispatch's write advice and
// its CLI can name the document a step holds without reading the daemon's record. Issue is the
// issue key, and Gates the steps it records a document for, empty for an issue whose gates record
// none, which is the set that clears what Dispatch holds.
//
// The row belongs to its issue, unlike the gated_steps_publish beside it, so it waits on that
// issue's board lane among the issue's other Dispatch writes. The write replaces the issue's
// whole set and tells nobody, so a set Dispatch already holds costs an issue nothing.
type DesignGatesPublish struct {
	Issue string               `json:"issue"`
	Gates []DesignGateDocument `json:"gates"`
}

// DesignGateDocument is one step of a publication, the document that step's gate recorded, and
// Holds, whether the issue's tree is gated on that step (GateDocuments: TreeSteps over the
// deployment's steps and the issue's own rows). Dispatch's advice reads Holds to name the gates a
// new version of an approved document closes: a criteria row a root registered beside a
// deployment that gates spec and not criteria holds that tree although the project's gated steps
// do not name criteria, and a row kept under `gates.design: off` holds nothing although the row is
// there.
// Holds is nil only in a payload an older daemon queued, which carried no such field; it is sent
// as null, which Dispatch reads as a row an older daemon published and answers by the project's
// gated steps, as it answered every row before this field.
type DesignGateDocument struct {
	Step       DesignStep `json:"step"`
	ArtifactID string     `json:"artifactId"`
	Holds      *bool      `json:"holds"`
}

func (DesignGatesPublish) OutboxKind() OutboxKind { return OutboxKindDesignGatesPublish }

// DesignNewsReconcile is the reconcile of one tree's news of its approved design: the row's issue
// column is the tree's root, which is the whole of what it carries. Its executor (the daemon's
// designChanges) reads what the tree owes (OwedNews) when it runs, never what it owed when the row
// was queued, so one unrun row stands for every reason to run one, and a write that would queue a
// second beside it queues nothing (QueueDesignNews). A row already running is not that row: it
// may have read the record before the write that would queue one, so such a write queues another.
type DesignNewsReconcile struct{}

func (DesignNewsReconcile) OutboxKind() OutboxKind { return OutboxKindDesignChanges }

// RoleHoldersPublish writes the session of each live Legion role of Issue onto the issue's
// Dispatch row (PUT /api/v1/issues/{key}/role-holders), which Dispatch reads to take the role of a
// result from the session that wrote it. The payload names the issue alone: the executor reads the
// live set from the claims when the row runs (publishRoleHolders), so a row queued before a claim
// ended sends the set without it, and two events of one issue never publish a stale set.
//
// The row waits on its issue's board lane among the issue's other Dispatch writes. The write
// replaces the issue's whole set and tells nobody, so a set Dispatch already holds costs nothing.
type RoleHoldersPublish struct {
	Issue string `json:"issue"`
}

func (RoleHoldersPublish) OutboxKind() OutboxKind { return OutboxKindRoleHoldersPublish }

// StepRecord is one write of an issue's record of one step onto Dispatch (PUT
// /api/v1/issues/{key}/steps/{id}): the start of the step, its end, or the report alone of a
// completion that landed after that end (workflow's reportEndedStep). ID is the record's id, a
// uuid every write of one run of the step computes, and the row's issue column is the issue. Each
// other field is a field of the route's body under the same name.
//
// Dispatch writes each field once and keeps every field a write leaves out, so the writes of one
// run merge in any order and a write that runs again changes nothing. Every write carries the
// identity of the step (task, step, kind, role, type, agent and generation), the empty type of a
// retro or a merge and the place 0 of either included, and each field a write may not know yet is
// a pointer, nil for one the write leaves to another. The route takes a text field sent as "" as
// left out, so for a text field the pointer only says what the write knows; for a time and for
// ReturnedTo it is what tells "not known" apart from a value: a zero time would be a moment
// Dispatch keeps for good, and ReturnedTo 0 is a return to the retro, whose place is 0.
type StepRecord struct {
	ID         string     `json:"id"`
	Task       string     `json:"task"`
	Step       int        `json:"step"`
	Kind       string     `json:"kind"`
	Role       string     `json:"role"`
	Type       string     `json:"type"`
	Agent      string     `json:"agent"`
	Generation uint64     `json:"generation"`
	Session    *string    `json:"session,omitempty"`
	Model      *string    `json:"model,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	Result     *string    `json:"result,omitempty"`
	ReturnedTo *int       `json:"returned_to,omitempty"`
	Report     *string    `json:"report,omitempty"`
	// PlanRating is the rating the retro gave the plan, with its reason: the one write that carries
	// it is the retro's rating, which Dispatch takes on the record's rating route
	// (dispatch.HTTPClient.PutStep), and every other write leaves it out. It is embedded, so its two
	// members are members of the payload itself, written together and left out together when it is
	// nil (validateOutboxPayload holds a stored row to that).
	*PlanRating
	// StepExperiment is the experiment a draw put the record's issue in and the variant it drew
	// there, nil for an issue that no such draw names. Dispatch takes it on the record's experiment
	// route, after the write of the record itself (dispatch.HTTPClient.PutStepExperiment). It is
	// embedded, as PlanRating is, so its two members are members of the payload, written together
	// and left out together (validateOutboxPayload holds a stored row to that).
	*StepExperiment
}

// PlanRating is a retro's rating of the plan its issue ran: Rating, on the scale
// github.com/sjawhar/envoy/steprecord holds (RatingLeast, the plan cost the work, to RatingMost,
// the plan fit the work), and Reason, why. The two are one value, so a step record carries a
// rating with its reason or no rating. It mirrors dispatch.PlanRating, which the publication
// converts it to (publishStepRecord), because the dispatch package imports no record of the daemon's;
// internal/daemon's TestAStepRecordReachesDispatchWithEveryField holds the two to the same fields.
type PlanRating struct {
	Rating int    `json:"rating"`
	Reason string `json:"rating_reason"`
}

// StepExperiment is the experiment a draw put an issue in and the variant it drew there. The two
// are one value, so a step record names both or neither. It mirrors dispatch.StepExperiment, which
// the publication converts it to (publishStepExperiment), as PlanRating mirrors
// dispatch.PlanRating; internal/daemon's TestAStepRecordReachesDispatchWithEveryField holds the two
// to the same fields.
type StepExperiment struct {
	Experiment string `json:"experiment"`
	Variant    string `json:"variant"`
}

func (StepRecord) OutboxKind() OutboxKind { return OutboxKindStepRecord }

// PlanRecord is the plan one run of an issue carries out, written onto Dispatch (PUT
// /api/v1/issues/{key}/plans/{id}) when the run's work starts and again at each later version of
// the owner's plan that Legion takes (workflow's planNode). ID is the plan's id, a uuid of the
// issue and its run, one for each run, and the row's issue column is the issue. Each other field
// is a field of the route's body under the same name: the run (record.Issue.Generation), where the
// plan came from (default, the steps Legion runs when the owner gave none, or owner), the owner's
// document with its version, which a plan of the source default leaves out, and each task with its
// steps in order. The source, the document, the version and the tasks are one fact, so each write
// carries all four: a write whose version is above the version Dispatch holds replaces all four,
// and a write at or below it changes nothing, a version left out (the source default) counting as
// 0. So a write that arrives late or twice does no harm.
type PlanRecord struct {
	ID         string `json:"id"`
	Generation uint64 `json:"generation"`
	Source     string `json:"source"`
	// PlanDocument is embedded, so its two members are members of the payload itself, written
	// together and left out together when it is nil.
	*PlanDocument
	Tasks []PlanRecordTask `json:"tasks"`
}

// PlanDocument is the owner's document a plan of the source owner was taken from: the artifact and
// the version of it Legion took. The two are one value, so a plan names its document with its
// version or no document. It mirrors dispatch.PlanDocument, which the publication converts it to
// (publishPlanRecord), because the dispatch package imports no record of the daemon's.
type PlanDocument struct {
	ArtifactID string `json:"artifact_id"`
	Version    int    `json:"version"`
}

// PlanRecordTask is one task of a plan: its id, its steps in order, each a role or `role:key`, and
// the tasks it needs, each by its id (record.PlanTask.Needs, as register_plan resolved them), [] for
// a task that needs none. Needs is nil in a row the release before wrote, which carried no needs:
// the write then carries no member, and Dispatch reads the plan's needs as not recorded. The
// release before decodes strictly (DisallowUnknownFields), so the way back in docs/kubernetes.md,
// "Rolling back the release that records each task's needs", takes `needs` off each queued row
// before that release boots.
type PlanRecordTask struct {
	ID    string    `json:"id"`
	Steps []string  `json:"steps"`
	Needs *[]string `json:"needs,omitempty"`
}

func (PlanRecord) OutboxKind() OutboxKind { return OutboxKindPlanRecord }

// IssueBranch creates the row's issue's branch, legion/<KEY>, on GitHub at the repository's default
// branch, where GitHub does not have it yet, before any role of the issue starts: GitHub can refuse
// the push that creates a branch of a large repository, where it takes a push that moves one. A
// start of the issue waits while the row is unfinished (outboxInOrder).
type IssueBranch struct {
	// Generation is the issue generation the branch is created for. A row of an earlier generation,
	// or of a tree that lingers, finishes without acting.
	Generation uint64 `json:"generation"`
}

func (IssueBranch) OutboxKind() OutboxKind { return OutboxKindIssueBranch }

// CheckWakeDue is the one row that can start a production check's next look beside the check's
// own bound (CheckBound): the wake the checking agent named for itself on a pending report
// (ProductionCheck.WakeAt), or the retry of a look nobody reported. It is enqueued at the end of a
// look and consumed when it fires. Two writes queue another while it waits, each in the
// transaction that moves the check's wake: workflow's checkWakeDue, for an issue back in the
// workflow, which finishes the row it came from, and a failure of the checking role's claim
// between looks, which names the next look later (workflow's failedBetweenLooks) and leaves the
// row before it standing. Nothing else wakes a check: no boot, no build and no deploy event reads
// production any more.
//
// Look is the look the check stood at when the wake was named, so a row left over from an earlier
// look starts nothing; OpenedAt is the check instance it was named for
// (ProductionCheck.OpenedAt), since a later merge of the same issue replaces the check and counts
// its looks from one again — zero names no instance and is not fenced. WakeAt is the wake the row
// was queued for (ProductionCheck.WakeAt, at the microsecond), so a row whose wake the check no
// longer records, the one a failure between looks moved, starts nothing either; zero, a row a
// release before this field queued, is not fenced. Reason is what the wake is, for the look's
// wake and the log: the agent's own wake, or the retry of a look nobody reported.
type CheckWakeDue struct {
	Look     int       `json:"look"`
	OpenedAt time.Time `json:"openedAt,omitzero"`
	WakeAt   time.Time `json:"wakeAt,omitzero"`
	Reason   string    `json:"reason"`
}

func (CheckWakeDue) OutboxKind() OutboxKind { return OutboxKindCheckWake }

// CheckLook is a look's bound: due at the look's wake plus the look bound, it ends look Look of
// the issue's production check as a pending report would, when the look is still open then.
// OpenedAt is the check instance the look is of (ProductionCheck.OpenedAt): a later merge of the
// same issue replaces the check and counts its looks from one again, and a bound row of the
// replaced check's look must not end the new check's; zero names no instance and is not fenced.
type CheckLook struct {
	Look     int       `json:"look"`
	OpenedAt time.Time `json:"openedAt,omitzero"`
}

func (CheckLook) OutboxKind() OutboxKind { return OutboxKindCheckLook }

// CheckBound is the whole check's bound, due at its bound_at: it starts the check's last look, or
// defers while a look is open.
type CheckBound struct{}

func (CheckBound) OutboxKind() OutboxKind { return OutboxKindCheckBound }

// PhaseEnded tells the worker of a parent that the admission pass moved into awaiting_children in
// the middle of its phase that its phase is over, on that role's own Envoy topic: a parent whose
// Dispatch child is open starts no phase, so the daemon ends the phase its worker is in and no task
// of that worker's follows (workflow's StopForChildren, dispatch://INFRA-3320 R10). Without it
// nothing tells the worker, since the daemon's next task for a role waits for that role's turn to
// end. Role is the recipient, From the phase that ended, To the phase the issue is in now, which is
// awaiting_children, Reason the open children that hold the parent, and MergeForward the merge
// forward of the parent's pull request that the move dropped before it landed, which chooses the
// form of the message (Message). It stops nothing: the recipient keeps its turn, its jobs, its
// process and its session.
//
// The kind is phase_ended, which the releases before dispatch://INFRA-4374 wrote for a head that
// conflicts with its base, and whose queued rows INFRA-4374's migration 20261119003157 deleted. A
// row that names no dropped merge forward has that kind's shape, so a release that still decodes
// it reads such a row. `mergeForward` is this release's field, and a release before this one
// decodes strictly (DisallowUnknownFields), so it refuses a row that carries it.
type PhaseEnded struct {
	Role   claim.Role  `json:"role"`
	From   phase.Phase `json:"from"`
	To     phase.Phase `json:"to"`
	Reason string      `json:"reason"`
	// MergeForward is the merge forward the move into awaiting_children dropped before it landed
	// (PullRequest.MergeForward with Landed empty), nil where it dropped none.
	MergeForward *DroppedMergeForward `json:"mergeForward,omitempty"`
}

// DroppedMergeForward is a merge forward a move into awaiting_children dropped before it landed
// (PhaseEnded.MergeForward): Head is the head the merge forward was asked of, Base the base branch
// it was to merge forward onto that head, and Role the role it was asked of (MergeForward.Asked):
// the implementer, or the merger for an ask made in merging. Its one writer, workflow's
// awaitChildren, sets Role on every value, and no release before this one wrote a row that carries
// the type, since such a release refuses a row with `mergeForward` (PhaseEnded). The row's
// validation refuses a dropped merge forward whose role is neither of the two, so no reader meets
// an empty role.
type DroppedMergeForward struct {
	Head string     `json:"head"`
	Base string     `json:"base"`
	Role claim.Role `json:"role"`
}

// childrenFinished is when a waiting parent's next task comes, in every message of a move into
// awaiting_children (PhaseEnded.Message, DroppedMergeForward.RoleMessageBody).
const childrenFinished = "The daemon gives the issue its next task once its children are finished."

// RoleMessageBody is the body of the role message (RoleMessage) the role d was asked of (Role) is
// sent when a move into awaiting_children that ends no phase of that role's drops d (workflow's
// awaitChildren): the issue waits on its open children, d named with that role's rule for it
// (told), and when the issue's next task comes.
func (d DroppedMergeForward) RoleMessageBody(issue string) string {
	return fmt.Sprintf("%s: the issue moved into %s, where it waits on its open Dispatch children. %s %s",
		issue, phase.AwaitingChildren, d.told(issue), childrenFinished)
}

// told is what the role d was asked of is told of d, in its phase-ended message
// (PhaseEnded.Message) or in its role message (RoleMessageBody), as dispatch://INFRA-3320 R10
// rules it for the implementer: d named, with its base and its head; a role that has begun the
// merge finishes it, pushes it and does nothing more, and one that has not begun it begins none;
// and the daemon does not ask for it again, so the role merges the base forward in a later task
// only once GitHub reads the pull request CONFLICTING, since the tester's merge with the base finds
// a conflict that is still there and fails its round.
func (d DroppedMergeForward) told(issue string) string {
	return fmt.Sprintf("The daemon dropped the merge forward of %[2]s onto head %[3]s that it asked of you for %[1]s:"+
		" if you have begun that merge, finish it, push it and do nothing more; if you have not begun it, begin none."+
		" The daemon does not ask for that merge forward again: in a later task of the issue, merge %[2]s forward only once GitHub reads its pull request CONFLICTING.",
		issue, d.Base, d.Head)
}

func (PhaseEnded) OutboxKind() OutboxKind { return OutboxKindPhaseEnded }

// Message is what the runner publishes for issue, R10's own words (dispatch://INFRA-3320 R10): the
// phase ended and no task follows, a completion of it is refused, what the worker does with the
// work it has, and the issue's next task comes once its children are finished. The reason comes
// last, behind the `Reason:` label a started worker's task carries it under (workflow's task), and
// names each open child. What the worker does with its work takes one of three forms:
//
//   - where the move dropped no merge forward that had not landed, the worker commits and pushes
//     what it has and does no more work on the phase, whatever its role;
//   - where it dropped one and the worker is the role it was asked of (DroppedMergeForward.Role),
//     the same, with the dropped merge forward named and that role's rule for it
//     (DroppedMergeForward.told);
//   - where it dropped one and the worker is another, it writes and pushes nothing more, since
//     the role the merge forward was asked of may be merging in the workspace the roles share,
//     and puts what it found on the Dispatch issue.
func (p PhaseEnded) Message(issue string) string {
	work := fmt.Sprintf("Commit and push what you have to the issue's branch, and do no more work on %s.", p.From)
	if dropped := p.MergeForward; dropped != nil {
		if p.Role == dropped.Role {
			work += " " + dropped.told(issue)
		} else {
			work = fmt.Sprintf("Write and push nothing more, because the %s may be merging in the workspace you share:"+
				" put what you found on the Dispatch issue, and do no more work on %s.", dropped.Role, p.From)
		}
	}
	return fmt.Sprintf("%s: the daemon ended your %s phase, and the issue is in %s now: it waits on its open Dispatch children, and no task of yours follows."+
		" A completion of %s is refused (HANDOFF_NOT_CURRENT_PHASE)."+
		" %s %s"+
		" Nothing of yours is stopped: your turn, your jobs, your process and your session run on."+
		" Reason: %s.",
		issue, p.From, p.To, p.From, work, childrenFinished, p.Reason)
}

// HookPrompt is a hook of the deployment that fired on a move back of the issue, and its words
// put in front of one role of the issue on that role's own Envoy topic. Hook is the hook's name
// as the deployment spells it, Role the recipient, From the phase the work left and To the phase
// it went back to, Count the hook's own count of that pair at the firing, Counts every
// backward-move count of the issue after the move, PullRequest the issue's pull request as
// `owner/name#n` where the issue has one, and Prompt the hook's own words.
//
// It rides the role-message lane (LaneRoleMessage) because a role hears a hook prompt while it is
// news, as it hears a merge forward's messages (MergeForwardMessage): a prompt held behind
// whatever the architect that owns its issue is owed would reach the role rounds after the round
// the firing wrote it for.
//
// The daemon writes the facts and never substitutes anything into the hook's own words: Message
// states the move, the counts and the pull request in sentences of the daemon's own, and then the
// prompt exactly as the deployment wrote it, so a hook that spells a word in angle brackets says
// that word. There is nothing for a hook to interpolate, because every fact it could ask for
// stands above its words already.
//
// The prompt is published with keep: for a role no live session holds, the listener answers it
// kept and holds it for an hour for the role's next live holder, and the row ends with one log
// line, the listener its keeper. A listener before #4079 keeps nothing for a claim whose holder is
// not live. The daemon's publishKeptToRole says what each answer does to the row, and why a later
// round's session can be handed the prompt and what bounds that; the move back and the counts
// Message states tell that reader which round the words were written for.
type HookPrompt struct {
	Hook        string         `json:"hook"`
	Role        claim.Role     `json:"role"`
	From        phase.Phase    `json:"from"`
	To          phase.Phase    `json:"to"`
	Count       int            `json:"count"`
	Counts      map[string]int `json:"counts"`
	PullRequest string         `json:"pullRequest,omitempty"`
	Prompt      string         `json:"prompt"`
}

func (HookPrompt) OutboxKind() OutboxKind { return OutboxKindHookPrompt }

// Message is what the runner publishes for issue, in three parts with a blank line between them:
// the daemon's header, which names the hook and the move back; the hook's words exactly as
// written; and the daemon's remaining facts, the count that fired the hook, every backward-move
// count of the issue and the pull request where the issue has one.
//
// The counts stand below the words because the words point at them: the shipped hook says the
// work "has gone back the number of times recorded below", and a message that put them above it
// would point at nothing. The header stays above, because a worker knows a hook by the line it
// starts with (`Legion hook `), which its role prompt and skill both name. The pull-request
// sentence is left out for an issue with no pull request.
//
// The counts are listed in the order phase.MovePairs() gives rather than the map's own, so
// two readings of one row read alike; a pair the map does not hold is left out. An empty map
// reads `Counts by move: none.` rather than dropping the sentence: the firing counted the move
// that fired it, so the map cannot normally be empty, and a sentence that says `none` tells a
// reader of a row written any other way that the daemon counted nothing, where a missing sentence
// would leave it to guess.
func (p HookPrompt) Message(issue string) string {
	listed := make([]string, 0, len(p.Counts))
	for _, pair := range phase.MovePairs() {
		if count, ok := p.Counts[pair]; ok {
			listed = append(listed, fmt.Sprintf("%s=%d", pair, count))
		}
	}
	counts := "none"
	if len(listed) > 0 {
		counts = strings.Join(listed, ", ")
	}
	pull := ""
	if p.PullRequest != "" {
		pull = fmt.Sprintf(" Pull request: %s.", p.PullRequest)
	}
	return fmt.Sprintf("Legion hook %s on %s: work went back from %s to %s.\n\n%s\n\n"+
		"Moves back this hook counts: %d. Counts by move: %s.%s",
		p.Hook, issue, p.From, p.To, p.Prompt, p.Count, counts, pull)
}

// UpdateAsk is an update round's ask to one role of the issue, on that role's own Envoy topic:
// the daemon opened Round over Pod and will replace that pod, and Text is the whole ask, as the
// sweep built it with supervise.UpdateTask from the round's recorded reasons and its recorded
// opening. Role is the recipient: a phase worker or the issue's architect, since every role whose
// process runs in the pod loses that process at the replacement. Pod is the pod the round was
// opened over: the sweep asks only a round whose claims record one.
//
// The row carries the finished text rather than the facts to build it from, for two reasons.
// record does not import supervise, which owns the words. And the round's terms are fixed when
// it opens, so a text built from what a later tick read would name drift the round was not
// opened for.
//
// The ask is best effort: the round ends at its bound whether or not the role reads it. So a
// row whose role no live session holds finishes undelivered, and a row whose round the claim no
// longer records, or whose claim the daemon suspended after the row was queued, finishes without
// a publish (the daemon's updateAsk). The daemon marks a claim asked only once its row's publish
// reached a session, so an ask that finished undelivered is lost, not given, and the daemon's
// sweep writes the claim a new row of this kind at a later tick.
type UpdateAsk struct {
	Role  claim.Role `json:"role"`
	Round string     `json:"round"`
	Pod   string     `json:"pod,omitempty"`
	Text  string     `json:"text"`
}

func (UpdateAsk) OutboxKind() OutboxKind { return OutboxKindUpdateAsk }

// Message is what the runner publishes for issue: the text the sweep built, unchanged. Nothing is
// put in front of it, since the text names the round and the topic it goes to is the role's own
// claim on issue.
func (p UpdateAsk) Message(string) string { return p.Text }

// MergeQueueRead is the daemon's one read of a pull request that GitHub reported leaving the merge
// queue (intake.PullRequestDequeued). The transaction that applies that event writes it, while the
// issue is in merging at the head the event names, and its executor reads the pull request from
// GitHub once, retrying only a read that failed. A merged answer applies the merge, as the
// webhook's merge would. Any other answer, or no answer from any read the executor makes, applies
// the fact that writes the merger's message (MergeQueueRemoval), and ends this row. Number is the
// pull request, and Head the head that left the queue.
//
// The read and the message are two rows because no row's payload is rewritten and every retry is
// an attempt: one row that both read and waited for the merger's session would read GitHub again
// at each of those waits, and a later failed read would hold back a message an earlier read had
// already decided. A durable row, rather than a read held in memory, is what a restart between the
// event and the read cannot lose, since the event is acknowledged once its transaction commits.
type MergeQueueRead struct {
	Number int    `json:"number"`
	Head   string `json:"head"`
}

func (MergeQueueRead) OutboxKind() OutboxKind { return OutboxKindMergeQueueRead }

// MergeQueueRemoval tells the issue's merger, on the merger's own role topic, that the pull request
// left the merge queue at Head. The fact the merge-queue read applies writes it when GitHub did not
// report the pull request merged (MergeQueueRead). Number is the pull request and Head the head
// that left the queue. Unread is that no read of GitHub succeeded, so the daemon does not know
// whether GitHub merged the pull request, and the message says so. GitHub's event names no reason
// for the removal, so the merger reads the reason on GitHub itself. The row names no role: the
// merger is the one role it is ever for.
//
// The row is held as an architect's notice is held, not ended as a hook prompt nobody holds is:
// it wakes a released merger and waits until the merger's session holds its role, since
// a merger whose pod was released would otherwise never learn that its pull request is out of the
// queue. While the root's design gate is closed, or while the merger is owed news of the approved
// design whose change set is not recorded yet, it neither wakes nor tells a released merger, which
// could enqueue the pull request again against the gate or before it reads what changed, and it
// waits as the merger's start waits; a merger whose session holds its role is told at once. The
// row ends undelivered when the issue has left merging at Head (its phase is not merging, it is
// out of the workflow, its tree lingers, or its pull request is not open at Head), and when the
// merger's claim is failed or retired. A merger with no claim yet is no end: the row waits for it.
type MergeQueueRemoval struct {
	Number int    `json:"number"`
	Head   string `json:"head"`
	Unread bool   `json:"unread,omitempty"`
}

func (MergeQueueRemoval) OutboxKind() OutboxKind { return OutboxKindMergeQueueRemoval }

// Message is what the runner publishes for issue to its merger, where repo is the repository the
// record holds the pull request under (PullRequest.Repo): it starts with the issue key, as a merge
// forward's message does, names the pull request as `owner/name#n` and the head, and says that
// GitHub's event names no reason, so the merger reads the reason on GitHub as step 5 of its role
// prompt says (internal/prompts/roles/merger.md). With Unread it also says that the daemon could
// not read the pull request, so the merger does not take the message to mean that GitHub did not
// merge it. The sentences are short and plain, one instruction to a sentence, because an agent
// reads them (ASD-STE100).
func (p MergeQueueRemoval) Message(issue, repo string) string {
	unread := ""
	if p.Unread {
		unread = " The daemon could not read the pull request from GitHub. So it does not know if GitHub merged it."
	}
	return fmt.Sprintf("%s: pull request %s#%d left the merge queue at head %s.%s"+
		" The event the daemon received names no reason for the removal."+
		" Read the pull request and its removals on GitHub, as step 5 of your role says."+
		" Then act on what you read.",
		issue, repo, p.Number, p.Head, unread)
}

// MergeHoldsDocument is the slug of the Dispatch document of a project that holds the project's
// merge holds (merge-holds.md). Only the project's controller writes it, one line per hold
// (skill://legion-controller, "Merge holds"). An architect reads it before every sign_off, and a
// merger reads it before it merges. Dispatch publishes each event of the document on
// notifications.dispatch.document.<project>.merge-holds-md.<type>, which the daemon's Dispatch
// consumer hears (intake.MergeHoldsChanged).
const MergeHoldsDocument = "merge-holds-md"

// MergeHoldsChangedReason is what a role is told when an event of project's merge holds document
// reaches the daemon: the holds changed, the document that holds them, and what to do. The
// architect's merge-holds-changed notice carries it as its reason, and the merger's message
// (MergeHoldsChanged.Message) carries it after the issue key, so the two roles read one text. It
// names no hold: the document is the whole hold, and the role reads the document again. Every
// role in accepting or merging is told, held or not, so the text tells each one what to do in
// both cases. The sentences are short and plain, because an agent reads them (ASD-STE100).
func MergeHoldsChangedReason(project string) string {
	return fmt.Sprintf("The merge holds of project %s changed. They are in dispatch://%s/artifact/%s."+
		" Read that document again."+
		" If a line holds your sign-off or your merge, end your turn again with a WAITING line that names the hold."+
		" If no line holds it, go on.",
		project, project, MergeHoldsDocument)
}

// MergeHoldsChanged tells the merger of an issue in merging, on the merger's own role topic, that
// the merge holds document of Project changed (MergeHoldsDocument). The fact of an event of that
// document writes it (intake.MergeHoldsChanged), and the same fact writes a merge-holds-changed
// notice to the architect of each issue in accepting. A held merger ends its turn with a WAITING
// line and subscribes to nothing, so this row is what wakes it when a hold changes, whether its pod
// was released or not. The row names no role: the merger is the one role it is ever for.
//
// The row is held as a merge-queue removal is held (MergeQueueRemoval): it wakes a released merger
// and waits until the merger's session holds its role, and while the rule that holds the merger's
// start holds it (the daemon's mergerWaitsForTheDesignGate: the root's design gate is closed, or
// the news of the approved design the merger is owed is not yet recorded) it neither wakes nor
// tells a released merger. It ends undelivered when the issue has left
// merging (its phase is not merging, it is out of the workflow, its tree lingers, or its pull
// request has merged), and when the merger's claim is failed or retired.
type MergeHoldsChanged struct {
	Project string `json:"project"`
}

func (MergeHoldsChanged) OutboxKind() OutboxKind { return OutboxKindMergeHoldsChanged }

// Message is what the runner publishes for issue to its merger: the issue key, as a merge-queue
// removal's message starts, and then the text every role is told of the change
// (MergeHoldsChangedReason).
func (p MergeHoldsChanged) Message(issue string) string {
	return issue + ": " + MergeHoldsChangedReason(p.Project)
}

// ConflictConfirm is the daemon's second read of a pull request whose head a first read found
// conflicting with its base (PullRequest.ConflictRead, dispatch://INFRA-4374 R2). The transaction
// that records the first read writes it, due a minute after that read (workflow's mergeability),
// and its executor reads the pull request from GitHub again. GitHub computes mergeability in a
// background job and can answer CONFLICTING about a base it has not caught up with, so one read
// acts on nothing: a CONFLICTING answer about the same head and base branch, a minute or more
// after the first read, confirms the conflict, and a MERGEABLE answer releases it, each through
// the mergeability fact every read of the pull request applies. An UNKNOWN answer is read again,
// a bounded number of times; when the last read still answers UNKNOWN, or no read succeeded, the
// row applies the fact that releases the first read (intake.ConflictConfirmDone). Number is the
// pull request, Head the head the first read found conflicting, and FirstReadAt when that first
// read was made (PullRequest.ConflictRead.At).
//
// A first read a MERGEABLE read released and a later CONFLICTING read of the same head made again
// is a new first read with a row of its own, so the head alone does not say which first read a row
// is the second read of: FirstReadAt does. Its executor finishes without a read once the record's
// first read was made at another time, and the fact its last attempt applies carries the time
// (intake.ConflictConfirmDone.FirstReadAt), so a row of a released first read can neither read for
// nor release the newer one. Both this payload and pull_requests.conflict_read are jsonb, where
// Go's time is kept to the nanosecond, so the two times compare equal for the same read.
//
// It is a durable row rather than a timer held in memory for the reason MergeQueueRead is: a
// restart between the two reads loses nothing, since the first read's fact is acknowledged once
// its transaction commits.
type ConflictConfirm struct {
	Number      int       `json:"number"`
	Head        string    `json:"head"`
	FirstReadAt time.Time `json:"firstReadAt"`
}

func (ConflictConfirm) OutboxKind() OutboxKind { return OutboxKindConflictConfirm }

// MergeForwardStep is which message of a merge forward a MergeForwardMessage carries
// (PullRequest.MergeForward): the two the act on a confirmed conflict writes, and the two the
// landing of a new head writes.
type MergeForwardStep string

const (
	// MergeForwardAsk asks the role the daemon asks (MergeForwardAsker), the implementer before
	// merging and the merger in merging, to merge the base forward onto the head and push.
	MergeForwardAsk MergeForwardStep = "ask"
	// MergeForwardHold tells the worker of the phase the issue stays in, when that is the
	// tester, the reviewer or the merger, to stop writing the workspace while the merge runs.
	MergeForwardHold MergeForwardStep = "hold"
	// MergeForwardLanded tells that same worker that the merge landed, and which head it left.
	MergeForwardLanded MergeForwardStep = "landed"
	// MergeForwardReview asks the reviewer to read the merge that landed on an issue whose task's
	// reviewer step already ran, and to approve the new head or request changes.
	MergeForwardReview MergeForwardStep = "review"
)

// mergeForwardRoles is the roles each step of a merge forward may go to: the ask to the
// implementer or the merger, whose merge it is (MergeForwardAsker, which only in merging names
// the merger); the hold to a worker of the phase the issue stays in that is neither the role
// asked, whom the ask tells, nor the architect, whom the checks-red notice tells; the landed to the
// worker of that phase, the merger included, which in merging made the merge and learns from it
// to merge only once the reviewer approved the new head, where its task has a reviewer step; and
// the review to the reviewer. The hold still lists the merger, which a daemon before
// cybermonklabs/legion#4979 held in merging while it asked the implementer, so a row that daemon
// queued is still read after the upgrade.
var mergeForwardRoles = map[MergeForwardStep][]claim.Role{
	MergeForwardAsk:    {claim.RoleImplementer, claim.RoleMerger},
	MergeForwardHold:   {claim.RoleTester, claim.RoleReviewer, claim.RoleMerger},
	MergeForwardLanded: {claim.RoleTester, claim.RoleReviewer, claim.RoleMerger},
	MergeForwardReview: {claim.RoleReviewer},
}

// MergeForwardReaches says whether step of a merge forward goes to role (mergeForwardRoles): the
// one list the workflow addresses the hold and the landed by (workflow's mergeForward and
// mergeForwardLanded) and a row is validated against (validateOutboxPayload), a merge-forward
// message by its step and the dropped merge forward a phase-ended message names by the ask.
func MergeForwardReaches(step MergeForwardStep, role claim.Role) bool {
	return slices.Contains(mergeForwardRoles[step], role)
}

// MergeForwardMessage is one message of a merge forward (PullRequest.MergeForward,
// dispatch://INFRA-4374 R1) to one role of the issue, on that role's own Envoy topic. The daemon
// acts on a confirmed conflict without moving the issue out of its phase: it asks the implementer,
// or in merging the merger (MergeForwardAsker, cybermonklabs/legion#4979), to merge Base forward onto
// Head in the issue workspace and push (MergeForwardAsk), and tells the worker of the phase the
// issue stays in, where that is another role, to stop writing that workspace while the merge runs
// (MergeForwardHold). Once a new head lands it tells that worker so (MergeForwardLanded), and asks
// the reviewer to read the merge where the reviewer step of the issue's task already ran
// (MergeForwardReview).
//
// Role is the recipient, and Step the message (mergeForwardRoles says which role each step goes
// to). Head is the head that conflicted, Landed the head the merge left ("" before it lands), Base
// the base branch, Phase the phase the issue stays in, PullRequest the pull request's number, and
// Why the first of R0's reasons that held (MergeForward.Why), which the ask and the hold state.
//
// It is a row of its own kind rather than a RoleMessage, because a RoleMessage ends undelivered on
// a released claim, and a released implementer must still be woken for the merge; its executor
// follows the merge-queue removal's (MergeQueueRemoval).
type MergeForwardMessage struct {
	Role        claim.Role       `json:"role"`
	Step        MergeForwardStep `json:"step"`
	Head        string           `json:"head"`
	Landed      string           `json:"landed,omitempty"`
	Base        string           `json:"base"`
	Phase       phase.Phase      `json:"phase"`
	PullRequest int              `json:"pullRequest"`
	Why         ConflictWhy      `json:"why,omitempty"`
}

func (MergeForwardMessage) OutboxKind() OutboxKind { return OutboxKindMergeForward }

// mergeForwardReason is the sentence the ask and the hold give for why, one of R0's reasons
// (ConflictWhy), of the base branch base, and "" for any other word. validateOutboxPayload refuses
// a row whose why this answers "" for, save the empty why of a landed or a review, which gives no
// reason, so no message can go out with a reason it cannot state.
func mergeForwardReason(why ConflictWhy, base string) string {
	switch why {
	case ConflictMergeNext:
		return "The next step of the issue is the merge, and GitHub cannot merge a head that conflicts with its base."
	case ConflictRulesUnread:
		return fmt.Sprintf("The daemon could not read the rules of %s, so it cannot tell if every check that %s requires passed on this head.", base, base)
	case ConflictRequiresCurrent:
		return fmt.Sprintf("A ruleset of %s requires a pull request to be current with %s before it merges.", base, base)
	case ConflictCheckNotGreen:
		return fmt.Sprintf("A check that %s requires has not passed on this head, and GitHub runs no checks on a head that conflicts with its base.", base)
	}
	return ""
}

// Message is what the runner publishes for issue to Role, where pr is the pull request as the
// record holds it when the row is delivered: the repository it is held under (PullRequest.Repo),
// and its merge forward (PullRequest.MergeForward), whose ReviewAsked says whether the landing
// asked the reviewer to read the merge. It starts with the issue key, as every message of this
// lane does, and names the pull request as `owner/name#n`. The sentences are short and plain, one
// instruction to a sentence, because an agent reads them (ASD-STE100).
//
// The ask names the pull request, the head, the base and the phase the issue stays in, gives why
// the daemon acted, and tells the role asked (the implementer, or in merging the merger) to merge
// the base forward in the issue workspace, the one workspace every role of the issue shares, as
// the worker skill's conflicts reference says, and to push the issue branch, with no handoff and
// no completion for the merge. In implementing it also says that the implementer's own
// assignment and its completion stand: the act starts no phase entry there. To the merger it also
// says that the merge is its own to resolve and that, where its task has a reviewer step, the
// reviewer is asked to approve the head it pushes (cybermonklabs/legion#4979); the ask is written
// before the landing reads the task, so it names the condition, and the landed, which comes when
// that matters, is what holds its merge to that approval (MergeForward.ReviewAsked).
//
// The hold gives why the daemon acted, and tells the worker that the implementer is asked to merge
// the head forward in the workspace they share and that the issue stays in its phase, to stop
// writing the workspace now (the merger also: not to merge the pull request or put it in the merge
// queue), not to complete its phase at the old head but at the new head the daemon names, to keep
// its session and its turn's jobs, that it is told the new head when it lands, and that a result
// it recorded at the old head stays a result of the old head.
//
// The landed names both heads and how to read what changed (readMerge): for a merge of the base,
// `git show --remerge-diff` for how the merge resolved the conflict and `git diff --stat` against
// the merge's first parent for the files the base brought in, and for any other new head `git
// diff` from the old head to the new. Then the worker goes on at the new head. To the merger in
// merging, where the landing asked the reviewer to read the merge (MergeForward.ReviewAsked), it
// also says to merge the pull request or put it in the merge queue at the new head only once the
// reviewer's approval of that head is on the pull request, and the merger's task says the same
// (workflow's task). A landing on a task with no reviewer step asks no review, so its merger is
// told to go on at the new head as every other worker is, with no approval to wait for. The review
// names both heads and the phase,
// tells the reviewer to read the merge the same way and to approve the new head by its SHA, with
// its review's Legion footer naming that head, or to request changes, with no handoff and no
// completion, and then to tell what it decided, on their role topics, to the architect and to the
// worker of the phase the issue stands in (the tester in testing, the implementer in retro, the
// merger in merging; the architect alone in accepting).
func (p MergeForwardMessage) Message(issue string, pr PullRequest) string {
	pull := fmt.Sprintf("pull request %s#%d", pr.Repo, p.PullRequest)
	switch p.Step {
	case MergeForwardAsk:
		own := ""
		switch {
		case p.Phase == phase.Implementing:
			own = " Your own assignment in implementing stands, and so does its completion."
		case p.Role == claim.RoleMerger:
			own = " The merge and its conflict are yours to resolve, as your role says." +
				" When your merge lands, the daemon asks the reviewer to read it and approve the new head, where your task has a reviewer step."
		}
		return fmt.Sprintf("%s: %s at head %s conflicts with its base branch %s, and a second read of GitHub confirmed it."+
			" The issue stays in %s. %s"+
			" Merge %s forward in the issue workspace, $LEGION_WORKSPACE. Every role of the issue shares that one workspace."+
			" Merge as skill://legion-worker/references/conflicts-and-rewrites.md says, and push the issue branch."+
			" Write no handoff and make no completion for this forward merge.%s",
			issue, pull, p.Head, p.Base, p.Phase, mergeForwardReason(p.Why, p.Base), p.Base, own)
	case MergeForwardHold:
		merger := ""
		if p.Role == claim.RoleMerger {
			merger = " Do not merge the pull request, and do not put it in the merge queue."
		}
		return fmt.Sprintf("%s: %s at head %s conflicts with its base branch %s. %s"+
			" The implementer is asked to merge %s forward in the issue workspace, $LEGION_WORKSPACE, which you share with it."+
			" The issue stays in %s. Stop writing the workspace now.%s"+
			" Do not complete your phase at head %s. Complete it at the new head that the daemon names."+
			" Keep your session and the jobs of your turn. The daemon tells you the new head when it lands."+
			" A result you recorded at head %s stays a result of head %s.",
			issue, pull, p.Head, p.Base, mergeForwardReason(p.Why, p.Base), p.Base, p.Phase, merger, p.Head, p.Head, p.Head)
	case MergeForwardLanded:
		merge := ""
		if p.Role == claim.RoleMerger && p.Phase == phase.Merging && pr.MergeForward != nil && pr.MergeForward.ReviewAsked {
			merge = fmt.Sprintf(" Merge the pull request or put it in the merge queue at head %s only once the reviewer's approval of that head is on the pull request.", p.Landed)
		}
		return fmt.Sprintf("%s: the merge of %s forward landed on %s. The old head is %s, and the new head is %s."+
			" The issue stays in %s. Read what changed since the old head: %s Then go on with your work at the new head.%s",
			issue, p.Base, pull, p.Head, p.Landed, p.Phase, p.readMerge(), merge)
	default: // MergeForwardReview, the one step left: validateOutboxPayload admits no other.
		tell := "the architect what you decided, on its role topic"
		switch p.Phase {
		case phase.Testing:
			tell = "the architect and the tester what you decided, each on its own role topic"
		case phase.Retro:
			tell = "the architect and the implementer what you decided, each on its own role topic"
		case phase.Merging:
			tell = "the architect and the merger what you decided, each on its own role topic"
		}
		return fmt.Sprintf("%s: the merge of %s forward landed on %s, and the issue stands in %s. The old head is %s, and the new head is %s."+
			" Read what the merge changed since the old head: %s"+
			" Then approve the new head %s by its SHA, with the Legion footer of your review naming that head, or request changes."+
			" Write no handoff and make no completion for this review. Then tell %s.",
			issue, p.Base, pull, p.Phase, p.Head, p.Landed, p.readMerge(), p.Landed, tell)
	}
}

// readMerge is how the landed and the review tell their role to read what a landed merge forward
// changed (Message): for a merge of the base, `git show --remerge-diff` for how the merge resolved
// the conflict, which is what the reviewer reads (cybermonklabs/legion#4979), and `git diff --stat`
// against the merge's first parent for the files the base brought in; for any other new head,
// `git diff` from the old head to the new, whose diff holds the base's own changes too.
func (p MergeForwardMessage) readMerge() string {
	return fmt.Sprintf("for a merge of the base, `git show --remerge-diff %s` shows how the merge resolved the conflict,"+
		" and `git diff --stat %s^1 %s` lists the files the base brought in; for any other new head, run `git diff %s %s`.",
		p.Landed, p.Landed, p.Landed, p.Head, p.Landed)
}

// retryBackoffDoublings is how many times a row's backoff doubles before it stops at
// RetryBackoffMost.
const retryBackoffDoublings = 6

// RetryBackoffMost is the longest a row waits to run again after a failed run: the backoff's cap.
const RetryBackoffMost = time.Second << retryBackoffDoublings

// RetryBackoff is how long a row waits to run again after a failed run, given its attempts before
// that run (the count RetryOutbox raises): one second, doubled at each attempt, at most
// RetryBackoffMost. The daemon's outbox sets a retried row's next run by it.
func RetryBackoff(attempts int) time.Duration {
	return time.Second << min(max(attempts, 0), retryBackoffDoublings)
}

// NewOutboxRow encodes one validated payload for durable delivery at nextAt.
func NewOutboxRow(issue string, payload OutboxPayload, nextAt time.Time) (OutboxRow, error) {
	if err := validateOutboxPayload(issue, payload); err != nil {
		return OutboxRow{}, err
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return OutboxRow{}, fmt.Errorf("encode outbox payload: %w", err)
	}
	return OutboxRow{Kind: payload.OutboxKind(), Issue: issue, Payload: encoded, NextAt: nextAt}, nil
}

// DecodeOutboxPayload strictly decodes a row's typed payload.
func DecodeOutboxPayload(row OutboxRow) (OutboxPayload, error) {
	payload, err := decodeOutboxJSON(row)
	if err != nil {
		return nil, err
	}
	if err := validateOutboxPayload(row.Issue, payload); err != nil {
		return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	if payload.OutboxKind() != row.Kind {
		return nil, fmt.Errorf("decode outbox row %d: kind %q does not match payload kind %q", row.ID, row.Kind, payload.OutboxKind())
	}
	return payload, nil
}

func decodeOutboxJSON(row OutboxRow) (OutboxPayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(row.Payload))
	decoder.DisallowUnknownFields()
	var payload OutboxPayload
	switch row.Kind {
	case OutboxKindDispatchStatus:
		value := StatusWrite{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindDispatchMessage:
		value := MessagePost{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindNotice:
		value := Notice{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindControllerNotice:
		value := ControllerNotice{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindSupervise:
		value := SuperviseRequest{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindGateSeed:
		value := GateSeed{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindPendingBasisSeed:
		value := PendingBasisSeed{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindReparentSeed:
		value := ReparentSeed{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindLingerClose:
		value := LingerClose{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindIssueSuspend:
		value := IssueSuspend{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindIssueBundle:
		value := IssueWorkspaceCheck{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindIssueRelease:
		value := IssueRelease{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindWorkspaceRemove:
		value := WorkspaceRemove{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindRoleMessage:
		value := RoleMessage{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindGatedStepsPublish:
		value := GatedStepsPublish{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindDesignGatesPublish:
		value := DesignGatesPublish{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindRoleHoldersPublish:
		value := RoleHoldersPublish{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindIssueBranch:
		value := IssueBranch{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindCheckWake:
		value := CheckWakeDue{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindCheckLook:
		value := CheckLook{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindCheckBound:
		value := CheckBound{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindPhaseEnded:
		value := PhaseEnded{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindHookPrompt:
		value := HookPrompt{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindUpdateAsk:
		value := UpdateAsk{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindMergeQueueRead:
		value := MergeQueueRead{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindMergeQueueRemoval:
		value := MergeQueueRemoval{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindMergeHoldsChanged:
		value := MergeHoldsChanged{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindConflictConfirm:
		value := ConflictConfirm{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindMergeForward:
		value := MergeForwardMessage{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindStepRecord:
		value := StepRecord{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindPlanRecord:
		value := PlanRecord{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindClaimRelease:
		value := ClaimRelease{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	case OutboxKindDesignChanges:
		value := DesignNewsReconcile{}
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		payload = value
	default:
		return nil, fmt.Errorf("decode outbox row %d: unknown kind %q", row.ID, row.Kind)
	}
	if row.Kind == OutboxKindDispatchMessage {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row.Payload, &fields); err != nil {
			return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
		}
		if _, ok := fields["body"]; !ok {
			return nil, fmt.Errorf("decode outbox row %d: payload has no body", row.ID)
		}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode outbox row %d: payload has trailing JSON", row.ID)
		}
		return nil, fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	return payload, nil
}

// validateOutboxPayload refuses a payload no row may carry. issue is the row's key
// (OutboxRow.Issue), which a `pause_stop` row's own Issue is read against.
func validateOutboxPayload(issue string, payload OutboxPayload) error {
	switch value := payload.(type) {
	case StatusWrite:
		// A row whose kind is not one of StatusRowKinds can never be queued, and one stored by an
		// older release is given its kind by migration 20261102003600 before this release reads
		// it: so a kind the outbox has no rule for is a kind somebody added to StatusRowKinds
		// and nowhere else, which fails here rather than reaching the outbox to be judged by
		// another kind's rule.
		if !slices.Contains(StatusRowKinds, value.Kind) {
			return fmt.Errorf("outbox status write has unknown kind %q", value.Kind)
		}
		if value.Status == "" {
			return fmt.Errorf("outbox status write has empty status")
		}
		// A reason is posted on the issue before the status lands: a done write's, and an ordinary
		// move's that nothing else on Dispatch explains (workflow's moveReason: a worker's backward
		// move). A hand-over, a re-assertion or a person's handover repeats or carries out a move
		// whose cause is already on the issue, so it posts none.
		if value.Reason != "" && value.Status != "done" && value.Kind != OrdinaryStatusRow {
			return fmt.Errorf("a %s status write to %s carries a reason; only a done write or the workflow's own move does", value.Kind, value.Status)
		}
	case MessagePost:
		// Each is a refusal Dispatch would make of the upload on every run, or a link that would
		// name no document, so the row is refused before it is queued.
		if artifact := value.Artifact; artifact != nil {
			if !artifactSlug.MatchString(artifact.Name) {
				return fmt.Errorf("a message's artifact is named %q, which is no Dispatch slug: runs of lowercase ASCII letters and digits, each two joined by one dash", artifact.Name)
			}
			if strings.TrimSpace(artifact.Content) == "" {
				return fmt.Errorf("a message's artifact %s holds no content", artifact.Name)
			}
			if len(artifact.Content) > MaxMessageArtifactBytes {
				return fmt.Errorf("a message's artifact %s holds %d bytes, more than the %d of one Dispatch document", artifact.Name, len(artifact.Content), MaxMessageArtifactBytes)
			}
		}
	case GateSeed, LingerClose, IssueBranch, CheckBound, ClaimRelease, DesignNewsReconcile:
	case PendingBasisSeed:
		if !value.Step.Valid() || value.ArtifactID == "" || value.BasisArtifactID == "" || value.BasisVersion <= 0 || value.Ask == "" || value.Generation == 0 || value.RegisteredAt.IsZero() {
			return fmt.Errorf("a pending-basis seed names the root's step and document, the ancestor's document and the version its request asks approval of, the request, the generation that registered the step and when it registered")
		}
	case ReparentSeed:
		if value.Parent == "" || value.Tree == "" || value.Ran == "" {
			return fmt.Errorf("a re-parent seed names the root's new parent, the live tree that parent is in and why the root stays a tree of its own")
		}
	case IssueSuspend:
		if value.Tree == "" || value.Generation == 0 || value.TreeGeneration == 0 {
			return fmt.Errorf("issue suspension requires its tree and both workflow generations")
		}
	case IssueWorkspaceCheck:
		if value.Tree == "" || value.Generation == 0 || value.TreeGeneration == 0 || (value.Role == "") != value.Launcher {
			return fmt.Errorf("a release's workspace check requires its tree, both workflow generations and the role whose shim reads the workspace, and names no role where the pod's launcher reads it")
		}
		if value.Launcher && len(value.CloseRoles) > 0 {
			return fmt.Errorf("a workspace check the pod's launcher reads stops no role, so it names no close roles")
		}
		if value.Close && !value.Launcher {
			return fmt.Errorf("a close's workspace check that stops no role is read by the pod's launcher")
		}
	case IssueRelease:
		if value.Tree == "" || value.Generation == 0 || value.TreeGeneration == 0 {
			return fmt.Errorf("an issue release requires its tree and both workflow generations")
		}
	case WorkspaceRemove:
		if value.Linger == 0 {
			return fmt.Errorf("workspace removal requires the root generation of its linger")
		}
	case Notice:
		if !validNoticeKind(value.Kind) {
			return fmt.Errorf("unknown notice kind %q", value.Kind)
		}
		if (value.Kind == "catch-up") != (value.CatchUp != nil) {
			return fmt.Errorf("a %s notice carries a catch-up only when it is one", value.Kind)
		}
		if _, spec := SpecDecision(value.Kind); !spec && !basisNoticeKind(value.Kind) && (value.Actor != "" || value.Ref != "") {
			return fmt.Errorf("a %s notice carries an actor or a ref; only a spec decision's does, and a basis notice's", value.Kind)
		}
		if value.Decision != "" || value.Root != "" {
			return fmt.Errorf("a %s notice carries a decision or a root; only a parked-decision controller notice does", value.Kind)
		}
		if catchUp := value.CatchUp; catchUp != nil && catchUp.Kept != nil {
			if err := catchUp.Kept.validate(); err != nil {
				return err
			}
		}
		if value.Update != nil {
			return fmt.Errorf("a %s notice carries an update round; only the controller's update-stalled notice does", value.Kind)
		}
		if value.Pause != nil {
			return fmt.Errorf("a %s notice carries a project pause; only the controller's project-paused and project-resumed notices do", value.Kind)
		}
		if (value.Kind == VolumeNearFullNotice) != (value.Volume != nil) {
			return fmt.Errorf("a %s notice carries a volume sweep only when it is one", value.Kind)
		}
		if volume := value.Volume; volume != nil && (volume.PodUID == "" || volume.Trimmer == "" || volume.Episode <= 0) {
			return fmt.Errorf("a volume-near-full notice requires the pod it is of, the trimmer that swept it and the episode it was in")
		}
		if err := validateNoticeFailure(value.Kind, value.Failure); err != nil {
			return err
		}
		if err := validateNoticeEscalation(value.Kind, value.Reason, value.Escalation); err != nil {
			return err
		}
		if err := validateNoticeGiveUps(value.Kind, value.Role, value.Count, value.GiveUps); err != nil {
			return err
		}
		if err := validateNoticeStopper(value.Kind, value.StoppedBy); err != nil {
			return err
		}
	case ControllerNotice:
		if !controllerOnlyNoticeKind(value.Kind) && !validNoticeKind(value.Kind) {
			return fmt.Errorf("unknown controller notice kind %q", value.Kind)
		}
		if value.Kind == ParkedDecisionNotice {
			if !validDecisionKind(value.Decision) || value.Root == "" || value.Ref == "" || !PersonActor(value.Actor) {
				return fmt.Errorf("a parked-decision notice needs a known decision, its tree's root, the decision's Dispatch reference and the person who made it")
			}
		} else if value.Decision != "" || value.Actor != "" || value.Ref != "" || value.Root != "" {
			return fmt.Errorf("a %s controller notice carries a decision, an actor, a ref or a root; only a parked-decision does", value.Kind)
		}
		if (value.Kind == UpdateStalledNotice) != (value.Update != nil) {
			return fmt.Errorf("a %s notice carries an update round only when it is one", value.Kind)
		}
		if update := value.Update; update != nil && (update.Round == "" || len(update.Waiting) == 0) {
			return fmt.Errorf("an update-stalled notice requires its round and the claims it waits on")
		}
		if value.Kind != TickNotice && len(value.Held) > 0 {
			return fmt.Errorf("a %s notice carries held issues only when it is a tick", value.Kind)
		}
		pauseKind := value.Kind == ProjectPausedNotice || value.Kind == ProjectResumedNotice
		if pauseKind != (value.Pause != nil) {
			return fmt.Errorf("a %s notice carries a project pause only when it is a project-paused or a project-resumed notice", value.Kind)
		}
		if pause := value.Pause; pause != nil {
			if value.Reason != "" {
				return fmt.Errorf("a %s notice carries its reason in its pause, not beside it", value.Kind)
			}
			if err := pause.validate(value.Kind); err != nil {
				return err
			}
		}
		if value.Volume != nil {
			return fmt.Errorf("a %s controller notice carries a volume sweep; a filling issue pod's volume is its own architect's to read", value.Kind)
		}
		if err := validateNoticeFailure(value.Kind, value.Failure); err != nil {
			return err
		}
		if err := validateNoticeEscalation(value.Kind, value.Reason, value.Escalation); err != nil {
			return err
		}
		if err := validateNoticeStopper(value.Kind, value.StoppedBy); err != nil {
			return err
		}
		if value.Kind == ArchitectStalledNotice && value.Reason == "" {
			return fmt.Errorf("an architect-stalled notice carries the reason that names its arm, the root and what the record says")
		}
		if err := validateNoticeGiveUps(value.Kind, value.Role, value.Count, value.GiveUps); err != nil {
			return err
		}
	case SuperviseRequest:
		if !validSuperviseOp(value.Op) {
			return fmt.Errorf("unknown supervise operation %q", value.Op)
		}
		if value.Op == "start" && value.Tree == "" {
			return fmt.Errorf("supervise start requires tree")
		}
		if value.Op == "start" && value.Role == "" {
			return fmt.Errorf("supervise start requires role")
		}
		if (value.Op == "tree_close") != (value.Linger != 0) {
			return fmt.Errorf("a tree close, and only a tree close, names the root generation of its linger")
		}
		if value.Op == "retire_task" && (value.Role == "" || value.Phase == "" || value.Task != "" || value.NeedsSlot) {
			return fmt.Errorf("a supervise retire_task names the role and the phase whose task it retires, and carries no task")
		}
		if value.ResumeTask && (value.Op != "start" || value.Task == "" || value.Phase == "") {
			return fmt.Errorf("a supervise resume task requires a start with a task and a phase")
		}
		if value.ResendEntry && (value.Op != "start" || value.Task == "" || value.Phase == "") {
			return fmt.Errorf("a supervise start that resends the entry its role's row names requires a start with a task and a phase")
		}
		if value.Decided && (value.Op != "start" || value.News || value.CheckLook != 0) {
			return fmt.Errorf("a supervise request someone decided is a start, and neither a start for news alone nor a production check's look")
		}
		if value.NeedsSlot && (value.Op != "start" || value.Task == "" || value.Phase == "") {
			return fmt.Errorf("a supervise start held for an admission slot requires a start with a task and a phase")
		}
		if value.Relaunch && (value.Op != "start" || value.Task != "" || value.Phase == "" || value.NeedsSlot) {
			return fmt.Errorf("the relaunch half of a start carries a phase and no task, and waits for no slot")
		}
		if value.CheckLook != 0 && (value.CheckLook < 0 || value.Op != "start" || !worksCheck(value.Role) || value.Phase != "") {
			return fmt.Errorf("a supervise check look names a start of the implementer or the tester with no phase, and only a start carries one")
		}
		if value.News && (value.Op != "start" || value.Task != "" || value.Entry != 0 || value.RuleResume || value.CheckLook != 0 ||
			(value.Phase == "") != (value.Role == claim.RoleArchitect)) {
			return fmt.Errorf("a supervise start for news alone carries no task and no entry, and names the phase of the holder it tells, or none for an architect")
		}
		if value.Kept != nil {
			if value.Op != "start" || value.Task == "" {
				return fmt.Errorf("only a supervise start with a task names the decisions it kept")
			}
			if err := value.Kept.validate(); err != nil {
				return err
			}
		}
		if value.Op == "pause_stop" && (value.Pause <= 0 || !claim.IsRole(value.Role)) {
			return fmt.Errorf("a pause's stop names its pause and the role it stops; it names pause %d and role %q", value.Pause, value.Role)
		}
		if value.Issue != "" && (value.Op != "pause_stop" || !claim.IsIssueKey(value.Issue) || claim.IsIssueKey(issue)) {
			return fmt.Errorf("a supervise %s names the issue %q on a row keyed by %q; only a pause's stop keyed by its project names its claim's issue, and that is an issue key",
				value.Op, value.Issue, issue)
		}
		if value.Op == "pause_stop" && value.Issue == "" && !claim.IsIssueKey(issue) {
			return fmt.Errorf("a pause's stop keyed by %q, which is no issue key, names the issue of the claim it stops", issue)
		}
		if value.Pause < 0 || (value.Pause != 0 && value.Op != "pause_stop" && value.Op != "start") {
			return fmt.Errorf("a supervise %s names pause %d; only a pause's stop and a start name one", value.Op, value.Pause)
		}
		if value.WakeKind != "" && (value.Op != "start" || !validWakeKind(value.WakeKind)) {
			return fmt.Errorf("a supervise %s names the wake kind %q; only a start names one, and it is one of the pods wake kinds", value.Op, value.WakeKind)
		}
		if value.WakeKind == pods.WakePause && value.Pause == 0 {
			return fmt.Errorf("a start for a %s wake names the pause that owed it", value.WakeKind)
		}
		if value.TurnStart != nil && value.Op != "suspend" {
			return fmt.Errorf("a supervise %s names a turn start; only a left pod's close's suspend names one", value.Op)
		}
	case RoleMessage:
		if value.Role == "" {
			return fmt.Errorf("role message requires role")
		}
		if value.Body == "" {
			return fmt.Errorf("role message requires body")
		}
	case GatedStepsPublish:
		if value.Project == "" {
			return fmt.Errorf("a gated-steps publish requires the project whose row it writes")
		}
		for _, step := range value.Steps {
			if !step.Valid() {
				return fmt.Errorf("a gated-steps publish names %q, which is no design-gate step", step)
			}
		}
	// Dispatch refuses each of these as 400 INVALID_ISSUE, which the executor's rule retries
	// rather than ends, so a row carrying one would be sent for as long as the issue lives:
	// the enqueue is where such a set stops.
	case DesignGatesPublish:
		if value.Issue == "" {
			return fmt.Errorf("a design-gates publish requires the issue whose gates it writes")
		}
		named := make(map[DesignStep]bool, len(value.Gates))
		for _, gate := range value.Gates {
			if !gate.Step.Valid() {
				return fmt.Errorf("a design-gates publish names %q, which is no design-gate step", gate.Step)
			}
			if gate.ArtifactID == "" {
				return fmt.Errorf("a design-gates publish names no document for the %s step", gate.Step)
			}
			if named[gate.Step] {
				return fmt.Errorf("a design-gates publish names the %s step twice", gate.Step)
			}
			named[gate.Step] = true
		}
	case RoleHoldersPublish:
		if value.Issue == "" {
			return fmt.Errorf("a role-holders publish requires the issue whose role sessions it writes")
		}
	case CheckWakeDue:
		if value.Look <= 0 {
			return fmt.Errorf("check wake requires the look it was named at")
		}
		if value.Reason == "" {
			return fmt.Errorf("check wake requires the reason it wakes the check")
		}
	case CheckLook:
		if value.Look <= 0 {
			return fmt.Errorf("check look bound requires the look it bounds")
		}
	case PhaseEnded:
		if !claim.IsRole(value.Role) || value.Role == claim.RoleArchitect {
			return fmt.Errorf("a phase-ended message goes to the worker whose phase ended, never to an architect, which its awaiting-children notice tells; %q is neither", value.Role)
		}
		if value.From == "" || value.From == phase.AwaitingChildren || value.To != phase.AwaitingChildren {
			return fmt.Errorf("a phase-ended message names the phase that ended and awaiting_children, the phase the parent waits in; it names %q and %q", value.From, value.To)
		}
		if value.Reason == "" {
			return fmt.Errorf("a phase-ended message carries the open children that hold the parent")
		}
		if dropped := value.MergeForward; dropped != nil && (dropped.Head == "" || dropped.Base == "") {
			return fmt.Errorf("a phase-ended message that names a dropped merge forward names its head and its base; it names head %q and base %q", dropped.Head, dropped.Base)
		}
		// The role is one a merge forward's ask goes to (MergeForwardReaches on MergeForwardAsk),
		// the role MergeForwardAsker names, which awaitChildren writes on every dropped merge
		// forward (DroppedMergeForward.Role): a row with another role, or none, would address its
		// message to no role that was asked.
		if dropped := value.MergeForward; dropped != nil && !MergeForwardReaches(MergeForwardAsk, dropped.Role) {
			return fmt.Errorf("a phase-ended message that names a dropped merge forward names the role it was asked of, the implementer or the merger; it names %q", dropped.Role)
		}
	case HookPrompt:
		if value.Hook == "" {
			return fmt.Errorf("a hook prompt carries the name of the hook that fired it")
		}
		if !claim.IsRole(value.Role) || value.Role == claim.RoleArchitect {
			return fmt.Errorf("a hook prompt goes to a phase worker of the issue, never to an architect, whose own wake the daemon writes as a notice; %q is neither", value.Role)
		}
		// A move back is one by the order of the issue's task (workflow's Walk.MovesBack), which need
		// not be today's, so the pair is any of two different workflow phases (phase.MovePairs).
		if !phase.IsMovePair(value.From, value.To) {
			return fmt.Errorf("a hook prompt names the move back that fired it; %q to %q is no move back", value.From, value.To)
		}
		if value.Count <= 0 {
			return fmt.Errorf("a hook prompt counts the move that fired it, so its count is one or more; it is %d", value.Count)
		}
		if value.Prompt == "" {
			return fmt.Errorf("a hook prompt carries the hook's own words, which the daemon puts in front of the role")
		}
	case UpdateAsk:
		if !claim.IsRole(value.Role) {
			return fmt.Errorf("an update ask goes to a role whose process runs in the issue's pod, the architect among them; %q is no role", value.Role)
		}
		if value.Round == "" {
			return fmt.Errorf("an update ask names the round it asks for")
		}
		if value.Text == "" {
			return fmt.Errorf("an update ask carries the text the sweep built for its round")
		}
	case MergeQueueRead:
		if value.Number < 1 || value.Head == "" {
			return fmt.Errorf("a merge-queue read names the pull request it reads and the head that left the queue; it names #%d at %q", value.Number, value.Head)
		}
	case MergeQueueRemoval:
		if value.Number < 1 || value.Head == "" {
			return fmt.Errorf("a merge-queue removal names the pull request that left the queue and the head it left at; it names #%d at %q", value.Number, value.Head)
		}
	case MergeHoldsChanged:
		if value.Project == "" {
			return fmt.Errorf("a merge-holds change names the project whose merge holds document changed")
		}
	case ConflictConfirm:
		if value.Number < 1 || value.Head == "" {
			return fmt.Errorf("a conflict confirm names the pull request it reads again and the head the first read found conflicting; it names #%d at %q", value.Number, value.Head)
		}
		if value.FirstReadAt.IsZero() {
			return fmt.Errorf("a conflict confirm names when the first read it reads again was made; its read of #%d at %q names no time", value.Number, value.Head)
		}
	case MergeForwardMessage:
		if _, known := mergeForwardRoles[value.Step]; !known {
			return fmt.Errorf("a merge-forward message is an ask, a hold, a landed or a review; %q is none of them", value.Step)
		}
		if !MergeForwardReaches(value.Step, value.Role) {
			return fmt.Errorf("a merge-forward ask goes to the implementer or the merger, a hold or a landed to the tester, the reviewer or the merger, and a review to the reviewer; a %s to %q is none of these", value.Step, value.Role)
		}
		if value.Head == "" || value.Base == "" || value.Phase == "" || value.PullRequest < 1 {
			return fmt.Errorf("a merge-forward message names the head, the base branch, the phase the issue stays in and the pull request; it names %q, %q, %q and #%d", value.Head, value.Base, value.Phase, value.PullRequest)
		}
		// The merger is asked only where MergeForwardAsker names it, in merging. The implementer's ask
		// is valid in every phase, so a row a daemon before cybermonklabs/legion#4979 queued past the
		// review still reads.
		if value.Step == MergeForwardAsk && value.Role == claim.RoleMerger && MergeForwardAsker(value.Phase) != claim.RoleMerger {
			return fmt.Errorf("a merge-forward ask goes to the merger only while the issue stands in merging; this one names %q", value.Phase)
		}
		landing := value.Step == MergeForwardLanded || value.Step == MergeForwardReview
		if landing != (value.Landed != "") {
			return fmt.Errorf("a merge-forward landed or review names the head that landed, and an ask or a hold names none; a %s names %q", value.Step, value.Landed)
		}
		if mergeForwardReason(value.Why, value.Base) == "" && (value.Why != "" || !landing) {
			return fmt.Errorf("a merge-forward ask or hold names why the daemon acted, as one of R0's reasons, and a landed or a review names one of them or none; a %s names %q", value.Step, value.Why)
		}
	// The rest of what Dispatch takes is Dispatch's to judge, and a value it refuses ends the row
	// with a warning (the outbox's permanentRefusal). These two it would not: an empty id
	// addresses the route's parent path, which Dispatch's router answers NOT_FOUND "no route for
	// …", the one answer the row waits on for ever; and a write without the step's identity is
	// refused only when it is the record's first, so the same defect would make a record on one
	// run and none on another.
	// A rating is held whole here too, though Dispatch's rating route would refuse half of one: the
	// workflow (rateRetro) writes the rating and its reason as one value, so a rating of 0 or an
	// empty reason, which is what a stored row naming only one of the two reads as, is a defect of
	// the daemon, refused where it is queued rather than found by a warning after the completion
	// that carried it committed. The experiment and the variant of the record's issue are held
	// whole for the same reason: they are one value, which a row naming one of them alone breaks.
	case StepRecord:
		if _, err := uuid.Parse(value.ID); err != nil {
			return fmt.Errorf("a step record names no uuid as its id; it names %q", value.ID)
		}
		if value.Task == "" || value.Kind == "" || value.Role == "" {
			return fmt.Errorf("a step record carries its task, kind and role on every write; it carries %q, %q and %q", value.Task, value.Kind, value.Role)
		}
		// The words and the place rule are Dispatch's own (steprecord, from contracts): one it
		// refuses is a 400 STEP_INPUT that ends the row, and the run would have no record.
		if !steprecord.PlaceHolds(value.Kind, value.Step) {
			return fmt.Errorf("a step record's step %d is no place of a run of kind %q", value.Step, value.Kind)
		}
		if value.Result != nil && !slices.Contains([]string{steprecord.ResultPassed, steprecord.ResultFailed, steprecord.ResultStopped}, *value.Result) {
			return fmt.Errorf("a step record's result %q is no result of a run", *value.Result)
		}
		if value.PlanRating != nil && (value.PlanRating.Rating == 0 || value.PlanRating.Reason == "") {
			return fmt.Errorf("a step record carries the rating of the plan with its reason or neither; it carries the rating %d with the reason %q",
				value.PlanRating.Rating, value.PlanRating.Reason)
		}
		if value.StepExperiment != nil && (value.StepExperiment.Experiment == "" || value.StepExperiment.Variant == "") {
			return fmt.Errorf("a step record carries the experiment of its issue with its variant or neither; it carries the experiment %q with the variant %q",
				value.StepExperiment.Experiment, value.StepExperiment.Variant)
		}
	// The rest of a plan Dispatch judges, as it judges a step record's; its id is held here for the
	// same reason a step record's is.
	case PlanRecord:
		if _, err := uuid.Parse(value.ID); err != nil {
			return fmt.Errorf("a plan record names no uuid as its id; it names %q", value.ID)
		}
	default:
		return fmt.Errorf("unknown outbox payload %T", payload)
	}
	return nil
}

// validNoticeKinds is every notice kind the daemon publishes, which is also every kind a row may
// decode as (DecodeOutboxPayload) and every kind the validation accepts: a kind reaches the
// outbox only by being on this list, since validNoticeKind decides from it alone and NewOutboxRow
// refuses a notice of any other kind. It is a list rather than a switch so a test can range over
// it, and a kind added here turns up as a row that test is missing rather than as a kind nobody
// checked. `ready-refused` is not one of them: READY went with the daemon's merge, so nothing
// writes the kind, and migration 20261029120800 deletes every row an older daemon left carrying
// it, in the same migration that drops the four merge kinds from this list.
//
// `worker-stalled` is what the daemon's stall backstop did to a worker whose agent stopped
// stepping, with the task it was serving still owed (internal/daemon's backstopStalledTurns),
// which is a fact of that worker's issue and so the issue's architect's to read, as
// `worker-died` is. What it did is usually the turn ended and the session brought back; where
// the wake pacing parked the relaunch, it is the task taken back with that launch queued and
// the stalled process still running until the launch's turn, which the notice names. The notice
// says what became of that stalled process as well, where the end let it go: that it was
// stopped, or that the runtime did not confirm the stop and it may be running still
// (internal/daemon's stallBackstopEnds, stallStopClause).
//
// `worker-silent` is the worker that stopped between turns rather than inside one: its process
// is up, it holds no task, and its agent has not stepped for longer than the bound the
// deployment sets (`worker_silence_minutes`), so nothing in the daemon and nothing in that
// process will prompt it (workflow's SilentPhaseWorker, workerSilent). It is that worker's
// issue's architect's to read, as `worker-stalled` is, and it says what the daemon wrote about
// it, by the phase entry the role's phases row names: the start row that sends that entry's own
// work to a claim behind it, or the phase's task again to a claim level with it (workflow's
// repairSilence, workerSilentDid).
//
// `worker-background-stalled` is the worker the silence repair must leave alone: its own
// session reports background work of its own running, which no bound can tell from a job that
// hung, and that report has stood past `worker_background_hours` (workflow's
// workerBackgroundStalled). It is that worker's issue's architect's to read, and it is a telling
// and not a repair: it names the kinds the session reported and how long the report has stood,
// and says that nothing was sent and nothing will be sent for it.
//
// `merge-holds-changed` is an event of the project's merge holds document (MergeHoldsDocument):
// the controller added, changed or deleted a hold. The daemon tells the architect of each issue in
// accepting, because a line of the document can hold that architect's sign_off, and the architect
// that a hold stopped waits with a WAITING line and subscribes to nothing (workflow's
// mergeHoldsChanged). Its reason names the document and says to read it again
// (MergeHoldsChangedReason). The merger of each issue in merging is told the same text by a row of
// its own (MergeHoldsChanged), since a notice reaches only an architect.
//
// `provider-errors` is a claim whose turns have ended six times in a row in an error the model's
// provider returned (supervise's tellOfProviderErrors). It is told to an architect and to the
// controller, the controller's copy in a row of its own (ProviderErrorsNotice); internal/daemon's
// providerErrorRunReached says which architect, and for which run none (NoticeStartsAtParent).
//
// The three basis notices (basisNoticeKinds) are of a root's design-gate step on an ancestor's
// approval (dispatch://INFRA-3320): `basis-ended` tells a root's architect that its step's wait on
// the ancestor's approval request has ended, and what ended it; `basis-changes-requested` tells a
// root's architect, and the ancestor's, of a person's changes request on the ancestor's version
// an open step rests on; and `resting-root` tells the ancestor's architect that a root's step
// rests on it.
//
// `awaiting-children` tells a parent's architect that the parent entered awaiting_children in place
// of a phase start, because a Dispatch child of it is open (workflow's awaitChildren,
// dispatch://INFRA-3320 R10): the start it holds, each open child and what ends the wait. It
// replaces the phase-finished notice that start would have sent, and carries that notice's summary
// and verdict.
//
// `task-stopped` is a task stopped where it stands, its issue held: by its owner's stop_task
// (workflow's taskStopped), whose notice names the architect that made the call (StoppedBy) and
// reaches the issue's architect only when that is another claim (the daemon's deliverNotice);
// or by a failed step that no step before it of the type implementation can take the work back
// from (workflow's stopUnreturned). The task's owner reads which task, and which step or which
// architect, in its reason.
//
// `retro-held` is a parent whose task's last step is not the owner's acceptance, held before its
// retro because it passed its last step, or moved back out of merging, while a hold on its
// acceptance holds it: issues beneath it still in the workflow, roots that rest on its approval
// still in the workflow, or its open Dispatch children (workflow's retroHeld, retroHeldNotice;
// HoldForChildren). Its reason names the move that entered the hold, each issue of the first of
// them that holds it and what starts the retro: none of them holding it any longer (workflow's
// StartHeldRetro). It is told once for each of the three holds and each run of the retro
// (workflow's tellRetroHeld).
//
// `plan-not-taken` is a plan its issue's first step did not take, the first step telling the
// issue's architect once, whatever number of versions it passed over, after it deleted their
// rows; it carries one of two meanings. A plan an earlier owner gave (workflow's
// passOverEarlierOwners, tellPassedOver): its giver is no architect of the issue's tree now
// (ArchitectOfTree), as after an orphan is admitted again as a root of its own or an adoption
// moves a member to another tree; the reason names each version once, its document and the
// givers of its copies, the plan the first step runs (an owner's, of the issue's generation or
// carried to it, or the default steps), and that the architect may give the plan again with
// register_plan as its own.
// Or a plan of an issue whose experiment's group takes no plan from its owner (workflow's
// passOverOwners, tellNotTaken): every plan its owner or an earlier owner gave is passed over, the
// run takes the project's default steps, and the reason names each version and the group
// (intake's GroupClause), and offers no register_plan, which that issue's owner is answered
// PLAN_NOT_TAKEN for.
//
// `session-forgotten` is the worker whose claim forgot its recorded session because the pod's
// launcher, older than the release that added the kind, could not look the session up by its id,
// and Oh My Pi did not resume it by that id (workflow's sessionLost). It is a kind of its own and
// not `session-lost` because the kind is published as the notice's one-line message, and that
// session was not lost: nothing of it was deleted. No earlier release decodes it, so the way back
// of migration 20261120042902 deletes its unsent rows.
//
// `workspace-kept` (WorkspaceKeptNotice) is a pod no role runs in that the daemon keeps for its
// disk, or a pod an update round kept for its disk; `workspace-taken` (WorkspaceTakenNotice) is a
// replacement that still took a disk that may hold work.
var validNoticeKinds = []NoticeKind{
	"phase-finished", "worker-died", "worker-stalled", "worker-silent", "worker-background-stalled", "held",
	"pr-blocked", "pr-merged", "pr-closed-unmerged", "pull-request-missing", "message-refused",
	"design-approved", "design-changes-requested", "carried-gate-opened",
	"basis-ended", "basis-changes-requested", "resting-root",
	"child-closed", "child-status", "children-first", "awaiting-children", "child-release-stalled", "child-not-recorded",
	"catch-up", "checks-red", "review-stuck", "status-reasserted", VolumeNearFullNotice,
	"session-lost", "session-forgotten", "session-store-fault", "merge-holds-changed", ProviderErrorsNotice,
	"task-stopped", "plan-not-taken", "retro-held",
	WorkspaceKeptNotice, WorkspaceTakenNotice,
}

// VolumeNearFullNotice is the architect's notice of a sweep of its issue pod's volume that left
// that volume still above its low mark, or of the trimmer having switched itself off: the one
// word anybody in Legion gets that the disk the roles of an issue share is filling under them
// (INFRA-2009). It is the architect's and not the controller's because the work that filled the
// volume is the issue's — a repository cache, a build tree, a test run's artefacts — and the
// architect is who can stop or redirect it.
const VolumeNearFullNotice NoticeKind = "volume-near-full"

// WorkspaceKeptNotice is the architect's notice that the daemon keeps a pod in which no role
// process runs, because the pod's launcher read its workspace and found work that no branch or tag
// on origin holds, or could not read it (R6 of #5285, internal/daemon's
// keptMessage): the release of a pod no claim owns and the close of a left pod with no live claim
// delete a disk only after a read shows it clean. It names the state of each claim of the issue
// and what the read found, and it is sent once for each pod kept so (store's recordDecline). It is
// the architect's because no live role of the issue is there to push the work, and the architect
// is who can bring one back. Where the owning architect will not hold its role again, the notice
// reaches the project's controller instead, as a ControllerNotice of the same kind and words
// (internal/daemon's tellControllerOfKeptPod), since the pod counts as told once its notice is
// queued. No earlier release decodes it, so a rollback deletes its unsent rows, the controller's
// among them, first (docs/kubernetes.md, "Rolling back the release that keeps no saved workspace").
//
// An update round over a drift the pod's roles can keep working under that reads work in the pod's
// workspace, or cannot read it, keeps the pod too (R7 of #5285, internal/daemon's
// keepPodForRound): where the open phase's worker has no live process in the pod, its architect is
// told by this notice, once for the pod, with each claim's state and what the read found.
const WorkspaceKeptNotice NoticeKind = "workspace-kept"

// WorkspaceTakenNotice is the architect's notice that a replacement of its issue's pod took a disk
// that may hold work no branch or tag on origin holds (R7 of #5285): an update
// round over a drift that breaks the pod (runtime.Breaks), or a launch's own
// replacement of an issue pod (runtime.TakenPod), whose read of the workspace before the
// replacement found such work or could not be made. It names each commit and the working-copy
// change, or that no read could be made and why, and it is sent once for each replacement. It is
// the architect's because the work is gone with the pod and the architect is who can have it made
// again. Where the owning architect will not hold its role again, it reaches the project's
// controller instead, as WorkspaceKeptNotice does. No earlier release decodes it, so a rollback
// deletes its unsent rows with the workspace-kept ones (docs/kubernetes.md, "Rolling back the
// release that keeps no saved workspace").
const WorkspaceTakenNotice NoticeKind = "workspace-taken"

// validNoticeKind says whether kind is a notice an architect is told.
func validNoticeKind(kind NoticeKind) bool {
	return slices.Contains(validNoticeKinds, kind)
}

// basisNoticeKinds are the notices of a step on an ancestor's approval, which may carry the
// Dispatch reference of the document they are about and the person who acted on it (Notice.Ref,
// Notice.Actor) as a spec decision's notice does, and which are no spec decision: SpecDecision
// names none of them, so the backlog relay passes none of them on (a root back from the backlog
// starts a new generation with no gate, and its architect reads the ancestor again when it
// registers).
var basisNoticeKinds = []NoticeKind{"basis-ended", "basis-changes-requested", "resting-root"}

// basisNoticeKind says whether kind is one of basisNoticeKinds.
func basisNoticeKind(kind NoticeKind) bool {
	return slices.Contains(basisNoticeKinds, kind)
}

// validateNoticeFailure holds a notice's failure record to the two kinds that report a claim's
// failure: the hold a phase worker's failed claim puts its issue in and the worker-died that
// names the death (workflow's claimFailed). Both arms of the switch above call it, because a
// controller notice of either kind is a converted copy of the architect's (workflow's
// noticeWithController), and a rule written in one arm alone would take the copy of a notice the
// other refused.
//
// A notice of either kind carrying no record is valid and stays valid: the escalation's held
// notice and the re-admission's report a hold rather than a failure (workflow's retryOrEscalate,
// HeldNotice), a failure the daemon recorded no cause for names none, and a record Published
// never drops is a record no re-hold of these kinds can be refused for want of.
func validateNoticeFailure(kind NoticeKind, failure *NoticeFailure) error {
	if failure == nil {
		return nil
	}
	if kind != "held" && kind != "worker-died" {
		return fmt.Errorf("a %s notice names a claim failure; only a held or a worker-died notice does", kind)
	}
	if failure.Cause == "" {
		return fmt.Errorf("a %s notice naming a claim failure names the cause it failed under", kind)
	}
	return nil
}

// validateNoticeEscalation holds the architect's own words for an escalation to the one notice
// that carries them: the held notice of that escalation, which names it in reason (workflow's
// retryOrEscalate). Both arms of the switch above call it, as they call validateNoticeFailure
// and for the same reason — the controller reads a converted copy of the architect's notice.
//
// The rule is positive rather than a list of the kinds that may not carry it: an escalation
// that named no reason carries none, and every other notice carries none at all.
func validateNoticeEscalation(kind NoticeKind, reason, escalation string) error {
	if escalation == "" {
		return nil
	}
	if kind != "held" || reason != string(HoldEscalated) {
		return fmt.Errorf("a %s notice with reason %q carries the architect's words for an escalation; only a held notice of an escalation does", kind, reason)
	}
	return nil
}

// validateNoticeGiveUps holds a notice's count and named give-ups to the intake-gave-up notice,
// which counts one give-up or more, names between one and min(Count, giveUpNoticeNames) of them,
// each by its record's positive id, and carries no role, since it is the controller's alone.
// Every other kind carries neither. Both arms of the switch above call it, as they call
// validateNoticeFailure: a Notice can never be of this kind, so there it refuses any count.
func validateNoticeGiveUps(kind NoticeKind, role claim.Role, count int, giveUps []NoticeGiveUp) error {
	if kind != IntakeGaveUpNotice {
		if count != 0 || len(giveUps) > 0 {
			return fmt.Errorf("a %s notice carries a count or names give-ups; only an intake-gave-up notice does", kind)
		}
		return nil
	}
	if count < 1 {
		return fmt.Errorf("an intake-gave-up notice counts one give-up or more, not %d", count)
	}
	if most := min(count, giveUpNoticeNames); len(giveUps) < 1 || len(giveUps) > most {
		return fmt.Errorf("an intake-gave-up notice that counts %d names between 1 and %d of them, not %d", count, most, len(giveUps))
	}
	for _, giveUp := range giveUps {
		if giveUp.ID <= 0 {
			return fmt.Errorf("an intake-gave-up notice names each give-up by its positive record id, not %d", giveUp.ID)
		}
	}
	if role != "" {
		return fmt.Errorf("an intake-gave-up notice carries no role, not %q: it is the controller's alone", role)
	}
	return nil
}

// validateNoticeStopper holds the claim of the architect that stopped a task to the one notice
// that carries it: the task-stopped notice of its owner's stop_task (workflow's taskStopped),
// which the daemon's executor publishes only to an architect that is not that claim. Both arms
// of the switch above call it, as they call validateNoticeFailure and for the same reason.
func validateNoticeStopper(kind NoticeKind, stoppedBy claim.Token) error {
	if stoppedBy != "" && kind != "task-stopped" {
		return fmt.Errorf("a %s notice names the architect that stopped its task; only a task-stopped notice does", kind)
	}
	return nil
}

// controllerOnlyNoticeKind says whether kind is a notice for the controller alone, never an
// architect's.
func controllerOnlyNoticeKind(kind NoticeKind) bool {
	switch kind {
	case TriageNotice, SlotFreeNotice, TodoNotice, TickNotice, ParkedDecisionNotice, UpdateStalledNotice, TreeStalledNotice,
		ArchitectStalledNotice, MemoryGuardKillNotice, ProjectPausedNotice, ProjectResumedNotice, IntakeGaveUpNotice:
		return true
	default:
		return false
	}
}

func validSuperviseOp(op SuperviseOp) bool {
	switch op {
	case "start", "suspend", "release", "step_stop", "tree_close", "issue_close", "retire_task", "pause_stop":
		return true
	default:
		return false
	}
}

// validWakeKind says whether kind is one of the pods wake kinds a resume records, the one
// vocabulary a start's WakeKind is held to.
func validWakeKind(kind string) bool {
	switch kind {
	case pods.WakeDelivery, pods.WakeChecks, pods.WakeRoleMessage, pods.WakeAgentMessage, pods.WakeBackstop, pods.WakePause:
		return true
	default:
		return false
	}
}

// worksCheck says whether role may work a production check's look: the two values a check's
// record can hold, the migration's default (implementer) and the one value the project's
// `production_check` setting takes (tester). An architect's look is refused.
func worksCheck(role claim.Role) bool {
	return role == claim.RoleImplementer || role == claim.RoleTester
}
