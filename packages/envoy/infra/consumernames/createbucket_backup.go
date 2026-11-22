package consumernames

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/jsm.go"
	"github.com/nats-io/jsm.go/api"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	selection "github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus/kvrecords"
)

// The --backup formats create-bucket --after-loss restores from (createBucketReadBackup), told
// apart by their files:
//   - a walk, the released operator's read-only backup (kvrecords.Write): every message of the
//     bucket's stream read by sequence with STREAM.MSG.GET, in kvrecords.Format
//     (envoy-kv-records-v1: kvrecords.ManifestFile, kvrecords.RecordsFile and kvrecords.SumsFile).
//     A directory holding kvrecords.ManifestFile is a walk, read through kvrecords.Read and
//     kvrecords.ForRestore (createBucketReadWalk), and loaded by publishing each key's last value
//     into a fresh bucket (createBucketReplayWalk), so its sequences start over;
//   - a snapshot, jsm.go's backup of the selection bucket's stream (jsm.SnapshotMetaFile and its
//     archive), which only a hold's operator takes: a snapshot's chunks go to a deliver subject the
//     server publishes to without checking the requester's publish rights, so the grant names
//     STREAM.SNAPSHOT only while every other client is refused. Any other directory is read as a
//     snapshot (createBucketReadSnapshot), which refuses one holding none, and restored with
//     STREAM.RESTORE.
const (
	createBucketFormatSnapshot = "snapshot"
	createBucketFormatWalk     = "walk"
)

// createBucketBackup is a --backup directory, read and hashed before the hold's journal records
// anything.
type createBucketBackup struct {
	dir string
	// format is createBucketFormatSnapshot or createBucketFormatWalk.
	format string
	// sha256 identifies the backup: a snapshot's meta file and archive (createBucketReadSnapshot),
	// or a walk's SHA256SUMS, which names the sum of each of its files.
	sha256 string
	// archive is a snapshot's recorded stream state, which its restore reproduces; nil for a walk,
	// whose load publishes each record again and so reproduces no sequence (kvrecords).
	archive *createBucketArchive
	// walk is a walk backup's records, nil for a snapshot.
	walk *createBucketWalk
}

// createBucketReadBackup reads dir in the format its files show: a walk when it holds
// kvrecords.ManifestFile, whose reader refuses a manifest of any format but kvrecords.Format, and
// otherwise a snapshot, whose reader refuses a directory that holds none.
func createBucketReadBackup(dir string) (*createBucketBackup, error) {
	if _, err := os.Lstat(filepath.Join(dir, kvrecords.ManifestFile)); !errors.Is(err, fs.ErrNotExist) {
		return createBucketReadWalk(dir)
	}
	return createBucketReadSnapshot(dir)
}

// createBucketReadSnapshot reads dir, a jsm.go backup of the selection bucket's stream
// (jsm.SnapshotToDirectory's backup.json and archive): its meta file must name the bucket's
// stream, and its sha256 is of the meta file's bytes and then the archive's, so a backup whose
// configuration or whose data differs is another backup.
func createBucketReadSnapshot(dir string) (*createBucketBackup, error) {
	dataPath, err := jsm.SnapshotDataPath(dir)
	if err != nil {
		return nil, fmt.Errorf("--backup %s holds neither a jsm.go snapshot nor a %s walk: %w", dir, kvrecords.Format, err)
	}
	meta, err := os.ReadFile(filepath.Join(dir, jsm.SnapshotMetaFile))
	if err != nil {
		return nil, fmt.Errorf("--backup %s: %w", dir, err)
	}
	var request api.JSApiStreamRestoreRequest
	if err := json.Unmarshal(meta, &request); err != nil {
		return nil, fmt.Errorf("--backup %s: its %s does not decode: %w", dir, jsm.SnapshotMetaFile, err)
	}
	if request.Config.Name != selectionStreamName {
		return nil, fmt.Errorf("--backup %s is a backup of %q, not of %s", dir, request.Config.Name, selectionStreamName)
	}
	data, err := os.Open(dataPath)
	if err != nil {
		return nil, fmt.Errorf("--backup %s: %w", dir, err)
	}
	defer data.Close()
	sum := sha256.New()
	sum.Write(meta)
	if _, err := io.Copy(sum, data); err != nil {
		return nil, fmt.Errorf("--backup %s: read its archive: %w", dir, err)
	}
	return &createBucketBackup{dir: dir, format: createBucketFormatSnapshot, sha256: hex.EncodeToString(sum.Sum(nil)), archive: &createBucketArchive{
		FirstSeq: request.State.FirstSeq, LastSeq: request.State.LastSeq, Msgs: request.State.Msgs,
	}}, nil
}

