// The leader-answered reads of one key of a KV bucket, beside their test, each read so because a
// copy behind the leader, which a direct get may reach, would answer it wrongly, and because only
// the leader's answer carries the headers the stream stored a message with:
// KeyValue.GetFromLeader, whose callers decide a delete or a release on the strength of a key
// being absent (session.SessionRegistry.RefreshFromLeader, for the delete of a session's consumer
// and the interest and role claim reapers, and internal/session's readForStop, for a conditional
// stop), whether a claim takes a role from its holder or is written for its claimant
// (RefreshFromLeader again: a soft claim's reads of the holder, and the claim route's read of its
// claimant), which client instance a delete names (RefreshFromLeader again: the registration view
// of GET /v1/registry/<id>, whose caller names the id it reads in the delete that follows), the
// revision a write or a delete is fenced on (KeyValue.ReadStored, and store's latestStored),
// whether a CI record's change is declined and the revision its retry writes at,
// and whether a settlement's claim still holds before it is published (internal/cistore's
// Store.casState and Store.ClaimStillHeld, each passing context.Background, so RequestTimeout
// alone bounds it), and which session holds a Legion controller's notice lane, which decides a
// session's consumer filter (store's roleClaimFromLeader, under Registry.ReadLaneHolders);
// KeyValue.NewestFromLeader and KeyValue.ReadFromLeader, whose callers decide a delete or the
// revision a write is fenced on, and who holds a role, for the session relay's judgement of a role
// message and the kept lane's handover (store.Registry.RoleClaimFromLeader); and
// KeyValue.GetLatestFromLeader, whose one caller, internal/session's readLatest, reads it for the
// revision a conditional write that lost to another writes at next (session.SessionRegistry's
// registration writes), for the record each pass of a rewrite of a session's entry decides from
// and the revision it writes at, its first pass included, before any write was lost
// (SessionRegistry.PutClearingGap, SetRepositoryGap, ClearRepositoryGap and RenewWithin), and for
// SessionRegistry.StoredByWriter, whose callers decide whether the edge listener shows a session,
// sends to it or names it as a role's holder (cmd/listener's project_scope.go). The read of a key's
// newest message they all make (newestMessage) is GetLatest's marker read too.
//
// Three deletes are decided from a copy - a store's cache, fed by its watcher of the bucket, or
// the list of chunk markers that watcher delivered - and not from the leader, and no
// compare-and-swap refuses them where that copy is behind:
//   - The collection of chunks no record names (internal/kvchunk, store's collectInterestChunks
//     and session's collectChunks). No read of one key proves that no manifest of the bucket
//     names a chunk, so the cache decides, and only for a chunk its copy has held for
//     kvchunk.Grace (10 minutes) while no record named it: that is safe only while the copy is
//     less than the grace behind the bucket. The collectors judge nothing while
//     kvwatch.Watcher.Following is false, which proves that a full read of the bucket completed
//     since the copy last stopped following, not how far behind the live watch is now.
//   - A known-session write's delete of the chunks of the record it replaces (session's
//     KnownSessions.writeWithin, through kvchunk.Write), whose layout is this listener's copy of
//     that record. The record itself is last-writer-wins, a plain Put that no compare-and-swap
//     fences (main's #4325). Every generation is unique to one write, which names its chunks by a
//     crypto/rand generation of its own. This listener's copy saw the head whose chunks it deletes
//     before this write's own head landed, so that head comes earlier in the bucket than this
//     write's head, and no later write names that generation again; the delete is made only once
//     this write's head has landed, so no head of this record's key names the generation it
//     deletes. The chunks of a head the copy had not taken, which landed between the two, are left
//     for the collection, and so is a generation another record of the copy names (session's
//     ownLayout). The delete is safe so far as the copy holds every head that names the
//     generation: a manifest copied under another key, which any writer of the bucket can store
//     and the server stores as its answer to a direct get that names that key's subject as its
//     reply, still names the generation while this listener's copy has not taken it, and its
//     chunks are deleted under it, so that key then reads as a damaged record. No record a write
//     of this build stored loses a chunk so. Session's
//     TestAKnownSessionWriteFromACopyBehindTheBucketDeletesOnlyChunksNoHeadNames pins it.
//   - The known-session store's purge of the markers its chunk keys' deletes left (session's
//     purgeChunkMarkers, kvchunk.PurgeMarkers), each one its watcher was delivered
//     kvchunk.MarkerGrace (5 minutes) ago or more. A chunk key is never written again, since each
//     write names a new generation, so the purge of a deleted chunk's subject takes no chunk a
//     record names; and the grace is long enough that every running listener's watcher has been
//     delivered the delete and dropped the chunk from its copy, so the purge takes nothing a copy
//     still holds. A watcher that missed the delete drops the chunk at its next read of the bucket.
//
// A reaper's delete of a record its copy judged gone (session's SessionRegistry.deleteGone, store's
// reapInterest) and an interest write's delete of the chunks of the row it replaced (store's
// putInterestAt) are judged on a copy too, but the bucket decides each: the delete, or the write,
// is a compare-and-swap at the revision the judgement read, which the bucket refuses once another
// write has landed since, and the chunks it leaves unnamed are deleted only once it has landed.
// The interest marker collector's purge is decided from the stream itself, its own scan of the
// bucket and two reads of the stream's state, not from a store's cache (store's
// interestMarkerCollector).

