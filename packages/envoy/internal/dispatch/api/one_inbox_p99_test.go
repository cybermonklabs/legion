//go:build basebuild

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
	"github.com/sjawhar/envoy/internal/testtmp"
)

// c-p99 (the spec's R11, the plan's T16): neither the one read nor a write gets slower
// beyond R11's bounds, every figure of a run taken in that run on two builds over one corpus: each
// write held to its bound in its own run, and the read judged over three such runs.
//
// It times two processes the same way: the build of the commit BASELINE_SHA names (buildBaseline),
// checked out as base-p99 under a directory of the test's own in TMPDIR (testtmp.Dir) and booted
// over $ISSUE_P99_DATABASE_URL, and this build, built from this checkout with the stamp the base
// build gets and booted over a database of the test's own the way the base build is booted
// (bootOneInboxThisBuild, startBuild: the person read from one trusted header, NATS off, a
// stand-in listener, and the Go runtime settings this process runs under). It boots a build of an
// earlier commit because what it holds this build to - the reads this
// one replaces, and each write as it was before this build's node and tuple writes - this build
// cannot answer. Each corpus is the one one_inbox_scale_seed_test.go seeds, through its build's API
// and its own pool: the base build's through its process, and this build's through this build's
// handler in the process (bootOneInboxHere), which then takes the tuples of every row the seed wrote
// in SQL from the backfill (graphcensus.Backfill), as an older build's rows take them, so the heavy
// person's history is in the store the read reads. The handler in the process is kept for what only
// it can do: it serves the seed before this build's process is booted, and its traced store records
// the statements and rows of each counted move (countOneInboxMoves). Each process is timed once it
// has drained what the corpus owes its background work (settleOneInbox), and a person's request asks
// for display trees on both, as the dashboard asks.
//
// Both databases run with jit = off, set on each before any measured session opens on it
// (turnJITOff, jitOffStore) and read back on a new session of each before the first reading, which
// the first c-p99: line prints with pg_jit_available (turnJITOff says why).
//
// Before any timing it runs `vacuum analyze` and then `explain (analyze)` on the one read of each
// reader below as the dashboard's request of it ran through this build's handler: the read's
// statement prepared, and its execute explained with that request's own arguments under the
// settings the read's transaction makes (meInboxSettings), so the plan is the generic plan every
// execution of the read runs. It fails on a sequential scan of comments or messages, the two tables
// the corpus fills to scale (the scan rule), and on any planner setting that steers that plan (the
// hint rule, explainOneInboxRead); it prints the plan over asks and events, which bounds nothing.
// Then, alternating between the two processes one operation at a time,
// oneInboxReadings readings of each after one not counted, each the round trip on loopback, taken by
// this test's client the same way against both:
//
//   - the reads, per shape (oneInboxReaders): this build's GET /api/v1/me/inbox?state=inbox, which
//     the dashboard reads, against the reads it replaces on the base build - GET /api/v1/inbox
//     ?assignee=me, GET /api/v1/me/replies?kind=reply,agent&state=inbox and GET
//     /api/v1/agents/{session_id}/messages for each conversation the reader started - taken one
//     after another and summed, which is the base build's p50 the read's own p50 is set beside.
//     Each run prints both p50s and fails on neither: the read is judged over three runs, the
//     median of their p50s of it at most oneInboxReadBound times the median of the base build's
//     (Verification's c-p99, dispatch://INFRA-1098/spec#b-r-no-regression@v173);
//   - the writes (oneInboxWritePaths), each held in its own run to at most its own bound in
//     oneInboxWriteBounds times the base build's p50 of the same path, but for
//     putProjectDefaultAssignee's move of many asks, which moves more than one ask a reading,
//     holds to no bound, and is printed with the count it moved. Its move of one ask is timed and
//     bounded as patchIssue's and putAskAskedOf's are.
//
// Then, on an audit of each database (installOneInboxAudit): the rows one comment in the thread
// at the bound writes, held to two per participant beside its fixed rows; and, on this build, each
// counted move of who an ask waits on (countOneInboxMoves), held to three tuple rows for each ask of
// the first sort and four for each of the second, with its fixed rows printed apart, and to a count
// of statements that does not grow from its move of one ask to its move of many.
//
// Every figure is printed on a line of its own under the prefix c-p99:. The writes' p50 bounds,
// each in its own run, the scan rule, the hint rule, the row rules and the statement rule are what
// fail it, and, before any of them, a table of write bounds that does not match the write paths
// (requireOneInboxWriteBoundsMatchPaths). The read's p50 ratio fails no run: each run prints the
// two p50s of each shape again on a c-p99: read-p50 line, and Verification's c-p99 judges the read
// over three runs' lines. Every p95 and p99 is a reading of the pod and fails nothing. It runs
// when LEGION_SCALE_TEST is 1, as a14 does: seeding two corpora and taking its readings costs
// minutes, which no ordinary run pays.

const (
	// oneInboxReadBound is R11's bound on the one read: the median of three runs' p50 of it at most
	// twice the median of the three runs' p50 of the reads it replaces, each run taking both (the
	// root spec's R22). Verification's c-p99 judges it over the three runs' c-p99: read-p50 lines
	// (dispatch://INFRA-1098/spec#b-r-no-regression@v173); no run fails on it, and each prints it
	// on its c-p99: read lines beside that run's ratio.
	oneInboxReadBound = 2.0
	// oneInboxAgentToken is the agent bearer newTestServer's server trusts (sharedAgentTokens).
	oneInboxAgentToken = "agent-token"
	// oneInboxOneRead is the one read as the dashboard asks for it (display trees beside it), which
	// every reading of the read times (measureOneInboxRead) and the scan rule explains
	// (explainOneInboxRead).
	oneInboxOneRead = "/api/v1/me/inbox?state=inbox"
)

