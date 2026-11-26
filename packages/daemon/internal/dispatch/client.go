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
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/tlsalert"
)

const requestTimeout = 10 * time.Second

// HTTPClient calls Dispatch's native /api/v1 routes, presenting the daemon's credential through the
// transport it was made with (New).
type HTTPClient struct {
	baseURL    string
	timeout    time.Duration
	httpClient *http.Client
}

// New creates a client for one Dispatch server whose every request goes through transport, which
// presents the daemon's credential: BearerTransport for the bearer dispatch_token_file names, or,
// under dispatch_token_broker, the daemon's own broker token (cipherclient.NewTokenTransport). The
// client sets no Authorization of its own.
func New(baseURL string, transport http.RoundTripper) *HTTPClient {
	return &HTTPClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		timeout:    requestTimeout,
		httpClient: &http.Client{Transport: transport},
	}
}

// BearerTransport is a RoundTripper that sends each request through base (http.DefaultTransport
// when nil) with `Authorization: Bearer <token>` and changes nothing else, so a request goes on the
// wire as it did when the client set the header itself. A redirect to another host name than the
// request was made to is sent without it, as Go's client sent no Authorization header its caller
// set on such a redirect: the bearer reaches Dispatch's host and no other. Go's client also kept
// it on a redirect to a subdomain of that host; this does not.
func BearerTransport(token string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return bearerTransport{authorization: "Bearer " + token, base: base}
}

type bearerTransport struct {
	authorization string
	base          http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	presented := r.Clone(r.Context())
	if strings.EqualFold(r.URL.Hostname(), firstRequest(r).URL.Hostname()) {
		presented.Header.Set("Authorization", b.authorization)
	}
	return b.base.RoundTrip(presented)
}

