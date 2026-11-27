package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// c-render-at-version, the cases of the api package (R15 of the issue-tree spec, with R14 and R7). A
// version the writer rendered from the tree holds a version_renders row, and every read of it
// renders it again as of the time that row records (docs.RenderVersion). A version of a document no
// query record covers - an upload no role names, a project's document, the issue's tree artifact -
// has no record of R14: the pass that states it records the time and the seq of its cut on its row
// and no digest, so artifact_version_text_sha256, and an approval of it, take the sha256 of its
// stored markdown. A version of the decisions query renders the query's answer as of its row's
// time, and a move that changes a node's place in the tree, and not its made_at or its id, changes
// no byte of it. The census of the rendered versions (docs.ReadVersionRenderCensus, which `census
// --store` prints) reads no difference at any of them and counts none of them as deferred.

// renderAtVersionReader is who every render these cases read answers: the renderer leaves out no
// node for a reader, so every reader reads one rendering.
var renderAtVersionReader = model.Actor{Kind: "user", ID: "alice"}

// renderAtVersionRow is the version_renders row of one version: whether one stands, the time it
// renders the version as of, the seq its pass's cut read and its record's digest, each nil where
// the row holds none.
type renderAtVersionRow struct {
	held    bool
	at      *time.Time
	readSeq *int64
	digest  *string
}

// renderAtVersionRowOf reads the version_renders row of version number of artifactID.
func renderAtVersionRowOf(t *testing.T, database *store.Store, artifactID string, number int) renderAtVersionRow {
	t.Helper()
	var row renderAtVersionRow
	err := database.Pool.QueryRow(context.Background(),
		`select at, read_seq, record_sha256 from version_renders where artifact_id = $1::uuid and number = $2`,
		artifactID, number).Scan(&row.at, &row.readSeq, &row.digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return row
	}
	if err != nil {
		t.Fatalf("read the version_renders row of version %d of %s: %v", number, artifactID, err)
	}
	row.held = true
	return row
}

// words are the row's time, seq and digest as a failure names them: "no row" for each where none
// stands, and "null" for a column that holds none.
func (row renderAtVersionRow) words() (at, readSeq, digest string) {
	if !row.held {
		return "no row", "no row", "no row"
	}
	at, readSeq, digest = "null", "null", "null"
	if row.at != nil {
		at = row.at.UTC().Format(time.RFC3339Nano)
	}
	if row.readSeq != nil {
		readSeq = strconv.FormatInt(*row.readSeq, 10)
	}
	if row.digest != nil {
		digest = *row.digest
	}
	return at, readSeq, digest
}

// storedVersionMarkdown is the stored markdown of version number of the document artifactID, ""
// where the version stores none.
func storedVersionMarkdown(t *testing.T, database *store.Store, artifactID string, number int) string {
	t.Helper()
	var markdown string
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(markdown, '') from artifact_versions where artifact_id = $1::uuid and number = $2`, artifactID, number).Scan(&markdown); err != nil {
		t.Fatalf("read the stored markdown of version %d of %s: %v", number, artifactID, err)
	}
	return markdown
}

// renderAtVersionNewest is the number of the newest version of artifactID, 0 where it holds none.
func renderAtVersionNewest(t *testing.T, database *store.Store, artifactID string) int {
	t.Helper()
	var newest int
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(max(number), 0) from artifact_versions where artifact_id = $1::uuid`, artifactID).Scan(&newest); err != nil {
		t.Fatalf("read the newest version of %s: %v", artifactID, err)
	}
	return newest
}

// renderAtVersionFirstDifference is the first byte at which a and b differ, the length of the
// shorter where one is the head of the other.
func renderAtVersionFirstDifference(a, b string) int {
	at := 0
	for at < len(a) && at < len(b) && a[at] == b[at] {
		at++
	}
	return at
}

