package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The lines the boot's run of the backfill logs, as a reader of the process's output matches them:
// the done line is the one lib-tree.sh's tree_backfill_end waits for.
const (
	bootBackfillDoneMatch    = `msg="dispatch: boot backfill done"`
	bootBackfillSkippedMatch = `msg="dispatch: boot backfill skipped"`
)

// bootBackfillOutcome is what one call of runBootBackfill did: what it answered its caller, whether
// it called the run, whether it called what follows a done line (R26's step at a boot), and every
// line it logged.
type bootBackfillOutcome struct {
	answer   error
	ran      bool
	followed bool
	lines    []string
}

// holding is every line of the outcome that holds part.
func (o bootBackfillOutcome) holding(part string) []string {
	var held []string
	for _, line := range o.lines {
		if strings.Contains(line, part) {
			held = append(held, line)
		}
	}
	return held
}

// callBootBackfill calls runBootBackfill with record and a run that answers ran, logging through
// the handler the process logs through, and answers what it did.
func callBootBackfill(t *testing.T, record func(context.Context) error, ran error) bootBackfillOutcome {
	t.Helper()
	var logged bytes.Buffer
	outcome := bootBackfillOutcome{}
	outcome.answer = runBootBackfill(context.Background(), slog.New(slog.NewTextHandler(&logged, nil)), record,
		func(context.Context) error {
			outcome.ran = true
			return ran
		}, func(context.Context) { outcome.followed = true })
	for line := range strings.Lines(logged.String()) {
		outcome.lines = append(outcome.lines, strings.TrimRight(line, "\n"))
	}
	t.Logf("logged:\n%s", logged.String())
	return outcome
}

// projectMovedCount is the count of project-moved rows the done line names, or "no count".
var projectMovedCount = regexp.MustCompile(` project_moved=(\d+)\b`)

// The boot's run of the backfill logs what each outcome of the backfill was and answers its caller
// nil every time, so the process serves on whatever the backfill answered. The record and the run
// are the test's own, in place of the record of the horizon and the passes.
//
// The record succeeds in the first four calls. A run with no error logs the done line. A run whose
// error joins two failures, each graphstore.ErrProjectMoved, as Backfill answers rows whose node the
// store holds in another project, logs the done line and the count 2 beside it. A run that fails
// otherwise logs its error and no done line, and so does a run that answers two project-moved errors
// joined again with another, the shape Backfill answers where a pass fails after project-moved rows
// (errors.Join(moved, err)), which errors.Is alone would read as project-moved. In the fifth call the
// record fails: the function logs that error, calls no run and logs no done line. In the sixth the
// record ends at its bound, as an error that wraps context.DeadlineExceeded and names the oldest
// writer of the event log, backend pid 4242, open 31 s: the function logs the skip line with that
// pid and that age, calls no run and logs no done line.
func TestTheBootBackfillRunLogsItsOutcome(t *testing.T) {
	moved := func(row int) error {
		return fmt.Errorf("write graph node issue/CORE-%d in project CORE, which the store holds in KEY: %w", row, graphstore.ErrProjectMoved)
	}
	other := errors.New("read the comments that name no node: connection reset by peer")
	recorded := func(context.Context) error { return nil }
	returned := func(outcome bootBackfillOutcome) {
		t.Helper()
		if outcome.answer != nil {
			t.Errorf("the boot backfill returned %v to its caller, which would stop the process", outcome.answer)
		}
		if done := len(outcome.holding(bootBackfillDoneMatch)) == 1; outcome.followed != done {
			t.Errorf("the boot backfill called what follows a done line %v, and logged a done line %v: the two go together", outcome.followed, done)
		}
	}

	succeeded := callBootBackfill(t, recorded, nil)
	returned(succeeded)
	if !succeeded.ran || len(succeeded.holding(bootBackfillDoneMatch)) != 1 {
		t.Errorf("the boot backfill logged no done line after a run that succeeded")
	}

	movedRows := callBootBackfill(t, recorded, errors.Join(moved(1), moved(2)))
	returned(movedRows)
	if done := movedRows.holding(bootBackfillDoneMatch); len(done) != 1 {
		t.Errorf("the boot backfill logged no done line after a run that left 2 project-moved rows")
	} else if count := projectMovedCount.FindStringSubmatch(done[0]); count == nil || count[1] != "2" {
		n := "no count"
		if count != nil {
			n = count[1]
		}
		t.Errorf("the boot backfill logged the count %s, want 2", n)
	}

	failed := callBootBackfill(t, recorded, other)
	returned(failed)
	if len(failed.holding(bootBackfillDoneMatch)) != 0 {
		t.Errorf("the boot backfill logged a done line after a run that failed")
	}
	if len(failed.holding(other.Error())) != 1 {
		t.Errorf("the boot backfill logged no line naming the error of a run that failed")
	}

	failedBeside := callBootBackfill(t, recorded, errors.Join(errors.Join(moved(1), moved(2)), other))
	returned(failedBeside)
	if len(failedBeside.holding(bootBackfillDoneMatch)) != 0 {
		t.Errorf("the boot backfill logged a done line after a run that failed beside 2 project-moved rows")
	}
	if len(failedBeside.holding(other.Error())) != 1 {
		t.Errorf("the boot backfill logged no line naming the error of a run that failed beside 2 project-moved rows")
	}

	refused := errors.New("record the event log's horizon the backfill began at: connection refused")
	unrecorded := callBootBackfill(t, func(context.Context) error { return refused }, nil)
	returned(unrecorded)
	if unrecorded.ran {
		t.Errorf("the boot backfill ran its passes after its record of the horizon failed")
	}
	if len(unrecorded.holding(bootBackfillDoneMatch)) != 0 {
		t.Errorf("the boot backfill logged a done line after its record of the horizon failed")
	}
	if len(unrecorded.holding(refused.Error())) != 1 {
		t.Errorf("the boot backfill logged no line naming the error of its record of the horizon")
	}

	bound := fmt.Errorf("record the event log's horizon the backfill began at: %w", &graphcensus.HorizonBoundError{Pid: 4242, Age: 31 * time.Second})
	if !errors.Is(bound, context.DeadlineExceeded) {
		t.Fatalf("the record's error at its bound, %v, wraps no context.DeadlineExceeded, so this call proves nothing", bound)
	}
	skipped := callBootBackfill(t, func(context.Context) error { return bound }, nil)
	returned(skipped)
	if skip := skipped.holding(bootBackfillSkippedMatch); len(skip) != 1 || !strings.Contains(skip[0], " pid=4242 ") || !strings.Contains(skip[0], " age=31s") {
		t.Errorf("the boot backfill logged no skip after its record of the horizon reached its bound")
	}
	if skipped.ran {
		t.Errorf("the boot backfill ran its passes after its record of the horizon reached its bound")
	}
	if len(skipped.holding(bootBackfillDoneMatch)) != 0 {
		t.Errorf("the boot backfill logged a done line after its record of the horizon reached its bound")
	}
}

