package consumernames

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/holdrecord"
)

// malformedValue is the value the selection bucket holds under key, read as the readers read it
// (statusSelectionKeys), nil while the key holds none.
func malformedValue(t *testing.T, nc *nats.Conn, key string) []byte {
	t.Helper()
	keys, err := statusSelectionKeys(t.Context(), nc)
	require.NoError(t, err)
	for _, k := range keys {
		if k.Key == key {
			return k.Value
		}
	}
	return nil
}

// malformedReplyStore stores the server's STREAM.INFO answer of the notification stream under key
// of the selection bucket, as nc, by naming the key's subject as the request's reply subject, as
// any principal whose grant holds a JetStream API request can, and answers the value the key then
// holds (storeStreamInfoAnswer, read on nc).
func malformedReplyStore(t *testing.T, nc *nats.Conn, key string) []byte {
	t.Helper()
	return storeStreamInfoAnswer(t, nc, key, func() []byte { return malformedValue(t, nc, key) })
}

// storeStreamInfoAnswer stores the server's STREAM.INFO answer of the notification stream under key
// of the selection bucket, as as, by naming the key's subject as the request's reply subject, and
// answers the value the key then holds, as read reads it, once it holds the answer.
func storeStreamInfoAnswer(t *testing.T, as *nats.Conn, key string, read func() []byte) []byte {
	t.Helper()
	require.NoError(t, as.PublishRequest("$JS.API.STREAM.INFO."+notifications, "$KV."+selection.SessionBucket+"."+key, nil))
	require.NoError(t, as.Flush())
	var value []byte
	require.Eventually(t, func() bool {
		value = read()
		return bytes.Contains(value, []byte(`"io.nats.jetstream.api.v1.stream_info_response"`))
	}, 10*time.Second, 50*time.Millisecond, "the STREAM.INFO answer never reached %s", key)
	return value
}

// malformedCase is one malformed value of the selection bucket a row plants: the key it plants
// under, what status prints for that key, plant (which stores it as nc and answers the value the
// key then holds), and lenient, which shows that a lenient decode (json.Unmarshal) takes the
// value, so a reader on it would act on what it reads.
type malformedCase struct {
	name, key, statusLine string
	plant                 func(t *testing.T, nc *nats.Conn) []byte
	lenient               func(t *testing.T, value []byte)
}

// malformedCases are the three malformed values the rows plant, victim being a session key that
// holds a record and source another: a STREAM.INFO answer under victim, which a lenient decode
// (json.Unmarshal) reads as an empty record that selects nothing; one under the sentinel, which it
// reads as an empty store state; and source's well-formed record copied under victim, which shape
// alone cannot refuse, so only a record that carries its own key lets a reader refuse it (the
// library's check, selection.DecodeKVValue, which readSelectionValue calls).
func malformedCases(victim, source string) []malformedCase {
	recordLine := "record " + familySession + " " + victim + ": UNKNOWN, its value is malformed"
	return []malformedCase{{
		name: "a STREAM.INFO answer under a session's key", key: victim, statusLine: recordLine,
		plant: func(t *testing.T, nc *nats.Conn) []byte { return malformedReplyStore(t, nc, victim) },
		lenient: func(t *testing.T, value []byte) {
			var record selection.Record
			require.NoError(t, json.Unmarshal(value, &record), "control: today's decode refused the answer")
			require.Equal(t, selection.Record{}, record, "control: today's decode reads the answer as a record that selects nothing")
		},
	}, {
		name: "a STREAM.INFO answer under the sentinel", key: selection.SentinelKey,
		statusLine: "store " + familySession + " (" + selectionStreamName + "): sentinel UNKNOWN, its value is malformed",
		plant:      func(t *testing.T, nc *nats.Conn) []byte { return malformedReplyStore(t, nc, selection.SentinelKey) },
		lenient: func(t *testing.T, value []byte) {
			var state selection.StoreState
			require.NoError(t, json.Unmarshal(value, &state), "control: today's decode refused the answer")
			require.Equal(t, selection.StoreState{}, state, "control: today's decode reads the answer as an empty store state")
		},
	}, {
		name: "a record of another key copied under a session's key", key: victim, statusLine: recordLine,
		plant: func(t *testing.T, nc *nats.Conn) []byte {
			value := malformedValue(t, nc, source)
			require.NotNil(t, value, "%s holds no record to copy", source)
			statusPublishKV(t, nc, victim, value, "")
			return malformedValue(t, nc, victim)
		},
		lenient: func(t *testing.T, value []byte) {
			var record selection.Record
			require.NoError(t, json.Unmarshal(value, &record), "control: today's decode refused the copy")
			require.NotEmpty(t, record.Current, "control: today's decode reads the copy as %s's record", source)
		},
	}}
}

