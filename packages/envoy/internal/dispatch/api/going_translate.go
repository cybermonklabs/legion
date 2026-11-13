package api

// going_translate.go is what each row of the going catalogue's routes takes of a call (R3). A
// tool's arguments are the `dispatch` CLI's, and the CLI does more than fill a path and send the
// rest: it resolves a document named by slug to the id a route's path takes, a session named by
// title to its id, moves an argument a route reads under another name (`reply_to_ask` is a
// comment's `ask_id`, a suggestion's `replace_with` its `suggestion`), merges the links an update
// adds into the set the route replaces, and posts a closing reason before the close
// (packages/envoy-client/src/dispatch-commands). Each row's translator does that work for the
// route as it stands, and every read it makes goes through the caller's own Doer, so it reads
// only what the caller may read.
//
// What a translator cannot send it refuses by name (goingUntranslatable), before anything is
// written: a picture or a file the CLI reads off the machine it runs on, which the bar has no
// way to reach, and a `ref` the bar names by issue, project and document instead.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// goingCallInput is one call as its row's translation reads it: the arguments the action's
// schema accepted, the value of each placeholder of the row's path as chooseGoingRoute filled
// it, and the session the caller acts as ("" for none).
type goingCallInput struct {
	arguments map[string]any
	path      map[string]string
	session   string
}

// goingTranslator is one row's translation: what the row's route takes of a call, read against
// the route as it stands. Its reads go through read.
type goingTranslator func(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error)

// goingCall is what a translation makes of a call: the value of each placeholder of the row's
// path, the query, the body - JSON, a multipart upload, or none - and any write that must land
// before the row's own.
type goingCall struct {
	path  map[string]string
	query url.Values
	body  map[string]any
	form  *goingUpload
	first []goingSend
}

// goingUpload is a multipart body: its fields, and the one file part, named `file`.
type goingUpload struct {
	fields      map[string]string
	filename    string
	contentType string
	content     []byte
}

// goingSend is one request a call sends, whole.
type goingSend struct {
	method      string
	path        string
	query       url.Values
	contentType string
	body        []byte
}

// goingUntranslatable is a call a row's route cannot be sent, named by why: GoingCaller answers
// it as a GoingArgumentError, before any write.
type goingUntranslatable string

func (e goingUntranslatable) Error() string { return string(e) }

