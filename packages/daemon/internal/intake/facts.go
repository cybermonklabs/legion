// Package intake turns durable Envoy notifications and Go API observations into one transactional fact path.
package intake

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/steprecord"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Fact is a closed vocabulary of observations that can change Legion's workflow record. Each fact
// type says, by its one unexported method, which issue it names: one issue's key and true, or false
// for a fact that names none or several (a pull request's by its repository and number, a push, a
// tree's adoption of its children, the controller's tick, the daemon's own sweeps). So a fact type
// cannot be added without that answer (IssueOf), and the method keeps arbitrary external values out
// of the transaction path.
type Fact interface{ namedIssue() (string, bool) }

// DispatchIssue records Dispatch's complete issue observation. Rank is Dispatch's fractional key,
// compared as bytes — the order rank.Between generates. HandedOver is resolved once at decode
// (decodeDispatchFact), from the event's own raw labels, and every decision acts on this bool;
// the raw list rides on the fact as well (Labels), for the copy of Dispatch's issues, which is
// its one reader (blockers.Mirror).
type DispatchIssue struct {
	Key    string
	Seq    int64
	Type   string
	Status string
	Title  string
	Parent string
	Rank   string
	// HandedOver is whether the event's own labels carried dispatch.LegionLabel.
	HandedOver bool
	// Priority is the issue's own Dispatch priority as the observation showed it: an event's
	// payload, the listing's or the detail's (record.DispatchPriority over Dispatch's 0 to 3 or
	// null). The record takes it wherever it takes the observation's rank.
	Priority record.Priority
	// Labels is the issue's labels as the observation showed them, in Dispatch's order. The copy
	// of Dispatch's issues writes them with the rest of the observation (blockers.Mirror), and
	// nothing else reads them: a decision reads HandedOver.
	Labels []string
	// ActorSession is the id of a non-daemon session that wrote the event. DaemonActor tells its
	// own status write from a user or an untyped actor: a pending hand-over fence is consumed only
	// by its daemon-authored todo event.
	ActorSession string
	DaemonActor  bool
	// ResolvedRepo is the repository (owner/name) Dispatch resolves the issue to at the event: its
	// own, else its nearest ancestor's, else its project's default; empty when it resolves to none.
	// The workflow takes it when it first records the issue, and never after.
	ResolvedRepo string
	// Blockers is the keys of the issues the event says this issue waits on, in Dispatch's own
	// order. An event retained from a Dispatch that did not yet send them carries none, as an issue
	// waiting on nothing does: no reader needs to tell those two apart.
	Blockers []string
	// ResolvedRepoMissing is an event JetStream retained from before Dispatch resolved an issue's
	// repository: its payload has no resolved_repo, so ResolvedRepo says nothing. The consumer's
	// PrepareRepository fills ResolvedRepo from the issue's record, or from Dispatch for an issue
	// not yet recorded, and clears this; the workflow fills a recorded issue's from its record, and
	// never records an issue from a fact still missing it.
	ResolvedRepoMissing bool
	// RepositoryRefusal is why the GitHub reads this event's preparation made for the repository
	// the issue resolves to record the issue on no repository: the role App that is not installed
	// on it, a repository GitHub does not have, or a name that is no <owner>/<name> at all. It is
	// read where the fact is applied, in place of what the daemon holds in memory
	// (workflow.Engine.ResolveIssueRepository), because no such refusal decides a later
	// preparation: the next preparation of the issue reads GitHub again, so an operator who
	// installs the App is taken at that preparation with no wait and no restart. Empty for every
	// other fact, the decode's own among them: only a preparation sets it.
	RepositoryRefusal string
	// Ancestry is the keys of the issue's Dispatch parent chain, from its parent upward to the top,
	// stopping only at a repeated key or back at the issue: the chain release_children's own walk
	// reads. It is read from Dispatch when the fact is prepared, outside the fact lock, for a
	// labelled todo of an unrecorded issue or a recorded root under a parent, recorded or not;
	// empty for any other fact. Admission records it, and keeps such a root out of its waiting line
	// while any live root is on it (record.Waiting).
	Ancestry []string
	// Reread is the Ancestry read again, when an event of an issue the workflow does not record, or
	// a fresh event of a recorded issue whose Dispatch parent differs from its record's, is
	// prepared, of every labelled todo root whose recorded Ancestry holds the issue's key, keyed by
	// that root, for each one the read changes (nil for none now): a person may have moved that
	// very issue, and the root under it has no event of its own. Nil when none changes. Admission
	// writes each before anything else it applies; the workflow reads none of it.
	Reread map[string][]string
	// ClaimedBy is who the observation shows holding the issue's Dispatch claim, `<kind>:<id>`, or
	// "" for nobody, and ClaimKnown says the observation showed the claim at all: an event retained
	// from a Dispatch that did not yet send it, or an event or read whose claim names no actor
	// (dispatch.IssueSummary.ClaimUnread), says nothing of it. blockers.Mirror writes a known claim
	// at the observation's Seq, behind the claim's own fence (record.PutDispatchClaim).
	ClaimedBy  string
	ClaimKnown bool
	// Observed is a fact made from Dispatch's view of an issue the workflow did not record when the
	// fact was prepared (ObservedIssue), not from one event, so no actor is known to have written
	// its state. It reaches only an issue still unrecorded when it applies, and only while no later
	// issue fact of the issue has applied (the copy's applied sequence,
	// record.Store.DispatchIssueApplied): ApplyFact,
	// under the fact lock, applies Delivered in its place once the issue is recorded
	// (release_children, or another delivery, recording it meanwhile), or nothing when Delivered is
	// nil, so a state the view shows never reaches a record without the actor of the event that
	// wrote it.
	Observed bool
	// Delivered is the event an Observed fact was prepared in place of, as it was delivered, or the
	// event a route's own status write made (api's recordOwnStatusWrite); nil for a view prepared in
	// place of no event (release_children's view of a child Dispatch already showed todo).
	Delivered *DispatchIssue
	// StatusMove is a status Dispatch confirmed the daemon wrote: the fact an API route applies for
	// its own write (OwnStatusWrite), at the sequence Dispatch answered it with. The workflow takes
	// its status as a move even when the record already shows Dispatch at that status: the record
	// can be behind events the consumer has not delivered (a session's status and the daemon's
	// re-assert of its own), so Dispatch's status at the event before this one is not known, and the
	// edit test (a status equal to the one Dispatch showed before) does not hold for it. Never set by
	// a decoded event.
	StatusMove bool
	// LabelSetter is the kind of the Dispatch actor (`user`, `session` or `system`) that added
	// dispatch.LegionLabel to the issue, by the rule the function LabelSetter states: the actor of
	// the newest issue.created or issue.updated event in the issue's Dispatch event log whose
	// labels carry the label while the previous such event's did not. The triggering event's own
	// actor decides nothing. It is read from the log when the fact is prepared, outside the fact
	// lock, only for an unrecorded root in `triage` that carries the label: by the prepare stage
	// (the daemon's workflowRuntime.prepareIssue) and by the boot listing (reconcileListing). It is
	// "" for every other fact, when the log shows no such transition, and when nothing read it.
	// Only "user" has admission queue a person's handover (a todo through the outbox); every other
	// value, "" included, keeps today's `triage` wake of the controller.
	LabelSetter string
}

// LabelledTriageRoot says whether the fact shows a root (no Dispatch parent) in triage that
// carries the label: the one kind of issue whose label's setter the prepare stage and the boot
// listing read (LabelSetter), and admission then judges, so the two sides test one predicate.
func (i DispatchIssue) LabelledTriageRoot() bool {
	return i.Status == "triage" && i.Parent == "" && i.HandedOver
}

// LabelSetByPerson says whether a person set the label (LabelSetter), by the one rule the daemon
// has for who is a person (dispatch.Actor.Person): a session's or Dispatch's own label is not,
// and neither is a setter nothing read.
func (i DispatchIssue) LabelSetByPerson() bool {
	return dispatch.Actor{Kind: i.LabelSetter}.Person()
}

// ObservedIssue is issue as Dispatch's detail shows it now, as an Observed fact at the sequence
// Dispatch has run to: its status, title, parent, rank, priority, labels, hand-over, blockers and
// repository, with no event's type or actor, since no one event wrote that state.
// release_children's todo child and an unrecorded issue's admission event are applied from it.
func ObservedIssue(issue dispatch.Issue) DispatchIssue {
	parent := ""
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	return DispatchIssue{Key: issue.Key, Seq: issue.LastSeq, Status: issue.Status, Title: issue.Title, Parent: parent,
		Rank: issue.Rank, Priority: dispatchPriority(issue.Priority), HandedOver: issue.HandedOver, Labels: issue.Labels, ResolvedRepo: issue.ResolvedRepo,
		Blockers: issue.BlockedBy, ClaimedBy: issue.Claim.Holder(), ClaimKnown: !issue.ClaimUnread, Observed: true}
}

// OwnStatusWrite is the event the daemon's own status write makes, as the Dispatch consumer
// decodes it (decodeDispatchEvent): written is the issue Dispatch answered that write with
// (dispatch.Client.SetStatus), the state the event carries, at the event's sequence, and its actor
// is the daemon's own session. An API route that writes a status applies it in the request, so the
// record does not wait for the event to come back through the consumer; the event, at a sequence no
// later than this one, then finds its fence.
func OwnStatusWrite(written dispatch.Issue) DispatchIssue {
	event := ObservedIssue(written)
	event.Observed, event.DaemonActor, event.StatusMove, event.Type = false, true, true, "issue.updated"
	if written.Status == "done" {
		event.Type = "issue.closed"
	}
	return event
}

// ListedIssue is issue as Dispatch's issue listing shows it, as a fact at the issue's sequence, with
// no event's type or actor: what a boot's reconcile records or prepares from.
func ListedIssue(issue dispatch.IssueSummary) DispatchIssue {
	parent := ""
	if issue.Parent != nil {
		parent = *issue.Parent
	}
	return DispatchIssue{Key: issue.Key, Seq: issue.LastSeq, Status: issue.Status, Title: issue.Title, Parent: parent,
		Rank: issue.Rank, Priority: dispatchPriority(issue.Priority), HandedOver: issue.HandedOver, ResolvedRepo: issue.ResolvedRepo, Blockers: issue.BlockedBy,
		ClaimedBy: issue.ClaimedBy, ClaimKnown: !issue.ClaimUnread}
}

// dispatchPriority is level, the priority a Dispatch read serves, as the record keeps it. The
// listing and the detail answer no event's fields the decoder could report, and Dispatch writes
// no level outside 0 to 3, so a level it could not read is none here (record.DispatchPriority).
func dispatchPriority(level *int) record.Priority {
	priority, _ := record.DispatchPriority(level)
	return priority
}

// Handover is the outbox's direct completion of a hand-over whose Dispatch state is already todo
// with the legion label, so PATCH would make no event. Admission validates the durable fence.
type Handover struct {
	Issue      string
	Tree       string
	Generation uint64
}

func (f Handover) namedIssue() (string, bool) { return f.Issue, true }

// ChildAdoption is an architect's release of children recorded outside its tree Tree, the root
// key its grant names: each a root that queued in admission before a person made it a child of
// Tree, under the Dispatch parent the release read. Admission adopts every one into Tree or, when
// any cannot join it (workflow's AdoptionRefusal, or Tree is not live), refuses the whole fact and
// writes nothing.
type ChildAdoption struct {
	Tree     string
	Children []AdoptedChild
}

// AdoptedChild is one child a ChildAdoption moves into its tree, as the release's own Dispatch
// read showed it: its Dispatch parent, the status and the keys it waits on, and the sequence
// Dispatch had run to at that read. The daemon's copy of those links can be older than the read
// — a person's re-parent and link are one edit of Dispatch, whose event the consumer may not have
// delivered — and an adopted root is already recorded, so no observation of it applies to correct
// the copy (settleObserved). The read therefore travels with the fact: the mirror takes it
// (blockers.Mirror) before the adoption judges the child, as a reading of the parent, the status
// and the blockers at Seq, each of which it changes only where the copy holds it from an older
// reading or from none, leaving the labels and the rank, which the read does not carry, as the
// copy holds them; and the workflow holds the child on it as well as on the copy's row (workflow's
// enterChild).
type AdoptedChild struct {
	Issue     string
	Parent    string
	Status    string
	BlockedBy []string
	Seq       int64
}

func (ChildAdoption) namedIssue() (string, bool) { return "", false }

func (f DispatchIssue) namedIssue() (string, bool) { return f.Key, true }

// DispatchArtifactKind identifies the three Dispatch artifact observations that affect a gate.
type DispatchArtifactKind string

const (
	DispatchArtifactApproved         DispatchArtifactKind = "approved"
	DispatchArtifactChangesRequested DispatchArtifactKind = "changes_requested"
	DispatchArtifactVersion          DispatchArtifactKind = "version"
)

// DispatchArtifact records one review or version event for a registered design document, from a
// live event or from the daemon's own read of Dispatch's approval (the gate seed). Every source
// applies alike: a key per review, not the source, keeps the architect from hearing one changes
// request twice (the workflow's tellChanges). Mark places the event in the issue's Dispatch event log: the event's
// sequence and Dispatch's time of it, from the event itself, or, for a review, from the read's
// latest review (dispatch.Approval's Seq and At), whichever of them that read gave. A version the
// gate seed read from Dispatch carries none, and says instead whether that read showed the
// document's approval request waiting on the agent (RequestWaitsOnAgent), which is what a newer
// version does to it: a version with neither a mark nor that cannot be placed against a root's
// wait on its gate, and answers none. Actor is the event's actor: for a review, the person who
// reviewed, named on the notice the review sends, since that notice goes to the controller when
// nobody in its tree can hear it (the daemon's parkDecision); the gate seed's read names the
// reviewer that read found (dispatch.Approval.LatestReviewer).
type DispatchArtifact struct {
	Key                 string
	ArtifactID          string
	Kind                DispatchArtifactKind
	Version             int
	Reason              string
	Mark                record.EventMark
	RequestWaitsOnAgent bool
	// Actor is who Dispatch names as having made the review: the event's own actor, decoded from an
	// artifact.approved or artifact.changes_requested event wherever one is decoded (the live
	// consumer, the boot catch-up's DecodeDispatchEvent). It is zero for a version event and for a
	// fact the gate seed builds from its read of Dispatch's approval, which names nobody. A
	// person's (Actor.Person()) approval of a parked labelled root's primary document is what has
	// the workflow queue that root; a review a session or Dispatch itself wrote queues nothing.
	Actor dispatch.Actor
	// Issue is Dispatch's view of the issue the review is on, read when the fact is prepared by
	// the daemon's workflowRuntime.prepareArtifact, for a person's approval that is not a carried
	// one (CarriedApproval), outside the fact lock; nil for every other fact, a seed's or the boot
	// catch-up's among them. MayQueueHandover reads its parent, label, status and primary document,
	// for the preparation to decide whether to read the issue's event log and for the workflow to
	// decide whether the approval queues the root: a nil view queues nothing and is never an
	// error, since nothing but that decision reads it.
	Issue *dispatch.Issue
	// StatusChangedSeq is the sequence of the first issue event of the issue's Dispatch event log
	// after the approval (Mark.Seq) that carries another status than the issue event before it
	// (WithStatusChange), set when the daemon prepares a person's approval that needs the log
	// (NeedsEventLog). It is 0 when no later event changed the status, and for every fact
	// nothing read the log for.
	StatusChangedSeq int64
}

