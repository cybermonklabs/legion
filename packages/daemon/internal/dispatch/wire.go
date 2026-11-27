package dispatch

import (
	"encoding/json"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// The JSON forms of Dispatch's answers and of the daemon's request bodies, which HTTPClient
// decodes and encodes (client.go) and hands to its callers only as the package's own types.

type issueSummaryResponse struct {
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	Status   string  `json:"status"`
	Parent   *string `json:"parent"`
	Rank     string  `json:"rank"`
	Priority *int    `json:"priority"`
	// ResolvedPriority is raw so that an absent field, a Dispatch release that resolves no
	// priority, reads apart from a present null (resolvedPriority).
	ResolvedPriority json.RawMessage `json:"resolved_priority"`
	PriorityFrom     *string         `json:"priority_from"`
	Labels           []string        `json:"labels"`
	LastSeq          int64           `json:"last_seq"`
	ResolvedRepo     *string         `json:"resolved_repo"`
	Needs            *[]string       `json:"needs"`
	BlockedBy        []string        `json:"blocked_by"`
	Claim            *claimResponse  `json:"claim"`
}

// WaitsOf is the issues an issue answer says the issue waits on: its needs, which a Dispatch of
// the issue-tree spec's release on answers, and else its blocked_by, which the Dispatch of the
// release before answers alone, since a new daemon can meet an old Dispatch. An answer holding
// needs is read by it, whatever blocked_by holds beside it.
func WaitsOf(needs *[]string, blockedBy []string) []string {
	if needs != nil {
		return *needs
	}
	return blockedBy
}

type issueResponse struct {
	Key                    string          `json:"key"`
	Project                string          `json:"project"`
	Title                  string          `json:"title"`
	Status                 string          `json:"status"`
	Parent                 *string         `json:"parent"`
	Rank                   string          `json:"rank"`
	Priority               *int            `json:"priority"`
	ResolvedPriority       json.RawMessage `json:"resolved_priority"`
	PriorityFrom           *string         `json:"priority_from"`
	Labels                 []string        `json:"labels"`
	PrimaryArtifactID      string          `json:"primary_artifact_id"`
	VerificationArtifactID string          `json:"verification_artifact_id"`
	LastSeq                int64           `json:"last_seq"`
	ResolvedRepo           *string         `json:"resolved_repo"`
	Needs                  *[]string       `json:"needs"`
	BlockedBy              []string        `json:"blocked_by"`
	Claim                  *claimResponse  `json:"claim"`
}

// issue is the response as Issue: Dispatch's issue detail, and the issue its PATCH answers with.
func (r issueResponse) issue() Issue {
	return Issue{
		Key:                    r.Key,
		Project:                r.Project,
		Title:                  r.Title,
		Status:                 r.Status,
		Parent:                 r.Parent,
		Rank:                   r.Rank,
		Priority:               r.Priority,
		Resolved:               resolvedPriority(r.ResolvedPriority, r.PriorityFrom),
		Labels:                 r.Labels,
		HandedOver:             CarriesLegionLabel(r.Labels),
		PrimaryArtifactID:      r.PrimaryArtifactID,
		VerificationArtifactID: r.VerificationArtifactID,
		LastSeq:                r.LastSeq,
		ResolvedRepo:           deref(r.ResolvedRepo),
		BlockedBy:              WaitsOf(r.Needs, r.BlockedBy),
		Claim:                  r.Claim.claim(),
		ClaimUnread:            r.Claim.unread(),
	}
}

// resolvedPriority is a read's `resolved_priority` and `priority_from`: nil where the read carries no
// `resolved_priority`, and else its level, nil for a present null, and the issue it comes from. A
// value that is no integer at all reads as a present null: Dispatch writes none, and the issue's own
// fields are still read. A `priority_from` that is no issue key reads as none, as null and empty do:
// the state serves it as `priorityFrom`, which its readers take for an issue key (the plugin's
// schema refuses the whole state for one that is not), and these reads have no channel to name it
// on, as they have none for the level that is no integer.
func resolvedPriority(raw json.RawMessage, from *string) *ResolvedPriority {
	if raw == nil {
		return nil
	}
	resolved := &ResolvedPriority{}
	if key := deref(from); claim.IsIssueKey(key) {
		resolved.From = key
	}
	if err := json.Unmarshal(raw, &resolved.Priority); err != nil {
		resolved.Priority = nil
	}
	return resolved
}

// claimResponse is an issue's claim as Dispatch's issue reads serve it (null for none).
type claimResponse struct {
	Actor actor `json:"actor"`
}

// claim is the claim as Claim, nil for none or for one naming no actor (unread).
func (c *claimResponse) claim() *Claim {
	if c == nil || c.unread() {
		return nil
	}
	return &Claim{Kind: c.Actor.Kind, ID: c.Actor.ID}
}

// unread says the read showed a claim that names no actor: not nobody, but a claim the daemon
// cannot name, as intake's decoder takes the same claim on an issue event.
func (c *claimResponse) unread() bool {
	return c != nil && (c.Actor.Kind == "" || c.Actor.ID == "")
}

// holder is the claim as Claim.Holder writes it.
func (c *claimResponse) holder() string {
	return c.claim().Holder()
}

type issueEventResponse struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Payload   json.RawMessage `json:"payload"`
}

type artifactResponse struct {
	IssueKey *string           `json:"issue_key"`
	Primary  bool              `json:"primary"`
	Role     string            `json:"role"`
	Approval *approvalResponse `json:"approval"`
}

