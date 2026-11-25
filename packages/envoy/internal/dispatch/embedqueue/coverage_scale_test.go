package embedqueue

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// coverageScaleURLEnv names the Postgres server TestCoverageStatementScale builds its corpora on,
// one database per scale, which it creates and drops: never a server that holds anything else of
// value, and never CI's, which leaves it unset. Its database path is ignored, as storetest's
// DISPATCH_TEST_DATABASE_URL's is. coverageScalesEnv lists the scales, space-separated; 1 is
// production's counts of 2026-11-17. coverageScaleServeEnv is scripts/coverage-scale.sh's
// COVERAGE_SCALE_SERVE, which reaches the test as it is: set to 1, it makes the test copy the
// database of the largest scale before any measurement and leave the copy for a server's start on
// it (copyForServerStart).
const (
	coverageScaleURLEnv   = "DISPATCH_COVERAGE_SCALE_URL"
	coverageScalesEnv     = "DISPATCH_COVERAGE_SCALES"
	coverageScalesDefault = "1 2"
	coverageScaleServeEnv = "COVERAGE_SCALE_SERVE"
)

// scaleMeasurementBound is the bound the fill and each recount the scale test times run under in
// place of coverageFillTimeout and coverageRecountTimeout, the go test timeout
// scripts/coverage-scale.sh sets: what is timed is how long each takes to its end, which the test
// then judges against the two bounds the counts loop holds them to.
const scaleMeasurementBound = 3 * time.Hour

// coverageCorpusIssues, coverageCorpusVersions and coverageCorpusWindows are production's counts
// of 2026-11-17, the corpus at scale 1, and coverageCorpusSpecSizes the bytes of the 30 sampled
// INFRA specs each issue's latest spec takes in turn: what testdata/coverage_corpus.sql builds,
// held here a second time so the test checks the corpus it loaded rather than trusting the
// script that loaded it.
const (
	coverageCorpusIssues   = 11151
	coverageCorpusVersions = 216414
)

var (
	coverageCorpusWindows = []struct {
		kind string
		rows int64
	}{
		{"document_block", 738255},
		{"comment_block", 184555},
		{"message_block", 48386},
		{"ask_part", 12816},
	}
	coverageCorpusSpecSizes = []int64{
		6568, 2203, 19057, 8454, 1661, 533, 4488, 1500, 1383, 2190,
		1692, 2015, 1687, 1972, 2873, 27483, 48689, 1123, 100033, 100970,
		3272, 29335, 3380, 5772, 22233, 6240, 8548, 4371, 10955, 8145,
	}
)

// coverageScale is one scale the corpus is built at: its value, and its text as the corpus
// script and the database's name read it.
type coverageScale struct {
	value float64
	text  string
}

// coverageScaleTables are the tables the corpus fills: analyzed before the record step, whose
// statements are planned on their statistics, and vacuumed for the fresh state.
const coverageScaleTables = "issues, artifacts, artifact_versions, embeddings, embedding_issue_sources"

// scaleState is one state of embeddings the recount is timed in, as R2 of dispatch://INFRA-4241
// names them: its name; identity, the configured embedder's identity in the state, the one the
// counts are compared for; changeEmbedder, whether the state begins with the configured embedder
// changing to identity, whose pass's identity step drops every vector another identity made, so
// that the state is not made unless that step drops a vector; prepare, the statements that then put
// the table in the state from the one measured before, each run on its own, since VACUUM runs in no
// transaction block; and lagging, whether the state's statistics record more of the table as
// all-visible than is, so that the state is not made unless they record at least
// laggingVisibleShare.
type scaleState struct {
	name           string
	identity       string
	changeEmbedder bool
	prepare        []string
	lagging        bool
}

// changedEmbedderIdentity is the identity the scale test's embedder change moves the configured
// embedder to: another Azure deployment, which made no vector of the corpus.
const changedEmbedderIdentity = "azure:example.services.ai.azure.com/embed-v-5-0"

// scaleStates are the six states, in the order they are measured, each made from the one before:
// four of embeddings' visibility map and statistics, then two of an embedder change. Autovacuum
// is off on embeddings, so nothing else sets its visibility map or statistics. They are the states
// INFRA-4241 measured main's coverage statement in, and the recount's window statement
// (windowClasses), the one statement of the counts that reads every window row, reads those rows
// the way that statement's window arm did, so each is a state the recount's time can turn on.
//
//   - fresh: every table vacuumed and analyzed, as a quiet production table is, so nearly every
//     page of embeddings is all-visible.
//   - third: one row of every third heap page of embeddings updated, then the table analyzed, so
//     about a third of its pages are not all-visible and its statistics say so, as an ANALYZE
//     records them between two vacuums of a table the queue keeps writing. INFRA-4241's tester
//     found an earlier version of this statement, which left the planner free, reading the
//     unembedded half of the windows by a Seq Scan of embeddings once a third or more of the pages
//     were not all-visible (dispatch://INFRA-4241/artifact/tester-scale-job-b3756b91-log).
//   - all: one row of every page updated the same way, then the table analyzed, so no page is
//     all-visible and its statistics say so.
//   - stale: embeddings vacuumed, so every page is all-visible again; one row of every even page
//     updated, then the table analyzed, so its statistics record about half the pages
//     all-visible; then one row of every odd page updated with no ANALYZE after it, so no page is
//     all-visible while the statistics still record about half. Autoanalyze waits for a tenth of
//     the rows to change, and one row of each of half the pages is fewer (97,369 rows against
//     99,566 at production's size), so a production table can stand so until its next ANALYZE.
//     Its statistics must record at least laggingVisibleShare of the pages all-visible; an
//     earlier version of this statement, which read the windows by index-only scans at that
//     share, fetched every window row from the heap one at a time in this state
//     (dispatch://INFRA-4241/artifact/tester-r2-job-2f9a6fc1-log).
//   - embedder: the configured embedder changes to changedEmbedderIdentity, and its pass's
//     identity step drops every vector another identity made, over every covered kind
//     (dropForeignVectors over coverageKinds, as runIdentitySteps runs it), so every window row
//     leaves the embedded half for the other until the queue embeds it again, which takes hours at
//     production's size; with no ANALYZE after it, so the statistics are the stale state's.
//     INFRA-4241's tester found an earlier version of this statement, which had the bitmap from
//     the planner's underestimate of the embedded half, reading the windows in this state through
//     embeddings_kind_project, with the blank test's regex as the heap scan's filter
//     (dispatch://INFRA-4241/artifact/tester-r3-job-0ec6b4ad-log).
//   - embedder-analyzed: the same table, then analyzed.
//
// From the embedder state on, the statements are compared and timed for the new identity; which
// items there are stays the fresh state's, since dropping a vector removes no row.
var scaleStates = []scaleState{
	{name: "fresh", identity: configuredIdentity, prepare: []string{"vacuum (analyze) " + coverageScaleTables}},
	{name: "third", identity: configuredIdentity, prepare: []string{updateOneRowOfPages("where ((ctid::text::point)[0])::int % 3 = 0"), "analyze embeddings"}},
	{name: "all", identity: configuredIdentity, prepare: []string{updateOneRowOfPages(""), "analyze embeddings"}},
	{name: "stale", identity: configuredIdentity, lagging: true, prepare: []string{
		"vacuum embeddings",
		updateOneRowOfPages("where ((ctid::text::point)[0])::int % 2 = 0"),
		"analyze embeddings",
		updateOneRowOfPages("where ((ctid::text::point)[0])::int % 2 = 1"),
	}},
	{name: "embedder", identity: changedEmbedderIdentity, changeEmbedder: true},
	{name: "embedder-analyzed", identity: changedEmbedderIdentity, prepare: []string{"analyze embeddings"}},
}

