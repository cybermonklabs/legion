package main

// The two cost measurements of dispatch://INFRA-1149 share this file. c-write-cost is
// TestNoWritePathThisChildChangesGetsSlower (write_cost_test.go) and c-read-cost is
// TestNoReadThisChildChangesGetsSlowerAndNoEventOutrunsItsNode (read_cost_test.go). What they
// share is the pair of arms they compare, the corpus writer that seeds each arm through that
// arm's own API, the clock, the order statistic and the table every reading is printed in.
//
// # The two arms, and why each has a database of its own
//
// One arm is this working copy's build, the other the build of the commit this branch forked
// from (`git merge-base origin/main HEAD`, baseBuild). Each serves a database of its own and
// migrates it at boot, so each arm runs its own code over its own schema: the older build has
// no graph_nodes, no graph_tuples and no graph_store_cutover, and this build's graph_edges
// reads the tuple arm behind that scalar. One database cannot be both schemas at once, and an
// older build serving this schema would be measured reading this child's view, which is neither
// arm. The corpus is therefore written twice, by one seeding pass issuing the same requests to
// each arm, and both arms are measured in one run, reading by reading, so the pod's load falls
// on both alike.
//
// # The clock
//
// The time of record is one HTTP request, measured from the test's own goroutine: the body is
// encoded before the clock starts and the answer is read to its end before it stops, as
// api/inbox_dismissed_scale_test.go's timeDismissedScaleRequest does. It therefore carries one
// loopback round trip beyond the server's own time, which both arms pay alike. The server's own
// access log prints an elapsed_ms for each of these requests, which is the same span less the
// socket, but at a whole millisecond it is too coarse for the reads below.
//
// # What these numbers are
//
// They are this pod's, under gVisor, which runs syscall-heavy work 2.2 to 3.8 times slower than
// the same work outside it, against a Postgres shared with whatever else the pod runs. Every
// comparison here is between two arms measured in one run on one corpus, interleaved; no number
// here is comparable with a number from another run, and none is production latency.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

const (
	// costWriteRuns is how many readings the write measurement takes of each path on each arm,
	// as api/inbox_dismissed_scale_test.go's dismissedScaleRuns takes 20. A write path costs
	// seconds and its budget is a relative one with a wide margin, so the readings need only be
	// enough to show a path that moved.
	costWriteRuns = 20
	// costReadReadings is how many the read measurement takes, which is more, because a read
	// costs tens of milliseconds and p99 by nearest rank is the ceil(0.99 x n)th reading: at
	// twenty readings, and at fifty, that is the slowest of them, so one scheduling hiccup of
	// this pod decides the cell. At two hundred it is the 198th, which sits below the top.
	costReadReadings = 200
	// costWarmUp is the reading taken before those 20 and not counted: the first run of a
	// statement on a fresh connection pays for the pages every run after it finds cached, and
	// a measurement of a corpus is not a measurement of a cold cache (measureDismissedScale).
	costWarmUp = 1
	// costBudget is what c-write-cost and c-read-cost hold every path to: a path whose p99
	// rises by more than this much above the same path at the commit before this child fails.
	costBudget = 100 * time.Millisecond
	// costSigningKey is the one DISPATCH_SIGNING_KEY both processes of a run carry.
	costSigningKey = "graph-cost-signing-key"
	// costLogin is the person every request below is made as, which is the login
	// dispatchProcess.call sends (dispatchTestLogin).
	costLogin = dispatchTestLogin
	// costGVisorNote is printed by both measurements, because a reader of the table has to
	// know what the figures are of.
	costGVisorNote = "these are gVisor figures: this pod runs syscall-heavy work 2.2 to 3.8 times " +
		"slower than the same work outside it, on a Postgres shared with every other suite of the " +
		"pod, so compare the two arms of one run with each other and never a figure here with a " +
		"figure from another run"
)

// costArm is one build serving one database of this run's own.
type costArm struct {
	// name is how the arm is printed: "this child" or "the commit before this child".
	name string
	// head reports this working copy's build, the arm the budgets are taken on.
	head bool
	// server is the process, databaseURL its database and pool the test's own connections to
	// that database, which seed it, read its row counts and take the block-index lease.
	server      *dispatchProcess
	databaseURL string
	pool        *pgxpool.Pool
	// cutover is the state of graph_store_cutover this arm's readings are taken in, nil for an
	// arm read in whatever state it is in. Two arms differing only in it are two readings of
	// one server, which is how c-read-cost reads this child with the cutover rows off and with
	// them on.
	cutover *bool
}

