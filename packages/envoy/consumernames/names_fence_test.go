package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// testEpoch is when the rows of this file reserved an operation they write by hand.
var testEpoch = time.Unix(1_700_000_000, 0)

// A record names only its own family's consumers: its current, each retired name and an open
// operation's candidate are <prefix>~<random> names of its key's prefix, and an operation's old name
// one of those or the key's fixed name. Any other name, another family's consumer (a daemon's
// durable, the listener's durable for a session key, a session's for a durable key), is refused as a
// malformed record, and the protocol neither reads, creates, deletes nor binds it. The control for
// each row is today's reader, the store's strict decode, which takes the record, since every name
// in it is a name a consumer can carry; the rows that name the family's own names are served.
func TestARecordNamesOnlyItsOwnFamilysConsumers(t *testing.T) {
	const key = "ses-1"
	for _, tc := range []struct {
		name   string
		prefix string
		record Record
		served bool
	}{
		{name: "a current of the daemon's durable", prefix: "session", record: Record{Current: "legion-go-p-dispatch~AAAAAAAAAAAAAAAAAAAAAA", Start: 3}},
		{name: "a current of the listener's durable", prefix: "session", record: Record{Current: "listener-m~AAAAAAAAAAAAAAAAAAAAAA", Start: 3}},
		{name: "a current that is the fixed name", prefix: "session", record: Record{Current: "s-ses-1", Start: 3}},
		{name: "a current of the prefix with no suffix", prefix: "session", record: Record{Current: "session~", Start: 3}},
		{name: "a current of the prefix and more", prefix: "session", record: Record{Current: "session-x~AAAA", Start: 3}},
		{name: "a retired name of the daemon's durable", prefix: "session", record: Record{Current: "session~A", Start: 3, Retired: []string{"legion-go-p-dispatch"}}},
		{name: "a candidate of another family", prefix: "session", record: Record{Start: 3, Op: &Op{Kind: OpCreate, Candidate: "delivery-jobs~B", ReservedAt: testEpoch}}},
		{name: "an old name of another family", prefix: "session", record: Record{Current: "session~A", Start: 3, Op: &Op{Kind: OpDelete, Old: "delivery-events", ReservedAt: testEpoch}}},
		{name: "a session's current under a durable's prefix", prefix: "listener-m", record: Record{Current: "session~A", Start: 3}},
		{name: "control: a current of the family", prefix: "session", record: Record{Current: "session~A", Start: 3}, served: true},
		{name: "control: an old name that is the key's fixed name", prefix: "session", record: Record{Current: "session~A", Start: 3, Op: &Op{Kind: OpDelete, Old: "s-ses-1", ReservedAt: testEpoch}}, served: true},
		{name: "control: a durable's own current", prefix: "listener-m", record: Record{Current: "listener-m~A", Start: 3}, served: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			record := tc.record
			record.Key = key
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeRecord(key, data); err != nil {
				t.Fatalf("control: today's reader refused %s: %v", data, err)
			}
			for _, name := range []string{"session~A", "listener-m~A"} {
				if _, err := h.server.create(key)(context.Background(), name, Start{Seq: 3}); err != nil {
					t.Fatal(err)
				}
			}
			h.store.put(key, record)
			names := h.names("p1")
			names.cfg.Prefix = func(string) string { return tc.prefix }
			names.cfg.Fixed = func(logical string) string { return "s-" + logical }
			creates := len(h.server.creates)
			// Each call is bounded: a record taken as served can leave a call waiting on an
			// operation the row's clock never lets settle.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err = names.Select(ctx, key, h.server.create(key))
			if tc.served {
				if err != nil {
					t.Fatalf("Select of %+v = %v, want it served", record, err)
				}
				return
			}
			if !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("Select of %+v = %v, want ErrMalformedRecord", record, err)
			}
			if _, err := names.Cleanup(ctx, key); !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("Cleanup of %+v = %v, want ErrMalformedRecord", record, err)
			}
			if _, err := names.Delete(ctx, key, nil); !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("Delete of %+v = %v, want ErrMalformedRecord", record, err)
			}
			if len(h.server.creates) != creates || len(h.server.deletes) != 0 {
				t.Fatalf("the refused record had the server create %v and delete %v", h.server.creates[creates:], h.server.deletes)
			}
		})
	}
}

