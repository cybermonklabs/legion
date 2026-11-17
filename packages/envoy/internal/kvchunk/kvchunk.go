// Package kvchunk stores a KV record whose encoding is larger than one NATS message, and reads it
// back whole. A store of a JetStream KV bucket whose record can outgrow the server's max payload -
// the interest record (internal/store) and the known-session record (internal/session) - writes
// and reads that record through this package, and every other record of the bucket is untouched.
//
// A record that fits one message is stored whole under its key, as every earlier build stores it.
// A larger record is stored in chunks, each a value of its own under a key of the chunk namespace
// (Prefix), and the value under the record's key is a manifest that names them (Layout). The size
// of a chunk is read from the connection's max payload at each write (bus.KeyValue.MaxPayload), so
// a server that raises or lowers its max payload changes the layout with no new build.
//
// Every reader gets the whole record one write stored, or an error (ErrDamaged), never part of one
// and never parts of two. Three things make that so. A chunk is written once, under a generation
// no other write uses, and never rewritten. A write stores its chunks before its manifest, with the
// write the store makes of a whole record (Write). A reader checks the length and the SHA-256 the
// manifest names before it hands the record back. A read from the bucket that meets a chunk a
// rewrite deleted under it reads the manifest again (Read). A cache fed by a watcher of the bucket,
// which delivers each key's last message in stream order, is delivered a manifest's chunks before
// the manifest itself, and joins it from the chunks it was delivered (Chunks).
//
// No chunk stays in the bucket that no record names. A write deletes the chunks of the record it
// replaced once its own head has landed, and its own chunks when the bucket refused its head or it
// passed WriteBound before sending it. What a crash or a call that did not answer leaves is
// deleted by the store's own periodic collection (Chunks.Unnamed, Collect), once the cache took it
// longer ago than Grace.
package kvchunk

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// Prefix begins the key of every chunk, and no record's key: a write of a record under a key with
// this prefix is refused (ReservedKey). A chunk's key is the prefix, the generation of the write
// that stored it and the chunk's index, `=chunk.<generation>.<index>`. A store's collection
// deletes every key under it that no record names, so a later layout that this build cannot read
// names its chunks under another prefix.
const Prefix = "=chunk."

// manifestPrefix begins a manifest, the layout as JSON after it. A record stored whole is JSON and
// begins with `{`, so the two are told apart by their first byte, and a build that reads every
// value as one JSON record, as every earlier build does, reads a manifest as a record it cannot
// decode rather than as an empty one.
var manifestPrefix = []byte("chunked ")

// HeaderBytes is the room a value leaves under the server's max payload for the headers nats.go
// adds to a KV write: a compare-and-swap's expected-last-sequence header is under 100 bytes.
const HeaderBytes = 1 << 10

// WriteBound is the longest a write may take from the start of its first chunk to its head: a
// write past it sends no head and deletes the chunks it wrote. A manifest therefore lands within
// this bound and one request of its oldest chunk, which is what lets a collection judge a chunk by
// its age (Grace). An HTTP route's budget is seconds, so only a write with no deadline of its own
// can come near it.
const WriteBound = time.Minute

// Grace is how long a chunk must have been in a cache's copy before a collection may delete it for
// being named by no record (Chunks.Unnamed). It is ten times WriteBound, so a chunk of a write still
// under way, whose manifest no cache holds yet, is never that old; and it is measured on the
// store's own clock from when its copy took the chunk, so no difference between the store's clock
// and the server's can shorten it.
const Grace = 10 * time.Minute

// readAttempts bounds the times a read from the bucket reads a record again after a rewrite
// deleted one of its chunks under it (Read). A record is rewritten seconds apart at the most, so a
// read is overtaken that often only by a burst of writes, past which the caller sees ErrMoved.
const readAttempts = 5

var (
	// ErrDamaged is a record no read can join: a manifest this build cannot decode, chunks that do
	// not join into the record their manifest names, or a chunk the bucket does not hold while the
	// manifest naming it still stands. Reading it again answers the same, so a store answers it as
	// a record it cannot decode, which its next write replaces.
	ErrDamaged = errors.New("the chunked record is damaged")
	// ErrMoved is a read from the bucket that every rewrite of readAttempts overtook. It is not
	// the bucket's answer about the record: the next read can succeed.
	ErrMoved = errors.New("the chunked record was rewritten under every read of it")
	// errMissing is a chunk a manifest names that a reader could not find.
	errMissing = errors.New("a chunk the manifest names is not there")
)