func (f DispatchArtifact) namedIssue() (string, bool) { return f.Key, true }

// CarriedApprovalReasonPrefix opens the reason of a carried approval: the review Dispatch writes
// in a person's name when their answer to a decision block was the whole change since the version
// they approved, when a later version's text is the text of the version they approved, or when a
// later version's text is that text with the questions a reopen opened again
// (packages/envoy/internal/dispatch/docs/approval_carry.go, carryReason, restoreReason and
// reopenReason, whose format is "Carried forward from the approval of version %d: ..."). The
// daemon cannot import that package, so it holds the prefix before the version number as a
// literal of its own.
const CarriedApprovalReasonPrefix = "Carried forward from the approval of version"

// CarriedApproval says whether the review is a carried approval, by its reason's prefix
// (CarriedApprovalReasonPrefix). A carry is Dispatch's bookkeeping of an earlier approval, not a
// person's new act, so it queues no parked root. An empty reason (a null `reason` on the event, as
// an approval given with none carries) is not a carry.
func (a DispatchArtifact) CarriedApproval() bool {
	return strings.HasPrefix(a.Reason, CarriedApprovalReasonPrefix)
}

// PersonApproval says whether the review is an approval a person made themselves: an approval,
// not a changes request or a version, by an actor Dispatch names a person, and not a carried one
// (CarriedApproval). It is the one review the daemon reads the issue for when it prepares the
// fact, and the one that can queue a parked labelled root as the person's handover, so the read
// side and the deciding side test one predicate.
func (a DispatchArtifact) PersonApproval() bool {
	return a.Kind == DispatchArtifactApproved && a.Actor.Person() && !a.CarriedApproval()
}

// MayQueueHandover says whether the review is a person's approval (PersonApproval) of the primary
// document of a root that Dispatch's view (Issue) shows parked: no parent, the legion label, status
// backlog or icebox. Such an approval queues the root as the person's handover unless the root's
// tree is live or its status changed after the approval (StatusChangedSeq), which the workflow
// decides. The daemon reads the issue's event log for it when it prepares the fact (NeedsEventLog),
// and the workflow decides from it, so the read side and the deciding side test one predicate.
func (a DispatchArtifact) MayQueueHandover() bool {
	if a.Issue == nil || !a.PersonApproval() {
		return false
	}
	view := a.Issue
	parked := view.Status == "backlog" || view.Status == "icebox"
	return view.Parent == nil && view.HandedOver && parked && view.PrimaryArtifactID == a.ArtifactID
}

// GateAboveDispatch is the gate seed of a generation (Generation) finding the document at a version
// below the one the seed was queued at (Above): Read is what the seed read of Dispatch, as the fact
// it would otherwise apply, at Dispatch's latest version. Dispatch's versions only rise and the
// seed reads after the daemon applied whatever moved the gate, so the gate stands above any version
// Dispatch wrote, and is reset to Dispatch's.
type GateAboveDispatch struct {
	Generation uint64
	Above      int
	Read       DispatchArtifact
}

func (f GateAboveDispatch) namedIssue() (string, bool) { return f.Read.Key, true }

// DispatchAskOpened is an ask a session or a person opened on an issue, with its kind (Kind:
// record.AskQuestion, record.AskApproval, or "" for a kind this daemon does not know, such as an
// event retained from before Dispatch had the two), the document an approval request asks a person
// to approve (Artifact, "" for a question; every ask.* event of an approval ask carries it), the
// session that opened it (AuthorSession, "" for a person), and its event's place in the issue's
// Dispatch event log (Mark). On a root it opens the ask's mirror row (record.RootAsk).
type DispatchAskOpened struct {
	Key           string
	Ask           string
	Kind          string
	Artifact      string
	AuthorSession string
	Mark          record.EventMark
}

func (f DispatchAskOpened) namedIssue() (string, bool) { return f.Key, true }

// DispatchAskRestored is Dispatch's settlement restoring a decision block it retracted: the ask
// opened again (Open), or closed again with the answer given before the retraction. Nobody acted,
// so it moves the ask's mirror row alone and is never a person's act.
type DispatchAskRestored struct {
	Key      string
	Ask      string
	Kind     string
	Artifact string
	Open     bool
	Mark     record.EventMark
}

func (f DispatchAskRestored) namedIssue() (string, bool) { return f.Key, true }

// DispatchAskClosed is an answered or resolved ask (State `answered` or `resolved`), its kind and
// document as DispatchAskOpened names them, the actor who closed it, and where Dispatch recorded
// that act (Mark).
//
// Decline is the person's decline to answer, set where the close carries a resolution of the one
// kind this daemon acts on (ResolutionDeclined) and nil for every other close: an answer, a close
// whose payload carries no resolution, and a resolution kind this daemon does not know, which is
// no defect to report — Dispatch resolves asks for several reasons and the daemon acts on this
// one. It carries the person's own note and Dispatch's own words apart (record.AskDecline), each
// as Dispatch recorded it.
type DispatchAskClosed struct {
	Key      string
	Ask      string
	Kind     string
	Artifact string
	State    string
	Decline  *record.AskDecline
	Actor    dispatch.Actor
	Mark     record.EventMark
}

// ResolutionDeclined is the resolution kind of an ask a person chose not to answer
// (DispatchAskClosed.Decline), which Dispatch writes on its POST /api/v1/asks/{id}/decline.
const ResolutionDeclined = "declined"

func (f DispatchAskClosed) namedIssue() (string, bool) { return f.Key, true }

// DispatchAskReply is a comment on an ask (comment.created, or comment.answered for a reply sent
// through a delivery's callback), or Dispatch's ask.handed_back of an approval request (Kind
// record.AskApproval and its Artifact; a comment names neither), with whose turn it leaves the ask
// on (WaitingOn: `agent` or `human` on an open ask, a hand-back `human`, and "" on a closed one),
// the actor who made it, its event's place in the issue's Dispatch event log (Mark), and the
// comment's own id (Comment, empty for a hand-back), which is where in Dispatch a person's reply
// relayed to the controller was made (workflow's relayParkedDecision).
type DispatchAskReply struct {
	Key       string
	Ask       string
	Kind      string
	Artifact  string
	WaitingOn string
	Actor     dispatch.Actor
	Mark      record.EventMark
	Comment   string
}

func (f DispatchAskReply) namedIssue() (string, bool) { return f.Key, true }

// DispatchApprovalMoved is an approval request of Artifact that a version of its document moved
// past the version its agent last handed to the person (Dispatch's ask.edited of an approval ask
// whose requested_version is below its version), at Mark: Dispatch shows such a request waiting on
// its agent until the agent hands it back. Nobody replied, so it is never a person's act.
type DispatchApprovalMoved struct {
	Key      string
	Ask      string
	Artifact string
	Mark     record.EventMark
}

func (f DispatchApprovalMoved) namedIssue() (string, bool) { return f.Key, true }

// DispatchClaim is the holder Dispatch records for an issue at Seq, from its issue.claimed or
// issue.released event: ClaimedBy is `<kind>:<id>`, or "" when the release cleared the claim.
// blockers.Mirror writes it to the claims mirror (record.PutDispatchClaim) behind the claim's own
// fence, whether or not the issue has a mirrored row yet.
type DispatchClaim struct {
	Key       string
	Seq       int64
	ClaimedBy string
}

func (f DispatchClaim) namedIssue() (string, bool) { return f.Key, true }

// DispatchResultChanged is a change of what Dispatch computes for the checks of Key's
// Verification document, at Seq in Key's event log: a result recorded against one of them
// (result.recorded, with its id, its check's block id and its verdict), a retraction of one
// (result.retracted, with no verdict), a pull request that reached Key or left it
// (issue.pull_request_attribution_changed), or a new deploy of a program a live check of Key reads
// (deploy.recorded), the last two with no result and no check. Dispatch appends a result's events
// on the issue whose document holds the check, whichever issue the result was recorded on, so Key
// is the issue whose close the change may decide. None of them decides a check: the read of the
// checks does (record.CloseRead), which each queues for an issue whose close waits.
type DispatchResultChanged struct {
	Key      string
	Seq      int64
	ResultID string
	Check    string
	Verdict  string
}

func (f DispatchResultChanged) namedIssue() (string, bool) { return f.Key, true }

// DispatchConsumerPosition is one measurement of the Dispatch consumer's own position: its ack
// floor as a stream sequence — the point before which every matching message is acknowledged,
// redeliveries and naks included — and whether it is idle, nothing pending or unacknowledged at
// all. The daemon's boot-owned poll reads one (Consumers.DispatchPosition) for Reconcile, and
// again on a ticker while admission holds anything back, applying each changed reading through
// ApplyFact as a synthetic fact — never decoded from a real Dispatch event. Admission is the only
// handler that acts on it: it releases every hold whose target the position has Reached, applies
// each released key's own listing snapshot, and promotes, all in the same transaction.
type DispatchConsumerPosition struct {
	AckFloorStream int64
	Idle           bool
}

func (DispatchConsumerPosition) namedIssue() (string, bool) { return "", false }

// Reached says whether the consumer at this position has caught up to target, a notification
// stream sequence: its ack floor is at or past it, or it is idle. Idle covers a target the ack
// floor alone can never reach — one set past the consumer's own last matching message, since the
// stream also carries GitHub subjects the Dispatch consumer's filter never matches, or past
// messages that landed before a consumer created at DeliverNewPolicy existed, which it will never
// be given.
func (p DispatchConsumerPosition) Reached(target int64) bool {
	return p.AckFloorStream >= target || p.Idle
}

// IssueStatus is one issue as Dispatch shows it now: its key, the project that key names, its
// status, the keys it says it is blocked by, its Dispatch parent, its labels, its board rank and
// the Dispatch sequence that view stands at. The periodic turn's re-read carries them on its
// ControllerTick, as a listing does (blockers.Observed), and blockers.Mirror writes each as one
// reading of all five values: each value keeps the sequence of the reading that wrote it, and a
// reading changes it only at a higher sequence or where no reading wrote it
// (record.PutDispatchIssue), so a view older than an event already applied changes nothing that
// event wrote. ClaimedBy and ClaimKnown are DispatchIssue's: the issue read behind this view shows
// the claim unless it names no actor.
type IssueStatus struct {
	Key string
	// Project is the Dispatch project of Key, as the read that saw the issue reports it, since a
	// turn re-reads blockers of other projects too (workflowRuntime.readObserved) and the mirror
	// row of such a key is that project's rather than the daemon's (blockers.Mirror.put).
	Project    string
	Status     string
	Blockers   []string
	Seq        int64
	ClaimedBy  string
	ClaimKnown bool
	// Parent is the key of the issue's Dispatch parent, empty for an issue Dispatch shows under
	// none. The children-first rule reads the copy's parent column to find what hangs under an
	// issue (record.ChildWaits), so every observation carries the parent it saw, and empty is
	// the observation that the issue has no parent: it takes the child out from under the parent
	// the copy holds wherever it is the newer reading of the parent (record.PutDispatchIssue).
	Parent string
	// Labels and Rank are the issue's labels, in Dispatch's order, and its board rank as the view
	// showed them. The copy of Dispatch's issues is their one reader.
	Labels []string
	Rank   string
}

// ControllerTick is the daemon's periodic wake for the project's controller, applied as a
// synthetic fact every `controller_wake_interval_seconds` and never decoded from an event. Three
// handlers act on it, in the order the daemon wires them (the workflow runtime's openWorkflow):
// blockers.Mirror writes what the tick's own Dispatch read saw, the workflow engine runs its
// status repair (workflow.Engine.correctStatuses), and admission decides whether to wake the
// controller (admit.Admission.wakeController and its callers).
type ControllerTick struct {
	// Observed is what the turn's own re-read of Dispatch saw: the issues anything is currently
	// waiting on, as Dispatch answers for them now (workflowRuntime.tickController reads them
	// before the fact is applied, since no handler may perform I/O). The mirror handler writes
	// them, ahead of the handlers that judge from the mirror in this same transaction, so a
	// blocker whose closing event never arrived is caught here — at the cost of one wake interval
	// rather than standing until the next boot's listing. Empty is the tick as it always was: an
	// ordinary wake that writes nothing, which is almost every turn, and also what a failed read
	// leaves.
	Observed []IssueStatus
}

func (ControllerTick) namedIssue() (string, bool) { return "", false }

// EntrySweep is the daemon's periodic convergence of the phase entries, applied as a synthetic
// fact every entrySweepInterval by the workflow runtime's own ticker (internal/daemon's
// tickEntrySweep) and never decoded from an event. Admission is the only handler that acts on
// it: it reads the project's issues, its slots and its line facts as its promote path does and
// sends every role whose claim is behind the entry its phases row names that entry's work
// (admit.Admission.convergeEntries).
//
// It carries nothing, because the difference it closes is one the record already holds and the
// handler reads the whole project. What it is for is the running: an admission pass runs on the
// facts its rule names (admit.Admission.Apply), and a daemon with none arriving — a quiet
// project, a transition whose own start row the outbox fenced out — would otherwise leave a role
// behind its entry until something else happened. Going through ApplyFact takes the one lock
// every fact takes, so the sweep reads no record a transition is in the middle of writing.
type EntrySweep struct{}

func (EntrySweep) namedIssue() (string, bool) { return "", false }

// ProjectResumed is the resume of the daemon's project's pause, applied as a synthetic fact once
// the resume has committed and never decoded from an event (internal/daemon's
// projectPause.Resumed, decision 22 of dispatch://INFRA-3759). It is the resume's one admission
// pass: the pause held every promotion, and the pass runs now rather than at the next pass the
// rule of admit.Admission.Apply runs, which names this fact among those that run it whatever they
// wrote; the workflow engine and the blockers mirror do nothing for it. Unlike the controller's
// tick, which queues a tick notice, it queues none: a slot its pass's own release leaves free
// wakes the controller as any fact's pass does (admit.Admission.wakeForFreeSlot), and nothing
// else. The rows a re-admission freed while the pause held (admit's releaseTreeSlots) wake
// nothing; the resume's notice tells the controller of that capacity. Where the pass fails, the
// next pass the rule runs promotes the line, and any other fact that writes nothing runs none.
//
// It carries nothing, as EntrySweep carries nothing: the pass reads the whole project. Its event
// is keyed on the pause the resume ended (internal/daemon's applyResumePass), so a resume applies
// its pass once.
type ProjectResumed struct{}

func (ProjectResumed) namedIssue() (string, bool) { return "", false }