// updateOneRowOfPages updates one row of each heap page of embeddings that where selects (a WHERE
// clause on the page's number, (ctid::text::point)[0]; empty for every page), setting
// pending_since to its own value. The row's new version is one no vacuum has seen, so neither the
// page that held the row nor the page its new version lands on is all-visible any more, and no
// row's answer changes.
func updateOneRowOfPages(where string) string {
	return `update embeddings e set pending_since = e.pending_since
		from (select distinct on ((ctid::text::point)[0]) ctid from embeddings ` + where + `
		      order by (ctid::text::point)[0], ctid) s
		where e.ctid = s.ctid`
}

// coverageVisibleShare reads the share of embeddings' pages the statistics record as all-visible:
// pg_class.relallvisible over relpages, which only VACUUM and ANALYZE set. The scale test logs it
// in each state. A table with no page recorded counts as all-visible.
const coverageVisibleShare = `select case when relpages > 0 then relallvisible::float8 / relpages else 1 end
	from pg_class where oid = 'embeddings'::regclass`

// laggingVisibleShare is the least share of embeddings' pages (coverageVisibleShare) that the
// statistics of a lagging table must record as all-visible while no page is: the scale test's
// stale state and the table of
// TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap are not made unless
// their statistics record at least this share. The planner prices an index-only scan's heap
// fetches by that record, so on such a table it takes that share of a window index's rows to be
// read from the index alone, where every one is fetched from the heap. An earlier version of this
// statement read each window half by an index-only scan of its own index from this share up, and
// on such a table fetched every window row from the heap one at a time (planCoverage gives its
// times, dispatch://INFRA-4241/artifact/tester-r2-job-2f9a6fc1-log).
const laggingVisibleShare = 0.3

// coverageWindowIndexes are the two window indexes the recount's window statement reads and the
// report probes, each with the migration that builds it, whose file rebuildWindowIndexes builds it
// again from.
var coverageWindowIndexes = []struct{ index, migration string }{
	{"embeddings_window_embedded_since", "20261124011836_embeddings_window_embedded_since"},
	{"embeddings_window_unembedded_since", "20261124011835_embeddings_window_unembedded_since"},
}

// windowIndexBuild is what one build of a window index measured: the index, how long its
// migration's statement took, and the index's size once built.
type windowIndexBuild struct {
	index    string
	took     time.Duration
	size     int64
	sizeText string
}

// groupTime is how long one statement of a fill or a recount took: a group's statement
// (recountStatement over the group's classes), or, as "refresh", every statement of the refresh of
// the issues' state rows that comes before the groups (refreshIssueStates), the finds and the
// refreshes, summed.
type groupTime struct {
	group string
	took  time.Duration
}

// countsRun is what one fill or one recount measured: its whole time, from its call to its
// answer, and each of its statements' (groupTime), in the order it sends them.
type countsRun struct {
	whole  time.Duration
	groups []groupTime
}

// String is the run as the coverage-scale lines print it.
func (r countsRun) String() string {
	parts := make([]string, len(r.groups))
	for i, group := range r.groups {
		parts[i] = group.group + " " + inMilliseconds(group.took)
	}
	return inMilliseconds(r.whole) + " (" + strings.Join(parts, ", ") + ")"
}

// statePoint is what one state measured at one scale: how many vectors its embedder change
// dropped (none in a state that changes no embedder); how many of embeddings' pages its
// statistics record as all-visible, of how many, and that share (coverageVisibleShare); and the
// whole recount run in it.
type statePoint struct {
	dropped        int64
	visible, pages int64
	share          float64
	recount        countsRun
}

// coverageScalePoint is what one scale measured: the corpus's items (every covered kind's, as the
// report counts them), the builds of the window indexes, in coverageWindowIndexes' order, the
// fill, and each state's point, in scaleStates' order.
type coverageScalePoint struct {
	scale  coverageScale
	items  int64
	builds []windowIndexBuild
	fill   countsRun
	states []statePoint
}

