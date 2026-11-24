package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
)

// A value under a key is that key's record only when it is one JSON object of a Record's fields
// and nothing else, carries the key it belongs to, and every field it sets is well formed. Anything
// else is ErrMalformedRecord: any principal holding a JetStream API grant can have the server store
// its answer under any key of the bucket through a request's reply subject, or copy another key's
// message there whole. The control of each malformed value is today's decode, json.Unmarshal into
// a Record, which takes every one of them that is JSON for a record.
func TestDecodeRecordTakesOnlyTheRecordOfTheKeyItRead(t *testing.T) {
	const key = "ses_reader"
	reserved := `"reserved_at":"2026-11-21T12:00:00Z"`
	for _, value := range []string{
		`{"key":"ses_reader"}`,
		`{"key":"ses_reader","current":"session~A","start":5,"last_floor":{"floor":4,"name":"session~A","stream_id":"stream"},"retired":["session~B"]}`,
		`{"key":"ses_reader","current":"s-ses_reader"}`,
		`{"key":"ses_reader","current":"session~A","op":{"kind":"replace","owner":"o","candidate":"session~C","old":"session~A",` + reserved + `}}`,
		`{"key":"ses_reader","op":{"kind":"create","candidate":"session~C","native":"new",` + reserved + `}}`,
		`{"key":"ses_reader","current":"session~A","op":{"kind":"delete","old":"session~A","floor_recorded":true,` + reserved + `}}`,
		`{"key":"ses_reader","restore":{"target":"s-ses_reader","start":3,"phase":"planned"}}`,
		` {"key":"ses_reader"} ` + "\n",
	} {
		t.Run("takes "+value, func(t *testing.T) {
			record, err := DecodeRecord(key, []byte(value))
			if err != nil || record.Key != key {
				t.Fatalf("DecodeRecord(%q) = %+v, %v; want the record of %s", value, record, err, key)
			}
		})
	}
	for name, value := range map[string]string{
		"a STREAM.INFO answer":       `{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0,"config":{"name":"KV_envoy_session_consumers"},"state":{"messages":1}}`,
		"a lease of another bucket":  `{"holder":"listener-a","renewed_at":"2026-11-21T12:00:00Z"}`,
		"another key's record":       `{"key":"ses_other","current":"session~A","start":5}`,
		"a record carrying no key":   `{"current":"session~A","start":5}`,
		"an empty key":               `{"key":"","current":"session~A"}`,
		"a field no record has":      `{"key":"ses_reader","currnet":"session~A"}`,
		"an op field no op has":      `{"key":"ses_reader","op":{"kind":"create","candidate":"session~C","extra":1,` + reserved + `}}`,
		"data after the record":      `{"key":"ses_reader"} {"key":"ses_reader"}`,
		"bytes after the record":     `{"key":"ses_reader"}x`,
		"a field of the wrong type":  `{"key":"ses_reader","start":"5"}`,
		"an unknown op kind":         `{"key":"ses_reader","op":{"kind":"rename","candidate":"session~C",` + reserved + `}}`,
		"an op reserved at no time":  `{"key":"ses_reader","op":{"kind":"create","candidate":"session~C"}}`,
		"a create with no candidate": `{"key":"ses_reader","op":{"kind":"create",` + reserved + `}}`,
		"a replace with no old":      `{"key":"ses_reader","op":{"kind":"replace","candidate":"session~C",` + reserved + `}}`,
		"a delete with no old":       `{"key":"ses_reader","op":{"kind":"delete",` + reserved + `}}`,
		"an unknown native policy":   `{"key":"ses_reader","op":{"kind":"create","candidate":"session~C","native":"last",` + reserved + `}}`,
		"a current no consumer has":  `{"key":"ses_reader","current":"session A"}`,
		"a dotted current":           `{"key":"ses_reader","current":"session.A"}`,
		"an empty retired name":      `{"key":"ses_reader","retired":[""]}`,
		"a last floor of no name":    `{"key":"ses_reader","last_floor":{"floor":4,"stream_id":"stream"}}`,
		"a restore of no target":     `{"key":"ses_reader","restore":{"start":3,"phase":"planned"}}`,
		"a restore of no phase":      `{"key":"ses_reader","restore":{"target":"s-ses_reader","start":3}}`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := DecodeRecord(key, []byte(value)); !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("DecodeRecord(%q) = %v, want ErrMalformedRecord", value, err)
			}
		})
	}
	for name, value := range map[string]string{"null": "null", "an array": "[]", "a string": `"x"`, "nothing": "", "a number": "5"} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := DecodeRecord(key, []byte(value)); !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("DecodeRecord(%q) = %v, want ErrMalformedRecord", value, err)
			}
		})
	}
	t.Run("control: today's decode takes a STREAM.INFO answer and another key's record", func(t *testing.T) {
		for _, value := range []string{
			`{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0,"config":{"name":"KV_envoy_session_consumers"}}`,
			`{"key":"ses_other","current":"session~A","start":5}`,
		} {
			var record Record
			if err := json.Unmarshal([]byte(value), &record); err != nil {
				t.Fatalf("json.Unmarshal(%q) = %v, want today's lenient decode to take it", value, err)
			}
		}
	})
}

