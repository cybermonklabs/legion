package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/fold"
)

const (
	// maxArtifactBlobSize is the file store's own bound, so an upload this route takes is one
	// the store accepts and serves back.
	maxArtifactBlobSize      = files.MaxObjectSize
	maxDocumentMarkdownBytes = pmdoc.MaxDocumentBytes // A Markdown document, and what a write may grow one to (docs' refuseGrowth); maxJSONRequestBytes bounds an issue's spec and every edit the same way (LEGION-465).
	// maxNewArtifactKeyBytes bounds, in bytes, a new artifact's name and the reference key
	// `<owner>/<slug>` that name gives it (newArtifactNameRefusal, newArtifactKeyRefusal).
	// artifacts_ref_key_key is a B-tree index on ref_key, and a Postgres B-tree entry holds at
	// most 2704 bytes: an 8-byte tuple header, then the value's 4-byte length word and its bytes,
	// so incompressible text of 2692 bytes fits and one byte more fails the insert (SQLSTATE
	// 54000). No index holds the name, but it is held to the same bytes: its slug is the part of
	// the key a caller writes, so one number names what a new artifact may be called. An artifact
	// already stored past it, under a key Postgres compressed into the index, keeps its name and
	// key: an upload naming it stores a version as before.
	maxNewArtifactKeyBytes = 2692
)

// artifactColumns is the canonical artifact column list scanArtifact reads. role is read through
// coalesce because model.Artifact carries it as "" when the document plays no role.
const artifactColumns = `id::text, issue_key, project_key, session_id, ref_key, slug, name, kind, is_primary, coalesce(role, ''), created_by, created_at`

// documentVisible is access.DocumentClause over the artifact whose id idExpr names, for a
// statement that reads a row its document owns - a version, a review, a comment anchored to it -
// rather than the artifact's own row: the row is read only where its reader may read the document.
func documentVisible(idExpr, viewerParam string) string {
	return `exists (select 1 from artifacts visible_document where visible_document.id = ` + idExpr +
		` and ` + access.DocumentClause("visible_document", viewerParam) + `)`
}

func (s *server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	artifacts, err := s.loadArtifacts(r.Context(), s.deps.Store.Pool, who, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	reads := artifactReads(artifacts, reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, reads)
}

type artifactUploadInput struct {
	name    string
	content []byte
	// precondition is the token of the version an upload believes it is replacing, nil when the
	// upload names none. A replacement of a document that already exists names it, and one that
	// names none is taken unchecked and counted (storeArtifact); a replacement of a
	// file or an image names its standing version's token, and one that names none is taken
	// unchecked. A first upload never names one.
	precondition *model.EditPrecondition
	contentType  string
	summary      string
	supplied     *model.Actor
	// blank reports a JSON body that carries no content, or whose content is blank: such a
	// body makes a new document holding nothing, and storeArtifact refuses it where the name
	// already names a document or gives the slug of one Dispatch makes itself. A multipart
	// upload always carries its bytes, so it never sets this. A precondition that is not well
	// formed is answered as malformed first (uploadArtifactFor, ValidateUploadPrecondition);
	// past that, the refusal of a blank write over a document that exists comes ahead of every
	// check of the token against the document, so a blank write is never told to name a token
	// for a write the server would refuse whatever token it named.
	blank bool
}

// artifactTarget is the owner an upload or a slug lookup names: an issue (IssueKey), a project
// (Project alone), or an agent's conversation (Session alone), which holds files and images only.
// Exactly one of the three is set; every reader tests Session first, then IssueKey, and
// agent_artifacts.go holds what a conversation does differently.
type artifactTarget struct {
	IssueKey *string
	Project  string
	Session  string
}

// refPrefix is what the target's artifacts' ref_key holds before `/<slug>`.
func (target artifactTarget) refPrefix() string {
	if target.Session != "" {
		return agentOwnerKey(target.Session)
	}
	if target.IssueKey != nil {
		return *target.IssueKey
	}
	return target.Project
}

type jsonArtifactUpload struct {
	Name string `json:"name" required:"true"`
	// Content is what a JSON upload stores, which is always a markdown document. A body with
	// no content, or whose content is blank, makes a new document holding nothing, which a
	// person then writes in Dispatch. It is refused only where the name, or the slug that name
	// gives, is a document the owner already holds (400 ARTIFACT_INPUT, storeArtifact), or
	// where that slug is one of the documents Dispatch makes itself (400 ARTIFACT_ROLE_NAME).
	// A multipart upload sends its bytes as the `file` part instead and carries no content at
	// all, which is a body of its own (openAPIMultipartSchema).
	//
	// Content of whitespace alone is carried to the store as it was typed, and the store makes
	// it empty: the write parses the markdown into the document and stores what that document
	// renders back (Docs.SeedText, storeArtifact), and whitespace alone is no block, so the
	// version holds an empty markdown of size 0. Such a document is as removable as one made
	// with no content at all, measured on 2026-11-11 through the route itself.
	Content      *string                 `json:"content"`
	Primary      json.RawMessage         `json:"primary"`
	Summary      string                  `json:"summary"`
	Actor        *model.Actor            `json:"actor"`
	Precondition *model.EditPrecondition `json:"precondition"`
}

func (s *server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	issueKey := r.PathValue("key")
	s.uploadArtifactFor(w, r, artifactTarget{IssueKey: &issueKey})
}

// issueUploadRoute is the upload to an issue, which refuseUploadToAClosedIssue guards.
const issueUploadRoute = http.MethodPost + " /api/v1/issues/{key}/artifacts"

// refuseUploadToAClosedIssue answers an upload to an issue that is closed, or missing, with the
// refusal its handler would make (requireOpenOwnerStatus) before anything spools or parses the
// upload's body: the keyed layer spools a keyed body whole to disk before its handler runs
// (readKeyedBody), and the multipart parser spools an unkeyed one whole before the handler reaches
// the issue, so an upload to a closed issue would otherwise be written out in full only to be
// refused. The body is still read off the connection, and discarded, before the refusal is written
// (drainBeforeAnswer), so a client that sends it whole before it reads the answer gets the
// answer. register mounts it ahead of the keyed layer, so the refusal claims no key. The handler's
// locked check still decides an issue that closes meanwhile; this one only spares the spool. A
// caller that resolves to nobody is left to the 401 written further in (the keyed layer's for a
// keyed upload, the handler's for an unkeyed one), and a malformed Idempotency-Key to the keyed
// layer's 400, which spools no body either and refuses the request's form before anything is
// looked up.
//
// The refusal stands in for the handler, so it answers only a request whose handler would run. A
// repeat under a key the keyed layer answers itself (requestKeyAnswersItself) - its first upload's
// stored answer, a first upload still running, one that committed and lost its answer - goes on to
// that layer whatever the issue's state now: an upload that landed before the issue closed is
// replayed its 201, and one still running is told KEY_IN_FLIGHT, unspooled, until the first
// answers.
func (s *server) refuseUploadToAClosedIssue(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestKey, keyed, err := requestKeyHeader(r)
		if keyed && err != nil {
			handler(w, r)
			return
		}
		actor, human, err := s.optionalActor(r)
		if err != nil {
			handler(w, r)
			return
		}
		// The caller is resolved once a request: the key layer and the handler read this.
		r = r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human}))
		// The reader withinProjectScope resolved in front of this check: an issue the caller does
		// not see is refused here as a key no issue holds.
		who, err := s.resolveReader(r)
		if err != nil {
			handler(w, r)
			return
		}
		refusal := s.refuseOwnerBeforeResolving(r.Context(), who, issueOwner(r.PathValue("key")), refusedWhenClosed)
		if refusal == nil {
			handler(w, r)
			return
		}
		// Only the issue's own refusal (closed, missing) stands aside for the key: a check that
		// failed is answered as it always was.
		var ownerRefusal *apiError
		if keyed && errors.As(refusal, &ownerRefusal) && ownerRefusal.status < http.StatusInternalServerError {
			answers, err := s.requestKeyAnswersItself(r.Context(), requestKeyLogin(actor, human), requestKey)
			if err != nil {
				// As the keyed layer answers its own failed lookup: under the key, which nothing has
				// claimed, so the client asks again under it.
				w.Header().Set(contracts.IdempotencyKeyHeader, requestKey)
				s.writeHandlerError(w, err)
				return
			}
			if answers {
				handler(w, r)
				return
			}
		}
		s.writeHandlerError(w, refusal)
	}
}

// artifactUploadResponse is the body POST /api/v1/issues/{key}/artifacts,
// POST /api/v1/projects/{key}/artifacts and POST /api/v1/agents/{session_id}/artifacts answer.
type artifactUploadResponse struct {
	Artifact model.Artifact `json:"artifact"`
	Version  model.Version  `json:"version"`
}

func (s *server) uploadArtifactFor(w http.ResponseWriter, r *http.Request, target artifactTarget) {
	r.Body = http.MaxBytesReader(w, r.Body, maxArtifactBlobSize+(1<<20))
	requestType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "invalid artifact input type")
		return
	}

	var input artifactUploadInput
	var ok bool
	switch requestType {
	case "application/json":
		input, ok = s.jsonArtifactUpload(w, r)
	case "multipart/form-data":
		input, ok = s.multipartArtifactUpload(w, r)
	default:
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "artifact input must be JSON or multipart")
		return
	}
	if !ok {
		return
	}
	actor, ok := s.requireActor(w, r, input.supplied)
	if !ok {
		return
	}
	// The reader the owner's project is read as (loadProject), resolved before the upload's
	// transaction opens, as every resolution of a viewer is (access.Resolve takes a connection).
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	if input.name == "" {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact name is required")
		return
	}
	if len(input.content) > maxArtifactBlobSize {
		s.writeHandlerError(w, errUploadTooLarge)
		return
	}
	mediaType, _, err := mime.ParseMediaType(input.contentType)
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "invalid artifact content type")
		return
	}
	kind, storedType := artifactKind(mediaType, input.contentType, input.content)
	if kind == "doc" && target.Session != "" {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, agentOwnsFilesOnly)
		return
	}
	if kind == "doc" && len(input.content) > maxDocumentMarkdownBytes {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "a markdown document is at most 1 MiB; a larger file is stored as a binary artifact under another content type")
		return
	}
	// A markdown document is stored as text. Its inline content was read as a JSON string, which
	// holds no byte that is not UTF-8 and was read for U+0000, so only a multipart file reaches here
	// holding either. Any other file is stored as bytes, beside its part's Content-Type as text; the
	// document path stores no part header.
	if kind == "doc" && !text.StorableBytes(input.content) {
		s.writeHandlerError(w, unstorableText("file", string(input.content)))
		return
	}
	if kind != "doc" {
		if refusal := unstorableText("file Content-Type", input.contentType); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
	}
	// The token an upload names is held to what an upload can mean before anything is written:
	// the token of the version it replaces alone, in the shape every edit's precondition takes.
	// A document's is its live tree's, which the document write checks; a file's or an image's is
	// its standing version's (model.FileVersionToken), which storeArtifact checks. A token nothing
	// would read is refused rather than taken and dropped, because the caller believes its write
	// was conditional. A replacement that names none is taken unchecked, as it was before #3000,
	// and a document's is counted (storeArtifact).
	if input.precondition != nil {
		if refusal := docs.ValidateUploadPrecondition(*input.precondition); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
	}
	input.contentType = storedType
	s.storeArtifact(w, r, who, input, actor, kind, target)
}

// jsonArtifactUpload reads an inline upload's JSON body. As decodeJSON does, it ignores a member
// jsonArtifactUpload does not declare and logs it at debug with the route; it keeps its own size
// refusal for the artifact cap.
func (s *server) jsonArtifactUpload(w http.ResponseWriter, r *http.Request) (artifactUploadInput, bool) {
	var body jsonArtifactUpload
	var read bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(r.Body, &read))
	if err := decoder.Decode(&body); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			s.writeHandlerError(w, errUploadTooLarge)
		} else {
			writeError(w, "INVALID_JSON", http.StatusBadRequest, "invalid JSON body")
		}
		return artifactUploadInput{}, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, "INVALID_JSON", http.StatusBadRequest, "request body must contain one JSON value")
		return artifactUploadInput{}, false
	}
	if refusal := unstorableJSON("", read.Bytes()); refusal != nil {
		s.writeHandlerError(w, refusal)
		return artifactUploadInput{}, false
	}
	logUndeclaredMembers(r, &body, read.Bytes())
	// `primary` stays declared, though no upload may set it, so that it is refused by name rather
	// than ignored: an upload never makes a document the issue's primary one.
	if body.Primary != nil {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "primary is fixed at issue creation")
		return artifactUploadInput{}, false
	}
	blank := body.Content == nil || strings.TrimSpace(*body.Content) == ""
	var content []byte
	if body.Content != nil {
		content = []byte(*body.Content)
	}
	return artifactUploadInput{
		name:         strings.TrimSpace(body.Name),
		content:      content,
		precondition: body.Precondition,
		contentType:  "text/markdown; charset=utf-8",
		summary:      strings.TrimSpace(body.Summary),
		supplied:     body.Actor,
		blank:        blank,
	}, true
}

