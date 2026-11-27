package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The first two tests below hold the role routes' hand-off to a stopped Legion agent's daemon to
// the record of the role's holder as the session bucket stream's leader holds it, whatever this
// listener's session cache holds (leaderOnlyHolderSessions). The leader holds a registration the
// holder made a moment ago, live. The cache follows the bucket through a watcher and so trails
// it: it still holds the holder's registration before that one, one millisecond inside its live
// window. A resolution that answers from the cache while the cached record reads live looks that
// record up live and judges it a moment later past its window (roles.ResolveHolder reads the
// holder's state again), so the holder reads lapsed, and the hand-off, which nothing takes back,
// gives the daemon the message while the live holder never receives it. The row's clock puts the
// look and the judgement on either side of that window's end (crossingClock). Each test's control
// is the same message while the cached record reads live when judged too. The third test holds a
// publish whose first resolution was read from the leaders to that one resolution.

// crossingClock is the session registry's clock of a row in which this listener's cached record of
// a role's holder reads live when a resolution looks it up and lapsed when the resolution judges
// it. Until running, it answers registered, the moment the cached registration is stamped with.
// Once running, it answers judged, except that the first read after each read of the role's claim
// (armed, claimReadArms) answers looked: a resolution reads the claim, then looks its holder up in
// the cache, then judges the record that look answered, with no other read of this clock between
// (holderRecord, roles.ResolveHolder). A resolution that reads the holder from the leader instead
// spends looked on applying what it read (session.SessionRegistry.RefreshFromLeader) and judges
// the leader's record at judged.
type crossingClock struct {
	registered, looked, judged time.Time
	running, armed             atomic.Bool
}

func (c *crossingClock) now() time.Time {
	switch {
	case !c.running.Load():
		return c.registered
	case c.armed.CompareAndSwap(true, false):
		return c.looked
	}
	return c.judged
}

// claimReadArms is the role bucket with each read of role's claim, a direct get (Get) or a read
// from the bucket stream's leader (NewestFromLeader, bus.LeaderReads), arming clock once it has
// answered: a direct get answered by the bucket, and a leader read by leader, the handle this one
// stands in for. Every other call is the bucket's own.
type claimReadArms struct {
	natsgo.KeyValue
	leader bus.KeyValue
	role   string
	clock  *crossingClock
}

func (kv claimReadArms) Get(key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.KeyValue.Get(key)
	kv.arm(key)
	return entry, err
}

func (kv claimReadArms) NewestFromLeader(ctx context.Context, key string) (natsgo.KeyValueEntry, error) {
	entry, err := kv.leader.NewestFromLeader(ctx, key)
	kv.arm(key)
	return entry, err
}

func (kv claimReadArms) arm(key string) {
	if key == kv.role {
		kv.clock.armed.Store(true)
	}
}

// crossedHolderRole is a Legion role of the project infra whose registry entry names a stopped
// agent, the newest of its role, which has no session record, and whose claim names a holder the
// row hears on its agent subject (heardHolder). The session bucket's leader holds the holder's
// registration made at the clock's looked, live; deps' session cache, which stopped following the
// bucket, holds the holder's registration before it (crossingClock's registered). Where the row
// crosses, the clock judges one millisecond past that registration's live window; otherwise it
// judges at looked, where the cached registration still reads live.
type crossedHolderRole struct {
	deps        *listenerDeps
	role        string
	clock       *crossingClock
	heardHolder <-chan contracts.Envelope
}

// crossCachedHolder builds a crossedHolderRole, and fails the row as UNKNOWN unless the cache
// holds the holder's earlier registration, which reads live when looked up and, where crossing is
// set, lapsed when judged.
func crossCachedHolder(t *testing.T, stopped, holder string, crossing bool) crossedHolderRole {
	t.Helper()
	start := time.Now().Truncate(time.Millisecond)
	clock := &crossingClock{registered: start.Add(-session.LiveWindow + time.Millisecond), looked: start, judged: start}
	if crossing {
		clock.judged = start.Add(2 * time.Millisecond)
	}
	harness := newListenerDeliveryHarnessOn(t, setupPublishTestClient(t), session.WithSessionClock(clock.now))
	d := harness.publishDeps(t)
	entry := legionAgentEntry(stopped, "INFRA-206", "reviewer")
	withLegionAgent(t, d, entry)
	claimRoleFor(t, d.registry, d.sessions, holder, entry.RoleToken, nil)
	d.sessions.StopWatch()
	storeSessionHeartbeat(t, d.client, holder, start)
	cached, state := d.sessions.Lookup(holder)
	if cached.UpdatedAt != clock.registered.UnixMilli() || state != session.Live {
		t.Fatalf("UNKNOWN: the cache holds %+v in state %s, not %s's earlier registration live, so the row tests nothing", cached, state, holder)
	}
	looked := session.StateOf(cached, clock.looked, session.LiveWindow, session.GoneHorizon)
	judged := session.StateOf(cached, clock.judged, session.LiveWindow, session.GoneHorizon)
	if looked != session.Live || (judged != session.Live) != crossing {
		t.Fatalf("UNKNOWN: the cached registration of %s reads %s when looked up and %s when judged, so the row tests nothing", holder, looked, judged)
	}
	heard := startStandIn(t, testnats.URL(t), holder)
	swapRegistryRoleHandle(t, d.registry, func(old bus.KeyValue) bus.KeyValue {
		return bus.NewKeyValue(claimReadArms{KeyValue: old.Legacy(), leader: old, role: entry.RoleToken, clock: clock}, old.WatchHandle(), old.Streams())
	})
	clock.running.Store(true)
	return crossedHolderRole{deps: d, role: entry.RoleToken, clock: clock, heardHolder: heard}
}

