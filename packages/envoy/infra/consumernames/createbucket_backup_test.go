package consumernames

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/jsm.go"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/kvrecords"
	"github.com/sjawhar/envoy/internal/holdrecord"
	"github.com/sjawhar/envoy/natsgrant"
)

// createBucketMakeBucket makes the selection bucket at one copy with its sentinel state through
// nc as create-bucket does: the create, the new stream's leader (createBucketAwaitLeader), and the
// sentinel's write sent again while no stream answers it (createBucketFreshWrite).
// selection.CreateSessionBucket writes it at once, which a fresh stream answers "no responders" on
// a cluster, one copy or three.
func createBucketMakeBucket(t testing.TB, nc *nats.Conn, state selection.StoreState) *selection.KVStore {
	t.Helper()
	ctx := context.Background()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	_, err = js.CreateKeyValue(ctx, selection.SessionBucketConfig(1))
	require.NoError(t, err)
	require.NoError(t, createBucketAwaitLeader(ctx, nc))
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	require.NoError(t, createBucketFreshWrite(ctx, func() error {
		_, err := store.WriteSentinel(ctx, state, 0)
		return err
	}))
	return store
}

// createBucketRefused is the journal's entries of command after one refusal after its intent: its
// intent, and its stop where workflow's table makes it Stoppable.
func createBucketRefused(w holdrecord.Workflow, command string) []string {
	if holdrecord.Stoppable(w, command) {
		return []string{"intent", "stop"}
	}
	return []string{"intent"}
}

// createBucketWalkRecord is a walk's record (kvrecords.Record) of the message on the selection
// bucket's key at seq, holding data and headers.
func createBucketWalkRecord(seq uint64, key string, data []byte, headers ...kvrecords.Header) kvrecords.Record {
	return kvrecords.Record{Seq: seq, Subject: "$KV." + selection.SessionBucket + "." + key, Key: key, Time: time.Now().UTC(), Headers: headers, Data: data}
}

// createBucketWalkMarker is the headers of a KV marker (op DEL or PURGE) as nats.go's Delete and
// Purge send them, and a walk records them.
func createBucketWalkMarker(op string) []kvrecords.Header {
	headers := []kvrecords.Header{{Name: kvOperationHeader, Values: []string{op}}}
	if op == "PURGE" {
		headers = append(headers, kvrecords.Header{Name: "Nats-Rollup", Values: []string{"sub"}})
	}
	return headers
}

// createBucketWriteWalk writes records into dir as an envoy-kv-records-v1 walk of the selection
// bucket in kvrecords' own encoding (kvrecords.Record's line, kvrecords.Manifest, and SHA256SUMS
// naming manifest.json and then records.jsonl, as kvrecords.Write lays them out), its manifest
// naming first..last: a walk of a stream the test chooses, which kvrecords.Write takes only from a
// stream that holds it.
func createBucketWriteWalk(t *testing.T, dir string, records []kvrecords.Record, first, last uint64) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	var lines bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		require.NoError(t, err)
		lines.Write(line)
		lines.WriteByte('\n')
	}
	recordsSum := sha256.Sum256(lines.Bytes())
	manifest, err := json.Marshal(kvrecords.Manifest{Format: kvrecords.Format, Bucket: selection.SessionBucket, Stream: selectionStreamName,
		TakenAt: time.Now().UTC(), FirstSeq: first, LastSeq: last, Records: len(records), RecordsSHA256: hex.EncodeToString(recordsSum[:])})
	require.NoError(t, err)
	manifestSum := sha256.Sum256(manifest)
	require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.RecordsFile), lines.Bytes(), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.ManifestFile), manifest, 0o600))
	sums := fmt.Sprintf("%x  %s\n%x  %s\n", manifestSum, kvrecords.ManifestFile, recordsSum, kvrecords.RecordsFile)
	require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.SumsFile), []byte(sums), 0o600))
}

// createBucketMarker publishes a KV marker (op DEL or PURGE) on key through js, as nats.go's
// Delete and Purge send one.
func createBucketMarker(t *testing.T, js jetstream.JetStream, key, op string) {
	t.Helper()
	msg := nats.NewMsg("$KV." + selection.SessionBucket + "." + key)
	msg.Header.Set(kvOperationHeader, op)
	if op == "PURGE" {
		msg.Header.Set("Nats-Rollup", "sub")
	}
	_, err := js.PublishMsg(context.Background(), msg)
	require.NoError(t, err)
}

// createBucketResignWalk changes the manifest of the walk in dir and writes its SHA256SUMS line
// again, so the walk's sums agree and only the change is left to refuse.
func createBucketResignWalk(t *testing.T, dir string, change func(*kvrecords.Manifest)) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, kvrecords.ManifestFile))
	require.NoError(t, err)
	var m kvrecords.Manifest
	require.NoError(t, json.Unmarshal(raw, &m))
	change(&m)
	raw, err = json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.ManifestFile), raw, 0o600))
	sums, err := os.ReadFile(filepath.Join(dir, kvrecords.SumsFile))
	require.NoError(t, err)
	_, rest, _ := strings.Cut(string(sums), "\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, kvrecords.SumsFile), []byte(fmt.Sprintf("%x  %s\n%s", sha256.Sum256(raw), kvrecords.ManifestFile, rest)), 0o600))
}

