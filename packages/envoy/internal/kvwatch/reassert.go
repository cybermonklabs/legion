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
// Each writer is a listener, and each holds in its copy what the deleted bucket held: every record
// its watcher delivered or its own writes put there, kept through the new bucket's read until that
// read completes (Cache). So a cache whose read is the first of a new epoch (Read.Recreated)
// writes back the records it owns - the ones its own listener wrote and still answers for - from
// its BeginRead, before that read can complete and sweep them (Reasserter, Reassert). What a write
// site learns instead of the ack's incarnation is that the incarnation moved while its call was in
// flight: the copy drops that call's result (Dropped), and the site writes it again into the
// incarnation the handle now reaches (WriteAgain).

// Kept is one entry of a copy whose cache writes back what it holds: the value, the record it was
// decoded from or written as, byte for byte, so a write-back writes exactly the record and loses
// no field a newer build wrote, and when that record was written, on the clock the bucket's MaxAge
// is measured on (the entry's Created, or the moment of a write-through), so a write-back keeps
// the expiry the record had where the bucket allows it (Held.Expires).
type Kept[V any] struct {
	Value   V
	Record  []byte
	Written time.Time
}

// KeptEntry is the Kept a put record decodes to.
func KeptEntry[V any](entry Entry, value V) Kept[V] {
	return Kept[V]{Value: value, Record: entry.Value, Written: entry.Created}
}

// Held is one record a cache writes back into a recreated bucket (Reassert): its key, the record
// byte for byte, the entry the copy keeps for it, which the cache stores again once the new bucket
// took the record, and when the bucket would have dropped it (zero for a bucket that keeps its
// records, or a record the cache cannot date).
type Held[V any] struct {
	Key     string
	Record  []byte
	Value   V
	Expires time.Time
}

// HeldFrom is the Held of key's kept entry, which the copy stores again once the bucket took it,
// expiring maxAge after its record was written when the bucket has a MaxAge.
func HeldFrom[V any](key string, kept Kept[V], maxAge time.Duration) Held[Kept[V]] {
	held := Held[Kept[V]]{Key: key, Record: kept.Record, Value: kept}
	if maxAge > 0 && !kept.Written.IsZero() {
		held.Expires = kept.Written.Add(maxAge)
	}
	return held
}

// Reasserted is what one write-back did with the records a cache owned: how many it owned, how
// many the new bucket took, how many keys the new bucket already held a record of that stands (a
// write made in the new incarnation as new as the owned one or newer, a delete marker, or the same
// record written back by another process of this listener), how many had expired, so the bucket
// would have dropped them, and how many writes failed or were not answered before the bus went
// silent for reassertIdle, which the next read of the same epoch writes again (Reasserter).
// Window is the time from the new bucket's creation, as its stream reports it, to the end of the
// write-back, the bound on how long the records stood missing, and zero when the stream could not
// be read.
type Reasserted struct {
	Held    int
	Written int
	Present int
	Expired int
	Failed  int
	Elapsed time.Duration
	Window  time.Duration
}

// reassertWorkers is how many write-backs are in flight at once: a listener's thousand sessions
// are written in tens of round trips rather than a thousand, and no burst of one listener's writes
// is larger than this.
const reassertWorkers = 16

// reassertIdle bounds one write-back by the bus answering it: once no write of it has been answered
// for this long, every write still in flight or not yet sent fails, and is written again on the
// next read of the epoch. The hook it runs from holds the read's apply lock (Watcher.applyMu), so
// the new bucket's read waits for it, and a bus that stopped answering must not hold that read for
// good; a bucket of many records on a bus that answers is written whole, however long that takes.
// It is the bound the bus gives one JetStream request (bus.RequestTimeout).
const reassertIdle = bus.RequestTimeout

// settleAttempts bounds the writes one record takes against writes of the same key landing in
// between (settle): each refused write reads the key again and weighs what landed.
const settleAttempts = 3

// Reasserter is one cache's write-back between reads: it says which records a read writes back
// (Due) and keeps those its write-back could not write (Write), for the next read of the same
// epoch. A cache keeps one beside its copy, and only its hooks call it; no two of them run at once
// (Cache), so it takes no lock.
type Reasserter[V any] struct {
	epoch  uint64
	failed []Held[V]
}

