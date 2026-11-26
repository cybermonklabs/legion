package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
)

// streamMessageReads is a legacy bucket handle whose reads are stream message gets
// ($JS.API.STREAM.MSG.GET.KV_<bucket>, nats.go's GetLastMsg and GetMsg without nats.DirectGet),
// for a client that asked for them (WithStreamMessageReads). Every other call goes to the handle
// it wraps unchanged.
//
// nats.go reads a key of an AllowDirect bucket, which is every bucket this module creates, with a
// direct get, and so do two of its writes inside the handle: Create, on a write a key already
// holds, reads the key to tell a delete marker from a live value. The stream answers a direct get
// itself, through the account's own JetStream client, on whatever reply subject the request names,
// with the stored value as the body. A reply subject under $JS.API. is therefore taken by the
// JetStream API as a request of the account's (nats-server 2.15.1, measured by natsgrant's
// TestTheEdgeHoldsNoDirectGetWhoseAnswerReachesTheJetStreamAPI): a value written as a stream update changes
// that stream, an empty one aimed at a delete deletes it. A user whose grant holds a direct get
// and a write of some bucket can thus make any JetStream API call of its account. A stream message
// get is answered by the JetStream API, and its answer, aimed the same way, reaches no API call in
// that measurement. So an edge listener, whose grant holds no direct get, reads this way, and its
// Create is this one.
//
// The reads answer what nats.go's own reads answer: a key the bucket does not hold, and one whose
// newest message is a delete or purge marker, is nats.ErrKeyNotFound; a revision whose message is
// another key's is nats.ErrKeyNotFound too. What the stream's leader answers is always current, so
// none of the copy lag a direct get carries (GetFromLeader) applies.
type streamMessageReads struct {
	nats.KeyValue
	// js is a legacy JetStream context on the handle's connection. Every get passes
	// nats.Context(ctx) with its own deadline, so js's MaxWait is not reached.
	js nats.JetStreamContext
	// owner is the client that owns the handle's connection, whose record of the server's refusals
	// answers a get the grant refuses (refusedBy, through leaderGet).
	owner *Client
}

// newStreamMessageReads wraps kv, a legacy handle on conn, to read with stream message gets.
func newStreamMessageReads(conn *nats.Conn, kv nats.KeyValue) (nats.KeyValue, error) {
	js, err := conn.JetStream()
	if err != nil {
		return nil, fmt.Errorf("the stream message reads of KV bucket %s: %w", kv.Bucket(), err)
	}
	return streamMessageReads{KeyValue: kv, js: js, owner: clientOwning(conn)}, nil
}

// Get reads key's newest value with a stream message get.
func (kv streamMessageReads) Get(key string) (nats.KeyValueEntry, error) {
	entry, _, err := kv.read(key, 0)
	return entry, err
}

// GetRevision reads key at revision with a stream message get.
func (kv streamMessageReads) GetRevision(key string, revision uint64) (nats.KeyValueEntry, error) {
	entry, _, err := kv.read(key, revision)
	return entry, err
}

// readsDirectly is false (statedReads): Get and GetRevision send a get from the stream's leader
// whatever the stream's AllowDirect, where the nats.go handle this wraps, built from that
// AllowDirect, would send a direct get. So a read whose own request the grant refuses names that
// refusal; refusedBy takes it on the send itself.
func (streamMessageReads) readsDirectly() bool { return false }

// Create writes value to key when the bucket holds no live value of it, as nats.go's Create does:
// a write at revision 0, and when that is refused, a write over the key's delete or purge marker
// at the marker's revision. A key whose newest message is a live value is refused with
// nats.ErrKeyExists beside the stream's "wrong last sequence" answer.
func (kv streamMessageReads) Create(key string, value []byte) (uint64, error) {
	revision, err := kv.Update(key, value, 0)
	if err == nil {
		return revision, nil
	}
	if _, marker, readErr := kv.read(key, 0); readErr == nil || errors.Is(readErr, nats.ErrKeyNotFound) {
		if marker > 0 {
			return kv.Update(key, value, marker)
		}
	}
	var apiErr *nats.APIError
	if errors.As(err, &apiErr) && (apiErr.ErrorCode == nats.JSErrCodeStreamWrongLastSequence || apiErr.ErrorCode == nats.JSErrCodeStreamWrongLastSequenceConstant) {
		return 0, fmt.Errorf("%w: %w", err, nats.ErrKeyExists)
	}
	return 0, err
}

// read is the stream message get of key's newest message (revision 0) or of the message at
// revision. It answers the entry of a live value; and for a delete or purge marker,
// nats.ErrKeyNotFound with the marker's revision, which Create writes over. An unanswered get is
// sent again (leaderGet): it is a read and cannot change the bucket. A get the grant refuses draws
// no answer either, since the server reports the refusal on the connection, so each send ends as
// that refusal arrives (refusedBy) and is sent once.
func (kv streamMessageReads) read(key string, revision uint64) (nats.KeyValueEntry, uint64, error) {
	if !natsKeyValid(key) || strings.Contains(key, "..") {
		return nil, 0, nats.ErrInvalidKey
	}
	bucket := kv.Bucket()
	stream, subject := "KV_"+bucket, "$KV."+bucket+"."+key
	ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
	defer cancel()
	message, err := leaderGet(ctx, kv.owner, stream, func(sendCtx context.Context) (*nats.RawStreamMsg, error) {
		if revision == 0 {
			return kv.js.GetLastMsg(stream, subject, nats.Context(sendCtx))
		}
		return kv.js.GetMsg(stream, revision, nats.Context(sendCtx))
	})
	if err == nil && revision != 0 && message.Subject != subject {
		return nil, 0, nats.ErrKeyNotFound
	}
	if errors.Is(err, nats.ErrMsgNotFound) {
		return nil, 0, nats.ErrKeyNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	switch message.Header.Get(kvOperationHeader) {
	case kvDeleteMarker, kvPurgeMarker:
		return nil, message.Sequence, nats.ErrKeyNotFound
	}
	return leaderEntry{bucket: bucket, key: key, value: message.Data, revision: message.Sequence, created: message.Time}, 0, nil
}

// leaderGet sends a STREAM.MSG.GET read of stream and sends it again only when a send draws no
// answer (resentOnTimeout, servingReadBound per send): a read the server answers within that bound
// is sent once and logs nothing, where a start's readAgain would send duplicates. A get the grant
// refuses ends at once (refusedBy on owner). The resend WARNs go where owner writes (Client.log),
// or where slog.Default() points when no client owns the connection. Both the leader read
// (kv_leader_read.go) and the edge stream-message read use this.
func leaderGet(ctx context.Context, owner *Client, stream string, get func(context.Context) (*nats.RawStreamMsg, error)) (*nats.RawStreamMsg, error) {
	subjects := []string{jsAPI("STREAM.MSG.GET", stream)}
	return resentOnTimeout(ctx, owner.log(), stream+" STREAM.MSG.GET", servingReadBound, func(sendCtx context.Context) (*nats.RawStreamMsg, error) {
		return refusedBy(owner, subjects, func() (*nats.RawStreamMsg, error) { return get(sendCtx) })
	})
}
