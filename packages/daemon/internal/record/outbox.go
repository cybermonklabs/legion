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
	OutboxKindHookPrompt       OutboxKind = "hook_prompt"
	OutboxKindUpdateAsk        OutboxKind = "update_ask"
	OutboxKindIssueBundle      OutboxKind = "issue_bundle"
	OutboxKindIssueRelease     OutboxKind = "issue_release"
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
	// OutboxKindRoleHoldersPublish publishes the live role sessions of one issue onto its Dispatch
	// row (RoleHoldersPublish).
	OutboxKindRoleHoldersPublish OutboxKind = "role_holders_publish"
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

// OutboxLane is which executor of an issue runs a kind. An issue's rows run on five lanes side by
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
// none of them back. The role-message lane runs the kinds the daemon addresses to one role of
// the issue on that role's own topic: a hook's words, which go to a phase worker (hook_prompt),
// the message that tells the merger its pull request left the merge queue (merge_queue_removal),
// the messages of a merge forward, which go to the implementer and to the worker of the phase the
// issue stays in (merge_forward), and an update round's ask, which goes to any role whose process
// runs in the issue's pod, its architect included (update_ask). It also runs the two reads of
// GitHub that come before a message: the read before a merge-queue removal's message
// (merge_queue_read), and the second read of a head a first read found conflicting, whose
// confirmation is what writes a merge forward's messages (conflict_confirm). It is a lane of its
// own because the board lane's architect notices are ordered among themselves and one of them
// waits for as long as its architect holds no role (the daemon's errNoticeWaits): a role given the
// words a hook fired on a move back, told that its pull request left the merge queue, asked to
// merge the base forward or to hold while that merge runs, or asked to get ready for the
// replacement of its pod must hear it while that is news, not after whatever the architect that
// owns its issue is owed.
// The claim lane runs the release of an issue's Dispatch claim (claim_release) on a lane of its
// own, because that row may wait up to an hour on a holder that has not stopped, and an hour of
// waiting on one issue's claim must delay no status write and no notice of that same issue, which
// is what a place on the board lane would cost it.
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
	LaneRecord      OutboxLane = "record"
)