// issueArtifactResponse is one entry of an issue's artifact list as Dispatch answers it, with the
// number of each version it lists and none of the rest of the version.
type issueArtifactResponse struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Kind     string `json:"kind"`
	Primary  bool   `json:"primary"`
	Role     string `json:"role"`
	Versions []struct {
		Number int `json:"number"`
	} `json:"versions"`
}

// reviewResponse is one review of a document's review list as Dispatch answers it, its kind, the
// request it answered and its content digests left undecoded.
type reviewResponse struct {
	Version   int       `json:"version"`
	State     string    `json:"state"`
	Actor     actor     `json:"actor"`
	Reason    *string   `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// versionBlocksResponse is a document version as GET …/versions/{n}?blocks=true answers it: its
// markdown, its number, and its typed blocks, nil when Dispatch sent no blocks key.
type versionBlocksResponse struct {
	Markdown string                `json:"markdown"`
	Number   int                   `json:"number"`
	Blocks   *[]versionBlockAnswer `json:"blocks"`
}

// versionBlockAnswer is one block of that answer: its id, type, byte range in the answer's
// markdown and attributes, each a name and the value its opening line writes.
type versionBlockAnswer struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	From       int    `json:"from"`
	To         int    `json:"to"`
	Attributes []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"attributes"`
}

type approvalResponse struct {
	State         string  `json:"state"`
	LatestVersion int     `json:"latest_version"`
	Version       *int    `json:"version"`
	Reason        *string `json:"reason"`
	// AskID and WaitingOn name the open approval request and whose turn it waits on, sent while
	// State is awaiting.
	AskID     *string `json:"ask_id"`
	WaitingOn string  `json:"waiting_on"`
	// At is the latest review's created_at and Seq its event's sequence on the issue; a Dispatch
	// that predates the sequence, or a review recorded before it kept one, sends no seq.
	At  *string `json:"at"`
	Seq *int64  `json:"seq"`
	// By is who gave the review State shows, and is absent where Dispatch names nobody.
	By *actor `json:"by"`
}

// askReadResponse is one ask as GET /api/v1/asks/{id} answers it, its thread left undecoded: the
// ask's state, the login of the person its answer names (no answer for an ask nobody answered),
// and the actor its resolution names (no resolution for an open or answered ask).
type askReadResponse struct {
	Ask struct {
		State  string `json:"state"`
		Answer *struct {
			User string `json:"user"`
		} `json:"answer"`
		Resolution *struct {
			Actor actor `json:"actor"`
		} `json:"resolution"`
	} `json:"ask"`
}

// handoffResponse is a handoff record as Dispatch answers it, its body left undecoded: Handoff's
// callers hold a completion to who wrote which record, never to what it says.
type handoffResponse struct {
	ID       string `json:"id"`
	IssueKey string `json:"issue_key"`
	Phase    string `json:"phase"`
	Version  int    `json:"version"`
	Author   actor  `json:"author"`
}

// handoff is the record without its body.
func (r handoffResponse) handoff() Handoff {
	return Handoff{
		ID:       r.ID,
		IssueKey: r.IssueKey,
		Phase:    r.Phase,
		Version:  r.Version,
		Author:   HandoffAuthor{Kind: r.Author.Kind, ID: r.Author.ID},
	}
}

type actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type daemonWrite struct {
	Status           string   `json:"status,omitempty"`
	Labels           []string `json:"labels,omitempty"`
	LabelsAdd        []string `json:"labels_add,omitempty"`
	ExpectedSequence int64    `json:"expected_sequence,omitempty"`
	Body             string   `json:"body,omitempty"`
	Actor            actor    `json:"actor"`
}

// artifactUpload is the body of a document upload: its name, its markdown and the daemon's actor,
// all fields Dispatch's JSON upload reads.
type artifactUpload struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	Actor   actor  `json:"actor"`
}

// gatedStepsWrite is the body of the gated-steps publish: the list, never omitted, and the
// daemon's actor.
type gatedStepsWrite struct {
	Steps []string `json:"steps"`
	Actor actor    `json:"actor"`
}

// designGatesWrite is the body of the design-gates publish: the set, never omitted, and the
// daemon's actor.
type designGatesWrite struct {
	Gates []DesignGateDocument `json:"gates"`
	Actor actor                `json:"actor"`
}

// roleHoldersWrite is the body of the role-holder publication: the issue's whole set and the
// daemon's actor.
type roleHoldersWrite struct {
	Holders []RoleHolder `json:"holders"`
	Actor   actor        `json:"actor"`
}

// stepWrite is the body of a step record's write: the record's fields and the daemon's actor.
type stepWrite struct {
	StepWrite
	Actor actor `json:"actor"`
}

// stepRatingWrite is the body of a rating's write: the record's fields, the rating and its reason,
// and the daemon's actor.
type stepRatingWrite struct {
	StepWrite
	PlanRating
	Actor actor `json:"actor"`
}

// stepExperimentWrite is the body of a variant's write: the run's identity, the experiment and the
// variant, and the daemon's actor.
type stepExperimentWrite struct {
	StepWrite
	StepExperiment
	Actor actor `json:"actor"`
}

// planWrite is the body of a plan's write: the plan's fields and the daemon's actor.
type planWrite struct {
	PlanWrite
	Actor actor `json:"actor"`
}
