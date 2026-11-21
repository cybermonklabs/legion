// Package natsgrant is the one table of NATS grants Envoy's clients hold: the sets a hub renders
// into its static authorization (packages/envoy/infra/core wraps each as the rules a User takes) and
// the grants the auth service answers a connection with (internal/cipher/natscallout). Both read
// the same functions, so a hub's static user and a connection the auth service admits for the same
// kind hold the same lists, built from the names internal/bus and internal/contracts export.
//
// Each call returns lists of its own, so a caller may narrow a grant without changing another's.
package natsgrant

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Permissions are one NATS user's publish and subscribe allow lists, and the deny lists nats-server
// applies after them: a subject an allow entry admits and a deny entry names is refused. A deny
// list narrows a direction and never opens one: a direction whose allow list is empty is every
// subject however many deny entries stand beside it, which OpenDirection refuses. A subscribe entry
// may carry a queue group after a space, as nats-server's configuration writes one.
type Permissions struct {
	Publish, Subscribe         []string
	PublishDeny, SubscribeDeny []string
}

// The subject spaces of NATS itself that nats.go sends JetStream and KV calls in. nats.go exports
// no name for them.
const (
	jsAPI      = "$JS.API." // the JetStream API
	jsAck      = "$JS.ACK." // a consumer's acknowledgements
	jsFlow     = "$JS.FC."  // an ordered consumer's flow-control replies
	kvStream   = "KV_"      // the stream behind a KV bucket, KV_<bucket>
	kvSubjects = "$KV."     // a KV bucket's keys, $KV.<bucket>.<key>
)

// inboxes matches every reply inbox nats.go makes.
const inboxes = nats.InboxPrefix + ">"

// listenerGrant is the grant every user that runs envoy-listener holds as machine machineID, built
// from Envoy's own names: Listener adds the session consumers' calls to it (sessionConsumerCalls),
// ListenerHost those and the subscribe to every notification topic, Dispatch what Dispatch itself
// needs, and Host the session's entries. Publish, in this order:
//   - every notification topic (bus.NotificationTopicPrefix): the stream's subjects, the role
//     lanes, every exception lane, the session relay subjects (bus.DeliverSubjectPrefix), and any
//     other topic the listener publishes over core NATS, a role lane's forward among them
//     (/v1/messages/publish refuses by name a notifications. topic no stream subject captures);
//   - the stream's info, create and update, its stored-message get, its durable's
//     (bus.ListenerDurable) info and create, the durable's acknowledgements, and JetStream's
//     account info. The stored-message get reads one stored message at a time and can create,
//     change or delete nothing; SessionPattern does not hold it, so no session reads the stream's
//     stored messages itself and each takes its own from the consumer its listener relays;
//   - for each bucket of bus.ListenerBuckets in turn (the buckets the listener's keeper raises),
//     the calls nats.go's KV makes and the raise a start sends (KVWrite): the bucket's stream
//     info, create and update, direct and stream message gets, writes to its keys, its watchers'
//     consumer create and delete, and their flow-control replies;
//   - the purge of the interest bucket, which its delete-marker collection sends.
//
// Subscribe: reply inboxes, then the role lanes in the machine's queue group (bus.ListenerQueue),
// which a listener of an earlier release subscribes in, and in the queue group every listener
// shares (bus.RoleLaneQueue). Every user built on this grant runs a listener that subscribes a
// role lane, Dispatch's user included (the envoy-listener beside envoy-dispatch), so each holds
// both groups.
func listenerGrant(machineID string) Permissions {
	durable := bus.ListenerDurable(machineID)
	publish := []string{
		bus.NotificationTopicPrefix + ">",
		jsAPI + "STREAM.INFO." + bus.Stream,
		jsAPI + "STREAM.CREATE." + bus.Stream,
		jsAPI + "STREAM.UPDATE." + bus.Stream,
		jsAPI + "STREAM.MSG.GET." + bus.Stream,
		jsAPI + "CONSUMER.INFO." + bus.Stream + "." + durable,
		jsAPI + "CONSUMER.CREATE." + bus.Stream + "." + durable,
		jsAck + bus.Stream + "." + durable + ".>",
		jsAPI + "INFO",
	}
	// Each bucket named here is granted to every user built on this grant: the listener's own,
	// the host user a host's sessions share (Host) and Dispatch's. What that lets a session on a
	// host write, and where it is narrowed, is the README's to say
	// (packages/envoy/infra/README.md, "Permission sets").
	for _, bucket := range bus.ListenerBuckets() {
		publish = append(publish, KVWrite(bucket)...)
	}
	publish = append(publish, jsAPI+"STREAM.PURGE."+kvStream+bus.InterestBucket)
	return Permissions{
		Publish: publish,
		Subscribe: []string{
			inboxes,
			contracts.RoleTopicPrefix + "> " + bus.ListenerQueue(machineID),
			contracts.RoleTopicPrefix + "> " + bus.RoleLaneQueue,
		},
	}
}