// malformedRefused requires that the readers' one decode (readSelectionValue) refuses value under
// key: the check each row's command answers on.
func malformedRefused(t *testing.T, key string, value []byte) {
	t.Helper()
	_, err := readSelectionValue(statusKey{Key: key, Value: value})
	require.Error(t, err, "the readers' decode took the value under %s", key)
}

// The readers' decode (readSelectionValue, through selection.DecodeKVValue) takes exactly what the
// library writes under a key and nothing else. It takes a record json.Marshal wrote with every
// field set (an open operation, a last floor, retired names, a restore), the empty record of its
// key, and the bucket's sentinel; a barrier key holding its hold id. It refuses a STREAM.INFO-shaped
// document, null, an array, a string, nothing at all, data after the object, another key's record,
// a record carrying no key, an operation without its reserved_at, a sentinel of another family, a
// barrier holding another hold's id, and a barrier key whose id is two tokens. Control: a lenient
// decode (json.Unmarshal) takes every refused value that is one JSON object, so each refusal is the
// readers' decode's own.
func TestTheStrictDecodeTakesOnlyAnExactValue(t *testing.T) {
	full := selection.Record{
		Key: "ses_a", Current: "session~a", Start: 4, Retired: []string{"session~r"},
		Op:        &selection.Op{Kind: selection.OpReplace, Owner: "o", Candidate: "session~n", Old: "session~a", ReservedAt: time.Unix(1, 0).UTC()},
		LastFloor: &selection.Floor{Floor: 3, Name: "session~a", StreamID: "s"},
		Restore:   &selection.Restore{Target: "s-a", Start: 2, Phase: "planned"},
	}
	for _, record := range []selection.Record{full, {Key: "ses_b"}} {
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		got, err := readSelectionValue(statusKey{Key: record.Key, Value: raw})
		require.NoError(t, err, "the readers' decode refused %s", raw)
		require.Equal(t, &record, got.record)
	}
	raw, err := json.Marshal(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 7, StreamID: "s", Orphans: []string{"x"}}))
	require.NoError(t, err)
	got, err := readSelectionValue(statusKey{Key: selection.SentinelKey, Value: raw})
	require.NoError(t, err)
	require.NotNil(t, got.state)
	require.NoError(t, strictBarrier("hold.barrier.h1", []byte("h1")))

	for _, value := range []string{
		`{"type":"io.nats.jetstream.api.v1.stream_info_response","config":{"name":"ENVOY_NOTIFICATIONS"}}`,
		`null`, `[]`, `"session~a"`, ``,
		`{"key":"ses_x","current":"session~a"} {}`,
		`{"key":"ses_y","current":"session~a"}`,
		`{"current":"session~a"}`,
		`{"key":"ses_x","op":{"kind":"create","candidate":"session~n"}}`,
	} {
		_, err := readSelectionValue(statusKey{Key: "ses_x", Value: []byte(value)})
		require.Error(t, err, "the readers' decode took %q", value)
		var lenient selection.Record
		if strings.HasPrefix(value, "{") && !strings.Contains(value, "} {") {
			require.NoError(t, json.Unmarshal([]byte(value), &lenient), "control: today's decode refused %q", value)
		}
	}
	for _, value := range []string{
		`{"type":"io.nats.jetstream.api.v1.stream_info_response"}`,
		`{"key":"envoy.bucket","family":"dispatch","cutover":"done"}`,
	} {
		_, err := readSelectionValue(statusKey{Key: selection.SentinelKey, Value: []byte(value)})
		require.Error(t, err, "the readers' decode took the sentinel %q", value)
	}
	require.Error(t, strictBarrier("hold.barrier.h1", []byte("h2")), "a barrier holding another hold's id")
	require.Error(t, strictBarrier("hold.barrier.a.b", []byte("a.b")), "a barrier key of two tokens")
}