// The calls the bar can never send, each in one wording.
const (
	goingNoPictures = "images names picture files on the machine the dispatch CLI runs on, which the bar cannot read: post the words without them"
	goingNoFilePath = "path names a file on the machine the dispatch CLI runs on, which the bar cannot read: give the content itself"
	goingNoRef      = "ref is a dispatch:// reference the CLI reads its target from; the bar names the issue, project and document instead"
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

// goingIssuePath is an issue's own route, and goingIssuePath(key, "claim") one under it.
func goingIssuePath(key string, below ...string) string {
	return "/api/v1/issues/" + url.PathEscape(key) + goingBelow(below)
}

// goingAskPath, goingCommentPath and goingArtifactPath are the same for an ask, a comment and a
// document by id.
func goingAskPath(id string, below ...string) string {
	return "/api/v1/asks/" + url.PathEscape(id) + goingBelow(below)
}

func goingCommentPath(id string) string {
	return "/api/v1/comments/" + url.PathEscape(id)
}

func goingArtifactPath(id string, below ...string) string {
	return "/api/v1/artifacts/" + url.PathEscape(id) + goingBelow(below)
}

func goingBelow(segments []string) string {
	if len(segments) == 0 {
		return ""
	}
	return "/" + strings.Join(segments, "/")
}

// goingIsID reports whether reference is a whole id in a form uuid.Parse reads, rather than a
// name: a document's slug or filename, a session's title.
func goingIsID(reference string) bool {
	_, err := uuid.Parse(reference)
	return err == nil
}

// issueDocument is the id of the document of issue that reference names: the issue's spec for
// none or `spec`, the reference itself for a whole id, and otherwise the document the issue's
// slug route resolves the slug or filename to.
func (r goingReader) issueDocument(ctx context.Context, issue, reference string) (string, error) {
	if reference == "" || reference == "spec" {
		var read struct {
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		if err := r.get(ctx, goingIssuePath(issue), &read); err != nil {
			return "", err
		}
		if read.PrimaryArtifactID == "" {
			return "", goingUntranslatable(issue + " has no spec document")
		}
		return read.PrimaryArtifactID, nil
	}
	if goingIsID(reference) {
		return reference, nil
	}
	var read struct {
		ID string `json:"id"`
	}
	if err := r.get(ctx, goingIssuePath(issue, "artifacts", url.PathEscape(reference)), &read); err != nil {
		return "", err
	}
	return read.ID, nil
}

// projectDocument is the id of the project document reference names, by id or by the slug or
// filename the project's route resolves.
func (r goingReader) projectDocument(ctx context.Context, project, reference string) (string, error) {
	if goingIsID(reference) {
		return reference, nil
	}
	var read struct {
		ID string `json:"id"`
	}
	if err := r.get(ctx, "/api/v1/projects/"+url.PathEscape(project)+"/artifacts/"+url.PathEscape(reference), &read); err != nil {
		return "", err
	}
	return read.ID, nil
}

// session is the id of the live session name names: the session whose id it is, or the one
// session whose title it is, whatever its case, since a person names a session by the title they
// know it by. None is 404 GOING_SESSION_NOT_FOUND and more than one 409
// GOING_SESSION_AMBIGUOUS, each naming what was asked for. A whole id is taken as it is when the
// roster cannot be read, so a write naming a session exactly is refused by its own route rather
// than by the roster.
func (r goingReader) session(ctx context.Context, name string) (string, error) {
	var sessions []struct {
		SessionID string `json:"session_id"`
		Title     string `json:"title"`
	}
	if err := r.get(ctx, "/api/v1/agents", &sessions); err != nil {
		if goingIsID(name) {
			return name, nil
		}
		return "", err
	}
	wanted := strings.TrimSpace(name)
	var titled []string
	for _, session := range sessions {
		if session.SessionID == wanted {
			return wanted, nil
		}
		if strings.EqualFold(strings.TrimSpace(session.Title), wanted) {
			titled = append(titled, session.SessionID)
		}
	}
	switch len(titled) {
	case 1:
		return titled[0], nil
	case 0:
		return "", goingRefusal{Status: http.StatusNotFound, Code: codeGoingSessionNotFound,
			Message: fmt.Sprintf("no live session has the id or the title %q", wanted)}
	}
	slices.Sort(titled)
	return "", goingRefusal{Status: http.StatusConflict, Code: codeGoingSessionAmbiguous,
		Message: fmt.Sprintf("%d live sessions are titled %q (%s): name one by its id", len(titled), wanted, strings.Join(titled, ", "))}
}

// The refusals of a session the person named.
const (
	codeGoingSessionNotFound  = "GOING_SESSION_NOT_FOUND"
	codeGoingSessionAmbiguous = "GOING_SESSION_AMBIGUOUS"
)

// build is the request, against base.
func (r goingSend) build(ctx context.Context, base string) (*http.Request, error) {
	target := base + r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	request, err := http.NewRequestWithContext(ctx, r.method, target, body)
	if err != nil {
		return nil, err
	}
	if r.contentType != "" {
		request.Header.Set("Content-Type", r.contentType)
	}
	return request, nil
}

// goingJSONRequest is one JSON request.
func goingJSONRequest(method, path string, body any) (goingSend, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return goingSend{}, err
	}
	return goingSend{method: method, path: path, contentType: "application/json", body: encoded}, nil
}

// requests is every request the call sends: the writes that land first, then the row's own.
func (c goingCall) requests(route GoingRoute) ([]goingSend, error) {
	own := goingSend{method: route.Method, path: goingPath(route.Pattern, c.path), query: c.query}
	switch {
	case c.form != nil:
		body, contentType, err := c.form.encode()
		if err != nil {
			return nil, err
		}
		own.body, own.contentType = body, contentType
	case c.body != nil:
		encoded, err := json.Marshal(c.body)
		if err != nil {
			return nil, err
		}
		own.body, own.contentType = encoded, "application/json"
	}
	return append(slices.Clone(c.first), own), nil
}

// encode is the upload as a multipart/form-data body, and its content type.
func (u goingUpload) encode() ([]byte, string, error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for _, name := range slices.Sorted(maps.Keys(u.fields)) {
		if err := writer.WriteField(name, u.fields[name]); err != nil {
			return nil, "", err
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, u.filename))
	header.Set("Content-Type", u.contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(u.content); err != nil {
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buffer.Bytes(), writer.FormDataContentType(), nil
}

// goingPick is the arguments named, those the call carries.
func goingPick(arguments map[string]any, names ...string) map[string]any {
	picked := make(map[string]any, len(names))
	for _, name := range names {
		if value, named := arguments[name]; named {
			picked[name] = value
		}
	}
	return picked
}

// goingText is a text argument, "" where the call carries none.
func goingText(arguments map[string]any, name string) string {
	value, _ := arguments[name].(string)
	return value
}

// goingRefuseNamed refuses the call where it carries any of the arguments named, with why.
func goingRefuseNamed(arguments map[string]any, why string, names ...string) error {
	for _, name := range names {
		if _, named := arguments[name]; named {
			return goingUntranslatable(why)
		}
	}
	return nil
}

// goingReplyTarget is the item and the block a reply argument names: a reference to a block of a
// comment, an ask or a message splits into the item's id and the block's, a reference to the
// item names it, and any other value is the item's id as written, which the route checks.
func goingReplyTarget(value string) (id, block string) {
	if ref, parsed := text.ParseRef(value, ""); parsed {
		switch {
		case ref.Kind == "block" && (ref.Item == "comment" || ref.Item == "ask" || ref.Item == "message"):
			return ref.ID, ref.Block
		case ref.Kind == "comment" || ref.Kind == "ask" || ref.Kind == "message":
			return ref.ID, ""
		}
	}
	return value, ""
}

// goingAnchor is an anchor on the document id: the quote and its occurrence the call names.
func goingAnchor(id string, arguments map[string]any) map[string]any {
	anchor := goingPick(arguments, "quote", "occurrence")
	anchor["artifact"] = id
	return anchor
}

// goingDocument is the id of the document a call names in reference: a project's document when
// it names a project, and otherwise the reference as an id, which the route resolves (a prefix
// included).
func goingDocument(ctx context.Context, read goingReader, arguments map[string]any, reference string) (string, error) {
	if project := goingText(arguments, "project"); project != "" {
		return read.projectDocument(ctx, project, reference)
	}
	return reference, nil
}

// goingActorSession is the `actor` a route a session answers on reads: the session itself.
func goingActorSession(session string) map[string]any {
	return map[string]any{"kind": "session", "id": session}
}

// goingArgumentsAsBody is every argument as the body, for a row whose path takes none and whose
// route reads each argument under the tool's own name: the issue create and the project create.
func goingArgumentsAsBody(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	body := make(map[string]any, len(call.arguments))
	for name, value := range call.arguments {
		body[name] = value
	}
	return goingCall{path: call.path, body: body}, nil
}

// goingTranslateNoBody sends no body: a comment's resolve, a follow and its unfollow, and an
// architecture sync take everything from their path and the caller's credential. An
// architecture sync's `repo` is not sent: the row imports every source of the project, the named
// one among them.
func goingTranslateNoBody(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path}, nil
}

// goingTranslateClaim sends a claim, and its release, an empty JSON body: both routes decode one,
// and take the claimant from the caller's credential. `release` only picks the row.
func goingTranslateClaim(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, body: map[string]any{}}, nil
}

// goingTranslateIssueUpdate is `dispatch issue-update`'s translation: `parent` and `repo` given
// empty are cleared, `before` and `after` are the rank's neighbours, `external_links` adds the
// links named to the set the issue holds (the route replaces the set; an empty list clears it),
// and a `reason`, which the schema takes only beside a close, is posted on the issue as a message
// before the close, as `dispatch issue-update --reason` posts it (updateIssueInput has no reason
// field). An issue already closed is refused before its reason is posted, so no reason lands as
// a stray message.
func goingTranslateIssueUpdate(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	arguments, key := call.arguments, call.path["key"]
	body := goingPick(arguments, "status", "title", "labels", "priority", "assignee", "route", "blocked_by", "components")
	for _, name := range []string{"parent", "repo"} {
		if value, named := arguments[name]; named {
			if value == "" {
				value = nil
			}
			body[name] = value
		}
	}
	if rank := goingPick(arguments, "before", "after"); len(rank) > 0 {
		body["rank"] = rank
	}
	translated := goingCall{path: call.path, body: body}
	links, linking := arguments["external_links"].([]any)
	reason, reasoned := arguments["reason"].(string)
	if reasoned && arguments["status"] != "done" {
		return goingCall{}, goingUntranslatable("reason is the note a close carries, so it is taken only beside status done")
	}
	if linking && len(links) == 0 {
		body["external_links"] = []model.ExternalLink{}
	}
	if !reasoned && (!linking || len(links) == 0) {
		return translated, nil
	}
	var issue struct {
		ClosedAt      *string              `json:"closed_at"`
		ExternalLinks []model.ExternalLink `json:"external_links"`
	}
	if err := read.get(ctx, goingIssuePath(key), &issue); err != nil {
		return goingCall{}, err
	}
	if reasoned && issue.ClosedAt != nil {
		return goingCall{}, goingRefusal{Status: http.StatusConflict, Code: "ISSUE_CLOSED",
			Message: fmt.Sprintf("%s closed at %s, so the reason was not posted and the close was not sent", key, *issue.ClosedAt)}
	}
	if linking && len(links) > 0 {
		merged := slices.Clone(issue.ExternalLinks)
		for _, link := range links {
			address, _ := link.(string)
			if !slices.ContainsFunc(merged, func(held model.ExternalLink) bool { return held.URL == address }) {
				merged = append(merged, model.ExternalLink{URL: address})
			}
		}
		body["external_links"] = merged
	}
	if reasoned {
		message, err := goingJSONRequest(http.MethodPost, goingIssuePath(key, "messages"), map[string]any{"body": reason})
		if err != nil {
			return goingCall{}, err
		}
		translated.first = []goingSend{message}
	}
	return translated, nil
}

// goingAskBody is what both ask rows take of a call: the question, with a `ref` the question
// does not already hold written under it as `dispatch ask` writes it, its options, multiple and
// urgency.
func goingAskBody(arguments map[string]any) (map[string]any, error) {
	if err := goingRefuseNamed(arguments, goingNoPictures, "images"); err != nil {
		return nil, err
	}
	body := goingPick(arguments, "options", "multiple", "urgency")
	question := goingText(arguments, "question")
	if ref := goingText(arguments, "ref"); ref != "" && !strings.Contains(question, ref) {
		question += "\n\nRef: " + ref
	}
	body["question"] = question
	return body, nil
}

// goingTranslateIssueAsk opens the ask on the issue, anchored to the passage of the issue's
// document the anchor names, that document resolved from its slug.
func goingTranslateIssueAsk(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	body, err := goingAskBody(call.arguments)
	if err != nil {
		return goingCall{}, err
	}
	if anchor, anchored := call.arguments["anchor"].(map[string]any); anchored {
		id, err := read.issueDocument(ctx, call.path["key"], goingText(anchor, "artifact"))
		if err != nil {
			return goingCall{}, err
		}
		body["anchor"] = goingAnchor(id, anchor)
	}
	return goingCall{path: call.path, body: body}, nil
}

// goingTranslateDocumentAsk opens the ask on a document named by id, or by slug on the project
// the call names, its anchor on that same document.
func goingTranslateDocumentAsk(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	body, err := goingAskBody(call.arguments)
	if err != nil {
		return goingCall{}, err
	}
	id, err := goingDocument(ctx, read, call.arguments, call.path["id"])
	if err != nil {
		return goingCall{}, err
	}
	if anchor, anchored := call.arguments["anchor"].(map[string]any); anchored {
		body["anchor"] = goingAnchor(id, anchor)
	}
	return goingCall{path: map[string]string{"id": id}, body: body}, nil
}

// goingTranslateEditAsk sends the parts of the ask the call rewrites.
func goingTranslateEditAsk(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, body: goingPick(call.arguments, "question", "options", "multiple", "urgency")}, nil
}

