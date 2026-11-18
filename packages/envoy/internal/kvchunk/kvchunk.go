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
// A manifest is itself a record an earlier build reads: a JSON object whose first member,
// `"chunked"`, holds the layout, and whose other members are the store's view of the record for a
// reader that does not join chunks (Write's view), which fits in one message. Every earlier build
// decodes a record with encoding/json and ignores a member it does not know, so while builds share
// a bucket, an earlier listener serves the session from that view rather than losing it.
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
)

// Prefix begins the key of every chunk, and no record's key: a write of a record under a key with
// this prefix is refused (ReservedKey). A chunk's key is the prefix, the generation of the write
// that stored it and the chunk's index, `=chunk.<generation>.<index>`. A store's collection
// deletes every key under it that no record names, so a later layout that this build cannot read
// names its chunks under another prefix.
const Prefix = "=chunk."

// manifestPrefix begins a manifest: the opening of a JSON object and its first member's name, which
// holds the layout. A record stored whole is its own JSON object, whose first member is never
// "chunked" (each store's record type opens with session_id), so a manifest is told apart by this
// prefix alone, and a build that does not know the member decodes the rest of the object - the
// store's view of the record - as a record.
var manifestPrefix = []byte(`{"chunked":`)

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

// Keys is the key of each of the layout's chunks, none for a record stored whole. It is called
// only for a layout this build laid out (Write) or one whose chunks a read joined (Read,
// Chunks.Join), whose count is the chunks that exist: a manifest's own claim of its count is never
// trusted to size anything (join).
func (l Layout) Keys() []string {
	keys := make([]string, l.Chunks)
	for i := range keys {
		keys[i] = l.key(i)
	}
	return keys
}

func (l Layout) key(index int) string { return Prefix + l.Generation + "." + strconv.Itoa(index) }

// IsChunkKey reports whether key is a chunk's, which no record's key is.
func IsChunkKey(key string) bool { return strings.HasPrefix(key, Prefix) }

