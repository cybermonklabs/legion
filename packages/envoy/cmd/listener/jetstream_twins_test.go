package main

// The registries' bucket writes are given their caller's context (bus.KeyValue.Within), which sends
// them through the handle's jetstream handle rather than its legacy one. So each legacy wrapper
// below that holds a write fault has a jetstream twin holding the same fault over the wrapper's own
// state, and the swaps that install a wrapper (swapSessionBucket, setRegistryRoleKV) install its
// twin as the handle's jetstream handle.

import (
	"context"
	"fmt"
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

// refusingEntryJS is refusingEntryKV's jetstream twin: every put fails after before has run.
type refusingEntryJS struct {
	jetstream.KeyValue
	kv refusingEntryKV
}

func (j refusingEntryJS) Put(context.Context, string, []byte) (uint64, error) {
	j.kv.before()
	return 0, fmt.Errorf("injected session entry write failure")
}