// firstRequest is the request the caller made that r is a redirect of, or r itself: Go's client
// sets each redirect's Response to the answer that redirected it, whose Request is the one before.
func firstRequest(r *http.Request) *http.Request {
	for r.Response != nil && r.Response.Request != nil {
		r = r.Response.Request
	}
	return r
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
			Key:           issue.Key,
			Title:         issue.Title,
			Status:        issue.Status,
			Parent:        issue.Parent,
			Rank:          issue.Rank,
			Priority:      issue.Priority,
			HandedOver:    CarriesLegionLabel(issue.Labels),
			Labels:        issue.Labels,
			LastSeq:       issue.LastSeq,
			ResolvedRepo:  deref(issue.ResolvedRepo),
			BlockedBy:     issue.BlockedBy,
			ClaimedBy:     issue.Claim.holder(),
			ClaimUnread:   issue.Claim.unread(),
			Assignee:      issue.Assignee.login,
			AssigneeKnown: issue.Assignee.known,
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

// OpenAsks reads GET /api/v1/asks/open?project=<project>: the project's open asks and its default
// assignee, which Dispatch reads in one statement, so no move of the default falls between the
// two. It keeps the asks of issues alone: an ask of a project's document is no ask of an issue, and
// the approvals signal counts the asks of issues, as the replay does. A Dispatch before
// default_assignee answers none, and so does one that stores no default of the project (JSON null):
// OpenAsks.DefaultAssignee is then "", never a person guessed from the one an ask waits on
// (waiting_on_person). An answer that lists an ask of an issue with no id or no key is an error
// rather than a read with a hole. One whose as_of is no time reads as of no time (OpenAsks.AsOf
// zero): the store keeps the time beside the default assignee and no reading of the signal reads
// it, so it fails no read.
func (c *HTTPClient) OpenAsks(ctx context.Context, project string) (OpenAsks, error) {
	query := url.Values{}
	query.Set("project", project)
	var response struct {
		AsOf string `json:"as_of"`
		Asks []struct {
			ID    string `json:"id"`
			Owner struct {
				Issue *struct {
					Key string `json:"key"`
				} `json:"issue"`
			} `json:"owner"`
		} `json:"asks"`
		DefaultAssignee personMember `json:"default_assignee"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/asks/open?"+query.Encode(), nil, &response); err != nil {
		return OpenAsks{}, err
	}
	open := OpenAsks{Asks: []OpenAsk{}, DefaultAssignee: response.DefaultAssignee.login}
	if asOf, err := time.Parse(time.RFC3339Nano, response.AsOf); err == nil {
		open.AsOf = asOf.UTC()
	}
	for _, ask := range response.Asks {
		if ask.Owner.Issue == nil {
			continue
		}
		if ask.ID == "" || ask.Owner.Issue.Key == "" {
			return OpenAsks{}, fmt.Errorf("read the open asks of %s: an ask of an issue names no id or no issue key", project)
		}
		open.Asks = append(open.Asks, OpenAsk{ID: ask.ID, Issue: ask.Owner.Issue.Key})
	}
	return open, nil
}

// IssueAsks reads GET /api/v1/issues/<key>/asks?state=open: each open ask of the issue, with the
// person it was handed to (asked_of, "" for none) and whether it waits on a person, as Dispatch
// computes it (waiting_on: `human` or `agent`). An open ask whose turn is neither is an error, since
// the daemon would otherwise guess it.
func (c *HTTPClient) IssueAsks(ctx context.Context, key string) ([]IssueAsk, error) {
	var response []struct {
		ID        string  `json:"id"`
		State     string  `json:"state"`
		AskedOf   *string `json:"asked_of"`
		WaitingOn string  `json:"waiting_on"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/issues/"+url.PathEscape(key)+"/asks?state=open", nil, &response); err != nil {
		return nil, err
	}
	asks := make([]IssueAsk, 0, len(response))
	for _, ask := range response {
		if ask.ID == "" || (ask.WaitingOn != "human" && ask.WaitingOn != "agent") {
			return nil, fmt.Errorf("read the open asks of %s: the ask %q is open and waits on %q, neither the person nor the agent", key, ask.ID, ask.WaitingOn)
		}
		asks = append(asks, IssueAsk{ID: ask.ID, AskedOf: deref(ask.AskedOf), WaitsOnPerson: ask.WaitingOn == "human"})
	}
	return asks, nil
}

// personMember is a person member of a Dispatch read (an issue's assignee, a project's default
// assignee): the login, "" for JSON null, and whether the read carried a login or null at all,
// which a Dispatch before the member does not. A member that is neither reads as not carried, as
// an unreadable claim does (claimResponse.unread), so an issue read the workflow acts on is never
// lost to a member only the approvals signal reads.
type personMember struct {
	login string
	known bool
}

func (p *personMember) UnmarshalJSON(raw []byte) error {
	var login *string
	if json.Unmarshal(raw, &login) == nil {
		p.login, p.known = deref(login), true
	}
	return nil
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
// be refused where it means the same thing. A Dispatch older than the route answers its router's
// 404 (NoRoute), which the outbox's row waits on: the route arrives with the next server release,
// and nothing is lost by waiting for it.
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
// older than the route answers its router's 404 (NoRoute), which the outbox's row waits on until
// the server release that has the route. A Dispatch that has the route but not an entry's `holds`
// ignores that member, as it ignores every member a body carries that it does not declare (its
// decodeJSON), and answers 200: the publish finishes, and that Dispatch keeps no `holds` for the
// row. Its advice then reads the row by the project's gated steps, so a `criteria` row a root
// registered beside a deployment whose list names `spec` and not `criteria` is left out of the
// superseded-approval advice until the set is published again, which every daemon boot does
// (queueRecordedGatesPublish). So deploy Dispatch first, or restart the daemon once Dispatch has
// the column.
func (c *HTTPClient) PutDesignGates(ctx context.Context, key string, gates []DesignGateDocument) error {
	if gates == nil {
		gates = []DesignGateDocument{}
	}
	return c.request(ctx, http.MethodPut, "/api/v1/issues/"+url.PathEscape(key)+"/design-gates", designGatesWrite{
		Gates: gates,
		Actor: daemonActor(key),
	}, nil)
}

// PutRoleHolders publishes the session of each Legion role of the issue through PUT
// /api/v1/issues/{key}/role-holders, as the daemon's own session, which is the one caller that
// route takes. The write replaces the issue's whole set, appends no event and notifies nobody.
// The list is always sent, empty for an issue where no session holds a role: Dispatch refuses a
// body that names no list, and the empty list is the publication that removes every holder.
// Dispatch answers 400 INVALID_ROLE_HOLDER for a set it will not take and 404 ISSUE_NOT_FOUND for
// an issue it does not know; a Dispatch older than the route answers its router's 404 (NoRoute),
// which the outbox's row waits on until the server release that has it.
func (c *HTTPClient) PutRoleHolders(ctx context.Context, key string, holders []RoleHolder) error {
	if holders == nil {
		holders = []RoleHolder{}
	}
	return c.request(ctx, http.MethodPut, "/api/v1/issues/"+url.PathEscape(key)+"/role-holders", roleHoldersWrite{
		Holders: holders,
		Actor:   daemonActor(key),
	}, nil)
}

// PutStep writes one start or one end of a step onto Dispatch's record id of the issue key,
// through PUT /api/v1/issues/{key}/steps/{id}, as the daemon's own session, the one session the
// route takes a record of the issue from. The write appends no event and notifies nobody. Dispatch
// answers 400 STEP_INPUT for a value it will not take, 403 STEP_WRITER for any other writer, 404
// ISSUE_NOT_FOUND for an issue it does not know and 409 STEP_OTHER_ISSUE for an id that is a
// record of another issue; a Dispatch older than the route answers its router's 404 (NoRoute),
// which the outbox's row waits on until the server release that has it.
func (c *HTTPClient) PutStep(ctx context.Context, key, id string, write StepWrite) error {
	return c.request(ctx, http.MethodPut, "/api/v1/issues/"+url.PathEscape(key)+"/steps/"+url.PathEscape(id), stepWrite{
		StepWrite: write,
		Actor:     daemonActor(key),
	}, nil)
}

// PutStepRating writes the rating a retro gave the plan onto the retro's record id of the issue
// key, through the record's rating route, PUT /api/v1/issues/{key}/steps/{id}/rating, as the
// daemon's own session. The body is the step write's, with the identity of the retro's run, which
// makes the record where its start and end have not landed yet, and the rating and its reason. The
// rating has a route of its own because the step route ignores a member it does not declare, so a
// rating sent there would be answered 200 and lost. Dispatch answers the step route's refusals,
// and 400 STEP_INPUT for a rating off its rule or on a record that is no retro; a Dispatch older
// than the route answers its router's 404 (NoRoute), which the outbox's row waits on.
func (c *HTTPClient) PutStepRating(ctx context.Context, key, id string, write StepWrite, rating PlanRating) error {
	return c.request(ctx, http.MethodPut, "/api/v1/issues/"+url.PathEscape(key)+"/steps/"+url.PathEscape(id)+"/rating", stepRatingWrite{
		StepWrite:  write,
		PlanRating: rating,
		Actor:      daemonActor(key),
	}, nil)
}

// PutStepExperiment writes the experiment and the variant of the record's issue onto the record id
// of the issue key, through the record's experiment route, PUT
// /api/v1/issues/{key}/steps/{id}/experiment, as the daemon's own session. The body is the run's
// identity out of write, of which Dispatch sets each field the record does not hold yet and keeps
// each it holds, and the pair, and no other field: the start, the end and the rating are the
// record's own writes, which the outbox sends first (publishStepRecord), so a Dispatch older than
// this route takes the record and only the pair waits. The pair has a route of its own because the
// step route and the rating route ignore a member they do not declare, so a pair sent there would
// be answered 200 and lost. Dispatch answers the step route's refusals, and 400 STEP_INPUT for a
// pair off its rule; a Dispatch older than the route answers its router's 404 (NoRoute), which the
// outbox's row waits on.
func (c *HTTPClient) PutStepExperiment(ctx context.Context, key, id string, write StepWrite, experiment StepExperiment) error {
	identity := StepWrite{Task: write.Task, Step: write.Step, Kind: write.Kind, Role: write.Role, Type: write.Type,
		Agent: write.Agent, Generation: write.Generation}
	return c.request(ctx, http.MethodPut, "/api/v1/issues/"+url.PathEscape(key)+"/steps/"+url.PathEscape(id)+"/experiment", stepExperimentWrite{
		StepWrite:      identity,
		StepExperiment: experiment,
		Actor:          daemonActor(key),
	}, nil)
}

// PutPlan writes the plan one run of the issue key carries out onto Dispatch's plan id of that
// issue, through PUT /api/v1/issues/{key}/plans/{id}, as the daemon's own session, the one session
// the route takes a plan of the issue from. The write appends no event and notifies nobody. The
// plan's source, document, version and tasks are one fact: a write whose version is above the
// version Dispatch holds replaces all four, and a write at or below it changes nothing, a version
// left out (the source default) counting as 0, so a plan sent again or late changes nothing.
// Dispatch answers 400 PLAN_INPUT for a plan it will not take, 403 STEP_WRITER for any other
// writer, 404 ISSUE_NOT_FOUND for an issue it does not know and 409 PLAN_OTHER_ISSUE for an id
// that is a plan of another issue; a Dispatch older than the route answers its router's 404
// (NoRoute), which the outbox's row waits on until the server release that has it.
func (c *HTTPClient) PutPlan(ctx context.Context, key, id string, plan PlanWrite) error {
	return c.request(ctx, http.MethodPut, "/api/v1/issues/"+url.PathEscape(key)+"/plans/"+url.PathEscape(id), planWrite{
		PlanWrite: plan,
		Actor:     daemonActor(key),
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
// other. It names no token, so it is meant as a first upload, and the outbox uploads only a name
// the issue lists no document under (messageArtifact). Since legion-envoy-v32.36.1 Dispatch stores
// an upload naming no token over a name the issue already holds, as it did before it began to ask
// for one, and counts it (envoy_dispatch_document_replacements_unguarded_total); a token behind
// the live document is still refused 409 PRECONDITION_FAILED.
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
// decoder, with its own sequence. A page that holds an event that does not decode, or whose seq is
// not above the sequence read before it, fails the read with an *EventLogError naming that event.
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
				return nil, &EventLogError{Event: len(events) + 1, Err: fmt.Errorf("page issue events for %s: %w", key, err)}
			}
			if event.Seq <= after {
				return nil, &EventLogError{Event: len(events) + 1, Seq: event.Seq,
					Err: fmt.Errorf("page issue events for %s: non-ascending sequence %d after %d", key, event.Seq, after)}
			}
			after = event.Seq
			events = append(events, raw)
		}
		if len(page) < eventPageLimit {
			return events, nil
		}
	}
}

// EventLogError is the error for an event of an issue's Dispatch event log that the daemon cannot
// read. EventsAfter gives it for an event of a page that it cannot read: an event that does not
// decode, or whose seq is not above the sequence read before it. The intake's walk of the log
// (walkIssueEvents in internal/intake) gives it for an event that it cannot decode. Event is the
// event's place in the log read, counted from 1. Seq is the event's sequence where the event
// decoded, and 0 where it did not. Err names the cause and, from EventsAfter, the issue; Error
// returns its text.
type EventLogError struct {
	Event int
	Seq   int64
	Err   error
}

func (e *EventLogError) Error() string { return e.Err.Error() }
func (e *EventLogError) Unwrap() error { return e.Err }

const eventPageLimit = 200

// MessageBodiesSince reads newest-first issue events until the bounded outbox recovery window.
// Message bodies are carried by message.created events, which hold at most the first part of a
// long body (contracts.DispatchEventBodyMax) and say so with body_truncated; the whole of such a
// body is read from GET /api/v1/issues/{key}/messages/{id}, since the outbox's marker closes a
// body and the cut would drop it. A message its author deleted keeps its message.created event,
// whose every read carries deleted_at and no body: it is no notice the outbox posted, so the read
// passes over it, and any other event with no body is still refused.
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
				ID            string          `json:"id"`
				Body          string          `json:"body"`
				BodyTruncated bool            `json:"body_truncated"`
				DeletedAt     json.RawMessage `json:"deleted_at"`
			}
			if err := json.Unmarshal(event.Payload, &message); err != nil {
				return nil, fmt.Errorf("decode message.created event %d for %s: %w", event.Seq, key, err)
			}
			if len(message.DeletedAt) > 0 && string(message.DeletedAt) != "null" {
				continue
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

// PlanDocument reads one version of a document with its typed blocks: GET
// /api/v1/artifacts/{id}/versions/{version}?blocks=true, whose blocks Dispatch reads from that
// version's own text, then GET /api/v1/artifacts/{id} for the issue carrying it. Each block's
// text is its body, cut from the markdown of the same answer at the block's byte range, without
// its opening line and its closing fence. A Dispatch that answers no blocks key predates that read
// and gives ErrNoBlocks. An answer for another version, or a block whose range is not a fenced
// block of its markdown, is an error naming it; a refusal of Dispatch's passes through (Missing).
func (c *HTTPClient) PlanDocument(ctx context.Context, artifactID string, version int) (DocumentVersion, error) {
	if version < 1 {
		return DocumentVersion{}, fmt.Errorf("read Dispatch document %s at version %d: a version is 1 or more", artifactID, version)
	}
	var response versionBlocksResponse
	path := "/api/v1/artifacts/" + url.PathEscape(artifactID) + "/versions/" + strconv.Itoa(version) + "?blocks=true"
	if err := c.request(ctx, http.MethodGet, path, nil, &response); err != nil {
		return DocumentVersion{}, err
	}
	if response.Blocks == nil {
		return DocumentVersion{}, ErrNoBlocks
	}
	if response.Number != version {
		return DocumentVersion{}, fmt.Errorf("read Dispatch document %s at version %d: Dispatch answered version %d", artifactID, version, response.Number)
	}
	document := DocumentVersion{ArtifactID: artifactID, Version: version, Blocks: make([]DocumentBlock, 0, len(*response.Blocks))}
	for _, block := range *response.Blocks {
		if block.From < 0 || block.To < block.From || block.To > len(response.Markdown) {
			return DocumentVersion{}, fmt.Errorf("read Dispatch document %s at version %d: block %s spans %d to %d of %d bytes", artifactID, version, block.ID, block.From, block.To, len(response.Markdown))
		}
		rendering := response.Markdown[block.From:block.To]
		_, rest, opened := strings.Cut(rendering, "\n")
		closing := strings.LastIndexByte(rest, '\n')
		if !opened || closing < 0 {
			return DocumentVersion{}, fmt.Errorf("read Dispatch document %s at version %d: block %s is %q, not a fenced block", artifactID, version, block.ID, rendering)
		}
		attributes := make(map[string]string, len(block.Attributes))
		for _, attribute := range block.Attributes {
			attributes[attribute.Name] = attribute.Value
		}
		document.Blocks = append(document.Blocks, DocumentBlock{ID: block.ID, Type: block.Type, Attributes: attributes, Text: rest[:closing]})
	}
	var artifact artifactResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(artifactID), nil, &artifact); err != nil {
		return DocumentVersion{}, err
	}
	if artifact.IssueKey != nil {
		document.IssueKey = *artifact.IssueKey
	}
	return document, nil
}

