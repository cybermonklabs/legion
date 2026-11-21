package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// The rows below put under a key of the selection bucket a value that is not that key's record,
// in each of the ways a principal holding a JetStream API grant can, and hold the listener to
// failing closed for that key alone: a session's registration is refused 503 consumer_error
// naming the key, nothing is created or bound for it, and /healthz names the key, while another
// session is served; the sentinel's key holding such a value is a missing sentinel. Each row's
// control is today's lenient decode of the same bytes (json.Unmarshal into the record), which
// takes them for a record the library would act on.

// awaitKeyHolds waits until key of the selection bucket holds data, and returns it.
func awaitKeyHolds(t *testing.T, conn *natsgo.Conn, key string, holds func([]byte) bool) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(ctx, consumernames.SessionBucket)
	if err != nil {
		t.Fatal(err)
	}
	for {
		entry, err := kv.Get(ctx, key)
		if err == nil && holds(entry.Value()) {
			return entry.Value()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the key %s of %s never held the value the row put there (last read: %v)", key, consumernames.SessionBucket, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// storeStreamInfoAnswer has the server store its answer to a STREAM.INFO request under key: the
// request names the key's subject as its reply, and the bucket's stream stores what the server
// sends there. It returns the bytes the key then holds.
func storeStreamInfoAnswer(t *testing.T, conn *natsgo.Conn, key string) []byte {
	t.Helper()
	if err := conn.PublishRequest("$JS.API.STREAM.INFO.KV_"+consumernames.SessionBucket, "$KV."+consumernames.SessionBucket+"."+key, nil); err != nil {
		t.Fatal(err)
	}
	return awaitKeyHolds(t, conn, key, func(value []byte) bool { return bytes.Contains(value, []byte("stream_info_response")) })
}

// copyStoredMessage copies the last message on subject from in stream, its bytes and its headers,
// onto key of the selection bucket, as a principal holding a direct get of from and a publish on
// the key's subject can. It drops the stored Nats-Expected-* headers (a record's write carries its
// expected last sequence), as such a principal would, since the server refuses a publish whose
// expectation the target key does not meet. It returns the bytes the key then holds.
func copyStoredMessage(t *testing.T, conn *natsgo.Conn, stream, from, key string) []byte {
	t.Helper()
	request, err := json.Marshal(map[string]string{"last_by_subj": from})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := conn.Request("$JS.API.DIRECT.GET."+stream, request, 5*time.Second)
	if err != nil {
		t.Fatalf("the direct get of %s from %s: %v", from, stream, err)
	}
	if status := stored.Header.Get("Status"); status != "" {
		t.Fatalf("the direct get of %s from %s answered status %s", from, stream, status)
	}
	header := natsgo.Header{}
	for name, values := range stored.Header {
		if !strings.HasPrefix(name, "Nats-Expected-") {
			header[name] = values
		}
	}
	if err := conn.PublishMsg(&natsgo.Msg{Subject: "$KV." + consumernames.SessionBucket + "." + key, Header: header, Data: stored.Data}); err != nil {
		t.Fatal(err)
	}
	return awaitKeyHolds(t, conn, key, func(value []byte) bool { return bytes.Equal(value, stored.Data) })
}

// freshDelivery is a listener's session-consumer connection in the fresh naming mode on uri over
// a selection bucket whose cutover is done, and register asks it for sessionID's consumer as a
// registration does.
func freshDelivery(t *testing.T, uri string) (*sessionDelivery, func(sessionID string) (*deliveryRefusal, sessionDeliveryOutcome)) {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithConsumerNames(consumernames.ModeFresh))
	if err != nil {
		t.Fatalf("connect the listener's client: %v", err)
	}
	t.Cleanup(client.Close)
	delivery, err := connectSessionDelivery([]string{uri}, bus.SessionConsumerInactiveThreshold, client, logging.NewWithWriter("test", io.Discard))
	if err != nil {
		t.Fatalf("open the session-consumer connection: %v", err)
	}
	t.Cleanup(delivery.close)
	return delivery, func(sessionID string) (*deliveryRefusal, sessionDeliveryOutcome) {
		outcome, refusal, unlock := delivery.consumerFor(context.Background(), listenerwire.SubscribeRequest{
			SessionID: sessionID,
			Delivery:  &listenerwire.SubscribeDelivery{Consumer: true},
		}, []string{contracts.LegacyAgentSubject(sessionID)})
		unlock()
		return refusal, outcome
	}
}

// selectionHub is a server whose selection bucket's cutover is done, and an admin connection to it.
func selectionHub(t *testing.T) (string, *natsgo.Conn) {
	t.Helper()
	_, uri := testnats.Start(t)
	admin := testnats.Connect(t, uri)
	t.Cleanup(admin.Close)
	if _, err := consumernames.CreateSessionBucket(context.Background(), admin, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1}); err != nil {
		t.Fatal(err)
	}
	return uri, admin
}

