// Package dispatch is the Go daemon's bounded client for Dispatch's native HTTP API.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Client is the Dispatch surface the workflow needs. It deliberately exposes only the reads and
// writes the daemon owns; callers preserve the order ListIssues returns when deciding admission.
type Client interface {
	ListIssues(ctx context.Context, project string, statuses []string) ([]IssueSummary, error)
	GetIssue(ctx context.Context, key string) (Issue, error)
	SetStatus(ctx context.Context, key, status string) (Issue, error)
	// SetStatusAt is SetStatus on the issue's sequence as the caller read it, so a change Dispatch
	// recorded since that read is refused rather than overwritten: the daemon's own queued writes
	// (the outbox's status row) go through it, while a route that writes a status on a person's or
	// an architect's command takes SetStatus, having read nothing to be overtaken.
	SetStatusAt(ctx context.Context, issue Issue, status string) (Issue, error)
	// ReleaseClaim clears the issue's Dispatch claim through DELETE
	// /api/v1/issues/{key}/claim, as the daemon's own session. Dispatch answers 409
	// ISSUE_CLAIMED when this caller may not release the holder, and 409 CLAIM_CONTENDED when
	// the holder changed twice while the request ran, so nothing was applied.
	ReleaseClaim(ctx context.Context, key string) error
	HandOver(ctx context.Context, issue Issue, status string) error
	PostMessage(ctx context.Context, key, body string) error
	MessageBodiesSince(ctx context.Context, key string, since time.Time) ([]string, error)
	// ArtifactSlugs is the slug of every document and file the issue holds.
	ArtifactSlugs(ctx context.Context, key string) ([]string, error)
	// UploadArtifact writes a markdown document on the issue under name, and answers the slug
	// Dispatch filed it under. It names no token, so it is meant as a first upload, and the outbox
	// uploads only a name the issue lists no document under (messageArtifact). Since
	// legion-envoy-v32.36.1 Dispatch stores an upload naming no token over a name the issue already
	// holds, as it did before it began to ask for one, and counts it
	// (envoy_dispatch_document_replacements_unguarded_total); a token behind the live document is
	// still refused 409 PRECONDITION_FAILED.
	UploadArtifact(ctx context.Context, key, name, content string) (slug string, err error)
	// EventsAfter is the issue's event log after a sequence, oldest first, each event as Dispatch
	// serves it: the boot catch-up's read of what a root's durable stream may not have delivered.
	EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error)
	Approval(ctx context.Context, artifactID string) (Approval, error)
	// PlanDocument reads one version of a document with its typed blocks and the issue carrying
	// it (DocumentVersion): what register_plan reads a plan from at the version its owner gave. A
	// Dispatch that predates the read with blocks answers ErrNoBlocks.
	PlanDocument(ctx context.Context, artifactID string, version int) (DocumentVersion, error)
	// ArtifactChanges is what changed in a document between two of its stored versions, as
	// Dispatch reads the two (GET /api/v1/artifacts/{id}/changes). A Dispatch that answers 404
	// ARTIFACT_NOT_FOUND there is ErrDocumentNotFound, and its router's 404 `no route for …` or a
	// 405 is ErrNoChangesRoute.
	ArtifactChanges(ctx context.Context, artifactID string, from, to int) (ArtifactChanges, error)
	// ApprovedVersionBefore is the highest version of the document below version that a review
	// approved (GET /api/v1/artifacts/{id}/reviews), 0 when no review approved one. A Dispatch
	// that answers 404 ARTIFACT_NOT_FOUND there is ErrDocumentNotFound.
	ApprovedVersionBefore(ctx context.Context, artifactID string, version int) (int, error)
	Handoff(ctx context.Context, id string) (Handoff, error)
	// PutGatedSteps publishes the design-gate steps the deployment gates onto project's Dispatch
	// project row, in the order it gates them; an empty list is a deployment that gates none.
	PutGatedSteps(ctx context.Context, project string, steps []string) error
	// PutDesignGates publishes the document each of the issue's design-gate steps records onto
	// the issue's Dispatch row, replacing whatever set it holds; an empty list is an issue whose
	// gates record none.
	PutDesignGates(ctx context.Context, key string, gates []DesignGateDocument) error
	// PutRoleHolders publishes the session that holds each Legion role of the issue onto the
	// issue's Dispatch row, replacing whatever set it holds; an empty list is an issue where no
	// session holds a role.
	PutRoleHolders(ctx context.Context, key string, holders []RoleHolder) error
	// PutStep writes one start or one end of a step onto Dispatch's record id of the issue's
	// steps. Dispatch writes each field once and keeps every field the write leaves out.
	PutStep(ctx context.Context, key, id string, write StepWrite) error
	// PutStepRating writes the rating a retro gave the plan onto the retro's record id of the
	// issue, with the record's identity in write, on the record's rating route. Dispatch writes
	// each field once, so a rating sent again changes nothing.
	PutStepRating(ctx context.Context, key, id string, write StepWrite, rating PlanRating) error
	// PutPlan writes the plan one run of the issue carries out onto Dispatch's plan id of the
	// issue. Its source, document, version and tasks are one fact: a write whose version is above
	// the version Dispatch holds replaces all four, and one at or below it changes nothing (a
	// version left out, the source default, counts as 0), so a plan sent again or late changes nothing.
	PutPlan(ctx context.Context, key, id string, plan PlanWrite) error
	// WebhookAppInstalled is whether Dispatch's GitHub App, the one GitHub sends a repository's
	// webhook events through, is installed on repo (owner/name).
	WebhookAppInstalled(ctx context.Context, repo string) (bool, error)
}

