package api

// v2_documents.go is the document routes of /api/v2 (R1 of dispatch://INFRA-4071/spec): an
// artifact by id with what include= adds, its writes, its versions, its reviews, its asks, its
// comments, its references and its subscribers; the artifacts of an issue, of a project and of an
// agent's conversation, each a list a slug filters; and the uploads. Every artifact a read
// answers is one row, artifactV2: the artifact's own fields with its latest version number, never
// its versions, which are a list of their own (the route map's J2). The slug routes of /api/v1
// are filters of those lists (J7), and the edit and the named version of a slug are the writes of
// the artifact the list answers. Every route with a /api/v1 route runs that route's work - the
// list of a document's versions, which has none, reads the versions row 10 answered inside the
// artifact - each object its path names that Dispatch stores checked first and answered with its
// own 404 (R7): ARTIFACT_NOT_FOUND, BLOCK_NOT_FOUND, VERSION_NOT_FOUND, ISSUE_NOT_FOUND and
// PROJECT_NOT_FOUND, and a {session_id} keeps the answer /api/v1 gives it (R7).

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The refusals of the objects a document route's path names (dispatch://INFRA-4071/spec R7).
var (
	documentMissingRefusal = apicode.Refusal{Condition: apicode.DocumentMissing, Code: apicode.ArtifactNotFound}
	blockMissingRefusal    = apicode.Refusal{Condition: apicode.BlockMissing, Code: apicode.BlockNotFound}
	versionMissingRefusal  = apicode.Refusal{Condition: apicode.VersionMissing, Code: apicode.VersionNotFound}
	issueMissingRefusal    = apicode.Refusal{Condition: apicode.IssueMissing, Code: apicode.IssueNotFound}
	projectMissingRefusal  = apicode.Refusal{Condition: apicode.ProjectMissing, Code: apicode.ProjectNotFound}
)

// artifactRowText is what every description of a route answering artifacts says of the row.
const artifactRowText = "Each artifact is one row: its own fields (id, issue_key, project, session_id, ref_key, slug, " +
	"name, kind doc, image or file, primary, role, created_by, created_at), latest_version, the number of its newest " +
	"version, latest_version_at, when that version was written, and latest_version_size, the bytes it stores (each " +
	"null for an artifact with no version, and the size null for a version that records none), version_count, how " +
	"many versions it holds, and a document's approval {state, latest_version, version, by, at, reason, ask_id, ...}; " +
	"its versions are GET /api/v2/artifacts/{id}/versions."

// artifactIncludeText is what every description of a read of documents says include= adds.
const artifactIncludeText = "include=text adds to each document its canonical markdown, its whole-document token, " +
	"its version, the newest version whose stored markdown is that markdown, and unsettled, false there; " +
	unsettledText + " (unsettled is null on a row read without include=text); include=blocks adds its blocks, each " +
	"with its markdown range, token, reference counts, attributes, table and embedding state; include=text,blocks " +
	"reads all of them from one tree, so another session's write cannot split the blocks' ranges from the markdown " +
	"they index. A file or an image carries neither."

// artifactSlugText is what the descriptions of an issue's and a project's artifact lists say of
// slug=, which answers for the /api/v1 read by slug (J7).
const artifactSlugText = "slug=<slug or file name> keeps the artifacts at that slug and the documents of that exact " +
	"file name, as dispatch://<owner>/artifact/<slug> names one: one row, or more where one document's file name is " +
	"another's slug or documents share a file name."

// artifactUploadText is what the descriptions of the three uploads say alike.
const artifactUploadText = "JSON {name, content?, summary?, actor?, precondition?} stores a markdown document, or " +
	"multipart with a file part and the fields name, summary, actor and precondition stores the file: a text/markdown " +
	"file is a document, any other an image only when its bytes are a PNG, JPEG, GIF or WebP, stored under the type its " +
	"bytes are, and otherwise a file under its declared type. An upload under a name or slug the owner already holds is " +
	"that artifact's next version. An upload that replaces a document the owner already holds names the token of the " +
	"version it replaces, precondition {document}, the whole-document token include=text answers (a multipart " +
	"upload's precondition field holds the same JSON object): one that names none is 400 PRECONDITION_REQUIRED, and a " +
	"token behind the live document is 409 PRECONDITION_FAILED, whose current names the live token and the number of " +
	"the version the document stands at; both write nothing. A precondition naming blocks, or on an upload stored as a " +
	"file, is 400 INVALID_PRECONDITION, and one on a first upload of a name, which replaces no version, is 409 " +
	"PRECONDITION_FAILED. " +
	"An issue's plan, Verification document and decisions document are always documents: a file or an image under " +
	"the name or the slug of one of them (plan.md, verification.md, decisions.md; plan-md, verification-md, " +
	"decisions-md) is 400 ARTIFACT_ROLE_NAME, and a Markdown upload under such a name where an earlier upload stored " +
	"a file or an image makes it that document as its next version, its earlier versions still served as the files they " +
	"are. " +
	"Answers 201 {artifact, version}, artifact the artifact's row as it stands once the upload is stored (its own " +
	"fields, latest_version, latest_version_at, latest_version_size, version_count and a document's approval) and " +
	"version the version the upload wrote, for a new artifact and for a new version alike. A body that is neither " +
	"JSON nor multipart is 415 JSON_CONTENT_TYPE, and an empty JSON body 400 INVALID_JSON; 400 ARTIFACT_INPUT " +
	"(primary given, content beside a file, blank content under an existing document's name), ARTIFACT_KIND_MISMATCH, " +
	"ARTIFACT_ROLE_NAME, INVALID_ARTIFACT (no name, no file, a new name or key over 2692 bytes), 413 " +
	"REQUEST_TOO_LARGE past 25 MB or a markdown document past 1 MiB."