// requireNoHandoff fails the test when the stream holds a hand-off to stopped's daemon.
func (crossed crossedHolderRole) requireNoHandoff(t *testing.T, stopped, holder string) {
	t.Helper()
	if count, _ := handoffs(t, crossed.deps.client, stopped); count != 0 {
		t.Fatalf("the stream holds %d hand-offs for %s, want none: %s holds %s live, so the message is its", count, stopped, holder, crossed.role)
	}
}

// requireReachedHolder fails the test unless holder receives eventID, the one role message the
// test sends, and then fails it as UNKNOWN unless the session bucket stream's leader holds a
// registration of holder that reads live when judged.
func (crossed crossedHolderRole) requireReachedHolder(t *testing.T, holder, eventID string) {
	t.Helper()
	if heard := testwait.Receive(t, "the live holder "+holder+" to receive the role message "+eventID, crossed.heardHolder); heard.EventID != eventID {
		t.Fatalf("the live holder %s received %s, want the role message %s", holder, heard.EventID, eventID)
	}
	leader, err := bucketSessionEntry(context.Background(), crossed.deps.sessions, holder, roleHolderReadTimeout)
	if err != nil {
		t.Fatalf("read %s from the leader: %v", holder, err)
	}
	if state := session.StateOf(leader, crossed.clock.judged, session.LiveWindow, session.GoneHorizon); state != session.Live {
		t.Fatalf("UNKNOWN: the leader's registration of %s reads %s when judged, not live, so the row tests nothing", holder, state)
	}
}

// A publish to a Legion role whose live holder this listener's cache last saw one registration
// ago. The publish route's first resolution looks the holder up in the cache, finds that
// registration live, and judges it past its live window (sendToRole), so the route confirms the
// miss before it hands the message off (roleSend.noHolder), with the holder's record read from the
// session bucket stream's leader whatever the cache holds (roleSend.missConfirmed). The leader
// holds the holder's newer registration, live, so nothing is handed off: the message is kept, 202,
// and the kept lane's handover gives it to the holder. When the confirmation took the cache's
// answer while that read live (leaderHolderSessions), it judged the same cached registration
// lapsed, and the publish was handed to the stopped agent's daemon, 200: the live holder never
// received it. Control: with the cached registration live when judged too, the route finds the
// holder live and forwards the publish to it, 200. The publish names a source session, so its
// first resolution reads as GET /v1/roles/<role> does (roleSend.resolve).
func TestARolePublishReachesTheLiveHolderWhoseCachedRecordLapsesBetweenLookAndJudgement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		crossing bool
		status   int
		kept     bool
	}{
		{"the cached registration lapses between the look and the judgement", true, http.StatusAccepted, true},
		{"control: the cached registration is live when judged too", false, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const stopped, holder = "ses_publish_cache_lapse_stopped", "ses_publish_cache_lapse_holder"
			crossed := crossCachedHolder(t, stopped, holder, tc.crossing)

			encoded, err := json.Marshal(roleMessage(crossed.role, "ses_publish_cache_lapse_sender", "what did the review find?"))
			if err != nil {
				t.Fatal(err)
			}
			recorder, _ := publishThrough(crossed.deps, string(encoded))
			crossed.requireNoHandoff(t, stopped, holder)
			var answer keptPublishAnswer
			if recorder.Code != tc.status || json.Unmarshal(recorder.Body.Bytes(), &answer) != nil || answer.Kept != tc.kept {
				t.Fatalf("a publish to %s, held live by %s: status %d, body %s; want %d with kept %t",
					crossed.role, holder, recorder.Code, recorder.Body.String(), tc.status, tc.kept)
			}
			crossed.requireReachedHolder(t, holder, answer.EventID)
		})
	}
}