// PullRequestOpened registers a pull request whose branch or body identifies a Dispatch issue.
type PullRequestOpened struct {
	Repo    string
	Number  int
	Branch  string
	HeadSHA string
	// Base is the branch the pull request merges into (the listener's `base_ref`, GitHub's
	// base.ref), empty from a listener that predates the field; the workflow records it so a push
	// onto that branch finds the pull request again (record.PullRequest.Base).
	Base      string
	Body      string
	URL       string
	UpdatedAt time.Time
	// Reopened is GitHub's reopened action; false is opened, which GitHub sends once per pull
	// request.
	Reopened bool
}

func (PullRequestOpened) namedIssue() (string, bool) { return "", false }

// OpenedRead is the PullRequestOpened a read of GitHub that found pull open on branch of repo
// stands for, as its webhook would have carried it - its base, which the reads of the merge state
// follow on before any of them has answered, and a reopen when recorded is the same pull request -
// and the event id, under source "github", it is applied under: the project, the
// repository and number, what the read observed, the head and GitHub's updated_at, and what the
// workflow decides an opened by beside it, the issue's generation and its pull request record as
// the read found it (nil for none: its number, state and clock). The id holds no read time and no
// boot, so the same observation applied twice against the same record is one fact (ApplyFact
// answers the second Duplicate, and Applied finds it before), whichever reader applies it - the
// daemon's per-issue read or the implementer's completion - while a pull request that changed since,
// or a record that changed since (a new generation removes a closed one the opened was declined
// against), makes a new one.
func OpenedRead(project, repo, branch string, pull githubrest.PullRequest, generation uint64, recorded *record.PullRequest) (PullRequestOpened, string) {
	fact := PullRequestOpened{Repo: repo, Number: pull.Number, Branch: branch, Base: pull.Base.Ref, HeadSHA: pull.Head.SHA, Body: pull.Body,
		URL: pull.HTMLURL, UpdatedAt: pull.UpdatedAt, Reopened: recorded != nil && recorded.Number == pull.Number}
	against := "none"
	if recorded != nil {
		against = fmt.Sprintf("%d/%s/%s", recorded.Number, recorded.State, recorded.HeadUpdatedAt.UTC().Format(time.RFC3339Nano))
	}
	return fact, fmt.Sprintf("opened-read:%s:%s#%d:%s:%s:%d:%s", project, repo, pull.Number, pull.Head.SHA, pull.UpdatedAt.UTC().Format(time.RFC3339Nano), generation, against)
}

// PullRequestSynchronized records a new observed head, including the fields the shipped reducer
// reads to fence stale observations and find its issue. Before is the head GitHub's synchronize
// says the branch moved from, the move the head arrives by (classify.AdvancePullRequestHead):
// empty from a listener that predates the field, and from the daemon's own read of the head
// (daemon's followHead), which knows no move. Sender is the login of the account whose push moved
// the branch (the webhook's sender), empty from a listener that predates the field and from the
// daemon's own read.
type PullRequestSynchronized struct {
	Repo    string
	Number  int
	Branch  string
	Before  string
	Sender  string
	HeadSHA string
	// Base is the branch the pull request merges into, as PullRequestOpened.Base is; empty from a
	// listener that predates the field and from the daemon's own read of the head, and an empty
	// one leaves the recorded base as it is.
	Base      string
	Body      string
	UpdatedAt time.Time
}

func (PullRequestSynchronized) namedIssue() (string, bool) { return "", false }

// PullRequestReview is a submitted review on a pull request.
type PullRequestReview struct {
	Repo   string
	Number int
	// ID is GitHub's review id, assigned when the review is created: a pending review (a draft)
	// keeps the id it was created with when it is submitted later. Zero when the listener did not
	// carry it.
	ID int64
	// SubmittedAt is when the review was submitted, zero when the listener did not carry it or
	// carried one that could not be read. Reviews are ordered by it, then by ID
	// (record.ReviewOrder).
	SubmittedAt time.Time
	State       string
	CommitID    string
	HeadSHA     string
	Author      string
	// AuthorCanWrite is whether Author has write access or higher to the repository, as GitHub's
	// collaborator permission answers it (ConsumerSpec.ReviewPermission reads it before the fact is
	// applied, since the workflow decides inside a transaction and performs no I/O). It is never
	// looked up, and stays false, for a review that does not decide (Decides), one with no author,
	// one on a pull request the daemon does not record, and one the review App submitted, which
	// decides by its login and the reviewer's session (workflow's DecidesRound). It is also false
	// for an author GitHub does not give write access.
	AuthorCanWrite bool
	// BodyTruncated is whether Envoy's normalizer capped the review's body before the daemon saw
	// it (`body_truncated: "true"`), which can cut a long review's trailing Legion footer off
	// (LegionSession). ConsumerSpec.ReviewBody restores the full body from GitHub for a truncated
	// review-App review before the fact is applied, and clears this.
	BodyTruncated bool
	Body          string
	// ReadFromList is whether the daemon's own read of GitHub's list of the pull request's
	// reviews produced this fact (daemon's reviewToApply) rather than the review's webhook, which
	// the decoder never sets it from. The list answers with GitHub's current commit_id for every
	// review it holds (LegionHead, dispatch://INFRA-1557), where the webhook's envelope carries
	// the commit_id the event was delivered with, so the workflow lets a person's listed review
	// decide no round while the same person's webhook review decides as it always has
	// (workflow's refusesRound).
	ReadFromList bool
}

func (PullRequestReview) namedIssue() (string, bool) { return "", false }

// Decides says whether the review's state can decide a review round: an approval or a request for
// changes, in whatever case its producer spelled it. A comment decides nothing, whoever writes it.
// Intake reads the author's permission only for a review that decides, and the workflow lets only
// such a review decide, so both ask this.
func (r PullRequestReview) Decides() bool {
	state := strings.ToLower(r.State)
	return state == "approved" || state == "changes_requested"
}

// legionFooterMarker opens the Legion footer every Legion role appends to a pull-request review:
// a JSON object naming the session and phase that wrote it and the head it read, and in the
// reviewer's review the entry of the round it is of (the reviewer template and its footer,
// prompts/roles/reviewer.md).
const legionFooterMarker = "<!-- legion:"

// legionFooter is what the daemon reads out of that JSON object: the session that submitted the
// review (LegionSession), the head the review says it read (LegionHead) and the entry of the
// round it says it is of (LegionEntry). A role writes its phase there too, which nothing here
// reads. The entry is kept raw and read on its own, so an entry of the wrong JSON type costs the
// entry alone and never the session and head beside it, which decide the round.
type legionFooter struct {
	Session string          `json:"session"`
	Head    string          `json:"head"`
	Entry   json.RawMessage `json:"entry"`
}

// lastLegionFooter decodes the last Legion footer in the review's body: the JSON object between
// the last occurrence of legionFooterMarker and the next `-->`. Every field is empty when the
// body carries no footer, no `-->` closes the last one, or its JSON cannot be parsed. The last
// occurrence wins, since an earlier footer quoted in the body (a reply, a reviewer's own draft)
// is not the one that closes it.
func (r PullRequestReview) lastLegionFooter() legionFooter {
	at := strings.LastIndex(r.Body, legionFooterMarker)
	if at < 0 {
		return legionFooter{}
	}
	rest := r.Body[at+len(legionFooterMarker):]
	end := strings.Index(rest, "-->")
	if end < 0 {
		return legionFooter{}
	}
	var footer legionFooter
	if err := json.Unmarshal([]byte(rest[:end]), &footer); err != nil {
		return legionFooter{}
	}
	return footer
}

// LegionSession is the session named in the last Legion footer in the review's body
// (lastLegionFooter). It is "" when the body carries no such footer, the last one's JSON cannot
// be parsed, or its session is empty. The footer is JSON, whose \u0000 escape decodes to a NUL
// that the body as stored never shows, so the session is given as the store would keep it
// (storetext.Text): the workflow compares it with the reviewer claim's stored session.
func (r PullRequestReview) LegionSession() string {
	return storetext.Text(r.lastLegionFooter().Session)
}

// LegionHead is the head the review says it read: the head field of the last Legion footer in the
// review's body (lastLegionFooter). It is "" when the body carries no footer, the footer's JSON
// cannot be parsed, it names no head, or the head it names is not spelled as a head is
// (reviewround.IsHead) - a short SHA or a branch name names no commit, since only the whole of it
// says which code was read.
//
// The workflow holds a review the review App submitted to this rather than to CommitID because
// GitHub rewrites commit_id to the pull request's current head, so its REST record of a review
// can name a head the review never saw (dispatch://INFRA-1557). Nothing rewrites the body a
// review was submitted with, so the footer is the review's own word for the code it read, and it
// is the only thing a force push back to an earlier head cannot forge: no clock tells those
// heads apart.
func (r PullRequestReview) LegionHead() string {
	head := r.lastLegionFooter().Head
	if !reviewround.IsHead(head) {
		return ""
	}
	return head
}

// LegionEntry is the entry of the round the review says it is of: the entry field of the last
// Legion footer in the review's body (lastLegionFooter), the number the reviewer's task names
// (reviewround.EntryLabel, record.PhaseRow.Entry). The reviewer's texts write it as a JSON
// string of digits (`"entry":"<entry>"`), so a placeholder left unfilled keeps the footer JSON,
// and it is read as a positive whole number written either that way or as a JSON integer. It is
// 0 when the body carries no footer, the footer's JSON cannot be parsed, it names no entry, or
// the entry is anything else: an unfilled placeholder, an empty string, a word, a fraction, an
// exponent, a sign, zero, one past int64, null, a boolean, an array or an object names no entry.
// Such a footer still names its session and head, so the review decides its round as before;
// only the commit the reviewer read (record.PhaseRow.ReviewedHead) waits for a review that names
// the entry that stands (workflow's review).
func (r PullRequestReview) LegionEntry() int64 {
	digits := string(r.lastLegionFooter().Entry)
	if strings.HasPrefix(digits, `"`) {
		if err := json.Unmarshal([]byte(digits), &digits); err != nil {
			return 0
		}
	}
	entry, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || entry > math.MaxInt64 {
		return 0
	}
	return int64(entry)
}

// Order is where the review falls among the pull request's reviews (record.ReviewOrder): when it
// was submitted, then GitHub's review id. The workflow orders a deciding review against the pull
// request's newest by it, and the daemon's read of a pull request's reviews orders what GitHub
// lists by it, so both order reviews alike.
func (r PullRequestReview) Order() record.ReviewOrder {
	return record.ReviewOrder{SubmittedAt: r.SubmittedAt, ID: r.ID}
}

// CheckRun is one latest-run identity in a checks settlement.
type CheckRun = record.AttemptRun

// PullRequestChecks is Envoy's settled checks observation: the latest run of each check, and the
// checks the settlement names as failed (any failing conclusion) and as cancelled, kept apart since
// a cancellation is a property of the run - a later push can cancel it - where a failure is of the
// code. What they come to is told to the roles that work the issue, which decide it
// (workflow's tellChecks).
//
// Statuses are the commit statuses the settlement names, each with its context, its state and
// GitHub's created_at, in context order and with no stamp (record.CommitStatus): entries of their
// own kind, apart from the check runs, which a context sharing a check run's name never joins. A
// settlement that names none, a listener's of an earlier release among them, carries an empty
// list. The pull request keeps the newest status of each context the settlements of the head
// named (classify.ApplySettlement), so a context a settlement leaves out stays as it is kept.
// StatusesUnread is a list the decoder could not read (decodeChecks): the rest of the settlement
// stands, it names no status, and the pull request keeps the statuses it holds for the head, since
// the merge keeps a context a settlement leaves out (classify.ApplySettlement). The flag's one
// reader is the task of a role the settlement wakes, which names the failed ones of those kept
// statuses (workflow's wakeOnSettlement).
type PullRequestChecks struct {
	Repo           string
	Number         int
	HeadSHA        string
	CheckRuns      []record.AttemptRun
	Generation     int64
	Snapshot       string
	Failing        []string
	Cancelled      []string
	Statuses       []record.CommitStatus
	StatusesUnread bool
}

func (PullRequestChecks) namedIssue() (string, bool) { return "", false }

// PullRequestMergeability is GitHub's lazily computed verdict for whether a pull request's head
// can be merged into Base without a conflict, decoded from GitHub's nullable `mergeable` field on
// readPullRequest's own /pulls/{number} read, which also names the base branch: never
// decoded from an event, applied as a synthetic fact by the daemon's own read of one issue's pull
// request (workflowRuntime.readIssuePullRequest). Mergeable is record.MergeabilityUnknown while
// GitHub is still computing it, record.MergeabilityMergeable or record.MergeabilityConflicting
// once it has. MergeState is
// GitHub's own merge state for the head from the same answer's `mergeable_state`, and it is what
// an approval standing on the head waits for; what each state comes to for the round is
// classify.MergeOutcomeOf, the one place the rule is written. It tells a head GitHub holds on a
// check that is still running, a review, or a rule about the reader from one GitHub would merge
// now, which Mergeable cannot.
//
// ReviewsRequested and HeadChecks are the rest of the answer MergeOutcomeOf reads for a `blocked`
// head, written beside MergeState (record.PullRequest.ReviewsRequested and HeadChecks): how many
// review requests GitHub holds open, people, Apps and teams alike, and whether a check or a
// commit status of the head was still running. A REST read counts `requested_reviewers` and
// `requested_teams` and reads the head's `statusCheckRollup` from GitHub's GraphQL API when it
// answers `blocked` and MERGEABLE; a base's GraphQL batch reads `reviewRequests { totalCount }`
// and the same rollup in its one query. A nil count and record.HeadChecksUnread are an answer that
// gave neither, which the round does not end on. A fact minted by anything that did not read
// GitHub states them as it states MergeState.
//
// HeadSHA is the head GitHub answered both of them for, from the same answer's `head.sha`. Every
// read fills it, and the engine records the answer only while the record still stands at that
// head (workflow's mergeability): a read is one round trip to GitHub, a push can land during it,
// and a verdict about another head's tree says nothing about this one.
//
// ReadAt is the moment the reader had GitHub's answer in hand, taken from the reader's own clock
// rather than from the clock of whoever applies the fact: the engine takes no clock of its own, so
// what it records is the time of the read rather than the time of the apply, and its tests stay
// deterministic because every time they record is one their own fact named. It is what
// record.PullRequest.StateReadAt is written from, and a confirming read moves it exactly as a read
// that changes something does, since it is the age of the knowledge and not the age of the last
// change to it. A read whose ReadAt is before the stamp the record holds is dropped whole, as one
// of another head is: GitHub answered the recorded read later, so the older answer can be one it
// has since changed. A fact minted by anything that did not read GitHub - every test that states
// a verdict rather than reading one - leaves it zero, and the engine then writes no stamp, erases
// none and drops nothing for it.
//
// Rules is what the daemon read of the base branch's rules for its required status checks
// (record.BaseRules), which a confirmed conflict is decided on (workflow's mergeability,
// classify.ConflictNeedsMerge). It is set only on a read whose Mergeable is
// record.MergeabilityConflicting, since a head that merges cleanly needs no decision of that kind,
// and nil on every other read; the reader that fills it is the daemon's, which reads the base's
// rulesets and its classic protection beside the pull request. A CONFLICTING read that carries
// none is a read whose rules the daemon does not know, and it is decided as rules it could not
// read are.
type PullRequestMergeability struct {
	Repo       string
	Number     int
	Base       string
	HeadSHA    string
	Mergeable  record.Mergeability
	MergeState record.MergeState
	// ReviewsRequested and HeadChecks are the rest of a `blocked` answer (above).
	ReviewsRequested *int
	HeadChecks       record.HeadChecks
	ReadAt           time.Time
	Rules            *record.BaseRules
}