// inCutover is this arm read in one state of graph_store_cutover: the same process and the same
// database under a name of its own, so a reading is kept apart from the other state's.
func (a *costArm) inCutover(complete bool) *costArm {
	in := *a
	in.cutover = &complete
	in.name = a.name + ", the cutover rows off"
	if complete {
		in.name = a.name + ", the cutover rows on"
	}
	return &in
}

// ready puts the arm in the state its readings are taken in. It is off the clock.
func (a *costArm) ready(t *testing.T) {
	t.Helper()
	if a.cutover != nil {
		costCutover(t, a, *a.cutover)
	}
}

// startCostArms starts both arms: this working copy's build and the build of the commit this
// branch forked from, each on an empty database of its own which that build migrates at boot,
// and both delivering to one stub Envoy listener, so a delivery costs the same transport on
// either arm. It answers them in the order they are measured in, this child first.
func startCostArms(t *testing.T, envoyURL string) (*costArm, *costArm) {
	t.Helper()
	older := baseBuild(t)
	head := startCostArm(t, "this child", true, dispatchOptions{
		SigningKey: costSigningKey,
		Env:        map[string]string{"ENVOY_URL": envoyURL},
	})
	base := startCostArm(t, "the commit before this child", false, dispatchOptions{
		Binary:     older,
		SigningKey: costSigningKey,
		Env:        map[string]string{"ENVOY_URL": envoyURL},
	})
	t.Logf("arm %q is this working copy's build, serving %s", head.name, costDatabaseName(head.databaseURL))
	t.Logf("arm %q is %s, serving %s", base.name, older, costDatabaseName(base.databaseURL))
	return head, base
}

// startCostArm starts one arm on an empty database of its own. The database is empty rather
// than storetest's clone of a migrated template, because each build has to run its own
// migrations on it: that is what makes the older arm the older schema.
func startCostArm(t *testing.T, name string, head bool, options dispatchOptions) *costArm {
	t.Helper()
	databaseURL := censusDatabase(t)
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open %s's own connections: %v", name, err)
	}
	t.Cleanup(pool.Close)
	server := startDispatchProcessWith(t, databaseURL, options)
	server.waitHealthy(t)
	return &costArm{name: name, head: head, server: server, databaseURL: databaseURL, pool: pool}
}

// costDatabaseName is the database a URL names, for the line that says which arm served which.
func costDatabaseName(databaseURL string) string {
	if cut := strings.LastIndex(databaseURL, "/"); cut >= 0 {
		return databaseURL[cut+1:]
	}
	return databaseURL
}

// costListener is a stub of the Envoy listener a comment delivery sends through: it answers the
// session registry read and takes the send. Both arms deliver through this one stub, so a
// delivery costs the same transport on either, and what the two readings differ by is what each
// build does around the send. Without it a delivery to a session no listener answers for spends
// about 12 s in the dial, which is a measurement of a timeout and not of this child.
//
// It answers a session for every id asked of it, advertising every delivery kind a comment
// mention can carry, so a delivery to a session this run names succeeds. While accepting is
// false it knows no session at all, which is how the seed leaves a mention's first attempt
// failed: the delivery each arm is measured on is then the first that reaches that session, and
// the one that writes its node on this child's arm.
type costListener struct {
	URL       string
	accepting atomic.Bool
}

// Accept opens or closes the stub to deliveries.
func (l *costListener) Accept(accepting bool) { l.accepting.Store(accepting) }

