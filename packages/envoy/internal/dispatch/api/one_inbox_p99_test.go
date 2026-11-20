//go:build basebuild

package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
)

// c-p99 (dispatch://INFRA-1152 spec R11, plan T16): neither the one read nor a write gets slower
// beyond R11's bounds, every figure taken in this one run on two builds over one corpus.
//
// It times two processes the same way: the build of the commit BASELINE_SHA names (buildBaseline),
// checked out at $TMPDIR/base-p99 and booted over $ISSUE_P99_DATABASE_URL, and this build, built
// from this checkout with the stamp the base build gets and booted over a database of the test's own
// the way the base build is booted (bootOneInboxThisBuild, startBuild: the person read from one
// trusted header, NATS off, a stand-in listener, and the Go runtime settings this process runs
// under). It boots a build of an earlier commit because what it holds this build to - the reads this
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
// Before any timing it runs `vacuum analyze` and then `explain (analyze)` on the one read of each
// reader below, and fails on a sequential scan of comments or messages, the two tables the corpus
// fills to scale; it prints the plan over asks and events, which bounds nothing, and sets no
// planner setting. Then, alternating between the two processes one operation at a time,
// oneInboxReadings readings of each after one not counted, each the round trip on loopback, taken by
// this test's client the same way against both:
//
//   - the reads, per shape (oneInboxReaders): this build's GET /api/v1/me/inbox?state=inbox, which
//     the dashboard reads, against the reads it replaces on the base build - GET /api/v1/inbox
//     ?assignee=me, GET /api/v1/me/replies?kind=reply,agent&state=inbox and GET
//     /api/v1/agents/{session_id}/messages for each conversation the reader started - taken one
//     after another and summed, which is the base build's p50 the read is held to: at most
//     oneInboxReadBound times it;
//   - the writes (oneInboxWritePaths), each held to at most oneInboxWriteBound times the base
//     build's p50 of the same path, but for putProjectDefaultAssignee, which moves more than one
//     ask a reading and is printed with the count it moved.
//
// Then, on an audit of each database (installOneInboxAudit): the rows one comment in the thread
// at the bound writes, held to two per participant beside its fixed rows; and, on this build, each
// counted move of who an ask waits on (countOneInboxMoves), held to three tuple rows for each ask of
// the first sort and four for each of the second, with its fixed rows printed apart, and to a count
// of statements that does not grow from its move of one ask to its move of many.
//
// Every figure is printed on a line of its own under the prefix c-p99:. The two p50 bounds, the
// scan rule, the row rules and the statement rule are what fail it; every p95 and p99 is a reading
// of the pod and fails nothing. It runs when LEGION_SCALE_TEST is 1, as a14 does: seeding two
// corpora and taking its readings costs minutes, which no ordinary run pays.

const (
	// oneInboxReadBound is R11's bound on the one read: its p50 at most twice the p50 of the
	// reads it replaces, taken in the same run (dispatch://INFRA-1098's R22).
	oneInboxReadBound = 2.0
	// oneInboxWriteBound is R11's bound on each write path: its p50 at most 1.25 times the base
	// build's p50 of the same path in the same run.
	oneInboxWriteBound = 1.25
	// oneInboxAgentToken is the agent bearer newTestServer's server trusts (sharedAgentTokens).
	oneInboxAgentToken = "agent-token"
)

// oneInboxReader is one shape the read is measured over: the person whose read it is, and the
// sessions of the conversations they started, one direct-reply read of the base build each.
type oneInboxReader struct {
	shape, login string
	sessions     []string
}

// oneInboxReaders are the corpus's shapes as reads: the median person, whose history holds the
// thread at the bound, which every ordinary person writes in; the person of the thread of one; the
// person of the reply chain at the walk's depth bound; the person whose history is a hundred times
// the median; and the person whose history is empty.
var oneInboxReaders = []oneInboxReader{
	{"the thread of 1,000 participants", oneInboxMedianReader, []string{oneInboxPersonSession(17)}},
	{"the thread of one", oneInboxSolo, nil},
	{"the reply chain at the walk's depth bound", oneInboxChain, nil},
	{"the history a hundred times the median", oneInboxHeavy, []string{oneInboxHeavySession}},
	{"the empty history", oneInboxEmpty, nil},
}

// oneInboxHere is this build as c-p99 drives it: its handler in the process, over a store traced
// for the statements a request marked countedRead runs (meInboxStatements), served on loopback,
// which seeds the corpus and makes the counted moves; and, once the scan rule has passed, its own
// process over the same database (bootOneInboxThisBuild), which every timed reading reads.
type oneInboxHere struct {
	side       *oneInboxSide
	handler    http.Handler
	traced     *store.Store
	statements *meInboxStatements
}