// sessionConsumerCalls are the calls bus.SessionConsumers makes, which only the sets of a user no
// session shares hold (Listener and ListenerHost): consumer create, with and without a filter
// subject in its subject, info, delete, pull (CONSUMER.MSG.NEXT) and acknowledgements, each on any
// consumer of the stream, since a session consumer is named for its session
// (bus.SessionConsumerName), and the stream's consumer list, which a listener that starts relaying
// every session at once reads instead of one info each
// (bus.SessionConsumers.ListSessionConsumers). A NATS wildcard is a whole token, so these reach
// every consumer of the stream, other listeners' durables and Dispatch's included, and a consumer
// created through them can deliver the stream's messages to any subject. That reach is the
// listener user's alone: Dispatch and Host keep their per-durable scope and hold none of these, so
// the listener those users run refuses every registration that asks for consumer delivery 503
// no_grant (the consumer call returns bus.ErrConsumerNotGranted), writes nothing of the session it
// names, and keeps only its own durable.
func sessionConsumerCalls() []string {
	return []string{
		jsAPI + "CONSUMER.CREATE." + bus.Stream + ".*",
		jsAPI + "CONSUMER.CREATE." + bus.Stream + ".*.>",
		jsAPI + "CONSUMER.INFO." + bus.Stream + ".*",
		jsAPI + "CONSUMER.DELETE." + bus.Stream + ".*",
		jsAPI + "CONSUMER.MSG.NEXT." + bus.Stream + ".*",
		jsAPI + "CONSUMER.LIST." + bus.Stream,
		jsAck + bus.Stream + ".*.>",
	}
}

// Listener is the grant envoy-listener needs as machine machineID: the listener grant
// (listenerGrant), then the session consumers' calls (sessionConsumerCalls), which only this set
// and ListenerHost hold. A hub that renders this set for its own listener gives that listener
// consumer delivery; a listener connecting as a host or Dispatch user does not have it, and has no
// session of its own registered at all: every registration it cannot give a consumer is refused by
// name and writes nothing.
func Listener(machineID string) Permissions {
	grant := listenerGrant(machineID)
	grant.Publish = append(grant.Publish, sessionConsumerCalls()...)
	return grant
}

// ListenerHost is the grant of a listener's own NATS user, one no session shares, as machine
// machineID: the listener's set (Listener), the session consumers' calls included, then a
// subscribe to every notification topic (bus.NotificationTopicPrefix) that nothing now uses. It
// existed for the opt-in that had a listener hold a core subscription on notifications.> for the
// sessions it served, and that opt-in went with the listener's push to a session's serving port,
// so this set grants the listener's set and the one subscribe nothing reads. It keeps the
// subscribe rather than dropping it, and so stays a set of its own rather than an alias, because a
// change to a set rewrites every hub's rendered authorization: it replaces a VM hub's instance and
// reaches a clustered hub only as a live reload, which is left to later work. The set holds none
// of the session's publish entries, so a process connecting as this user can neither publish a
// session's agent stream frames nor subscribe to a session's control subject. Host stays the set
// of a user a host's sessions share.
func ListenerHost(machineID string) Permissions {
	grant := Listener(machineID)
	grant.Subscribe = append(grant.Subscribe, bus.NotificationTopicPrefix+">")
	return grant
}

