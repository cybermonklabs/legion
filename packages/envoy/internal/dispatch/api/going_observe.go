package api

// going_observe.go reads what a command-bar write changes, for the write and for its undo: the
// value a write is about to change and the value it left (goingObserver), each read in-process as
// the caller through the caller's own Doer (goingReader), and the refusal a route answered one of
// those reads or writes with (goingRefusal). The writes route (going_writes.go) records what the
// observer read before and after a write; the undo (going_undo.go) reads the same values again to
// tell whether another writer changed them since.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingRefusal is a refusal of a request the going writes made in-process, or one of their own
// in the same shape: its status, its code and its words, and the answer exactly as the route
// wrote it where a route wrote one, which the writes route then answers whole.
type goingRefusal struct {
	Status  int
	Code    string
	Message string
	header  http.Header
	body    []byte
}

func (e goingRefusal) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

// outcome is the refusal as a write's or an undo's outcome records it.
func (e goingRefusal) outcome() goingWriteOutcome {
	return goingWriteOutcome{Status: e.Status, Code: e.Code, Message: e.Message}
}

// goingRefusalOf is the refusal response answered, read off its body.
func goingRefusalOf(response *http.Response, body []byte) goingRefusal {
	var parsed errorBody
	_ = json.Unmarshal(body, &parsed)
	return goingRefusal{
		Status: response.StatusCode, Code: parsed.Code, Message: parsed.Error,
		header: response.Header.Clone(), body: body,
	}
}

// goingRefusalHeaders are the headers of a refusal that say more than its body: the scope a
// credential lacks (RFC 6750) and when to come back.
var goingRefusalHeaders = []string{"WWW-Authenticate", "Retry-After"}

// write answers the refusal: the route's own answer, byte for byte, where a route wrote one, and
// the shared error shape for one of the going writes' own.
func (e goingRefusal) write(w http.ResponseWriter) {
	if e.body == nil {
		writeError(w, e.Code, e.Status, e.Message)
		return
	}
	for _, name := range goingRefusalHeaders {
		if value := e.header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.body)
}

// goingReader makes the reads a translation, a before or an after, or an undo needs, through the
// caller's own Doer: every read is the caller's own.
type goingReader struct {
	doer GoingDoer
	base string
}