func (PullRequestMergeability) namedIssue() (string, bool) { return "", false }

// PullRequestMergeStateUnread is the daemon's own fact that a read of GitHub's merge state for the
// pull request at head HeadSHA failed - GitHub refused it, timed out or answered nothing - while
// the record holds a `blocked` answer of that head with `mergeable` null. It carries no answer:
// it asks the review round again at the moment it is applied (workflow's mergeStateUnread), since
// a persistent null is told once classify.NullMergeableTellAfter has passed since its first read,
// whatever the count of reads, and when every read fails no answer comes to ask the round.
type PullRequestMergeStateUnread struct {
	Repo    string
	Number  int
	HeadSHA string
}

func (PullRequestMergeStateUnread) namedIssue() (string, bool) { return "", false }

// PullRequestReviewRequests is GitHub's pull_request action `review_requested` or
// `review_request_removed`: the review requests GitHub holds open on the pull request changed, at
// head HeadSHA. It carries no count: the read it asks for counts them (workflow's askMergeState),
// since a request opened or withdrawn is what moves a `blocked` head's round between waiting for
// a review and ending (classify.MergeOutcomeOf), and GitHub sends no other event of it.
type PullRequestReviewRequests struct {
	Repo    string
	Number  int
	HeadSHA string
}

func (PullRequestReviewRequests) namedIssue() (string, bool) { return "", false }

// PullRequestMerged records GitHub's terminal merged observation: the merge commit, HeadSHA, the
// head the pull request merged at, "" when the observation names none, and MergedAt, GitHub's
// merged_at of the pull request, which orders a later merge of the issue after an earlier one. It
// is zero where the observation carries none, or one that does not parse: no reader invents one.
type PullRequestMerged struct {
	Repo     string
	Number   int
	MergeSHA string
	HeadSHA  string
	MergedAt time.Time
}

func (PullRequestMerged) namedIssue() (string, bool) { return "", false }

// PullRequestClosed records GitHub's unmerged close observation: the head the pull request closed
// at, and its updated_at, which orders the close against a reopen and the synchronizes before it.
type PullRequestClosed struct {
	Repo      string
	Number    int
	HeadSHA   string
	UpdatedAt time.Time
}

func (PullRequestClosed) namedIssue() (string, bool) { return "", false }

// PullRequestDequeued is GitHub's pull_request action `dequeued`: the pull request left the merge
// queue, and HeadSHA is the head it left the queue at. GitHub's payload names why it left (its
// `reason`), but the listener drops that field (packages/envoy/internal/contracts/normalize.go,
// the pull_request payload), so the fact carries no reason.
type PullRequestDequeued struct {
	Repo    string
	Number  int
	HeadSHA string
}

func (PullRequestDequeued) namedIssue() (string, bool) { return "", false }

// MergeQueueReadDone is the outbox's merge-queue read (record.MergeQueueRead) finishing with no
// merge: never decoded from an event. GitHub answered that pull request Number of Issue is open,
// or closed unmerged, or, with Unread, no read of it succeeded, so the daemon does not know
// whether GitHub merged it. Head is the head that left the merge queue (PullRequestDequeued). The
// workflow writes the message that tells the issue's merger, while the issue still stands in
// merging at Head (workflow's mergeQueueReadDone). A merged answer applies PullRequestMerged
// instead, as the webhook's merge would.
type MergeQueueReadDone struct {
	Issue  string
	Number int
	Head   string
	Unread bool
}

func (f MergeQueueReadDone) namedIssue() (string, bool) { return f.Issue, true }

// CloseChecksRead is the outbox's read of Dispatch's states for the close of Issue
// (record.CloseRead), carried back as a fact: never decoded from an event. Generation is the
// generation the read was queued for, and ReadAt the moment its row began its first Dispatch call,
// so a read in flight across an entry into verifying is not taken as a read after it. Answer is
// what Dispatch answered, in the layout the close's stored read keeps (record.CheckReadAnswer): the
// results, the results at the newest merge's final head where the close needs them, the issue's
// children, and the Verification document's approval. It is nil where the read failed, and Failure
// then says why. The workflow stores it as the issue's newest read (record.CheckRead) and judges
// the close on it.
type CloseChecksRead struct {
	Issue      string
	Generation uint64
	ReadAt     time.Time
	Failure    string
	Answer     *record.CheckReadAnswer
}

func (f CloseChecksRead) namedIssue() (string, bool) { return f.Issue, true }

// ConflictConfirmDone is the outbox's second read of a conflicting head (record.ConflictConfirm)
// finishing with no answer that decides: never decoded from an event. GitHub answered UNKNOWN for
// pull request Number of Issue at the row's last attempt, or, with Unread, no read of it
// succeeded, so no second read confirmed the first read's conflict at Head, made at FirstReadAt
// (record.ConflictConfirm.FirstReadAt), and none released it. The workflow then clears the first
// read while the record still holds that one, of Head and made at FirstReadAt (workflow's
// conflictConfirmDone), so the next read that finds the head conflicting is a first read again. A
// first read of Head made at another time is one a later read made after this row's was released,
// with a row of its own, and it is left standing. A CONFLICTING or MERGEABLE answer applies
// PullRequestMergeability instead, as every read of the pull request does.
type ConflictConfirmDone struct {
	Issue       string
	Number      int
	Head        string
	FirstReadAt time.Time
	Unread      bool
}

func (f ConflictConfirmDone) namedIssue() (string, bool) { return f.Issue, true }

// PullRequestMissing is the daemon's per-issue read of GitHub finding no open pull request on
// Branch, the branch of Issue, whose implementer recorded its handoff at HandoffCommit while Issue
// is in implementing with no open or merged pull request recorded: never decoded from an event.
// The issue waits in implementing for a pull request nobody is told is missing, so the workflow
// tells its architect (workflow's pullRequestMissing), once per handoff commit: the read applies it
// under an event id naming the issue and the commit (the daemon's tellMissing).
type PullRequestMissing struct {
	Issue         string
	Repo          string
	Branch        string
	HandoffCommit string
}

func (f PullRequestMissing) namedIssue() (string, bool) { return f.Issue, true }

// Push records a branch push.
type Push struct {
	Repo   string
	Branch string
	// Before is the head the push replaced, After the head it left. A push that deleted the branch
	// leaves no head: its After is record.NoCommit, which decodePush makes no fact.
	Before string
	After  string
	// Pusher is the push's pusher login (the listener's `pusher`, GitHub's pusher.name).
	Pusher string
	// Sender is the login of the account that pushed (the webhook's sender), which a synchronize's
	// sender names; empty from a listener that predates the field.
	Sender string
}

func (Push) namedIssue() (string, bool) { return "", false }

// BasePush is a push to a branch that is no issue's (decodePush), which is the shape of a branch
// pull requests sit on: a base. GitHub recomputes whether each head on that base still merges into
// it and sends no event for the pull request when it does, so a head the record holds as clean can
// already be behind or dirty; this fact is the only notice the daemon gets. The workflow finds the
// open pull requests recorded on Branch and records that their base moved to After (workflow's
// basePush), so the next read of each is known to be needed. Nothing in it is a decision: a push
// to a branch no pull request is on changes nothing.
type BasePush struct {
	Repo   string
	Branch string
	// After is the commit the push left the base branch at.
	After string
}

func (BasePush) namedIssue() (string, bool) { return "", false }

// HandoffComplete is an API-originated phase-completion fact.
type HandoffComplete struct {
	Issue string
	Role  claim.Role
	Claim claim.Token
	// Generation is the run whose task the worker took, which the daemon reads from the claim —
	// the delivery whose turn is running, or the run the claim is left serving once that turn
	// ends. A child taken back to todo can leave the prior run's worker live: its completion names
	// that prior generation and is refused. Nothing on the worker's own request tells the two apart,
	// since the claim token is stable across runs and the session is kept live. For a claim serving
	// no run the route takes the run of the task the claim is holding, when that task is of the
	// issue's own current generation and phase, and refuses a completion it can attribute to no run
	// at all, so a fact reaching the workflow always names a run.
	Generation uint64
	Summary    string
	Verdict    string
	// Commit is the commit the completion reports: for the planner, the implementer's implementing
	// round, the tester and the reviewer the head their handoff record names, empty where a
	// planner's, a tester's or a reviewer's record names none; and the commit retro and merging
	// stand on.
	Commit string
	// HandoffRecord and HandoffVersion name the Dispatch handoff record the completion of a phase
	// with a handoff ends with (phase.Handoff): the route read it back from Dispatch and held it to
	// the issue, the phase's word, this version and the claim's session before it applied the fact.
	// Empty and zero for a phase that writes none.
	HandoffRecord  string
	HandoffVersion int
	// PlanRating is the rating of the plan the issue ran, as the completion named it: the route held
	// it to the scale and to a reason (api's planRatingOf, its one producer), and nil is a
	// completion that named none. Only the completion that ends the retro carries one, and which
	// completion that is the workflow judges where it ends the retro, on the issue as the fact's
	// own transaction reads it (workflow's planRatingRefusal); it writes the rating onto the
	// retro's record as it stands.
	PlanRating *record.PlanRating
	// MustRate is whether the completion that ends the retro must carry PlanRating: the claim's
	// plugin declared the contract that added the rating (api.PlanRatingContract) or a later one,
	// whose tool takes it. The route reads it from the claim, which no state of the issue changes;
	// a plugin before that contract sends no rating, and its retro ends with none.
	MustRate bool
	// ReturnTo is the step of the issue's running task that a failed step names as the one the
	// task returns to: the place of that step in the task, 1 for its first. The route takes
	// it only on a completion whose verdict is fail, and holds it to 1 or above; which step it may
	// name is the workflow's to judge against the task's steps. 0 on every other completion, which
	// returns to the step the workflow reads from the steps' types.
	ReturnTo int
}

func (f HandoffComplete) namedIssue() (string, bool) { return f.Issue, true }

// BackwardMove asks the workflow to return to an earlier phase. By, Generation and Entry are the
// architect's request from accepting (Requester claim.RoleArchitect, api's phaseBackward): the
// claim of the architect, the issue's generation and the run of the step the issue stood in
// (record.Store.OpenStep) as its route read them, which the workflow judges again under the fact
// lock (ArchitectRequest, EnteredRequest). A phase worker's move carries none of the three.
type BackwardMove struct {
	Issue      string
	Requester  claim.Role
	To         phase.Phase
	Reason     string
	By         claim.Token
	Generation uint64
	Entry      record.StepEntry
}

func (f BackwardMove) namedIssue() (string, bool) { return f.Issue, true }

// RetryOrEscalateDecision is the architect's response to a held phase.
type RetryOrEscalateDecision string

const (
	RetryDecision    RetryOrEscalateDecision = "retry"
	EscalateDecision RetryOrEscalateDecision = "escalate"
)

// RetryOrEscalate is an API-originated response to a held phase. Reason is why the architect
// answered as it did, which a retry writes into the task the restarted role is given and into
// the daemon's own log line for the release; it is empty for a response that names none, which
// leaves the daemon's own words in both. By and Generation are the architect's retry_or_escalate
// (api's phaseRetry): its claim and the issue's generation its route read, which the workflow
// judges again under the fact lock (ArchitectRequest). The operator's retry of a held claim
// (`legion claims retry`, api's retryClaim) is no architect's request, and carries neither.
type RetryOrEscalate struct {
	Issue      string
	Decision   RetryOrEscalateDecision
	Reason     string
	By         claim.Token
	Generation uint64
}

func (f RetryOrEscalate) namedIssue() (string, bool) { return f.Issue, true }

// RestartPhase is the operator's retry of one phase worker, from `legion claims retry` of a claim
// that failed or that an operator stopped (api's retryClaim). The workflow starts Role's phase
// again, so a worker relaunched with no task — a new session above all, which has read none of the
// issue's work — is given the task of the phase it is to work. An issue held for Role is restored
// from its hold first, as the architect's RetryOrEscalate restores it; it is the operator's own
// fact rather than that one because an operator's stop leaves no hold for an architect to decide.
//
// FreshSession is `legion claims retry --fresh-session`: the start this fact queues retries the
// claim in a new session rather than resuming the one it recorded (supervise's RequestRetry). It
// rides the fact so that the session is forgotten inside the retry decision itself, which means a
// restart the workflow refuses leaves the claim's session where the operator still has it.
type RestartPhase struct {
	Issue        string
	Role         claim.Role
	FreshSession bool
}

func (f RestartPhase) namedIssue() (string, bool) { return f.Issue, true }

// SignOff records the architect's post-production-check approval: the claim of the architect, and
// the issue's generation and the run of the step it stood in (record.Store.OpenStep, the run of
// accepting) as the route read them (api's signOff), which the workflow judges again under the
// fact lock (ArchitectRequest, EnteredRequest).
type SignOff struct {
	Issue      string
	By         claim.Token
	Generation uint64
	Entry      record.StepEntry
}

func (f SignOff) namedIssue() (string, bool) { return f.Issue, true }

// GateRegistered records a design document and its current version, with what register_gate read
// of Dispatch's approval of it, so a person's review given before the architect registered is
// applied at registration. ApprovedVersion is the version Dispatch showed approved, set only when
// that approval is of Dispatch's latest version, and 0 otherwise: the gate opens at registration,
// keeping that approval's place in the issue's Dispatch event log (ApprovedMark).
// ChangesRequestedVersion is the version of a changes request Dispatch showed on its latest
// version, with its reason and the review's place in the issue's Dispatch event log
// (ChangesRequestedMark), and 0 otherwise: the gate closes with the architect told.
// DispatchVersion is the document's latest version as that read found it: Version is the caller's,
// and the gate is registered at the newer of the two. Since is the issue's event sequence read
// before the approval, with no time, so every review the read did not show comes after it
// (record.EventMark.After): a review the gate already applied after Since is newer than the read.
// RequestAsk is the approval request the read found open (dispatch.Approval.AskID, "" when none
// was), and RequestWaitsOnAgent says it waited on the agent (a reply or a newer version since it
// was handed to the person): the registration writes that request's mirror row as the read found
// it, at Since. ReviewedBy is who gave the review either version names
// (dispatch.Approval.LatestReviewer), zero with neither: the notice the registration sends names
// that person, since it goes to the controller when nobody in the tree can hear it (the daemon's
// parkDecision).
type GateRegistered struct {
	Issue string
	// Step is the design-gate step this registration records, which register_gate resolved
	// against the deployment's list and bound to ArtifactID (api's gateRegister). The zero value
	// is the step spec, as it is in the row the registration writes (record.Store.PutGate).
	Step                    record.DesignStep
	ArtifactID              string
	Version                 int
	ApprovedVersion         int
	ApprovedMark            record.EventMark
	ChangesRequestedVersion int
	ChangesReason           string
	ChangesRequestedMark    record.EventMark
	DispatchVersion         int
	Since                   record.EventMark
	RequestAsk              string
	RequestWaitsOnAgent     bool
	ReviewedBy              dispatch.Actor
	// By and Generation are the architect's register_gate (api's gateRegister): its claim and the
	// root's generation its route read, which the workflow judges again under the fact lock
	// (ArchitectRequest). A registration the workflow makes itself carries neither.
	By         claim.Token
	Generation uint64
}