// TestCoverageStatementScale times the coverage counts' fill and recount, the statements of the
// counts that read every row they count, on a synthetic corpus at each scale
// DISPATCH_COVERAGE_SCALES names, and judges them against the bounds the counts loop holds them
// to: it fails when four times the fill at the largest scale passes coverageFillTimeout, or four
// times the longest whole recount there passes coverageRecountTimeout. The route reads the kept
// counts and never main's coverage statement (coverageRead), so the route's own time is held
// elsewhere (TestCoverageReadsTheSameRowsAtTwiceTheCorpus) and not here. It also times the builds of
// the two window indexes (rebuildWindowIndexes), which a deploy of their migrations waits for
// before it serves. scripts/coverage-scale.sh runs it in a Job of its own, with a Postgres of its
// own; a pod's test server is too small and too shared for corpora of production's size, and CI
// never sets the URL that runs it.
//
// Each scale gets a database of its own, migrated by the store's own path (store.Migrate), with
// autovacuum off on embeddings, so that nothing but the test sets its visibility map or its
// statistics; the corpus loaded (testdata/coverage_corpus.sql over this repository's prose) with
// its tables' triggers off, so no count holds anything of it, as at the first deploy of the counts;
// the record step run to completion (recordSources) as the boot pass leaves a database; and the two
// window indexes dropped and built again by their migrations' own statements, each build timed.
// The fill (FillCoverage) is then timed once, before any count, so its refresh computes every
// issue's state row. The recount (recountCoverage) is timed in each state of scaleStates in turn:
// freshly vacuumed; about a third of embeddings' pages not all-visible after an ANALYZE; every page
// not all-visible after an ANALYZE; every page not all-visible while the statistics still record
// about half of them all-visible from an earlier ANALYZE; and, once the configured embedder has
// changed and its identity step has dropped every vector the corpus held, with the statistics of
// the state before and then after an ANALYZE. After each recount the report's three sides are
// compared in one snapshot for the state's configured identity (requireMainsAnswerFor), so a time
// is the time of counts that answer what main's statements answer. The database is dropped before
// the next scale is built.
//
// Under COVERAGE_SCALE_SERVE=1 (coverageScaleServeEnv) the database of the largest scale, S, is
// copied once the corpus is loaded, analyzed, recorded and checked, before the window indexes are
// built again and before any count (copyForServerStart): the copy holds the corpus and no row of
// the counts, done_at unset, as the first deploy of the counts finds production. The test prints
// its name on a line that starts `coverage-scale-database:` and leaves it, and
// scripts/coverage-scale.sh starts the server on it (cmd/dispatch's
// TestTheServerFillsTheCoverageCountsAtScale) and drops it.
//
// The fill and each recount run under scaleMeasurementBound rather than their own bounds, so the
// time read is their whole length, and on a pool whose statements a tracer times (statementTimer),
// so each group statement's time is read too. In each state the size at which the whole recount
// reaches coverageRecountTimeout is taken from the two smallest scales' points, fitted both
// linearly and as a power law t = a × s^k; the smaller of the two is that state's, and the state
// with the smallest is the slowest. Each scale prints a line that starts `coverage-scale:` with its
// fill and its recounts, every group statement's time among them, and the last line it prints
// starts `coverage-scale:` and names every number the verdict rests on, so a reader of the Job's
// log judges it from its end.
func TestCoverageStatementScale(t *testing.T) {
	server := os.Getenv(coverageScaleURLEnv)
	if server == "" {
		t.Skip(coverageScaleURLEnv + " is unset: it must name a Postgres server this test may create and drop " +
			"databases on, which packages/envoy/scripts/coverage-scale.sh starts and names; CI never sets it")
	}
	fillBound, recountBound := coverageFillTimeout, coverageRecountTimeout
	setForTest(t, &coverageFillTimeout, scaleMeasurementBound)
	setForTest(t, &coverageRecountTimeout, scaleMeasurementBound)
	scales := coverageScales(t)
	serve := os.Getenv(coverageScaleServeEnv) == "1"
	largest := slices.MaxFunc(scales, func(a, b coverageScale) int { return cmp.Compare(a.value, b.value) })
	prose := repositoryProse(t)
	script, err := os.ReadFile(filepath.Join("testdata", "coverage_corpus.sql"))
	if err != nil {
		t.Fatalf("read the corpus script: %v", err)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, serverDatabaseURL(t, server, "postgres"))
	if err != nil {
		t.Fatalf("connect to %s's postgres database: %v", coverageScaleURLEnv, err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	var points []coverageScalePoint
	for _, scale := range scales {
		t.Run("scale="+scale.text, func(t *testing.T) {
			points = append(points, measureCoverageAtScale(t, admin, server, scale, prose, string(script), serve && scale == largest))
		})
	}
	judgeCoverageScale(t, scales, points, fillBound, recountBound)
}

// coverageScales reads DISPATCH_COVERAGE_SCALES, or its default, as scales above 0, each named
// once.
func coverageScales(t *testing.T) []coverageScale {
	t.Helper()
	raw := os.Getenv(coverageScalesEnv)
	if strings.TrimSpace(raw) == "" {
		raw = coverageScalesDefault
	}
	var scales []coverageScale
	for _, field := range strings.Fields(raw) {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil || !(value > 0) || math.IsInf(value, 0) {
			t.Fatalf("%s names %q, which is no scale above 0", coverageScalesEnv, field)
		}
		if slices.ContainsFunc(scales, func(s coverageScale) bool { return s.value == value }) {
			t.Fatalf("%s names the scale %s twice", coverageScalesEnv, field)
		}
		scales = append(scales, coverageScale{value: value, text: strconv.FormatFloat(value, 'f', -1, 64)})
	}
	return scales
}

// repositoryProse is the English prose the corpus's texts are cut from: every markdown file
// under this repository's docs/ and skills/ (the module root is three directories up from this
// package, the repository's two above that), in the order a directory walk reads them, one row
// (n, line) a line, with no empty line. Real prose is what makes the texts compress and hash
// as Dispatch's do. A line that is no valid UTF-8, or that holds a NUL, which Postgres text
// cannot, is left out.
func repositoryProse(t *testing.T) [][]any {
	t.Helper()
	moduleRoot := filepath.Join("..", "..", "..")
	repository := filepath.Join(moduleRoot, "..", "..")
	var rows [][]any
	for _, dir := range []string{"docs", "skills"} {
		root := filepath.Join(repository, dir)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if path != root && (entry.Name() == "node_modules" || strings.HasPrefix(entry.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(entry.Name(), ".md") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for line := range strings.SplitSeq(string(data), "\n") {
				line = strings.TrimSuffix(line, "\r")
				if strings.TrimSpace(line) == "" || !utf8.ValidString(line) || strings.ContainsRune(line, 0) {
					continue
				}
				rows = append(rows, []any{int64(len(rows) + 1), line})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read the prose under %s: %v", root, err)
		}
	}
	// The corpus script cuts specs of up to 100 KB, each from a start of its own.
	if len(rows) < 10000 {
		t.Fatalf("found %d lines of prose under docs/ and skills/ of %s; the corpus needs at least 10,000", len(rows), repository)
	}
	return rows
}

// serverDatabaseURL is raw, the URL of a server, naming database instead of its own.
func serverDatabaseURL(t *testing.T, raw, database string) string {
	t.Helper()
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", coverageScaleURLEnv, err)
	}
	server.Path = "/" + database
	return server.String()
}

// measureCoverageAtScale builds the corpus at scale in a database of its own, checks it, copies it
// for a server's start when serve (copyForServerStart), builds the window indexes again from their
// migrations (rebuildWindowIndexes), times the fill, and times the recount in each state of
// scaleStates, comparing the counts with main's statements after each (requireMainsAnswerFor),
// logging what it read and printing the scale's coverage-scale line; the database is dropped when
// t ends, and the copy is left.
func measureCoverageAtScale(t *testing.T, admin *pgx.Conn, server string, scale coverageScale, prose [][]any, script string, serve bool) coverageScalePoint {
	ctx := context.Background()
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	name := "coverage_scale_" + strings.ReplaceAll(scale.text, ".", "_") + "_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		t.Fatalf("create the database of scale %s: %v", scale.text, err)
	}
	t.Cleanup(func() { dropScaleDatabase(t, admin, name) })
	// Every session of the database commits synchronously, whatever the server's setting
	// (legion-testpg runs synchronous_commit=off). A vacuum marks a page all-visible only once it
	// can set the committed hint bit of every row on it, and Postgres refuses that bit while the
	// latest asynchronous commit among the row's transaction's group of 32 in the commit log is
	// not yet flushed. The analyze and the record step commit in the load's group, so with
	// asynchronous commits the vacuum after them set no page of embeddings all-visible in two
	// runs of three on a pod's test server, and neither the fresh state nor the stale one would
	// be the state it names.
	if _, err := admin.Exec(ctx, "alter database "+name+" set synchronous_commit = on"); err != nil {
		t.Fatalf("make %s commit synchronously: %v", name, err)
	}
	databaseURL := serverDatabaseURL(t, server, name)
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(database.Pool.Close)
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate %s: %v", name, err)
	}
	// No autovacuum or autoanalyze of embeddings while the test sets its states: one would mark
	// the pages a state left not all-visible all-visible again, or record a share of them the
	// state did not set, and the stale state is the statistics recording a share the table no
	// longer holds.
	mustExec(t, database.Pool, `alter table embeddings set (autovacuum_enabled = false)`)

	started := time.Now()
	loadCoverageCorpus(t, databaseURL, scale, prose, script)
	loaded := time.Since(started)
	// Statistics before the record step, whose statements are planned on them; the fresh state's
	// vacuum is what sets the visibility map.
	mustExec(t, database.Pool, "analyze "+coverageScaleTables)
	started = time.Now()
	recorded := recordOnce(t, database)
	recording := time.Since(started)
	t.Logf("scale %s: corpus loaded in %v, %d issue sources recorded in %v",
		scale.text, loaded.Round(time.Millisecond), recorded, recording.Round(time.Millisecond))
	checkCoverageCorpus(t, database, scale, recorded)
	if serve {
		// Postgres copies no database a session is connected to: the pool closes first, and a
		// pool of its own reads the database from then on.
		database.Pool.Close()
		copyForServerStart(t, admin, server, name)
		if database, err = store.Open(ctx, databaseURL); err != nil {
			t.Fatalf("open %s again once it is copied: %v", name, err)
		}
		t.Cleanup(database.Pool.Close)
	}

	point := coverageScalePoint{scale: scale, builds: rebuildWindowIndexes(t, database, "scale "+scale.text)}
	timer := &statementTimer{}
	timed := &store.Store{Pool: storetest.Traced(t, database.Pool, timer, nil)}
	point.fill = timeCountsRun(t, timer, "scale "+scale.text+": the fill", func() error { return FillCoverage(ctx, timed) })
	t.Logf("scale %s: the fill took %s", scale.text, point.fill)
	for i, state := range scaleStates {
		stage := "scale " + scale.text + ", state " + state.name
		var measured statePoint
		if state.changeEmbedder {
			// The new embedder's pass drops every vector another identity made, over every covered
			// kind, as runIdentitySteps does.
			for _, kind := range coverageKinds {
				step := coverStep{&measured.dropped, dropForeignVectors, []any{kind, state.identity, coverageStatementRows}}
				if err := step.run(ctx, Deps{Store: database}, kind); err != nil {
					t.Fatalf("%s: drop the vectors of %s another identity than %s made: %v", stage, kind, state.identity, err)
				}
			}
			t.Logf("%s: the identity step of %s dropped %d vectors another identity made", stage, state.identity, measured.dropped)
			if measured.dropped == 0 {
				t.Fatalf("%s: the identity step of %s dropped no vector, so the state is no embedder change", stage, state.identity)
			}
		}
		for _, statement := range state.prepare {
			mustExec(t, database.Pool, statement)
		}
		var size int64
		var sizeText string
		if err := database.Pool.QueryRow(ctx, `select relallvisible::bigint, relpages::bigint,
			pg_database_size(current_database()), pg_size_pretty(pg_database_size(current_database()))
			from pg_class where oid = 'embeddings'::regclass`).Scan(&measured.visible, &measured.pages, &size, &sizeText); err != nil {
			t.Fatalf("%s: read embeddings' statistics: %v", stage, err)
		}
		if err := database.Pool.QueryRow(ctx, coverageVisibleShare).Scan(&measured.share); err != nil {
			t.Fatalf("%s: read the share of embeddings the statistics record as all-visible: %v", stage, err)
		}
		t.Logf("%s: the statistics record %d of embeddings' %d pages all-visible, a share of %.3f; database %s (%d bytes)",
			stage, measured.visible, measured.pages, measured.share, sizeText, size)
		if state.lagging && measured.share < laggingVisibleShare {
			t.Fatalf("%s: the statistics record %.3f of embeddings as all-visible with no page all-visible; the state needs at least %g (laggingVisibleShare)",
				stage, measured.share, laggingVisibleShare)
		}

		measured.recount = timeCountsRun(t, timer, stage+": the recount", func() error { return recountCoverage(ctx, timed) })
		t.Logf("%s: the whole recount took %s", stage, measured.recount)
		_, heads := requireMainsAnswerFor(t, database, stage+", after the recount", state.identity)
		// No state changes which items there are: updateOneRowOfPages changes no row's answer, and
		// the embedder change drops vectors, not rows. So the fresh state's report counts every
		// state's items.
		if i == 0 {
			var report []string
			for _, row := range heads {
				if row.indexed {
					point.items += row.items
				}
				report = append(report, row.String())
			}
			slices.Sort(report)
			t.Logf("scale %s: %d items; the report, kind by kind:\n  %s", scale.text, point.items, strings.Join(report, "\n  "))
		}
		point.states = append(point.states, measured)
	}
	recounts := make([]string, len(scaleStates))
	for i, state := range scaleStates {
		recounts[i] = "state " + state.name + " recount " + point.states[i].recount.String()
	}
	fmt.Printf("coverage-scale: scale %s, %d items: fill %s; %s\n", scale.text, point.items, point.fill, strings.Join(recounts, "; "))
	return point
}

// rebuildWindowIndexes drops the two window indexes the corpus script built and builds each again
// by its migration's own statement, read from its file under store/migrations, timing each build
// and reading the index's size: what a deploy of the two migrations waits for before it serves,
// since Dispatch runs one replica under a Recreate strategy and migrates before it serves. Each
// file runs whole, the non-transactional marker and the header with it, which Postgres reads as
// comments, on a connection of database's pool outside any transaction, over the extended
// protocol, as the runner runs a non-transactional migration (pgmigrate's execWatched). The table
// is vacuumed first, which sets the hint bits of the rows the load wrote: without it a build's
// first scan of the table would set them, writing every page of it, which no build over a
// production table long since read and vacuumed does. It answers the builds in
// coverageWindowIndexes' order.
func rebuildWindowIndexes(t *testing.T, database *store.Store, stage string) []windowIndexBuild {
	t.Helper()
	ctx := context.Background()
	mustExec(t, database.Pool, `vacuum embeddings`)
	builds := make([]windowIndexBuild, 0, len(coverageWindowIndexes))
	for _, window := range coverageWindowIndexes {
		migration, err := os.ReadFile(filepath.Join("..", "store", "migrations", window.migration+".up.sql"))
		if err != nil {
			t.Fatalf("%s: read the migration that builds %s: %v", stage, window.index, err)
		}
		mustExec(t, database.Pool, "drop index "+window.index)
		started := time.Now()
		if _, err := database.Pool.Exec(ctx, string(migration), pgx.QueryExecModeExec); err != nil {
			t.Fatalf("%s: build %s by %s: %v", stage, window.index, window.migration, err)
		}
		build := windowIndexBuild{index: window.index, took: time.Since(started)}
		if err := database.Pool.QueryRow(ctx, `select pg_relation_size($1::regclass), pg_size_pretty(pg_relation_size($1::regclass))`,
			window.index).Scan(&build.size, &build.sizeText); err != nil {
			t.Fatalf("%s: read the size of %s: %v", stage, window.index, err)
		}
		t.Logf("%s: %s built by %s in %s, %s (%d bytes)", stage, window.index, window.migration, inMilliseconds(build.took), build.sizeText, build.size)
		builds = append(builds, build)
	}
	return builds
}

// loadCoverageCorpus runs the corpus script at scale on one connection of databaseURL: the prose
// copied into the temporary table the script reads (COPY FROM STDIN), the scale and the two
// identities set, then the script as one simple-protocol statement, which commits once.
func loadCoverageCorpus(t *testing.T, databaseURL string, scale coverageScale, prose [][]any, script string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to load the corpus: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	mustExec(t, conn, `set statement_timeout = 0`)
	mustExec(t, conn, `create temp table coverage_corpus_prose (n bigint not null, line text not null)`)
	if copied, err := conn.CopyFrom(ctx, pgx.Identifier{"coverage_corpus_prose"}, []string{"n", "line"}, pgx.CopyFromRows(prose)); err != nil || copied != int64(len(prose)) {
		t.Fatalf("copy the prose: %d of %d lines, %v", copied, len(prose), err)
	}
	mustExec(t, conn, `select set_config('coverage_corpus.scale', $1, false), set_config('coverage_corpus.identity', $2, false),
		set_config('coverage_corpus.foreign_identity', $3, false)`, scale.text, configuredIdentity, foreignIdentity)
	if _, err := conn.Exec(ctx, script); err != nil {
		t.Fatalf("build the corpus at scale %s: %v", scale.text, err)
	}
}

// loadSmallCoverageCorpus loads the corpus at 0.005 x production's size into database
// (loadCoverageCorpus), about 4,900 window rows and 56 issues, and analyzes the tables it fills
// (coverageScaleTables): the table each test of the coverage statement in a state of embeddings
// builds that state from, on a test server, once it has set what else the state needs.
func loadSmallCoverageCorpus(t *testing.T, database *store.Store) {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("testdata", "coverage_corpus.sql"))
	if err != nil {
		t.Fatalf("read the corpus script: %v", err)
	}
	loadCoverageCorpus(t, database.Pool.Config().ConnString(), coverageScale{value: 0.005, text: "0.005"}, repositoryProse(t), string(script))
	mustExec(t, database.Pool, `analyze `+coverageScaleTables)
}