// v2DocumentRows are the document routes: rows 5; 4; 9; 10, 15 and 29; 11; 12; 13; 14; 16;
// 18; 19; 20, 89 and 161; 22; 23; 24; 25; 26 and 32; 27; 28; 30, 91 and 164; 6, 31, 92 and 165; 85,
// 87, 88 and 90; 86; 157, 159, 160, 162 and 163; and 158 of the route map, and the list of a
// document's versions, which row 10's versions[] member becomes.
func (s *server) v2DocumentRows() []routeRow {
	documentIDs := func() *idRoute { return &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound} }
	person := routeScopeSet{model.ScopePerson}
	// artifactList and askList are the List of an owner's artifacts and of a project document's
	// asks: no bounds of their own, so 50 and 200 (listBounds.limits), which each list's
	// description words its page from.
	artifactList, askList := listBounds{}, listBounds{}
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/agents/{session_id}/artifacts", Auth: authAny, Versions: onlyV2,
			Description: "List the files and images of an agent's conversation, oldest first. " + artifactRowText +
				" slug=<slug> keeps the one at ref_key agent/<session_id>/<slug>, as dispatch://agent/<session_id>/artifact/<slug> " +
				"names it; a conversation holds no document, so no file name is matched. limit 1 to 200 (default 50) and offset; " +
				"{items, total, limit, offset}. A session Dispatch holds no file of answers the empty list; a session_id that " +
				"is empty or holds /, ?, #, whitespace, a control character or <>\"'`[]| is 400 INVALID_SESSION_ID.",
			Handler:  s.listAgentArtifactsV2,
			Resource: "artifacts", Kind: kindCollection, Key: "{id}",
			Query:    agentArtifactsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[artifactV2]{}},
			Refusals: listRefusals(),
		},
		{
			Method: http.MethodPost, Pattern: "/agents/{session_id}/artifacts", Auth: authAny, Versions: onlyV2,
			Description: "Upload a file or image to an agent's conversation: ref_key agent/<session_id>/<slug>, addressed " +
				"dispatch://agent/<session_id>/artifact/<slug>. " + artifactUploadText + " A conversation holds files and " +
				"images only: a markdown document (JSON content or a text/markdown file) is 400 ARTIFACT_INPUT. No event is " +
				"appended. A caller whose credential proves a session of its own uploads only into that session: any other " +
				"session_id is 403 SESSION_SCOPE; a session_id that is no session id is 400 INVALID_SESSION_ID.",
			Handler:  s.uploadAgentArtifactV2,
			Resource: "artifacts", Kind: kindCollection, Key: "{id}",
			Body:  requestBody{Kind: bodyUpload, Type: jsonArtifactUpload{}},
			Shape: routeShape{Status: http.StatusCreated, Response: artifactUploadV2{}},
			Scope: scopeSession, SessionPath: "session_id",
			Refusals: []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodDelete, Pattern: "/artifacts/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Remove a document of an issue that is still untouched: made by typing a title and never written " +
				"in (the person scope). It answers 204 and appends artifact.removed on the issue, and it leaves no row behind. " +
				"A document that holds writing or a later version, an edit that has not settled, or a comment, ask, review, " +
				"approval request, check result, read mark or link from anything in Dispatch is 409 ARTIFACT_NOT_UNTOUCHED, " +
				"saying which of those it is; the issue's spec, a document that plays a role and an artifact that is no " +
				"document read the same refusal; a project's document is 400 ARTIFACT_PROJECT_DOCUMENT; a closed issue's 409 " +
				"ISSUE_CLOSED; 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.removeArtifact,
			Resource: "artifacts", Kind: kindItem, Key: "{id}",
			Shape:       routeShape{Status: http.StatusNoContent},
			OAuthScopes: person,
			Scope:       scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Read one artifact, of any owner and any kind, as every list of artifacts answers it. " +
				artifactRowText + " " + artifactIncludeText + " 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.getArtifactV2,
			Resource: "artifacts", Kind: kindItem, Key: "{id}",
			Query:    artifactReadV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: artifactV2{}},
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodPatch, Pattern: "/artifacts/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Correct a document's title {name} (the person scope). It writes the name alone - the slug, the " +
				"reference key it gives and the role the document plays do not move - appends artifact.renamed, and answers " +
				"the artifact as GET /api/v2/artifacts/{id} reads it. A name another of the owner's artifacts holds by name or " +
				"slug is 409 ARTIFACT_NAME_TAKEN, a name whose slug is one of the documents Dispatch makes itself, on a document " +
				"that is not that one, 400 ARTIFACT_ROLE_NAME, the issue's spec 400 ARTIFACT_PRIMARY, an artifact that is no " +
				"document 400 ARTIFACT_KIND_MISMATCH, a name that is empty or too long 400 INVALID_ARTIFACT, a closed issue's " +
				"document 409 ISSUE_CLOSED; 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.renameArtifactV2,
			Resource: "artifacts", Kind: kindItem, Key: "{id}",
			Body:        requestBody{Kind: bodyJSON, Type: renameArtifactInput{}},
			Shape:       routeShape{Status: http.StatusOK, Response: artifactV2{}},
			OAuthScopes: person,
			Scope:       scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/approval-requests", Auth: authAny, Versions: onlyV2,
			Description: "Open an approval ask for a document's latest version {summary?, include_verification?, actor?}, " +
				"any authenticated caller: include_verification puts the issue's Verification document beside the spec, each " +
				"at its own current version (409 NO_VERIFICATION_DOCUMENT when it has none, 409 NOT_SPEC_DOCUMENT on any document " +
				"but the issue's spec). A document a person approved before carries that approval on the ask. An open request " +
				"over a document it names is renewed or handed back to its person rather than a second one opened, and a " +
				"document approved throughout at its current version needs none. Answers {ask, artifact_id, version, approval, " +
				"coverage, coverage_unread?}: 201 when the request wrote anything, 200 when it stood as asked (ask null when " +
				"nothing is asked); coverage is one line per requirement of the spec with no check, or `the spec marks no " +
				"requirements`, and refuses nothing. 409 PLAN_APPROVED_BY_REVIEW for a plan, 409 NO_VERSION, 409 ISSUE_CLOSED, " +
				"400 SUMMARY_INPUT for a blank summary, 400 NOT_DOCUMENT, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.requestArtifactApprovalV2,
			Resource: "approval-requests", Kind: kindCollection, Key: "{id}",
			Body: requestBody{Kind: bodyJSON, Type: approvalRequestInput{}, Optional: true},
			Shape: routeShape{
				Status: http.StatusCreated, Response: approvalRequestV2{},
				Also: []routeAnswer{{Status: http.StatusOK, Body: approvalRequestV2{}}},
			},
			Scope: scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/asks", Auth: authAny, Versions: onlyV2,
			Description: "List a project document's asks, oldest first, each as GET /api/v2/asks answers it. state=all " +
				"(the default), open, answered or resolved. " + askRowText + " " + includedPageLimitText(askList, artifactAsksV2Query{}) +
				" {items, total, limit, offset}. An issue's document is 400 ARTIFACT_LINKED, its asks being GET " +
				"/api/v2/issues/{key}/asks, and a file of an agent's conversation 400 ARTIFACT_AGENT_OWNED; 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.listArtifactAsksV2,
			Resource: "asks", Kind: kindCollection, Key: "{id}",
			Query:       artifactAsksV2Query{},
			List:        askList,
			Shape:       routeShape{Status: http.StatusOK, Response: page[askV2]{}},
			Authorities: []authorityRule{askMarkAuthority},
			IDs:         documentIDs(),
			Refusals:    listRefusals(documentMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/asks", Auth: authAny, Versions: onlyV2,
			Description: "Open a question ask on a project document {question, options?, multiple?, urgency?, anchor?}, a " +
				"bearer naming its session in actor: at most 8 options with unique labels, urgency low, med (the default), high " +
				"or blocking. Answers 201 the ask as GET /api/v2/asks/{id} reads it, with advice beside it. After the write " +
				"commits, a related-item check searches the document's project for the question and waits at most 5 seconds; " +
				"the advice's suggestions say whether it found related items (outcome, with the reason in missing when not " +
				"made), how it searched, and the three items most like it with each ask item's answer state. 400 INVALID_ASK or " +
				"ASK_KIND_INPUT for a question it refuses, 400 CAP_EXCEEDED past a bound, 400 ARTIFACT_LINKED for an issue's " +
				"document, 400 ARTIFACT_AGENT_OWNED for a conversation's file, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.createArtifactAskV2,
			Resource: "asks", Kind: kindCollection, Key: "{id}",
			Body: requestBody{Kind: bodyJSON, Type: createAskInput{}},
			Shape: routeShape{
				Status: http.StatusCreated, Response: askV2{}, Advice: writeAdvice{},
				OtherAdvice: []any{suggestionsOnlyAdvice{}},
			},
			Scope: scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/blocks/{block_id}", Auth: authAny, Versions: onlyV2,
			Description: "Where one block stands in the live document: its path from the top-level block down (type, id, " +
				"child index), and for a table block, row or cell the table's id, the row index (0 is the header), the cell's " +
				"column index, the text of the header cell drawn above it and the row's cells. block_id is the block's whole id " +
				"or a unique prefix of 8 characters or more. 404 BLOCK_NOT_FOUND for a block the live document does not hold, " +
				"400 NOT_DOCUMENT for a file or an image, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.getArtifactBlockPathV2,
			Resource: "blocks", Kind: kindItem, Key: "{block_id}",
			Shape: routeShape{Status: http.StatusOK, Response: model.BlockPath{}},
			IDs: &idRoute{
				Kind: "artifact", Blocks: true, NotFound: apicode.ArtifactNotFound, BlockNotFound: apicode.BlockNotFound,
			},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal, blockMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/comments", Auth: authAny, Versions: onlyV2,
			Description: "List a project document's comments, every comment and reply, oldest first, each as the issue " +
				"comment list answers it. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. The answer " +
				"carries an ETag, which moves for every write the list shows, a delivery attempt that appends no event " +
				"included, and for every page; If-None-Match answers 304. An issue's document is 400 ARTIFACT_LINKED, its " +
				"comments being its issue's, and a file of an agent's conversation 400 ARTIFACT_AGENT_OWNED; 404 " +
				"ARTIFACT_NOT_FOUND.",
			Handler:  s.listArtifactCommentsV2,
			Resource: "comments", Kind: kindCollection, Key: "{id}",
			Query: artifactCommentsV2Query{},
			Shape: routeShape{
				Status: http.StatusOK, Response: page[shapedComment]{},
				Also: []routeAnswer{{Status: http.StatusNotModified}},
			},
			IDs:      documentIDs(),
			Refusals: listRefusals(documentMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/comments", Auth: authAny, Versions: onlyV2,
			Description: "Comment on a project document or one of its quotes {body, anchor?, reply_to?, reply_to_block?, " +
				"ask_id?, mentions?, delivery?, turn?, suggestion?}, a bearer naming its session in actor: a reply names the " +
				"comment it answers in reply_to, and a suggestion {replace_with} proposes text for its anchored quote. Answers " +
				"201 the comment with ask_waiting_on and advice beside it; a repeat under its Idempotency-Key answers the " +
				"comment as it stands, its owed mentions sent again. 400 INVALID_COMMENT for an empty body, 400 CAP_EXCEEDED " +
				"past a bound, 400 ARTIFACT_LINKED for an issue's document, 400 ARTIFACT_AGENT_OWNED for a conversation's file, " +
				"404 ARTIFACT_NOT_FOUND.",
			Handler:  s.createArtifactCommentV2,
			Resource: "comments", Kind: kindCollection, Key: "{id}",
			Body:  requestBody{Kind: bodyJSON, Type: commentInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: commentWriteResponse{}, Advice: writeAdvice{}},
			Scope: scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Replay:   replayRecovered(s, s.replayComment),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/edits", Auth: authAny, Versions: onlyV2,
			Description: "Apply a batch of quote- and block-anchored edit ops to a document {ops, summary?, precondition?, " +
				"actor?}, a bearer naming its session in actor: each op {op, find?, with?, occurrence?, markdown?, after?, " +
				"before?, block?, index?, type?, attributes?}; precondition {document?, blocks?} holds the batch to the tokens " +
				"the caller read. A batch that changed the document writes a version, named with the summary, else a snapshot, " +
				"and appends artifact.version. Answers 200 {applied, changed, lost_ops, token, unchanged_ops, version, written} " +
				"with advice. 409 PRECONDITION_FAILED {code, error, current, mismatches} for a precondition the document no " +
				"longer meets, 409 EDIT_LOST_TO_CONCURRENT_CHANGE {code, error, lost_ops or suggestion, participants} for text " +
				"a concurrent change removed first, 409 TARGET_AMBIGUOUS {code, error, candidates} for a quote found in several " +
				"places, 409 ISSUE_CLOSED, 400 INVALID_OP, 400 NOT_DOCUMENT, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.editArtifactV2,
			Resource: "edits", Kind: kindCollection,
			Body: requestBody{Kind: bodyJSON, Type: editArtifactInput{}},
			Shape: routeShape{
				Status: http.StatusOK, Response: editArtifactResponse{}, Advice: writeAdvice{},
				// A document of no issue, or one whose issue advice could not be read, carries the
				// document half alone (applyEdits, the withRawAdvice arm).
				OtherAdvice: []any{documentWriteAdvice{}},
				Refusals: []routeAnswer{
					{Status: http.StatusConflict, Body: editPreconditionError{}},
					{Status: http.StatusConflict, Body: editLostError{}},
					{Status: http.StatusConflict, Body: ambiguousTargetError{}},
				},
			},
			Scope: scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/plan-reviews", Auth: authAny, Versions: onlyV2,
			Description: "Record a plan review's approval of a plan version {version, actor?}, any authenticated caller, on " +
				"an issue's plan.md alone, and only while it is a document (409 NOT_PLAN_DOCUMENT otherwise; for a plan an " +
				"upload stored as a file or an image, the refusal names its kind, and a Markdown upload under its name makes " +
				"it a document): version is any settled version from 1 to the latest (400 INVALID_REVIEW otherwise). A plan " +
				"is never sent to a person for approval, so POST /api/v2/artifacts/{id}/approval-requests refuses it with 409 " +
				"PLAN_APPROVED_BY_REVIEW; a later plan version makes this approval stale, as a spec's goes stale. Answers 201 " +
				"the review. 409 NO_VERSION, 409 ISSUE_CLOSED, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.createArtifactPlanReviewV2,
			Resource: "plan-reviews", Kind: kindCollection, Key: "{id}",
			Body:  requestBody{Kind: bodyJSON, Type: createPlanReviewInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: model.ArtifactReview{}},
			Scope: scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/rebuilds", Auth: authAny, Versions: onlyV2,
			Description: "Rebuild a document history ygo cannot load (the person scope) {markdown?}: the history is replaced " +
				"by one seed update from the latest saved version, or from the markdown given, which writes a version and " +
				"appends artifact.version where it differs from that version. Answers 200 the report {head, " +
				"removed_checkpoints, removed_snapshots, removed_updates, source_version, validation_error}. A document a room " +
				"holds open is 409 DOCUMENT_LIVE, one whose history loads 409 DOCUMENT_LOADS, markdown given to a closed issue's " +
				"document 409 ISSUE_CLOSED, a file or an image 400 NOT_DOCUMENT; 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.rebuildArtifactV2,
			Resource: "rebuilds", Kind: kindCollection,
			Body:        requestBody{Kind: bodyJSON, Type: rebuildArtifactInput{}},
			Shape:       routeShape{Status: http.StatusOK, Response: docs.RebuildReport{}},
			OAuthScopes: person,
			Scope:       scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/references", Auth: authAny, Versions: onlyV2,
			Description: "The mentions of a document, by kind and id. direction=in (the default) lists the items that cite " +
				"it, one row per citing item {kind, id, issue_key?, project?, excerpt?, ref_key?}, each ? absent where the " +
				"item has none (an empty excerpt among them), an ask citing it from an option or an answer among them; " +
				"direction=out the items it cites {kind, to_id, artifact?}, artifact the cited " +
				"document's row where the item is one. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. " +
				"GET /api/v2/references reads every edge kind of one node, a page by position. 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.listArtifactReferencesV2,
			Resource: "references", Kind: kindCollection,
			Query:    artifactReferencesV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[artifactReferenceV2]{}},
			IDs:      documentIDs(),
			Refusals: listRefusals(documentMissingRefusal),
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/reviews", Auth: authAny, Versions: onlyV2,
			Description: "List a document's reviews, newest first in the order their events committed: each {id, " +
				"artifact_id, version, state, kind (person or plan_review), actor, reason, ask_id, created_at, content_sha256, " +
				"content_sha256_source}; a file or an image has none. limit 1 to 200 (default 50) and offset; {items, total, " +
				"limit, offset}. 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.listArtifactReviewsV2,
			Resource: "reviews", Kind: kindCollection, Key: "{id}",
			Query:    artifactReviewsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[artifactReviewRead]{}},
			IDs:      documentIDs(),
			Refusals: listRefusals(documentMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/reviews", Auth: authAny, Versions: onlyV2,
			Description: "Approve or request changes on a document {state: approved or changes_requested, reason?, version?} " +
				"(the person and approve scopes); changes_requested needs a reason (400 REASON_REQUIRED), and any other state " +
				"is 400 INVALID_REVIEW. The review records the document's latest settled version, or, with an approval ask " +
				"open, the version that ask names for it, and then answers that ask, preserving its thread, and reviews every " +
				"document it named, each at the version it names. version is the version the caller read, a guard: a review " +
				"that would record another is 409 REVIEW_VERSION_MOVED, naming the version it would record, with nothing " +
				"written, and a version that is an integer below 1 is 400 INVALID_REVIEW (one that is no integer 400 " +
				"INVALID_JSON). Answers 201 the review of " +
				"this document. 409 NO_VERSION, 409 ISSUE_CLOSED, 400 NOT_DOCUMENT, 400 CAP_EXCEEDED for a reason past the " +
				"bound, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.createArtifactReviewV2,
			Resource: "reviews", Kind: kindCollection, Key: "{id}",
			Body:        requestBody{Kind: bodyJSON, Type: artifactReviewV2Input{}},
			Shape:       routeShape{Status: http.StatusCreated, Response: model.ArtifactReview{}},
			OAuthScopes: routeScopeSet{model.ScopePerson, model.ScopeApprove},
			Scope:       scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/subscribers", Auth: authAny, Versions: onlyV2,
			Description: "The sessions subscribed to a project document's topics (the person scope), newest heartbeat first: " +
				"each {session_id, title, live, state?, last_seen, topics, removable, via?}, removable false where only a " +
				"broader topic matches, which via names. limit 1 to 200 (default 50) and offset; {items, total, limit, offset}. " +
				"An issue's document is 400 ARTIFACT_LINKED, a conversation's file 400 ARTIFACT_AGENT_OWNED; 503 " +
				"ENVOY_UNAVAILABLE or ENVOY_REFILLING when the listener cannot answer; 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.listArtifactSubscribersV2,
			Resource: "subscribers", Kind: kindCollection, Key: "{session_id}",
			Query:       artifactSubscribersV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[subscriber]{}},
			OAuthScopes: person,
			IDs:         documentIDs(),
			Refusals:    listRefusals(documentMissingRefusal),
		},
		{
			Method: http.MethodDelete, Pattern: "/artifacts/{id}/subscribers/{session_id}", Auth: authAny, Versions: onlyV2,
			Description: "Unsubscribe a session from a project document's topics (the person scope): the topics scoped to this " +
				"document alone are removed from the session's Envoy interests, and subscription.remove_requested and then " +
				"subscription.removed are appended on the document. 204. 409 NOT_REMOVABLE where only a broader topic matches, " +
				"404 SUBSCRIBER_NOT_FOUND for a session that is not subscribed, 502 ENVOY_UNSUBSCRIBE_FAILED, 503 " +
				"ENVOY_UNAVAILABLE, 400 ARTIFACT_LINKED or ARTIFACT_AGENT_OWNED, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.unsubscribeArtifactSession,
			Resource: "subscribers", Kind: kindItem, Key: "{session_id}",
			Shape:       routeShape{Status: http.StatusNoContent},
			OAuthScopes: person,
			Scope:       scopeProject, ProjectOf: projectPathArtifact, SessionPath: "session_id",
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/versions", Auth: authAny, Versions: onlyV2,
			Description: "List an artifact's versions: each {number, named, summary, authors, created_at, size?, mime?, " +
				"sha256?}, no markdown. sort=number (the default, oldest first) or -number. limit 1 to 200 (default 50) and " +
				"offset; {items, total, limit, offset}. GET /api/v2/artifacts/{id}/versions/{number} reads one. 404 " +
				"ARTIFACT_NOT_FOUND.",
			Handler:  s.listArtifactVersionsV2,
			Resource: "versions", Kind: kindCollection, Key: "{number}",
			Query:    artifactVersionsV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[model.Version]{}},
			IDs:      documentIDs(),
			Refusals: listRefusals(documentMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/artifacts/{id}/versions", Auth: authAny, Versions: onlyV2,
			Description: "Name a document's current version {summary, actor?}, a bearer naming its session in actor: a new " +
				"named version of the live document, and artifact.version appended. Answers 201 the version. 400 " +
				"INVALID_VERSION for a blank summary, 400 NOT_DOCUMENT, 409 ISSUE_CLOSED, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.createNamedVersionV2,
			Resource: "versions", Kind: kindCollection, Key: "{number}",
			Body:  requestBody{Kind: bodyJSON, Type: createVersionInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: model.Version{}},
			Scope: scopeProject, ProjectOf: projectPathArtifact,
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/artifacts/{id}/versions/{number}", Auth: authAny, Versions: onlyV2,
			Description: "One version of a document, a file or an image, of any owner. A document version answers as JSON " +
				"{markdown, number, named, summary, authors, created_at, size, mime?, sha256}; a file or an image version answers " +
				"its bytes under its own content type, with Content-Disposition: attachment naming the artifact's name, " +
				"-v<number> before the extension for an older version (report-v2.pdf), and Cache-Control: private, " +
				"max-age=31536000, immutable. download=true answers every kind with Content-Disposition: attachment under that " +
				"name and Cache-Control: private, no-cache, and a document as its Markdown, text/markdown, its name ending in " +
				".md when the stored name has no Markdown extension. A version's author carries proven: true only when the " +
				"credential of every write that author made to the version proved it. 404 VERSION_NOT_FOUND for a number " +
				"that names no version of it, 404 ARTIFACT_NOT_FOUND.",
			Handler:  s.getArtifactVersionV2,
			Resource: "versions", Kind: kindBytes, Key: "{number}",
			Query: artifactVersionV2Query{},
			Shape: routeShape{
				Status: http.StatusOK, Response: artifactVersionText{},
				Opaque: "a file version answers that file's bytes under its own content type, named by Content-Disposition (serveFileVersion); with download=true a document version answers its Markdown as text/markdown; only a document version read without that parameter carries the JSON above",
				// A file's own content type is any type: `*/*`, the media range OpenAPI keys such
				// an answer by.
				OpaqueContentTypes: []string{"*/*", "text/markdown"},
			},
			IDs:      documentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, documentMissingRefusal, versionMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/artifacts", Auth: authAny, Versions: onlyV2,
			Description: "List an issue's documents, files and images, its spec among them, oldest first. " + artifactRowText +
				" " + artifactSlugText + " " + artifactIncludeText + " " + includedPageLimitText(artifactList, artifactListV2Query{}) +
				" {items, total, limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
			Handler:  s.listIssueArtifactsV2,
			Resource: "artifacts", Kind: kindCollection, Key: "{id}",
			Query:         artifactListV2Query{},
			List:          artifactList,
			Shape:         routeShape{Status: http.StatusOK, Response: page[artifactV2]{}},
			DocumentOwner: documentOwnerIssue,
			Refusals:      listRefusals(issueMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/artifacts", Auth: authAny, Versions: onlyV2,
			Description: "Upload an issue document or file. " + artifactUploadText + " The write's advice rides the answer. " +
				"409 ISSUE_CLOSED for a closed issue and 404 ISSUE_NOT_FOUND for one that does not exist, both answered before " +
				"the body is read.",
			Handler:  s.uploadIssueArtifactV2,
			Resource: "artifacts", Kind: kindCollection, Key: "{id}",
			Body: requestBody{Kind: bodyUpload, Type: jsonArtifactUpload{}},
			// An issue document whose issue advice could not be read carries the document half
			// alone (storeArtifact, the withRawAdvice arm).
			Shape: routeShape{
				Status: http.StatusCreated, Response: artifactUploadV2{}, Advice: writeAdvice{},
				OtherAdvice: []any{documentWriteAdvice{}},
			},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			RefusesClosedOwner: true,
			Refusals:           []apicode.Refusal{undeclaredKeyRefusal, issueMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/projects/{key}/artifacts", Auth: authAny, Versions: onlyV2,
			Description: "List a project's documents, files and images, oldest first: its own and every one of its issues' " +
				"but each issue's spec. issue=none keeps the project's own, which no issue holds, and issue=<key> one issue's. " +
				artifactRowText + " " + artifactSlugText + " " + artifactIncludeText + " " +
				includedPageLimitText(artifactList, projectArtifactsV2Query{}) + " {items, total, limit, offset}. 404 " +
				"PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.listProjectArtifactsV2,
			Resource: "artifacts", Kind: kindCollection, Key: "{id}",
			Query:         projectArtifactsV2Query{},
			List:          artifactList,
			Shape:         routeShape{Status: http.StatusOK, Response: page[artifactV2]{}},
			DocumentOwner: documentOwnerProject,
			Refusals:      listRefusals(projectMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/projects/{key}/artifacts", Auth: authAny, Versions: onlyV2,
			Description: "Upload a project document or file, which no issue holds; its events are its own. " +
				artifactUploadText + " 404 PROJECT_NOT_FOUND for a project that does not exist.",
			Handler:  s.uploadProjectArtifactV2,
			Resource: "artifacts", Kind: kindCollection, Key: "{id}",
			Body: requestBody{Kind: bodyUpload, Type: jsonArtifactUpload{}},
			// A project document has no issue state to report, so its write advice is the document
			// half alone (storeArtifact, the withRawAdvice arm).
			Shape: routeShape{Status: http.StatusCreated, Response: artifactUploadV2{}, Advice: documentWriteAdvice{}},
			Scope: scopeProject, ProjectOf: projectPathProject,
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, projectMissingRefusal},
		},
	}
}

