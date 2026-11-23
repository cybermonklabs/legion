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

// watchingTwin is a wrapper of another package with a jetstream twin, testnats.RacedKeyValue
// among them: its method is exported, since an unexported one is no method of another package's
// type.
type watchingTwin interface {
	Watching(inner jetstream.KeyValue) jetstream.KeyValue
}

// watchHandleFor is inner wrapped in kv's jetstream twin when kv has one, and inner otherwise.
func watchHandleFor(kv any, inner jetstream.KeyValue) jetstream.KeyValue {
	switch twin := kv.(type) {
	case watchingKV:
		return twin.watching(inner)
	case watchingTwin:
		return twin.Watching(inner)
	}
	return inner
}

func (kv *delayingKV) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &delayingJS{KeyValue: inner, kv: kv}
}

// delayingJS is delayingKV's jetstream twin: a write of a key it was asked to delay passes its
// gate once the write has answered, a Put and the Create and Update an entry write sends alike.
type delayingJS struct {
	jetstream.KeyValue
	kv *delayingKV
}

func (j *delayingJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	return j.kv.wrote(key)(j.KeyValue.Put(ctx, key, value))
}

func (j *delayingJS) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	return j.kv.wrote(key)(j.KeyValue.Create(ctx, key, value, opts...))
}

func (j *delayingJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	return j.kv.wrote(key)(j.KeyValue.Update(ctx, key, value, revision))
}

func (kv *interleavingSessionPutKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &interleavingSessionPutJS{KeyValue: inner, kv: kv}
}

// interleavingSessionPutJS is interleavingSessionPutKeyValue's jetstream twin: the entry writes
// that land through it count with the wrapper's, and the first runs afterFirstPut.
type interleavingSessionPutJS struct {
	jetstream.KeyValue
	kv *interleavingSessionPutKeyValue
}

func (j *interleavingSessionPutJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	return j.kv.wrote(j.KeyValue.Put(ctx, key, value))
}

func (j *interleavingSessionPutJS) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	return j.kv.wrote(j.KeyValue.Create(ctx, key, value, opts...))
}

func (j *interleavingSessionPutJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	return j.kv.wrote(j.KeyValue.Update(ctx, key, value, revision))
}

func (kv losingUpdateKV) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return losingUpdateJS{KeyValue: inner, kv: kv}
}

// losingUpdateJS is losingUpdateKV's jetstream twin: every conditional update loses, counted
// with the wrapper's.
type losingUpdateJS struct {
	jetstream.KeyValue
	kv losingUpdateKV
}

func (j losingUpdateJS) Update(_ context.Context, key string, value []byte, revision uint64) (uint64, error) {
	return j.kv.Update(key, value, revision)
}

func (kv *faultyDeleteKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &faultyDeleteJS{KeyValue: inner, kv: kv}
}

// faultyDeleteJS is faultyDeleteKeyValue's jetstream twin: its deletes count with the wrapper's
// and take the fault armed on it.
type faultyDeleteJS struct {
	jetstream.KeyValue
	kv *faultyDeleteKeyValue
}

func (j *faultyDeleteJS) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if err := j.kv.faulted(); err != nil {
		return err
	}
	return j.KeyValue.Delete(ctx, key, opts...)
}

func (kv *raceOnFirstUpdate) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &raceOnFirstUpdateJS{KeyValue: inner, kv: kv}
}

// raceOnFirstUpdateJS is raceOnFirstUpdate's jetstream twin: its updates count with the
// wrapper's, and before runs ahead of the first.
type raceOnFirstUpdateJS struct {
	jetstream.KeyValue
	kv *raceOnFirstUpdate
}

func (j *raceOnFirstUpdateJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	j.kv.beforeUpdate()
	return j.KeyValue.Update(ctx, key, value, revision)
}
