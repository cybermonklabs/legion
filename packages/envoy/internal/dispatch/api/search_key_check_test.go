package api

//ci:builds packages/envoy ./cmd/dispatch convertIssues builds envoy-dispatch, whose convert-issues TestSearchFollowsABlockThatChangesOwner runs as the conversion of this issue's spec R6
//ci:reads packages/envoy/internal/dispatch/store/migrations/*_issue_tree_*.up.sql searchKeyMigrationSet, which TestSearchFollowsABlockThatChangesOwner reads through pgmigrate.TouchedTables

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// searchKeyWindows is the id of every window of kind refs.DocumentWindowKind that ownerKey owns,
// with no window arm run first: what the store holds now.
func searchKeyWindows(t *testing.T, database *store.Store, ownerKey string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select id from embeddings where kind = $1 and split_part(id, '#', 1) = $2 order by id`, refs.DocumentWindowKind, ownerKey)
	if err != nil {
		t.Fatalf("read the windows of %s: %v", ownerKey, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the windows of %s: %v", ownerKey, err)
	}
	return ids
}

// searchKeyHit is one answer of the search route, as two answers are compared.
type searchKeyHit struct {
	kind, id, block string
}

func searchKeyHits(response model.SearchResponse) []searchKeyHit {
	hits := make([]searchKeyHit, 0, len(response.Results))
	for _, result := range response.Results {
		hit := searchKeyHit{kind: result.Kind, id: result.ID}
		if result.BlockID != nil {
			hit.block = *result.BlockID
		}
		hits = append(hits, hit)
	}
	return hits
}

// searchKeyMigrationSet is every migration of this issue's tree (store's issueTreeMigrationName,
// <timestamp>_issue_tree_<what>.up.sql), by file name, with its text.
func searchKeyMigrationSet(t *testing.T) map[string]string {
	t.Helper()
	paths, err := filepath.Glob("../store/migrations/*_issue_tree_*.up.sql")
	if err != nil || len(paths) == 0 {
		t.Fatalf("list this issue's migrations: %v (%d found)", err, len(paths))
	}
	set := map[string]string{}
	for _, path := range paths {
		text, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		set[filepath.Base(path)] = string(text)
	}
	return set
}

// searchKeyEmbeddingKinds are the kinds embeddings_kind_check admits as migration
// 20261102233625_block_embedding_windows states it, which no later migration restates: the
// whole-text kinds and each window kind (refs' windowKinds).
var searchKeyEmbeddingKinds = []string{
	"ask", "ask_part", "comment", "comment_block", "document", "document_block", "issue", "message", "message_block",
}

// Search follows a block that changes owner (this issue's spec R17). A window's id is
// `<owner key>#<first block>`, and the owner key of a document's blocks becomes the issue's tree
// node once the conversion moves them there: the windows are then written under the tree, the old
// owner's rows stay for one release, which a rollback's search by meaning reads, and this build's
// search reads only the windows of the current owner, so the moved block answers once. The
// lexical search reads node_search, one row per node that holds text, and answers the node that
// matched, while the artifact_versions_document_search trigger, its function and document_search
// stay for a process of the release before, which reads that table alone. The conversion carries
// each moved block's authors to its node in the room (this issue's spec R22): alice's, who wrote
// the spec's one version before the move. The store census holds each node to her, read off the
// spec's own record of the block, never off the room's first version of the spec, which the
// conversion wrote, nor off the node it checks: once the authored_by tuples the conversion
// carried are taken off, it reads a row no tuple states at each block.
func TestSearchFollowsABlockThatChangesOwner(t *testing.T) {
	t.Parallel()
	const project = "SKEY"
	const query = "theodolite recalibration"
	const first = "Alpha notes on the mounting bracket."
	const moved = "The theodolite needs recalibration before the survey."
	const word = "zorptangle"
	ctx := context.Background()
	embedder := &fakeEmbedder{vectors: map[string][]float32{query: queryVector()}}
	handler, database, deps := newTestServer(t, testServerOptions{
		embedder: embedder, duplicateBound: nearCopyVerdictBound(t), relatedBound: meaningVerdictBound(t),
	})
	holdIssuesUnconverted(t, database)
	issue := createInteractionIssue(t, handler, project, "Survey gear", first+"\n\n"+moved+"\n")
	if spec := readDecisionDocument(t, database, issue.Key, "spec"); spec.room != spec.id {
		t.Fatalf("the spec of %s is edited in the room %s, want its own room: an issue created beside a process that lists no issue room is not converted", issue.Key, spec.room)
	}
	blocks := blockIDsByText(t, handler, issue.PrimaryArtifactID)
	firstBlock, movedBlock := blocks[first], blocks[moved]
	if firstBlock == "" || movedBlock == "" {
		t.Fatalf("the spec of %s lists the blocks %v, want one holding %q and one holding %q", issue.Key, blocks, first, moved)
	}

	// The windows of the spec's blocks are written under the artifact uuid as their owner key.
	windowOwedOwners(t, deps)
	oldWindows := searchKeyWindows(t, database, issue.PrimaryArtifactID)
	if len(oldWindows) == 0 {
		t.Fatalf("the spec %s holds no window under its artifact uuid before the move", issue.PrimaryArtifactID)
	}

	// The conversion's transaction rolled back (--dry-run) leaves the old owner's windows and
	// writes none under the issue's tree node.
	openTheConversionFleet(t, database)
	convertIssues(t, database, project, true)
	if after := searchKeyWindows(t, database, issue.PrimaryArtifactID); !slices.Equal(after, oldWindows) {
		t.Errorf("the conversion rolled back left the old owner's windows %v, want %v as they were", after, oldWindows)
	}
	if tree, err := issueTreeOf(ctx, database.Pool, issue.Key); err != nil {
		t.Fatal(err)
	} else if tree != "" {
		if under := searchKeyWindows(t, database, tree); len(under) != 0 {
			t.Errorf("the conversion rolled back left the windows %v under the issue's tree node %s, want none", under, tree)
		}
	}

	// The conversion committed moves the blocks into the issue's tree.
	convertIssues(t, database, project, false)
	spec := readDecisionDocument(t, database, issue.Key, "spec")
	tree, err := issueTreeOf(ctx, database.Pool, issue.Key)
	if err != nil {
		t.Fatal(err)
	}
	if tree == "" || spec.room != tree {
		t.Fatalf("after the conversion the spec of %s is edited in the room %s and the issue's tree is %q, want the spec a fragment of its issue's room", issue.Key, spec.room, tree)
	}
	windowOwedOwners(t, deps)
	newWindows := windowTexts(t, database, refs.DocumentWindowKind, tree)
	for _, text := range []string{first, moved} {
		covering := 0
		for _, window := range newWindows {
			if strings.Contains(window, text) {
				covering++
			}
		}
		if covering != 1 {
			t.Errorf("%d windows under the issue's tree node %s cover the block %q, want 1: %v", covering, tree, text, newWindows)
		}
	}
	if after := searchKeyWindows(t, database, issue.PrimaryArtifactID); !slices.Equal(after, oldWindows) {
		t.Errorf("after the move the old owner %s holds the windows %v, want %v still stored for the release before", issue.PrimaryArtifactID, after, oldWindows)
	}

	// The census counts the old owner's windows once, as the class the root's pull request deletes.
	report := storeCensus(t, database)
	var census bytes.Buffer
	report.Write(&census)
	t.Logf("the store census after the move:\n%s", census.String())
	counted := regexp.MustCompile(`(?i)^census: store .*window.*=` + strconv.Itoa(len(oldWindows)) + `\b`)
	countedOnce := 0
	for _, line := range strings.Split(census.String(), "\n") {
		if counted.MatchString(line) {
			countedOnce++
		}
	}
	if countedOnce != 1 {
		t.Errorf("the store census names the %d windows the old owner keeps on %d lines, want one line counting them as the class the root deletes", len(oldWindows), countedOnce)
	}

	// The conversion carried each block's authors to its node in the room: alice's, and never the
	// conversion's, whose version of the spec the room records the blocks in first. The census
	// holds each node to them and agrees over the two blocks on both sides.
	roomBlocks := []string{spec.address(firstBlock), spec.address(movedBlock)}
	for _, address := range roomBlocks {
		if held := currentTies(t, database, address, graphstore.TupleAuthoredBy); len(held) != 1 ||
			held[0].ToKind != graphstore.KindPerson || held[0].ToID != "alice" {
			t.Errorf("after the move the block %s states the authored_by %+v, want alice's alone, who wrote the spec's first version", address, held)
		}
	}
	if authors := censusKindOf(t, report, "block_author"); authors.Rows != 2 || authors.Nodes != 2 ||
		authors.RowDifferences != 0 || authors.NodeDifferences != 0 {
		t.Errorf("after the move the store census reads block_author rows=%d tuples=%d with the differences %+v and %+v, want the authored_by to alice of each of the two moved blocks on both sides and none",
			authors.Rows, authors.Nodes, authors.ReportedRows, authors.ReportedNodes)
	}

	// Search by meaning reads the windows of the current owner alone: the moved block answers once.
	embedWindowsAs(t, database, embedder, refs.DocumentWindowKind, tree, angledVector(0.9, 2))
	embedWindowsAs(t, database, embedder, refs.DocumentWindowKind, issue.PrimaryArtifactID, angledVector(0.9, 2))
	processAllPending(t, database, embedder)
	meaning := searchResponse(t, handler, "q="+url.QueryEscape(query))
	if meaning.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty", meaning.Degraded)
	}
	answered := 0
	for _, hit := range searchKeyHits(meaning) {
		if hit.block == movedBlock {
			answered++
		}
	}
	if answered != 1 {
		t.Errorf("a search by meaning for the moved block's words answers the block %s %d times, want once: %+v", movedBlock, answered, meaning.Results)
	}

	// No migration of this issue touches embeddings, so its kind constraint stands as it is.
	var constraint string
	if err := database.Pool.QueryRow(ctx,
		`select pg_get_constraintdef(oid) from pg_constraint where conname = 'embeddings_kind_check' and conrelid = 'embeddings'::regclass`).Scan(&constraint); err != nil {
		t.Fatalf("read embeddings_kind_check from the catalog: %v", err)
	}
	var kinds []string
	for _, literal := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(constraint, -1) {
		kinds = append(kinds, literal[1])
	}
	slices.Sort(kinds)
	if !slices.Equal(kinds, searchKeyEmbeddingKinds) || !slices.Contains(kinds, refs.DocumentWindowKind) {
		t.Errorf("embeddings_kind_check reads %s, want it unchanged, admitting %v", constraint, searchKeyEmbeddingKinds)
	}
	migrations := searchKeyMigrationSet(t)
	for _, name := range slices.Sorted(maps.Keys(migrations)) {
		for _, table := range pgmigrate.TouchedTables(migrations[name]) {
			if table == "embeddings" || table == "artifact_versions" || table == "document_search" {
				t.Errorf("%s runs a statement against %s, which no migration of this issue touches", name, table)
			}
		}
	}

	// The lexical search: a word written into one block is held by node_search for that node, and
	// the search route answers that node.
	succeeded(t, "write a word into the first block", dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Alpha notes", "with": "Alpha " + word + " notes"}},
	}, "alice"))
	reconcileUntilNothingOwed(t, handler, deps, issue.Key)
	address := spec.address(firstBlock)
	var held int
	if err := database.Pool.QueryRow(ctx, `
		select count(*)::int from node_search s
		where strpos(to_jsonb(s)::text, $1) > 0 and strpos(to_jsonb(s)::text, $2) > 0
	`, word, address).Scan(&held); err != nil {
		t.Fatalf("read node_search for the node %s: %v", address, err)
	}
	if held != 1 {
		t.Errorf("node_search holds %d rows naming the node %s with the word %q, want 1", held, address, word)
	}
	// The route answers the node that matched rather than a document: the spec's one document hit
	// names the block that holds the word. The issue's own vector carries its spec's text
	// (migration 20261105145242), so the issue may answer beside it; an issue hit is no document
	// and names no block.
	lexical := searchResponse(t, handler, "q="+word+"&keywords_only=true")
	var documentHits []searchKeyHit
	for _, hit := range searchKeyHits(lexical) {
		if hit.kind == "document" {
			documentHits = append(documentHits, hit)
		}
	}
	if len(documentHits) != 1 || documentHits[0].id != issue.PrimaryArtifactID || documentHits[0].block != firstBlock {
		t.Errorf("the search route answers the document hits %+v for %q, want one, the spec %s, naming the node %s that matched rather than the document alone",
			documentHits, word, issue.PrimaryArtifactID, address)
	}

	// The trigger, its function and document_search stay in place, and the trigger still fills
	// the table on a version write, for a process of the release before.
	var trigger, function, table bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (select 1 from pg_trigger where tgname = 'artifact_versions_document_search' and tgrelid = 'artifact_versions'::regclass),
		       exists (select 1 from pg_proc where proname = 'artifact_versions_document_search'),
		       to_regclass('public.document_search') is not null
	`).Scan(&trigger, &function, &table); err != nil {
		t.Fatalf("read the catalog: %v", err)
	}
	if !trigger {
		t.Error("the catalog holds no trigger artifact_versions_document_search on artifact_versions, which a process of the release before reads document_search through")
	}
	if !function {
		t.Error("the catalog holds no function artifact_versions_document_search")
	}
	if !table {
		t.Fatal("the catalog holds no table document_search")
	}
	version := latestVersion(t, database, issue.PrimaryArtifactID).Number
	var filled int
	if err := database.Pool.QueryRow(ctx, `select coalesce((select number from document_search where artifact_id = $1::uuid), 0)`,
		issue.PrimaryArtifactID).Scan(&filled); err != nil {
		t.Fatalf("read the document_search row of the spec: %v", err)
	}
	if filled != version {
		t.Errorf("the document_search row of the spec names version %d, want version %d, the one the trigger filled it from", filled, version)
	}

	var whole, blocksBeside int
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from node_search s join document_search old on old.artifact_id = $1::uuid
		        where s.node_kind = 'artifact' and s.node_address = $2
		          and s.text = '' and s.text_seq = old.number and s.search = old.search),
		       (select count(*) from node_search where node_kind = 'block' and node_address = any($3::text[]))
	`, issue.PrimaryArtifactID, spec.refKey, roomBlocks).Scan(&whole, &blocksBeside); err != nil {
		t.Fatalf("read the search rows of %s and its blocks: %v", spec.refKey, err)
	}
	if whole != 1 || blocksBeside != len(roomBlocks) {
		t.Fatalf("node_search holds %d whole-document rows matching the version vector of %s and %d block rows, want 1 and %d",
			whole, spec.refKey, blocksBeside, len(roomBlocks))
	}
	if kind := censusKindOf(t, storeCensus(t, database), "document_search_node"); kind.Rows == 0 || kind.Nodes == 0 ||
		kind.RowDifferences != 0 || kind.NodeDifferences != 0 {
		t.Fatalf("document_search_node census: %+v; want document rows and zero differences", kind)
	}

	// Emptied, document_search changes no answer of this build's search, which reads node_search.
	if _, err := database.Pool.Exec(ctx, `delete from document_search`); err != nil {
		t.Fatalf("empty document_search: %v", err)
	}
	if again := searchKeyHits(searchResponse(t, handler, "q="+word+"&keywords_only=true")); !slices.Equal(again, searchKeyHits(lexical)) {
		t.Errorf("with document_search empty the search route answers %+v for %q, want %+v as before: this build's search reads node_search alone", again, word, searchKeyHits(lexical))
	}

	requireRelatedDocumentRows(t)
	// The issue matches the check's text twice besides its spec, and either of its rows takes the
	// spec's place: the check keeps one item per owner, and a row that is no document replaces the
	// owner's document (fillRelatedItems). Its keyword vector carries its spec's text (migration
	// 20261105145242), so it is emptied. Its own embedding is the fake embedder's default vector,
	// which the check's text embeds to as well, so it answers the meaning leg at cosine 1
	// (meaning_issue_candidates reads it whatever its hash); it is written orthogonal to that
	// vector. The document windows hold angledVector(0.9, 2), orthogonal to it too, so the keyword
	// document arm is what must answer the spec, and the near-copy gate must score its vector.
	if _, err := database.Pool.Exec(ctx, `update issues set search = ''::tsvector where key = $1`, issue.Key); err != nil {
		t.Fatalf("clear the issue vector of %s: %v", issue.Key, err)
	}
	if tag, err := database.Pool.Exec(ctx, `update embeddings set embedding = $2::vector where kind = 'issue' and id = $1`,
		issue.Key, embed.Literal(angledVector(0, 3))); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("turn the embedding of %s away from the check's text: %v (%d rows)", issue.Key, err, tag.RowsAffected())
	}
	refreshTermStats(t, database, project)
	created := createIssueRequest(t, handler, map[string]any{
		"project": project, "title": word + " theodolite recalibration mounting bracket", "force": true,
	})
	requireAnsweredCreate(t, created)
	suggestions := createdSuggestions(t, created)
	if !slices.ContainsFunc(suggestions.Related, func(hit model.WriteSuggestion) bool {
		return hit.Kind == "document" && hit.ID == issue.PrimaryArtifactID
	}) {
		t.Errorf("with document_search empty, the related check returned %+v, want document %s",
			suggestions, issue.PrimaryArtifactID)
	}
	if !slices.ContainsFunc(suggestions.TitleMatches, func(hit model.DuplicateCandidate) bool {
		return hit.Key == issue.Key
	}) {
		t.Errorf("with document_search empty, the duplicate check returned %+v, want the issue of document %s",
			suggestions.TitleMatches, issue.PrimaryArtifactID)
	}

	// The census reads the authors a moved block's node is to name off the spec's record of the
	// block before the move, never off that node: with the authored_by tuples the conversion
	// carried taken off both nodes, each block reads as a row no tuple states naming alice.
	if removed, err := database.Pool.Exec(ctx, `delete from graph_tuples where from_kind = $1 and from_id = any($2) and kind = $3`,
		refs.BlockNodeKind, roomBlocks, graphstore.TupleAuthoredBy); err != nil || removed.RowsAffected() == 0 {
		t.Fatalf("take the authored_by the conversion carried off the blocks %v: %d rows, %v", roomBlocks, removed.RowsAffected(), err)
	}
	report = storeCensus(t, database)
	var want, read []string
	for _, address := range roomBlocks {
		want = append(want, "a row no tuple states: block_author "+address+" "+graphstore.KindPerson+":alice "+
			graphstore.TupleAuthoredBy+" -> "+graphstore.KindPerson+" alice")
	}
	authors := censusKindOf(t, report, "block_author")
	for _, difference := range authors.ReportedRows {
		read = append(read, difference.Cause+": "+difference.At)
	}
	slices.Sort(want)
	slices.Sort(read)
	if authors.Rows != 2 || authors.Nodes != 0 || authors.RowDifferences != 2 || authors.NodeDifferences != 0 || !slices.Equal(read, want) {
		t.Errorf("with the carried authors taken off the store census reads block_author rows=%d tuples=%d, the row differences %v and the tuple differences %+v, want two rows, no tuple and %v",
			authors.Rows, authors.Nodes, read, authors.ReportedNodes, want)
	}
}