// oneInboxWriteBounds is R11's bound on each write path that holds to one, by the path's name in
// oneInboxWritePaths: its p50 at most this many times the base build's p50 of the same path in the
// same run, as the Verification's c-p99 list names it. Each bound is the worst p50 ratio three
// clean runs of this check measured for the path, times 1.1, rounded up to the next 0.25, so a
// path slower than any run measured by a tenth fails. The table names every write path but the one
// that moves more than one ask a reading (project-default-assignee), which holds to no bound, and
// names nothing else: the test fails at its start where it does not
// (requireOneInboxWriteBoundsMatchPaths).
var oneInboxWriteBounds = map[string]float64{
	"comment":                          3.50,
	"agent-clear-and-read":             3.00,
	"mark-all-read":                    2.75,
	"broadcast":                        2.75,
	"direct-reply":                     2.25,
	"comment-mention-person":           2.25,
	"comment-mention":                  2.00,
	"issue-message":                    2.00,
	"patch-issue-assignee":             2.00,
	"ask-asked-of":                     1.75,
	"ask-answer":                       1.75,
	"project-default-assignee-one-ask": 2.25,
	"dismissal":                        1.50,
	"thread-read":                      1.50,
	"snooze":                           1.50,
	// A person's reply under an Agent root, the write the Inbox requests' R1 added and their c-p99
	// check times, bounded by the same rule: three clean runs gave p50 ratios of 1.80, 2.13 and 2.20.
	"agent-reply": 2.50,
}

// oneInboxReader is one shape the read is measured over: the shape in words; its key, which holds
// no space, as the c-p99: read-p50 line prints it; the person whose read it is; and the sessions
// of the conversations they started, one direct-reply read of the base build each.
type oneInboxReader struct {
	shape, key, login string
	sessions          []string
}

// oneInboxReaders are the corpus's shapes as reads: the median person, whose history holds the
// thread at the bound, which every ordinary person writes in; the person of the thread of one; the
// person of the reply chain at the walk's depth bound; the person whose history is a hundred times
// the median; and the person whose history is empty.
var oneInboxReaders = []oneInboxReader{
	{"the thread of 1,000 participants", "thread-of-1000", oneInboxMedianReader, []string{oneInboxPersonSession(17)}},
	{"the thread of one", "thread-of-one", oneInboxSolo, nil},
	{"the reply chain at the walk's depth bound", "reply-chain", oneInboxChain, nil},
	{"the history a hundred times the median", "heavy-history", oneInboxHeavy, []string{oneInboxHeavySession}},
	{"the empty history", "empty-history", oneInboxEmpty, nil},
}

// oneInboxHere is this build as c-p99 drives it: its handler in the process, over a store traced
// for the statements a request marked countedRead runs (meInboxStatements) and those of each
// request of the one read the scan rule explains (oneInboxTracer), served on loopback, which seeds
// the corpus and makes the counted moves; and, once the scan rule and the hint rule have passed,
// its own process over the same database (bootOneInboxThisBuild), which every timed reading reads.
type oneInboxHere struct {
	side       *oneInboxSide
	handler    http.Handler
	statements *meInboxStatements
	reads      *oneInboxTracer
}

func TestOneInboxP99HoldsTheReadsAndTheWrites(t *testing.T) {
	// Not parallel: it prints the one read's p50 beside the base build's, which Verification's
	// c-p99 judges over three runs against oneInboxReadBound
	// (dispatch://INFRA-1098/spec#b-r-no-regression@v173), and fails this run where a write's p50
	// is above its bound in oneInboxWriteBounds times the base build's, round trips on loopback
	// timed in this run, wall clock that the load of the tests beside it can push past.
	if os.Getenv(dismissedScaleEnv) != "1" {
		t.Skipf("set %s=1 to take c-p99's measurement", dismissedScaleEnv)
	}
	requireOneInboxWriteBoundsMatchPaths(t)
	ctx := context.Background()
	databaseURL := baselineDatabaseURL(t, oneInboxDatabaseEnv, oneInboxDatabase)
	turnJITOff(t, "the base build's database", databaseURL)

	here := bootOneInboxHere(t)
	this := here.side
	this.corpus = seedOneInboxCorpus(t, this.database, this.server)
	backfillOneInbox(t, this.database)
	requireOneInboxHistoryInTheStore(t, this.database)
	analyzeOneInbox(t, this)
	thisJIT := requireJITOff(t, "this build's database", this.database.Pool.Config().ConnString())
	// The scan rule and the hint rule, before anything is timed: a read whose plan scans a table
	// the corpus fills to scale, or whose plan a planner setting steers, is no read a timing of it
	// would say anything about.
	explainOneInboxRead(t, here)
	this.server = bootOneInboxThisBuild(t, this.database)
	settleOneInbox(t, this, oneInboxOwedWorkHere)
	analyzeOneInbox(t, this)

	binary, commit := buildBaseline(t, filepath.Join(testtmp.Dir(t, oneInboxWorktree), oneInboxWorktree))
	baseServer := bootBaselineTrustingHeader(t, binary, databaseURL)
	baseDatabase, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the database %s names: %v", oneInboxDatabaseEnv, err)
	}
	t.Cleanup(baseDatabase.Pool.Close)
	base := &oneInboxSide{name: "the base build", server: baseServer, database: baseDatabase}
	base.corpus = seedOneInboxCorpus(t, base.database, base.server)
	settleOneInbox(t, base, oneInboxOwedWork)
	analyzeOneInbox(t, base)
	baseJIT := requireJITOff(t, "the base build's database", databaseURL)

	this.server.trees, base.server.trees = true, true
	t.Logf("c-p99: the base build %s against this build at %s, each a process of its own, %d readings of each operation and shape "+
		"after one not counted, alternating between the two one operation at a time, each the round trip on loopback; "+
		"each database read on a new session: this build's %s, the base build's %s; taken_at=%s",
		commit, gitOutput(t, ".", "rev-parse", "HEAD"), oneInboxReadings, thisJIT, baseJIT, oneInboxNow())
	for _, reader := range oneInboxReaders {
		measureOneInboxRead(t, reader, base, this)
	}
	for _, path := range oneInboxWritePaths() {
		measureOneInboxWrite(t, path, base, this)
	}

	installOneInboxAudit(t, base.database)
	installOneInboxAudit(t, this.database)
	countOneInboxCommentRows(t, "c-p99", base)
	countOneInboxCommentRows(t, "c-p99", this)
	countOneInboxMoves(t, here)
}

