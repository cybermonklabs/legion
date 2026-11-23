package kvrecords_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus/kvrecords"
	"github.com/sjawhar/envoy/internal/bus/signedrecord"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// bucket is the one bucket Operator(OperatorPhaseReleased) can read message by message.
const bucket = "envoy_session_consumers"

// userEntry is one nkey user of a server's configuration, holding exactly grant.
func userEntry(nkey string, grant natsgrant.Permissions) string {
	quoted := func(subjects []string) string {
		out := make([]string, len(subjects))
		for index, subject := range subjects {
			out[index] = fmt.Sprintf("%q", subject)
		}
		return strings.Join(out, ", ")
	}
	return fmt.Sprintf("{ nkey: %q, permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }",
		nkey, quoted(grant.Publish), quoted(grant.Subscribe))
}

// rig is an in-process nats-server with an admin user, who may do anything, the operator outside a
// hold, holding exactly Operator(OperatorPhaseReleased), with the operator's hold signing key, whose
// public half keys holds, and a daemon user holding exactly Daemon().
type rig struct {
	admin    *nats.Conn
	js       nats.JetStreamContext
	operator *nats.Conn
	daemon   *nats.Conn
	key      ed25519.PrivateKey
	keys     []ed25519.PublicKey
}

func newRig(t *testing.T) *rig {
	t.Helper()
	adminSeed, adminKey := testnats.User(t)
	operatorSeed, operatorKey := testnats.User(t)
	daemonSeed, daemonKey := testnats.User(t)
	config := "jetstream {}\nauthorization {\n  users = [\n    " +
		userEntry(adminKey, natsgrant.Permissions{Publish: []string{">"}, Subscribe: []string{">"}}) + "\n    " +
		userEntry(operatorKey, natsgrant.Operator(natsgrant.OperatorPhaseReleased)) + "\n    " +
		userEntry(daemonKey, natsgrant.Daemon()) + "\n  ]\n}\n"
	_, url := testnats.StartConfig(t, config, []string{"-js"}, false)
	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	js, err := admin.JetStream()
	require.NoError(t, err)
	operator, err := testnats.ConnectSeed(t, url, operatorSeed, nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(operator.Close)
	daemon, err := testnats.ConnectSeed(t, url, daemonSeed)
	require.NoError(t, err)
	t.Cleanup(daemon.Close)
	public, private := newKey(t)
	return &rig{admin: admin, js: js, operator: operator, daemon: daemon, key: private, keys: []ed25519.PublicKey{public}}
}

// newKey is a fresh Ed25519 key pair.
func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return public, private
}

// stored is one message as the server stores it, read back with STREAM.MSG.GET by the admin.
type stored struct {
	Subject string    `json:"subject"`
	Seq     uint64    `json:"seq"`
	Header  []byte    `json:"hdrs"`
	Data    []byte    `json:"data"`
	Time    time.Time `json:"time"`
}