package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// kvOperationHeader is the header nats.go marks a KV delete or a KV purge marker with, and
// kvDeleteMarker and kvPurgeMarker are the two values it writes (nats.go@v1.55.0 kv.go:304-306).
// A read that takes a key's newest message from the bucket's stream itself (NewestFromLeader, and
// the marker read of KeyValue.GetLatest) reads them for itself, where nats.go's Get reads them on
// its caller's behalf and reports either marker as a key that is not there.
const (
	kvOperationHeader = "KV-Operation"
	kvDeleteMarker    = "DEL"
	kvPurgeMarker     = "PURGE"
)

// kvOperation is the KV operation a message of a bucket's stream records, read from its
// KV-Operation header: a delete or a purge marker, and a put for every other message.
func kvOperation(header nats.Header) nats.KeyValueOp {
	switch header.Get(kvOperationHeader) {
	case kvDeleteMarker:
		return nats.KeyValueDelete
	case kvPurgeMarker:
		return nats.KeyValuePurge
	}
	return nats.KeyValuePut
}

// newestMessage is key's newest message in the bucket's stream, the one GetLastMsg sent with opts
// answers for the key's subject `$KV.<bucket>.<key>`, and whether it is a delete or purge marker:
// the read NewestFromLeader and GetLatest's marker read (newestMarker) both make. It goes out on
// the legacy JetStream context of the connection this handle reads the bucket's stream through;
// building one sends nothing, so each read builds its own rather than keep one on the handle. A
// subject that holds no message answers nats.ErrMsgNotFound, and every error is nats.go's own,
// which each caller names.
func (kv KeyValue) newestMessage(key string, opts ...nats.JSOpt) (*nats.RawStreamMsg, bool, error) {
	js, err := kv.streams.Conn().JetStream()
	if err != nil {
		return nil, false, err
	}
	bucket := kv.Bucket()
	message, err := js.GetLastMsg("KV_"+bucket, "$KV."+bucket+"."+key, opts...)
	if err != nil {
		return nil, false, err
	}
	return message, kvOperation(message.Header) != nats.KeyValuePut, nil
}

// leaderEntry is a key's newest message, read from the bucket's own stream (NewestFromLeader), as
// the nats.KeyValueEntry Get answers for that key: the bucket, the key, the value, the stream
// sequence that is the key's revision, the time the stream stored it, and whether it is a put, a
// delete marker or a purge marker. Its Delta is 0, which is what nats.go's own entry answers for a
// read of one key (kv.go:676-682, whose kve carries no delta either). header is the headers the
// stream stores with the message, read by isServerWritten.
type leaderEntry struct {
	bucket   string
	key      string
	value    []byte
	revision uint64
	created  time.Time
	op       nats.KeyValueOp
	header   nats.Header
}

