package main

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
)

// The listener's durable creator names as the family table does: its Config's prefix, fixed name
// and key rule are the table's for a durable's key (consumernames.DurableNaming), the prefix and
// fixed name both the durable's own name, so its fence is the one consumernames.CheckFamily applies
// for that key, which gives the answer each round-6 case pins. Its key rule takes its own durable's
// key and refuses the DurableKey of a daemon's or Dispatch's durable, which are no listener's. The
// controls, each run with a fault and seen failing: a creator whose prefix is not the table's fails
// the naming row, and so does a creator whose key rule is the session's (CheckKey), on its own key,
// or any durable's (CheckDurableKey), on another family's durable.
func TestTheDurableCreatorNamesAsTheFamilyTable(t *testing.T) {
	r := newDurableRig(t, consumernames.ModeFresh)
	names := durableNames(r.client, r.durable)
	key := consumernames.DurableKey(r.durable)
	creator, table := names.Naming(), consumernames.DurableNaming()
	if got, want := creator.Prefix(key), table.Prefix(key); got != want || got != r.durable {
		t.Errorf("the durable creator's prefix of %s is %q, the table's %q; want both %q", key, got, want, r.durable)
	}
	if got, want := creator.Fixed(key), table.Fixed(key); got != want || got != r.durable {
		t.Errorf("the durable creator's fixed name of %s is %q, the table's %q; want both the durable's own %q", key, got, want, r.durable)
	}
	if creator.CheckKey == nil {
		t.Errorf("the durable creator's key rule is nil; want the table's")
	} else {
		foreign := []string{consumernames.DurableKey("legion-go-q-wake"), consumernames.DurableKey(bus.DispatchDeliveryDurable)}
		for _, k := range append([]string{key, r.durable, "durable.a=zz", "ses-A", ""}, foreign...) {
			got, want := creator.CheckKey(k), table.CheckKey(k)
			if (got == nil) != (want == nil) || errors.Is(got, consumernames.ErrInvalidKey) != errors.Is(want, consumernames.ErrInvalidKey) {
				t.Errorf("the durable creator's key rule of %q is %v, the table's %v", k, got, want)
			}
		}
		if err := creator.CheckKey(key); err != nil {
			t.Errorf("the durable creator's key rule refuses its own durable's key %q: %v", key, err)
		}
		for _, k := range foreign {
			if err := creator.CheckKey(k); !errors.Is(err, consumernames.ErrInvalidKey) {
				t.Errorf("the durable creator's key rule of %q, another family's durable, is %v; want ErrInvalidKey", k, err)
			}
		}
	}
	reserved := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		name   string
		record consumernames.Record
		served bool
	}{
		{"its own current", consumernames.Record{Current: r.durable + "~A", Start: 3}, true},
		{"an open replace of its own name", consumernames.Record{Start: 3, Op: &consumernames.Op{Kind: consumernames.OpReplace, Old: r.durable, Candidate: r.durable + "~B", ReservedAt: reserved}}, true},
		{"a current of the session prefix", consumernames.Record{Current: "session~A", Start: 3}, false},
		{"another durable's old name", consumernames.Record{Start: 3, Op: &consumernames.Op{Kind: consumernames.OpReplace, Old: "listener-other", Candidate: r.durable + "~B", ReservedAt: reserved}}, false},
	} {
		if err := consumernames.CheckFamily(consumernames.SessionFamily, key, c.record); (err == nil) != c.served {
			t.Errorf("%s: CheckFamily for the durable's key = %v; want served %t", c.name, err, c.served)
		}
	}
	// Its owner check is OwnsFamily's for its key: one implementation, alike on its own consumer, its
	// fixed one, another durable's and an unmarked one.
	consumer := func(name string, metadata map[string]string) *jetstream.ConsumerInfo {
		return &jetstream.ConsumerInfo{Name: name, Config: jetstream.ConsumerConfig{Durable: name, Metadata: metadata}}
	}
	for _, c := range []struct {
		name  string
		info  *jetstream.ConsumerInfo
		owned bool
	}{
		{"its own consumer", consumer(r.durable+"~A", durableMetadata(r.durable)), true},
		{"its fixed consumer, unmarked", consumer(r.durable, nil), true},
		{"another durable's consumer", consumer(r.durable+"~A", durableMetadata("listener-other")), false},
		{"an unmarked consumer", consumer(r.durable+"~A", nil), false},
	} {
		own, family := names.Owns(key, c.info), consumernames.OwnsFamily(consumernames.SessionFamily, key, c.info)
		if (own == nil) != c.owned || (family == nil) != c.owned {
			t.Errorf("%s: the durable creator's Owns = %v, OwnsFamily = %v; want both owned %t", c.name, own, family, c.owned)
		}
	}
}