func (f GateRegistered) namedIssue() (string, bool) { return f.Issue, true }

// PlanGiven is the plan the owner of an issue gave Legion with register_plan (api's
// registerPlan): the blocks of one version of its plan document, as the route read them from
// Dispatch at that version and held them to plan.Supported and plan.Validate, so a plan either
// check refuses never reaches the workflow. Generation is the issue's generation the route read,
// and 0 for an issue the daemon has not recorded, whose plan waits at generation 0 until the
// issue's first step takes the newest plan its owner gave, or, where the issue's experiment's
// group takes no plan from its owner, passes it over. GivenBy is the claim of the architect whose
// grant gave it, which is how the first step tells its owner's plan from an earlier owner's.
//
// The workflow judges the giver and the generation again in the fact's own transaction, since
// the route read them before the fact lock: a giver that owns the issue no longer is refused
// ISSUE_OUTSIDE_TREE (IssueOutsideTree), and a plan of a run the issue has left
// PLAN_STALE_GENERATION, a plan given at generation 0 among them once the daemon recorded the
// issue and its first step took a plan. It stores the plan (workflow's planGiven) where its task
// has not started, and applies the rules of a later version. It answers PLAN_REFUSED
// (RunningTaskChanged, or StartedBeforePlans for an issue that ran before Legion took plans),
// PLAN_VERSION_OLD, PLAN_DOCUMENT_CHANGED, TASK_DONE (FinishedTask, ChangeOfTask),
// PLAN_STALE_GENERATION, ISSUE_OUTSIDE_TREE, PLAN_NOT_TAKEN (an issue whose experiment's group
// takes no plan from its owner) and TREE_LINGERING, and it leaves each of those refusals unspent
// (Result.Unspent). The route names one registration by the issue, the generation, the document
// and the version, so a version refused while its task ran is the same fact when the owner gives
// it again after stop_task, and must apply then.
type PlanGiven struct {
	Issue      string
	Generation uint64
	Plan       plan.Plan
	GivenBy    claim.Token
}

func (f PlanGiven) namedIssue() (string, bool) { return f.Issue, true }

// TaskStopped is the owner's stop_task of one task of a recorded issue (api's stopTask), by the
// claim of the architect whose grant asked for it, at Generation, the issue's generation the route
// read. The workflow judges the stop again in the fact's own transaction: an architect that owns
// the issue no longer is refused ISSUE_OUTSIDE_TREE (IssueOutsideTree), and a stop of a run the
// issue has left TASK_STALE_GENERATION (TaskStaleGeneration), each left unspent. It then answers
// TaskNotStopped for every task of an issue whose experiment's group takes no plan from its owner,
// whatever state the task stands in, and for any other issue stops the task or answers
// TaskNotFound or TaskDone; a stop of a task already stopped changes nothing and is no refusal.
// Each request is its own fact, so a task the owner stops, starts again with a later
// version and stops once more is stopped twice.
type TaskStopped struct {
	Issue, Task string
	By          claim.Token
	Generation  uint64
}

func (f TaskStopped) namedIssue() (string, bool) { return f.Issue, true }

// ArchitectRequest is a fact an architect's request applies to one issue (Request). Its route
// judged the requester and read the issue before the fact lock (api's architectForIssue,
// architectOwns and planOwner), and an adoption, a new run or a re-parent can commit between that
// read and the fact. So the workflow judges the request again on the record as the fact reads it,
// before the fact's own handler reads anything (workflow's Engine.Apply): the owner, by the rule
// its route judged, then the generation. Each refusal there is left unspent.
type ArchitectRequest interface {
	Fact
	// Request is the request the fact carries; ok is false where the fact is no architect's
	// request: a phase worker's backward move, or the operator's retry.
	Request() (request Request, ok bool)
	// StaleGeneration is the refusal of the request where the issue is on generation now, and
	// not on the one its route read.
	StaleGeneration(now uint64) *Refusal
}

// EnteredRequest is an ArchitectRequest about the run of the step its issue stands in, accepting:
// sign_off and request_backward_move. The run its route read (record.Store.OpenStep) is judged
// too, so the request of an architect that read one run of accepting does not act on the next.
type EnteredRequest interface {
	ArchitectRequest
	// ReadEntry is the run of the step its route read.
	ReadEntry() record.StepEntry
	// StaleEntry is the refusal of the request where now is the run that stands.
	StaleEntry(now record.StepEntry) *Refusal
}

// Request is one architect's request on one issue as its route read it: Issue the issue the
// request names, By the claim of the architect that made it, and Generation the issue's
// generation the route read, 0 where it read no record of the issue, which only register_plan
// takes (that plan waits at generation 0 for the issue's first step). Owners is the rule of who
// owns the issue for the request, the one its route judges, and Operation the request's name, which
// a refusal names.
type Request struct {
	Issue      string
	By         claim.Token
	Generation uint64
	Owners     record.OwnerRule
	Operation  string
}

func (f PlanGiven) Request() (Request, bool) {
	return Request{Issue: f.Issue, By: f.GivenBy, Generation: f.Generation, Owners: record.OwnerOfTree, Operation: "register_plan"}, true
}

func (f PlanGiven) StaleGeneration(now uint64) *Refusal {
	return PlanStaleGeneration(f.Issue, f.Generation, now)
}

func (f TaskStopped) Request() (Request, bool) {
	return Request{Issue: f.Issue, By: f.By, Generation: f.Generation, Owners: record.OwnerOfTree, Operation: "stop_task"}, true
}

func (f TaskStopped) StaleGeneration(now uint64) *Refusal {
	return TaskStaleGeneration(f.Issue, f.Generation, now)
}

func (f SignOff) Request() (Request, bool) {
	return Request{Issue: f.Issue, By: f.By, Generation: f.Generation, Owners: record.OwnerAbove, Operation: "sign_off"}, true
}

func (f SignOff) StaleGeneration(now uint64) *Refusal {
	return staleGeneration("SIGNOFF_STALE_GENERATION", "the sign-off", f.Issue, f.Generation, now, "Read the issue again before you sign it off. This sign-off changed nothing.")
}

func (f SignOff) ReadEntry() record.StepEntry { return f.Entry }

func (f SignOff) StaleEntry(now record.StepEntry) *Refusal {
	return staleEntry("SIGNOFF_STALE_ENTRY", "the sign-off", f.Issue, f.Entry, now, "Read the evidence of the run that stands before you sign it off. This sign-off changed nothing.")
}

// Request is the architect's move from accepting alone (Requester claim.RoleArchitect): a phase
// worker's move is judged by its role (workflow's backwardRefusal).
func (f BackwardMove) Request() (Request, bool) {
	return Request{Issue: f.Issue, By: f.By, Generation: f.Generation, Owners: record.OwnerAbove, Operation: "request_backward_move"},
		f.Requester == claim.RoleArchitect
}

func (f BackwardMove) StaleGeneration(now uint64) *Refusal {
	return staleGeneration("BACKWARD_STALE_GENERATION", "the backward move", f.Issue, f.Generation, now, "Read the issue again before you move it back. This move changed nothing.")
}

func (f BackwardMove) ReadEntry() record.StepEntry { return f.Entry }

func (f BackwardMove) StaleEntry(now record.StepEntry) *Refusal {
	return staleEntry("BACKWARD_STALE_ENTRY", "the backward move", f.Issue, f.Entry, now, "Read the run that stands before you move it back. This move changed nothing.")
}

// Request is the architect's retry_or_escalate; the operator's retry carries no claim (By "").
func (f RetryOrEscalate) Request() (Request, bool) {
	return Request{Issue: f.Issue, By: f.By, Generation: f.Generation, Owners: record.OwnerAbove, Operation: "retry_or_escalate"}, f.By != ""
}

func (f RetryOrEscalate) StaleGeneration(now uint64) *Refusal {
	return staleGeneration("RETRY_STALE_GENERATION", "the retry_or_escalate", f.Issue, f.Generation, now, "Read the issue again before you decide its hold. This decision changed nothing.")
}

// Request is the root architect's close_root (api's closeRoot), which only the root's own
// architect makes (record.ArchitectAbove); a fact that names no claim is none of an architect's
// request.
func (f CloseRoot) Request() (Request, bool) {
	return Request{Issue: f.Issue, By: f.By, Generation: f.Generation, Owners: record.OwnerAbove, Operation: "close_root"}, f.By != ""
}

// Request is the root architect's register_gate (api's gateRegister), which only the root's own
// architect makes (record.ArchitectAbove); a registration the workflow makes itself names no claim
// and is no architect's request.
func (f GateRegistered) Request() (Request, bool) {
	return Request{Issue: f.Issue, By: f.By, Generation: f.Generation, Owners: record.OwnerAbove, Operation: "register_gate"}, f.By != ""
}

func (f CloseRoot) StaleGeneration(now uint64) *Refusal {
	return staleGeneration("CLOSE_STALE_GENERATION", "the close", f.Issue, f.Generation, now, "Read the root again before you close it. This close changed nothing.")
}

func (f GateRegistered) StaleGeneration(now uint64) *Refusal {
	return staleGeneration("GATE_STALE_GENERATION", "the registration", f.Issue, f.Generation, now, "Register the document again for the run that stands. This registration changed nothing.")
}

// CloseRoot is a tree root's architect ending its tree before the tree's first phase starts, with
// the reason it gives: a human decided no change at the design gate, or the issue is moot. By and
// Generation are the architect's claim and the root's generation its route read (api's
// closeRoot), which the workflow judges again under the fact lock (ArchitectRequest).
type CloseRoot struct {
	Issue      string
	Reason     string
	By         claim.Token
	Generation uint64
}

func (f CloseRoot) namedIssue() (string, bool) { return f.Issue, true }

// Message is what the close posts on the issue: that its architect closed it before its first
// phase, and the reason it gave.
func (f CloseRoot) Message() string {
	return "Closed by its architect before its first phase: " + f.Reason
}

// ClaimFailed is the supervision terminal failure observation. Reason is the persisted cause when
// the supervisor has one, so workflow notices can say what failed after a daemon restart. Death is
// why the claim's process was last found gone and StartFailure why its last start did not produce
// a running agent, each as the runtime said it and empty when the claim has none: the two answer
// different questions — a worker that kept dying and a worker that could never start — and the
// failure record a notice of this fact carries, where the fact names a Cause, holds both whole,
// since the architect reading it has no other way to learn either (dispatch://INFRA-714 R6;
// workflow's noticeFailure). A worker-died's reason names neither whole: it names the first line
// of Death only where its notice carries no failure record, Cause is supervise.CauseDied and no
// PodEnd was read, which is the tree architect's notice alone, and StartFailure nowhere
// (workflow's deathReason).
//
// PodEnd is the pod end the claim's process was last observed to have died of, as the runtime
// read it off the pod itself and the claim kept it (supervise.Claim.PodEnd,
// runtime.Observation.PodEnd): `Evicted: The node was low on resource: ephemeral-storage. ...`
// and nothing of the role's own log. It is carried beside Reason and never folded into it,
// because the two say different things — Reason is what the supervisor decided, PodEnd what the
// cluster did — and because it is persisted on the claim, so the boot's replay of a failure no
// daemon applied reports the same end (the daemon's replayTerminal).
type ClaimFailed struct {
	Issue        string
	Role         claim.Role
	Reason       string
	Death        string
	StartFailure string
	PodEnd       string
	// Cause is the one word for what failed the claim, chosen by the site that failed it and
	// carried beside Reason's sentence (supervise.FailureCause, Claim.FailureCause): one sentence
	// covers several events, and a notice that names the word says which of them this was. It is
	// "" for a claim whose failure the daemon recorded no cause for.
	Cause supervise.FailureCause
	// Budgets are the claim's retry counters as the failure left them, and Limits the
	// deployment's, where each of those counters runs out. Both travel on the fact because the
	// workflow engine holds neither the claim nor the configuration the limits are built from:
	// the daemon takes them from the supervisor it supervises under (workflowRuntime.attach) and
	// fills them in where the fact is built (applyTerminal).
	Budgets supervise.Budgets
	Limits  supervise.Limits
}

func (f ClaimFailed) namedIssue() (string, bool) { return f.Issue, true }

// SessionLost is a claim that forgot its recorded session: its worker now runs a fresh agent that
// has read none of its own earlier work (supervise's startFresh). Detail is why, as the supervisor
// said it. It is told on the issue, to its architect and to the controller, because a worker that
// silently lost its memory looks like one that is merely slow.
//
// Cause is why the claim forgot the session; empty reads as gone (supervise.SessionLossGone).
// Session is the session the claim forgot (supervise.AbandonedSession.Session), which the
// workflow names in the words of a cause whose message does not quote Detail.
type SessionLost struct {
	Issue   string
	Role    claim.Role
	Detail  string
	Cause   supervise.SessionLoss
	Session string
}

func (f SessionLost) namedIssue() (string, bool) { return f.Issue, true }

// SessionStoreFault is a start that could not reach the session store at all, which is a
// deployment fault rather than the claim's: nothing of the claim is lost and nothing is charged,
// and the start is tried again every minute until the store answers (supervise's
// startWaitsForStore). It is told on the issue and to the controller at the first such start and
// hourly while it lasts, since no worker of the issue runs meanwhile.
type SessionStoreFault struct {
	Issue  string
	Role   claim.Role
	Detail string
}

func (f SessionStoreFault) namedIssue() (string, bool) { return f.Issue, true }

// MemoryGuardKill is one process an issue pod's memory guard ended with SIGKILL because the pod
// was near its memory limit, or, in a microVM pod, one that the pod's own kernel stopped because
// the pod ran out of memory (the report's Cause, shimwire.MemoryKillCauseKernel, or
// shimwire.MemoryKillCauseKernelProtected for a process that runs the role's session), as the pod's
// launcher reported it on the connection of the role whose process it was
// (shimwire.LauncherMemoryKill). It is never decoded from an event: the launcher's frame reaches
// the runtime, and the daemon's Kubernetes wiring applies one fact per report under the id
// `memory-guard-kill:<guard>:<sequence>`, which names the kill once, so a report the launcher sent
// twice is applied once (INFRA-3557). For the kernel's kill, <guard> is the id of the launcher's
// guest watch (memoryguard.Guest).
//
// Issue, Token and PodUID are the launcher session's own (the issue its pod is of, the claim of
// the role the connection authenticated for, and that pod), never anything the report says.
// The workflow tells that role, on its own role topic, which of its processes was ended and why,
// in the note the report's Cause chooses (workflow's memoryGuardKill).
type MemoryGuardKill struct {
	Issue  string
	Token  claim.Token
	PodUID string
	Report shimwire.LauncherMemoryKill
}

