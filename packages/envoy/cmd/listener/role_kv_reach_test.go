package main

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"unsafe"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/store"
)

// registryRoleKV and setRegistryRoleKV reach into store.Registry's unexported roleKV field so a
// test can stall or wrap the role bucket the same way internal/store/kv_test.go does from inside
// package store. Each holds the mutex that guards the field (store.Registry's kvMu), which every
// reader of it takes, so a swap made while a goroutine of the listener reads the handle (a claim's
// handover, a relay's check) is ordered with that read rather than racing it.
func registryRoleKV(t *testing.T, registry *store.Registry) natsgo.KeyValue {
	t.Helper()
	mu := registryRoleKVMutex(registry)
	mu.RLock()
	defer mu.RUnlock()
	field := reflect.ValueOf(registry).Elem().FieldByName("roleKV")
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(bus.KeyValue).Legacy()
}

func setRegistryRoleKV(t *testing.T, registry *store.Registry, kv natsgo.KeyValue) {
	t.Helper()
	// The replacement keeps the bucket's jetstream handle, which every scan of it goes through.
	swapRegistryRoleHandle(t, registry, func(base bus.KeyValue) bus.KeyValue {
		return bus.NewKeyValue(kv, base.WatchHandle(), base.Streams())
	})
}

// swapRegistryRoleHandle is setRegistryRoleKV for a test whose stand-in needs the handle it
// replaces, such as one that makes each read from the stream's leader through it
// (leaderReadCount): registry reads the role bucket through what swap returns over that handle.
func swapRegistryRoleHandle(t *testing.T, registry *store.Registry, swap func(bus.KeyValue) bus.KeyValue) {
	t.Helper()
	mu := registryRoleKVMutex(registry)
	mu.Lock()
	defer mu.Unlock()
	field := reflect.ValueOf(registry).Elem().FieldByName("roleKV")
	current := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	current.Set(reflect.ValueOf(swap(current.Interface().(bus.KeyValue))))
}

// registryRoleKVMutex is the mutex store.Registry guards its roleKV field with (kvMu).
func registryRoleKVMutex(registry *store.Registry) *sync.RWMutex {
	field := reflect.ValueOf(registry).Elem().FieldByName("kvMu")
	return (*sync.RWMutex)(unsafe.Pointer(field.UnsafeAddr()))
}

// bucketThatFails is a bucket no read of which answers, a direct get (Get) or a read from the
// stream's leader (NewestFromLeader, bus.LeaderReads): err is what each read answers with, and
// before runs first, which is where a test ends the context of the handover making that read, or
// counts the read (failRoleClaimReads).
type bucketThatFails struct {
	natsgo.KeyValue
	before func()
	err    error
}

func (kv bucketThatFails) Get(string) (natsgo.KeyValueEntry, error) {
	if kv.before != nil {
		kv.before()
	}
	return nil, kv.err
}

func (kv bucketThatFails) NewestFromLeader(context.Context, string) (natsgo.KeyValueEntry, error) {
	return kv.Get("")
}