// Due answers the records read writes back: for the first read of a new epoch, owned(), every
// record the cache owns and still holds live; for a later read of the same epoch, the records the
// write-back of that epoch could not write. A cache calls it in its BeginRead under its own mutex,
// after it began the copy's read.
func (p *Reasserter[V]) Due(read Read, owned func() []Held[V]) []Held[V] {
	if read.Recreated {
		p.epoch, p.failed = read.Epoch, nil
		return owned()
	}
	due := p.failed
	p.failed = nil
	if read.Epoch != p.epoch {
		return nil
	}
	return due
}

// Write writes held, Due's answer for read, back into the bucket read reads (Reassert), and keeps
// what it could not write for the next read of read's epoch. A cache calls it in its BeginRead after
// Due, with its own mutex released: took takes that mutex again, as Copy.Put needs it. A later read
// with nothing due writes and logs nothing; the first read of an epoch logs its line whatever it
// owned.
func (p *Reasserter[V]) Write(w *Watcher, read Read, held []Held[V], stands func(stored []byte, held Held[V]) bool, took func(held Held[V], revision uint64)) Reasserted {
	if !read.Recreated && len(held) == 0 {
		return Reasserted{}
	}
	result, failed := Reassert(w, read, held, stands, took)
	p.failed = append(p.failed, failed...)
	return result
}

