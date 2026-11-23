package legionagents_test

// The store's writes go through the bucket handle's jetstream handle (bus.KeyValue.Within), so each
// write fault a test wraps around the legacy handle has a jetstream twin holding the same fault over
// the wrapper's own state, which WrapBucket installs (legionagents.JetStreamTwin).

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

func (c *claimFirst) JetStreamTwin(inner jetstream.KeyValue) jetstream.KeyValue {
	return &claimFirstJS{KeyValue: inner, c: c}
}

// claimFirstJS is claimFirst's jetstream twin: the first create of the key runs before.
type claimFirstJS struct {
	jetstream.KeyValue
	c *claimFirst
}

func (j *claimFirstJS) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	if key == j.c.key {
		j.c.once.Do(j.c.before)
	}
	return j.KeyValue.Create(ctx, key, value, opts...)
}

func (f *failingPuts) JetStreamTwin(inner jetstream.KeyValue) jetstream.KeyValue {
	return &failingPutsJS{KeyValue: inner, f: f}
}

// failingPutsJS is failingPuts' jetstream twin: puts beyond the ones left fail, counted with the
// wrapper's.
type failingPutsJS struct {
	jetstream.KeyValue
	f *failingPuts
}

func (j *failingPutsJS) Put(ctx context.Context, key string, value []byte) (uint64, error) {
	j.f.mu.Lock()
	defer j.f.mu.Unlock()
	if j.f.left == 0 {
		return 0, errors.New("the write ended here")
	}
	j.f.left--
	return j.KeyValue.Put(ctx, key, value)
}
