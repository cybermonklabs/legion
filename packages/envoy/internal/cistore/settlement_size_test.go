package cistore

import (
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The store accepts a check only while the settlement it would publish stays within
// maxSettlementBytes, 64 KiB under NATS's 1 MiB default max payload. A commit whose settlement is
// at that bound, of failing checks named in `"` (12 bytes a character there), settles through the
// listener's real publisher and a real NATS server at the default max payload: the stream stores
// it, and the record is settled rather than overflowed.
func TestASettlementAtItsBoundIsStored(t *testing.T) {
	conn, cleanup := connectNATS(t)
	defer cleanup()
	store := openStore(t, conn)
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the bus: %v", err)
	}
	defer client.Close()
	if got := client.Conn.MaxPayload(); got != natsDefaultMaxPayload {
		t.Fatalf("the server's max payload is %d, want the %d default", got, natsDefaultMaxPayload)
	}
	const (
		owner  = "example-org"
		repo   = "example-repo"
		number = "42"
		sha    = "abcdef1234567890abcdef1234567890abcdef12"
	)
	url := func(i int) string { return "https://github.com/example-org/example-repo/runs/" + strconv.Itoa(1000+i) }
	name := func(i, runes int) string { return fmt.Sprintf("%s%04d", strings.Repeat(`"`, runes-4), i) }
	// sizeOf is the size of the settlement state would publish with one more failing check, check i
	// named name, which the store would accept only within maxSettlementBytes. It adds the check to
	// a copy of state's checks, so one read of the record sizes every name tried beside it. A few
	// bytes are left for what the record's later writes add (a longer generation).
	const slack = 16
	sizeOf := func(state State, i int, name string) int {
		t.Helper()
		state.Checks = maps.Clone(state.Checks)
		state.Checks[name] = Check{Name: name, CheckRunID: checkRunID(1000 + i), URL: url(i), Status: "completed", Conclusion: "failure", ObservedAt: "2026-09-27T00:00:00Z"}
		size, err := settlementSize(state)
		if err != nil {
			t.Fatalf("settlement size: %v", err)
		}
		return size + slack
	}
	record := func(i int, name string) {
		t.Helper()
		if err := recordCheck(store, owner, repo, number, sha, name, strconv.Itoa(1000+i), url(i), "completed", "failure", "2026-09-27T00:00:00Z"); err != nil {
			t.Fatalf("record check %d: %v", i, err)
		}
	}

	// Checks named at the envelope text cap while the next one fits, then one whose name is as long
	// as still fits. Each name is sized beside the record as the bucket holds it after the write
	// before: the store's write leaves its cache to the watcher, which applies the record it wrote
	// later, so the store holds no copy of it when Record returns.
	//
	// A check named at 2048 runes grows the settlement by the same bytes each time, give or take a
	// digit of the record's generation: its name, run id and URL take one width for every check
	// here (%04d of i, 1000+i, four digits up to 8999). So the growth the second check makes places
	// the last one that fits, and the checks up to one short of it are recorded with no read
	// between them, one at a time as before. Their record is then read once and sized whole: a
	// settlement only grows with its checks, so a whole size within the bound shows that each of
	// those checks fitted beside the record before it, as the loop below asks of each check it
	// records. The loop goes on from that read a check at a time, so the check it stops at is one
	// that does not fit beside the record as the bucket holds it.
	record(0, name(0, 2048))
	checks := 1
	state := getState(t, store, owner, repo, number, sha)
	over := sizeOf(state, checks, name(checks, 2048))
	if first := over; first <= maxSettlementBytes {
		record(checks, name(checks, 2048))
		checks++
		state = getState(t, store, owner, repo, number, sha)
		over = sizeOf(state, checks, name(checks, 2048))
		if growth := over - first; growth > 0 && over <= maxSettlementBytes {
			if unread := (maxSettlementBytes - over) / growth; unread > 0 {
				for range unread {
					record(checks, name(checks, 2048))
					checks++
				}
				state = getState(t, store, owner, repo, number, sha)
				whole, err := settlementSize(state)
				if err != nil {
					t.Fatalf("settlement size: %v", err)
				}
				if whole+slack > maxSettlementBytes {
					t.Fatalf("the %d checks recorded with no read between them settle in %d bytes, past the %d-byte bound less %d: a check did not grow the settlement by the %d bytes the second one did",
						unread, whole, maxSettlementBytes, slack, growth)
				}
				over = sizeOf(state, checks, name(checks, 2048))
			}
		}
	}
	for over <= maxSettlementBytes {
		record(checks, name(checks, 2048))
		checks++
		state = getState(t, store, owner, repo, number, sha)
		over = sizeOf(state, checks, name(checks, 2048))
	}
	// The last name is sized beside the record the loop read last, which no write has changed
	// since. A name's settlement grows by the same bytes for each rune, so the sizes at 5 runes and
	// at 2048 place the longest name that fits; the lengths at and past that place confirm it, and a
	// search of the whole range runs only where they do not.
	fits := func(runes int) bool { return sizeOf(state, checks, name(checks, runes)) <= maxSettlementBytes }
	last := 4
	if shortest := sizeOf(state, checks, name(checks, 5)); shortest <= maxSettlementBytes {
		last = 5 + (maxSettlementBytes-shortest)*(2048-5)/(over-shortest)
		if !fits(last) || fits(last+1) {
			last = sort.Search(2048-4, func(runes int) bool { return !fits(runes + 5) }) + 4
		}
	}
	record(checks, name(checks, last))
	checks++
	state = getState(t, store, owner, repo, number, sha)
	size, err := settlementSize(state)
	if err != nil {
		t.Fatalf("settlement size: %v", err)
	}
	if state.Overflowed || size > maxSettlementBytes || size < maxSettlementBytes-64 {
		t.Fatalf("the record of %d checks settles in %d bytes (overflowed %v), want within 64 bytes under its %d-byte bound",
			checks, size, state.Overflowed, maxSettlementBytes)
	}

	waitCacheChecks(t, store, owner, repo, number, sha, checks)
	runSummaryTick(store, client, 0, logging.New("test"))
	stored, err := client.NextStoredMessage(callContext(t), "notifications.github.example-org.example-repo.pr.42.checks", 0)
	if err != nil {
		t.Fatalf("read the stored settlement: %v", err)
	}
	if len(stored.Data) < maxSettlementBytes-64 {
		t.Fatalf("the stored settlement is %d bytes, want the %d-byte one the record settles in", len(stored.Data), size)
	}
	if got := getState(t, store, owner, repo, number, sha); !got.SettledEmitted || got.Overflowed {
		t.Fatalf("after its settlement was stored the record is settled %v, overflowed %v; want settled", got.SettledEmitted, got.Overflowed)
	}
	t.Logf("the stored settlement of %d checks is %d bytes", checks, len(stored.Data))
}
