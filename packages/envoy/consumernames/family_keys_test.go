package consumernames

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The table takes only its family's keys: Dispatch's store only Dispatch's two durables, a
// daemon's store only legion-go-<project>-<kind> of its own project, and the session store a key
// CheckKey passes or the DurableKey of a listener's own durable (listener-<machine id>). So
// FamilyNaming, CheckFamily and OwnsFamily refuse any other key as ErrInvalidKey, naming it, before
// they judge a record or a consumer: the fence and the owner check judge by the key's own prefix and
// fixed name, so a record of daemon/p keyed by the listener's durable, an open replace of that
// durable, passed both, and so did the durable itself; and a session-store key DurableKey made of a
// daemon's or Dispatch's durable did the same for that durable. The control is each family's own
// keys, which pass. Before, FamilyNaming took any key for Dispatch's and the daemons' stores and any
// key without DurableKey's prefix for the session store, and an empty Dispatch key got an empty
// prefix; at ef39a215be the session store still took DurableKey of any durable.
func TestTheFamilyTableTakesOnlyItsFamilysKeys(t *testing.T) {
	consumer := func(name, logical string) *jetstream.ConsumerInfo {
		return &jetstream.ConsumerInfo{Name: name, Config: jetstream.ConsumerConfig{Durable: name, Metadata: map[string]string{LogicalMetadataKey: logical}}}
	}
	// replace is an open replace of old by a fresh name of prefix.
	replace := func(old, prefix string) Record {
		return Record{Start: 3, Op: &Op{Kind: OpReplace, Old: old, Candidate: prefix + "~C", ReservedAt: testEpoch}}
	}
	for _, c := range []struct{ name, family, logical string }{
		{"the listener's durable in a daemon's store", "daemon/p", "listener-m"},
		{"another project's durable in a daemon's store", "daemon/p", "legion-go-q-wake"},
		{"a durable of a project that ends in the project's name", "daemon/p", "legion-go-xp-wake"},
		{"a durable of no daemon kind", "daemon/p", "legion-go-p-other"},
		{"a session's id in a daemon's store", "daemon/p", "ses-A"},
		{"Dispatch's durable in a daemon's store", "daemon/p", "delivery-events"},
		{"no key in Dispatch's store", DispatchFamily, ""},
		{"the listener's durable in Dispatch's store", DispatchFamily, "listener-m"},
		{"a daemon's durable in Dispatch's store", DispatchFamily, "legion-go-p-wake"},
		{"no key in the session store", SessionFamily, ""},
		{"a key holding a '.' in the session store", SessionFamily, "ses.A"},
		{"a daemon's durable under DurableKey in the session store", SessionFamily, DurableKey("legion-go-q-wake")},
		{"Dispatch's durable under DurableKey in the session store", SessionFamily, DurableKey("delivery-events")},
		{"a durable of the listener prefix alone under DurableKey in the session store", SessionFamily, DurableKey("listener-")},
	} {
		t.Run(c.name, func(t *testing.T) {
			named := fmt.Sprintf("%q", c.logical)
			refused := func(call string, err error) {
				t.Helper()
				if !errors.Is(err, ErrInvalidKey) || !strings.Contains(err.Error(), named) {
					t.Errorf("%s(%s, %s) = %v; want ErrInvalidKey naming %s", call, c.family, named, err, named)
				}
			}
			_, err := FamilyNaming(c.family, c.logical)
			refused("FamilyNaming", err)
			refused("CheckFamily", CheckFamily(c.family, c.logical, replace(c.logical, c.logical)))
			refused("OwnsFamily", OwnsFamily(c.family, c.logical, consumer(c.logical, c.logical)))
		})
	}
	for _, c := range []struct{ family, logical string }{
		{DispatchFamily, "delivery-events"},
		{DispatchFamily, "delivery-jobs"},
		{"daemon/p", "legion-go-p-dispatch"},
		{"daemon/p", "legion-go-p-github"},
		{"daemon/p", "legion-go-p-wake"},
		{"daemon/p", "legion-go-p-ready"},
		{"daemon/a-b", "legion-go-a-b-wake"},
		{SessionFamily, "ses-A"},
		{SessionFamily, DurableKey("listener-m")},
		{SessionFamily, DurableKey("listener-m a")},
	} {
		naming, err := FamilyNaming(c.family, c.logical)
		if err != nil {
			t.Errorf("control: FamilyNaming(%s, %s) = %v", c.family, c.logical, err)
			continue
		}
		fixed := naming.Fixed(c.logical)
		if err := CheckFamily(c.family, c.logical, replace(fixed, naming.Prefix(c.logical))); err != nil {
			t.Errorf("control: CheckFamily(%s, %s) of an open replace of its fixed name = %v", c.family, c.logical, err)
		}
		if err := OwnsFamily(c.family, c.logical, consumer(fixed, "")); err != nil {
			t.Errorf("control: OwnsFamily(%s, %s) of its fixed consumer = %v", c.family, c.logical, err)
		}
	}
}