// A walk in envoy-kv-records-v1 reads through kvrecords.Read and kvrecords.ForRestore into the
// selection bucket's records: each key's last message in the order of the last messages (a key the
// walk holds twice keeps its later value), a DEL and a PURGE marker kept as markers, a listener
// durable's record (a key with a '.') kept, the sentinel taken out as the walk's sentinel, and a
// hold's barrier key left out; its sha256 is SHA256SUMS's own, and it records no archive state, as
// its load starts the sequences over. Controls, each refused with kvrecords' own refusal: a changed
// byte in records.jsonl, a manifest whose records count disagrees, a manifest of another format
// (a directory holding manifest.json is read as a walk, never as a snapshot), and a key with a '.'
// that is no system key kvrecords knows; a consistent walk of another bucket is refused naming it,
// and a directory of neither format is refused naming both.
func TestAWalkBackupReadsIntoTheBucketsRecords(t *testing.T) {
	sentinel, err := json.Marshal(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 2, Hold: "the-cutover"}))
	require.NoError(t, err)
	durable := selection.DurableKey(bus.ListenerDurable("hub-a"))
	records := []kvrecords.Record{
		createBucketWalkRecord(1, selection.SentinelKey, sentinel),
		createBucketWalkRecord(2, "ses_a", []byte(`{"key":"ses_a","start":2}`)),
		createBucketWalkRecord(3, "ses_a", []byte(`{"key":"ses_a","start":3}`)),
		createBucketWalkRecord(4, "ses_gone", nil, createBucketWalkMarker("DEL")...),
		createBucketWalkRecord(5, "hold.barrier.h1", []byte("h1")),
		createBucketWalkRecord(6, durable, []byte(`{"key":"`+durable+`","start":5}`)),
		createBucketWalkRecord(8, "ses_purged", nil, createBucketWalkMarker("PURGE")...),
	}
	dir := filepath.Join(t.TempDir(), "walk")
	createBucketWriteWalk(t, dir, records, 1, 8)
	backup, err := createBucketReadBackup(dir)
	require.NoError(t, err)
	require.Equal(t, createBucketFormatWalk, backup.format)
	sums, err := os.ReadFile(filepath.Join(dir, kvrecords.SumsFile))
	require.NoError(t, err)
	want := sha256.Sum256(sums)
	require.Equal(t, hex.EncodeToString(want[:]), backup.sha256)
	walked := ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 2, Hold: "the-cutover"})
	require.Equal(t, &walked, backup.walk.Sentinel)
	var keys, ops []string
	for _, entry := range backup.walk.Entries {
		keys, ops = append(keys, entry.Key), append(ops, entry.Op)
	}
	require.Equal(t, []string{"ses_a", "ses_gone", durable, "ses_purged"}, keys)
	require.Equal(t, []string{"", "DEL", "", "PURGE"}, ops)
	require.Equal(t, `{"key":"ses_a","start":3}`, string(backup.walk.Entries[0].Value), "the walk's later value of ses_a")
	require.Nil(t, backup.archive, "a walk's load reproduces no sequence, so it records no archive state")

	t.Run("a changed byte", func(t *testing.T) {
		changed := filepath.Join(t.TempDir(), "walk")
		createBucketWriteWalk(t, changed, records, 1, 8)
		path := filepath.Join(changed, kvrecords.RecordsFile)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		raw[len(raw)/2] ^= 1
		require.NoError(t, os.WriteFile(path, raw, 0o600))
		_, err = createBucketReadBackup(changed)
		require.ErrorContains(t, err, "for "+kvrecords.RecordsFile+", whose sum is")
	})
	t.Run("a records count that disagrees", func(t *testing.T) {
		short := filepath.Join(t.TempDir(), "walk")
		createBucketWriteWalk(t, short, records, 1, 8)
		createBucketResignWalk(t, short, func(m *kvrecords.Manifest) { m.Records-- })
		_, err := createBucketReadBackup(short)
		require.ErrorContains(t, err, fmt.Sprintf("gives %d records, and %s holds %d", len(records)-1, kvrecords.RecordsFile, len(records)))
	})
	t.Run("another format", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "walk")
		createBucketWriteWalk(t, other, records, 1, 8)
		createBucketResignWalk(t, other, func(m *kvrecords.Manifest) { m.Format = "envoy-kv-records-v2" })
		_, err := createBucketReadBackup(other)
		require.ErrorContains(t, err, `names the format "envoy-kv-records-v2", not "`+kvrecords.Format+`"`)
	})
	t.Run("an unknown key with a dot", func(t *testing.T) {
		odd := filepath.Join(t.TempDir(), "walk")
		createBucketWriteWalk(t, odd, append(slices.Clone(records), createBucketWalkRecord(9, "other.key", []byte("x"))), 1, 9)
		_, err := createBucketReadBackup(odd)
		require.ErrorContains(t, err, `holds the key "other.key", a dotted key this format does not know`)
	})
	t.Run("a walk of another bucket", func(t *testing.T) {
		another := filepath.Join(t.TempDir(), "walk")
		createBucketWriteWalk(t, another, []kvrecords.Record{{Seq: 1, Subject: "$KV.envoy_sessions.k", Key: "k", Time: time.Now().UTC(), Data: []byte("x")}}, 1, 1)
		createBucketResignWalk(t, another, func(m *kvrecords.Manifest) { m.Bucket, m.Stream = "envoy_sessions", "KV_envoy_sessions" })
		_, err := createBucketReadBackup(another)
		require.ErrorContains(t, err, `is a walk of "envoy_sessions" (KV_envoy_sessions), not of `+selection.SessionBucket)
	})
	t.Run("neither format", func(t *testing.T) {
		_, err := createBucketReadBackup(t.TempDir())
		require.ErrorContains(t, err, "holds neither a jsm.go snapshot nor a "+kvrecords.Format+" walk")
	})
}

