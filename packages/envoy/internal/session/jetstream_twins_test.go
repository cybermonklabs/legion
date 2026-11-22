package session

// A write through a bucket handle given its caller's context (bus.KeyValue.Within) goes through the
// handle's jetstream handle, not its legacy one, so a test that injects a fault into a write
// injects it into both: each legacy wrapper below that holds a write fault has a jetstream twin
// that holds the same fault, sharing the wrapper's state, and useKV installs the twin beside it.

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"
)

// watchingKV is a legacy wrapper whose write faults have a jetstream twin: watching wraps the
// bucket's jetstream handle in it.
type watchingKV interface {
	watching(inner jetstream.KeyValue) jetstream.KeyValue
}

// watchHandleFor is inner wrapped in kv's jetstream twin when kv has one, and inner otherwise.
func watchHandleFor(kv any, inner jetstream.KeyValue) jetstream.KeyValue {
	if twin, ok := kv.(watchingKV); ok {
		return twin.watching(inner)
	}
	return inner
}

func (kv *delayingKV) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &delayingJS{KeyValue: inner, kv: kv}
}

// delayingJS is delayingKV's jetstream twin: a put of a key it was asked to delay passes its gate
// once the put has answered.
type delayingJS struct {
	jetstream.KeyValue
	kv *delayingKV
}

func (j *delayingJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	revision, err := j.KeyValue.Put(ctx, key, value)
	if g := j.kv.take(j.kv.puts, key); g != nil {
		g.pass(func() {})
	}
	return revision, err
}

func (kv *interleavingSessionPutKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &interleavingSessionPutJS{KeyValue: inner, kv: kv}
}

// interleavingSessionPutJS is interleavingSessionPutKeyValue's jetstream twin: its puts count with
// the wrapper's, and the first that lands runs afterFirstPut.
type interleavingSessionPutJS struct {
	jetstream.KeyValue
	kv *interleavingSessionPutKeyValue
}

func (j *interleavingSessionPutJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	j.kv.puts++
	revision, err := j.KeyValue.Put(ctx, key, value)
	if err != nil {
		return 0, err
	}
	if j.kv.puts == 1 {
		j.kv.afterFirstPut()
	}
	return revision, nil
}
