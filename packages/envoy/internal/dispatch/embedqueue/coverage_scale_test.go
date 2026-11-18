package embedqueue

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// coverageScaleURLEnv names the Postgres server TestCoverageStatementScale builds its corpora on,
// one database per scale, which it creates and drops: never a server that holds anything else of
// value, and never CI's, which leaves it unset. Its database path is ignored, as storetest's
// DISPATCH_TEST_DATABASE_URL's is. coverageScalesEnv lists the scales, space-separated; 1 is
// production's counts of 2026-11-17.
const (
	coverageScaleURLEnv   = "DISPATCH_COVERAGE_SCALE_URL"
	coverageScalesEnv     = "DISPATCH_COVERAGE_SCALES"
	coverageScalesDefault = "1 2"
)

const (
	// coverageScaleRuns is how many times each statement is timed at a scale, after one warm-up
	// run of each, so the median is the middle reading of five.
	coverageScaleRuns = 5
	// coverageScaleMinMultiple is R2's bar (dispatch://INFRA-4241): the corpus at which the
	// statement reaches its bound must be at least this many times production's of 2026-11-17.
	coverageScaleMinMultiple = 4.0
)

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

// scaleState is one state of embeddings the statements are measured in, as R2 of
// dispatch://INFRA-4241 names them: its name; identity, the configured embedder's identity in the
// state, the one the statements are compared and timed for; changeEmbedder, whether the state
// begins with the configured embedder changing to identity, whose pass's identity step drops every
// vector another identity made, so that the state is not made unless that step drops a vector;
// prepare, the statements that then put the table in the state from the one measured before, each
// run on its own, since VACUUM runs in no transaction block; and lagging, whether the state's
// statistics record more of the table as all-visible than is, so that the state is not made
// unless they record at least coverageIndexOnlyShare.
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
// is off on embeddings, so nothing else sets its visibility map or statistics.
//
//   - fresh: every table vacuumed and analyzed, as a quiet production table is, so nearly every
//     page of embeddings is all-visible.
//   - third: one row of every third heap page of embeddings updated, then the table analyzed, so
//     about a third of its pages are not all-visible and its statistics say so, as an ANALYZE
//     records them between two vacuums of a table the queue keeps writing. INFRA-4241's tester
//     found b3756b91's statement, which left the planner free, reading the unembedded half of the
//     windows by a Seq Scan of embeddings once a third or more of the pages were not all-visible.
//   - all: one row of every page updated the same way, then the table analyzed, so no page is
//     all-visible and its statistics say so.
//   - stale: embeddings vacuumed, so every page is all-visible again; one row of every even page
//     updated, then the table analyzed, so its statistics record about half the pages
//     all-visible; then one row of every odd page updated with no ANALYZE after it, so no page is
//     all-visible while the statistics still record about half. Autoanalyze waits for a tenth of
//     the rows to change, and one row of each of half the pages is fewer (97,369 rows against
//     99,566 at production's size), so a production table can stand so until its next ANALYZE.
//     It is the state 2f9a6fc1's planCoverage misjudged (coverageIndexOnlyShare).
//   - embedder: the configured embedder changes to changedEmbedderIdentity, and its pass's
//     identity step drops every vector another identity made, over every covered kind
//     (dropForeignVectors over coverageKinds, as runIdentitySteps runs it), so every window row
//     leaves the embedded half for the other until the queue embeds it again, which takes hours at
//     production's size; with no ANALYZE after it, so the statistics are the stale state's.
//     INFRA-4241's tester found 0ec6b4ad's statement reading the windows in this state through
//     embeddings_kind_project, with the blank test's regex as the heap scan's filter.
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

// coverageIndexOnlyShare is the share of embeddings' pages recorded as all-visible
// (coverageVisibleShare) at and above which 2f9a6fc1's planCoverage chose to read each window
// half by an index-only scan of its own index, a rule this change dropped (planCoverage says
// why). TestCoverageFetchesNoWindowRowByRowWhileTheStatisticsLagTheVisibilityMap builds
// statistics that record at least this share while no page is all-visible, and the scale test's
// stale state is not made unless its statistics record at least this share, so the state each
// makes is the one that rule misjudged.
const coverageIndexOnlyShare = 0.3