// A record takes a consumer it names only when that consumer's own metadata names the record's
// key: a session~ consumer of ses-2 named by ses-1's record, as its current, a retired name, an
// open candidate or a listed current, is refused as a malformed record, and the protocol never
// adopts, deletes or hands it out for ses-1. The control is the same record naming ses-1's own
// consumer, which is served.
func TestARecordTakesOnlyAConsumerWhoseMetadataNamesItsKey(t *testing.T) {
	const key, other = "ses-1", "ses-2"
	for _, tc := range []struct {
		name string
		// owner is the key the named consumer's metadata names.
		owner  string
		record Record
		call   func(ctx context.Context, names *Names, h *harness) error
	}{
		{name: "a current", owner: other, record: Record{Current: "session~X", Start: 3}, call: selectCall},
		{name: "a current on Ensure", owner: other, record: Record{Current: "session~X", Start: 3}, call: func(ctx context.Context, names *Names, h *harness) error {
			_, _, err := names.Ensure(ctx, key, CreateRequest{Create: h.server.create(key), First: first(1)})
			return err
		}},
		{name: "a candidate Step 0 would adopt", owner: other, record: Record{Start: 3, Op: &Op{Kind: OpCreate, Candidate: "session~X", ReservedAt: testEpoch}}, call: selectCall},
		{name: "a retired name", owner: other, record: Record{Retired: []string{"session~X"}}, call: func(ctx context.Context, names *Names, _ *harness) error {
			_, err := names.Cleanup(ctx, key)
			return err
		}},
		{name: "a listed current on Delete", owner: other, record: Record{Current: "session~X", Start: 3}, call: func(ctx context.Context, names *Names, h *harness) error {
			listed, err := h.server.Info(ctx, "session~X")
			if err != nil {
				return err
			}
			_, err = names.Delete(ctx, key, listed)
			return err
		}},
		{name: "control: its own current", owner: key, record: Record{Current: "session~X", Start: 3}, call: selectCall},
		{name: "control: its own retired name", owner: key, record: Record{Retired: []string{"session~X"}}, call: func(ctx context.Context, names *Names, _ *harness) error {
			_, err := names.Cleanup(ctx, key)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			if _, err := h.server.create(tc.owner)(context.Background(), "session~X", Start{Seq: 3}); err != nil {
				t.Fatal(err)
			}
			record := tc.record
			record.Key = key
			h.store.put(key, record)
			names := h.names("p1")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			creates := len(h.server.creates)
			err := tc.call(ctx, names, h)
			if tc.owner == key {
				if err != nil {
					t.Fatalf("the call over its own consumer = %v, want it served", err)
				}
				return
			}
			if !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("the call over %s's consumer = %v, want ErrMalformedRecord", tc.owner, err)
			}
			if !h.server.holds("session~X") || len(h.server.deletes) != 0 || len(h.server.creates) != creates {
				t.Fatalf("the refused call touched the server: held %t, deletes %v, creates %v", h.server.holds("session~X"), h.server.deletes, h.server.creates[creates:])
			}
			if got := h.store.record(t, key); got.Current != tc.record.Current {
				t.Fatalf("the refused call published %q over the record's current %q", got.Current, tc.record.Current)
			}
		})
	}
}

// selectCall is Select of ses-1, which reads the record's current.
func selectCall(ctx context.Context, names *Names, h *harness) error {
	_, err := names.Select(ctx, "ses-1", h.server.create("ses-1"))
	return err
}

// Step 0 adopts a candidate the server holds only when it is as the operation reserved it: a start
// by sequence no later than the saved start (a clamp to an earlier sequence can only deliver
// again), and what the family's own check requires (Config.Adopt). Any other is refused as
// ErrUnexpectedConsumer naming it, left on the server and never published. A family check that
// cannot read the candidate refuses nothing: the call fails with the read's own error, the
// operation left open, where today's adoption called that read a refused candidate. The controls
// are a candidate at the saved start and one clamped earlier, each adopted.
func TestStep0AdoptsOnlyACandidateAsItsOperationReservedIt(t *testing.T) {
	const key = "ses-1"
	errRead := errors.New("the family's read of the candidate timed out")
	for _, tc := range []struct {
		name    string
		created Start
		family  error
		adopted bool
	}{
		{name: "a start past the saved start", created: Start{Seq: 20}},
		{name: "a native deliver new for a create by sequence", created: Start{Native: NativeNew}},
		{name: "a setting the family's check refuses", created: Start{Seq: 9}, family: fmt.Errorf("%w: session~N has ack policy all", ErrUnexpectedConsumer)},
		{name: "a read of the family's check that fails", created: Start{Seq: 9}, family: errRead},
		{name: "control: the saved start", created: Start{Seq: 9}, adopted: true},
		{name: "control: a start clamped earlier", created: Start{Seq: 5}, adopted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			if _, err := h.server.create(key)(context.Background(), "session~N", tc.created); err != nil {
				t.Fatal(err)
			}
			h.store.put(key, Record{Key: key, Start: 9, Op: &Op{Kind: OpCreate, Owner: "p1", Candidate: "session~N", ReservedAt: h.clock.Now()}})
			names := h.names("p2")
			names.cfg.Adopt = func(context.Context, string, *jetstream.ConsumerInfo) error { return tc.family }
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			sel, err := names.Select(ctx, key, h.server.create(key))
			switch {
			case tc.adopted:
				if err != nil || sel.Record.Current != "session~N" {
					t.Fatalf("Step 0 = %q, %v; want session~N adopted", sel.Record.Current, err)
				}
				return
			case tc.family == errRead:
				if !errors.Is(err, errRead) || errors.Is(err, ErrUnexpectedConsumer) {
					t.Fatalf("Step 0 over a failed read = %v, want the read's error and no refused candidate", err)
				}
			case !errors.Is(err, ErrUnexpectedConsumer) || !strings.Contains(err.Error(), "session~N"):
				t.Fatalf("Step 0 = %v, want ErrUnexpectedConsumer naming session~N", err)
			}
			if record := h.store.record(t, key); record.Current != "" || record.Op == nil || !h.server.holds("session~N") {
				t.Fatalf("the candidate was published or removed: record %+v, held %t", record, h.server.holds("session~N"))
			}
		})
	}
}
