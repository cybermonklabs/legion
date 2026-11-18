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

// coverageScalePoint is what one scale measured: the corpus's items (every covered kind's, as
// the head's report counts them) and the median planning plus execution time of each statement.
type coverageScalePoint struct {
	scale      coverageScale
	items      int64
	head, base time.Duration
}

// TestCoverageStatementScale measures the search coverage statement (coverageIndexOnlyQuery, the
// head) against the statement it replaced (mainCoverageQuery, the base) on a synthetic corpus at
// each scale DISPATCH_COVERAGE_SCALES names, and states the corpus size at which the head would
// reach its bound (coverageStatementTimeout), as a multiple of production's: R2 of
// dispatch://INFRA-4241, which fails below 4. scripts/coverage-scale.sh runs it in a Job of its
// own, with a Postgres of its own; a pod's test server is too small and too shared for corpora of
// production's size, and CI never sets the URL that runs it.
//
// Each scale gets a database of its own, migrated by the store's own path (store.Migrate), the
// corpus loaded (testdata/coverage_corpus.sql over this repository's prose), the record step run
// to completion (recordSources) as the boot pass leaves a database, and the tables vacuumed, so
// the head finds the visibility map a quiet production table has: the head is the text
// planCoverage chooses while most of embeddings' pages are all-visible, read by index-only scans,
// timed here with the planner free. The text it chooses below that share (coverageBitmapQuery),
// for a table many of whose pages a write changed since its last vacuum, is not timed here. The
// database is dropped before the next scale is built. The base's answer is compared with both
// texts' in one snapshot (requireMainsAnswer), so the time read is the time of the same answer.
//
// The times are read on one connection, each statement once before the timed runs and then
// alternately five times under EXPLAIN (ANALYZE, BUFFERS), each run's time its planning plus
// execution, with no statement_timeout, so neither is cut off by a bound. Every run re-plans
// (QueryExecModeDescribeExec, as benchtime's runner does): a cached prepared statement switches
// to a generic plan from its sixth execution, which is not the plan a call gets.
//
// The size at which the head reaches the bound is taken from the two smallest scales' points,
// fitted both linearly and as a power law t = a × s^k, since the base's time grew faster than
// its corpus; the smaller of the two is the one stated. The last line it prints starts
// `coverage-scale:` and names every number the verdict rests on, so a reader of the Job's log
// judges it from its end.
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

// measureCoverageAtScale builds the corpus at scale in a database of its own, checks it, and
// measures both statements on it, logging what it read; the database is dropped when t ends.
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
	// runs of three on a pod's test server, and the head's index-only scans read the heap.
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

	started := time.Now()
	loadCoverageCorpus(t, databaseURL, scale, prose, script)
	loaded := time.Since(started)
	// Statistics before the record step, whose statements are planned on them; the vacuum after
	// it is what sets the visibility map.
	const tables = "issues, artifacts, artifact_versions, embeddings, embedding_issue_sources"
	mustExec(t, database.Pool, "analyze "+tables)
	started = time.Now()
	recorded := recordOnce(t, database)
	recording := time.Since(started)
	mustExec(t, database.Pool, "vacuum (analyze) "+tables)
	t.Logf("scale %s: corpus loaded in %v, %d issue sources recorded in %v",
		scale.text, loaded.Round(time.Millisecond), recorded, recording.Round(time.Millisecond))
	checkCoverageCorpus(t, database, scale, recorded)

	_, heads := requireMainsAnswer(t, database, "scale "+scale.text)
	var items int64
	var report []string
	for _, row := range heads {
		if row.indexed {
			items += row.items
		}
		report = append(report, row.String())
	}
	slices.Sort(report)
	t.Logf("scale %s: the head's report, kind by kind:\n  %s", scale.text, strings.Join(report, "\n  "))
	var size int64
	var sizeText string
	if err := database.Pool.QueryRow(ctx, `select pg_database_size(current_database()), pg_size_pretty(pg_database_size(current_database()))`).Scan(&size, &sizeText); err != nil {
		t.Fatalf("read the database's size: %v", err)
	}

	head, base := timeCoverageStatements(t, databaseURL, scale)
	point := coverageScalePoint{scale: scale, items: items, head: medianRun(head), base: medianRun(base)}
	t.Logf("scale %s: %d items, database %s (%d bytes); head median %s (runs %s); base median %s (runs %s)",
		scale.text, items, sizeText, size, inMilliseconds(point.head), runsInMilliseconds(head), inMilliseconds(point.base), runsInMilliseconds(base))
	return point
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
// sampled size in turn, beginning `## Summary`. It logs the counts, and how many of embeddings'
// pages the vacuum marked all-visible, which the head's index-only scans read the heap without.
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
	pages, visible := count(`select relpages from pg_class where oid = 'embeddings'::regclass`),
		count(`select relallvisible from pg_class where oid = 'embeddings'::regclass`)
	t.Logf("scale %s: %d issues (%d titled outside ASCII 33 to 126), %d document versions, window rows %s; %d latest specs of the sampled sizes; %d of embeddings' %d pages all-visible",
		scale.text, issues, outsideASCII, versions, strings.Join(windows, ", "), specs-wrong, visible, pages)
}

