package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
)

// leaderReadCount is a bucket handle that counts the reads it is asked to make from the bucket
// stream's leader (bus.LeaderReads) and makes each through leader, the handle it stands in for,
// so every read answers as the bucket does. Every other call is the bucket's own.
type leaderReadCount struct {
	natsgo.KeyValue
	leader bus.KeyValue
	reads  *atomic.Int64
}

func (kv leaderReadCount) NewestFromLeader(ctx context.Context, key string) (natsgo.KeyValueEntry, error) {
	kv.reads.Add(1)
	return kv.leader.NewestFromLeader(ctx, key)
}

// countLeaderReads has d read the role bucket and the session bucket through a leaderReadCount
// each, from here on, and answers the count both add to.
func countLeaderReads(t *testing.T, d *listenerDeps) *atomic.Int64 {
	t.Helper()
	reads := &atomic.Int64{}
	counted := func(old bus.KeyValue) bus.KeyValue {
		return bus.NewKeyValue(leaderReadCount{KeyValue: old.Legacy(), leader: old, reads: reads}, old.WatchHandle(), old.Streams())
	}
	swapRegistryRoleHandle(t, d.registry, counted)
	swapSessionHandle(t, d.sessions, counted)
	return reads
}

// A role publish that finds no live holder pays the reads from the buckets' stream leaders that
// confirm the miss (roleSend.missConfirmed) only where its message can be handed to a stopped
// Legion agent's daemon (legionHandoff.mayHandOff). A plain Envoy role's token names no Legion
// agent, so its publish makes no leader read, whether nobody claims the role or its holder has no
// record. Before, the publish confirmed the miss from the leaders and the hand-off then turned
// the token down (answeringAgent). Each publish asks keep false, so no kept lane handover runs
// beside it, and is answered 404. Control: the same publishes to a Legion workflow role's token
// that the registry holds no entry for make the confirmation's reads, which the count sees.
func TestAPlainRolePublishWithNoLiveHolderMakesNoLeaderRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		role    string
		claimed bool
		legion  bool
	}{
		{"a plain role nobody claims", "plain-reads-unclaimed", false, false},
		{"a plain role whose holder has no record", "plain-reads-lapsed", true, false},
		{"control: a Legion role nobody claims", legionagents.RoleToken("infra", "INFRA-211", "reviewer"), false, true},
		{"control: a Legion role whose holder has no record", legionagents.RoleToken("infra", "INFRA-212", "reviewer"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := keptRoleDeps(t)
			withLegionAgent(t, d, legionAgentEntry("ses_plain_reads_agent", "INFRA-213", "planner"))
			if tc.claimed {
				const holder = "ses_plain_reads_holder"
				claimRoleFor(t, d.registry, d.sessions, holder, tc.role, nil)
				if err := d.sessions.Delete(context.Background(), holder); err != nil {
					t.Fatalf("remove %s's session record: %v", holder, err)
				}
			}
			reads := countLeaderReads(t, d)

			recorder, _ := publishThrough(d, `{"topic":"`+contracts.RoleTopicPrefix+tc.role+`","source":"agent","message":"who holds this?","keep":false}`)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("a publish with keep false to %s: status %d, body %s; want 404 with nothing kept or handed off", tc.role, recorder.Code, recorder.Body.String())
			}
			switch got := reads.Load(); {
			case !tc.legion && got != 0:
				t.Errorf("a publish to the plain role %s with no live holder made %d reads from a bucket stream's leader, want none: no Legion agent can be handed its message", tc.role, got)
			case tc.legion && got == 0:
				t.Errorf("control: a publish to the Legion role %s with no live holder made no read from a bucket stream's leader, want its confirmation's: the count does not see them", tc.role)
			}
		})
	}
}
