package kvchunk

import (
	"fmt"
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
// generations of the records the store's cache holds.
//
// The store judges with it only while its cache follows the bucket (kvwatch.Watcher.Following): a
// copy that is behind could lack the manifest that names a chunk. A copy that follows holds every
// manifest the bucket held a moment ago, and a manifest lands within WriteBound and one request of
// its oldest chunk, so a chunk older than Grace that a manifest names is named by a record that
// cache holds. A manifest the cache could not join names its chunks to no record either: that
// record is damaged, which the store's next write of it replaces, and its chunks are collected like
// any other. A key under Prefix not in the shape this build writes (wellFormed) is no chunk of this
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
