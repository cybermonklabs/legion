package consumernames

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

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
			record, err := decodeRecord(key, []byte(value))
			if err != nil || record.Key != key {
				t.Fatalf("decodeRecord(%q) = %+v, %v; want the record of %s", value, record, err, key)
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
			if _, err := decodeRecord(key, []byte(value)); !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("decodeRecord(%q) = %v, want ErrMalformedRecord", value, err)
			}
		})
	}
	for name, value := range map[string]string{"null": "null", "an array": "[]", "a string": `"x"`, "nothing": "", "a number": "5"} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := decodeRecord(key, []byte(value)); !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("decodeRecord(%q) = %v, want ErrMalformedRecord", value, err)
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
// store's family, which a sentinel without one, or with one of no form, fails: Key alone cannot
// tell two Postgres stores apart. The controls are today's decode, which takes a STREAM.INFO answer
// for a sentinel whose cutover has not run, and takes a sentinel that names no family.
func TestDecodeSentinelTakesOnlyTheSentinelOfItsKey(t *testing.T) {
	for value, family := range map[string]string{
		`{"key":"envoy.bucket","family":"session"}`:                                                                                                     SessionFamily,
		`{"key":"envoy.bucket","family":"session","cutover":"done","boundary":1,"stream_id":"stream"}`:                                                  SessionFamily,
		`{"key":"envoy.bucket","family":"dispatch","cutover":"rolled_back","replay_from":7,"orphans":["session~A"],"backup_sha256":"none","hold":"h1"}`: DispatchFamily,
		`{"key":"envoy.bucket","family":"daemon/legion-go"}`:                                                                                            DaemonFamily("legion-go"),
	} {
		t.Run("takes "+value, func(t *testing.T) {
			state, err := decodeSentinel(SentinelKey, []byte(value))
			if err != nil || state.Key != SentinelKey || state.Family != family {
				t.Fatalf("decodeSentinel(%q) = %+v, %v; want the sentinel of the family %s", value, state, err, family)
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
		"an unknown cutover marker":     `{"key":"envoy.bucket","family":"session","cutover":"started"}`,
		"an orphan no consumer has":     `{"key":"envoy.bucket","family":"session","orphans":["session A"]}`,
		"data after the sentinel":       `{"key":"envoy.bucket","family":"session"}{}`,
		"null":                          `null`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := decodeSentinel(SentinelKey, []byte(value)); !errors.Is(err, ErrMalformedRecord) {
				t.Fatalf("decodeSentinel(%q) = %v, want ErrMalformedRecord", value, err)
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

// A sentinel's family round-trips through both stores and CreateSessionBucket: what the caller
// gives is what State reads back, and CreateSessionBucket names the session family alone.
func TestASentinelsFamilyRoundTrips(t *testing.T) {
	ctx := context.Background()
	db := &fakePG{rows: map[string]pgRow{}, written: map[string][]byte{}}
	store := NewPGStore(db)
	for _, family := range []string{DispatchFamily, DaemonFamily("legion-go")} {
		if _, err := store.WriteSentinel(ctx, StoreState{Family: family, Cutover: CutoverDone}, 0); err != nil {
			t.Fatal(err)
		}
		db.rows[PGSentinelKey] = pgRow{data: db.written[PGSentinelKey], revision: 1}
		if state, err := store.State(ctx); err != nil || state.Family != family {
			t.Fatalf("PGStore's sentinel written with the family %s read back as %+v, %v", family, state, err)
		}
	}
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

// pgRow is one row of a fake PGQuerier: the record's bytes and the revision, or err.
type pgRow struct {
	data     []byte
	revision int64
	err      error
}

func (r pgRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	switch len(dest) {
	case 1:
		*dest[0].(*int64) = r.revision
	case 2:
		*dest[0].(*[]byte) = r.data
		*dest[1].(*int64) = r.revision
	}
	return nil
}

// fakePG answers every select from rows by logical name and records the record each write sends.
type fakePG struct {
	rows    map[string]pgRow
	written map[string][]byte
}

func (f *fakePG) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	logical := args[0].(string)
	if strings.HasPrefix(strings.TrimSpace(sql), "select") {
		row, ok := f.rows[logical]
		if !ok {
			return pgRow{err: pgx.ErrNoRows}
		}
		return row
	}
	for _, arg := range args[1:] {
		if data, ok := arg.([]byte); ok {
			f.written[logical] = data
		}
	}
	return pgRow{revision: 1}
}

// PGStore reads and writes the same self-keyed records: a write stamps the record and the sentinel
// with their own keys, and a read refuses a row whose record is another key's or no record at all,
// as KVStore does.
func TestThePGStoreWritesAndReadsOnlySelfKeyedRecords(t *testing.T) {
	ctx := context.Background()
	db := &fakePG{rows: map[string]pgRow{}, written: map[string][]byte{}}
	store := NewPGStore(db)
	if _, err := store.Write(ctx, "legion-go-CAPTURE-wake", Record{Current: "legion-go-CAPTURE-wake~A"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WriteSentinel(ctx, StoreState{Family: DaemonFamily("legion-go"), Cutover: CutoverDone}, 0); err != nil {
		t.Fatal(err)
	}
	if got := string(db.written["legion-go-CAPTURE-wake"]); !strings.Contains(got, `"key":"legion-go-CAPTURE-wake"`) {
		t.Fatalf("the written record %s carries no key", got)
	}
	if got := string(db.written[PGSentinelKey]); !strings.Contains(got, `"key":"`+PGSentinelKey+`"`) {
		t.Fatalf("the written sentinel %s carries no key", got)
	}
	db.rows["legion-go-CAPTURE-wake"] = pgRow{data: db.written["legion-go-CAPTURE-wake"], revision: 1}
	db.rows["legion-go-other"] = pgRow{data: db.written["legion-go-CAPTURE-wake"], revision: 4}
	db.rows[PGSentinelKey] = pgRow{data: db.written[PGSentinelKey], revision: 1}
	if record, _, err := store.Read(ctx, "legion-go-CAPTURE-wake"); err != nil || record.Current != "legion-go-CAPTURE-wake~A" {
		t.Fatalf("control: Read of the record's own key = %+v, %v", record, err)
	}
	if state, err := store.State(ctx); err != nil || state.Cutover != CutoverDone {
		t.Fatalf("control: State = %+v, %v", state, err)
	}
	if _, _, err := store.Read(ctx, "legion-go-other"); !errors.Is(err, ErrMalformedRecord) || !strings.Contains(err.Error(), "legion-go-other") {
		t.Fatalf("Read of a row holding another key's record = %v, want ErrMalformedRecord naming legion-go-other", err)
	}
	db.rows[PGSentinelKey] = pgRow{data: []byte(`{"cutover":"done"}`), revision: 2}
	if _, err := store.State(ctx); !errors.Is(err, ErrNoSentinel) {
		t.Fatalf("State of a sentinel row carrying no key = %v, want ErrNoSentinel", err)
	}
}