// requireRenderAtVersionFromTree reports, through t.Errorf, unless docs.RenderVersion answers
// version number of the document artifactID, whose ref_key is refKey, from the tree with its
// stored bytes, stored, byte for byte.
func requireRenderAtVersionFromTree(t *testing.T, database *store.Store, refKey, artifactID string, number int, stored string) {
	t.Helper()
	rendering, err := docs.RenderVersion(context.Background(), database.Pool, renderAtVersionReader, artifactID, number)
	if err != nil {
		t.Errorf("render version %d of %s: %v", number, refKey, err)
		return
	}
	if rendering.Source != docs.RenderedFromTree || rendering.Markdown != stored {
		t.Errorf("version %d of %s renders %d bytes from %s, want its %d stored bytes from %s",
			number, refKey, len(rendering.Markdown), rendering.Source, len(stored), docs.RenderedFromTree)
	}
}

// renderAtVersionCensus is the census of the rendered versions as `census --store` reads it
// (docs.ReadVersionRenderCensus), and the lines it prints, as the command prints a store report
// that holds it (graphcensus.StoreReport.Write).
func renderAtVersionCensus(t *testing.T, database *store.Store) (graphcensus.VersionRenderCensus, []string) {
	t.Helper()
	renders, err := docs.ReadVersionRenderCensus(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("read the census of the rendered versions: %v", err)
	}
	var printed bytes.Buffer
	(&graphcensus.StoreReport{VersionRenders: &renders}).Write(&printed)
	return renders, strings.Split(printed.String(), "\n")
}

// renderAtVersionCounted is what the census of the rendered versions says of the version whose
// address is address (graphstore.VersionID): the cause of the difference it reads there, "" for
// none, and the line it prints that counts the version among the deferred ones, "" for none.
func renderAtVersionCounted(renders graphcensus.VersionRenderCensus, printed []string, address string) (difference, deferred string) {
	for _, reported := range renders.Reported {
		if reported.At == "version "+address {
			difference = reported.Cause
		}
	}
	for _, line := range printed {
		if strings.Contains(line, "deferred") && renderAtVersionNames(line, address) {
			deferred = line
		}
	}
	return difference, deferred
}

// renderAtVersionNames reports whether line names address whole, and not only as the head of a
// later version's address (ref@v1 in ref@v10).
func renderAtVersionNames(line, address string) bool {
	for rest := line; ; {
		at := strings.Index(rest, address)
		if at < 0 {
			return false
		}
		rest = rest[at+len(address):]
		if rest == "" || rest[0] < '0' || rest[0] > '9' {
			return true
		}
	}
}

