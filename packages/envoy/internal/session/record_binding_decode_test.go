package session

import (
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

// DecodeEntry reads a stored record naming a key as the registration of the key it is stored
// under only when it names that key, a machine, a registration time and its projects (the key
// present, null or a list), and refuses every other such record with ErrEntryUnbound: one naming
// another key, the empty key or null, and a session's own interest row, which names its key, its
// machine and a time but no projects. A record naming no key is read as before, its entry naming
// no key (SessionID empty), which the edge's scope test reads as no project's. A value that is not
// JSON is the decode error itself, as it was before records named their key.
func TestDecodeEntryReadsOnlyARecordBoundToItsKey(t *testing.T) {
	updated := strconv.FormatInt(time.Now().UnixMilli(), 10)
	const info = `{"type":"io.nats.jetstream.api.v1.stream_info_response","config":{"name":"KV_envoy_sessions"},"created":"2026-11-21T00:00:00Z"}`
	for _, tc := range []struct {
		name      string
		value     string
		unbound   bool
		malformed bool
		machine   string
		// key is the SessionID the entry decodes with: "" for a record naming no key.
		key string
	}{
		{name: "a record naming its key", value: `{"session_id":"ses_k","machine_id":"m","updated_at":` + updated + `,"projects":["PRIV"]}`, machine: "m", key: "ses_k"},
		{name: "a record naming its key and null projects", value: `{"session_id":"ses_k","machine_id":"m","updated_at":` + updated + `,"projects":null}`, machine: "m", key: "ses_k"},
		{name: "an older build's record naming no key", value: `{"machine_id":"m","updated_at":` + updated + `}`, machine: "m"},
		{name: "a stream info answer, which names no key", value: info},
		{name: "a record naming another key", value: `{"session_id":"ses_other","machine_id":"m","updated_at":` + updated + `,"projects":[]}`, unbound: true},
		{name: "a record naming the empty key", value: `{"session_id":"","machine_id":"m","updated_at":` + updated + `,"projects":[]}`, unbound: true},
		{name: "a record naming a null key", value: `{"session_id":null,"machine_id":"m","updated_at":` + updated + `,"projects":[]}`, unbound: true},
		{name: "a record naming its key and no machine", value: `{"session_id":"ses_k","updated_at":` + updated + `,"projects":[]}`, unbound: true},
		{name: "a record naming its key and no registration time", value: `{"session_id":"ses_k","machine_id":"m","projects":[]}`, unbound: true},
		{name: "a record naming its key and a negative registration time", value: `{"session_id":"ses_k","machine_id":"m","updated_at":-1,"projects":[]}`, unbound: true},
		{name: "the session's own interest row", value: `{"session_id":"ses_k","machine_id":"m","dir":"","topics":["notifications.agent.ses_k"],"updated_at":` + updated + `}`, unbound: true},
		{name: "an empty value", value: ``, malformed: true},
		{name: "a value that is not JSON", value: `not json`, malformed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry, err := DecodeEntry("ses_k", []byte(tc.value))
			switch {
			case tc.unbound:
				if !errors.Is(err, ErrEntryUnbound) {
					t.Fatalf("DecodeEntry(ses_k, %s) = %+v, %v; want ErrEntryUnbound", tc.value, entry, err)
				}
			case tc.malformed:
				if err == nil || errors.Is(err, ErrEntryUnbound) {
					t.Fatalf("DecodeEntry(ses_k, %q) = %+v, %v; want the JSON decode error", tc.value, entry, err)
				}
			default:
				if err != nil || entry.MachineID != tc.machine || entry.SessionID != tc.key {
					t.Fatalf("DecodeEntry(ses_k, %s) = %+v, %v; want the record, machine %q, naming key %q", tc.value, entry, err, tc.machine, tc.key)
				}
			}
		})
	}
}

// putOlderBuildRecord stores at key the record a build that wrote no session id stores: a machine
// and a registration time, and no key.
func putOlderBuildRecord(t *testing.T, conn *natsgo.Conn, key string) {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.SessionBucket, err)
	}
	value, err := json.Marshal(map[string]any{"machine_id": "shared-a", "title": key, "updated_at": time.Now().UnixMilli(), "projects": []string{"PUB"}})
	if err != nil {
		t.Fatalf("encode %s: %v", key, err)
	}
	if _, err := kv.Put(key, value); err != nil {
		t.Fatalf("store %s: %v", key, err)
	}
}

