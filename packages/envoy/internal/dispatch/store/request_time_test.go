package store

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// requestTimings is the run of an access-log line's fields from status on: status, elapsed_ms and
// bytes next to each other, then the four timings requestTimer and the route roster's read add to.
var requestTimings = regexp.MustCompile(` status=\d+ elapsed_ms=\d+ bytes=\d+ db_ms=(\d+) db_calls=(\d+) db_wait_ms=(\d+) roster_wait_ms=(\d+)(?: |$)`)

// captureRequestLines sends the default logger's lines to a buffer for the test's lifetime, as
// cmd/dispatch's text handler writes them.
func captureRequestLines(t *testing.T) *logtest.Buffer {
	t.Helper()
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	return logged
}

// serveLogged serves one request to handle through an access-log line (accesslog.Handler).
func serveLogged(handle http.HandlerFunc) {
	accesslog.Handler("/r", handle).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/r", nil))
}

// requestTimingsOf is the four timings of the one access-log line logged holds, in the line's
// order: db_ms, db_calls, db_wait_ms and roster_wait_ms.
func requestTimingsOf(t *testing.T, logged *logtest.Buffer) [4]int64 {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, `msg="`+accesslog.Message+`"`) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want one access-log line, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	match := requestTimings.FindStringSubmatch(lines[0])
	if match == nil {
		t.Fatalf("access-log line %q does not carry status, elapsed_ms, bytes, db_ms, db_calls, db_wait_ms and roster_wait_ms in one run", lines[0])
	}
	var read [4]int64
	for index := range read {
		read[index], _ = strconv.ParseInt(match[index+1], 10, 64)
	}
	return read
}

// A request's line counts every statement the request ran on its own context, each from its send
// to its end: one sent alone, a query whose rows it read, one row read, a batch of three, a
// transaction's begin, statement and commit, and two reads side by side, eleven in all. The one
// sent alone and the batch each sleep 20 ms in the database, and the two side by side 50 ms each,
// so db_ms is at least 140 however fast the machine is: each of the two concurrent reads adds its
// own time. A statement on a context that carries no line, a background one or one detached from
// the line, counts nowhere, and a request that reads no roster has a roster_wait_ms of 0.
func TestARequestsLineCountsEveryStatementItRanAndTheirTime(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
	database := openEmptyTestStore(t)
	logged := captureRequestLines(t)
	serveLogged(func(_ http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		must := func(what string, err error) {
			if err != nil {
				t.Errorf("%s: %v", what, err)
			}
		}
		_, err := database.Pool.Exec(ctx, `select pg_sleep(0.02)`)
		must("sleep 20 ms", err)

		rows, err := database.Pool.Query(ctx, `select generate_series(1, 3)`)
		must("query three rows", err)
		if err == nil {
			_, err = pgx.CollectRows(rows, pgx.RowTo[int])
			must("read three rows", err)
		}

		var answer int
		must("read one row", database.Pool.QueryRow(ctx, `select $1::int + 1`, 41).Scan(&answer))

		batch := &pgx.Batch{}
		batch.Queue(`select 1`)
		batch.Queue(`select 2`)
		batch.Queue(`select pg_sleep(0.02)`)
		results := database.Pool.SendBatch(ctx, batch)
		for range batch.Len() {
			_, err := results.Exec()
			must("run a batch's query", err)
		}
		must("close the batch", results.Close())

		tx, err := database.Pool.Begin(ctx)
		must("begin", err)
		if err == nil {
			_, err = tx.Exec(ctx, `select 1`)
			must("run a statement in the transaction", err)
			must("commit", tx.Commit(ctx))
		}

		sleep := func(ctx context.Context) error {
			_, err := database.Pool.Exec(ctx, `select pg_sleep(0.05)`)
			return err
		}
		must("sleep 50 ms twice side by side", ReadSideBySide(ctx, sleep, sleep))

		_, err = database.Pool.Exec(context.Background(), `select 1`)
		must("run a statement on a background context", err)
		_, err = database.Pool.Exec(accesslog.Detached(ctx), `select 1`)
		must("run a statement on a detached context", err)
	})
	read := requestTimingsOf(t, logged)
	if dbMS := read[0]; dbMS < 140 {
		t.Errorf("db_ms = %d, want at least 140: two statements of 20 ms and two of 50 ms side by side", dbMS)
	}
	if dbCalls := read[1]; dbCalls != 11 {
		t.Errorf("db_calls = %d, want the 11 statements the request ran on its own context", dbCalls)
	}
	if rosterWaitMS := read[3]; rosterWaitMS != 0 {
		t.Errorf("roster_wait_ms = %d, want 0 for a request that read no roster", rosterWaitMS)
	}
}

// requestAcquires is a pool tracer that sends on its channel when an acquire on a request's
// context begins. Installed after the store's own tracer, it sends once requestTimer has started
// timing that wait.
type requestAcquires chan<- struct{}

func (requestAcquires) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (requestAcquires) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (acquires requestAcquires) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	if accesslog.TimingOf(ctx) != nil {
		select {
		case acquires <- struct{}{}:
		default:
		}
	}
	return ctx
}

func (requestAcquires) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {}

// A request's line counts its wait for a pooled connection as db_wait_ms. On a pool of one
// connection that another caller holds, the request waits until that caller gives it back, which
// it does 30 ms after the request began to wait, as a tracer of the test's own, run after the
// store's, reports; so db_wait_ms is at least 30, and the request's one statement is its db_calls.
// The other caller's acquire, on a context that carries no line, counts nowhere.
func TestARequestsLineCountsItsWaitForAPooledConnection(t *testing.T) {
	// Not parallel: it swaps the default logger (logtest.SetDefault).
	const held = 30 * time.Millisecond
	opened := openEmptyTestStore(t)
	config := opened.Pool.Config()
	config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
	waiting := make(chan struct{}, 1)
	config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, requestAcquires(waiting))
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool of one connection: %v", err)
	}
	t.Cleanup(pool.Close)
	database := &Store{Pool: NewPool(pool)}
	holder, err := database.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold the pool's one connection: %v", err)
	}
	// Given back before the pool's close, which waits for every connection in use, should the
	// test stop before it gives it back itself.
	giveBack := sync.OnceFunc(holder.Release)
	t.Cleanup(giveBack)
	logged := captureRequestLines(t)

	served := make(chan struct{})
	go func() {
		defer close(served)
		serveLogged(func(_ http.ResponseWriter, r *http.Request) {
			if _, err := database.Pool.Exec(r.Context(), `select 1`); err != nil {
				t.Errorf("run the request's statement: %v", err)
			}
		})
	}()
	testwait.Receive[struct{}](t, "the request to begin waiting for the pool's one connection", waiting)
	testwait.Pause(held)
	giveBack()
	testwait.Receive[struct{}](t, "the request to be served", served)

	read := requestTimingsOf(t, logged)
	if dbWaitMS := read[2]; dbWaitMS < held.Milliseconds() {
		t.Errorf("db_wait_ms = %d, want at least the %d ms the other caller held the pool's one connection", dbWaitMS, held.Milliseconds())
	}
	if dbCalls := read[1]; dbCalls != 1 {
		t.Errorf("db_calls = %d, want the request's one statement", dbCalls)
	}
}