// TestADocumentNoQueryRecordCoversRecordsNoDigest is c-render-at-version's case of a version no
// query record covers (R15, R14). On an issue born converted it reads three such versions: version 1
// of notes.md, which it uploads to the issue (POST /api/v1/issues/{key}/artifacts), a name no role
// uses, since an upload named plan.md or verification.md joins the issue's room and has a record
// (documentJoinsIssueRoom); version 1 of runbook.md, a document of the project (POST
// /api/v1/projects/{key}/artifacts); and version 1 of the issue's tree artifact, which the create
// writes. Of each it reads the version_renders row holding at and read_seq and a null
// record_sha256, artifact_version_text_sha256 answering the sha256 of the version's stored
// markdown, and docs.RenderVersion answering from the source tree with the stored bytes, byte for
// byte, as the version route answers it too (GET /api/v1/artifacts/{id}/versions/1?download=true,
// Dispatch-Render-Source tree), the tree artifact's as a document's (readsAsDocument). The census
// of the rendered versions reads no difference at any of the three and counts none of them as
// deferred. Alice then approves the upload's version 1 through the approval route, and the
// review's content_sha256 is the sha256 of its stored markdown. Each read reports its own failure,
// and the test goes on to the next read.
func TestADocumentNoQueryRecordCoversRecordsNoDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{})
	const project = "NODIGEST"
	createRoomProject(t, handler, project)
	issue, tree := createRoomIssue(t, handler, database, project, "A document no record covers", "The spec of the issue.\n", "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree, want it born converted", issue.Key)
	}
	notes := uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "notes.md", "# Notes\n\nNo query of the issue covers these notes.\n")
	runbook := uploadDocument(t, handler, "/api/v1/projects/"+project, "runbook.md", "# Runbook\n\nNo issue holds this document.\n")
	awaitSettled(t, database, issue.PrimaryArtifactID, notes.Artifact.ID, runbook.Artifact.ID)

	renders, printed := renderAtVersionCensus(t, database)
	var notesStored, notesRef string
	for _, document := range []struct{ what, id string }{
		{"the upload notes.md of " + issue.Key, notes.Artifact.ID},
		{"the project document runbook.md of " + project, runbook.Artifact.ID},
		{"the tree artifact of " + issue.Key, tree},
	} {
		refKey := refKeyOf(t, database, document.id)
		address := graphstore.VersionID(refKey, 1)
		stored := storedVersionMarkdown(t, database, document.id, 1)
		if document.id == notes.Artifact.ID {
			notesStored, notesRef = stored, refKey
		}

		if row := renderAtVersionRowOf(t, database, document.id, 1); !row.held || row.at == nil || row.readSeq == nil || row.digest != nil {
			at, readSeq, digest := row.words()
			t.Errorf("the version_renders row of %s holds %s, %s and %s, want a time, a seq and no digest", address, at, readSeq, digest)
		}
		var answered *string
		if err := database.Pool.QueryRow(ctx, `select artifact_version_text_sha256($1::uuid, 1)`, document.id).Scan(&answered); err != nil {
			t.Errorf("read artifact_version_text_sha256 of %s: %v", address, err)
		} else if want := contentHash(stored); answered == nil || *answered != want {
			got := "null"
			if answered != nil {
				got = *answered
			}
			t.Errorf("artifact_version_text_sha256 answers %s for %s, want the stored markdown's %s", got, address, want)
		}
		requireRenderAtVersionFromTree(t, database, refKey, document.id, 1, stored)
		downloaded := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.id+"/versions/1?download=true", nil, "alice")
		if source := downloaded.Header().Get(renderSourceHeader); downloaded.Code != http.StatusOK || source != string(docs.RenderedFromTree) || downloaded.Body.String() != stored {
			t.Errorf("the version route answers %s with %d from %q holding %d bytes, want 200 from %s holding its %d stored bytes: %.300s",
				address, downloaded.Code, source, downloaded.Body.Len(), docs.RenderedFromTree, len(stored), downloaded.Body.String())
		}
		difference, deferred := renderAtVersionCounted(renders, printed, address)
		if difference != "" {
			t.Errorf("the census counts version 1 of %s as a difference: %s", refKey, difference)
		}
		if deferred != "" {
			t.Errorf("the census counts version 1 of %s as deferred: %s", refKey, deferred)
		}
	}

	approval := reviewDocument(t, handler, notes.Artifact.ID, "alice", "approved", "")
	want := contentHash(notesStored)
	switch {
	case approval.Version != 1:
		t.Errorf("alice's approval of %s is of version %d, want version 1", notesRef, approval.Version)
	case approval.ContentSHA256 == nil || *approval.ContentSHA256 != want:
		got := "null"
		if approval.ContentSHA256 != nil {
			got = *approval.ContentSHA256
		}
		t.Errorf("the approval of %s at version 1 pins %s, want the stored markdown's %s", notesRef, got, want)
	}
}

// The words of the decisions case's spec (renderAtVersionDecisionsSpec): the ask's question and
// the words of each of its two decisions, which every version of the decisions query stores.
const (
	renderAtVersionQuestion = "Which cipher suite does the gateway offer first?"
	renderAtVersionRollout  = "We roll the gateway out region by region"
	renderAtVersionLogging  = "We log every handshake the gateway makes."
)

// renderAtVersionDecisionsSpec is the spec of the decisions case: a criterion k1, an ask a1 and two
// decisions, in that order, the first, d1, citing the issue other (dispatch://<other>), and the
// second d2.
func renderAtVersionDecisionsSpec(other string) string {
	return ":::criterion{#k1}\nThe gateway negotiates TLS 1.3 on every port.\n:::\n\n" +
		":::ask{#a1 urgency=\"med\" multiple=\"false\"}\n" + renderAtVersionQuestion + "\n\n- AES: AES-GCM first\n- ChaCha: ChaCha20-Poly1305 first\n:::\n\n" +
		":::decision{#d1}\n" + renderAtVersionRollout + ", as dispatch://" + other + " plans it.\n:::\n\n" +
		":::decision{#d2}\n" + renderAtVersionLogging + "\n:::\n"
}