// coverageWindowIndexes are the two window indexes the coverage statement reads, each with the
// migration that builds it, whose file rebuildWindowIndexes builds it again from.
var coverageWindowIndexes = []struct{ index, migration string }{
	{"embeddings_window_embedded", "20261118011208_embeddings_window_embedded"},
	{"embeddings_window_unembedded", "20261118011258_embeddings_window_unembedded"},
}

// windowIndexBuild is what one build of a window index measured: the index, how long its
// migration's statement took, and the index's size once built.
type windowIndexBuild struct {
	index    string
	took     time.Duration
	size     int64
	sizeText string
}

// statePoint is what one state measured at one scale: how many vectors its embedder change
// dropped (none in a state that changes no embedder); how many of embeddings' pages its
// statistics record as all-visible, of how many, and that share (coverageVisibleShare); each
// statement's median planning plus execution time; and the negative control's time and whether
// it read embeddings by a Seq Scan.
type statePoint struct {
	dropped        int64
	visible, pages int64
	share          float64
	head, base     time.Duration
	control        time.Duration
	controlSeqScan bool
}

// coverageScalePoint is what one scale measured: the corpus's items (every covered kind's, as
// the head's report counts them), the builds of the window indexes, in coverageWindowIndexes'
// order, and each state's point, in scaleStates' order.
type coverageScalePoint struct {
	scale  coverageScale
	items  int64
	builds []windowIndexBuild
	states []statePoint
}

// TestCoverageStatementScale measures the search coverage statement as the route runs it (the
// head: coverageQuery under the settings planCoverage sets) against the statement it replaced
// (mainCoverageQuery, the base) on a synthetic corpus at each scale DISPATCH_COVERAGE_SCALES
// names, in each of six states of embeddings (scaleStates): four of its visibility map and
// statistics, and two after an embedder change has dropped every vector. It states the corpus
// size at which the head would reach its bound (coverageStatementTimeout) in the slowest of them,
// as a multiple of production's: R2 of dispatch://INFRA-4241, which fails below 4. It also times
// the builds of the two window indexes (rebuildWindowIndexes), which a deploy of their migrations
// waits for before it serves. scripts/coverage-scale.sh runs it in a Job of its own, with a
// Postgres of its own; a pod's test server is too small and too shared for corpora of
// production's size, and CI never sets the URL that runs it.
//
// Each scale gets a database of its own, migrated by the store's own path (store.Migrate), with
// autovacuum off on embeddings, so that nothing but the test sets its visibility map or its
// statistics; the corpus loaded (testdata/coverage_corpus.sql over this repository's prose); the
// record step run to completion (recordSources) as the boot pass leaves a database; and the two
// window indexes dropped and built again by their migrations' own statements, each build timed.
// The statements are then measured in each state in turn: freshly vacuumed; about a third of
// embeddings' pages not all-visible after an ANALYZE; every page not all-visible after an
// ANALYZE; every page not all-visible while the statistics still record about half of them
// all-visible from an earlier ANALYZE; and, once the configured embedder has changed and its
// identity step has dropped every vector the corpus held, with the statistics of the state before
// and then after an ANALYZE. In each state the base's answer is compared with the head's in one
// snapshot for the state's configured identity (requireMainsAnswerFor), so the time read is the
// time of the same answer, and a negative control runs the head's text once with none of
// planCoverage's settings, the planner free, and logs whether it read embeddings by a Seq Scan.
// The database is dropped before the next scale is built.
//
// The times are read on one connection, each statement once before the timed runs and then
// alternately five times under EXPLAIN (ANALYZE, BUFFERS), each run's time its planning plus
// execution, for the state's configured identity. The base runs on the connection, under no
// setting. The head runs as Coverage runs it, in a read-only transaction planCoverage plans
// (runHead), but with statement_timeout 0 where Coverage sets its bound, so neither statement is
// cut off by a bound. Every run re-plans (QueryExecModeDescribeExec, as benchtime's runner does):
// a cached prepared statement switches to a generic plan from its sixth execution, which is not
// the plan a call gets.
//
// In each state the size at which the head reaches the bound is taken from the two smallest
// scales' points, fitted both linearly and as a power law t = a × s^k, since the base's time grew
// faster than its corpus; the smaller of the two is that state's. The slowest of the six states
// is the one whose size is the smallest, and its size is the one judged. The last line it prints
// starts `coverage-scale:` and names every number the verdict rests on, with the index builds'
// times and sizes, so a reader of the Job's log judges it from its end.
func TestCoverageStatementScale(t *testing.T) {
	server := os.Getenv(coverageScaleURLEnv)
	if server == "" {
		t.Skip(coverageScaleURLEnv + " is unset: it must name a Postgres server this test may create and drop " +
			"databases on, which packages/envoy/scripts/coverage-scale.sh starts and names; CI never sets it")
	}
	bound, err := time.ParseDuration(coverageStatementTimeout)
	if err != nil {
		t.Fatalf("read the coverage statement's bound %q as a duration: %v", coverageStatementTimeout, err)
	}
	scales := coverageScales(t)
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
			points = append(points, measureCoverageAtScale(t, admin, server, scale, prose, string(script)))
		})
	}
	judgeCoverageScale(t, scales, points, bound)
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