func costEnvoy(t *testing.T) *costListener {
	t.Helper()
	stub := &costListener{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !stub.accepting.Load():
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			session := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
			_, _ = fmt.Fprintf(w,
				`{"session_id":%q,"title":"the cost run's session","capabilities":["btw","aside","steer"]}`, session)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			_, _ = w.Write([]byte(`{"event_id":"cost-envelope","recipient":"cost"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	stub.URL = server.URL
	return stub
}

// costReading is one path on one arm: every reading it took, in the order they were taken.
type costReading struct {
	// operation is the path or read measured, arm the arm it was measured on.
	operation, arm string
	// shape is the corpus the readings were taken over, empty where the measurement has one.
	shape string
	runs  []time.Duration
}

// percentile is the nearest-rank percentile of this reading's runs: the ceil(q*n)th of them
// sorted, so p50 of 200 readings is the hundredth and p99 the 198th. No interpolation, so every
// number printed is a run that happened - the rule api/inbox_dismissed_scale_test.go's
// percentileOfDismissedScale states, which these two measurements follow.
func (r costReading) percentile(quantile float64) time.Duration {
	if len(r.runs) == 0 {
		return 0
	}
	sorted := slices.Clone(r.runs)
	slices.Sort(sorted)
	rank := int(math.Ceil(quantile * float64(len(sorted))))
	rank = max(rank, 1)
	rank = min(rank, len(sorted))
	return sorted[rank-1]
}

func (r costReading) p50() time.Duration { return r.percentile(0.50) }
func (r costReading) p99() time.Duration { return r.percentile(0.99) }

// printed is every reading of this path, in the order it was taken, in milliseconds: a swinging
// reading is visible here rather than hidden in the summary above it.
func (r costReading) printed() string {
	parts := make([]string, len(r.runs))
	for index, run := range r.runs {
		parts[index] = strconv.FormatFloat(costMilliseconds(run), 'f', 1, 64)
	}
	return strings.Join(parts, " ")
}

// costMilliseconds is one duration in milliseconds, to a tenth.
func costMilliseconds(taken time.Duration) float64 {
	return float64(taken.Microseconds()) / 1000
}

// costInterleaved measures one path on both arms in one pass, runs readings of each: each run
// takes one reading on each arm before the next run takes another, and which arm goes first
// alternates from run to run, so neither arm is systematically the one that runs while the pod
// is busiest and neither warms the pod for the other. costWarmUp readings are taken on each arm
// first and not counted: the first run of a statement on a fresh connection pays for the pages
// every run after it finds cached (measureDismissedScale).
func costInterleaved(
	t *testing.T, operation, shape string, runs int, arms []*costArm,
	take func(arm *costArm, run int) time.Duration,
) map[*costArm]costReading {
	t.Helper()
	readings := map[*costArm]costReading{}
	for _, arm := range arms {
		readings[arm] = costReading{operation: operation, arm: arm.name, shape: shape, runs: make([]time.Duration, 0, runs)}
		for warm := range costWarmUp {
			take(arm, -1-warm)
		}
	}
	for run := range runs {
		order := slices.Clone(arms)
		if run%2 == 1 {
			slices.Reverse(order)
		}
		for _, arm := range order {
			reading := readings[arm]
			reading.runs = append(reading.runs, take(arm, run))
			readings[arm] = reading
		}
	}
	return readings
}

// timed is one request on this arm, timed from the test's own goroutine: body is encoded by the
// caller before the clock starts, and the answer is read to its end before it stops. It fails
// the test on any status but want, because a measurement of a refusal is a measurement of
// nothing.
func (a *costArm) timed(t *testing.T, method, path, body string, want int) (time.Duration, httpAnswer) {
	t.Helper()
	start := time.Now()
	answer, err := a.server.trySend(method, path, body, asUser(costLogin))
	taken := time.Since(start)
	if err != nil {
		t.Fatalf("%s: %s %s: %v", a.name, method, path, err)
	}
	if answer.status != want {
		t.Fatalf("%s: %s %s: status %d, want %d: %s", a.name, method, path, answer.status, want, answer.body)
	}
	return taken, answer
}

// count is one count read out of this arm's database, for the row counts each corpus prints and
// for the owed reads the tick measurement polls.
func (a *costArm) count(t *testing.T, statement string, args ...any) int64 {
	t.Helper()
	var counted int64
	if err := a.pool.QueryRow(context.Background(), statement, args...).Scan(&counted); err != nil {
		t.Fatalf("%s: %s: %v", a.name, statement, err)
	}
	return counted
}

// costTableOf prints every reading of a measurement: the summary line of each path on each arm,
// then every reading it took. Both are printed for every path, because a reader has to see the
// spread a percentile came out of - and for the write paths, whose 20 readings make the p99
// their maximum, that spread is the whole of what a single swinging reading means.
func costTableOf(t *testing.T, readings []costReading) {
	t.Helper()
	t.Log("reading\tshape\tarm\tp50 ms\tp99 ms\truns")
	for _, reading := range readings {
		t.Logf("%s\t%s\t%s\t%.1f\t%.1f\t%d",
			reading.operation, costShapeName(reading.shape), reading.arm,
			costMilliseconds(reading.p50()), costMilliseconds(reading.p99()), len(reading.runs))
	}
	t.Log("every reading, in the order it was taken, in milliseconds:")
	for _, reading := range readings {
		t.Logf("%s\t%s\t%s\t%s", reading.operation, costShapeName(reading.shape), reading.arm, reading.printed())
	}
}

func costShapeName(shape string) string {
	if shape == "" {
		return "-"
	}
	return shape
}

// costBlockIndexDrained waits until the arm's block-index reconciliation has nothing owed, so
// that no tick of its own runs inside a measured window. It is the same read
// store_census_test.go's waitForTheBlockIndex takes, over the three queues a tick serves.
func costBlockIndexDrained(t *testing.T, arm *costArm, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		owed := arm.count(t, `select count(*) from document_block_versions where version is null`)
		if owed == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d document(s) were still owed a block index after %s:\n%s",
				arm.name, owed, within, arm.server.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// costLeaseNamespace is the first key every loop lease is taken under - duty's leaseNamespace,
// which stays unexported - the four bytes of "LOOP". The second key is the lease's own name,
// which Postgres hashes with hashtext. memory_harness_test.go states the same two for the same
// lease, behind the `memory` build tag, which these two measurements do not carry.
const costLeaseNamespace int32 = 0x4C4F4F50

// costLeaseWait bounds the wait for the block-index lease. A pass reads one batch of each queue
// it serves and gives the lease back, so this is a hang guard on a wait nothing healthy reaches.
const costLeaseWait = 5 * time.Minute

// costHoldBlockIndex takes the block-index pass's own lease on this arm and answers the call
// that gives it back, so no tick runs while a window is measured. It is memory_harness_test.go's
// holdBlockIndexLease over a costArm's pool: duty.Exclusive takes that lease before every pass
// and skips the pass where another session holds it.
func costHoldBlockIndex(t *testing.T, arm *costArm) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := arm.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("%s: take a connection for the block-index lease: %v", arm.name, err)
	}
	deadline := time.Now().Add(costLeaseWait)
	for {
		var held bool
		if err := conn.QueryRow(ctx, `select pg_try_advisory_lock($1, hashtext($2))`,
			costLeaseNamespace, docs.BlockIndexLease).Scan(&held); err != nil {
			conn.Release()
			t.Fatalf("%s: take the block-index lease: %v", arm.name, err)
		}
		if held {
			break
		}
		if time.Now().After(deadline) {
			conn.Release()
			t.Fatalf("%s: the block-index pass held the %q lease for more than %s:\n%s",
				arm.name, docs.BlockIndexLease, costLeaseWait, arm.server.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return func() {
		if err := duty.FreeSessionLock(conn, `select pg_advisory_unlock($1, hashtext($2))`,
			costLeaseNamespace, docs.BlockIndexLease); err != nil {
			t.Errorf("%s: give the block-index lease back: %v", arm.name, err)
		}
	}
}

// costCutover sets every row of graph_store_cutover on this child's arm, which is the state the
// backfill leaves (graphcensus.Backfill) and the state in which graph_edges answers from the
// tuple arm. The older arm has no such table and no such scalar, so this is a no-op there.
func costCutover(t *testing.T, arm *costArm, complete bool) {
	t.Helper()
	if !arm.head {
		return
	}
	if _, err := arm.pool.Exec(context.Background(),
		`update graph_store_cutover set complete = $1`, complete); err != nil {
		t.Fatalf("%s: set the cutover rows to %v: %v", arm.name, complete, err)
	}
}

// costRunBackfill runs `envoy-dispatch backfill-graph-store` on this child's arm and waits for
// the block index the backfill marks owed, so that the store holds a node and its tuples for
// every projection row the seed wrote and the cutover rows read true. It is what a deployment
// runs once the new binary serves (store_census_test.go's runBackfill).
func costRunBackfill(t *testing.T, arm *costArm) {
	t.Helper()
	if !arm.head {
		return
	}
	runBackfill(t, arm.databaseURL)
	costBlockIndexDrained(t, arm, 10*time.Minute)
	incomplete := arm.count(t, `select count(*) from graph_store_cutover where not complete`)
	if incomplete != 0 {
		t.Fatalf("%s: %d kind(s) of graph_store_cutover read false after the backfill, so no reading below is of the tuple arm",
			arm.name, incomplete)
	}
}

// analyze gives the planner statistics for what the corpus just wrote. A table written in one
// burst and read at once still reads reltuples = -1, which no deployment serves: autovacuum has
// analysed a live table long before a person reads it, and a plan chosen from no statistics is
// not the plan the budget is about.
func (a *costArm) analyze(t *testing.T) {
	t.Helper()
	if _, err := a.pool.Exec(context.Background(), "analyze"); err != nil {
		t.Fatalf("%s: analyze the corpus: %v", a.name, err)
	}
}