func (e leaderEntry) Bucket() string             { return e.bucket }
func (e leaderEntry) Key() string                { return e.key }
func (e leaderEntry) Value() []byte              { return e.value }
func (e leaderEntry) Revision() uint64           { return e.revision }
func (e leaderEntry) Created() time.Time         { return e.created }
func (e leaderEntry) Delta() uint64              { return 0 }
func (e leaderEntry) Operation() nats.KeyValueOp { return e.op }

// serverWrittenHeaders are the headers the server itself puts on a message it stores or hands
// out: its stream and subject, its sequence and its time, which a direct get and a RePublish add
// to what they send, and its source, which a stream that sources another adds (nats.go@v1.53.1
// js.go:524-530, and Nats-Stream-Source). No writer of a KV bucket sends one: nats.go's Put,
// Create, Update, Delete and Purge send the message with none of them.
var serverWrittenHeaders = [...]string{nats.JSStream, nats.JSSubject, nats.JSSequence, nats.JSTimeStamp, "Nats-Stream-Source"}

// isServerWritten reports whether entry, a read of NewestFromLeader or GetFromLeader, is a message
// the server wrote on its own and no writer's record: its stored headers carry one of
// serverWrittenHeaders, or a status line (which nats.go decodes as the Status header). The server
// stores such a message as its answer to a request naming the key's subject as its reply subject
// (a direct get's copy of another key's record, a JetStream answer, a status record), and a
// RePublish of another stream that names the key's subject stores one too, whatever body that
// stream was given. nats.go reads a KV entry's revision from the first Nats-Sequence header of a
// direct get's answer, so such a message carrying one is reported at another stream's sequence, and
// a write fenced on that is refused at every attempt; the leader's read reports the sequence it is
// stored at. An entry of any other read is no leader read, and answers false. ReadFromLeader and
// GetLatestFromLeader classify a leader read with it; LeaderEntry.WrittenByServer answers the
// classified result.
func isServerWritten(entry nats.KeyValueEntry) bool {
	read, ok := entry.(leaderEntry)
	if !ok {
		return false
	}
	if read.header.Get("Status") != "" {
		return true
	}
	for _, name := range serverWrittenHeaders {
		if read.header.Get(name) != "" {
			return true
		}
	}
	return false
}

// LeaderKind classifies a key's newest message read from the bucket's stream leader.
type LeaderKind uint8

const (
	// LeaderAbsent is a key the stream holds no message for.
	LeaderAbsent LeaderKind = iota
	// LeaderMarker is a delete or purge marker.
	LeaderMarker
	// LeaderServerWritten is a put the server wrote on its own (isServerWritten), no writer's record.
	LeaderServerWritten
	// LeaderPut is a writer's put.
	LeaderPut
)

// LeaderEntry is a key's newest message read from the bucket's stream leader (ReadFromLeader),
// classified: its kind, the sequence it is stored at (Revision, 0 for an absent key, the true
// stored sequence otherwise and never a copied one), and the value of a writer's put (nil for every
// other kind). It is the one reading every caller that decides a delete, or the revision a write is
// fenced on, shares, so a new kind of server-written message is classified in one place.
type LeaderEntry struct {
	Kind     LeaderKind
	Revision uint64
	Value    []byte
}

// WrittenByServer reports whether the leader's newest message for the key is a put the server wrote
// on its own and no writer's record.
func (e LeaderEntry) WrittenByServer() bool { return e.Kind == LeaderServerWritten }