// Layout is how one record is laid out in the bucket: whole under its key, the zero value, or in
// Chunks chunks of the write Generation, which the manifest under the record's key names with the
// record's length and SHA-256.
type Layout struct {
	Generation string `json:"generation"`
	Chunks     int    `json:"chunks"`
	Bytes      int    `json:"bytes"`
	SHA256     string `json:"sha256"`
}

// Chunked reports whether the record is stored in chunks.
func (l Layout) Chunked() bool { return l.Generation != "" }

// Keys is the key of each of the layout's chunks, none for a record stored whole.
func (l Layout) Keys() []string {
	keys := make([]string, l.Chunks)
	for i := range keys {
		keys[i] = Prefix + l.Generation + "." + strconv.Itoa(i)
	}
	return keys
}

// IsChunkKey reports whether key is a chunk's, which no record's key is.
func IsChunkKey(key string) bool { return strings.HasPrefix(key, Prefix) }

// generation is the generation a chunk's key names.
func generation(key string) string {
	generation, _, _ := strings.Cut(strings.TrimPrefix(key, Prefix), ".")
	return generation
}

// IsManifest reports whether value, stored under a record's key, is a manifest.
func IsManifest(value []byte) bool { return bytes.HasPrefix(value, manifestPrefix) }

// ReservedKey is the refusal of a write of a record under a key with Prefix, where no record may
// stand. It is a bus.ErrRefused, so a listener route answers it 400 naming the key, as it answers a
// key nats.go refuses.
type ReservedKey string

func (key ReservedKey) Error() string {
	return fmt.Sprintf("key %q is under %q, the keys a bucket keeps for the chunks of its records", string(key), Prefix)
}

func (ReservedKey) Is(target error) bool { return target == bus.ErrRefused }

// layOut lays record out for a bucket whose messages take at most maxPayload bytes: the value for
// the record's key and the chunks it names. A record that fits one message with room for its
// headers is that value itself; a larger one is chunks of that size under a new generation, and a
// manifest naming them.
func layOut(record []byte, maxPayload int) ([]byte, Layout, [][]byte, error) {
	limit := maxPayload - HeaderBytes
	if limit <= 0 {
		return nil, Layout{}, nil, fmt.Errorf("%w: the server's max payload of %d bytes leaves no room for a value beside %d bytes of headers",
			bus.ErrTooLarge, maxPayload, HeaderBytes)
	}
	if len(record) <= limit {
		return record, Layout{}, nil, nil
	}
	sum := sha256.Sum256(record)
	layout := Layout{Generation: rand.Text(), Bytes: len(record), SHA256: hex.EncodeToString(sum[:])}
	chunks := slices.Collect(slices.Chunk(record, limit))
	layout.Chunks = len(chunks)
	manifest, err := json.Marshal(layout)
	if err != nil {
		return nil, Layout{}, nil, err
	}
	return append(slices.Clone(manifestPrefix), manifest...), layout, chunks, nil
}

// ReadLayout is the layout a value under a record's key names: the zero layout for a record stored
// whole, and the manifest's for a manifest. A manifest this build cannot decode, or one that names
// no chunk, is ErrDamaged.
func ReadLayout(value []byte) (Layout, error) {
	manifest, ok := bytes.CutPrefix(value, manifestPrefix)
	if !ok {
		return Layout{}, nil
	}
	var layout Layout
	if err := json.Unmarshal(manifest, &layout); err != nil {
		return Layout{}, fmt.Errorf("%w: decode its manifest: %w", ErrDamaged, err)
	}
	if layout.Generation == "" || layout.Chunks <= 0 {
		return Layout{}, fmt.Errorf("%w: its manifest names no chunks: %s", ErrDamaged, manifest)
	}
	return layout, nil
}