// KVWrite are the calls nats.go's KV makes on one bucket, in the order the grants list them:
// listenerGrant lists them for each of bus.ListenerBuckets, Dispatch for the redelivery bucket.
// Beside them is the update of the bucket's stream: a start, and the program's keeper every 30
// seconds after it, raises a bucket found with fewer copies than the deployment's replica count
// (bus.ReplicasEnvVar) with a stream update (bus.EnsureKeyValue, bus.WithKeptBuckets). Without
// that grant the server refuses the raise and the start stops naming the bucket, the setting and
// the refusal.
func KVWrite(bucket string) []string {
	stream := kvStream + bucket
	return []string{
		jsAPI + "STREAM.INFO." + stream,
		jsAPI + "STREAM.CREATE." + stream,
		jsAPI + "STREAM.UPDATE." + stream,
		jsAPI + "DIRECT.GET." + stream + ".>",
		jsAPI + "STREAM.MSG.GET." + stream,
		kvSubjects + bucket + ".>",
		jsAPI + "CONSUMER.CREATE." + stream + ".>",
		jsAPI + "CONSUMER.DELETE." + stream + ".>",
		jsFlow + stream + ".>",
	}
}

// KVRead are the calls a reader of one bucket makes and nothing that writes it: the bucket's
// stream info, direct and stream message gets, its watchers' consumer create and delete, and their
// flow-control replies. It holds no write to a key ($KV.<bucket>.>) and no create or update of
// the bucket's stream, so a holder can neither change an entry nor make the bucket.
func KVRead(bucket string) []string {
	stream := kvStream + bucket
	return []string{
		jsAPI + "STREAM.INFO." + stream,
		jsAPI + "DIRECT.GET." + stream + ".>",
		jsAPI + "STREAM.MSG.GET." + stream,
		jsAPI + "CONSUMER.CREATE." + stream + ".>",
		jsAPI + "CONSUMER.DELETE." + stream + ".>",
		jsFlow + stream + ".>",
	}
}

// DispatchDurables are the fixed durable consumers of the stream that Dispatch's grant lets it
// bind, beside the listener durable of its machine id. Two users holding Dispatch would bind the
// same ones.
func DispatchDurables() []string {
	return []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable}
}

// Dispatch is the grant of Dispatch's one NATS user, which envoy-dispatch and the envoy-listener
// beside it share: the listener grant (listenerGrant) of the listener's machine id, then what
// Dispatch itself needs. It holds no session-consumer call (sessionConsumerCalls), so the listener
// beside envoy-dispatch keeps its own durable alone and refuses every registration that asks for
// consumer delivery 503 no_grant (bus.ErrConsumerNotGranted), writing nothing of the session it
// names. No session connects as this user either, so it holds none of the session's grant: it is
// a direct publish to a session's frames, or a subscription to its control subject, that is
// refused. The user's reach is the whole server even so: its _INBOX.> subscription receives every
// reply any client gets, other listeners' durable push deliveries included; the filtered create on
// delivery-events lets it create that durable on any filter, so it can read any retained
// notification; and a consumer it creates can deliver any payload onto a session's frames, since
// NATS checks no push consumer's deliver subject. Publish, in this order: the stream calls and
// acknowledgements of each durable Dispatch binds, the KV calls for its webhook-redelivery bucket,
// and every session's agent stream control subject. Subscribe: every session's agent stream
// frames.
func Dispatch(machineID string) Permissions {
	listener := listenerGrant(machineID)
	publish := listener.Publish
	for _, durable := range DispatchDurables() {
		publish = append(publish,
			jsAPI+"CONSUMER.INFO."+bus.Stream+"."+durable,
			jsAPI+"CONSUMER.CREATE."+bus.Stream+"."+durable+".>",
			jsAPI+"CONSUMER.DELETE."+bus.Stream+"."+durable,
			jsAck+bus.Stream+"."+durable+".>",
		)
	}
	publish = append(publish, KVWrite(bus.WebhookRedeliveryBucket)...)
	publish = append(publish, contracts.AgentStreamControlSubject("*"))
	return Permissions{
		Publish:   publish,
		Subscribe: append(listener.Subscribe, contracts.AgentStreamFramesSubject("*")),
	}
}

