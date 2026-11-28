package embedqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// coverageReadRuns is how many times TestTheCoverageReadsOfMainAndTheHead explains each side.
const coverageReadRuns = 3

// coverageReadBuffers is what one run of a coverage statement under `explain (analyze, buffers,
// format json)` read: the shared blocks its execution hit in the cache and read into it, from the
// plan's top node, whose counts hold every node under it, and its planning and execution times.
type coverageReadBuffers struct {
	hit, read           int64
	planning, execution float64
}

// buffers is every shared block the run touched, hit or read.
func (b coverageReadBuffers) buffers() int64 { return b.hit + b.read }

// String is the run as the test's lines print it.
func (b coverageReadBuffers) String() string {
	return fmt.Sprintf("shared hit %d read %d, %d buffers; planning %.3f ms, execution %.3f ms", b.hit, b.read, b.buffers(), b.planning, b.execution)
}

// coverageReadSide is one side of the comparison: its name in the lines, the statement, and what
// its transaction sets before it.
type coverageReadSide struct {
	name      string
	statement string
	plan      func(context.Context, pgx.Tx) error
}

// TestTheCoverageReadsOfMainAndTheHead measures the shared buffers the coverage route's read reads
// against those main's route statement read, on the seeded measurement database
// (cmd/dispatch-seed) that DISPATCH_BENCH_DATABASE_URL names, gated as TestStatementTimings is
// (benchtime.Open): CI leaves it unset, so CI runs no measurement. It fills the counts first
// (FillCoverage), since the route's read answers nothing until a fill has ended; a database a
// fill has filled already is recounted, which the fill is. Then, on benchtime's one connection,
// it explains each side three times in turn, main's first, each run in a read-only transaction it
// rolls back: main's route statement (mainRouteCoverageStatement) under the settings main's
// planCoverage ran it under (mainRouteCoverageSettings), and the head's read
// (coverageReadStatement) under planCoverage's, both for the seeded embedder's identity. Neither
// runs under the route's 10 s bound, so main's statement is read to its end whatever it takes.
//
// It prints one line that starts `coverage-reads:` for each run of each side, with its shared hit
// and read blocks, their sum and its times, and last a line that starts `coverage-reads: pass` or
// `coverage-reads: FAIL`. It fails when the head's read reads as many buffers as main's statement
// or more in any run, each run of one side held to the same run of the other.
func TestTheCoverageReadsOfMainAndTheHead(t *testing.T) {
	t.Parallel()
	databaseURL := os.Getenv(benchtime.DatabaseURLEnv)
	runner := benchtime.Open(t, databaseURL)
	identity := runner.Subjects().EmbeddingModel
	ctx := context.Background()
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the measurement database: %v", err)
	}
	t.Cleanup(database.Pool.Close)
	started := time.Now()
	fillCounts(t, database)
	fmt.Printf("coverage-reads: the fill took %s; both sides read for the identity %q\n", inMilliseconds(time.Since(started)), identity)

	sides := []coverageReadSide{
		{"mainRouteCoverageStatement", mainRouteCoverageStatement, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, mainRouteCoverageSettings)
			return err
		}},
		{"coverageReadStatement", coverageReadStatement, planCoverage},
	}
	var failures []string
	runs := make([][]coverageReadBuffers, len(sides))
	for run := 1; run <= coverageReadRuns; run++ {
		for i, side := range sides {
			read := explainCoverageBuffers(t, runner.Conn(), side, identity)
			runs[i] = append(runs[i], read)
			fmt.Printf("coverage-reads: run %d of %d, %s: %s\n", run, coverageReadRuns, side.name, read)
		}
		if main, head := runs[0][run-1], runs[1][run-1]; head.buffers() >= main.buffers() {
			failures = append(failures, fmt.Sprintf("in run %d the head's read read %d buffers, main's statement %d", run, head.buffers(), main.buffers()))
		}
	}
	verdict := "pass"
	if len(failures) > 0 {
		verdict = "FAIL: " + strings.Join(failures, "; ")
	}
	summary := make([]string, len(sides))
	for i, side := range sides {
		buffers := make([]string, len(runs[i]))
		for j, read := range runs[i] {
			buffers[j] = fmt.Sprint(read.buffers())
		}
		summary[i] = side.name + " " + strings.Join(buffers, ", ")
	}
	fmt.Printf("coverage-reads: %s: shared buffers hit and read in runs 1 to %d: %s\n", verdict, coverageReadRuns, strings.Join(summary, "; "))
	for _, failure := range failures {
		t.Error(failure)
	}
}

// explainCoverageBuffers runs side's statement for identity once under `explain (analyze,
// buffers, format json)`, in a read-only transaction on conn that side.plan has set and that it
// rolls back, and answers what the run read.
func explainCoverageBuffers(t *testing.T, conn *pgx.Conn, side coverageReadSide, identity string) coverageReadBuffers {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("%s: begin the explain: %v", side.name, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := side.plan(ctx, tx); err != nil {
		t.Fatalf("%s: set the statement's settings: %v", side.name, err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+side.statement, identity).Scan(&raw); err != nil {
		t.Fatalf("%s: explain the statement: %v", side.name, err)
	}
	var plans []struct {
		Plan struct {
			SharedHitBlocks  int64 `json:"Shared Hit Blocks"`
			SharedReadBlocks int64 `json:"Shared Read Blocks"`
		} `json:"Plan"`
		PlanningTime  float64 `json:"Planning Time"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("%s: read the explain's answer as one plan (%v):\n%s", side.name, err, raw)
	}
	plan := plans[0]
	return coverageReadBuffers{
		hit: plan.Plan.SharedHitBlocks, read: plan.Plan.SharedReadBlocks,
		planning: plan.PlanningTime, execution: plan.ExecutionTime,
	}
}
