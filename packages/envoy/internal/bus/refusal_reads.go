package bus

import (
	"cmp"
	"errors"
	"regexp"
	"strconv"

	"github.com/nats-io/nats.go"
)

// jsAPI is the subject a JetStream API request of the given kind and names is sent on, under the
// default API prefix every connection of this package uses: jsAPI("STREAM.INFO", "KV_x") is
// "$JS.API.STREAM.INFO.KV_x". The kinds are nats.go's own (nats.go@v1.55.0 js.go:145-225).
func jsAPI(kind string, names ...string) string {
	subject := "$JS.API." + kind
	for _, name := range names {
		subject += "." + name
	}
	return subject
}

// readsRefusals is js with the JetStream API requests this package sends through it that the bus
// makes once in a while (the methods below) sent through refusedBy, each under the subject that
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
// themselves, after the publish (sendStored) or as the refusal arrives (window.acknowledged);
// GetLastMsg, a read of one key that GetFromLeader reads the record for after the read
// (refusedAfter); and CreateKeyValue, whose requests EnsureKeyValue makes one at a time through
// the methods below (createBucketStream), since nats.go makes them in one call that no single
// subject names. On a connection no client owns, every method sends as js does.
func readsRefusals(conn *nats.Conn, js nats.JetStreamContext) nats.JetStreamContext {
	if _, ok := js.(refusalReads); ok {
		return js
	}
	return refusalReads{JetStreamContext: js, owner: clientOwning(conn), conn: conn}
}

// readsOwnRefusals is readsRefusals with c named as the client whose record is read, for c's own
// JetStream context on conn and its stream ensure: the constructor ensures the stream on the
// connection it is installing before c owns it (install), when clientOwning cannot find c yet.
func (c *Client) readsOwnRefusals(conn *nats.Conn, js nats.JetStreamContext) nats.JetStreamContext {
	if wrapped, ok := js.(refusalReads); ok {
		js = wrapped.JetStreamContext
	}
	return refusalReads{JetStreamContext: js, owner: c, conn: conn}
}

// refusalReads is a JetStream context whose requests read the record of owner, the client that
// owns its connection, found once when it is built; nil when no client owns it. conn is that
// connection, whose server's version decides which subject a consumer's create is sent to
// (consumerCreateSubject).
type refusalReads struct {
	nats.JetStreamContext
	owner *Client
	conn  *nats.Conn
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

// consumerCreateSubject is the one subject a create or an update of the named consumer cfg, on
// stream, is sent to, chosen as nats.go chooses it as it sends (nats.go@v1.55.0 jsm.go:505-534):
// when serverVersion, what the connection reports of its server, is 2.9.0 or later
// (natsServerMinVersion), the consumer's create, with the filter subject in it when cfg has one
// other than ">"; on an older server, the durable create for a durable and the legacy create of an
// unnamed consumer otherwise. Only that one subject is listed: a create takes the refusal of any
// subject it lists, and a refusal of a subject it never sends is another request's to answer.
// nats.go also sends a durable's create to the durable create on a newer server when its context
// carries UseLegacyDurableConsumers, which no context of this package does.
//
// nats.go reads the version off the connection just before it sends the create (jsm.go:511), and
// the callers read it just before they hand the create to nats.go, with no request between the
// two reads (UpdateConsumer, and AddConsumer's create, which follows its read of the consumer). A
// connection that is not CONNECTED reports no version (ConnectedServerVersion answers "",
// nats.go:2828-2830), which nats.go reads as a server older than 2.9.0, and so does this. While
// the connection reconnects nothing is sent: a bus connection buffers nothing then
// (ReconnectBufSize -1, internal/bus Connect), so nats.go fails the create at once with
// nats.ErrReconnectBufExceeded and there is no refusal to read
// (TestACreateBeforeTheFirstConnectEndsAtOnceWithItsOwnError). Nor is anything sent once a drain
// has reached the publishes, or the connection is closed (nats.go:4738-4746). While a drain
// unsubscribes, a publish still goes out, to the subject nats.go picks for an older server, which
// this picks too. So the two reads choose differently only when the connection's state changes in
// the moment between them: a first connect, a drain that starts, or a reconnect to a server of
// another version. The create then goes to a subject this did not list, its refusal is not named,
// and it ends at its own wait with its own error. The version is read before the send, not when
// the record is read: the record is read when any refusal arrives or once the send's own wait has
// ended, up to RequestTimeout after the send, and the connection's state could change over all of
// that.
func consumerCreateSubject(serverVersion, stream string, cfg *nats.ConsumerConfig) string {
	name := cmp.Or(cfg.Name, cfg.Durable)
	if !natsServerMinVersion(serverVersion, 2, 9, 0) {
		if cfg.Durable != "" {
			return jsAPI("CONSUMER.DURABLE.CREATE", stream, name)
		}
		return jsAPI("CONSUMER.CREATE", stream)
	}
	if cfg.FilterSubject == "" || cfg.FilterSubject == ">" {
		return jsAPI("CONSUMER.CREATE", stream, name)
	}
	return jsAPI("CONSUMER.CREATE", stream, name, cfg.FilterSubject)
}

// natsVersionPattern, natsVersionComponents and natsServerMinVersion copy nats.go's semVerRe,
// versionComponents and serverMinVersion (nats.go@v1.55.0 nats.go:2788-2817), which it does not
// export, by which nats.go picks a consumer create's subject (consumerCreateSubject) and a bucket
// stream's discard policy (bucketStreamConfig). A version with no number at its start, the empty
// one among them, reads as 0.0.0, and one that misses its minor or its patch as -1.-1.-1: each is
// older than any minimum.
var natsVersionPattern = regexp.MustCompile(`\Av?([0-9]+)\.?([0-9]+)?\.?([0-9]+)?`)

func natsVersionComponents(version string) (major, minor, patch int) {
	match := natsVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return 0, 0, 0
	}
	var components [3]int
	for index := range components {
		number, err := strconv.Atoi(match[index+1])
		if err != nil {
			return -1, -1, -1
		}
		components[index] = number
	}
	return components[0], components[1], components[2]
}