// malformedSessions writes, as the operator on nc, a record for each of sessions selecting a
// session consumer of its own, created as the listener's fresh mode creates it (censusSessionConsumer),
// and answers each session's consumer.
func malformedSessions(t *testing.T, nc *nats.Conn, sessions ...string) map[string]string {
	t.Helper()
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	consumers := map[string]string{}
	for _, sessionID := range sessions {
		name := selection.FreshName(sessionFreshPrefix)
		censusSessionConsumer(t, nc, sessionID, name, bus.SessionConsumerFilters([]string{contracts.LegacyAgentSubject(sessionID)}))
		_, err := store.Write(t.Context(), sessionID, selection.Record{Current: name, Start: 1}, 0)
		require.NoError(t, err)
		consumers[sessionID] = name
	}
	return consumers
}

// For each malformed value of the selection bucket (malformedCases: a STREAM.INFO answer stored
// under a session's key or under the sentinel through a request's reply subject, and one session's
// record copied under another's key), streams census is UNKNOWN, exit 2, naming the session
// family's store and the key, and removes nothing: both sessions' consumers and a stray
// listener-hub~<nuid> stay, and its stop entry records only this hold's barrier key, which it put
// before it read the records. Control: a lenient decode (json.Unmarshal) takes each value
// (malformedCase.lenient), and a census on it removes a session's consumer or runs complete and
// removes the stray (shown by an overlay run of a lenient decode). The copied record is refused by
// the key rule of the selection package's records (readSelectionValue).
func TestTheCensusIsUnknownOnAMalformedSelectionValue(t *testing.T) {
	for _, c := range malformedCases("ses_b", "ses_a") {
		t.Run(c.name, func(t *testing.T) {
			h := newStack(t)
			ctx := t.Context()
			id := h.heldHold(holdrecord.WorkflowStreamsCensus)
			nc, js := h.operator()
			censusNotificationStream(t, js)
			_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
			require.NoError(t, err)
			consumers := malformedSessions(t, nc, "ses_a", "ses_b")
			stray := selection.FreshName(bus.ListenerDurable("hub"))
			censusListenerDurable(t, nc, bus.ListenerDurable("hub"), stray)
			_, err = h.records().PutCutover(ctx, familySession, selection.CutoverDone, id)
			require.NoError(t, err)
			value := c.plant(t, nc)
			c.lenient(t, value)

			code, stdout, stderr := h.censusCommand(id, "--family", familySession, "--listener-machine-id", "hub")
			require.Equal(t, 2, code, "the census on a malformed value: %s%s", stdout, stderr)
			require.Contains(t, stderr, "UNKNOWN")
			require.Contains(t, stderr, familySession+"'s store "+selection.SessionBucket)
			require.Contains(t, stderr, c.key)
			require.NotContains(t, stdout, "CHANGED")
			last := h.lastEntry(id, "")
			require.Equal(t, censusCommandName+" stop", last.Command+" "+last.State)
			require.Equal(t, barrierLeft, last.Outputs["left"], "the stop records the barrier key the census put, and nothing more")
			nc, _ = h.operator()
			left := censusConsumers(t, nc)
			for _, name := range []string{consumers["ses_a"], consumers["ses_b"], stray} {
				require.Contains(t, left, name, "the UNKNOWN census removed %s", name)
			}
			malformedRefused(t, c.key, value)
		})
	}
}