// goingTranslateResolveAsk sends the resolution's kind and reason.
func goingTranslateResolveAsk(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, body: goingPick(call.arguments, "kind", "reason")}, nil
}

// goingCommentBody is what both comment rows take of a call besides an anchor: the body, the
// comment or block it replies to, the ask (`reply_to_ask`, the route's `ask_id`) or ask part it
// replies under, and the turn.
func goingCommentBody(arguments map[string]any) (map[string]any, error) {
	if err := goingRefuseNamed(arguments, goingNoPictures, "images"); err != nil {
		return nil, err
	}
	if err := goingRefuseNamed(arguments, goingNoRef, "ref"); err != nil {
		return nil, err
	}
	body := goingPick(arguments, "body", "turn")
	if replyTo := goingText(arguments, "reply_to"); replyTo != "" {
		id, block := goingReplyTarget(replyTo)
		body["reply_to"] = id
		if block != "" {
			body["reply_to_block"] = block
		}
	}
	if replyTo := goingText(arguments, "reply_to_ask"); replyTo != "" {
		id, block := goingReplyTarget(replyTo)
		body["ask_id"] = id
		if block != "" {
			body["reply_to_block"] = block
		}
	}
	return body, nil
}

// goingTranslateIssueComment posts on the issue, on a quote of one of its documents where the
// call names the document and the quote.
func goingTranslateIssueComment(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	body, err := goingCommentBody(call.arguments)
	if err != nil {
		return goingCall{}, err
	}
	if document := goingText(call.arguments, "artifact"); document != "" && goingText(call.arguments, "quote") != "" {
		id, err := read.issueDocument(ctx, call.path["key"], document)
		if err != nil {
			return goingCall{}, err
		}
		body["anchor"] = goingAnchor(id, call.arguments)
	}
	return goingCall{path: call.path, body: body}, nil
}