// consumersOf is every consumer of ENVOY_NOTIFICATIONS that belongs to sessionID: its fixed name, or
// a name carrying it as its logical name.
func consumersOf(t *testing.T, conn *natsgo.Conn, sessionID string) []string {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range js.ConsumerNames(bus.Stream) {
		info, err := js.ConsumerInfo(bus.Stream, name)
		if name == "s-"+sessionID || (err == nil && info.Config.Metadata[consumernames.LogicalMetadataKey] == sessionID) {
			names = append(names, name)
		}
	}
	return names
}

// requireRefusedForKeyAlone holds the listener to the fail-closed answer for key: the registration
// is refused 503 consumer_error naming the key and the bucket, the stream holds no consumer of it,
// /healthz names it, and another session is then served while /healthz still names key.
func requireRefusedForKeyAlone(t *testing.T, admin *natsgo.Conn, delivery *sessionDelivery, register func(string) (*deliveryRefusal, sessionDeliveryOutcome), key, other string) {
	t.Helper()
	refusal, outcome := register(key)
	if refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.Reason != deliveryReasonConsumerError ||
		!strings.Contains(refusal.Error, key) || !strings.Contains(refusal.Error, consumernames.SessionBucket) {
		t.Fatalf("the registration of %s, whose key holds no record of its own, answered %+v (outcome %+v); want 503 %s naming the key %s of %s",
			key, refusal, outcome.answer, deliveryReasonConsumerError, key, consumernames.SessionBucket)
	}
	if names := consumersOf(t, admin, key); len(names) != 0 {
		t.Fatalf("the stream holds %v for %s after the refusal, want no consumer", names, key)
	}
	ctx := context.Background()
	if health := delivery.consumers.SelectionHealth(ctx); !errors.Is(health, consumernames.ErrMalformedRecord) || !strings.Contains(health.Error(), key) {
		t.Fatalf("the selection health after the refusal = %v, want %v naming the key %s", health, consumernames.ErrMalformedRecord, key)
	}
	refusal, outcome = register(other)
	if refusal != nil || outcome.answer == nil || outcome.answer.Mode != deliveryModeConsumer {
		t.Fatalf("the registration of another session answered %+v (refusal %+v), want consumer delivery", outcome.answer, refusal)
	}
	if health := delivery.consumers.SelectionHealth(ctx); health == nil || !strings.Contains(health.Error(), key) {
		t.Fatalf("after another session was served, the selection health = %v, want it still to name %s", health, key)
	}
}

// A session key holding the server's own STREAM.INFO answer, stored there through a request's
// reply subject. Today's decode takes it for a record that selects no consumer and saves no
// start, from which Ensure creates one at the first-create rule.
func TestAFreshSessionKeyHoldingAStreamInfoAnswerIsRefusedForThatSessionAlone(t *testing.T) {
	uri, admin := selectionHub(t)
	const key = "ses_stream_info_key"
	stored := storeStreamInfoAnswer(t, admin, key)
	var lenient consumernames.Record
	if err := json.Unmarshal(stored, &lenient); err != nil || lenient.Current != "" || lenient.Op != nil || lenient.Start != 0 {
		t.Fatalf("control: today's decode of the stored answer = %+v, %v; want a record that selects nothing, with no error", lenient, err)
	}
	delivery, register := freshDelivery(t, uri)
	requireRefusedForKeyAlone(t, admin, delivery, register, key, "ses_beside_the_stream_info_key")
}

