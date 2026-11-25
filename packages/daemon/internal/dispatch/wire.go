package dispatch

import (
	"encoding/json"
	"time"
)

// The JSON forms of Dispatch's answers and of the daemon's request bodies, which HTTPClient
// decodes and encodes (client.go) and hands to its callers only as the package's own types.

type issueSummaryResponse struct {
	Key          string         `json:"key"`
	Title        string         `json:"title"`
	Status       string         `json:"status"`
	Parent       *string        `json:"parent"`
	Rank         string         `json:"rank"`
	Priority     *int           `json:"priority"`
	Labels       []string       `json:"labels"`
	LastSeq      int64          `json:"last_seq"`
	ResolvedRepo *string        `json:"resolved_repo"`
	BlockedBy    []string       `json:"blocked_by"`
	Claim        *claimResponse `json:"claim"`
}

type issueResponse struct {
	Key                    string         `json:"key"`
	Project                string         `json:"project"`
	Title                  string         `json:"title"`
	Status                 string         `json:"status"`
	Parent                 *string        `json:"parent"`
	Rank                   string         `json:"rank"`
	Priority               *int           `json:"priority"`
	Labels                 []string       `json:"labels"`
	PrimaryArtifactID      string         `json:"primary_artifact_id"`
	VerificationArtifactID string         `json:"verification_artifact_id"`
	DecisionsArtifactID    string         `json:"decisions_artifact_id"`
	LastSeq                int64          `json:"last_seq"`
	ResolvedRepo           *string        `json:"resolved_repo"`
	BlockedBy              []string       `json:"blocked_by"`
	Claim                  *claimResponse `json:"claim"`
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
		Labels:                 r.Labels,
		HandedOver:             CarriesLegionLabel(r.Labels),
		PrimaryArtifactID:      r.PrimaryArtifactID,
		VerificationArtifactID: r.VerificationArtifactID,
		DecisionsArtifactID:    r.DecisionsArtifactID,
		LastSeq:                r.LastSeq,
		ResolvedRepo:           deref(r.ResolvedRepo),
		BlockedBy:              r.BlockedBy,
		Claim:                  r.Claim.claim(),
		ClaimUnread:            r.Claim.unread(),
	}
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
	Slug     string            `json:"slug"`
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

// statusChangeWrite is the body of a status-change call: the status, the expected sequence and the
// actor as the PATCH's daemonWrite carries them, and the route's own options, each left out when
// false. It is a type of its own so that no option can ride a PATCH, which a Dispatch ignores.
type statusChangeWrite struct {
	Status           string `json:"status"`
	ExpectedSequence int64  `json:"expected_sequence,omitempty"`
	IncludeChildren  bool   `json:"include_children,omitempty"`
	UnlinkChildren   bool   `json:"unlink_children,omitempty"`
	IncludeParent    bool   `json:"include_parent,omitempty"`
	DryRun           bool   `json:"dry_run,omitempty"`
	Actor            actor  `json:"actor"`
}

// statusChangeResponse is the status-change route's answer: the named issue as the PATCH answers
// it and an entry for each issue the call changed.
type statusChangeResponse struct {
	Issue   issueResponse               `json:"issue"`
	Changed []statusChangeEntryResponse `json:"changed"`
}

// statusChangeEntryResponse is one entry of a status-change answer's changed list, its issue null
// on a dry run.
type statusChangeEntryResponse struct {
	Key             string         `json:"key"`
	Status          string         `json:"status"`
	Parent          *string        `json:"parent"`
	PreviousLastSeq int64          `json:"previous_last_seq"`
	Issue           *issueResponse `json:"issue"`
}

// statusChangeEntries is a changed list as StatusChangeEntry values, nil for an empty one.
func statusChangeEntries(responses []statusChangeEntryResponse) []StatusChangeEntry {
	var entries []StatusChangeEntry
	for _, response := range responses {
		entry := StatusChangeEntry{
			Key: response.Key, Status: response.Status, Parent: response.Parent, PreviousLastSeq: response.PreviousLastSeq,
		}
		if response.Issue != nil {
			issue := response.Issue.issue()
			entry.Issue = &issue
		}
		entries = append(entries, entry)
	}
	return entries
}

// conflictIssueResponse is one issue a PARENT_STATUS_CONFLICT refusal names; its title and its
// relation to the written issue are left undecoded.
type conflictIssueResponse struct {
	Key    string `json:"key"`
	Status string `json:"status"`
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
