package api

// going_observe.go reads what a command-bar write changes, for the write and for its undo: the
// value a write is about to change and the value it left (goingObserver), each read in-process as
// the caller through the caller's own Doer (goingReader), and the refusal a route answered one of
// those reads or writes with (goingRefusal). A plan shows the value before a write and holds its
// row to it (going_plan.go); the undo (going_undo.go) reads what a landed write found and left off
// what its route answered of its own transaction, recorded beside its landing, and compares what
// is there now with what the write left, in the statement of its own write back.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// goingRefusal is a refusal of a request the going writes made in-process, or one of their own
// in the same shape: its status, its code and its words, and the answer exactly as the route
// wrote it where a route wrote one, which a route of the bar's own then answers whole (write).
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
// the shared error shape for one of the going writes' own; either carries the headers of the
// route's answer that say more than its body.
func (e goingRefusal) write(w http.ResponseWriter) {
	for _, name := range goingRefusalHeaders {
		if value := e.header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if e.body == nil {
		writeError(w, e.Code, e.Status, e.Message)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.body)
}

// goingReader makes the reads a translation, a before or an after, or an undo needs, through the
// caller's own Doer: every read is the caller's own. knownSession is the listener's own lookup of
// one session, running or not (server.goingKnownSession), which a send's recipient is read by;
// nil for a call to another server's table, whose reads go through its routes alone.
type goingReader struct {
	doer         GoingDoer
	base         string
	knownSession func(ctx context.Context, id string) (bool, error)
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
// write changes, the value, the issue the item is or belongs to, where it is one, for a send to
// an agent the message's last delivery, after a claim, a release or a create whether its route
// answered that it changed nothing (unchanged), and where the write's route answered what stood
// before it in its own transaction, that value (stood), which the undo reads over the read
// before: another writer may move the item between that read and the write.
type goingObservation struct {
	target    string
	fields    []string
	value     any
	issue     string
	delivery  *goingDelivery
	unchanged bool
	stood     json.RawMessage
	// held is the document an upload's read before found under the upload's name, with its
	// versions (uploadedOver).
	held *model.Artifact
	// token is a document's token as the read that saw its value answered it, which a document
	// edit's route answers too, for the document it found and the one it left (editArtifactResponse).
	token string
	// beforeUnread is a write whose route answered it found a value other than the one the read
	// before saw: another writer changed the item between the two, and the route answers no value
	// of what it replaced, so its undo puts nothing back.
	beforeUnread bool
	// note is what the write's line says of a write its route answered changed nothing, where the
	// line has nothing else to say: a create that named an issue Dispatch already holds.
	note string
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
// store (rankPlace), since no route answers an issue's neighbours in rank order. landed reports
// whether the write's last request committed a change in its route's own transaction, as that
// request's landing records it: a follow answers 204 whether or not it changed anything.
type goingObserver struct {
	read   goingReader
	s      *server
	landed func(context.Context) (bool, error)
}

// The field each write that names no field of its own changes.
var goingWriteFields = map[string]string{
	"dispatch_issue":             "issue",
	"dispatch_claim":             "claim",
	"dispatch_ask":               "ask",
	"dispatch_resolve_ask":       "state",
	"dispatch_resolve_comment":   "resolved",
	"dispatch_reopen_comment":    "resolved",
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

// goingIssueFields are the issue's own fields each argument of an update writes, by argument. The
// advances relation is one of them: a write replaces the issue's own set, so its value before and
// after is the own priority ids and the revision the read answered (goingAdvancesValue,
// goingIssueFieldValue), which the undo writes back against as a compare-and-set.
var goingIssueFields = map[string]string{
	"status": "status", "title": "title", "labels": "labels", "priority": "priority",
	"assignee": "assignee", "route": "route", "parent": "parent", "blocked_by": "blocked_by",
	"components": "components", "repo": "repo", "external_links": "external_links",
	"advances": goingAdvances, "before": "rank", "after": "rank",
}

// goingAdvances is the field an issue's `advances` relation is recorded under, and goingAdvancesValue
// its value as a write back reads it: the priority ids the issue's own relations state, and the
// revision the read answered. A write of `advances` carries the whole own set and no inherited
// entry, so the undo writes the own ids back (updateIssueInput.Advances) and carries the revision
// the write left as the route's own compare-and-set (advances_revision, refuseAdvancesRevision), so
// a second writer's change since the write refuses the write back rather than overwriting it.
const goingAdvances = "advances"

type goingAdvancesValue struct {
	IDs      []string `json:"ids"`
	Revision int64    `json:"revision"`
}

// goingAskFields are the parts of an ask an edit writes.
var goingAskFields = []string{"question", "options", "multiple", "urgency"}

// observe reads what the prepared write changes: before it (answer nil) and after it, where
// answer is what the write's route answered and before what the read before saw, by the write
// tool's own reader (goingWriteTool.Observe). A write that changes a value of an item already
// there reads that value before it, for a refusal's line, and takes from its route's answer the
// value its transaction found and the one it left (stood and value): an issue's fields, an ask's
// parts, a thread's resolution, a claim, a follow's change (goingObserver.landed) and, for a
// document edit, the tokens that say whether the reads on either side of the edit read the
// document it found and the one it left. A write that makes an item reads nothing before it and
// reads the made item off its answer (made), but for an upload, which reads before it the
// document its name writes the next version of, where there is one, and takes the version it
// wrote over from its answer. status is the status the route answered, 0 before.
func (o goingObserver) observe(ctx context.Context, prepared goingPrepared, answer []byte, status int, before goingObservation) (goingObservation, error) {
	read := goingWriteTools[prepared.action].Observe
	if read == nil {
		read = goingObserver.made
	}
	return read(o, ctx, prepared, answer, status, before)
}

// goingObservedOf is what every observation of a write names beside its value: the field its
// tool changes (goingWriteFields) and the issue the call names.
func goingObservedOf(prepared goingPrepared) (fields []string, issue string) {
	return goingFields(goingWriteFields[prepared.action]), goingText(prepared.arguments, "issue")
}

// made reads a write that makes an item: nothing of it is there before it, and after it the
// made item's id, as its route answered it, else the item the read before named.
func (o goingObserver) made(_ context.Context, prepared goingPrepared, answer []byte, _ int, before goingObservation) (goingObservation, error) {
	fields, issue := goingObservedOf(prepared)
	if answer == nil {
		return goingObservation{target: goingTargetOfPath(prepared.call.path), fields: fields, issue: issue}, nil
	}
	made := goingMadeID(answer)
	if made == "" {
		made = before.target
	}
	return goingObservation{target: made, fields: fields, issue: issue, value: map[string]any{"id": made}}, nil
}

// issueUpdated reads the issue's fields the update writes before it, and after it what the
// update's route answered it found and left.
func (o goingObserver) issueUpdated(ctx context.Context, prepared goingPrepared, answer []byte, _ int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		return o.issueFields(ctx, prepared.call.path["key"], goingIssueFieldsOf(prepared.arguments))
	}
	return goingIssueUpdated(answer, before)
}

// claimed reads the issue's claim before a claim or a release, and after it what the claim
// route answered.
func (o goingObserver) claimed(ctx context.Context, prepared goingPrepared, answer []byte, _ int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		return o.claim(ctx, prepared.call.path["key"])
	}
	// The claim route answers, from its own transaction, whether it changed the claim, the
	// claim it left and the one it found (claimAnswer), which no read on either side of it can
	// say: the claim may move between them and it. An answer that does not say whether it
	// changed the claim is the value after unread, so the undo puts nothing back.
	var claimed struct {
		ClaimChanged *bool                        `json:"claim_changed"`
		Claim        *struct{ Actor goingHolder } `json:"claim"`
		Previous     *struct{ Actor goingHolder } `json:"previous_claim"`
	}
	observed := goingObservation{target: before.target, fields: before.fields, issue: before.issue, stood: json.RawMessage("null")}
	if err := json.Unmarshal(answer, &claimed); err != nil {
		return observed, fmt.Errorf("read the claim's answer: %w", err)
	}
	if claimed.ClaimChanged == nil {
		return observed, errors.New("the claim's answer does not say whether the claim changed")
	}
	observed.unchanged = !*claimed.ClaimChanged
	if claimed.Claim != nil {
		observed.value = &claimed.Claim.Actor
	}
	if claimed.Previous != nil {
		observed.stood = goingJSONOrNull(&claimed.Previous.Actor)
	}
	return observed, nil
}

// askEdited reads the ask's parts the edit writes before it, and after it what the edit's route
// answered.
func (o goingObserver) askEdited(ctx context.Context, prepared goingPrepared, answer []byte, _ int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		return o.askFields(ctx, prepared.call.path["id"], goingAskFieldsOf(prepared.arguments))
	}
	return goingAskEdited(answer, before)
}

// askResolved reads the ask's state on either side of its resolution.
func (o goingObserver) askResolved(ctx context.Context, prepared goingPrepared, _ []byte, _ int, _ goingObservation) (goingObservation, error) {
	return o.askState(ctx, prepared.call.path["id"])
}

// commentResolvedBy reads the thread's resolution before it is resolved or reopened, and after it
// what the resolve or the reopen route answered: each answers the resolution it left and the one
// it found (resolvedComment).
func (o goingObserver) commentResolvedBy(ctx context.Context, prepared goingPrepared, answer []byte, _ int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		return o.commentResolved(ctx, prepared.call.path["id"])
	}
	return goingCommentResolved(answer, before)
}

