package main

// The registries' bucket writes are given their caller's context (bus.KeyValue.Within), which sends
// them through the handle's jetstream handle rather than its legacy one. So each legacy wrapper
// below that holds a write fault has a jetstream twin holding the same fault over the wrapper's own
// state, and the swaps that install a wrapper (swapSessionBucket, setRegistryRoleKV) install its
// twin as the handle's jetstream handle.

import (
	"context"
	"errors"
	"time"

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

func (kv *racingRoleClaimUpdate) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &racingRoleClaimUpdateJS{KeyValue: inner, kv: kv}
}

// racingRoleClaimUpdateJS is racingRoleClaimUpdate's jetstream twin: the first update runs the hook
// over the legacy handle first, counted with the wrapper's.
type racingRoleClaimUpdateJS struct {
	jetstream.KeyValue
	kv *racingRoleClaimUpdate
}

func (j *racingRoleClaimUpdateJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	j.kv.updates++
	if j.kv.updates == 1 {
		j.kv.beforeFirstUpdate(j.kv.KeyValue)
	}
	return j.KeyValue.Update(ctx, key, value, revision)
}

func (kv *sweepRouteRoleKV) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return &sweepRouteRoleJS{KeyValue: inner, kv: kv}
}

// sweepRouteRoleJS is sweepRouteRoleKV's jetstream twin: its deletes count with the wrapper's and
// take its delay.
type sweepRouteRoleJS struct {
	jetstream.KeyValue
	kv *sweepRouteRoleKV
}

func (j *sweepRouteRoleJS) Delete(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	j.kv.mu.Lock()
	j.kv.deletes[key]++
	j.kv.deleteTotal++
	j.kv.mu.Unlock()
	time.Sleep(sweepRouteCallDelay)
	return j.KeyValue.Delete(ctx, key, opts...)
}

func (kv refusingEntryKV) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return refusingEntryJS{KeyValue: inner, kv: kv}
}

// refusingEntryJS is refusingEntryKV's jetstream twin: every write of an entry fails after before
// has run, a Put and the Create and Update the entry write sends alike.
type refusingEntryJS struct {
	jetstream.KeyValue
	kv refusingEntryKV
}

func (j refusingEntryJS) Put(context.Context, string, []byte) (uint64, error) { return j.kv.refuse() }

func (j refusingEntryJS) Create(context.Context, string, []byte, ...jetstream.KVCreateOpt) (uint64, error) {
	return j.kv.refuse()
}

func (j refusingEntryJS) Update(context.Context, string, []byte, uint64) (uint64, error) {
	return j.kv.refuse()
}

func (kv stalledSessionEntryKV) watching(inner jetstream.KeyValue) jetstream.KeyValue {
	return stalledSessionEntryJS{KeyValue: inner, kv: kv}
}

// stalledSessionEntryJS is stalledSessionEntryKV's jetstream twin: its conditional write of the
// wrapper's key does not answer until released is closed or the write's context ends, as a
// jetstream request to a bucket that stopped answering ends with its context.
type stalledSessionEntryJS struct {
	jetstream.KeyValue
	kv stalledSessionEntryKV
}

func (j stalledSessionEntryJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key != j.kv.key {
		return j.KeyValue.Update(ctx, key, value, revision)
	}
	select {
	case <-j.kv.released:
		return 0, errors.New("the session bucket's write of the entry never answered")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