// createBucketLoseBucket sets a selection-recovery hold and deletes the selection bucket in it,
// as its loss leaves the hold to recover from, and answers the hold's id.
func createBucketLoseBucket(t *testing.T, h *stack) string {
	t.Helper()
	id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
	_, js := h.operator()
	require.NoError(t, js.DeleteKeyValue(context.Background(), selection.SessionBucket))
	return id
}

// operatorInHold runs fill as the held operator inside a hold of w, a workflow of one command
// (buckets-create, streams-update), set with that command admitted, then writes the command's done
// entry and releases the hold, so the row is released again when it returns.
func operatorInHold(t *testing.T, h *stack, w holdrecord.Workflow, fill func(nc *nats.Conn, js jetstream.JetStream)) {
	t.Helper()
	ctx := context.Background()
	id := h.heldHold(w)
	command := holdrecord.Commands(w)[0]
	_, err := h.records().Admit(ctx, id, holdrecord.Request{Command: command}, nil)
	require.NoError(t, err)
	nc, js := h.operator()
	fill(nc, js)
	require.NoError(t, h.records().AppendDone(ctx, id, command, nil))
	h.closeOperators()
	require.NoError(t, h.release(id))
}

// createBucketBackedBucket makes the selection bucket as a cutover left it, inside a
// buckets-create hold set and released with its journal complete (operatorInHold): its
// sentinel (Cutover done, Boundary 2), a record, a deleted record (a DEL marker), a purged record
// (a PURGE marker over a value) and a listener durable's record. The row is released again when it
// returns.
func createBucketBackedBucket(t *testing.T, h *stack) map[string]selection.Record {
	t.Helper()
	ctx := context.Background()
	records := map[string]selection.Record{
		"ses_backed": {Start: 4, Retired: []string{selection.FreshName(sessionFreshPrefix)}},
		selection.DurableKey(bus.ListenerDurable("hub-a")): {Current: selection.FreshName(bus.ListenerDurable("hub-a")), Start: 2},
	}
	operatorInHold(t, h, holdrecord.WorkflowBucketsCreate, func(nc *nats.Conn, js jetstream.JetStream) {
		store := createBucketMakeBucket(t, nc, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 2, Hold: "the-cutover"})
		for key, record := range records {
			_, err := store.Write(ctx, key, record, 0)
			require.NoError(t, err)
		}
		_, err := store.Write(ctx, "ses_gone", selection.Record{Start: 1}, 0)
		require.NoError(t, err)
		createBucketMarker(t, js, "ses_gone", "DEL")
		_, err = store.Write(ctx, "ses_purged", selection.Record{Start: 1}, 0)
		require.NoError(t, err)
		createBucketMarker(t, js, "ses_purged", "PURGE")
	})
	return records
}

// createBucketHeldSnapshot takes a snapshot of the selection bucket inside a buckets-create hold,
// as the held operator, through jsm.go's SnapshotToDirectory on its own connection, and answers
// its directory. The row is released again when it returns.
func createBucketHeldSnapshot(t *testing.T, h *stack) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "snapshot")
	operatorInHold(t, h, holdrecord.WorkflowBucketsCreate, func(held *nats.Conn, _ jetstream.JetStream) {
		mgr, err := jsm.New(held, jsm.WithTimeout(30*time.Second))
		require.NoError(t, err)
		stream, err := mgr.LoadStream(selectionStreamName)
		require.NoError(t, err)
		_, err = stream.SnapshotToDirectory(context.Background(), dir)
		require.NoError(t, err, "the snapshot as the held operator")
	})
	return dir
}