// requireOneInboxWriteBoundsMatchPaths fails the test unless oneInboxWriteBounds names exactly the
// write paths of oneInboxWritePaths that hold to a bound. Each of these fails it: a name no write
// path carries, which a path since renamed leaves; a path that is not many and that the table does
// not name, which would hold to no bound; and a many path the table names, a bound nothing holds.
func requireOneInboxWriteBoundsMatchPaths(t *testing.T) {
	t.Helper()
	var mismatches []string
	carried := make(map[string]bool)
	for _, path := range oneInboxWritePaths() {
		carried[path.name] = true
		_, named := oneInboxWriteBounds[path.name]
		switch {
		case path.many && named:
			mismatches = append(mismatches, fmt.Sprintf("names the write %s, which moves more than one ask a reading and holds to no bound", path.name))
		case !path.many && !named:
			mismatches = append(mismatches, fmt.Sprintf("does not name the write %s, which holds to a bound", path.name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(oneInboxWriteBounds)) {
		if !carried[name] {
			mismatches = append(mismatches, fmt.Sprintf("names the write %s, which no write path carries", name))
		}
	}
	if len(mismatches) > 0 {
		t.Fatalf("oneInboxWriteBounds does not match oneInboxWritePaths: it %s", strings.Join(mismatches, "; it "))
	}
}

// bootOneInboxHere builds this build's server in the process over a database of the test's own
// with JIT off (jitOffStore): a person read from the header the base build is told to trust too,
// on the untraced pool so a sign-in is no statement of a counted write; the stand-in listener the
// base build delivers to; this checkout's commit as its build commit, as every server a two-build
// check boots carries its own; and an event horizon that marks each statement it runs
// (markedHorizonPool), so a counted write's statements leave out the horizon's own and no other.
// Its side's server is the handler served on loopback until bootOneInboxThisBuild.
func bootOneInboxHere(t *testing.T) *oneInboxHere {
	t.Helper()
	database := jitOffStore(t, "this build's database", storetest.Open(t))
	statements := &meInboxStatements{}
	reads := &oneInboxTracer{meInboxStatements: statements}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, reads, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store:       traced,
		identity:    headerIdentity(database),
		envoyURL:    standInListener(t),
		buildCommit: gitOutput(t, ".", "rev-parse", "HEAD"),
		horizonDB:   markedHorizonPool(t, traced),
	})
	return &oneInboxHere{
		side:    &oneInboxSide{name: "this build", server: serveOnLoopback(t, handler, oneInboxAgentToken), database: database},
		handler: handler, statements: statements, reads: reads,
	}
}

// bootOneInboxThisBuild builds this checkout's ./cmd/dispatch stamped with its commit, as
// buildBaseline stamps the base build, and boots it over database as the base build is booted over
// its own (startBuild), so the two builds' readings are taken of two processes alike: a timing of
// the handler in this process would share its processor and its collector with the client and the
// test's own corpus, which the base build's process does not.
func bootOneInboxThisBuild(t *testing.T, database *store.Store) *baselineServer {
	t.Helper()
	root := gitOutput(t, ".", "rev-parse", "--show-toplevel")
	commit := gitOutput(t, root, "rev-parse", "HEAD")
	binary := cmdtest.BuildCommand(t, cmdtest.Command{
		Name:   "this-build-dispatch",
		Dir:    filepath.Join(root, "packages", "envoy"),
		Target: "./cmd/dispatch",
		Flags:  []string{"-ldflags", "-X main.buildCommit=" + commit},
	})
	t.Logf("this build is %s, built from %s at %s", binary, root, commit)
	return startBuild(t, "this build", binary, database.Pool.Config().ConnString(), baselineIdentityHeader)
}

// backfillOneInbox runs the backfill over this build's corpus, which states every row the seed
// wrote in SQL - its comments, messages and read marks, written as an older build writes them - in
// the store, and fails unless every kind it walks is complete.
func backfillOneInbox(t *testing.T, database *store.Store) {
	t.Helper()
	started := time.Now()
	var out strings.Builder
	report, err := graphcensus.Backfill(context.Background(), database, &out)
	if err != nil {
		t.Fatalf("backfill this build's corpus: %v\n%s", err, out.String())
	}
	var rows, nodes, tuples int64
	for _, kind := range report.Kinds {
		if !kind.Complete {
			t.Fatalf("the backfill left %s owing %d rows:\n%s", kind.Kind, kind.Remaining, out.String())
		}
		rows, nodes, tuples = rows+kind.Rows, nodes+kind.Nodes, tuples+kind.Tuples
	}
	t.Logf("c-p99: the backfill stated this build's corpus in %s: %d kinds complete, rows=%d nodes=%d tuples=%d",
		time.Since(started).Round(time.Second), len(report.Kinds), rows, nodes, tuples)
}

// requireOneInboxHistoryInTheStore holds this build's store to the heavy person's history: a
// participates_in tuple from their node into the root of every thread they wrote in and of every
// conversation they started, which is what the one read walks; and prints every tuple at their
// node by kind, and the store's size beside the two tables the corpus fills to scale.
func requireOneInboxHistoryInTheStore(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	var threads, conversations int64
	if err := database.Pool.QueryRow(ctx, `
		select count(distinct to_id) filter (where to_kind = $3), count(distinct to_id) filter (where to_kind = $4)
		from graph_tuples
		where from_kind = $1 and from_id = $2 and kind = $5 and asserted
	`, graphstore.KindPerson, oneInboxHeavy, graphstore.KindComment, graphstore.KindMessage, graphstore.TupleParticipatesIn).
		Scan(&threads, &conversations); err != nil {
		t.Fatalf("read the heavy person's participation: %v", err)
	}
	rows, err := database.Pool.Query(ctx, `
		select kind, count(*) from graph_tuples
		where (to_kind = $1 and to_id = $2) or (from_kind = $1 and from_id = $2)
		group by kind order by kind
	`, graphstore.KindPerson, oneInboxHeavy)
	if err != nil {
		t.Fatalf("read the heavy person's tuples: %v", err)
	}
	var kinds []string
	var held int64
	var kind string
	var count int64
	if _, err := pgx.ForEachRow(rows, []any{&kind, &count}, func() error {
		kinds = append(kinds, fmt.Sprintf("%s=%d", kind, count))
		held += count
		return nil
	}); err != nil {
		t.Fatalf("read the heavy person's tuples: %v", err)
	}
	var comments, messages, tuples, nodes int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from comments), (select count(*) from messages),
		       (select count(*) from graph_tuples), (select count(*) from graph_nodes)
	`).Scan(&comments, &messages, &tuples, &nodes); err != nil {
		t.Fatalf("count this build's corpus: %v", err)
	}
	t.Logf("c-p99: this build's store holds %d tuples and %d nodes beside comments=%d messages=%d; the heavy person's node "+
		"holds %d tuples (%s), and takes part in %d thread roots and %d conversation roots",
		tuples, nodes, comments, messages, held, strings.Join(kinds, " "), threads, conversations)
	if want := int64(oneInboxHeavyFactor * oneInboxMedianHistory); threads < want || conversations < want {
		t.Fatalf("the heavy person takes part in %d thread roots and %d conversation roots by the store, want at least %d of each: "+
			"the history the read is measured over is not in the store", threads, conversations, want)
	}
}

// explainOneInboxRead holds the one read of each reader to the scan rule and the hint rule. It sends
// the dashboard's request of the read (oneInboxOneRead, display trees beside it) as the reader
// through this build's handler, whose store keeps every statement that request runs
// (oneInboxTracer), and explains the read's statement - the one meInboxStatement builds for that
// request, which the request must have run once - as the request ran it: prepared, and its execute
// explained with ANALYZE, with the request's own arguments, under the settings the read's own
// transaction makes (meInboxSettings), in a transaction rolled back (explainDismissedScaleGeneric).
// That is the generic plan, the plan every execution of the read runs (meInboxGenericPlan). It
// prints each plan's times and its scans of asks and events, which bound nothing, and fails, once
// every plan is printed, on either rule:
//
//   - the scan rule: a sequential scan of comments or messages, the two tables the corpus fills to
//     scale, in the plan of any read, printing that plan;
//   - the hint rule, that no planner setting steers the plan the scan rule reads, wherever it is
//     made: the connection the plans are made on holds no planner setting its database, its role,
//     its parameters or its session set but the jit = off the test sets on its database
//     (oneInboxHeldPlannerSettings); the request makes the planner settings meInboxSettings make,
//     each before the read's statement, and no other, and carries no planner hint comment; and the
//     read's plan made under the generic plan alone
//     (plan_cache_mode = force_generic_plan, which explainDismissedScaleGeneric always sets, as
//     meInboxGenericPlan does), without meInboxSettings' other settings, scans neither table where
//     the plan under all of meInboxSettings does not, so no setting of theirs beside the generic
//     plan hides a scan.
func explainOneInboxRead(t *testing.T, here *oneInboxHere) {
	t.Helper()
	ctx := context.Background()
	acquired, err := here.side.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans of the one read: %v", err)
	}
	defer acquired.Release()
	connection := acquired.Conn()
	planner := oneInboxPlannerSettings(t, connection)
	hints := oneInboxHeldPlannerSettings(t, connection)
	var settings []string
	for _, setting := range meInboxSettings {
		if oneInboxSetsPlanner(setting, planner) {
			settings = append(settings, setting)
		}
	}
	var scans, plans []string
	for _, reader := range oneInboxReaders {
		read := fmt.Sprintf("the one read of %s (%s)", reader.shape, reader.login)
		// The text alone: no argument of a request changes it (meInboxShape).
		statement, _, err := meInboxStatement(model.Actor{Kind: "user", ID: model.CanonicalLogin(reader.login)}, 0,
			meInboxRequest{state: myRepliesStateInbox}, true, true, nil)
		if err != nil {
			t.Fatalf("build %s: %v", read, err)
		}
		ran := here.reads.read(t, here.handler, reader.login)
		at, runs := -1, 0
		var made []string
		for index, statementRan := range ran {
			own := statementRan.sql == statement
			if own {
				at, runs = index, runs+1
			}
			if strings.Contains(statementRan.sql, "/*+") {
				hints = append(hints, fmt.Sprintf("the request of %s carries a planner hint comment: %.300q", read, statementRan.sql))
			}
			switch {
			case !oneInboxSetsPlanner(statementRan.sql, planner):
			case own:
				hints = append(hints, fmt.Sprintf("the statement of %s makes a planner setting of its own", read))
			default:
				made = append(made, statementRan.sql)
				if at >= 0 {
					hints = append(hints, fmt.Sprintf("the request of %s makes %q after the read's statement", read, statementRan.sql))
				}
			}
		}
		if runs != 1 {
			t.Fatalf("the request GET %s of %s ran the statement meInboxStatement builds for it %d times, want once: the scan rule "+
				"would explain a statement the read does not run", oneInboxOneRead, read, runs)
		}
		if !slices.Equal(made, settings) {
			hints = append(hints, fmt.Sprintf("the request of %s makes the planner settings %q, where meInboxSettings make %q",
				read, made, settings))
		}
		args := ran[at].args
		plan, text := explainDismissedScaleGeneric(t, connection, read, statement, args, meInboxSettings...)
		bare, bareText := explainDismissedScaleGeneric(t, connection, read+" under the generic plan alone", statement, args)
		over := map[string]map[string]float64{"asks": {}, "events": {}}
		for _, node := range explaintest.Nodes(t, plan) {
			if scanned, held := over[node.Relation]; held {
				scanned[node.String()] += cmp.Or(node.Loops, 1)
			}
		}
		t.Logf("c-p99: plan of %s, its generic plan under meInboxSettings %q with its request's own arguments (the request made the "+
			"planner settings %q): planning=%s execution=%s; over asks: %s; over events: %s",
			read, meInboxSettings, made, oneInboxPlanTime(text, "Planning Time"), oneInboxPlanTime(text, "Execution Time"),
			oneInboxScansOver(over["asks"]), oneInboxScansOver(over["events"]))
		printed, printedBare := false, false
		for _, relation := range []string{"comments", "messages"} {
			switch {
			case explaintest.SeqScansRelation(t, plan, relation):
				scans = append(scans, fmt.Sprintf("%s scans %s sequentially under its generic plan", read, relation))
				if !printed {
					plans = append(plans, fmt.Sprintf("the generic plan of %s:\n%s", read, text))
					printed = true
				}
			case explaintest.SeqScansRelation(t, bare, relation):
				hints = append(hints, fmt.Sprintf("%s scans %s sequentially under the generic plan alone, without meInboxSettings' "+
					"other settings, and not under all of them: a setting of theirs beside the generic plan hides the scan", read, relation))
				if !printedBare {
					plans = append(plans, fmt.Sprintf("the generic plan of %s under plan_cache_mode = force_generic_plan alone, "+
						"without meInboxSettings' other settings:\n%s", read, bareText))
					printedBare = true
				}
			}
		}
	}
	var failures []string
	if len(scans) > 0 {
		failures = append(failures, fmt.Sprintf("scan rule: %s, a table the corpus fills to scale; no timing is taken over a read "+
			"that scans one", strings.Join(scans, "; ")))
	}
	if len(hints) > 0 {
		failures = append(failures, fmt.Sprintf("hint rule: %s; no timing is taken over a read whose plan a planner setting steers",
			strings.Join(hints, "; ")))
	}
	if len(failures) > 0 {
		t.Fatalf("%s\n%s", strings.Join(failures, "\n"), strings.Join(plans, "\n"))
	}
}

// oneInboxKept marks the context of a request whose statements oneInboxTracer keeps: a request of
// the one read the scan rule explains (oneInboxTracer.read), which meInboxStatements does not count.
type oneInboxKept struct{}

// oneInboxTracer is the tracer of this build's traced store: meInboxStatements, which records what
// a request marked countedRead runs, and beside it every statement of a request oneInboxKept marks,
// so the scan rule explains the read's statement with the arguments its request sent and the hint
// rule reads every setting that request made.
type oneInboxTracer struct {
	*meInboxStatements
	mu   sync.Mutex
	kept []sentStatement
}

func (r *oneInboxTracer) keep(ctx context.Context, sql string, args []any) {
	if ctx.Value(oneInboxKept{}) == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kept = append(r.kept, sentStatement{sql: sql, args: args})
}

func (r *oneInboxTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.keep(ctx, data.SQL, data.Args)
	return r.meInboxStatements.TraceQueryStart(ctx, conn, data)
}

func (r *oneInboxTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, queued := range data.Batch.QueuedQueries {
		r.keep(ctx, queued.SQL, queued.Arguments)
	}
	return r.meInboxStatements.TraceBatchStart(ctx, conn, data)
}

// read sends the dashboard's request of the one read (oneInboxOneRead, display trees beside it) as
// login through handler, on a context oneInboxKept marks, and answers every statement it ran.
func (r *oneInboxTracer) read(t *testing.T, handler http.Handler, login string) []sentStatement {
	t.Helper()
	r.mu.Lock()
	r.kept = nil
	r.mu.Unlock()
	request := httptest.NewRequestWithContext(context.WithValue(context.Background(), oneInboxKept{}, true),
		http.MethodGet, oneInboxOneRead, nil)
	request.Header.Set(baselineIdentityHeader, login)
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s as %s through this build's handler: %d %s", oneInboxOneRead, login, response.Code, response.Body.String())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.kept)
}

// oneInboxPlannerSettings is the name of every setting that steers the planner, as pg_settings files
// them: its Query Tuning categories, which hold the planner's methods, its cost constants, the
// genetic optimizer and its other options, JIT and the plan cache's mode among them.
func oneInboxPlannerSettings(t *testing.T, connection *pgx.Conn) map[string]bool {
	t.Helper()
	rows, err := connection.Query(context.Background(), `select name from pg_settings where category like 'Query Tuning%'`)
	if err != nil {
		t.Fatalf("read the planner's settings: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(names) == 0 {
		t.Fatalf("read the planner's settings: %d of them, %v", len(names), err)
	}
	planner := make(map[string]bool, len(names))
	for _, name := range names {
		planner[name] = true
	}
	return planner
}

// oneInboxHeldPlannerSettings names each planner setting connection holds other than as the server
// was started with it (pg_settings' source: its default, its configuration file, its command line,
// its environment or an override of its own): one its database, its role, the connection's
// parameters or a statement of its session set, which would steer every plan made on it, the scan
// rule's among them, and which neither the read's statement nor its settings name. It names no
// jit = off, which the test sets on the database (jitOffStore) and which steers no plan: JIT
// compiles the plan the planner chose once it has chosen it, and the read's own transaction sets
// it off too (meInboxNoJIT).
func oneInboxHeldPlannerSettings(t *testing.T, connection *pgx.Conn) []string {
	t.Helper()
	rows, err := connection.Query(context.Background(), `
		select format('the connection the plans are made on holds %s = %s, which its %s set', name, setting, source)
		from pg_settings
		where category like 'Query Tuning%'
		  and source not in ('default', 'environment variable', 'configuration file', 'command line', 'override')
		  and not (name = 'jit' and setting = 'off')
		order by name
	`)
	if err != nil {
		t.Fatalf("read the planner settings of the connection the plans are made on: %v", err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the planner settings of the connection the plans are made on: %v", err)
	}
	return held
}

// oneInboxSettingNamed matches each setting a statement makes: `set`, `set local` or `set session`
// <name> and `reset <name>` as the statement itself, and set_config('<name>', ...) anywhere in it.
var oneInboxSettingNamed = regexp.MustCompile(`(?is)^\s*(?:set|reset)\s+(?:local\s+|session\s+)?([a-z_][a-z0-9_.]*)|set_config\s*\(\s*'([^']+)'`)

