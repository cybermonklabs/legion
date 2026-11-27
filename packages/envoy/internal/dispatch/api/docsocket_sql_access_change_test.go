package api

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// TestADocumentSocketClosesOnItsHeartbeatOnceItsPersonLosesTheRoomBySQLAlone: the document service
// re-checks its sockets' access on the sockets' heartbeat (Deps.StreamHeartbeat), the one the live
// socket and the Delivery views resolve their readers again on (accessRecheckEvery), so an access
// change made by SQL alone, which appends no event and announces nothing on
// store.AccessChangedChannel, closes the socket of the person it took the room from as an
// announced change does: with 4412, on which the provider reconnects. alice, signed in by her
// cookie, opens the spec of TEST, the
// project she made; the socket outlives several heartbeats while she reads it. Then TEST is given
// to carol and made private by one statement, as at a psql prompt, which takes it from alice, and
// her socket closes within a heartbeat and the pass's own reads, though her cookie still resolves.
//
// Control: register starts no access re-check on the heartbeat; the cookie's heartbeat keeps the
// socket open and the row fails at its wait.
func TestADocumentSocketClosesOnItsHeartbeatOnceItsPersonLosesTheRoomBySQLAlone(t *testing.T) {
	const heartbeat = 200 * time.Millisecond
	rig := newDocumentSocketTokenRig(t, heartbeat, nil)
	peer := rig.openDocument(t, nil)

	time.Sleep(4 * heartbeat)
	select {
	case <-peer.connection().Ended:
		t.Fatalf("alice's document socket closed (code %d) within four heartbeats while she read TEST", peer.connection().CloseCode())
	default:
	}
	peer.barrier(t)

	if _, err := rig.tokens.database.Pool.Exec(context.Background(),
		`update projects set owner = 'carol@d.example', visibility = 'private' where key = 'TEST'`); err != nil {
		t.Fatalf("give TEST to carol and make it private by SQL alone: %v", err)
	}
	changed := time.Now()
	const within = heartbeat + 3*time.Second
	select {
	case <-peer.connection().Ended:
		t.Logf("alice's document socket closed %s after the change committed (heartbeat %s)", peer.connection().EndedAt().Sub(changed).Round(time.Millisecond), heartbeat)
	case <-time.After(within):
		t.Fatalf("alice's document socket was still open %s after TEST was taken from her by SQL alone (heartbeat %s)", within, heartbeat)
	}
	if code := peer.connection().CloseCode(); code != contracts.DocumentAdmissionRevokedCloseCode {
		t.Fatalf("alice's document socket closed with %d, want %d, as an announced access change closes it", code, contracts.DocumentAdmissionRevokedCloseCode)
	}
}