// The sentinel is the sentinel key's own StoreState, read as strictly as a record: anything else
// under that key is a missing sentinel, never a sentinel with no cutover marker. It names its
// store's family, which a sentinel without one, with one of no form, or with another store's fails:
// Key alone cannot tell two Postgres stores apart. The controls are today's decode, which takes a
// STREAM.INFO answer for a sentinel whose cutover has not run, and takes a sentinel that names no
// family.
func TestDecodeSentinelTakesOnlyTheSentinelOfItsKey(t *testing.T) {
	for value, family := range map[string]string{
		`{"key":"envoy.bucket","family":"session"}`:                                                                                                     SessionFamily,
		`{"key":"envoy.bucket","family":"session","cutover":"done","boundary":1,"stream_id":"stream"}`:                                                  SessionFamily,
		`{"key":"envoy.bucket","family":"dispatch","cutover":"rolled_back","replay_from":7,"orphans":["session~A"],"backup_sha256":"none","hold":"h1"}`: DispatchFamily,
		`{"key":"envoy.bucket","family":"daemon/legion-go"}`:                                                                                            daemonFamily(t, "legion-go"),
	} {
		t.Run("takes "+value, func(t *testing.T) {
			state, err := DecodeSentinel(SentinelKey, family, []byte(value))
			if err != nil || state.Key != SentinelKey || state.Family != family {
				t.Fatalf("DecodeSentinel(%q) = %+v, %v; want the sentinel of the family %s", value, state, err, family)
			}
		})
	}
	for name, value := range map[string]string{
		"a STREAM.INFO answer":          `{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0,"config":{"name":"KV_envoy_session_consumers"}}`,
		"a sentinel carrying no key":    `{"family":"session","cutover":"done","boundary":1}`,
		"a sentinel naming no family":   `{"key":"envoy.bucket","cutover":"done","boundary":1}`,
		"an empty family":               `{"key":"envoy.bucket","family":"","cutover":"done"}`,
		"a family of no form":           `{"key":"envoy.bucket","family":"sessions","cutover":"done"}`,
		"a daemon family of no project": `{"key":"envoy.bucket","family":"daemon/","cutover":"done"}`,
		"a daemon project with a slash": `{"key":"envoy.bucket","family":"daemon/a/b","cutover":"done"}`,
		"a record of a session":         `{"key":"ses_a","current":"session~A"}`,
		"another store's sentinel":      `{"key":"envoy.sentinel","family":"session","cutover":"done"}`,
		"another family's sentinel":     `{"key":"envoy.bucket","family":"dispatch","cutover":"done"}`,
		"a daemon's sentinel":           `{"key":"envoy.bucket","family":"daemon/legion-go","cutover":"done"}`,
		"an unknown cutover marker":     `{"key":"envoy.bucket","family":"session","cutover":"started"}`,
		"an orphan no consumer has":     `{"key":"envoy.bucket","family":"session","orphans":["session A"]}`,
		"data after the sentinel":       `{"key":"envoy.bucket","family":"session"}{}`,
		"null":                          `null`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := DecodeSentinel(SentinelKey, SessionFamily, []byte(value)); !errors.Is(err, ErrMalformedSentinel) || errors.Is(err, ErrNoSentinel) {
				t.Fatalf("DecodeSentinel(%q) = %v, want ErrMalformedSentinel", value, err)
			}
		})
	}
	t.Run("control: today's decode takes a STREAM.INFO answer for a sentinel with no marker", func(t *testing.T) {
		var state StoreState
		if err := json.Unmarshal([]byte(`{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0}`), &state); err != nil || state.Cutover != "" {
			t.Fatalf("json.Unmarshal = %+v, %v; want a sentinel with no marker, taken", state, err)
		}
	})
	t.Run("control: today's decode takes a sentinel that names no family", func(t *testing.T) {
		var state StoreState
		if err := json.Unmarshal([]byte(`{"key":"envoy.bucket","cutover":"done","boundary":1}`), &state); err != nil || state.Family != "" || state.Cutover != CutoverDone {
			t.Fatalf("json.Unmarshal = %+v, %v; want a done sentinel with no family, taken", state, err)
		}
	})
}

