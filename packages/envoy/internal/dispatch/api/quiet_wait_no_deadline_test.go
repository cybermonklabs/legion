package api

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// Under go test -timeout 0 the test binary has no deadline, so neither has the quiet wait
// (awaitQuietDatabase, testwait.Until): it ends only once a read of it finds nothing busy. An xid
// fixture arms its spec's settlement xidFixtureSettlesNothing out, a settlement that runs beside
// no measurement, so a read of the wait over a built fixture with no deadline finds nothing busy,
// as a read under a deadline does. A read that counts that armed settlement as work holds every
// test that builds an xid fixture - the seeding's own wait first (seedXidTemplate) - for the whole
// of that delay under -timeout 0, where the same test passed at the merge base in seconds.
// The read runs through testwait.Returns, so a writer that holds it gives up with the test's other
// waits; its own context keeps no deadline, as the wait's has none under -timeout 0. It runs off
// the test goroutine, so a failure of its own stops it through stoppingTB, which the test reports.
func TestAQuietReadWithNoDeadlineFindsABuiltXidFixtureIdle(t *testing.T) {
	f := newXidFixture(t, testServerOptions{}, "")
	name := f.database.Pool.Config().ConnConfig.Database
	stopping := &stoppingTB{TB: t}
	var busy string
	testwait.Returns(t, "one read of the quiet wait with no deadline", func() {
		busy = quietBusy(stopping, context.Background(), f.database, name, false, func(quietRead) {})
	})
	if stop := stopping.stop(); stop != "" {
		t.Fatalf("the read of the quiet wait stopped: %s", stop)
	}
	if busy != "" {
		t.Fatalf("a read of the quiet wait with no deadline (go test -timeout 0) over a built xid fixture found %q busy, want nothing: the wait over every xid fixture would last its settle delay, %s", busy, xidFixtureSettlesNothing)
	}
}