// oneInboxSetsPlanner is whether statement makes a setting planner names.
func oneInboxSetsPlanner(statement string, planner map[string]bool) bool {
	for _, named := range oneInboxSettingNamed.FindAllStringSubmatch(statement, -1) {
		if planner[strings.ToLower(named[1]+named[2])] {
			return true
		}
	}
	return false
}

// oneInboxPlanTime is what an analysed plan's text reports under label: "Planning Time" or
// "Execution Time".
func oneInboxPlanTime(text, label string) string {
	for line := range strings.Lines(text) {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), label+": "); found {
			return value
		}
	}
	return "not reported"
}

// oneInboxScansOver prints the scans of one relation a plan makes, each with how many times it ran.
func oneInboxScansOver(scans map[string]float64) string {
	if len(scans) == 0 {
		return "none"
	}
	var printed []string
	for _, scan := range slices.Sorted(maps.Keys(scans)) {
		printed = append(printed, fmt.Sprintf("%s x%.0f", scan, scans[scan]))
	}
	return strings.Join(printed, ", ")
}

// oneInboxPercentiles prints readings' p50, p95 and p99, each the nearest-rank reading, so every
// number is a run that happened, and answers the p50.
func oneInboxPercentiles(readings []time.Duration) (string, time.Duration) {
	sorted := slices.Sorted(slices.Values(readings))
	p50 := percentileOfDismissedScale(sorted, 0.50)
	return fmt.Sprintf("p50=%.2fms p95=%.2fms p99=%.2fms", msOf(p50), msOf(percentileOfDismissedScale(sorted, 0.95)),
		msOf(percentileOfDismissedScale(sorted, 0.99))), p50
}