// ArtifactChanges reads GET /api/v1/artifacts/{id}/changes?from=&to=: what changed in the document
// between versions from and to, both positive and from below to, as Dispatch answers them (a pair
// from 0 has no earlier version to compare, and Dispatch refuses it). A 404 ARTIFACT_NOT_FOUND is
// ErrDocumentNotFound wrapped around Dispatch's own refusal (documentNotFound): Dispatch holds no
// document of that id, a document deleted since its gate recorded it, and the caller records the
// pair unreadable at once, since a deleted document does not come back. The router's `404
// NOT_FOUND` `no route for <METHOD> <path>`, and a 405, are ErrNoChangesRoute wrapped the same
// way: the build that answered has no such route. During a roll of Dispatch two builds answer,
// and the next attempt can reach the one that has it, so the caller retries that answer as any
// other failure within its bounded attempts. Dispatch also answers 404 NOT_FOUND for a version
// the document does not have, which the daemon does not ask for, since the versions it reads are
// the ones its gate recorded; that answer, and every other failure, is the request's own, which a
// caller rides out.
func (c *HTTPClient) ArtifactChanges(ctx context.Context, artifactID string, from, to int) (ArtifactChanges, error) {
	query := url.Values{}
	query.Set("from", fmt.Sprint(from))
	query.Set("to", fmt.Sprint(to))
	var changes ArtifactChanges
	if err := c.request(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(artifactID)+"/changes?"+query.Encode(), nil, &changes); err != nil {
		var refusal *Error
		switch {
		case !errors.As(err, &refusal):
		case artifactNotFound(refusal):
			return ArtifactChanges{}, documentNotFound(err)
		case NoRoute(err), refusal.Status == http.StatusMethodNotAllowed:
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
// version at or above the one asked about is not before it. A 404 ARTIFACT_NOT_FOUND is
// ErrDocumentNotFound wrapped around Dispatch's own refusal, as ArtifactChanges answers it: a
// document deleted since its gate recorded it, whose review history the caller records unread at
// once rather than read again on its backoff.
func (c *HTTPClient) ApprovedVersionBefore(ctx context.Context, artifactID string, version int) (int, error) {
	var reviews []reviewResponse
	if err := c.request(ctx, http.MethodGet, "/api/v1/artifacts/"+url.PathEscape(artifactID)+"/reviews", nil, &reviews); err != nil {
		var refusal *Error
		if errors.As(err, &refusal) && artifactNotFound(refusal) {
			return 0, documentNotFound(err)
		}
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

// documentNotFound is Dispatch's 404 ARTIFACT_NOT_FOUND, err, as the reads of one document answer
// it: ErrDocumentNotFound wrapped around Dispatch's own refusal, so errors.Is finds it and the
// message keeps what Dispatch said.
func documentNotFound(err error) error {
	return fmt.Errorf("%w: %w", ErrDocumentNotFound, err)
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

// Whoami reads GET /api/v1/whoami: who Dispatch takes this client's credential for. The boot reads
// it under dispatch_token_broker, to refuse a broker token whose writes Dispatch would refuse.
func (c *HTTPClient) Whoami(ctx context.Context) (Caller, error) {
	var caller Caller
	if err := c.request(ctx, http.MethodGet, "/api/v1/whoami", nil, &caller); err != nil {
		return Caller{}, err
	}
	return caller, nil
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

// TransportRefusal reports whether err, anywhere in its chain, is a failure of Dispatch's transport
// that no wait fixes, never an outage: a certificate Go's client refuses to trust (a wrong or
// missing CA, a hostname mismatch, an expired or otherwise invalid certificate — crypto/tls wraps
// every one of these as *tls.CertificateVerificationError, never a bare x509 error, since
// httpClient sets no VerifyPeerCertificate or VerifyConnection of its own), a TLS alert, on the
// handshake or on a later record (tlsalert.In: a front door that demands a client certificate, say,
// or speaks no TLS version the client does; crypto/tls returns it as a *net.OpError), an https://
// dispatch_url pointed at a plain-HTTP port (http.ErrSchemeMismatch), or a TLS record the client
// cannot take (tls.RecordHeaderError, on the handshake or on a later record), most often from a
// port that answers in neither TLS nor HTTP/1.x, such as an SSH server or a server that answers
// the client hello as an HTTP/0.9 request (net/http turns the record error into
// http.ErrSchemeMismatch only when the answer starts with "HTTP/", net/http/client.go).
// dispatchBase's scheme check (internal/config) catches a typo in the scheme itself before boot,
// but not a correct https:// whose port does not serve TLS. transportFailure keeps each of these
// out of TransientError, Unreachable reads it first so it wins over an outage it is joined with,
// and the boot's catch-up of the roots that run or wait on a person (internal/daemon's
// catchUpWithRetries) stops the start on one rather than skip the root.
func TransportRefusal(err error) bool {
	var certErr *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	return errors.As(err, &certErr) || errors.As(err, &record) || tlsalert.In(err) ||
		errors.Is(err, http.ErrSchemeMismatch)
}

// transportFailure wraps err as a TransientError unless it is a refusal of the transport no wait
// fixes (TransportRefusal): TransientError says Dispatch gave no answer a later attempt may get,
// which no refusal of the transport is.
func transportFailure(err error) error {
	if TransportRefusal(err) {
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
	Priority     *int           `json:"priority"`
	Labels       []string       `json:"labels"`
	LastSeq      int64          `json:"last_seq"`
	ResolvedRepo *string        `json:"resolved_repo"`
	BlockedBy    []string       `json:"blocked_by"`
	Claim        *claimResponse `json:"claim"`
	Assignee     personMember   `json:"assignee"`
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
	Assignee          personMember   `json:"assignee"`
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
		Assignee:          r.Assignee.login,
		AssigneeKnown:     r.Assignee.known,
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
