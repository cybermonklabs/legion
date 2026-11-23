package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A spent-proof create its request gave up on writes nothing after the request has answered, so
// the proof presented again is not refused as a replay. Tester4926 found the case on #4926: with
// the bucket's stream moved off its keys, a check given 700 ms gave up, and once the stream took
// its keys again the create the request had left running landed, and the same proof read as spent
// 4 s later, 3 runs of 3, with the create sent through the handle given no context. A 503 is what
// such a check answers (refuseBrokerToken's check_failed), and the client and the balancer in front
// of an edge both send the request again with the same proof, which #4820 exists to serve.
//
// Here no clock decides: the create's first send is held at a gate once it has started, the
// request's context ends while it is held, the check answers, and only then does the send go on.
// Through the handle given the request's context (bus.KeyValue.Within) it sends nothing, so the
// proof presented again is unspent. Control (Tester4926's qJ): the create through the handle given
// no context, `s.kv.Create(key, nil)` in CheckAndSetDPoPProofUsed; the held send goes on, stores
// the record after the request answered, and the proof presented again reads as spent.
func TestASpentProofCreateItsRequestGaveUpOnWritesNothingLater(t *testing.T) {
	client := setupPublishTestClient(t)
	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	kv, err := ensureSpentProofBucket(t.Context(), client.Conn, js, 1)
	if err != nil {
		t.Fatalf("open the spent-proof bucket: %v", err)
	}
	gate := &proofCreateGate{entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan error, 1)}
	held := spentProofs{kv: bus.NewKeyValue(gatedProofLegacy{KeyValue: kv.Legacy(), gate: gate}, gatedProofJS{KeyValue: kv.WatchHandle(), gate: gate}, kv.Streams())}
	const jti, htu = "jti-given-up-create", listenerBrokerOrigin + "/v1/sessions"
	ctx, giveUp := context.WithCancel(t.Context())
	defer giveUp()
	checked := make(chan error, 1)
	go func() {
		_, err := held.CheckAndSetDPoPProofUsed(ctx, jti, "", "", http.MethodGet, htu, time.Time{})
		checked <- err
	}()
	testwait.Receive(t, "the create's first send, held at the gate", (<-chan struct{})(gate.entered))
	giveUp()
	if err := testwait.Receive(t, "the given-up check's answer", (<-chan error)(checked)); err == nil {
		t.Fatal("the check whose request gave up during its create answered that it recorded the proof")
	}
	close(gate.release)
	sent := testwait.Receive(t, "the held send's answer once it went on", (<-chan error)(gate.returned))
	again, err := spentProofs{kv: kv}.CheckAndSetDPoPProofUsed(t.Context(), jti, "", "", http.MethodGet, htu, time.Time{})
	if err != nil || again {
		t.Fatalf("the proof whose create its request gave up on reads (spent %v, %v) once that create's held send went on (it answered %v); want unspent: the create landed after its request answered, so the retry of that proof is refused as a replay", again, err, sent)
	}
}

// proofCreateGate holds the first create a spent-proof bucket handle sends, through its legacy
// handle or its jetstream handle alike: it closes entered, waits for release, sends, and puts the
// send's answer on returned.
type proofCreateGate struct {
	once     sync.Once
	entered  chan struct{}
	release  chan struct{}
	returned chan error
}

func (g *proofCreateGate) hold(send func() (uint64, error)) (uint64, error) {
	first := false
	g.once.Do(func() { first = true })
	if !first {
		return send()
	}
	close(g.entered)
	<-g.release
	revision, err := send()
	g.returned <- err
	return revision, err
}

// gatedProofLegacy is a spent-proof bucket's legacy handle whose first create is held at gate.
type gatedProofLegacy struct {
	nats.KeyValue
	gate *proofCreateGate
}

func (k gatedProofLegacy) Create(key string, value []byte) (uint64, error) {
	return k.gate.hold(func() (uint64, error) { return k.KeyValue.Create(key, value) })
}

// gatedProofJS is gatedProofLegacy's jetstream twin: the create a handle given its caller's context
// sends is held at the same gate, a Create and the Update a stream-message-reads handle sends in
// its place alike.
type gatedProofJS struct {
	jetstream.KeyValue
	gate *proofCreateGate
}

func (k gatedProofJS) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	return k.gate.hold(func() (uint64, error) { return k.KeyValue.Create(ctx, key, value, opts...) })
}

func (k gatedProofJS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	return k.gate.hold(func() (uint64, error) { return k.KeyValue.Update(ctx, key, value, revision) })
}