// Inspect the document keyword arm in the statements the check runs, not a separate copy.
// The page may still look up a meaning hit's block vector; that lookup is not a keyword arm.
func requireRelatedDocumentRows(t *testing.T) {
	t.Helper()
	for _, statement := range []struct{ name, sql string }{
		{"relatedKeywordQuery", relatedKeywordQuery},
		{"relatedKeywordExcludingQuery", relatedKeywordExcludingQuery},
		{"relatedMeaningQuery", relatedMeaningQuery},
		{"relatedMeaningExcludingQuery", relatedMeaningExcludingQuery},
		{"relatedMeaningScopedQuery", relatedMeaningScopedQuery},
		{"relatedMeaningScopedExcludingQuery", relatedMeaningScopedExcludingQuery},
		{"relatedMeaningJoinedQuery", relatedMeaningJoinedQuery},
		{"relatedMeaningJoinedExcludingQuery", relatedMeaningJoinedExcludingQuery},
	} {
		_, arms, starts := strings.Cut(statement.sql, "select 'document' as kind, 'keyword'")
		arm, _, ends := strings.Cut(arms, "select 'comment' as kind, 'keyword'")
		if !starts || !ends || strings.Count(arm, "node_search") != 1 ||
			!strings.Contains(arm, "s.node_kind = 'artifact' and s.search @@") ||
			!strings.Contains(arm, "ts_rank_cd(s.search,") || strings.Contains(arm, "v.search") {
			t.Errorf("%s must match and rank its document keyword arm from document rows alone:\n%s", statement.name, arm)
		}
	}
}
