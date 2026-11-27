//go:build odatrace

package hooktest

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The two duplicate-window points, testnats.DedupeReset and testnats.DedupePurged, each held as
// hooks_test.go holds the others: the point is reached, what the server was about to do there
// does not happen while it is held, and it happens once the hold is released, after a control
// that shows the same action land unheld. nats-server v2.10.29 has neither point (hooks.go's
// missing), and both rows skip there. cmd/listener's
// TestForcedOrderARepeatIsStoredThoughThePurgeRanAtTheCut holds DedupeReset to force the order in
// which a purge reads the window an update is replacing.

// dedupeStream is a lone server with the stream DEDUPE, whose duplicate window is window, and a
// JetStream client of it whose requests wait until the test's deadline (testnats.MaxWait), so a
// held one never times out short of it.
func dedupeStream(t *testing.T, window time.Duration) (*testnats.Server, *testnats.Hooks, natsgo.JetStreamContext) {
	t.Helper()
	srv, uri := testnats.Start(t)
	hooks := testnats.Hook(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	if _, err := js.AddStream(&natsgo.StreamConfig{Name: "DEDUPE", Subjects: []string{"dedupe.>"}, Duplicates: window}); err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	return srv, hooks, js
}

// skipWithout skips t on a nats-server that has no point.
func skipWithout(t *testing.T, point testnats.Point) {
	t.Helper()
	if ok, why := point.Exists(); !ok {
		t.Skipf("nats-server %s has no %s: %s", natsserver.VERSION, point, why)
	}
}

// windowOf takes the duplicate-window events whose window is window: at DedupeReset the one the
// update is replacing, at DedupePurged the one the purge measured the ids' ages against.
func windowOf(window time.Duration) testnats.Match {
	return testnats.Where("window "+window.String(), func(e testnats.Event) bool { return e.Window == window })
}

// A stream update whose duplicate window changes stops at DedupeReset, past its re-arm of the
// purge of remembered ids and before it stores the new window: held there, the update is not
// answered, and released, it is, with the new window stored. The control is an unheld update.
func TestForcedOrderHoldsAStreamUpdateBeforeItStoresItsNewDuplicateWindow(t *testing.T) {
	skipWithout(t, testnats.DedupeReset)
	srv, hooks, js := dedupeStream(t, time.Minute)
	update := func(window time.Duration) <-chan error {
		return answer(func() error {
			_, err := js.UpdateStream(&natsgo.StreamConfig{Name: "DEDUPE", Subjects: []string{"dedupe.>"}, Duplicates: window})
			return err
		})
	}
	answered(t, update(50*time.Second), "the unheld update to a 50s window")

	hold := hooks.Hold(testnats.DedupeReset, testnats.OnServer(srv), testnats.Stream("DEDUPE"), windowOf(50*time.Second))
	done := update(40 * time.Second)
	reached := hold.Reached(t)
	t.Logf("held %s", reached)
	if reached.NewWindow != 40*time.Second {
		t.Fatalf("held %s, want the update from 50s to 40s", reached)
	}
	unanswered(t, done, "the update to a 40s window")
	hold.Release()
	answered(t, done, "the update to a 40s window")
	info, err := js.StreamInfo("DEDUPE")
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.Config.Duplicates != 40*time.Second {
		t.Fatalf("the stream's duplicate window is %s after the released update, want 40s", info.Config.Duplicates)
	}
}

// A purge of remembered duplicate ids stops at DedupePurged with the dedupe lock held, its window
// the stream's own: held there, a publish with a message id, which takes that lock to check and
// store the id, is not acknowledged, and released, it is. The control is the publish whose id
// arms that purge, a window later, acknowledged while no purge is held.
func TestForcedOrderHoldsAPurgeOfRememberedIdsWithItsLockHeld(t *testing.T) {
	skipWithout(t, testnats.DedupePurged)
	const window = time.Second
	srv, hooks, js := dedupeStream(t, window)
	publish := func(id string) <-chan error {
		return answer(func() error {
			_, err := js.Publish("dedupe.one", nil, natsgo.MsgId(id))
			return err
		})
	}
	hold := hooks.Hold(testnats.DedupePurged, testnats.OnServer(srv), testnats.Stream("DEDUPE"), windowOf(window))
	answered(t, publish("arms-the-purge"), "the publish whose id arms the purge")

	t.Logf("held %s", hold.Reached(t))
	done := publish("while-held")
	unanswered(t, done, "the publish while the purge is held")
	hold.Release()
	answered(t, done, "the publish while the purge is held")
}