// session is the grant of the session sessionID names, which SessionPattern fills with * and
// Session with one session's id. Publish: replies (the receipt of each relay frame, the answer to
// the listener's connection probe and to Dispatch's agent stream replay), on the subjects replies
// names, then the session's agent stream frames (contracts.AgentStreamFramesSubject). Subscribe:
// its relay subject (bus.DeliverSubject), the subject the listener that owns the session publishes
// every message of the session's consumer (bus.SessionConsumerName) on, then its agent stream's
// control subject (contracts.AgentStreamControlSubject), and no other subject.
func session(sessionID, replies string) Permissions {
	return Permissions{
		Publish: []string{replies, contracts.AgentStreamFramesSubject(sessionID)},
		Subscribe: []string{
			bus.DeliverSubject(sessionID),
			contracts.AgentStreamControlSubject(sessionID),
		},
	}
}

// SessionPattern is the grant any agent session needs, with * for the session id: the user a hub
// renders for the sessions that share it. A session subscribes to no notification topic that
// carries a message: it receives what its consumer delivers, on the relay subject, and nothing
// else. The listener's connection probe (cmd/listener) asks on that same relay subject and is
// answered on a reply subject under _INBOX. that names the session (bus.SessionReplySubject),
// which this set's _INBOX.> publish admits, so it needs no rule of its own; the README names the
// entries that carry it (packages/envoy/infra/README.md, "Permission sets").
//
// A Legion controller session needs nothing added for its project's notice lane
// (notifications.legion.<project>.controller, the daemon's notify.ControllerTopic): it registers
// that lane as an interest while it holds the project's controller role
// (bus.ReachesPrivateStreamSubject), and the listener's relay hands it the notices on the relay
// subject, as it hands it every other message. No session subscribes to the lane itself, which
// this set rightly refuses.
//
// What this set does not solve: every session on a shared user holds the same set, so the relay
// subject carries * and one session can read another's relay frames and answer them. A session the
// auth service admits holds Session instead, which names its own id alone.
func SessionPattern() Permissions {
	return session("*", inboxes)
}

// Session is the grant of one session, sessionID: SessionPattern's entries with that id in place
// of *, so the session subscribes its own relay subject and control subject and publishes its own
// frames, and no other session's. It publishes replies under its own id alone
// (bus.SessionReplies, _INBOX.*.<session id>.>), where SessionPattern publishes to every _INBOX.
// subject: every request a session answers - each relay frame, the listener's connection probe,
// Dispatch's agent stream replay - asks on a reply subject naming the session it asks
// (bus.SessionReplySubject), so the session answers all of them and can answer no other session's
// request, nor publish into a requester's own inbox. An id that is not one
// subject token (SessionToken) is refused naming it, because a dot, a wildcard or a space in it
// would reach other sessions' subjects.
func Session(sessionID string) (Permissions, error) {
	if !SessionToken(sessionID) {
		return Permissions{}, fmt.Errorf("session id %q is not one subject token", sessionID)
	}
	return session(sessionID, bus.SessionReplies(sessionID)), nil
}

