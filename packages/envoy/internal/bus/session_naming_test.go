package bus

import (
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
)

// The session creator names as the family table does: its Config's prefix, fixed name, metadata and
// key rule are the table's for the session family (consumernames.SessionNaming), so its fence is
// the one consumernames.CheckFamily applies for the session family, which gives the answer each
// round-6 case pins: a convert's open replace of s-ses-A served, a retired fixed name, another
// session's fixed name and another family's current refused. The controls, each run with a fault
// and seen failing: a creator whose prefix is not the table's fails the naming row, so does one
// with no metadata of its own (its owner check then falls back to envoy.logical alone, which the
// owner cases below cannot tell from the table's), and one whose key rule is the durable's
// (CheckDurableKey) on a session id; a table with no fixed rule fails the convert's case.
func TestTheSessionCreatorNamesAsTheFamilyTable(t *testing.T) {
	_, consumers, _ := freshSessionConsumersOnStream(t)
	names := consumers.fresh.names
	creator, table := names.Naming(), consumernames.SessionNaming()
	for _, id := range []string{"ses-A", "ses_x9"} {
		if got, want := creator.Prefix(id), table.Prefix(id); got != want {
			t.Errorf("the session creator's prefix of %s is %q, the table's %q", id, got, want)
		}
		if got, want := creator.Fixed(id), table.Fixed(id); got != want {
			t.Errorf("the session creator's fixed name of %s is %q, the table's %q", id, got, want)
		}
		if creator.Metadata == nil {
			t.Errorf("the session creator has no metadata of its own; want the table's %v for %s", table.Metadata(id), id)
		} else if got, want := creator.Metadata(id), table.Metadata(id); !maps.Equal(got, want) {
			t.Errorf("the session creator's metadata of %s is %v, the table's %v", id, got, want)
		}
	}
	if creator.CheckKey == nil {
		t.Errorf("the session creator's key rule is nil; want the table's")
	} else {
		for _, key := range []string{"ses-A", "", "ses.A", consumernames.DurableKey("listener-m")} {
			got, want := creator.CheckKey(key), table.CheckKey(key)
			if (got == nil) != (want == nil) || errors.Is(got, consumernames.ErrInvalidKey) != errors.Is(want, consumernames.ErrInvalidKey) {
				t.Errorf("the session creator's key rule of %q is %v, the table's %v", key, got, want)
			}
		}
	}
	reserved := time.Unix(1_700_000_000, 0)
	for _, c := range []struct {
		name   string
		record consumernames.Record
		served bool
	}{
		{"its own current", consumernames.Record{Current: "session~A", Start: 3}, true},
		{"a convert's open replace of s-ses-A", consumernames.Record{Start: 3, Op: &consumernames.Op{Kind: consumernames.OpReplace, Old: "s-ses-A", Candidate: "session~B", ReservedAt: reserved}}, true},
		{"another session's fixed name as the old name", consumernames.Record{Start: 3, Op: &consumernames.Op{Kind: consumernames.OpReplace, Old: "s-ses-B", Candidate: "session~B", ReservedAt: reserved}}, false},
		{"a retired fixed name", consumernames.Record{Current: "session~A", Start: 3, Retired: []string{"s-ses-A"}}, false},
		{"a current of the listener's durable", consumernames.Record{Current: "listener-m~A", Start: 3}, false},
	} {
		if err := consumernames.CheckFamily(consumernames.SessionFamily, "ses-A", c.record); (err == nil) != c.served {
			t.Errorf("%s: CheckFamily for the session family = %v; want served %t", c.name, err, c.served)
		}
	}
	// Its owner check is OwnsFamily's for the session family: one implementation, alike on its own
	// consumer, its fixed one, another session's and an unmarked one.
	consumer := func(name string, metadata map[string]string) *jetstream.ConsumerInfo {
		return &jetstream.ConsumerInfo{Name: name, Config: jetstream.ConsumerConfig{Durable: name, Metadata: metadata}}
	}
	for _, c := range []struct {
		name  string
		info  *jetstream.ConsumerInfo
		owned bool
	}{
		{"its own consumer", consumer("session~A", sessionConsumerMetadata("ses-A")), true},
		{"its fixed consumer, unmarked", consumer("s-ses-A", nil), true},
		{"another session's consumer", consumer("session~A", sessionConsumerMetadata("ses-B")), false},
		{"an unmarked consumer", consumer("session~A", nil), false},
	} {
		own, family := names.Owns("ses-A", c.info), consumernames.OwnsFamily(consumernames.SessionFamily, "ses-A", c.info)
		if (own == nil) != c.owned || (family == nil) != c.owned {
			t.Errorf("%s: the session creator's Owns = %v, OwnsFamily = %v; want both owned %t", c.name, own, family, c.owned)
		}
	}
}