// measureOneInboxRead takes oneInboxReadings readings of the one read of reader on this build and
// of the reads it replaces on the base build, after one of each not counted, alternating which
// build is read first, and prints both builds' p50, p95 and p99 on one line, with each of the base
// build's reads apart and what each build answered, and the two p50s again on a c-p99: read-p50
// line under reader's key. It fails no run on the read's ratio: Verification's c-p99 judges the
// read over three runs' c-p99: read-p50 lines, the median of their p50s of the one read at most
// oneInboxReadBound times the median of the base build's
// (dispatch://INFRA-1098/spec#b-r-no-regression@v173). Each write still fails in its own run
// (measureOneInboxWrite). For the reader whose Inbox holds the gated issues' items
// (seedOneInboxGate), the reading not counted must carry an approval gate on every entry of those
// issues (requireOneInboxGates): a build whose statement leaves the gate out fails there. The
// corpus does not change between readings, so that one answer proves what each would.
func measureOneInboxRead(t *testing.T, reader oneInboxReader, base, this *oneInboxSide) {
	t.Helper()
	replaced := []string{"/api/v1/inbox?assignee=me", "/api/v1/me/replies?kind=reply,agent&state=inbox"}
	for _, session := range reader.sessions {
		replaced = append(replaced, "/api/v1/agents/"+session+"/messages")
	}
	var ones, bases []time.Duration
	parts := make([][]time.Duration, len(replaced))
	var entries, asks, rows int
	// The reading not counted keeps its answer, to say what each build answered and to check the
	// gates; every counted one, on both builds, drops it as it arrives (baselineServer.read), so
	// neither build's clock pays for keeping or decoding an answer the other's does not.
	gated := reader.login == oneInboxHeavy && len(this.corpus.gateIssues) > 0
	get := func(side *oneInboxSide, reading int, path string) apiAnswer {
		if reading == 0 {
			return oneInboxExpect(t, side.name+": "+path+" of "+reader.shape,
				side.server.call(t, reader.login, http.MethodGet, path, nil), http.StatusOK)
		}
		return oneInboxExpect(t, side.name+": "+path+" of "+reader.shape, side.server.read(t, reader.login, path), http.StatusOK)
	}
	readOne := func(reading int) {
		answer := get(this, reading, oneInboxOneRead)
		if reading == 0 {
			if gated {
				requireOneInboxGates(t, reading, answer.body, this.corpus.gateIssues)
			}
			var read meInboxResponse
			if err := json.Unmarshal(answer.body, &read); err != nil {
				t.Fatalf("decode the one read of %s: %v", reader.shape, err)
			}
			entries = read.Total
		} else {
			ones = append(ones, answer.took)
		}
	}
	readBase := func(reading int) {
		var took time.Duration
		for index, path := range replaced {
			answer := get(base, reading, path)
			took += answer.took
			if reading > 0 {
				parts[index] = append(parts[index], answer.took)
				continue
			}
			switch index {
			case 0:
				var listed []json.RawMessage
				if err := json.Unmarshal(answer.body, &listed); err != nil {
					t.Fatalf("decode the base build's Inbox of %s: %v", reader.shape, err)
				}
				asks = len(listed)
			case 1:
				var listed model.MyRepliesResponse
				if err := json.Unmarshal(answer.body, &listed); err != nil {
					t.Fatalf("decode the base build's Replies of %s: %v", reader.shape, err)
				}
				rows = len(listed.Rows)
			}
		}
		if reading > 0 {
			bases = append(bases, took)
		}
	}
	for reading := 0; reading <= oneInboxReadings; reading++ {
		if reading%2 == 0 {
			readBase(reading)
			readOne(reading)
		} else {
			readOne(reading)
			readBase(reading)
		}
	}
	thisFigures, thisP50 := oneInboxPercentiles(ones)
	baseFigures, baseP50 := oneInboxPercentiles(bases)
	var each []string
	for index, path := range replaced {
		figures, _ := oneInboxPercentiles(parts[index])
		each = append(each, path+" "+figures)
	}
	ratio := float64(thisP50) / float64(baseP50)
	t.Logf("c-p99: read %s (%s): this build GET %s %s entries=%d | the base build, its %d reads in turn, %s asks=%d rows=%d "+
		"(%s) | p50 ratio=%.2f bound=%.2f n=%d taken_at=%s",
		reader.shape, reader.login, oneInboxOneRead, thisFigures, entries, len(replaced), baseFigures, asks, rows,
		strings.Join(each, "; "), ratio, oneInboxReadBound, len(ones), oneInboxNow())
	t.Logf("c-p99: read-p50 shape=%s this_ms=%.2f base_ms=%.2f", reader.key, msOf(thisP50), msOf(baseP50))
}

