package main

// legacy_record_test.go holds what a listener does with a session record an earlier build wrote,
// one naming no key (session.SessionEntry.SessionID empty): every listener, the edge included,
// reads it as its session's for its own bookkeeping, so the edge's fleet-wide sweep keeps a live
// session's consumer; and the edge's scope test reads it as no project's, so the edge lists it to
// nobody and answers a send to it as to a missing session.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/listenerwire"
)

// rewriteAsEarlierBuild writes sessionID's record again as a listener of an earlier build writes
// it: the same fields, a registration time of now, and no session_id.
func rewriteAsEarlierBuild(t *testing.T, conn *natsgo.Conn, sessionID string) {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.SessionBucket, err)
	}
	got, err := kv.Get(sessionID)
	if err != nil {
		t.Fatalf("read %s's record: %v", sessionID, err)
	}
	var fields map[string]any
	if err := json.Unmarshal(got.Value(), &fields); err != nil {
		t.Fatalf("decode %s's record: %v", sessionID, err)
	}
	if _, named := fields["session_id"]; !named {
		t.Fatalf("%s's record %s names no key before the rewrite: this build writes the key", sessionID, got.Value())
	}
	delete(fields, "session_id")
	fields["updated_at"] = time.Now().UnixMilli()
	legacy, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode %s's record: %v", sessionID, err)
	}
	if _, err := kv.Put(sessionID, legacy); err != nil {
		t.Fatalf("write %s's record as an earlier build writes it: %v", sessionID, err)
	}
}

// awaitEarlierBuildRecord waits until registry reads sessionID's record as live and naming no key.
func awaitEarlierBuildRecord(t *testing.T, registry *session.SessionRegistry, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(listenerWaitBound(t))
	for {
		entry, state := registry.Lookup(sessionID)
		if state == session.Live && entry.SessionID == "" && entry.MachineID != "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the registry reads %s as %s (%+v), want the earlier build's record, live and naming no key", sessionID, state, entry)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A live session whose record an earlier build wrote keeps its consumer through a pass of the
// consumer sweep, which the pod holding the delivery lease runs over the whole stream
// (holderLoops): the registry reads the record as live, as before, and so does the reapers' test
// (sessionMayBeLive). The control is the same session's record deleted: the same pass deletes the
// consumer of a session that is gone.
func TestTheSweepKeepsTheConsumerOfALiveSessionAnEarlierBuildRegistered(t *testing.T) {
	t.Parallel()
	for _, gone := range []bool{false, true} {
		name := "its record as an earlier build writes it"
		if gone {
			name = "its record deleted (control)"
		}
		t.Run(name, func(t *testing.T) {
			now := &stateClock{}
			rig := newSessionConsumerRig(t,
				session.WithSessionTTL(time.Hour),
				session.WithSessionClock(now.Now),
				session.WithSessionLiveWindow(time.Hour),
				session.WithSessionGoneHorizon(time.Hour),
			)
			// A pass deletes only while its receive term is current (sessionRelays.termContext), so
			// the relay set the pass reads holds an endless one, as the sweep rig's does.
			relays := &sessionRelays{term: endlessTerm()}
			relays.owned.Store(true)
			sweep := &sessionConsumerSweep{delivery: rig.deps.consumers, sessions: rig.harness.sessions, deps: rig.deps, relays: relays, logger: rig.harness.config.logger}
			const sessionID = "ses_earlier_build_live"
			rig.subscribe(t, `{"session_id":"`+sessionID+`","self_subscribed":true,"topics":[],"delivery":{"consumer":true,"epoch":"e"}}`)
			rig.requireConsumer(t, sessionID)

			if gone {
				if err := rig.harness.sessions.Delete(context.Background(), sessionID); err != nil {
					t.Fatalf("delete %s's record: %v", sessionID, err)
				}
				if _, state := rig.harness.sessions.Lookup(sessionID); state != session.Gone {
					t.Fatalf("the registry reads %s as %s after its delete, want gone", sessionID, state)
				}
			} else {
				rewriteAsEarlierBuild(t, rig.harness.client.Conn, sessionID)
				awaitEarlierBuildRecord(t, rig.harness.sessions, sessionID)
				if live, err := sessionMayBeLive(rig.harness.sessions, sessionID); err != nil || !live {
					t.Fatalf("sessionMayBeLive(%s) = %t, %v; want true: the reapers would take its claims and row", sessionID, live, err)
				}
			}

			sweep.pass(context.Background())
			if gone {
				rig.requireNoConsumer(t, sessionID)
			} else {
				rig.requireConsumer(t, sessionID)
			}
		})
	}
}

// The edge grants nothing across a project on a record that names no key: such a record cannot be
// told from another session's record copied under that key, so a session whose record an earlier
// listener wrote, of the edge's own project, is listed to no session at the edge and a send to it
// is answered as to a missing session. The control is the same session's record as this build
// writes it, naming its key: the edge lists it and takes the send.
func TestAnEdgeGrantsNoScopeToARecordNamingNoKey(t *testing.T) {
	t.Parallel()
	for _, earlier := range []bool{true, false} {
		name := "a record an earlier build wrote"
		if !earlier {
			name = "a record this build wrote (control)"
		}
		t.Run(name, func(t *testing.T) {
			rig := newEdgeRigServing(t, []string{"PRIV"})
			const reader, target = "ses_edge_legacy_reader", "ses_inhouse_legacy"
			token := rig.personToken(reader, "PRIV")
			requireStatus(t, rig.subscribe(t, token, reader), http.StatusOK, "the reader's registration")
			rig.placeElsewhere(t, target, "PRIV")
			if earlier {
				rewriteAsEarlierBuild(t, rig.harness.client.Conn, target)
				awaitEarlierBuildRecord(t, rig.deps.sessions, target)
			}

			listed := rig.call(t, http.MethodGet, token, "/v1/sessions", "")
			requireStatus(t, listed, http.StatusOK, "GET /v1/sessions")
			var rows []listenerwire.SessionInfo
			if err := json.Unmarshal(listed.Body.Bytes(), &rows); err != nil {
				t.Fatalf("decode %s: %v", listed.Body.String(), err)
			}
			ids := make([]string, 0, len(rows))
			for _, row := range rows {
				ids = append(ids, row.SessionID)
			}
			sent := rig.call(t, http.MethodPost, token, "/v1/messages/send",
				fmt.Sprintf(`{"target_session":%q,"source_session":%q,"message":"from the edge","source":"agent"}`, target, reader))
			if earlier {
				if slices.Contains(ids, target) {
					t.Errorf("GET /v1/sessions lists %s, whose record names no key: %v", target, ids)
				}
				var refusal listenerwire.NoLiveSessionRefusal
				_ = json.Unmarshal(sent.Body.Bytes(), &refusal)
				if sent.Code != http.StatusNotFound || !strings.HasPrefix(refusal.Error, "no live session "+target) || refusal.LastSeen != 0 {
					t.Errorf("a send to %s: status %d (%s), want the 404 a missing session gets", target, sent.Code, sent.Body.String())
				}
				return
			}
			if !slices.Contains(ids, target) {
				t.Errorf("GET /v1/sessions lists %v, want %s, whose record names its key", ids, target)
			}
			if sent.Code != http.StatusOK {
				t.Errorf("a send to %s: status %d (%s), want it taken", target, sent.Code, sent.Body.String())
			}
		})
	}
}