// Reassert writes each held record into the bucket read reads, a bucket deleted and created again,
// and stores in the copy each one the bucket took (took), so the read's sweep keeps it. It answers
// what it did, and the records it could not write.
//
// A record whose expiry has passed is left out: the bucket would have dropped it. Each other write
// is create-only first: the expected-last-subject-sequence 0, which the server takes only while
// the key's subject holds no message at all (nats.go's Create would write over a delete marker
// too). Where the key holds a message, the write-back reads it: a delete marker stands - a delete
// made in the new bucket, which bus.KeyValue.DeleteAtRead leaves for a key it finds missing - and
// so does a record stands judges as new as the held one or newer, by the record's own stamp; an
// older record, such as one a copy that lagged wrote back first, is written over at its revision.
// So a write-back never writes over a delete or a newer write, and a lagged copy never leaves an
// older value. Where the new stream allows per-message TTLs (bus.KeyValue.StreamAllowsMessageTTL),
// the write carries what remained of the record's expiry, so the record leaves the bucket when it
// would have; the buckets this build creates do not allow them, and there a written-back record
// lives one MaxAge from its write-back.
//
// A read whose epoch the watcher has already left (another recreate came first) writes nothing
// back: the read of that newer bucket writes back for itself. One line is logged per write-back, at
// INFO, or at WARN when a write failed.
func Reassert[V any](w *Watcher, read Read, held []Held[V], stands func(stored []byte, held Held[V]) bool, took func(held Held[V], revision uint64)) (Reasserted, []Held[V]) {
	started := time.Now()
	result := Reasserted{Held: len(held)}
	kv, epoch := w.Handle()
	if epoch != read.Epoch {
		return result, nil
	}
	allowsTTL, created, streamErr := streamOf(kv)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	silent := time.AfterFunc(reassertIdle, cancel)
	defer silent.Stop()
	var (
		mu       sync.Mutex
		failed   []Held[V]
		firstErr error
		next     = make(chan Held[V])
		workers  sync.WaitGroup
	)
	for range min(reassertWorkers, len(held)) {
		workers.Go(func() {
			for h := range next {
				var ttl time.Duration
				expired := false
				if !h.Expires.IsZero() {
					ttl = time.Until(h.Expires)
					expired = ttl <= 0
				}
				if !allowsTTL {
					ttl = 0
				}
				var (
					revision uint64
					outcome  settled
					err      error
				)
				if !expired {
					revision, outcome, err = settle(ctx, kv, h.Key, h.Record, ttl, func(stored []byte) bool { return stands(stored, h) })
					silent.Reset(reassertIdle)
				}
				if outcome == settledWritten {
					took(h, revision)
				}
				mu.Lock()
				switch {
				case expired:
					result.Expired++
				case outcome == settledWritten:
					result.Written++
				case outcome == settledStands:
					result.Present++
				default:
					result.Failed++
					failed = append(failed, h)
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
	if streamErr == nil {
		result.Window = time.Since(created)
	}
	attrs := []any{
		slog.String("bucket", w.bucket),
		slog.Uint64("epoch", read.Epoch),
		slog.Bool("again", !read.Recreated),
		slog.Int("held", result.Held),
		slog.Int("written", result.Written),
		slog.Int("present", result.Present),
		slog.Int("expired", result.Expired),
		slog.Int("failed", result.Failed),
		slog.Bool("message_ttl", allowsTTL),
		slog.Int64("elapsed_ms", result.Elapsed.Milliseconds()),
		slog.Int64("window_ms", result.Window.Milliseconds()),
	}
	if firstErr != nil || result.Failed > 0 {
		if firstErr != nil {
			attrs = append(attrs, slog.String("error", firstErr.Error()))
		}
		w.logger().Warn(w.name+" re-asserted what it owned into the recreated bucket; some writes failed and are written again at its next read", attrs...)
		return result, failed
	}
	w.logger().Info(w.name+" re-asserted what it owned into the recreated bucket", attrs...)
	return result, nil
}

// settled is what one record's write-back came to (settle).
type settled uint8

const (
	// settledFailed is a write or read that failed, did not answer, or lost settleAttempts races.
	settledFailed settled = iota
	// settledWritten is the record written.
	settledWritten
	// settledStands is a record or a delete marker the bucket holds that stands.
	settledStands
)

// settle writes record for key into the bucket kv reaches, create-only, and where the key holds a
// message, over a record stands judges older, at that record's revision; a delete marker and a
// record as new or newer stand. A write refused because a write landed in between is weighed again,
// up to settleAttempts writes. ttl rides each write (bus.KeyValue.WriteBack). Each bucket call waits
// no longer than ctx.
func settle(ctx context.Context, kv bus.KeyValue, key string, record []byte, ttl time.Duration, stands func(stored []byte) bool) (uint64, settled, error) {
	var last uint64
	for range settleAttempts {
		revision, err := kv.WriteBack(ctx, key, record, last, ttl)
		if err == nil {
			return revision, settledWritten, nil
		}
		if !reassertFound(err) {
			return 0, settledFailed, err
		}
		got, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(key) })
		switch {
		case !answered:
			return 0, settledFailed, err
		case errors.Is(err, nats.ErrKeyNotFound):
			return 0, settledStands, nil
		case err != nil:
			return 0, settledFailed, err
		case stands(got.Value()):
			return 0, settledStands, nil
		}
		last = got.Revision()
	}
	return 0, settledFailed, fmt.Errorf("write %s back: a write landed under each of %d attempts", key, settleAttempts)
}

// reassertFound reports whether err is the server refusing a write whose expected last subject
// sequence is not the key's: the wrong last sequence JetStream answers (10071, and 10164 on a
// replicated stream).
func reassertFound(err error) bool {
	return errors.Is(err, nats.ErrKeyRevisionMismatch) || bus.IsCASConflict(err)
}

// streamOf reads whether the stream behind kv allows per-message TTLs and when it was created,
// within bus.RequestTimeout, for the expiry a write-back carries and the window it logs.
func streamOf(kv bus.KeyValue) (bool, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), bus.RequestTimeout)
	defer cancel()
	return kv.StreamAllowsMessageTTL(ctx)
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
// registration time, a known session's last_seen, a hard claim's claim time), so a record the new
// bucket holds can be ordered against it.
//
// The write is settled as a write-back is (settle): create-only, and where the key holds a message,
// over a record stands judges older - the one the copy held, written back by the read that found the
// new bucket (Reassert) - and never over a record as new as this write or newer, which read takes
// into the copy. A delete marker stands: a delete was made in the new bucket, and this write, made
// before the recreate, does not undo it. A write that landed is handed to took, which stores it in
// the copy at epoch and answers what the copy did; a write the copy drops again was overtaken by
// another recreate and is sent again, up to WriteAgainAttempts sends. Each bucket call waits no
// longer than ctx (bus.Bounded).
func WriteAgain(ctx context.Context, w *Watcher, key string, record []byte, stands func(stored []byte) bool, took func(epoch, revision uint64) Outcome, read func(entry Entry, epoch uint64)) error {
	for range WriteAgainAttempts {
		kv, epoch := w.Handle()
		revision, outcome, err := settle(ctx, kv, key, record, 0, stands)
		switch outcome {
		case settledFailed:
			return fmt.Errorf("write %s to the recreated bucket %s: %w", key, w.bucket, err)
		case settledStands:
			got, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return kv.Get(key) })
			if answered && err == nil {
				read(LegacyEntry(got), epoch)
			}
			return nil
		}
		if took(epoch, revision) != Dropped {
			return nil
		}
	}
	return fmt.Errorf("write %s to bucket %s: %w (%d sends)", key, w.bucket, ErrRecreatedUnderEachWrite, WriteAgainAttempts)
}