// requireOneInboxGates fails unless body, one answer of the one read, holds an entry of each of
// issues and carries a gate of at least one line on every entry of them.
func requireOneInboxGates(t *testing.T, reading int, body []byte, issues []string) {
	t.Helper()
	var read struct {
		Entries []struct {
			Ask *struct {
				Issue *struct {
					Key string `json:"key"`
				} `json:"issue"`
			} `json:"ask"`
			Thread *struct {
				Issue *struct {
					Key string `json:"key"`
				} `json:"issue"`
			} `json:"thread"`
			ApprovalGate []json.RawMessage `json:"approval_gate"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("reading %d: decode the one read for its gates: %v", reading, err)
	}
	held := map[string]int{}
	for _, entry := range read.Entries {
		key := ""
		switch {
		case entry.Ask != nil && entry.Ask.Issue != nil:
			key = entry.Ask.Issue.Key
		case entry.Thread != nil && entry.Thread.Issue != nil:
			key = entry.Thread.Issue.Key
		}
		if !slices.Contains(issues, key) {
			continue
		}
		if len(entry.ApprovalGate) == 0 {
			t.Fatalf("reading %d: the one read answered a row of %s with no approval gate", reading, key)
		}
		held[key]++
	}
	for _, issue := range issues {
		if held[issue] == 0 {
			t.Fatalf("reading %d: the one read answered no row of the gated issue %s", reading, issue)
		}
	}
}

// measureOneInboxWrite takes oneInboxReadings readings of path on each build, after one of each
// not counted, alternating which build writes first, and prints both builds' p50, p95 and p99 on
// one line with the path's bound in oneInboxWriteBounds. It fails where this build's p50 is above
// that bound times the base build's, but for a path that moves more than one ask a reading
// (putProjectDefaultAssignee's move of many asks), which the table does not name, and whose times
// it prints with the count it moved and holds to nothing (R11).
func measureOneInboxWrite(t *testing.T, path oneInboxPath, base, this *oneInboxSide) {
	t.Helper()
	sides := [2]*oneInboxSide{base, this}
	var taken [2][]time.Duration
	for reading := 0; reading <= oneInboxReadings; reading++ {
		order := [2]int{0, 1}
		if reading%2 == 1 {
			order = [2]int{1, 0}
		}
		for _, index := range order {
			took := path.run(t, sides[index], reading)
			if reading > 0 {
				taken[index] = append(taken[index], took)
			}
		}
	}
	var beside [2]string
	if path.beside != nil {
		for index, side := range sides {
			beside[index] = " " + path.beside(t, side)
		}
	}
	baseFigures, baseP50 := oneInboxPercentiles(taken[0])
	thisFigures, thisP50 := oneInboxPercentiles(taken[1])
	ratio := float64(thisP50) / float64(baseP50)
	bound := oneInboxWriteBounds[path.name]
	held := fmt.Sprintf("bound=%.2f", bound)
	if path.many {
		held = "bounds nothing: it moves more than one ask"
	}
	t.Logf("c-p99: write %s: this build %s%s | the base build %s%s | p50 ratio=%.2f %s n=%d taken_at=%s",
		path.name, thisFigures, beside[1], baseFigures, beside[0], ratio, held, len(taken[1]), oneInboxNow())
	if !path.many && ratio > bound {
		t.Errorf("the write %s has p50 %.2fms on this build, %.2f times the base build's %.2fms, above its bound %.2f (R11)",
			path.name, msOf(thisP50), ratio, msOf(baseP50), bound)
	}
}

// countOneInboxMoves makes each counted move of who an ask waits on (oneInboxCorpus.moves) through
// this build's handler, on a request whose statements the traced store records, and counts the
// tuple rows the move's transaction appended for each ask it moved - three for an ask of the first
// sort, four for one of the second - with every other row of that transaction printed apart as its
// fixed rows (installOneInboxAudit). Then, for each write, it holds the statements of its move of
// many to the most its moves of one ask ran: a count that grows with the asks moved fails.
func countOneInboxMoves(t *testing.T, here *oneInboxHere) {
	t.Helper()
	ctx := context.Background()
	database := here.side.database
	type ran struct {
		statements int
		many       bool
		what       string
		sql        []string
	}
	counts := map[string][]ran{}
	var writes []string
	for _, move := range here.side.corpus.moves {
		var before int64
		if err := database.Pool.QueryRow(ctx, `select coalesce(max(seq), 0) from graph_tuples`).Scan(&before); err != nil {
			t.Fatalf("read the store's last tuple before %s's move %q: %v", move.write, move.what, err)
		}
		here.statements.ownStatements()
		response := countedWrite(t, here.handler, move.method, move.path, move.body, oneInboxHeavy)
		if response.Code != http.StatusOK {
			t.Fatalf("%s's move %q: %d %s", move.write, move.what, response.Code, response.Body.String())
		}
		statements := quietOneInboxStatements(t, here)
		asks := make([]string, 0, len(move.asks))
		expected := map[string]int{}
		for _, ask := range move.asks {
			asks = append(asks, ask.id)
			expected[ask.id] = ask.rows
		}
		if moved := oneInboxAsksWaitingOn(t, database, `a.id = any($1::uuid[])`, asks, move.to); moved != len(asks) {
			t.Fatalf("%s's move %q left %d of its %d asks waiting on %s", move.write, move.what, moved, len(asks), move.to)
		}
		transactions := oneInboxStrings(t, database, `
			select distinct xmin::text from graph_tuples
			where seq > $1 and ((from_kind = $2 and from_id = any($3)) or (to_kind = $2 and to_id = any($3)))
		`, before, graphstore.KindAsk, asks)
		if len(transactions) != 1 {
			t.Fatalf("%s's move %q appended its asks' tuples in transactions %v, want one", move.write, move.what, transactions)
		}
		var xid int64
		if _, err := fmt.Sscan(transactions[0], &xid); err != nil {
			t.Fatalf("read transaction %q: %v", transactions[0], err)
		}
		rows, err := database.Pool.Query(ctx, `
			select ask, count(*)::int, string_agg(kind || case when asserted then '+' else '-' end, ',' order by seq)
			from (
				select seq, kind, asserted, case when from_kind = $3 then from_id else to_id end as ask
				from graph_tuples
				where seq > $1 and xmin::text::bigint = $2
				  and ((from_kind = $3 and from_id = any($4)) or (to_kind = $3 and to_id = any($4)))
			) moved
			group by ask
		`, before, xid, graphstore.KindAsk, asks)
		if err != nil {
			t.Fatalf("read the tuples %s's move %q appended: %v", move.write, move.what, err)
		}
		appended := map[string]int{}
		kinds := map[int]string{}
		var ask, appendedKinds string
		var count int
		if _, err := pgx.ForEachRow(rows, []any{&ask, &count, &appendedKinds}, func() error {
			appended[ask] = count
			kinds[expected[ask]] = appendedKinds
			return nil
		}); err != nil {
			t.Fatalf("read the tuples %s's move %q appended: %v", move.write, move.what, err)
		}
		// An ask's sort is the rows the seed has its move append, less two: three for the first sort,
		// four for the second.
		perAsk := map[string]int{}
		var tuples int
		for _, id := range asks {
			perAsk[fmt.Sprintf("sort %d at %d rows", expected[id]-2, appended[id])]++
			tuples += appended[id]
			if appended[id] != expected[id] {
				t.Errorf("row rule: %s's move %q appended %d tuple rows for ask %s (%s), want %d: at most four an ask, three where the person "+
					"it waited on also wrote in its thread", move.write, move.what, appended[id], id, kinds[expected[id]], expected[id])
			}
		}
		var fixed int64
		var parts []string
		for _, part := range oneInboxWrittenBy(t, database, xid) {
			written := part.rows
			if part.relation == "graph_tuples" && part.operation == "INSERT" {
				written -= int64(tuples)
			}
			if written > 0 {
				fixed += written
				parts = append(parts, fmt.Sprintf("%s.%s=%d", part.relation, strings.ToLower(part.operation), written))
			}
		}
		var tally []string
		for _, sort := range slices.Sorted(maps.Keys(perAsk)) {
			tally = append(tally, fmt.Sprintf("%dx %s", perAsk[sort], sort))
		}
		var shapes []string
		for _, rows := range slices.Sorted(maps.Keys(kinds)) {
			shapes = append(shapes, fmt.Sprintf("sort %d: %s", rows-2, kinds[rows]))
		}
		t.Logf("c-p99: move %s (%s): asks_moved=%d tuple rows per ask: %s [%s] fixed=%d (%s) statements=%d taken_at=%s",
			move.write, move.what, len(asks), strings.Join(tally, ", "), strings.Join(shapes, "; "), fixed, strings.Join(parts, " "),
			len(statements), oneInboxNow())
		if _, seen := counts[move.write]; !seen {
			writes = append(writes, move.write)
		}
		counts[move.write] = append(counts[move.write], ran{statements: len(statements), many: move.many, what: move.what, sql: statements})
	}
	for _, write := range writes {
		var one ran
		var many []ran
		for _, made := range counts[write] {
			if made.many {
				many = append(many, made)
			} else if made.statements > one.statements {
				one = made
			}
		}
		for _, made := range many {
			t.Logf("c-p99: statements of %s: %d moving %s against at most %d moving one ask (%s)", write, made.statements, made.what,
				one.statements, one.what)
			if made.statements > one.statements {
				t.Errorf("statement rule: %s ran %d statements moving %s, above the %d it ran moving one ask (%s): the statements grow "+
					"with the asks it moves\nmoving %s:\n%s\nmoving one ask:\n%s", write, made.statements, made.what, one.statements,
					one.what, made.what, strings.Join(made.sql, "\n"), strings.Join(one.sql, "\n"))
			}
		}
	}
}

// quietOneInboxStatements answers the statements a counted write ran once it has run none for a
// quarter of a second: work a write detached from its answer runs on a context derived from the
// request's, and is the write's own.
func quietOneInboxStatements(t *testing.T, here *oneInboxHere) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last, since := len(here.statements.recorder.Statements()), time.Now()
	for time.Since(since) < 250*time.Millisecond {
		if time.Now().After(deadline) {
			t.Fatal("a counted write's statements did not stop within 10s")
		}
		time.Sleep(25 * time.Millisecond)
		if recorded := len(here.statements.recorder.Statements()); recorded != last {
			last, since = recorded, time.Now()
		}
	}
	return here.statements.ownStatements()
}