// The selection bucket's walk, taken as the released operator by kvrecords.Write (its STREAM.INFO
// and STREAM.MSG.GET, the only reads natsgrant.Operator(released) holds of the bucket, in
// envoy-kv-records-v1), restores with create-bucket --after-loss --backup after the bucket is lost:
// the load publishes each key's last value into a fresh bucket, its sequences started over; every
// record, the DEL and PURGE markers (a deleted record is refused after the recovery as before it)
// and the listener durable's record come back, the sentinel holds the walked sentinel's boundary
// and cutover marker in replay mode, the postcheck passes and the hold completes. Controls, each on
// the walk's path itself, on a hub whose operator user holds exactly Operator(held), each entry it
// takes away first seen in that grant: the same walk restored by it succeeds, and so it does
// without the snapshot's STREAM.RESTORE, which the walk's path never sends; without
// $KV.envoy_session_consumers.> it fails at the load's first put, and without the bucket's
// STREAM.CREATE at the load's create, each naming the refused subject; and the same loss recovered
// with no backup brings no record back.
func TestTheReleasedOperatorsBackupRestoresTheSelectionBucket(t *testing.T) {
	ctx := context.Background()
	h := newStack(t)
	createBucketNotifications(t, h, 3)
	records := createBucketBackedBucket(t, h)

	require.Equal(t, natsgrant.OperatorPhaseReleased, h.row().Phase, "the walk is the released operator's")
	released, _ := h.operator()
	before, err := streamInfo(ctx, released, selectionStreamName)
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "walk")
	_, err = kvrecords.Write(ctx, released, selection.SessionBucket, dir)
	require.NoError(t, err, "the walk as the released operator")
	h.closeOperators()

	// The controls on the walk's path: the held grant, without the snapshot's restore, then without
	// each of the load's subjects.
	held := natsgrant.Operator(natsgrant.OperatorPhaseHeld)
	for _, row := range []struct {
		name, without, at string
	}{
		{name: "the whole held grant"},
		{name: "without the snapshot's restore", without: "$JS.API.STREAM.RESTORE." + selectionStreamName},
		{name: "without the load's publishes", without: "$KV." + selection.SessionBucket + ".>", at: "the replay's put of"},
		{name: "without the bucket's create", without: "$JS.API.STREAM.CREATE." + selectionStreamName, at: "create " + selection.SessionBucket},
	} {
		grant := held
		if row.without != "" {
			require.Contains(t, held.Publish, row.without, "control %s: the held grant does not name it", row.name)
			grant.Publish = slices.DeleteFunc(slices.Clone(held.Publish), func(entry string) bool { return entry == row.without })
		}
		g := grantsNewRig(t, 1, grant)
		g.notifications(0)
		id := g.hold(holdrecord.WorkflowSelectionRecovery, natsgrant.OperatorPhaseHeld)
		code, stdout, stderr := createBucketRun(t, ctx, g.env(), "create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		if row.at == "" {
			require.Equal(t, 0, code, "control %s\n%s%s", row.name, stdout, stderr)
			continue
		}
		require.NotEqual(t, 0, code, "control %s\n%s", row.name, stderr)
		require.True(t, grantsNamesOneOf(stderr, []string{row.without}), "control %s names no refused subject of %s\n%s", row.name, row.without, stderr)
		require.Contains(t, stderr, row.at, "control %s was refused off the walk's path", row.name)
	}

	id := createBucketLoseBucket(t, h)
	code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "restored and its sentinel written")
	require.Contains(t, stdout, "postcheck passed")
	require.True(t, h.journal(id).Complete)
	info, _, sentinel, _ := createBucketState(h)
	require.Less(t, info.State.LastSeq, before.State.LastSeq, "the replay's sequences start over")
	require.Equal(t, uint64(2), sentinel.state.Boundary)
	require.Equal(t, selection.CutoverDone, sentinel.state.Cutover)
	require.Equal(t, uint64(1), sentinel.state.ReplayFrom)
	nc, _ := h.operator()
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	for key, record := range records {
		got, _, err := store.Read(ctx, key)
		require.NoError(t, err, key)
		record.Key = key
		require.Equal(t, record, got, key)
	}
	for _, key := range []string{"ses_gone", "ses_purged"} {
		_, _, err := store.Read(ctx, key)
		require.ErrorIs(t, err, selection.ErrRecordDeleted, "%s's marker came back", key)
	}
	h.closeOperators()
	require.NoError(t, h.release(id))

	// The control: the same loss, recovered with no backup.
	id = createBucketLoseBucket(t, h)
	code, _, stderr = h.run("create-bucket", "--hold", id, "--after-loss")
	require.Equal(t, 0, code, stderr)
	nc, _ = h.operator()
	_, _, err = selection.NewKVStore(selection.ConnRequester{Conn: nc}).Read(ctx, "ses_backed")
	require.ErrorIs(t, err, selection.ErrNoRecord, "control: a recovery with no backup brought the record back")
}

// createBucketPutCutDialer dials as nats.go does, and once, when a connection writes its after-th
// put of a subject starting with on (a PUB, or a headered HPUB, whose subject starts so), ends the
// run (cut) and closes that connection before that put is sent: a run killed at that put.
type createBucketPutCutDialer struct {
	on    string
	after int
	cut   func()
	puts  atomic.Int32
	done  atomic.Bool
}

// createBucketRecordPuts is the put-cut dialer's subject of the walk's record keys, ses_<i>
// (createBucketWalkOfRecords): each of the load's puts.
const createBucketRecordPuts = "$KV." + selection.SessionBucket + ".ses_"

func (d *createBucketPutCutDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).Dial(network, address)
	if err != nil {
		return nil, err
	}
	return &createBucketPutCutConn{Conn: conn, d: d}, nil
}

type createBucketPutCutConn struct {
	net.Conn
	d *createBucketPutCutDialer
}

