package dispatch

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const requestTimeout = 10 * time.Second

// HTTPClient calls Dispatch's native /api/v1 routes with the daemon's bearer token.
type HTTPClient struct {
	baseURL    string
	token      string
	timeout    time.Duration
	httpClient *http.Client
}

// New creates a client for one Dispatch server. The token is sent only as a bearer header.
func New(baseURL, token string) *HTTPClient {
	return &HTTPClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		timeout:    requestTimeout,
		httpClient: &http.Client{},
	}
}

// ListIssues reads GET /api/v1/issues?project=<project>. Dispatch accepts one status filter, so
// the client filters the complete ordered list itself. This retains Dispatch's rank order for any
// requested status subset.
func (c *HTTPClient) ListIssues(ctx context.Context, project string, statuses []string) ([]IssueSummary, error) {
	query := url.Values{}
	query.Set("project", project)

	var response []issueSummaryResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/issues?"+query.Encode(), nil, &response); err != nil {
		return nil, err
	}

	wanted := make(map[string]struct{}, len(statuses))
	for _, status := range statuses {
		wanted[status] = struct{}{}
	}
	issues := make([]IssueSummary, 0, len(response))
	for _, issue := range response {
		if len(wanted) > 0 {
			if _, ok := wanted[issue.Status]; !ok {
				continue
			}
		}
		issues = append(issues, IssueSummary{
			Key:          issue.Key,
			Title:        issue.Title,
			Status:       issue.Status,
			Parent:       issue.Parent,
			Rank:         issue.Rank,
			HandedOver:   CarriesLegionLabel(issue.Labels),
			LastSeq:      issue.LastSeq,
			ResolvedRepo: deref(issue.ResolvedRepo),
			BlockedBy:    issue.BlockedBy,
			ClaimedBy:    issue.Claim.holder(),
			ClaimUnread:  issue.Claim.unread(),
		})
	}
	return issues, nil
}

// GetIssue reads GET /api/v1/issues/{key}.
func (c *HTTPClient) GetIssue(ctx context.Context, key string) (Issue, error) {
	var response issueResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/issues/"+url.PathEscape(key), nil, &response); err != nil {
		return Issue{}, err
	}
	return response.issue(), nil
}

// ListRepoProjects reads GET /api/v1/settings/repo-projects: every project's repository mappings,
// which the caller filters to its own project.
func (c *HTTPClient) ListRepoProjects(ctx context.Context) ([]RepoProject, error) {
	var response []repoProjectResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/settings/repo-projects", nil, &response); err != nil {
		return nil, err
	}
	mappings := make([]RepoProject, 0, len(response))
	for _, mapping := range response {
		mappings = append(mappings, RepoProject{Repo: mapping.Repo, Project: mapping.Project, Default: mapping.Default})
	}
	return mappings, nil
}