// DesignGateDocument is one entry of an issue's design-gate publication, as Dispatch's route
// takes and answers it: Step is one of `decisions`, `criteria` and `spec`, ArtifactID the
// document that step's gate recorded, and Holds whether the issue's tree is gated on that step,
// which Dispatch's advice reads in place of the project's gated steps. Holds is sent as null only
// for an entry an older daemon queued without it, which Dispatch answers by the project's list.
// The step is a string here rather than the record's own type, because this package names
// Dispatch's wire shapes and imports no record of the daemon's.
type DesignGateDocument struct {
	Step       string `json:"step"`
	ArtifactID string `json:"artifact_id"`
	Holds      *bool  `json:"holds"`
}

// RoleHolder is one entry of an issue's role-holder publication, as Dispatch's route takes it: Role
// is a role of the issue, one of the six Legion roles or a role the deployment defines, Session the
// session that holds it, Claim the token of the claim that session runs, and WorksStep whether that
// claim is the worker on the role's step as the record gives it (record.WorkerClaim). Two claims of
// one issue can hold one role — the agent of a key on a step of another role, beside that role's
// own claim of an earlier version of the plan, or two agents at two steps of the role — so a role
// can be named for several sessions, each by its own claim; Dispatch keeps the session of the
// step's worker where its way back folds a role to one session. The role is a string here rather
// than claim.Role, because this package names Dispatch's wire shapes.
type RoleHolder struct {
	Role      string `json:"role"`
	Session   string `json:"session"`
	Claim     string `json:"claim"`
	WorksStep bool   `json:"works_step"`
}

// StepWrite is the body of a step record's write, as Dispatch's route takes it, the actor aside:
// every identity field of the step, written whatever its value, and each field the write may not
// know as a pointer, left out when nil. Dispatch writes each field once and keeps what a write
// leaves out, and it takes a text field sent as "" as left out; a time and ReturnedTo are pointers
// because their zero is a value (a moment, and the retro's place 0). It mirrors the record's own
// StepRecord, the rating aside, because this package imports no record of the daemon's.
type StepWrite struct {
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
}

// PlanRating is a retro's rating of the plan, from 1 to 5, and its reason, as the record's rating
// route takes them beside the step write (HTTPClient.PutStepRating). It mirrors the record's own
// PlanRating, which the outbox converts to it, because this package imports no record of the
// daemon's; internal/daemon's TestAStepRecordReachesDispatchWithEveryField holds the two to the
// same fields.
type PlanRating struct {
	Rating int    `json:"rating"`
	Reason string `json:"rating_reason"`
}

// PlanWrite is the body of a plan's write, as Dispatch's route takes it, the actor aside: the run
// the plan is of, its source (default or owner), the owner's document with its version, which a
// plan of the source default leaves out because Dispatch refuses either on one, and each task with
// its steps in order. It mirrors the record's own PlanRecord, because this package imports no
// record of the daemon's.
type PlanWrite struct {
	Generation uint64 `json:"generation"`
	Source     string `json:"source"`
	*PlanDocument
	Tasks []PlanTask `json:"tasks"`
}