// followedBy reads whether the session follows the ask before a follow or an unfollow, and after
// it whether the write changed that (goingObserver.landed).
func (o goingObserver) followedBy(ctx context.Context, prepared goingPrepared, answer []byte, _ int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		path := prepared.call.path
		return o.following(ctx, path["id"], path["session_id"])
	}
	return o.followed(ctx, prepared, before)
}

// documentEditedBy reads the document an edit names before it, and after it the tokens and the
// content the edit's route answered.
func (o goingObserver) documentEditedBy(ctx context.Context, prepared goingPrepared, answer []byte, _ int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		resolved, err := o.editedDocument(ctx, prepared)
		if err != nil {
			fields, issue := goingObservedOf(prepared)
			return goingObservation{fields: fields, issue: issue}, err
		}
		return o.document(ctx, resolved)
	}
	return o.documentEdited(ctx, answer, before)
}

// issueCreated reads nothing before a create, and after it the issue made.
func (o goingObserver) issueCreated(ctx context.Context, prepared goingPrepared, answer []byte, status int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		return o.made(ctx, prepared, answer, status, before)
	}
	fields, _ := goingObservedOf(prepared)
	var created struct {
		Key    string `json:"key"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(answer, &created); err != nil {
		return goingObservation{}, fmt.Errorf("read the issue made: %w", err)
	}
	observed := goingObservation{target: created.Key, fields: fields, issue: created.Key,
		value: map[string]any{"key": created.Key, "title": created.Title, "status": created.Status}}
	// The create route answers 201 for an issue it made and 200 for one Dispatch already holds
	// for the external reference the create names (respondWithExistingExternalIssue), the
	// create's own verdict: that issue is no issue of the turn's, so the write changed nothing
	// and its undo closes nothing (goingUndoer.closeCreated).
	if status != http.StatusCreated {
		observed.unchanged = true
		observed.note = fmt.Sprintf("Dispatch already holds %s for the external issue this create named, so nothing was created", created.Key)
	}
	return observed, nil
}

// askOpened reads nothing before an ask is opened, and after it the ask made.
func (o goingObserver) askOpened(ctx context.Context, prepared goingPrepared, answer []byte, status int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		return o.made(ctx, prepared, answer, status, before)
	}
	fields, _ := goingObservedOf(prepared)
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
}

// approvalRequested reads nothing before an approval request: whether it opened its ask is what
// its route answers (approvalRequestResponse.Opened), not a read before it, which another request
// may overtake. After it, the request's ask, document and version.
func (o goingObserver) approvalRequested(ctx context.Context, prepared goingPrepared, answer []byte, status int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		return o.made(ctx, prepared, answer, status, before)
	}
	fields, issue := goingObservedOf(prepared)
	var requested struct {
		Ask *struct {
			ID string `json:"id"`
		} `json:"ask"`
		Opened     bool   `json:"opened"`
		ArtifactID string `json:"artifact_id"`
		Version    int    `json:"version"`
	}
	if err := json.Unmarshal(answer, &requested); err != nil {
		return goingObservation{}, fmt.Errorf("read the approval request: %w", err)
	}
	value := map[string]any{"ask": nil, "opened": requested.Opened, "artifact": requested.ArtifactID, "version": requested.Version}
	target := requested.ArtifactID
	if requested.Ask != nil {
		value["ask"], target = requested.Ask.ID, requested.Ask.ID
	}
	return goingObservation{target: target, fields: fields, issue: issue, value: value}, nil
}

// sentToAgent reads nothing before a send to a session, and after it the message sent with its
// last delivery.
func (o goingObserver) sentToAgent(ctx context.Context, prepared goingPrepared, answer []byte, status int, before goingObservation) (goingObservation, error) {
	if answer == nil {
		return o.made(ctx, prepared, answer, status, before)
	}
	fields, issue := goingObservedOf(prepared)
	// The route answers 201 even when the delivery failed, and when the listener queued it
	// for a session that is not running, so the line carries the message's last delivery, its
	// state and its error, for the bar to tell a failed or a queued send from one that
	// reached the session.
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

// uploaded reads, before an upload, the document its name writes the next version of, where its
// owner holds one, and after it the version the upload wrote over.
func (o goingObserver) uploaded(ctx context.Context, prepared goingPrepared, answer []byte, _ int, _ goingObservation) (goingObservation, error) {
	fields, issue := goingObservedOf(prepared)
	if answer == nil {
		// An upload under a name its owner already holds writes that document's next
		// version: the version it writes over is the value before it.
		return o.uploadedOver(ctx, prepared, fields, issue)
	}
	// The version the upload wrote its next version over is what its own transaction found
	// under its owner's lock (artifactUploadResponse.PreviousVersion), which another upload
	// may have written between the read before and the upload; none where it made the
	// document.
	var upload struct {
		Artifact struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"artifact"`
		PreviousVersion *int `json:"previous_version"`
	}
	if err := json.Unmarshal(answer, &upload); err != nil {
		return goingObservation{}, fmt.Errorf("read the upload's answer: %w", err)
	}
	observed := goingObservation{target: upload.Artifact.ID, fields: fields, issue: issue,
		value: map[string]any{"id": upload.Artifact.ID}, stood: json.RawMessage("null")}
	if upload.PreviousVersion != nil {
		observed.stood = goingJSONOrNull(goingUploadBefore{ID: upload.Artifact.ID, Name: upload.Artifact.Name, Version: *upload.PreviousVersion})
	}
	return observed, nil
}

