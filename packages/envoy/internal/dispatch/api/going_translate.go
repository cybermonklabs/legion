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
// A read tool's rows are translated the same way: a search's words are its route's `q`, and a read
// whose command picks what it reads from what its arguments name - dispatch_read and
// dispatch_doc_read, an item named by its dispatch:// reference above all - picks its row as that
// command does (goingPickRead, goingPickDocRead).
//
// What the bar can never send - a picture or a file the CLI reads off the machine it runs on, which
// the bar has no way to reach, a `ref` where the bar places a write by issue, project and document
// instead, and the rest goingUnsendable names - is decided by goingUnsendable, which needs no read:
// GoingCaller refuses such a call before its row's translation. What a translator cannot send for
// a reason of its own it refuses by name (goingUntranslatable), before anything is written.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// goingCallInput is one call as its row's translation reads it: the arguments the action's
// schema accepted, the value of each placeholder of the row's path as chooseGoingRoute filled
// it, the session the caller acts as ("" for none), and resumed (GoingCaller.resumeAfter), which
// says what of the requests the call would send first an earlier write of the invocation landed,
// those the call resumes after: a translation's own refusal words what was not sent by it.
type goingCallInput struct {
	arguments map[string]any
	path      map[string]string
	session   string
	resumed   func(specs []goingSend) []goingWriteRequest
}

// resumes is what of specs, the requests the call sends first, an earlier write of its invocation
// landed, which the call resumes after; none for a call that resumes after nothing.
func (c goingCallInput) resumes(specs ...goingSend) []goingWriteRequest {
	if c.resumed == nil {
		return nil
	}
	return c.resumed(specs)
}

// goingTranslator is one row's translation: what the row's route takes of a call, read against
// the route as it stands. Its reads go through read.
type goingTranslator func(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error)

// goingCall is what a translation makes of a call: the value of each placeholder of the row's
// path, the query, the body - JSON, a multipart upload, or none - any write that must land before
// the row's own, and the compare-and-set the row's own request carries to its route on its
// context (going_conditions.go), nil for none.
type goingCall struct {
	path      map[string]string
	query     url.Values
	body      map[string]any
	form      *goingUpload
	first     []goingSend
	condition *goingCondition
}

// goingUpload is a multipart body: its fields, and the one file part, named `file`.
type goingUpload struct {
	fields      map[string]string
	filename    string
	contentType string
	content     []byte
}

// goingSend is one request a call sends, whole. stable is what its digest is taken of where the
// body itself differs between two sends of the same request - a multipart upload, whose boundary
// is drawn at random - nil where the body is its own (digest). condition is the compare-and-set
// the request carries to its route on its context (build), nil for none. what is what a request a
// call sends before its own writes, in the words a row tells its person where the call failed in
// the server after it landed (goingPanickedText); it is no part of what the request sends.
type goingSend struct {
	method      string
	path        string
	query       url.Values
	contentType string
	body        []byte
	stable      []byte
	condition   *goingCondition
	what        string
}

