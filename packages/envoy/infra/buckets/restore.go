package buckets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/jsm.go"
	"github.com/nats-io/jsm.go/api"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/infra/writerhold"
)

// ManifestFile is the backup's manifest, beside each bucket's directory.
const ManifestFile = "manifest.json"

// Manifest is a backup's manifest: each bucket's archive, by its bucket's name.
type Manifest struct {
	Buckets map[string]Archive `json:"buckets"`
}

// Archive is one bucket's archive in a backup: the sha256 of its data file and of jsm.go's meta
// file (backup.json), and the stream's state the snapshot recorded, which a restore of it must
// reproduce.
type Archive struct {
	SHA256     string `json:"sha256"`
	MetaSHA256 string `json:"meta_sha256"`
	LastSeq    uint64 `json:"last_seq"`
	Messages   uint64 `json:"messages"`
}

// snapshotMeta is the part of jsm.go's backup.json the manifest records: the stream's state at
// the snapshot.
type snapshotMeta struct {
	State struct {
		Msgs    uint64 `json:"messages"`
		LastSeq uint64 `json:"last_seq"`
	} `json:"state"`
}

// Backup snapshots each of buckets' streams on nc into dir/<bucket> (jsm.go's
// SnapshotToDirectory, which takes its replies under nc's inbox prefix), then writes dir's
// manifest, and returns the done entry's outputs: each bucket's archive sha256. It runs only
// inside a hold, as `buckets backup`, the first command of the buckets-recreate and
// buckets-restore workflows, since a snapshot names its own deliver subject. A dir that already
// holds a manifest is refused, so a backup never overwrites one.
func Backup(ctx context.Context, nc *nats.Conn, dir string, buckets []string, stdout io.Writer) (map[string]string, error) {
	if _, err := os.Stat(filepath.Join(dir, ManifestFile)); err == nil {
		return nil, fmt.Errorf("%s already holds a backup's manifest: back up into a new directory", dir)
	}
	mgr, err := jsm.New(nc, jsm.WithTimeout(30*time.Second))
	if err != nil {
		return nil, err
	}
	manifest := Manifest{Buckets: map[string]Archive{}}
	outputs := map[string]string{}
	for _, bucket := range buckets {
		stream, err := mgr.LoadStream("KV_" + bucket)
		if err != nil {
			return nil, fmt.Errorf("read KV_%s: %w", bucket, err)
		}
		bucketDir := filepath.Join(dir, bucket)
		if err := os.MkdirAll(bucketDir, 0o700); err != nil {
			return nil, err
		}
		if _, err := stream.SnapshotToDirectory(ctx, bucketDir); err != nil {
			return nil, fmt.Errorf("snapshot KV_%s: %w", bucket, err)
		}
		archive, err := readArchive(bucketDir)
		if err != nil {
			return nil, fmt.Errorf("the snapshot of KV_%s: %w", bucket, err)
		}
		manifest.Buckets[bucket] = archive
		outputs["sha256."+bucket] = archive.SHA256
		fmt.Fprintf(stdout, "backed up %s: %d messages to seq %d, sha256 %s\n", bucket, archive.Messages, archive.LastSeq, archive.SHA256)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, ManifestFile+".tmp")
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return nil, err
	}
	return outputs, os.Rename(tmp, filepath.Join(dir, ManifestFile))
}

// readArchive reads the archive in bucketDir: its files' sha256 and the state its meta file holds.
func readArchive(bucketDir string) (Archive, error) {
	data, err := jsm.SnapshotDataPath(bucketDir)
	if err != nil {
		return Archive{}, err
	}
	sum, err := fileSHA256(data)
	if err != nil {
		return Archive{}, err
	}
	metaPath := filepath.Join(bucketDir, jsm.SnapshotMetaFile)
	metaSum, err := fileSHA256(metaPath)
	if err != nil {
		return Archive{}, err
	}
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return Archive{}, err
	}
	var meta snapshotMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Archive{}, fmt.Errorf("%s does not decode: %w", metaPath, err)
	}
	return Archive{SHA256: sum, MetaSHA256: metaSum, LastSeq: meta.State.LastSeq, Messages: meta.State.Msgs}, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ReadManifest reads dir's manifest and validates each of buckets' archives against it: the