// goingTranslateDocumentComment posts on a document named by id, or by slug on the project the
// call names, on the quote the call names.
func goingTranslateDocumentComment(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	body, err := goingCommentBody(call.arguments)
	if err != nil {
		return goingCall{}, err
	}
	id, err := goingDocument(ctx, read, call.arguments, call.path["id"])
	if err != nil {
		return goingCall{}, err
	}
	if goingText(call.arguments, "quote") != "" {
		body["anchor"] = goingAnchor(id, call.arguments)
	}
	return goingCall{path: map[string]string{"id": id}, body: body}, nil
}

// goingSuggestion is a suggestion on the document id: its anchor, its replacement under the
// route's `suggestion`, and its optional words.
func goingSuggestion(id string, arguments map[string]any) (map[string]any, error) {
	if err := goingRefuseNamed(arguments, goingNoRef, "ref"); err != nil {
		return nil, err
	}
	body := goingPick(arguments, "body")
	body["anchor"] = goingAnchor(id, arguments)
	body["suggestion"] = map[string]any{"replace_with": arguments["replace_with"]}
	return body, nil
}

// goingTranslateIssueSuggest suggests on the issue's document the call names, the spec where it
// names none.
func goingTranslateIssueSuggest(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	id, err := read.issueDocument(ctx, call.path["key"], goingText(call.arguments, "artifact"))
	if err != nil {
		return goingCall{}, err
	}
	body, err := goingSuggestion(id, call.arguments)
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{path: call.path, body: body}, nil
}