// artifactV2 is one artifact as every /api/v2 read and list of artifacts answers it (the route
// map's J2): the artifact's own fields, what a reader shows of its versions - the number, the time
// and the size of its newest and how many there are - in place of the versions themselves, which
// GET /api/v2/artifacts/{id}/versions lists, and a document's approval; it is the row GET
// /api/v1/projects/{key}/artifacts/{slug}/summary answered, and more. include= adds a document's
// text and blocks (J1). It is a type of its own, so no /api/v1 shape's tags change
// (dispatch://INFRA-4071/spec R3).
type artifactV2 struct {
	ID        string      `json:"id"`
	IssueKey  *string     `json:"issue_key"`
	Project   string      `json:"project"`
	SessionID *string     `json:"session_id"`
	RefKey    string      `json:"ref_key"`
	Slug      string      `json:"slug"`
	Name      string      `json:"name"`
	Kind      string      `json:"kind"`
	Primary   bool        `json:"primary"`
	Role      string      `json:"role,omitempty"`
	CreatedBy model.Actor `json:"created_by"`
	CreatedAt time.Time   `json:"created_at"`
	// LatestVersion, LatestVersionAt and LatestVersionSize are the number of the artifact's newest
	// version, when it was written and how many bytes it stores (model.Version's Size), each null
	// for an artifact with none and the size null for a version that records none; VersionCount
	// is how many versions it holds. Each is on every row, the three pointers with no omitempty, so
	// a list shows them without a read of each artifact's versions.
	LatestVersion     *int       `json:"latest_version"`
	LatestVersionAt   *time.Time `json:"latest_version_at"`
	LatestVersionSize *int       `json:"latest_version_size"`
	VersionCount      int        `json:"version_count"`
	// Approval is a document's approval as of its latest version; absent for a file or an image.
	Approval *artifactApprovalRead `json:"approval,omitempty"`
	// Markdown, Token and Version are a document's canonical markdown, its whole-document token
	// and the version that markdown is, with include=text (documentText): the newest version whose
	// stored markdown is Markdown, else the newest version. The token is the write's concurrency key.
	Markdown *string `json:"markdown,omitempty"`
	// MarkdownTree is the tree Markdown is shown as, for a caller that sent `Dispatch-Trees: 1`.
	MarkdownTree model.DisplayTree `json:"markdown_tree,omitempty"`
	Token        *string           `json:"token,omitempty"`
	Version      *int              `json:"version,omitempty"`
	// Unsettled is false where Version's stored markdown is Markdown, and true when Markdown equals
	// no stored version's markdown: the document holds edits no version holds yet (a room's
	// unsettled typing), or its newest version was rendered by an older build; Version is then the
	// newest version, not the version of this text. It is on every row that carries Markdown
	// (include=text on a document), and null on a row that carries none, one read without
	// include=text or a file or an image, so it never says false of text the read did not answer.
	// A pointer with no omitempty: the member is on every row, required and nullable.
	Unsettled *bool `json:"unsettled"`
	// Blocks are a document's blocks, with include=blocks: from the tree Markdown was rendered
	// from where include names text too.
	Blocks *[]model.ArtifactBlock `json:"blocks,omitempty"`
}