// wellFormed reports whether key has the shape of a chunk key this build writes: Prefix, a
// generation of crypto/rand.Text's length and alphabet (validGeneration), `.`, and a decimal index
// with no sign and no leading zero. IsChunkKey reserves the whole prefix for chunks, but a key
// under it reaches a store from whatever a writer of the bucket stored there, so only a well-formed
// one is turned into a purge filter (PurgeMarkers), tracked for its purge (Markers) or collected
// (Chunks.Unnamed): a key holding a wildcard would widen a purge to every chunk of the bucket.
func wellFormed(key string) bool {
	rest, ok := strings.CutPrefix(key, Prefix)
	if !ok {
		return false
	}
	generation, index, ok := strings.Cut(rest, ".")
	if !ok || !validGeneration(generation) || index == "" || (len(index) > 1 && index[0] == '0') {
		return false
	}
	for _, c := range []byte(index) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

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
// headers is that value itself, on a server of any max payload. A larger one is chunks under a new
// generation, and a manifest naming them; a chunk is three quarters of that room less HeaderBytes,
// so the answer of the leader's read of it (Read), which carries the chunk in base64 inside a JSON
// object, fits under the max payload too, and a server that holds an answer to it - a leafnode
// connection does - takes it. A max payload that leaves a chunk no room refuses the larger record.
//
// The manifest is the layout's member followed by view's members: view is called with the bytes
// the manifest leaves it, and must answer a JSON object no longer than that, or an error (a
// bus.ErrTooLarge when no view of the record fits), which refuses the write before anything is
// sent. A nil view is the empty object.
func layOut(record []byte, maxPayload int, view func(room int) ([]byte, error)) ([]byte, Layout, [][]byte, error) {
	whole := maxPayload - HeaderBytes
	if len(record) <= whole {
		return record, Layout{}, nil, nil
	}
	limit := whole*3/4 - HeaderBytes
	if limit <= 0 {
		return nil, Layout{}, nil, fmt.Errorf("%w: the server's max payload of %d bytes leaves no room for a chunk beside %d bytes of headers",
			bus.ErrTooLarge, maxPayload, HeaderBytes)
	}
	sum := sha256.Sum256(record)
	layout := Layout{Generation: rand.Text(), Bytes: len(record), SHA256: hex.EncodeToString(sum[:])}
	chunks := slices.Collect(slices.Chunk(record, limit))
	layout.Chunks = len(chunks)
	member, err := json.Marshal(layout)
	if err != nil {
		return nil, Layout{}, nil, err
	}
	manifest := append(slices.Clone(manifestPrefix), member...)
	object := []byte("{}")
	if view != nil {
		if object, err = view(whole - len(manifest)); err != nil {
			return nil, Layout{}, nil, err
		}
	}
	if len(object) < 2 || object[0] != '{' || object[len(object)-1] != '}' || !json.Valid(object) {
		return nil, Layout{}, nil, fmt.Errorf("the view of a chunked record is no JSON object: %.80q", object)
	}
	if len(object) > 2 {
		manifest = append(append(manifest, ','), object[1:]...)
	} else {
		manifest = append(manifest, '}')
	}
	if len(manifest) > whole {
		return nil, Layout{}, nil, fmt.Errorf("%w: the manifest of a record of %d bytes is %d bytes, past the %d a whole record may take",
			bus.ErrTooLarge, len(record), len(manifest), whole)
	}
	return manifest, layout, chunks, nil
}

// generationLength is the length of a generation, the 26 characters of crypto/rand.Text, from the
// alphabet of RFC 4648 base32 (A to Z and 2 to 7), every one of them a character a KV key and a
// subject token take.
const generationLength = 26

// ReadLayout is the layout a value under a record's key names: the zero layout for a record stored
// whole, and the manifest's for a manifest. A manifest is ErrDamaged unless it is one a write of
// this build can make: a generation of crypto/rand.Text's length and alphabet, a length of at
// least one byte, between one chunk and one chunk per byte, and a SHA-256 in lower-case hex. Any
// writer of the bucket can store a value under a record's key, so nothing a reader does - an
// allocation, a key it builds - is sized or named by a manifest that fails this.
func ReadLayout(value []byte) (Layout, error) {
	if !IsManifest(value) {
		return Layout{}, nil
	}
	var manifest struct {
		Chunked *Layout `json:"chunked"`
	}
	if err := json.Unmarshal(value, &manifest); err != nil {
		return Layout{}, fmt.Errorf("%w: decode its manifest: %w", ErrDamaged, err)
	}
	if manifest.Chunked == nil {
		return Layout{}, fmt.Errorf("%w: its manifest names no layout", ErrDamaged)
	}
	layout := *manifest.Chunked
	if !validGeneration(layout.Generation) || layout.Bytes < 1 || layout.Chunks < 1 || layout.Chunks > layout.Bytes || !lowerHex(layout.SHA256, sha256.Size) {
		return Layout{}, fmt.Errorf("%w: its manifest is no write's: %+v", ErrDamaged, layout)
	}
	return layout, nil
}

func validGeneration(generation string) bool {
	if len(generation) != generationLength {
		return false
	}
	for _, c := range []byte(generation) {
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

func lowerHex(text string, size int) bool {
	if len(text) != 2*size {
		return false
	}
	for _, c := range []byte(text) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// join joins the layout's chunks, each read by chunk, and returns the record when it is the one the
// manifest names: its length and its SHA-256, ErrDamaged otherwise. An error chunk returns,
// errMissing among them, is returned as it is. It allocates by the bytes the chunks hold, never by
// the manifest's claim, and builds each chunk's key as it reads it, so a manifest that claims more
// than its record holds ends at its first missing chunk.
func (l Layout) join(chunk func(key string) ([]byte, error)) ([]byte, error) {
	var record []byte
	for index := range l.Chunks {
		value, err := chunk(l.key(index))
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
// record whose chunks do not join into what its manifest names is ErrDamaged too. A damaged
// record's layout is not returned: the chunks it names were never proved to be its, so the write
// that replaces it deletes none of them, and the store's collection removes the ones the bucket
// holds. Any other error is a read that did not answer. Each read waits no longer than ctx.
//
// The read again of the record's key is a leader read too, so a rewrite that made the record a
// whole value of more than three quarters of the max payload answers it base64-encoded past that
// bound: a client connection and a route take that answer, and a leafnode hop, which enforces the
// max payload on what it forwards, would not. The hubs envoy-infra renders configure no leafnode
// (infra/gcp's clustered hub turns its leafnode port off).
func Read(ctx context.Context, kv bus.KeyValue, key string, entry nats.KeyValueEntry) ([]byte, Layout, uint64, error) {
	for attempt := 1; ; attempt++ {
		revision := entry.Revision()
		layout, err := ReadLayout(entry.Value())
		if err != nil {
			return nil, Layout{}, revision, err
		}
		if !layout.Chunked() {
			return entry.Value(), layout, revision, nil
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
		if errors.Is(err, ErrDamaged) {
			return nil, Layout{}, revision, err
		}
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
			return nil, Layout{}, revision, fmt.Errorf("%w: %w", ErrDamaged, err)
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
// chunks of replaced once head landed, and its own chunks once the bucket refused head - a
// compare-and-swap another write beat (nats.ErrKeyExists, Written.Stored false and no error), any
// other refusal the server answered (a *nats.APIError) or one this client made before sending
// (bus.ErrRefused), none of which stores anything - once a chunk write after them answered an
// error, and once the write passed WriteBound before sending head. A call that did not answer, or
// failed in any other way, deletes nothing, since what it sent can still land, and leaves its
// chunks to the store's collection, as a crash does. head's
// other errors are returned as they are. Each call waits no longer than ctx (bus.Bounded). now is
// the store's clock, and WriteBound is read off its wall time, so a host that slept between the
// chunks and the head counts the sleep. what names the record in the line a chunk left behind
// logs (Delete).
//
// view is the store's view of the record for a reader that does not join chunks - an earlier
// build - which the manifest carries beside its layout (layOut): called only for a record past one
// message, with the bytes the manifest leaves it, it answers a JSON object of the record's type
// that an earlier build reads as the same session, or a bus.ErrTooLarge that refuses the write
// before anything is sent. nil carries no view.
func Write(ctx context.Context, kv bus.KeyValue, key string, record []byte, view func(room int) ([]byte, error), replaced Layout,
	head func(value []byte) (uint64, error), now func() time.Time, log *slog.Logger, what string) (Written, error) {
	if IsChunkKey(key) {
		return Written{}, ReservedKey(key)
	}
	value, layout, chunks, err := layOut(record, kv.MaxPayload(), view)
	if err != nil {
		return Written{}, fmt.Errorf("write the %s %s of %d bytes: %w", what, key, len(record), err)
	}
	started := now().Round(0)
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
	if took := now().Round(0).Sub(started); took > WriteBound {
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
		var refused *nats.APIError
		if errors.As(err, &refused) || errors.Is(err, bus.ErrRefused) {
			Delete(ctx, kv, keys, log, what, key)
		}
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