// measureCoverageAtScale builds the corpus at scale in a database of its own, checks it, builds
// the window indexes again from their migrations (rebuildWindowIndexes), and measures both
// statements on it in each state of scaleStates, logging what it read; the database is dropped
// when t ends.
func measureCoverageAtScale(t *testing.T, admin *pgx.Conn, server string, scale coverageScale, prose [][]any, script string) coverageScalePoint {
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

	point := coverageScalePoint{scale: scale, builds: rebuildWindowIndexes(t, database, "scale "+scale.text)}
	conn := measurementConn(t, databaseURL)
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
		if state.lagging && measured.share < coverageIndexOnlyShare {
			t.Fatalf("%s: the statistics record %.3f of embeddings as all-visible with no page all-visible; the state needs at least %g, the share 2f9a6fc1's planCoverage read the index-only plan from",
				stage, measured.share, coverageIndexOnlyShare)
		}

		_, heads := requireMainsAnswerFor(t, database, stage, state.identity)
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
			t.Logf("scale %s: %d items; the head's report, kind by kind:\n  %s", scale.text, point.items, strings.Join(report, "\n  "))
		}

		head, base := timeCoverageStatements(t, conn, stage, state.identity)
		measured.head, measured.base = medianRun(head), medianRun(base)
		t.Logf("%s: head median %s (runs %s); base median %s (runs %s)",
			stage, inMilliseconds(measured.head), runsInMilliseconds(head), inMilliseconds(measured.base), runsInMilliseconds(base))
		measured.control, measured.controlSeqScan = coverageNegativeControl(t, conn, stage, state.identity)
		t.Logf("%s: negative control, coverageQuery with none of planCoverage's settings, the planner free: %s, embeddings read by a Seq Scan: %t",
			stage, inMilliseconds(measured.control), measured.controlSeqScan)
		point.states = append(point.states, measured)
	}
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