// archive is there and its data and meta files hash to what the manifest records. Anything else
// refuses the restore, naming the bucket, before any hold is set.
func ReadManifest(dir string, buckets []string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, fmt.Errorf("read the backup's manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("the backup's manifest does not decode: %w", err)
	}
	var problems []string
	for _, bucket := range buckets {
		want, ok := m.Buckets[bucket]
		if !ok {
			problems = append(problems, bucket+" is not in the backup")
			continue
		}
		got, err := readArchive(filepath.Join(dir, bucket))
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s's archive: %v", bucket, err))
		case got.SHA256 != want.SHA256 || got.MetaSHA256 != want.MetaSHA256:
			problems = append(problems, fmt.Sprintf("%s's archive does not hash to the manifest's sha256", bucket))
		case got.LastSeq != want.LastSeq || got.Messages != want.Messages:
			problems = append(problems, fmt.Sprintf("%s's archive's state is not the manifest's", bucket))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("the backup in %s is not whole: %s", dir, strings.Join(problems, "; "))
	}
	return &m, nil
}

// The JetStream errors a restore's start answers, nats-server's errors.json: 10130 a restore
// refused because the stream's name is still taken, the leftover of a cut restore; 10049 the
// stream's store not made yet, which passes.
const (
	errRestoreNameInUse = 10130
	errCreatingStore    = 10049
)

// restoreStart bounds how long a restore's start is retried through 10130, 10049 and "no
// responders", which a cut restore's leftover answers for about 10 s on a clustered hub.
var restoreStart = 60 * time.Second

// restoreAll restores each of buckets from dir/<bucket> on nc, each with restoreOne. A restore that
// stops is a *writerhold.Left naming what the command left: each bucket restored before it, and the
// stopped one's stream deleted.
func restoreAll(ctx context.Context, nc *nats.Conn, stdout io.Writer, dir string, m *Manifest, buckets []string, runs int) (map[string]writerhold.Change, map[string]string, error) {
	changes, outputs := map[string]writerhold.Change{}, map[string]string{}
	var left []string
	for _, bucket := range buckets {
		archive := m.Buckets[bucket]
		if err := restoreOne(ctx, nc, stdout, bucket, filepath.Join(dir, bucket), archive, runs); err != nil {
			left = append(left, "KV_"+bucket+" deleted, its restore stopped")
			return nil, nil, &writerhold.Left{What: strings.Join(left, ", "), Err: err}
		}
		left = append(left, "KV_"+bucket+" restored")
		fmt.Fprintf(stdout, "restored %s: %d messages to seq %d\n", bucket, archive.Messages, archive.LastSeq)
		changes["KV_"+bucket] = writerhold.Change{Kind: writerhold.ChangeRestored, Archive: writerhold.ArchiveState{LastSeq: archive.LastSeq, Messages: archive.Messages}}
		outputs["sha256."+bucket] = archive.SHA256
	}
	return changes, outputs, nil
}

// restoreOne restores bucket's stream from bucketDir, which replaces the stream: it deletes the
// stream first, since a restore makes it and one that exists refuses it. A restore cut part way
// (its connection closed, an answer that does not come) is run again from its first chunk, runs
// times in all, each after the cut's leftover is gone; a cut on the last run stops the command,
// naming the bucket and each run's error, with the leftover deleted, so no partial stream stays.
// A restored stream whose state is not the archive's stops it too.
func restoreOne(ctx context.Context, nc *nats.Conn, stdout io.Writer, bucket, bucketDir string, archive Archive, runs int) error {
	stream := "KV_" + bucket
	js, err := nc.JetStream(nats.Context(ctx))
	if err != nil {
		return err
	}
	if err := js.DeleteStream(stream); err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
		return fmt.Errorf("delete %s before its restore: %w", stream, err)
	}
	var errs []error
	for run := 1; run <= runs; run++ {
		state, err := startRestore(ctx, nc, stdout, stream, bucketDir)
		if err == nil {
			if state.LastSeq != archive.LastSeq || state.Msgs != archive.Messages {
				errs = append(errs, fmt.Errorf("run %d restored %d messages to seq %d, and the archive holds %d to seq %d", run, state.Msgs, state.LastSeq, archive.Messages, archive.LastSeq))
				break
			}
			return nil
		}
		errs = append(errs, fmt.Errorf("run %d: %w", run, err))
		fmt.Fprintf(stdout, "restore of %s: run %d of %d stopped part way: %v\n", bucket, run, runs, err)
		if run == runs {
			break
		}
		if err := deleteLeftover(ctx, nc, stream); err != nil {
			errs = append(errs, err)
			break
		}
	}
	if err := deleteLeftover(ctx, nc, stream); err != nil {
		errs = append(errs, err)
	}
	return fmt.Errorf("restore %s stopped, and %s is deleted: %w", bucket, stream, errors.Join(errs...))
}