// A sentinel's family round-trips through CreateSessionBucket and the KV store: CreateSessionBucket
// names the session family alone, and State reads it back. (The Postgres store's half is
// consumernames/pgstore's row of this name.)
func TestASentinelsFamilyRoundTrips(t *testing.T) {
	ctx := context.Background()
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	if _, err := CreateSessionBucket(ctx, conn, 1, StoreState{Family: DispatchFamily, Cutover: CutoverDone}); err == nil || !strings.Contains(err.Error(), SessionFamily) {
		t.Fatalf("CreateSessionBucket with the family %s = %v, want a refusal naming %s", DispatchFamily, err, SessionFamily)
	}
	kv, err := CreateSessionBucket(ctx, conn, 1, StoreState{Cutover: CutoverDone, Boundary: 1})
	if err != nil {
		t.Fatal(err)
	}
	if state, err := kv.State(ctx); err != nil || state.Family != SessionFamily {
		t.Fatalf("the session bucket's sentinel read back as %+v, %v; want the family %s", state, err, SessionFamily)
	}
}

// sentinelStore is a store's sentinel, as each store reads and writes it.
type sentinelStore interface {
	State(ctx context.Context) (StoreState, error)
	WriteSentinel(ctx context.Context, state StoreState, revision uint64) (uint64, error)
}

// The KV store's WriteSentinel writes its own family itself: a state that names no family is
// written naming the store's, and one that names another family is refused, naming both, with
// nothing written, so no caller writes another family's sentinel, or one its store cannot read, by
// mistake. Before, WriteSentinel wrote whatever family its caller gave: a caller that forgot the
// family wrote a sentinel its own store then refused to read, and one that named another family
// wrote that family's. The control is the family-less sentinel put under the key as bytes, which
// the store refuses to read. (The Postgres store's half is consumernames/pgstore's row of this
// name.)
func TestWriteSentinelWritesItsStoresOwnFamily(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, store sentinelStore, family, key string, put func(data []byte)) {
		revision, err := store.WriteSentinel(ctx, StoreState{Cutover: CutoverDone, Boundary: 1}, 0)
		if err != nil {
			t.Fatalf("WriteSentinel of a state naming no family = %v", err)
		}
		if state, err := store.State(ctx); err != nil || state.Family != family || state.Cutover != CutoverDone {
			t.Errorf("the sentinel written with no family read back as %+v, %v; want the store's family %s, done", state, err, family)
		}
		_, err = store.WriteSentinel(ctx, StoreState{Family: DispatchFamily, Cutover: CutoverRolledBack}, revision)
		if !errors.Is(err, ErrSentinelFamily) || !strings.Contains(err.Error(), `"`+DispatchFamily+`"`) || !strings.Contains(err.Error(), `"`+family+`"`) {
			t.Errorf("WriteSentinel of another family's state = %v; want ErrSentinelFamily naming both families", err)
		}
		if state, err := store.State(ctx); err != nil || state.Cutover == CutoverRolledBack {
			t.Errorf("after the refused write the sentinel reads %+v, %v; want the one written before", state, err)
		}
		data, err := json.Marshal(StoreState{Key: key, Cutover: CutoverDone})
		if err != nil {
			t.Fatal(err)
		}
		put(data)
		if state, err := store.State(ctx); !errors.Is(err, ErrMalformedSentinel) {
			t.Errorf("control: a sentinel under the key that names no family read as %+v, %v; want ErrMalformedSentinel", state, err)
		}
	}
	t.Run("KVStore", func(t *testing.T) {
		live := newLiveStore(t, testnats.URL(t), nil)
		run(t, live.store, SessionFamily, SentinelKey, func(data []byte) {
			if _, err := live.js.Publish(ctx, "$KV."+SessionBucket+"."+SentinelKey, data); err != nil {
				t.Fatal(err)
			}
		})
	})
}