// A prepared transaction holds no backend, so the read of the oldest writer of the event log
// answers it with pid 0 and the time it was prepared (events.OldestEventWriter's second arm, as
// events.StallHolderSQL reports one). The skip line built from the record's error at its bound over
// that answer names the writer a prepared transaction, pid 0, and its age. No test database of this
// repository can hold a prepared transaction (its max_prepared_transactions is 0), so the answer is
// the test's own, turned into the record's error as the record turns it (NewHorizonBoundError).
func TestTheBootSkipLogsAPreparedWriterAsPidZero(t *testing.T) {
	now := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	prepared := now.Add(-47 * time.Second)
	bound := graphcensus.NewHorizonBoundError(0, prepared, now)
	if !errors.Is(bound, context.DeadlineExceeded) {
		t.Fatalf("the record's error at its bound, %v, wraps no context.DeadlineExceeded", bound)
	}
	skipped := callBootBackfill(t, func(context.Context) error { return bound }, nil)
	if skipped.answer != nil {
		t.Errorf("the boot backfill returned %v to its caller, which would stop the process", skipped.answer)
	}
	skip := skipped.holding(bootBackfillSkippedMatch)
	if len(skip) != 1 {
		t.Fatalf("the boot backfill logged no skip after its record of the horizon reached its bound over a prepared transaction")
	}
	for _, want := range []string{` writer="prepared transaction"`, " pid=0 ", " age=47s"} {
		if !strings.Contains(skip[0], want) {
			t.Errorf("the skip line over a prepared transaction prepared 47 s ago holds no %q: %s", want, skip[0])
		}
	}
	if skipped.ran || len(skipped.holding(bootBackfillDoneMatch)) != 0 {
		t.Errorf("the boot backfill ran its passes or logged a done line after its record of the horizon reached its bound")
	}
}

// bootBackfillSigningKey is the one cookie key every process of the readiness test takes, as every
// process of a deployment does, so no process waits out a live window of the one before it
// (processes.Register).
const bootBackfillSigningKey = "boot-backfill-key"

