package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// searchFreshness is how long a write has to become findable. A person typing the words of
// something they just wrote expects it back, so the index is held to a stated wait rather than
// to "eventually" (dispatch://INFRA-1042, check k5).
const searchFreshness = 60 * time.Second

// within runs probe until it answers true, failing the test when searchFreshness has passed
// since written. It polls rather than waits the whole time out, so a check that holds at once -
// which every one of these does, since each kind is indexed in the write's own transaction -
// costs nothing.
func within(t *testing.T, written time.Time, what string, probe func() bool) {
	t.Helper()
	for {
		if probe() {
			return
		}
		if waited := time.Since(written); waited > searchFreshness {
			t.Fatalf("%s was still not there %s after the write, want it within %s", what, waited, searchFreshness)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// windowsUnder counts the embeddings windows of kind kind the indexers wrote under one owner:
// a document's under its artifact id, a comment's, an ask's or a message's under <kind>/<id>
// (refs.writeWindows).
func windowsUnder(t *testing.T, database *store.Store, kind, owner string) int64 {
	t.Helper()
	var count int64
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from embeddings where kind = $1 and split_part(id, '#', 1) = $2`, kind, owner).Scan(&count); err != nil {
		t.Fatalf("count the %s windows of %s: %v", kind, owner, err)
	}
	return count
}

// A write of each kind the index holds - an issue, and a document, a comment, an ask and a
// message - is returned by a search of its own words within 60 seconds of the write, and the
// blocks of the document, the comment, the ask and the message are indexed as windows under
// them over that same wait.
func TestASearchReturnsEveryIndexedKindWithinSixtySeconds(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)

	written := time.Now()
	corpus := seedSearchCorpus(t, handler)

	for _, item := range []struct{ query, kind, id string }{
		{"astrolabe", "document", corpus.primaryArtifactID},
		{"sextant", "comment", corpus.commentID},
		{"quadrant", "ask", corpus.askID},
		{"compass", "message", corpus.messageID},
		{"instruments", "issue", corpus.issueKey},
	} {
		within(t, written, "the "+item.kind+" searched for by its own word "+item.query, func() bool {
			return searchResultOf(searchResponse(t, handler, "q="+item.query), item.kind, item.id) != nil
		})
	}

	for _, window := range []struct{ kind, owner string }{
		{"document_block", corpus.primaryArtifactID},
		{"comment_block", "comment/" + corpus.commentID},
		{"ask_part", "ask/" + corpus.askID},
		{"message_block", "message/" + corpus.messageID},
	} {
		within(t, written, "a "+window.kind+" window under "+window.owner, func() bool {
			return windowsUnder(t, database, window.kind, window.owner) > 0
		})
	}
	within(t, written, "the issue's own embeddings row", func() bool {
		return windowsUnder(t, database, "issue", corpus.issueKey) == 1
	})
}

// The coverage pass over a database holding content no trigger saw, and the queue behind it,
// leave every indexed kind complete: the route reports each with every item current, nothing
// lacking, nothing failed and no oldest age, and the total the same.
func TestTheBackfillLeavesEveryIndexedKindComplete(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	seedSearchCorpus(t, handler)

	// What a restore or a replication writes with the triggers off: the issue is there and its
	// queue row is not, which is the gap the coverage pass exists to close.
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, "delete from embeddings where kind = 'issue'"); err != nil {
		t.Fatalf("delete the issue's queue row: %v", err)
	}
	if err := embedqueue.Cover(ctx, embedqueue.Deps{Store: database, Embedder: embedder}); err != nil {
		t.Fatalf("the coverage pass: %v", err)
	}
	processAllPending(t, database, embedder)

	report := searchCoverageResponse(t, handler)
	items := requireEveryKindCovered(t, report)
	for kind, counts := range coverageKinds(report) {
		if counts.Current != counts.Items || counts.Lacking != 0 || counts.Failed != 0 || counts.OldestLackingSeconds != nil {
			t.Errorf("%s = %+v, want every item current with nothing lacking, nothing failed and no oldest age", kind, counts)
		}
	}
	if report.Total.Current != items || report.Total.Lacking != 0 || report.Total.Failed != 0 || report.Total.OldestLackingSeconds != nil {
		t.Errorf("total = %+v, want current %d with nothing lacking, nothing failed and no oldest age", report.Total, items)
	}
}

// notIndexedKindTables is each kind the route must name as not indexed, in the order it must
// name them, with the statement this test counts it by: the table that holds that kind, read
// here so the number the route serves is checked against its source rather than against itself.
var notIndexedKindTables = []struct{ kind, count string }{
	{"component", "select count(*) from components"},
	{"session", "select coalesce((select jsonb_array_length(sessions) from route_roster), 0)"},
	{"document_version", "select count(*) from artifact_versions"},
	{"result", "select count(*) from check_results"},
	{"handoff", "select count(*) from handoffs"},
}

// notIndexedTableCounts counts every notIndexedKindTables kind from its own table.
func notIndexedTableCounts(t *testing.T, database *store.Store) map[string]int64 {
	t.Helper()
	counts := make(map[string]int64, len(notIndexedKindTables))
	for _, kind := range notIndexedKindTables {
		var count int64
		if err := database.Pool.QueryRow(context.Background(), kind.count).Scan(&count); err != nil {
			t.Fatalf("count the %s rows: %v", kind.kind, err)
		}
		counts[kind.kind] = count
	}
	return counts
}

// requireNotIndexedNamesEveryKind holds the report's not_indexed list to every kind the route
// does not cover, in order and with no kind twice, and answers each kind's reported count. A
// kind missing from the list is the failure this check exists for: the route omitting a kind it
// covers nothing of leaves a reader unable to tell it from a covered kind with nothing lacking.
func requireNotIndexedNamesEveryKind(t *testing.T, report model.SearchCoverage) map[string]int64 {
	t.Helper()
	if len(report.NotIndexed) != len(notIndexedKindTables) {
		t.Fatalf("not_indexed = %+v, want one row for each of the %d kinds the route does not cover",
			report.NotIndexed, len(notIndexedKindTables))
	}
	counts := make(map[string]int64, len(report.NotIndexed))
	for position, row := range report.NotIndexed {
		if want := notIndexedKindTables[position].kind; row.Kind != want {
			t.Fatalf("not_indexed[%d] names %q, want %q: %+v", position, row.Kind, want, report.NotIndexed)
		}
		counts[row.Kind] = row.Items
	}
	return counts
}

// requireNotIndexedCountsItsTables holds every count the route serves for a kind it does not
// cover to the count of the table that holds that kind.
func requireNotIndexedCountsItsTables(t *testing.T, database *store.Store, report model.SearchCoverage) map[string]int64 {
	t.Helper()
	reported := requireNotIndexedNamesEveryKind(t, report)
	for kind, count := range notIndexedTableCounts(t, database) {
		if reported[kind] != count {
			t.Errorf("not_indexed %s = %d, want the %d its table holds", kind, reported[kind], count)
		}
	}
	return reported
}

// seedNotIndexedKinds writes one component, two sessions, one result and one handoff, each
// through the table the route counts that kind from.
func seedNotIndexedKinds(t *testing.T, database *store.Store, corpus searchCorpus, project string) {
	t.Helper()
	ctx := context.Background()
	for _, write := range []struct {
		what, sql string
		args      []any
	}{
		{"the architecture source and snapshot a component belongs to",
			`insert into architecture_sources (project_key, id, repo, branch, installation_id, created_by)
			 values ($1, 1, 'ship/arch', 'main', 1, '{"kind":"system","id":"test"}')`,
			[]any{project}},
		{"the architecture snapshot a component belongs to",
			`insert into architecture_snapshots (id, project_key, commit, files) values (900, $1, 'c1', '{}')`,
			[]any{project}},
		{"a component",
			`insert into components (project_key, id, title, prose, snapshot_id, source_id)
			 values ($1, 'navigation', 'Navigation', 'The instruments the ship steers by.', 900, 1)`,
			[]any{project}},
		// The listener's stored session list, which is Dispatch's whole record of which sessions
		// exist. Written over whatever a route-status read left, since no read after this one
		// touches it.
		{"the roster's two sessions",
			`insert into route_roster (answered, sessions, sent_at, expires_at)
			 values (true, '[{"session_id":"s-navigator"},{"session_id":"s-helm"}]'::jsonb, now(), now() + interval '5 seconds')
			 on conflict (id) do update
			 set answered = excluded.answered, sessions = excluded.sessions,
			     sent_at = excluded.sent_at, expires_at = excluded.expires_at`,
			nil},
		{"a result",
			`insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id,
			                            check_version, check_sha256, role, commit_sha, verdict, actor)
			 values ($1, $1, $2::uuid, 'k1', 1, 'a-check-sha', 'tester', repeat('a', 40), 'pass',
			         '{"kind":"user","id":"alice"}'::jsonb)`,
			[]any{corpus.issueKey, corpus.primaryArtifactID}},
		{"a handoff",
			`insert into handoffs (issue_key, phase, version, body, author)
			 values ($1, 'implement', 1, '{}', '{"kind":"user","id":"alice"}'::jsonb)`,
			[]any{corpus.issueKey}},
	} {
		if _, err := database.Pool.Exec(ctx, write.sql, write.args...); err != nil {
			t.Fatalf("write %s: %v", write.what, err)
		}
	}
}

// The coverage route names every kind of node it does not index - a component, a session, a
// document version, a result and a handoff - with how many of that kind the database holds,
// rather than leaving it out. On a database whose every indexed kind is complete, a kind the
// route omitted would be indistinguishable from one that is covered with nothing lacking, which
// is what the list stops. The uncovered counts come from the tables that hold those kinds, so
// they move when those tables do, and they are in no total: the covered kinds' numbers are
// exactly what they were.
func TestSearchCoverageNamesEveryKindItDoesNotIndex(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	corpus := seedSearchCorpus(t, handler)
	processAllPending(t, database, embedder)

	before := searchCoverageResponse(t, handler)
	for kind, counts := range coverageKinds(before) {
		if counts.Items < 1 || counts.Current != counts.Items || counts.Lacking != 0 || counts.Failed != 0 {
			t.Fatalf("%s = %+v, want a complete kind to tell an uncovered one from", kind, counts)
		}
	}
	empty := requireNotIndexedCountsItsTables(t, database, before)
	for _, kind := range []string{"component", "session", "result", "handoff"} {
		if empty[kind] != 0 {
			t.Errorf("not_indexed %s = %d before anything of it is written, want 0 named rather than omitted", kind, empty[kind])
		}
	}
	// A document's versions exist from the moment the issue has a spec, and none of them is an
	// item of any covered kind: the index holds windows of the document's current blocks alone.
	if empty["document_version"] < 1 {
		t.Errorf("not_indexed document_version = %d, want the versions the corpus's documents hold", empty["document_version"])
	}

	seedNotIndexedKinds(t, database, corpus, "SRCH")

	after := searchCoverageResponse(t, handler)
	counts := requireNotIndexedCountsItsTables(t, database, after)
	for kind, want := range map[string]int64{
		"component": 1, "session": 2, "result": 1, "handoff": 1,
		"document_version": empty["document_version"],
	} {
		if counts[kind] != want {
			t.Errorf("not_indexed %s = %d, want %d", kind, counts[kind], want)
		}
	}

	// Writing them changed nothing the index covers: an uncovered kind is in no kind's counts
	// and in no total.
	if after.Total != before.Total {
		t.Errorf("total = %+v, want the %+v it was: an uncovered kind is in no total", after.Total, before.Total)
	}
	if after.Kinds != before.Kinds {
		t.Errorf("kinds = %+v, want the %+v they were", after.Kinds, before.Kinds)
	}
}

// Every kind the report names is named once, whether it is covered or not: no kind is in both
// halves, which would let one reader count it and another not.
func TestSearchCoverageNamesNoKindTwice(t *testing.T) {
	handler := newTestHandler(t)

	report := searchCoverageResponse(t, handler)
	covered := coverageKinds(report)
	seen := map[string]bool{}
	for _, row := range report.NotIndexed {
		if _, both := covered[row.Kind]; both {
			t.Errorf("%q is both a covered kind and a kind the route says it does not index", row.Kind)
		}
		if seen[row.Kind] {
			t.Errorf("not_indexed names %q twice: %+v", row.Kind, report.NotIndexed)
		}
		seen[row.Kind] = true
	}
	if len(seen) != len(notIndexedKindTables) {
		t.Errorf("not_indexed named %d kinds, want the %d the route does not cover", len(seen), len(notIndexedKindTables))
	}
}