func (c *createBucketPutCutConn) Write(p []byte) (int, error) {
	// "PUB $KV.…" matches a headered HPUB too, which each replay put and the sentinel's write is.
	puts := bytes.Count(p, []byte("PUB "+c.d.on))
	if puts > 0 && int(c.d.puts.Add(int32(puts))) >= c.d.after && c.d.done.CompareAndSwap(false, true) {
		c.d.cut()
		_ = c.Conn.Close()
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

// createBucketKilledRun runs create-bucket --after-loss --backup dir in hold id on a connection
// of dialer's, which ends the run at its cut, and requires the run cut and failed.
func createBucketKilledRun(t *testing.T, h *stack, id, dir string, dialer *createBucketPutCutDialer) {
	t.Helper()
	run, kill := context.WithCancel(context.Background())
	defer kill()
	dialer.cut = kill
	code, _, stderr := h.runDialed(run, dialer, "create-bucket", "--hold", id, "--after-loss", "--backup", dir)
	require.True(t, dialer.done.Load(), "the run was never cut at its put of %s\n%s", dialer.on, stderr)
	require.NotEqual(t, 0, code, stderr)
}

// createBucketWalkOfRecords is a walk of the selection bucket holding a sentinel with boundary 2
// at sequence 1 and n records ses_<i>, the i-th at sequence i+2.
func createBucketWalkOfRecords(t *testing.T, n int) []kvrecords.Record {
	t.Helper()
	sentinel, err := json.Marshal(ownSentinel(familySession, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 2, Hold: "the-cutover"}))
	require.NoError(t, err)
	records := []kvrecords.Record{createBucketWalkRecord(1, selection.SentinelKey, sentinel)}
	for i := range n {
		key := fmt.Sprintf("ses_%02d", i)
		record, err := json.Marshal(selection.Record{Key: key, Start: uint64(i + 1)})
		require.NoError(t, err)
		records = append(records, createBucketWalkRecord(uint64(i+2), key, record))
	}
	return records
}

// A cut walk load is deleted and loaded again (plan v66, "a partly loaded bucket with no sentinel
// whose keys are a subset of the walk's"): create-bucket --after-loss from a walk of 12 records,
// killed after it put 3 of them (its connection closed and its run ended), leaves a bucket
// holding no sentinel and part of the walk's keys, each with the walk's value; the rerun reads
// that, by each key's last value and the count of its keys, as the leftover of its own cut load,
// deletes the bucket and loads the walk into a fresh one, writes the sentinel, passes its
// postcheck and completes: all 12 records, and no message but theirs, the sentinel and this hold's
// barrier. Controls, each stopping the rerun with "selection bucket exists and is not this
// recovery's" and deleting nothing: a bucket that also holds a key the walk does not hold, named by
// the count of such keys, and a walk key holding another value than the walk's, named.
func TestACutWalkLoadIsDeletedAndLoadedAgain(t *testing.T) {
	ctx := context.Background()
	records := createBucketWalkOfRecords(t, 12)
	killed := func(t *testing.T) (*stack, string, string) {
		h := newStack(t)
		createBucketNotifications(t, h, 3)
		dir := filepath.Join(t.TempDir(), "walk")
		createBucketWriteWalk(t, dir, records, 1, uint64(len(records)))
		id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
		createBucketKilledRun(t, h, id, dir, &createBucketPutCutDialer{on: createBucketRecordPuts, after: 4})
		info, present, _, found := createBucketState(h)
		require.True(t, present)
		require.False(t, found, "the cut replay wrote the sentinel")
		require.Less(t, info.State.Msgs, uint64(12), "the cut replay put every record")
		for subject := range info.State.Subjects {
			key := strings.TrimPrefix(subject, "$KV."+selection.SessionBucket+".")
			require.True(t, slices.ContainsFunc(records[1:], func(r kvrecords.Record) bool { return r.Key == key }) || key == "hold.barrier."+id,
				"the cut load left the key %s, which the walk does not hold", key)
		}
		return h, id, dir
	}

	t.Run("deleted and loaded again", func(t *testing.T) {
		h, id, dir := killed(t)
		cut, _, _, _ := createBucketState(h)
		code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 0, code, stderr)
		require.Contains(t, stdout, "restored and its sentinel written")
		require.Contains(t, stdout, "postcheck passed")
		require.True(t, h.journal(id).Complete)
		info, _, sentinel, _ := createBucketState(h)
		require.False(t, info.Created.Equal(cut.Created), "the partly loaded bucket was not deleted and made again")
		require.Equal(t, uint64(12+2), info.State.Msgs, "the loaded bucket: the walk's 12 records, the sentinel and this hold's barrier")
		require.Equal(t, uint64(2), sentinel.state.Boundary)
		nc, _ := h.operator()
		store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
		for i := range 12 {
			got, _, err := store.Read(ctx, fmt.Sprintf("ses_%02d", i))
			require.NoError(t, err)
			require.Equal(t, uint64(i+1), got.Start)
		}
	})

	t.Run("control: a key the walk does not hold", func(t *testing.T) {
		h, id, dir := killed(t)
		nc, _ := h.operator()
		_, err := selection.NewKVStore(selection.ConnRequester{Conn: nc}).Write(ctx, "ses_foreign", selection.Record{Start: 9}, 0)
		require.NoError(t, err)
		code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "selection bucket exists and is not this recovery's: "+selection.SessionBucket+" holds 1 keys that are no record of the walk")
		require.False(t, h.journal(id).Complete)
		_, present, _, _ := createBucketState(h)
		require.True(t, present, "control: the stopped rerun deleted the bucket")
	})

	t.Run("control: a walk key holding another value", func(t *testing.T) {
		h, id, dir := killed(t)
		nc, _ := h.operator()
		store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
		_, revision, err := store.Read(ctx, "ses_00")
		require.NoError(t, err)
		_, err = store.Write(ctx, "ses_00", selection.Record{Start: 99}, revision)
		require.NoError(t, err)
		code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, "selection bucket exists and is not this recovery's: "+selection.SessionBucket+"'s key ses_00 holds another message than the walk's")
		require.False(t, h.journal(id).Complete)
		_, present, _, _ := createBucketState(h)
		require.True(t, present, "control: the stopped rerun deleted the bucket")
	})
}

