// A legacy JetStream context whose requests are bound to the life of the connection they are sent
// on (Request).

package bus

import (
	"context"

	"github.com/nats-io/nats.go"
)

// JetStream opens conn's JetStream context with opts, with the calls this codebase makes of it bound
// to the life of conn when a client owns it (boundJetStream), and unbound when none does (a
// connection bus.Dial or a test opened). Every program opens its legacy JetStream contexts through
// it, and the client opens its own the same way (install, ensureConnWithContext).
func JetStream(conn *nats.Conn, opts ...nats.JSOpt) (nats.JetStreamContext, error) {
	js, err := conn.JetStream(opts...)
	if err != nil {
		return nil, err
	}
	return bindJetStream(clientOwning(conn), js), nil
}

// bindJetStream is js with its calls bound to the life of c's connection, and js itself for a nil c
// and for a js already bound.
func bindJetStream(c *Client, js nats.JetStreamContext) nats.JetStreamContext {
	if _, bound := js.(boundJetStream); c == nil || bound {
		return js
	}
	return boundJetStream{JetStreamContext: js, client: c}
}

// boundJetStream is a legacy JetStream context whose account, stream, consumer, bucket-open,
// message-read and publish calls are each a request bound to the life of client's connection
// (RequestBounded): one whose answer the connection's loss cut off answers ErrConnectionLost at the
// loss, and the call goes on to its own end with its answer dropped. Each call keeps the bound it
// had: the context its caller passed (nats.Context), or else the context's MaxWait. That context
// binds the request too (callContext), so the request's read of its connection's state waits no
// longer than the call, and answers within the probe's budget when the caller marked it as made
// under a lock (WithLockHeld).
//
// What it leaves unbound is named here. Subscribe and its kinds: nats.go unsubscribes a JetStream
// subscription made under a context once that context ends (js.go:2060-2066), so a subscription
// bound to the connection's life would end at its first disconnect, and one left to run on after
// its caller was answered would be a subscription nobody holds; the client binds its one JetStream
// subscription holding no lock across the request (bind), and its recovery binds it again after a
// bind that failed. PublishAsync and PublishMsgAsync: their acknowledgements are futures a publish
// window answers ErrUnavailable for once the window's connection is lost (window.acknowledged). The
// listing calls (StreamNames, ConsumerNames and their kinds), which page on a goroutine of
// nats.go's own and hand their names over a channel: a start reads them once
// (migrateLegacyConsumerFilters), and nothing waits on them under a lock.
type boundJetStream struct {
	nats.JetStreamContext
	client *Client
	// open is the context its bucket opens (KeyValue, CreateKeyValue) are bound with: nats.go's
	// KeyValue and CreateKeyValue take no options, so the open passes its caller's context here
	// (openWithin). nil for a wrapper no open has bound (bindJetStream), whose opens are made
	// under context.Background (openContext).
	open context.Context
}

// openWithin is js with its bucket opens bound under ctx, the context of the open that makes them
// (OpenKeyValue, ensureKeyValue): a caller that holds a lock across the open and marks ctx so
// (WithLockHeld) hears a loss at once, and one that holds none waits for the connection within
// ctx. A js that is not this package's wrapper, under a refusal reader or not, is returned as it is.
func openWithin(ctx context.Context, js nats.JetStreamContext) nats.JetStreamContext {
	switch wrapped := js.(type) {
	case boundJetStream:
		wrapped.open = ctx
		return wrapped
	case refusalReads:
		wrapped.JetStreamContext = openWithin(ctx, wrapped.JetStreamContext)
		return wrapped
	}
	return js
}

func (js boundJetStream) AccountInfo(opts ...nats.JSOpt) (*nats.AccountInfo, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.AccountInfo, error) {
		return js.JetStreamContext.AccountInfo(opts...)
	})
}

func (js boundJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.StreamInfo, error) {
		return js.JetStreamContext.StreamInfo(stream, opts...)
	})
}

