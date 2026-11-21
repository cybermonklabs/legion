package bus

import (
	"context"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// BarrierKey is the one key of every bucket that WriteBarrier writes and no other call may name:
// check refuses it for every get, write, delete, purge and watch through a KeyValue
// (ErrReservedKey), StoredKeys lists no key under it, and kvwatch drops its records before a copy
// or a scan sees them. What WriteBarrier writes under it is a kvwatch read's barrier: once the
// read's scan has ended, the read writes a delete marker on this key and waits until its own
// consumer has delivered a record at or past that marker's stream sequence (kvwatch's settling),
// before it removes anything. It cannot be a key any store here writes: a session id the listener
// stores or deletes is a key through this package's check; a role is `^[a-z0-9][a-z0-9_-]*$`
// (roles.PatternString), which no key beginning with '_' matches; a CI record's key is
// `<owner>.<repo>.pr<number>.<sha>` (cistore.Key), four tokens; and an interest or known-session
// record is keyed by a session id. It is a key nats.go accepts in every bucket: letters, '-' and
// '_', no dot (natsKeyValid).
const BarrierKey = "_kvwatch-barrier"

// WriteBarrier writes a delete marker to BarrierKey, as nats.go's KV delete writes one (the
// KV-Operation header DEL, jetstream/kv.go), and returns the stream sequence the acknowledgement
// names. The acknowledgement is the one thing a majority of the stream's group confirms: the
// leader sends it only from its apply of the committed entry (nats-server v2.15.1
// jetstream_cluster.go:5947, :6025, applyStreamMsgOp calling processJetStreamMsg, which answers
// only as leader, stream.go:6560), so a member cut off from the others cannot write one, and a
// store holding the barrier has applied every entry committed before it, in log order (raft.go:4009
// applyCommit, jetstream_cluster.go:5513 applyStreamEntries). A kvwatch read writes one once its
// scan has ended (Watcher.beginSettle) and waits until its own consumer has delivered a record at
// or past the sequence this returns (settling). Only then does it list the keys the stream holds a
// message on, read from the stream's leader each key it has no evidence of, and remove what the
// leader answers it holds no record of (Watcher.verifyUnmarked, Copy.CompleteRead). The pending
// count a delivery carries is not read for that wait: it is the server's cached count, and it is
// wrong both ways.
//
// It publishes through the jetstream context, which a KeyValue's own Delete does not reach, since
// check refuses BarrierKey there. The publish is held to RequestTimeout under ctx, and the server's
// refusal of it under the connection's grant ends it as soon as it arrives, an error wrapping
// ErrPublishDenied (refusedOn), rather than after that wait with a bare "context deadline
// exceeded": a read writes one barrier, so this costs what a watch's create costs, not what every
// key write would.
func (kv KeyValue) WriteBarrier(ctx context.Context) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	msg := nats.NewMsg("$KV." + kv.Bucket() + "." + BarrierKey)
	msg.Header.Set(kvOperationHeader, kvDeleteMarker)
	ack, err := refusedOn(kv, kv.keySubjects(BarrierKey)(), func() (*jetstream.PubAck, error) {
		return kv.streams.PublishMsg(ctx, msg, jetstream.WithExpectStream("KV_"+kv.Bucket()))
	})
	if err != nil {
		return 0, err
	}
	return ack.Sequence, nil
}