// createBucketReadWalk reads dir, a walk backup of the selection bucket, through kvrecords.Read,
// which refuses a walk whose files, sums, format, records count, sequences or subjects disagree,
// naming what failed; holds its manifest to the selection bucket and its stream; and reads its
// records into the bucket's (createBucketWalkFrom). Its sha256 is that of its SHA256SUMS, which
// names the sum of each of its files and which kvrecords.Read holds to them.
func createBucketReadWalk(dir string) (*createBucketBackup, error) {
	sum, err := walkSHA256(dir)
	if err != nil {
		return nil, fmt.Errorf("--backup %s: %w", dir, err)
	}
	manifest, records, err := kvrecords.Read(dir)
	if err != nil {
		return nil, fmt.Errorf("--backup %s: %w", dir, err)
	}
	if manifest.Bucket != selection.SessionBucket || manifest.Stream != selectionStreamName {
		return nil, fmt.Errorf("--backup %s is a walk of %q (%s), not of %s", dir, manifest.Bucket, manifest.Stream, selection.SessionBucket)
	}
	walk, err := createBucketWalkFrom(records)
	if err != nil {
		return nil, fmt.Errorf("--backup %s: %w", dir, err)
	}
	return &createBucketBackup{dir: dir, format: createBucketFormatWalk, sha256: sum, walk: walk}, nil
}

// createBucketWalk is a walk backup's content: the records a restore writes back
// (kvrecords.ForRestore: each listener durable's record, selection.DurableKey's durable.<name>,
// and every key holding no '.'), each key's last message in the order of those last messages, and
// the walked sentinel's state, nil when the bucket held none (or a marker in its place). The
// sentinel is not loaded: the recovery writes it new, with the walked one's boundary and cutover
// marker (createBucketSentinelState), and a hold's barrier key is one no reader reads.
// kvrecords.ForRestore refuses a walk holding any other key with a '.', since nothing says what it
// is.
type createBucketWalk struct {
	Entries  []createBucketWalkEntry
	Sentinel *selection.StoreState
}

// createBucketWalkEntry is one record key's last message: its value and headers, a KV delete or
// purge marker's among them (Op "DEL" or "PURGE"), which the load writes again, so a key whose
// record was deleted is refused after the recovery as before it (selection.ErrRecordDeleted).
type createBucketWalkEntry struct {
	Key    string
	Value  []byte
	Header kvrecords.Headers
	Op     string
}

// createBucketWalkFrom is the content (createBucketWalk) of records, a walk's records in sequence
// order as kvrecords.Read answers them. A key the walk holds twice keeps its later message, as the
// bucket's history of one does. Each value it keeps, the sentinel's among them, must be an exact
// value of its key's kind (createBucketExactValue), or the walk is refused, naming the key and its
// sequence, before anything is loaded.
func createBucketWalkFrom(records []kvrecords.Record) (*createBucketWalk, error) {
	walk := &createBucketWalk{}
	var sentinel *kvrecords.Record
	for i := range records {
		if records[i].Key == selection.SentinelKey {
			sentinel = &records[i]
		}
	}
	if sentinel != nil {
		state, err := createBucketExactValue(sentinel.Key, createBucketWalkOp(sentinel.Headers), sentinel.Data)
		if err != nil {
			return nil, fmt.Errorf("the walk's key %s at sequence %d %w", sentinel.Key, sentinel.Seq, err)
		}
		walk.Sentinel = state
	}
	restore, err := kvrecords.ForRestore(records)
	if err != nil {
		return nil, err
	}
	last := make(map[string]int, len(restore))
	for i, record := range restore {
		last[record.Key] = i
	}
	walk.Entries = make([]createBucketWalkEntry, 0, len(last))
	for i, record := range restore {
		if last[record.Key] != i {
			continue
		}
		op := createBucketWalkOp(record.Headers)
		if _, err := createBucketExactValue(record.Key, op, record.Data); err != nil {
			return nil, fmt.Errorf("the walk's key %s at sequence %d %w", record.Key, record.Seq, err)
		}
		walk.Entries = append(walk.Entries, createBucketWalkEntry{Key: record.Key, Value: record.Data, Header: record.Headers, Op: op})
	}
	return walk, nil
}

// kvOperationHeader marks a KV delete or purge marker (nats.go kv.go).
const kvOperationHeader = "KV-Operation"

// createBucketWalkOp is the KV operation a walk's record carries (kvOperationHeader): "DEL" or
// "PURGE" for a marker, and "" for a value.
func createBucketWalkOp(headers kvrecords.Headers) string {
	for _, header := range headers {
		if header.Name == kvOperationHeader && len(header.Values) > 0 {
			return header.Values[0]
		}
	}
	return ""
}

