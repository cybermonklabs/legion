package api

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheStoreReportNamesEveryBackfillNotRun: the store a private project's reads would answer
// wrongly is one whose project-owned rows lack their nodes, or one that either backfill the switch
// to private waits for has not finished: backfill-session-projects (session_projects_filled) and
// scrub-access-events (access_events_scrubbed). The boot's and the hourly report name each, with
// the command that repairs it, as the switch's 409 PRIVATE_PROJECTS_INCOMPLETE does, and a
// complete store is no report at all. A row or node the project census names is printed as
// graphcensus.ProjectComplete answers it, with its own cure, and the report adds no cure of its
// own: an orphaned issue node no build removes names no in-band cure, never the backfill.
func TestTheStoreReportNamesEveryBackfillNotRun(t *testing.T) {
	t.Parallel()
	alice := model.Actor{Kind: "user", ID: "alice"}
	for _, row := range []struct {
		name, seed string
		// nodes are written after the seed, each with no row behind it.
		nodes []graphstore.Node
		want  []string
		// refuse is text the report must not hold.
		refuse []string
	}{
		{"a complete store", `
			insert into session_projects_filled default values;
			insert into access_events_scrubbed default values;`, nil, nil, nil},
		{"session_projects_filled empty", `
			insert into access_events_scrubbed default values;`, nil,
			[]string{"session_projects_filled holds no row", "envoy-dispatch backfill-session-projects"}, nil},
		{"access_events_scrubbed empty", `
			insert into session_projects_filled default values;`, nil,
			[]string{"access_events_scrubbed holds no row", "envoy-dispatch scrub-access-events"}, nil},
		{"an issue with no node", `
			insert into session_projects_filled default values;
			insert into access_events_scrubbed default values;
			insert into projects (key, name, default_assignee) values ('PRIV', 'Private', 'alice');
			insert into issues (key, project_key, number, title, created_by, rank)
			values ('PRIV-1', 'PRIV', 1, 'One', '{"kind":"user","id":"alice"}', 'U');`, nil,
			[]string{"issue PRIV-1: missing; run envoy-dispatch backfill-graph-store"},
			[]string{"writes their nodes"}},
		{"an issue node no row stands behind", `
			insert into session_projects_filled default values;
			insert into access_events_scrubbed default values;`,
			[]graphstore.Node{graphstore.IssueNode("PRIV-9", "PRIV", alice, time.Now().UTC())},
			[]string{"issue PRIV-9: orphaned; no in-band cure: a delete outside the builds made it"},
			[]string{"backfill-graph-store"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			if _, err := database.Pool.Exec(ctx, row.seed); err != nil {
				t.Fatalf("seed %s: %v", row.name, err)
			}
			if len(row.nodes) > 0 {
				if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
					return graphstore.Write(ctx, tx, row.nodes, nil)
				}); err != nil {
					t.Fatalf("%s: write the nodes no row stands behind: %v", row.name, err)
				}
			}
			err := storeIncomplete(store.WithTransactionTracking(ctx), database.Pool)
			if row.want == nil {
				if err != nil {
					t.Fatalf("%s: the report = %v, want none", row.name, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: no report, want one naming %q", row.name, row.want)
			}
			for _, want := range row.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: the report %q does not name %q", row.name, err, want)
				}
			}
			for _, refused := range row.refuse {
				if strings.Contains(err.Error(), refused) {
					t.Errorf("%s: the report %q holds %q, a cure no line of it names", row.name, err, refused)
				}
			}
		})
	}
}

// maintenanceTestInterval is the pace both runners keep here, far shorter than the server's
// minute so a handful of periods passes inside one test.
const maintenanceTestInterval = 300 * time.Millisecond

// keySweep is the statement a pass of the key sweep is recognised by (sweepRequestKeys).
const keySweep = "delete from request_keys"

// startOfPeriodAfterNext is the start of the period after the one running now, as duty.Exclusive
// buckets a tick's due time (periodBoundary: Unix nanoseconds less their remainder).
func startOfPeriodAfterNext(interval time.Duration) time.Time {
	ns := time.Now().UnixNano()
	return time.Unix(0, ns-ns%interval.Nanoseconds()).Add(interval)
}

// Two envoy-dispatch processes run the maintenance sweep on one database, each on a pool of its
// own (storetest.SecondProcess, as a second process has). The keys past their window and the
// deliveries a dead process owed need one pass per period across the two, not one per process:
// each pass records its period under the lease (duty.RunLeased), so the other process's tick
// inside a period already served makes no statement of its own.
func TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	recorders := make([]*storetest.StatementRecorder, 0, 2)
	swept := func() int {
		return countStatements(recorders[0].Statements(), keySweep) +
			countStatements(recorders[1].Statements(), keySweep)
	}
	// The resume is the last step of a pass, run after the key sweep's delete has returned and
	// still under the lease, so no pass of the other runner is in flight while it runs: there the
	// key sweeps that ever started are exactly the passes that ran, this one included. The
	// recorder sees a delete as it starts, so a count taken anywhere else could include a pass
	// still in its round trip.
	var resumes atomic.Int64
	var unpaired atomic.Pointer[string]
	resume := func(context.Context) error {
		pass := resumes.Add(1)
		if sweeps := swept(); int64(sweeps) != pass {
			message := fmt.Sprintf("pass %d resumed deliveries after %d key sweeps; every pass runs both", pass, sweeps)
			unpaired.CompareAndSwap(nil, &message)
		}
		return nil
	}
	runners := make([]func(context.Context), 0, 2)
	for _, process := range []*store.Store{database, second} {
		recorder := &storetest.StatementRecorder{}
		pool := storetest.Traced(t, process.Pool, recorder, nil)
		leases := sweepLeases(t, &store.Store{Pool: pool})
		recorders = append(recorders, recorder)
		runners = append(runners, func(ctx context.Context) {
			RunMaintenanceSweep(ctx, pool, maintenanceTestInterval, leases, resume, nil, nil, nil)
		})
	}

	// Both runners start a quarter of a period past a boundary, so every tick of both falls well
	// inside one bucket rather than straddling two: this row is about the two processes' ticks
	// inside one period, not about which bucket a tick landed in.
	started := startOfPeriodAfterNext(maintenanceTestInterval).Add(maintenanceTestInterval / 4)
	time.Sleep(time.Until(started))
	for _, run := range runners {
		runSweep(t, run)
	}

	const passes = 6
	waitUntil(t, 30*time.Second, "six completed passes across the two runners", func() bool { return resumes.Load() >= passes })
	if elapsed, least := time.Since(started), (passes-1)*maintenanceTestInterval; elapsed < least {
		t.Fatalf("%d passes over %v across the two runners; one pass per period of %v takes at least %v: both processes are sweeping every period",
			resumes.Load(), elapsed, maintenanceTestInterval, least)
	}
	if message := unpaired.Load(); message != nil {
		t.Fatal(*message)
	}
}
