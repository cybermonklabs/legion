package session

// The server writes a session record itself when a request names that record's subject as its
// reply subject (testnats.ServerWritesKey). The record it writes there can be another key's
// record, the session's own interest row, a JetStream API answer or an empty status answer, and
// none of them is a registration of the session the key names, so the registry reads each as no
// record at all: a copy of another session's record would otherwise answer that session's scope
// under the victim's key.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// openBindingRegistry opens a registry on the test's own session bucket and waits until its copy
// follows the bucket.
func openBindingRegistry(t *testing.T, client *bus.Client, options ...SessionRegistryOption) *SessionRegistry {
	t.Helper()
	r, err := OpenSessionRegistry(t.Context(), client.Conn, append([]SessionRegistryOption{WithSessionReplicas(1), WithSessionTTL(time.Minute)}, options...)...)
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	waitFor(t, 5*time.Second, r.watcher.Following)
	return r
}

// awaitApplied waits until the registry's copy has taken the record stored for key at revision,
// whether it kept it or evicted it: the copy's fence holds the revision of every record it applied.
func awaitApplied(t *testing.T, r *SessionRegistry, key string, revision uint64) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		_, epoch := r.watcher.Handle()
		return r.cache.FenceFor(epoch, key) >= revision
	})
}

// storedValue is the value of the newest message the session bucket's stream holds for key, read
// from the stream's leader.
func storedValue(t *testing.T, conn *natsgo.Conn, key string) []byte {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	message, err := js.GetLastMsg("KV_"+bus.SessionBucket, testnats.KVSubject(bus.SessionBucket, key))
	if err != nil {
		t.Fatalf("read %s's newest record: %v", key, err)
	}
	return message.Data
}

// requireAbsent fails the test unless the registry reads no record for key: a read of the bucket
// is not found, the copy holds none, and List names no such session.
func requireAbsent(t *testing.T, r *SessionRegistry, key, what string) {
	t.Helper()
	if entry, err := r.Refresh(context.Background(), key); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Errorf("Refresh(%s) after %s = %+v, %v; want nats.ErrKeyNotFound", key, what, entry, err)
	}
	if entry, state := r.Lookup(key); state != Gone || entry.MachineID != "" || entry.Projects != nil {
		t.Errorf("Lookup(%s) after %s = %+v, %s; want the zero entry, gone", key, what, entry, state)
	}
	if ids := listed(t, r); slices.Contains(ids, key) {
		t.Errorf("List after %s names %s: %v", what, key, ids)
	}
}