// createBucketExactValue is create-bucket --after-loss's one check of a value it loads or adopts (a
// walk's, a restored snapshot's or a present bucket's, createBucketCheckValues): a KV delete or
// purge marker holds no data, and any other value is an exact value of its key's kind as
// readSelectionValue reads it, the package's one decode of the bucket's values (strictdecode.go:
// a session's or a durable's selection.Record and the sentinel's selection.StoreState through
// selection.DecodeKVValue, the state answered here, nil for a marker or another key; a hold's
// barrier, its hold id; no other key). Any principal with a JetStream API grant can have the server
// store its JSON answer under any key of the bucket through a request's reply subject, which a
// lenient decode takes as an empty record. Its errors begin "holds", for the caller to name the key
// and its sequence before them.
func createBucketExactValue(key, op string, data []byte) (*selection.StoreState, error) {
	switch op {
	case "":
	case "DEL", "PURGE":
		if len(data) > 0 {
			return nil, fmt.Errorf("holds a %s marker with %d bytes of data", op, len(data))
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("holds the KV operation %q, which is neither DEL nor PURGE", op)
	}
	value, err := readSelectionValue(statusKey{Key: key, Value: data})
	if err != nil {
		return nil, fmt.Errorf("holds %w", err)
	}
	return value.state, nil
}

// createBucketCheckValues reads every message the selection bucket's stream holds through its
// sequence last, one STREAM.MSG.GET each (next_by_subj over the bucket's keys from the sequence
// after the one before, so a sequence the stream no longer holds costs no request), and refuses
// the first that is no exact value of its key's kind (createBucketExactValue), naming its key and
// sequence.
func createBucketCheckValues(ctx context.Context, nc *nats.Conn, last uint64) error {
	for seq := uint64(1); seq <= last; {
		m, held, err := replayStreamMessage(ctx, nc, selection.SessionBucket, map[string]any{"seq": seq, "next_by_subj": selectionKeyPrefix + ">"})
		if err != nil {
			return err
		}
		if !held || m.Seq > last {
			return nil
		}
		if m.Seq < seq {
			return unknownf(nil, "%s answered sequence %d for the next message from %d", selectionStreamName, m.Seq, seq)
		}
		key := strings.TrimPrefix(m.Subject, selectionKeyPrefix)
		if _, err := createBucketExactValue(key, m.Op, m.Data); err != nil {
			return fmt.Errorf("selection bucket %s's key %s at sequence %d %w", selection.SessionBucket, key, m.Seq, err)
		}
		seq = m.Seq + 1
	}
	return nil
}

// createBucketWalkHolds reads what the selection bucket holds against walk, behind the caller's
// barrier, for a bucket with no sentinel, by each key's last message and the count of its keys,
// never by a sequence: a load publishes each record again, so its sequences start over (the plan).
// It answers held, each of walk's keys whose last message is the walk's (createBucketSameEntry);
// extra, how many of the bucket's keys (info's num_subjects) are neither one of those nor a
// hold's barrier key (createBucketBarrierKey, by info's subjects, which the caller read); and
// foreign, the first of walk's keys whose last message is another than the walk's.
func createBucketWalkHolds(ctx context.Context, nc *nats.Conn, info *jetstream.StreamInfo, walk *createBucketWalk, holdID string) (held map[string]bool, extra uint64, foreign string, err error) {
	held = map[string]bool{}
	for _, entry := range walk.Entries {
		m, found, err := replayLeaderRead(ctx, nc, selection.SessionBucket, entry.Key)
		if err != nil {
			return nil, 0, "", err
		}
		if !found {
			continue
		}
		if !createBucketSameEntry(m, entry) {
			return held, 0, entry.Key, nil
		}
		held[entry.Key] = true
	}
	counted := uint64(len(held))
	for subject := range info.State.Subjects {
		if createBucketBarrierKey(strings.TrimPrefix(subject, selectionKeyPrefix), holdID) {
			counted++
		}
	}
	if info.State.NumSubjects > counted {
		extra = info.State.NumSubjects - counted
	}
	return held, extra, "", nil
}

// createBucketSameEntry reports whether m, a key's last message, is entry's.
func createBucketSameEntry(m replayMessage, entry createBucketWalkEntry) bool {
	return m.Op == entry.Op && bytes.Equal(m.Data, entry.Value)
}

// createBucketReplayWalk loads walk into the fresh bucket: each key's last message published
// again, in the walk's order (the plan: a walk is loaded by publishing, never restored as a
// snapshot), and adds the sequence each put was acknowledged at to own: the postcheck's account of
// the command's own writes, whose count is also what a stop says the load left.
func createBucketReplayWalk(ctx context.Context, nc *nats.Conn, walk *createBucketWalk, own map[uint64]bool) error {
	for _, entry := range walk.Entries {
		seq, err := createBucketPutWalkEntry(ctx, nc, entry)
		if err != nil {
			return err
		}
		own[seq] = true
	}
	return nil
}

// createBucketWalkPutTries bounds the tries of one replay put whose request went unanswered: the
// operator's user expires within 10 minutes of its decision, and its connection then reconnects
// on a fresh decision (plan §3a), so a long replay meets a closed connection.
const createBucketWalkPutTries = 3

// errKeyPresent is a replay put the bucket refused because the key already holds a message
// (10071, the expected last subject sequence 0 not met).
var errKeyPresent = errors.New("the key already holds a message")

// createBucketPutWalkEntry writes entry as a new key: conditional on the key holding no message,
// so the replay never overwrites a record. A put no stream answered ("no responders", as a bucket
// just created on three servers answers it) is sent again (createBucketFreshWrite); a put whose
// answer was lost is tried again once the connection is back; one the bucket then refuses as
// present is this replay's own when the key holds entry's message (plan §2, "Ambiguous results"),
// and otherwise the bucket is not this recovery's.
func createBucketPutWalkEntry(ctx context.Context, nc *nats.Conn, entry createBucketWalkEntry) (uint64, error) {
	for try := 1; ; try++ {
		var seq uint64
		err := createBucketFreshWrite(ctx, func() error {
			var err error
			seq, err = createBucketPutNew(ctx, nc, entry)
			return err
		})
		var answer *jetstream.APIError
		switch {
		case err == nil:
			return seq, nil
		case errors.Is(err, errKeyPresent):
			m, found, rerr := replayLeaderRead(ctx, nc, selection.SessionBucket, entry.Key)
			if rerr != nil {
				return 0, rerr
			}
			if try > 1 && found && createBucketSameEntry(m, entry) {
				return m.Seq, nil
			}
			return 0, fmt.Errorf("selection bucket exists and is not this recovery's: %s's key %s holds a message the replay did not write", selection.SessionBucket, entry.Key)
		case errors.As(err, &answer) || errors.Is(err, nats.ErrPermissionViolation) || try == createBucketWalkPutTries:
			return 0, err
		}
		if werr := createBucketAwaitConnected(ctx, nc); werr != nil {
			return 0, fmt.Errorf("%w (after %w)", werr, err)
		}
	}
}

// createBucketPutNew publishes entry on its key in the selection bucket with the walk's headers in
// their order, conditional on the key's last sequence being 0 (no message yet), and answers the
// acknowledged sequence. A header the publish's own condition sets is not taken from the walk.
func createBucketPutNew(ctx context.Context, nc *nats.Conn, entry createBucketWalkEntry) (uint64, error) {
	ctx, cancel := withRequestTimeout(ctx)
	defer cancel()
	msg := nats.NewMsg(selectionKeyPrefix + entry.Key)
	for _, header := range entry.Header {
		if strings.HasPrefix(header.Name, "Nats-Expected-") {
			continue
		}
		for _, value := range header.Values {
			msg.Header.Add(header.Name, value)
		}
	}
	msg.Header.Set(jetstream.ExpectedLastSubjSeqHeader, "0")
	msg.Data = entry.Value
	reply, err := nc.RequestMsgWithContext(ctx, msg)
	if err != nil {
		return 0, apiRefusal(nc, msg.Subject, fmt.Errorf("the replay's put of %s: %w", entry.Key, err))
	}
	var ack struct {
		Seq   uint64              `json:"seq"`
		Error *jetstream.APIError `json:"error"`
	}
	if err := json.Unmarshal(reply.Data, &ack); err != nil {
		return 0, fmt.Errorf("the acknowledgement of the replay's put of %s: %w", entry.Key, err)
	}
	switch {
	case ack.Error != nil && ack.Error.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence:
		return 0, fmt.Errorf("the replay's put of %s: %w", entry.Key, errKeyPresent)
	case ack.Error != nil:
		return 0, fmt.Errorf("the replay's put of %s: %w", entry.Key, ack.Error)
	case ack.Seq == 0:
		return 0, fmt.Errorf("the replay's put of %s was acknowledged with no sequence", entry.Key)
	}
	return ack.Seq, nil
}

// createBucketAwaitConnected waits until nc is connected again, within createBucketReconnectWait.
func createBucketAwaitConnected(ctx context.Context, nc *nats.Conn) error {
	deadline := time.Now().Add(createBucketReconnectWait)
	for !nc.IsConnected() {
		if nc.IsClosed() || time.Now().After(deadline) {
			return unknownf(nil, "the operator's connection did not reconnect within %s during the replay of %s", createBucketReconnectWait, selection.SessionBucket)
		}
		if err := sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}