// goingTranslateDocumentSuggest suggests on a document named by id, or by slug on the project the
// call names.
func goingTranslateDocumentSuggest(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	id, err := goingDocument(ctx, read, call.arguments, call.path["id"])
	if err != nil {
		return goingCall{}, err
	}
	body, err := goingSuggestion(id, call.arguments)
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{path: map[string]string{"id": id}, body: body}, nil
}

// goingTranslateIssueMessage posts on the issue's conversation, as a reply to the message or the
// block of one `in_reply_to` names.
func goingTranslateIssueMessage(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	if err := goingRefuseNamed(call.arguments, goingNoPictures, "images"); err != nil {
		return goingCall{}, err
	}
	body := goingPick(call.arguments, "body")
	if replyTo := goingText(call.arguments, "in_reply_to"); replyTo != "" {
		id, block := goingReplyTarget(replyTo)
		body["in_reply_to"] = id
		if block != "" {
			body["in_reply_to_block"] = block
		}
	}
	return goingCall{path: call.path, body: body}, nil
}

// goingTranslateMessageReply is a session's answer to a direct message, as `dispatch message`
// sends it: to the message's first delivery attempt, which is the one it was handed, naming the
// session as its actor, and asking to post a follow-up where that attempt is already answered.
func goingTranslateMessageReply(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	if err := goingRefuseNamed(call.arguments, goingNoPictures, "images"); err != nil {
		return goingCall{}, err
	}
	if call.session == "" {
		return goingCall{}, goingUntranslatable("a reply to a direct message is the session's own, and this caller acts as no session")
	}
	id, block := goingReplyTarget(call.path["id"])
	body := map[string]any{"body": call.arguments["body"], "attempt": 1, "actor": goingActorSession(call.session)}
	if block != "" {
		body["in_reply_to_block"] = block
	}
	return goingCall{path: map[string]string{"id": id}, query: url.Values{"follow_up": {"true"}}, body: body}, nil
}