// A record an older build wrote names no key, so a reader cannot tell it from a copy of another
// session's record. Every registry reads it as its session's, as before, so a listener's sweeps,
// reapers and role routes keep every session an earlier listener registered; its entry names no
// key, which is what the edge's scope test (cmd/listener's edgeScope.shows) reads as no
// project's. The control: a record this build wrote names its key.
func TestARecordNamingNoKeyIsReadAsBeforeAndNamesNoKey(t *testing.T) {
	client := setupNATS(t)
	registry := openBindingRegistry(t, client)
	if err := registry.Put("ses_bound", SessionEntry{MachineID: "edge-a", Title: "bound", Projects: []string{"PRIV"}}); err != nil {
		t.Fatalf("register ses_bound: %v", err)
	}
	putOlderBuildRecord(t, client.Conn, "ses_older")
	ctx := context.Background()

	entry, err := registry.Refresh(ctx, "ses_older")
	if err != nil || entry.MachineID != "shared-a" || entry.SessionID != "" {
		t.Fatalf("Refresh(ses_older) = %+v, %v; want the older build's record, naming no key", entry, err)
	}
	if _, state := registry.Lookup("ses_older"); state != Live {
		t.Fatalf("Lookup(ses_older) = %s, want live", state)
	}
	if entry, err := registry.Refresh(ctx, "ses_bound"); err != nil || entry.MachineID != "edge-a" || entry.SessionID != "ses_bound" || !slices.Equal(entry.Projects, []string{"PRIV"}) {
		t.Fatalf("Refresh(ses_bound) = %+v, %v; want its record, naming its key", entry, err)
	}
	if ids := listed(t, registry); !slices.Equal(ids, []string{"ses_bound", "ses_older"}) {
		t.Fatalf("the registry lists %v, want both sessions", ids)
	}
}

// A1 and N1 at the store: the server answers a JetStream API request (a stream's info) and a pull
// request (a 404 status with no data) onto two sessions' keys. The status answer is no JSON at
// all, which the registry reads as no record. The info answer names no key, as an older build's
// record does, so it is read as it always has been: a record naming no machine, no registration
// time and no key, whose session is gone and of no project.
func TestAJetStreamAnswerAndAnEmptyRecordReadAbsent(t *testing.T) {
	client := setupNATS(t)
	registry := openBindingRegistry(t, client)
	for _, key := range []string{"ses_info", "ses_status"} {
		if err := registry.Put(key, SessionEntry{MachineID: "edge-a", Title: key, Projects: []string{"PRIV"}}); err != nil {
			t.Fatalf("register %s: %v", key, err)
		}
		// The control: each reads live before the server writes over it.
		if entry, err := registry.Refresh(context.Background(), key); err != nil || entry.MachineID != "edge-a" {
			t.Fatalf("Refresh(%s) before the write = %+v, %v; want its record", key, entry, err)
		}
	}

	info := testnats.ServerWritesKey(t, client.Conn, bus.SessionBucket, "ses_info", testnats.StreamInfo("KV_"+bus.SessionBucket), nil)
	request, data := testnats.NoWaitPull(t, client.Conn, bus.SessionBucket)
	status := testnats.ServerWritesKey(t, client.Conn, bus.SessionBucket, "ses_status", request, data)
	if value := storedValue(t, client.Conn, "ses_status"); len(value) != 0 {
		t.Fatalf("the pull's status answer stored %q under ses_status, want an empty record", value)
	}
	awaitApplied(t, registry, "ses_info", info)
	awaitApplied(t, registry, "ses_status", status)

	requireAbsent(t, registry, "ses_status", "a pull status answer onto its key")
	if entry, state := registry.Lookup("ses_info"); state != Gone || entry.MachineID != "" || entry.SessionID != "" || entry.Projects != nil {
		t.Errorf("Lookup(ses_info) = %+v, %s; want a record naming nothing, gone, as before", entry, state)
	}
}

// A newer build may add a field to the record. A reader that refused fields it does not know would
// read every session that build registers as gone for the length of a rolling deploy, so a
// record naming its key decodes live whatever else it carries.
func TestANewerBuildsFieldDoesNotHideARecord(t *testing.T) {
	client := setupNATS(t)
	registry := openBindingRegistry(t, client)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open %s: %v", bus.SessionBucket, err)
	}
	value := []byte(`{"session_id":"ses_newer","machine_id":"edge-a","updated_at":` + strconv.FormatInt(time.Now().UnixMilli(), 10) +
		`,"projects":["PRIV"],"a_field_a_newer_build_writes":{"nested":[1,2]}}`)
	if _, err := kv.Put("ses_newer", value); err != nil {
		t.Fatalf("store ses_newer: %v", err)
	}
	if decoded, err := DecodeEntry("ses_newer", value); err != nil || decoded.MachineID != "edge-a" {
		t.Fatalf("DecodeEntry(ses_newer) = %+v, %v; want the record", decoded, err)
	}
	entry, err := registry.Refresh(context.Background(), "ses_newer")
	if err != nil || entry.MachineID != "edge-a" || !slices.Equal(entry.Projects, []string{"PRIV"}) || entry.SessionID != "ses_newer" {
		t.Fatalf("Refresh(ses_newer) = %+v, %v; want its record", entry, err)
	}
	if _, state := registry.Lookup("ses_newer"); state != Live {
		t.Fatalf("Lookup(ses_newer) = %s, want live", state)
	}
}