// PlanDocument is the owner's document a plan of the source owner was taken from, as the plan
// route takes it: embedded in PlanWrite, its artifact_id and version are members of the body, both
// written or neither. It mirrors the record's own PlanDocument, which publishPlanRecord converts
// to it, because this package imports no record of the daemon's.
type PlanDocument struct {
	ArtifactID string `json:"artifact_id"`
	Version    int    `json:"version"`
}

// PlanTask is one task of a plan's write: its id and its steps in order, each a role or
// `role:key`.
type PlanTask struct {
	ID    string   `json:"id"`
	Steps []string `json:"steps"`
}

// LegionLabel is the Dispatch label that hands an issue to Legion. A human sets it from the issue
// header in the Dispatch dashboard, an agent with Dispatch's issue tools. A label is the mark, and
// not the issue's route, because a route has Dispatch publish every event of the issue to the
// route's topic, while a label only marks it.
const LegionLabel = "legion"

// CarriesLegionLabel says whether labels include LegionLabel. Dispatch keeps a label's case as it
// was typed and holds labels differing only in case as one label, so the match ignores case.
func CarriesLegionLabel(labels []string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, LegionLabel) {
			return true
		}
	}
	return false
}

// IssueSummary is one issue from Dispatch's project list. Rank is Dispatch's fractional key;
// callers preserve the list's order when filtering. HandedOver is resolved once here, at the
// client boundary, from the raw labels Dispatch's list route answers, and every decision acts on
// this bool; Labels is the raw list itself, whose one reader is the copy of Dispatch's issues.
type IssueSummary struct {
	Key        string
	Title      string
	Status     string
	Parent     *string
	Rank       string
	HandedOver bool
	// Priority is the issue's own priority as Dispatch serves it: 0 for P0 to 3 for P3, nil for
	// none. The workflow record keeps it as record.Priority (intake's ListedIssue).
	Priority *int
	// Labels is the issue's labels as the listing answers them, in Dispatch's order. The copy of
	// Dispatch's issues writes them with the rest of the summary (blockers.Observed, or the Issue
	// a release reads from the summary), and no decision reads them: a decision reads HandedOver.
	Labels []string
	// LastSeq is how far Dispatch's own event log for the issue has run. A record whose applied
	// sequence is behind it has events still to come on the stream, which carry the actor this
	// snapshot does not.
	LastSeq int64
	// ResolvedRepo is the repository (owner/name) Dispatch resolves the issue to: its own, else its
	// nearest ancestor's, else its project's default; empty when it resolves to none.
	ResolvedRepo string
	// BlockedBy is the keys of the issues that this issue waits on, in the order Dispatch lists
	// them; none when it waits on nothing. A key may belong to another project.
	BlockedBy []string
	// ClaimedBy is who Dispatch shows holding the issue's claim, as Claim.Holder writes it, or
	// empty for nobody. ClaimUnread says Dispatch showed a claim naming no actor: neither nobody
	// nor anyone the daemon can name, so the read says nothing of the claim.
	ClaimedBy   string
	ClaimUnread bool
}

// Issue is the summary as an Issue, for a reader that has only Dispatch's listing of the issue:
// every field the listing carries, its labels among them, the claim from its holder
// (Claim.Holder's `<kind>:<id>`). A summary carries no project and no primary document, so those
// are empty.
func (s IssueSummary) Issue() Issue {
	var claim *Claim
	if kind, id, ok := strings.Cut(s.ClaimedBy, ":"); ok {
		claim = &Claim{Kind: kind, ID: id}
	}
	return Issue{Key: s.Key, Title: s.Title, Status: s.Status, Parent: s.Parent, Rank: s.Rank, Priority: s.Priority, Labels: s.Labels, HandedOver: s.HandedOver,
		LastSeq: s.LastSeq, ResolvedRepo: s.ResolvedRepo, BlockedBy: s.BlockedBy, Claim: claim, ClaimUnread: s.ClaimUnread}
}

