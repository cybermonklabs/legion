package bus

import (
	"context"
	"slices"

	"github.com/nats-io/nats.go"
)

// requestsUnder is js with every request the stream's ensure and its subject read make (the
// methods below) bounded by ctx, which nats.go then waits on in place of js's own MaxWait:
// nats.go's legacy JetStream context takes a context per call only (nats.Context; one given to
// JetStream() itself is not read by these calls), so each call here adds it. A reconnect hook
// gives it the run's context (whileConnected), so a request sent on a connection that then drops
// ends with the connection rather than at its MaxWait. A caller must not pass nats.MaxWait to
// these methods: nats.go refuses a context and a wait together. Every other method is js's own and
// is not bounded by ctx, the purge among them, which nats.go sends under js's own wait whatever
// context it is handed; the stream ensure's purges are waited for up to the start's write limit
// instead (purgeRoleLane).
func requestsUnder(ctx context.Context, js nats.JetStreamContext) nats.JetStreamContext {
	return contextRequests{JetStreamContext: js, ctx: ctx}
}

type contextRequests struct {
	nats.JetStreamContext
	ctx context.Context
}

func (r contextRequests) with(opts []nats.JSOpt) []nats.JSOpt {
	return append(slices.Clone(opts), nats.Context(r.ctx))
}

func (r contextRequests) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return r.JetStreamContext.StreamInfo(stream, r.with(opts)...)
}

func (r contextRequests) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return r.JetStreamContext.AddStream(cfg, r.with(opts)...)
}

func (r contextRequests) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return r.JetStreamContext.UpdateStream(cfg, r.with(opts)...)
}

func (r contextRequests) ConsumerInfo(stream, consumer string, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return r.JetStreamContext.ConsumerInfo(stream, consumer, r.with(opts)...)
}

func (r contextRequests) UpdateConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return r.JetStreamContext.UpdateConsumer(stream, cfg, r.with(opts)...)
}

func (r contextRequests) ConsumerNames(stream string, opts ...nats.JSOpt) <-chan string {
	return r.JetStreamContext.ConsumerNames(stream, r.with(opts)...)
}