func natsServerMinVersion(version string, major, minor, patch int) bool {
	gotMajor, gotMinor, gotPatch := natsVersionComponents(version)
	return !(gotMajor < major || (gotMajor == major && gotMinor < minor) || (gotMajor == major && gotMinor == minor && gotPatch < patch))
}

// UpdateConsumer's request goes to the consumer's create subject (consumerCreateSubject). A
// consumer with no config or no name is sent as js sends it: nats.go refuses it before it sends
// anything (nats.go@v1.55.0 jsm.go:474-486).
func (r refusalReads) UpdateConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	if cfg == nil || cmp.Or(cfg.Name, cfg.Durable) == "" {
		return r.JetStreamContext.UpdateConsumer(stream, cfg, opts...)
	}
	return refusedVia(r, []string{consumerCreateSubject(r.conn.ConnectedServerVersion(), stream, cfg)}, func() (*nats.ConsumerInfo, error) {
		return r.JetStreamContext.UpdateConsumer(stream, cfg, opts...)
	})
}

// AddConsumer of a named consumer is nats.go's AddConsumer made one request at a time
// (nats.go@v1.55.0 jsm.go:447-471), each under its own subject: the read of the consumer
// (ConsumerInfo), and then, when the read answers that the consumer or its stream is not there,
// the create, the request nats.go's AddConsumer sends then, on the create subject
// (UpdateConsumer, consumerCreateSubject). So a refusal of the create does not answer the call
// while its read is still in flight, nor once the read has found the consumer and no create is
// sent. A found consumer is handed to js's own AddConsumer, under the read's subject alone: nats.go
// compares the found configuration with cfg by a check it does not export, and that call reads the
// consumer again first. A consumer deleted between the two reads is created by that call, and a
// refusal of that create is then not named: the create ends at its own wait. An unnamed consumer,
// an ephemeral one, is sent as js sends it: no start or repair makes one through this context.
func (r refusalReads) AddConsumer(stream string, cfg *nats.ConsumerConfig, opts ...nats.JSOpt) (*nats.ConsumerInfo, error) {
	if cfg == nil || cmp.Or(cfg.Name, cfg.Durable) == "" {
		return r.JetStreamContext.AddConsumer(stream, cfg, opts...)
	}
	read := jsAPI("CONSUMER.INFO", stream, cmp.Or(cfg.Name, cfg.Durable))
	_, err := r.ConsumerInfo(stream, cmp.Or(cfg.Name, cfg.Durable), opts...)
	switch {
	case errors.Is(err, nats.ErrConsumerNotFound) || errors.Is(err, nats.ErrStreamNotFound):
		return r.UpdateConsumer(stream, cfg, opts...)
	case err != nil:
		return nil, err
	}
	return refusedVia(r, []string{read}, func() (*nats.ConsumerInfo, error) {
		return r.JetStreamContext.AddConsumer(stream, cfg, opts...)
	})
}

// DeleteConsumer is one request, the consumer's delete (nats.go@v1.55.0 jsm.go:599-630).
func (r refusalReads) DeleteConsumer(stream, consumer string, opts ...nats.JSOpt) error {
	_, err := refusedVia(r, []string{jsAPI("CONSUMER.DELETE", stream, consumer)}, func() (struct{}, error) {
		return struct{}{}, r.JetStreamContext.DeleteConsumer(stream, consumer, opts...)
	})
	return err
}

// KeyValue is the lookup of a bucket: one read of its stream (nats.go@v1.55.0 kv.go:397-418,
// js.go:193-199).
func (r refusalReads) KeyValue(bucket string) (nats.KeyValue, error) {
	return refusedVia(r, []string{jsAPI("STREAM.INFO", "KV_"+bucket)}, func() (nats.KeyValue, error) {
		return r.JetStreamContext.KeyValue(bucket)
	})
}

// AccountInfo is one request, the account's JetStream info (nats.go@v1.55.0 jsm.go:400-430): the
// first request of a bucket's create (createBucketStream).
func (r refusalReads) AccountInfo(opts ...nats.JSOpt) (*nats.AccountInfo, error) {
	return refusedVia(r, []string{jsAPI("INFO")}, func() (*nats.AccountInfo, error) {
		return r.JetStreamContext.AccountInfo(opts...)
	})
}