// ReadFromLeader reads key's newest message from the bucket's stream leader (NewestFromLeader) and
// classifies it (LeaderEntry): a key the stream holds no message for is LeaderAbsent at revision 0,
// a delete or purge marker is LeaderMarker, a put the server wrote is LeaderServerWritten, and a
// writer's put is LeaderPut with its value, each marker and put at the sequence it is stored at. A
// read the leader refused or did not answer is the error, not a kind: no caller may decide a delete
// or a write over a key it could not read.
func (kv KeyValue) ReadFromLeader(ctx context.Context, key string) (LeaderEntry, error) {
	newest, err := kv.NewestFromLeader(ctx, key)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
		return LeaderEntry{Kind: LeaderAbsent}, nil
	case err != nil:
		return LeaderEntry{}, err
	case newest.Operation() != nats.KeyValuePut:
		return LeaderEntry{Kind: LeaderMarker, Revision: newest.Revision()}, nil
	case isServerWritten(newest):
		return LeaderEntry{Kind: LeaderServerWritten, Revision: newest.Revision()}, nil
	default:
		return LeaderEntry{Kind: LeaderPut, Revision: newest.Revision(), Value: newest.Value()}, nil
	}
}

// GetFromLeader reads key as Get reads it, with one difference: the leader of the bucket's
// stream answers it. It is the stream message get of that stream,
// `$JS.API.STREAM.MSG.GET.KV_<bucket>`, asking for the newest message on the key's subject
// `$KV.<bucket>.<key>`, where Get is a direct get. nats.go sends exactly that request for
// GetLastMsg unless the call is given nats.DirectGet (nats.go@v1.55.0 jsm.go:1317-1334), so this
// is that call rather than a request of this package's own; TestTheLeaderReadSendsTheStreamMessageGetWhereGetSendsTheDirectGet
// reads the wire and is what holds nats.go to it.
//
// What that buys is the one hazard a direct get carries. Every bucket this package opens is
// created AllowDirect, so nats.go answers each of its reads with a direct get, and nats-server
// hands such a read to any copy of the stream that is 90% caught up rather than to the leader
// (kv.go records the same hazard against the same bucket family, from
// server/jetstream_cluster.go:2622-2641, v2.10.29). A copy that has not applied a Put made a
// moment ago answers that the key is not there. A caller that only enriches what it holds can
// read again; a caller that takes something away on the strength of a key being absent cannot,
// because what it takes away is gone. The listener's delete of a session's JetStream consumer is
// that caller: it re-reads the session bucket under the session's lock and deletes the consumer
// only where the session has no record at all, so a stale miss deletes the consumer of a
// relaunch that is live, and that session's relay stops until it registers again. A stream
// message get is answered by the stream's leader alone (nats-server v2.15.1
// jetstream_api.go:3956-3958), which holds every write the bucket has taken, so a record the
// bucket holds is never absent from its answer.
//
// It answers in Get's vocabulary wherever it can. A key the bucket does not hold is
// nats.ErrKeyNotFound, and so is one whose newest message is a delete or a purge marker, which
// is how Get reports both (NewestFromLeader answers the marker itself). A key Get refuses before
// it sends anything is refused here before anything is sent and with the same error
// (RefusesRead): the two read the same keys, so a caller that moves from one to the other is
// refused the same ones. Any other answer from the stream is the error nats.go reports, named
// with the key and the bucket.
//
// ctx bounds the whole read, the one request it sends included. A ctx carrying no deadline is
// bounded by RequestTimeout, the bound every other read of a bucket's stream through this handle
// takes (ReadStreamState), so no caller waits on this read for good.
func (kv KeyValue) GetFromLeader(ctx context.Context, key string) (nats.KeyValueEntry, error) {
	entry, err := kv.NewestFromLeader(ctx, key)
	if err != nil {
		return nil, err
	}
	if entry.Operation() != nats.KeyValuePut {
		return nil, nats.ErrKeyNotFound
	}
	return entry, nil
}

