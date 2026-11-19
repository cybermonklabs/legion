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

// readsRefusals is js with the JetStream API requests this package sends through it that the bus
// makes once in a while (the methods below) sent through refusedBy, each under the subjects that
// request publishes to, so a request the connection's grant refuses ends at once with the server's
// refusal (ErrPublishDenied) rather than at the end of its own wait as a request nothing answered:
// the requests of a start, a raise or a repair, a durable consumer's create and delete (the
// listener's start, Dispatch's take-over of a held durable), and a health check's read of a
// consumer. It is the one path for those: the client's own JetStream context is one (install,
// ensureConnWithContext), and EnsureKeyValue, SetKeyValueMaxAge, OpenKeyValue, the stream's ensure
// and the stream's subject read take one over the context their caller hands them. A context
// already wrapped is returned as it is. Every other method is js's own: ConsumerNames, since its
// list reports no error a refusal could answer; Publish and the asynchronous publishes, which the
// bus makes for every message it stores and whose refusal they read off the same record
// themselves, after the publish (sendStored) or as the refusal arrives (window.acknowledged); and
// GetLastMsg, a read of one key that GetFromLeader reads the record for after the read
// (refusedAfter). On a connection no client owns, every method sends as js does.
func readsRefusals(conn *nats.Conn, js nats.JetStreamContext) nats.JetStreamContext {
	if _, ok := js.(refusalReads); ok {
		return js
	}
	return refusalReads{JetStreamContext: js, owner: clientOwning(conn)}
}

// readsOwnRefusals is readsRefusals with c named as the client whose record is read, for c's own
// JetStream context and its stream ensure: the constructor ensures the stream on the connection it
// is installing before c owns it (install), when clientOwning cannot find c yet.
func (c *Client) readsOwnRefusals(js nats.JetStreamContext) nats.JetStreamContext {
	if wrapped, ok := js.(refusalReads); ok {
		js = wrapped.JetStreamContext
	}
	return refusalReads{JetStreamContext: js, owner: c}
}

// refusalReads is a JetStream context whose requests read the record of owner, the client that
// owns its connection, found once when it is built; nil when no client owns it.
type refusalReads struct {
	nats.JetStreamContext
	owner *Client
}

// refusedVia is refusedBy through r's owner.
func refusedVia[T any](r refusalReads, subjects []string, send func() (T, error)) (T, error) {
	return refusedBy(r.owner, subjects, send)
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

// consumerCreateSubjects are the subjects a create or an update of the consumer cfg names, on
// stream, can go to: nats.go picks one of three by the server's version and the consumer's filter
// (nats.go@v1.53.1 jsm.go:483-533), so all three are read.
func consumerCreateSubjects(stream string, cfg *nats.ConsumerConfig) []string {
	name := cmp.Or(cfg.Name, cfg.Durable)
	subjects := []string{jsAPI("CONSUMER.CREATE", stream, name), jsAPI("CONSUMER.DURABLE.CREATE", stream, name)}
	if cfg.FilterSubject != "" {
		subjects = append(subjects, jsAPI("CONSUMER.CREATE", stream, name, cfg.FilterSubject))
	}
	return subjects
}

// UpdateConsumer's request goes to one of the consumer's create subjects
// (consumerCreateSubjects).
func (r refusalReads) UpdateConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	return refusedVia(r, consumerCreateSubjects(stream, cfg), func() (*nats.ConsumerInfo, error) {
		return r.JetStreamContext.UpdateConsumer(stream, cfg, opts...)
	})
}

// AddConsumer of a named consumer reads the consumer first and then creates it on one of its
// create subjects (nats.go@v1.53.1 jsm.go:447-473, consumerCreateSubjects), in that order, so the
// refusal it ends on is the one of the request it stopped at. An unnamed consumer, an ephemeral
// one, is sent as js sends it: no start or repair makes one through this context.
func (r refusalReads) AddConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	if cfg == nil || cmp.Or(cfg.Name, cfg.Durable) == "" {
		return r.JetStreamContext.AddConsumer(stream, cfg, opts...)
	}
	subjects := append([]string{jsAPI("CONSUMER.INFO", stream, cmp.Or(cfg.Name, cfg.Durable))}, consumerCreateSubjects(stream, cfg)...)
	return refusedVia(r, subjects, func() (*nats.ConsumerInfo, error) {
		return r.JetStreamContext.AddConsumer(stream, cfg, opts...)
	})
}

// DeleteConsumer is one request, the consumer's delete (nats.go@v1.53.1 jsm.go:599-615).
func (r refusalReads) DeleteConsumer(stream, consumer string, opts ...nats.JSOpt) error {
	_, err := refusedVia(r, []string{jsAPI("CONSUMER.DELETE", stream, consumer)}, func() (struct{}, error) {
		return struct{}{}, r.JetStreamContext.DeleteConsumer(stream, consumer, opts...)
	})
	return err
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
