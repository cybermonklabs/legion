// Package daemonpermissions defines the NATS subjects a Legion daemon needs from Envoy.
package daemonpermissions

import (
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/legiontasks"
)

// Permissions is one NATS user's publish and subscribe allow lists.
type Permissions struct {
	Publish, Subscribe []string
}

// Grant returns the NATS grant for one Legion daemon. Its three durable consumers inspect and
// create consumers in Envoy's notification stream, fetch their messages, and acknowledge them; the
// consumer subjects end in `>`, so the grant reaches every consumer of the stream, not only those
// three. Its state read gets stored messages of the stream one at a time (`STREAM.MSG.GET`): the
// oldest message each consumer has not acknowledged, whose time is the consumer's lag. The get only
// reads: it can create, change or delete nothing. It also receives role-lane delivery exceptions
// and JetStream API replies. Those replies arrive on `_INBOX.>`, so the daemon's seed receives every
// reply any client on the server gets, key-value reads and watch updates included, such as the
// listener's envoy_roles and envoy_sessions watchers.
//
// After those come the calls the daemon makes on the task bucket (legiontasks.Bucket), whose one
// writer it is: JetStream's account info (`$JS.API.INFO`), which nats.go asks before it creates
// any bucket; the info, create and update of the bucket's stream (legiontasks.Stream), since the
// daemon creates the bucket when it is missing, reads the stream's creation time as the bucket's
// epoch, and, when another daemon created the bucket first, nats.go reads the stream and sends an
// update of the configuration it already holds; the stream's direct get and stored-message get,
// which read one key; the publish to its keys (`$KV.legion_tasks.>`), every write and delete of a
// value; and the calls of its watch of the whole bucket, an ordered consumer: the consumer's
// create (with the filter subject in its subject) and delete, which a watch's reset and stop
// send, and its flow-control replies (`$JS.FC.KV_legion_tasks.>`). The watch delivers on a
// subject nats.go makes under `_INBOX.`, which the reply subscribe above already admits, so the
// bucket adds no subscribe. Every key is a claim of some project, and the grant reaches every
// project's keys: a daemon writes and deletes only its own project's (legiontasks.OwnedBy).
//
// Each call owns its lists so a consumer can narrow a grant without changing another caller's.
func Grant() Permissions {
	return Permissions{
		Publish: []string{
			"$JS.API.STREAM.INFO." + bus.Stream,
			"$JS.API.STREAM.MSG.GET." + bus.Stream,
			"$JS.API.CONSUMER.INFO." + bus.Stream + ".>",
			"$JS.API.CONSUMER.CREATE." + bus.Stream + ".>",
			"$JS.API.CONSUMER.MSG.NEXT." + bus.Stream + ".>",
			"$JS.ACK." + bus.Stream + ".>",
			"$JS.API.INFO",
			"$JS.API.STREAM.INFO." + legiontasks.Stream,
			"$JS.API.STREAM.CREATE." + legiontasks.Stream,
			"$JS.API.STREAM.UPDATE." + legiontasks.Stream,
			"$JS.API.DIRECT.GET." + legiontasks.Stream + ".>",
			"$JS.API.STREAM.MSG.GET." + legiontasks.Stream,
			"$KV." + legiontasks.Bucket + ".>",
			"$JS.API.CONSUMER.CREATE." + legiontasks.Stream + ".>",
			"$JS.API.CONSUMER.DELETE." + legiontasks.Stream + ".>",
			"$JS.FC." + legiontasks.Stream + ".>",
		},
		Subscribe: []string{
			bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">",
			nats.InboxPrefix + ">",
		},
	}
}
