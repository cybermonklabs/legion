package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// leaderReadLostBound is the bound the row holds its read to, internal/bus's lostReadBound: well
// above the reconnect the row cuts the connection into (nats.go dials the same server again after
// its ReconnectWait, 1 s), which the row requires to take at most a quarter of it, so a read that
// fails before half of it failed at the reconnect and not at its bound.
const leaderReadLostBound = 8 * time.Second

// A read of a role's claim from the role bucket stream's leader (Registry.RoleClaimFromLeader, the
// read the session relay judges a role message by and the kept lane's handover resolves the role's
// holder with) whose request reached the socket and died with it fails with
// bus.ErrRequestLostWithConnection at the reconnect, rather than waiting out its bound for an
// answer nothing will send: nats.go fails no request its connection drops. The read is a stream
// message get, `$JS.API.STREAM.MSG.GET.KV_<role bucket>`, on the registry's handle, which is
// opened on the connection of the client that dialled through the relay; the relay drops that
// request, and the cut comes once the request is out. The reconnect takes at most a quarter of the
// bound, and the read fails before half of it; the read made again on the connection that came
// back answers the claim. It is internal/bus's
// TestALeaderReadLostWithItsConnectionFailsAtTheReconnectNotAtItsBound for the registry's read.
// Control: with the read made without bus.BoundedOn, it waits out its bound and the row fails.
func TestARoleClaimFromTheLeaderLostWithItsConnectionFailsAtTheReconnectNotAtItsBound(t *testing.T) {
	const (
		holder = "ses_role_claim_leader_lost"
		role   = "reviewer"
	)
	relay, conn, registry := relayedRegistry(t, "$JS.API.STREAM.MSG.GET.KV_"+bus.RoleClaimBucket)
	if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("%s claims %s: %v", holder, role, err)
	}
	requireLostBeforeHalfTheBound(t, relay, conn, "the leader read of "+role+"'s claim", func(ctx context.Context) error {
		_, err := registry.RoleClaimFromLeader(ctx, role)
		return err
	})

	// The read made again on the connection that came back is answered.
	again, cancelAgain := context.WithTimeout(context.Background(), leaderReadLostBound)
	defer cancelAgain()
	if claim, err := registry.RoleClaimFromLeader(again, role); err != nil || claim.HolderSessionID != holder {
		t.Fatalf("the leader read of %s's claim again after the reconnect = %+v, %v; want %s's claim", role, claim, err, holder)
	}
}
