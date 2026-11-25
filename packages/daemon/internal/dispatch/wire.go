package dispatch

// The JSON shapes the client reads of Dispatch's answers and writes in its own requests: each
// answer decoded through the answer (Answer.Decode) and turned into the package's types, and each
// write's body, which carries the daemon's actor.

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
	Key               string         `json:"key"`
	Project           string         `json:"project"`
	Title             string         `json:"title"`
	Status            string         `json:"status"`
	Parent            *string        `json:"parent"`
	Rank              string         `json:"rank"`
	Priority          *int           `json:"priority"`
	Labels            []string       `json:"labels"`
	PrimaryArtifactID string         `json:"primary_artifact_id"`
	LastSeq           int64          `json:"last_seq"`
	ResolvedRepo      *string        `json:"resolved_repo"`
	BlockedBy         []string       `json:"blocked_by"`
	Claim             *claimResponse `json:"claim"`
}

// issue is the response as Issue: Dispatch's issue detail, and the issue its PATCH answers with.
func (r issueResponse) issue() Issue {
	return Issue{
		Key:               r.Key,
		Project:           r.Project,
		Title:             r.Title,
		Status:            r.Status,
		Parent:            r.Parent,
		Rank:              r.Rank,
		Priority:          r.Priority,
		Labels:            r.Labels,
		HandedOver:        CarriesLegionLabel(r.Labels),
		PrimaryArtifactID: r.PrimaryArtifactID,
		LastSeq:           r.LastSeq,
		ResolvedRepo:      deref(r.ResolvedRepo),
		BlockedBy:         r.BlockedBy,
		Claim:             r.Claim.claim(),
		ClaimUnread:       r.Claim.unread(),
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

type artifactResponse struct {
	IssueKey *string           `json:"issue_key"`
	Slug     string            `json:"slug"`
	Primary  bool              `json:"primary"`
	Role     string            `json:"role"`
	Approval *approvalResponse `json:"approval"`
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

// reviewResponse is one review as GET /api/v1/artifacts/{id}/reviews serves it, read for the
// version it was given on and its state (`approved` or `changes_requested`) alone.
type reviewResponse struct {
	Version int    `json:"version"`
	State   string `json:"state"`
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

// planWrite is the body of a plan's write: the plan's fields and the daemon's actor.
type planWrite struct {
	PlanWrite
	Actor actor `json:"actor"`
}