// goingRecordedCall is the call of action that sent request, as much as its landing names: the
// action's row whose method is the request's and whose pattern its path fills, and the value of
// each placeholder of the path (goingPathValues), so what a landed call left is read off its
// landing as observe reads a call's, with nothing of the call translated again (goingUndoer.recorded).
func goingRecordedCall(catalogue GoingCatalogue, action string, request goingWriteRequest) (goingPrepared, bool) {
	entry, held := catalogue.Lookup(action)
	if !held {
		return goingPrepared{}, false
	}
	for _, route := range entry.Routes {
		if route.Method != request.Method {
			continue
		}
		if values, fills := goingPathValues(route.Pattern, request.Path); fills {
			return goingPrepared{action: action, route: route, call: goingCall{path: values}}, true
		}
	}
	return goingPrepared{}, false
}

// goingPathValues is the value of each placeholder of pattern in path, unescaped as goingPath
// escaped it, and whether path is pattern filled.
func goingPathValues(pattern, path string) (map[string]string, bool) {
	patterns, segments := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(patterns) != len(segments) {
		return nil, false
	}
	values := map[string]string{}
	for index, part := range patterns {
		name, placeholder := strings.CutPrefix(part, "{")
		if !placeholder {
			if part != segments[index] {
				return nil, false
			}
			continue
		}
		value, err := url.PathUnescape(segments[index])
		if err != nil || value == "" {
			return nil, false
		}
		values[strings.TrimSuffix(name, "}")] = value
	}
	return values, true
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
		value, err := goingIssueFieldValue(issue, field)
		if err != nil {
			return goingObservation{target: key, fields: fields, issue: key}, fmt.Errorf("read %s's %s: %w", key, field, err)
		}
		values[field] = value
	}
	return goingObservation{target: key, fields: fields, issue: key, value: values}, nil
}