// join joins the layout's chunks, each read by chunk, and returns the record when it is the one the
// manifest names: its length and its SHA-256, ErrDamaged otherwise. An error chunk returns,
// errMissing among them, is returned as it is.
func (l Layout) join(chunk func(key string) ([]byte, error)) ([]byte, error) {
	record := make([]byte, 0, l.Bytes)
	for _, key := range l.Keys() {
		value, err := chunk(key)
		if err != nil {
			return nil, err
		}
		if len(record)+len(value) > l.Bytes {
			return nil, fmt.Errorf("%w: generation %s holds more than %d bytes", ErrDamaged, l.Generation, l.Bytes)
		}
		record = append(record, value...)
	}
	if len(record) != l.Bytes {
		return nil, fmt.Errorf("%w: generation %s holds %d bytes, not %d", ErrDamaged, l.Generation, len(record), l.Bytes)
	}
	if sum := sha256.Sum256(record); hex.EncodeToString(sum[:]) != l.SHA256 {
		return nil, fmt.Errorf("%w: generation %s joins into SHA-256 %x, not %s", ErrDamaged, l.Generation, sum, l.SHA256)
	}
	return record, nil
}

// Read is the record entry, the value under key that a read through kv answered, stores, with the
// layout it is stored in and the revision of the entry it was read from. A record stored whole is
// entry's value itself. A chunk is read from the leader of the bucket's stream
// (bus.KeyValue.GetFromLeader), because a copy of the stream that is behind the leader can lack a
// chunk the leader holds. A chunk the leader does not hold either was deleted by a rewrite that
// replaced the manifest since, or the record is damaged, and the manifest the leader holds now says
// which: one at another revision is read in its place, up to readAttempts times (ErrMoved past
// them), none at all is nats.ErrKeyNotFound, and one at the same revision is ErrDamaged. A
// record whose chunks do not join into what its manifest names is ErrDamaged too, and carries its
// layout, so the write that replaces it deletes its chunks. Any other error is a read that did not
// answer. Each read waits no longer than ctx.
func Read(ctx context.Context, kv bus.KeyValue, key string, entry nats.KeyValueEntry) ([]byte, Layout, uint64, error) {
	for attempt := 1; ; attempt++ {
		revision := entry.Revision()
		layout, err := ReadLayout(entry.Value())
		if err != nil || !layout.Chunked() {
			return entry.Value(), layout, revision, err
		}
		record, err := layout.join(func(chunk string) ([]byte, error) {
			read, err := kv.GetFromLeader(ctx, chunk)
			if errors.Is(err, nats.ErrKeyNotFound) {
				return nil, fmt.Errorf("%w: the bucket holds no %s", errMissing, chunk)
			}
			if err != nil {
				return nil, err
			}
			return read.Value(), nil
		})
		if !errors.Is(err, errMissing) {
			return record, layout, revision, err
		}
		latest, readErr := kv.GetFromLeader(ctx, key)
		switch {
		case errors.Is(readErr, nats.ErrKeyNotFound):
			return nil, Layout{}, 0, readErr
		case readErr != nil:
			return nil, Layout{}, 0, fmt.Errorf("read %s again after a chunk was not there: %w", key, readErr)
		case latest.Revision() == revision:
			return nil, layout, revision, fmt.Errorf("%w: %w", ErrDamaged, err)
		case attempt == readAttempts:
			return nil, Layout{}, 0, fmt.Errorf("read %s: %w (%d reads)", key, ErrMoved, attempt)
		}
		entry = latest
	}
}

// Written is what a Write stored: the revision its head was written at, and the layout the record
// is stored in. Stored is false for a head the bucket refused (nats.ErrKeyExists), which stores
// nothing.
type Written struct {
	Revision uint64
	Layout   Layout
	Stored   bool
}