func (f MemoryGuardKill) namedIssue() (string, bool) { return f.Issue, true }

// ControllerMemoryGuardKill is one process the memory guard of a gVisor controller pod ended with
// SIGKILL because the pod was near its memory limit, or one that a microVM controller pod's own
// kernel stopped because the pod ran out of memory (the report's Cause,
// shimwire.MemoryKillCauseKernel, or shimwire.MemoryKillCauseKernelProtected for a process that
// runs the controller's session), as that pod's launcher reported it on the controller's
// connection (shimwire.LauncherMemoryKill). It is never decoded from an event: the launcher's
// frame reaches the runtime, and the daemon's Kubernetes wiring applies one fact per report under
// the id MemoryGuardKill's are applied under, `memory-guard-kill:<guard>:<sequence>`, which names
// the kill once, so a report the launcher sent twice is applied once. For the kernel's kill,
// <guard> is the id of the launcher's guest watch (memoryguard.Guest).
//
// Token and PodUID are the launcher session's own (the claim the connection authenticated for and
// its pod), never anything the report says. The controller's pod is of no issue, so the fact names
// none. The workflow tells the project's controller, in a controller notice of its own for each
// kill, which of its programs was ended and why, in the note the report's Cause chooses
// (workflow's controllerMemoryGuardKill).
type ControllerMemoryGuardKill struct {
	Token  claim.Token
	PodUID string
	Report shimwire.LauncherMemoryKill
}

func (ControllerMemoryGuardKill) namedIssue() (string, bool) { return "", false }

// VolumeNearFull is one sweep of an issue pod's volume as its launcher reported it: the cache
// trimmer in that pod's launcher process swept the cache roots and the volume was still above
// its low mark when it stopped, or the trimmer has switched itself off after three faults. It is
// never decoded from an event — the launcher's frame reaches the runtime, and the daemon's
// Kubernetes wiring applies one fact per report under an id of its own (INFRA-2009).
//
// Issue is the issue whose pod reported, PodUID the pod the launcher is authenticated for, and
// Report everything the sweep said, as the record keeps it. The workflow's handler decides from
// the daemon's bounds whether the issue's architect is told (workflow's volumeNearFull): two
// reports of one sweep are two facts, so the id the caller applies it under must be unique per
// report.
type VolumeNearFull struct {
	Issue  string
	PodUID string
	Report record.VolumeNearFull
}

func (f VolumeNearFull) namedIssue() (string, bool) { return f.Issue, true }

// ClaimReady is a claim ready to be prompted, from either of two sources: the supervision
// observation of a launch's ready, its agent having taken its Envoy role and said it can be
// prompted (workflowRuntime.applyTerminal), and a start the outbox runs that finds an architect
// already running, whose launch has no second ready (the outbox's start). Launch is the claim's
// launch generation the ready belongs to, the running one for the second source.
type ClaimReady struct {
	Issue  string
	Role   claim.Role
	Launch uint64
	// Paced is the second source alone saying that the catch-up this ready writes stands in place
	// of a launch, and so is a wake the daemon's pacing holds to its turn like the launch it
	// replaces (workflow.Config.Reserve). The launch's own ready sets none: that launch took its
	// turn at the machine's gate (supervise's reserveWake) and the catch-up it leads to is part of
	// the same wake. A fact applies once for the row that wrote it (ApplyFact's id), so the turn
	// is taken once per start row however often the row runs.
	Paced bool
	// Relaunch is the sentence that tells the claim's new process what the last let-go of its
	// process ended (supervise.RelaunchSentence), while the claim's record of it is untold
	// (supervise.EndedWork.Tells); "" otherwise. A record that still tells is one of the claim's
	// current session's processes, since abandoning a session ends the record of its process
	// (supervise's forgetSession). The first source alone sets it, from the claim its ready wrote
	// (the daemon's applyTerminal): an architect's catch-up opens with it (workflow's
	// QueueCatchUp), because the first turn of a relaunched architect can be that catch-up's
	// rather than its prompt's. The catch-up's copy tells nothing: the record is told by the
	// prompt the claim's machine sends, at that prompt's confirmation.
	Relaunch string
}

func (f ClaimReady) namedIssue() (string, bool) { return f.Issue, true }

// TaskConfirmed is a role's agent taking the work the daemon gave it: a turn of its pending
// delivery started (supervise's confirmCarrying). The workflow changes nothing on it; admission's
// pass rule (admit.Admission.Apply) runs the pass on it whatever it wrote, since it stands for the
// confirmation the supervisor wrote outside the fact, so a root whose wait was held because its
// architect had not yet taken its task is decided here rather than at the next tick (workflow's
// ReconcileAwaiting).
type TaskConfirmed struct {
	Issue      string
	Role       claim.Role
	Generation uint64
}

func (f TaskConfirmed) namedIssue() (string, bool) { return f.Issue, true }

// GateRequestFound is the boot catch-up's read of the approval request Dispatch shows open on a
// root's registered spec document (dispatch.Approval.AskID), for a gate whose document the asks'
// mirror holds no approval request of: a gate a daemon of an earlier release registered before the
// mirror existed, or one whose request the daemon never saw an event of. Since is the issue's
// Dispatch sequence read before the approval, and WaitsOnAgent says the request waited on the
// agent. The workflow writes the request's mirror row as the read found it, at Since, when it holds
// none; when the gate is closed and armed and the request waits on the person, that is the
// architect's hand-back of the spec, declared as a registration declares it.
type GateRequestFound struct {
	Issue        string
	Generation   uint64
	ArtifactID   string
	RequestAsk   string
	WaitsOnAgent bool
	Since        int64
}

func (f GateRequestFound) namedIssue() (string, bool) { return f.Issue, true }

// CatchUpApplied is the boot catch-up of a root finishing: every event of its Dispatch event log
// up to Through (0 when the catch-up applied nothing and moves no cursor) has been applied. The
// workflow raises the root's catch-up cursor to Through; admission, which held the root out of its
// decision while its window was applied, decides it at this fact, once.
type CatchUpApplied struct {
	Issue   string
	Through int64
}

func (f CatchUpApplied) namedIssue() (string, bool) { return f.Issue, true }

// LingerExpired records the generation whose post-close linger elapsed.
type LingerExpired struct {
	Issue      string
	Generation uint64
}

func (f LingerExpired) namedIssue() (string, bool) { return f.Issue, true }

// DaemonBooted records a boot of the daemon: every stored claim has its machine again, and the
// boot replayed what its claims' terminal states owe the workflow. Boot is the boot's record id
// (daemon_boot), which makes each boot's fact its own.
type DaemonBooted struct {
	Boot int64
}

func (DaemonBooted) namedIssue() (string, bool) { return "", false }

// CutoverMergerRepair is the one-time boot repair of the mergers the `awaiting_merge` cutover
// left stopped (dispatch://INFRA-3274/spec R2): an earlier build's idle release stopped the
// merger's pod while its issue waited in `awaiting_merge`, which no role worked, so it armed no
// backstop, and the migration that moved the issue to `merging` gave the merger no task. It is
// never decoded from an event. The boot applies it once per Dispatch project, under a fixed
// event id (`cutover-merger-repair:<project>`), before the consumers start, so processed_events
// is its once-only record: a later boot's apply is a duplicate, and no handler runs.
type CutoverMergerRepair struct{}

func (CutoverMergerRepair) namedIssue() (string, bool) { return "", false }

// CheckReport is the checking role's report on one look of its closed issue's production check:
// the `legion` tool's `handoff_complete` on an issue whose check is open (api/handoff.go),
// carrying no commit. Verdict is pass, pending or fail; Summary is what the agent saw. Look is
// the look the route read as the check's current one, so a report of a look its bound or a hold
// already ended is refused rather than recorded against the look after it. WakeAt is when the
// agent asks for its next look, which a pending on a look that is not the check's last carries
// and no other report does (the route holds it to that: CHECK_WAKE_REQUIRED,
// CHECK_WAKE_UNEXPECTED); the engine clamps it and writes the check's one wake row for it.
type CheckReport struct {
	Issue   string
	Claim   claim.Token
	Look    int
	Verdict string
	Summary string
	WakeAt  *time.Time
}

func (f CheckReport) namedIssue() (string, bool) { return f.Issue, true }

// CheckWakeDue is the wake of issue Issue's production check coming due (the check_wake row at
// ProductionCheck.WakeAt): the next look starts, woken by Reason — what the checking agent named
// its own next look for on a pending report, or the retry of a look nobody reported. Look is the
// look the check stood at when the wake was named, so a row of a look the check has since left
// starts nothing, and OpenedAt the check instance it was named for, so a row of a check a later
// merge of the same issue replaced — whose looks count from one again — starts nothing either; a
// zero OpenedAt names none. WakeAt is the wake the row was queued for, so a row whose wake the
// check no longer records — a failure between looks named the next look later — starts nothing;
// a zero WakeAt, a row a release before the field queued, is not fenced.
type CheckWakeDue struct {
	Issue    string
	Look     int
	OpenedAt time.Time
	WakeAt   time.Time
	Reason   string
}

func (f CheckWakeDue) namedIssue() (string, bool) { return f.Issue, true }

// CheckWakeOf is the fact a check_wake row of issue applies when it comes due (the daemon's
// outbox), made here alone so that every field the row carries reaches the fact.
func CheckWakeOf(issue string, wake record.CheckWakeDue) CheckWakeDue {
	return CheckWakeDue{Issue: issue, Look: wake.Look, OpenedAt: wake.OpenedAt, WakeAt: wake.WakeAt, Reason: wake.Reason}
}

// CheckLookEnded is a look of issue Issue's production check ending with no report: Cause is
// `look bound` (the check_look row due at the look's bound), `operator hold` (a check start the
// outbox met an operator's suspension on) or `claim failed` (the implementer's claim failed in
// the look). Look names the look, so a row of a look already ended changes nothing, and OpenedAt
// the check instance (its OpenedAt), so a row of a check a later merge of the same issue replaced
// — whose looks count from one again — changes nothing either; a zero OpenedAt names none.
type CheckLookEnded struct {
	Issue    string
	Look     int
	Cause    string
	OpenedAt time.Time
}

func (f CheckLookEnded) namedIssue() (string, bool) { return f.Issue, true }

// CheckBound is the whole production check's bound of issue Issue coming due (the check_bound
// row at its bound_at): the check's last look starts, or the bound defers while a look is open.
type CheckBound struct {
	Issue string
}

func (f CheckBound) namedIssue() (string, bool) { return f.Issue, true }

// ListenerWake is one report the Envoy listener publishes on notifications.legion.wake, the one
// subject whose consumer every daemon runs: the listener alone sees a message published to a
// role topic with no live holder and a message sent to the agent subject of a session that is
// gone, and both must bring a released pod back. The listener does not know which daemon owns a
// session and must not learn: it reports the role token or the session id it saw, every daemon
// reads the report, and each acts only on a released claim of its own project.
//
// Kind is ListenerWakeRole, ListenerWakeAgent or ListenerWakeReady. Role is the claim token of
// the role topic a message was published to, Session the session a message was sent to, and At
// the listener's own clock at the report, because the stream's store time is the NATS server's.
// A ListenerWakeReady report names neither a role nor a session: it is the positive liveness
// signal R7's gate reads, published at the listener's start and every five minutes after it.
type ListenerWake struct {
	Kind    string
	Role    string
	Session string
	At      time.Time
	// Report is the listener's own id for this report, which makes a redelivered report the same
	// wake rather than a second one.
	Report string
}

func (ListenerWake) namedIssue() (string, bool) { return "", false }

// The kinds a ListenerWake report carries, as the listener writes them on the wire.
const (
	ListenerWakeRole  = "role_message"
	ListenerWakeAgent = "agent_message"
	ListenerWakeReady = "listener_ready"
)

// PodBackstopDue is one armed backstop of a released pod coming due (the pod_wake_backstops row
// at its due_at): the phase worker it names is woken whatever else did or did not happen since
// the release. It is never decoded from an event — the daemon's own pass reads the due rows and
// applies one fact per row, under the row's id, so a backstop that came due while the daemon was
// down fires once after it starts.
type PodBackstopDue struct {
	ID    int64
	Issue string
	Role  claim.Role
}

func (f PodBackstopDue) namedIssue() (string, bool) { return f.Issue, true }

// WorkerSilent is one claim the daemon's minute sweep read as a silent phase worker: up and
// between turns, in no update round, its last step older than Bound (config.Config.WorkerSilence),
// and its session's report explaining none of that quiet. The sweep applies it from the live
// claims, never from an event, and its handler decides in the fact's own transaction
// (internal/daemon's tellSilentPhaseWorkers says why, workerSilentEventID what its id is).
type WorkerSilent struct {
	Issue      string
	Role       claim.Role
	Generation uint64
	LastStep   time.Time
	Bound      time.Duration
}

func (f WorkerSilent) namedIssue() (string, bool) { return f.Issue, true }

// WorkerBackgroundStalled is one claim the daemon's minute sweep read as a phase worker waiting
// on background work of its own (Kinds) whose session's report, at ReportedAt, is older than
// Bound (config.Config.WorkerBackground). The sweep applies it from the live claims, never from
// an event, and its handler tells the architect and sends nothing (internal/daemon's
// workerBackgroundEventID says what its id is).
type WorkerBackgroundStalled struct {
	Issue      string
	Role       claim.Role
	Generation uint64
	ReportedAt time.Time
	Kinds      []string
	Bound      time.Duration
}

func (f WorkerBackgroundStalled) namedIssue() (string, bool) { return f.Issue, true }

// Handler changes the workflow record inside the caller-owned transaction.
type Handler interface {
	Apply(context.Context, pgx.Tx, Fact) (Result, error)
}

// Result carries a refusal that is durable: handlers finish and the transaction commits. Duplicate
// reports that the event id was already processed, so no handler ran and nothing changed.
//
// Unspent is a handler leaving the event unmarked, so the same id applies again when it is next
// sent (ApplyFact). A handler sets it only for a refusal whose cause can clear while the fact's
// id stays the same, and writes nothing where it sets it.
type Result struct {
	Refusal   *Refusal
	Duplicate bool
	Unspent   bool
}

// Refusal is a committed API response or JetStream log record, never a transaction failure.
// Reasons is set on a refused plan alone (PlanRefused): one reason for each cause, each naming
// its task where it has one, which the route writes beside the code; every other refusal is its
// code and its sentence, and carries none.
type Refusal struct {
	Status  int
	Code    string
	Message string
	Reasons []plan.Refusal
}