// goingIssueUpdated is what an update of the issue's fields found and left, read off its route's
// answer (issueUpdated): each field before.fields names as the update left it (value), and as the
// update found it under the issue's lock (stood), the rank as the places the update answers. An
// answer that does not say what the update found of a field it writes, or says what it found or
// left in a value that does not decode (goingIssueFieldValue), is the value after unread, so the
// undo puts nothing back.
func goingIssueUpdated(answer []byte, before goingObservation) (goingObservation, error) {
	observed := goingObservation{target: before.target, fields: before.fields, issue: before.issue}
	var issue map[string]json.RawMessage
	if err := json.Unmarshal(answer, &issue); err != nil {
		return observed, fmt.Errorf("read the update's answer: %w", err)
	}
	var previous map[string]json.RawMessage
	if err := json.Unmarshal(issue["previous"], &previous); err != nil || previous == nil {
		return observed, errors.New("the update's answer does not say what it found of the fields it wrote")
	}
	var key string
	if json.Unmarshal(issue["key"], &key) == nil && key != "" {
		observed.target, observed.issue = key, key
	}
	left, found := make(map[string]any, len(before.fields)), make(map[string]any, len(before.fields))
	for _, field := range before.fields {
		if _, answered := previous[field]; !answered {
			return observed, fmt.Errorf("the update's answer does not say what it found of %s", field)
		}
		if field == goingRank {
			var from, to issueRankPlace
			if json.Unmarshal(previous[field], &from) != nil || json.Unmarshal(issue["rank_place"], &to) != nil || to.Rank == "" {
				return observed, errors.New("the update's answer does not say the places it moved the issue between")
			}
			left[field], found[field] = to, from
			continue
		}
		value, err := goingIssueFieldValue(issue, field)
		if err != nil {
			return observed, fmt.Errorf("the update's answer does not say what it left of %s: %w", field, err)
		}
		stood, err := goingIssueFieldValue(previous, field)
		if err != nil {
			return observed, fmt.Errorf("the update's answer does not say what it found of %s: %w", field, err)
		}
		left[field], found[field] = value, stood
	}
	observed.value, observed.stood = left, goingJSONOrNull(found)
	return observed, nil
}

