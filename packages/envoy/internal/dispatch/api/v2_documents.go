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

// The refusals of what a document route's request names or meets beside its path's objects
// (dispatch://INFRA-4071/spec R7).
var (
	roleDocumentNameRefusal     = apicode.Refusal{Condition: apicode.RoleDocumentName, Code: apicode.ArtifactRoleName}
	noSettledVersionRefusal     = apicode.Refusal{Condition: apicode.NoSettledVersion, Code: apicode.NoVersion}
	reviewInvalidRefusal        = apicode.Refusal{Condition: apicode.ReviewInvalid, Code: apicode.InvalidReview}
	fileStoreDownRefusal        = apicode.Refusal{Condition: apicode.FileStoreDown, Code: apicode.FileStoreUnavailable}
	documentPreconditionRefusal = apicode.Refusal{Condition: apicode.DocumentPreconditionUnmet, Code: apicode.PreconditionFailed}
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

// artifactUploadText is what the descriptions of the three uploads say alike of the body and the
// name, before what each says of a precondition and the refusals of a body
// (artifactUploadRefusals).
const artifactUploadText = "JSON {name, content?, summary?, actor?, precondition?} stores a markdown document, or " +
	"multipart with a file part and the fields name, summary, actor and precondition stores the file: a text/markdown " +
	"file is a document, any other an image only when its bytes are a PNG, JPEG, GIF or WebP, stored under the type its " +
	"bytes are, and otherwise a file under its declared type. An upload under a name or slug the owner already holds is " +
	"that artifact's next version."

// fileReplacementText is what the three upload routes say of the token a replacement of a file or
// a picture names, which the versions list answers on each version (listArtifactVersionsV2).
const fileReplacementText = "To replace a file or a picture, precondition {document} names the latest version's " +
	"token, which GET /api/v2/artifacts/{id}/versions?sort=-number&limit=1 answers on that version; a replacement " +
	"naming none is taken unchecked."

// uploadPreconditionRefusalText is what the three upload routes say of a precondition they refuse,
// a document's and a file's alike, each refusal under its code once.
const uploadPreconditionRefusalText = "A stale token is 409 PRECONDITION_FAILED with the token and version that " +
	"stand; a first upload or a version with no token also refuses a named token. A precondition naming blocks is " +
	"400 INVALID_PRECONDITION."

// fileUploadPreconditionText is the guard of an agent conversation's upload, which holds files and
// pictures alone.
const fileUploadPreconditionText = fileReplacementText + " " + uploadPreconditionRefusalText

// artifactUploadPreconditionText is the guard of an issue's and a project's upload: the token of
// live document text, which GET /api/v2/artifacts/{id}?include=text answers, and a file's or a
// picture's.
const artifactUploadPreconditionText = "To replace a document, precondition {document} names the token " +
	"GET /api/v2/artifacts/{id}?include=text answers (multipart takes the same JSON in the precondition field), " +
	"which Dispatch checks under the write lock; a replacement naming none is taken for now and counted " +
	"(envoy_dispatch_document_replacements_unguarded_total on /metrics). " + fileReplacementText + " " +
	uploadPreconditionRefusalText

// artifactUploadRoleText is what the description of an issue's upload says of the documents that
// play a role on an issue.
const artifactUploadRoleText = "An issue's plan, Verification document and decisions document are always documents: " +
	"a file or an image under the name or the slug of one of them (plan.md, verification.md, decisions.md; plan-md, " +
	"verification-md, decisions-md) is 400 ARTIFACT_ROLE_NAME, and a Markdown upload under such a name where an " +
	"earlier upload stored a file or an image makes it that document as its next version, its earlier versions still " +
	"served as the files they are."

// artifactUploadAnswerText is what the descriptions of the three uploads say of their answer.
const artifactUploadAnswerText = "Answers 201 {artifact, version}, artifact the artifact's row as it stands once the " +
	"upload is stored (its own fields, latest_version, latest_version_at, latest_version_size, version_count and a " +
	"document's approval) and version the version the upload wrote, for a new artifact and for a new version alike."

// artifactUploadRefusals is what the descriptions of the three uploads say of the bodies they
// refuse, moreInput the cases of 400 ARTIFACT_INPUT an upload answers beyond the three every
// upload does, each led by a comma ("" for none), so the one clause of that code names them all.
// ARTIFACT_ROLE_NAME is not among them: artifactUploadRoleText, which the issue's upload alone
// carries, since only an issue's documents play a role, names it with its status.
func artifactUploadRefusals(moreInput string) string {
	return "A body that is neither JSON nor multipart is 415 JSON_CONTENT_TYPE, and an empty JSON body 400 " +
		"INVALID_JSON; 400 ARTIFACT_INPUT (primary given, content beside a file, blank content under an existing " +
		"document's name" + moreInput + "), ARTIFACT_KIND_MISMATCH, INVALID_ARTIFACT (no name, no " +
		"file, a new name or key over 2692 bytes), 413 REQUEST_TOO_LARGE past 25 MB or a markdown document past 1 MiB."
}

// uploadBodyRefusals are the refusals every upload answers for its body (storeUpload,
// storeArtifact), with more, the upload's own, after them: a body with no name, no file or a
// content type that does not parse, input the upload does not take, a kind other than its
// artifact's, a precondition no upload can hold to, and a file the file store does not take.
func uploadBodyRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append([]apicode.Refusal{
		undeclaredKeyRefusal,
		{Condition: apicode.UploadInvalid, Code: apicode.InvalidArtifact},
		{Condition: apicode.UploadInputRefused, Code: apicode.ArtifactInput},
		{Condition: apicode.UploadKindMismatch, Code: apicode.ArtifactKindMismatch},
		{Condition: apicode.PreconditionMalformed, Code: apicode.InvalidPrecondition},
		fileStoreDownRefusal,
	}, more...)
}

