package kvchunk

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// Fenced is how many chunk keys the copy's revision fence holds (kvwatch.Copy.FenceLen): one for
// each chunk the copy holds, since a chunk's delete leaves none (Delivered).
func (c *Chunks) Fenced() int { return c.copy.FenceLen() }

// chunk is one chunk in a copy: its bytes, and when the copy took it on the store's clock.
type chunk struct {
	value []byte
	taken time.Time
}

// Chunks is the copy of a bucket's chunks that a cache fed by a watcher of the bucket keeps beside
// the copy of its records, fed and swept by the same reads (BeginRead, Delivered, CompleteRead), so
// it holds exactly the chunks the bucket does once a read completes. The cache joins a manifest
// from it (Join), and the store's collection finds in it the chunks no record names (Unnamed).
//
// It has no lock of its own: the cache calls every method under the mutex that guards its copy of
// the records, as kvwatch.Copy's methods are called.
type Chunks struct {
	copy kvwatch.Copy[chunk]
	now  func() time.Time
}

// NewChunks is an empty copy of a bucket's chunks on now, the store's clock.
func NewChunks(now func() time.Time) *Chunks {
	c := &Chunks{now: now}
	c.copy = kvwatch.NewCopy(kvwatch.Rows[chunk]{Decode: c.decode})
	return c
}

// decode is the copy's decode (kvwatch.Rows). A chunk the copy already holds keeps the moment the
// copy first took it, so a read of the bucket again (a reconnect's, a recreated bucket's) does not
// restart its grace: a chunk is written once and never rewritten, so what the copy holds under its
// key is the same chunk.
func (c *Chunks) decode(entry kvwatch.Entry) (chunk, bool, error) {
	taken := c.now()
	if held, ok := c.copy.Get(entry.Key); ok {
		taken = held.taken
	}
	return chunk{value: entry.Value, taken: taken}, true, nil
}

// BeginRead starts a read of the bucket (kvwatch.Copy.BeginRead). Every chunk stays.
func (c *Chunks) BeginRead(read kvwatch.Read) { c.copy.BeginRead(read) }

// Delivered takes one record of a chunk's key the read delivered (kvwatch.Copy.Delivered). A
// chunk's delete, purge or removal for its age also drops its key from the fence
// (kvwatch.Copy.Forget): a chunk key is never written again, so no later record of it needs
// fencing, and a fence that kept it would grow by every chunk each rewrite replaced until a
// completed read of the bucket, which a watcher makes only on a reconnect.
func (c *Chunks) Delivered(entry kvwatch.Entry) error {
	err := c.copy.Delivered(entry)
	if entry.Op != kvwatch.OpPut {
		c.copy.Forget(entry.Key)
	}
	return err
}

// CompleteRead removes every chunk the read found no evidence of (kvwatch.Copy.CompleteRead).
func (c *Chunks) CompleteRead() { c.copy.CompleteRead() }

// Join is the record value, a value under a record's key that the watcher delivered, stores, with
// its layout: value itself for a record stored whole, and for a manifest the chunks it names,
// joined from the copy. The watcher delivers each key's last message in stream order, and a write
// stores a manifest's chunks before the manifest, so the copy holds every chunk a manifest names
// by the time the watcher delivers the manifest; one it does not hold, and chunks that do not join
// into what the manifest names, are ErrDamaged.
func (c *Chunks) Join(value []byte) ([]byte, Layout, error) {
	layout, err := ReadLayout(value)
	if err != nil || !layout.Chunked() {
		return value, layout, err
	}
	record, err := layout.join(func(key string) ([]byte, error) {
		if held, ok := c.copy.Get(key); ok {
			return held.value, nil
		}
		return nil, fmt.Errorf("%w: the copy holds no %s", ErrDamaged, key)
	})
	return record, layout, err
}

// Unnamed is every chunk of the copy whose generation named does not report and that the copy took
// before cutoff, sorted: the chunks a store's collection deletes (Collect). named reports the
// generations of the records the store's cache holds. The store judges with it only in a pass that
// CollectionReady let judge.
//
// A manifest the cache could not join names its chunks to no record either: that record is
// damaged, which the store's next write of it replaces, and its chunks are collected like any
// other. A key under Prefix not in the shape this build writes (wellFormed) is no chunk of this
// build's and is left where it is: a key nats.go refuses would fail its delete at every pass, and
// the collection stops at a failed delete.
func (c *Chunks) Unnamed(named func(generation string) bool, cutoff time.Time) []string {
	var unnamed []string
	for key, held := range c.copy.All() {
		if wellFormed(key) && !named(generation(key)) && held.taken.Before(cutoff) {
			unnamed = append(unnamed, key)
		}
	}
	slices.Sort(unnamed)
	return unnamed
}

// CollectionReady reports whether a pass of a store's chunk collection may judge from watcher's
// copy (Unnamed), and each pass asks it first. The copy must follow its bucket
// (kvwatch.Watcher.Following), then its current read must have applied a record at or past the
// bucket stream's last sequence, read once at the pass's start, within wait
// (kvwatch.Watcher.WaitCaughtUp; zero waits WriteBound, and only tests give another), and the copy
// must still follow. A copy that stays behind skips the pass with one WARN on log, naming the
// store, the current read's applied sequence and the stream's last sequence; a stream read that
// fails skips it with that read's error.
//
// Following alone proves only that a full read of the bucket completed since the copy last stopped
// following, not how far behind the live watch is now. Past the barrier, the current read has
// completed and has itself applied the bucket through that last sequence, so every manifest the
// bucket stored before the pass's read of its stream is in the copy, or a later record of its key:
// one the replaced watcher delivered and the current read swept is waited for until the current
// watcher delivers it again (WaitCaughtUp names that window). A manifest lands within WriteBound
// and one request of its oldest chunk, so a chunk the copy took more than Grace ago that a
// manifest names is named by a record the cache holds. That closes a copy that took a chunk and
// then applied nothing for Grace less WriteBound while it still reported following: a stall
// between nats.go's queue and the cache, or a frozen process.
func CollectionReady(ctx context.Context, watcher *kvwatch.Watcher, wait time.Duration, log *slog.Logger) (bool, error) {
	if !watcher.Following() {
		return false, nil
	}
	progress, err := watcher.WaitCaughtUp(ctx, cmp.Or(wait, WriteBound))
	if err != nil {
		return false, err
	}
	if !progress.CaughtUp {
		log.Warn(watcher.Name()+" skipped chunk collection: its cache is behind its bucket",
			slog.String("store", watcher.Name()),
			slog.Uint64("applied_seq", progress.Applied),
			slog.Uint64("last_seq", progress.LastSeq),
		)
		return false, nil
	}
	return watcher.Following(), nil
}