// startRestore runs one restore of stream, retrying its start, before the server accepts it,
// through a cut restore's leftover: 10130 (the leftover deleted first), 10049 and "no responders",
// within restoreStart. Any other error, or any once the server accepted the restore, is the run's.
func startRestore(ctx context.Context, nc *nats.Conn, stdout io.Writer, stream, bucketDir string) (streamState, error) {
	deadline := time.Now().Add(restoreStart)
	for {
		started, state, err := restoreRun(ctx, nc, stream, bucketDir)
		if err == nil {
			return streamState{Msgs: state.Msgs, LastSeq: state.LastSeq}, nil
		}
		if started || time.Now().After(deadline) {
			return streamState{}, err
		}
		var apiErr *nats.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.ErrorCode == errRestoreNameInUse,
			strings.Contains(err.Error(), fmt.Sprint(errRestoreNameInUse)):
			if err := deleteNow(nc, stream); err != nil {
				return streamState{}, err
			}
		case errors.As(err, &apiErr) && apiErr.ErrorCode == errCreatingStore,
			strings.Contains(err.Error(), fmt.Sprint(errCreatingStore)),
			errors.Is(err, nats.ErrNoResponders), strings.Contains(err.Error(), "no responders"):
		default:
			return streamState{}, err
		}
		fmt.Fprintf(stdout, "restore of %s: its start was refused (%v); it starts again\n", stream, err)
		select {
		case <-ctx.Done():
			return streamState{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// restoreRun is one restore of stream through jsm.go, on a connection of its own made with nc's
// options (its credential, its inbox prefix, its dialer) and no reconnect: a restore whose
// connection is cut part way must end then, not resume its chunks on a reconnected connection that
// the server's restore no longer answers, whose last request jsm.go waits an hour for. started
// says the server accepted the restore (jsm.go's first progress notice comes after its
// STREAM.RESTORE is answered and before its first chunk), so an error after it is a cut restore,
// whose leftover must go before a rerun, and never a start to retry.
func restoreRun(ctx context.Context, nc *nats.Conn, stream, bucketDir string) (started bool, state *api.StreamState, err error) {
	opts := nc.Opts
	opts.AllowReconnect = false
	opts.MaxReconnect = 0
	opts.Name = nc.Opts.Name + " restore"
	opts.DisconnectedErrCB, opts.ReconnectedCB, opts.ClosedCB = nil, nil, nil
	conn, err := opts.Connect()
	if err != nil {
		return false, nil, fmt.Errorf("connect for the restore of %s: %w", stream, err)
	}
	defer conn.Close()
	mgr, err := jsm.New(conn, jsm.WithTimeout(30*time.Second))
	if err != nil {
		return false, nil, err
	}
	var notified atomic.Bool
	_, state, err = mgr.RestoreSnapshotFromDirectory(ctx, stream, bucketDir, jsm.RestoreNotify(func(jsm.RestoreProgress) { notified.Store(true) }))
	return notified.Load(), state, err
}

// streamState is the part of a restored stream's state restoreOne compares.
type streamState struct {
	Msgs, LastSeq uint64
}

// leftoverWait is how long deleteLeftover watches for a cut restore's stream to appear: on a
// clustered hub STREAM.INFO answers 10059 for about 10 s after the cut, then an empty stream
// appears and stays.
var leftoverWait = 20 * time.Second

// deleteLeftover deletes what a cut restore of stream left: it watches for leftoverWait, deleting
// the stream each time it is found, and returns once the stream has stayed absent to the end.
func deleteLeftover(ctx context.Context, nc *nats.Conn, stream string) error {
	end := time.Now().Add(leftoverWait)
	for time.Now().Before(end) {
		if err := deleteNow(nc, stream); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return deleteNow(nc, stream)
}

// deleteNow deletes stream if it is there.
func deleteNow(nc *nats.Conn, stream string) error {
	js, err := nc.JetStream()
	if err != nil {
		return err
	}
	if err := js.DeleteStream(stream); err != nil && !errors.Is(err, nats.ErrStreamNotFound) {
		return fmt.Errorf("delete the leftover of a cut restore of %s: %w", stream, err)
	}
	return nil
}