// For each malformed value (malformedCases), status, with the row released, reports that key
// UNKNOWN and decodes nothing of it: it prints the key as UNKNOWN (the sentinel's on its store
// line), still prints the session record that is well formed, and exits 2 naming the key. Control:
// a lenient decode (json.Unmarshal) takes each value (malformedCase.lenient), and status on it
// prints the value as a record or a store state and exits 0 (shown by an overlay run of a lenient
// decode). The copied record is refused by the key rule of the selection package's records
// (readSelectionValue).
func TestStatusReportsAMalformedSelectionValueUnknown(t *testing.T) {
	for _, c := range malformedCases("ses_b", "ses_a") {
		t.Run(c.name, func(t *testing.T) {
			h := newStack(t)
			ctx := t.Context()
			h.database(familyDispatch, dispatchMigrations...)
			id := h.heldHold(holdrecord.WorkflowStreamsCensus)
			nc, js := h.operator()
			censusNotificationStream(t, js)
			_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
			require.NoError(t, err)
			consumers := malformedSessions(t, nc, "ses_a", "ses_b")
			value := c.plant(t, nc)
			c.lenient(t, value)
			h.censusDone(id)
			require.NoError(t, h.release(id))

			code, stdout, stderr := h.run(actionStatus, "--dispatch-database-file", "dispatch.url")
			require.Equal(t, 2, code, "status on a malformed value: %s%s", stdout, stderr)
			require.Contains(t, stderr, "UNKNOWN")
			require.Contains(t, stderr, c.key+": its value in "+selectionStreamName+" is malformed")
			require.Contains(t, stdout, c.statusLine)
			if c.key != "ses_a" {
				require.Contains(t, stdout, "record "+familySession+" ses_a: current "+consumers["ses_a"]+" (on the stream)", "status printed no well-formed record")
			}
			if c.key == selection.SentinelKey {
				require.NotContains(t, stdout, "cutover=", "status printed the malformed sentinel as a store state")
			} else {
				require.NotContains(t, stdout, "record "+familySession+" "+c.key+": current", "status printed the malformed value as a record")
			}
			malformedRefused(t, c.key, value)
		})
	}
}

// For each malformed value (malformedCases), cleanup in a consumer-cleanup hold is UNKNOWN, exit 2,
// naming the key, and deletes nothing: ses_a's retired session~<nuid>, which the server holds,
// stays on the stream and in ses_a's record, and its stop entry records only this hold's barrier
// key, which it put before the walk (TestACleanupStopNamesTheBarrierKeyItPut).
// Control: a lenient decode (json.Unmarshal) takes each value (malformedCase.lenient), and cleanup
// on it deletes that retired name (shown by an overlay run of a lenient decode). The copied record
// is refused by the key rule of the selection package's records (readSelectionValue).
func TestCleanupRefusesAMalformedSelectionValue(t *testing.T) {
	for _, c := range malformedCases("ses_b", "ses_a") {
		t.Run(c.name, func(t *testing.T) {
			h := newStack(t)
			ctx := t.Context()
			h.database(familyDispatch, dispatchMigrations...)
			id := h.heldHold(holdrecord.WorkflowConsumerCleanup)
			nc, js := h.operator()
			censusNotificationStream(t, js)
			_, err := selection.CreateSessionBucket(ctx, nc, 1, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 1})
			require.NoError(t, err)
			consumers := malformedSessions(t, nc, "ses_a", "ses_b")
			retired := selection.FreshName(sessionFreshPrefix)
			censusSessionConsumer(t, nc, "ses_a", retired, bus.SessionConsumerFilters([]string{contracts.LegacyAgentSubject("ses_a")}))
			kv := selection.NewKVStore(selection.ConnRequester{Conn: nc})
			_, revision, err := kv.Read(ctx, "ses_a")
			require.NoError(t, err)
			_, err = kv.Write(ctx, "ses_a", selection.Record{Current: consumers["ses_a"], Start: 1, Retired: []string{retired}}, revision)
			require.NoError(t, err)
			value := c.plant(t, nc)
			c.lenient(t, value)

			code, stdout, stderr := h.run(actionCleanup, "--hold", id, "--dispatch-database-file", "dispatch.url")
			require.Equal(t, 2, code, "cleanup on a malformed value: %s%s", stdout, stderr)
			require.Contains(t, stderr, "UNKNOWN")
			require.Contains(t, stderr, c.key)
			require.NotContains(t, stdout, "deleted")
			last := h.lastEntry(id, "")
			require.Equal(t, "consumer-names cleanup stop", last.Command+" "+last.State, "the UNKNOWN cleanup ends in its stop entry")
			// An UNKNOWN exit's stop records its left as unknown, naming what it read and the
			// value it could not read, then the run's own account, which is what it was before.
			left := last.Outputs["left"]
			require.True(t, strings.HasPrefix(left, leftUnknownPrefix), "the UNKNOWN cleanup's stop records its left as unknown: %q", left)
			require.Contains(t, left, "unread the values of "+c.key, "the stop names the value it could not read")
			require.True(t, strings.HasSuffix(left, "this run deleted no consumer and wrote no record; it put "+barrierLeft),
				"the stop records a deletion or a record, or leaves out the barrier key: %q", left)
			nc, _ = h.operator()
			require.Contains(t, censusConsumers(t, nc), retired, "the refused cleanup deleted ses_a's retired name")
			record, _, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).Read(ctx, "ses_a")
			require.NoError(t, err)
			require.Equal(t, []string{retired}, record.Retired, "the refused cleanup changed ses_a's record")
			malformedRefused(t, c.key, value)
		})
	}
}