// WebhookAppInstalled reads GET /api/v1/github/app-installation/{owner}/{repo}: whether
// Dispatch's GitHub App, the one GitHub sends repo's webhook events through to Envoy, is installed
// on repo (owner/name). A Dispatch older than the route answers 404, an *Error Missing reports.
func (c *HTTPClient) WebhookAppInstalled(ctx context.Context, repo string) (bool, error) {
	owner, name, _ := strings.Cut(repo, "/")
	var response struct {
		Installed bool `json:"installed"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/github/app-installation/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil, &response); err != nil {
		return false, err
	}
	return response.Installed, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// SetStatus updates an issue through PATCH /api/v1/issues/{key} and returns the issue as Dispatch
// answers the write: as the write left it, at the sequence of the event the write made
// (issue.updated, or issue.closed for done). Dispatch appends that event for every status it is
// sent, the issue's own status included.
func (c *HTTPClient) SetStatus(ctx context.Context, key, status string) (Issue, error) {
	var response issueResponse
	if err := c.request(ctx, http.MethodPatch, "/api/v1/issues/"+url.PathEscape(key), daemonWrite{
		Status: status,
		Actor:  daemonActor(key),
	}, &response); err != nil {
		return Issue{}, err
	}
	return response.issue(), nil
}

// SetStatusAt is SetStatus fenced on the issue's sequence as the caller read it
// (expected_sequence), as HandOver fences its own write: Dispatch refuses the write with
// ISSUE_CHANGED when anything was recorded on the issue since that read, rather than writing over
// it. Every event moves the sequence, the daemon's own messages among them, so a refusal says only
// that the read is stale: the caller reads the issue again and decides from what it then shows
// (the outbox's status row).
func (c *HTTPClient) SetStatusAt(ctx context.Context, issue Issue, status string) (Issue, error) {
	var response issueResponse
	if err := c.request(ctx, http.MethodPatch, "/api/v1/issues/"+url.PathEscape(issue.Key), daemonWrite{
		Status:           status,
		ExpectedSequence: issue.LastSeq,
		Actor:            daemonActor(issue.Key),
	}, &response); err != nil {
		return Issue{}, err
	}
	return response.issue(), nil
}

// ReleaseClaim clears the issue's Dispatch claim through DELETE /api/v1/issues/{key}/claim, as
// the daemon's own session. The actor body is required, not an extra: Dispatch's route decodes
// the request and refuses one that carries no Content-Type: application/json, which request sets
// only for a non-nil body. Every other daemonWrite field is omitempty, so what goes over the wire
// is the actor alone, which is all the route accepts. Dispatch answers 409 ISSUE_CLAIMED when
// this caller may not release the holder, and 409 CLAIM_CONTENDED when the holder changed twice
// while the request ran, so nothing was applied.
func (c *HTTPClient) ReleaseClaim(ctx context.Context, key string) error {
	return c.request(ctx, http.MethodDelete, "/api/v1/issues/"+url.PathEscape(key)+"/claim", daemonWrite{Actor: daemonActor(key)}, nil)
}

// PutGatedSteps publishes the deployment's design-gate steps onto project's row through PUT
// /api/v1/projects/{key}/gated-steps, as the daemon's own session, the actor every write of this
// client carries. The list is always sent, empty for a deployment that gates nothing: Dispatch
// refuses a body that names no list rather than read it as the empty one, so a nil slice would
// be refused where it means the same thing. A Dispatch older than the route answers 404, which
// the caller retries: the route arrives with the next server release, and nothing is lost by
// waiting for it.
func (c *HTTPClient) PutGatedSteps(ctx context.Context, project string, steps []string) error {
	if steps == nil {
		steps = []string{}
	}
	return c.request(ctx, http.MethodPut, "/api/v1/projects/"+url.PathEscape(project)+"/gated-steps", gatedStepsWrite{
		Steps: steps,
		Actor: daemonActor(project),
	}, nil)
}

// PutDesignGates publishes the document each of the issue's design-gate steps records onto its
// row through PUT /api/v1/issues/{key}/design-gates, as the daemon's own session, the actor
// every write of this client carries. The write replaces the issue's whole set, appends no event
// and notifies nobody. The list is always sent, empty for an issue whose gates record no
// document: Dispatch refuses a body that names no list rather than read it as the empty one, so a
// nil slice would be refused where it means the same thing. Dispatch answers 400 INVALID_ISSUE
// for a set it will not take and 404 ISSUE_NOT_FOUND for an issue it does not know; a Dispatch
// older than the route answers its router's 404 under the code NOT_FOUND, and one older than an
// entry's `holds` refuses the body 400 INVALID_JSON, since it refuses a field it does not know.
// The caller retries both until the server release that has them.
func (c *HTTPClient) PutDesignGates(ctx context.Context, key string, gates []DesignGateDocument) error {
	if gates == nil {
		gates = []DesignGateDocument{}
	}
	return c.request(ctx, http.MethodPut, "/api/v1/issues/"+url.PathEscape(key)+"/design-gates", designGatesWrite{
		Gates: gates,
		Actor: daemonActor(key),
	}, nil)
}

// HandOver writes issue's status for its hand-over to Legion, on the issue's sequence as the client
// read it (expected_sequence), so a person's move since that read is refused (ISSUE_CHANGED) rather
// than overwritten. An issue that does not yet carry LegionLabel has it added in the same PATCH
// (labels_add, resolved inside Dispatch's row lock so a label another actor adds after this read
// cannot be lost), and the one event the write makes carries both. An issue a person already
// labelled gets the status alone.
func (c *HTTPClient) HandOver(ctx context.Context, issue Issue, status string) error {
	write := daemonWrite{Status: status, ExpectedSequence: issue.LastSeq, Actor: daemonActor(issue.Key)}
	if !issue.HandedOver {
		write.LabelsAdd = []string{LegionLabel}
	}
	return c.request(ctx, http.MethodPatch, "/api/v1/issues/"+url.PathEscape(issue.Key), write, nil)
}

// PostMessage writes a durable notice through POST /api/v1/issues/{key}/messages. Dispatch's
// issue-message route has no idempotency-key field, so this method deliberately sends none.
func (c *HTTPClient) PostMessage(ctx context.Context, key, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/issues/"+url.PathEscape(key)+"/messages", daemonWrite{
		Body:  body,
		Actor: daemonActor(key),
	}, nil)
}

// ArtifactSlugs reads GET /api/v1/issues/{key}/artifacts and returns the slug of every document
// and file the issue holds. A caller that names its own document reads it before an upload, so
// a retry finds the document its first attempt made rather than writing it a second version.
func (c *HTTPClient) ArtifactSlugs(ctx context.Context, key string) ([]string, error) {
	var artifacts []struct {
		Slug string `json:"slug"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/issues/"+url.PathEscape(key)+"/artifacts", nil, &artifacts); err != nil {
		return nil, err
	}
	slugs := make([]string, len(artifacts))
	for i, artifact := range artifacts {
		slugs[i] = artifact.Slug
	}
	return slugs, nil
}