// A reader's Names built from the table deletes no consumer of a key outside its family. A store
// holds a planted record keyed by another family's durable: an open replace of that durable, its
// fixed name, with a saved start, while the server holds the durable. In daemon/p's store the key is
// listener-m, the listener's durable; in the session store it is DurableKey of q's daemon durable or
// of Dispatch's. A reader that builds its Names from FamilyNaming of the planted key is refused the
// key; one that built its Names for its own key has the family's key rule in it, so its Cleanup of
// the planted key is ErrInvalidKey. Neither sends a delete, and the server still holds the durable.
// The control is the same reader's Cleanup of its own key over the same record shape, which deletes
// the fixed name as Step 0 does. Before, each reader's Cleanup deleted the other family's live
// durable: the listener's in daemon/p's store until 97925b0d3d, and q's or Dispatch's in the session
// store until the session store's durable keys were held to the listener's.
func TestAReaderOfTheTableDeletesNoConsumerOfAKeyOutsideItsFamily(t *testing.T) {
	ctx := context.Background()
	reader := func(h *harness, naming Naming) *Names {
		cfg := naming.names().cfg
		cfg.Store, cfg.Server, cfg.Owner, cfg.Now, cfg.CreateBound = h.store, h.server, "reader", h.clock.Now, testCreateBound
		cfg.Floor = func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream }
		cfg.StreamID = func(context.Context) (string, error) { return "stream-1", nil }
		return New(cfg)
	}
	// plant puts a record of key holding an open replace of fixed with a saved start, while the
	// server holds fixed.
	plant := func(t *testing.T, key, fixed string) *harness {
		t.Helper()
		h := newHarness()
		if _, err := h.server.create(key)(ctx, fixed, Start{Seq: 3}); err != nil {
			t.Fatal(err)
		}
		h.store.put(key, Record{Key: key, Start: 3, Op: &Op{Kind: OpReplace, Old: fixed, Candidate: fixed + "~C", Owner: "planter", ReservedAt: h.clock.Now().Add(-time.Hour)}})
		return h
	}
	for _, c := range []struct {
		name, family, planted, plantedFixed, own, ownFixed string
	}{
		{"daemon/p keyed by the listener's durable", "daemon/p", "listener-m", "listener-m", "legion-go-p-wake", "legion-go-p-wake"},
		{"the session store keyed by a daemon's durable", SessionFamily, DurableKey("legion-go-q-wake"), "legion-go-q-wake", DurableKey("listener-m"), "listener-m"},
		{"the session store keyed by Dispatch's durable", SessionFamily, DurableKey("delivery-events"), "delivery-events", DurableKey("listener-m"), "listener-m"},
	} {
		t.Run(c.name, func(t *testing.T) {
			refused := func(t *testing.T, h *harness, err error) {
				t.Helper()
				if !errors.Is(err, ErrInvalidKey) || len(h.server.deletes) != 0 || !h.server.holds(c.plantedFixed) {
					t.Fatalf("a reader's Cleanup of %s's planted %s = %v; deletes sent %v, the server still holds %s: %t; want ErrInvalidKey, no delete, %s held",
						c.family, c.planted, err, h.server.deletes, c.plantedFixed, h.server.holds(c.plantedFixed), c.plantedFixed)
				}
			}
			t.Run("a Names built from the planted key", func(t *testing.T) {
				h := plant(t, c.planted, c.plantedFixed)
				naming, err := FamilyNaming(c.family, c.planted)
				if err == nil {
					_, err = reader(h, naming).Cleanup(ctx, c.planted)
				}
				refused(t, h, err)
			})
			t.Run("a Names built from the reader's own key", func(t *testing.T) {
				h := plant(t, c.planted, c.plantedFixed)
				naming, err := FamilyNaming(c.family, c.own)
				if err != nil {
					t.Fatal(err)
				}
				_, err = reader(h, naming).Cleanup(ctx, c.planted)
				refused(t, h, err)
			})
			t.Run("control: the reader's own key", func(t *testing.T) {
				h := plant(t, c.own, c.ownFixed)
				naming, err := FamilyNaming(c.family, c.own)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := reader(h, naming).Cleanup(ctx, c.own); err != nil || !slices.Equal(h.server.deletes, []string{c.ownFixed}) || h.server.holds(c.ownFixed) {
					t.Fatalf("control: the reader's Cleanup of its own %s = %v; deletes sent %v, the server still holds %s: %t; want %s deleted",
						c.own, err, h.server.deletes, c.ownFixed, h.server.holds(c.ownFixed), c.ownFixed)
				}
			})
		})
	}
}
