package bus

import (
	"cmp"

	"github.com/nats-io/nats.go"
)

// jsAPI is the subject a JetStream API request of the given kind and names is sent on, under the
// default API prefix every connection of this package uses: jsAPI("STREAM.INFO", "KV_x") is
// "$JS.API.STREAM.INFO.KV_x". The kinds are nats.go's own (nats.go@v1.53.1 js.go:151-207).
func jsAPI(kind string, names ...string) string {
	subject := "$JS.API." + kind
	for _, name := range names {
		subject += "." + name
	}
	return subject
}

// readsRefusals is js with every JetStream API request this package sends through it (the methods
// below) sent through refusedSend, each under the subjects that request publishes to, so a request
// the connection's grant refuses ends at once with the server's refusal (ErrPublishDenied) rather
// than at the end of its own wait as a request nothing answered. It is the one path: the client's
// own JetStream context is one (install, ensureConnWithContext), and EnsureKeyValue,
// SetKeyValueMaxAge, OpenKeyValue, the stream's ensure and the stream's subject read take one over
// the context their caller hands them. A context already wrapped is returned as it is. Every
// other method is js's own; ConsumerNames is among them, since its list reports no error a
// refusal could answer, and so are the asynchronous publishes, whose refusal the window reads off
// the same record (window.acknowledged). On a connection no client owns, refusedSend sends as js
// does.
func readsRefusals(conn *nats.Conn, js nats.JetStreamContext) nats.JetStreamContext {
	if _, ok := js.(refusalReads); ok {
		return js
	}
	return refusalReads{JetStreamContext: js, conn: conn}
}

// readsOwnRefusals is readsRefusals with c named as the client whose record is read, for c's own
// stream ensure: the constructor ensures the stream on the connection it is installing before c
// owns it (install), when clientOwning cannot find c yet.
func (c *Client) readsOwnRefusals(conn *nats.Conn, js nats.JetStreamContext) nats.JetStreamContext {
	if wrapped, ok := js.(refusalReads); ok {
		js = wrapped.JetStreamContext
	}
	return refusalReads{JetStreamContext: js, conn: conn, owner: c}
}

type refusalReads struct {
	nats.JetStreamContext
	conn  *nats.Conn
	owner *Client
}

// refusedVia is refusedSend through r: its named owner, or the client owning its connection.
func refusedVia[T any](r refusalReads, subjects []string, send func() (T, error)) (T, error) {
	if r.owner != nil {
		return refusedBy(r.owner, subjects, send)
	}
	return refusedSend(r.conn, subjects, send)
}

func (r refusalReads) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return refusedVia(r, []string{jsAPI("STREAM.INFO", stream)}, func() (*nats.StreamInfo, error) {
		return r.JetStreamContext.StreamInfo(stream, opts...)
	})
}

func (r refusalReads) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return refusedVia(r, []string{jsAPI("STREAM.CREATE", cfg.Name)}, func() (*nats.StreamInfo, error) {
		return r.JetStreamContext.AddStream(cfg, opts...)
	})
}

func (r refusalReads) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return refusedVia(r, []string{jsAPI("STREAM.UPDATE", cfg.Name)}, func() (*nats.StreamInfo, error) {
		return r.JetStreamContext.UpdateStream(cfg, opts...)
	})
}

func (r refusalReads) PurgeStream(stream string, opts ...nats.JSOpt) error {
	_, err := refusedVia(r, []string{jsAPI("STREAM.PURGE", stream)}, func() (struct{}, error) {
		return struct{}{}, r.JetStreamContext.PurgeStream(stream, opts...)
	})
	return err
}

func (r refusalReads) ConsumerInfo(stream, consumer string, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return refusedVia(r, []string{jsAPI("CONSUMER.INFO", stream, consumer)}, func() (*nats.ConsumerInfo, error) {
		return r.JetStreamContext.ConsumerInfo(stream, consumer, opts...)
	})
}

// UpdateConsumer's request goes to one of three subjects, which nats.go picks by the server's
// version and the consumer's filter (nats.go@v1.53.1 jsm.go:505-533), so all three are read.
func (r refusalReads) UpdateConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	name := cmp.Or(cfg.Name, cfg.Durable)
	subjects := []string{jsAPI("CONSUMER.CREATE", stream, name), jsAPI("CONSUMER.DURABLE.CREATE", stream, name)}
	if cfg.FilterSubject != "" {
		subjects = append(subjects, jsAPI("CONSUMER.CREATE", stream, name, cfg.FilterSubject))
	}
	return refusedVia(r, subjects, func() (*nats.ConsumerInfo, error) {
		return r.JetStreamContext.UpdateConsumer(stream, cfg, opts...)
	})
}

func (r refusalReads) GetLastMsg(stream, subject string, opts ...nats.JSOpt) (*nats.RawStreamMsg, error) {
	return refusedVia(r, []string{jsAPI("STREAM.MSG.GET", stream)}, func() (*nats.RawStreamMsg, error) {
		return r.JetStreamContext.GetLastMsg(stream, subject, opts...)
	})
}

// Publish is a stored publish to subject, which the stream acknowledges and which the server
// refuses by reporting the violation and acknowledging nothing (sendStored).
func (r refusalReads) Publish(subject string, data []byte, opts ...nats.PubOpt) (*nats.PubAck, error) {
	return refusedVia(r, []string{subject}, func() (*nats.PubAck, error) {
		return r.JetStreamContext.Publish(subject, data, opts...)
	})
}

// KeyValue is the lookup of a bucket: one read of its stream (nats.go@v1.53.1 kv.go:396-411,
// js.go:189-192).
func (r refusalReads) KeyValue(bucket string) (nats.KeyValue, error) {
	return refusedVia(r, []string{jsAPI("STREAM.INFO", "KV_"+bucket)}, func() (nats.KeyValue, error) {
		return r.JetStreamContext.KeyValue(bucket)
	})
}

// CreateKeyValue is the create of a bucket, which asks the account for its JetStream info first
// and then creates the stream, and on "stream name already in use" reads the stream and sometimes
// updates it (nats.go@v1.53.1 kv.go:432-434, :525, :534-545), in that order, so the refusal a step
// ends on is the one of the request it stopped at.
func (r refusalReads) CreateKeyValue(cfg *nats.KeyValueConfig) (nats.KeyValue, error) {
	stream := "KV_" + cfg.Bucket
	subjects := []string{jsAPI("INFO"), jsAPI("STREAM.CREATE", stream), jsAPI("STREAM.INFO", stream), jsAPI("STREAM.UPDATE", stream)}
	return refusedVia(r, subjects, func() (nats.KeyValue, error) {
		return r.JetStreamContext.CreateKeyValue(cfg)
	})
}