// UploadArtifact writes a markdown document on the issue through POST
// /api/v1/issues/{key}/artifacts, as the daemon's own session, and returns the slug Dispatch filed
// it under, which a link to it names (dispatch://<key>/artifact/<slug>). The body holds only
// fields Dispatch's JSON upload reads (its jsonArtifactUpload), since its decoder refuses any
// other. A name the issue already holds is that document's next version.
func (c *HTTPClient) UploadArtifact(ctx context.Context, key, name, content string) (string, error) {
	var response struct {
		Artifact struct {
			Slug string `json:"slug"`
		} `json:"artifact"`
	}
	if err := c.request(ctx, http.MethodPost, "/api/v1/issues/"+url.PathEscape(key)+"/artifacts", artifactUpload{
		Name:    name,
		Content: content,
		Actor:   daemonActor(key),
	}, &response); err != nil {
		return "", err
	}
	if response.Artifact.Slug == "" {
		return "", fmt.Errorf("upload Dispatch document %s of %s: the answer names no slug", name, key)
	}
	return response.Artifact.Slug, nil
}

// EventsAfter reads the issue's Dispatch event log after sequence after, oldest first, through GET
// /api/v1/issues/{key}/events?after=…, every page of it: each event as Dispatch serves it, which is
// the shape its durable stream carries, so the boot catch-up applies each through the live
// decoder, with its own sequence.
func (c *HTTPClient) EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error) {
	events := []json.RawMessage{}
	for {
		query := url.Values{}
		query.Set("after", fmt.Sprint(after))
		query.Set("limit", fmt.Sprint(eventPageLimit))
		var page []json.RawMessage
		if err := c.request(ctx, http.MethodGet, "/api/v1/issues/"+url.PathEscape(key)+"/events?"+query.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, raw := range page {
			var event struct {
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				return nil, fmt.Errorf("page issue events for %s: %w", key, err)
			}
			if event.Seq <= after {
				return nil, fmt.Errorf("page issue events for %s: non-ascending sequence %d after %d", key, event.Seq, after)
			}
			after = event.Seq
			events = append(events, raw)
		}
		if len(page) < eventPageLimit {
			return events, nil
		}
	}
}

const eventPageLimit = 200

// MessageBodiesSince reads newest-first issue events until the bounded outbox recovery window.
// Message bodies are carried by message.created events, which hold at most the first part of a
// long body (contracts.DispatchEventBodyMax) and say so with body_truncated; the whole of such a
// body is read from GET /api/v1/issues/{key}/messages/{id}, since the outbox's marker closes a
// body and the cut would drop it.
func (c *HTTPClient) MessageBodiesSince(ctx context.Context, key string, since time.Time) ([]string, error) {
	before := int64(0)
	bodies := []string{}
	for {
		query := url.Values{}
		query.Set("order", "desc")
		query.Set("limit", fmt.Sprint(eventPageLimit))
		if before != 0 {
			query.Set("before", fmt.Sprint(before))
		}
		var events []issueEventResponse
		path := "/api/v1/issues/" + url.PathEscape(key) + "/events?" + query.Encode()
		if err := c.request(ctx, http.MethodGet, path, nil, &events); err != nil {
			return nil, err
		}
		if len(events) == 0 {
			return bodies, nil
		}
		for _, event := range events {
			if event.CreatedAt.Before(since) {
				return bodies, nil
			}
			if event.Type != "message.created" {
				continue
			}
			var message struct {
				ID            string `json:"id"`
				Body          string `json:"body"`
				BodyTruncated bool   `json:"body_truncated"`
			}
			if err := json.Unmarshal(event.Payload, &message); err != nil {
				return nil, fmt.Errorf("decode message.created event %d for %s: %w", event.Seq, key, err)
			}
			if message.Body == "" {
				return nil, fmt.Errorf("decode message.created event %d for %s: payload body is required", event.Seq, key)
			}
			body := message.Body
			if message.BodyTruncated {
				whole, err := c.messageBody(ctx, key, message.ID)
				if err != nil {
					return nil, err
				}
				body = whole
			}
			bodies = append(bodies, body)
		}
		if len(events) < eventPageLimit {
			return bodies, nil
		}
		next := events[len(events)-1].Seq
		if next <= 0 || next >= before && before != 0 {
			return nil, fmt.Errorf("page issue events for %s: non-descending sequence %d", key, next)
		}
		before = next
	}
}

// messageBody is the whole stored body of message id on issue key, read from
// GET /api/v1/issues/{key}/messages/{id}, whose answer is the message with its deliveries and its
// replies (the Dispatch server's messageRead). It is what an event carrying only the first part of
// a long body points at.
func (c *HTTPClient) messageBody(ctx context.Context, key, id string) (string, error) {
	var response struct {
		Message struct {
			Body string `json:"body"`
		} `json:"message"`
	}
	path := "/api/v1/issues/" + url.PathEscape(key) + "/messages/" + url.PathEscape(id)
	if err := c.request(ctx, http.MethodGet, path, nil, &response); err != nil {
		return "", fmt.Errorf("read Dispatch message %s of %s: %w", id, key, err)
	}
	if response.Message.Body == "" {
		return "", fmt.Errorf("read Dispatch message %s of %s: the message carries no body", id, key)
	}
	return response.Message.Body, nil
}

// Approval reads GET /api/v1/artifacts/{id} and returns the document's derived approval state,
// the issue carrying it, and what the document is on that issue: its primary flag, its role and
// its slug, which is how a design-gate step finds the document it binds to.
func (c *HTTPClient) Approval(ctx context.Context, artifactID string) (Approval, error) {
	var response artifactResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(artifactID), nil, &response); err != nil {
		return Approval{}, err
	}
	if response.Approval == nil {
		return Approval{}, fmt.Errorf("Dispatch artifact %q has no approval", artifactID)
	}
	issueKey := ""
	if response.IssueKey != nil {
		issueKey = *response.IssueKey
	}
	approved := Approval{
		IssueKey:      issueKey,
		State:         response.Approval.State,
		LatestVersion: response.Approval.LatestVersion,
		Version:       response.Approval.Version,
		Reason:        response.Approval.Reason,
		Primary:       response.Primary,
		Role:          response.Role,
		Slug:          response.Slug,
	}
	if response.Approval.At != nil {
		at, err := time.Parse(time.RFC3339Nano, *response.Approval.At)
		if err != nil {
			return Approval{}, fmt.Errorf("Dispatch artifact %q approval has no RFC 3339 at: %w", artifactID, err)
		}
		approved.At = at.UTC()
	}
	if response.Approval.Seq != nil {
		approved.Seq = *response.Approval.Seq
	}
	if response.Approval.By != nil {
		approved.By = Actor(*response.Approval.By)
	}
	// Only an open request is named: Dispatch names the ask an older review answered too, which is
	// no request anyone is asked to act on now.
	if response.Approval.State == "awaiting" {
		approved.AskID, approved.WaitingOn = deref(response.Approval.AskID), response.Approval.WaitingOn
	}
	return approved, nil
}

