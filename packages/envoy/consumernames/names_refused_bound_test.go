package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// A session refused a consumer is asked again at every heartbeat (the listener's registration),
// and here every Ensure of its key is refused, 200 times: at the stream's max_consumers, or by a
// create the server answers with an API error. The record stays bounded, and so do each call's
// reads. A candidate the server refused at the cap is not retired, since the server answered its
// create; and each retirement drops the retired names the server does not hold whose time has
// passed (DropRetired's rule), so the record keeps only the candidates retired less than the
// create bound (and the settle age, the dropper's margin) ago, which may still land. The bound: at
// calls 30 s apart, past both, one retired name, the candidate that call's Step 0 retired, and at
// most 3 info reads a call (Step 0's candidate, the name its retirement drops, the cap's read of
// the one it keeps); at calls 3 s apart, 4 names and 9 reads; under an API error, which reads
// nothing at the cap, one name and 2 reads. Before, each call at the cap added two names, its
// refused candidate and the last call's, that nothing but a Cleanup removed, and read them all:
// after 200 calls 30 s apart, 399 retired names, a 42,486-byte record and 399 info reads on the
// last call (the reviewer's measurement on this harness).
func TestAKeyRefusedAtEveryCallKeepsABoundedRecord(t *testing.T) {
	const key, calls, maxBytes = "ses-refused", 200, 1024
	full := func(h *harness) error {
		if _, err := h.server.create("ses-other")(context.Background(), "session~other", Start{Seq: 1}); err != nil {
			t.Fatal(err)
		}
		h.server.capacity = 1
		return ErrConsumerLimit
	}
	insufficient := &jetstream.APIError{Code: 500, ErrorCode: 10023, Description: "insufficient resources"}
	refusing := func(h *harness) error {
		h.server.createFault = func(string) (bool, error) { return false, insufficient }
		return insufficient
	}
	for _, c := range []struct {
		name    string
		apart   time.Duration
		refuse  func(*harness) error
		retired int
		infos   int
	}{
		{name: "at a full cap, calls 30 s apart", apart: 30 * time.Second, refuse: full, retired: 1, infos: 3},
		{name: "at a full cap, calls 3 s apart", apart: 3 * time.Second, refuse: full, retired: 4, infos: 9},
		{name: "a create the server refuses, calls 30 s apart", apart: 30 * time.Second, refuse: refusing, retired: 1, infos: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			refusal := c.refuse(h)
			counting := &countingServer{fakeServer: h.server}
			names := h.names("p1")
			names.cfg.Server = counting
			most := struct{ retired, abandoned, infos, bytes int }{}
			for call := range calls {
				if call > 0 {
					h.clock.advance(c.apart)
				}
				counting.calls = nil
				if _, _, err := names.Ensure(context.Background(), key, CreateRequest{Create: h.server.create(key), First: first(1)}); !errors.Is(err, refusal) {
					t.Fatalf("Ensure %d = %v, want it refused with %v", call+1, err, refusal)
				}
				infos := 0
				for _, n := range counting.calls {
					infos += n
				}
				record := h.store.record(t, key)
				data, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				most.retired, most.abandoned = max(most.retired, len(record.Retired)), max(most.abandoned, len(record.Abandoned))
				most.infos, most.bytes = max(most.infos, infos), max(most.bytes, len(data))
				if len(record.Retired) > c.retired || len(record.Abandoned) > c.retired || infos > c.infos || len(data) > maxBytes {
					t.Fatalf("after Ensure %d the record holds %d retired and %d abandoned names in %d bytes, and the call read %d infos; want at most %d names, %d bytes and %d infos",
						call+1, len(record.Retired), len(record.Abandoned), len(data), infos, c.retired, maxBytes, c.infos)
				}
			}
			h.server.createdTwice(t)
			t.Logf("over %d calls: at most %d retired and %d abandoned names, %d infos in a call, %d bytes", calls, most.retired, most.abandoned, most.infos, most.bytes)
		})
	}
}