// timeCoverageStatements times the head (coverageIndexOnlyQuery) and the base (mainCoverageQuery)
// for the corpus's identity on one connection with no statement_timeout: one warm-up run of each,
// then coverageScaleRuns runs of each, alternately, under EXPLAIN (ANALYZE, BUFFERS, FORMAT
// JSON), each run's time its planning plus execution. It logs one plan of each in text form, from
// an EXPLAIN (ANALYZE, BUFFERS) of its own after the timed runs.
func timeCoverageStatements(t *testing.T, databaseURL string, scale coverageScale) (head, base []time.Duration) {
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
	defer func() { _ = conn.Close(ctx) }()
	mustExec(t, conn, `set statement_timeout = 0`)
	statements := []struct {
		name, sql string
		runs      *[]time.Duration
	}{
		{"base (mainCoverageQuery)", mainCoverageQuery, &base},
		{"head (coverageIndexOnlyQuery)", coverageIndexOnlyQuery, &head},
	}
	for _, statement := range statements {
		mustExec(t, conn, statement.sql, configuredIdentity)
	}
	for range coverageScaleRuns {
		for _, statement := range statements {
			*statement.runs = append(*statement.runs, explainedTime(t, conn, statement.name, statement.sql))
		}
	}
	for _, statement := range statements {
		rows, err := conn.Query(ctx, "explain (analyze, buffers) "+statement.sql, configuredIdentity)
		if err != nil {
			t.Fatalf("explain the %s: %v", statement.name, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("read the plan of the %s: %v", statement.name, err)
		}
		t.Logf("scale %s: the %s's plan:\n%s", scale.text, statement.name, strings.Join(lines, "\n"))
	}
	return head, base
}

// explainedTime is one run of statement under EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON): its
// planning plus execution time as the run reports them. A plan missing either is a failure
// rather than a zero, which would read as the fastest run there is.
func explainedTime(t *testing.T, conn *pgx.Conn, name, statement string) time.Duration {
	t.Helper()
	var plan []byte
	if err := conn.QueryRow(context.Background(), "explain (analyze, buffers, format json) "+statement, configuredIdentity).Scan(&plan); err != nil {
		t.Fatalf("explain the %s: %v", name, err)
	}
	var explained []struct {
		Planning  *float64 `json:"Planning Time"`
		Execution *float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(plan, &explained); err != nil || len(explained) != 1 || explained[0].Planning == nil || explained[0].Execution == nil {
		t.Fatalf("read the %s's planning and execution times (%v): %s", name, err, bytes.TrimSpace(plan))
	}
	return time.Duration((*explained[0].Planning + *explained[0].Execution) * float64(time.Millisecond))
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

// judgeCoverageScale fits the head's time to the two smallest scales' points, logs the four
// medians, the item count at which the head would reach bound by a linear fit and by a power law
// t = a × s^k (k = ln(t2/t1) / ln(s2/s1)), the smaller of the two and that as a multiple of the
// 1 × corpus's items, and whether the base's median at scale 1 is above the bound (the negative
// control: the base is what timed out in production). It fails when the multiple is below
// coverageScaleMinMultiple, or when the head's time does not grow between the two scales, where
// neither fit is defined. Its last line starts `coverage-scale:` and is printed whatever the
// verdict, on standard output rather than through t.Log, so the line begins with it in the log.
func judgeCoverageScale(t *testing.T, scales []coverageScale, points []coverageScalePoint, bound time.Duration) {
	t.Helper()
	if len(points) < 2 {
		fmt.Printf("coverage-scale: FAIL: %d of the %d scales were measured; the fit needs two\n", len(points), len(scales))
		t.Fatalf("%d of the %d scales were measured; the fit needs two, the failures are above", len(points), len(scales))
	}
	slices.SortFunc(points, func(a, b coverageScalePoint) int { return cmp.Compare(a.scale.value, b.scale.value) })
	first, second := points[0], points[1]
	s1, s2 := float64(first.items), float64(second.items)
	t1, t2 := float64(first.head), float64(second.head)
	line := float64(bound)

	oneX := s1 / first.scale.value
	oneXFrom := fmt.Sprintf("scale %s's %d items divided by %s", first.scale.text, first.items, first.scale.text)
	control := "not measured, since scale 1 is not among " + coverageScalesEnv
	for _, point := range points {
		if point.scale.value == 1 {
			oneX, oneXFrom = float64(point.items), "scale 1"
			control = fmt.Sprintf("the base's median at scale 1 is %s, above %v: %t", inMilliseconds(point.base), bound, point.base > bound)
		}
	}
	t.Logf("the four medians: scale %s (%d items) head %s, base %s; scale %s (%d items) head %s, base %s",
		first.scale.text, first.items, inMilliseconds(first.head), inMilliseconds(first.base),
		second.scale.text, second.items, inMilliseconds(second.head), inMilliseconds(second.base))
	t.Logf("negative control: %s", control)
	medians := fmt.Sprintf("scales %s and %s: items %d and %d; head median %s and %s; base median %s and %s",
		first.scale.text, second.scale.text, first.items, second.items,
		inMilliseconds(first.head), inMilliseconds(second.head), inMilliseconds(first.base), inMilliseconds(second.base))
	if !(s2 > s1) || !(t2 > t1) {
		fmt.Printf("coverage-scale: FAIL: %s; the head's time does not grow from the smaller scale to the larger, so no fit is defined; negative control: %s\n", medians, control)
		t.Fatalf("the head's median does not grow from scale %s to scale %s (%s to %s over %d to %d items), so neither fit is defined: "+
			"measure at larger scales, where the head's time stands above the noise", first.scale.text, second.scale.text,
			inMilliseconds(first.head), inMilliseconds(second.head), first.items, second.items)
	}

	linear := s1 + (line-t1)*(s2-s1)/(t2-t1)
	k := math.Log(t2/t1) / math.Log(s2/s1)
	power := s1 * math.Pow(line/t1, 1/k)
	smaller := math.Min(linear, power)
	multiple := smaller / oneX
	t.Logf("the head reaches %v at %.0f items by the linear fit and at %.0f items by the power law (k = %.3f); "+
		"the smaller, %.0f items, is %.2f × the 1 × corpus's %.0f items (%s)", bound, linear, power, k, smaller, multiple, oneX, oneXFrom)
	verdict := "pass"
	if multiple < coverageScaleMinMultiple {
		verdict = "FAIL"
	}
	fmt.Printf("coverage-scale: %s: %s; the head reaches %v at %.0f items (linear) and %.0f items (power law, k = %.3f); "+
		"the smaller, %.0f items, is %.2f x the 1 x corpus's %.0f items, at least %g wanted; negative control: %s\n",
		verdict, medians, bound, linear, power, k, smaller, multiple, oneX, coverageScaleMinMultiple, control)
	if multiple < coverageScaleMinMultiple {
		t.Errorf("the head would reach %v at %.2f × production's corpus, below the %g × R2 asks for", bound, multiple, coverageScaleMinMultiple)
	}
}