// GetLatestFromLeader reads key as GetLatest reads it, from the leader of the bucket's stream, as
// NewestFromLeader reads it: the one stream message get of the key's newest message, which answers
// a delete or purge marker's revision too, so no second read finds the marker. A conditional write
// that lost to another write reads the key so before it writes again (internal/session's
// registration writes), since a copy that keeps answering a write behind would hand it the revision
// it just lost on, again and again. A put the server wrote (isServerWritten) is answered as no
// entry, nats.ErrKeyNotFound with the revision it is stored at, as a marker is: the headers that
// tell it from a writer's record reach this read alone, never GetLatest's direct get, so a write
// fenced on that revision replaces it there and never carries its body forward as a writer's.
// Everything else is GetFromLeader's: its refusals, its errors and its bound on ctx.
func (kv KeyValue) GetLatestFromLeader(ctx context.Context, key string) (LatestEntry, error) {
	entry, err := kv.NewestFromLeader(ctx, key)
	switch {
	case err != nil:
		return LatestEntry{}, err
	case entry.Operation() != nats.KeyValuePut || isServerWritten(entry):
		return LatestEntry{Revision: entry.Revision()}, nats.ErrKeyNotFound
	}
	return LatestEntry{Entry: entry, Revision: entry.Revision()}, nil
}

// LeaderReads is a bucket handle that answers a read from the stream's leader itself, in place of
// the stream: the fault seam for a test whose wrapper of a handle (NewKeyValue, KeyValue.Legacy)
// plants a fault in every read of the bucket, the leader's included. NewestFromLeader, and so
// GetFromLeader and ReadFromLeader, asks a handle that implements it after its own refusals and
// sends nothing to the stream. No nats.go handle implements it, so every production read goes to
// the stream's leader, and a wrapper that does not implement it leaves the leader's reads to the
// stream, as a copy that is behind leaves them.
type LeaderReads interface {
	NewestFromLeader(ctx context.Context, key string) (nats.KeyValueEntry, error)
}

// NewestFromLeader is GetFromLeader with the delete and purge markers answered rather than
// reported as a key that is not there: key's newest message on the bucket's stream, read from the
// stream's leader, whose Operation is nats.KeyValueDelete or nats.KeyValuePurge for a marker and
// whose Revision is the sequence the message is stored at. A key the stream holds no message for
// is nats.ErrKeyNotFound; refusals, errors and ctx are GetFromLeader's.
//
// It is for a write that names the revision it replaces, made where a direct get answered that
// the key holds no value: the marker the key may hold is what that write replaces, and nats.go's
// Create replaces it at the revision a direct get reports for it, which is not the marker's own
// when the server stored the marker as its answer to a direct get of another key's marker. Such a
// copy carries its source's Nats-Sequence header ahead of its own, and a direct get reports the
// first; a write fenced on it is refused at every attempt. A stream message get reports the
// sequence the message is stored at (nats.RawStreamMsg.Sequence).
func (kv KeyValue) NewestFromLeader(ctx context.Context, key string) (nats.KeyValueEntry, error) {
	if err := kv.RefusesRead(key); err != nil {
		return nil, err
	}
	if reads, ok := kv.legacy.(LeaderReads); ok {
		return reads.NewestFromLeader(ctx, key)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
	}
	bucket := kv.Bucket()
	// A read the grant refuses draws no answer; the record names the refusal after the read's own
	// wait (refusedAfter), which costs a read the leader answers nothing.
	message, err := refusedAfter(kv.owner, func() []string { return []string{jsAPI("STREAM.MSG.GET", "KV_"+bucket)} }, func() (*nats.RawStreamMsg, error) {
		message, _, err := kv.newestMessage(key, nats.Context(ctx))
		return message, err
	})
	if errors.Is(err, nats.ErrMsgNotFound) {
		return nil, nats.ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read key %s of KV bucket %s from its stream's leader: %w", key, bucket, err)
	}
	return leaderEntry{
		bucket:   bucket,
		key:      key,
		value:    message.Data,
		revision: message.Sequence,
		created:  message.Time,
		op:       kvOperation(message.Header),
		header:   message.Header,
	}, nil
}