// checkCoverageCorpus fails t unless the database holds the corpus the scale makes: production's
// counts times the scale, rounded as the script rounds them, of issues, document versions and
// each window kind's rows; every issue's source recorded; and every issue's latest spec its
// sampled size in turn, beginning `## Summary`. It logs the counts.
func checkCoverageCorpus(t *testing.T, database *store.Store, scale coverageScale, recorded int64) {
	t.Helper()
	ctx := context.Background()
	scaled := func(rows int64) int64 {
		var want int64
		if err := database.Pool.QueryRow(ctx, `select round($1::numeric * $2::numeric)::bigint`, rows, scale.text).Scan(&want); err != nil {
			t.Fatalf("scale %d rows: %v", rows, err)
		}
		return want
	}
	count := func(statement string) int64 {
		var got int64
		if err := database.Pool.QueryRow(ctx, statement).Scan(&got); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
		return got
	}
	issues, versions := count(`select count(*) from issues`), count(`select count(*) from artifact_versions`)
	if want := scaled(coverageCorpusIssues); issues != want {
		t.Errorf("the corpus holds %d issues, want %d", issues, want)
	}
	if want := scaled(coverageCorpusVersions); versions != want {
		t.Errorf("the corpus holds %d document versions, want %d", versions, want)
	}
	if recorded != issues {
		t.Errorf("the record step recorded %d issue sources, want one for each of the %d issues, whose rows all hold their current text", recorded, issues)
	}
	var windows []string
	for _, kind := range coverageCorpusWindows {
		rows := count(`select count(*) from embeddings where kind = '` + kind.kind + `'`)
		if want := scaled(kind.rows); rows != want {
			t.Errorf("the corpus holds %d %s rows, want %d", rows, kind.kind, want)
		}
		windows = append(windows, fmt.Sprintf("%s %d", kind.kind, rows))
	}
	outsideASCII := count(`select count(*) from issues where ascii(title) not between 33 and 126`)

	rows, err := database.Pool.Query(ctx, `
		select i.number, coalesce(octet_length(v.markdown), 0), coalesce(starts_with(v.markdown, E'## Summary\n\n'), false)
		from issues i
		join artifacts a on a.issue_key = i.key and a.is_primary
		join lateral (
			select markdown from artifact_versions where artifact_id = a.id order by number desc limit 1
		) v on true
		order by i.number`)
	if err != nil {
		t.Fatalf("read the specs' sizes: %v", err)
	}
	var specs, wrong int64
	for rows.Next() {
		var number, size int64
		var heading bool
		if err := rows.Scan(&number, &size, &heading); err != nil {
			t.Fatalf("scan a spec's size: %v", err)
		}
		specs++
		if want := coverageCorpusSpecSizes[(number-1)%int64(len(coverageCorpusSpecSizes))]; size != want || !heading {
			wrong++
			if wrong <= 5 {
				t.Errorf("issue %d's latest spec is %d bytes (beginning `## Summary`: %t), want %d", number, size, heading, want)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the specs' sizes: %v", err)
	}
	if specs != issues || wrong > 0 {
		t.Errorf("%d of %d issues have a latest spec, %d of them not of its sampled size or not beginning `## Summary`", specs, issues, wrong)
	}
	t.Logf("scale %s: %d issues (%d titled outside ASCII 33 to 126), %d document versions, window rows %s; %d latest specs of the sampled sizes",
		scale.text, issues, outsideASCII, versions, strings.Join(windows, ", "), specs-wrong)
}

// statementTimer times every statement started on the pool it traces (storetest.Traced): the SQL
// each sent and how long it took from its start to its end, in the order they ended.
type statementTimer struct {
	mu    sync.Mutex
	times []timedStatement
}

type timedStatement struct {
	sql  string
	took time.Duration
}

// statementStart is the start of a statement the timer saw begin, carried in its context to the
// end of the statement.
type statementStart struct {
	sql string
	at  time.Time
}

type statementStartKey struct{}

func (s *statementTimer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, statementStartKey{}, statementStart{sql: data.SQL, at: time.Now()})
}

func (s *statementTimer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	start, ok := ctx.Value(statementStartKey{}).(statementStart)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.times = append(s.times, timedStatement{sql: start.sql, took: time.Since(start.at)})
}

// take answers the statements timed since the last take, and forgets them.
func (s *statementTimer) take() []timedStatement {
	s.mu.Lock()
	defer s.mu.Unlock()
	taken := s.times
	s.times = nil
	return taken
}

// countsGroupName is a group of countsGroups as the coverage-scale lines name it: "windows" for
// the window kinds' group, and otherwise its one kind.
func countsGroupName(group countsGroup) string {
	if group.planned {
		return "windows"
	}
	return group.kinds[0]
}

// timeCountsRun runs run, one fill or one recount on a pool timer traces, failing the test with
// what when it fails, and answers its whole time and each of its statements' (groupTime): every
// statement of the refresh of the issues' state rows (staleIssueKeys, goneIssueKeys,
// refreshIssues), summed as "refresh", then each group's statement (recountStatement over the
// group's classes) in the order sent, which is countsGroups'.
func timeCountsRun(t *testing.T, timer *statementTimer, what string, run func() error) countsRun {
	t.Helper()
	timer.take()
	started := time.Now()
	if err := run(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	measured := countsRun{whole: time.Since(started)}
	names := map[string]string{staleIssueKeys: "refresh", goneIssueKeys: "refresh", refreshIssues: "refresh"}
	for _, group := range countsGroups {
		names[group.statement] = countsGroupName(group)
	}
	refresh := groupTime{group: "refresh"}
	var groups []groupTime
	for _, statement := range timer.take() {
		switch name, ok := names[statement.sql]; {
		case !ok:
		case name == "refresh":
			refresh.took += statement.took
		default:
			groups = append(groups, groupTime{group: name, took: statement.took})
		}
	}
	if len(groups) != len(countsGroups) {
		t.Fatalf("%s: the tracer timed %d group statements, want one for each of the %d groups: %v", what, len(groups), len(countsGroups), groups)
	}
	measured.groups = append([]groupTime{refresh}, groups...)
	return measured
}

func inMilliseconds(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64) + " ms"
}

// dropScaleDatabase drops name as storetest drops a test's database: plainly, again while
// Postgres answers that it is in use (SQLSTATE 55006, a backend of the closed pools still being
// torn down), and with force as the last attempt.
func dropScaleDatabase(t *testing.T, admin *pgx.Conn, name string) {
	ctx := context.Background()
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := admin.Exec(ctx, "drop database "+name)
		if err == nil {
			return
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55006" || time.Now().After(deadline) {
			if _, forced := admin.Exec(ctx, "drop database "+name+" with (force)"); forced != nil {
				t.Errorf("drop %s: %v, after the plain drop answered: %v", name, forced, err)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// copyForServerStart copies name, a scale's database that no session is connected to, to
// name_serve (create database ... template ... strategy file_copy), again while Postgres answers
// that name is in use (SQLSTATE 55006, a backend of the closed pool still being torn down), and
// prints the copy's name on a line that starts `coverage-scale-database:`, which
// scripts/coverage-scale.sh reads to start the server on it and drop it. It is the database a
// server's first start meets at the first deploy of the counts: the corpus as loaded with its
// tables' triggers off, analyzed and its issues' sources recorded, and no row of
// search_coverage_filled, search_coverage_counts, search_coverage_deltas or
// search_coverage_issues, so done_at is unset. On one connection of its own, closed before it
// answers, it then makes the copy what production is beside that: autovacuum on embeddings as the
// migrations leave it, which the scale test turned off on the database it copied; every session
// committing synchronously, Postgres's default, as the copied database's did, a setting no copy
// carries; the boot's document_search pass (store.FillDocumentSearch) recorded as done, as every
// database a server has booted on since the release that brought document_search records it,
// since the load writes the document versions with their triggers off, so none holds the search
// vector its trigger writes, and the pass, which the boot runs before it serves, would refuse the
// first one and end the boot; and the corpus's tables vacuumed and analyzed, so the server's fill
// reads the tables with their hint bits and visibility map set, as the scale test's fill reads them
// after rebuildWindowIndexes' vacuum and as a production table long since read and vacuumed is,
// rather than writing every page it reads.
func copyForServerStart(t *testing.T, admin *pgx.Conn, server, name string) {
	t.Helper()
	ctx := context.Background()
	copied := name + "_serve"
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, err := admin.Exec(ctx, "create database "+copied+" template "+name+" strategy file_copy")
		if err == nil {
			break
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55006" || time.Now().After(deadline) {
			t.Fatalf("copy %s to %s for the server's start: %v", name, copied, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("coverage-scale-database: %s\n", copied)
	if _, err := admin.Exec(ctx, "alter database "+copied+" set synchronous_commit = on"); err != nil {
		t.Fatalf("make %s commit synchronously: %v", copied, err)
	}
	conn, err := pgx.Connect(ctx, serverDatabaseURL(t, server, copied))
	if err != nil {
		t.Fatalf("connect to %s: %v", copied, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	started := time.Now()
	mustExec(t, conn, `set statement_timeout = 0`)
	mustExec(t, conn, `alter table embeddings reset (autovacuum_enabled)`)
	mustExec(t, conn, `update document_search_backfill set done = true`)
	mustExec(t, conn, "vacuum (analyze) "+coverageScaleTables)
	t.Logf("%s copied to %s, which is vacuumed and analyzed in %v and left for the server's start", name, copied, time.Since(started).Round(time.Millisecond))
}

// judgeCoverageScale judges the fill and the recount at S, the largest scale measured, against the
// bounds the counts loop holds them to, fillBound (coverageFillTimeout) and recountBound
// (coverageRecountTimeout): it fails when four times the fill at S passes fillBound, or four times
// the longest whole recount at S, of every state's, passes recountBound. It fits the whole
// recount's time in each state of scaleStates to the two smallest scales' points, logging the item
// count at which it would reach recountBound by a linear fit and by a power law t = a × s^k
// (k = ln(t2/t1) / ln(s2/s1)), and the smaller of the two as a multiple of the 1 × corpus's items;
// the slowest state is the one whose multiple is the smallest, and a state whose recount does not
// grow between the two scales has no fit. Its last line starts `coverage-scale:` and is printed
// whatever the verdict, on standard output rather than through t.Log, so the line begins with it
// in the log. It names S, its items, the fill at S and four times it against fillBound, and the
// longest whole recount at S, its state and four times it against recountBound; each window index's
// build time and size at every scale; for each state the vectors its embedder change dropped at the
// two smallest scales, where it changes the embedder, the share of embeddings' pages its statistics
// record as all-visible there, the whole recount there and the fits and their multiple; then the
// slowest state with the corpus size, in items, at which its whole recount would reach
// recountBound and that size's multiple, or the states where no fit is defined.
func judgeCoverageScale(t *testing.T, scales []coverageScale, points []coverageScalePoint, fillBound, recountBound time.Duration) {
	t.Helper()
	if len(points) < 2 {
		fmt.Printf("coverage-scale: FAIL: %d of the %d scales were measured; the fit needs two\n", len(points), len(scales))
		t.Fatalf("%d of the %d scales were measured; the fit needs two, the failures are above", len(points), len(scales))
	}
	slices.SortFunc(points, func(a, b coverageScalePoint) int { return cmp.Compare(a.scale.value, b.scale.value) })
	first, second, largest := points[0], points[1], points[len(points)-1]
	s1, s2 := float64(first.items), float64(second.items)
	line := float64(recountBound)

	oneX := s1 / first.scale.value
	oneXFrom := fmt.Sprintf("scale %s's %d items divided by %s", first.scale.text, first.items, first.scale.text)
	for _, point := range points {
		if point.scale.value == 1 {
			oneX, oneXFrom = float64(point.items), "scale 1"
		}
	}

	var failures []string
	fill := largest.fill.whole
	if 4*fill > fillBound {
		failures = append(failures, fmt.Sprintf("four times the fill at scale %s, %s, passes coverageFillTimeout, %v", largest.scale.text, inMilliseconds(4*fill), fillBound))
	}
	longest, longestState := time.Duration(0), ""
	for i, state := range scaleStates {
		if recount := largest.states[i].recount.whole; recount > longest {
			longest, longestState = recount, state.name
		}
	}
	if 4*longest > recountBound {
		failures = append(failures, fmt.Sprintf("four times the longest whole recount at scale %s, %s in state %s, passes coverageRecountTimeout, %v",
			largest.scale.text, inMilliseconds(4*longest), longestState, recountBound))
	}

	builds := make([]string, 0, len(coverageWindowIndexes))
	for i, window := range coverageWindowIndexes {
		atScales := make([]string, len(points))
		for j, point := range points {
			atScales[j] = fmt.Sprintf("%s at scale %s (%s)", inMilliseconds(point.builds[i].took), point.scale.text, point.builds[i].sizeText)
		}
		builds = append(builds, window.index+" built in "+strings.Join(atScales, ", "))
	}
	var states, unfitStates []string
	slowest, slowestMultiple := "", math.Inf(1)
	for i, state := range scaleStates {
		a, b := first.states[i], second.states[i]
		t.Logf("state %s: scale %s (%d items, %d of %d pages all-visible) recount %s; scale %s (%d items, %d of %d pages all-visible) recount %s",
			state.name, first.scale.text, first.items, a.visible, a.pages, a.recount, second.scale.text, second.items, b.visible, b.pages, b.recount)
		summary := state.name + ": "
		if state.changeEmbedder {
			summary += fmt.Sprintf("%d and %d vectors dropped for %s, ", a.dropped, b.dropped, state.identity)
		}
		summary += fmt.Sprintf("statistics recording %.3f and %.3f of the pages all-visible, whole recount %s and %s",
			a.share, b.share, inMilliseconds(a.recount.whole), inMilliseconds(b.recount.whole))
		t1, t2 := float64(a.recount.whole), float64(b.recount.whole)
		if !(s2 > s1) || !(t2 > t1) {
			states = append(states, summary+", no fit: the whole recount does not grow")
			unfitStates = append(unfitStates, state.name)
			t.Logf("state %s: the whole recount does not grow from scale %s to scale %s (%s to %s over %d to %d items), so neither fit is defined",
				state.name, first.scale.text, second.scale.text, inMilliseconds(a.recount.whole), inMilliseconds(b.recount.whole), first.items, second.items)
			continue
		}
		linear := s1 + (line-t1)*(s2-s1)/(t2-t1)
		k := math.Log(t2/t1) / math.Log(s2/s1)
		power := s1 * math.Pow(line/t1, 1/k)
		smaller := math.Min(linear, power)
		multiple := smaller / oneX
		t.Logf("state %s: the whole recount reaches %v at %.0f items by the linear fit and at %.0f items by the power law (k = %.3f); "+
			"the smaller, %.0f items, is %.2f × the 1 × corpus's %.0f items (%s)", state.name, recountBound, linear, power, k, smaller, multiple, oneX, oneXFrom)
		states = append(states, fmt.Sprintf("%s, the whole recount reaches %v at %.0f items (linear) and %.0f items (power law, k = %.3f), %.2f x",
			summary, recountBound, linear, power, k, multiple))
		if multiple < slowestMultiple {
			slowest, slowestMultiple = state.name, multiple
		}
	}
	var fitted string
	switch {
	case slowest == "":
		fitted = "no fit is defined in any state"
	case len(unfitStates) > 0:
		fitted = fmt.Sprintf("no fit is defined in %s; of the others, %s is the slowest, its whole recount reaching %v at %.0f items, %.2f x the 1 x corpus's %.0f items",
			strings.Join(unfitStates, " and "), slowest, recountBound, slowestMultiple*oneX, slowestMultiple, oneX)
	default:
		fitted = fmt.Sprintf("the slowest state, %s, its whole recount reaching %v at %.0f items, %.2f x the 1 x corpus's %.0f items",
			slowest, recountBound, slowestMultiple*oneX, slowestMultiple, oneX)
	}
	verdict := "pass"
	if len(failures) > 0 {
		verdict = "FAIL"
	}
	fmt.Printf("coverage-scale: %s: scales %s, S %s with %d items; fill at S %s, 4 x %s against coverageFillTimeout %v; "+
		"longest whole recount at S %s in state %s, 4 x %s against coverageRecountTimeout %v; %s; %s; %s\n",
		verdict, strings.Join(scaleTexts(points), " and "), largest.scale.text, largest.items, inMilliseconds(fill), inMilliseconds(4*fill), fillBound,
		inMilliseconds(longest), longestState, inMilliseconds(4*longest), recountBound, strings.Join(builds, "; "), strings.Join(states, "; "), fitted)
	for _, failure := range failures {
		t.Error(failure)
	}
}

// scaleTexts is each point's scale as the corpus script reads it, in the points' order.
func scaleTexts(points []coverageScalePoint) []string {
	texts := make([]string, len(points))
	for i, point := range points {
		texts[i] = point.scale.text
	}
	return texts
}