// renderAtVersionDocument is the decisions query node of an issue: its id, its ref_key, which the
// census names its versions under (graphstore.VersionID), and its dispatch:// address, which the
// references read takes.
type renderAtVersionDocument struct {
	id, refKey, ref string
}

// renderAtVersionDecisionsOf reads the decisions query node of the issue key of project
// (refs.DecisionsQuerySQL).
func renderAtVersionDecisionsOf(t *testing.T, database *store.Store, key, project string) renderAtVersionDocument {
	t.Helper()
	var document renderAtVersionDocument
	var slug string
	if err := database.Pool.QueryRow(context.Background(),
		`select a.id::text, a.ref_key, a.slug from artifacts a where a.issue_key = $1 and `+refs.DecisionsQuerySQL("a"), key,
	).Scan(&document.id, &document.refKey, &slug); err != nil {
		t.Fatalf("read the decisions query node of %s: %v", key, err)
	}
	document.ref = refs.ArtifactRef(&key, project, nil, slug, "doc", false)
	return document
}

// renderAtVersionQuiet waits until the spec of the issue key, specID, owes no settlement and the
// issue's queries owe no drain (issue_query_marks), so the decisions query holds every version the
// writes so far owe it.
func renderAtVersionQuiet(t *testing.T, database *store.Store, key, specID string) {
	t.Helper()
	awaitSettled(t, database, specID)
	testwait.UntilPolling(t, 20*time.Millisecond, "the queries of "+key+" to owe no drain (issue_query_marks)", func() (bool, bool) {
		_, owed := readQueryMarkRow(t, database, key)
		return owed, !owed
	})
}

// renderAtVersionAtCreate and renderAtVersionOnceOpened are the words a version of the decisions
// query of renderAtVersionDecisionsSpec stores. Version 1, the create's, stores the rendering of
// the query's answer at the create (R15: "Each of its versions stores the readable rendering of
// the decisions query's answer: version 1, which the issue create writes, and each later version,
// which ReviseQueries writes"), and that answer holds the two decisions and not the ask: the
// query takes an ask by its own node, never by the block that shows it, and the spec's settlement
// opens the ask a1, which gives it its node, only once the create has committed (R8, R10). Every
// version written once the ask is open stores its question too.
var (
	renderAtVersionAtCreate   = []string{renderAtVersionRollout, renderAtVersionLogging}
	renderAtVersionOnceOpened = []string{renderAtVersionQuestion, renderAtVersionRollout, renderAtVersionLogging}
)

// requireRenderAtVersionDecisionsVersion reports, through t.Errorf, unless version number of the
// decisions query decisions holds a version_renders row with a time and a seq, stores each of
// words, and renders from the tree as its stored bytes, byte for byte. It answers the version's
// stored markdown and its row.
func requireRenderAtVersionDecisionsVersion(t *testing.T, database *store.Store, decisions renderAtVersionDocument, number int, words []string) (string, renderAtVersionRow) {
	t.Helper()
	address := graphstore.VersionID(decisions.refKey, number)
	row := renderAtVersionRowOf(t, database, decisions.id, number)
	if !row.held || row.at == nil || row.readSeq == nil {
		at, readSeq, _ := row.words()
		t.Errorf("the version_renders row of %s holds the time %s and the seq %s, want a time and a seq", address, at, readSeq)
	}
	stored := storedVersionMarkdown(t, database, decisions.id, number)
	for _, held := range words {
		if !strings.Contains(stored, held) {
			t.Errorf("version %d of %s stores %q, want %q in it", number, decisions.refKey, stored, held)
		}
	}
	requireRenderAtVersionFromTree(t, database, decisions.refKey, decisions.id, number, stored)
	return stored, row
}