// A session key holding a message of another bucket, the listener lease of KV_envoy_listener,
// copied whole, bytes and headers, as a principal holding a direct get of the lease key can.
// Today's decode takes it for a record that selects nothing, from which Ensure creates one.
func TestAFreshSessionKeyHoldingACopiedLeaseMessageIsRefusedForThatSessionAlone(t *testing.T) {
	uri, admin := selectionHub(t)
	ctx := context.Background()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	leases, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "envoy_listener"})
	if err != nil {
		t.Fatal(err)
	}
	// The lease's body is a stand-in of the deployment's lease: what the row holds is that a
	// message of another bucket is no record, whatever its body.
	if _, err := leases.Put(ctx, "lease.listener-a", []byte(`{"holder":"listener-a","renewed_at":"2026-11-21T12:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	const key = "ses_lease_copy_key"
	stored := copyStoredMessage(t, admin, "KV_envoy_listener", "$KV.envoy_listener.lease.listener-a", key)
	var lenient consumernames.Record
	if err := json.Unmarshal(stored, &lenient); err != nil || lenient.Current != "" || lenient.Op != nil || lenient.Start != 0 {
		t.Fatalf("control: today's decode of the copied lease = %+v, %v; want a record that selects nothing, with no error", lenient, err)
	}
	delivery, register := freshDelivery(t, uri)
	requireRefusedForKeyAlone(t, admin, delivery, register, key, "ses_beside_the_lease_copy_key")
}

// A session key holding another session's record, a well-formed record of key X copied whole
// under key Y. Today's decode takes it for Y's record and binds Y to X's consumer.
func TestAFreshSessionKeyHoldingAnotherSessionsRecordIsRefusedForThatSessionAlone(t *testing.T) {
	uri, admin := selectionHub(t)
	delivery, register := freshDelivery(t, uri)
	const owner, key = "ses_record_owner", "ses_record_copy_key"
	refusal, outcome := register(owner)
	if refusal != nil || outcome.answer == nil || outcome.answer.Mode != deliveryModeConsumer {
		t.Fatalf("the registration of %s answered %+v (refusal %+v), want consumer delivery", owner, outcome.answer, refusal)
	}
	ownerConsumers := consumersOf(t, admin, owner)
	if len(ownerConsumers) != 1 {
		t.Fatalf("the stream holds %v for %s, want one consumer", ownerConsumers, owner)
	}
	stored := copyStoredMessage(t, admin, "KV_"+consumernames.SessionBucket, "$KV."+consumernames.SessionBucket+"."+owner, key)
	var lenient consumernames.Record
	if err := json.Unmarshal(stored, &lenient); err != nil || lenient.Current != ownerConsumers[0] {
		t.Fatalf("control: today's decode of the copied record = %+v, %v; want %s's record, naming its consumer %s", lenient, err, owner, ownerConsumers[0])
	}
	requireRefusedForKeyAlone(t, admin, delivery, register, key, "ses_beside_the_record_copy_key")
	if refusal, outcome := register(owner); refusal != nil || outcome.answer == nil || outcome.answer.Mode != deliveryModeConsumer {
		t.Fatalf("the owner %s registering again answered %+v (refusal %+v), want consumer delivery", owner, outcome.answer, refusal)
	}
	if names := consumersOf(t, admin, owner); len(names) != 1 || names[0] != ownerConsumers[0] {
		t.Fatalf("the owner %s holds %v after the refusal of the copy, want its one consumer %s", owner, names, ownerConsumers[0])
	}
}

// The sentinel's key holding the server's STREAM.INFO answer is a missing sentinel: a registration
// is refused 503 consumer_error with nothing created, and the selection health is ErrNoSentinel.
// Today's decode takes the answer for a sentinel with no cutover marker, so the store read as one
// whose cutover has not run rather than one whose sentinel is lost.
func TestAFreshSentinelKeyHoldingAStreamInfoAnswerIsAMissingSentinel(t *testing.T) {
	uri, admin := selectionHub(t)
	stored := storeStreamInfoAnswer(t, admin, consumernames.SentinelKey)
	var lenient consumernames.StoreState
	if err := json.Unmarshal(stored, &lenient); err != nil || lenient.Cutover != "" {
		t.Fatalf("control: today's decode of the stored answer = %+v, %v; want a sentinel with no cutover marker, with no error", lenient, err)
	}
	delivery, register := freshDelivery(t, uri)
	const sessionID = "ses_after_the_stream_info_sentinel"
	refusal, outcome := register(sessionID)
	if refusal == nil || refusal.status != http.StatusServiceUnavailable || refusal.Reason != deliveryReasonConsumerError {
		t.Fatalf("a registration over a sentinel holding a STREAM.INFO answer answered %+v (outcome %+v), want 503 %s", refusal, outcome.answer, deliveryReasonConsumerError)
	}
	if names := consumersOf(t, admin, sessionID); len(names) != 0 {
		t.Fatalf("the stream holds %v for %s after the refusal, want no consumer", names, sessionID)
	}
	if health := delivery.consumers.SelectionHealth(context.Background()); !errors.Is(health, consumernames.ErrNoSentinel) {
		t.Fatalf("the selection health = %v, want %v", health, consumernames.ErrNoSentinel)
	}
}