// multipartJSONField reads one JSON object a multipart upload sends as a form field - its
// `actor`, its `precondition` - as the JSON body's decoder reads the same object: a member no
// field of the type names is ignored and logged at debug with the route, as `decodeJSON` and
// `jsonArtifactUpload` do it, so one build of a fleet takes an upload another build's member
// rides in (#2461). What a misspelled key cannot do is pass as the key it misspells: a
// precondition left naming nothing is refused `400 INVALID_PRECONDITION` by its own shape check
// (`docs.ValidateUploadPrecondition`), and an actor naming no kind is refused when the caller is
// a bearer that proves no session (`bearerSessionActor`), the one caller the actor names.
// A field holding more than one JSON value is refused, and a field the form does not carry, or
// carries empty, is no field at all and answers nil. Every refusal is written here, naming the
// field, so a caller reading false returns. A field is logged only once it passed those checks,
// as the JSON body is.
func multipartJSONField[T any](s *server, w http.ResponseWriter, r *http.Request, field string) (*T, bool) {
	raw := r.FormValue(field)
	if raw == "" {
		return nil, true
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var read T
	if err := decoder.Decode(&read); err != nil {
		writeError(w, "INVALID_JSON", http.StatusBadRequest, "invalid multipart "+field)
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, "INVALID_JSON", http.StatusBadRequest, "multipart "+field+" must contain one JSON value")
		return nil, false
	}
	if refusal := unstorableJSON(field, []byte(raw)); refusal != nil {
		s.writeHandlerError(w, refusal)
		return nil, false
	}
	logUndeclaredMembers(r, &read, []byte(raw))
	return &read, true
}

func (s *server) multipartArtifactUpload(w http.ResponseWriter, r *http.Request) (artifactUploadInput, bool) {
	// Only the size limits are 413: the body past maxArtifactBlobSize and a megabyte of fields
	// (uploadArtifactFor's MaxBytesReader), or fields past what the parser holds in memory. A file
	// the server cannot spool to its temporary directory is the server's failure, 500. Any other
	// refusal is a body the parser cannot read, answered with its reason.
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		var spool *fs.PathError
		switch {
		case errors.As(err, &tooLarge):
			s.writeHandlerError(w, errUploadTooLarge)
		case errors.Is(err, multipart.ErrMessageTooLarge):
			writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, "the upload's form fields are too large to read")
		case errors.As(err, &spool):
			s.writeHandlerError(w, fmt.Errorf("spool multipart upload: %w", err))
		default:
			writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "invalid multipart body: "+err.Error())
		}
		return artifactUploadInput{}, false
	}
	// The parser spools a file part past a megabyte to a temporary file. net/http removes the
	// files of the request it handed its handler, but this is a copy (the access log and the
	// transaction tracking each wrap it with WithContext), so the server never sees them; left
	// alone, every such upload kept its file on the pod's disk until the pod's ephemeral storage
	// ran out. The content is read into memory below, so the files go when this returns, stored
	// or refused.
	defer func() {
		if err := r.MultipartForm.RemoveAll(); err != nil {
			slog.Error("dispatch: remove a multipart upload's spooled files", "error", err)
		}
	}()
	if refusal := unstorableForm(r.MultipartForm.Value); refusal != nil {
		s.writeHandlerError(w, refusal)
		return artifactUploadInput{}, false
	}
	supplied, read := multipartJSONField[model.Actor](s, w, r, "actor")
	if !read {
		return artifactUploadInput{}, false
	}
	if _, exists := r.MultipartForm.Value["primary"]; exists {
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "primary is fixed at issue creation")
		return artifactUploadInput{}, false
	}
	// A multipart upload names the token of the version it replaces in the form field
	// `precondition`, holding the same JSON object the JSON body's `precondition` holds.
	precondition, read := multipartJSONField[model.EditPrecondition](s, w, r, "precondition")
	if !read {
		return artifactUploadInput{}, false
	}
	name := strings.TrimSpace(r.FormValue("name"))
	file, header, err := r.FormFile("file")
	if len(r.MultipartForm.Value["content"]) > 0 {
		if err == nil {
			_ = file.Close()
		}
		writeError(w, "ARTIFACT_INPUT", http.StatusBadRequest, "provide either a file or inline JSON content")
		return artifactUploadInput{}, false
	}
	if err != nil {
		writeError(w, "INVALID_ARTIFACT", http.StatusBadRequest, "artifact file is required")
		return artifactUploadInput{}, false
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxArtifactBlobSize+1))
	if err != nil {
		s.writeHandlerError(w, err)
		return artifactUploadInput{}, false
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(name))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return artifactUploadInput{
		name:         name,
		content:      content,
		precondition: precondition,
		contentType:  contentType,
		summary:      strings.TrimSpace(r.FormValue("summary")),
		supplied:     supplied,
	}, true
}

// uploadAdvice tells the reader of an upload refused for a token the artifact has moved past,
// `what` naming its kind (document, file or picture), where the token of the version it is
// replacing is printed. It names no flag of a command: a caller that named a token may have sent
// it straight to this route, from a client whose `dispatch artifact` has no flag for one
// (INFRA-4980), so it names the field the token goes in.
func uploadAdvice(what string) string {
	return "read the " + what + " with `dispatch doc-read`, which prints " +
		"`Document token: sha256:…`, and upload again naming that token as the upload's " +
		"precondition, `{\"document\":\"sha256:…\"}`"
}

// writeUploadDocumentError answers an upload whose document write was refused, which is where
// this route's words meet the document service's: the service reports which tree a token missed
// and knows neither the version rows nor a command its reader runs, so the number of the version
// the client is behind and the sentence saying where a current token is printed are added here.
// That number is read here, in the upload's read phase, once the write is refused and while tx
// still holds the lock the write read the document's tree under, so it is the version standing
// at the tree the token missed; the refusal is a read and writes nothing. It is read only for a
// token the document moved past, the one refusal that names it, so a write refused for any other
// reason is reported as it was and nothing more is asked of tx.
//
// A repair - a document whose live tree is outside the Proof schema - has no token for the
// upload to name, and is refused by writeUploadNoTokenError below, as a first upload naming a
// token is (storeArtifact).
// who is the writer as a read binds it (readerOf): the version number is read as who reads the
// document, which the write has already shown who may see.
func (s *server) writeUploadDocumentError(ctx context.Context, w http.ResponseWriter, tx pgx.Tx, who model.Actor, err error, name, artifactID string) {
	var stale *docs.ErrPreconditionFailed
	if errors.As(err, &stale) {
		version, readErr := settledVersionNumber(ctx, tx, who, artifactID)
		if readErr != nil {
			s.writeHandlerError(w, readErr)
			return
		}
		WriteJSON(w, http.StatusConflict, uploadPreconditionBody(stale, version))
		return
	}
	if errors.Is(err, docs.ErrPreconditionUnavailable) {
		s.writeUploadNoTokenError(w, name)
		return
	}
	s.writeHandlerError(w, err)
}

// uploadPreconditionBody is the 409 PRECONDITION_FAILED body of a document's upload naming the
// token of a version that no longer stands: the refusal's own body (editPreconditionBody), the
// number of the version that stands, and the sentence saying where its token is printed. A file's
// is fileUploadPreconditionBody.
func uploadPreconditionBody(stale *docs.ErrPreconditionFailed, version int) editPreconditionError {
	body := editPreconditionBody(stale)
	body.Current.Version = &version
	body.Error += "; " + uploadAdvice("document")
	return body
}

// fileUploadPreconditionBody is the 409 PRECONDITION_FAILED body of a file's or a picture's upload
// naming a token that is not its standing version's: the shape a document's refusal answers
// (preconditionBody), holding the standing token in current.document and the one mismatch scoped
// to it, with the standing version's number, in words that name the artifact's kind rather than
// a document.
func fileUploadPreconditionBody(kind, name, named string, standing model.Version) editPreconditionError {
	what := "file"
	if kind == "image" {
		what = "picture"
	}
	token := *standing.Token
	body := preconditionBody(token, []model.EditPreconditionMismatch{{Scope: "document", Expected: named, Current: &token}},
		fmt.Sprintf("%s upload precondition failed: %q stands at version %d, whose token is not the one this upload names; %s",
			what, name, standing.Number, uploadAdvice(what)))
	body.Current.Version = &standing.Number
	return body
}

// refuseFileBase answers an upload of a file or an image of the given kind that names base, the
// token of the version it believes it replaces, when base is not standing's token, standing being
// the artifact's newest version, and reports whether it answered. A base naming any other token
// is refused 409 PRECONDITION_FAILED with the standing version's token and number, as a
// document's is; a version stored with no hash has no token for a base to name, and is refused as
// a first upload naming a token is (writeUploadNoTokenError). An upload naming no base is never
// refused here, a first upload among them: one naming a token is refused before it reaches here,
// by storeArtifact and, before its object, by refuseUploadBeforeItsObject, so a first upload
// arrives naming none and passes. It runs twice, the same check each time:
// before the upload's object is written, from the pool (putUploadedFile, its version read by
// newestFileVersion), so an upload refused there leaves no object, and in storeArtifact's
// transaction under the owner's lock, which decides, its version the one that transaction loaded
// with the artifact. An upload the first passed and the second refuses, because another upload
// replaced the version between them, leaves an object no row names, as putUploadedFile's other
// refusals can.
func (s *server) refuseFileBase(w http.ResponseWriter, kind, name string, base *model.EditPrecondition, standing model.Version) bool {
	if base == nil {
		return false
	}
	if standing.Token == nil {
		s.writeUploadNoTokenError(w, name)
		return true
	}
	if base.Document == *standing.Token {
		return false
	}
	WriteJSON(w, http.StatusConflict, fileUploadPreconditionBody(kind, name, base.Document, standing))
	return true
}

// uploadNoTokenRefusal refuses an upload that names a token where the artifact has none for it
// to name, with no live token beside it: a first upload under a name the owner holds nothing
// under names a version that is not there (storeArtifact, the one statement of that rule), a
// repair's live tree renders no markdown and has no token at all, and a file's version stored
// with no hash has none. No reader can be sent to `dispatch doc-read` for a current token,
// because nothing can print one, so each is told to send the upload again naming none, which is
// taken: it creates the artifact the owner holds nothing under, replaces the repaired document
// whole, or replaces the file unchecked.
func uploadNoTokenRefusal(name string) error {
	return errorf(http.StatusConflict, "PRECONDITION_FAILED",
		"%q has no token for an upload to name: send it again naming no token", name)
}

// writeUploadNoTokenError answers uploadNoTokenRefusal.
func (s *server) writeUploadNoTokenError(w http.ResponseWriter, name string) {
	s.writeHandlerError(w, uploadNoTokenRefusal(name))
}

// queueNewArtifact queues the insert of the artifact an upload creates and returns the row as the
// insert will leave it. Its id, which the document's own writes name before the row is written, is
// taken here (gen_random_uuid, which assigns no transaction id), in the statement that reads its
// created_at, the column's default, the transaction's now(), which the insert writes explicitly;
// its ref_key is the one the 0071 trigger fills in, the target's owner key and the slug
// (artifactTarget.refPrefix). role is the part the new document plays, which its slug decided
// (documentRole), read once by the caller, which refuses a blank create that takes one: the row
// the insert leaves carries it, so the reads this transaction makes of the artifact it just
// created see it as every later reader will.
func (s *server) queueNewArtifact(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	target artifactTarget,
	project, session *string,
	slug, name, kind string,
	role *string,
	actor model.Actor,
) (model.Artifact, error) {
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		return model.Artifact{}, err
	}
	artifact := model.Artifact{
		IssueKey: target.IssueKey, Project: deref(project), SessionID: session,
		RefKey: target.refPrefix() + "/" + slug, Slug: slug, Name: name, Kind: kind, Role: deref(role),
	}
	if err := json.Unmarshal(actorJSON, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode artifact author: %w", err)
	}
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, now()`).Scan(&artifact.ID, &artifact.CreatedAt); err != nil {
		return model.Artifact{}, err
	}
	// The document work that follows runs before the insert below, so the address that insert
	// gives the row - its key, the issue holding it, none for a project's or a session's
	// document, and its own dispatch:// address - is recorded for the reads of this
	// transaction that need it: a block pointer of a new document is held to the document's key
	// and its issue (docs.addressOfDocument), and text this transaction indexes that cites the
	// new document resolves to that row (refs.Pending). The issue already has its row, so the
	// address names no pending parent (refs.DocumentAddress).
	ledger.RegisterPendingArtifact(artifact.ID, refs.DocumentAddress{
		RefKey: artifact.RefKey, IssueKey: deref(target.IssueKey), Ref: artifactReference(artifact),
	})
	return artifact, queueExec(ledger, `
		insert into artifacts (id, issue_key, project_key, session_id, slug, name, kind, is_primary, role, created_by, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, false, $8, $9, $10)
	`, artifact.ID, target.IssueKey, project, session, slug, name, kind, role, actorJSON, artifact.CreatedAt)
}

// queueFileVersion queues the insert of an uploaded file's or image's version and returns the row
// as the insert will leave it: numbered and dated by the ledger (docs.Ledger.NextVersion), with
// its uploader as its one author. The bytes are written to the row only where no file store holds
// them (putUploadedFile), or while a live server process has no bucket and so serves a file only
// from its row (fleetServesFilesFromRows, read here in the read phase through db).
func (s *server) queueFileVersion(
	ctx context.Context,
	db processes.Querier,
	ledger *docs.Ledger,
	artifactID string,
	input artifactUploadInput,
	sha string,
	summary *string,
	actor model.Actor,
) (model.Version, error) {
	authors, err := encodeJSON([]model.Actor{actor})
	if err != nil {
		return model.Version{}, err
	}
	number, createdAt, err := ledger.NextVersion(ctx, artifactID)
	if err != nil {
		return model.Version{}, err
	}
	size := len(input.content)
	token := model.FileVersionToken(sha)
	version := model.Version{
		Number: number, Named: summary != nil, Summary: summary, CreatedAt: createdAt,
		Size: &size, MIME: &input.contentType, SHA256: &sha, Token: &token,
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, err
	}
	content := input.content
	if s.deps.Files != nil {
		keep, err := fleetServesFilesFromRows(ctx, db)
		if err != nil {
			return model.Version{}, err
		}
		if !keep {
			content = nil
		}
	}
	return version, queueExec(ledger, `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, authors, named, summary, created_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, artifactID, number, content, input.contentType, size, sha, authors, version.Named, summary, createdAt)
}