// ArtifactChanges reads GET /api/v1/artifacts/{id}/changes?from=&to=: what changed in the document
// between versions from and to, both positive and from below to, as Dispatch answers them (a pair
// from 0 has no earlier version to compare, and Dispatch refuses it). A 404 ARTIFACT_NOT_FOUND is
// ErrDocumentNotFound wrapped around Dispatch's own refusal: Dispatch holds no document of that
// id, a document deleted since its gate recorded it. Any other 404, and a 405, is
// ErrNoChangesRoute wrapped the same way: a Dispatch whose release has no such route answers its
// router's 404 or 405. Either way the caller records the pair unreadable rather than waiting, for
// an upgrade or for a document that does not come back. Dispatch also answers 404 for a version
// the document does not have, which the daemon does not ask for, since the versions it reads are
// the ones its gate recorded. Every other failure is the request's own, which a caller rides out.
func (c *HTTPClient) ArtifactChanges(ctx context.Context, artifactID string, from, to int) (ArtifactChanges, error) {
	query := url.Values{}
	query.Set("from", fmt.Sprint(from))
	query.Set("to", fmt.Sprint(to))
	var changes ArtifactChanges
	if err := c.request(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(artifactID)+"/changes?"+query.Encode(), nil, &changes); err != nil {
		var refusal *Error
		switch {
		case !errors.As(err, &refusal):
		case refusal.Status == http.StatusNotFound && refusal.Code == ArtifactNotFound:
			return ArtifactChanges{}, fmt.Errorf("%w: %w", ErrDocumentNotFound, err)
		case refusal.Status == http.StatusNotFound || refusal.Status == http.StatusMethodNotAllowed:
			return ArtifactChanges{}, fmt.Errorf("%w: %w", ErrNoChangesRoute, err)
		}
		return ArtifactChanges{}, err
	}
	return changes, nil
}

