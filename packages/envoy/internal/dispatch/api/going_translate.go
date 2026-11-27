package api

// going_translate.go is what each row of the going catalogue's routes takes of a call (R12). A
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
// command does (goingPickRead, goingPickDocRead). dispatch_doc_read reads the record of the item
// it names first, as its command does, and reads an uploaded file at one of its versions
// (goingChooseDocRead).
//
// What the bar can never send - a picture or a file the CLI reads off the machine it runs on, which
// the bar has no way to reach, a `ref` that names no place a write goes, and the rest
// goingUnsendable names - is decided by goingUnsendable, which needs no read: GoingCaller refuses
// such a call before its row's translation. A `ref` that names an issue or a document is read as
// that place (goingRefPlace), and the call goes on as the call naming it. What a translator cannot
// send for a reason of its own it refuses by name (goingUntranslatable), before anything is
// written.

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
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
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
// the row's own, the compare-and-set the row's own request carries to its route on its context
// (going_conditions.go), nil for none, the uploaded file a read of one of its versions reads,
// whose answer the model is told in words of its own (goingUploadedFile), nil for any other call,
// and whether it is the roster read, whose answer the model is told with the claims of each
// session (goingRosterTold).
type goingCall struct {
	path      map[string]string
	query     url.Values
	body      map[string]any
	form      *goingUpload
	first     []goingSend
	condition *goingCondition
	file      *goingUploadedFile
	roster    bool
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
// the server after it landed (goingPanickedText); it is no part of what the request sends. file
// is the uploaded file whose version the request reads, whose answer the call tells as
// goingUploadedFile.told words it, nil for any other request, and roster whether it is the roster
// read, whose answer the call tells as goingRosterTold does; neither is any part of what is sent.
type goingSend struct {
	method      string
	path        string
	query       url.Values
	contentType string
	body        []byte
	stable      []byte
	condition   *goingCondition
	what        string
	file        *goingUploadedFile
	roster      bool
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
// it runs on and the bar has no way to open; a `ref` of a comment, a suggestion or an edit that
// names no place such a write goes (goingRefPlace); an upload with no text; a closing `reason` on
// an update that does not close the issue, which the update posts only before a close; an update
// that sets an option of the status change (include_children, unlink_children, include_parent),
// whose route writes several issues, each in a transaction of its own, which no landing of one
// request records and no Undo walks. A plan call never reaches this: it writes such a change out
// as one update row for each issue the option reaches (expandStatusOptions), so no row of a plan
// carries an option, and this refusal is the backstop for a call that reaches a row with one; and
// a sync's source repository not written as owner/name, the two segments its route's path takes.
// An ask's `ref` is none of these: the question carries it as a line of its own (goingAskBody).
// None needs a read or a row, so GoingCaller refuses such a call before it holds the call to the
// tool's rule and chooses its row (prepare), with words neither the rule nor a translator
// repeats. Each is worded for the person reading the bar's panel - what the bar cannot send, and
// what that person can do there - and names no tool, no argument, no command line and no flag,
// none of which that person holds. A link or a file path written empty names no link or file, so
// it counts as absent, as null does (which the action's schema refuses first), and a picture list
// written empty is refused by the schema first too, since it holds one picture at least: no
// translator sends any of the three on, and the call is sent as it would be without it.
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
	case "dispatch_ask":
		if named("images") {
			return goingNoPictures("a question")
		}
	case "dispatch_message":
		if named("images") {
			return goingNoPictures("a message")
		}
	case "dispatch_artifact":
		if named("path") {
			return "the bar cannot upload a file, since the file is one the answer named and the bar has no way to open it. Ask again with the text to upload written in your question."
		}
		if _, text := arguments["content"].(string); !text {
			return "the bar cannot send an upload with no text, since it uploads only the text the answer writes. Ask again with the text to upload written in your question."
		}
	case "dispatch_issue_update":
		for _, option := range goingStatusOptions {
			if arguments[option] == true {
				return "the bar cannot change the issues under or above an issue together with it in one change, since it takes back a change one issue at a time. Ask again for each issue's change on its own."
			}
		}
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
	if _, why := goingRefPlace(action, arguments); why != "" {
		return why
	}
	return ""
}

// goingNoPictures is why the bar cannot post write - a comment, a question, a message - with the
// picture files the call names (goingUnsendable).
func goingNoPictures(write string) string {
	return fmt.Sprintf("the bar cannot post %s with a picture, since the picture is a file the answer named and the bar has no way to open it. Ask again for the words alone.", write)
}

// goingRefWrites are the writes whose spec lets a `ref`, a dispatch:// reference to an issue or a
// document, name their place, each by what it is in the words its refusal says.
var goingRefWrites = map[string]string{
	"dispatch_comment":  "a comment",
	"dispatch_suggest":  "a suggested change",
	"dispatch_doc_edit": "an edit of a document",
}

// goingRefPlace is arguments of action, a write that takes a `ref` (goingRefWrites), with that
// reference read as the place it names, as the server reads a reference (text.ParseRef), and as if
// the call had named that place: an issue's reference as the issue, a reference to a document of
// an issue, its spec included, as the issue and that document, and one to a project's document as
// the project and that document, the ref left out, so a plan's row is the row of the call that
// names the place. A suggestion is a change in a document, so an issue's reference names the
// issue's spec where the call names no document, as a suggestion on the issue does
// (goingTranslateIssueSuggest). It answers arguments themselves where action takes no ref or the
// call names none, and otherwise, as why, the refusal of a reference the bar cannot read as such a
// place: one that is no reference; one of a block, which places nothing a quote does not; one of
// an ask, a comment, a message, a component or a file of an agent's conversation, none of which
// these writes go on; one of a version of a document, where a write goes on the document as it
// stands; and one that names another place than the issue, the project or the document the call
// names besides it. Each refusal is worded as goingUnsendable's are.
func goingRefPlace(action string, arguments map[string]any) (map[string]any, string) {
	write, takes := goingRefWrites[action]
	value, _ := arguments["ref"].(string)
	if !takes || value == "" {
		return arguments, ""
	}
	ref, parsed := text.ParseRef(value, "")
	elsewhere := func(what string) string {
		return fmt.Sprintf("the bar cannot send %s whose place is given by the link of %s, since it goes on an issue or a document. Ask again naming the issue or the document it is for.", write, what)
	}
	switch {
	case !parsed:
		return nil, fmt.Sprintf("the bar cannot send %s whose place is given by a link that names no issue or document of Dispatch. Ask again naming the issue or the document it is for.", write)
	case ref.Kind == "block":
		return nil, fmt.Sprintf("the bar cannot send %s whose place is given by the link of one block, since it places it by its issue or its document and the words it quotes there. Ask again naming the document and quoting the words of that block.", write)
	case ref.Kind == "artifact" && ref.Session != "":
		return nil, elsewhere("a file of a conversation on the Agents page")
	case ref.Kind != "issue" && ref.Kind != "artifact":
		return nil, elsewhere(cmp.Or(goingRefOthers[ref.Kind], "something that is no issue or document"))
	case goingRefVersion.MatchString(value):
		return nil, fmt.Sprintf("the bar cannot send %s on a version of a document given by a link, since it writes on the document as it stands. Ask again naming the document without its version.", write)
	}
	owner, key := "project", ref.Project
	if ref.IssueKey != "" {
		owner, key = "issue", ref.IssueKey
	}
	twoPlaces := func(named string) string {
		return fmt.Sprintf("the bar cannot send %s whose link, %s, names another place than %s. Ask again naming the one place it is for.", write, value, named)
	}
	for _, name := range []string{"issue", "project"} {
		if named := goingText(arguments, name); named != "" && (name != owner || named != key) {
			return nil, twoPlaces(named)
		}
	}
	// An issue's reference beside a document of that issue names that document.
	document := goingText(arguments, "artifact")
	switch {
	case ref.Kind == "artifact" && document != "" && document != ref.ID:
		return nil, twoPlaces(document)
	case ref.Kind == "artifact":
		document = ref.ID
	case action == "dispatch_suggest" && document == "":
		document = "spec"
	}
	placed := maps.Clone(arguments)
	delete(placed, "ref")
	placed[owner] = key
	if document != "" {
		placed["artifact"] = document
	}
	return placed, ""
}

// goingRefOthers is what each kind of reference names that no write of goingRefWrites goes on,
// in the words goingRefPlace refuses it with.
var goingRefOthers = map[string]string{"ask": "an ask", "comment": "a comment", "message": "a message", "component": "a component"}

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

// goingAskPath, goingCommentPath, goingMessagePath and goingArtifactPath are the same for an ask,
// a comment, a message and a document by id.
func goingAskPath(id string, below ...string) string {
	return "/api/v1/asks/" + url.PathEscape(id) + goingBelow(below)
}

func goingCommentPath(id string) string {
	return "/api/v1/comments/" + url.PathEscape(id)
}

func goingMessagePath(id string) string {
	return "/api/v1/messages/" + url.PathEscape(id)
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
// carries the call's condition, the file it reads and whether it is the roster read.
func (c goingCall) requests(route GoingRoute) ([]goingSend, error) {
	own := goingSend{method: route.Method, path: goingPath(route.Pattern, c.path), query: c.query, condition: c.condition, file: c.file, roster: c.roster}
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

// goingPostID resolves a short post reference within the owner it names, as the CLI does.
// The reference graph read already resolves such prefixes for the caller, including those
// on project documents. Its node carries the full id. Passing the prefix directly to the
// post route would instead let the route match posts on other owners.
// A bare comment prefix, as reopen takes, uses the comment route's unscoped lookup.
func goingPostID(ctx context.Context, read goingReader, kind, value string) (string, error) {
	id, _ := goingReplyTarget(value)
	if !text.IsIDPrefix(id) {
		return id, nil
	}
	if ref, ok := text.ParseRef(value, ""); ok && (ref.Kind == kind || ref.Kind == "block" && ref.Item == kind) {
		// Resolve the post itself, even when the supplied reference names one of its blocks.
		post, _, _ := strings.Cut(value, "#")
		var answer struct {
			Node struct {
				ID string `json:"id"`
			} `json:"node"`
		}
		query := url.Values{"to": {post}}
		if err := read.get(ctx, "/api/v1/references?"+query.Encode(), &answer); err != nil {
			var refusal goingRefusal
			if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound {
				refusal.Message = fmt.Sprintf("No %s matches %s. Use the full id.", kind, value)
				return "", refusal
			}
			return "", err
		}
		return answer.Node.ID, nil
	}
	if kind == "comment" && id == value {
		var answer struct {
			Comment struct {
				ID string `json:"id"`
			} `json:"comment"`
		}
		if err := read.get(ctx, goingCommentPath(id), &answer); err != nil {
			return "", err
		}
		return answer.Comment.ID, nil
	}
	return id, nil
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

// goingTranslateNoBody sends no body: a follow and its unfollow, an architecture sync of every
// source, and read_progress of an issue, which reads the issue its path names whatever else the
// call names, take everything from their path and the caller's credential. The every-source sync
// row is chosen when the call names no `repo`; a sync that names one takes the one-source row
// (goingTranslateArchitectureSyncRepo).
func goingTranslateNoBody(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path}, nil
}

// goingTranslateResolveComment resolves the thread whose root comment the call names, by id or by
// a dispatch:// reference, whose id may be written short (goingPostID), and sends no body: the
// resolve takes everything else from the caller's credential.
func goingTranslateResolveComment(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	id, err := goingPostID(ctx, read, "comment", call.path["id"])
	if err != nil {
		return goingCall{}, err
	}
	return goingCall{path: map[string]string{"id": id}}, nil
}

// goingTranslateRoster sends read_roster, which takes no argument, as GET /api/v1/agents of the
// goingReadSessionMax sessions seen last, the most the answer run's own roster read lists
// (going_reads.go), so the route reads the open asks and activity of those alone and loads no more
// rows of a roster of any size than it answers. The model is told that answer with the claims of
// each session, the gone sessions that hold one and the total of every session (goingRosterTold).
func goingTranslateRoster(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, query: url.Values{"limit": {strconv.Itoa(goingReadSessionMax)}}, roster: true}, nil
}

// goingTranslateProjectIssues sends read_progress of a project, a call that names no issue, as GET
// /api/v1/issues of that project with limit contracts.MaxIssuePageLimit: a page of the project's
// issues, each with its status and its progress, as the project's page lists them, and the total
// of every issue of it, which the route counts in SQL ({issues, total, limit, offset}). So, as
// goingTranslateRoster bounds the roster, the answer of a project of any size stays inside what
// the server's writer carries (agentwire.CallBodyLimit), and the model is told the project's total
// before the list (going/agent's resultCount). A call that names no project either, on a run whose
// page names neither (goingPageArguments), is refused as the answer run's read refuses it
// (goingReadRun.project): the listing of every project's issues is no one project's progress.
func goingTranslateProjectIssues(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	project := strings.TrimSpace(goingText(call.arguments, "project"))
	if project == "" {
		return goingCall{}, goingUntranslatable("name the issue or the project to read: the call names neither and none is on the page")
	}
	return goingCall{path: call.path, query: url.Values{"project": {project}, "limit": {strconv.Itoa(contracts.MaxIssuePageLimit)}}}, nil
}

// goingTranslateDeliveryWindow sends read_delivery of the measures or the timeline as its view's
// route over the window from and to name, each under its own name; the view only picks the row.
func goingTranslateDeliveryWindow(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	query, err := goingDeliveryQuery(call.arguments, []goingQueryKey{{argument: "from", key: "from"}, {argument: "to", key: "to"}})
	return goingCall{path: call.path, query: query}, err
}

// goingTranslateDeliveryPipeline sends read_delivery of the pipeline as GET
// /api/v1/delivery/pipeline of the seven days ending on the call's to, the route's at, as the
// answer run reads it (goingReadRun.readDelivery), which reads no from of a pipeline either.
func goingTranslateDeliveryPipeline(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	query, err := goingDeliveryQuery(call.arguments, []goingQueryKey{{argument: "to", key: "at"}})
	return goingCall{path: call.path, query: query}, err
}

// goingQueryKey is an argument of a call and the key of the route's query that the argument is
// sent under. A translation lists them in the call's order, so the first argument it cannot send
// is the one its error names.
type goingQueryKey struct{ argument, key string }

// goingDeliveryQuery is the query of a delivery read: each argument of keys the call names, under
// its key, a blank one left out as the answer run's read leaves it out.
func goingDeliveryQuery(arguments map[string]any, keys []goingQueryKey) (url.Values, error) {
	query := url.Values{}
	for _, sent := range keys {
		value, named := arguments[sent.argument]
		if !named || value == nil {
			continue
		}
		text, isText := value.(string)
		if !isText {
			return nil, goingUntranslatable(fmt.Sprintf("%s is a time in RFC 3339, and the call sends a %T", sent.argument, value))
		}
		if text = strings.TrimSpace(text); text != "" {
			query.Set(sent.key, text)
		}
	}
	return query, nil
}

// goingTranslateSearch sends dispatch_search's words as the search route's q, and its project,
// its limit and its offset under their own names, as `dispatch search` sends them.
func goingTranslateSearch(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	query := url.Values{}
	for _, sent := range []goingQueryKey{
		{argument: "query", key: "q"}, {argument: "project", key: "project"}, {argument: "limit", key: "limit"}, {argument: "offset", key: "offset"},
	} {
		value, named := call.arguments[sent.argument]
		if !named || value == nil {
			continue
		}
		text, isText := goingArgumentText(value)
		if !isText {
			return goingCall{}, goingUntranslatable(fmt.Sprintf("%s is sent in the query, which takes no %T", sent.argument, value))
		}
		query.Set(sent.key, text)
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
// a read reads. An issue's log, its children and its comments are read first (goingParseReadOf):
// text.ParseRef reads `dispatch://KEY/log`, `/activity` and `/children` as the issue itself, as
// the dashboard draws a link to a tab of its page, where a read of them reads that part.
func goingParseReadRef(value, serverURL string) (goingReadRef, bool) {
	if read, named := goingParseReadOf(value); named {
		return read, true
	}
	if ref, parsed := text.ParseRef(value, serverURL); parsed {
		read := goingReadRef{Ref: ref}
		if ref.Kind == "artifact" {
			if version := goingRefVersion.FindStringSubmatch(value); version != nil {
				read.version = version[1]
			}
		}
		return read, true
	}
	return goingReadRef{}, false
}

// goingParseReadOf reads value as a reference to an issue's log (or its activity), its children
// or its comments, `dispatch://KEY/<part>`, false for any other value.
func goingParseReadOf(value string) (goingReadRef, bool) {
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

// goingReadHistoryRule is why a read with history reads nothing: a post's history is that of
// one comment or one issue message, named by a reference, as `dispatch read --history` reads it.
const goingReadHistoryRule = "history reads the bodies the edits and the delete of one post replaced, so it needs a ref naming one comment or one issue message"

// goingPickRead is the row of dispatch_read a call reads, and what that row's route takes of the
// call, as `dispatch read` picks what it reads (packages/envoy-client/src/dispatch-commands/read.ts),
// each item through the route a person reads it with: the item a reference names
// (goingPickReadRef); else a message, on its issue where the call names one; an issue's or a
// project's document; an issue (goingIssueRead); and a project's documents on no issue.
func goingPickRead(arguments map[string]any, serverURL string) (string, goingCall, error) {
	if value := strings.TrimSpace(goingText(arguments, "ref")); value != "" {
		return goingPickReadRef(value, arguments, serverURL)
	}
	if arguments["history"] == true {
		return "", goingCall{}, goingUntranslatable(goingReadHistoryRule)
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
// message. With history it is the history of the comment or the issue message the reference
// names, each body its edits and its delete replaced, and of nothing else.
func goingPickReadRef(value string, arguments map[string]any, serverURL string) (string, goingCall, error) {
	ref, parsed := goingParseReadRef(value, serverURL)
	history := arguments["history"] == true
	switch {
	case !parsed:
		return "", goingCall{}, goingUntranslatable(fmt.Sprintf(
			"ref %q is no reference to an issue, its log, its children or its comments, a document, a block, an ask, a comment or a message", value))
	case history && (ref.of != "" || (ref.Kind != "comment" && ref.Kind != "message")):
		return "", goingCall{}, goingUntranslatable(goingReadHistoryRule)
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
	case ref.Kind == "comment" && history:
		return "GET /api/v1/comments/{id}/history", goingCall{path: map[string]string{"id": ref.ID}}, nil
	case ref.Kind == "comment":
		return "GET /api/v1/comments/{id}", goingCall{path: map[string]string{"id": ref.ID}}, nil
	case ref.Kind == "message" && history:
		return "GET /api/v1/messages/{id}/history", goingCall{path: map[string]string{"id": ref.ID}}, nil
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

// goingDocRead is the item a dispatch_doc_read call names, as `dispatch doc-read` names it
// (packages/envoy-client/src/dispatch-commands/documents.ts): the document a reference names, or
// the artifact the call names on its issue - the issue's spec where it names none - on its
// project, by the document's id, or in an agent's conversation, which only a reference names.
// record is the pattern of the route that reads the item's record, and path the value of each of
// its placeholders; version is the version the call or the reference names, where versioned is
// set; blocks is whether the call asks for the document's blocks.
type goingDocRead struct {
	record    string
	path      map[string]string
	version   string
	versioned bool
	blocks    bool
}

// goingDocReadOf is the item arguments name (goingDocRead), refused where they name none, or name
// blocks at a version.
func goingDocReadOf(arguments map[string]any, serverURL string) (goingDocRead, error) {
	issue, project, artifact, session := goingText(arguments, "issue"), goingText(arguments, "project"), goingText(arguments, "artifact"), ""
	version, versioned := goingArgumentText(arguments["version"])
	if value := strings.TrimSpace(goingText(arguments, "ref")); value != "" {
		ref, parsed := goingParseReadRef(value, serverURL)
		if !parsed || ref.Kind != "artifact" {
			return goingDocRead{}, goingUntranslatable(fmt.Sprintf("ref %q is no reference to a document", value))
		}
		issue, project, session = ref.IssueKey, ref.Project, ref.Session
		if artifact == "" {
			artifact = ref.ID
		}
		if !versioned && ref.version != "" {
			version, versioned = ref.version, true
		}
	}
	item := goingDocRead{version: version, versioned: versioned, blocks: arguments["blocks"] == true}
	if item.blocks && versioned {
		return goingDocRead{}, goingUntranslatable("blocks lists the live document's blocks, and takes no version")
	}
	switch {
	case session != "":
		item.record, item.path = "/api/v1/agents/{session_id}/artifacts/{slug}", map[string]string{"session_id": session, "slug": artifact}
	case issue != "":
		item.record, item.path = "/api/v1/issues/{key}/artifacts/{slug}", map[string]string{"key": issue, "slug": cmp.Or(artifact, "spec")}
	case project != "" && artifact != "":
		item.record, item.path = "/api/v1/projects/{key}/artifacts/{slug}", map[string]string{"key": project, "slug": artifact}
	case project == "" && artifact != "":
		item.record, item.path = "/api/v1/artifacts/{id}", map[string]string{"id": artifact}
	default:
		return goingDocRead{}, goingUntranslatable("name ref; an issue, with its artifact or for its spec; a project with its artifact; or a document's id as artifact")
	}
	return item, nil
}

// goingPickDocRead is the row of dispatch_doc_read a call reads where its item is a document, and
// what that row's route takes of the call, with no read made (goingDocRead.pick). A call is sent
// to the row goingChooseDocRead chooses, which reads the item's record first.
func goingPickDocRead(arguments map[string]any, serverURL string) (string, goingCall, error) {
	item, err := goingDocReadOf(arguments, serverURL)
	if err != nil {
		return "", goingCall{}, err
	}
	row, call := item.pick()
	return row, call, nil
}

// pick is the row that reads the item where it is a document, and what that row's route takes of
// the call: its text, with its blocks where blocks is set, or the version named. A file of an
// agent's conversation is read at the version named, and its record, which names its versions,
// where none is.
func (d goingDocRead) pick() (string, goingCall) {
	row, call := http.MethodGet+" "+d.record, goingCall{path: d.path}
	switch {
	case d.versioned:
		row, call.path = row+"/versions/{number}", maps.Clone(d.path)
		call.path["number"] = d.version
	case d.path["session_id"] != "":
		// A conversation holds files alone, which no text route reads.
	default:
		row += "/text"
		if d.blocks {
			call.query = url.Values{"blocks": {"true"}}
		}
	}
	return row, call
}

// goingChooseDocRead is the row of dispatch_doc_read a call is sent to, and what that row's route
// takes of the call, as `dispatch doc-read` reads the item the call names (goingDocReadOf): it
// reads the item's record through read first, as the command's resolveDocument does, and reads a
// document as pick picks it. An uploaded file or picture, whose text route answers 400
// NOT_DOCUMENT, is read as the command's readUploadedFile reads one: at the version the call
// names, else at its latest, through the row of that version, whose answer the model is told in
// the command's words (goingUploadedFile.told); a call for its blocks is refused in the command's
// words. Where the read of the record is refused, the call keeps the row pick picks, whose route
// answers its own refusal, as before any record was read.
func goingChooseDocRead(ctx context.Context, read goingReader, arguments map[string]any, serverURL string) (string, goingCall, error) {
	item, err := goingDocReadOf(arguments, serverURL)
	if err != nil {
		return "", goingCall{}, err
	}
	row, call := item.pick()
	var record struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Versions []struct {
			Number int  `json:"number"`
			Size   *int `json:"size"`
		} `json:"versions"`
	}
	if err := read.get(ctx, goingPath(item.record, item.path), &record); err != nil {
		var refused goingRefusal
		if errors.As(err, &refused) {
			return row, call, nil
		}
		return "", goingCall{}, err
	}
	if record.Kind != "file" && record.Kind != "image" {
		return row, call, nil
	}
	if item.blocks {
		return "", goingCall{}, goingUntranslatable(fmt.Sprintf(
			"%s is an uploaded %s, not a document, so it has no blocks. Read it with dispatch doc-read without --blocks.", record.Name, record.Kind))
	}
	file := goingUploadedFile{id: record.ID, name: record.Name, kind: record.Kind}
	for _, version := range record.Versions {
		file.latest = max(file.latest, version.Number)
	}
	file.version = file.latest
	if item.versioned {
		if file.version, err = strconv.Atoi(item.version); err != nil {
			// The version's route refuses a version that is no number, in its own words.
			return row, call, nil
		}
	}
	for _, version := range record.Versions {
		if version.Number == file.version && version.Size != nil {
			file.size = *version.Size
		}
	}
	item.path["number"] = strconv.Itoa(file.version)
	return http.MethodGet + " " + item.record + "/versions/{number}", goingCall{path: item.path, file: &file}, nil
}

// goingUploadedFile is the uploaded file or picture whose version a dispatch_doc_read reads
// (goingChooseDocRead): its id, name and kind, the version read and its latest version, and the
// bytes its record states that version holds, 0 where it states none.
type goingUploadedFile struct {
	id, name, kind  string
	version, latest int
	size            int
}

// told is the answer of the version's route as the model is told it, in the words `dispatch
// doc-read` writes (readUploadedFile): a file whose bytes are UTF-8 text is that text, after a line
// that names the file, its type, the version and its size; any other file, and every picture, is
// those facts and the route that serves its bytes, which are not sent. A refusal is told as the
// route answered it. The text of an answer the server cut ends at its last whole character, and
// its size is the one its record states.
func (f goingUploadedFile) told(answer agentwire.CallAnswer) agentwire.CallAnswer {
	if answer.Status != http.StatusOK {
		return answer
	}
	media, size, content := cmp.Or(answer.ContentType, "application/octet-stream"), len(answer.Body), answer.Body
	if answer.Cut {
		size, content = max(size, f.size), goingWholeRunes(content)
	}
	of := ""
	if f.version != f.latest {
		of = fmt.Sprintf(" of %d", f.latest)
	}
	told := agentwire.CallAnswer{Status: http.StatusOK, ContentType: "text/plain; charset=utf-8"}
	if f.kind == "image" || !utf8.Valid(content) {
		told.Body = fmt.Appendf(nil, "%s is an uploaded %s file (version %d%s, %d bytes) that is not UTF-8 text, so dispatch doc-read cannot show it. GET %s serves its bytes.",
			f.name, media, f.version, of, size, goingArtifactPath(f.id, "versions", strconv.Itoa(f.version)))
		return told
	}
	header := fmt.Sprintf("File %s: %s, version %d%s, %d bytes.\n\n", f.name, media, f.version, of, size)
	told.Body, told.Cut = append([]byte(header), content...), answer.Cut
	return told
}

// goingWholeRunes is content less the first bytes of a character its end cuts through.
func goingWholeRunes(content []byte) []byte {
	for back := 1; back <= min(utf8.UTFMax, len(content)); back++ {
		if at := len(content) - back; utf8.RuneStart(content[at]) {
			if utf8.FullRune(content[at:]) {
				return content
			}
			return content[:at]
		}
	}
	return content
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
// empty are cleared, `before` and `after` are the rank's neighbours, `labels_add` is sent as the
// route takes it, adding to the labels the issue holds when the write commits, `advances` is the
// priority ids the issue states and `advances_revision` the revision the read it answers returned
// (the route's own compare-and-set), `external_links` adds the links named to the set the issue
// holds (the route replaces the set; an empty list clears it), and a `reason`, which
// goingUnsendable takes only beside a close, is posted on the issue as a message
// before the close, as `dispatch issue-update --reason` posts it (updateIssueInput has no reason
// field). An issue already closed is refused before its reason is posted, so no reason lands as
// a stray message; a write that resumes after its own reason, landed by an earlier try of the
// change (goingCallInput.resumes), is refused for the close alone, and the refusal says the reason
// was written.
func goingTranslateIssueUpdate(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	arguments, key := call.arguments, call.path["key"]
	body := goingPick(arguments, "status", "title", "labels", "labels_add", "priority", "assignee", "route",
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

// goingTranslateIssueState is dispatch_set_issue_state's translation. PUT
// /api/v1/me/issues/{key}/state replaces the person's whole stored list of the issue's items
// (putUserState), which holds the marker of each item pinned on the issue's page beside the items
// dismissed, so a call that sent its own items alone would delete every earlier one. The
// translation reads the person's state as GET /api/v1/me/state answers it and sends the stored
// list with the call's items added after it, each once, and the next seq of the row it read: the
// route refuses a write whose seq is not larger than the row's, 409 STATE_STALE, so a write
// between the read and this one that carried a seq - every write of the list carries one - is
// refused rather than written over. The issue page's pin toggle carries no seq and leaves the
// row's as it was, so a pin or an unpin made there in between is not refused, and a call that
// names pinned writes over it. An issue the person holds no state of reads as the row the route
// would create, nothing in the list and seq 0. pinned and last_read_seq are sent as the call names
// them.
func goingTranslateIssueState(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	var state userState
	if err := read.get(ctx, "/api/v1/me/state", &state); err != nil {
		return goingCall{}, err
	}
	stored := state[call.path["key"]]
	body := goingPick(call.arguments, "pinned", "last_read_seq")
	if items, named := call.arguments["dismissed"].([]any); named && len(items) > 0 {
		dismissed := append([]string{}, stored.Dismissed...)
		for _, item := range items {
			text, isText := item.(string)
			if !isText {
				return goingCall{}, goingUntranslatable(fmt.Sprintf("dismissed holds %T, and takes texts alone", item))
			}
			if !slices.Contains(dismissed, text) {
				dismissed = append(dismissed, text)
			}
		}
		body["dismissed"] = dismissed
	}
	body["seq"] = stored.Seq + 1
	return goingCall{path: call.path, body: body}, nil
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

// goingTranslateEditPost sends an edit of a comment or an issue message: the new body, to the
// post the call names (goingCorrection).
func goingTranslateEditPost(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	return goingCorrection(ctx, read, call, goingPick(call.arguments, "body"))
}

// goingTranslateDeletePost sends a delete of a comment or an issue message, which takes nothing
// but the writer (goingCorrection).
func goingTranslateDeletePost(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	return goingCorrection(ctx, read, call, nil)
}

// goingCorrection is a correction of the post the call names, as the route takes it: the post's
// id in the path, taken out of a dispatch:// reference where the call names one, since the path
// takes an id and no reference, and matched among the posts of the reference's owner where the
// reference writes it short (goingPostID); and in body the session the caller acts as, where it
// acts as one, which is how the route reads a session's correction (correctionActor). A person's
// credential names the person, so a person's call sends body as it is. The post is a message
// where the call names one by its argument message, and a comment otherwise.
func goingCorrection(ctx context.Context, read goingReader, call goingCallInput, body map[string]any) (goingCall, error) {
	kind := "comment"
	if _, named := call.arguments["message"]; named {
		kind = "message"
	}
	id, err := goingPostID(ctx, read, kind, call.path["id"])
	if err != nil {
		return goingCall{}, err
	}
	if call.session != "" {
		if body == nil {
			body = map[string]any{}
		}
		body["actor"] = goingActorSession(call.session)
	}
	return goingCall{path: map[string]string{"id": id}, body: body}, nil
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

// goingTranslateDocEdit sends the edit's operations, its precondition and its summary; the
// document's own row takes its id in its path, and the project row its slug, as the call names it.
func goingTranslateDocEdit(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, body: goingPick(call.arguments, "ops", "precondition", "summary")}, nil
}

// goingTranslateIssueDocEdit is goingTranslateDocEdit on a document of the issue: the one the call
// names by slug or filename, and the issue's spec where it names none, as `dispatch doc-edit`
// edits the primary document by default. The row binds the issue to both placeholders, so the
// slug is read here.
func goingTranslateIssueDocEdit(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	edit, err := goingTranslateDocEdit(ctx, read, call)
	edit.path = map[string]string{"key": call.path["key"], "slug": cmp.Or(goingText(call.arguments, "artifact"), "spec")}
	return edit, err
}

// goingTranslateRequestApproval asks for approval of the document the call names: on the issue it
// names, its spec where it names none; on its project; or by id.
func goingTranslateRequestApproval(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	reference := goingText(call.arguments, "artifact")
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

// goingTranslatePlanApprove records the plan review on the issue's document the call names, its
// plan.md where it names none.
func goingTranslatePlanApprove(ctx context.Context, read goingReader, call goingCallInput) (goingCall, error) {
	id, err := read.issueDocument(ctx, call.path["id"], cmp.Or(goingText(call.arguments, "artifact"), "plan.md"))
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

// goingTranslateDeploy sends the deploy's own fields, each under the name its route reads it by.
// json, which has the `dispatch` command print the route's whole answer, is the command's own and
// no member of the route's body: the row shows what the route answered.
func goingTranslateDeploy(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	return goingCall{path: call.path, body: goingPick(call.arguments, "repo", "program", "commit", "deployed_at", "url")}, nil
}

// goingTranslateUpload uploads the call's own content as a document of the issue or the project,
// as the JSON form of the route takes it (goingUnsendable refused a call naming a file to read it
// from, or no content, before the translation). `force` is no field of the route's: every upload
// is sent as a create alone (goingUploadCondition), so a document of its name the owner holds by
// the time the upload takes the owner's lock - one another writer made after the write's read
// before - is the route's to refuse, 409 DOCUMENT_EXISTS. A row of a run's plan whose approval
// showed the document of its name, and whose read before finds that document with the text the
// approval showed, is sent as its next version on the condition that the document is still at the
// version that read found (goingCondition.ExpectVersion), naming the token that read answered where
// it answered one; a row whose read before finds anything else under the name waits for its person
// again (AgentListener.rowUpload, uploadChanged).
func goingTranslateUpload(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
	body := goingPick(call.arguments, "name", "summary")
	body["content"] = goingText(call.arguments, "content")
	return goingCall{path: call.path, body: body, condition: goingUploadCondition()}, nil
}

// goingUploadCondition is the condition an upload carries to its route on its context
// (going_conditions.go): a create alone (ExpectNew), since whether an upload writes over a
// document is known only once its read before is in. A row of a plan trades it at the write for
// the version its read before found (ExpectVersion), only where its approval showed the document
// or file of its name and that read finds it as the approval showed it (AgentListener.rowUpload).
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