// artifactV2Include is what include= adds to each document row.
type artifactV2Include struct {
	text, blocks bool
}

// artifactIncludeOf reads include's members.
func artifactIncludeOf(include []string) artifactV2Include {
	return artifactV2Include{text: slices.Contains(include, "text"), blocks: slices.Contains(include, "blocks")}
}

// newArtifactV2 is artifact as a row, read with its versions and its approval
// (loadArtifactDetails): every version is read already, in one statement for every artifact of
// the read, so the facts of the newest and the count read no more.
func newArtifactV2(artifact model.Artifact) artifactV2 {
	row := artifactV2{
		ID: artifact.ID, IssueKey: artifact.IssueKey, Project: artifact.Project, SessionID: artifact.SessionID,
		RefKey: artifact.RefKey, Slug: artifact.Slug, Name: artifact.Name, Kind: artifact.Kind,
		Primary: artifact.Primary, Role: artifact.Role, CreatedBy: artifact.CreatedBy, CreatedAt: artifact.CreatedAt,
		VersionCount: len(artifact.Versions),
	}
	for index := range artifact.Versions {
		version := &artifact.Versions[index]
		if row.LatestVersion == nil || version.Number > *row.LatestVersion {
			row.LatestVersion, row.LatestVersionAt, row.LatestVersionSize = &version.Number, &version.CreatedAt, version.Size
		}
	}
	if artifact.Approval != nil {
		row.Approval = &artifactApprovalRead{ArtifactApproval: *artifact.Approval}
	}
	return row
}