// digest is the hex SHA-256 of what the request sends, its condition included, which a landing
// records (goingWriteRequest.BodySHA256) so a later attempt resumes only after the very request it
// sends: a close's reason of other words is another request, and so is an upload that is a create
// alone beside one that is not.
func (s goingSend) digest() string {
	content := s.body
	if s.stable != nil {
		content = s.stable
	}
	hash := sha256.New()
	hash.Write(content)
	if s.condition != nil {
		// A request with no condition digests as its body alone, as every request did before
		// conditions left the body.
		condition, _ := json.Marshal(s.condition)
		hash.Write([]byte{0})
		hash.Write(condition)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// goingUntranslatable is a call a row's route cannot be sent, named by why: GoingCaller answers
// it as a GoingArgumentError, before any write.
type goingUntranslatable string

func (e goingUntranslatable) Error() string { return string(e) }

// goingUnsendable is why the bar can never send a call of action with these arguments, "" where
// it may: a picture file or a file to upload, which the `dispatch` command reads off the machine
// it runs on and the bar has no way to open; a `ref`, the dispatch:// reference the command finds
// a comment's, a suggestion's or an edit's place by, where the bar places a write by the issue,
// project and document the call names; an upload with no text; a closing `reason` on an update
// that does not close the issue, which the update posts only before a close; and a sync's source
// repository not written as owner/name, the two segments its route's path takes. An ask's `ref`
// is none of these: the question carries it as a line of its own (goingAskBody). None needs a
// read, so GoingCaller refuses such a call before its row's translation (prepare), with words no
// translator repeats. Each is worded for the person reading the bar's panel - what the bar cannot
// send, and what that person can do there - and names no tool, no argument, no command line and
// no flag, none of which that person holds. A picture list, a link or a file path written empty
// names no picture, link or file, so it counts as absent, as null does (which the action's schema
// refuses first): no translator sends any of the three on, and the call is sent as it would be
// without it.
func goingUnsendable(action string, arguments map[string]any) string {
	named := func(argument string) bool {
		switch value := arguments[argument].(type) {
		case nil:
			return false
		case string:
			return value != ""
		case []any:
			return len(value) > 0
		}
		return true
	}
	switch action {
	case "dispatch_comment":
		if named("images") {
			return goingNoPictures("a comment")
		}
		if named("ref") {
			return goingNoLink("a comment")
		}
	case "dispatch_ask":
		if named("images") {
			return goingNoPictures("a question")
		}
	case "dispatch_message":
		if named("images") {
			return goingNoPictures("a message")
		}
	case "dispatch_suggest":
		if named("ref") {
			return goingNoLink("a suggested change")
		}
	case "dispatch_doc_edit":
		if named("ref") {
			return goingNoLink("an edit of a document")
		}
	case "dispatch_artifact":
		if named("path") {
			return "the bar cannot upload a file, since the file is one the answer named and the bar has no way to open it. Ask again with the text to upload written in your question."
		}
		if _, text := arguments["content"].(string); !text {
			return "the bar cannot send an upload with no text, since it uploads only the text the answer writes. Ask again with the text to upload written in your question."
		}
	case "dispatch_issue_update":
		if _, noted := arguments["reason"].(string); noted && arguments["status"] != "done" {
			return "the bar cannot send an update with a closing note that does not close the issue, since the note is posted only as the issue is marked done. Ask again to close the issue, or for the update without the note."
		}
	case "dispatch_architecture_sync":
		// A sync naming no repository syncs every source of the project (goingTranslateNoBody).
		if repository := goingText(arguments, "repo"); repository != "" {
			if _, _, whole := goingSourceRepository(repository); !whole {
				return "the bar cannot sync an architecture source whose repository is not written whole as owner/name. Ask again naming the repository that way, as in acme/widgets."
			}
		}
	}
	return ""
}

// goingNoPictures is why the bar cannot post write - a comment, a question, a message - with the
// picture files the call names (goingUnsendable).
func goingNoPictures(write string) string {
	return fmt.Sprintf("the bar cannot post %s with a picture, since the picture is a file the answer named and the bar has no way to open it. Ask again for the words alone.", write)
}

// goingNoLink is why the bar cannot send write where the call names its place by a dispatch://
// reference (goingUnsendable).
func goingNoLink(write string) string {
	return fmt.Sprintf("the bar cannot send %s whose place is given by a link, since the bar finds where a write goes only by the issue, project or document the answer names. Ask again naming the issue or the document it is for.", write)
}

// goingSourceRepository splits a source repository written as owner/name on its slash, the way
// the server reads a repository elsewhere (strings.Cut), into the two segments the one-source
// sync route's path takes; whole is false for any other value.
func goingSourceRepository(repository string) (owner, name string, whole bool) {
	owner, name, found := strings.Cut(repository, "/")
	return owner, name, found && owner != "" && name != "" && !strings.Contains(name, "/")
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
// than by the roster. It is the session a write acts as (the @session fill), which must be
// running to act; a send's recipient, which need not be, is read by recipient.
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

// recipient is the id of the session a send to an agent goes to, name naming it by its id or its
// title. A session the roster lists resolves as session resolves it. One the roster does not list
// - it lists running sessions alone - is read by its id as the send's own route reads its target
// (resolveDeliveryTarget): through the listener's registry and its known-session records
// (goingReader.knownSession). So a session the listener knows and that is not running, one that
// stopped since the bar offered it, reaches the route, which stores the message for it and records
// the attempt queued, and so does every other session the route knows, which the route then
// sends to or refuses in its own words. A name the listener knows no session by keeps session's
// refusal and nothing is written: it may be a title, and the listener finds no session by its
// title. A listener that could not be asked is that refusal instead (goingKnownSession), never
// a session that does not exist. A reader with no knownSession, a call to another server's table,
// reads the roster alone.
func (r goingReader) recipient(ctx context.Context, name string) (string, error) {
	resolved, err := r.session(ctx, name)
	var refused goingRefusal
	if err == nil || r.knownSession == nil || errors.As(err, &refused) && refused.Code == codeGoingSessionAmbiguous {
		return resolved, err
	}
	wanted := strings.TrimSpace(name)
	if wanted == "" {
		return "", err
	}
	known, lookup := r.knownSession(ctx, wanted)
	if lookup != nil {
		return "", lookup
	}
	if !known {
		return "", err
	}
	return wanted, nil
}

// goingKnownSession reports whether the Envoy listener knows id as a session, running or not, by
// the lookup the send route makes of its target (resolveDeliveryTarget): its registry entry, or
// its known-session record (SessionIncludingStopped). A listener that knows no such session, or
// none configured, answers false; one that could not be asked is refused 503 ENVOY_UNAVAILABLE, as
// the agents route answers it (writeEnvoyError).
func (s *server) goingKnownSession(ctx context.Context, id string) (bool, error) {
	if s.deps.Envoy == nil {
		return false, nil
	}
	if _, err := s.deps.Envoy.SessionIncludingStopped(ctx, id); err != nil {
		if errors.Is(err, dispatchenvoy.ErrNotFound) {
			return false, nil
		}
		return false, goingRefusal{Status: http.StatusServiceUnavailable, Code: "ENVOY_UNAVAILABLE", Message: err.Error()}
	}
	return true, nil
}

// The refusals of a session the person named.
const (
	codeGoingSessionNotFound  = "GOING_SESSION_NOT_FOUND"
	codeGoingSessionAmbiguous = "GOING_SESSION_AMBIGUOUS"
)

// build is the request, against base. Its condition rides on its context
// (withGoingCondition), which reaches a route only through the server's own table called
// in-process: a request against another server's base would arrive unconditioned, so one that
// carries a condition is refused for it rather than built.
func (r goingSend) build(ctx context.Context, base string) (*http.Request, error) {
	if r.condition != nil {
		if base != "" {
			return nil, fmt.Errorf("%s %s carries a condition, which reaches its route only in-process, and not against %s", r.method, r.path, base)
		}
		ctx = withGoingCondition(ctx, *r.condition)
	}
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

// requests is every request the call sends: the writes that land first, then the row's own, which
// carries the call's condition.
func (c goingCall) requests(route GoingRoute) ([]goingSend, error) {
	own := goingSend{method: route.Method, path: goingPath(route.Pattern, c.path), query: c.query, condition: c.condition}
	switch {
	case c.form != nil:
		body, contentType, err := c.form.encode()
		if err != nil {
			return nil, err
		}
		// Its fields, file name, type and bytes, which every encoding of it carries alike.
		stable, err := json.Marshal(struct {
			Fields      map[string]string `json:"fields"`
			Filename    string            `json:"filename"`
			ContentType string            `json:"content_type"`
			Content     []byte            `json:"content"`
		}{c.form.fields, c.form.filename, c.form.contentType, c.form.content})
		if err != nil {
			return nil, err
		}
		own.body, own.contentType, own.stable = body, contentType, stable
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
// architecture sync of every source take everything from their path and the caller's credential.
// This is the every-source sync row, chosen when the call names no `repo`; a sync that names one
// takes the one-source row (goingTranslateArchitectureSyncRepo).
func goingTranslateNoBody(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path}, nil
}

// goingTranslateRoster sends read_roster, which takes no argument, as GET /api/v1/agents of the
// goingReadSessionMax sessions seen last, the most the answer run's own roster read lists
// (going_reads.go), so the route reads the open asks and activity of those alone and loads no more
// rows of a roster of any size than it answers.
func goingTranslateRoster(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, query: url.Values{"limit": {strconv.Itoa(goingReadSessionMax)}}}, nil
}

// goingTranslateSearch sends dispatch_search's words as the search route's q, and its project,
// its limit and its offset under their own names, as `dispatch search` sends them.
func goingTranslateSearch(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	query := url.Values{}
	for argument, key := range map[string]string{"query": "q", "project": "project", "limit": "limit", "offset": "offset"} {
		value, named := call.arguments[argument]
		if !named || value == nil {
			continue
		}
		text, isText := goingArgumentText(value)
		if !isText {
			return goingCall{}, goingUntranslatable(fmt.Sprintf("%s is sent in the query, which takes no %T", argument, value))
		}
		query.Set(key, text)
	}
	return goingCall{path: call.path, query: query}, nil
}

// goingTranslateIssueList sends dispatch_issues' filters in the query, as goingTranslateRead sends
// a read's arguments, a null priority as the route's `none`, which `dispatch issues` sends for an
// issue with no priority.
func goingTranslateIssueList(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	if priorities, listed := call.arguments["priority"].([]any); listed {
		named := make([]any, len(priorities))
		for index, priority := range priorities {
			named[index] = priority
			if priority == nil {
				named[index] = "none"
			}
		}
		call.arguments = maps.Clone(call.arguments)
		call.arguments["priority"] = named
	}
	// The listing's path takes no argument, so its row is its method alone.
	return goingTranslateRead(GoingRoute{Method: http.MethodGet})(ctx, read, call)
}

// goingTranslateHandoffVersion reads the version of a phase's handoff that the call names, which
// Dispatch reads by the handoff's id alone, as `dispatch handoff-read` reads it: the issue's
// handoffs name the id of each version, and the row reads the handoff whose version filled its
// {id} (chooseGoingRoute).
func goingTranslateHandoffVersion(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	issue, phase, version := goingText(call.arguments, "issue"), goingText(call.arguments, "phase"), call.path["id"]
	if issue == "" || phase == "" {
		return goingCall{}, goingUntranslatable("version names a handoff of the issue and the phase the call names")
	}
	var handoffs []model.HandoffSummary
	if err := read.get(ctx, goingIssuePath(issue, "handoffs"), &handoffs); err != nil {
		return goingCall{}, err
	}
	for _, handoff := range handoffs {
		if handoff.Phase == phase && strconv.Itoa(handoff.Version) == version {
			return goingCall{path: map[string]string{"id": handoff.ID}}, nil
		}
	}
	return goingCall{}, goingUntranslatable(fmt.Sprintf("%s has no %s handoff of version %s", issue, phase, version))
}

// goingReadRef is what a read's `ref` names, as the `dispatch` command reads one
// (parseDispatchRef, packages/envoy-client/src/dispatch-owner.ts): the reference text.ParseRef
// reads, a dashboard URL under the server's own address included; and, after an issue's key, one
// of the three reads of the issue only a read names (of): its log, `log` or the Activity tab's
// `activity`, its children, and its comments. version is the version an `@v<N>` after a
// document's slug names, "" for none.
type goingReadRef struct {
	text.Ref
	of      string
	version string
}

// The reads of an issue only a read's reference names (goingReadRef.of).
const (
	goingRefLog      = "log"
	goingRefChildren = "children"
	goingRefComments = "comments"
)

// goingRefVersion is the `@v<N>` that ends a reference to a document at version N.
var goingRefVersion = regexp.MustCompile(`/artifact/[^/#@]+@v([1-9][0-9]*)$`)

// goingIssueReadEvents is how many of an issue's newest events its log lists where the read names
// no count, as `dispatch read` lists them (DEFAULT_ISSUE_READ_EVENTS, packages/contracts).
const goingIssueReadEvents = 10

// goingParseReadRef reads value as a read's reference (goingReadRef), false where it names nothing
// a read reads.
func goingParseReadRef(value, serverURL string) (goingReadRef, bool) {
	if ref, parsed := text.ParseRef(value, serverURL); parsed {
		read := goingReadRef{Ref: ref}
		if ref.Kind == "artifact" {
			if version := goingRefVersion.FindStringSubmatch(value); version != nil {
				read.version = version[1]
			}
		}
		return read, true
	}
	rest, scheme := strings.CutPrefix(value, "dispatch://")
	key, of, below := strings.Cut(rest, "/")
	if !scheme || !below {
		return goingReadRef{}, false
	}
	switch of {
	case "log", "activity":
		of = goingRefLog
	case "children":
		of = goingRefChildren
	case "comments":
		of = goingRefComments
	default:
		return goingReadRef{}, false
	}
	ref, parsed := text.ParseRef("dispatch://"+key, "")
	if !parsed || ref.Kind != "issue" {
		return goingReadRef{}, false
	}
	return goingReadRef{Ref: ref, of: of}, true
}

// goingPickRead is the row of dispatch_read a call reads, and what that row's route takes of the
// call, as `dispatch read` picks what it reads (packages/envoy-client/src/dispatch-commands/read.ts),
// each item through the route a person reads it with: the item a reference names
// (goingPickReadRef); else a message, on its issue where the call names one; an issue's or a
// project's document; an issue (goingIssueRead); and a project's documents on no issue.
func goingPickRead(arguments map[string]any, serverURL string) (string, goingCall, error) {
	if value := strings.TrimSpace(goingText(arguments, "ref")); value != "" {
		return goingPickReadRef(value, arguments, serverURL)
	}
	issue, project, artifact, message := goingText(arguments, "issue"), goingText(arguments, "project"),
		goingText(arguments, "artifact"), goingText(arguments, "message")
	switch {
	case message != "" && issue != "":
		return "GET /api/v1/issues/{key}/messages/{id}", goingCall{path: map[string]string{"key": issue, "id": message}}, nil
	case message != "":
		return "GET /api/v1/messages/{id}", goingCall{path: map[string]string{"id": message}}, nil
	case issue != "" && artifact != "":
		return "GET /api/v1/issues/{key}/artifacts/{slug}", goingCall{path: map[string]string{"key": issue, "slug": artifact}}, nil
	case project != "" && artifact != "":
		return "GET /api/v1/projects/{key}/artifacts/{slug}", goingCall{path: map[string]string{"key": project, "slug": artifact}}, nil
	case issue != "":
		return goingIssueRead(issue, arguments)
	case project != "":
		return "GET /api/v1/projects/{key}/artifacts", goingCall{path: map[string]string{"key": project}, query: url.Values{"unlinked": {"true"}}}, nil
	}
	return "", goingCall{}, goingUntranslatable("name ref; an issue, alone or with its artifact; a project, alone or with its artifact; or a message")
}

// goingPickReadRef is the read of the item value, a read's reference, names (goingReadRef): an
// issue (goingIssueRead), and its children, which the issue's own read lists; its log, as its
// events (goingIssueEvents); its comments; a document of an issue or a project; a block, as its
// node in the reference graph with what references it; an ask; a comment with its thread; and a
// message.
func goingPickReadRef(value string, arguments map[string]any, serverURL string) (string, goingCall, error) {
	ref, parsed := goingParseReadRef(value, serverURL)
	switch {
	case !parsed:
		return "", goingCall{}, goingUntranslatable(fmt.Sprintf(
			"ref %q is no reference to an issue, its log, its children or its comments, a document, a block, an ask, a comment or a message", value))
	case ref.of == goingRefLog:
		return goingIssueEvents(ref.IssueKey, arguments)
	case ref.of == goingRefChildren:
		return "GET /api/v1/issues/{key}", goingCall{path: map[string]string{"key": ref.IssueKey}}, nil
	case ref.of == goingRefComments:
		return "GET /api/v1/issues/{key}/comments", goingCall{path: map[string]string{"key": ref.IssueKey}}, nil
	case ref.Kind == "issue":
		return goingIssueRead(ref.IssueKey, arguments)
	case ref.Kind == "artifact" && ref.IssueKey != "":
		return "GET /api/v1/issues/{key}/artifacts/{slug}", goingCall{path: map[string]string{"key": ref.IssueKey, "slug": ref.ID}}, nil
	case ref.Kind == "artifact" && ref.Project != "":
		return "GET /api/v1/projects/{key}/artifacts/{slug}", goingCall{path: map[string]string{"key": ref.Project, "slug": ref.ID}}, nil
	case ref.Kind == "artifact":
		return "", goingCall{}, goingUntranslatable(fmt.Sprintf("ref %q names a file of an agent's conversation, which dispatch_doc_read reads", value))
	case ref.Kind == "block":
		return "GET /api/v1/references", goingCall{path: map[string]string{}, query: url.Values{"to": {value}}}, nil
	case ref.Kind == "ask":
		return "GET /api/v1/asks/{id}", goingCall{path: map[string]string{"id": ref.ID}}, nil
	case ref.Kind == "comment":
		return "GET /api/v1/comments/{id}", goingCall{path: map[string]string{"id": ref.ID}}, nil
	case ref.Kind == "message":
		return "GET /api/v1/issues/{key}/messages/{id}", goingCall{path: map[string]string{"key": ref.IssueKey, "id": ref.ID}}, nil
	}
	return "", goingCall{}, goingUntranslatable(fmt.Sprintf("ref %q names a %s, which a read does not read", value, ref.Kind))
}

// goingIssueRead is the read of an issue: its events where the call names how many of them it
// lists (events, all_events), which the issue's own read does not carry, and the issue otherwise.
func goingIssueRead(issue string, arguments map[string]any) (string, goingCall, error) {
	if arguments["events"] != nil || arguments["all_events"] == true {
		return goingIssueEvents(issue, arguments)
	}
	return "GET /api/v1/issues/{key}", goingCall{path: map[string]string{"key": issue}}, nil
}

// goingIssueEvents is the read of an issue's events: every one (all_events), oldest first, as
// many as one page of the route holds; else the newest of them, newest first, as many as events
// names or goingIssueReadEvents.
func goingIssueEvents(issue string, arguments map[string]any) (string, goingCall, error) {
	count := strconv.Itoa(goingIssueReadEvents)
	if named := arguments["events"]; named != nil {
		text, isText := goingArgumentText(named)
		if !isText {
			return "", goingCall{}, goingUntranslatable("events is how many of the issue's events the read lists, a number")
		}
		count = text
	}
	call := goingCall{path: map[string]string{"key": issue}}
	switch {
	case arguments["all_events"] == true && arguments["events"] != nil:
		return "", goingCall{}, goingUntranslatable("events and all_events both say how many events the read lists: name one")
	case arguments["all_events"] != true:
		call.query = url.Values{"order": {"desc"}, "limit": {count}}
	}
	return "GET /api/v1/issues/{key}/events", call, nil
}

// goingPickDocRead is the row of dispatch_doc_read a call reads, and what that row's route takes
// of the call, as `dispatch doc-read` reads a document
// (packages/envoy-client/src/dispatch-commands/documents.ts): the document a reference names, or
// the artifact the call names on its issue - the issue's spec where it names none - or on its
// project, or by the document's id; its text, with its blocks where blocks is set, or the version
// the call or the reference names. A file of an agent's conversation is read at the version named,
// and its record, which names its versions, where none is.
func goingPickDocRead(arguments map[string]any, serverURL string) (string, goingCall, error) {
	issue, project, artifact, session := goingText(arguments, "issue"), goingText(arguments, "project"), goingText(arguments, "artifact"), ""
	version, versioned := goingArgumentText(arguments["version"])
	if value := strings.TrimSpace(goingText(arguments, "ref")); value != "" {
		ref, parsed := goingParseReadRef(value, serverURL)
		if !parsed || ref.Kind != "artifact" {
			return "", goingCall{}, goingUntranslatable(fmt.Sprintf("ref %q is no reference to a document", value))
		}
		issue, project, session = ref.IssueKey, ref.Project, ref.Session
		if artifact == "" {
			artifact = ref.ID
		}
		if !versioned && ref.version != "" {
			version, versioned = ref.version, true
		}
	}
	blocks := arguments["blocks"] == true
	if blocks && versioned {
		return "", goingCall{}, goingUntranslatable("blocks lists the live document's blocks, and takes no version")
	}
	var row string
	var path map[string]string
	switch {
	case session != "":
		row, path = "GET /api/v1/agents/{session_id}/artifacts/{slug}", map[string]string{"session_id": session, "slug": artifact}
	case issue != "":
		row, path = "GET /api/v1/issues/{key}/artifacts/{slug}", map[string]string{"key": issue, "slug": cmp.Or(artifact, "spec")}
	case project != "" && artifact != "":
		row, path = "GET /api/v1/projects/{key}/artifacts/{slug}", map[string]string{"key": project, "slug": artifact}
	case project == "" && artifact != "":
		row, path = "GET /api/v1/artifacts/{id}", map[string]string{"id": artifact}
	default:
		return "", goingCall{}, goingUntranslatable("name ref; an issue, with its artifact or for its spec; a project with its artifact; or a document's id as artifact")
	}
	call := goingCall{path: path}
	switch {
	case versioned:
		row, path["number"] = row+"/versions/{number}", version
	case session != "":
		// A conversation holds files alone, which no text route reads.
	default:
		row += "/text"
		if blocks {
			call.query = url.Values{"blocks": {"true"}}
		}
	}
	return row, call, nil
}

// goingTranslateArchitectureSyncRepo imports one architecture source, named by its owner/name
// repository as the sync route's path takes it (one {owner} and one {repo} segment), split by
// goingSourceRepository; goingUnsendable refused a value not written so before the translation.
// The project fills {key}. The row that chose it bound both path segments to `repo`, so the split
// is read here.
func goingTranslateArchitectureSyncRepo(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	owner, name, _ := goingSourceRepository(goingText(call.arguments, "repo"))
	return goingCall{path: map[string]string{"key": call.path["key"], "owner": owner, "repo": name}}, nil
}

// goingTranslateClaim sends a claim, and its release, an empty JSON body: both routes decode one,
// and take the claimant from the caller's credential. `release` only picks the row.
func goingTranslateClaim(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, body: map[string]any{}}, nil
}

// goingTranslateIssueUpdate is `dispatch issue-update`'s translation: `parent` and `repo` given
// empty are cleared, `before` and `after` are the rank's neighbours, `advances` is the priority
// ids the issue states and `advances_revision` the revision the read it answers returned (the
// route's own compare-and-set), `external_links` adds the links named to the set the issue holds
// (the route replaces the set; an empty list clears it), and a `reason`, which goingUnsendable
// takes only beside a close, is posted on the issue as a message
// before the close, as `dispatch issue-update --reason` posts it (updateIssueInput has no reason
// field). An issue already closed is refused before its reason is posted, so no reason lands as
// a stray message; a write that resumes after its own reason, landed by an earlier try of the
// change (goingCallInput.resumes), is refused for the close alone, and the refusal says the reason
// was written.
func goingTranslateIssueUpdate(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	arguments, key := call.arguments, call.path["key"]
	body := goingPick(arguments, "status", "title", "labels", "priority", "assignee", "route",
		"blocked_by", "components", "advances", "advances_revision")
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
		refusal := goingRefusal{Status: http.StatusConflict, Code: "ISSUE_CLOSED",
			Message: fmt.Sprintf("%s closed at %s, so the reason was not posted and the close was not sent", key, *issue.ClosedAt)}
		message, err := goingJSONRequest(http.MethodPost, goingIssuePath(key, "messages"), map[string]any{"body": reason})
		if err != nil {
			return goingCall{}, err
		}
		message.what = goingReasonWhat(key)
		if landed := call.resumes(message); len(landed) > 0 {
			// The call resumes after its own reason, which stands: only the close was not sent.
			refusal.Message = fmt.Sprintf("%s was written; the close was not sent: %s closed at %s", message.what, key, *issue.ClosedAt)
		}
		return goingCall{}, refusal
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
		message.what = goingReasonWhat(key)
		translated.first = []goingSend{message}
	}
	return translated, nil
}

// goingReasonWhat is the words of the message a reasoned close posts on issue key before the close
// (goingSend.what).
func goingReasonWhat(key string) string {
	return "the reason posted on " + key
}

// goingAskBody is what both ask rows take of a call: the question, with a `ref` the question
// does not already hold written under it as `dispatch ask` writes it, its options, multiple and
// urgency.
func goingAskBody(arguments map[string]any) map[string]any {
	body := goingPick(arguments, "options", "multiple", "urgency")
	question := goingText(arguments, "question")
	if ref := goingText(arguments, "ref"); ref != "" && !strings.Contains(question, ref) {
		question += "\n\nRef: " + ref
	}
	body["question"] = question
	return body
}

// goingTranslateIssueAsk opens the ask on the issue, anchored to the passage of the issue's
// document the anchor names, that document resolved from its slug.
func goingTranslateIssueAsk(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	body := goingAskBody(call.arguments)
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
	body := goingAskBody(call.arguments)
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
// replies under, the turn, and each target it mentions, which the route reads as `{target}`, as
// `dispatch comment --mention` sends it.
func goingCommentBody(arguments map[string]any) map[string]any {
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
	if targets, named := arguments["mentions"].([]any); named {
		mentions := make([]map[string]any, 0, len(targets))
		for _, target := range targets {
			mentions = append(mentions, map[string]any{"target": target})
		}
		body["mentions"] = mentions
	}
	return body
}

// goingTranslateIssueComment posts on the issue, on a quote of one of its documents where the
// call names the document and the quote.
func goingTranslateIssueComment(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	body := goingCommentBody(call.arguments)
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
	body := goingCommentBody(call.arguments)
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
func goingSuggestion(id string, arguments map[string]any) map[string]any {
	body := goingPick(arguments, "body")
	body["anchor"] = goingAnchor(id, arguments)
	body["suggestion"] = map[string]any{"replace_with": arguments["replace_with"]}
	return body
}

// goingTranslateIssueSuggest suggests on the issue's document the call names, the spec where it
// names none.
func goingTranslateIssueSuggest(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	id, err := read.issueDocument(ctx, call.path["key"], goingText(call.arguments, "artifact"))
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{path: call.path, body: goingSuggestion(id, call.arguments)}, nil
}

// goingTranslateDocumentSuggest suggests on a document named by id, or by slug on the project the
// call names.
func goingTranslateDocumentSuggest(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	id, err := goingDocument(ctx, read, call.arguments, call.path["id"])
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{path: map[string]string{"id": id}, body: goingSuggestion(id, call.arguments)}, nil
}

// goingTranslateIssueMessage posts on the issue's conversation, as a reply to the message or the
// block of one `in_reply_to` names.
func goingTranslateIssueMessage(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
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

// goingTranslateSendToAgent sends to the session the call names by id or by title (recipient).
func goingTranslateSendToAgent(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	session, err := read.recipient(ctx, call.path["session_id"])
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{
		path: map[string]string{"session_id": session},
		body: goingPick(call.arguments, "body", "delivery", "in_reply_to"),
	}, nil
}

// goingTranslateSendOnIssue posts a send to a session on the issue the session works on
// (going_messages.go): a message on that issue's conversation, threaded as an issue message is
// (goingTranslateIssueMessage), that targets the session the call names by id or by title
// (recipient) in the delivery the call names.
func goingTranslateSendOnIssue(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	posted, err := goingTranslateIssueMessage(ctx, read, call)
	if err != nil {
		return goingCall{}, err
	}
	if name := goingText(call.arguments, "session"); name != "" {
		session, err := read.recipient(ctx, name)
		if err != nil {
			return goingCall{}, err
		}
		posted.body["target"] = "session:" + session
	}
	if delivery, named := call.arguments["delivery"]; named {
		posted.body["delivery"] = delivery
	}
	return posted, nil
}

// goingTranslateDocEdit sends the edit's operations, its precondition and its summary; the issue
// and project rows take the document's slug in their path as the call names it.
func goingTranslateDocEdit(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
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

// goingTranslateReview records the review of the version the call names on the issue's document
// the call names, its spec by default: the row binds the issue to the document placeholder, and
// the document's id is read here. The tool's verdict is the route's state.
func goingTranslateReview(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	id, err := read.issueDocument(ctx, call.path["id"], goingText(call.arguments, "artifact"))
	if err != nil {
		return goingCall{}, err
	}
	body := goingPick(call.arguments, "reason")
	body["state"] = call.arguments["verdict"]
	return goingCall{path: map[string]string{"id": id, "number": call.path["number"]}, body: body}, nil
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

// goingTranslateUpload uploads the call's own content as a document of the issue or the project,
// as the JSON form of the route takes it (goingUnsendable refused a call naming a file to read it
// from, or no content, before the translation). `force` is no field of the route's: every upload
// is sent as a create alone (goingUploadCondition), so a document of its name the owner holds by
// the time the upload takes the owner's lock - one another writer made after the write's read
// before - is the route's to refuse, 409 DOCUMENT_EXISTS. A row of a run's plan whose approval
// showed the document of its name, and whose read before finds that document with the text the
// approval showed, is sent as its next version, with no condition and naming the token that read
// answered; a row whose read before finds anything else under the name waits for its person again
// (AgentListener.rowUpload, uploadChanged).
func goingTranslateUpload(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	body := goingPick(call.arguments, "name", "summary")
	body["content"] = goingText(call.arguments, "content")
	return goingCall{path: call.path, body: body, condition: goingUploadCondition()}, nil
}

// goingUploadCondition is the condition an upload carries to its route on its context
// (going_conditions.go): a create alone (ExpectNew), since whether an upload writes over a
// document is known only once its read before is in. A row of a plan sheds it at the write only
// where its approval showed the document of its name and its read before finds that document as
// the approval showed it (AgentListener.rowUpload).
func goingUploadCondition() *goingCondition {
	return &goingCondition{ExpectNew: true}
}

// goingTranslateAgentUpload uploads the call's own content as a file of the session's
// conversation, which takes a multipart upload alone: the file part named as the call names it,
// typed by its extension, and plain text where the extension says nothing. It is sent as a create
// alone, as goingTranslateUpload sends one (goingUploadCondition).
func goingTranslateAgentUpload(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	content := goingText(call.arguments, "content")
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
	}, condition: goingUploadCondition()}, nil
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