// newestFileVersion is the newest version of the file or image artifactID of the given kind, with
// its token (model.StateFileTokens), for the check putUploadedFile makes from the pool, where the
// lookup that found the artifact read no versions. An artifact holding no version answers a
// version with no token. The check that decides runs in storeArtifact's transaction on the
// versions that transaction loaded with the artifact under the owner's lock, and reads nothing
// more.
func newestFileVersion(ctx context.Context, q queryer, kind, artifactID string) (model.Version, error) {
	version, err := scanVersion(q.QueryRow(ctx, `
		select `+versionColumns+`
		from artifact_versions v where v.artifact_id = $1 order by v.number desc limit 1
	`, artifactID))
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Version{}, nil
	}
	if err != nil {
		return model.Version{}, fmt.Errorf("read the newest version of a file: %w", err)
	}
	versions := []model.Version{version}
	model.StateFileTokens(kind, versions)
	return versions[0], nil
}

// recordDocumentTaskProgress records the task count of the document a write just stored.
// readBack is the rendering that write stored, read back by its own parse (docs'
// Ledger.ReadBackOf): the count reads that tree rather than parsing the same markdown again, and
// parses markdown itself when the write left none.
func recordDocumentTaskProgress(ctx context.Context, tx pgx.Tx, artifactID string, readBack *pmdoc.Node, markdown string) error {
	if readBack != nil {
		return docs.RecordTaskProgress(ctx, tx, artifactID, readBack)
	}
	return docs.RecordTaskProgressMarkdown(ctx, tx, artifactID, markdown)
}