// G4 at the store: a record this build wrote (S_priv, bound to its key by a conditional create) is
// copied with a direct get. Its copy carries S_priv's stored expectation of a key with no message,
// so the server stores it only on a key no session has used (ses_absent); onto S_pub's occupied key
// it is refused, since a direct-get copy of a bound record can never overwrite a different session's
// live record. The copy at ses_absent is byte for byte S_priv's registration, so read plainly it
// answers S_priv's machine and projects under that key; it names S_priv's key, so the registry
// reads it as no record.
func TestASessionRecordNamingAnotherKeyReadsAbsent(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	if err := r.Put("ses_priv", SessionEntry{MachineID: "edge-a", Title: "priv", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register ses_priv: %v", err)
	}
	if err := r.Put("ses_pub", SessionEntry{MachineID: "shared-a", Title: "pub", Projects: []string{"PUB"}}); err != nil {
		t.Fatalf("register ses_pub: %v", err)
	}

	stored := testnats.ServerCopiesKey(t, client.Conn, bus.SessionBucket, "ses_absent", bus.SessionBucket, "ses_priv")
	// The control that the write is the forge it is meant to be: the bucket now holds S_priv's own
	// bytes under the never-used key.
	if copied, source := storedValue(t, client.Conn, "ses_absent"), storedValue(t, client.Conn, "ses_priv"); !bytes.Equal(copied, source) {
		t.Fatalf("the bucket holds %s under ses_absent, want ses_priv's record %s", copied, source)
	}
	awaitApplied(t, r, "ses_absent", stored)
	requireAbsent(t, r, "ses_absent", "a copy of ses_priv's record onto a never-used key")

	// The same copy onto S_pub's occupied key is refused: a direct-get copy of a bound record can
	// never overwrite a different session's live record, and S_pub keeps its own.
	testnats.ServerCopyRefused(t, client.Conn, bus.SessionBucket, "ses_pub", bus.SessionBucket, "ses_priv")
	if entry, err := r.Refresh(context.Background(), "ses_pub"); err != nil || entry.MachineID != "shared-a" {
		t.Fatalf("Refresh(ses_pub) after the refused copy = %+v, %v; want its own record, machine shared-a", entry, err)
	}

	// The control: S_priv's own record still reads live, with its own machine.
	entry, err := r.Refresh(context.Background(), "ses_priv")
	if err != nil || entry.MachineID != "edge-a" || !slices.Equal(entry.Projects, []string{"PRIV"}) {
		t.Fatalf("Refresh(ses_priv) = %+v, %v; want its own record, machine edge-a, projects [PRIV]", entry, err)
	}
	if _, state := r.Lookup("ses_priv"); state != Live {
		t.Fatalf("Lookup(ses_priv) = %s, want live", state)
	}
}

// A delete of a session whose key holds a copy of another session's record removes the copy. The
// direct get of the copy reports the revision the copy's own headers name, the source record's, so
// a delete fenced on it is refused with wrong last sequence at every attempt; the delete is
// fenced on the revision the copy is stored at instead, and the source's record is not touched.
func TestADeleteRemovesASessionRecordNamingAnotherKey(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	if err := r.Put("ses_priv", SessionEntry{MachineID: "edge-a", Title: "priv", ClientInstanceID: "instance-priv", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register ses_priv: %v", err)
	}
	source := storedValue(t, client.Conn, "ses_priv")
	for _, tc := range []struct {
		name   string
		victim string
		delete func(victim string) error
	}{
		{"a delete", "ses_deleted", func(victim string) error { return r.Delete(context.Background(), victim) }},
		{"a conditional delete", "ses_deleted_if", func(victim string) error {
			outcome, err := r.DeleteIf(context.Background(), victim, DeleteConditions{ClientInstanceID: "instance-victim"})
			if err == nil && outcome != EntryDeleted {
				return errors.New(outcome.String())
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testnats.ServerCopiesKey(t, client.Conn, bus.SessionBucket, tc.victim, bus.SessionBucket, "ses_priv")
			if err := tc.delete(tc.victim); err != nil {
				t.Fatalf("%s of %s over a copy of ses_priv's record: %v", tc.name, tc.victim, err)
			}
			kv, err := client.JS().KeyValue(bus.SessionBucket)
			if err != nil {
				t.Fatalf("open %s: %v", bus.SessionBucket, err)
			}
			if entry, err := kv.Get(tc.victim); !errors.Is(err, natsgo.ErrKeyNotFound) {
				t.Fatalf("the bucket's %s after %s = %v, %v; want it deleted", tc.victim, tc.name, entry, err)
			}
			if after := storedValue(t, client.Conn, "ses_priv"); !bytes.Equal(after, source) {
				t.Fatalf("ses_priv's record after %s is %s, want it unchanged: %s", tc.name, after, source)
			}
		})
	}
}

// The server copies a session's own interest row onto its session key with a direct get of the
// interest bucket. The row names the session's key, its machine and a time, so read plainly it is
// a registration of that session naming no projects. Every registration of this build names its
// projects, so a record naming its key and no projects is read as no record. The row was created
// with no previous revision and its copy carries that expectation, so the stream stores the copy
// only on a session key that holds no record yet.
func TestASessionsOwnInterestRowReadsAbsentAtItsSessionKey(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	interests, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Storage: natsgo.MemoryStorage})
	if err != nil {
		t.Fatalf("create %s: %v", bus.InterestBucket, err)
	}
	row := []byte(`{"session_id":"ses_pub","machine_id":"shared-a","dir":"","topics":["notifications.agent.ses_pub"],"updated_at":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `}`)
	if _, err := interests.Create("ses_pub", row); err != nil {
		t.Fatalf("store ses_pub's interest row: %v", err)
	}

	stored := testnats.ServerCopiesKey(t, client.Conn, bus.SessionBucket, "ses_pub", bus.InterestBucket, "ses_pub")
	if copied := storedValue(t, client.Conn, "ses_pub"); !bytes.Equal(copied, row) {
		t.Fatalf("the session bucket holds %s under ses_pub, want its interest row %s", copied, row)
	}
	awaitApplied(t, r, "ses_pub", stored)

	requireAbsent(t, r, "ses_pub", "a copy of its own interest row onto its session key")
}

// A registration carries the keys this build does not know only from a record bound to the
// session's key (stampClaim): one naming the key and a machine, as every build that writes such a
// key does. A RePublish of another stream stores a body at the key, which the copy holds at the
// sequence it is stored at, so the registration's first write lands over it. A body naming no key
// reads as an earlier build's registration, whether it names a machine or none, and its keys ride
// into no write; a body naming another key the copy evicts, and its keys ride into none either. A
// writer's record bound to the key keeps its unknown key across the registration (the roll rule).
//
// Control: the stored record's unknown keys carried with no binding check (stampClaim before this
// row) store the forged key of both bodies that name no key.
func TestARegistrationCarriesUnknownKeysOnlyFromARecordBoundToItsKey(t *testing.T) {
	client := setupNATS(t)
	r := openBindingRegistry(t, client)
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	// unknownAfter registers sessionID over what the bucket holds and answers the stored record's
	// value of key, "" when it holds none.
	unknownAfter := func(t *testing.T, sessionID, key string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.PutWithin(ctx, sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", sessionID, err)
		}
		var stored map[string]json.RawMessage
		value := storedValue(t, client.Conn, sessionID)
		if err := json.Unmarshal(value, &stored); err != nil {
			t.Fatalf("decode %s's stored record %s: %v", sessionID, value, err)
		}
		if string(stored["session_id"]) != strconv.Quote(sessionID) || string(stored["machine_id"]) != `"edge-a"` {
			t.Fatalf("the registration of %s stored %s, want its own key and machine", sessionID, value)
		}
		return string(stored[key])
	}
	for _, tc := range []struct{ name, sessionID, body string }{
		{"a body naming no key and a machine", "ses_unknown_no_key", `{"machine_id":"edge-x","updated_at":` + now + `,"projects":["PUB"],"forged_key":"forged"}`},
		{"a body naming no key and no machine", "ses_unknown_no_machine", `{"updated_at":` + now + `,"forged_key":"forged"}`},
		{"a body naming another key", "ses_unknown_other_key", `{"session_id":"ses_other","machine_id":"edge-x","updated_at":` + now + `,"projects":["PUB"],"forged_key":"forged"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Put(tc.sessionID, SessionEntry{MachineID: "edge-a", Projects: []string{"PRIV"}}); err != nil {
				t.Fatalf("register %s: %v", tc.sessionID, err)
			}
			awaitApplied(t, r, tc.sessionID, testnats.RepublishesKey(t, client.Conn, bus.SessionBucket, tc.sessionID, []byte(tc.body)))
			if got := unknownAfter(t, tc.sessionID, "forged_key"); got != "" {
				t.Errorf("the registration of %s over %s stored its unknown key forged_key = %s, want none", tc.sessionID, tc.name, got)
			}
		})
	}
	t.Run("a writer's record bound to the key", func(t *testing.T) {
		const sessionID = "ses_unknown_bound"
		kv, err := client.JS().KeyValue(bus.SessionBucket)
		if err != nil {
			t.Fatalf("open %s: %v", bus.SessionBucket, err)
		}
		revision, err := kv.Put(sessionID, []byte(`{"session_id":"`+sessionID+`","machine_id":"edge-a","updated_at":`+now+`,"projects":["PRIV"],"future_key":"kept"}`))
		if err != nil {
			t.Fatalf("store a newer build's record of %s: %v", sessionID, err)
		}
		awaitApplied(t, r, sessionID, revision)
		if got := unknownAfter(t, sessionID, "future_key"); got != `"kept"` {
			t.Errorf("the registration of %s over its newer build's record stored future_key = %q, want \"kept\"", sessionID, got)
		}
	})
}