// Issue is the workflow data from Dispatch's issue-detail route. HandedOver is resolved here, at
// the client boundary, as IssueSummary's is. Labels remain available to readers, while HandOver
// adds LegionLabel through Dispatch's additive labels_add update rather than rewriting them.
type Issue struct {
	Key               string
	Project           string
	Title             string
	Status            string
	Parent            *string
	Rank              string
	Labels            []string
	HandedOver        bool
	PrimaryArtifactID string
	LastSeq           int64
	// Priority is IssueSummary.Priority.
	Priority *int
	// ResolvedRepo is IssueSummary.ResolvedRepo.
	ResolvedRepo string
	// BlockedBy is IssueSummary.BlockedBy.
	BlockedBy []string
	// Claim is the issue's claim Dispatch shows, nil for none; ClaimUnread is IssueSummary's.
	Claim       *Claim
	ClaimUnread bool
}

// Claim is the actor Dispatch records as an issue's holder: a session or a person.
type Claim struct {
	Kind string
	ID   string
}

// Holder is the claim as the daemon keeps it, `<kind>:<id>`, or empty for no claim.
func (c *Claim) Holder() string {
	if c == nil {
		return ""
	}
	return c.Kind + ":" + c.ID
}

// Actor is who Dispatch names as having made a change: Kind `user` is a person, `session` an agent
// session, and `system` Dispatch itself (its document settlement among them). Every Dispatch event
// carries one.
type Actor struct {
	Kind string
	ID   string
}

// Person says whether the actor is a person.
func (a Actor) Person() bool { return a.Kind == "user" }

// Name is the actor as `<kind>:<id>`, the form a claim's holder takes and Legion's notices name a
// decision's author in (record.Notice.Actor), or empty for no actor.
func (a Actor) Name() string {
	if a == (Actor{}) {
		return ""
	}
	return a.Kind + ":" + a.ID
}

// SettlementActor is Dispatch's document settlement, which closes an ask whose decision block left
// its document in its own name (Dispatch's docs.SettlementActor).
var SettlementActor = Actor{Kind: "system", ID: "document-settlement"}

// Approval is the current approval state Dispatch derives for a document, and the issue carrying
// the document (empty for a project document). While an approval request is open (State
// `awaiting`), AskID names it and WaitingOn is whose reply it needs next, `human` or `agent`; both
// are empty in every other state. At and Seq identify the document's latest review,
// whatever State shows: At is Dispatch's time of it (its `created_at`, which its artifact.* event
// shares, since one transaction writes both), and Seq that event's sequence on the issue. Each is
// zero when Dispatch gave none: At when the document has no review, Seq also for a review Dispatch
// recorded before it kept the sequence, and from a Dispatch that predates it. By is who gave the
// review State shows (an approval or a changes request), zero when Dispatch names nobody.
//
// Primary, Role and Slug are what the document is on its issue, which is how a design-gate step
// finds the document it binds to (api's gateRegister): Primary is the issue's primary document,
// Role the part it plays beside it (ArtifactRoleVerification, ArtifactRoleDecisions, or "" for
// none) and Slug the name Dispatch filed it under.
type Approval struct {
	IssueKey      string
	State         string
	LatestVersion int
	Version       *int
	Reason        *string
	AskID         string
	WaitingOn     string
	At            time.Time
	Seq           int64
	By            Actor
	Primary       bool
	Role          string
	Slug          string
}

// ArtifactRoleVerification and ArtifactRoleDecisions are two of the parts Dispatch records a
// document playing on its issue (model.Artifact.Role), and DecisionsSlug and VerificationSlug the
// names it files those two documents under, which a reference to either names
// (workflow's stepRef). No mechanism shares a value between this module and Dispatch's, so the
// spellings change together.
const (
	ArtifactRoleVerification = "verification"
	ArtifactRoleDecisions    = "decisions"
	DecisionsSlug            = "decisions-md"
	VerificationSlug         = "verification-md"
)

// ApprovedLatest is the version Dispatch shows approved when that approval is of its latest
// version, and 0 otherwise.
func (a Approval) ApprovedLatest() int {
	if a.State == "approved" && a.Version != nil && *a.Version == a.LatestVersion {
		return *a.Version
	}
	return 0
}

// ChangesRequestedLatest is the version of the changes request Dispatch shows on its latest version
// and the request's reason, or 0 when the latest version has none: a newer version superseded it,
// the latest review approves, or an approval request is open again.
func (a Approval) ChangesRequestedLatest() (int, string) {
	if a.State != "changes_requested" || a.Version == nil || *a.Version != a.LatestVersion {
		return 0, ""
	}
	reason := "Dispatch reports changes requested"
	if a.Reason != nil && *a.Reason != "" {
		reason = *a.Reason
	}
	return *a.Version, reason
}

