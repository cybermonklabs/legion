package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// c-one-leader-read (dispatch://INFRA-5459). Each read the listener makes of a role's claim from
// the role bucket stream's leader names the same holder for one stored message: the session
// relay's and the kept lane's (cmd/listener's readRoleClaimHolder and leaderRoleClaims, through
// Registry.RoleClaimFromLeader), and a controller notice lane's (Registry.ReadLaneHolders, which a
// registration's plan and write, a stored row's filter and a publish's recipient count make). The
// messages are a writer's claim, a delete marker, and three the server writes under the role's key
// itself: a JetStream API answer, a direct get's copy of another role's claim, and a RePublish of
// a claim. A message the server wrote is no writer's claim, so it names no holder: a read that
// took the copy or the RePublish for a claim would count its session a holder of the lane while
// the relay reads the role as held by nobody. The writer's claim, the marker and the API answer,
// which every read names alike, are the rows that keep the comparison from failing for nothing.
func TestEveryLeaderReadOfARoleClaimNamesTheSameHolder(t *testing.T) {
	conn, cleanup := connectNATS(t)
	t.Cleanup(cleanup)
	reg := openRegistry(t, conn)
	roles := reg.roles().Legacy()
	const (
		writer = "ses_leader_read_writer"
		copied = "ses_leader_read_copied"
		forger = "ses_leader_read_forger"
		source = "leader-read-source"
	)
	putRoleClaim(t, roles, source, copied)
	forged, err := json.Marshal(RoleClaim{HolderSessionID: forger, ClaimedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatalf("encode the claim the RePublish stores: %v", err)
	}
	for _, row := range []struct {
		name, project string
		// store leaves the message under role's key; want is the holder every read names for it.
		store func(t *testing.T, role string)
		want  string
	}{
		{"a writer's claim", "LEADPUT", func(t *testing.T, role string) { putRoleClaim(t, roles, role, writer) }, writer},
		{"a delete marker", "LEADDEL", func(t *testing.T, role string) {
			putRoleClaim(t, roles, role, writer)
			if err := roles.Delete(role); err != nil {
				t.Fatalf("delete %s's claim: %v", role, err)
			}
		}, ""},
		{"a JetStream API answer", "LEADINFO", func(t *testing.T, role string) {
			testnats.ServerWritesKey(t, conn, bus.RoleClaimBucket, role, testnats.StreamInfo("KV_"+bus.RoleClaimBucket), nil)
		}, ""},
		{"a direct get's copy of another role's claim", "LEADCOPY", func(t *testing.T, role string) {
			testnats.ServerCopiesKey(t, conn, bus.RoleClaimBucket, role, bus.RoleClaimBucket, source)
		}, ""},
		{"a RePublish of a claim", "LEADREPUB", func(t *testing.T, role string) {
			testnats.RepublishesKey(t, conn, bus.RoleClaimBucket, role, forged)
		}, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			role := contracts.ControllerRoleToken(row.project)
			lane, isLane := bus.ControllerNoticeLane(role)
			if !isLane {
				t.Fatalf("%s names no controller notice lane", role)
			}
			row.store(t, role)

			fromLeader := leaderReadHolder(t, reg, role)
			lanes, err := reg.ReadLaneHolders(context.Background(), []string{lane})
			if err != nil {
				t.Fatalf("ReadLaneHolders(%s): %v", lane, err)
			}
			fromLane, weighed := lanes[role]
			if !weighed {
				t.Fatalf("ReadLaneHolders(%s) = %v, want it to weigh %s's claim", lane, lanes, role)
			}
			if fromLeader != fromLane {
				t.Fatalf("over %s, RoleClaimFromLeader reads %s's claim as naming %q and ReadLaneHolders as naming %q; want one holder from both",
					row.name, role, fromLeader, fromLane)
			}
			if fromLeader != row.want {
				t.Fatalf("over %s, every leader read names %q as %s's holder, want %q", row.name, fromLeader, role, row.want)
			}
		})
	}
}

// leaderReadHolder is the holder Registry.RoleClaimFromLeader reads role's claim as naming, in the
// terms a controller notice lane's read answers (LaneHolders): the session a claim names, and the
// empty string for a role no claim names or whose record no read can decode
// (ErrMalformedRoleClaim). Any other error fails the test.
func leaderReadHolder(t *testing.T, reg *Registry, role string) string {
	t.Helper()
	claim, err := reg.RoleClaimFromLeader(context.Background(), role)
	switch {
	case errors.Is(err, ErrMalformedRoleClaim):
		return ""
	case err != nil:
		t.Fatalf("RoleClaimFromLeader(%s): %v", role, err)
	}
	return claim.HolderSessionID
}
