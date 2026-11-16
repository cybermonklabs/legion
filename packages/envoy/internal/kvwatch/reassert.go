package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// A bucket deleted and created again loses every record the deleted stream held, and a write that
// stream acknowledged is acknowledged all the same: a JetStream publish ack names the stream and
// its sequence (nats.PubAck: Stream, Sequence, Duplicate, Domain), never the incarnation, and a
// recreated bucket's stream keeps the old one's name. So a writer told its write landed cannot
// tell from the ack whether the bucket it landed in is still there, and the copy, which follows
// the new bucket, agrees with that bucket that the record is gone (INFRA-4094).
//
// The writers are the listeners, and each holds in its copy what the deleted bucket held: every
// record its watcher delivered or its own writes put there, kept through the new bucket's read
// until that read completes (Cache). So a cache whose read is the first of a new epoch
// (Read.Recreated) writes back what it held (Reassert), from its BeginRead, before that read can
// complete and sweep it. What a write site learns instead of the ack's incarnation is that the
// incarnation moved while its call was in flight: the copy drops that call's result (Dropped),
// and the site writes it again into the incarnation the handle now reaches.

// Held is one record a cache re-asserts into a recreated bucket (Reassert): its key, the record as
// the cache writes it, and the entry the copy keeps for it, which the cache stores again once the
// new bucket took the record.
type Held[V any] struct {
	Key    string
	Record []byte
	Value  V
}

// Reasserted is what one re-assert did with the records a cache held: how many it held, how many
// the new bucket took, how many keys the new bucket already held a record of (a write made in the
// new incarnation, a delete marker, or another listener's re-assert), and how many writes failed or
// were not answered before the bus went silent for reassertIdle. Window is the time from the new
// bucket's creation, as its stream reports it, to the end of the re-assert, the bound on how long
// the records stood missing, and zero when the stream could not be read.
type Reasserted struct {
	Held    int
	Written int
	Present int
	Failed  int
	Elapsed time.Duration
	Window  time.Duration
}

// reassertWorkers is how many re-assert writes are in flight at once: a fleet's thousand sessions
// are written in tens of round trips rather than a thousand, and no burst of one listener's writes
// is larger than this.
const reassertWorkers = 16

// reassertIdle bounds one re-assert by the bus answering it: once no write of it has been answered
// for this long, every write still in flight or not yet sent fails. The hook it runs from holds the
// read's apply lock (Watcher.applyMu), so the new bucket's read waits for it, and a bus that stopped
// answering must not hold that read for good; a bucket of many records on a bus that answers is
// written whole, however long that takes. It is the bound the bus gives one JetStream request
// (bus.RequestTimeout).
const reassertIdle = bus.RequestTimeout

// Reassert writes each held record into the bucket read reads, a bucket deleted and created
// again, and stores in the copy each one the bucket took (took), so the read's sweep keeps it. A
// cache calls it from its BeginRead for a read whose Recreated is set, once it has started that
// read and released its own mutex: took takes that mutex again, and Copy.Put needs it.
//
// Each write is create-only: nats.KeyValue.Update at revision 0, which sends the
// expected-last-subject-sequence 0 the server takes only while the key's subject holds no message
// at all (nats.go's Create would also write over a delete marker, nats.go kv.go Create). So a
// re-assert never writes over anything the new incarnation holds: a write made there since, a
// delete made there since (bus.KeyValue.DeleteAtRead marks a key it finds missing), or another
// listener's re-assert of the same key. Every listener re-asserts what it held, and whichever lands
// first stands; the others are counted Present.
//
// A read whose epoch the watcher has already left (another recreate came first) re-asserts nothing:
// the read of that newer bucket re-asserts for itself. One line is logged per re-assert, at INFO,
// or at WARN when a write failed.
func Reassert[V any](w *Watcher, read Read, held []Held[V], took func(held Held[V], revision uint64)) Reasserted {
	started := time.Now()
	result := Reasserted{Held: len(held)}
	kv, epoch := w.Handle()
	if epoch != read.Epoch {
		return result
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	silent := time.AfterFunc(reassertIdle, cancel)
	defer silent.Stop()
	var (
		mu       sync.Mutex
		firstErr error
		next     = make(chan Held[V])
		workers  sync.WaitGroup
	)
	for range min(reassertWorkers, len(held)) {
		workers.Go(func() {
			for h := range next {
				revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.Update(h.Key, h.Record, 0) })
				if answered && err == nil {
					took(h, revision)
				}
				silent.Reset(reassertIdle)
				mu.Lock()
				switch {
				case answered && err == nil:
					result.Written++
				case answered && reassertFound(err):
					result.Present++
				default:
					result.Failed++
					if firstErr == nil {
						firstErr = err
					}
				}
				mu.Unlock()
			}
		})
	}
	for _, h := range held {
		next <- h
	}
	close(next)
	workers.Wait()
	result.Elapsed = time.Since(started)
	if created, err := streamCreated(kv); err == nil {
		result.Window = time.Since(created)
	}
	attrs := []any{
		slog.String("bucket", w.bucket),
		slog.Uint64("epoch", read.Epoch),
		slog.Int("held", result.Held),
		slog.Int("written", result.Written),
		slog.Int("present", result.Present),
		slog.Int("failed", result.Failed),
		slog.Int64("elapsed_ms", result.Elapsed.Milliseconds()),
		slog.Int64("window_ms", result.Window.Milliseconds()),
	}
	if firstErr != nil {
		w.logger().Warn(w.name+" re-asserted what it held into the recreated bucket; some writes failed", append(attrs, slog.String("error", firstErr.Error()))...)
		return result
	}
	w.logger().Info(w.name+" re-asserted what it held into the recreated bucket", attrs...)
	return result
}

