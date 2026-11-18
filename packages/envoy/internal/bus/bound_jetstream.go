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
// had: the context its caller passed (nats.Context), or else the context's MaxWait.
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
}

func (js boundJetStream) AccountInfo(opts ...nats.JSOpt) (*nats.AccountInfo, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.AccountInfo, error) {
		return js.JetStreamContext.AccountInfo(opts...)
	})
}

func (js boundJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.StreamInfo, error) {
		return js.JetStreamContext.StreamInfo(stream, opts...)
	})
}

func (js boundJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.StreamInfo, error) {
		return js.JetStreamContext.AddStream(cfg, opts...)
	})
}

func (js boundJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.StreamInfo, error) {
		return js.JetStreamContext.UpdateStream(cfg, opts...)
	})
}

func (js boundJetStream) DeleteStream(name string, opts ...nats.JSOpt) error {
	_, err := RequestBounded(context.Background(), js.client, func() (struct{}, error) {
		return struct{}{}, js.JetStreamContext.DeleteStream(name, opts...)
	})
	return err
}

func (js boundJetStream) PurgeStream(name string, opts ...nats.JSOpt) error {
	_, err := RequestBounded(context.Background(), js.client, func() (struct{}, error) {
		return struct{}{}, js.JetStreamContext.PurgeStream(name, opts...)
	})
	return err
}

func (js boundJetStream) ConsumerInfo(stream, name string, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.ConsumerInfo, error) {
		return js.JetStreamContext.ConsumerInfo(stream, name, opts...)
	})
}

func (js boundJetStream) AddConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.ConsumerInfo, error) {
		return js.JetStreamContext.AddConsumer(stream, cfg, opts...)
	})
}

func (js boundJetStream) UpdateConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.ConsumerInfo, error) {
		return js.JetStreamContext.UpdateConsumer(stream, cfg, opts...)
	})
}

func (js boundJetStream) DeleteConsumer(stream, consumer string, opts ...nats.JSOpt) error {
	_, err := RequestBounded(context.Background(), js.client, func() (struct{}, error) {
		return struct{}{}, js.JetStreamContext.DeleteConsumer(stream, consumer, opts...)
	})
	return err
}

func (js boundJetStream) GetMsg(name string, seq uint64, opts ...nats.JSOpt) (*nats.RawStreamMsg, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.RawStreamMsg, error) {
		return js.JetStreamContext.GetMsg(name, seq, opts...)
	})
}

func (js boundJetStream) GetLastMsg(name, subject string, opts ...nats.JSOpt) (*nats.RawStreamMsg, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.RawStreamMsg, error) {
		return js.JetStreamContext.GetLastMsg(name, subject, opts...)
	})
}

func (js boundJetStream) KeyValue(bucket string) (nats.KeyValue, error) {
	return RequestBounded(context.Background(), js.client, func() (nats.KeyValue, error) {
		return js.JetStreamContext.KeyValue(bucket)
	})
}

func (js boundJetStream) CreateKeyValue(cfg *nats.KeyValueConfig) (nats.KeyValue, error) {
	return RequestBounded(context.Background(), js.client, func() (nats.KeyValue, error) {
		return js.JetStreamContext.CreateKeyValue(cfg)
	})
}

func (js boundJetStream) Publish(subj string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.PubAck, error) {
		return js.JetStreamContext.Publish(subj, data, opts...)
	})
}

func (js boundJetStream) PublishMsg(m *nats.Msg, opts ...nats.PubOpt) (*nats.PubAck, error) {
	return RequestBounded(context.Background(), js.client, func() (*nats.PubAck, error) {
		return js.JetStreamContext.PublishMsg(m, opts...)
	})
}
