//go:build odatrace

// The forced-order control for the two-update cut of the stream's duplicate window
// (stored_copy_edges_test.go#cutTheDuplicateWindow): it runs the purge of remembered ids where
// nats-server v2.15.1's own scheduling runs it only sometimes.

package main

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A repeat is stored once the stream forgets its id even when the purge of remembered ids runs at
// the cut of the duplicate window, between the update's re-arm of that purge and the store of its
// new window (testnats.DedupeReset). With the update held there, the purge reads the window being
// replaced, the deployed 72 hours, so it keeps the id and re-arms itself for about 72 hours: the
// order CI met by chance, which the row TestARepeatAfterTheStreamForgetsTheIdStoresANewCopy
// failed under before its cut became two updates (cybermonklabs/legion#5497).
//
// The hold is what makes the order certain, so this row is the control that cut must keep: with
// one update the purge that runs during the hold is the stream's last, the id is never
// forgotten, and the closing wait (awaitARepeatStoredOnceForgotten) fails; with two, the second
// update re-arms the purge once the first one's window is stored, whatever that purge did. Two
// readings inside the row can fail before that one: the repeat before the cut, which must answer
// the first copy as a duplicate, and the purge's own report, which must say it kept the id under
// the deployed window.
func TestForcedOrderARepeatIsStoredThoughThePurgeRanAtTheCut(t *testing.T) {
	// Not parallel: it installs the server's forced-order hook (testnats.Hook), a process-wide
	// CompareAndSwap that refuses a second test hooking the server at once.
	if ok, why := testnats.DedupeReset.Exists(); !ok {
		t.Skipf("nats-server %s has no %s: %s", natsserver.VERSION, testnats.DedupeReset, why)
	}
	// The keeper is held off for the reason TestARepeatAfterTheStreamForgetsTheIdStoresANewCopy
	// holds it off: a pass of it between the cut and the forget would put the deployed window back.
	rig, server := newSenderLimitRigOnServer(t, bus.WithKeepInterval(backgroundReadsHeld))
	hooks := testnats.Hook(t)
	js := rig.harness.client.JS()
	const sender = "ses_forced_forgotten_id"
	subject := "notifications.team.forced-forgotten-" + id.New()
	key := map[string]any{"idempotency_key": uuid.NewString()}

	first := held(t, rig.publishFrom(t, sender, subject, key))
	first.requireStatus(t, "the first publish", http.StatusOK)
	// Control: while the stream remembers the id, a repeat answers the first copy as a duplicate,
	// so the reading after the cut, a repeat with its own envelope, is one that does change.
	remembered := rig.publishFrom(t, sender, subject, key)
	remembered.requireStatus(t, "the repeat while the stream remembers the id", http.StatusOK)
	if !remembered.Duplicate || remembered.EventID != first.envelope.EventID {
		t.Fatalf("the repeat while the stream remembers the id answered %s, want the first copy's event %s as a duplicate",
			remembered.raw, first.envelope.EventID)
	}

	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	deployed := info.Config.Duplicates
	onTheStream := []testnats.Match{testnats.OnServer(server), testnats.Stream(bus.Stream),
		testnats.Where("under the deployed window "+deployed.String(), func(e testnats.Event) bool { return e.Window == deployed })}
	hold := hooks.Hold(testnats.DedupeReset, onTheStream...)

	// The cut runs in a goroutine of its own: its first update is the one the hold pauses, and the
	// stream answers that update only once the hold is released.
	cut := make(chan error, 1)
	go func() { cut <- cutTheDuplicateWindow(js, info.Config, forgottenIdWindow) }()
	reached := hold.Reached(t)
	purged := hooks.Await(t, testnats.DedupePurged, onTheStream...)
	if purged.Remembered == 0 {
		t.Fatalf("the purge at %s kept no id (%s): the cut was not held where a purge keeps the id under the window being replaced, so the closing wait would pass whatever the cut does",
			reached, purged)
	}
	hold.Release()
	if err := <-cut; err != nil {
		t.Fatal(err)
	}
	awaitARepeatStoredOnceForgotten(t, rig, sender, subject, key, first)
}