// get reads path into into, nil reading nothing; any answer but 200 is the route's refusal.
func (r goingReader) get(ctx context.Context, path string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
	if err != nil {
		return err
	}
	response, err := r.doer.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if response.StatusCode != http.StatusOK {
		return goingRefusalOf(response, body)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// goingObservation is what a read before or after a write saw: the item, the fields of it the
// write changes, the value, the issue the item is or belongs to, where it is one, and for a send
// to an agent the message's last delivery.
type goingObservation struct {
	target   string
	fields   []string
	value    any
	issue    string
	delivery *goingDelivery
}

// goingFields is the one field a write changes as an observation lists it, none for "".
func goingFields(field string) []string {
	if field == "" {
		return nil
	}
	return []string{field}
}

// goingObserver reads the value a write is about to change and the value it left,
// through the caller's own Doer. An issue's place in its project's ranking is read from the
// store (rankPlace), since no route answers an issue's neighbours in rank order.
type goingObserver struct {
	read goingReader
	s    *server
}

// The field each write that names no field of its own changes.
var goingWriteFields = map[string]string{
	"dispatch_issue":             "issue",
	"dispatch_claim":             "claim",
	"dispatch_ask":               "ask",
	"dispatch_resolve_ask":       "state",
	"dispatch_resolve_comment":   "resolved",
	"dispatch_follow":            "following",
	"dispatch_comment":           "comment",
	"dispatch_suggest":           "suggestion",
	"dispatch_message":           "message",
	"dispatch_send_to_agent":     "message",
	"dispatch_doc_edit":          "content",
	"dispatch_request_approval":  "approval",
	"dispatch_plan_approve":      "plan_review",
	"dispatch_result":            "result",
	"dispatch_artifact":          "artifact",
	"dispatch_project":           "project",
	"dispatch_architecture_sync": "architecture",
	"dispatch_handoff_write":     "handoff",
}

// goingIssueFields are the issue's own fields each argument of an update writes, by argument.
var goingIssueFields = map[string]string{
	"status": "status", "title": "title", "labels": "labels", "priority": "priority",
	"assignee": "assignee", "route": "route", "parent": "parent", "blocked_by": "blocked_by",
	"components": "components", "repo": "repo", "external_links": "external_links",
	"before": "rank", "after": "rank",
}

// goingAskFields are the parts of an ask an edit writes.
var goingAskFields = []string{"question", "options", "multiple", "urgency"}

// observe reads what the prepared write changes: before it (answer nil) and after it, where
// answer is what the write's route answered and before what the read before saw. A write that
// changes a value of an item already there reads that value both times; a write that makes an
// item reads nothing before it and reads the made item off its answer, but for an upload, which
// reads before it the document its name writes the next version of, where there is one.
func (o goingObserver) observe(ctx context.Context, prepared goingPrepared, answer []byte, before goingObservation) (goingObservation, error) {
	path, arguments := prepared.call.path, prepared.arguments
	fields := goingFields(goingWriteFields[prepared.action])
	issue := goingText(arguments, "issue")
	switch prepared.action {
	case "dispatch_issue_update":
		return o.issueFields(ctx, path["key"], goingIssueFieldsOf(arguments))
	case "dispatch_claim":
		return o.claim(ctx, path["key"])
	case "dispatch_edit_ask":
		return o.askFields(ctx, path["id"], goingAskFieldsOf(arguments))
	case "dispatch_resolve_ask":
		return o.askState(ctx, path["id"])
	case "dispatch_resolve_comment":
		return o.commentResolved(ctx, path["id"])
	case "dispatch_follow":
		return o.following(ctx, path["id"], path["session_id"])
	case "dispatch_doc_edit":
		if before.target != "" {
			// The read after: an edit changes neither the document's id nor its issue, which the
			// read before read, so only its content is read again.
			observed := goingObservation{target: before.target, fields: before.fields, issue: before.issue}
			value, err := o.documentValue(ctx, before.target)
			if err != nil {
				return observed, err
			}
			observed.value = value
			return observed, nil
		}
		resolved, err := o.editedDocument(ctx, prepared)
		if err != nil {
			return goingObservation{fields: fields, issue: issue}, err
		}
		return o.document(ctx, resolved)
	}
	if answer == nil {
		if prepared.action == "dispatch_request_approval" {
			// A request renews an approval ask already open rather than opening one, so the
			// asks open before it are what tells the undo whether this write opened its ask.
			open, err := o.openApprovalAsks(ctx, path["id"])
			return goingObservation{target: path["id"], fields: fields, issue: issue,
				value: goingApprovalBefore{OpenAsks: open}}, err
		}
		if prepared.action == "dispatch_artifact" {
			// An upload under a name its owner already holds writes that document's next
			// version: the version it writes over is the value before it.
			return o.uploadedOver(ctx, prepared, fields, issue)
		}
		// A write that makes an item: nothing of it is there before it.
		return goingObservation{target: goingTargetOfPath(path), fields: fields, issue: issue}, nil
	}
	switch prepared.action {
	case "dispatch_issue":
		var created struct {
			Key    string `json:"key"`
			Title  string `json:"title"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(answer, &created); err != nil {
			return goingObservation{}, fmt.Errorf("read the issue made: %w", err)
		}
		return goingObservation{target: created.Key, fields: fields, issue: created.Key,
			value: map[string]any{"key": created.Key, "title": created.Title, "status": created.Status}}, nil
	case "dispatch_ask":
		var created struct {
			ID       string  `json:"id"`
			Question string  `json:"question"`
			State    string  `json:"state"`
			IssueKey *string `json:"issue_key"`
		}
		if err := json.Unmarshal(answer, &created); err != nil {
			return goingObservation{}, fmt.Errorf("read the ask made: %w", err)
		}
		return goingObservation{target: created.ID, fields: fields, issue: deref(created.IssueKey),
			value: map[string]any{"id": created.ID, "question": created.Question, "state": created.State}}, nil
	case "dispatch_request_approval":
		var requested struct {
			Ask *struct {
				ID string `json:"id"`
			} `json:"ask"`
			ArtifactID string `json:"artifact_id"`
			Version    int    `json:"version"`
		}
		if err := json.Unmarshal(answer, &requested); err != nil {
			return goingObservation{}, fmt.Errorf("read the approval request: %w", err)
		}
		value := map[string]any{"ask": nil, "artifact": requested.ArtifactID, "version": requested.Version}
		target := requested.ArtifactID
		if requested.Ask != nil {
			value["ask"], target = requested.Ask.ID, requested.Ask.ID
		}
		return goingObservation{target: target, fields: fields, issue: issue, value: value}, nil
	case "dispatch_send_to_agent":
		// The route answers 201 even when the delivery failed, so the line carries the message's
		// last delivery, its state and its error, for the bar to tell a failed send.
		var sent struct {
			ID         string          `json:"id"`
			Deliveries []goingDelivery `json:"deliveries"`
		}
		if err := json.Unmarshal(answer, &sent); err != nil {
			return goingObservation{}, fmt.Errorf("read the message sent: %w", err)
		}
		observed := goingObservation{target: sent.ID, fields: fields, issue: issue, value: map[string]any{"id": sent.ID}}
		if last := len(sent.Deliveries) - 1; last >= 0 {
			observed.delivery = &sent.Deliveries[last]
		}
		return observed, nil
	}
	made := goingMadeID(answer)
	if made == "" {
		made = before.target
	}
	return goingObservation{target: made, fields: fields, issue: issue, value: map[string]any{"id": made}}, nil
}

// goingTargetOfPath is the item a write's path names, before the write: the last placeholder
// value of its path that names one, "" for a write whose path names none.
func goingTargetOfPath(path map[string]string) string {
	for _, placeholder := range []string{"id", "session_id", "key"} {
		if value := path[placeholder]; value != "" {
			return value
		}
	}
	return ""
}

// goingMadeID is the id of the item a write's answer made: its own id, its key, or the id of the
// document or the result it carries.
func goingMadeID(answer []byte) string {
	var made struct {
		ID       string `json:"id"`
		Key      string `json:"key"`
		Artifact *struct {
			ID string `json:"id"`
		} `json:"artifact"`
		Result *struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if json.Unmarshal(answer, &made) != nil {
		return ""
	}
	switch {
	case made.ID != "":
		return made.ID
	case made.Key != "":
		return made.Key
	case made.Artifact != nil:
		return made.Artifact.ID
	case made.Result != nil:
		return made.Result.ID
	}
	return ""
}

// goingIssueFieldsOf is the issue's fields an update's arguments write, in order.
func goingIssueFieldsOf(arguments map[string]any) []string {
	var fields []string
	for argument := range arguments {
		if field, writes := goingIssueFields[argument]; writes && !slices.Contains(fields, field) {
			fields = append(fields, field)
		}
	}
	slices.Sort(fields)
	return fields
}

// goingAskFieldsOf is the parts of an ask an edit's arguments write.
func goingAskFieldsOf(arguments map[string]any) []string {
	var fields []string
	for _, field := range goingAskFields {
		if _, named := arguments[field]; named {
			fields = append(fields, field)
		}
	}
	return fields
}

// issueFields is the issue's fields named, as the issue's own read answers them: each one the
// value a write back sends, an issue's components as the mode, ids and reason an update takes.
func (o goingObserver) issueFields(ctx context.Context, key string, fields []string) (goingObservation, error) {
	var issue map[string]json.RawMessage
	if err := o.read.get(ctx, goingIssuePath(key), &issue); err != nil {
		return goingObservation{target: key, fields: fields, issue: key}, err
	}
	var canonical string
	_ = json.Unmarshal(issue["key"], &canonical)
	key = cmp.Or(canonical, key)
	values := make(map[string]any, len(fields))
	for _, field := range fields {
		if field == goingRank {
			place, err := o.rankPlace(ctx, key)
			if err != nil {
				return goingObservation{target: key, fields: fields, issue: key}, err
			}
			values[field] = place
			continue
		}
		values[field] = goingIssueFieldValue(issue, field)
	}
	return goingObservation{target: key, fields: fields, issue: key, value: values}, nil
}

// goingRankPlace is an issue's place in its project's ranking: its rank, and the issues just
// before it (After, the rank route's `after`) and just after it (Before, its `before`) among every
// issue of the project in rank order, the ordering the rank route places an issue in
// (rankForInput). A neighbour is nil where the issue is first or last.
type goingRankPlace struct {
	Rank   string  `json:"rank"`
	After  *string `json:"after"`
	Before *string `json:"before"`
}

// rankPlace reads the issue's place in its project's ranking.
func (o goingObserver) rankPlace(ctx context.Context, key string) (goingRankPlace, error) {
	var place goingRankPlace
	err := o.s.deps.Store.Pool.QueryRow(ctx, `
		select i.rank,
		  (select p.key from issues p where p.project_key = i.project_key and p.key <> i.key and p.rank < i.rank
		     order by p.rank desc limit 1),
		  (select n.key from issues n where n.project_key = i.project_key and n.key <> i.key and n.rank > i.rank
		     order by n.rank limit 1)
		from issues i where i.key = $1
	`, key).Scan(&place.Rank, &place.After, &place.Before)
	if err != nil {
		return goingRankPlace{}, fmt.Errorf("read %s's place in its project's ranking: %w", key, err)
	}
	return place, nil
}

// goingApprovalBefore is an approval request's value before it: the id of every approval ask open
// where the request could renew one rather than open its own (openApprovalAsks).
type goingApprovalBefore struct {
	OpenAsks []string `json:"open_asks"`
}

// openApprovalAsks is the id of every approval ask open that an approval request of the document
// could renew instead of opening one: each naming the document, and each of the document's issue,
// since a request renews one open over any document it names - an issue's Verification document
// beside its spec (requestArtifactApproval). It is read from the store, as rankPlace is, since no
// route lists a document's open approval asks.
func (o goingObserver) openApprovalAsks(ctx context.Context, artifact string) ([]string, error) {
	open := []string{}
	if err := o.s.deps.Store.Pool.QueryRow(ctx, `
		select coalesce(array_agg(a.id::text order by a.id), '{}')
		from asks a
		where a.kind = 'approval' and a.state = 'open'
		  and (`+docs.ApprovalDocumentsJSONPath+` ? $1
		       or a.issue_key = (select d.issue_key from artifacts d where d.id = $2::uuid))
	`, artifact, artifact).Scan(&open); err != nil {
		return nil, fmt.Errorf("read the approval asks open on %s: %w", artifact, err)
	}
	return open, nil
}

// goingUploadBefore is an upload's value before it where its name finds a document its owner
// already holds: that document, and the version the upload writes its next version over.
type goingUploadBefore struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// uploadedOver is what an upload finds under its name before it: the document of the owner its
// row reaches whose name or slug the name is, as the upload's own lookup finds it
// (findArtifactByNameOrSlug), at its latest version, and no value where the owner holds none, so
// the upload makes one. It is read from the store, as rankPlace is, since no route answers a
// document by the name an upload gives it.
func (o goingObserver) uploadedOver(ctx context.Context, prepared goingPrepared, fields []string, issue string) (goingObservation, error) {
	observed := goingObservation{target: goingTargetOfPath(prepared.call.path), fields: fields, issue: issue}
	owner, known := goingUploadOwner(prepared.arguments, prepared.call.path["session_id"])
	if !known {
		return observed, nil
	}
	name := strings.TrimSpace(goingText(prepared.arguments, "name"))
	artifact, err := o.s.findArtifactByNameOrSlug(ctx, o.s.deps.Store.Pool, owner, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return observed, nil
	}
	if err != nil {
		return observed, fmt.Errorf("read the document an upload named %q writes over: %w", name, err)
	}
	before := goingUploadBefore{ID: artifact.ID, Name: artifact.Name}
	for _, version := range artifact.Versions {
		before.Version = max(before.Version, version.Number)
	}
	observed.target, observed.value = artifact.ID, before
	return observed, nil
}

// goingIssueFieldValue is one field of an issue's read as an update writes it.
func goingIssueFieldValue(issue map[string]json.RawMessage, field string) any {
	raw := issue[field]
	if field == "components" {
		var components model.IssueComponents
		if json.Unmarshal(raw, &components) == nil {
			ids := components.IDs
			if ids == nil {
				ids = []string{}
			}
			return map[string]any{"mode": components.Mode, "ids": ids, "reason": components.Reason}
		}
	}
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return value
}

// claim is who holds the issue's claim, nil for nobody.
func (o goingObserver) claim(ctx context.Context, key string) (goingObservation, error) {
	holder, canonical, err := o.claimHolder(ctx, key)
	observed := goingObservation{target: cmp.Or(canonical, key), fields: goingFields("claim"), issue: cmp.Or(canonical, key)}
	if err != nil {
		return observed, err
	}
	if holder != nil {
		observed.value = holder
	}
	return observed, nil
}

// goingHolder is a claim's holder as a write's record keeps it.
type goingHolder struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// claimHolder is the holder of the issue's claim, nil for nobody, and the issue's own key.
func (o goingObserver) claimHolder(ctx context.Context, key string) (*goingHolder, string, error) {
	var issue struct {
		Key   string `json:"key"`
		Claim *struct {
			Actor goingHolder `json:"actor"`
		} `json:"claim"`
	}
	if err := o.read.get(ctx, goingIssuePath(key), &issue); err != nil {
		return nil, "", err
	}
	if issue.Claim == nil {
		return nil, issue.Key, nil
	}
	return &issue.Claim.Actor, issue.Key, nil
}

// goingAskRead is the part of an ask's read the writes and their undos read.
type goingAskRead struct {
	Ask struct {
		ID       string            `json:"id"`
		IssueKey *string           `json:"issue_key"`
		Question string            `json:"question"`
		Options  []model.AskOption `json:"options"`
		Multiple bool              `json:"multiple"`
		Urgency  string            `json:"urgency"`
		State    string            `json:"state"`
	} `json:"ask"`
}

func (o goingObserver) readAsk(ctx context.Context, id string) (goingAskRead, error) {
	var read goingAskRead
	err := o.read.get(ctx, goingAskPath(id), &read)
	return read, err
}

// askFields is the parts of the ask named, each as an edit writes it: an option as its label and
// description, which an edit takes.
func (o goingObserver) askFields(ctx context.Context, id string, fields []string) (goingObservation, error) {
	read, err := o.readAsk(ctx, id)
	observed := goingObservation{target: cmp.Or(read.Ask.ID, id), fields: fields, issue: deref(read.Ask.IssueKey)}
	if err != nil {
		return observed, err
	}
	values := make(map[string]any, len(fields))
	for _, field := range fields {
		switch field {
		case "question":
			values[field] = read.Ask.Question
		case "options":
			options := make([]map[string]any, 0, len(read.Ask.Options))
			for _, option := range read.Ask.Options {
				written := map[string]any{"label": option.Label}
				if option.Description != "" {
					written["description"] = option.Description
				}
				options = append(options, written)
			}
			values[field] = options
		case "multiple":
			values[field] = read.Ask.Multiple
		case "urgency":
			values[field] = read.Ask.Urgency
		}
	}
	observed.value = values
	return observed, nil
}

// askState is the ask's state: open, answered or resolved.
func (o goingObserver) askState(ctx context.Context, id string) (goingObservation, error) {
	read, err := o.readAsk(ctx, id)
	observed := goingObservation{target: cmp.Or(read.Ask.ID, id), fields: goingFields("state"), issue: deref(read.Ask.IssueKey)}
	if err != nil {
		return observed, err
	}
	observed.value = read.Ask.State
	return observed, nil
}

// commentResolved is whether the comment's thread is resolved.
func (o goingObserver) commentResolved(ctx context.Context, id string) (goingObservation, error) {
	var read struct {
		Comment struct {
			ID       string  `json:"id"`
			IssueKey *string `json:"issue_key"`
			Resolved bool    `json:"resolved"`
		} `json:"comment"`
	}
	err := o.read.get(ctx, goingCommentPath(id), &read)
	observed := goingObservation{target: cmp.Or(read.Comment.ID, id), fields: goingFields("resolved"), issue: deref(read.Comment.IssueKey)}
	if err != nil {
		return observed, err
	}
	observed.value = read.Comment.Resolved
	return observed, nil
}

// goingFollowing is a follow's value: the session and whether it follows the ask.
type goingFollowing struct {
	Session   string `json:"session"`
	Following bool   `json:"following"`
}

// following is whether session follows the ask.
func (o goingObserver) following(ctx context.Context, ask, session string) (goingObservation, error) {
	following, err := o.follows(ctx, ask, session)
	observed := goingObservation{target: ask, fields: goingFields("following")}
	if err != nil {
		return observed, err
	}
	observed.value = goingFollowing{Session: session, Following: following}
	return observed, nil
}

// follows reports whether session follows the ask.
func (o goingObserver) follows(ctx context.Context, ask, session string) (bool, error) {
	var read struct {
		Followers []struct {
			SessionID string `json:"session_id"`
		} `json:"followers"`
	}
	if err := o.read.get(ctx, goingAskPath(ask, "followers"), &read); err != nil {
		return false, err
	}
	for _, follower := range read.Followers {
		if follower.SessionID == session {
			return true, nil
		}
	}
	return false, nil
}

// editedDocument is the id of the document an edit writes, resolved from the slug the issue and
// project rows take.
func (o goingObserver) editedDocument(ctx context.Context, prepared goingPrepared) (string, error) {
	path := prepared.call.path
	switch {
	case path["id"] != "":
		return path["id"], nil
	case goingText(prepared.arguments, "issue") != "" && path["key"] != "" && prepared.route.Pattern == "/api/v1/issues/{key}/artifacts/{slug}/edits":
		return o.read.issueDocument(ctx, path["key"], path["slug"])
	default:
		return o.read.projectDocument(ctx, path["key"], path["slug"])
	}
}

// goingDocumentRead is the part of a document's read the writes and their undos read.
type goingDocumentRead struct {
	ID       string  `json:"id"`
	IssueKey *string `json:"issue_key"`
	Project  string  `json:"project"`
	Name     string  `json:"name"`
}

// goingDocumentValue is a document edit's value: the content, and the version it is.
type goingDocumentValue struct {
	Version *int   `json:"version"`
	Content string `json:"content"`
}

// document is the document's content and version, read by id.
func (o goingObserver) document(ctx context.Context, id string) (goingObservation, error) {
	var artifact goingDocumentRead
	if err := o.read.get(ctx, goingArtifactPath(id), &artifact); err != nil {
		return goingObservation{target: id, fields: goingFields("content")}, err
	}
	observed := goingObservation{target: artifact.ID, fields: goingFields("content"), issue: deref(artifact.IssueKey)}
	value, err := o.documentValue(ctx, artifact.ID)
	if err != nil {
		return observed, err
	}
	observed.value = value
	return observed, nil
}

func (o goingObserver) documentValue(ctx context.Context, id string) (goingDocumentValue, error) {
	var text struct {
		Markdown string `json:"markdown"`
		Version  *int   `json:"version"`
	}
	if err := o.read.get(ctx, goingArtifactPath(id, "text"), &text); err != nil {
		return goingDocumentValue{}, err
	}
	return goingDocumentValue{Version: text.Version, Content: text.Markdown}, nil
}

// goingJSONOrNull is value as JSON, the literal null for none.
func goingJSONOrNull(value any) json.RawMessage {
	if value == nil {
		return json.RawMessage("null")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return encoded
}
