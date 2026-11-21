package intake

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/consumernames"
)

// The five helpers through which the race regressions of durable_names_race_test.go and
// durable_names_forced_order_test.go reach the daemon's naming. Each is written once more, in
// main's terms, in the copy of this file the run at main uses (the report of this change names it):
// there the daemon has no fresh names, so freshSpec is main's spec, cutover and the two
// interruptions write nothing, and the current name is the fixed one. A race regression is then the
// same scenario on main's code, and it fails there.

// freshSpec is spec on fresh names, logging JSON to logs as `legion start`'s daemon does.
func freshSpec(logs *lockedBuffer) ConsumerSpec {
	spec := consumerSpec(logs)
	spec.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	spec.Names = consumernames.ModeFresh
	return spec
}

// cutover writes the daemon database's sentinel with marker, as the operator's convert writes
// "done" and its restore "rolled_back".
func cutover(t testing.TB, pool *pgxpool.Pool, marker string) {
	t.Helper()
	if _, err := consumernames.NewPGStore(pool).WriteSentinel(context.Background(), consumernames.StoreState{Cutover: marker}, 0); err != nil {
		t.Fatalf("write the sentinel %q: %v", marker, err)
	}
}

// currentName is the physical name logical's record selects.
func currentName(t testing.TB, pool *pgxpool.Pool, logical string) string {
	t.Helper()
	current := selectionOf(t, pool, logical).Current
	if current == "" {
		t.Fatalf("the selection record of %s selects no consumer", logical)
	}
	return current
}

// interruptedFirstCreate leaves logical as a daemon killed in its first create leaves it: the
// create of a candidate reserved a minute ago at the stream's end, boundary, whose create never
// reached the server.
func interruptedFirstCreate(t testing.TB, pool *pgxpool.Pool, logical string, boundary uint64) {
	t.Helper()
	putRecord(t, pool, logical, consumernames.Record{Start: boundary, Op: &consumernames.Op{
		Kind: consumernames.OpCreate, Owner: "a daemon killed in its first create", Candidate: consumernames.FreshName(logical),
		Native: consumernames.NativeNew, ReservedAt: time.Now().Add(-time.Minute),
	}})
}

// selectedAndLost leaves logical's record selecting a consumer the server does not hold, with no
// saved start and no last floor: nothing in it proves where a consumer made again may start.
func selectedAndLost(t testing.TB, pool *pgxpool.Pool, logical string) {
	t.Helper()
	putRecord(t, pool, logical, consumernames.Record{Current: consumernames.FreshName(logical)})
}
