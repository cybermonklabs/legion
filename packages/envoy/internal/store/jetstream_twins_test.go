package store

// A write through a bucket handle given its caller's context (bus.KeyValue.Within) goes through the
// handle's jetstream handle, not its legacy one, and every write of the registries takes that
// route. So a test that injects a fault into a write injects it into both: each legacy wrapper
// below that holds a write fault has a jetstream twin holding the same fault over the wrapper's own
// state, and every handle a test builds over such a wrapper takes the twin as its jetstream handle
// (watchHandleFor).

import (
	"context"
	"errors"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// withTwin is a handle over kv, a test's legacy wrapper, and base's jetstream handle and streams,
// that handle wrapped in kv's jetstream twin when kv has one (watchHandleFor).
func withTwin(kv natsgo.KeyValue, base bus.KeyValue) bus.KeyValue {
	return bus.NewKeyValue(kv, watchHandleFor(kv, base.WatchHandle()), base.Streams())
}

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

func (kv *racingInterestWrite) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &racingInterestWriteJS{KeyValue: inner, kv: kv}
}

type racingInterestWriteJS struct {
	jetstream.KeyValue
	kv *racingInterestWrite
}

func (j *racingInterestWriteJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	j.kv.racing()
	return j.KeyValue.Update(ctx, key, value, revision)
}

func (j *racingInterestWriteJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	j.kv.racing()
	return j.KeyValue.Put(ctx, key, value)
}

func (j *racingInterestWriteJS) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	j.kv.racing()
	return j.KeyValue.Create(ctx, key, value, opts...)
}

func (kv *racingUpdateKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &racingUpdateJS{KeyValue: inner, kv: kv}
}

type racingUpdateJS struct {
	jetstream.KeyValue
	kv *racingUpdateKeyValue
}

func (j *racingUpdateJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	kv := j.kv
	if kv.hooking {
		return j.KeyValue.Update(ctx, key, value, revision)
	}
	kv.updates++
	if kv.updates == 1 {
		kv.hooking = true
		kv.beforeFirstUpdate()
		kv.hooking = false
	}
	return j.KeyValue.Update(ctx, key, value, revision)
}

func (kv *putBeforeDeleteKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &putBeforeDeleteJS{KeyValue: inner, kv: kv}
}

type putBeforeDeleteJS struct {
	jetstream.KeyValue
	kv *putBeforeDeleteKeyValue
}

func (j *putBeforeDeleteJS) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	j.kv.deletes++
	if j.kv.deletes == 1 {
		j.kv.beforeDelete()
	}
	return j.KeyValue.Delete(ctx, key, opts...)
}

func (kv *failingKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &failingJS{KeyValue: inner, kv: kv}
}

type failingJS struct {
	jetstream.KeyValue
	kv *failingKeyValue
}

func (j *failingJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	j.kv.puts++
	if j.kv.failPutAt == j.kv.puts {
		return 0, j.kv.err
	}
	return j.KeyValue.Put(ctx, key, value)
}

func (j *failingJS) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	j.kv.creates++
	if j.kv.failCreateAt == j.kv.creates {
		return 0, j.kv.err
	}
	return j.KeyValue.Create(ctx, key, value, opts...)
}

func (j *failingJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	j.kv.updates++
	if j.kv.failUpdateAt == j.kv.updates {
		return 0, j.kv.err
	}
	return j.KeyValue.Update(ctx, key, value, revision)
}

func (kv *afterRoleWriteKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &afterRoleWriteJS{KeyValue: inner, kv: kv}
}

type afterRoleWriteJS struct {
	jetstream.KeyValue
	kv *afterRoleWriteKeyValue
}

func (j *afterRoleWriteJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	revision, err := j.KeyValue.Put(ctx, key, value)
	if err == nil && j.kv.after != nil {
		j.kv.after()
	}
	return revision, err
}

func (j *afterRoleWriteJS) Update(ctx context.Context, key string, value []byte, last uint64) (uint64, error) {
	revision, err := j.KeyValue.Update(ctx, key, value, last)
	if err == nil && j.kv.after != nil {
		j.kv.after()
	}
	return revision, err
}

func (kv *racingCreateKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &racingCreateJS{KeyValue: inner, kv: kv}
}

type racingCreateJS struct {
	jetstream.KeyValue
	kv *racingCreateKeyValue
}

func (j *racingCreateJS) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	j.kv.creates++
	if j.kv.creates == 1 {
		j.kv.beforeFirstCreate()
		return 0, natsgo.ErrKeyExists
	}
	return j.KeyValue.Create(ctx, key, value, opts...)
}

func (kv *racingDeleteKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &racingDeleteJS{KeyValue: inner, kv: kv}
}

type racingDeleteJS struct {
	jetstream.KeyValue
	kv *racingDeleteKeyValue
}