// No pod's readiness waits for the boot's backfill. A process booted with its passes held
// (DISPATCH_TEST_HOLD_BOOT_BACKFILL) is healthy, serves a write of a project and an issue, holds the
// horizon its boot recorded before the passes in graph_backfill_horizon, the row of this build's
// marking rule (refs.BlockIndexMarkingRule, the table's key), and logs no done line. A process
// booted without the hold over the same database logs the done line once its passes end.
//
// Then the bound of the record. With a transaction of the test's own holding an event insert open,
// a third process whose bound is 2 s (DISPATCH_TEST_BOOT_BACKFILL_HORIZON_WAIT) logs the skip,
// naming the test's backend pid, before its listening line, becomes healthy, and logs no done line.
// The test ends its transaction only once that process is healthy.
func TestTheBootBackfillHoldsNoPodsReadiness(t *testing.T) {
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	ctx := context.Background()
	var started time.Time
	if err := database.Pool.QueryRow(ctx, `select now()`).Scan(&started); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}
	stop := func(p *dispatchProcess) {
		t.Helper()
		p.Terminate(t)
		p.WaitExit(t, "SIGTERM")
	}

	held := startDispatchProcessWith(t, databaseURL, dispatchOptions{SigningKey: bootBackfillSigningKey,
		Env: map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_HOLD_BOOT_BACKFILL": "1"}})
	held.waitHealthy(t)
	held.call(t, http.MethodGet, "/healthz", "", http.StatusOK)
	key, _ := held.createIssue(t)
	var recorded bool
	if err := database.Pool.QueryRow(ctx, `
		select exists (select 1 from graph_backfill_horizon where marking_rule = $2 and recorded_at >= $1)
	`, started, refs.BlockIndexMarkingRule).Scan(&recorded); err != nil {
		t.Fatalf("read graph_backfill_horizon: %v", err)
	}
	if !recorded {
		t.Fatalf("the boot recorded no backfill horizon of rule 2 before its passes:\n%s", held.Output.String())
	}
	if done := held.logLines(bootBackfillDoneMatch); len(done) > 0 {
		t.Fatalf("the boot backfill ended while its passes were held: %s", done[0])
	}
	stop(held)

	unheld := startDispatchProcessWith(t, databaseURL, dispatchOptions{SigningKey: bootBackfillSigningKey,
		Env: map[string]string{"DISPATCH_TEST_HOOKS": "1"}})
	unheld.waitHealthy(t)
	for deadline := time.Now().Add(300 * time.Second); !unheld.logged(bootBackfillDoneMatch); time.Sleep(2 * time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("the boot backfill logged no done line within 300 s:\n%s", unheld.Output.String())
		}
	}
	t.Logf("the process without the hold: %s", unheld.logLines(bootBackfillDoneMatch)[0])
	stop(unheld)

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the test's own connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	writer, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the test's writer of the event log: %v", err)
	}
	t.Cleanup(func() { _ = writer.Rollback(context.Background()) })
	var pid int32
	if err := writer.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the writer's backend pid: %v", err)
	}
	if _, err := writer.Exec(ctx, `
		insert into events (issue_key, seq, type, actor, payload, notify, published_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1), 'issue.updated',
			'{"kind":"system","id":"boot-backfill-test"}', '{}'::jsonb, false, now())
	`, key); err != nil {
		t.Fatalf("insert an event of %s and hold it open: %v", key, err)
	}

	bounded := startDispatchProcessWith(t, databaseURL, dispatchOptions{SigningKey: bootBackfillSigningKey,
		Env: map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_BOOT_BACKFILL_HORIZON_WAIT": "2s"}})
	bounded.waitHealthy(t)
	output := bounded.Output.String()
	skip := strings.Index(output, bootBackfillSkippedMatch)
	if skip < 0 {
		t.Fatalf("the boot logged no skip of the backfill while a writer of the event log stayed open past the bound:\n%s", output)
	}
	line, _, _ := strings.Cut(output[skip:], "\n")
	if named := regexp.MustCompile(` pid=(\d+)\b`).FindStringSubmatch(line); named == nil || named[1] != strconv.Itoa(int(pid)) {
		got := "none"
		if named != nil {
			got = named[1]
		}
		t.Fatalf("the boot's skip names pid %s, want %d: %s", got, pid, line)
	}
	t.Logf("the process bounded at 2 s, beside the test's writer of the event log, backend pid %d: %s", pid, line)
	if listening := strings.Index(output, bounded.listeningLine()); listening < skip {
		t.Fatalf("the boot logged its skip of the backfill after its listening line, so the bound held its readiness:\n%s", output)
	}
	if done := bounded.logLines(bootBackfillDoneMatch); len(done) > 0 {
		t.Fatalf("the boot backfill ended while a writer of the event log stayed open: %s", done[0])
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatalf("end the test's writer of the event log: %v", err)
	}
}
