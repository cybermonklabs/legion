package intake

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/consumernames"
)

// failingReads is a Pool whose first reads fail with err, then read the pool it wraps: a store
// whose cutover marker the boot cannot read yet.
type failingReads struct {
	Pool
	err   error
	fails int64
	reads atomic.Int64
}

func (f *failingReads) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.reads.Add(1) <= f.fails {
		return failedRow{f.err}
	}
	return f.Pool.QueryRow(ctx, sql, args...)
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

// On fixed names the boot reads the cutover marker before it opens anything, and only a marker it
// read as "done" refuses it. What says there is no marker opens the fixed names as main does: no
// table, and a read the database refuses (insufficient_privilege, 42501), since a login that may
// not read the table is a deployment that never ran the cutover on it. What says nothing either
// way — no connection, a timeout — is read again until it answers, and the boot never runs the
// fixed names on a marker it did not read: it ends on the read's error once its deadline passes.
func TestFixedNamesNeverStartOnACutoverMarkerTheyDidNotRead(t *testing.T) {
	unreachable := errors.New("failed to connect to `host=127.0.0.1 user=legion database=legion`: dial error (dial tcp 127.0.0.1:5432: connect: connection refused)")
	for _, tc := range []struct {
		name   string
		err    error
		fails  int64
		marker string
		want   string
	}{
		{name: "no table", err: &pgconn.PgError{Code: "42P01", Message: `relation "intake_consumers" does not exist`}, fails: 1 << 30},
		{name: "a refused read", err: &pgconn.PgError{Code: "42501", Message: "permission denied for table intake_consumers"}, fails: 1 << 30},
		{name: "a read that answers on its third try, a marker done", err: unreachable, fails: 2, marker: consumernames.CutoverDone, want: "consumer_names"},
		{name: "a read that answers on its third try, no marker", err: unreachable, fails: 2},
		{name: "a read that never answers", err: context.DeadlineExceeded, fails: 1 << 30, want: "read the cutover marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			if tc.marker != "" {
				cutover(t, pool, tc.marker)
			}
			reads := &failingReads{Pool: pool, err: tc.err, fails: tc.fails}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := refuseConvertedStore(ctx, reads)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refuseConvertedStore = %v, want the fixed names opened", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("refuseConvertedStore = %v, want a refusal naming %q", err, tc.want)
			}
			if tc.fails == 2 && reads.reads.Load() != 3 {
				t.Fatalf("the boot read the marker %d times, want the two failed reads and the one that answered", reads.reads.Load())
			}
			if tc.want == "read the cutover marker" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("refuseConvertedStore = %v, want the last read's error in it", err)
			}
		})
	}
}
