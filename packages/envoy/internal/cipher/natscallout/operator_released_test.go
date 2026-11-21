package natscallout

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/kvrecords"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// The operator's grant outside a hold names no subject that writes. It reads every info the hold's
// first step reads, backs up KV_envoy_session_consumers as a records-only copy read message by
// message and signed with the hold key (kvrecords.Write), each walked message with its sequence,
// headers and time, and reads what consumer-names status reads. Of the writes it once held, a
// snapshot delivered to a protected key, and a create of an absent ENVOY_MEASURE whose republish
// targets a protected key followed by a publish the grant holds, are each refused, and both keys
// are unchanged; it creates, changes and deletes no protected stream, its consumer creates are
// refused, and the retired hold bucket is not reachable. Each refused request gets no answer, and
// the server reports the publish it refused, by subject. What the grant can still store is what any
// JetStream request in $G can: the server's JSON answer, on a subject it names as the request's
// reply (natsgrant's operatorReleased says who can store more).
//
// It fails when released's grant holds a write again: run by hand against the grant that held
// STREAM.SNAPSHOT and STREAM.CREATE.ENVOY_MEASURE, both writes reach their keys and the test fails
// naming each. And it fails when released loses a read the records copy needs.
func TestTheReleasedOperatorReadsAndBacksUp(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	adminNC, js := r.admin(t)
	r.makeStreams(t, 20, 1024, 0)
	_, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Name: "session-a", AckPolicy: nats.AckExplicitPolicy, InactiveThreshold: time.Hour})
	require.NoError(t, err)
	// A retired session's record, deleted: its DEL marker is a stored message with headers, which
	// the copy must keep.
	recordsBucket, err := js.KeyValue(sessionConsumers)
	require.NoError(t, err)
	_, err = recordsBucket.Put("s-ses_retired", []byte(`{"name":"session-retired"}`))
	require.NoError(t, err)
	require.NoError(t, recordsBucket.Delete("s-ses_retired"))

	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)

	// The records copy first, so a grant without a read it needs fails the test inside it.
	dir := filepath.Join(t.TempDir(), "consumer-names")
	manifest, err := kvrecords.Write(t.Context(), operator.Conn, sessionConsumers, dir, r.signer.key)
	require.NoError(t, err, "the released operator's records copy of %s; the server sent:\n%s", kvBucket(sessionConsumers), operator.errs)
	_, records, err := kvrecords.Read(dir, r.signer.HoldKeys)
	require.NoError(t, err)
	want := storedMessages(t, adminNC, kvBucket(sessionConsumers))
	require.Equal(t, len(want), manifest.Records)
	require.Len(t, records, len(want), "the copy holds %d of the %d stored messages", len(records), len(want))
	for index, record := range records {
		require.Equal(t, want[index].Seq, record.Seq, "record %d's sequence", index)
		require.Equal(t, want[index].Subject, record.Subject, "sequence %d's subject", record.Seq)
		require.True(t, want[index].Time.Equal(record.Time), "sequence %d stored at %s, the record says %s", record.Seq, want[index].Time, record.Time)
		require.Equal(t, string(want[index].Header), string(record.Headers.Block()), "sequence %d's headers", record.Seq)
		require.Equal(t, string(want[index].Data), string(record.Data), "sequence %d's data", record.Seq)
	}
	require.True(t, slices.ContainsFunc(records, func(record kvrecords.Record) bool { return len(record.Headers) > 0 }),
		"no record carries headers, so the copy's headers went untested")

	succeeds(t, operator.Conn, "$JS.API.INFO", nil)
	for _, stream := range append([]string{bus.Stream, kvBucket(sessionConsumers)}, mapped(copiedBuckets, kvBucket)...) {
		succeeds(t, operator.Conn, "$JS.API.STREAM.INFO."+stream, nil)
	}
	var names struct {
		Consumers []string `json:"consumers"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, operator.Conn, "$JS.API.CONSUMER.NAMES."+bus.Stream, nil).raw, &names))
	require.Contains(t, names.Consumers, "session-a")
	succeeds(t, operator.Conn, "$JS.API.CONSUMER.INFO."+bus.Stream+".session-a", nil)

	// The two writes released's grant once held, each aimed at a protected key of its own, so a
	// failure names which one wrote.
	snapshotVictim, measureVictim := putProtectedKey(t, js, "victim-snapshot"), putProtectedKey(t, js, "victim-measure")
	snapshot := map[string]any{"deliver_subject": snapshotVictim.subject, "no_consumers": true, "chunk_size": snapshotChunk}
	measure := map[string]any{"name": "ENVOY_MEASURE", "subjects": []string{"$JS.API.STREAM.INFO.ENVOY_MEASURE"}, "no_ack": true,
		"storage": "memory", "num_replicas": 1, "republish": map[string]any{"src": "$JS.API.STREAM.INFO.ENVOY_MEASURE", "dest": measureVictim.subject}}
	_, _ = operator.Request("$JS.API.STREAM.SNAPSHOT."+kvBucket(bus.InterestBucket), encoded(t, snapshot), time.Second)
	_, _ = operator.Request("$JS.API.STREAM.CREATE.ENVOY_MEASURE", encoded(t, measure), time.Second)
	require.NoError(t, operator.Publish("$JS.API.STREAM.INFO.ENVOY_MEASURE", []byte("chosen by the released operator")))
	require.NoError(t, operator.Flush())
	snapshotVictim.checkUnchanged(t, js)
	measureVictim.checkUnchanged(t, js)
	if t.Failed() {
		t.FailNow()
	}
	operator.refused(t, "$JS.API.STREAM.SNAPSHOT."+kvBucket(bus.InterestBucket), snapshot)
	operator.refused(t, "$JS.API.STREAM.CREATE.ENVOY_MEASURE", measure)
	absent := answered(t, operator.Conn, "$JS.API.STREAM.INFO.ENVOY_MEASURE", nil)
	require.NotNil(t, absent.Error, "ENVOY_MEASURE exists after the released operator's refused create: %s", absent.raw)
	require.Equal(t, 10059, absent.Error.ErrCode, "the info of the absent ENVOY_MEASURE: %s", absent.raw)

	sessions := streamConfig(t, adminNC, kvBucket(bus.SessionBucket))
	for _, request := range []struct {
		subject string
		body    any
	}{
		{"$JS.API.STREAM.CREATE." + kvBucket(bus.SessionBucket), sessions},
		{"$JS.API.STREAM.CREATE." + kvBucket(bus.LegionAgentBucket), map[string]any{"name": kvBucket(bus.LegionAgentBucket)}},
		{"$JS.API.STREAM.UPDATE." + kvBucket(bus.SessionBucket), sessions},
		{"$JS.API.STREAM.DELETE." + kvBucket(bus.SessionBucket), nil},
		{"$JS.API.STREAM.SNAPSHOT." + kvBucket(sessionConsumers), map[string]any{"deliver_subject": operator.NewRespInbox()}},
		{"$JS.API.CONSUMER.CREATE." + bus.Stream + ".x", map[string]any{"stream_name": bus.Stream, "config": map[string]any{"name": "x"}}},
		{retiredHoldBucketKV, []byte("released")},
	} {
		operator.refused(t, request.subject, request.body)
	}
	info, err := js.StreamInfo(kvBucket(bus.SessionBucket))
	require.NoError(t, err, "the released operator's refused delete removed %s", kvBucket(bus.SessionBucket))
	require.EqualValues(t, 22, info.State.Msgs, "KV_envoy_sessions holds other than its 20 entries and the two victim keys")
}

// The measurement job's grant makes no stream: on a lone server, a user holding exactly
// natsgrant.Measure() is refused the create of ENVOY_MEASURE, whose republish would have targeted
// a key of envoy_sessions, and is refused the publish that stream would have captured; the key is
// unchanged. A create carries a configuration of the caller's choosing, and a republish in it
// writes any subject from the stream's own sender.
//
// Run by hand, the control is Measure() as it was, holding STREAM.CREATE.ENVOY_MEASURE and
// envoy-measure.>: the create is answered, the publish is republished onto the key, and the test
// fails naming the key.
func TestTheMeasureUserMakesNoStream(t *testing.T) {
	adminSeed, adminKey := testnats.User(t)
	measureSeed, measureKey := testnats.User(t)
	config := "jetstream {}\nauthorization {\n  users = [\n    " +
		staticUserEntry(adminKey, natsgrant.Permissions{Publish: []string{">"}, Subscribe: []string{">"}}) + "\n    " +
		staticUserEntry(measureKey, natsgrant.Measure()) + "\n  ]\n}\n"
	_, url := testnats.StartConfig(t, config, []string{"-js"}, false)
	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	js, err := admin.JetStream()
	require.NoError(t, err)
	_, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bus.SessionBucket})
	require.NoError(t, err)
	victim := putProtectedKey(t, js, "victim")

	errs := &serverErrors{}
	measure, err := testnats.ConnectSeed(t, url, measureSeed, nats.CustomInboxPrefix("_INBOX_envoy_measure"), nats.ErrorHandler(errs.handle))
	require.NoError(t, err)
	t.Cleanup(measure.Close)
	o := &operatorConn{Conn: measure, errs: errs}
	create := map[string]any{"name": "ENVOY_MEASURE", "subjects": []string{"envoy-measure.x"}, "storage": "memory", "num_replicas": 1,
		"republish": map[string]any{"src": "envoy-measure.x", "dest": victim.subject}}
	_, _ = measure.Request("$JS.API.STREAM.CREATE.ENVOY_MEASURE", encoded(t, create), time.Second)
	require.NoError(t, measure.Publish("envoy-measure.x", []byte("chosen by the measurement user")))
	require.NoError(t, measure.Flush())
	victim.checkUnchanged(t, js)
	if t.Failed() {
		t.FailNow()
	}
	o.refused(t, "$JS.API.STREAM.CREATE.ENVOY_MEASURE", create)
	want := `Permissions Violation for Publish to "envoy-measure.x"`
	testwait.WithinPolling(t, 5*time.Second, 20*time.Millisecond, want, func() bool { return errs.seen(want) })
	_, err = js.StreamInfo("ENVOY_MEASURE")
	require.ErrorIs(t, err, nats.ErrStreamNotFound, "the measurement user made ENVOY_MEASURE")
}