// Write writes record under key through kv, laid out for the max payload the connection reads now:
// its chunks first, each created, then its head - the record itself, or the manifest naming the
// chunks - through head, the write the store makes of a whole record (a compare-and-swap, or a
// put). replaced is the layout of the record head replaces. A write that stops at any step leaves
// the record it would have replaced as it was.
//
// What the write leaves unnamed it deletes once the bucket has answered for it (Delete): the
// chunks of replaced once head landed, and its own chunks once the bucket refused head
// (nats.ErrKeyExists, Written.Stored false and no error), once a chunk write after them answered an
// error, and once the write passed WriteBound before sending head. A call that did not answer
// deletes nothing, since what it sent can still land, and leaves its chunks to the store's
// collection, as a crash does. head's other errors are returned as they are. Each call waits no
// longer than ctx (bus.Bounded), and now is the store's clock, which WriteBound is read on. what
// names the record in the line a chunk left behind logs (Delete).
func Write(ctx context.Context, kv bus.KeyValue, key string, record []byte, replaced Layout, head func(value []byte) (uint64, error),
	now func() time.Time, log *slog.Logger, what string) (Written, error) {
	if IsChunkKey(key) {
		return Written{}, ReservedKey(key)
	}
	value, layout, chunks, err := layOut(record, kv.MaxPayload())
	if err != nil {
		return Written{}, fmt.Errorf("write the %s %s of %d bytes: %w", what, key, len(record), err)
	}
	started := now()
	keys := layout.Keys()
	for index, chunk := range chunks {
		_, answered, err := bus.Bounded(ctx, func() (uint64, error) { return kv.Create(keys[index], chunk) })
		if !answered || err != nil {
			if answered {
				Delete(ctx, kv, keys[:index], log, what, key)
			}
			return Written{}, fmt.Errorf("write chunk %d of %d of the %s %s: %w", index+1, len(chunks), what, key, err)
		}
	}
	if took := now().Sub(started); took > WriteBound {
		Delete(ctx, kv, keys, log, what, key)
		return Written{}, fmt.Errorf("write the %s %s: its %d chunks took %s, past the %s a write may take before its manifest",
			what, key, len(chunks), took, WriteBound)
	}
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return head(value) })
	if !answered {
		return Written{}, fmt.Errorf("write the %s %s: %w", what, key, err)
	}
	if errors.Is(err, nats.ErrKeyExists) {
		Delete(ctx, kv, keys, log, what, key)
		return Written{}, nil
	}
	if err != nil {
		return Written{}, err
	}
	Delete(ctx, kv, replaced.Keys(), log, what, key)
	return Written{Revision: revision, Layout: layout, Stored: true}, nil
}

// Delete deletes chunks, chunks of the record under key that no record names any more, each within
// ctx. A chunk it could not delete is logged at WARN, naming what and key, and left to the store's
// collection: the write or delete that unnamed it has already landed, and a chunk no record names
// is read by nothing.
func Delete(ctx context.Context, kv bus.KeyValue, chunks []string, log *slog.Logger, what, key string) {
	for _, chunk := range chunks {
		_, answered, err := bus.Bounded(ctx, func() (struct{}, error) { return struct{}{}, kv.Delete(chunk) })
		if !answered || err != nil {
			log.Warn("chunks of a record are left to the store's collection",
				slog.String("record", what), slog.String("key", key), slog.String("chunk", chunk), slog.String("error", fmt.Sprint(err)))
			return
		}
	}
}

// Collect deletes chunks, the keys a store's collection found no record names (Chunks.Unnamed),
// each within ctx, and reports how many it deleted. A delete that failed ends it with that error.
func Collect(ctx context.Context, kv bus.KeyValue, chunks []string) (int, error) {
	for collected, chunk := range chunks {
		if _, answered, err := bus.Bounded(ctx, func() (struct{}, error) { return struct{}{}, kv.Delete(chunk) }); !answered || err != nil {
			return collected, fmt.Errorf("collect chunk %s: %w", chunk, err)
		}
	}
	return len(chunks), nil
}

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

// Delivered takes one record of a chunk's key the read delivered (kvwatch.Copy.Delivered).
func (c *Chunks) Delivered(entry kvwatch.Entry) error { return c.copy.Delivered(entry) }

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
// any other.
func (c *Chunks) Unnamed(named func(generation string) bool, cutoff time.Time) []string {
	var unnamed []string
	for key, held := range c.copy.All() {
		if !named(generation(key)) && held.taken.Before(cutoff) {
			unnamed = append(unnamed, key)
		}
	}
	slices.Sort(unnamed)
	return unnamed
}