// measurementConn opens the one connection of databaseURL the statements are timed on, which t's
// end closes. Its statement_timeout is 0, so no run is cut off by a bound, and every statement on
// it is planned again at each run (QueryExecModeDescribeExec, as benchtime's runner does): a
// cached prepared statement switches to a generic plan from its sixth execution, which is not the
// plan a call gets.
func measurementConn(t *testing.T, databaseURL string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse the measurement database's URL: %v", err)
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to measure: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	mustExec(t, conn, `set statement_timeout = 0`)
	return conn
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

// scaleQuerier is what the measurement runs a statement on: its connection, or the head's
// transaction on it.
type scaleQuerier interface {
	querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// timeCoverageStatements times the base (mainCoverageQuery, on conn itself under no setting) and
// the head (coverageQuery as planCoverage plans it, as runHead runs it) for identity, the state's
// configured one, on conn: one warm-up run of each, then coverageScaleRuns runs of each,
// alternately, under EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON), each run's time its planning plus
// execution. It logs one plan of each in text form, from an EXPLAIN (ANALYZE, BUFFERS) of its own
// after the timed runs.
func timeCoverageStatements(t *testing.T, conn *pgx.Conn, stage, identity string) (head, base []time.Duration) {
	t.Helper()
	const baseName, headName = "base (mainCoverageQuery)", "head (coverageQuery, planned by planCoverage)"
	warm := func(q scaleQuerier, statement string) { mustExec(t, q, statement, identity) }
	warm(conn, mainCoverageQuery)
	runHead(t, conn, warm)
	for range coverageScaleRuns {
		took, _ := explainedRun(t, conn, stage+": "+baseName, mainCoverageQuery, identity)
		base = append(base, took)
		runHead(t, conn, func(q scaleQuerier, statement string) {
			took, _ := explainedRun(t, q, stage+": "+headName, statement, identity)
			head = append(head, took)
		})
	}
	logTextPlan(t, conn, stage, baseName, mainCoverageQuery, identity)
	runHead(t, conn, func(q scaleQuerier, statement string) { logTextPlan(t, q, stage, headName, statement, identity) })
	return head, base
}

// runHead runs the head on conn as Coverage runs it: in a read-only transaction,
// statement_timeout set local to 0 where Coverage sets its bound (coverageStatementTimeout), so
// no run is cut off by it, then planCoverage, which runs its settings in the transaction and
// answers the plan. do then runs the plan's query in the transaction, which is rolled back,
// undoing the settings.
func runHead(t *testing.T, conn *pgx.Conn, do func(q scaleQuerier, statement string)) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin the head's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	mustExec(t, tx, `set local statement_timeout = 0`)
	plan, err := planCoverage(ctx, tx)
	if err != nil {
		t.Fatalf("plan the head as Coverage does: %v", err)
	}
	do(tx, plan.query)
}

// logTextPlan logs statement's plan for identity on q in text form, from an EXPLAIN (ANALYZE,
// BUFFERS) of its own.
func logTextPlan(t *testing.T, q scaleQuerier, stage, name, statement, identity string) {
	t.Helper()
	rows, err := q.Query(context.Background(), "explain (analyze, buffers) "+statement, identity)
	if err != nil {
		t.Fatalf("%s: explain the %s: %v", stage, name, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("%s: read the plan of the %s: %v", stage, name, err)
	}
	t.Logf("%s: the %s's plan:\n%s", stage, name, strings.Join(lines, "\n"))
}

// coverageNegativeControl runs coverageQuery once on conn for identity under EXPLAIN (ANALYZE,
// BUFFERS, FORMAT JSON) with none of planCoverage's settings, the planner free to read embeddings
// as its estimates price it. It answers the run's planning plus execution time and whether the
// plan read embeddings by a Seq Scan, which runs the blank test's regex over every window's text
// again: the plan planCoverage's settings keep the statement from.
func coverageNegativeControl(t *testing.T, conn *pgx.Conn, stage, identity string) (time.Duration, bool) {
	t.Helper()
	took, plan := explainedRun(t, conn, stage+": negative control (coverageQuery, the planner free)", coverageQuery, identity)
	return took, explaintest.SeqScansRelation(t, plan, "embeddings")
}

// explainedRun is one run of statement for identity ($1) on q under EXPLAIN (ANALYZE, BUFFERS,
// FORMAT JSON): its planning plus execution time as the run reports them, and its plan's one
// "Plan" node. A plan missing either time is a failure rather than a zero, which would read as the
// fastest run there is.
func explainedRun(t *testing.T, q querier, name, statement, identity string) (time.Duration, json.RawMessage) {
	t.Helper()
	var raw []byte
	if err := q.QueryRow(context.Background(), "explain (analyze, buffers, format json) "+statement, identity).Scan(&raw); err != nil {
		t.Fatalf("explain the %s: %v", name, err)
	}
	var explained []struct {
		Plan      json.RawMessage `json:"Plan"`
		Planning  *float64        `json:"Planning Time"`
		Execution *float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &explained); err != nil || len(explained) != 1 || explained[0].Planning == nil || explained[0].Execution == nil {
		t.Fatalf("read the %s's planning and execution times (%v): %s", name, err, bytes.TrimSpace(raw))
	}
	return time.Duration((*explained[0].Planning + *explained[0].Execution) * float64(time.Millisecond)), explained[0].Plan
}

// medianRun is the middle of an odd number of run times.
func medianRun(times []time.Duration) time.Duration {
	sorted := slices.Clone(times)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

func inMilliseconds(d time.Duration) string {
	return strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64) + " ms"
}

func runsInMilliseconds(times []time.Duration) string {
	texts := make([]string, len(times))
	for i, d := range times {
		texts[i] = strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64)
	}
	return strings.Join(texts, " ") + " ms"
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

// judgeCoverageScale fits the head's time in each state of scaleStates, all six, to the two
// smallest scales' points. For each state it logs the four medians, the item count at which the
// head would reach bound by a linear fit and by a power law t = a × s^k
// (k = ln(t2/t1) / ln(s2/s1)), and the smaller of the two as a multiple of the 1 × corpus's items;
// the slowest state is the one whose multiple is the smallest. It fails when that multiple is
// below coverageScaleMinMultiple, and when the head's time in a state does not grow between the
// two scales, where neither fit is defined. Its last line starts `coverage-scale:` and is printed
// whatever the verdict, on standard output rather than through t.Log, so the line begins with it
// in the log. It names each window index's build time and size at both scales; for each state the
// vectors its embedder change dropped at both scales, where it changes the embedder, the share of
// embeddings' pages its statistics record as all-visible at both scales, the head's and the
// base's medians, the negative control's times and whether it read embeddings by a Seq Scan, and
// the fits and their multiple; then the slowest state and its multiple, or, where a state has no
// fit, that the slowest state is not known, and the slowest of the others.
func judgeCoverageScale(t *testing.T, scales []coverageScale, points []coverageScalePoint, bound time.Duration) {
	t.Helper()
	if len(points) < 2 {
		fmt.Printf("coverage-scale: FAIL: %d of the %d scales were measured; the fit needs two\n", len(points), len(scales))
		t.Fatalf("%d of the %d scales were measured; the fit needs two, the failures are above", len(points), len(scales))
	}
	slices.SortFunc(points, func(a, b coverageScalePoint) int { return cmp.Compare(a.scale.value, b.scale.value) })
	first, second := points[0], points[1]
	s1, s2 := float64(first.items), float64(second.items)
	line := float64(bound)

	oneX := s1 / first.scale.value
	oneXFrom := fmt.Sprintf("scale %s's %d items divided by %s", first.scale.text, first.items, first.scale.text)
	for _, point := range points {
		if point.scale.value == 1 {
			oneX, oneXFrom = float64(point.items), "scale 1"
		}
	}
	builds := make([]string, 0, len(coverageWindowIndexes))
	for i, window := range coverageWindowIndexes {
		a, b := first.builds[i], second.builds[i]
		builds = append(builds, fmt.Sprintf("%s built in %s and %s, %s and %s",
			window.index, inMilliseconds(a.took), inMilliseconds(b.took), a.sizeText, b.sizeText))
	}
	var states, unfit, unfitStates []string
	slowest, slowestMultiple := "", math.Inf(1)
	for i, state := range scaleStates {
		a, b := first.states[i], second.states[i]
		t.Logf("state %s: scale %s (%d items, %d of %d pages all-visible) head %s, base %s; scale %s (%d items, %d of %d pages all-visible) head %s, base %s",
			state.name, first.scale.text, first.items, a.visible, a.pages, inMilliseconds(a.head), inMilliseconds(a.base),
			second.scale.text, second.items, b.visible, b.pages, inMilliseconds(b.head), inMilliseconds(b.base))
		summary := state.name + ": "
		if state.changeEmbedder {
			summary += fmt.Sprintf("%d and %d vectors dropped for %s, ", a.dropped, b.dropped, state.identity)
		}
		summary += fmt.Sprintf("statistics recording %.3f and %.3f of the pages all-visible, head median %s and %s, base median %s and %s, "+
			"negative control (the planner free) %s and %s, Seq Scan on embeddings %t and %t",
			a.share, b.share, inMilliseconds(a.head), inMilliseconds(b.head), inMilliseconds(a.base), inMilliseconds(b.base),
			inMilliseconds(a.control), inMilliseconds(b.control), a.controlSeqScan, b.controlSeqScan)
		t1, t2 := float64(a.head), float64(b.head)
		if !(s2 > s1) || !(t2 > t1) {
			states = append(states, summary+", no fit: the head's median does not grow")
			unfitStates = append(unfitStates, state.name)
			unfit = append(unfit, fmt.Sprintf("state %s: the head's median does not grow from scale %s to scale %s (%s to %s over %d to %d items), so neither fit is defined",
				state.name, first.scale.text, second.scale.text, inMilliseconds(a.head), inMilliseconds(b.head), first.items, second.items))
			continue
		}
		linear := s1 + (line-t1)*(s2-s1)/(t2-t1)
		k := math.Log(t2/t1) / math.Log(s2/s1)
		power := s1 * math.Pow(line/t1, 1/k)
		smaller := math.Min(linear, power)
		multiple := smaller / oneX
		t.Logf("state %s: the head reaches %v at %.0f items by the linear fit and at %.0f items by the power law (k = %.3f); "+
			"the smaller, %.0f items, is %.2f × the 1 × corpus's %.0f items (%s)", state.name, bound, linear, power, k, smaller, multiple, oneX, oneXFrom)
		states = append(states, fmt.Sprintf("%s, the head reaches %v at %.0f items (linear) and %.0f items (power law, k = %.3f), %.2f x",
			summary, bound, linear, power, k, multiple))
		if multiple < slowestMultiple {
			slowest, slowestMultiple = state.name, multiple
		}
	}
	verdict := "pass"
	if len(unfit) > 0 || slowestMultiple < coverageScaleMinMultiple {
		verdict = "FAIL"
	}
	var judged string
	switch {
	case slowest == "":
		judged = "no fit is defined in any state, so the slowest state is not known"
	case len(unfitStates) > 0:
		judged = fmt.Sprintf("no fit is defined in %s, so the slowest state is not known; of the others, %s is the slowest, reaching %v at %.2f x the 1 x corpus's %.0f items, at least %g wanted",
			strings.Join(unfitStates, " and "), slowest, bound, slowestMultiple, oneX, coverageScaleMinMultiple)
	default:
		judged = fmt.Sprintf("the slowest state, %s, reaches %v at %.2f x the 1 x corpus's %.0f items, at least %g wanted",
			slowest, bound, slowestMultiple, oneX, coverageScaleMinMultiple)
	}
	fmt.Printf("coverage-scale: %s: scales %s and %s, items %d and %d; %s; %s; %s\n",
		verdict, first.scale.text, second.scale.text, first.items, second.items, strings.Join(builds, "; "), strings.Join(states, "; "), judged)
	for _, failure := range unfit {
		t.Errorf("%s: measure at larger scales, where the head's time stands above the noise", failure)
	}
	if slowestMultiple < coverageScaleMinMultiple {
		t.Errorf("the head would reach %v at %.2f × production's corpus in state %s, the slowest of the states with a fit, below the %g × R2 asks for",
			bound, slowestMultiple, slowest, coverageScaleMinMultiple)
	}
}