// ApprovedVersionBefore reads GET /api/v1/artifacts/{id}/reviews, every review of the document,
// newest first, and answers the highest version below version that one of them approved, 0 when
// none did: the version a holder whose told version the record does not know is taken to have
// worked from. It takes the highest such version wherever it stands in the list, so the answer
// rests on no order of the list: a changes request names no approved version, and a review of a
// version at or above the one asked about is not before it.
func (c *HTTPClient) ApprovedVersionBefore(ctx context.Context, artifactID string, version int) (int, error) {
	var reviews []reviewResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(artifactID)+"/reviews", nil, &reviews); err != nil {
		return 0, err
	}
	approved := 0
	for _, review := range reviews {
		if review.State == "approved" && review.Version < version && review.Version > approved {
			approved = review.Version
		}
	}
	return approved, nil
}

// Handoff reads GET /api/v1/handoffs/{id}: one handoff record with its issue, phase, version and
// author. A record Dispatch does not have is its 404 HANDOFF_NOT_FOUND (Missing).
func (c *HTTPClient) Handoff(ctx context.Context, id string) (Handoff, error) {
	var response handoffResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/handoffs/"+url.PathEscape(id), nil, &response); err != nil {
		return Handoff{}, err
	}
	return response.handoff(), nil
}

func (c *HTTPClient) request(ctx context.Context, method, path string, body, into any) error {
	bounded, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Dispatch %s %s: %w", method, path, err)
		}
	}
	request, err := http.NewRequestWithContext(bounded, method, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create Dispatch %s %s: %w", method, path, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call Dispatch %s %s: %w", method, path, transportFailure(err))
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read Dispatch %s %s: %w", method, path, transportFailure(err))
	}
	if response.StatusCode/100 != 2 {
		return dispatchError(response.StatusCode, response.Status, payload)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(payload, into); err != nil {
		return fmt.Errorf("decode Dispatch %s %s response: %w", method, path, err)
	}
	return nil
}

// transportFailure wraps err as a TransientError unless it is a misconfiguration no wait fixes,
// never an outage: a certificate Go's client refuses to trust (a wrong or missing CA, a hostname
// mismatch, an expired or otherwise invalid certificate — crypto/tls wraps every one of these as
// *tls.CertificateVerificationError, never a bare x509 error, since httpClient sets no
// VerifyPeerCertificate or VerifyConnection of its own), or an https:// dispatch_url pointed at a
// plain-HTTP port (http.ErrSchemeMismatch) — dispatchBase's scheme check (internal/config)
// catches a typo in the scheme itself before boot, but not a correct https:// whose port serves
// plain HTTP. Left wrapped, either would satisfy Unreachable exactly like a dial refused or reset
// would, and a boot gate would retry a certificate or scheme problem forever instead of refusing
// loud.
func transportFailure(err error) error {
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return err
	}
	if errors.Is(err, http.ErrSchemeMismatch) {
		return err
	}
	return &TransientError{Err: err}
}

// DaemonSession is the session the daemon writes every Dispatch status and message of the issue key
// under: legion-daemon:<PROJECT>, PROJECT being the key's.
func DaemonSession(key string) string {
	project, _, _ := strings.Cut(key, "-")
	return "legion-daemon:" + project
}

func daemonActor(key string) actor {
	return actor{Kind: "session", ID: DaemonSession(key)}
}

func dispatchError(status int, statusText string, payload []byte) *Error {
	var response struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		response.Error = strings.TrimSpace(string(payload))
	}
	if response.Error == "" {
		response.Error = statusText
	}
	return &Error{Status: status, Code: response.Code, Message: response.Error}
}

type issueSummaryResponse struct {
	Key          string         `json:"key"`
	Title        string         `json:"title"`
	Status       string         `json:"status"`
	Parent       *string        `json:"parent"`
	Rank         string         `json:"rank"`
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

type repoProjectResponse struct {
	Repo    string `json:"repo"`
	Project string `json:"project"`
	Default bool   `json:"default"`
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