// createBucketHeldSnapshotGrant is the held operator's grant of the selection bucket's snapshot
// (STREAM.SNAPSHOT and its chunks' acknowledgements), which natsgrant.Operator(held) alone names.
var createBucketHeldSnapshotGrant = []string{"$JS.API.STREAM.SNAPSHOT." + selectionStreamName, "$JS.SNAPSHOT.ACK." + selectionStreamName + ".>"}

// A snapshot of the selection bucket taken inside a hold, as the held operator, through jsm.go's
// SnapshotToDirectory on its own connection, restores with create-bucket --after-loss --backup
// after the bucket is lost (createBucketReadBackup reads a directory with no kvrecords manifest as
// a snapshot): the recovery completes and the records come back. The row first requires the held
// grant to name the snapshot's subjects.
func TestAHeldSnapshotRestoresTheSelectionBucket(t *testing.T) {
	for _, subject := range createBucketHeldSnapshotGrant {
		require.Contains(t, natsgrant.Operator(natsgrant.OperatorPhaseHeld).Publish, subject, "the held grant does not take the selection bucket's snapshot")
	}
	ctx := context.Background()
	h := newStack(t)
	createBucketNotifications(t, h, 3)
	records := createBucketBackedBucket(t, h)

	dir := createBucketHeldSnapshot(t, h)
	id := createBucketLoseBucket(t, h)
	code, stdout, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, stdout, "restored and its sentinel written")
	require.True(t, h.journal(id).Complete)
	nc, _ := h.operator()
	store := selection.NewKVStore(selection.ConnRequester{Conn: nc})
	for key, record := range records {
		got, _, err := store.Read(ctx, key)
		require.NoError(t, err, key)
		record.Key = key
		require.Equal(t, record, got, key)
	}
}

// createBucketStoreAnswer has the server store its answer to request under the selection bucket's
// key, as sender, any principal whose grant names request, can: the request names
// $KV.envoy_session_consumers.<key> as its reply subject, and the server publishes its answer
// there with no check of the sender's publish rights, so the bucket's stream stores it. It reads
// the stored message through reader and answers its sequence and data.
func createBucketStoreAnswer(t *testing.T, sender, reader *nats.Conn, request, key string) (uint64, []byte) {
	t.Helper()
	ctx := context.Background()
	before, _, err := replayLeaderRead(ctx, reader, selection.SessionBucket, key)
	require.NoError(t, err)
	require.NoError(t, sender.PublishRequest(request, "$KV."+selection.SessionBucket+"."+key, nil))
	require.NoError(t, sender.Flush())
	var stored replayMessage
	require.Eventually(t, func() bool {
		m, found, err := replayLeaderRead(ctx, reader, selection.SessionBucket, key)
		stored = m
		return err == nil && found && m.Seq != before.Seq
	}, 10*time.Second, 20*time.Millisecond, "the server stored no answer to %s under %s", request, key)
	return stored.Seq, stored.Data
}

// createBucketStreamInfo is the request whose answer a principal holding the selection bucket's
// STREAM.INFO (the released operator, here) has the server store under a key.
const createBucketStreamInfo = "$JS.API.STREAM.INFO." + selectionStreamName