// Dispatch marks a send with a Legion role and aims it at the role's stopped agent, while the
// role's holder is live and this listener's cache last saw it one registration ago. The send route
// reads the claim from the leader before the send's target (answerRoleSend) and finds the holder,
// and its resolution of that holder (handOffOrKeepRoleSend) reads the holder's record from the
// session bucket stream's leader whatever the cache holds. The leader holds the holder's newer
// registration, live, so the send is kept for the holder, 202 kept, the kept lane's handover
// gives it to the holder, and the stopped agent's daemon is handed nothing. When the resolution
// took the cache's answer while that read live (leaderHolderSessions), it looked the cached
// registration up live and judged it lapsed, and the send was handed to the stopped agent's
// daemon, 200: the live holder never received it. Control: with the cached registration live when
// judged too, the same send is kept for the holder and reaches it.
func TestAMarkedSendReachesTheLiveHolderWhoseCachedRecordLapsesBetweenLookAndJudgement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		crossing bool
	}{
		{"the cached registration lapses between the look and the judgement", true},
		{"control: the cached registration is live when judged too", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const stopped, holder = "ses_marked_cache_lapse_stopped", "ses_marked_cache_lapse_holder"
			const principal = "system:serviceaccount:dispatch:example-dispatch"
			crossed := crossCachedHolder(t, stopped, holder, tc.crossing)
			useDispatchPrincipal(crossed.deps, principal)
			mux := http.NewServeMux()
			mux.Handle("/v1/messages/send", sendHandler(crossed.deps, logging.New("test")))

			got := sendMarkedByDispatch(t, mux, principal, stopped, crossed.role, "what did the review find?")
			crossed.requireNoHandoff(t, stopped, holder)
			if got.status != http.StatusAccepted || !got.Kept {
				t.Fatalf("a send marked with %s to %s, held live by %s: status %d, body %s; want 202 kept for %s",
					crossed.role, stopped, holder, got.status, got.raw, holder)
			}
			crossed.requireReachedHolder(t, holder, got.EventID)
		})
	}
}

// A publish that asks to be kept and names no source session resolves its role's holder from the
// buckets' stream leaders from the start (roleSend.resolve), so when it finds no live holder that
// resolution is the confirmation a hand-off asks for (roleSend.missConfirmed), and the route reads
// the leaders no second time. The role is a Legion workflow role's, so its miss can be handed off
// (legionHandoff.mayHandOff), and the registry holds no entry for it, so nothing is: each publish
// is answered 404. A resolution reads the role's claim once (roles.ResolveHolder), so the reads of
// the role bucket from its stream's leader count the leader resolutions. Before, the confirmation
// resolved the holder from the leaders again, and each publish read the claim there twice.
// Control: a keep false publish resolves its holder first from the cache and direct gets, and its
// confirmation is its one leader resolution.
func TestANoSessionPublishWithNoLiveHolderResolvesFromTheLeadersOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		claimed bool
		keep    bool
	}{
		{"nobody claims the role", false, true},
		{"the holder has no record", true, true},
		{"control: a keep false publish whose holder has no record", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := keptRoleDeps(t)
			withLegionAgent(t, d, legionAgentEntry("ses_leader_once_agent", "INFRA-213", "planner"))
			role := legionAgentEntry("ses_leader_once_unrecorded", "INFRA-214", "reviewer").RoleToken
			if tc.claimed {
				const holder = "ses_leader_once_holder"
				claimRoleFor(t, d.registry, d.sessions, holder, role, nil)
				if err := d.sessions.Delete(context.Background(), holder); err != nil {
					t.Fatalf("remove %s's session record: %v", holder, err)
				}
			}
			reads := &atomic.Int64{}
			swapRegistryRoleHandle(t, d.registry, func(old bus.KeyValue) bus.KeyValue {
				return bus.NewKeyValue(leaderReadCount{KeyValue: old.Legacy(), leader: old, reads: reads}, old.WatchHandle(), old.Streams())
			})
			fields := roleMessage(role, "", "who holds this?")
			fields["keep"] = tc.keep
			encoded, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}

			recorder, _ := publishThrough(d, string(encoded))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("a publish with no source session and keep %t to %s: status %d, body %s; want 404 with nothing kept or handed off",
					tc.keep, role, recorder.Code, recorder.Body.String())
			}
			if got := reads.Load(); got != 1 {
				t.Errorf("a publish with no source session and keep %t to %s, which has no live holder, read the role bucket from its stream's leader %d times, want once: one resolution from the leaders",
					tc.keep, role, got)
			}
		})
	}
}