// artifactRowsV2 is artifacts, each read with its details, as rows, include's members on each
// document, and the trees of every approval's reason and every markdown for a caller that asked
// for them.
func (s *server) artifactRowsV2(ctx context.Context, r *http.Request, artifacts []model.Artifact, include artifactV2Include) ([]artifactV2, error) {
	rows := make([]artifactV2, len(artifacts))
	for index := range artifacts {
		row := &rows[index]
		*row = newArtifactV2(artifacts[index])
		if row.Kind != "doc" {
			continue
		}
		switch {
		case include.text:
			text, err := s.documentText(ctx, row.ID, include.blocks)
			if err != nil {
				return nil, err
			}
			row.Markdown, row.Token, row.Version, row.Unsettled, row.Blocks = &text.Markdown, &text.Token, text.Version, &text.Unsettled, text.Blocks
		case include.blocks:
			blocks, err := s.documentBlocks(ctx, row.ID)
			if err != nil {
				return nil, err
			}
			// A document with no stored state has no blocks, which the member answers as the
			// empty list, as include=text,blocks does.
			if blocks == nil {
				blocks = []model.ArtifactBlock{}
			}
			row.Blocks = &blocks
		}
	}
	reader := s.readDisplayTrees(ctx, wantsDisplayTrees(r))
	for index := range rows {
		if approval := rows[index].Approval; approval != nil {
			reader.textOf(&approval.ReasonTree, approval.Reason)
		}
		if rows[index].Markdown != nil {
			reader.text(&rows[index].MarkdownTree, *rows[index].Markdown)
		}
	}
	reader.resolve()
	return rows, nil
}

// artifactReadV2Query is GET /api/v2/artifacts/{id}'s query.
type artifactReadV2Query struct {
	Include []string `query:"include,comma" enum:"text,blocks"`
}

// artifactListV2Query is GET /api/v2/issues/{key}/artifacts's query.
type artifactListV2Query struct {
	pageQuery
	// Slug keeps the artifacts at this slug and the documents of this exact file name.
	Slug string `query:"slug"`
	// Include adds each document's text (text) and its blocks (blocks), each of which renders
	// each listed row whole, so a page that names one holds at most includedPageMax (renders).
	Include []string `query:"include,comma" enum:"text,blocks" renders:"text,blocks"`
}

// projectArtifactsV2Query is GET /api/v2/projects/{key}/artifacts's query.
type projectArtifactsV2Query struct {
	pageQuery
	// Issue is none for the project's own artifacts, which no issue holds, or one issue's key.
	Issue queryFilter[string] `query:"issue"`
	Slug  string              `query:"slug"`
	// Include adds each document's text (text) and its blocks (blocks), each of which renders
	// each listed row whole, so a page that names one holds at most includedPageMax (renders).
	Include []string `query:"include,comma" enum:"text,blocks" renders:"text,blocks"`
}

// agentArtifactsV2Query is GET /api/v2/agents/{session_id}/artifacts's query.
type agentArtifactsV2Query struct {
	pageQuery
	// Slug keeps the one artifact at this slug: a conversation holds no document, so no file name
	// is matched (loadAgentArtifact).
	Slug string `query:"slug"`
}

// artifactList is one statement over the artifacts a /api/v2 list reads: the conditions its rows
// meet and the arguments they bind.
type artifactList struct {
	conditions []string
	arguments  []any
}

// argument binds value and answers its placeholder.
func (list *artifactList) argument(value any) string {
	list.arguments = append(list.arguments, value)
	return "$" + strconv.Itoa(len(list.arguments))
}

// where adds a condition every row meets.
func (list *artifactList) where(condition string) {
	list.conditions = append(list.conditions, condition)
}

// bySlugOrName keeps the artifacts at slug and the documents whose file name it is, the two
// steps a /api/v1 read by slug resolves (resolveDocumentReference) as one filter.
func (list *artifactList) bySlugOrName(slug string) {
	if slug == "" {
		return
	}
	placeholder := list.argument(slug)
	list.where("(slug = " + placeholder + " or (kind = 'doc' and name = " + placeholder + "))")
}