func (s *server) storeArtifact(
	w http.ResponseWriter,
	r *http.Request,
	who model.Actor,
	input artifactUploadInput,
	actor model.Actor,
	kind string,
	target artifactTarget,
) {
	checksum := sha256.Sum256(input.content)
	sha := hex.EncodeToString(checksum[:])
	// A file's object is written before the transaction opens, once the refusals the transaction
	// would make have been made (putUploadedFile, artifact_files.go).
	if !s.putUploadedFile(w, r, who, kind, sha, input, target) {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The upload runs in two phases (write_queue.go): every lock, read and the document work run
	// here, holding no transaction id, and each write - the artifact, the document's, the version,
	// the event - is queued at the point it is made, for Commit to run back to back.
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	var issueStatus *string
	var project, session *string
	switch {
	case target.Session != "":
		session = &target.Session
		if err := lockAgentArtifacts(r.Context(), tx, target.Session); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	case target.IssueKey != nil:
		issueStatus, err = s.requireOpenOwnerStatus(r.Context(), tx, who, issueOwner(*target.IssueKey))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := tx.QueryRow(r.Context(), `select project_key from issues where key = $1`, *target.IssueKey).Scan(&project); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	default:
		loaded, err := s.loadProject(r.Context(), tx, who, target.Project)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		project = &loaded.Key
		if err := lockProjectArtifacts(r.Context(), tx, loaded.Key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var artifact model.Artifact
	var created bool
	artifact, err = s.findArtifactByNameOrSlug(r.Context(), tx, target, input.name)
	if errors.Is(err, pgx.ErrNoRows) {
		created = true
		if refusal := newArtifactNameRefusal(input.name); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
		slug, err := s.nextArtifactSlug(r.Context(), tx, target, input.name)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if refusal := newArtifactKeyRefusal(target, slug); refusal != nil {
			s.writeHandlerError(w, refusal)
			return
		}
		// The role the insert below writes, which the minted slug decides. A create with no
		// content takes none of the documents Dispatch makes itself, so every spelling of the
		// name that gives one of their slugs is refused.
		role := documentRole(target, kind, slug)
		if input.blank && role != nil {
			s.writeHandlerError(w, createRoleNameRefusal(input.name, slug, *role))
			return
		}
		artifact, err = s.queueNewArtifact(r.Context(), tx, ledger, target, project, session, slug, input.name, kind, role, actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else if err != nil {
		s.writeHandlerError(w, err)
		return
	} else if artifact.Kind != kind {
		s.writeHandlerError(w, artifactKindMismatch(input.name, artifact, kind))
		return
	} else if input.blank {
		s.writeHandlerError(w, blankWriteToADocumentThatExists(input.name, artifact))
		return
	}
	// A document the release before this one created during the rolling deploy carries no role:
	// migration 20261102110000 backfills the rows that existed when it ran, and that release
	// writes none of its own. Until the role is set the document is invisible to every reader
	// that looks for an issue's Verification or plan document by it, so the next version written
	// under the row repairs it from the same slug that would have set it. The slug is unique
	// within the owner, so at most one row per issue can take either role and the partial unique
	// index artifacts_issue_role holds. The repair is a write like every other this route makes:
	// it is queued here and runs in the write phase, and the row in hand carries the role from
	// here on, so the reads that follow it in this transaction read the role it writes.
	if !created && artifact.Role == "" {
		if role := documentRole(target, artifact.Kind, artifact.Slug); role != nil {
			if err := queueExec(ledger, `
				update artifacts set role = $2 where id = $1 and role is null
			`, artifact.ID, *role); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			artifact.Role = *role
		}
	}
	// A project artifact's owner is the artifact itself, and until here this transaction has not
	// taken its lock: the issue branch above locks its issue, but this one only read its project.
	// For a document the document service takes that lock itself, before the room, when the write
	// below joins the document (docs' joinLiveWrite, lockArtifactOwner), so it is not this line that
	// keeps the upload from running room -> owner against a settlement's owner -> room. A file is
	// written by this route alone, with no document service call, and this line is the one lock it
	// takes before its version's insert; without it the upload would meet the owner's lock only in
	// its event's append, after that insert, holding a transaction id.
	if target.IssueKey == nil && target.Session == "" && !created {
		if err := s.requireOpenOwner(r.Context(), tx, who, ownerForArtifact(artifact)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	var version model.Version
	var documentSummary *string
	if input.summary != "" {
		documentSummary = &input.summary
	}
	var documentMarkdown string
	// documentReadBack is the rendering this write stored, read back by its own parse (docs'
	// Ledger.ReadBackOf), which the task count and the advice's blocks read in place of parsing
	// the same markdown again (recordDocumentTaskProgress, documentBlocksFor).
	var documentReadBack *pmdoc.Node
	var documentChanges model.ReferenceChanges
	// indexedBlocks is what the block index stated in the node and tuple store, which waits for
	// the event this upload queues below: that event is the cause every node and tuple of the
	// pass carries, and no tuple may be updated once appended.
	var indexedBlocks graphstore.Uncaused
	// A first upload under a name the owner holds nothing under replaces nothing, so a token names
	// a version that is not there: the caller read something this owner does not hold, or another
	// writer has taken the name since. The caller believed its write was conditional, so it is
	// refused rather than taken, as a token is where the artifact has none (uploadNoTokenRefusal).
	// This is the one statement of that rule, a document's and a file's alike; a file's upload is
	// held to it before its object is written too (refuseUploadBeforeItsObject).
	if created && input.precondition != nil {
		s.writeUploadNoTokenError(w, input.name)
		return
	}
	if kind == "doc" {
		if created {
			documentMarkdown, err = s.deps.Docs.SeedText(documentCtx, artifact.ID, string(input.content), actor)
		} else {
			// An upload over a document that already exists replaces every word of it, so it
			// names the token of the version it is replacing, which the write checks on the tree
			// it reads under its own lock. One that names none is taken unchecked, as it was
			// before #3000, and counted once it commits (noteUnguardedReplacement): the refusal
			// of a missing token comes back once every deployed client sends one (INFRA-4980).
			// A repair - a live tree outside the Proof schema, which has no token for anyone to
			// name - names none by design.
			documentMarkdown, err = s.deps.Docs.ReplaceText(documentCtx, artifact.ID, string(input.content), actor, input.precondition)
		}
		if err != nil {
			// The document service knows the tree a token missed and nothing of this route's
			// own; the refusal the client reads is worded where the two meet. Nothing is
			// written: every write of this upload was queued, and Commit never runs.
			s.writeUploadDocumentError(r.Context(), w, tx, who, err, input.name, artifact.ID)
			return
		}
		version, err = ledger.QueueVersion(r.Context(), artifact.ID, documentMarkdown, []model.Actor{actor}, documentSummary)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// The version credits the uploader alone, and holds the upload's write to the document,
		// which clears the credit of every edit that write read when it changed the document. It
		// carries the markdown it stored, whose display tree the commit stores
		// (Ledger.storeVersionTrees), since this route writes the row itself.
		ledger.WroteVersion(artifact.ID, version, documentMarkdown)
		documentReadBack = ledger.ReadBackOf(artifact.ID, documentMarkdown)
		// The count is queued after the version's insert, so it names that version (LEGION-542).
		if err := recordDocumentTaskProgress(documentCtx, tx, artifact.ID, documentReadBack, documentMarkdown); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		documentChanges, indexedBlocks, err = s.deps.Docs.IndexDocumentReferences(documentCtx, artifact.ID, version.Number, documentMarkdown)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		// An upload over a file or an image that already exists names the token of the version
		// it is replacing, as a document's does, checked here under the owner's lock against the
		// newest of the versions this transaction loaded with the artifact under that lock (none,
		// for a row with no version, has no token); one that names none is taken unchecked and
		// counted once it commits, as a document's is. A first upload reaches here naming none,
		// since one naming a token was refused above, so refuseFileBase passes it.
		var standing model.Version
		if count := len(artifact.Versions); count > 0 {
			standing = artifact.Versions[count-1]
		}
		if s.refuseFileBase(w, kind, input.name, input.precondition, standing) {
			return
		}
		version, err = s.queueFileVersion(r.Context(), tx, ledger, artifact.ID, input, sha, documentSummary, actor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var diff *string
	if !created && kind == "doc" {
		// This route writes its version itself rather than through the document service, so its
		// open approval request follows that version just as every other version write does. An
		// upload is no settlement's version, so the request moves to its agent whatever text it
		// stores (docs.MoveApprovalAsk). The move's writes and its event are queued after the
		// version's; Commit publishes the event before this route publishes its own.
		if err := docs.MoveApprovalAsk(
			documentCtx, tx, s.deps.Events, artifact.ID, version, documentMarkdown, actor, false, s.deps.Origins,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// It carries the document's newest approval when its text is the approved text again and
		// its decision blocks are as their rows have them, as every version the document service
		// writes does (docs.carryApproval): an exact undo uploaded is the approved document. It
		// runs where the route's other document work runs, so the open approval request and the
		// ask rows it reads are as this transaction's own queued writes leave them, its own
		// writes join the queue, and its events reach the ledger beside the move's.
		if err := docs.CarryRestoredApproval(
			documentCtx, tx, s.deps.Events, artifact.ID, version.Number, documentMarkdown, documentReadBack, s.deps.Origins,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		diff, err = s.namedVersionDiff(r.Context(), tx, ledger, artifact.ID, version)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// An uploaded document's body cites nodes whose rows carry a backlink count, exactly as a
	// message or comment body does, so both payload shapes name what the upload moved.
	eventType := "artifact.version"
	payload := docs.ArtifactVersionEventPayload(artifact.ID, artifact.Name, version, diff, documentChanges)
	if created {
		eventType = "artifact.created"
		artifact.Versions = []model.Version{version}
		payload = artifactCreatedEventPayload(artifact, documentChanges)
	}
	var published []*model.Event
	// The cause the block index's facts carry: the event this upload queues, named as the id
	// sequence left it right after its insert, or the sentinel where its owner records none of
	// its own (an agent's workspace document).
	blockCause := graphstore.NoEvent
	if target.ownsEvents() {
		stampKind := ""
		if kind == "doc" {
			stampKind = "artifact"
		}
		event, err := s.queueStampedEvent(ledger, ownerForArtifact(artifact).event(eventType, actor, payload), stampKind, artifact.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		blockCause = graphstore.LatestEvent
		published = append(published, event)
	}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, indexedBlocks, blockCause)
		return nil
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The approval this upload's own version superseded, read in its read phase as the edit route
	// reads it: an upload to an existing document always writes a version, so a replacement of an
	// approved spec closes its design gate as an edit does, unless the restore above carried the
	// approval to it. A new document has no review of its own to supersede.
	var superseded *supersededApproval
	var moved *movedApprovalRequest
	if kind == "doc" && !created {
		// This handler serves the project upload as well (uploadProjectArtifact), and a document
		// upload is an issue's or a project's, so the route both reads below name is the one the
		// request came in on: a read that failed is logged where it ran.
		route := "POST /api/v1/projects/{key}/artifacts"
		if target.IssueKey != nil {
			route = "POST /api/v1/issues/{key}/artifacts"
		}
		superseded = s.supersededApprovalAdvice(
			r.Context(), tx, ledger, who, route, artifactIssueKey(artifact), artifact.ID, version.Number,
		)
		// The open approval request this upload's own version took back from the person it was
		// waiting on, read off the ledger the move above and the restore's carry are queued on,
		// so a carry that answered the request reports nothing. A new document carries no
		// request either.
		moved = s.movedApprovalRequestAdvice(
			r.Context(), tx, ledger, route, artifactIssueKey(artifact), artifact.ID,
		)
	}
	var advice *writeAdvice
	if target.IssueKey != nil && issueStatus != nil {
		advice = s.writeAdvice(
			r.Context(), tx, "POST /api/v1/issues/{key}/artifacts", *target.IssueKey, actor, adviceAsks{}, *issueStatus, "", false,
		)
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !created && input.precondition == nil {
		if kind == "doc" {
			s.noteUnguardedReplacement(artifact)
		} else {
			s.noteUnguardedFileReplacement(artifact)
		}
	}
	if kind == "doc" {
		// The seeded or replaced text was written inside this transaction, which suppresses
		// the live settlement the edits path relies on; queue the closer now so the
		// document's ask blocks are indexed and its block ids repaired.
		s.deps.Docs.ScheduleSettlement(artifact.ID)
	}
	s.publish(r.Context(), appendedEvents(published...)...)
	var blocks *documentBlocks
	if kind == "doc" {
		blocks = documentBlocksFor(documentReadBack, documentMarkdown)
	}
	if advice != nil {
		advice.documentBlocks = blocks
		advice.SupersededApproval = superseded
		advice.MovedApprovalRequest = moved
	}
	responsePayload := artifactUploadResponse{Artifact: artifact, Version: version}
	// An issue document whose bounded issue-advice read failed leaves advice nil, and the three
	// document reports with nothing to ride on: they are about the document alone, so they go
	// out on their own shape here rather than being dropped with the issue state that failed,
	// which is what withAdvice would do with a nil *writeAdvice. The edit route answers the
	// same way. A write with nothing at all to report still answers the bare payload, so no
	// reader is handed an empty advice envelope.
	if target.IssueKey != nil && advice != nil {
		WriteJSON(w, http.StatusCreated, withAdvice(responsePayload, advice))
	} else if blocks != nil || superseded != nil || moved != nil {
		WriteJSON(w, http.StatusCreated, withRawAdvice(responsePayload, documentWriteAdvice{
			documentBlocks: blocks, SupersededApproval: superseded, MovedApprovalRequest: moved,
		}))
	} else {
		WriteJSON(w, http.StatusCreated, responsePayload)
	}
}

func (s *server) getArtifact(w http.ResponseWriter, r *http.Request) {
	who, ok := s.artifactReaderOf(w, r)
	if !ok {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The artifact detail is the artifact. Its inbound edges are the graph's to read
	// (`GET /api/v1/references?to=`, and `GET /api/v1/artifacts/{id}/references` for agents),
	// so this read does not pay for them.
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	read := artifactRead{Artifact: artifact}
	read.shape(reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, read)
}

// artifactText is the body GET /api/v1/artifacts/{id}/text,
// GET /api/v1/issues/{key}/artifacts/{slug}/text and
// GET /api/v1/projects/{key}/artifacts/{slug}/text answer. Blocks is absent unless the request
// asked for it with ?blocks=true.
type artifactText struct {
	Markdown string `json:"markdown"`
	// MarkdownTree is the ProseMirror tree Markdown is shown as, present only for a caller that
	// sent `Dispatch-Trees: 1`.
	MarkdownTree model.DisplayTree      `json:"markdown_tree,omitempty"`
	Token        string                 `json:"token"`
	Version      *int                   `json:"version"`
	Blocks       *[]model.ArtifactBlock `json:"blocks,omitempty"`
}

func (s *server) getArtifactText(w http.ResponseWriter, r *http.Request) {
	who, ok := s.artifactReaderOf(w, r)
	if !ok {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	markdown, token, blocks, err := s.artifactTextBody(r, who, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The markdown and the version are two unsynchronised reads, in both directions: the live
	// markdown can be ahead of the version while browser text is unsettled, and a version minted
	// between the two reads can be ahead of the markdown. `token` is the route's concurrency
	// primitive; `version` says which version the document has, not which one this markdown is.
	version, err := latestVersionNumber(r.Context(), s.deps.Store.Pool, who, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	answer := artifactText{Markdown: markdown, Token: token, Version: version, Blocks: blocks}
	// The tree is built only for a caller that asked for one: the command line and the agent
	// clients read this route for its markdown, and a tree they discard would cost them the
	// read and evict from the cache what a browser is reading.
	//
	// This markdown is a document version's own text, which the ledger stored the tree of behind
	// the commit that wrote it (docs.Ledger.storeVersionTrees), so a browser opening a document
	// this process has not served since it started copies those bytes.
	if wantsDisplayTrees(r) {
		reader := s.readDisplayTrees(r.Context(), true)
		reader.text(&answer.MarkdownTree, markdown)
		reader.resolve()
	}
	WriteJSON(w, http.StatusOK, answer)
}

// artifactTextBody is the markdown and the token the three text routes answer, with the blocks
// beside them for a request that asked for them with ?blocks=true. The three come from one
// rendering of one tree (docs.TextWithTokenAndBlocks), so a write by another session cannot leave
// a listing's byte ranges indexing markdown they were not computed over; nil blocks for a request
// that asked for none, which pays for none of that rendering.
func (s *server) artifactTextBody(r *http.Request, who model.Actor, artifactID string) (string, string, *[]model.ArtifactBlock, error) {
	if r.URL.Query().Get("blocks") != "true" {
		markdown, token, err := s.deps.Docs.TextWithToken(r.Context(), who, artifactID)
		return markdown, token, nil, err
	}
	markdown, token, blocks, err := s.deps.Docs.TextWithTokenAndBlocks(r.Context(), who, artifactID)
	if err != nil {
		return "", "", nil, err
	}
	if err := s.describeBlocks(r.Context(), who, artifactID, blocks); err != nil {
		return "", "", nil, err
	}
	// A document with no blocks answers the empty list, never a missing key: the key is absent
	// only where the request asked for no blocks.
	if blocks == nil {
		blocks = []model.ArtifactBlock{}
	}
	return markdown, token, &blocks, nil
}

func (s *server) getArtifactBlocks(w http.ResponseWriter, r *http.Request) {
	who, ok := s.artifactReaderOf(w, r)
	if !ok {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	blocks, err := s.deps.Docs.Blocks(r.Context(), who, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.describeBlocks(r.Context(), who, artifact.ID, blocks); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, blocks)
}

// describeBlocks writes onto each block what the document's rows say about it, which its
// rendering does not hold: the comments and asks anchored to it, a table's folded in from the
// cells it holds, and where meaning search stands on it. The windows of a document's blocks are
// written under the artifact id; a block the live document holds and no window covers - a block
// inside another, whose text its parent's window already holds, and every block of a version the
// indexer has not windowed yet - reads as model.BlockNotEmbedded.
func (s *server) describeBlocks(ctx context.Context, who model.Actor, artifactID string, blocks []model.ArtifactBlock) error {
	references, err := s.blockReferences(ctx, who, artifactID)
	if err != nil {
		return err
	}
	embedding, err := refs.BlockEmbeddingStates(ctx, s.deps.Store.Pool, who, refs.DocumentWindowKind, artifactID)
	if err != nil {
		return err
	}
	for index := range blocks {
		counts := references[blocks[index].ID]
		for _, descendantID := range blocks[index].DescendantIDs {
			descendant := references[descendantID]
			counts.Comments += descendant.Comments
			counts.Asks += descendant.Asks
		}
		blocks[index].References = counts
		blocks[index].Embedding = blockEmbedding(embedding, blocks[index].ID)
	}
	return nil
}

func (s *server) getArtifactBlockPath(w http.ResponseWriter, r *http.Request) {
	who, ok := s.artifactReaderOf(w, r)
	if !ok {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	path, err := s.deps.Docs.BlockPath(r.Context(), who, artifact.ID, r.PathValue("block_id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, path)
}

// blockReferences counts the comments and asks anchored directly to each block.
// getArtifactBlocks adds a table's descendant-cell counts before it serves the
// table block. The predicate is two conjuncts (is not null, then not the empty
// string) rather than nullif(...) is not null so the planner can use the partial
// comments_/asks_anchor_block_id_idx indexes. Each comment and ask is counted only where who may
// read it (access.ItemOwnerClause over its own owner): a stored anchor that names this document
// from a comment or an ask of a project who does not see counts as one no row holds.
func (s *server) blockReferences(ctx context.Context, who model.Actor, artifactID string) (map[string]model.BlockReferences, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select anchor_block_id,
		       count(*) filter (where kind = 'comment'),
		       count(*) filter (where kind = 'ask')
		from (
			select anchor->>'block_id' as anchor_block_id, 'comment' as kind
			from comments
			where anchor->>'artifact_id' = $1 and anchor->>'block_id' is not null and anchor->>'block_id' <> ''
			  and `+access.ItemOwnerClause("comments.issue_key", "comments.artifact_id", "$2")+`
			union all
			select anchor->>'block_id' as anchor_block_id, 'ask' as kind
			from asks
			where anchor->>'artifact_id' = $1 and anchor->>'block_id' is not null and anchor->>'block_id' <> ''
			  and `+access.ItemOwnerClause("asks.issue_key", "asks.artifact_id", "$2")+`
		) anchored
		group by anchor_block_id
	`, artifactID, refs.VisibilityArg(who))
	if err != nil {
		return nil, fmt.Errorf("list block references: %w", err)
	}
	defer rows.Close()

	references := make(map[string]model.BlockReferences)
	for rows.Next() {
		var blockID string
		var comments, asks int
		if err := rows.Scan(&blockID, &comments, &asks); err != nil {
			return nil, fmt.Errorf("scan block references: %w", err)
		}
		references[blockID] = model.BlockReferences{Comments: comments, Asks: asks}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate block references: %w", err)
	}
	return references, nil
}

// artifactVersionText is the body the document arm of GET …/artifacts/…/versions/{number}
// answers without `download=true`, on all three artifact patterns. Blocks is absent unless the
// request asked for it with ?blocks=true: then it lists the version's typed blocks
// (docs.VersionBlocks), and Markdown, with the version's sha256 and size, is the rendering their
// byte ranges index.
type artifactVersionText struct {
	Markdown string `json:"markdown"`
	// MarkdownTree is the ProseMirror tree Markdown is shown as, present only for a caller that
	// sent `Dispatch-Trees: 1`.
	MarkdownTree model.DisplayTree `json:"markdown_tree,omitempty"`
	model.Version
	Blocks *[]model.ArtifactBlock `json:"blocks,omitempty"`
}

// maxIntegerColumn is the largest number a Postgres integer column holds. A number the caller
// names that such a column keys or stores is no row and no value at all past it:
// artifact_versions.number and every column that names one of a document's versions
// (user_document_reads.version among them), message_deliveries.attempt and
// comment_deliveries.attempt, events.seq, the position an issue's or a document's event list
// pages by, going_events.sequence, check_results' failed, skipped and excluded counts, and
// user_issue_state.last_read_seq. pgx refuses to encode a larger number against such a column,
// which a route would answer 500 as its own fault.
const maxIntegerColumn = math.MaxInt32

// fitsIntegerColumn reports whether a number the caller named is no greater than
// maxIntegerColumn. Each handler that takes such a number for an integer column holds it to this
// bound before any query is made with it, each with its own lower bound and its own refusal: the
// two of this file that read a version number from a path or a query (parseVersionNumber), the
// read mark's body (putDocumentRead), the delivery accept's attempt in its path (acceptDelivery,
// which answers it as the attempt it is not, 404 MESSAGE_NOT_FOUND), the attempt a reply names
// in its body (replyComment and replyMessage, each answering it as the delivery it is not, 404),
// an event list's after and before (parseEventPosition), a result's three counts
// (checkResultInput.refusal, 400 RESULT_INPUT), the issue read position's last_read_seq
// (putUserState, 400 INVALID_STATE), and a going stream's Last-Event-ID header
// (goingResumeFrom, which reads it as no point to resume from). The plan review's body needs no
// check of its own, since it refuses every version above the plan's latest, which is within the
// bound (createArtifactPlanReview). A reference's `@v<n>` pin is held to the same bound by its own
// checks in refs (BlockTextAt, blockPrefixCandidates), which answer a version past it as one the
// document does not have.
func fitsIntegerColumn[N int | int64](number N) bool {
	return number <= maxIntegerColumn
}

// parseVersionNumber reads a version number the caller named in a path or a query: a positive
// integer that fits the column (fitsIntegerColumn), and false for anything else, which is the
// caller's input refused, 400 INVALID_VERSION, with no query made with it. Both routes that read
// a version number from a path or a query read it here: GET …/versions/{number} and the change
// set (getArtifactChanges), each answering the refusal in its own words.
func parseVersionNumber(text string) (int, bool) {
	number, err := strconv.Atoi(text)
	if err != nil || number < 1 || !fitsIntegerColumn(number) {
		return 0, false
	}
	return number, true
}

// getArtifactVersion answers one version of an artifact on all four version routes: a document's
// text as artifactVersionText, or as its Markdown under its file name at the download address,
// and a file's or an image's bytes (serveFileVersion).
func (s *server) getArtifactVersion(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	number, valid := parseVersionNumber(r.PathValue("number"))
	if !valid {
		writeError(w, "INVALID_VERSION", http.StatusBadRequest, fmt.Sprintf("version number must be from 1 to %d", maxIntegerColumn))
		return
	}
	// Exactly `true` asks for the download, as `blocks=true` asks for the blocks of the /text
	// routes; any other value, `1` included, answers as without the parameter. `blocks` reads the
	// same way here.
	download := r.URL.Query().Get("download") == "true"
	withBlocks := r.URL.Query().Get("blocks") == "true"
	who, ok := s.artifactReaderOf(w, r)
	if !ok {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	latest := 0
	for _, version := range artifact.Versions {
		latest = max(latest, version.Number)
	}
	// The file name is the artifact's name as this read finds it, so a renamed document downloads
	// under its new name, and an older version's carries its number (versionFilename).
	filename := versionFilename(artifact.Name, artifact.Kind, number, latest)
	if artifact.Kind == "doc" {
		var markdown string
		version, err := s.loadVersion(r.Context(), s.deps.Store.Pool, who, artifact.ID, number, &markdown)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if download {
			// The download is the Markdown file itself, not the JSON that wraps it: the text the
			// JSON's `markdown` field holds, with no display tree read.
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Disposition", attachmentDisposition(filename))
			w.Header().Set("Cache-Control", downloadCacheControl)
			w.Header().Set("Content-Length", strconv.Itoa(len(markdown)))
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, markdown)
			return
		}
		var blocks *[]model.ArtifactBlock
		if withBlocks {
			// The version's own blocks, never the live tree's, and none of what describeBlocks adds
			// from the live document's rows: a comment count or an embedding state is the live
			// document's, not this version's. A block's range indexes the rendering it was read
			// from, so the answer carries that text, its digest and its size.
			rendered, listed, err := docs.VersionBlocks(markdown)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if listed == nil {
				listed = []model.ArtifactBlock{}
			}
			markdown, blocks = rendered, &listed
			size := len(markdown)
			version.Size = &size
		}
		hash := sha256.Sum256([]byte(markdown))
		sha := hex.EncodeToString(hash[:])
		version.SHA256 = &sha
		// A past version's text is read in the version mode: the attributes the server wrote
		// into that text are part of what that version said (pmdoc.DisplayVersion).
		//
		// No write stores that mode - every writer stores the latest one, and the two modes read
		// one text into two different trees - so this read is where a version's tree first
		// reaches the table: it reads the table, and stores what the table did not hold on a
		// goroutine of its own, beside the rest of the request. A version is read again far
		// more often than it is written, which is what makes the first read's store worth its
		// one statement.
		reader := s.readVersionDisplayTrees(r.Context(), wantsDisplayTrees(r))
		answer := artifactVersionText{Markdown: markdown, Version: version, Blocks: blocks}
		reader.text(&answer.MarkdownTree, markdown)
		reader.resolve()
		WriteJSON(w, http.StatusOK, answer)
		return
	}
	var content []byte
	var contentType, sha *string
	var size *int64
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select content, mime, sha256, size from artifact_versions where artifact_id = $1 and number = $2
	`, artifact.ID, number).Scan(&content, &contentType, &sha, &size); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.serveFileVersion(w, r, artifact.ID, filename, number, download, content, contentType, sha, size)
}

// versionsMarkdown is the stored markdown of each of numbers that the document has, by number, in
// one read; a number the document has no version of has no entry. Both readers of named versions'
// own text read it here: the change set (twoVersionsMarkdown) and the decisions of each named
// version (decisionSumsAt).
func (s *server) versionsMarkdown(ctx context.Context, artifactID string, numbers []int) (map[int]string, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select number, coalesce(markdown, '') from artifact_versions
		where artifact_id = $1::uuid and number = any($2::int[])
	`, artifactID, numbers)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	markdown := make(map[int]string, len(numbers))
	for rows.Next() {
		var number int
		var text string
		if err := rows.Scan(&number, &text); err != nil {
			return nil, err
		}
		markdown[number] = text
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return markdown, nil
}

// deref is the string a nullable column holds, or "".
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// commitArtifactVersionEvent queues the artifact.version event of a version the transaction wrote
// and the stamp of the document's new mention edges with it (refs.QueueStamp), after the append so
// stamping never touches sequence allocation. The event is filled in once Commit has run the write
// phase (queueStampedEvent). Right after it, it queues what that version's block index stated in
// the node and tuple store (written.Blocks), which waited for this event: the pass ran before the
// event existed, and no tuple may be updated once it is appended.
func (s *server) commitArtifactVersionEvent(
	ledger *docs.Ledger,
	eventOwner owner,
	actor model.Actor,
	artifactID, name string,
	written docs.VersionResult,
	diff *string,
) (*model.Event, error) {
	event, err := s.queueStampedEvent(ledger, eventOwner.event(
		"artifact.version",
		actor,
		docs.ArtifactVersionEventPayload(artifactID, name, written.Version, diff, written.Changes),
	), "artifact", artifactID)
	if err != nil {
		return nil, err
	}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		graphstore.QueueCaused(b, written.Blocks, graphstore.LatestEvent)
		return nil
	}); err != nil {
		return nil, err
	}
	return event, nil
}

// createVersionInput is the body of POST /api/v1/artifacts/{id}/versions and its issue and
// project artifact patterns.
type createVersionInput struct {
	Summary string       `json:"summary" required:"true"`
	Actor   *model.Actor `json:"actor"`
}

func (s *server) createNamedVersion(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input createVersionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	summary := strings.TrimSpace(input.Summary)
	if summary == "" {
		writeError(w, "INVALID_VERSION", http.StatusBadRequest, "named versions require a summary")
		return
	}
	who, ok := s.artifactReaderOf(w, r)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	artifact, err := s.loadArtifactForRequest(r.Context(), tx, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	eventOwner := ownerForArtifact(artifact)
	if err := s.requireOpenOwner(r.Context(), tx, who, eventOwner); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	named, err := s.deps.Docs.NamedVersion(documentCtx, artifact.ID, summary, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	version := named.Version
	diff, err := s.namedVersionDiff(r.Context(), tx, ledger, artifact.ID, version)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.commitArtifactVersionEvent(ledger, eventOwner, actor, artifact.ID, artifact.Name, named, diff)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), *event)
	WriteJSON(w, http.StatusCreated, version)
}

// editArtifactInput is the body of POST /api/v1/artifacts/{id}/edits and its issue and project
// artifact patterns.
type editArtifactInput struct {
	Ops          []model.EditOp          `json:"ops"`
	Summary      string                  `json:"summary"`
	Precondition *model.EditPrecondition `json:"precondition"`
	Actor        *model.Actor            `json:"actor"`
}

// jsonMember decodes any JSON value into whether it is null, so a map of them says which members
// a JSON object names, and which of those are null, and reading one copies none of its value.
type jsonMember struct{ null bool }

func (m *jsonMember) UnmarshalJSON(data []byte) error {
	m.null = string(data) == "null"
	return nil
}

// replaceTakes reports whether member is one a replace takes: op, find, with and occurrence. Every
// other member is another operation's or none's, so a replace carrying one sends what replace
// never reads.
func replaceTakes(member string) bool {
	switch member {
	case "op", "find", "with", "occurrence":
		return true
	}
	return false
}

// replaceFieldsRefusal is the refusal of the first replace of ops whose JSON in body, the request's
// bytes, names no with or carries a member a replace does not take (replaceTakes), whatever that
// member holds: 400 REPLACE_FIELDS naming the operation and each such member; and nil where every
// replace names with and replace's members alone. Read as an empty with, a replace naming none
// deleted its match, and one carrying another member was applied as if it held none. Only the
// body says either: a decoded op's With is "" both for no with and for with "", which deletes the
// match on purpose, a JSON null with is none, and a member model.EditOp does not declare decodes
// to nothing. So a batch holding a replace reads body a second time, for each op's member names
// alone (jsonMember). The decoder stops after body's one value, as the first read did
// (readJSONBody, which accepts a value followed by a closing bracket or brace), so a body the
// route accepted reads here too.
func replaceFieldsRefusal(ops []model.EditOp, body []byte) error {
	if !slices.ContainsFunc(ops, func(op model.EditOp) bool { return op.Op == "replace" }) {
		return nil
	}
	var named struct {
		Ops []map[string]jsonMember `json:"ops"`
	}
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&named); err != nil {
		return fmt.Errorf("read the edit's members: %w", err)
	}
	if len(named.Ops) != len(ops) {
		return fmt.Errorf("read the edit's members: the second read found %d operations, the first %d", len(named.Ops), len(ops))
	}
	for index, op := range ops {
		if op.Op != "replace" {
			continue
		}
		with, withNamed := named.Ops[index]["with"]
		omitted := !withNamed || with.null
		var foreign []string
		for member := range named.Ops[index] {
			if !replaceTakes(member) {
				foreign = append(foreign, member)
			}
		}
		if !omitted && len(foreign) == 0 {
			continue
		}
		var problems []string
		if omitted {
			problems = append(problems, "names no with, which would delete the text find matches")
		}
		if len(foreign) > 0 {
			slices.Sort(foreign)
			problems = append(problems, fmt.Sprintf("carries %s, which replace does not take", strings.Join(foreign, " and ")))
		}
		return errorf(http.StatusBadRequest, "REPLACE_FIELDS",
			`operation %d: replace %s; a replace takes find, with and occurrence alone: write the new text in with, or with "" to delete the match on purpose`,
			index, strings.Join(problems, ", and "))
	}
	return nil
}

// editArtifactResponse is the body POST /api/v1/artifacts/{id}/edits and its issue and project
// artifact patterns answer.
type editArtifactResponse struct {
	Applied int  `json:"applied"`
	Changed bool `json:"changed"`
	// LostOps are which operations the live document did not hold once this write reached it: a
	// concurrent change that landed after the version was rendered and before the publish is
	// past undoing, so the edit reports it rather than refusing (LEGION-269). null means the
	// check reached no verdict - the publish failed and the room is reloading, or the room
	// holds a tree too deep to read - which is not the same statement as the empty list.
	LostOps      []int          `json:"lost_ops"`
	Token        string         `json:"token"`
	UnchangedOps []int          `json:"unchanged_ops"`
	Version      *model.Version `json:"version"`
	// Written is the markdown the batch wrote, read back off the tree it wrote
	// (docs.EditOutcome.Written): each of the document's own blocks the write added, changed or
	// moved, in document order, and a line for each one it removed or emptied. It is empty for a
	// batch that left the document as it was.
	Written string `json:"written"`
}

func (s *server) editArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input editArtifactInput
	body, err := decodeJSONBody(r, &input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := replaceFieldsRefusal(input.Ops, body); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	who, ok := s.artifactReaderOf(w, r)
	if !ok {
		return
	}
	artifact, err := s.loadArtifactForRequest(r.Context(), s.deps.Store.Pool, who, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "artifact is not a document")
		return
	}
	// release ends the conditional edit gate once: right after the commit, which is all a later
	// conditional edit of this document waits on, so the checks of this edit's new decision blocks
	// never hold it; on every earlier return, here.
	release := func() {}
	if input.Precondition != nil {
		acquired, err := s.deps.Docs.AcquireConditionalEdit(r.Context(), artifact.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		release = sync.OnceFunc(acquired)
		defer release()
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	eventOwner := ownerForArtifact(artifact)
	status, err := s.requireOpenOwnerStatus(r.Context(), tx, who, eventOwner)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	edit, err := s.deps.Docs.ApplyOps(documentCtx, artifact.ID, input.Ops, actor, input.Precondition)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var written *docs.VersionResult
	var published []*model.Event
	// A batch that left the document as it was names no version, however deliberate its summary,
	// so edits that change nothing never pile up byte-identical versions.
	if edit.Changed {
		summary := strings.TrimSpace(input.Summary)
		if summary != "" {
			namedVersion, err := s.deps.Docs.NamedVersion(documentCtx, artifact.ID, summary, actor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			written = &namedVersion
		} else {
			snapshot, err := s.deps.Docs.SnapshotVersion(documentCtx, artifact.ID, actor)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			if snapshot.Wrote {
				written = &snapshot
			}
		}
		if written != nil {
			var diff *string
			if written.Version.Named {
				diff, err = s.namedVersionDiff(r.Context(), tx, ledger, artifact.ID, written.Version)
				if err != nil {
					s.writeHandlerError(w, err)
					return
				}
			}
			event, err := s.commitArtifactVersionEvent(ledger, eventOwner, actor, artifact.ID, artifact.Name, *written, diff)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			published = append(published, event)
		}
	}
	// The approval this edit's own version superseded, read in the edit's read phase, where that
	// version and any carry it made are queued: a person's approval of a spec is Legion's design
	// gate, and the writer is the only one who learns in time that this write closed it.
	var superseded *supersededApproval
	var moved *movedApprovalRequest
	if written != nil {
		superseded = s.supersededApprovalAdvice(
			r.Context(), tx, ledger, who, routeEditDocument, artifactIssueKey(artifact), artifact.ID, written.Version.Number,
		)
		// The open approval request this edit's own version took back from the person it was
		// waiting on, read off the ledger that version's move (docs.MoveApprovalAsk) and any
		// carry are queued on: an edit that stored no version moved nothing, and one whose carry
		// answered the request leaves it with the person and reports nothing.
		moved = s.movedApprovalRequestAdvice(
			r.Context(), tx, ledger, routeEditDocument, artifactIssueKey(artifact), artifact.ID,
		)
	}
	// An edit hears about the asks of this issue a person declined to answer only where it added
	// an ask block, which is the write R6 asks the reminder for: "an edit of one of the issue's
	// documents that adds an ask block". Every other edit puts no question to anyone and runs no
	// such read.
	var advice *writeAdvice
	if artifact.IssueKey != nil && status != nil {
		advice = s.writeAdvice(
			r.Context(), tx, routeEditDocument, *artifact.IssueKey, actor, adviceAsks{}, *status, "", len(edit.AddedAskBlocks) > 0,
		)
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	release()
	var version *model.Version
	if written != nil {
		version = &written.Version
	}
	if edit.Applied > 0 {
		s.deps.Docs.ScheduleSettlement(artifact.ID)
	}
	s.publish(r.Context(), appendedEvents(published...)...)
	unchanged := edit.Unchanged
	if unchanged == nil {
		unchanged = []int{}
	}
	responsePayload := editArtifactResponse{
		Applied:      edit.Applied,
		Changed:      edit.Changed,
		LostOps:      lostOps(ledger, artifact.ID),
		Token:        edit.Token,
		UnchangedOps: unchanged,
		Version:      version,
		Written:      edit.Written,
	}
	// The ask blocks the edit added, read from the documents the batch was applied to: whether an
	// operation's markdown makes a block depends on where it lands and on the parser's reading of
	// code and containers, which a caller's own reading of that markdown would only approximate.
	blocks := &editBlocks{
		DecisionBlocksAdded: len(edit.AddedAskBlocks),
		DecisionBlockChecks: s.decisionBlockChecks(r.Context(), artifact, edit.AddedAskBlocks, actor, who, wantsDisplayTrees(r)),
	}
	if artifact.IssueKey == nil || advice == nil {
		WriteJSON(w, http.StatusOK, withRawAdvice(responsePayload, documentWriteAdvice{
			editBlocks: blocks, SupersededApproval: superseded, MovedApprovalRequest: moved,
		}))
		return
	}
	advice.editBlocks = blocks
	advice.SupersededApproval = superseded
	advice.MovedApprovalRequest = moved
	WriteJSON(w, http.StatusOK, withAdvice(responsePayload, advice))
}

// artifactIssueKey is the issue a document belongs to, or the empty string for a project
// document, for a log line that names where a write ran.
func artifactIssueKey(artifact model.Artifact) string {
	if artifact.IssueKey == nil {
		return ""
	}
	return *artifact.IssueKey
}

// lostOps is the edit response's lost_ops: the operations the live document did not hold after
// the write was published, or null when the write reached no verdict.
func lostOps(ledger *docs.Ledger, artifactID string) []int {
	lost, known := ledger.LostOps(artifactID)
	if !known {
		return nil
	}
	if lost == nil {
		return []int{}
	}
	return lost
}

func participantsOrEmpty(participants []model.Actor) []model.Actor {
	if participants == nil {
		return []model.Actor{}
	}
	return participants
}

// issueArtifactsStatement lists an issue's artifacts, $1 the issue key, in the order every
// reader of the issue shows them; collectArtifacts reads its rows. The issue read queues it in
// a batch with its other reads (getIssue), so the statement and the scan live apart from the
// sequential loader that every other route calls.
const issueArtifactsStatement = `
		select ` + artifactColumns + `
		from artifacts where issue_key = $1 order by created_at, id
	`

func (s *server) loadArtifacts(ctx context.Context, q queryer, who model.Actor, issueKey string) ([]model.Artifact, error) {
	rows, err := q.Query(ctx, issueArtifactsStatement, issueKey)
	if err != nil {
		return nil, err
	}
	artifacts, err := collectArtifacts(rows)
	if err != nil {
		return nil, err
	}
	if err := loadArtifactDetails(ctx, q, who, artifactPointers(artifacts)); err != nil {
		return nil, err
	}
	return artifacts, nil
}

// collectArtifacts reads the rows issueArtifactsStatement returns. An issue with no artifact
// keeps a non-nil empty list, which the response carries as [].
func collectArtifacts(rows pgx.Rows) ([]model.Artifact, error) {
	artifacts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Artifact, error) {
		return scanArtifact(row)
	})
	if err != nil {
		return nil, err
	}
	if artifacts == nil {
		artifacts = []model.Artifact{}
	}
	return artifacts, nil
}

// artifactPointers addresses each artifact in place, which the version and approval loaders fill.
func artifactPointers(artifacts []model.Artifact) []*model.Artifact {
	pointers := make([]*model.Artifact, len(artifacts))
	for index := range artifacts {
		pointers[index] = &artifacts[index]
	}
	return pointers
}

// artifactIDs is every listed artifact's id, the argument artifactVersionsStatement takes.
func artifactIDs(artifacts []*model.Artifact) []string {
	ids := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		ids = append(ids, artifact.ID)
	}
	return ids
}

// queueArtifactDetails queues in b what a list of artifacts carries beside its rows - every
// artifact's versions in one statement rather than a query per artifact, and every document's
// latest review and open approval request (queueApprovals) - and returns the apply that fills
// them in once the batch is closed. The statements are queued in that order wherever artifacts
// are read with their details: the issue read queues them beside its other reads
// (readIssueDetails), and a read of its own sends them as a batch of its own (loadArtifactDetails).
// The apply fills the versions first, since an approval's state reads them (applyApprovals), and
// leaves an artifact with no version a non-nil empty list (applyVersions). No artifact queues
// nothing and its apply fills nothing.
func queueArtifactDetails(b *pgx.Batch, who model.Actor, artifacts []*model.Artifact) func() {
	versions := map[string][]model.Version{}
	if ids := artifactIDs(artifacts); len(ids) > 0 {
		b.Queue(artifactVersionsStatement, ids).Query(func(rows pgx.Rows) error {
			return scanVersionsByArtifact(rows, versions)
		})
	}
	approvals := queueApprovals(b, who, artifacts)
	return func() {
		applyVersions(artifacts, versions)
		approvals()
	}
}

// loadArtifactDetails sends queueArtifactDetails's statements as a batch of their own on q, for a
// read that queues nothing else: an issue's artifacts (loadArtifacts), a project's
// (listProjectArtifacts), one artifact read by id (loadArtifact) and one read by ref_key or as
// an issue's spec (loadArtifactRow). The batch holds q's connection until its results close, so
// nothing else reads the database in between.
func loadArtifactDetails(ctx context.Context, q queryer, who model.Actor, artifacts []*model.Artifact) error {
	batch := &pgx.Batch{}
	apply := queueArtifactDetails(batch, who, artifacts)
	if batch.Len() == 0 {
		return nil
	}
	if err := q.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	apply()
	return nil
}

// validateArtifactRequestRef refuses an artifact route's {id} that is no uuid, as an id no row
// holds; a route that names its artifact by owner and slug ({key} or {session_id}) has none.
func validateArtifactRequestRef(r *http.Request) error {
	if r.PathValue("key") != "" || r.PathValue("session_id") != "" {
		return nil
	}
	_, err := parseArtifactID(r.PathValue("id"))
	return err
}

// artifactReaderOf is readerOf for a route that names an artifact: the caller is resolved as
// every gate resolves it, and then an {id} that is no uuid is refused as an id no row holds
// (validateArtifactRequestRef) before the viewer is read, so no statement binds such an id
// (TestArtifactIDRoutesValidateBeforeDatabaseUse). Each artifact route runs behind its gate
// (withinReadScope, withinProjectScope), which has already read the caller's viewer, the one
// statement such a request sends; the caller and the reader are each resolved once, kept in the
// request's context.
func (s *server) artifactReaderOf(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	r = s.withCallerResolved(r)
	if _, _, err := s.optionalActor(r); err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, false
	}
	if err := validateArtifactRequestRef(r); err != nil {
		s.writeHandlerError(w, err)
		return model.Actor{}, false
	}
	return s.readerOf(w, r)
}

func parseArtifactID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return "", errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	return parsed.String(), nil
}

func (s *server) loadArtifactForRequest(ctx context.Context, q queryer, who model.Actor, r *http.Request) (model.Artifact, error) {
	if r.PathValue("session_id") != "" {
		return s.loadAgentArtifact(ctx, q, who, r)
	}
	if key := r.PathValue("key"); key != "" {
		target := artifactTarget{Project: key}
		if strings.HasPrefix(r.URL.Path, "/api/v1/issues/") {
			target = artifactTarget{IssueKey: &key}
		}
		return s.loadArtifactByDocumentReference(ctx, q, who, target, r.PathValue("slug"))
	}
	return s.loadArtifact(ctx, q, who, r.PathValue("id"))
}

const documentHintLimit = 8

// loadArtifactByDocumentReference reads an issue's or a project's artifact at slug reference, or
// the one document whose filename it is.
func (s *server) loadArtifactByDocumentReference(ctx context.Context, q queryer, who model.Actor, target artifactTarget, reference string) (model.Artifact, error) {
	return resolveDocumentReference(ctx, s, q, who, target, reference,
		func(refKey string) (model.Artifact, error) { return s.loadArtifactByRefKey(ctx, q, who, refKey) },
		func(id string) (model.Artifact, error) { return s.loadArtifact(ctx, q, who, id) },
	)
}

// resolveDocumentReference is the one resolution of an issue's or a project's document reference,
// shared by the full read and the summary: the artifact at slug reference (byRefKey, given the
// ref_key), else the one document whose filename it is (byID, given its id). Each reader answers
// ARTIFACT_NOT_FOUND for no row, and reads only the shape its route serves. The documents a
// reference is matched among, and named in a refusal, are the ones who may read.
func resolveDocumentReference[T any](
	ctx context.Context,
	s *server,
	q queryer,
	who model.Actor,
	target artifactTarget,
	reference string,
	byRefKey func(refKey string) (T, error),
	byID func(id string) (T, error),
) (T, error) {
	found, err := byRefKey(target.refPrefix() + "/" + reference)
	if err == nil || !isArtifactNotFound(err) {
		return found, err
	}
	var none T
	documents, listErr := s.loadDocumentReferences(ctx, q, who, target)
	if listErr != nil {
		return none, listErr
	}
	matches := make([]model.Artifact, 0, 1)
	for index := range documents {
		if documents[index].Name == reference {
			matches = append(matches, documents[index])
		}
	}
	switch len(matches) {
	case 0:
		return none, documentReferenceError(target, reference, documents)
	case 1:
		return byID(matches[0].ID)
	default:
		return none, ambiguousDocumentReferenceError(target, reference, matches)
	}
}

func isArtifactNotFound(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.code == "ARTIFACT_NOT_FOUND"
}

func (s *server) loadDocumentReferences(ctx context.Context, q queryer, who model.Actor, target artifactTarget) ([]model.Artifact, error) {
	owner := `issue_key = $1`
	value := target.Project
	if target.IssueKey != nil {
		value = *target.IssueKey
	} else {
		owner = `project_key = $1 and issue_key is null`
	}
	rows, err := q.Query(ctx, `
		select `+artifactColumns+`
		from artifacts where `+owner+` and kind = 'doc' and `+access.DocumentClause("artifacts", "$2")+`
		order by created_at, id`, value, refs.VisibilityArg(who))
	if err != nil {
		return nil, err
	}
	documents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.Artifact, error) {
		return scanArtifact(row)
	})
	if err != nil {
		return nil, err
	}
	return documents, nil
}

// documentReferenceError refuses a reference that names none of the owner's documents. An owner
// with more documents than the hint lists (documentHintLimit) is named with its count, the
// documents nearest the reference first (documentsNearest), and the route that lists them all.
func documentReferenceError(target artifactTarget, reference string, documents []model.Artifact) error {
	scope := "this project's"
	everyDocument := "/api/v1/projects/" + target.Project + "/artifacts?unlinked=true"
	if target.IssueKey != nil {
		scope = "this issue's"
		everyDocument = "/api/v1/issues/" + *target.IssueKey + "/artifacts"
	}
	if len(documents) <= documentHintLimit {
		return errorf(
			http.StatusNotFound,
			"ARTIFACT_NOT_FOUND",
			`document %q not found by slug; %s documents: %s`,
			reference,
			scope,
			documentReferenceLabels(documents),
		)
	}
	return errorf(
		http.StatusNotFound,
		"ARTIFACT_NOT_FOUND",
		`document %q not found by slug; %s %d documents include: %s; GET %s lists them all`,
		reference,
		scope,
		len(documents),
		documentReferenceLabels(documentsNearest(reference, documents)),
		everyDocument,
	)
}

// documentsNearest orders documents, given oldest first, for a hint about reference: those whose
// slug or filename is close to it (documentCloseness) first, nearest first, then every other one,
// each group newest first. envoy-client's documentsNearest orders an issue's documents by the
// same rule for the hint the dispatch command gives.
func documentsNearest(reference string, documents []model.Artifact) []model.Artifact {
	type ranked struct {
		document model.Artifact
		index    int
		distance int
		near     bool
	}
	rankedDocuments := make([]ranked, len(documents))
	for index, document := range documents {
		distance, near := documentCloseness(reference, document.Slug, document.Name)
		rankedDocuments[index] = ranked{document: document, index: index, distance: distance, near: near}
	}
	slices.SortStableFunc(rankedDocuments, func(a, b ranked) int {
		if a.near != b.near {
			if a.near {
				return -1
			}
			return 1
		}
		if a.near && a.distance != b.distance {
			return a.distance - b.distance
		}
		return b.index - a.index
	})
	ordered := make([]model.Artifact, len(rankedDocuments))
	for index, document := range rankedDocuments {
		ordered[index] = document.document
	}
	return ordered
}

// documentCloseness compares a reference with a document's slug and filename, ASCII letter case
// ignored: the smaller edit distance of the two, and whether the document is close to it - within
// a third of the reference's length (at least 2 edits), or one holding the other where the one held
// is at least 4 characters long, whose distance is then exactly their difference in length. A
// spelling more than limit edits away gives no distance (boundedEditDistance knows only that it is
// past limit), so it never ties a document that is close through its other spelling. Only ASCII
// letters are folded because Go and JavaScript lowercase other letters differently, and
// envoy-client's documentCloseness must rank alike.
func documentCloseness(reference, slug, name string) (int, bool) {
	wanted := []rune(fold.ASCII(reference))
	limit := max(2, len(wanted)/3)
	distance := -1
	for _, candidate := range []string{slug, name} {
		label := []rune(fold.ASCII(candidate))
		edits := boundedEditDistance(wanted, label, limit)
		if (len(wanted) >= 4 && strings.Contains(string(label), string(wanted))) ||
			(len(label) >= 4 && strings.Contains(string(wanted), string(label))) {
			edits = max(len(label)-len(wanted), len(wanted)-len(label))
		} else if edits > limit {
			continue
		}
		if distance < 0 || edits < distance {
			distance = edits
		}
	}
	if distance < 0 {
		return limit + 1, false
	}
	return distance, true
}

// boundedEditDistance is the Levenshtein distance between a and b in characters, or limit+1 once
// it is known to be more than limit.
func boundedEditDistance(a, b []rune, limit int) int {
	if len(a)-len(b) > limit || len(b)-len(a) > limit {
		return limit + 1
	}
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		smallest := current[0]
		for j := 1; j <= len(b); j++ {
			substitution := previous[j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			current[j] = min(previous[j]+1, current[j-1]+1, substitution)
			smallest = min(smallest, current[j])
		}
		if smallest > limit {
			return limit + 1
		}
		previous, current = current, previous
	}
	return min(previous[len(b)], limit+1)
}

func ambiguousDocumentReferenceError(target artifactTarget, reference string, documents []model.Artifact) error {
	scope := "project"
	if target.IssueKey != nil {
		scope = "issue"
	}
	return errorf(
		http.StatusBadRequest,
		"ARTIFACT_REFERENCE_AMBIGUOUS",
		`%q names %d documents on this %s; use a slug: %s`,
		reference,
		len(documents),
		scope,
		documentReferenceLabels(documents),
	)
}

func documentReferenceLabels(documents []model.Artifact) string {
	labels := make([]string, 0, min(len(documents), documentHintLimit))
	for index, artifact := range documents {
		if index == documentHintLimit {
			break
		}
		labels = append(labels, fmt.Sprintf("%s (%s)", artifact.Slug, artifact.Name))
	}
	if len(labels) == 0 {
		return "none"
	}
	return strings.Join(labels, ", ")
}

// loadArtifact reads one artifact by id, with the details every artifact read carries: the row
// loadArtifactRowByID finds, and then its versions and its approval (loadArtifactDetails).
func (s *server) loadArtifact(ctx context.Context, q queryer, who model.Actor, id string) (model.Artifact, error) {
	artifact, err := loadArtifactRowByID(ctx, q, who, id)
	if err != nil {
		return model.Artifact{}, err
	}
	if err := loadArtifactDetails(ctx, q, who, []*model.Artifact{&artifact}); err != nil {
		return model.Artifact{}, err
	}
	return artifact, nil
}

func (s *server) loadArtifactByRefKey(ctx context.Context, q queryer, who model.Actor, refKey string) (model.Artifact, error) {
	return s.loadArtifactRow(ctx, q, who, q.QueryRow(ctx, `
		select `+artifactColumns+`
		from artifacts where ref_key = $1 and `+access.DocumentClause("artifacts", "$2")+`
	`, refKey, refs.VisibilityArg(who)))
}

// loadArtifactRow reads one artifact from the row a lookup by ref_key or by any other column
// returns, with the details every artifact read carries - its versions, and its approval when it
// is a document - in one batch (loadArtifactDetails), so the read costs that lookup and one
// round trip more. The lookup binds access.DocumentClause for who, as every read of an artifact
// does.
func (s *server) loadArtifactRow(ctx context.Context, q queryer, who model.Actor, row pgx.Row) (model.Artifact, error) {
	artifact, err := scanArtifactOrNotFound(row)
	if err != nil {
		return model.Artifact{}, err
	}
	if err := loadArtifactDetails(ctx, q, who, []*model.Artifact{&artifact}); err != nil {
		return model.Artifact{}, err
	}
	return artifact, nil
}

// scanArtifactOrNotFound reads the artifact's own row, answering the 404 every artifact read
// answers for a row that is not there, the one idRoutes names for these routes. It is the step
// both reads of an artifact share: the one that goes on to carry the versions and the approval
// (loadArtifactRow) and the one that takes the row alone (loadArtifactRowByID).
func scanArtifactOrNotFound(row pgx.Row) (model.Artifact, error) {
	artifact, err := scanArtifact(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.Artifact{}, errorf(http.StatusNotFound, "ARTIFACT_NOT_FOUND", "artifact not found")
	}
	if err != nil {
		return model.Artifact{}, err
	}
	return artifact, nil
}

// loadArtifactRowByID reads one artifact's own row by id, without the versions and the approval
// loadArtifactRow carries: a write to one artifact reads neither, before its owner's lock or
// under it (beginArtifactWrite), so each of its two reads costs one round trip rather than two.
// The id is a whole one: the prefix layer resolved a short one into the path before the handler
// saw it (resolveIDPrefixes), and parseArtifactID answers the same 404 as a row that is not
// there for anything that is no uuid. A document who may not read is a row the read does not find
// (access.DocumentClause), so it answers that same 404.
func loadArtifactRowByID(ctx context.Context, q queryer, who model.Actor, id string) (model.Artifact, error) {
	parsed, err := parseArtifactID(id)
	if err != nil {
		return model.Artifact{}, err
	}
	return scanArtifactOrNotFound(q.QueryRow(ctx, `
		select `+artifactColumns+`
		from artifacts where id = $1 and `+access.DocumentClause("artifacts", "$2")+`
	`, parsed, refs.VisibilityArg(who)))
}

// scanArtifact reads artifactColumns. A session-owned artifact's project is "".
func scanArtifact(row pgx.Row) (model.Artifact, error) {
	var artifact model.Artifact
	var project *string
	var createdBy []byte
	if err := row.Scan(
		&artifact.ID, &artifact.IssueKey, &project, &artifact.SessionID, &artifact.RefKey, &artifact.Slug,
		&artifact.Name, &artifact.Kind, &artifact.Primary, &artifact.Role, &createdBy, &artifact.CreatedAt,
	); err != nil {
		return model.Artifact{}, err
	}
	artifact.Project = deref(project)
	if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
		return model.Artifact{}, fmt.Errorf("decode artifact author: %w", err)
	}
	return artifact, nil
}

// artifactOwnerKey is the owner part of ref_key (artifactTarget.refPrefix), which the 0071
// trigger fills ref_key from, so a lookup by owner compares what ref_key was built from. Its
// session arm, 'agent/' || session_id, has a second reader: an agent's conversation's lookups
// (agentArtifactByNameOrSlugSQL, agentArtifactSlugTakenSQL in agent_artifacts.go) compare session_id
// itself, so a change to ref_key's owner part changes both.
const artifactOwnerKey = `coalesce(issue_key, project_key, 'agent/' || session_id)`

// artifactByNameOrSlugSQL and artifactSlugTakenSQL are an upload's two lookups by owner, $1 the
// owner key and $2 the name or the slug; an agent's conversation runs its own pair
// (artifactTarget.ownerLookup). An upload's name finds the artifact whose name or slug it is, so a
// name that is one artifact's slug is that artifact's next version rather than a second artifact
// the same reference would also name. An owner that already holds two such artifacts (one whose
// slug and another whose name it is, which uploads stored before this lookup read slugs) gets the
// one whose slug it is, the one a read by that reference resolves to (resolveDocumentReference).
const (
	artifactByNameOrSlugSQL = `select ` + artifactColumns + ` from artifacts where ` + artifactOwnerKey +
		` = $1 and $2 in (name, slug) order by slug = $2 desc limit 1`
	artifactSlugTakenSQL = `select exists(select 1 from artifacts where ` + artifactOwnerKey + ` = $1 and slug = $2)`
)

func (s *server) findArtifactByNameOrSlug(ctx context.Context, q queryer, target artifactTarget, name string) (model.Artifact, error) {
	artifact, err := lookupArtifactByNameOrSlug(ctx, q, target, name)
	if err != nil {
		return model.Artifact{}, err
	}
	versions, err := s.loadVersions(ctx, q, artifact.ID)
	if err != nil {
		return model.Artifact{}, err
	}
	artifact.Versions = versions
	model.StateFileTokens(artifact.Kind, artifact.Versions)
	return artifact, nil
}

// lookupArtifactByNameOrSlug reads the artifact name finds under target's owner, without its versions.
func lookupArtifactByNameOrSlug(ctx context.Context, q queryer, target artifactTarget, name string) (model.Artifact, error) {
	query, owner := target.ownerLookup(artifactByNameOrSlugSQL, agentArtifactByNameOrSlugSQL)
	return scanArtifact(q.QueryRow(ctx, query, owner, name))
}

// versionColumns is the column list a version read selects over artifact_versions aliased v, in
// the order scanned into model.Version: number, named, summary, authors, created_at, size, mime,
// sha256. The size never reads a document's markdown (model.VersionSize).
const versionColumns = `v.number, v.named, v.summary, v.authors, v.created_at, ` + model.VersionSize + `, v.mime, v.sha256`

// artifactVersionsStatement reads the versions of every artifact in $1 at once, so a list of
// artifacts costs one statement rather than one per artifact. Ordering by the artifact first
// leaves each artifact's own versions in `order by v.number`, the order one artifact's read
// answers; scanVersionsByArtifact keys them by the artifact id the columns end on.
const artifactVersionsStatement = `
		select ` + versionColumns + `, v.artifact_id::text
		from artifact_versions v where v.artifact_id = any($1::uuid[]) order by v.artifact_id, v.number
	`

// scanVersion reads versionColumns from row; extra receives the columns after them. It states no
// token: the caller, which knows the artifact's kind, does (model.StateFileTokens).
func scanVersion(row pgx.Row, extra ...any) (model.Version, error) {
	var version model.Version
	var authors []byte
	targets := make([]any, 0, 8+len(extra))
	targets = append(targets,
		&version.Number, &version.Named, &version.Summary, &authors,
		&version.CreatedAt, &version.Size, &version.MIME, &version.SHA256,
	)
	targets = append(targets, extra...)
	if err := row.Scan(targets...); err != nil {
		return model.Version{}, err
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode version authors: %w", err)
	}
	return version, nil
}

// scanVersionsByArtifact reads the rows artifactVersionsStatement returns into versions, keyed by
// artifact id.
func scanVersionsByArtifact(rows pgx.Rows, versions map[string][]model.Version) error {
	for rows.Next() {
		var artifactID string
		version, err := scanVersion(rows, &artifactID)
		if err != nil {
			return err
		}
		versions[artifactID] = append(versions[artifactID], version)
	}
	return rows.Err()
}

// applyVersions fills each artifact's versions from what the statement returned, with a file's
// or an image's tokens (model.StateFileTokens); an artifact with none keeps a non-nil empty list,
// which the response carries as [].
func applyVersions(artifacts []*model.Artifact, byArtifact map[string][]model.Version) {
	for _, artifact := range artifacts {
		versions := byArtifact[artifact.ID]
		if versions == nil {
			versions = []model.Version{}
		}
		model.StateFileTokens(artifact.Kind, versions)
		artifact.Versions = versions
	}
}

func (s *server) loadVersions(ctx context.Context, q queryer, artifactID string) ([]model.Version, error) {
	rows, err := q.Query(ctx, `
		select `+versionColumns+`
		from artifact_versions v where v.artifact_id = $1 order by v.number
	`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := []model.Version{}
	for rows.Next() {
		version, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, version)
	}
	return versions, rows.Err()
}

// loadVersion reads one version of a document with its markdown, which the content route answers,
// so its size is that markdown's length rather than a second read of the column.
func (s *server) loadVersion(ctx context.Context, q queryer, who model.Actor, artifactID string, number int, markdown *string) (model.Version, error) {
	var version model.Version
	var authors []byte
	if err := q.QueryRow(ctx, `
		select v.number, v.named, v.summary, v.authors, v.created_at, v.mime, v.sha256, v.markdown
		from artifact_versions v where v.artifact_id = $1 and v.number = $2
	`, artifactID, number).Scan(
		&version.Number, &version.Named, &version.Summary, &authors, &version.CreatedAt,
		&version.MIME, &version.SHA256, markdown,
	); err != nil {
		return model.Version{}, err
	}
	if err := json.Unmarshal(authors, &version.Authors); err != nil {
		return model.Version{}, fmt.Errorf("decode version authors: %w", err)
	}
	size := len(*markdown)
	version.Size = &size
	return version, nil
}

// artifactKind is what an upload is stored as - doc, image or file - and the type a file or an
// image is stored and served under. A declared text/markdown is a document. Any other upload is an
// image when, and only when, its bytes are one of the pictures a model is shown (PNG, JPEG, GIF,
// WebP), read from the bytes rather than the declared type, which a client sets at will; an image
// is stored under the type its bytes are. Anything else, an SVG for one, is a file under the
// declared type, so no client stores other bytes as an image.
func artifactKind(mediaType, declared string, content []byte) (kind, storedType string) {
	if mediaType == "text/markdown" {
		return "doc", declared
	}
	switch sniffed := http.DetectContentType(content); sniffed {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return "image", sniffed
	}
	return "file", declared
}

func artifactSlug(name string) string {
	var slug strings.Builder
	previousDash := false
	for _, r := range strings.ToLower(name) {
		// ASCII only: every grammar that reads a slug back - text.artifactSlugPattern, the
		// dashboard's own artifactSlugPattern, 0009's cleanup of malformed references - spells it
		// [a-z0-9]+(-[a-z0-9]+)*, so a slug holding another letter would be stored and then name
		// nothing, and the picture a `![name](dispatch://.../artifact/<slug>@vN)` line shows would
		// never load (LEGION-541).
		if ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') {
			slug.WriteRune(r)
			previousDash = false
			continue
		}
		if slug.Len() > 0 && !previousDash {
			slug.WriteByte('-')
			previousDash = true
		}
	}
	result := strings.Trim(slug.String(), "-")
	if result == "" {
		return "artifact"
	}
	return result
}

// roleSlugs pairs each role a document plays with the slug that names it: the one place the two
// spellings are written. Everything that asks what part a document plays is built from it —
// documentRole, which writes the role on a new row, the SQL predicates that hold a row to a role
// (documentRolePredicate), documentPlays, which asks the same of a row already read, and
// documentRoleOf, which names the part one row plays.
var roleSlugs = map[string]string{
	model.ArtifactRoleVerification: "verification-md",
	model.ArtifactRolePlan:         "plan-md",
	// The Decisions view already spells the decisions document's slug, which a reference to it
	// names, so the role takes that constant rather than a second copy of the same word.
	model.ArtifactRoleDecisions: decisionsDocumentSlug,
}

// documentRole is the role a document takes when its row is created: an issue's markdown
// document whose slug roleSlugs names plays that role; every other new artifact plays none. The
// slug decides, not the name, because the slug is the unique key the upload already took within
// the owner (nextArtifactSlug), so a second document whose name gives a slug already in use -
// verification-md-2 - plays no role rather than colliding with the one that holds it. The role
// is written once, as is_primary is: a later upload of the same name is that document's next
// version, under the row this made, and the only write that ever sets it afterwards is the
// repair of a row the rolling deploy left with none (storeArtifact).
func documentRole(target artifactTarget, kind, slug string) *string {
	if target.IssueKey == nil || kind != "doc" {
		return nil
	}
	for role, named := range roleSlugs {
		if named == slug {
			return &role
		}
	}
	return nil
}

// documentPlays reports whether an artifact already read plays role: it carries that role, or it
// is an issue's document that carries none and its slug is the role's, for a row a rolling deploy
// left with no role because the build that created it did not know the column. A role belongs to
// an issue's documents alone - documentRole gives none to any other, and the constraint
// artifacts_role_is_an_issue_document (migration 20261102110000_artifact_document_roles) holds the
// column to them - so a project document whose slug is a role's, a project's plan.md, plays no
// role and is a plain document. It is documentRolePredicate's answer about one row, so every
// reader — the queries that find a document by its role and the routes that judge the document in
// hand — agrees about the same row.
func documentPlays(artifact model.Artifact, role string) bool {
	return artifact.Role == role || (artifact.Role == "" && artifact.IssueKey != nil && artifact.Slug == roleSlugs[role])
}

// documentRoleOf is the part a document plays: the role of roleSlugs that documentPlays answers
// true for, asked of the row as every other reader asks it. A row plays at most one of them - a
// row that carries a role carries one of them, and an issue's row that carries none falls back to
// its slug, which the map pairs with a single role - so the order this walks the map in cannot
// change the answer. Asking documentPlays rather than reading artifacts.role is what gives a row
// whose role column is still null the role its slug decides, which is the state a rolling deploy
// leaves behind. A document that plays none of them answers "", which omits the field of a
// report that carries it (movedApprovalDocument).
func documentRoleOf(artifact model.Artifact) string {
	for role := range roleSlugs {
		if documentPlays(artifact, role) {
			return role
		}
	}
	return ""
}

// isSpecDocument reports whether an artifact already read is an issue's spec: the primary
// document of an issue. Both readers that ask it about an approval ask it here — the coverage
// an approval request's answer carries (approvalCoverage), which only a spec has, and
// include_verification, which names the spec and its issue's Verification document together
// (requestArtifactApproval).
func isSpecDocument(artifact model.Artifact) bool {
	return artifact.Primary && artifact.IssueKey != nil
}

// documentRolePredicate holds the artifacts row of the table aliased `alias` to the issue's
// document of one role: the role itself, or, for an issue's row a rolling deploy left with no
// role, the slug a document of that name carries. A project document never plays a role
// (documentPlays), whatever its slug. `alias` is empty where the query names no alias.
func documentRolePredicate(alias, role string) string {
	if alias != "" {
		alias += "."
	}
	return `(` + alias + `role = '` + role + `' or (` + alias + `role is null and ` + alias + `issue_key is not null and ` +
		alias + `slug = '` + roleSlugs[role] + `'))`
}

// verificationRolePredicate is documentRolePredicate for the Verification document of a query
// that names no alias, since the queries that hold a row to it are written as string literals.
var verificationRolePredicate = documentRolePredicate("", model.ArtifactRoleVerification)

// aliasedVerificationRole, aliasedPlanRole and aliasedDecisionsRole are the same predicate for a
// query whose artifacts table is aliased `a`: the issue read, the coverage read and the check
// resolver all join it that way.
var aliasedVerificationRole = documentRolePredicate("a", model.ArtifactRoleVerification)
var aliasedPlanRole = documentRolePredicate("a", model.ArtifactRolePlan)
var aliasedDecisionsRole = documentRolePredicate("a", model.ArtifactRoleDecisions)

// artifactKindMismatch is the 400 refusing an upload under name, of kind, as the next version of
// artifact, the one the name or slug finds, whose kind is another.
func artifactKindMismatch(name string, artifact model.Artifact, kind string) error {
	return errorf(http.StatusBadRequest, "ARTIFACT_KIND_MISMATCH",
		"%q names artifact %s (%s), whose kind is %s; this upload's kind is %s, so it cannot be its next version: give it another name",
		name, artifact.Slug, artifact.Name, artifact.Kind, kind)
}

// blankWriteToADocumentThatExists is the 400 refusing a write with no content under name, which
// names artifact, the document the name or the slug finds. A write with no content makes a new
// document; under a name a document already holds it would store a version holding nothing over
// the text that document has, which no caller means by typing a title.
func blankWriteToADocumentThatExists(name string, artifact model.Artifact) error {
	return errorf(http.StatusBadRequest, "ARTIFACT_INPUT",
		"%q names artifact %s (%s), which exists; a write with no content makes no document and writes no version: edit it with dispatch doc-edit, or give the new document another name",
		name, artifact.Slug, artifact.Name)
}

// createRoleNameRefusal is the 400 refusing a create with no content whose minted slug is the
// slug of one of the documents Dispatch makes itself (roleSlugs): a person who types `Plan.md`
// as a title means that document, which Dispatch writes with its own content. The slug is read,
// not the title, so every spelling that gives it reads the same refusal, and a title whose slug
// the owner already holds - which mints `plan-md-2` - is a document of its own.
func createRoleNameRefusal(name, slug, role string) error {
	return errorf(http.StatusBadRequest, "ARTIFACT_ROLE_NAME",
		"%q gives the slug %s, the slug of the issue's %s document, which Dispatch makes itself; give this document another name",
		name, slug, role)
}

// newArtifactNameRefusal is the 400 refusing a new artifact's name past maxNewArtifactKeyBytes,
// or nil. An upload naming an artifact that already exists is never held to it.
func newArtifactNameRefusal(name string) error {
	if len(name) <= maxNewArtifactKeyBytes {
		return nil
	}
	return errorf(http.StatusBadRequest, "INVALID_ARTIFACT",
		"artifact name is %d bytes, past the %d bytes a new artifact's name may hold; give it a shorter name",
		len(name), maxNewArtifactKeyBytes)
}

// newArtifactKeyRefusal is the 400 refusing a new artifact whose reference key, the target's
// owner, `/` and slug (the one nextArtifactSlug took, suffix and all), is past
// maxNewArtifactKeyBytes, or nil.
func newArtifactKeyRefusal(target artifactTarget, slug string) error {
	key := len(target.refPrefix()) + 1 + len(slug)
	if key <= maxNewArtifactKeyBytes {
		return nil
	}
	return errorf(http.StatusBadRequest, "INVALID_ARTIFACT",
		"the reference key `%s/<slug>` this name gives a new artifact is %d bytes, past the %d bytes a new artifact's reference key may hold; give it a shorter name",
		target.refPrefix(), key, maxNewArtifactKeyBytes)
}

// lockProjectArtifacts serialises one project's own uploads (its documents, not its issues') for
// the rest of tx, as lockAgentArtifacts does a conversation's. A project's upload otherwise locks
// no row before it looks its artifact up by name or slug and takes a new slug, so two uploads at
// once - `plan.md` and `plan-md` - could each miss the other and leave two documents that one
// reference names. These uploads take it, and so does a project document's rename or removal
// (beginArtifactWrite); each takes it before any row, room or the document's owner lock, which
// comes after it in every taker, so it joins no lock cycle.
func lockProjectArtifacts(ctx context.Context, tx pgx.Tx, project string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('project-artifacts:' || $1))`, project); err != nil {
		return fmt.Errorf("lock the project's documents: %w", err)
	}
	return nil
}

// beginArtifactWrite opens the transaction a write to one artifact runs in: it reads the
// artifact the request names, takes its owner's lock and reads the row again under that lock. It
// is the preamble the title correction and the removal share (renameArtifact, removeArtifact),
// and each of them holds only its own refusals after it.
//
// Both writes are mounted on /api/v1/artifacts/{id} alone (routes_table.go), so the id the
// request names is what each read takes, and each takes the row alone (loadArtifactRowByID): the
// versions and the approval an artifact read carries cost a second round trip, and neither
// handler reads them.
//
// The owner's own refusals come first, as every write to an owner takes them: a closed issue is
// refused once the document's row says which owner it hangs from, and before anything else of
// the document is read. An agent's conversation takes no lock because it holds no document
// (uploadArtifactFor), so the handler's own kind refusal is every answer an artifact of a
// conversation gets.
//
// The row is read again under that lock, so every refusal of the handler, and the document its
// event carries, read the row as it is now: the first read ran before the lock and a correction
// or a removal of the same document may have committed since.
//
// The caller rolls back the transaction it is answered, as it commits it; a preamble that
// refuses rolls its own transaction back and writes the refusal itself.
func (s *server) beginArtifactWrite(w http.ResponseWriter, r *http.Request, who model.Actor) (pgx.Tx, model.Artifact, artifactTarget, bool) {
	ctx := r.Context()
	artifact, err := loadArtifactRowByID(ctx, s.deps.Store.Pool, who, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return nil, model.Artifact{}, artifactTarget{}, false
	}
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return nil, model.Artifact{}, artifactTarget{}, false
	}
	refused := func(err error) (pgx.Tx, model.Artifact, artifactTarget, bool) {
		_ = tx.Rollback(ctx)
		s.writeHandlerError(w, err)
		return nil, model.Artifact{}, artifactTarget{}, false
	}
	var target artifactTarget
	switch {
	case artifact.IssueKey != nil:
		target = artifactTarget{IssueKey: artifact.IssueKey}
		if _, err := s.requireOpenOwnerStatus(ctx, tx, who, issueOwner(*artifact.IssueKey)); err != nil {
			return refused(err)
		}
	case artifact.Project != "":
		target = artifactTarget{Project: artifact.Project}
		// A project's document is its own owner (ownerForArtifact), and its event's append takes
		// that owner's lock (store.ArtifactOwner) and then advances the document row's last_seq.
		// Every other writer of the document takes that lock in its read phase and then appends
		// the same way (lockCommentOwner, docs' lockArtifactOwner), so a correction that met the
		// lock only at its append, after its update of the row, held the row such a writer waits
		// on while it waited on the lock that writer holds: a deadlock. The lock is taken here,
		// before any write, and after the project's upload lock, in the order a project upload
		// takes the two (storeArtifact: lockProjectArtifacts, then the document's owner), so the
		// correction and an upload of the same project wait on the upload lock holding neither.
		if err := lockProjectArtifacts(ctx, tx, artifact.Project); err != nil {
			return refused(err)
		}
		if err := store.LockOwner(ctx, tx, store.ArtifactOwner(artifact.ID)); err != nil {
			return refused(err)
		}
	}
	locked, err := loadArtifactRowByID(ctx, tx, who, artifact.ID)
	if err != nil {
		return refused(err)
	}
	return tx, locked, target, true
}

func (s *server) nextArtifactSlug(ctx context.Context, q queryer, target artifactTarget, name string) (string, error) {
	base := artifactSlug(name)
	query, owner := target.ownerLookup(artifactSlugTakenSQL, agentArtifactSlugTakenSQL)
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			candidate = fmt.Sprintf("%s-%d", base, suffix)
		}
		var inUse bool
		if err := q.QueryRow(ctx, query, owner, candidate).Scan(&inUse); err != nil {
			return "", err
		}
		if !inUse {
			return candidate, nil
		}
	}
}

// quiesceDocuments closes every live document on every process, flushing each through the store,
// and waits for the settlements in flight. Test-only: mounted by routes() only when
// Deps.TestHooksEnabled is set, so a browser-test harness can truncate its database between
// scenarios with no document the previous scenario had open still writing to it. It answers only
// once every process has closed its documents (runTestHookEverywhere): a room still open on
// another process would write to the tables the harness truncates next.
func (s *server) quiesceDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.runTestHookEverywhere(w, r, testHookCall{Hook: hookQuiesceDocuments})
}
