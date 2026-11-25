package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// noSessionPublish is a publish to role's topic that names no source session, as Legion's hook
// work-backs and a script that publishes over the HTTP API with no session send it.
func noSessionPublish(role, message string) string {
	return `{"topic":"` + contracts.RoleTopicPrefix + role + `","source":"agent","message":"` + message + `"}`
}

// requireDeliveredTo fails unless the publish was answered 200 naming holder with no kept field,
// the holder's stand-in was forwarded the answered event once, and nothing was kept for role.
func requireDeliveredTo(t *testing.T, d *listenerDeps, frames <-chan contracts.Envelope, role, holder, body string) {
	t.Helper()
	recorder, answer := publishToRole(t, d, role, body)
	if recorder.Code != http.StatusOK || answer.Kept || answer.Holder != holder {
		t.Fatalf("the publish with no source session answered %d %s, want 200 naming the live holder %s with nothing kept",
			recorder.Code, recorder.Body.String(), holder)
	}
	if frame := expectFrames(t, frames, 1, 250*time.Millisecond)[0]; frame.EventID != answer.EventID {
		t.Fatalf("the holder was forwarded event %s, want the answered %s", frame.EventID, answer.EventID)
	}
	assertNothingKept(t, d.client, role)
}

// A role a live session holds is delivered a publish that names no source session exactly as one
// that names a session: 200 naming the holder, the forward on its agent subject, nothing kept.
func TestAPublishWithNoSourceSessionToAHeldRoleIsDelivered(t *testing.T) {
	d, _ := keptRoleDeps(t)
	const role, holder = "no-session-held", "ses_no_session_held"
	claimRoleFor(t, d.registry, d.sessions, holder, role, nil)
	frames := startStandIn(t, testnats.URL(t), holder)
	requireDeliveredTo(t, d, frames, role, holder, noSessionPublish(role, "for the live holder"))
}

// A publish that names no source session, to a role no live session holds, is refused 404 and
// nothing is kept. A kept copy that no session takes within its hour is dropped with a dead letter
// to the publish's source session, and this publish names none, so its sender would never learn
// that the message it was told was kept had expired. The 404 carries the reason the role's
// resolution found (unclaimed, or holder_lapsed with the holder and its state), which Envoy's
// clients and the Legion daemon already read as "nothing was kept", and its error names the role
// and says why nothing was kept. The refusal's exception goes out as a keep: false refusal's does,
// and a lapsed holder's claim stands.
func TestAPublishWithNoSourceSessionToARoleWithNoLiveHolderIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		role   string
		holder string
		// register leaves the holder's record as the row needs it.
		register func(t *testing.T, d *listenerDeps, holder string)
		reason   roles.State
		state    session.State
	}{
		{
			name:     "nobody claims the role",
			role:     "no-session-unclaimed",
			register: func(*testing.T, *listenerDeps, string) {},
			reason:   roles.Unclaimed,
		},
		{
			name:     "the holder is gone",
			role:     "no-session-gone",
			holder:   "ses_no_session_gone",
			register: func(*testing.T, *listenerDeps, string) {},
			reason:   roles.Lapsed,
			state:    session.Gone,
		},
		{
			name:   "the holder is unresponsive",
			role:   "no-session-unresponsive",
			holder: "ses_no_session_unresponsive",
			register: func(t *testing.T, d *listenerDeps, holder string) {
				t.Helper()
				storeSessionHeartbeat(t, d.client, holder, time.Now().Add(-(session.LiveWindow + time.Minute)))
			},
			reason: roles.Lapsed,
			state:  session.Unresponsive,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, client := keptRoleDeps(t)
			tc.register(t, d, tc.holder)
			if tc.holder != "" {
				if _, err := d.registry.SetRole(tc.holder, "test-machine", tc.role, false); err != nil {
					t.Fatalf("claim %s for %s: %v", tc.role, tc.holder, err)
				}
			}
			probe := exceptionProbe(t, client)

			recorder, _ := publishThrough(d, noSessionPublish(tc.role, "nobody can take this now"))

			if recorder.Code != http.StatusNotFound {
				t.Fatalf("the publish with no source session to %s answered %d %s, want 404 with nothing kept",
					tc.role, recorder.Code, recorder.Body.String())
			}
			var refusal listenerwire.RoleRefusal
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if refusal.Reason != tc.reason || refusal.Holder != tc.holder || refusal.HolderState != tc.state {
				t.Fatalf("the refusal is %s, want reason %s naming holder %q in state %q", recorder.Body.String(), tc.reason, tc.holder, tc.state)
			}
			for _, words := range []string{tc.role, "nothing was kept", "source_session", "no notice"} {
				if !strings.Contains(refusal.Error, words) {
					t.Fatalf("the refusal's error is %q, want it to name %q: the role, that nothing was kept, and why", refusal.Error, words)
				}
			}
			if strings.Contains(recorder.Body.String(), `"kept"`) {
				t.Fatalf("the refusal %s carries a kept field, want none", recorder.Body.String())
			}
			message, err := probe.NextMsg(5 * time.Second)
			if err != nil {
				t.Fatalf("the refusal raised no exception: %v", err)
			}
			var exception contracts.Envelope
			if err := json.Unmarshal(message.Data, &exception); err != nil {
				t.Fatalf("decode the exception envelope: %v", err)
			}
			if payload := exceptionPayloadOf(t, exception); payload.Reason != string(tc.reason) || payload.RecipientSession != tc.holder {
				t.Fatalf("the exception payload = %+v, want reason %s naming %q", payload, tc.reason, tc.holder)
			}
			assertNothingKept(t, client, tc.role)
			if held, err := d.registry.RoleHolder(tc.role); err != nil || held != tc.holder {
				t.Fatalf("the role's claim after the refusal = %q, %v; want %q, untouched", held, err, tc.holder)
			}
		})
	}
}