// storedMessages is every message stream stores, in sequence order, read by the admin.
func (r *rig) storedMessages(t *testing.T, stream string) []stored {
	t.Helper()
	info, err := r.js.StreamInfo(stream)
	require.NoError(t, err)
	var messages []stored
	for seq := max(info.State.FirstSeq, 1); seq <= info.State.LastSeq; seq++ {
		body, _ := json.Marshal(map[string]uint64{"seq": seq})
		msg, err := r.admin.Request("$JS.API.STREAM.MSG.GET."+stream, body, 5*time.Second)
		require.NoError(t, err)
		var answer struct {
			Message *stored `json:"message"`
			Error   *struct {
				ErrCode int `json:"err_code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(msg.Data, &answer))
		if answer.Error != nil {
			require.Equal(t, 10037, answer.Error.ErrCode, "sequence %d: %s", seq, msg.Data)
			continue
		}
		messages = append(messages, *answer.Message)
	}
	return messages
}

// keyState is a key's last stored message, which a bucket reads as the key's value or marker.
type keyState struct {
	operation string
	data      string
}

// keyStates is each key of bucketName and its last stored message, read by the admin.
func (r *rig) keyStates(t *testing.T, bucketName string) map[string]keyState {
	t.Helper()
	states := map[string]keyState{}
	for _, message := range r.storedMessages(t, "KV_"+bucketName) {
		operation := "PUT"
		if header, err := nats.DecodeHeadersMsg(message.Header); err == nil && header.Get("KV-Operation") != "" {
			operation = header.Get("KV-Operation")
		}
		states[strings.TrimPrefix(message.Subject, "$KV."+bucketName+".")] = keyState{operation: operation, data: string(message.Data)}
	}
	return states
}

// restore publishes each of records into the fresh bucket named fresh as a restore does: its key's
// subject there, with its headers and data, in order. Each publish must be accepted.
func (r *rig) restore(t *testing.T, fresh string, records []kvrecords.Record) {
	t.Helper()
	_, err := r.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: fresh})
	require.NoError(t, err)
	for _, record := range records {
		msg := nats.NewMsg("$KV." + fresh + "." + record.Key)
		for _, header := range record.Headers {
			msg.Header[header.Name] = append(msg.Header[header.Name], header.Values...)
		}
		msg.Data = record.Data
		_, err := r.js.PublishMsg(msg)
		require.NoError(t, err, "the restore's publish of sequence %d (%s, headers %v)", record.Seq, record.Key, record.Headers)
	}
}

// fillBucket makes the bucket with every kind of stored message a copy must keep: puts, a put
// replaced by a later one (its sequence no longer stored), a deleted key (a DEL marker), a purged
// key (a PURGE marker, its earlier values rolled up), a listener durable's record durable.d1, the
// sentinel envoy.bucket and a hold's barrier key.
func (r *rig) fillBucket(t *testing.T) {
	t.Helper()
	kv, err := r.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	put := func(key, value string) {
		_, err := kv.Put(key, []byte(value))
		require.NoError(t, err)
	}
	put(kvrecords.SentinelKey, `{"id":"bucket-1"}`)
	put("s-ses_a", `{"name":"consumer-a-1"}`)
	put("s-ses_b", `{"name":"consumer-b"}`)
	put("s-ses_a", `{"name":"consumer-a-2"}`)
	put("s-ses_c", `{"name":"consumer-c"}`)
	require.NoError(t, kv.Delete("s-ses_c"))
	put("s-ses_d", `{"name":"consumer-d-1"}`)
	put("s-ses_d", `{"name":"consumer-d-2"}`)
	require.NoError(t, kv.Purge("s-ses_d"))
	put(kvrecords.DurableKeyPrefix+"d1", `{"current":"listener-d1"}`)
	put(kvrecords.HoldBarrierKeyPrefix+"h1", "")
	put("s-ses_e", "")
}

// A records-only copy, taken by the operator outside a hold and signed with the operator's hold
// key, holds every message the bucket's stream stores, each with the sequence, subject, key, time,
// headers and data the server stores, every dotted key included; read back with the operator's
// public key, it gives them all. ForRestore keeps the puts, both markers and durable.d1, skips the
// sentinel and the barrier key, skips a planted other.k when its caller names it and refuses it
// naming it when not; a restore of its entries gives a fresh bucket the same keys, values, delete
// markers and purge markers, durable.d1 among them.
//
// Controls, each run by hand: a writer that drops headers fails the restore row (the DEL marker
// restores as an empty put); a writer that skips keys holding a dot fails the copy row (the
// sentinel and durable.d1 are missing); a ForRestore that skips every key holding a dot fails the
// restore row (durable.d1 is missing after the restore); a ForRestore that keeps unknown dotted
// keys fails the ForRestore row (other.k is not refused); a ForRestore that ignores its skip list
// fails the ForRestore row (the named other.k is refused); a changed byte in records.jsonl fails the
// copy row, Read refusing it naming SHA256SUMS.
func TestARecordsCopyRoundTrips(t *testing.T) {
	r := newRig(t)
	r.fillBucket(t)
	want := r.storedMessages(t, "KV_"+bucket)
	dir := filepath.Join(t.TempDir(), "copy")

	manifest, err := kvrecords.Write(t.Context(), r.operator, bucket, dir, r.key)
	require.NoError(t, err, "the released operator's copy of %s", bucket)
	info, err := r.js.StreamInfo("KV_" + bucket)
	require.NoError(t, err)
	require.Equal(t, kvrecords.Format, manifest.Format)
	require.Equal(t, info.State.FirstSeq, manifest.FirstSeq)
	require.Equal(t, info.State.LastSeq, manifest.LastSeq)
	require.Greater(t, manifest.LastSeq-manifest.FirstSeq+1, uint64(len(want)), "no sequence was left out, so the 10037 rule went untested")
	durable := kvrecords.DurableKeyPrefix + "d1"

	t.Run("Read gives every stored message", func(t *testing.T) {
		read, records, err := kvrecords.Read(dir, bucket, r.keys, manifest.ManifestSHA256)
		require.NoError(t, err)
		require.Equal(t, manifest, read)
		require.Len(t, records, len(want), "the copy holds %d records of the %d messages stored", len(records), len(want))
		keys := map[string]bool{}
		for index, record := range records {
			message := want[index]
			require.Equal(t, message.Seq, record.Seq, "record %d", index)
			require.Equal(t, message.Subject, record.Subject, "sequence %d", message.Seq)
			require.Equal(t, strings.TrimPrefix(message.Subject, "$KV."+bucket+"."), record.Key, "sequence %d", message.Seq)
			require.True(t, message.Time.Equal(record.Time), "sequence %d stored at %s, the record says %s", message.Seq, message.Time, record.Time)
			require.Equal(t, string(message.Header), string(record.Headers.Block()), "sequence %d's headers", message.Seq)
			require.True(t, bytes.Equal(message.Data, record.Data), "sequence %d's data", message.Seq)
			keys[record.Key] = true
		}
		for _, key := range []string{kvrecords.SentinelKey, kvrecords.HoldBarrierKeyPrefix + "h1", durable, "s-ses_c", "s-ses_d"} {
			require.True(t, keys[key], "the copy holds no record of %s", key)
		}
	})

	t.Run("ForRestore keeps durables and undotted keys, skips system and named keys, refuses the rest", func(t *testing.T) {
		_, records, err := kvrecords.Read(dir, bucket, r.keys, manifest.ManifestSHA256)
		require.NoError(t, err)
		restore, err := kvrecords.ForRestore(records, nil)
		require.NoError(t, err)
		kept := map[string]bool{}
		for _, record := range restore {
			kept[record.Key] = true
		}
		for _, key := range []string{"s-ses_a", "s-ses_b", "s-ses_c", "s-ses_d", "s-ses_e", durable} {
			require.True(t, kept[key], "ForRestore left out %s", key)
		}
		for _, key := range []string{kvrecords.SentinelKey, kvrecords.HoldBarrierKeyPrefix + "h1"} {
			require.False(t, kept[key], "ForRestore kept the system key %s", key)
		}
		planted := append(slices.Clone(records), kvrecords.Record{Seq: manifest.LastSeq + 1, Subject: "$KV." + bucket + ".other.k", Key: "other.k"})
		_, err = kvrecords.ForRestore(planted, nil)
		require.Error(t, err, "ForRestore did not refuse the unknown dotted key other.k")
		require.ErrorContains(t, err, `"other.k"`)
		require.ErrorContains(t, err, fmt.Sprintf("sequence %d", manifest.LastSeq+1))
		_, err = kvrecords.ForRestore(planted, []string{"other.j"})
		require.ErrorContains(t, err, `"other.k"`, "a skip list naming another key let other.k through")
		named, err := kvrecords.ForRestore(planted, []string{"other.k"})
		require.NoError(t, err, "ForRestore refused other.k though its caller named it to skip")
		require.Equal(t, restore, named, "skipping the named other.k changed the other entries")
	})

	t.Run("a restore gives the same keys, values and markers", func(t *testing.T) {
		_, records, err := kvrecords.Read(dir, bucket, r.keys, manifest.ManifestSHA256)
		require.NoError(t, err)
		restore, err := kvrecords.ForRestore(records, nil)
		require.NoError(t, err)
		const fresh = "envoy_session_consumers_restored"
		r.restore(t, fresh, restore)
		original, restored := r.keyStates(t, bucket), r.keyStates(t, fresh)
		require.Equal(t, "DEL", original["s-ses_c"].operation)
		require.Equal(t, "PURGE", original["s-ses_d"].operation)
		delete(original, kvrecords.SentinelKey)
		delete(original, kvrecords.HoldBarrierKeyPrefix+"h1")
		require.Contains(t, original, durable)
		for key, state := range original {
			require.Equal(t, state, restored[key], "the restored %s", key)
		}
		require.Len(t, restored, len(original))
	})
}

// Keys written with a revision restore into a fresh bucket: a key made with kv.Create, one
// updated with kv.Update at its revision, and one deleted at its revision. The server stores the
// Nats-Expected-Last-Subject-Sequence header each of those writes sends, and a fresh stream refuses
// a publish whose expected sequence is not its own; ForRestore's entries carry no such header, so
// every publish is accepted, and the fresh bucket holds the same keys, values and markers.
//
// Run by hand, the control is a ForRestore that keeps every header: the first publish is refused
// "wrong last sequence".
func TestARestoreTakesKeysWrittenWithARevision(t *testing.T) {
	r := newRig(t)
	kv, err := r.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	_, err = kv.Create("s-created", []byte(`{"name":"created"}`))
	require.NoError(t, err)
	revision, err := kv.Create("s-updated", []byte(`{"name":"before"}`))
	require.NoError(t, err)
	_, err = kv.Update("s-updated", []byte(`{"name":"after"}`), revision)
	require.NoError(t, err)
	revision, err = kv.Create("s-deleted", []byte(`{"name":"deleted"}`))
	require.NoError(t, err)
	require.NoError(t, kv.Delete("s-deleted", nats.LastRevision(revision)))

	dir := filepath.Join(t.TempDir(), "copy")
	written, err := kvrecords.Write(t.Context(), r.operator, bucket, dir, r.key)
	require.NoError(t, err)
	_, records, err := kvrecords.Read(dir, bucket, r.keys, written.ManifestSHA256)
	require.NoError(t, err)
	expected := 0
	for _, record := range records {
		if strings.Contains(string(record.Headers.Block()), "Nats-Expected-Last-Subject-Sequence") {
			expected++
		}
	}
	require.Equal(t, len(records), expected, "a write with a revision stored no expected sequence, so the row tests nothing")
	restore, err := kvrecords.ForRestore(records, nil)
	require.NoError(t, err)
	r.restore(t, "envoy_session_consumers_restored", restore)
	original, restored := r.keyStates(t, bucket), r.keyStates(t, "envoy_session_consumers_restored")
	require.Equal(t, keyState{operation: "DEL"}, original["s-deleted"])
	require.Equal(t, original, restored)
}

// captureWalk makes a stream that captures the walk's STREAM.MSG.GET requests of the bucket's
// stream and republishes each onto the bucket's key, inside the server, so a get stores a message
// in the bucket before the walk can ask for its sequence; at most captures of them (0 for every
// one), the stream refusing each past them. It returns the stream's name.
func (r *rig) captureWalk(t *testing.T, key string, captures int64) string {
	t.Helper()
	const name = "WALK_GETS"
	gets := "$JS.API.STREAM.MSG.GET.KV_" + bucket
	config := &nats.StreamConfig{Name: name, Subjects: []string{gets}, NoAck: true, Storage: nats.MemoryStorage,
		RePublish: &nats.RePublish{Source: gets, Destination: "$KV." + bucket + "." + key}}
	if captures > 0 {
		config.MaxMsgs, config.Discard = captures, nats.DiscardNew
	}
	_, err := r.js.AddStream(config)
	require.NoError(t, err)
	return name
}

// A key rewritten while the walk runs is in the copy, with its new value: the bucket holds 2,000
// keys, one value each, and the walk's first get rewrites s-01500 (captureWalk, once), which stores
// it again at sequence 2,001 and removes sequence 1,500 long before the walk reaches it. The walk
// reads the stream's info again after it ends, walks on to the new last_seq, and records that one
// in the manifest.
//
// Run by hand, the control is the single pass, one info read: the copy holds 1,999 records, none of
// s-01500, and the test fails naming it.
func TestAKeyRewrittenDuringTheWalkIsInTheCopy(t *testing.T) {
	r := newRig(t)
	kv, err := r.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	for index := range 2000 {
		_, err := kv.Put(fmt.Sprintf("s-%05d", index+1), []byte("v1"))
		require.NoError(t, err)
	}
	captured := r.captureWalk(t, "s-01500", 1)

	dir := filepath.Join(t.TempDir(), "copy")
	manifest, err := kvrecords.Write(t.Context(), r.operator, bucket, dir, r.key)
	require.NoError(t, err)
	gets, err := r.js.StreamInfo(captured)
	require.NoError(t, err)
	require.EqualValues(t, 1, gets.State.Msgs, "the walk's first get rewrote nothing")
	stored, err := r.js.GetLastMsg("KV_"+bucket, "$KV."+bucket+".s-01500")
	require.NoError(t, err)
	require.EqualValues(t, 2001, stored.Sequence)
	_, records, err := kvrecords.Read(dir, bucket, r.keys, manifest.ManifestSHA256)
	require.NoError(t, err)
	last := slices.IndexFunc(records, func(record kvrecords.Record) bool { return record.Key == "s-01500" })
	require.GreaterOrEqual(t, last, 0, "the copy holds no record of s-01500, rewritten during the walk (%d records, last_seq %d)", len(records), manifest.LastSeq)
	require.Equal(t, string(stored.Data), string(records[last].Data), "the copy holds s-01500's old value")
	require.EqualValues(t, 2001, records[last].Seq)
	require.Len(t, records, 2000)
	require.EqualValues(t, 2001, manifest.LastSeq, "the manifest's last_seq is not the walk's final one")
}

// A message stored under a subject that is no key of the bucket is listed, not copied, and the
// walk goes on: the daemon, holding exactly Daemon(), pulls a notification from ENVOY_NOTIFICATIONS
// with the reply subject $KV.envoy_session_consumers.victim, and the bucket's stream stores it under
// the notification's own subject. The copy then holds a record of every key and none of the
// notification, the manifest lists it under foreign by its sequence and subject, and Read and
// ForRestore take the copy as if it were not there.
//
// Run by hand, the control is the walk that stops on such a message: Write fails with "is stored
// under "notifications.roles.x", which is no key of envoy_session_consumers", and the test fails.
func TestAMessageStoredUnderAForeignSubjectIsListedNotCopied(t *testing.T) {
	r := newRig(t)
	kv, err := r.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	for _, key := range []string{"s-ses_a", "victim", "s-ses_b"} {
		_, err := kv.Put(key, []byte(`{"name":"`+key+`"}`))
		require.NoError(t, err)
	}
	_, err = r.js.AddStream(&nats.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}})
	require.NoError(t, err)
	_, err = r.js.AddConsumer("ENVOY_NOTIFICATIONS", &nats.ConsumerConfig{Durable: "daemon-d1", FilterSubject: "notifications.roles.>", AckPolicy: nats.AckExplicitPolicy})
	require.NoError(t, err)
	_, err = r.js.Publish("notifications.roles.x", []byte("a notification"))
	require.NoError(t, err)
	require.NoError(t, r.daemon.PublishRequest("$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.daemon-d1", "$KV."+bucket+".victim", []byte(`{"batch":1}`)))
	require.NoError(t, r.daemon.Flush())
	var foreign uint64
	require.Eventually(t, func() bool {
		info, err := r.js.StreamInfo("KV_" + bucket)
		if err != nil || info.State.LastSeq < 4 {
			return false
		}
		msg, err := r.js.GetMsg("KV_"+bucket, info.State.LastSeq)
		if err != nil || msg.Subject != "notifications.roles.x" {
			return false
		}
		foreign = msg.Sequence
		return true
	}, 5*time.Second, 20*time.Millisecond, "the daemon's pull stored no notification in the bucket's stream, so the row tests nothing")

	dir := filepath.Join(t.TempDir(), "copy")
	manifest, err := kvrecords.Write(t.Context(), r.operator, bucket, dir, r.key)
	require.NoError(t, err, "the walk stopped on the notification stored in the bucket's stream")
	require.Equal(t, []kvrecords.ForeignMessage{{Seq: foreign, Subject: "notifications.roles.x"}}, manifest.Foreign)
	read, records, err := kvrecords.Read(dir, bucket, r.keys, manifest.ManifestSHA256)
	require.NoError(t, err)
	require.Equal(t, manifest, read)
	keys := make([]string, 0, len(records))
	for _, record := range records {
		keys = append(keys, record.Key)
	}
	require.Equal(t, []string{"s-ses_a", "victim", "s-ses_b"}, keys)
	restore, err := kvrecords.ForRestore(records, nil)
	require.NoError(t, err)
	require.Len(t, restore, 3)
}

// A walk that cannot catch the stream fails naming its bound and writes nothing: every get of the
// walk stores one more message in the bucket's stream (captureWalk, with no limit). The bucket
// starts with 1,000 keys, so each pass finds about as many new messages as it walked, far more
// than the few republishes still in flight when it reads the stream's info, and the stream's
// last_seq still moves after the walk's tenth pass.
//
// Run by hand, the control is the walk with no bound: it never ends, and the test fails at its
// one-minute deadline, naming no bound.
func TestAWalkThatCannotCatchTheStreamFailsNamingItsBound(t *testing.T) {
	r := newRig(t)
	kv, err := r.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
	require.NoError(t, err)
	for index := range 1000 {
		_, err := kv.Put(fmt.Sprintf("s-%05d", index+1), []byte("v1"))
		require.NoError(t, err)
	}
	captured := r.captureWalk(t, "s-moved", 0)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "copy")
	_, err = kvrecords.Write(ctx, r.operator, bucket, dir, r.key)
	moved, infoErr := r.js.StreamInfo(captured)
	require.NoError(t, infoErr)
	require.Error(t, err, "the walk ended though the stream never stopped moving (%d gets captured)", moved.State.Msgs)
	require.ErrorContains(t, err, "10 passes", "the walk's error does not name its bound")
	_, statErr := os.Stat(dir)
	require.ErrorIs(t, statErr, fs.ErrNotExist, "the walk past its bound left a copy")
}

// Read refuses a copy that is not what Write wrote, naming what failed, and Write refuses a
// directory that exists already. Every row but the signature's re-signs the manifest it rewrites
// with the operator's key, and is read under that manifest's own name, as if its caller had
// recorded it, so it reaches the check after the name and the signature; the signature's rows
// rewrite the three files so that they agree with each other, as anyone who can write the
// directory can, and do not sign, or sign with another key.
func TestARecordsCopyIsRefusedWhenItIsNotWhatWasWritten(t *testing.T) {
	r := newRig(t)
	r.fillBucket(t)
	source := filepath.Join(t.TempDir(), "copy")
	copied, err := kvrecords.Write(t.Context(), r.operator, bucket, source, r.key)
	require.NoError(t, err)
	_, _, err = kvrecords.Read(source, bucket, r.keys, copied.ManifestSHA256)
	require.NoError(t, err, "the genuine copy")
	_, err = kvrecords.Write(context.Background(), r.operator, bucket, source, r.key)
	require.ErrorContains(t, err, "exists already")

	// nameOf is the name a caller that recorded dir's manifest as it is now would pass, or the
	// genuine copy's when there is no manifest.
	nameOf := func(dir string) string {
		data, err := os.ReadFile(filepath.Join(dir, kvrecords.ManifestFile))
		if err != nil {
			return copied.ManifestSHA256
		}
		return sha(data)
	}
	read := func(t *testing.T, dir, name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return data
	}
	// resum rewrites SHA256SUMS to the files as they are.
	resum := func(t *testing.T, dir string) {
		t.Helper()
		var sums strings.Builder
		for _, name := range []string{kvrecords.ManifestFile, kvrecords.RecordsFile} {
			fmt.Fprintf(&sums, "%s  %s\n", sha(read(t, dir, name)), name)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.SumsFile), []byte(sums.String()), 0o600))
	}
	// resign signs the manifest as it is with key.
	resign := func(t *testing.T, dir string, key ed25519.PrivateKey) {
		t.Helper()
		sig, err := signedrecord.Sign(key, signedrecord.RecordsManifest(read(t, dir, kvrecords.ManifestFile)))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.SignatureFile), []byte(sig+"\n"), 0o600))
	}
	// manifest rewrites the manifest with change applied, its records sum kept true to the file.
	manifest := func(t *testing.T, dir string, change func(*kvrecords.Manifest)) {
		t.Helper()
		var m kvrecords.Manifest
		require.NoError(t, json.Unmarshal(read(t, dir, kvrecords.ManifestFile), &m))
		m.RecordsSHA256 = sha(read(t, dir, kvrecords.RecordsFile))
		m.Records = strings.Count(string(read(t, dir, kvrecords.RecordsFile)), "\n")
		change(&m)
		raw, err := json.MarshalIndent(m, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.ManifestFile), append(raw, '\n'), 0o600))
	}
	// line rewrites records.jsonl's first line with change applied to its fields, and the manifest
	// with it.
	line := func(t *testing.T, dir string, change func(map[string]any)) {
		t.Helper()
		lines := strings.SplitAfter(string(read(t, dir, kvrecords.RecordsFile)), "\n")
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(lines[0]), &fields))
		change(fields)
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		lines[0] = string(raw) + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.RecordsFile), []byte(strings.Join(lines, "")), 0o600))
		manifest(t, dir, func(*kvrecords.Manifest) {})
	}
	// signed is tamper, then the sums and the signature made true to what it wrote.
	signed := func(tamper func(t *testing.T, dir string)) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			tamper(t, dir)
			resum(t, dir)
			resign(t, dir, r.key)
		}
	}
	_, otherKey := newKey(t)
	dropFirstLine := func(t *testing.T, dir string) {
		lines := strings.SplitAfter(string(read(t, dir, kvrecords.RecordsFile)), "\n")
		require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.RecordsFile), []byte(strings.Join(lines[1:], "")), 0o600))
		manifest(t, dir, func(*kvrecords.Manifest) {})
		resum(t, dir)
	}
	for _, row := range []struct {
		name, refusal string
		tamper        func(t *testing.T, dir string)
	}{
		{"a missing file", "read " + kvrecords.ManifestFile, func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, kvrecords.ManifestFile)))
		}},
		{"a missing signature", "read " + kvrecords.SignatureFile, func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, kvrecords.SignatureFile)))
		}},
		{"a changed byte in records.jsonl", kvrecords.SumsFile + " names", func(t *testing.T, dir string) {
			data := read(t, dir, kvrecords.RecordsFile)
			data[len(data)/2] ^= 0x01
			require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.RecordsFile), data, 0o600))
		}},
		{"three files rewritten to agree, not signed again", kvrecords.SignatureFile + " does not verify", dropFirstLine},
		{"three files rewritten to agree, signed by another key", kvrecords.SignatureFile + " does not verify", func(t *testing.T, dir string) {
			dropFirstLine(t, dir)
			resign(t, dir, otherKey)
		}},
		{"another format", `a format other than "envoy-kv-records-v1"`, signed(func(t *testing.T, dir string) {
			manifest(t, dir, func(m *kvrecords.Manifest) { m.Format = "envoy-kv-records-v0" })
		})},
		{"a stream that is not the bucket's", "a stream other than its bucket's", signed(func(t *testing.T, dir string) {
			manifest(t, dir, func(m *kvrecords.Manifest) { m.Stream = "KV_envoy_sessions" })
		})},
		{"a record count that disagrees", "records, and records.jsonl holds", signed(func(t *testing.T, dir string) {
			manifest(t, dir, func(m *kvrecords.Manifest) { m.Records++ })
		})},
		{"a records sum that disagrees", "records_sha256", signed(func(t *testing.T, dir string) {
			manifest(t, dir, func(m *kvrecords.Manifest) { m.RecordsSHA256 = strings.Repeat("0", 64) })
		})},
		{"a sequence outside first_seq..last_seq", "outside", signed(func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["seq"] = 0 })
		})},
		{"a subject of another bucket", "is not the key", signed(func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["subject"] = "$KV.envoy_sessions." + f["key"].(string) })
		})},
		{"a key that disagrees with its subject", "is not the key", signed(func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["key"] = "another" })
		})},
		{"an empty key", "an empty key", signed(func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["subject"], f["key"] = "$KV."+bucket+".", "" })
		})},
		{"data that is not base64", "is not base64", signed(func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["data"] = "not base64!" })
		})},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "copy")
			require.NoError(t, os.CopyFS(dir, os.DirFS(source)))
			row.tamper(t, dir)
			_, _, err := kvrecords.Read(dir, bucket, r.keys, nameOf(dir))
			require.ErrorContains(t, err, row.refusal)
		})
	}
	t.Run("a copy read under another name", func(t *testing.T) {
		_, _, err := kvrecords.Read(source, bucket, r.keys, strings.Repeat("0", 64))
		require.ErrorContains(t, err, "is not the copy the caller names")
	})
	t.Run("a copy read under no name", func(t *testing.T) {
		_, _, err := kvrecords.Read(source, bucket, r.keys, "")
		require.ErrorContains(t, err, "names no manifest.json SHA-256")
	})
	// A genuine copy of another bucket is refused, though it verifies and its name is the one
	// recorded. Run by hand, the control is the reader at bb2b623923, which takes no bucket: a
	// caller that means envoy_session_consumers reads a copy of envoy_sessions with err=<nil>.
	t.Run("a copy of another bucket", func(t *testing.T) {
		_, _, err := kvrecords.Read(source, "envoy_sessions", r.keys, copied.ManifestSHA256)
		require.ErrorContains(t, err, `a copy of another bucket than "envoy_sessions"`)
	})
	// A line whose value does not decode, its value a canary, is refused naming the line and the
	// field, and the error holds no part of the canary: a records line is the record's data and
	// headers, which whatever logs the error must not get. The seq and time rows print the value at
	// 535cc56f0f; the data and header rows did not, and pin it.
	for _, row := range []struct {
		name, canary, names string
		value               any
		field               string
	}{
		{"a seq out of range", "1844674407370955161600123456789", `"seq"`, json.Number("1844674407370955161600123456789"), "seq"},
		{"a time that is no time", "CANARY-TIME-4f1c", `"time"`, "CANARY-TIME-4f1c", "time"},
		{"data that is a number", "987654321987654", `"data"`, json.Number("987654321987654"), "data"},
		{"data that is not base64", "CANARYDATA4f1c", `"data"`, "CANARYDATA4f1c!!", "data"},
		{"a header value that is a number", "987654321987654", `header "X-Canary"`, map[string]any{"X-Canary": []any{json.Number("987654321987654")}}, "headers"},
		{"a header value that is an object", "CANARY-HEADER-4f1c", `header "X-Canary"`, map[string]any{"X-Canary": []any{map[string]any{"CANARY-HEADER-4f1c": 1}}}, "headers"},
	} {
		t.Run("a line whose "+row.name+" prints no value", func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "copy")
			require.NoError(t, os.CopyFS(dir, os.DirFS(source)))
			signed(func(t *testing.T, dir string) { line(t, dir, func(f map[string]any) { f[row.field] = row.value }) })(t, dir)
			_, _, err := kvrecords.Read(dir, bucket, r.keys, nameOf(dir))
			require.ErrorContains(t, err, kvrecords.RecordsFile+" line 1")
			require.ErrorContains(t, err, row.names)
			require.NotContains(t, err.Error(), row.canary, "the error prints the line's value")
		})
	}
	// A line is read strictly: a line that leaves out one of the six fields, or gives it as null, is
	// refused naming the line and the field, where a reader that took the field's zero would read a
	// line with no data as an empty value.
	for _, field := range []string{"seq", "subject", "key", "time", "headers", "data"} {
		for _, absent := range []struct {
			how    string
			change func(map[string]any)
		}{
			{"missing", func(f map[string]any) { delete(f, field) }},
			{"null", func(f map[string]any) { f[field] = nil }},
		} {
			t.Run("a line with "+field+" "+absent.how, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "copy")
				require.NoError(t, os.CopyFS(dir, os.DirFS(source)))
				signed(func(t *testing.T, dir string) { line(t, dir, absent.change) })(t, dir)
				_, _, err := kvrecords.Read(dir, bucket, r.keys, nameOf(dir))
				require.ErrorContains(t, err, kvrecords.RecordsFile+" line 1", "a line with %s %s was read", field, absent.how)
				require.ErrorContains(t, err, `holds no "`+field+`"`)
			})
		}
	}
	t.Run("no public key", func(t *testing.T) {
		_, _, err := kvrecords.Read(source, bucket, nil, copied.ManifestSHA256)
		require.ErrorIs(t, err, signedrecord.ErrBadSignature, "a copy read with no public key")
	})
	t.Run("Write with no private key", func(t *testing.T) {
		_, err := kvrecords.Write(t.Context(), r.operator, bucket, filepath.Join(t.TempDir(), "copy"), nil)
		require.Error(t, err)
		require.False(t, errors.Is(err, fs.ErrExist))
	})
}

// An older copy the operator signed, put in a newer copy's place, is refused: the bucket's one key
// is copied as "old", then as "new", and the older copy's directory is moved over the newer's. Read
// under the newer copy's name, the one its caller recorded, refuses it naming manifest.json; under
// the older copy's own name it reads, since it is that copy.
//
// Run by hand, the control is the reader at bb2b623923, which names no copy: it reads the older
// copy in the newer's place with err=<nil>, last_seq=1 and s-a="old".
func TestAnOlderCopyInANewersPlaceIsRefused(t *testing.T) {
	r := newRig(t)
	kv, err := r.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, History: 1})
	require.NoError(t, err)
	_, err = kv.PutString("s-a", "old")
	require.NoError(t, err)
	base := t.TempDir()
	older, newer := filepath.Join(base, "older"), filepath.Join(base, "newer")
	olderCopy, err := kvrecords.Write(t.Context(), r.operator, bucket, older, r.key)
	require.NoError(t, err)
	_, err = kv.PutString("s-a", "new")
	require.NoError(t, err)
	newerCopy, err := kvrecords.Write(t.Context(), r.operator, bucket, newer, r.key)
	require.NoError(t, err)
	require.NotEqual(t, olderCopy.ManifestSHA256, newerCopy.ManifestSHA256)
	require.NoError(t, os.RemoveAll(newer))
	require.NoError(t, os.Rename(older, newer))

	read, records, err := kvrecords.Read(newer, bucket, r.keys, newerCopy.ManifestSHA256)
	require.Error(t, err, "the older copy read in the newer's place, last_seq %d, %d records", read.LastSeq, len(records))
	require.ErrorContains(t, err, kvrecords.ManifestFile+" is not the copy the caller names")
	read, records, err = kvrecords.Read(newer, bucket, r.keys, olderCopy.ManifestSHA256)
	require.NoError(t, err, "the older copy under its own name")
	require.EqualValues(t, 1, read.LastSeq)
	require.Equal(t, "old", string(records[0].Data))
}

// sha is data's hex SHA-256.
func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
