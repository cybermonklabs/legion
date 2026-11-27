package daemon

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A daemon that bootOverALiveTree boots publishes its boot event (the state_boot outbox row) and
// logs no ERROR line for it. The test waits until the state lane has handled the boot row, so it
// reads the log after the publish whether or not the lane runs before the boot's other work. A
// workflow daemon publishes its boot event through its Envoy listener, so a rig that gives the
// daemon no listener fails the publish: the row stays in the outbox with its error, and the lane
// logs "outbox row failed". TestABootOverARecordWhoseInstallationReadFailsAnswersItsState reads
// the same log before the lane has run in some runs, so it fails only when the lane runs first
// (CI's daemon-go job 136734654090 at 5e6f1091; 4 of 10 runs in the pod).
func TestABootOverALiveTreePublishesItsBootEventWithNoErrorLine(t *testing.T) {
	// Not parallel: it waits a fixed 30 s of real time for the boot event's outbox row to go (the deadline below), a span the load of tests running beside it could use up.
	var logged syncBuffer
	_, d, _ := bootOverALiveTree(t, "acme/widgets", &workflowTokenRecorder{}, repositoriesGitHub(t), slog.New(slog.NewJSONHandler(&logged, nil)))
	// A pool, as the daemon opens its store: the DSN a test is given may carry pgxpool's keys
	// (pool_max_conns), which a bare pgx.Connect sends to the server as settings it refuses.
	pool, err := pgxpool.New(context.Background(), d.cfg.PostgresDSN)
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(pool.Close)
	var (
		rows     int
		attempts int
		lastErr  string
	)
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := pool.QueryRow(context.Background(), `select count(*), coalesce(max(attempts), 0), coalesce(max(last_error), '')
			from outbox where kind = 'state_boot' and issue = $1`, d.cfg.Project).Scan(&rows, &attempts, &lastErr)
		if err != nil {
			t.Fatalf("read the boot event's outbox row: %v", err)
		}
		if rows == 0 || attempts > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rows != 0 {
		t.Fatalf("the boot event of %s was not published: its outbox row is still there after %d attempts, last error %q", d.cfg.Project, attempts, lastErr)
	}
	if failed := errorLines(logged.String()); len(failed) != 0 {
		t.Fatalf("ERROR lines = %q, want none once the boot event is published", failed)
	}
}