// PlanRefused is the 409 PLAN_REFUSED refusal of a plan the owner gave, with one reason for each
// cause: register_plan answers it for a plan that plan.Supported or plan.Validate refuses, and
// the workflow for a version that changes or removes a running task (RunningTaskChanged). The
// message holds every reason in its order, so a reader of the sentence alone reads every cause.
func PlanRefused(reasons []plan.Refusal) *Refusal {
	texts := make([]string, len(reasons))
	for i, reason := range reasons {
		texts[i] = reason.Reason
	}
	return &Refusal{Status: 409, Code: "PLAN_REFUSED", Message: "Legion refused this plan and recorded nothing: " + strings.Join(texts, " "), Reasons: reasons}
}

// RunningTaskChanged is the reason a later version of the plan is refused when it changes or
// removes task, which is running: the owner stops it first.
func RunningTaskChanged(task string) plan.Refusal {
	return plan.Refusal{Task: task, Reason: fmt.Sprintf("task %s is running: stop it with stop_task before you change it.", task)}
}

// PlanVersionOld is the 409 PLAN_VERSION_OLD refusal of version given of issue's plan document,
// at or below held, the version in force: Legion takes only a later version.
func PlanVersionOld(issue string, held, given int) *Refusal {
	return &Refusal{Status: 409, Code: "PLAN_VERSION_OLD", Message: fmt.Sprintf(
		"the plan in force of %s is version %d, and Legion takes only a later version: version %d changed nothing.", issue, held, given)}
}

// PlanDocumentChanged is the 409 PLAN_DOCUMENT_CHANGED refusal of a version of another document
// than artifact, the document of issue's plan in force.
func PlanDocumentChanged(issue, artifact string) *Refusal {
	return &Refusal{Status: 409, Code: "PLAN_DOCUMENT_CHANGED", Message: fmt.Sprintf(
		"the plan in force of %s is document %s; give a later version of that document.", issue, artifact)}
}

// GroupClause is the one text of a group: the sentence that names the variant of the experiment
// issue was drawn into (group, the issue's row of the experiment that sets how it is done),
// whose issues run the project's default steps, the one group of this release that takes no plan
// from an issue's owner (workflow's takesNoPlan). The refusals of register_plan and stop_task
// (PlanNotTaken, TaskNotStopped) and the plan-not-taken notice of the issue's first step each
// call it, and no other text states a group.
func GroupClause(issue string, group record.Assignment) string {
	return fmt.Sprintf("%s is in the variant %s of the experiment %s, whose issues run the project's default steps.", issue, group.Variant, group.Experiment)
}

// PlanNotTaken is the 409 PLAN_NOT_TAKEN answer to a plan given for issue, whose group (its row
// of the experiment that sets how it is done) names the arm's `plan` `default`: the experiment
// decides how the issue is done, so Legion takes no plan from its owner, and a plan it stored and
// did not carry out would be dropped in silence. It is no refusal of a plan's content, so it
// carries no reasons and its code is not PLAN_REFUSED. The workflow leaves the fact unspent, as
// each refusal of a plan, so the same registration sent again gets this same answer.
func PlanNotTaken(issue string, group record.Assignment) *Refusal {
	return &Refusal{Status: 409, Code: "PLAN_NOT_TAKEN", Message: GroupClause(issue, group) +
		" Legion takes no plan from its owner for this issue. This plan changed nothing."}
}

// TaskNotStopped is the 409 PLAN_NOT_TAKEN refusal of stop_task for a task of issue, whose group
// names the arm's `plan` `default`: the experiment decides how the issue is done, so Legion takes
// no stop of one of its tasks from its owner either, and the code is the one register_plan
// answers for the same issue.
func TaskNotStopped(issue string, group record.Assignment) *Refusal {
	return &Refusal{Status: 409, Code: "PLAN_NOT_TAKEN", Message: GroupClause(issue, group) +
		" Legion takes no plan from its owner for this issue, so its owner stops none of its tasks. This stop changed nothing."}
}

// StartedBeforePlans is the reason a plan is refused for issue, which the daemon recorded with no
// plan rows at its generation and whose work has started: it ran before Legion took plans, and it
// walks today's line to its end. It names no task, and it does not end with "This release
// of Legion does not carry it out yet.", since no later release gives such a run a plan.
func StartedBeforePlans(issue string) plan.Refusal {
	return plan.Refusal{Reason: fmt.Sprintf(
		"%s started its work before Legion took plans, so it runs the steps it started with to their end. This plan changed nothing.", issue)}
}

// PlanStaleGeneration is the 409 PLAN_STALE_GENERATION refusal of a plan given at generation given
// of issue, which is on generation now: the route read the issue before its generation moved, or
// read it unrecorded (given 0) before the daemon recorded it and its first step took a plan, so
// the plan was given for a run that is over or that started without it.
func PlanStaleGeneration(issue string, given, now uint64) *Refusal {
	return &Refusal{Status: 409, Code: "PLAN_STALE_GENERATION", Message: fmt.Sprintf(
		"the plan was given for generation %d of %s, and %s is on generation %d now. Give the plan again. This plan changed nothing.", given, issue, issue, now)}
}

// TaskStaleGeneration is the 409 TASK_STALE_GENERATION refusal of a stop of a task of issue asked
// at generation given, where issue is on generation now: the route read the issue before its
// generation moved, so the stop names a task of a run that is over, and a task of the new run has
// the same id where its plan gives it one.
func TaskStaleGeneration(issue string, given, now uint64) *Refusal {
	return staleGeneration("TASK_STALE_GENERATION", "the stop", issue, given, now, "Ask for the stop again if the task of this run must stop. This stop changed nothing.")
}

// staleGeneration is the 409 refusal code of request, an architect's request on issue that its
// route read at generation given, where issue is on generation now (ArchitectRequest's
// StaleGeneration), and then, what the architect does.
func staleGeneration(code, request, issue string, given, now uint64, then string) *Refusal {
	return &Refusal{Status: 409, Code: code, Message: fmt.Sprintf(
		"%s was asked for generation %d of %s, and %s is on generation %d now. %s", request, given, issue, issue, now, then)}
}

// staleEntry is the 409 refusal code of request, an architect's request on issue that its route
// read at the run given of the step the issue stood in, where the run now stands
// (EnteredRequest's StaleEntry), and then, what the architect does.
func staleEntry(code, request, issue string, given, now record.StepEntry, then string) *Refusal {
	return &Refusal{Status: 409, Code: code, Message: fmt.Sprintf(
		"%s was asked of %s of %s, and %s stands at %s now. %s", request, describeEntry(given), issue, issue, describeEntry(now), then)}
}

// describeEntry names a run of a step as a refusal does.
func describeEntry(entry record.StepEntry) string {
	if entry == (record.StepEntry{}) {
		return "no run of a step"
	}
	return fmt.Sprintf("run %d of the %s at place %d of task %s", entry.Starts, entry.Place.Kind, entry.Place.Place, entry.Place.Task)
}

// IssueOutsideTree is the 403 ISSUE_OUTSIDE_TREE refusal of operation, a request that architect
// made as an owner of issue, where architect is no architect of the issue's tree when the workflow
// applies it (record.ArchitectOfTree): an adoption moved the issue to another tree, or the member
// whose architect it is left the issue's tree, after the route judged the request. The code is the
// one the route answers for an issue outside the architect's tree.
func IssueOutsideTree(issue string, architect claim.Token, operation string) *Refusal {
	return &Refusal{Status: 403, Code: "ISSUE_OUTSIDE_TREE", Message: fmt.Sprintf(
		"%s is outside the architect tree now: %s is no architect of its tree, so %s changed nothing.", issue, architect, operation)}
}

// IssueNotOwned is the 403 ISSUE_NOT_OWNED refusal of operation, which only an issue's own
// architect or one above it in its tree makes (record.ArchitectAbove), where architect is neither
// when the workflow applies it: a person moved the issue under another issue of the tree after the
// route judged the request. The code is the one the route answers for that rule (api's
// architectOwns).
func IssueNotOwned(operation, issue string, architect claim.Token) *Refusal {
	return &Refusal{Status: 403, Code: "ISSUE_NOT_OWNED", Message: fmt.Sprintf(
		"%s of %s belongs to its own architect or one above it in the tree, and %s is neither now, so %s changed nothing.", operation, issue, architect, operation)}
}

// TaskNotFound is the 404 TASK_NOT_FOUND refusal of stop_task for task, which the plan Legion
// runs for issue does not hold; tasks are the ids that plan holds, none for an issue with no plan
// rows.
func TaskNotFound(issue, task string, tasks []string) *Refusal {
	message := fmt.Sprintf("%s has no plan of tasks in Legion's record, so it has no task %s. This stop changed nothing.", issue, task)
	if len(tasks) > 0 {
		message = fmt.Sprintf("the plan Legion runs for %s has no task %s; its tasks are %s. This stop changed nothing.", issue, task, strings.Join(tasks, ", "))
	}
	return &Refusal{Status: 404, Code: "TASK_NOT_FOUND", Message: message}
}

// TaskAct is what a call asks of a task, as its TASK_DONE refusal names it (FinishedTask).
type TaskAct int

const (
	// ChangeOfTask is a version of the plan that changes the task (register_plan).
	ChangeOfTask TaskAct = iota
	// StopOfTask is the owner's stop of the task (stop_task).
	StopOfTask
)

// words is what act does to a task, as the refusal says nothing of the task is left to do, and the
// call that changed nothing.
func (act TaskAct) words() (does, call string) {
	if act == StopOfTask {
		return "stop", "stop"
	}
	return "change", "plan"
}

// FinishedTask is the 409 TASK_DONE refusal of act on task of issue, whose state is done or left
// (record.TaskDone, record.TaskLeft), and nil for a task in any other state. A task that is done
// has nothing of it left, and one that left the workflow with its issue before it was done is
// never moved again, so each refuses every act with the one code, the code of a stop of a done
// task. Every refusal of a finished task is made here, so the workflow judges the state once for
// each act: a version of the plan (versionRefusal) and the owner's stop (taskStopped).
func FinishedTask(issue, task string, state record.TaskState, act TaskAct) *Refusal {
	var stands string
	switch state {
	case record.TaskDone:
		stands = "is done"
	case record.TaskLeft:
		stands = "left the workflow with its issue before it was done"
	default:
		return nil
	}
	does, call := act.words()
	return &Refusal{Status: 409, Code: "TASK_DONE", Message: fmt.Sprintf(
		"task %s of %s %s, so nothing of it is left to %s. This %s changed nothing.", task, issue, stands, does, call)}
}

// TaskAwaitsPerson is the 409 TASK_AWAITING_PERSON refusal of stop_task for task of issue, a root
// that waits on a person before its first step (phase.AwaitingPerson): the record holds no hold
// from that phase, so the task cannot stop there (workflow's stopRefusal). A task that has not
// started takes a later version of its plan, and the stop is taken once the person has answered.
func TaskAwaitsPerson(issue, task string) *Refusal {
	return &Refusal{Status: 409, Code: "TASK_AWAITING_PERSON", Message: fmt.Sprintf(
		"%s waits on a person before its first step, and Legion cannot stop task %s there. To change the task, give a later version of the plan with register_plan; to stop it, call stop_task again once the person has answered. This stop changed nothing.", issue, task)}
}

// RetryOfStoppedTask is the 409 TASK_STOPPED refusal of a release of issue's hold — the
// architect's retry or escalation, the operator's restart — where the issue is held because task
// was stopped for reason, the stop reason the task stores (record.PlanTask.StoppedReason): by its
// owner, or by a failed step with no step before it to take the work back (record.HoldTaskStopped).
// Its first sentence is that reason (stoppedSentence). A later version of the plan starts the task
// again, and so does a person's todo on a root (workflow's releaseHeldRoot), but no retry of the
// hold does.
func RetryOfStoppedTask(issue, task, reason string) *Refusal {
	return &Refusal{Status: 409, Code: "TASK_STOPPED", Message: fmt.Sprintf(
		"%s; a later version of the plan that changes the task starts it again at its first step (register_plan), and a task that never started waits for its gate and its line first. This retry changed nothing.", stoppedSentence(issue, task, reason))}
}

// stoppedSentence is the sentence that says why task of issue was stopped, from reason, the stop
// reason the task stores: the reason itself where it names the task already, as an owner's stop
// does, and otherwise that the task was stopped for it, as a failed step's stop does.
func stoppedSentence(issue, task, reason string) string {
	if strings.HasPrefix(reason, fmt.Sprintf("task %s of %s ", task, issue)) {
		return reason
	}
	return fmt.Sprintf("task %s of %s was stopped: %s", task, issue, reason)
}

// RetryOfHeldParent is the 409 HELD_FOR_CHILDREN refusal of a release of issue's hold where issue,
// a parent whose last step is not the owner's acceptance, waits before its retro for the issues beneath it,
// beneath (record.HoldForChildren): the last of them to leave starts the retro.
func RetryOfHeldParent(issue string, beneath []string) *Refusal {
	return &Refusal{Status: 409, Code: "HELD_FOR_CHILDREN", Message: HeldParentWaitsForChildren(issue, beneath) + " This retry changed nothing."}
}

// HeldParentWaitsForChildren says why issue, a parent held before its retro for the issues beneath
// it (record.HoldForChildren), still waits: beneath, the issues beneath it still open, the last of
// which to leave starts the retro. It says what is true of the issue for whoever reads it, a
// person beside a set-back todo included, and carries none of the architect's rules for ending a
// child, which the architect's own retro-held notice gives.
func HeldParentWaitsForChildren(issue string, beneath []string) string {
	return fmt.Sprintf("%s waits before its retro for these issues beneath it: %s. Legion starts the retro when the last one leaves.", issue, strings.Join(beneath, ", "))
}

// RetryOfHeldParentUntilTreeRuns is the 409 HELD_FOR_CHILDREN refusal of a release of issue's hold
// where issue, a parent held before its retro for the issues beneath it (record.HoldForChildren),
// has none of them open, so its retro waits only for its tree to run (HeldParentWaitsForTree).
func RetryOfHeldParentUntilTreeRuns(issue, tree string, lingers bool) *Refusal {
	return &Refusal{Status: 409, Code: "HELD_FOR_CHILDREN", Message: HeldParentWaitsForTree(issue, tree, lingers) + " This retry changed nothing."}
}

// HeldParentWaitsForTree says why issue, a parent held before its retro for the issues beneath it
// with none of them open, still waits: its retro waits only for its tree, tree, to run. The tree
// lingers after its root left the workflow, where lingers says so, and otherwise its root waits in
// the line for the tree to be opened. The reopen or the promotion that runs the tree starts the
// retro.
func HeldParentWaitsForTree(issue, tree string, lingers bool) string {
	waits := fmt.Sprintf("its tree %s waits in the line to be opened, and Legion starts the retro once the tree runs", tree)
	if lingers {
		waits = fmt.Sprintf("its tree %s lingers after its root left the workflow, and Legion starts the retro once the tree runs again", tree)
	}
	return fmt.Sprintf("%s waits before its retro, and nothing beneath it is open: %s.", issue, waits)
}

