package api

//ci:builds packages/envoy ./cmd/dispatch searchKeyConvert builds envoy-dispatch, whose convert-issues TestSearchFollowsABlockThatChangesOwner runs as the conversion of this issue's spec R6
//ci:reads packages/envoy/internal/dispatch/store/migrations/*_issue_tree_*.up.sql searchKeyMigrationSet, which TestSearchFollowsABlockThatChangesOwner reads through pgmigrate.TouchedTables

import (
	"bytes"
	"context"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// searchKeyCommit is the commit the test stamps the envoy-dispatch it converts with, and names in
// the server_build_announcements row it writes, so the fleet gate of this issue's spec R6 passes.
const searchKeyCommit = "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"

// searchKeyConvert runs `envoy-dispatch convert-issues --project <project>` over database, the
// conversion of this issue's spec R6 by hand, which the checks take as its entry point: with
// dryRun, `--dry-run`, which writes as a real run does and rolls each issue's transaction back. It
// fails t unless the command exits 0, and answers what it printed.
func searchKeyConvert(t *testing.T, database *store.Store, project string, dryRun bool) string {
	t.Helper()
	binary := cmdtest.BuildCommand(t, cmdtest.Command{
		Name: "envoy-dispatch", Dir: "../../../cmd/dispatch",
		Flags: []string{"-ldflags", "-X main.buildCommit=" + searchKeyCommit},
	})
	args := []string{"convert-issues", "--project", project}
	if dryRun {
		args = append(args, "--dry-run")
	}
	home := t.TempDir()
	command := exec.Command(binary, args...)
	command.Dir = home
	command.Env = []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_DATA_HOME=" + home,
		"DATABASE_URL=" + database.Pool.Config().ConnString(), "DISPATCH_NATS_DISABLED=1",
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("envoy-dispatch %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	t.Logf("envoy-dispatch %s printed:\n%s", strings.Join(args, " "), out)
	return string(out)
}

// searchKeyOpenTheFleet takes away the live process holdIssuesUnconverted added, which lists no
// issue room, and writes the announcement of the build the conversion runs as (searchKeyCommit),
// so the conversion's fleet gate passes: no live process serves another build, and the newest
// announcement names the commit the conversion runs against.
func searchKeyOpenTheFleet(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `delete from server_processes where heartbeat_at > now() + interval '1 hour'`); err != nil {
		t.Fatalf("take away the live process that lists no issue room: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `insert into server_build_announcements (commit) values ($1)`, searchKeyCommit); err != nil {
		t.Fatalf("announce the build the conversion runs as: %v", err)
	}
}

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
// stay for a process of the release before, which reads that table alone.
func TestSearchFollowsABlockThatChangesOwner(t *testing.T) {
	t.Parallel()
	const project = "SKEY"
	const query = "theodolite recalibration"
	const first = "Alpha notes on the mounting bracket."
	const moved = "The theodolite needs recalibration before the survey."
	const word = "zorptangle"
	ctx := context.Background()
	embedder := &fakeEmbedder{vectors: map[string][]float32{query: queryVector()}}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
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
	searchKeyOpenTheFleet(t, database)
	searchKeyConvert(t, database, project, true)
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
	searchKeyConvert(t, database, project, false)
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
	report, err := graphcensus.StoreCensus(ctx, database.Pool.Config().ConnString(), false, docs.ReturnDeclined)
	if err != nil {
		t.Fatalf("take the store census: %v", err)
	}
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
	lexical := searchResponse(t, handler, "q="+word+"&keywords_only=true")
	if hits := searchKeyHits(lexical); len(hits) != 1 || hits[0].block != firstBlock {
		t.Errorf("the search route answers %+v for %q, want one hit naming the node %s that matched rather than a document", hits, word, address)
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

	// Emptied, document_search changes no answer of this build's search, which reads node_search.
	if _, err := database.Pool.Exec(ctx, `delete from document_search`); err != nil {
		t.Fatalf("empty document_search: %v", err)
	}
	if again := searchKeyHits(searchResponse(t, handler, "q="+word+"&keywords_only=true")); !slices.Equal(again, searchKeyHits(lexical)) {
		t.Errorf("with document_search empty the search route answers %+v for %q, want %+v as before: this build's search reads node_search alone", again, word, searchKeyHits(lexical))
	}
}