// TestTheDecisionsQueryVersionsRenderTheirAnswerAsOfTheirRow is c-render-at-version's case of the
// decisions query (R15, R7). It creates, through POST /api/v1/issues, an issue born converted whose
// spec holds a criterion, an ask and two decisions, in that order, the first decision citing a
// second issue the test creates first, so the decisions query's answer holds nodes at the create.
// Version 1 of the issue's decisions query node, the create's, holds a version_renders row with a
// time and a seq, and docs.RenderVersion answers it from the source tree as its stored bytes, byte
// for byte, which hold the two decisions: the ask has no node until the spec's settlement opens it
// after the create (renderAtVersionAtCreate). Every version written once the ask is open holds its
// question too. Alice answers the ask through POST /api/v1/asks/{id}/answer, which changes the
// query's answer, so the query takes a version written for it, of which the test reads the same.
// She then moves the second decision's block under the
// criterion through PATCH /api/v1/issues/{key}/nodes/{id}, with the body {parent} and an If-Match
// naming the block's placement_seq: that changes the decision's place in the tree, before the ask
// in the tree's order, and not its made_at or its id. The move moves the query's record, so the
// query takes a version written for the move, whose stored bytes are the bytes of the version
// before it, byte for byte, and whose row's record digest differs from that version's. The tree
// holds no member of the decisions query node at the head (refs.TreeMembers), and holds the
// criterion, the ask and the two decisions among the spec's members, so a read that finds nothing
// cannot pass. GET /api/v1/references?from=<decisions ref>&kind=mentions answers no mention, since
// the index of a version of the decisions query indexes no reference; the same read from the spec
// answers the mention of the second issue, written from the paragraph inside the first decision.
// The census of the rendered versions reads no difference at any version of the decisions query
// and counts none of them as deferred.
func TestTheDecisionsQueryVersionsRenderTheirAnswerAsOfTheirRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, _ := newTestServer(t, testServerOptions{})
	const project = "DECIDE"
	createReferenceAPIProject(t, handler, project)
	other := createReferenceAPIIssue(t, handler, project)
	issue, tree := createRoomIssue(t, handler, database, project, "Decisions rendered as of their row", renderAtVersionDecisionsSpec(other.Key), "")
	if tree == "" {
		t.Fatalf("issue %s holds no tree, want it born converted", issue.Key)
	}
	key := issue.Key
	decisions := renderAtVersionDecisionsOf(t, database, key, project)

	// Version 1, the create's.
	requireRenderAtVersionDecisionsVersion(t, database, decisions, 1, renderAtVersionAtCreate)
	asks := readingAsksOpened(t, handler, key, map[string]string{"a1": renderAtVersionQuestion})
	renderAtVersionQuiet(t, database, key, issue.PrimaryArtifactID)

	// The answer changes the query's answer.
	unanswered := renderAtVersionNewest(t, database, decisions.id)
	answerDecisionBlock(t, handler, asks["a1"], "alice", []string{"AES"}, "", nil)
	renderAtVersionQuiet(t, database, key, issue.PrimaryArtifactID)
	answered := renderAtVersionNewest(t, database, decisions.id)
	if answered <= unanswered {
		t.Errorf("the answer of the ask a1 leaves %s at version %d, the version before it, want a version written for the changed answer", decisions.refKey, answered)
	}
	for number := unanswered + 1; number <= answered; number++ {
		requireRenderAtVersionDecisionsVersion(t, database, decisions, number, renderAtVersionOnceOpened)
	}

	// The move of the second decision under the criterion moves the query's record and no byte of
	// its answer.
	spec := readDecisionDocument(t, database, key, "spec")
	decision, criterion := spec.address("d2"), spec.address("k1")
	_, tag := placementOf(t, database, refs.BlockNodeKind, decision)
	if moved := patchNode(t, handler, key, decision, tag, map[string]any{"parent": criterion}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("the move of %s under %s answered %d %s, want 200", decision, criterion, moved.Code, moved.Body.String())
	}
	if placed, _ := placementOf(t, database, refs.BlockNodeKind, decision); placed.ToID != criterion {
		t.Fatalf("the move of %s under %s left it under %s %s", decision, criterion, placed.ToKind, placed.ToID)
	}
	renderAtVersionQuiet(t, database, key, issue.PrimaryArtifactID)
	movedTo := renderAtVersionNewest(t, database, decisions.id)
	before, beforeRow := storedVersionMarkdown(t, database, decisions.id, answered), renderAtVersionRowOf(t, database, decisions.id, answered)
	if movedTo <= answered {
		t.Errorf("the move of %s leaves %s at version %d, the version before it, want a version written for the moved record", decision, decisions.refKey, movedTo)
	}
	for number := answered + 1; number <= movedTo; number++ {
		stored, row := requireRenderAtVersionDecisionsVersion(t, database, decisions, number, renderAtVersionOnceOpened)
		if stored != before {
			t.Errorf("version %d of %s, written for the move of %s, differs from version %d from byte %d",
				number, decisions.refKey, decision, answered, renderAtVersionFirstDifference(stored, before))
		}
		if number == answered+1 && (row.digest == nil || beforeRow.digest == nil || *row.digest == *beforeRow.digest) {
			_, _, movedDigest := row.words()
			_, _, beforeDigest := beforeRow.words()
			t.Errorf("version %d of %s, written for the move of %s, records the record digest %s and version %d the digest %s, want two digests: the move changed the record",
				number, decisions.refKey, decision, movedDigest, answered, beforeDigest)
		}
	}

	// The tree holds no member of the decisions query node, and the spec's members.
	cut, err := refs.HeadCut(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the head's cut: %v", err)
	}
	if members, err := refs.TreeMembers(ctx, database.Pool, decisions.refKey, cut); err != nil {
		t.Errorf("read the members of %s: %v", decisions.refKey, err)
	} else if len(members) != 0 {
		t.Errorf("the tree holds %d members of the decisions query node of %s, want none: %v", len(members), key, members)
	}
	if members, err := refs.TreeMembers(ctx, database.Pool, spec.refKey, cut); err != nil {
		t.Errorf("read the members of %s: %v", spec.refKey, err)
	} else {
		var missing []string
		for _, block := range []string{"k1", "a1", "d1", "d2"} {
			if !slices.ContainsFunc(members, func(member refs.Target) bool { return member.ID == spec.address(block) }) {
				missing = append(missing, spec.address(block))
			}
		}
		if len(members) == 0 || len(missing) > 0 {
			t.Errorf("the tree holds no member of the spec of %s, so the read of members finds nothing: it holds %v, and not %v", key, members, missing)
		}
	}

	// The references read from the decisions query answers no mention; the same read from the spec
	// answers the first decision's.
	if read := graphRequest(t, handler, url.Values{"from": {decisions.ref}, "kind": {"mentions"}}); read.Code != http.StatusOK {
		t.Errorf("GET /api/v1/references?from=%s&kind=mentions: status=%d body=%s", decisions.ref, read.Code, read.Body.String())
	} else if edges := decodeBody[model.GraphReferences](t, read).Edges; len(edges) != 0 {
		t.Errorf("the references read from %s answers %d mentions, want none: %+v", decisions.ref, len(edges), edges)
	}
	specRef := "dispatch://" + key + "/spec"
	if read := graphRequest(t, handler, url.Values{"from": {specRef}, "kind": {"mentions"}}); read.Code != http.StatusOK {
		t.Errorf("GET /api/v1/references?from=%s&kind=mentions: status=%d body=%s", specRef, read.Code, read.Body.String())
	} else if edges := decodeBody[model.GraphReferences](t, read).Edges; !slices.ContainsFunc(edges, func(edge model.GraphEdge) bool {
		return edge.Node.Kind == graphstore.KindIssue && edge.Node.ID == other.Key
	}) {
		t.Errorf("the references read from the spec of %s answers no mention of %s, so the read finds nothing: %+v", key, other.Key, edges)
	}

	// The census of the rendered versions.
	renders, printed := renderAtVersionCensus(t, database)
	for number := 1; number <= movedTo; number++ {
		address := graphstore.VersionID(decisions.refKey, number)
		difference, deferred := renderAtVersionCounted(renders, printed, address)
		if difference != "" {
			t.Errorf("the census reads a difference at version %s: %s", address, difference)
		}
		if deferred != "" {
			t.Errorf("the census counts version %d of %s as deferred: %s", number, decisions.refKey, deferred)
		}
	}
	t.Logf("the decisions query %s stands at version %d: the answer's from version %d, the move's from version %d", decisions.refKey, movedTo, unanswered+1, answered+1)
}