// A publish that names a source session, to a role no live session holds, is still kept for the
// role's next live holder and answered 202: the dead letter of a copy that expires reaches that
// session.
func TestAPublishWithASourceSessionToARoleWithNoLiveHolderIsStillKept(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "no-session-control-kept"
	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","source_session":"ses_no_session_control_sender","message":"keep this for the next holder"}`)
	if recorder.Code != http.StatusAccepted || !answer.Kept || answer.KeptReason != string(roles.Unclaimed) {
		t.Fatalf("the publish with a source session to an unheld role answered %d %s, want 202 kept unclaimed", recorder.Code, recorder.Body.String())
	}
	if kept := waitForKeptRoleMessage(t, client, role, answer.EventID); kept.SourceSession != "ses_no_session_control_sender" {
		t.Fatalf("the kept copy names source session %q, want the sender's", kept.SourceSession)
	}
}

// claimlessRoleKV is a copy of the role bucket that has not applied role's claim, as a replica a
// direct get reaches can be: a direct get of role answers nats.ErrKeyNotFound, and every other call
// goes to the bucket. The leader's read of a key (store.Registry.RoleClaimFromLeader) is a stream
// message get on the bucket's own connection, not a call on this handle, so it answers the claim.
type claimlessRoleKV struct {
	natsgo.KeyValue
	role string
}

func (kv claimlessRoleKV) Get(key string) (natsgo.KeyValueEntry, error) {
	if key == kv.role {
		return nil, natsgo.ErrKeyNotFound
	}
	return kv.KeyValue.Get(key)
}