// The KV store reads only a sentinel that names the session family: a key alone cannot tell two
// stores apart, so a well-formed sentinel naming another family, or one naming none, under the
// store's sentinel key is a malformed sentinel, never a marker read. The control is a sentinel of
// its own family, read. Without the family, a store read another store's sentinel as its own:
// Dispatch's store took a daemon's done marker, which a creator in the fixed mode would obey. (The
// Postgres store's half is consumernames/pgstore's row of this name.)
func TestEachStoreReadsOnlyTheSentinelOfItsOwnFamily(t *testing.T) {
	ctx := context.Background()
	others := []string{DispatchFamily, daemonFamily(t, "legion-go")}
	refused := func(t *testing.T, store, family string, state StoreState, err error) {
		t.Helper()
		if !errors.Is(err, ErrMalformedSentinel) || errors.Is(err, ErrNoSentinel) {
			t.Fatalf("%s reading a sentinel of the family %q = %+v, %v; want ErrMalformedSentinel, never a missing sentinel", store, family, state, err)
		}
		if family != "" && (!errors.Is(err, ErrSentinelFamily) || !strings.Contains(err.Error(), family)) {
			t.Fatalf("%s reading a sentinel of the family %q = %v; want ErrSentinelFamily naming it", store, family, err)
		}
	}
	for _, other := range append(others, "") {
		t.Run("KVStore of session, a sentinel of "+other, func(t *testing.T) {
			live := newLiveStore(t, testnats.URL(t), nil)
			// WriteSentinel writes the session family alone, so another family's sentinel is put
			// under the key as bytes, as a principal with a write of the bucket can.
			data, err := json.Marshal(StoreState{Key: SentinelKey, Family: other, Cutover: CutoverDone, Boundary: 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := live.js.Publish(ctx, "$KV."+SessionBucket+"."+SentinelKey, data); err != nil {
				t.Fatal(err)
			}
			state, err := live.store.State(ctx)
			refused(t, "KVStore of session", other, state, err)
		})
	}
	live := newLiveStore(t, testnats.URL(t), &StoreState{Cutover: CutoverDone, Boundary: 1})
	if state, err := live.store.State(ctx); err != nil || state.Family != SessionFamily {
		t.Fatalf("control: KVStore of session reading its own family's sentinel = %+v, %v", state, err)
	}
}

// A sentinel key that holds anything but the KV store's own exact sentinel is never a missing
// sentinel: a creator in the fixed mode reads ErrNoSentinel as no marker (Dispatch and the daemons
// do), so such a value must be a failed read, which never runs fixed names over a store a newer
// build may have converted, and it still refuses every call in the fresh mode. The values are
// another family's sentinel, a sentinel under another key, one carrying a field this build does
// not know (as a newer operator build's could), and the server's STREAM.INFO answer. The control is
// the absent sentinel, the one missing sentinel. (The Postgres store's half, on a real Postgres
// too, is consumernames/pgstore's row of this name.)
func TestAMalformedSentinelIsNeverNoMarker(t *testing.T) {
	ctx := context.Background()
	// noMarker is how a fixed-mode creator reads a store's sentinel: ErrNoSentinel is no marker.
	noMarker := func(err error) bool { return errors.Is(err, ErrNoSentinel) }
	malformed := func(key, family string) map[string]string {
		return map[string]string{
			"another family's sentinel":        fmt.Sprintf(`{"key":%q,"family":"daemon/legion-other","cutover":"done"}`, key),
			"a sentinel under another key":     fmt.Sprintf(`{"key":"envoy.sentinel/daemon/legion-other","family":%q,"cutover":"done"}`, family),
			"a field this build does not know": fmt.Sprintf(`{"key":%q,"family":%q,"cutover":"done","epoch":2}`, key, family),
			"a STREAM.INFO answer":             `{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0}`,
		}
	}
	refused := func(t *testing.T, row string, err error) {
		t.Helper()
		if noMarker(err) || !errors.Is(err, ErrMalformedSentinel) || !IsSelectionStateLost(err) {
			t.Fatalf("State over %s = %v; want ErrMalformedSentinel, which is no missing sentinel and refuses every call", row, err)
		}
	}
	absent := func(t *testing.T, err error) {
		t.Helper()
		if !noMarker(err) {
			t.Fatalf("control: State with no sentinel row = %v, want ErrNoSentinel", err)
		}
	}
	t.Run("KVStore", func(t *testing.T) {
		live := newLiveStore(t, testnats.URL(t), nil)
		_, err := live.store.State(ctx)
		absent(t, err)
		for row, data := range malformed(SentinelKey, SessionFamily) {
			if _, err := live.js.Publish(ctx, "$KV."+SessionBucket+"."+SentinelKey, []byte(data)); err != nil {
				t.Fatal(err)
			}
			_, err := live.store.State(ctx)
			refused(t, row, err)
		}
	})
}

// A daemon's family is made only for a project a store can keep, so no store reads, writes or
// lists the rows of a family it does not keep: DaemonFamily and CheckStoreFamily refuse any other,
// as ErrInvalidFamily naming it. Before, a store of "daemon", with no project, listed every
// project's records, each well formed under its own key, and a store of daemon/p/b wrote rows
// that daemon/p's store then listed as its own. The control is the acceptance of every family a
// store keeps. (The Postgres store's constructor's half is consumernames/pgstore's row of this
// name.)
func TestAStoreIsMadeOnlyForAFamilyItCanKeep(t *testing.T) {
	for _, family := range []string{"daemon", "daemon/", "daemon/p/b", "daemon/p q", "", "sessions", "dispatch/x"} {
		if err := CheckStoreFamily(family); !errors.Is(err, ErrInvalidFamily) || !strings.Contains(err.Error(), fmt.Sprintf("%q", family)) {
			t.Fatalf("CheckStoreFamily(%q) = %v; want ErrInvalidFamily naming it", family, err)
		}
	}
	for _, project := range []string{"", "p/b", "p q"} {
		if family, err := DaemonFamily(project); !errors.Is(err, ErrInvalidFamily) || !strings.Contains(err.Error(), fmt.Sprintf("%q", "daemon/"+project)) || family != "" {
			t.Fatalf("DaemonFamily(%q) = %q, %v; want ErrInvalidFamily naming daemon/%s", project, family, err, project)
		}
	}
	for _, family := range []string{SessionFamily, DispatchFamily, "daemon/p", "daemon/legion-go_a.b"} {
		if err := CheckStoreFamily(family); err != nil {
			t.Fatalf("control: CheckStoreFamily(%q) = %v; want it kept", family, err)
		}
	}
	if family, err := DaemonFamily("legion-go"); err != nil || family != "daemon/legion-go" {
		t.Fatalf("control: DaemonFamily(legion-go) = %q, %v; want daemon/legion-go", family, err)
	}
}

// daemonFamily is DaemonFamily(project), failing the test when DaemonFamily refuses it.
func daemonFamily(t testing.TB, project string) string {
	t.Helper()
	family, err := DaemonFamily(project)
	if err != nil {
		t.Fatal(err)
	}
	return family
}