// SessionToken reports whether id is one NATS subject token that names nothing but itself:
// non-empty, and holding no '.', '*', '>', whitespace or control character.
func SessionToken(id string) bool {
	if id == "" {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool {
		return r == '.' || r == '*' || r == '>' || unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// Daemon is the NATS grant of one Legion daemon. Its three durable consumers inspect and create
// consumers in Envoy's notification stream, fetch their messages, and acknowledge them; the
// consumer subjects end in `>`, so the grant reaches every consumer of the stream, not only those
// three. Its state read gets stored messages of the stream one at a time (`STREAM.MSG.GET`): the
// oldest message each consumer has not acknowledged, whose time is the consumer's lag. The get
// only reads: it can create, change or delete nothing. It also receives role-lane delivery
// exceptions and JetStream API replies. Those replies arrive on `_INBOX.>`, so the daemon's user
// receives every reply any client on the server gets, key-value reads and watch updates included,
// such as the listener's envoy_roles and envoy_sessions watchers.
func Daemon() Permissions {
	return Permissions{
		Publish: []string{
			jsAPI + "STREAM.INFO." + bus.Stream,
			jsAPI + "STREAM.MSG.GET." + bus.Stream,
			jsAPI + "CONSUMER.INFO." + bus.Stream + ".>",
			jsAPI + "CONSUMER.CREATE." + bus.Stream + ".>",
			jsAPI + "CONSUMER.MSG.NEXT." + bus.Stream + ".>",
			jsAck + bus.Stream + ".>",
		},
		Subscribe: []string{
			bus.ExceptionTopicPrefix + contracts.RoleTopicPrefix + ">",
			inboxes,
		},
	}
}

// Host is the grant of a host's one NATS user, which its listener and every session on the host
// share: in each direction the listener grant (listenerGrant), then each entry of the session's it
// lacks, and in the subscribe direction the plain notifications.> entry this set names itself. It
// holds no session-consumer call (sessionConsumerCalls), so a listener connecting as a host user
// keeps its own durable alone and can give no session a consumer. A listener whose user no session
// shares takes ListenerHost instead, which holds those calls beside the same notifications.>
// subscribe.
//
// That subscribe is this set's own, not one it takes from the session's: SessionPattern holds it
// no longer. It is the reach the sessions sharing a host user already have - one user is one
// grant, so each of them may subscribe to every notification topic. No Envoy program reads it: the
// listener holds no core subscription on notifications.> any more, and a start that still sets
// ENVOY_SUBSCRIBE_ALL_TOPICS is warned about by name and otherwise ignored (cmd/listener/main.go's
// warnIgnoredEnv). Dropping it would rewrite every deployed host user, and a hub's users are
// rendered into the VM's cloud-config, which changes only by replacing the VM, so it stays until
// sessions connect through the auth service and take Session instead.
func Host(machineID string) Permissions {
	listener, agent := listenerGrant(machineID), SessionPattern()
	// notifications.> admits every notification subject the session's set names, its relay subject
	// among them, so only the session entries outside that family are added: a second entry for
	// subjects already allowed would re-render every deployed host user, and a hub's users are
	// rendered into the VM's cloud-config, which changes only by replacing the VM.
	subscribe := union(listener.Subscribe, []string{bus.NotificationTopicPrefix + ">"})
	outside := slices.DeleteFunc(agent.Subscribe, func(entry string) bool {
		return strings.HasPrefix(entry, bus.NotificationTopicPrefix)
	})
	return Permissions{
		Publish:   union(listener.Publish, agent.Publish),
		Subscribe: union(subscribe, outside),
	}
}

// Measure is the grant of Envoy's measurement job, which the deployment repository deploys as a
// static user of both hubs (envoy-cluster/hub.go, hubMeasurePublish and hubMeasureSubscribe). It
// reads the account's JetStream state, the listener's lease keys in KV_envoy_listener, and owns one
// test stream, ENVOY_MEASURE, on subjects of its own. It subscribes only its own inbox prefix,
// _INBOX_envoy_measure., never _INBOX.>, where the listener's and Dispatch's push consumer
// deliveries and request replies arrive, so it reads no message of another client.
func Measure() Permissions {
	return Permissions{
		Publish: []string{
			jsAPI + "INFO",
			jsAPI + "STREAM.NAMES",
			jsAPI + "STREAM.LIST",
			jsAPI + "STREAM.INFO.*",
			jsAPI + "CONSUMER.NAMES.*",
			jsAPI + "CONSUMER.LIST.*",
			jsAPI + "CONSUMER.INFO.*.*",
			jsAPI + "DIRECT.GET." + kvStream + "envoy_listener." + kvSubjects + "envoy_listener.lease.>",
			jsAPI + "STREAM.CREATE.ENVOY_MEASURE",
			jsAPI + "STREAM.UPDATE.ENVOY_MEASURE",
			jsAPI + "STREAM.DELETE.ENVOY_MEASURE",
			"envoy-measure.>",
		},
		Subscribe: []string{"_INBOX_envoy_measure.>"},
	}
}

// systemAccountPrefix is the first token of every subject of nats-server's own, the auth
// service's authorization requests ($SYS.REQ.USER.AUTH) among them.
const systemAccountPrefix = "$SYS"

// NamesSystemSubject returns the first entry of p, publish then subscribe, that can match a
// system subject: one whose first token is $SYS, or a wildcard that a $SYS subject's first token
// matches (* or >). A subscribe entry's queue group is not part of its subject, and the entry is
// split as nats-server splits it (server/client.go, splitSubjectQueue: strings.Fields), at any
// whitespace, so a tab or a no-break space before a queue group hides no subject from this.
// The auth service refuses any answer whose grant names one: the server takes from a callout
// client only the publish of $SYS.REQ.USER.AUTH, so a grant naming a system subject would hand a
// connection a subscribe the server does not take away.
func NamesSystemSubject(p Permissions) (string, bool) {
	for _, entries := range [...][]string{p.Publish, p.Subscribe} {
		for _, entry := range entries {
			fields := strings.Fields(entry)
			if len(fields) == 0 {
				continue
			}
			first, _, _ := strings.Cut(fields[0], ".")
			if first == systemAccountPrefix || first == "*" || first == ">" {
				return entry, true
			}
		}
	}
	return "", false
}

// OpenDirection returns the first direction of p, "publish" then "subscribe", whose allow list is
// empty or holds a blank entry, and true. An empty allow list is not "nothing": nats-io/jwt/v2
// drops it when it encodes a user (types.go, omitempty), and nats-server then applies no allow
// list to that direction (server/accounts.go, client.go), so a user answered with one may publish,
// or subscribe, to every subject but the ones a deny list names. A deny list beside it narrows
// that and closes nothing, so an empty allow list is open whatever PublishDeny or SubscribeDeny
// hold, and the auth service refuses every grant this names (internal/cipher/natscallout).
// A blank entry is the same fault: jwt skips an empty entry when it adds it to a user
// (StringList.Add skips ""), so a list of "" encodes as an empty one, and a whitespace entry names
// no subject any grant of this package means.
func OpenDirection(p Permissions) (string, bool) {
	blank := func(entry string) bool { return strings.TrimSpace(entry) == "" }
	switch {
	case len(p.Publish) == 0 || slices.ContainsFunc(p.Publish, blank):
		return "publish", true
	case len(p.Subscribe) == 0 || slices.ContainsFunc(p.Subscribe, blank):
		return "subscribe", true
	}
	return "", false
}

// union is a followed by each entry of b it does not hold yet.
func union(a, b []string) []string {
	for _, entry := range b {
		if !slices.Contains(a, entry) {
			a = append(a, entry)
		}
	}
	return a
}