// goingTranslateSendToAgent sends to the session the call names by id or by title.
func goingTranslateSendToAgent(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	session, err := read.session(ctx, call.path["session_id"])
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{
		path: map[string]string{"session_id": session},
		body: goingPick(call.arguments, "body", "delivery", "in_reply_to"),
	}, nil
}

// goingTranslateDocEdit sends the edit's operations, its precondition and its summary; the issue
// and project rows take the document's slug in their path as the call names it.
func goingTranslateDocEdit(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	if err := goingRefuseNamed(call.arguments, goingNoRef, "ref"); err != nil {
		return goingCall{}, err
	}
	return goingCall{path: call.path, body: goingPick(call.arguments, "ops", "precondition", "summary")}, nil
}

// goingTranslateRequestApproval asks for approval of the document the call names: on the issue it
// names, on its project, or by id.
func goingTranslateRequestApproval(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	reference := call.path["id"]
	var id string
	var err error
	if issue := goingText(call.arguments, "issue"); issue != "" {
		id, err = read.issueDocument(ctx, issue, reference)
	} else {
		id, err = goingDocument(ctx, read, call.arguments, reference)
	}
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{
		path: map[string]string{"id": id},
		body: goingPick(call.arguments, "summary", "include_verification"),
	}, nil
}

// goingTranslatePlanApprove records the plan review on the issue's document the call names.
func goingTranslatePlanApprove(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	id, err := read.issueDocument(ctx, goingText(call.arguments, "issue"), call.path["id"])
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{path: map[string]string{"id": id}, body: goingPick(call.arguments, "version")}, nil
}

// goingTranslateResultRetract sends the retraction's reason.
func goingTranslateResultRetract(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, body: goingPick(call.arguments, "reason")}, nil
}

// goingTranslateResultRecord sends the result's own fields.
func goingTranslateResultRecord(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, body: goingPick(call.arguments,
		"check", "role", "verdict", "commit", "surface", "run", "observed", "negative_control",
		"failed", "skipped", "excluded", "screenshots")}, nil
}

// goingUploadContent is the text an upload stores: the call's own content, since a path names a
// file the bar cannot read.
func goingUploadContent(arguments map[string]any) (string, error) {
	if err := goingRefuseNamed(arguments, goingNoFilePath, "path"); err != nil {
		return "", err
	}
	content, given := arguments["content"].(string)
	if !given {
		return "", goingUntranslatable("content is required: the bar uploads the text it was given")
	}
	return content, nil
}

// goingTranslateUpload uploads the content as a document of the issue or the project, as the
// JSON form of the route takes it. `force` is the CLI's own check of what the owner holds under
// the name, so it is not sent.
func goingTranslateUpload(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	content, err := goingUploadContent(call.arguments)
	if err != nil {
		return goingCall{}, err
	}
	body := goingPick(call.arguments, "name", "summary")
	body["content"] = content
	return goingCall{path: call.path, body: body}, nil
}

// goingTranslateAgentUpload uploads the content as a file of the session's conversation, which
// takes a multipart upload alone: the file part named as the call names it, typed by its
// extension, and plain text where the extension says nothing.
func goingTranslateAgentUpload(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	content, err := goingUploadContent(call.arguments)
	if err != nil {
		return goingCall{}, err
	}
	name := goingText(call.arguments, "name")
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	fields := map[string]string{"name": name}
	if summary := goingText(call.arguments, "summary"); summary != "" {
		fields["summary"] = summary
	}
	return goingCall{path: call.path, form: &goingUpload{
		fields: fields, filename: name, contentType: contentType, content: []byte(content),
	}}, nil
}

// goingTranslateHandoff records the handoff as the session the caller acts as, which the route
// takes from the body's actor.
func goingTranslateHandoff(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	if call.session == "" {
		return goingCall{}, goingUntranslatable("a handoff is a session's own, and this caller acts as no session")
	}
	return goingCall{path: call.path, body: map[string]any{
		"body": call.arguments["body"], "actor": goingActorSession(call.session),
	}}, nil
}