// answerArtifactList answers one page of the artifacts list holds, oldest first, as artifact rows
// with include's members, and the count of them all.
func (s *server) answerArtifactList(w http.ResponseWriter, r *http.Request, list artifactList, query pageQuery, include []string) {
	ctx, pool := r.Context(), s.deps.Store.Pool
	clause := strings.Join(list.conditions, " and ")
	var total int
	if err := pool.QueryRow(ctx, `select count(*) from artifacts where `+clause, list.arguments...).Scan(&total); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("count the artifacts: %w", err))
		return
	}
	limit, offset := list.argument(query.Limit), list.argument(query.Offset)
	rows, err := pool.Query(ctx, `
		select `+artifactColumns+`
		from artifacts where `+clause+`
		order by created_at, id limit `+limit+` offset `+offset, list.arguments...)
	if err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read the artifacts: %w", err))
		return
	}
	artifacts, err := collectArtifacts(rows)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := loadArtifactDetails(ctx, pool, artifactPointers(artifacts)); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	items, err := s.artifactRowsV2(ctx, r, artifacts, artifactIncludeOf(include))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(items, total, query))
}

// requireArtifactV2 answers whether the artifact id names exists, and where it does not writes 404
// ARTIFACT_NOT_FOUND (requireHeldV2).
func (s *server) requireArtifactV2(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.requireHeldV2(w, r, id, "artifact", `select exists (select 1 from artifacts where id = $1)`,
		apicode.ArtifactNotFound, "artifact not found")
}

// listAgentArtifactsV2 answers GET /api/v2/agents/{session_id}/artifacts: the files and images of
// one conversation, the list GET /api/v1/agents/{session_id}/artifacts/{slug} read one row of.
func (s *server) listAgentArtifactsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	query := v2Query[agentArtifactsV2Query](r)
	var list artifactList
	list.where("session_id = " + list.argument(session))
	if query.Slug != "" {
		list.where("slug = " + list.argument(query.Slug))
	}
	s.answerArtifactList(w, r, list, query.pageQuery, nil)
}

// listIssueArtifactsV2 answers GET /api/v2/issues/{key}/artifacts: an issue's artifacts, as GET
// /api/v1/issues/{key}/artifacts lists them, filtered as its slug reads resolve one.
func (s *server) listIssueArtifactsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[artifactListV2Query](r)
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	var list artifactList
	list.where("issue_key = " + list.argument(key))
	list.bySlugOrName(query.Slug)
	s.answerArtifactList(w, r, list, query.pageQuery, query.Include)
}

// listProjectArtifactsV2 answers GET /api/v2/projects/{key}/artifacts: a project's artifacts, as
// GET /api/v1/projects/{key}/artifacts lists them, issue=none in place of unlinked=true, filtered
// as its slug reads resolve one.
func (s *server) listProjectArtifactsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[projectArtifactsV2Query](r)
	key := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, key); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	var list artifactList
	list.where("project_key = " + list.argument(key))
	list.where("not is_primary")
	switch {
	case query.Issue.None:
		list.where("issue_key is null")
	case query.Issue.Set:
		list.where("issue_key = " + list.argument(query.Issue.Value))
	}
	list.bySlugOrName(query.Slug)
	s.answerArtifactList(w, r, list, query.pageQuery, query.Include)
}

// getArtifactV2 answers GET /api/v2/artifacts/{id}: one artifact by id, as GET
// /api/v1/artifacts/{id} reads it (loadArtifact), with what include= adds in place of the /text and
// /blocks reads.
func (s *server) getArtifactV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	rows, err := s.artifactRowsV2(r.Context(), r, []model.Artifact{artifact}, artifactIncludeOf(v2Query[artifactReadV2Query](r).Include))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, rows[0])
}

// renameArtifactV2 answers PATCH /api/v2/artifacts/{id}: the title correction PATCH
// /api/v1/artifacts/{id} makes (renameArtifactTo), answered as the artifact row.
func (s *server) renameArtifactV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	renamed, ok := s.renameArtifactTo(w, r, actor, v2Body[renameArtifactInput](r))
	if !ok {
		return
	}
	rows, err := s.artifactRowsV2(r.Context(), r, []model.Artifact{renamed}, artifactV2Include{})
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, rows[0])
}

// requestArtifactApprovalV2 answers POST /api/v2/artifacts/{id}/approval-requests: the request POST
// /api/v1/artifacts/{id}/approval-requests makes (requestApproval), answered with the ask as every
// /api/v2 write of one answers it (askV2), read on the write's transaction (writtenAskV2), so the
// request's ask carries its opened_event_id (R6).
func (s *server) requestArtifactApprovalV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireArtifactV2(w, r, r.PathValue("id")) {
		return
	}
	written := &writtenAskV2{server: s}
	status, answer, answered := s.requestApproval(w, r, v2Body[approvalRequestInput](r), written.readBack)
	if !answered {
		return
	}
	reply := approvalRequestV2{
		ArtifactID: answer.ArtifactID, Version: answer.Version, Approval: answer.Approval,
		Coverage: answer.Coverage, CoverageUnread: answer.CoverageUnread,
	}
	if answer.Ask != nil {
		ask, err := written.answer(r)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		reply.Ask = &ask
	}
	WriteJSON(w, status, reply)
}

// approvalRequestV2 is the body POST /api/v2/artifacts/{id}/approval-requests answers:
// approvalRequestResponse's, its ask the one ask row of /api/v2 rather than the /api/v1 shape,
// whose opened_event_id the /api/v1 document leaves optional (dispatch://INFRA-4071/spec R6).
type approvalRequestV2 struct {
	// Ask is the request's ask, null where nothing is asked: every document it names is approved
	// at its current version already.
	Ask        *askV2               `json:"ask"`
	ArtifactID string               `json:"artifact_id"`
	Version    int                  `json:"version"`
	Approval   artifactApprovalRead `json:"approval"`
	// Coverage and CoverageUnread are approvalRequestResponse's.
	Coverage       []string `json:"coverage"`
	CoverageUnread bool     `json:"coverage_unread,omitempty"`
}

// artifactAsksV2Query is GET /api/v2/artifacts/{id}/asks's query: the keys an issue's ask list
// takes, its state and what include= adds to each row.
type artifactAsksV2Query = issueAsksV2Query