// LatestReviewer is who gave the review Dispatch shows on its latest version, an approval
// (ApprovedLatest) or a changes request (ChangesRequestedLatest), and zero when the latest version
// has neither.
func (a Approval) LatestReviewer() Actor {
	if changes, _ := a.ChangesRequestedLatest(); a.ApprovedLatest() == 0 && changes == 0 {
		return Actor{}
	}
	return a.By
}

// DocumentVersion is one version of a document as the daemon reads a plan from it (PlanDocument on
// HTTPClient): the document, the issue carrying it (empty for a project document), the version
// read, and that version's typed blocks in document order. The blocks are the version's own, read
// from its stored text by Dispatch, never the live document's, so the same version reads the same
// blocks whenever it is read.
type DocumentVersion struct {
	ArtifactID string
	IssueKey   string
	Version    int
	Blocks     []DocumentBlock
}

// DocumentBlock is one typed block of a document version: its id, which its opening line writes
// and so stays the same in every version that keeps the block; its type (plan_task, requirement,
// check, ...); its attributes by name, each value as the opening line writes it (a block[]
// attribute's references space-separated); and Text, its body: the markdown between its opening
// line and its closing fence.
type DocumentBlock struct {
	ID         string
	Type       string
	Attributes map[string]string
	Text       string
}

// ErrNoBlocks is PlanDocument's answer to a version read that carries no blocks: a Dispatch of a
// release before the version read with blocks ignores ?blocks=true and answers the version's text
// alone. No plan can be read from that Dispatch until it is upgraded; an empty plan is never
// assumed.
var ErrNoBlocks = errors.New("Dispatch answered the document version without its blocks: it predates the version read with ?blocks=true")

// ArtifactChanges is what changed in a document from version From to version To, as Dispatch's
// GET /api/v1/artifacts/{id}/changes answers it (its artifactChanges), whose JSON it decodes as it
// stands, as DesignGateDocument names its route's shape. Both versions are compared with their
// decision blocks cut out, so an answer a person gave, or an ask added or moved, changes no
// section and no block. Dispatch makes exactly one of three things true of an answer:
// DecisionOnly, the cut is the whole change (the stored texts differ and cut to the same text);
// Unchanged, the two versions differ in nothing any section, run or block holds (their stored
// texts are the same, or what differs is in none of them); or Sections or Blocks names at least
// one item. So an answer that names nothing says which of the first two it is. A Dispatch from
// before Unchanged sends no such field, so an identical version from it decodes as an answer that
// names nothing and is neither. Two versions of a document never change, so the answer for one
// pair never goes stale.
type ArtifactChanges struct {
	ArtifactID   string `json:"artifact_id"`
	From         int    `json:"from"`
	To           int    `json:"to"`
	DecisionOnly bool   `json:"decision_only"`
	Unchanged    bool   `json:"unchanged"`
	// Sections are the document's top-level units whose text differs, that moved, or that one
	// version alone holds: each `##` section, and each run of the text outside every `##`
	// section, which carries the place it stands at (SectionChange). They are in the later
	// version's order, and then the units only the earlier version holds, in its order.
	Sections []SectionChange `json:"sections"`
	// Blocks are the typed blocks other than ask blocks whose text or any attribute differs.
	Blocks []BlockChange `json:"blocks"`
}

// SectionChange is one top-level unit of a document that changed, and how it changed:
// ChangeChanged, ChangeAdded, ChangeRemoved or ChangeMoved. A `##` section is named by its
// heading's text and by how many sections before it carry that heading, 0 for the first; a `##`
// heading with no words is counted from 1. The heading "" with occurrence 0 is one run of the text
// outside every `##` section, and Place says where it stands: PlaceBeforeFirstSection, or
// PlaceAfterSection with AfterHeading and AfterOccurrence naming the `##` section directly before
// it. The place is the run's in the later version for a changed, added or moved run, and in the
// earlier version for a removed one. A `##` section carries no place, and neither does a run from
// a Dispatch that predates the place. AfterHeading and AfterOccurrence decode to "" and 0 where
// Dispatch leaves them out, which they are only beside a place other than PlaceAfterSection.
type SectionChange struct {
	Heading         string `json:"heading"`
	Occurrence      int    `json:"occurrence"`
	Change          string `json:"change"`
	Place           string `json:"place,omitempty"`
	AfterHeading    string `json:"after_heading,omitempty"`
	AfterOccurrence int    `json:"after_occurrence,omitempty"`
}

