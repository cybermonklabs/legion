package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
)

// errorClass is the class of a decode's answer: none, a malformed record or a malformed sentinel.
func errorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, ErrMalformedSentinel):
		return "malformed sentinel"
	case errors.Is(err, ErrMalformedRecord):
		return "malformed record"
	}
	return "other: " + err.Error()
}

// DecodeKVValue is the KV store's own decode: every value the store's Read and State refuse under
// a key, DecodeKVValue refuses under that key with the same class of error, naming the key, and
// every record and sentinel they read, it reads the same. The values are the ones the store's own
// rows cover: another key's record, another family's sentinel, a sentinel under another key, the
// server's STREAM.INFO answer under a session key and under the sentinel's, another bucket's
// lease message, and a newer build's field in a record and in a sentinel; a sound record and a
// sound sentinel beside them. The control is a copy of the decode that skips the key check: it
// takes another key's record and a sentinel under another key, so it disagrees with the store.
func TestDecodeKVValueIsTheKVStoresDecode(t *testing.T) {
	live := newLiveStore(t, testnats.URL(t), nil)
	ctx := context.Background()
	record := func(key string, extra string) string {
		return `{"key":"` + key + `","start":7` + extra + `}`
	}
	rows := []struct {
		name, key, value, class string
	}{
		{"a sound record", "ses_sound", record("ses_sound", ""), "none"},
		{"a sound sentinel", SentinelKey, `{"key":"envoy.bucket","family":"session","cutover":"done","boundary":1}`, "none"},
		{"another key's record", "ses_copy", record("ses_owner", ""), "malformed record"},
		{"another family's sentinel", SentinelKey, `{"key":"envoy.bucket","family":"dispatch","cutover":"done"}`, "malformed sentinel"},
		{"a sentinel under another key", SentinelKey, `{"key":"envoy.sentinel/dispatch","family":"session","cutover":"done"}`, "malformed sentinel"},
		{"a STREAM.INFO answer under a session key", "ses_info", `{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0}`, "malformed record"},
		{"a STREAM.INFO answer under the sentinel's key", SentinelKey, `{"type":"io.nats.jetstream.api.v1.stream_info_response","total":0}`, "malformed sentinel"},
		{"another bucket's lease message", "ses_lease", `{"holder":"listener-a","renewed_at":"2026-11-21T12:00:00Z"}`, "malformed record"},
		{"a newer build's field in a record", "ses_newer", record("ses_newer", `,"epoch":2`), "malformed record"},
		{"a newer build's field in a sentinel", SentinelKey, `{"key":"envoy.bucket","family":"session","cutover":"done","epoch":2}`, "malformed sentinel"},
	}
	// skipKeyCheck is the control: the decode with no check that the value is its key's own.
	skipKeyCheck := func(key string, data []byte) (Record, StoreState, bool, error) {
		if key == SentinelKey {
			var state StoreState
			if err := decodeStrict(data, &state); err != nil {
				return Record{}, StoreState{}, true, errors.Join(ErrMalformedSentinel, err)
			}
			if state.Family != SessionFamily {
				return Record{}, StoreState{}, true, errors.Join(ErrMalformedSentinel, ErrSentinelFamily)
			}
			return Record{}, state, true, nil
		}
		var rec Record
		if err := decodeStrict(data, &rec); err != nil {
			return Record{}, StoreState{}, false, errors.Join(ErrMalformedRecord, err)
		}
		return rec, StoreState{}, false, nil
	}
	disagreements := map[string][]string{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := live.js.Publish(ctx, "$KV."+SessionBucket+"."+row.key, []byte(row.value)); err != nil {
				t.Fatal(err)
			}
			var data []byte
			var err error
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				if data, _, err = live.store.last(ctx, row.key); err == nil && string(data) == row.value || time.Now().After(deadline) {
					break
				}
			}
			if string(data) != row.value {
				t.Fatalf("the key %s never held the row's value: %q, %v", row.key, data, err)
			}
			var storeRecord Record
			var storeState StoreState
			var storeErr error
			if row.key == SentinelKey {
				storeState, storeErr = live.store.State(ctx)
			} else {
				storeRecord, _, storeErr = live.store.Read(ctx, row.key)
			}
			if got := errorClass(storeErr); got != row.class {
				t.Fatalf("the store's read of %s = %s (%v); want %s", row.name, got, storeErr, row.class)
			}
			for name, decode := range map[string]func(string, []byte) (Record, StoreState, bool, error){
				"DecodeKVValue": DecodeKVValue, "control": skipKeyCheck,
			} {
				rec, state, isSentinel, err := decode(row.key, data)
				var differs []string
				if errorClass(err) != row.class {
					differs = append(differs, "class "+errorClass(err))
				}
				if err != nil && name == "DecodeKVValue" && !strings.Contains(err.Error(), row.key) {
					differs = append(differs, "no key in "+err.Error())
				}
				if isSentinel != (row.key == SentinelKey) {
					differs = append(differs, "isSentinel")
				}
				if err == nil {
					a, _ := json.Marshal(rec)
					b, _ := json.Marshal(storeRecord)
					c, _ := json.Marshal(state)
					d, _ := json.Marshal(storeState)
					if string(a) != string(b) || string(c) != string(d) {
						differs = append(differs, "value "+string(a)+string(c))
					}
				}
				if len(differs) > 0 {
					disagreements[name] = append(disagreements[name], row.name+": "+strings.Join(differs, ", "))
				}
			}
		})
	}
	if got := disagreements["DecodeKVValue"]; len(got) > 0 {
		t.Errorf("DecodeKVValue disagrees with the store on %d rows: %v", len(got), got)
	}
	control := strings.Join(disagreements["control"], "; ")
	if !strings.Contains(control, "another key's record") || !strings.Contains(control, "a sentinel under another key") {
		t.Errorf("control: the decode that skips the key check disagrees with the store on %q; want it to on another key's record and a sentinel under another key", control)
	}
}