// ChildInOtherTree is the 409 CHILD_IN_OTHER_TREE refusal of key, a child recorded in the tree of
// tree, when it is asked to join want: it is that tree's, not want's. A release of children and
// the workflow's adoption both answer it.
func ChildInOtherTree(key, tree, want string) *Refusal {
	return &Refusal{Status: 409, Code: "CHILD_IN_OTHER_TREE", Message: fmt.Sprintf("%s runs in the tree of %s, not %s", key, tree, want)}
}

// ChildNotAdoptable is the 409 CHILD_NOT_ADOPTABLE refusal of key, a recorded root, when it is
// asked to join the tree of tree and cannot, for reason. The workflow's adoption and a release of
// children both answer it.
func ChildNotAdoptable(key, tree, reason string) *Refusal {
	return &Refusal{Status: 409, Code: "CHILD_NOT_ADOPTABLE", Message: fmt.Sprintf("%s cannot join the tree of %s: %s", key, tree, reason)}
}

// NotCurrentPhase is the 409 HANDOFF_NOT_CURRENT_PHASE refusal of a completion from role, which
// does not work p, the phase issue stands in: the code every phase worker's prompt names for a
// completion of a phase it no longer works (prompts/go/worker-common.md). The workflow's handoff
// answers it for such a completion it does not record (its leftItsPhase), and the completion
// route's entry fence for one whose entry differs from the role's phases row, before the fact
// (api's handoffComplete), so a worker reads the same words from either.
func NotCurrentPhase(role claim.Role, p phase.Phase, issue string) *Refusal {
	return &Refusal{Status: 409, Code: "HANDOFF_NOT_CURRENT_PHASE", Message: fmt.Sprintf("the %s does not run phase %s of %s; this completion changed nothing", role, p, issue)}
}

// LateReportOfStoppedTask is the 409 TASK_STOPPED refusal of a report of step, run by role, of task
// of issue, which was stopped at that step for reason, the stop reason the task stores
// (record.PlanTask.StoppedReason): the stop ended the step's run, and the report moves nothing
// (workflow's lateReport). Its first sentence is the stored reason (stoppedSentence). The task's row
// holds no stop time, so the text names none, and it names no result, because the daemon holds no
// record of how that run ended: the step record in Dispatch does (dispatch steps). Where the report
// names a handoff record (named), the workflow keeps it as the role's last one, so the step's next
// run is refused that record (HANDOFF_NOT_NEW), and the text says so.
func LateReportOfStoppedTask(issue, task, reason string, step int, role string, named bool) *Refusal {
	why := stoppedSentence(issue, task, reason)
	end := fmt.Sprintf("This report moved nothing. dispatch steps --issue %s shows how that run ended.", issue)
	if named {
		end += " Its handoff record counts as used, so the next run of this step writes a handoff of its own."
	}
	return &Refusal{Status: 409, Code: "TASK_STOPPED", Message: fmt.Sprintf("%s. Step %d (%s) has ended. %s", why, step, role, end)}
}

// PlanRatingRequired is the 400 PLAN_RATING_REQUIRED refusal of what, the completion that ends the
// retro, which carries no rating of the plan though its caller must rate (HandoffComplete.MustRate).
// The workflow answers it where it ends the retro (its planRatingRefusal), and records nothing.
func PlanRatingRequired(what string) *Refusal {
	return &Refusal{Status: 400, Code: "PLAN_RATING_REQUIRED", Message: fmt.Sprintf(
		"%s carries no rating of the plan, and the completion that ends the retro rates it: rate the plan the issue ran from %d (the plan cost the work) to %d (the plan fit the work) in planRating, say why in planReason, and send it again; nothing was recorded", what, steprecord.RatingLeast, steprecord.RatingMost)}
}

// PlanRatingUnexpected is the 400 PLAN_RATING_UNEXPECTED refusal of what, a completion or a report
// that names planRating or planReason and does not end the retro, which alone rates the plan.
// Dropping them unread would let an agent believe it had rated the plan. The workflow answers it
// for a completion that does not end the retro on the issue it reads (its planRatingRefusal), and
// the completion route for a report on a look of a production check, which ends no phase (api's
// checkReport), so an agent reads the same words from either. Neither records anything.
func PlanRatingUnexpected(what string) *Refusal {
	return &Refusal{Status: 400, Code: "PLAN_RATING_UNEXPECTED", Message: fmt.Sprintf(
		"%s names planRating or planReason, and only the completion that ends the retro rates the plan; send it again without both, and nothing was recorded", what)}
}

// ApplyFact is the only transaction path for facts, whether they arrived through JetStream, the
// API, or supervision. It records the event before running handlers, skips duplicates before any
// handler runs, remembers the first refusal while completing every handler, and rolls all writes
// back when any handler returns an error. An Observed issue fact is settled first, under the lock
// (settleObserved): the handlers see its view only while the workflow records no such issue. A
// fact a durable consumer delivered (withStreamPosition) records that consumer's stream position
// in the same transaction (recordStreamPosition), so a consumer found missing is created at the
// last message it applied (openConsumer). A fact a replay applies (withReplayClose) closes the
// give-up it was stored in, in the same transaction too, as applied or, for a duplicate, as
// already applied (closeReplayed). The fact commits through store.Transact, so a fact whose
// commit's reply is lost is reported by what the store says of it.
//
// Facts apply one at a time. Handlers read and rewrite whole records, and admission's slots span
// every tree, so two facts in flight together would each act on what the other is about to change;
// the transaction-scoped advisory lock makes the second wait for the first to commit.
//
// The lock is deliberately one lock for every fact, not one per tree. A per-tree lock does not
// hold: admission's promotion writes the issue record, admission slot and outbox rows of a tree
// other than the fact's — the candidate it promotes off the waiting line — so it would mutate a
// tree whose lock nobody holds, racing that tree's own handler on the same read-modify-write.
// Making per-tree locking correct means promotion taking the candidate tree's lock as well, which
// is cross-tree lock ordering, which is a deadlock a daemon supervising long-lived agents would
// hold until a human noticed. Serial facts are a throughput cost; that is the trade being made.
//
// A fact takes its pool's turn (factTurns, store.Pool.FactTurn) before it asks the pool for a
// connection, and hands it on at its COMMIT, as the first of its commit hooks (store.OnCommit), so
// the turn is held exactly as long as the fact lock plus the wait for a connection, and no later
// hook holds it; a fact that does not commit hands it on when store.Transact returns. The facts of
// one process then wait for each other outside the pool rather than each on a connection of its
// own, so a burst of facts leaves the rest of the pool to every other read. A fact whose ctx ends
// while it waits for the turn leaves the line holding nothing. A fact applied inside a transaction
// already open on ctx joins that transaction (store.TransactWith) and needs no connection, so it
// takes no turn: taking one there could wait on the very fact whose transaction it joins.
//
// Before anything else, source, eventID and every string fact carries are given through the
// store's own rule (storetext.Text, storetext.Strings), so MarkProcessed, settleObserved and every
// handler see the fact as the store keeps it. A handler compares what the fact says with what the
// store holds of it — a title, a check run's name, a head, a changes request's reason — and the
// store keeps each text as the rule makes it: compared as it arrived, a fact holding a text the
// rule changes would differ from its own stored copy every time it was delivered again, and be
// written again, or become another step of a pull request's head run. Every fact of GitHub,
// Dispatch, Envoy, the API and supervision passes here, so the rule is given to facts here once;
// the boot's Dispatch listing, which admission applies outside ApplyFact, takes it in admission's
// Reconcile.
func ApplyFact(ctx context.Context, pool store.TxPool, source, eventID string, fact Fact, handlers ...Handler) (Result, error) {
	source, eventID, fact = storetext.Text(source), storetext.Text(eventID), storetext.Strings(fact)
	turn := func() {}
	if turns, ok := pool.(factTurns); ok && !store.CommitScoped(ctx) {
		release, err := turns.FactTurn(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("wait for the pool's turn to apply fact %s/%s: %w", source, eventID, err)
		}
		turn = release
	}
	defer turn()
	var result Result
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		// The turn's release is the fact's first commit hook, so the next fact's turn starts at
		// this one's COMMIT, as its wait on the fact lock does, and no later hook holds it.
		store.OnCommit(ctx, turn)
		if err := SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize fact %s/%s: %w", source, eventID, err)
		}
		fresh, err := record.NewStore().MarkProcessed(ctx, tx, source, eventID)
		if err != nil {
			return err
		}
		// A duplicate records its position too: it is acknowledged as applied, so its consumer is
		// past it either way.
		if err := recordStreamPosition(ctx, tx); err != nil {
			return err
		}
		if !fresh {
			result = Result{Duplicate: true}
			return closeReplayed(ctx, tx, record.IntakeAlreadyApplied)
		}

		fact, apply, err := settleObserved(ctx, tx, fact)
		if err != nil {
			return err
		}
		if !apply {
			handlers = nil
		}
		ctx, settle := OpenSettlers(ctx, handlers...)
		for _, handler := range handlers {
			candidate, err := handler.Apply(ctx, tx, fact)
			if err != nil {
				return err
			}
			if result.Refusal == nil && candidate.Refusal != nil {
				result.Refusal = candidate.Refusal
			}
			result.Unspent = result.Unspent || candidate.Unspent
		}
		if err := settle(ctx, tx); err != nil {
			return err
		}
		// A refused report of a production check leaves its event unmarked, so a report sent
		// again — the same look and verdict, which is the same fact id its route mints (api's
		// checkReport) — applies instead of being answered as one already received. Beside it, a
		// handler leaves a fact unmarked only where the refusal's cause can clear without changing
		// the fact's id (Result.Unspent). An unmarked refusal of any other fact would make a
		// refused handoff apply on its next delivery, which those refusals exist to stop.
		_, report := fact.(CheckReport)
		if (report && result.Refusal != nil) || result.Unspent {
			if err := record.NewStore().ForgetProcessed(ctx, tx, source, eventID); err != nil {
				return err
			}
		}
		return closeReplayed(ctx, tx, record.IntakeApplied)
	}); err != nil {
		return Result{}, err
	}
	return result, nil
}

// factTurns is a pool that gives one fact at a time its turn to apply (store.Pool.FactTurn): a
// release that is safe to call more than once, or ctx's error once ctx ends first. A pool without
// it, such as a test's own pgx pool, gives every fact its turn at once.
type factTurns interface {
	FactTurn(ctx context.Context) (release func(), err error)
}

// Applied is which of eventIDs from source ApplyFact has already applied, read in one read-only
// transaction and outside the fact lock. A fact's processed_events row commits with the fact, and
// only a refused production check report or a fact a handler left unspent removes its own
// (ApplyFact), so an id read here as applied is one ApplyFact would only take the lock to skip. A
// caller that replays many facts it has most likely applied before (the boot's terminal replay)
// applies only the rest: the duplicates cost one read-only transaction between them, not a
// transaction each queued on the lock behind every live fact.
//
// ApplyFact records each id as the store keeps it (storetext.Text), so the ids are looked up as
// the store keeps them, and each one found is answered under the id the caller gave: a caller that
// looks up its own id finds it, whatever the rule made of it.
func Applied(ctx context.Context, pool store.TxPool, source string, eventIDs []string) (map[string]bool, error) {
	applied := make(map[string]bool, len(eventIDs))
	if len(eventIDs) == 0 {
		return applied, nil
	}
	stored := storetext.Strings(eventIDs)
	var ids []string
	err := store.TransactWith(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select event_id from processed_events where source = $1 and event_id = any($2::text[])`, storetext.Text(source), stored)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read which %s facts are applied: %w", source, err)
	}
	found := make(map[string]bool, len(ids))
	for _, id := range ids {
		found[id] = true
	}
	for i, id := range eventIDs {
		if found[stored[i]] {
			applied[id] = true
		}
	}
	return applied, nil
}

// settleObserved is the fact ApplyFact applies in fact's place, under the fact lock, and whether it
// applies one: an Observed issue fact's view, as a plain observation, while the workflow records no
// issue of its key, and once one is recorded the event it was prepared in place of (Delivered),
// carrying the view's Ancestry and Reread, which the preparation read for the view alone,
// or nothing when it was prepared in place of none. Every other fact applies as it is, so no
// handler sees Observed or Delivered.
//
// A view older than an issue fact of the issue the daemon has already applied (the copy's applied
// sequence, record.Store.DispatchIssueApplied, which blockers.Mirror raises for every applied issue
// fact) applies nothing while the issue is unrecorded: Dispatch has said something since, which the
// view does not show — a person's withdrawal after the release's write, say — and that later fact,
// applied, recorded no issue. A reading that decides nothing does not count: the periodic turn's
// re-read and a listing take Dispatch's last sequence of the issue, which a comment, a document
// edit, an ask or a claim raises too, and none of those reaches the handlers as an issue fact.
func settleObserved(ctx context.Context, tx pgx.Tx, fact Fact) (Fact, bool, error) {
	issue, ok := fact.(DispatchIssue)
	if !ok || !issue.Observed {
		return fact, true, nil
	}
	store := record.NewStore()
	recorded, err := store.Issue(ctx, tx, issue.Key)
	if err != nil {
		return nil, false, fmt.Errorf("read the record of %s before applying Dispatch's view of it: %w", issue.Key, err)
	}
	switch {
	case recorded == nil:
		applied, err := store.DispatchIssueApplied(ctx, tx, issue.Key)
		if err != nil {
			return nil, false, err
		}
		if applied > issue.Seq {
			return nil, false, nil
		}
		issue.Observed, issue.Delivered = false, nil
		return issue, true, nil
	case issue.Delivered == nil:
		return nil, false, nil
	default:
		delivered := *issue.Delivered
		delivered.Ancestry, delivered.Reread = issue.Ancestry, issue.Reread
		return delivered, true, nil
	}
}

// SerializeFacts takes, for tx's life, the lock every fact applies under (ApplyFact): until tx
// commits or rolls back, no fact resolves a repository and commits an issue recorded on it. The
// boot's reconcile, which records issues in its own transaction, takes it, as do the daemon's
// read of the live records for its repository caches, across that read and its publication, and
// its backfill of a record's repository columns (internal/daemon's refreshLive and
// backfillLater), so no read of the records is published over an issue an admission recorded
// meanwhile. The subscription update itself is outside this lock and outside any transaction of
// that pool: a stream call made under either left every connection of the pool idle in a
// transaction waiting on JetStream (cybermonklabs/legion#3786, dispatch://INFRA-3169), so the
// daemon's publication of the whole heard set reads the records in a transaction of its own and
// calls JetStream after it, and a recording's own update runs in the commit hook, after the
// transaction returned its connection and Postgres released this lock (internal/daemon's
// hearRecorded).
func SerializeFacts(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "select pg_advisory_xact_lock(hashtext('legion.apply_fact'))")
	return err
}
