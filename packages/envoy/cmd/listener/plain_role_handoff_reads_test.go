package main

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/testnats"
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

// legionRegistryConsumerCalls is how many of calls, the JetStream API calls one connection sent
// (jetStreamAPIProbe), were consumer calls on the Legion agent registry's bucket: what each scan
// of a role token or an issue sends (legionagents.Store's listings, kvwatch.ScanExistingKeys), and
// no other read of the registry does.
func legionRegistryConsumerCalls(calls []string) int {
	made := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "$JS.API.CONSUMER.") && strings.Contains(call, "KV_"+bus.LegionAgentBucket) {
			made++
		}
	}
	return made
}

// A role publish that finds no live holder pays the reads its hand-off makes - the buckets' stream
// leaders' confirmation of its miss (roleSend.missConfirmed) and the Legion agent registry's scan
// of the role token for its newest session (answeringAgent) - only where its message can be handed
// to a stopped Legion agent's daemon: the token is a Legion workflow role's
// (legionHandoff.mayHandOff) and the project it names last said it answers. A plain Envoy role's
// token names no Legion agent, so its publish makes neither, whether nobody claims the role or its
// holder has no record; and a Legion role of a project that has stated no answering record makes
// neither, since the hand-off reads that project's flag before both. Before, such a publish
// confirmed the miss from the leaders and scanned the registry before the token or the flag turned
// the hand-off down. Each publish asks keep false, so no kept lane handover runs beside it, and is
// answered 404. Control: the same publishes to a Legion workflow role of the project infra, which
// answers, that the registry holds no entry for make the confirmation's leader reads and the
// registry's scan, which the counts see.
func TestAPlainRolePublishWithNoLiveHolderMakesNoLeaderRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		role    string
		claimed bool
		// reads is whether the publish pays the hand-off's reads: the confirmation from the
		// leaders and the registry's scan.
		reads bool
	}{
		{"a plain role nobody claims", "plain-reads-unclaimed", false, false},
		{"a plain role whose holder has no record", "plain-reads-lapsed", true, false},
		{"a Legion role of a project with no answering record, nobody claiming it", legionagents.RoleToken("quiet", "INFRA-214", "reviewer"), false, false},
		{"a Legion role of a project with no answering record, its holder with no record", legionagents.RoleToken("quiet", "INFRA-215", "reviewer"), true, false},
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
			calls := watchJetStreamAPI(t, testnats.URL(t), d.client.Conn, "$JS.API.CONSUMER.>", "$JS.API.STREAM.INFO.>")

			recorder, _ := publishThrough(d, `{"topic":"`+contracts.RoleTopicPrefix+tc.role+`","source":"agent","message":"who holds this?","keep":false}`)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("a publish with keep false to %s: status %d, body %s; want 404 with nothing kept or handed off", tc.role, recorder.Code, recorder.Body.String())
			}
			switch got := reads.Load(); {
			case !tc.reads && got != 0:
				t.Errorf("a publish to %s with no live holder made %d reads from a bucket stream's leader, want none: no Legion agent's daemon can be handed its message", tc.role, got)
			case tc.reads && got == 0:
				t.Errorf("control: a publish to the Legion role %s with no live holder made no read from a bucket stream's leader, want its confirmation's: the count does not see them", tc.role)
			}
			switch scanned := legionRegistryConsumerCalls(calls.callsBeforeControl(t, d.client.JS())); {
			case !tc.reads && scanned != 0:
				t.Errorf("a publish to %s with no live holder made %d consumer calls on the Legion agent registry, want no scan of it: no Legion agent's daemon can be handed its message", tc.role, scanned)
			case tc.reads && scanned == 0:
				t.Errorf("control: a publish to the Legion role %s with no live holder made no consumer call on the Legion agent registry, want its scan's: the probe does not see them", tc.role)
			}
		})
	}
}