// PlaceBeforeFirstSection and PlaceAfterSection are the two places a run of the text outside
// every `##` section stands at (SectionChange's Place): above the first `##` section, or directly
// after the one AfterHeading and AfterOccurrence name. They are as Dispatch spells them (its
// api.placeBeforeFirstSection and api.placeAfterSection). No mechanism shares a value between this
// module and Dispatch's, so the spellings change together.
const (
	PlaceBeforeFirstSection = "before_first_section"
	PlaceAfterSection       = "after_section"
)

// BlockChange is one typed block that changed: its `#id`, its type in the later version (in the
// earlier one for a removed block), and how it changed.
type BlockChange struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Change string `json:"change"`
}

// ChangeChanged, ChangeAdded and ChangeRemoved are the three ways a top-level unit or a block
// stands between two versions of a document, and ChangeMoved a fourth that a unit alone takes: both
// versions hold it with the same text, at another place among the document's top-level units (its
// `##` sections and the runs of text outside them). They are as Dispatch spells them (its
// api.changeChanged and the three beside it). No mechanism shares a value between this module and
// Dispatch's, so the spellings change together.
const (
	ChangeChanged = "changed"
	ChangeAdded   = "added"
	ChangeRemoved = "removed"
	ChangeMoved   = "moved"
)

// ErrNoChangesRoute is a Dispatch build that cannot say what changed between two versions: it
// answered GET /api/v1/artifacts/{id}/changes with its router's 404 `no route for <METHOD> <path>`
// (NoRoute), or with a 405, which is what a build without the route answers. ArtifactChanges wraps
// it around Dispatch's own refusal, so errors.Is finds it and the message keeps what Dispatch
// said. It is no reason to stop asking: while a release of Dispatch rolls out, two builds serve,
// and the next attempt can reach the one with the route (packages/envoy/cmd/dispatch/README.md,
// two builds at once), so a caller retries it as any failure of the read, and records the pair
// unreadable only once its bounded attempts have all failed.
var ErrNoChangesRoute = errors.New("dispatch has no changes route")

// ErrDocumentNotFound is a Dispatch that holds no document of the id a read of one document
// names, GET /api/v1/artifacts/{id}/changes or GET /api/v1/artifacts/{id}/reviews: it answered
// 404 ARTIFACT_NOT_FOUND (ArtifactNotFound), a document deleted since its gate recorded it. A
// build without a route answers its router's own 404, whose code is not this one (NoRoute), so
// the two stay apart and the reason recorded says which it was. ArtifactChanges and
// ApprovedVersionBefore wrap it around Dispatch's own refusal, and a caller records what it asked
// unreadable at once: a deleted document does not come back with a wait.
var ErrDocumentNotFound = errors.New("dispatch holds no such document")

// ArtifactNotFound is Dispatch's own spelling of the 404 its artifact routes answer for an id
// that names no artifact (packages/envoy/internal/dispatch/api/artifacts.go,
// scanArtifactOrNotFound). No mechanism shares a value between this module and Dispatch's, so the
// spellings change together.
const ArtifactNotFound = "ARTIFACT_NOT_FOUND"

// artifactNotFound says whether refusal is Dispatch answering that it holds no document of the id
// a read of one document names: 404 under the code ArtifactNotFound (ErrDocumentNotFound).
func artifactNotFound(refusal *Error) bool {
	return refusal.Status == http.StatusNotFound && refusal.Code == ArtifactNotFound
}

// Handoff is one phase handoff record Dispatch keeps on an issue, as a phase completion names it:
// the issue carrying it, its phase word (plan, implement, test or review; phase.Handoff), its
// version among that phase's records, and the actor that wrote it. The record's text is not read
// here: the daemon holds a completion to who wrote which record, and what a handoff says is the
// next role's to read with `dispatch handoff-read`.
type Handoff struct {
	ID       string
	IssueKey string
	Phase    string
	Version  int
	Author   HandoffAuthor
}

// HandoffAuthor is the actor that wrote a handoff record: Kind is `session` for a record a session
// wrote with `dispatch handoff-write`, ID that session.
type HandoffAuthor struct {
	Kind string
	ID   string
}

// IsSession says whether the record's author is the session id.
func (a HandoffAuthor) IsSession(id string) bool { return a.Kind == "session" && a.ID == id }

