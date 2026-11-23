package legionagents

import (
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// WrapBucket replaces the store's bucket handle with the one wrap builds over it, so a test can
// hold or fail a call NATS would answer: two writes that both read before either claims, or a
// write that ends partway. The store's writes are given their caller's context
// (bus.KeyValue.Within), which sends them through the handle's jetstream handle, so a wrapper with
// a write fault has a jetstream twin holding the same fault (JetStreamTwin), and that twin becomes
// the handle's jetstream handle.
func (s *Store) WrapBucket(wrap func(nats.KeyValue) nats.KeyValue) {
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	wrapped := wrap(s.kv.Legacy())
	watch := s.kv.WatchHandle()
	if twin, ok := wrapped.(JetStreamTwin); ok {
		watch = twin.JetStreamTwin(watch)
	}
	s.kv = bus.NewKeyValue(wrapped, watch, s.kv.Streams())
}

// JetStreamTwin is a legacy wrapper whose write faults have a jetstream twin: JetStreamTwin wraps
// the bucket's jetstream handle in it.
type JetStreamTwin interface {
	JetStreamTwin(inner jetstream.KeyValue) jetstream.KeyValue
}

// WrapWatch replaces the handle a listing's watch is built on with the one wrap builds over it, so
// a test can stall a scan NATS would answer: a watch that delivers no key and no
// end-of-existing-keys marker.
func (s *Store) WrapWatch(wrap func(jetstream.KeyValue) jetstream.KeyValue) {
	s.kvMu.Lock()
	defer s.kvMu.Unlock()
	s.kv = bus.NewKeyValue(s.kv.Legacy(), wrap(s.kv.WatchHandle()), s.kv.Streams())
}
