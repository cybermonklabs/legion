package consumernames

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// A retired candidate whose create may still be in flight stays in the record until it can no
// longer land: Cleanup drops a retired name the server does not hold only once a read finds it
// absent the create's own bound (Config.CreateBound, recorded in the operation) after it was
// abandoned. The reviewer's reproduction: a registration's Ensure reserves a candidate and sends its
// create, which the server holds in flight past the settle age; a second process's Cleanup settles
// the open create (Step 0 abandons the candidate) and reads the candidate absent; then the create
// lands, the first process's publish loses its compare-and-set, and its Ensure goes on to a fresh
// name. The landed consumer is still named in the record's retired names, and the next Cleanup
// deletes it. Before, the first Cleanup dropped the candidate, and the landed consumer was named by
// no record and deleted by no cleanup: an orphan holding a slot of the stream. The control is a
// candidate whose create never lands: a Cleanup within the bound keeps it, and one past the bound
// drops it.
func TestARetiredCandidateWhoseCreateIsInFlightStaysNamed(t *testing.T) {
	const key = "ses-1"
	ctx := context.Background()
	t.Run("the create lands after the Cleanup", func(t *testing.T) {
		h := newHarness()
		sent, land := make(chan string, 1), make(chan struct{})
		var held string
		create := func(ctx context.Context, name string, start Start) (*jetstream.ConsumerInfo, error) {
			if held == "" {
				held = name
				sent <- name
				<-land
			}
			return h.server.create(key)(ctx, name, start)
		}
		ensured := make(chan error, 1)
		var sel Selection
		go func() {
			var err error
			sel, _, err = h.names("p1").Ensure(ctx, key, CreateRequest{Create: create, First: first(1)})
			ensured <- err
		}()
		inFlight := <-sent
		h.clock.advance(3 * time.Second)
		if _, err := h.names("p2").Cleanup(ctx, key); err != nil {
			t.Fatalf("the Cleanup over the create in flight = %v", err)
		}
		close(land)
		if err := <-ensured; err != nil {
			t.Fatalf("the first process's Ensure = %v", err)
		}
		if sel.Record.Current == inFlight || !h.server.holds(inFlight) {
			t.Fatalf("setup: the Ensure selected %s and the server holds %s: %t; want a fresh current, and %s landed",
				sel.Record.Current, inFlight, h.server.holds(inFlight), inFlight)
		}
		if rec := h.store.record(t, key); !slices.Contains(rec.Retired, inFlight) {
			t.Fatalf("the record once %s landed = %+v; want %s among its retired names: the server holds it and no record names it", inFlight, rec, inFlight)
		}
		if _, err := h.names("p3").Cleanup(ctx, key); err != nil {
			t.Fatalf("the next Cleanup = %v", err)
		}
		if rec := h.store.record(t, key); h.server.holds(inFlight) || slices.Contains(rec.Retired, inFlight) {
			t.Fatalf("after the next Cleanup the server holds %s: %t, the record %+v; want it deleted and dropped", inFlight, h.server.holds(inFlight), rec)
		}
	})
	t.Run("control: a create that never lands", func(t *testing.T) {
		h := newHarness()
		reservedAt := h.clock.Now().Add(-time.Hour)
		h.store.put(key, Record{Key: key, Start: 1, Op: &Op{Kind: OpCreate, Owner: "p1", Candidate: "session~X", CreateWithin: testCreateBound, ReservedAt: reservedAt}})
		if _, err := h.names("p2").Cleanup(ctx, key); err != nil {
			t.Fatal(err)
		}
		if rec := h.store.record(t, key); rec.Op != nil || !slices.Contains(rec.Retired, "session~X") {
			t.Fatalf("the record after a Cleanup within the create's bound = %+v; want session~X retired and kept", rec)
		}
		h.clock.advance(testCreateBound + testSettleAge + time.Second)
		if _, err := h.names("p3").Cleanup(ctx, key); err != nil {
			t.Fatal(err)
		}
		if rec := h.store.record(t, key); slices.Contains(rec.Retired, "session~X") || len(rec.Abandoned) != 0 {
			t.Fatalf("the record after a Cleanup past the create's bound = %+v; want session~X dropped", rec)
		}
	})
}