// Caller is who Dispatch takes a credential for, as GET /api/v1/whoami answers a credential that is
// not a person's browser session (its whoamiAgent): Session is the session the credential proved,
// Project the one project it lets its holder write, and Owner the person it acts for, each nil
// where Dispatch names none. A broker token Dispatch serves every write of the daemon's for names
// the daemon's session (DaemonSession), its project and a person.
type Caller struct {
	Session *string `json:"session"`
	Project *string `json:"project"`
	Owner   *string `json:"owner"`
}

// Error is a non-success response from Dispatch. Code is empty only when the response did not
// carry Dispatch's normal error body.
type Error struct {
	Status  int
	Code    string
	Message string
}

// Missing says whether a Dispatch call failed because what it asked for is not there, as against
// a call that failed: the one answer a handler turns into its own 404 rather than a 502.
func Missing(err error) bool {
	var refusal *Error
	return errors.As(err, &refusal) && refusal.Status == http.StatusNotFound
}

// Claimed is whether err is Dispatch's refusal that the issue's claim is held by someone this
// caller may not release (409 IssueClaimed).
func Claimed(err error) bool {
	var refusal *Error
	return errors.As(err, &refusal) && refusal.Status == http.StatusConflict && refusal.Code == IssueClaimed
}

// Contended is whether err is Dispatch's answer that the claim changed twice while the request
// ran, so nothing was applied and nobody's liveness was checked (409 ClaimContended).
func Contended(err error) bool {
	var refusal *Error
	return errors.As(err, &refusal) && refusal.Status == http.StatusConflict && refusal.Code == ClaimContended
}

// ParentContended is the code Dispatch answers an issue PATCH with when a concurrent reparent
// moved the issue's parent at every attempt to lock it, applying nothing
// (packages/envoy/internal/dispatch/api/issue_parent.go:99, parentContended). No mechanism shares
// a value between this module and Dispatch's, so the two spellings change together.
const ParentContended = "PARENT_CONTENDED"

// IssueClaimed and ClaimContended are Dispatch's own spellings of the two 409s a claim release
// meets, neither of them final: ISSUE_CLAIMED is the holder still holding the claim
// (packages/envoy/internal/dispatch/api/issue_claim.go, writeClaimError), and CLAIM_CONTENDED is
// a holder that changed twice while the request ran, so Dispatch applied nothing
// (answerContendedClaim). No mechanism shares a value between this module and Dispatch's, so the
// spellings change together.
const (
	IssueClaimed   = "ISSUE_CLAIMED"
	ClaimContended = "CLAIM_CONTENDED"
)

// IssueNotFound is Dispatch's own spelling of the 404 its issue routes answer for an issue it
// does not know. It is the one refusal a design-gates publish ends on, which is why the daemon
// names the code rather than the status: a Dispatch whose release has no design-gates route at
// all answers its router's own 404 under the code NOT_FOUND (measured against a built server and
// against this deployment's own Dispatch; the router writes it in
// packages/envoy/internal/dispatch/routes/router.go), which is not this code, so that publication
// is retried and waits for the upgrade. Narrowing the test to the status alone would end every
// one of them. No mechanism shares a value between this module and Dispatch's, so the spellings
// change together.
const IssueNotFound = "ISSUE_NOT_FOUND"

// NoRoute is whether err is a Dispatch process's answer that it has no route for the request: its
// router's 404 under the code NOT_FOUND whose message starts "no route for "
// (packages/envoy/internal/dispatch/routes/router.go, staticHandler; the TypeScript clients'
// NO_ROUTE_MESSAGE_PREFIX). A Dispatch older than a route answers it this way, and so does the
// older build while a release rolls out, so no handler ran and the request is taken once the
// release that serves the route is live. A handler's own 404 NOT_FOUND, for a resource it does not
// hold, carries another message ("the document has no version 5" among them), so the message is
// what tells the router's answer apart. No mechanism shares a value between this module and
// Dispatch's, so the spellings change together.
func NoRoute(err error) bool {
	var refusal *Error
	return errors.As(err, &refusal) && refusal.Status == http.StatusNotFound && refusal.Code == noRouteCode &&
		strings.HasPrefix(refusal.Message, noRouteMessagePrefix)
}

const (
	noRouteCode          = "NOT_FOUND"
	noRouteMessagePrefix = "no route for "
)