func (js boundJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.StreamInfo, error) {
		return js.JetStreamContext.AddStream(cfg, opts...)
	})
}

func (js boundJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.StreamInfo, error) {
		return js.JetStreamContext.UpdateStream(cfg, opts...)
	})
}

func (js boundJetStream) DeleteStream(name string, opts ...nats.JSOpt) error {
	_, err := RequestBounded(callContext(opts), js.client, func() (struct{}, error) {
		return struct{}{}, js.JetStreamContext.DeleteStream(name, opts...)
	})
	return err
}

func (js boundJetStream) PurgeStream(name string, opts ...nats.JSOpt) error {
	_, err := RequestBounded(callContext(opts), js.client, func() (struct{}, error) {
		return struct{}{}, js.JetStreamContext.PurgeStream(name, opts...)
	})
	return err
}

func (js boundJetStream) ConsumerInfo(stream, name string, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.ConsumerInfo, error) {
		return js.JetStreamContext.ConsumerInfo(stream, name, opts...)
	})
}

func (js boundJetStream) AddConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.ConsumerInfo, error) {
		return js.JetStreamContext.AddConsumer(stream, cfg, opts...)
	})
}

func (js boundJetStream) UpdateConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.ConsumerInfo, error) {
		return js.JetStreamContext.UpdateConsumer(stream, cfg, opts...)
	})
}

func (js boundJetStream) DeleteConsumer(stream, consumer string, opts ...nats.JSOpt) error {
	_, err := RequestBounded(callContext(opts), js.client, func() (struct{}, error) {
		return struct{}{}, js.JetStreamContext.DeleteConsumer(stream, consumer, opts...)
	})
	return err
}

func (js boundJetStream) GetMsg(name string, seq uint64, opts ...nats.JSOpt) (*nats.RawStreamMsg, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.RawStreamMsg, error) {
		return js.JetStreamContext.GetMsg(name, seq, opts...)
	})
}

func (js boundJetStream) GetLastMsg(name, subject string, opts ...nats.JSOpt) (*nats.RawStreamMsg, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.RawStreamMsg, error) {
		return js.JetStreamContext.GetLastMsg(name, subject, opts...)
	})
}

// openContext is the context a bucket open is bound with: the open's own (openWithin), and
// context.Background for a wrapper no open has bound, made here at the call, as callContext makes
// it for a call whose options carry none.
func (js boundJetStream) openContext() context.Context {
	if js.open == nil {
		return context.Background()
	}
	return js.open
}

func (js boundJetStream) KeyValue(bucket string) (nats.KeyValue, error) {
	return RequestBounded(js.openContext(), js.client, func() (nats.KeyValue, error) {
		return js.JetStreamContext.KeyValue(bucket)
	})
}

func (js boundJetStream) CreateKeyValue(cfg *nats.KeyValueConfig) (nats.KeyValue, error) {
	return RequestBounded(js.openContext(), js.client, func() (nats.KeyValue, error) {
		return js.JetStreamContext.CreateKeyValue(cfg)
	})
}

func (js boundJetStream) Publish(subj string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.PubAck, error) {
		return js.JetStreamContext.Publish(subj, data, opts...)
	})
}

func (js boundJetStream) PublishMsg(m *nats.Msg, opts ...nats.PubOpt) (*nats.PubAck, error) {
	return RequestBounded(callContext(opts), js.client, func() (*nats.PubAck, error) {
		return js.JetStreamContext.PublishMsg(m, opts...)
	})
}

// callContext is the context a call's options carry (nats.Context), and context.Background when
// none does: a call's deadline, and its caller's mark of a lock it holds across the call
// (WithLockHeld), reach the request it is bound as.
func callContext[O any](opts []O) context.Context {
	for _, opt := range opts {
		if carried, ok := any(opt).(nats.ContextOpt); ok && carried.Context != nil {
			return carried.Context
		}
	}
	return context.Background()
}