func (j *racingDeleteJS) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	j.kv.deletes++
	if j.kv.deletes == 1 {
		j.kv.beforeFirstDelete()
		return j.kv.err
	}
	return j.KeyValue.Delete(ctx, key, opts...)
}

func (kv *interleavingPutKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &interleavingPutJS{KeyValue: inner, kv: kv}
}

type interleavingPutJS struct {
	jetstream.KeyValue
	kv *interleavingPutKeyValue
}

func (j *interleavingPutJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
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

func (j *interleavingPutJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	j.kv.puts++
	written, err := j.KeyValue.Update(ctx, key, value, revision)
	if err != nil {
		return 0, err
	}
	if j.kv.puts == 1 {
		j.kv.afterFirstPut()
	}
	return written, nil
}

func (kv *endedContextRoleKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &endedContextRoleJS{KeyValue: inner, kv: kv}
}

type endedContextRoleJS struct {
	jetstream.KeyValue
	kv *endedContextRoleKeyValue
}

func (j *endedContextRoleJS) Delete(context.Context, string, ...jetstream.KVDeleteOpt) error {
	j.kv.mu.Lock()
	j.kv.deletes++
	j.kv.mu.Unlock()
	j.kv.end()
	return errors.New("nats: no responders available for request")
}

func (kv *blockedDeleteRoleKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &blockedDeleteRoleJS{KeyValue: inner, kv: kv}
}

type blockedDeleteRoleJS struct {
	jetstream.KeyValue
	kv *blockedDeleteRoleKeyValue
}

func (j *blockedDeleteRoleJS) Delete(context.Context, string, ...jetstream.KVDeleteOpt) error {
	j.kv.mu.Lock()
	j.kv.deletes++
	j.kv.mu.Unlock()
	j.kv.first.Do(func() { close(j.kv.sent) })
	<-j.kv.release
	return errors.New("nats: no responders available for request")
}

func (kv *refusedGetRoleKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &refusedGetRoleJS{KeyValue: inner, kv: kv}
}

type refusedGetRoleJS struct {
	jetstream.KeyValue
	kv *refusedGetRoleKeyValue
}

func (j *refusedGetRoleJS) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	j.kv.mu.Lock()
	j.kv.deletes++
	j.kv.mu.Unlock()
	return j.KeyValue.Delete(ctx, key, opts...)
}

func (kv *racingEachDeleteKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &racingEachDeleteJS{KeyValue: inner, kv: kv}
}

type racingEachDeleteJS struct {
	jetstream.KeyValue
	kv *racingEachDeleteKeyValue
}

func (j *racingEachDeleteJS) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	j.kv.deletes++
	if j.kv.deletes <= j.kv.racedDeletes {
		j.kv.beforeDelete()
	}
	return j.KeyValue.Delete(ctx, key, opts...)
}

func (kv *racingEachUpdateKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &racingEachUpdateJS{KeyValue: inner, kv: kv}
}

type racingEachUpdateJS struct {
	jetstream.KeyValue
	kv *racingEachUpdateKeyValue
}

func (j *racingEachUpdateJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	kv := j.kv
	if !kv.racing && kv.races < kv.racedUpdates {
		kv.races++
		kv.racing = true
		kv.beforeUpdate()
		kv.racing = false
	}
	return j.KeyValue.Update(ctx, key, value, revision)
}

func (kv *afterGetKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &afterGetJS{KeyValue: inner, kv: kv}
}

type afterGetJS struct {
	jetstream.KeyValue
	kv *afterGetKeyValue
}

func (j *afterGetJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if j.kv.update != nil {
		return j.kv.update(key, value, revision)
	}
	return j.KeyValue.Update(ctx, key, value, revision)
}

func (kv *sweepRoleKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &sweepRoleJS{KeyValue: inner, kv: kv}
}

type sweepRoleJS struct {
	jetstream.KeyValue
	kv *sweepRoleKeyValue
}

func (j *sweepRoleJS) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	kv := j.kv
	kv.mu.Lock()
	kv.deletes[key]++
	kv.deleteTotal++
	first := kv.deleteTotal == 1
	kv.mu.Unlock()
	if first && kv.beforeFirstDelete != nil {
		kv.beforeFirstDelete(key)
	}
	time.Sleep(kv.delay)
	if err := kv.failDelete[key]; err != nil {
		return err
	}
	return j.KeyValue.Delete(ctx, key, opts...)
}

func (kv *countedKeyValue) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &countedJS{KeyValue: inner, kv: kv}
}

type countedJS struct {
	jetstream.KeyValue
	kv *countedKeyValue
}

func (j *countedJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if j.kv.failUpdates.Load() {
		return 0, errClaimWriteFailed
	}
	return j.KeyValue.Update(ctx, key, value, revision)
}