// PermanentRefusal is a write Dispatch will refuse the same way however many times it is made:
// the issue is gone, or what it carries is something Dispatch will not take. Only a refusal
// Dispatch itself made counts, which is a 4xx carrying one of its error codes.
//
// Everything else is an outage a caller rides out: every 5xx, every transport failure, "come back
// later" (408, 429), a codeless 4xx from whatever sits in front of Dispatch, a credential answer
// (401, 403), and 409 PARENT_CONTENDED. A revoked or expired token says nothing about the write —
// an operator restores it and the same body is taken — so reading one as permanent would drop the
// write for good. PARENT_CONTENDED is an issue PATCH that found its parent moved by a concurrent
// reparent at every attempt to lock it, so nothing was applied and the same body is taken once the
// reparents stop. So is the refusal of a field this daemon added (newerFields) by a Dispatch from
// before #2461: it ends when that Dispatch is upgraded, so the two deploy in either order.
func PermanentRefusal(err error) (*Error, bool) {
	var refusal *Error
	if !errors.As(err, &refusal) {
		return nil, false
	}
	switch {
	case refusal.Code == "ISSUE_CHANGED",
		refusal.Code == "",
		refusal.Status < 400 || refusal.Status >= 500,
		refusal.Status == http.StatusRequestTimeout,
		refusal.Status == http.StatusTooManyRequests,
		refusal.Status == http.StatusUnauthorized,
		refusal.Status == http.StatusForbidden,
		refusal.Status == http.StatusConflict && refusal.Code == ParentContended,
		refusal.Status == http.StatusBadRequest && refusal.Code == "INVALID_JSON" && unknownNewerField(refusal.Message):
		return nil, false
	}
	return refusal, true
}

// newerFields are the request fields this daemon sends that a Dispatch from before #2461 refused by
// name: a hand-over's labels_add and expected_sequence. Since #2461 Dispatch ignores a body member
// it does not declare (decodeJSON in its api package) and refuses no field by name, so this retry
// serves only a Dispatch from before that change; a field whose loss would change a write ships as
// a route of its own instead (the record's rating route, HTTPClient.PutStepRating). Any other malformed
// body stays a permanent refusal.
var newerFields = []string{"labels_add", "expected_sequence"}

func unknownNewerField(message string) bool {
	for _, field := range newerFields {
		if strings.Contains(message, `unknown field "`+field+`"`) {
			return true
		}
	}
	return false
}

// TransientError wraps a Dispatch request failure that is a transport problem — no response at
// all (a dial refused or reset, a lookup that resolved nothing yet, the request's own bounded
// timeout firing before an answer came back) or the response cut off before its body was fully
// read — rather than anything Dispatch itself answered. request wraps every such failure in it
// (client.go), the way appauth.TransientError wraps GitHub's, so a caller can tell "Dispatch
// never answered" from "Dispatch answered and refused" without inspecting the wrapped error's
// own type. A refusal of the transport no wait fixes (TransportRefusal) is deliberately not
// wrapped.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// Unreachable reports whether err means Dispatch is not reachable yet, the same outage
// PermanentRefusal already rides out rather than drops a queued write over: a TransientError (no
// answer came back at all), or an *Error whose refusal PermanentRefusal does not call permanent —
// every 5xx, a codeless 4xx, 408, 429, 409 PARENT_CONTENDED, and a credential answer (401, 403;
// the Dispatch token (dispatch_token_file) is read once at boot, so a corrected file takes a
// restart to take effect, same as a queued write already accepts). Anything else — a genuine coded
// refusal PermanentRefusal does call permanent, or an error that is neither a TransientError nor
// a Dispatch *Error at all, such as a 200 response whose body is not the JSON it promised — is not
// an outage this waits out. A refusal of the transport (TransportRefusal) is read first and wins
// over an outage it is joined with — a TLS alert on one read beside a TransientError of another,
// as the boot's catch-up joins the reads of one root — so a boot gate stops on it rather than
// wait it out, as internal/natsauth's Unreachable lets an alert win.
func Unreachable(err error) bool {
	if TransportRefusal(err) {
		return false
	}
	var transient *TransientError
	if errors.As(err, &transient) {
		return true
	}
	var refusal *Error
	if !errors.As(err, &refusal) {
		return false
	}
	_, permanent := PermanentRefusal(err)
	return !permanent
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}