// For each malformed value (malformedCases), restore --prepare in a rollback hold is UNKNOWN, exit
// 2, naming the key, and changes nothing but this hold's barrier key, which it put before it
// listed the bucket (TestARestorePrepareStopNamesTheBarrierKeyItPut): its stop entry records only
// that key, and session keyed's record holds no restore. Control: a lenient decode
// (json.Unmarshal) takes each value (malformedCase.lenient), and restore --prepare on it plans the
// rollback and writes keyed's restore (shown by an overlay run of a lenient decode). The copied
// record is refused by the key rule of the selection package's records (readSelectionValue).
func TestRestorePrepareRefusesAMalformedSelectionValue(t *testing.T) {
	for _, c := range malformedCases("other", "keyed") {
		t.Run(c.name, func(t *testing.T) {
			rig := newRestoreRig(t)
			var conversions []restoreConversion
			for _, session := range []string{"keyed", "other"} {
				subject := "notifications.agent." + session
				seqs := rig.publish(subject, 3)
				rig.fixed(restoreFixedConfig(sessionFixedPrefix+session, subject), 1)
				conversions = append(conversions, restoreConversion{fixed: sessionFixedPrefix + session, start: seqs[1]})
			}
			rig.cutover(conversions...)
			rollback := rig.heldHold(holdrecord.WorkflowRollback)
			nc, _ := rig.operator()
			value := c.plant(t, nc)
			c.lenient(t, value)

			code, stdout, stderr := rig.restore(rollback, "--prepare")
			require.Equal(t, 2, code, "restore --prepare on a malformed value: %s%s", stdout, stderr)
			require.Contains(t, stderr, "UNKNOWN")
			require.Contains(t, stderr, c.key)
			last := rig.lastEntry(rollback, restoreCommandPrepare)
			require.Equal(t, "stop", last.State, "the refused restore --prepare ends in its stop entry")
			left := last.Outputs["left"]
			require.True(t, strings.HasPrefix(left, leftUnknownPrefix), "the UNKNOWN restore --prepare's stop records its left as unknown: %q", left)
			require.Contains(t, left, "unread the values of "+c.key, "the stop names the value it could not read")
			require.True(t, strings.HasSuffix(left, restoreNoBarrier), "the stop records a record or a consumer, or leaves out the barrier key: %q", left)
			require.Nil(t, rig.record(familySession, "keyed").Restore, "the refused restore --prepare wrote keyed's restore")
			malformedRefused(t, c.key, value)
		})
	}
}
