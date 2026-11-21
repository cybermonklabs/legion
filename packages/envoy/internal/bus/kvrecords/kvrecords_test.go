package kvrecords_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus/kvrecords"
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

// rig is an in-process nats-server with an admin user, who may do anything, and the operator
// outside a hold, holding exactly Operator(OperatorPhaseReleased).
type rig struct {
	admin    *nats.Conn
	js       nats.JetStreamContext
	operator *nats.Conn
}

func newRig(t *testing.T) *rig {
	t.Helper()
	adminSeed, adminKey := testnats.User(t)
	operatorSeed, operatorKey := testnats.User(t)
	config := "jetstream {}\nauthorization {\n  users = [\n    " +
		userEntry(adminKey, natsgrant.Permissions{Publish: []string{">"}, Subscribe: []string{">"}}) + "\n    " +
		userEntry(operatorKey, natsgrant.Operator(natsgrant.OperatorPhaseReleased)) + "\n  ]\n}\n"
	_, url := testnats.StartConfig(t, config, []string{"-js"}, false)
	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	js, err := admin.JetStream()
	require.NoError(t, err)
	operator, err := testnats.ConnectSeed(t, url, operatorSeed, nats.CustomInboxPrefix(natsgrant.OperatorInbox))
	require.NoError(t, err)
	t.Cleanup(operator.Close)
	return &rig{admin: admin, js: js, operator: operator}
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

// A records-only copy, taken by the operator outside a hold, holds every message the bucket's
// stream stores, each with the sequence, subject, key, time, headers and data the server stores,
// every dotted key included; read back, it gives them all. ForRestore keeps the puts, both markers
// and durable.d1, skips the sentinel and the barrier key, and refuses a planted other.k naming it;
// a restore of its entries gives a fresh bucket the same keys, values, delete markers and purge
// markers, durable.d1 among them.
//
// Controls, each run by hand: a writer that drops headers fails the restore row (the DEL marker
// restores as an empty put); a writer that skips keys holding a dot fails the copy row (the
// sentinel and durable.d1 are missing); a ForRestore that skips every key holding a dot fails the
// restore row (durable.d1 is missing after the restore); a ForRestore that keeps unknown dotted
// keys fails the ForRestore row (other.k is not refused); a changed byte in records.jsonl fails the
// copy row, Read refusing it naming SHA256SUMS.
func TestARecordsCopyRoundTrips(t *testing.T) {
	r := newRig(t)
	r.fillBucket(t)
	want := r.storedMessages(t, "KV_"+bucket)
	dir := filepath.Join(t.TempDir(), "copy")

	manifest, err := kvrecords.Write(t.Context(), r.operator, bucket, dir)
	require.NoError(t, err, "the released operator's copy of %s", bucket)
	info, err := r.js.StreamInfo("KV_" + bucket)
	require.NoError(t, err)
	require.Equal(t, kvrecords.Format, manifest.Format)
	require.Equal(t, info.State.FirstSeq, manifest.FirstSeq)
	require.Equal(t, info.State.LastSeq, manifest.LastSeq)
	require.Greater(t, manifest.LastSeq-manifest.FirstSeq+1, uint64(len(want)), "no sequence was left out, so the 10037 rule went untested")
	durable := kvrecords.DurableKeyPrefix + "d1"

	t.Run("Read gives every stored message", func(t *testing.T) {
		read, records, err := kvrecords.Read(dir)
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

	t.Run("ForRestore keeps durables and undotted keys, skips system keys, refuses the rest", func(t *testing.T) {
		_, records, err := kvrecords.Read(dir)
		require.NoError(t, err)
		restore, err := kvrecords.ForRestore(records)
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
		_, err = kvrecords.ForRestore(planted)
		require.Error(t, err, "ForRestore did not refuse the unknown dotted key other.k")
		require.ErrorContains(t, err, `"other.k"`)
		require.ErrorContains(t, err, fmt.Sprintf("sequence %d", manifest.LastSeq+1))
	})

	t.Run("a restore gives the same keys, values and markers", func(t *testing.T) {
		_, records, err := kvrecords.Read(dir)
		require.NoError(t, err)
		restore, err := kvrecords.ForRestore(records)
		require.NoError(t, err)
		const fresh = "envoy_session_consumers_restored"
		_, err = r.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: fresh})
		require.NoError(t, err)
		for _, record := range restore {
			msg := nats.NewMsg("$KV." + fresh + "." + record.Key)
			for _, header := range record.Headers {
				for _, value := range header.Values {
					msg.Header[header.Name] = append(msg.Header[header.Name], value)
				}
			}
			msg.Data = record.Data
			_, err := r.js.PublishMsg(msg)
			require.NoError(t, err, "restore sequence %d", record.Seq)
		}
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

// Read refuses a copy that is not what Write wrote, naming what failed, and Write refuses a
// directory that exists already.
func TestARecordsCopyIsRefusedWhenItIsNotWhatWasWritten(t *testing.T) {
	r := newRig(t)
	r.fillBucket(t)
	source := filepath.Join(t.TempDir(), "copy")
	_, err := kvrecords.Write(t.Context(), r.operator, bucket, source)
	require.NoError(t, err)
	_, err = kvrecords.Write(context.Background(), r.operator, bucket, source)
	require.ErrorContains(t, err, "exists already")

	read := func(t *testing.T, name string) []byte {
		data, err := os.ReadFile(filepath.Join(source, name))
		require.NoError(t, err)
		return data
	}
	// resum rewrites SHA256SUMS to the files as they are, so a row reaches the check after it.
	resum := func(t *testing.T, dir string) {
		t.Helper()
		var sums strings.Builder
		for _, name := range []string{kvrecords.ManifestFile, kvrecords.RecordsFile} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			require.NoError(t, err)
			fmt.Fprintf(&sums, "%s  %s\n", sha(data), name)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.SumsFile), []byte(sums.String()), 0o600))
	}
	// manifest rewrites the manifest with change applied, its records sum kept true to the file.
	manifest := func(t *testing.T, dir string, change func(*kvrecords.Manifest)) {
		t.Helper()
		var m kvrecords.Manifest
		require.NoError(t, json.Unmarshal(read(t, kvrecords.ManifestFile), &m))
		m.RecordsSHA256 = sha(mustRead(t, filepath.Join(dir, kvrecords.RecordsFile)))
		change(&m)
		raw, err := json.MarshalIndent(m, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.ManifestFile), append(raw, '\n'), 0o600))
	}
	// line rewrites records.jsonl's first line with change applied to its fields.
	line := func(t *testing.T, dir string, change func(map[string]any)) {
		t.Helper()
		lines := strings.SplitAfter(string(read(t, kvrecords.RecordsFile)), "\n")
		var fields map[string]any
		require.NoError(t, json.Unmarshal([]byte(lines[0]), &fields))
		change(fields)
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		lines[0] = string(raw) + "\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.RecordsFile), []byte(strings.Join(lines, "")), 0o600))
		manifest(t, dir, func(*kvrecords.Manifest) {})
	}
	for _, row := range []struct {
		name, refusal string
		tamper        func(t *testing.T, dir string)
	}{
		{"a missing file", "read " + kvrecords.ManifestFile, func(t *testing.T, dir string) {
			require.NoError(t, os.Remove(filepath.Join(dir, kvrecords.ManifestFile)))
		}},
		{"a changed byte in records.jsonl", kvrecords.SumsFile + " names", func(t *testing.T, dir string) {
			data := read(t, kvrecords.RecordsFile)
			data[len(data)/2] ^= 0x01
			require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.RecordsFile), data, 0o600))
		}},
		{"another format", `the format "envoy-kv-records-v0"`, func(t *testing.T, dir string) {
			manifest(t, dir, func(m *kvrecords.Manifest) { m.Format = "envoy-kv-records-v0" })
			resum(t, dir)
		}},
		{"a record count that disagrees", "records, and records.jsonl holds", func(t *testing.T, dir string) {
			manifest(t, dir, func(m *kvrecords.Manifest) { m.Records++ })
			resum(t, dir)
		}},
		{"a records sum that disagrees", "gives records_sha256", func(t *testing.T, dir string) {
			manifest(t, dir, func(m *kvrecords.Manifest) { m.RecordsSHA256 = strings.Repeat("0", 64) })
			resum(t, dir)
		}},
		{"a sequence outside first_seq..last_seq", "outside", func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["seq"] = 0 })
			resum(t, dir)
		}},
		{"a subject of another bucket", "is not the key", func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["subject"] = "$KV.envoy_sessions." + f["key"].(string) })
			resum(t, dir)
		}},
		{"a key that disagrees with its subject", "is not the key", func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["key"] = "another" })
			resum(t, dir)
		}},
		{"data that is not base64", "is not base64", func(t *testing.T, dir string) {
			line(t, dir, func(f map[string]any) { f["data"] = "not base64!" })
			resum(t, dir)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "copy")
			require.NoError(t, os.CopyFS(dir, os.DirFS(source)))
			row.tamper(t, dir)
			_, _, err := kvrecords.Read(dir)
			require.ErrorContains(t, err, row.refusal)
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// sha is data's hex SHA-256.
func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
