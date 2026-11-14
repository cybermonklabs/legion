// The leader-answered read of one key of a KV bucket (KeyValue.GetFromLeader), beside its test:
// the one read of this package that may not be a direct get, because what it decides is a delete.

package bus

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
)

// kvOperationHeader is the header nats.go marks a KV delete or a KV purge marker with, and
// kvDeleteMarker and kvPurgeMarker are the two values it writes (nats.go@v1.53.1 kv.go:304-306).
// A read that takes a key's newest message from the bucket's stream itself (GetFromLeader) reads
// them for itself, where nats.go's Get reads them on its caller's behalf and reports either
// marker as a key that is not there.
const (
	kvOperationHeader = "KV-Operation"
	kvDeleteMarker    = "DEL"
	kvPurgeMarker     = "PURGE"
)

// leaderEntry is a key's newest message, read from the bucket's own stream (GetFromLeader), as
// the nats.KeyValueEntry Get answers for that key: the bucket, the key, the value, the stream
// sequence that is the key's revision, and the time the stream stored it. Its operation is
// always a put, because a key whose newest message is a delete or a purge marker is reported as
// not found rather than returned, and its Delta is 0, which is what nats.go's own entry answers
// for a read of one key (kv.go:676-682, whose kve carries no delta either).
type leaderEntry struct {
	bucket   string
	key      string
	value    []byte
	revision uint64
	created  time.Time
}

func (e leaderEntry) Bucket() string             { return e.bucket }
func (e leaderEntry) Key() string                { return e.key }
func (e leaderEntry) Value() []byte              { return e.value }
func (e leaderEntry) Revision() uint64           { return e.revision }
func (e leaderEntry) Created() time.Time         { return e.created }
func (e leaderEntry) Delta() uint64              { return 0 }
func (e leaderEntry) Operation() nats.KeyValueOp { return nats.KeyValuePut }

// GetFromLeader reads key as Get reads it, with one difference: the leader of the bucket's
// stream answers it. It is the stream message get of that stream,
// `$JS.API.STREAM.MSG.GET.KV_<bucket>`, asking for the newest message on the key's subject
// `$KV.<bucket>.<key>`, where Get is a direct get. nats.go sends exactly that request for
// GetLastMsg unless the call is given nats.DirectGet (nats.go@v1.53.1 jsm.go:1242-1294), so this
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
// is how Get reports both. A key Get refuses before it sends anything is refused here before
// anything is sent and with the same error (RefusesRead): the two read the same keys, so a
// caller that moves from one to the other is refused the same ones. Any other answer from the
// stream is the error nats.go reports, named with the key and the bucket.
//
// ctx bounds the whole read, the one request it sends included. A ctx carrying no deadline is
// bounded by RequestTimeout, the bound every other read of a bucket's stream through this handle
// takes (ReadStreamState), so no caller waits on this read for good.
func (kv KeyValue) GetFromLeader(ctx context.Context, key string) (nats.KeyValueEntry, error) {
	if err := kv.RefusesRead(key); err != nil {
		return nil, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
	}
	bucket := kv.Bucket()
	named := func(err error) error {
		return fmt.Errorf("read key %s of KV bucket %s from its stream's leader: %w", key, bucket, err)
	}
	// The legacy JetStream context of the connection this handle reads the bucket's stream
	// through: building one sends nothing, and this read is the only caller, so it is built here
	// rather than kept on the handle.
	js, err := kv.streams.Conn().JetStream()
	if err != nil {
		return nil, named(err)
	}
	message, err := js.GetLastMsg("KV_"+bucket, "$KV."+bucket+"."+key, nats.Context(ctx))
	if errors.Is(err, nats.ErrMsgNotFound) {
		return nil, nats.ErrKeyNotFound
	}
	if err != nil {
		return nil, named(err)
	}
	switch message.Header.Get(kvOperationHeader) {
	case kvDeleteMarker, kvPurgeMarker:
		return nil, nats.ErrKeyNotFound
	}
	return leaderEntry{
		bucket:   bucket,
		key:      key,
		value:    message.Data,
		revision: message.Sequence,
		created:  message.Time,
	}, nil
}