// laneKinds is each lane's kinds in the order LaneKinds lists them. Every OutboxKind is in exactly
// one lane (TestEveryOutboxKindIsInExactlyOneLane).
var laneKinds = map[OutboxLane][]OutboxKind{
	LaneLifecycle:   {OutboxKindSupervise, OutboxKindIssueSuspend, OutboxKindIssueBundle, OutboxKindIssueRelease, OutboxKindLingerClose, OutboxKindWorkspaceRemove, OutboxKindGateSeed, OutboxKindIssueBranch, OutboxKindCheckWake, OutboxKindCheckLook, OutboxKindCheckBound},
	LaneBoard:       {OutboxKindDispatchStatus, OutboxKindDispatchMessage, OutboxKindNotice, OutboxKindControllerNotice, OutboxKindRoleMessage, OutboxKindGatedStepsPublish, OutboxKindDesignGatesPublish, OutboxKindRoleHoldersPublish},
	LaneRoleMessage: {OutboxKindHookPrompt, OutboxKindUpdateAsk, OutboxKindMergeQueueRead, OutboxKindMergeQueueRemoval, OutboxKindConflictConfirm, OutboxKindMergeForward},
	LaneClaim:       {OutboxKindClaimRelease},
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
	// in, the copy of that hold the controller reads, and the worker-died that names the death
	// (workflow's claimFailed). It is absent where the daemon recorded no cause for the failure,
	// and absent from the two held notices that report no failure at all: the escalation's
	// (workflow's retryOrEscalate) and the re-admission's (workflow's HeldNotice).
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
	// Resends counts the times the notice was queued again after the listener could not forward
	// it to its architect's session (the daemon's rehold), which stops at a cap.
	Resends int `json:"resends,omitempty"`
	// PacedAt is the moment the daemon's wake pacing reserved for a catch-up it holds to its turn,
	// and the zero value on every other notice: the catch-up the outbox writes in place of a
	// launch when a start row finds an architect already running (the outbox's start,
	// workflow.Config.Reserve). Reading that catch-up is a wake of the architect, and what the
	// organization's per-minute cap counts is the context it reads, so the row waits its turn like
	// a launch does.
	//
	// The moment is kept here rather than read back from the row's next_at because the two part
	// company: a failed attempt rewrites next_at to its backoff (RetryOutbox), while the turn the
	// pacing gave this wake never moves. The outbox writes that wake's one line from it, on
	// whichever attempt publishes (the daemon's wakeRan), and a ready that releases the
	// architect's waiting notices leaves the row where it stands only while the moment is still
	// ahead (the daemon's releaseWaiting), so a catch-up whose turn has come is released like any
	// other notice rather than waiting out a backoff the pacing never asked for. It is the
	// daemon's own bookkeeping, so the publish drops it (Published) as it drops ResendOf; a
	// re-held copy keeps it for that one line alone, its next_at being the re-send delay rather
	// than a reservation (the daemon's noticeAnswered).
	PacedAt time.Time `json:"paced_at,omitzero"`
	// PacedFrom is the moment the reservation above was made, on the clock that made it, and the
	// two travel together: PacedAt less PacedFrom is how long the pacing held this wake, which is
	// what the line it writes reports (the daemon's wakeRan).
	//
	// The wait is read from this pair rather than from PacedAt less the row's created_at, which
	// is two clocks subtracted from each other: created_at is the database's now() at the start
	// of the fact's transaction (store/migrations/0004_record.up.sql), while the reservation is
	// made from the engine's clock later in that same transaction (workflow's claimReady). A
	// catch-up the burst let straight through — PacedAt equal to PacedFrom, the first wake of a
	// wave — would otherwise report the transaction's own duration as a wait, and under clock
	// skew a wait the other way about.
	PacedFrom time.Time `json:"paced_from,omitzero"`
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
	// gates.design off opened the gate.
	Actor string `json:"actor,omitempty"`
	// Ref is that decision's Dispatch reference: the document of the design-gate step a design
	// decision is of (workflow's stepRef), `dispatch://<KEY>/spec` for the spec step,
	// `dispatch://<KEY>/ask/<id>` for an answer or a resolution, `dispatch://<KEY>/comment/<id>`
	// for a reply.
	Ref string `json:"ref,omitempty"`
	// Decision is what a parked-decision notice relays, and Root the root of the tree it was made
	// on. Every other kind carries neither.
	Decision DecisionKind `json:"decision,omitempty"`
	Root     string       `json:"root,omitempty"`
	// Update is an update-stalled notice's account of the round that is not finishing, and is
	// absent from every other kind. Only an UpdateStalledNotice carries one.
	Update *UpdateStalled `json:"update,omitempty"`
	// Volume is what a volume-near-full notice carries: one sweep of the issue pod's volume as
	// the cache trimmer in that pod's launcher reported it. Only that kind carries one.
	Volume *VolumeNearFull `json:"volume,omitempty"`
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
// opened over an issue's pod, and why something holds it past its bound. Round is the round's
// identity (supervise.Claim.UpdateRound), Pod the pod it was opened over, and Reasons the drift
// the round was opened for, as the ask states it. Waiting is every claim the round asked, each
// marked with whether it answered, so the controller can ask the operator to act on a suspended
// claim rather than read the whole claims list to find out who the round asked; what
// each entry says is UpdateWaiting's. The notice's issue is the outbox row's own.
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

// UpdateWaiting is one claim an update round asked: its token, the role it holds, the state it
// is in (supervise.ClaimState — an operator-suspended claim waits for the operator; a pod round
// lists no `failed` claim, since it asks none), and Answered for a claim that has already
// answered this round.
//
// The contract the claim's plugin records is not here. A plugin with no ready_for_update op is
// asked like any other, on its role's own topic (UpdateAsk), and the round ends at its bound
// whether or not it answers, so such a claim is one the round is waiting on like any other, and
// the controller has nothing of its own to do about it.
//
// Answered tells the controller what each role said. It is no list of roles to prompt: the round
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
// A catch-up carries one entry for each step the root must hold open before its tree works
// (classify.RequiredSteps), so a deployment that gates nothing carries the one spec entry with
// Gated false: that registration opens the step at once and asks no person, which is what the
// policy word `off` said. The list replaced that word, because one word cannot name a list and
// each entry now says its own step's part of it.
type CatchUpGate struct {
	Step         DesignStep `json:"step"`
	Gated        bool       `json:"gated"`
	Unregistered bool       `json:"unregistered,omitempty"`
	Artifact     string     `json:"artifact,omitempty"`
	Version      int        `json:"version,omitempty"`
	Approved     int        `json:"approved,omitempty"`
	Open         bool       `json:"open"`
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
// and puts nothing back, and only the receipt-timeout path restores the two paced moments by
// hand (the daemon's noticeAnswered). Both then validate the copy before queueing it
// (reholdNotice, through NewOutboxRow), so a field validateOutboxPayload requires for its kind,
// as it requires CatchUp of a catch-up notice, is stripped here only together with a restore of
// its own there, or every re-hold of that kind is refused for want of it
// (record's TestAPublishedNoticeIsStillAValidOutboxPayload).
func (n Notice) Published(id int64) (Notice, string) {
	n.PacedAt, n.PacedFrom = time.Time{}, time.Time{}
	if n.ResendOf == 0 {
		return n, OutboxKey(id)
	}
	copied := n.ResendOf
	n.ResendOf = 0
	return n, OutboxKey(copied)
}

// ControllerNotice is a Notice for the project's controller topic (notify.ControllerTopic) alone,
// in an outbox row of its own, so its publish retries apart from the architect's notice row. It is
// published as the Notice it is.
type ControllerNotice Notice

func (ControllerNotice) OutboxKind() OutboxKind { return OutboxKindControllerNotice }

// The controller notices no architect is told of. A triage notice's root is unrecorded, so no
// architect owns it; a slot-free notice names the issue whose slot admission released while no
// waiting issue took it. A todo notice names an issue not handed to Legion that changed in `todo`
// while a slot stood free. An awaiting-person notice names a root that waits on a person and
// keeps the slot it holds while it waits (workflow's enterAwaiting). The controller takes no issue for any of
// the three (skill://legion-controller, "Free slots"). A tick is the daemon's periodic wake, whose
// issue is the project key. An unslotted notice names a root admission found in the workflow with
// neither a slot nor a place in the waiting line, which it then put in phase.Done; its reason
// names the status the root stood at. An update-stalled notice names an issue whose update round
// has passed its bound (`pod_update_grace_minutes` after it opened) while something still holds
// it: an operator's suspension of a role the round asks, a ReplacePod the runtime refused, no
// claim recording a process in the round's pod (or in any pod), or claims recording two pods. It
// is told once per round (the daemon's reconcilePodUpdates). A
// provider-errors notice names an issue whose role has ended six turns in a row in a provider
// error: its Role is that role, and its Reason the model and the HTTP status the provider
// answered, where it answered one, in words built from structured fields alone (supervise's
// tellControllerOfProviderErrors). It is told once, and not again until a turn with no error puts
// that run back to zero; nothing is held and nothing is bounded, because a chain's bottom
// refusing deterministically costs one request every ten minutes. A tree-stalled notice names a
// root of the daemon's own whose tree has stopped: nothing of the tree has moved for longer than
// `stalled_tree_notice_hours`, no claim of it is at work, and it does not wait by design (admit's
// tellStalledTrees says which trees are told). Its issue is that root, and its Reason names the
// tree's quiet hours, each issue the workflow still runs with its phase, the role whose turn it
// is and the state of its pull request, and what the controller is to decide. It is told once
// for each stall: the daemon marks the root with the movement the notice was told for, and tells
// again only once the tree moves past that mark. When admission sends each is its rule
// (admit.Admission.wakeController and its callers, admit's releaseUnslotted,
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
// An architect-stalled notice names a root whose own architect has stalled, in one of the three
// readings ArchitectStallArm names below. Its Role is the architect, and its
// Reason names the arm, the root and what the record says. One root raises one notice per arm,
// per run of its tree (Store.TellArchitectsStalled, admit's architectStalls), so a root that
// two arms read raises two and neither hides the other. Nobody inside the tree is told: the
// architect is the role that would be, and each arm is a reading that it did not take, cannot be
// reached, or has not moved (dispatch://INFRA-2614).
const (
	TriageNotice           NoticeKind = "triage"
	SlotFreeNotice         NoticeKind = "slot-free"
	TodoNotice             NoticeKind = "todo"
	TickNotice             NoticeKind = "tick"
	UnslottedNotice        NoticeKind = "unslotted"
	AwaitingPersonNotice   NoticeKind = "awaiting-person"
	ParkedDecisionNotice   NoticeKind = "parked-decision"
	UpdateStalledNotice    NoticeKind = "update-stalled"
	ProviderErrorsNotice   NoticeKind = "provider-errors"
	TreeStalledNotice      NoticeKind = "tree-stalled"
	ArchitectStalledNotice NoticeKind = "architect-stalled"
)

// ArchitectStallArm names which reading raised an architect-stalled notice. It is part of the
// mark that keeps one wake to one root, one run and one arm (ArchitectStall,
// Store.TellArchitectsStalled), and the first words of the notice's reason, so a controller that
// reads two wakes of one root reads two different facts:
//
//   - ArchitectTookNothing: the task of the entry the architect's phases row names was first
//     queued to its claim more than workflow.StrandedReleaseWake ago and no turn of that claim
//     confirmed it. It is counted from the first queueing of that entry, which the claim's
//     handshake keeps (supervise.Claim.Handshake), however many times the daemon queued that
//     entry's task again. A task a let-go of the claim's process took back from a turn is counted
//     only while the claim is ready, working or idle, from the later of that first queueing and
//     the relaunched agent's ready. The daemon writes no second start of that entry while its task
//     stands (workflow.OwedEntry), so the wake is the one thing left. A task the root's own record
//     has superseded — one that carries an entry, of a run before its generation or of an entry
//     the row has moved past — is no such task: the sweep writes the start of the row's entry and
//     that start's task takes its place (workflow.ArchitectStartPending,
//     supervise.Delivery.SupersededBy). Nor is a task of no entry, such as a wake's: no start takes
//     its place, and the sweep holds the entry's start back until a turn takes it.
//   - ArchitectClaimUnreachable: the architect's claim is failed or retired, or the root has no
//     architect claim at all and the work of its entry has stood that same bound, and in either
//     case no start of the root's run is the daemon's next move for it, which
//     workflow.ArchitectRelaunchOf decides. A root waiting on a person is read as any root is,
//     since the daemon queues the next start of its architect only when that wait is answered.
//   - ArchitectSlowBeforeGate: the root stands at phase.Admitted with no design-gate step
//     registered 90 minutes after it was admitted, or after its architect's entry was last
//     raised, whichever is later.
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
// It is told at exactly the sixth turn in a row and never again until a turn with no error puts
// that run back to zero, so a second claim's notice dropped behind the first claim's would
// never be sent at all. Two claims of one issue are two such claims — an issue's architect and
// its worker fail their turns in the same outage — so the issue alone is not the key; the role
// is the rest of it.
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
// Every other kind says something about the project — a free slot, a todo issue, the day's tick
// — which one wake carries however many facts find it pending.
func NoticePerClaim(kind NoticeKind) bool {
	return kind == ProviderErrorsNotice || kind == TreeStalledNotice
}

// SuperviseOp identifies a worker-session operation: "start" starts, resumes, or retries the role's
// claim; "suspend" stops its process and keeps its session; "release" stops its process and keeps
// its session too, as one role stop of an idle pod's release, and is its own op rather than a
// second "suspend" because the open-issue invariant reads the op to say which act it is reading —
// a suspend stops a role of an issue the workflow is taking out, a release stops a role of an
// issue that stays in the workflow and comes back at the next wake; "tree_close" is the tree's
// close — the request that ends the claim, the tree's root claim included; "issue_close" is a
// child issue's close as done, which ends the claim as a tree close does and keeps its session, so
// the issue's next run, should a person set it todo again, resumes each claim's session rather
// than starting fresh (supervise's RequestIssueClose).
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
	// check's look (workflow's startLook), the pod wake (workflow's wakeRow) and the resume of a
	// cancelled idle release (the daemon's outbox). Each hands its role work it was already on, or
	// work no phase entry numbers at all.
	Entry int64 `json:"entry,omitempty"`
	// ResendEntry marks a start that hands the role the work of the entry its phases row already
	// names rather than a new entry, so the claim's given entry does not decline the task
	// (supervise.Claim.GivenEntry, the queue's entry test). The held phase's retry, by any route
	// into it, is that start (workflow.Engine.restart): it raises no entry, and a claim whose
	// worker confirmed that entry's last task and whose turn then ended holds it as its given one
	// with no delivery pending, so the entry test alone would drop the retry's own task and
	// relaunch the worker with no prompt. The outbox row-id guard still holds
	// (supervise.Claim.DeliveredRows), so a retry of the row itself gives that task no second
	// time.
	ResendEntry bool `json:"resendEntry,omitempty"`
	// ResumeTask marks Task as the phase's resume task (workflow.ResumePhaseTask), the one a
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
	// mark, since an issue past its review holds no row for such a start to wait for, and it is
	// claimable the moment it commits. workflow.StartOrOwe is the
	// one writer of the mark, which it reads from the start's own phase, so no caller decides
	// it. The hold is on this row's claimability alone (store.go's outboxInOrder): a row that is
	// merely unclaimable takes no lease, so none of its issue's later rows is held behind it.
	// The row carries the worker's task whether or not it is marked, since a start is one row
	// either way; what the mark decides is when the worker comes back with it.
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

// LingerClose records the issue generation whose linger should close.
type LingerClose struct {
	Generation uint64 `json:"generation"`
}

func (LingerClose) OutboxKind() OutboxKind { return OutboxKindLingerClose }

// IssueSuspend stops an issue's pod after its close has stopped every role, and deletes the
// Sandbox the roles ran in (runtime.IssueSuspender). Both workflow generations and this row's ID
// fence a later re-admission or start. The issue keeps nothing: its disk is its pod's own and
// goes with the pod, so a later todo re-enters the issue fresh on a Sandbox its next launch
// makes.
type IssueSuspend struct {
	Tree           string `json:"tree"`
	Generation     uint64 `json:"generation"`
	TreeGeneration uint64 `json:"treeGeneration"`
}

func (IssueSuspend) OutboxKind() OutboxKind { return OutboxKindIssueSuspend }

// IssueBundle is the first act of an idle pod's release: it asks the shim of Role — a role whose
// process is still running in the pod, because this row runs before the `release` rows that stop
// them — for the git bundle of the unpushed work of the workspace every role of the issue shares,
// and saves it in the daemon's own database (runtime.WorkspaceBundler). Only a shim can build it:
// it runs the `jj` of the workspace it already works in, and it dies with its role's process, so
// the save has to happen while that process is there.
//
// It stops nothing, so a release cancelled here costs only the save, and it sits before the
// release's boundary (the first `release` row), so a delivery queued while it runs still cancels
// the whole release and takes the stored bundle with it (outbox.cancelRelease).
type IssueBundle struct {
	Tree           string     `json:"tree"`
	Generation     uint64     `json:"generation"`
	TreeGeneration uint64     `json:"treeGeneration"`
	Role           claim.Role `json:"role"`
}

func (IssueBundle) OutboxKind() OutboxKind { return OutboxKindIssueBundle }

// IssueRelease is the last act of an idle pod's release, queued behind the save (IssueBundle) and
// the per-role `release` supervise rows that stopped the pod's role processes: it suspends the
// issue's Sandbox, waits its pod out and deletes the Sandbox with the role Secrets it owns
// (runtime.IssueSuspender), and records the release with the size of the bundle the save stored.
// Both workflow generations and this row's ID fence a start or a re-admission that came after, as
// an IssueSuspend's do.
//
// Unlike a close, the issue stays in the workflow: the claims keep their phase and their pending
// task, and the next wake for one of the roles brings a new pod that restores the bundle before
// any role starts.
type IssueRelease struct {
	Tree           string `json:"tree"`
	Generation     uint64 `json:"generation"`
	TreeGeneration uint64 `json:"treeGeneration"`
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
// beside either, so the pod would come back for a message the row had already dropped.
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

// DesignGateDocument is one step of a publication and the document that step's gate recorded.
type DesignGateDocument struct {
	Step       DesignStep `json:"step"`
	ArtifactID string     `json:"artifactId"`
}

func (DesignGatesPublish) OutboxKind() OutboxKind { return OutboxKindDesignGatesPublish }

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

func (StepRecord) OutboxKind() OutboxKind { return OutboxKindStepRecord }

// PlanRecord is the plan one run of an issue carries out, written onto Dispatch (PUT
// /api/v1/issues/{key}/plans/{id}) when the run's work starts. ID is the plan's id, a uuid of the
// issue and its run, and the row's issue column is the issue. Each other field is a field of the
// route's body under the same name: the run (record.Issue.Generation), where the plan came from
// (default, the steps Legion runs when the owner gave none, or owner), the owner's document with
// its version, which a plan of the source default leaves out, and each task with its steps in
// order. Dispatch sets each field once, so a plan sent again changes nothing.
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

// PlanRecordTask is one task of a plan: its id and its steps in order, each a role or `role:key`.
type PlanRecordTask struct {
	ID    string   `json:"id"`
	Steps []string `json:"steps"`
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
// (ProductionCheck.WakeAt), or the retry of a look nobody reported. There is at most one such row
// per check: it is enqueued once, at the end of a look, and consumed when it fires, and the one
// deferral that queues another — workflow's checkWakeDue, for an issue back in the workflow —
// queues it in the transaction that finishes the row it came from. Nothing else wakes a check: no
// boot, no build and no deploy event reads production any more.
//
// Look is the look the check stood at when the wake was named, so a row left over from an earlier
// look starts nothing; OpenedAt is the check instance it was named for
// (ProductionCheck.OpenedAt), since a later merge of the same issue replaces the check and counts
// its looks from one again — zero names no instance and is not fenced. Reason is what the wake
// is, for the look's wake and the log: the agent's own wake, or the retry of a look nobody
// reported.
type CheckWakeDue struct {
	Look     int       `json:"look"`
	OpenedAt time.Time `json:"openedAt,omitzero"`
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
// The counts are listed in the order phase.BackwardPairs() gives rather than the map's own, so
// two readings of one row read alike; a pair the map does not hold is left out. An empty map
// reads `Counts by move: none.` rather than dropping the sentence: the firing counted the move
// that fired it, so the map cannot normally be empty, and a sentence that says `none` tells a
// reader of a row written any other way that the daemon counted nothing, where a missing sentence
// would leave it to guess.
func (p HookPrompt) Message(issue string) string {
	listed := make([]string, 0, len(p.Counts))
	for _, pair := range phase.BackwardPairs() {
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
// longer records finishes without a publish (the daemon's updateAsk). The daemon marks a claim
// asked only once its row's publish reached a session, so an ask that finished undelivered is
// lost, not given, and the daemon's sweep writes the claim a new row of this kind at a later tick.
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
// queue. While the root's design gate is closed it neither wakes nor tells a released merger, which
// could enqueue the pull request again against the gate, and it waits for the gate to open; a
// merger whose session holds its role is told at once. The row ends undelivered when the issue has
// left merging at Head (its phase is not merging, it is out of the workflow, its tree lingers, or
// its pull request is not open at Head), and when the merger's claim is failed or retired. A merger
// with no claim yet is no end: the row waits for it.
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
	// MergeForwardAsk asks the implementer to merge the base forward onto the head and push.
	MergeForwardAsk MergeForwardStep = "ask"
	// MergeForwardHold tells the worker of the phase the issue stays in, when that is the
	// tester, the reviewer or the merger, to stop writing the workspace while the merge runs.
	MergeForwardHold MergeForwardStep = "hold"
	// MergeForwardLanded tells that same worker that the merge landed, and which head it left.
	MergeForwardLanded MergeForwardStep = "landed"
	// MergeForwardReview asks the reviewer to read the merge that landed on an issue standing
	// past its review, and to approve the new head or request changes.
	MergeForwardReview MergeForwardStep = "review"
)

// mergeForwardRoles is the roles each step of a merge forward may go to: the ask to the
// implementer, whose merge it is; the hold and the landed to a worker of the phase the issue
// stays in that is neither the implementer, whom the ask tells, nor the architect, whom the
// checks-red notice tells; and the review to the reviewer.
var mergeForwardRoles = map[MergeForwardStep][]claim.Role{
	MergeForwardAsk:    {claim.RoleImplementer},
	MergeForwardHold:   {claim.RoleTester, claim.RoleReviewer, claim.RoleMerger},
	MergeForwardLanded: {claim.RoleTester, claim.RoleReviewer, claim.RoleMerger},
	MergeForwardReview: {claim.RoleReviewer},
}

// MergeForwardReaches says whether step of a merge forward goes to role (mergeForwardRoles): the
// one list the workflow addresses the hold and the landed by (workflow's mergeForward and
// mergeForwardLanded) and a row is validated against (validateOutboxPayload).
func MergeForwardReaches(step MergeForwardStep, role claim.Role) bool {
	return slices.Contains(mergeForwardRoles[step], role)
}

// MergeForwardMessage is one message of a merge forward (PullRequest.MergeForward,
// dispatch://INFRA-4374 R1) to one role of the issue, on that role's own Envoy topic. The daemon
// acts on a confirmed conflict without moving the issue out of its phase: it asks the implementer
// to merge Base forward onto Head in the issue workspace and push (MergeForwardAsk), and tells the
// worker of the phase the issue stays in to stop writing that workspace while the merge runs
// (MergeForwardHold). Once a new head lands it tells that worker so (MergeForwardLanded), and asks
// the reviewer of an issue past its review to read the merge (MergeForwardReview).
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

// Message is what the runner publishes for issue to Role, where repo is the repository the record
// holds the pull request under (PullRequest.Repo). It starts with the issue key, as every message
// of this lane does, and names the pull request as `owner/name#n`. The sentences are short and
// plain, one instruction to a sentence, because an agent reads them (ASD-STE100).
//
// The ask names the pull request, the head, the base and the phase the issue stays in, gives why
// the daemon acted, and tells the implementer to merge the base forward in the issue workspace,
// the one workspace every role of the issue shares, as the worker skill's conflicts reference
// says, after the working-copy check of its foreign-path reference, and to push the issue branch,
// with no handoff and no completion for the merge. In implementing it also says that the
// implementer's own assignment and its completion stand: the act starts no phase entry there.
//
// The hold gives why the daemon acted, and tells the worker that the implementer is asked to merge
// the head forward in the workspace they share and that the issue stays in its phase, to stop
// writing the workspace now (the merger also: not to merge the pull request or put it in the merge
// queue), not to complete its phase at the old head but at the new head the daemon names, to keep
// its session and its turn's jobs, that it is told the new head when it lands, and that a result
// it recorded at the old head stays a result of the old head.
//
// The landed names both heads and how to read what changed: `git diff` from the old head to the
// new, and, for a merge of the base, `git show --remerge-diff` for how the merge resolved the
// conflict and `git diff --stat` against the merge's first parent for the files the base brought
// in. Then the worker goes on at the new head. To the merger in merging it also says to merge the
// pull request or put it in the merge queue at the new head only once the reviewer's approval of
// that head is on the pull request: in merging the landing asks the reviewer for that review, and
// the merger's task says the same (workflow's task). The review names both heads and the phase,
// tells the reviewer to read the merge the same way and to approve the new head by its SHA, with
// its review's Legion footer naming that head, or to request changes, with no handoff and no
// completion, and then to tell what it decided, on their role topics, to the architect and to the
// worker of the phase the issue stands in (the implementer in retro, the merger in merging; the
// architect alone in accepting).
func (p MergeForwardMessage) Message(issue, repo string) string {
	pull := fmt.Sprintf("pull request %s#%d", repo, p.PullRequest)
	switch p.Step {
	case MergeForwardAsk:
		own := ""
		if p.Phase == phase.Implementing {
			own = " Your own assignment in implementing stands, and so does its completion."
		}
		return fmt.Sprintf("%s: %s at head %s conflicts with its base branch %s, and a second read of GitHub confirmed it."+
			" The issue stays in %s. %s"+
			" Merge %s forward in the issue workspace, $LEGION_WORKSPACE. Every role of the issue shares that one workspace."+
			" First do the working-copy check of skill://legion-worker/references/foreign-path.md."+
			" Then merge as skill://legion-worker/references/conflicts-and-rewrites.md says, and push the issue branch."+
			" Write no handoff and make no completion for this merge.%s",
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
		if p.Role == claim.RoleMerger && p.Phase == phase.Merging {
			merge = fmt.Sprintf(" Merge the pull request or put it in the merge queue at head %s only once the reviewer's approval of that head is on the pull request.", p.Landed)
		}
		return fmt.Sprintf("%s: the merge of %s forward landed on %s. The old head is %s, and the new head is %s."+
			" The issue stays in %s. Read what changed since the old head: %s Then go on with your work at the new head.%s",
			issue, p.Base, pull, p.Head, p.Landed, p.Phase, p.readMerge(), merge)
	default: // MergeForwardReview, the one step left: validateOutboxPayload admits no other.
		tell := "the architect, on its role topic"
		switch p.Phase {
		case phase.Retro:
			tell = "the architect and the implementer, each on its own role topic"
		case phase.Merging:
			tell = "the architect and the merger, each on its own role topic"
		}
		return fmt.Sprintf("%s: the merge of %s forward landed on %s, and the issue stands in %s. The old head is %s, and the new head is %s."+
			" Read what the merge changed since the old head: %s"+
			" Then approve the new head %s by its SHA, with the Legion footer of your review naming that head, or request changes."+
			" Write no handoff and make no completion for this review. Then tell %s what you decided.",
			issue, p.Base, pull, p.Phase, p.Head, p.Landed, p.readMerge(), p.Landed, tell)
	}
}

// readMerge is how the landed and the review tell their role to read what a landed merge forward
// changed (Message): `git diff` from the old head to the new, and, for a merge of the base,
// `git show --remerge-diff` and `git diff --stat` against the merge's first parent.
func (p MergeForwardMessage) readMerge() string {
	return fmt.Sprintf("run `git diff %s %s`."+
		" For a merge of the base, `git show --remerge-diff %s` shows how the merge resolved the conflict,"+
		" and `git diff --stat %s^1 %s` lists the files the base brought in.",
		p.Head, p.Landed, p.Landed, p.Landed, p.Landed)
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
	if err := validateOutboxPayload(payload); err != nil {
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
	if err := validateOutboxPayload(payload); err != nil {
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
		value := IssueBundle{}
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

func validateOutboxPayload(payload OutboxPayload) error {
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
	case GateSeed, LingerClose, IssueBranch, CheckBound, ClaimRelease:
	case IssueSuspend:
		if value.Tree == "" || value.Generation == 0 || value.TreeGeneration == 0 {
			return fmt.Errorf("issue suspension requires its tree and both workflow generations")
		}
	case IssueBundle:
		if value.Tree == "" || value.Generation == 0 || value.TreeGeneration == 0 || value.Role == "" {
			return fmt.Errorf("a release's workspace save requires its tree, both workflow generations and the role whose shim is asked")
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
		if _, spec := SpecDecision(value.Kind); !spec && (value.Actor != "" || value.Ref != "") {
			return fmt.Errorf("a %s notice carries an actor or a ref; only a spec decision's does", value.Kind)
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
		if value.Volume != nil {
			return fmt.Errorf("a %s controller notice carries a volume sweep; a filling issue pod's volume is its own architect's to read", value.Kind)
		}
		if err := validateNoticeFailure(value.Kind, value.Failure); err != nil {
			return err
		}
		if err := validateNoticeEscalation(value.Kind, value.Reason, value.Escalation); err != nil {
			return err
		}
		if value.Kind == ArchitectStalledNotice && value.Reason == "" {
			return fmt.Errorf("an architect-stalled notice carries the reason that names its arm, the root and what the record says")
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
		if value.ResumeTask && (value.Op != "start" || value.Task == "" || value.Phase == "") {
			return fmt.Errorf("a supervise resume task requires a start with a task and a phase")
		}
		if value.ResendEntry && (value.Op != "start" || value.Task == "" || value.Phase == "") {
			return fmt.Errorf("a supervise start that resends the entry its role's row names requires a start with a task and a phase")
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
		if value.Kept != nil {
			if value.Op != "start" || value.Task == "" {
				return fmt.Errorf("only a supervise start with a task names the decisions it kept")
			}
			if err := value.Kept.validate(); err != nil {
				return err
			}
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
	case HookPrompt:
		if value.Hook == "" {
			return fmt.Errorf("a hook prompt carries the name of the hook that fired it")
		}
		if !claim.IsRole(value.Role) || value.Role == claim.RoleArchitect {
			return fmt.Errorf("a hook prompt goes to a phase worker of the issue, never to an architect, whose own wake the daemon writes as a notice; %q is neither", value.Role)
		}
		if !phase.MovesBack(value.From, value.To) {
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
			return fmt.Errorf("a merge-forward ask goes to the implementer, a hold or a landed to the tester, the reviewer or the merger, and a review to the reviewer; a %s to %q is none of these", value.Step, value.Role)
		}
		if value.Head == "" || value.Base == "" || value.Phase == "" || value.PullRequest < 1 {
			return fmt.Errorf("a merge-forward message names the head, the base branch, the phase the issue stays in and the pull request; it names %q, %q, %q and #%d", value.Head, value.Base, value.Phase, value.PullRequest)
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
	// that carried it committed.
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
// `session-forgotten` is the worker whose claim forgot its recorded session because the pod's
// launcher, older than the release that added the kind, could not look the session up by its id,
// and Oh My Pi did not resume it by that id (workflow's sessionLost). It is a kind of its own and
// not `session-lost` because the kind is published as the notice's one-line message, and that
// session was not lost: nothing of it was deleted. No earlier release decodes it, so the way back
// of migration 20261120042902 deletes its unsent rows.
var validNoticeKinds = []NoticeKind{
	"phase-finished", "worker-died", "worker-stalled", "worker-silent", "worker-background-stalled", "held",
	"pr-blocked", "pr-merged", "pr-closed-unmerged", "pull-request-missing", "message-refused",
	"design-approved", "design-changes-requested", "carried-gate-opened",
	"child-closed", "child-status", "children-first", "child-release-stalled", "child-not-recorded",
	"catch-up", "checks-red", "review-stuck", "status-reasserted", VolumeNearFullNotice,
	"session-lost", "session-forgotten", "session-store-fault",
}

// VolumeNearFullNotice is the architect's notice of a sweep of its issue pod's volume that left
// that volume still above its low mark, or of the trimmer having switched itself off: the one
// word anybody in Legion gets that the disk the roles of an issue share is filling under them
// (INFRA-2009). It is the architect's and not the controller's because the work that filled the
// volume is the issue's — a repository cache, a build tree, a test run's artefacts — and the
// architect is who can stop or redirect it.
const VolumeNearFullNotice NoticeKind = "volume-near-full"

// validNoticeKind says whether kind is a notice an architect is told.
func validNoticeKind(kind NoticeKind) bool {
	return slices.Contains(validNoticeKinds, kind)
}

// validateNoticeFailure holds a notice's failure record to the two kinds that report a claim's
// failure: the hold a phase worker's failed claim puts its issue in and the worker-died that
// names the death (workflow's claimFailed). Both arms of the switch above call it, because the
// held notice reaches the controller as a converted copy of the architect's (workflow's
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

// controllerOnlyNoticeKind says whether kind is a notice for the controller alone, never an
// architect's.
func controllerOnlyNoticeKind(kind NoticeKind) bool {
	switch kind {
	case TriageNotice, SlotFreeNotice, TodoNotice, TickNotice, UnslottedNotice, AwaitingPersonNotice, ParkedDecisionNotice,
		UpdateStalledNotice, ProviderErrorsNotice, TreeStalledNotice, ArchitectStalledNotice:
		return true
	default:
		return false
	}
}

func validSuperviseOp(op SuperviseOp) bool {
	switch op {
	case "start", "suspend", "release", "tree_close", "issue_close":
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