func TestOneInboxP99HoldsTheReadsAndTheWrites(t *testing.T) {
	if os.Getenv(dismissedScaleEnv) != "1" {
		t.Skipf("set %s=1 to take c-p99's measurement", dismissedScaleEnv)
	}
	ctx := context.Background()
	databaseURL := baselineDatabaseURL(t, oneInboxDatabaseEnv, oneInboxDatabase)

	here := bootOneInboxHere(t)
	this := here.side
	this.corpus = seedOneInboxCorpus(t, this.database, this.server)
	backfillOneInbox(t, this.database)
	requireOneInboxHistoryInTheStore(t, this.database)
	analyzeOneInbox(t, this)
	// The scan rule, before anything is timed: a read whose plan scans a table the corpus fills to
	// scale is no read a timing of it would say anything about.
	explainOneInboxRead(t, this.database)
	this.server = bootOneInboxThisBuild(t, this.database)
	settleOneInbox(t, this, oneInboxOwedWorkHere)
	analyzeOneInbox(t, this)

	binary, commit := buildBaseline(t, filepath.Join(os.TempDir(), oneInboxWorktree))
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

	this.server.trees, base.server.trees = true, true
	t.Logf("c-p99: the base build %s against this build at %s, each a process of its own, %d readings of each operation and shape "+
		"after one not counted, alternating between the two one operation at a time, each the round trip on loopback; taken_at=%s",
		commit, gitOutput(t, ".", "rev-parse", "HEAD"), oneInboxReadings, oneInboxNow())
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

// bootOneInboxHere builds this build's server in the process over a database of the test's own: a
// person read from the header the base build is told to trust too, on the untraced pool so a
// sign-in is no statement of a counted write; the stand-in listener the base build delivers to;
// and this checkout's commit as its build commit, as every server a two-build check boots carries
// its own. Its side's server is the handler served on loopback until bootOneInboxThisBuild.
func bootOneInboxHere(t *testing.T) *oneInboxHere {
	t.Helper()
	database := storetest.Open(t)
	statements := &meInboxStatements{}
	traced := &store.Store{Pool: storetest.Traced(t, database.Pool, statements, nil)}
	handler, _, _ := newTestServer(t, testServerOptions{
		store:       traced,
		identity:    headerIdentity(database),
		envoyURL:    standInListener(t),
		buildCommit: gitOutput(t, ".", "rev-parse", "HEAD"),
	})
	return &oneInboxHere{
		side:    &oneInboxSide{name: "this build", server: serveOnLoopback(t, handler, oneInboxAgentToken), database: database},
		handler: handler, traced: traced, statements: statements,
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

// explainOneInboxRead runs `explain (analyze, buffers)` on the statement of the one read of each
// reader, as the handler builds it for the dashboard's request (meInboxStatement: the inbox state,
// display trees, the listener's stored roster) at the event log's horizon, in a transaction rolled
// back. It prints each plan's times and its scans of asks and events, which bound nothing, and
// fails, once every plan is printed, on a sequential scan of comments or messages in any of them,
// printing the plan of each read that makes one.
func explainOneInboxRead(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	var horizon int64
	if err := database.Pool.QueryRow(ctx, `select coalesce(max(id), 0) from events`).Scan(&horizon); err != nil {
		t.Fatalf("read the event log's horizon: %v", err)
	}
	var scans []string
	var plans []string
	for _, reader := range oneInboxReaders {
		statement, args, err := meInboxStatement(model.Actor{Kind: "user", ID: model.CanonicalLogin(reader.login)}, horizon,
			meInboxRequest{state: myRepliesStateInbox}, true, true, nil)
		if err != nil {
			t.Fatalf("build the one read of %s: %v", reader.shape, err)
		}
		raw := explainOneInbox(t, database, "json", statement, args)
		var answered []struct {
			Plan      json.RawMessage `json:"Plan"`
			Planning  float64         `json:"Planning Time"`
			Execution float64         `json:"Execution Time"`
		}
		if err := json.Unmarshal([]byte(raw), &answered); err != nil || len(answered) != 1 {
			t.Fatalf("decode the plan of the one read of %s: %v\n%s", reader.shape, err, raw)
		}
		plan := answered[0].Plan
		over := map[string]map[string]float64{"asks": {}, "events": {}}
		for _, node := range explaintest.Nodes(t, plan) {
			if read, held := over[node.Relation]; held {
				read[node.String()] += cmp.Or(node.Loops, 1)
			}
		}
		t.Logf("c-p99: plan of the one read of %s (%s): planning=%.2fms execution=%.2fms; over asks: %s; over events: %s",
			reader.shape, reader.login, answered[0].Planning, answered[0].Execution,
			oneInboxScansOver(over["asks"]), oneInboxScansOver(over["events"]))
		scanned := false
		for _, relation := range []string{"comments", "messages"} {
			if explaintest.SeqScansRelation(t, plan, relation) {
				scans = append(scans, fmt.Sprintf("the one read of %s (%s) scans %s sequentially", reader.shape, reader.login, relation))
				scanned = true
			}
		}
		if scanned {
			plans = append(plans, fmt.Sprintf("the plan of the one read of %s:\n%s", reader.shape,
				explainOneInbox(t, database, "text", statement, args)))
		}
	}
	if len(scans) > 0 {
		t.Fatalf("scan rule: %s, a table the corpus fills to scale; no timing is taken over a read that scans one\n%s",
			strings.Join(scans, "; "), strings.Join(plans, "\n"))
	}
}

// explainOneInbox answers `explain (analyze, buffers, format <format>)` of statement, run in a
// transaction rolled back.
func explainOneInbox(t *testing.T, database *store.Store, format, statement string, args []any) string {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the explain: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, "explain (analyze, buffers, format "+format+") "+statement, args...)
	if err != nil {
		t.Fatalf("explain the one read: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain the one read: %v", err)
	}
	return strings.Join(lines, "\n")
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
// build's reads apart and what each build answered. It fails where the one read's p50 is above
// oneInboxReadBound times the base build's.
func measureOneInboxRead(t *testing.T, reader oneInboxReader, base, this *oneInboxSide) {
	t.Helper()
	const one = "/api/v1/me/inbox?state=inbox"
	replaced := []string{"/api/v1/inbox?assignee=me", "/api/v1/me/replies?kind=reply,agent&state=inbox"}
	for _, session := range reader.sessions {
		replaced = append(replaced, "/api/v1/agents/"+session+"/messages")
	}
	var ones, bases []time.Duration
	parts := make([][]time.Duration, len(replaced))
	var entries, asks, rows int
	// The reading not counted keeps its answer, to say what each build answered; every counted one
	// drops it as it arrives (baselineServer.read).
	get := func(side *oneInboxSide, reading int, path string) apiAnswer {
		if reading == 0 {
			return oneInboxExpect(t, side.name+": "+path+" of "+reader.shape,
				side.server.call(t, reader.login, http.MethodGet, path, nil), http.StatusOK)
		}
		return oneInboxExpect(t, side.name+": "+path+" of "+reader.shape, side.server.read(t, reader.login, path), http.StatusOK)
	}
	readOne := func(reading int) {
		answer := get(this, reading, one)
		if reading == 0 {
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
		reader.shape, reader.login, one, thisFigures, entries, len(replaced), baseFigures, asks, rows,
		strings.Join(each, "; "), ratio, oneInboxReadBound, len(ones), oneInboxNow())
	if ratio > oneInboxReadBound {
		t.Errorf("the one read of %s has p50 %.2fms, %.2f times the base build's %.2fms for the reads it replaces, above R11's %.2f",
			reader.shape, msOf(thisP50), ratio, msOf(baseP50), oneInboxReadBound)
	}
}

// measureOneInboxWrite takes oneInboxReadings readings of path on each build, after one of each
// not counted, alternating which build writes first, and prints both builds' p50, p95 and p99 on
// one line. It fails where this build's p50 is above oneInboxWriteBound times the base build's, but
// for a path that moves more than one ask a reading, whose times it prints with the count it moved
// and holds to nothing (R11).
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
	bound := fmt.Sprintf("bound=%.2f", oneInboxWriteBound)
	if path.many {
		bound = "bounds nothing: it moves more than one ask"
	}
	t.Logf("c-p99: write %s: this build %s%s | the base build %s%s | p50 ratio=%.2f %s n=%d taken_at=%s",
		path.name, thisFigures, beside[1], baseFigures, beside[0], ratio, bound, len(taken[1]), oneInboxNow())
	if !path.many && ratio > oneInboxWriteBound {
		t.Errorf("the write %s has p50 %.2fms on this build, %.2f times the base build's %.2fms, above R11's %.2f",
			path.name, msOf(thisP50), ratio, msOf(baseP50), oneInboxWriteBound)
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
		here.statements.ownStatements(t, here.traced)
		response := countedOneInboxWrite(t, here.handler, oneInboxHeavy, move.method, move.path, move.body)
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

// countedOneInboxWrite sends body to path as login through this build's handler, on a request
// marked countedRead, so the traced store records the statements it runs.
func countedOneInboxWrite(t *testing.T, handler http.Handler, login, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode %s %s: %v", method, path, err)
	}
	request := httptest.NewRequestWithContext(context.WithValue(context.Background(), countedRead{}, true), method, path,
		bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(baselineIdentityHeader, login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
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
	return here.statements.ownStatements(t, here.traced)
}