// rankPlace reads the issue's place in its project's ranking (issueRankPlace) from the store, as
// the update itself reads it in its own transaction.
func (o goingObserver) rankPlace(ctx context.Context, key string) (issueRankPlace, error) {
	return readIssueRankPlace(ctx, o.s.deps.Store.Pool, key)
}

// goingUploadBefore is an upload's value before it where its name finds a document its owner
// already holds: that document, the version the upload writes its next version over, and the
// document's text now (Content), which a plan row shows beside the text the upload writes and an
// approval is held to (goingRowShown.same): text another writer changed that no version holds yet
// is a change too. Content is the document's whole markdown, as a doc edit row's value carries it,
// so it is bounded by what a document holds (pmdoc.MaxDocumentBytes, 1 MiB); it is absent for a
// file, and for a document whose text renders as no markdown (DOC_SCHEMA).
type goingUploadBefore struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Version int     `json:"version"`
	Content *string `json:"content,omitempty"`
}

// uploadedOver is what an upload finds under its name before it: the document of the owner its
// route writes to (goingRouteUploadOwner) whose name or slug the name is, as the upload's own
// lookup finds it (findArtifactByNameOrSlug), at its latest version, with its text and the token
// of that text (goingObservation.token), and no value where the owner holds none, so the upload
// makes one. The document is found from the store, as rankPlace is, since no route answers a
// document by the name an upload gives it, and its text is read as the caller. A route whose owner
// it cannot name reads nothing. A file has no text and no token, and neither has a document whose
// tree is outside the Proof schema (409 DOC_SCHEMA), which only an upload naming no token repairs:
// an upload over one names none (AgentListener.rowUpload).
func (o goingObserver) uploadedOver(ctx context.Context, prepared goingPrepared, fields []string, issue string) (goingObservation, error) {
	observed := goingObservation{target: goingTargetOfPath(prepared.call.path), fields: fields, issue: issue}
	owner, known := goingRouteUploadOwner(prepared.route, prepared.call.path)
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
	if artifact.Kind == "doc" {
		text, token, err := o.documentValue(ctx, artifact.ID)
		var refused goingRefusal
		switch {
		case errors.As(err, &refused) && refused.Code == codeDocSchema:
		case err != nil:
			return observed, err
		default:
			before.Content, observed.token = &text.Content, token
		}
	}
	observed.target, observed.value, observed.held = artifact.ID, before, &artifact
	return observed, nil
}