// A value that is not exactly a value of its key's kind makes create-bucket --after-loss refuse,
// naming the key and its sequence, and load nothing. Any principal with a JetStream API grant can
// have the server store its JSON answer under any key of the selection bucket through a request's
// reply subject, and a holder of the bucket's direct get can copy one key's record onto another.
// In a walk taken as the released operator: the server's STREAM.INFO answer under a session key
// and under envoy.bucket, and ses_backed's record copied under ses_copy by a listener's direct get,
// each refused as the backup is read, before the command's intent, the bucket still absent. In a
// held snapshot: the answer under a session key, refused once restored, the restored bucket deleted
// again and the stop naming that. In a present bucket the recovery would adopt (another recovery's
// sentinel in replay mode): the answer under a session key and under envoy.bucket, each refused
// with the sentinel unwritten. Control, in each: a lenient decode (json.Unmarshal into the key's
// kind) takes the value.
func TestAValueThatIsNoExactRecordRefusesTheLoad(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, key string
		copied    bool
		decoded   any
	}{
		{name: "a walk's session key", key: "ses_forged", decoded: &selection.Record{}},
		{name: "a walk's sentinel", key: selection.SentinelKey, decoded: &selection.StoreState{}},
		{name: "a walk's record copied under another key", key: "ses_copy", copied: true, decoded: &selection.Record{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newStack(t)
			createBucketNotifications(t, h, 3)
			createBucketBackedBucket(t, h)
			released, _ := h.operator()
			var seq uint64
			var data []byte
			if c.copied {
				listener, err := h.s.Connect(h.s.Cluster.URL(), h.s.ListenerCredential(holdtest.ListenerMachines[0]))
				require.NoError(t, err)
				seq, data = createBucketStoreAnswer(t, listener, released, "$JS.API.DIRECT.GET."+selectionStreamName+".$KV."+selection.SessionBucket+".ses_backed", c.key)
				listener.Close()
			} else {
				seq, data = createBucketStoreAnswer(t, released, released, createBucketStreamInfo, c.key)
			}
			require.NoError(t, json.Unmarshal(data, c.decoded), "control: a lenient decode refuses the value under %s", c.key)
			dir := filepath.Join(t.TempDir(), "walk")
			_, err := kvrecords.Write(ctx, released, selection.SessionBucket, dir)
			require.NoError(t, err)
			h.closeOperators()

			id := createBucketLoseBucket(t, h)
			code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
			require.Equal(t, 1, code, stderr)
			require.Contains(t, stderr, fmt.Sprintf("the walk's key %s at sequence %d holds", c.key, seq))
			require.Empty(t, createBucketEntries(h.journal(id), createBucketAfterLossCommand), "the refused walk wrote its intent")
			_, present, _, _ := createBucketState(h)
			require.False(t, present, "the refused walk loaded the bucket")
		})
	}

	t.Run("a snapshot's session key", func(t *testing.T) {
		h := newStack(t)
		createBucketNotifications(t, h, 3)
		createBucketBackedBucket(t, h)
		released, _ := h.operator()
		seq, data := createBucketStoreAnswer(t, released, released, createBucketStreamInfo, "ses_forged")
		require.NoError(t, json.Unmarshal(data, &selection.Record{}), "control: a lenient decode refuses the value")
		h.closeOperators()
		dir := createBucketHeldSnapshot(t, h)

		id := createBucketLoseBucket(t, h)
		code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss", "--backup", dir)
		require.Equal(t, 1, code, stderr)
		require.Contains(t, stderr, fmt.Sprintf("selection bucket %s's key ses_forged at sequence %d holds ", selection.SessionBucket, seq))
		require.Contains(t, stderr, "the key holds no selection record of its own")
		require.Contains(t, stderr, "the bucket "+selectionStreamName+" restored from the backup and deleted again")
		require.False(t, h.journal(id).Complete)
		_, present, _, _ := createBucketState(h)
		require.False(t, present, "the refused snapshot's restored bucket was left")
	})

	for _, c := range []struct {
		name, key, kind string
		decoded         any
	}{
		{name: "a present bucket's session key", key: "ses_forged", kind: "the key holds no selection record of its own", decoded: &selection.Record{}},
		{name: "a present bucket's sentinel", key: selection.SentinelKey, kind: "the selection store's sentinel key holds no sentinel of its own", decoded: &selection.StoreState{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newStack(t)
			createBucketNotifications(t, h, 3)
			operatorInHold(t, h, holdrecord.WorkflowBucketsCreate, func(nc *nats.Conn, _ jetstream.JetStream) {
				store := createBucketMakeBucket(t, nc, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 2, ReplayFrom: 1,
					BackupSHA256: createBucketNoBackup, Hold: "another-recovery"})
				_, err := store.Write(ctx, "ses_kept", selection.Record{Start: 2}, 0)
				require.NoError(t, err)
			})
			released, _ := h.operator()
			seq, data := createBucketStoreAnswer(t, released, released, createBucketStreamInfo, c.key)
			require.NoError(t, json.Unmarshal(data, c.decoded), "control: a lenient decode refuses the value under %s", c.key)
			before, _, err := replayLeaderRead(ctx, released, selection.SessionBucket, selection.SentinelKey)
			require.NoError(t, err)
			h.closeOperators()

			id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
			code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss")
			require.Equal(t, 1, code, stderr)
			require.Contains(t, stderr, fmt.Sprintf("selection bucket %s's key %s at sequence %d holds ", selection.SessionBucket, c.key, seq))
			require.Contains(t, stderr, c.kind)
			require.False(t, h.journal(id).Complete)
			nc, _ := h.operator()
			after, _, err := replayLeaderRead(ctx, nc, selection.SessionBucket, selection.SentinelKey)
			require.NoError(t, err)
			require.Equal(t, before.Seq, after.Seq, "the refused recovery wrote the sentinel")
		})
	}
}