// "No live holder" is never read from a copy for a publish that names no source session: the
// refusal turns on it, and a copy that has not applied the newest claim or registration would
// refuse a message a live holder can take. The role's claim and its holder's registration are read
// from the leader of each bucket's stream, as the listener's other reads that act on an absence
// read them (store.Registry.ReadLaneHolders, session.SessionRegistry.RefreshFromLeader). Each row
// puts a copy behind that says the role has no live holder while the leader names one, and the
// publish is delivered to that holder. Control, in each row: the copy is behind for the row (a
// direct get answers no claim, or no registration).
func TestAPublishWithNoSourceSessionReadsTheHolderFromTheLeaderWhileACopyIsBehind(t *testing.T) {
	t.Run("the role bucket's copy holds no claim", func(t *testing.T) {
		d, _ := keptRoleDeps(t)
		const role, holder = "no-session-claim-behind", "ses_no_session_claim_behind"
		claimRoleFor(t, d.registry, d.sessions, holder, role, nil)
		frames := startStandIn(t, testnats.URL(t), holder)
		base := registryRoleKV(t, d.registry)
		setRegistryRoleKV(t, d.registry, claimlessRoleKV{KeyValue: base, role: role})
		t.Cleanup(func() { setRegistryRoleKV(t, d.registry, base) })
		if claim, err := d.registry.RoleClaim(context.Background(), role); err != nil || claim.HolderSessionID != "" {
			t.Fatalf("UNKNOWN: a direct get of %s's claim answers %+v, %v, not a copy holding no claim, so the row tests nothing", role, claim, err)
		}
		requireDeliveredTo(t, d, frames, role, holder, noSessionPublish(role, "the leader names my holder"))
	})

	t.Run("the session bucket's copy holds no registration", func(t *testing.T) {
		d, _ := keptRoleDeps(t)
		const role, holder = "no-session-session-behind", "ses_no_session_session_behind"
		// The cache stops following the bucket, so it never holds the registration below, and the
		// holder is read from the bucket: by a direct get, which the copy behind answers, or from
		// the stream's leader.
		d.sessions.StopWatch()
		storeSessionHeartbeat(t, d.client, holder, time.Now())
		if _, err := d.registry.SetRole(holder, "test-machine", role, false); err != nil {
			t.Fatalf("claim %s for %s: %v", role, holder, err)
		}
		frames := startStandIn(t, testnats.URL(t), holder)
		swapSessionBucket(t, d.sessions, func(kv natsgo.KeyValue) natsgo.KeyValue {
			return laggingFollowerBucket{KeyValue: kv, gets: &atomic.Int64{}}
		})
		if _, state := d.sessions.Lookup(holder); state == session.Live {
			t.Fatalf("UNKNOWN: the session cache holds %s live, so the row tests nothing", holder)
		}
		if _, err := d.sessions.Refresh(context.Background(), holder); !errors.Is(err, natsgo.ErrKeyNotFound) {
			t.Fatalf("UNKNOWN: a direct get of %s answers %v, not the not-found of a copy behind, so the row tests nothing", holder, err)
		}
		requireDeliveredTo(t, d, frames, role, holder, noSessionPublish(role, "the leader holds my registration"))
	})
}

// The holder of a publish that names no source session is never judged from the session cache:
// a cached registration live when it is looked up can pass its live deadline before the
// resolution judges it, which would refuse the send as holder_lapsed though the leader holds a
// fresh registration. The cache keeps a registration one millisecond inside its live window, the
// leader holds a fresh one, and the clock moves two milliseconds between the cache's look and the
// resolution's judgement. The resolution reads the holder from the leader and finds it live.
// Controls: the cache reads the old registration live before the clock moves, and the leader's
// registration is live at the resolution's time.
func TestANoSessionResolutionNeverJudgesTheCachedHolder(t *testing.T) {
	var armed atomic.Bool
	var reads atomic.Int64
	start := time.Now().Truncate(time.Millisecond)
	cachedAt := start.Add(-session.LiveWindow + time.Millisecond)
	clock := func() time.Time {
		if !armed.Load() {
			return cachedAt
		}
		if reads.Add(1) == 1 {
			return start
		}
		return start.Add(2 * time.Millisecond)
	}
	harness := newListenerDeliveryHarnessOn(t, setupPublishTestClient(t), session.WithSessionClock(clock))
	const holder, role = "ses_cache_lapse", "cache-lapse"
	claimRoleFor(t, harness.registry, harness.sessions, holder, role, nil)
	harness.sessions.StopWatch()
	storeSessionHeartbeat(t, harness.client, holder, start)
	if entry, state := harness.sessions.Lookup(holder); entry.UpdatedAt != cachedAt.UnixMilli() || state != session.Live {
		t.Fatalf("UNKNOWN: the cache holds %+v in state %s, not the old registration live, so the row tests nothing", entry, state)
	}
	armed.Store(true)
	result, err := roles.ResolveHolder(context.Background(), leaderRoleClaims{harness.registry}, leaderHolderSessions{harness.sessions}, role)
	armed.Store(false)
	if err != nil {
		t.Fatalf("resolve %s: %v", role, err)
	}
	leader, err := bucketSessionEntry(context.Background(), harness.sessions, holder, roleHolderReadTimeout)
	if err != nil {
		t.Fatalf("read %s from the leader: %v", holder, err)
	}
	if state := session.StateOf(leader, start.Add(2*time.Millisecond), session.LiveWindow, session.GoneHorizon); state != session.Live {
		t.Fatalf("UNKNOWN: the leader's registration of %s reads %s, not live, so the row tests nothing", holder, state)
	}
	if result.State != roles.Live {
		t.Fatalf("the resolution of %s is %s/%s from registration %d, want live from the leader's %d",
			role, result.State, result.HolderState, result.Entry.UpdatedAt, leader.UpdatedAt)
	}
}