// goingIssueFieldValue is one field of an issue's read as an update writes it. The advances
// relation is the issue's own priority ids and its revision (goingAdvancesValue): a write replaces
// the own set, so inherited entries are no part of its value and the revision is what the undo's
// write back carries. A value that does not decode, an advances relation with no revision beside
// it among them, is an error, never an empty value: recorded as the value a write found or left,
// an empty set at revision 0 would be what an undo writes back over the relation.
func goingIssueFieldValue(issue map[string]json.RawMessage, field string) (any, error) {
	raw := issue[field]
	if field == goingAdvances {
		var entries []model.IssueAdvance
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("decode advances: %w", err)
		}
		var revision *int64
		if err := json.Unmarshal(issue["advances_revision"], &revision); err != nil {
			return nil, fmt.Errorf("decode advances_revision: %w", err)
		}
		if revision == nil {
			return nil, errors.New("advances_revision is null")
		}
		return goingAdvancesValue{IDs: ownAdvances(entries), Revision: *revision}, nil
	}
	if field == "components" {
		var components model.IssueComponents
		if json.Unmarshal(raw, &components) == nil {
			ids := components.IDs
			if ids == nil {
				ids = []string{}
			}
			return map[string]any{"mode": components.Mode, "ids": ids, "reason": components.Reason}, nil
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode %s: %w", field, err)
	}
	return value, nil
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

// askFields is the parts of the ask named, each as an edit writes it (askFieldValue).
func (o goingObserver) askFields(ctx context.Context, id string, fields []string) (goingObservation, error) {
	read, err := o.readAsk(ctx, id)
	observed := goingObservation{target: cmp.Or(read.Ask.ID, id), fields: fields, issue: deref(read.Ask.IssueKey)}
	if err != nil {
		return observed, err
	}
	values := make(map[string]any, len(fields))
	for _, field := range fields {
		values[field] = askFieldValue(model.Ask{Question: read.Ask.Question, Options: read.Ask.Options,
			Multiple: read.Ask.Multiple, Urgency: read.Ask.Urgency}, field)
	}
	observed.value = values
	return observed, nil
}

// goingAskEdited is what an ask's edit found and left, read off its route's answer (askEdited):
// each part before.fields names as the edit left it (value) and as the edit found it under the
// ask's lock (stood), each as an edit writes it (askFieldValue). An answer that does not say what
// the edit found of a part it writes is the value after unread, so the undo puts nothing back.
func goingAskEdited(answer []byte, before goingObservation) (goingObservation, error) {
	observed := goingObservation{target: before.target, fields: before.fields, issue: before.issue}
	var edited struct {
		Question string                     `json:"question"`
		Options  []model.AskOption          `json:"options"`
		Multiple bool                       `json:"multiple"`
		Urgency  string                     `json:"urgency"`
		Previous map[string]json.RawMessage `json:"previous"`
	}
	if err := json.Unmarshal(answer, &edited); err != nil {
		return observed, fmt.Errorf("read the edit's answer: %w", err)
	}
	ask := model.Ask{Question: edited.Question, Options: edited.Options, Multiple: edited.Multiple, Urgency: edited.Urgency}
	left, found := make(map[string]any, len(before.fields)), make(map[string]any, len(before.fields))
	for _, field := range before.fields {
		previous, answered := edited.Previous[field]
		if !answered {
			return observed, fmt.Errorf("the edit's answer does not say what it found of the ask's %s", field)
		}
		left[field], found[field] = askFieldValue(ask, field), previous
	}
	observed.value, observed.stood = left, goingJSONOrNull(found)
	return observed, nil
}

// askFieldValue is one part of an ask as an edit writes it: an option as its label and
// description, which an edit takes, and nil for a part no edit writes.
func askFieldValue(ask model.Ask, field string) any {
	switch field {
	case "question":
		return ask.Question
	case "options":
		options := make([]map[string]any, 0, len(ask.Options))
		for _, option := range ask.Options {
			written := map[string]any{"label": option.Label}
			if option.Description != "" {
				written["description"] = option.Description
			}
			options = append(options, written)
		}
		return options
	case "multiple":
		return ask.Multiple
	case "urgency":
		return ask.Urgency
	}
	return nil
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

// commentResolved is the comment's thread's resolution (goingCommentResolution): whether it is
// resolved, by whom and when.
func (o goingObserver) commentResolved(ctx context.Context, id string) (goingObservation, error) {
	var read struct {
		Comment struct {
			ID       string  `json:"id"`
			IssueKey *string `json:"issue_key"`
			goingCommentResolution
		} `json:"comment"`
	}
	err := o.read.get(ctx, goingCommentPath(id), &read)
	observed := goingObservation{target: cmp.Or(read.Comment.ID, id), fields: goingFields("resolved"), issue: deref(read.Comment.IssueKey)}
	if err != nil {
		return observed, err
	}
	resolution := read.Comment.goingCommentResolution
	if len(resolution.ResolvedBy) == 0 {
		resolution.ResolvedBy = json.RawMessage("null")
	}
	observed.value = resolution
	return observed, nil
}

// goingCommentResolved is what a resolve or a reopen found and left of the thread, read off its
// route's answer (resolvedComment): the resolution it left (value) and the one it found under the
// comment's lock (stood). An answer that does not say what the write found is the value after
// unread, so the undo writes nothing back.
func goingCommentResolved(answer []byte, before goingObservation) (goingObservation, error) {
	observed := goingObservation{target: before.target, fields: before.fields, issue: before.issue}
	var resolved struct {
		goingCommentResolution
		Previous *goingCommentResolution `json:"previous"`
	}
	if err := json.Unmarshal(answer, &resolved); err != nil {
		return observed, fmt.Errorf("read the thread's answer: %w", err)
	}
	if resolved.Previous == nil {
		return observed, errors.New("the thread's answer does not say how the write found it")
	}
	left, found := resolved.goingCommentResolution, *resolved.Previous
	for _, resolution := range []*goingCommentResolution{&left, &found} {
		if len(resolution.ResolvedBy) == 0 {
			resolution.ResolvedBy = json.RawMessage("null")
		}
	}
	observed.value, observed.stood = left, goingJSONOrNull(found)
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

// followed is what a follow or an unfollow left, and what it found. Its route answers a follow
// 204 whether or not the session followed the ask already, and an unfollow 404 where it did not,
// so what a follow found is whether its own transaction committed a change, as its request's
// landing records it in that transaction (goingObserver.landed).
func (o goingObserver) followed(ctx context.Context, prepared goingPrepared, before goingObservation) (goingObservation, error) {
	session := prepared.call.path["session_id"]
	follows := prepared.route.Method == http.MethodPut
	observed := goingObservation{target: before.target, fields: before.fields, issue: before.issue,
		value: goingFollowing{Session: session, Following: follows}}
	changed := true
	if follows {
		if o.landed == nil {
			return observed, errors.New("whether the follow changed anything is unknown")
		}
		landed, err := o.landed(ctx)
		if err != nil {
			return observed, err
		}
		changed = landed
	}
	observed.stood = goingJSONOrNull(goingFollowing{Session: session, Following: follows != changed})
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

// document is the document's content and version, read by id, and the token of what the read
// read (goingObservation.token).
func (o goingObserver) document(ctx context.Context, id string) (goingObservation, error) {
	var artifact goingDocumentRead
	if err := o.read.get(ctx, goingArtifactPath(id), &artifact); err != nil {
		return goingObservation{target: id, fields: goingFields("content")}, err
	}
	observed := goingObservation{target: artifact.ID, fields: goingFields("content"), issue: deref(artifact.IssueKey)}
	value, token, err := o.documentValue(ctx, artifact.ID)
	if err != nil {
		return observed, err
	}
	observed.value, observed.token = value, token
	return observed, nil
}

// documentValue is the document's content and version, and the token the text read answers of
// that very content (artifactText).
func (o goingObserver) documentValue(ctx context.Context, id string) (goingDocumentValue, string, error) {
	var text struct {
		Markdown string `json:"markdown"`
		Token    string `json:"token"`
		Version  *int   `json:"version"`
	}
	if err := o.read.get(ctx, goingArtifactPath(id, "text"), &text); err != nil {
		return goingDocumentValue{}, "", err
	}
	return goingDocumentValue{Version: text.Version, Content: text.Markdown}, text.Token, nil
}

// documentEdited is what a document edit left, read after it and held to its route's answer, which
// names the token of the document the edit found and of the one it left, read in the edit's own
// transaction (editArtifactResponse). A read after whose token is not the one the edit left read
// another writer's document: the value after is unread, so the undo writes nothing back. A read
// before whose token is not the one the edit found read a document another writer changed before
// the edit, so its content is not what the edit replaced, which nothing answers: the write is
// marked so (beforeUnread), and its undo writes nothing back either.
func (o goingObserver) documentEdited(ctx context.Context, answer []byte, before goingObservation) (goingObservation, error) {
	observed := goingObservation{target: before.target, fields: before.fields, issue: before.issue}
	var edited struct {
		Token         string `json:"token"`
		PreviousToken string `json:"previous_token"`
	}
	if err := json.Unmarshal(answer, &edited); err != nil {
		return observed, fmt.Errorf("read the edit's answer: %w", err)
	}
	if edited.Token == "" || edited.PreviousToken == "" {
		return observed, errors.New("the edit's answer does not name the document it found and the one it left")
	}
	observed.beforeUnread = edited.PreviousToken != before.token
	value, token, err := o.documentValue(ctx, before.target)
	if err != nil {
		return observed, err
	}
	if token != edited.Token {
		return observed, goingRefusal{Status: http.StatusConflict, Code: "DOCUMENT_CHANGED",
			Message: "another writer changed the document after this edit, before what the edit left was read"}
	}
	observed.value, observed.token = value, token
	return observed, nil
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