// artifactChangesText is what GET /api/v2/artifacts/{id}/changes says of the change set it
// answers, the body GET /api/v1/artifacts/{id}/changes answers for the same document and versions
// (writeArtifactChanges), in that row's words.
const artifactChangesText = "{artifact_id, from, to, decision_only, unchanged, sections, blocks}, both versions read " +
	"from their stored markdown with every ask block cut out, so the block index's deferred pass is never waited for. " +
	"Exactly one of three holds: decision_only, true when the two stored texts differ and cut to the same text; " +
	"unchanged, true when the two differ in nothing any section or block holds (the stored texts are the same, or the " +
	"cut texts name nothing and are not decision-only); or sections and blocks name at least one item. sections and " +
	"blocks are empty when decision_only or unchanged is true. sections is each top-level unit whose text differs, " +
	"that moved, or that one version alone holds: a `##` section by heading and occurrence (the heading's text as a " +
	"heading anchor reads it, without markdown's syntax and with a line break in it as a line feed; 0 for the first " +
	"section of that heading, so a `##` heading with no words counts from occurrence 1), and a run of the text outside " +
	"every `##` section (the title and what stands above the first `##`, or what follows a `#` heading up to the next " +
	"`##`) under heading \"\" and occurrence 0 with its place: place before_first_section, or place after_section " +
	"with after_heading and after_occurrence naming the `##` section directly before the run, in the later version for " +
	"a changed, added or moved run and in the earlier for a removed one. A `#` or `##` heading that holds a line " +
	"break, which is stored in setext form with no `#`, cuts the document as any heading of its level does. blocks is " +
	"each typed block but an ask whose text or any attribute differs, by id and type. Each unit and block is marked " +
	"changed, added or removed, and a unit whose text is the same is marked moved when its place among the " +
	"document's top-level units is another: it stands outside one longest common subsequence of the two versions' " +
	"orders of the units both hold, and of several that long the one kept holds the later version's earliest units, " +
	"so of two units that swapped places the later version's second is moved. Both versions hold a run when both hold " +
	"its text at the same place, and then, among the runs left in each version, the n-th run of one text in each is " +
	"one run in both, so adding or removing one copy of a repeated run moves no other copy's match; a run the other " +
	"version does not hold pairs with that version's unpaired run at the same place and is marked changed, and a run " +
	"left unpaired is added or removed. A block is never marked moved. Each list holds the later version's items in " +
	"its document order, a moved unit where the later version holds it, then what only the earlier holds, in its " +
	"order."

func v2DocumentIDs() *idRoute {
	return &idRoute{Kind: "artifact", NotFound: apicode.ArtifactNotFound}
}

// v2ArtifactList is the List of an owner's artifacts: no bounds of its own, so 50 and 200
// (listBounds.limits), which each list's description words its page from.
var v2ArtifactList = listBounds{}

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

// agentArtifactListAuthority is the authority of GET /api/v2/agents/{session_id}/artifacts's list
// form, the read that names no slug: the person scope GET /api/v1/agents/{session_id}/artifacts
// needs (its declaration's OAuthScopes), since a conversation's files are part of the conversation. The form that
// names a slug reads one file, which GET /api/v1/agents/{session_id}/artifacts/{slug} reads with
// no scope of its own.
var agentArtifactListAuthority = authorityRule{
	Where: authorityWhere{In: partQuery, Name: "slug"}, Absent: true, Scopes: routeScopeSet{model.ScopePerson},
}