// listArtifactAsksV2 answers GET /api/v2/artifacts/{id}/asks: a project document's asks, oldest
// first, one page of them, each the row every list of asks answers.
func (s *server) listArtifactAsksV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, _, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	query := v2Query[artifactAsksV2Query](r)
	include, ok := s.askIncludeOf(w, r, query.Include)
	if !ok {
		return
	}
	read := askV2Read{order: "a.created_at, a.id", markOf: include.markOf}
	read.where("a.artifact_id = " + read.argument(artifact.ID))
	if condition := askStateCondition(query.State, false); condition != "" {
		read.where(condition)
	}
	// The count reads the ask's own columns alone, so it reads them through no join.
	var total int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select count(*) from asks a where `+read.clause(), read.arguments...).Scan(&total); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("count the document's asks: %w", err))
		return
	}
	rows, err := s.askV2Rows(r.Context(), s.deps.Store.Pool, read, &query.pageQuery)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if err := s.completeAsksV2(r.Context(), r, rows, include.thread, false); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(rows, total, query.pageQuery))
}

// createArtifactAskV2 answers POST /api/v2/artifacts/{id}/asks: the ask POST
// /api/v1/artifacts/{id}/asks opens (openAsk), answered as the ask row, read on the write's
// transaction (writtenAskV2), with the write's advice beside it.
func (s *server) createArtifactAskV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r)
	if !ok {
		return
	}
	written := &writtenAskV2{server: s}
	_, advice, suggestions, ok := s.openAsk(w, r, owner, v2Body[createAskInput](r), written.readBack)
	if !ok {
		return
	}
	row, err := written.answer(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusCreated, withSuggestions(row, advice, suggestions))
}

// getArtifactBlockPathV2 answers GET /api/v2/artifacts/{id}/blocks/{block_id}: where the block
// stands in the live document, as GET /api/v1/artifacts/{id}/blocks/{block_id} reads it, a block the
// document does not hold answered as the object the path names (BLOCK_NOT_FOUND).
func (s *server) getArtifactBlockPathV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := loadArtifactRowByID(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, r, apicode.NotDocument, "artifact is not a document")
		return
	}
	blockID := r.PathValue("block_id")
	path, err := s.deps.Docs.BlockPath(r.Context(), artifact.ID, blockID)
	if errors.Is(err, pmdoc.ErrTargetNotFound) {
		s.writeHandlerError(w, r, errorf(apicode.BlockNotFound, "the document holds no block %q", blockID))
		return
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, path)
}

// artifactCommentsV2Query is GET /api/v2/artifacts/{id}/comments's query: one page.
type artifactCommentsV2Query struct {
	pageQuery
}

// artifactCommentsV2Route names the kept answers of GET /api/v2/artifacts/{id}/comments apart from
// those of /api/v1's comment lists (commentListRoute), whose bodies for the same document differ.
const artifactCommentsV2Route = http.MethodGet + " /api/v2/artifacts/{id}/comments"

// artifactCommentsV2ETag is commentListETag for one page of a document's /api/v2 comment list: the
// document, the list's watermark and the document's event sequence, and the page, under a line
// naming the version, so no validator one version gave matches the other's answer.
func (s *server) artifactCommentsV2ETag(artifactID string, watermark, lastSeq int64, query pageQuery, trees bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version=v2\nartifact=%s\nwatermark=%d\nlast_seq=%d\nlimit=%d\noffset=%d\n",
		artifactID, watermark, lastSeq, query.Limit, query.Offset)
	return s.listETag(hash, trees)
}

// listArtifactCommentsV2 answers GET /api/v2/artifacts/{id}/comments: one page of the comment list
// GET /api/v1/artifacts/{id}/comments answers (answerCommentList), under a validator of its own and
// kept under the /api/v2 route's own key.
func (s *server) listArtifactCommentsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, owner, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	query := v2Query[artifactCommentsV2Query](r).pageQuery
	watermark, lastSeq, ownerHeld, err := s.commentListWatermark(r.Context(), owner)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	trees := wantsDisplayTrees(r)
	etag := s.artifactCommentsV2ETag(artifact.ID, watermark, lastSeq, query, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	kept := listAnswerKeyFor(r, artifactCommentsV2Route, artifact.ID, etag)
	if s.listAnswers.answer(w, kept) {
		return
	}
	listed, err := s.commentPageV2(r.Context(), owner, "", query)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), trees)
	reads := shapedComments(listed.Items, reader)
	reader.resolve()
	encoded, err := encodeListAnswer(newPage(reads, listed.Total, query))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.writeListAnswer(w, kept, encoded, ownerHeld)
}

// createArtifactCommentV2 answers POST /api/v2/artifacts/{id}/comments: the comment POST
// /api/v1/artifacts/{id}/comments writes (writeComment).
func (s *server) createArtifactCommentV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	owner, ok := s.documentOwnerForHandler(w, r)
	if !ok {
		return
	}
	s.writeComment(w, r, owner, v2Body[commentInput](r))
}

// editArtifactV2 answers POST /api/v2/artifacts/{id}/edits: the edit POST
// /api/v1/artifacts/{id}/edits applies (applyEdits), which reads the artifact first.
func (s *server) editArtifactV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.applyEdits(w, r, v2Body[editArtifactInput](r))
}

// createArtifactPlanReviewV2 answers POST /api/v2/artifacts/{id}/plan-reviews: the plan review POST
// /api/v1/artifacts/{id}/plan-reviews records (recordPlanReview), which reads the artifact first.
func (s *server) createArtifactPlanReviewV2(w http.ResponseWriter, r *http.Request) {
	s.recordPlanReview(w, r, v2Body[createPlanReviewInput](r))
}

// rebuildArtifactV2 answers POST /api/v2/artifacts/{id}/rebuilds: the rebuild POST
// /api/v1/artifacts/{id}/rebuild makes (rebuildDocument), which reads the artifact first.
func (s *server) rebuildArtifactV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	s.rebuildDocument(w, r, actor, v2Body[rebuildArtifactInput](r))
}

// artifactReferencesV2Query is GET /api/v2/artifacts/{id}/references's query.
type artifactReferencesV2Query struct {
	pageQuery
	// Direction is in, the items citing the document, or out, the items it cites.
	Direction string `query:"direction" enum:"in,out" default:"in"`
}

// artifactReferenceV2 is one mention of GET /api/v2/artifacts/{id}/references: an item citing the
// document (direction=in), the referenced_by row of GET /api/v1/artifacts/{id}/references, or an
// item it cites (direction=out), its outgoing row, the cited document's row in place of the
// artifact with every version. Kind is each's; the other members are the direction's own.
type artifactReferenceV2 struct {
	Kind string `json:"kind"`
	// ID, IssueKey, Project, Excerpt and RefKey are the citing item's, on an item citing the
	// document.
	ID       string  `json:"id,omitempty"`
	IssueKey *string `json:"issue_key,omitempty"`
	Project  string  `json:"project,omitempty"`
	Excerpt  string  `json:"excerpt,omitempty"`
	RefKey   string  `json:"ref_key,omitempty"`
	// ToID is the cited item's id, and Artifact the cited document's row where it is one, on an
	// item the document cites.
	ToID     string      `json:"to_id,omitempty"`
	Artifact *artifactV2 `json:"artifact,omitempty"`
}

// listArtifactReferencesV2 answers GET /api/v2/artifacts/{id}/references: one of the two lists GET
// /api/v1/artifacts/{id}/references answers together, by direction, one page of it.
func (s *server) listArtifactReferencesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := loadArtifactRowByID(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	query := v2Query[artifactReferencesV2Query](r)
	var items []artifactReferenceV2
	if query.Direction == "out" {
		outgoing, err := refs.Outgoing(r.Context(), s.deps.Store.Pool, artifact.ID)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		items = make([]artifactReferenceV2, len(outgoing))
		for index, edge := range outgoing {
			items[index] = artifactReferenceV2{Kind: edge.Kind, ToID: edge.ToID}
			if edge.Artifact != nil {
				cited := citedArtifactV2(*edge.Artifact)
				items[index].Artifact = &cited
			}
		}
	} else {
		referencedBy, err := refs.ReferencedBy(r.Context(), s.deps.Store.Pool, artifact.RefKey)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		items = make([]artifactReferenceV2, len(referencedBy))
		for index, edge := range referencedBy {
			items[index] = artifactReferenceV2{
				Kind: edge.Kind, ID: edge.ID, IssueKey: edge.IssueKey, Project: edge.Project, Excerpt: edge.Excerpt, RefKey: edge.RefKey,
			}
		}
	}
	WriteJSON(w, http.StatusOK, pageOf(items, query.pageQuery))
}

// citedArtifactV2 is the row of a document an item cites, as refs.Outgoing reads it. That read
// left-joins each cited document's versions and answers one that has none as holding a single
// version numbered 0, the join's coalesce, which GET /api/v1/artifacts/{id}/references answers as
// it stands. No stored version has that number - every writer numbers a document's versions from
// 1 - so the row counts no version and names no newest, as every other row of such a document.
func citedArtifactV2(artifact model.Artifact) artifactV2 {
	if len(artifact.Versions) == 1 && artifact.Versions[0].Number == 0 {
		artifact.Versions = nil
	}
	return newArtifactV2(artifact)
}

// artifactReviewsV2Query is GET /api/v2/artifacts/{id}/reviews's query: one page.
type artifactReviewsV2Query struct {
	pageQuery
}

// listArtifactReviewsV2 answers GET /api/v2/artifacts/{id}/reviews: one page of the reviews GET
// /api/v1/artifacts/{id}/reviews lists (loadReviews), newest first.
func (s *server) listArtifactReviewsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := loadArtifactRowByID(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	reviews, err := s.loadReviews(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	query := v2Query[artifactReviewsV2Query](r).pageQuery
	listed := pageOf(reviews, query)
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	reads := artifactReviewReads(listed.Items, reader)
	reader.resolve()
	WriteJSON(w, http.StatusOK, newPage(reads, listed.Total, query))
}

// artifactReviewV2Input is POST /api/v2/artifacts/{id}/reviews's body: a review, and the version the
// caller read, which POST /api/v1/artifacts/{id}/versions/{number}/reviews named in its path.
type artifactReviewV2Input struct {
	createArtifactReviewInput
	Version *int `json:"version"`
}

// createArtifactReviewV2 answers POST /api/v2/artifacts/{id}/reviews: the review both /api/v1
// review routes record (recordReview), guarded by the version the body names where it names one.
func (s *server) createArtifactReviewV2(w http.ResponseWriter, r *http.Request) {
	input := v2Body[artifactReviewV2Input](r)
	actor, ok := s.actingPerson(w, r)
	if !ok || !s.requireArtifactV2(w, r, r.PathValue("id")) {
		return
	}
	if input.Version != nil && *input.Version < 1 {
		writeError(w, r, apicode.InvalidReview, "version must name a version of the document, 1 or more")
		return
	}
	s.recordReview(w, r, actor, input.createArtifactReviewInput, input.Version)
}

// artifactSubscribersV2Query is GET /api/v2/artifacts/{id}/subscribers's query: one page.
type artifactSubscribersV2Query struct {
	pageQuery
}

// listArtifactSubscribersV2 answers GET /api/v2/artifacts/{id}/subscribers: one page of the sessions
// GET /api/v1/artifacts/{id}/subscribers lists (subscribersOf).
func (s *server) listArtifactSubscribersV2(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	artifact, _, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	subscribers, ok := s.subscribersOf(w, r, documentSubscriberBase(artifact.Project, artifact.Slug))
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, pageOf(subscribers, v2Query[artifactSubscribersV2Query](r).pageQuery))
}

// artifactVersionsV2Query is GET /api/v2/artifacts/{id}/versions's query.
type artifactVersionsV2Query struct {
	pageQuery
	// Sort is number, oldest first, or -number, newest first.
	Sort querySort `query:"sort" enum:"number" default:"number"`
}

// listArtifactVersionsV2 answers GET /api/v2/artifacts/{id}/versions: one page of the versions GET
// /api/v1/artifacts/{id} carried whole in its versions[] member (loadVersions), ordered by number.
func (s *server) listArtifactVersionsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ctx, pool := r.Context(), s.deps.Store.Pool
	artifact, err := loadArtifactRowByID(ctx, pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	query := v2Query[artifactVersionsV2Query](r)
	var total int
	if err := pool.QueryRow(ctx, `select count(*) from artifact_versions where artifact_id = $1`, artifact.ID).Scan(&total); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("count the artifact's versions: %w", err))
		return
	}
	rows, err := pool.Query(ctx, `
		select `+versionColumns+`
		from artifact_versions v where v.artifact_id = $1
		order by v.number `+sqlDirection(query.Sort)+` limit $2 offset $3`,
		artifact.ID, query.Limit, query.Offset)
	if err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read the artifact's versions: %w", err))
		return
	}
	defer rows.Close()
	versions := []model.Version{}
	for rows.Next() {
		version, err := scanVersion(rows)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read the artifact's versions: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, newPage(versions, total, query.pageQuery))
}

// createNamedVersionV2 answers POST /api/v2/artifacts/{id}/versions: the named version POST
// /api/v1/artifacts/{id}/versions writes (nameVersion), once the path's artifact is held to exist.
func (s *server) createNamedVersionV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireArtifactV2(w, r, r.PathValue("id")) {
		return
	}
	s.nameVersion(w, r, v2Body[createVersionInput](r))
}

// artifactVersionV2Query is GET /api/v2/artifacts/{id}/versions/{number}'s query.
type artifactVersionV2Query struct {
	// Download answers a download: Content-Disposition: attachment with the file's name,
	// Cache-Control: private, no-cache, and a document as its Markdown.
	Download bool `query:"download"`
}

// getArtifactVersionV2 answers GET /api/v2/artifacts/{id}/versions/{number}: the version the four
// /api/v1 version routes answer (serveArtifactVersion), its artifact by id whatever owns it, and a
// number that names no version of it answered as the object the path names (VERSION_NOT_FOUND).
func (s *server) getArtifactVersionV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || !slices.ContainsFunc(artifact.Versions, func(version model.Version) bool { return version.Number == number }) {
		s.writeHandlerError(w, r, errorf(apicode.VersionNotFound, "%s has no version %s", artifact.Name, r.PathValue("number")))
		return
	}
	s.serveArtifactVersion(w, r, artifact, number, v2Query[artifactVersionV2Query](r).Download)
}

// uploadIssueArtifactV2 answers POST /api/v2/issues/{key}/artifacts: the upload POST
// /api/v1/issues/{key}/artifacts stores. The issue was held to exist, and to be open, before the
// body was read (refuseUploadToAClosedIssue, the row's RefusesClosedOwner).
func (s *server) uploadIssueArtifactV2(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.uploadArtifactV2(w, r, artifactTarget{IssueKey: &key})
}

// uploadProjectArtifactV2 answers POST /api/v2/projects/{key}/artifacts: the upload POST
// /api/v1/projects/{key}/artifacts stores, once the path's project is held to exist.
func (s *server) uploadProjectArtifactV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if _, err := s.loadProject(r.Context(), s.deps.Store.Pool, key); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.uploadArtifactV2(w, r, artifactTarget{Project: key})
}

// uploadAgentArtifactV2 answers POST /api/v2/agents/{session_id}/artifacts: the upload POST
// /api/v1/agents/{session_id}/artifacts stores into the conversation.
func (s *server) uploadAgentArtifactV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.uploadArtifactV2(w, r, artifactTarget{Session: session})
}

// uploadArtifactV2 stores the upload the decoder read (v2Body, v2Upload) under target, as
// /api/v1's uploadArtifactFor stores the one it reads itself (storeUpload): a JSON body is a
// markdown document, as jsonUploadInput reads one, and a multipart body the file it holds. The
// answer's row is read as GET /api/v2/artifacts/{id} reads it, on the upload's own transaction
// before it commits (uploadAnswer), so it counts the version the upload wrote and carries the
// approval that version leaves, and a read that fails stores nothing: the artifact the upload
// holds carries no approval, and an artifact it found only the versions before it.
func (s *server) uploadArtifactV2(w http.ResponseWriter, r *http.Request, target artifactTarget) {
	body := v2Body[jsonArtifactUpload](r)
	var input artifactUploadInput
	var err error
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType == "multipart/form-data" {
		input, err = formUploadInput(body, v2Upload(r))
	} else {
		input, err = jsonUploadInput(body)
	}
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.storeUpload(w, r, input, target, func(ctx context.Context, tx queryer, stored model.Artifact, version model.Version) (any, error) {
		artifact, err := s.loadArtifact(ctx, tx, stored.ID)
		if err != nil {
			return nil, err
		}
		rows, err := s.artifactRowsV2(ctx, r, []model.Artifact{artifact}, artifactV2Include{})
		if err != nil {
			return nil, err
		}
		return artifactUploadV2{Artifact: rows[0], Version: version}, nil
	})
}

// artifactUploadV2 is the body the three /api/v2 uploads answer: the artifact's row, as every
// /api/v2 read answers it, in place of the artifact with every version that /api/v1's
// artifactUploadResponse answers (dispatch://INFRA-4071/spec R3), and the version the upload wrote.
type artifactUploadV2 struct {
	Artifact artifactV2    `json:"artifact"`
	Version  model.Version `json:"version"`
}

// formUploadInput is what a multipart upload stores, its declared fields and its file read by the
// decoder: refused as multipartArtifactUpload refuses the form, a primary named, a content field
// beside the file, or no file at all. Its precondition field holds the same JSON object a JSON
// body's precondition holds, as /api/v1's multipart precondition field does.
func formUploadInput(body jsonArtifactUpload, file *uploadFile) (artifactUploadInput, error) {
	switch {
	case body.Primary != nil:
		return artifactUploadInput{}, errorf(apicode.ArtifactInput, "primary is fixed at issue creation")
	case body.Content != nil:
		return artifactUploadInput{}, errorf(apicode.ArtifactInput, "provide either a file or inline JSON content")
	case file == nil:
		return artifactUploadInput{}, errorf(apicode.InvalidArtifact, "artifact file is required")
	}
	name := strings.TrimSpace(body.Name)
	return artifactUploadInput{
		name:         name,
		content:      file.Content,
		precondition: body.Precondition,
		contentType:  uploadContentType(file.ContentType, name),
		summary:      strings.TrimSpace(body.Summary),
		supplied:     body.Actor,
	}, nil
}
