package cistore

// The store's writes go through bucket handles given their caller's context (bus.KeyValue.Within),
// which send them through the handle's jetstream handle rather than its legacy one. So each legacy
// wrapper below that holds a write fault has a jetstream twin holding the same fault over the
// wrapper's own state, and useKV takes the twin as the handle's jetstream handle.

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

func (k *writeKV) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &writeJS{KeyValue: inner, kv: k}
}

// writeJS is writeKV's jetstream twin: its creates and updates take writeKV's delay, act and record.
type writeJS struct {
	jetstream.KeyValue
	kv *writeKV
}

func (j *writeJS) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	return j.kv.write(key, value, func() (uint64, error) { return j.KeyValue.Create(ctx, key, value, opts...) })
}

func (j *writeJS) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	return j.kv.write(key, value, func() (uint64, error) { return j.KeyValue.Update(ctx, key, value, last) })
}

func (kv *interleavingKV) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &interleavingJS{KeyValue: inner, kv: kv}
}

// interleavingJS is interleavingKV's jetstream twin: an update that lands runs afterUpdate.
type interleavingJS struct {
	jetstream.KeyValue
	kv *interleavingKV
}

func (j *interleavingJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	updated, err := j.KeyValue.Update(ctx, key, value, revision)
	if err == nil && j.kv.afterUpdate != nil {
		j.kv.afterUpdate(key, value, updated)
	}
	return updated, err
}