// createBucketRequireStop requires that command's latest entry in hold id's journal is a stop
// whose reason names each of left, and that the stop gives the hold its way out: hold abort is
// admitted. Then, as the control, it writes command's intent again as a run that dies before its
// stop would, and requires hold release refused while that intent is pending.
func createBucketRequireStop(t *testing.T, h *stack, id, command string, left ...string) {
	t.Helper()
	d := h.journal(id)
	last := lastOf(d, command)
	require.Equal(t, "stop", last.State, "%s's latest entry", command)
	for _, want := range left {
		require.Contains(t, last.Outputs["reason"], want, "%s's stop names what it left", command)
	}
	require.NoError(t, holdrecord.Decide(d, holdrecord.Request{Command: holdrecord.CommandAbort}), "the stop leaves the hold no way out")

	_, err := h.records().Admit(context.Background(), id, holdrecord.Request{Command: command}, nil)
	require.NoError(t, err)
	require.ErrorIs(t, holdrecord.Decide(h.journal(id), holdrecord.Request{Command: holdrecord.CommandRelease}), holdrecord.ErrBusy,
		"control: with no stop after %s's intent, hold release is admitted", command)
}

// requireStoppable requires that the workflow's table makes command Stoppable, which a row of its
// stop reads.
func requireStoppable(t *testing.T, w holdrecord.Workflow, command string) {
	t.Helper()
	require.True(t, holdrecord.Stoppable(w, command), "%s is not Stoppable in %s", command, w)
}

// create-bucket's refusal after its intent (a present bucket with another configuration, history
// 2, which is UNKNOWN) writes its stop entry naming what it left, here nothing outside the
// journal, and the hold has its way out. Control: the same command's intent with no stop after it
// leaves hold release refused.
func TestACreateBucketRefusalAfterItsIntentStops(t *testing.T) {
	requireStoppable(t, holdrecord.WorkflowCutover, createBucketCommand)
	h := newStack(t)
	createBucketNotifications(t, h, 0)
	id := h.prepared(holdrecord.WorkflowCutover)
	createBucketCohort(t, h.records(), id, 2)
	h.held(id)
	_, js := h.operator()
	config := selection.SessionBucketConfig(1)
	config.History = 2
	_, err := js.CreateKeyValue(context.Background(), config)
	require.NoError(t, err)
	code, _, stderr := h.run("create-bucket", "--hold", id)
	require.Equal(t, 2, code, stderr)
	require.Contains(t, stderr, "it left nothing outside the journal")
	createBucketRequireStop(t, h, id, createBucketCommand, "it left nothing outside the journal")
}

// create-bucket --after-loss's refusal after its intent (a bucket that is not this recovery's)
// writes its stop entry naming what it left: the hold's selection record and this hold's barrier
// key in the bucket; the hold has its way out. Control: the same command's intent with no stop
// after it leaves hold release refused.
func TestARecoveryRefusalAfterItsIntentStops(t *testing.T) {
	requireStoppable(t, holdrecord.WorkflowSelectionRecovery, createBucketAfterLossCommand)
	h := newStack(t)
	createBucketNotifications(t, h, 0)
	id := h.heldHold(holdrecord.WorkflowSelectionRecovery)
	nc, _ := h.operator()
	createBucketMakeBucket(t, nc, selection.StoreState{Cutover: selection.CutoverDone, Boundary: 3, Hold: "another-hold"})
	code, _, stderr := h.run("create-bucket", "--hold", id, "--after-loss")
	require.Equal(t, 1, code, stderr)
	createBucketRequireStop(t, h, id, createBucketAfterLossCommand, "the hold's selection record", "this hold's barrier key in "+selectionStreamName)
}

// replay's refusal after its intent writes its stop entry naming what it left, this hold's barrier
// key in the bucket, and the hold has its way out. The refusal is the bucket's stream's: made again
// by the operator with a max_msg_size the barrier's put fits and K's record does not, it refuses
// replay's write of the record with an answer that is no selection.ErrConflict. Control: the same
// command's intent with no stop after it leaves hold release refused.
func TestAReplayRefusalAfterItsIntentStops(t *testing.T) {
	requireStoppable(t, holdrecord.WorkflowCutover, replayCommand)
	ctx := context.Background()
	h := newStack(t)
	createBucketNotifications(t, h, 3)
	id := h.prepared(holdrecord.WorkflowCutover)
	createBucketCohort(t, h.records(), id, 1)
	h.held(id)
	code, _, stderr := h.run("create-bucket", "--hold", id)
	require.Equal(t, 0, code, stderr)
	_, js := h.operator()
	require.NoError(t, js.DeleteStream(ctx, selectionStreamName))
	config := createBucketStreamConfig(1)
	config.Name = selectionStreamName
	config.MaxMsgSize = int32(len(id)) // the barrier's put is the hold id alone
	_, err := js.CreateStream(ctx, config)
	require.NoError(t, err)
	code, _, stderr = h.run("replay", "--hold", id, "--key", "ses_bad", "--from", "2")
	require.Equal(t, 1, code, stderr)
	require.Contains(t, stderr, "message size exceeds maximum allowed", "the stream refused the record's write")
	createBucketRequireStop(t, h, id, replayCommand, "this hold's barrier key in "+selectionStreamName)
}
