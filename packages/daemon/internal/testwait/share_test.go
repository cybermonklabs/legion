package testwait

import (
	"testing"
	"time"
)

// A short bound is the caller's own: Bounded hands it back whole, not the share, however much of
// the deadline is left. A bound that names no ceiling of its own - 1<<60, the deadline written out
// by hand - gets the share of what is left of the deadline less spare, as every deadline-bound
// wait does. Read just before and just after the call, the share lies between the two readings,
// from one monotonic clock, so the bounds need no tolerance.
func TestBoundedKeepsAShortBoundAndSharesALongOne(t *testing.T) {
	const short, spare = 10 * time.Millisecond, time.Second
	if got := Bounded(t, short, spare); got != short {
		t.Fatalf("Bounded(t, %s, %s) = %s, want %s: a short bound is the caller's own and stays whole", short, spare, got, short)
	}
	deadline, ok := t.Deadline()
	if !ok {
		if got := Bounded(t, 1<<60, spare); got != 1<<60 {
			t.Fatalf("under -timeout 0, Bounded(t, 1<<60, %s) = %s, want the bound itself: there is no deadline to share", spare, got)
		}
		return
	}
	most := (time.Until(deadline) - spare) / Share
	got := Bounded(t, 1<<60, spare)
	least := (time.Until(deadline) - spare) / Share
	if got < least || got > most {
		t.Fatalf("Bounded(t, 1<<60, %s) = %s, want between %s and %s: a 1/%d share of the time to the deadline %s less %s",
			spare, got, least, most, Share, deadline.Format(time.RFC3339Nano), spare)
	}
}