// reassertFound reports whether err is the server refusing a create-only write because the key's
// subject already holds a message: the wrong last sequence JetStream answers a write whose
// expected last subject sequence is not the subject's (10071, and 10164 on a replicated stream).
func reassertFound(err error) bool {
	return errors.Is(err, nats.ErrKeyRevisionMismatch) || bus.IsCASConflict(err)
}

// streamCreated is when the stream behind kv was created, read through its jetstream context
// within bus.RequestTimeout, for the window a re-assert logs. It has a bound of its own, since a
// re-assert that spent its budget still logs its line.
func streamCreated(kv bus.KeyValue) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bus.RequestTimeout)
	defer cancel()
	stream, err := kv.Streams().Stream(ctx, "KV_"+kv.Bucket())
	if err != nil {
		return time.Time{}, err
	}
	return stream.CachedInfo().Created, nil
}

// WriteAgainAttempts bounds how many times one write is sent again after the copy dropped its
// result (WriteAgain, and a store's delete made again the same way): each time is a bucket
// recreated while that write was in flight, so one past it is a bucket recreated over and over,
// which the caller is told about (ErrRecreatedUnderEachWrite) rather than chased.
const WriteAgainAttempts = 3

// ErrRecreatedUnderEachWrite is a write the copy dropped the result of under every one of its
// WriteAgainAttempts sends: the bucket was recreated under each of them, so the write is not known
// to stand in any bucket.
var ErrRecreatedUnderEachWrite = errors.New("the bucket was recreated under each send of the write")

// WriteAgain writes record for key again into the bucket w's handle reaches now, after the copy
// dropped the first write's result (Dropped): the bucket was recreated while that write was in
// flight, and its ack, which names no incarnation, cannot say whether the deleted bucket or the new
// one took it. It is for a last-writer-wins write whose record carries its own stamp (a session's
// registration time, a known session's last_seen), so a record the new bucket holds can be ordered
// against it.
//
// The key is read in the new bucket first. A record there that stands (stands: as new as this
// write or newer - the write's own first send, or a later write) is left, and read takes it into
// the copy. An older record - the one the copy held, written back by the read that found the new
// bucket (Reassert) - is written over at its revision, and a key the bucket holds nothing for is
// written create-only. A key whose latest record is a delete or purge marker stands deleted: a
// delete was made in the new bucket, and this write, made before the recreate, does not undo it.
// A write that landed is handed to took, which stores it in the copy at epoch and answers what the
// copy did; a write the copy drops again was overtaken by another recreate and is sent again, up to
// WriteAgainAttempts sends. Each bucket call waits no longer than ctx (bus.Bounded).
func WriteAgain(ctx context.Context, w *Watcher, key string, record []byte, stands func(stored []byte) bool, took func(epoch, revision uint64) Outcome, read func(entry Entry, epoch uint64)) error {
	for range WriteAgainAttempts {
		kv, epoch := w.Handle()
		got, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(key) })
		if !answered || (err != nil && !errors.Is(err, nats.ErrKeyNotFound)) {
			return fmt.Errorf("read %s from the recreated bucket %s: %w", key, w.bucket, err)
		}
		var revision uint64
		if err == nil {
			if stands(got.Value()) {
				read(LegacyEntry(got), epoch)
				return nil
			}
			revision = got.Revision()
		}
		written, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.Update(key, record, revision) })
		if !answered {
			return fmt.Errorf("write %s to the recreated bucket %s: %w", key, w.bucket, err)
		}
		if err != nil && reassertFound(err) {
			if revision == 0 {
				// The read found no record and the create found the subject holding a message:
				// a record landed in between, which the next pass weighs, or the key's latest
				// record is a delete marker, which stands.
				_, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(key) })
				if !answered || (err != nil && !errors.Is(err, nats.ErrKeyNotFound)) {
					return fmt.Errorf("read %s from the recreated bucket %s: %w", key, w.bucket, err)
				}
				if err != nil {
					return nil
				}
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("write %s to the recreated bucket %s: %w", key, w.bucket, err)
		}
		if took(epoch, written) != Dropped {
			return nil
		}
	}
	return fmt.Errorf("write %s to bucket %s: %w (%d sends)", key, w.bucket, ErrRecreatedUnderEachWrite, WriteAgainAttempts)
}