// listAgentArtifactsV2 answers GET /api/v2/agents/{session_id}/artifacts: the files and images of
// one conversation, the list GET /api/v1/agents/{session_id}/artifacts reads and the row GET
// /api/v1/agents/{session_id}/artifacts/{slug} reads. The list form, which names no slug, admits
// the callers GET /api/v1/agents/{session_id}/artifacts admits: the person scope its authority
// rule holds it to (agentArtifactListAuthority) and actingPerson, which that read opens with.
func (s *server) listAgentArtifactsV2(w http.ResponseWriter, r *http.Request) {
	query := v2Query[agentArtifactsV2Query](r)
	if query.Slug == "" {
		if _, ok := s.actingPerson(w, r); !ok {
			return
		}
	} else if !s.requireAuthenticated(w, r) {
		return
	}
	session, err := requestSessionID(r)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
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
// /api/v2 write of one answers it (askWriteV2), read on the write's transaction (writtenAskV2), so
// the request's ask carries its opened_event_id (R6).
func (s *server) requestArtifactApprovalV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireArtifactV2(w, r, r.PathValue("id")) {
		return
	}
	written := s.newWrittenAskV2(r)
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
	Ask        *askWriteV2          `json:"ask"`
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
	written := s.newWrittenAskV2(r)
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
func (s *server) artifactCommentsV2ETag(artifactID string, basis commentListBasis, query pageQuery, trees bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version=v2\nartifact=%s\nwatermark=%d\nlast_seq=%d\nlimit=%d\noffset=%d\n",
		artifactID, basis.watermark, basis.lastSeq, query.Limit, query.Offset)
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
	basis, err := s.readCommentListBasis(r.Context(), owner)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	trees := wantsDisplayTrees(r)
	etag := s.artifactCommentsV2ETag(artifact.ID, basis, query, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	kept := listAnswerKeyFor(r, artifactCommentsV2Route, artifact.ID, etag)
	if s.listAnswers.answer(w, kept) {
		return
	}
	listed, err := s.commentPageV2(r.Context(), everyReader, owner, "", query)
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
	s.writeListAnswer(w, kept, encoded, basis.ownerHeld)
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
// /api/v1/artifacts/{id}/edits applies (applyEdits), which refuses a replace by the members the
// body's bytes name and then reads the artifact.
func (s *server) editArtifactV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.applyEdits(w, r, v2Body[editArtifactInput](r), v2BodyBytes(r))
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
		outgoing, err := refs.Outgoing(r.Context(), s.deps.Store.Pool, s.requestReader(r), artifact.ID)
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
		referencedBy, err := refs.ReferencedBy(r.Context(), s.deps.Store.Pool, s.requestReader(r), artifact.RefKey, s.deps.ServerURL)
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
// A file's or a picture's versions carry their tokens, as every read of a version row states them
// (model.StateFileTokens), so a replacement of the file names the token this list answers.
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
	model.StateFileTokens(artifact.Kind, versions)
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
	// Include=blocks adds a document version's own typed blocks, as /api/v1's blocks=true does.
	Include []string `query:"include,comma" enum:"blocks"`
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
	query := v2Query[artifactVersionV2Query](r)
	s.serveArtifactVersion(w, r, artifact, number, query.Download, slices.Contains(query.Include, "blocks"))
}

// artifactChangesV2Query is GET /api/v2/artifacts/{id}/changes's query: the two versions of the
// document compared, each a version number a document can have (1 to maxIntegerColumn, which the
// decoder holds each to, 400 INVALID_QUERY), version_from below version_to. Both are required, so
// each is nil where the query leaves it out, which the handler refuses.
type artifactChangesV2Query struct {
	// VersionFrom is the earlier version.
	VersionFrom *int `query:"version_from" min:"1" max:"2147483647"`
	// VersionTo is the later version.
	VersionTo *int `query:"version_to" min:"1" max:"2147483647"`
}

// getArtifactChangesV2 answers GET /api/v2/artifacts/{id}/changes: the change set GET
// /api/v1/artifacts/{id}/changes answers (writeArtifactChanges), of the document it reads
// (changedDocument), between the decoded query's version_from and version_to. A query that leaves
// either out, or names a version_from not below its version_to, is 400 INVALID_QUERY, as a window
// whose from is not before its to is on every /api/v2 read.
func (s *server) getArtifactChangesV2(w http.ResponseWriter, r *http.Request) {
	artifact, ok := s.changedDocument(w, r)
	if !ok {
		return
	}
	query := v2Query[artifactChangesV2Query](r)
	switch {
	case query.VersionFrom == nil || query.VersionTo == nil:
		writeError(w, r, apicode.InvalidQuery, "version_from and version_to name the two versions compared; both are required")
		return
	case *query.VersionFrom >= *query.VersionTo:
		writeError(w, r, apicode.InvalidQuery, fmt.Sprintf("version_from (%d) must be below version_to (%d)", *query.VersionFrom, *query.VersionTo))
		return
	}
	s.writeArtifactChanges(w, r, artifact.ID, *query.VersionFrom, *query.VersionTo)
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
		file:         true,
	}, nil
}
