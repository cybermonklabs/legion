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
// Session with one session's id. Publish: reply inboxes (the receipt of each relay frame), then the
// session's agent stream frames (contracts.AgentStreamFramesSubject). Subscribe: its relay subject
// (bus.DeliverSubject), the subject the listener that owns the session publishes every message of
// the session's consumer (bus.SessionConsumerName) on, then its agent stream's control subject
// (contracts.AgentStreamControlSubject), and no other subject.
func session(sessionID string) Permissions {
	return Permissions{
		Publish: []string{inboxes, contracts.AgentStreamFramesSubject(sessionID)},
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
// answered on a reply inbox, so it needs no rule of its own; the README names the entries that
// carry it (packages/envoy/infra/README.md, "Permission sets").
//
// A Legion controller session needs nothing added for its project's notice lane
// (notifications.legion.<project>.controller, the daemon's notify.ControllerTopic): it registers
// that lane as an interest while it holds the project's controller role
// (bus.ReachesPrivateStreamSubject), and the listener's relay hands it the notices on the relay
// subject, as it hands it every other message. No session subscribes to the lane itself, which
// this set rightly refuses.
//
// What this set does not solve: every session on a shared user holds the same set, so the relay
// subject carries * and one session can read another's relay frames. A session the auth service
// admits holds Session instead, which names its own id alone.
func SessionPattern() Permissions {
	return session("*")
}

// Session is the grant of one session, sessionID: SessionPattern's entries with that id in place
// of *, so the session subscribes its own relay subject and control subject and publishes its own
// frames, and no other session's. An id that is not one subject token (SessionToken) is refused
// naming it, because a dot, a wildcard or a space in it would reach other sessions' subjects.
func Session(sessionID string) (Permissions, error) {
	if !SessionToken(sessionID) {
		return Permissions{}, fmt.Errorf("session id %q is not one subject token", sessionID)
	}
	return session(sessionID), nil
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
// reads the account's JetStream state and the listener's lease keys in KV_envoy_listener, and
// publishes on its own cutover inbox, _INBOX_envoy_measure.cutover.>, under which a probe sends a
// numbered sequence and reads it back to count what a restart of the hub's servers loses. It makes,
// changes and removes no stream: a create or an update carries a configuration of the caller's
// choosing, and a source or a republish in it would read every stream of the account. The same
// grant writes any KV bucket of the account: a RePublish whose destination is a KV subject
// (Source envoy-measure.>, Destination $KV.envoy_session_consumers.>) is accepted, since
// nats-server checks only that the destination does not loop into the stream itself (v2.15.1
// stream.go:2479-2514, checkStreamCfgLocked; measured on 2.15.1 and 2.10.29), so its publishes
// become chosen self-keyed records and the bucket's sentinel.
// The streams change (#4687) removes those calls from every
// callout answer but the held operator's (ForCallout) and narrows every static user on a callout
// hub (core.Authorization.Render), so after the cutover the held operator is their only holder;
// ENVOY_MEASURE is the operator's to make (envoy-infra streams update). It subscribes only its own
// inbox prefix, _INBOX_envoy_measure., never _INBOX.>, where the listener's and Dispatch's push
// consumer deliveries and request replies arrive, so it reads no message of another client.
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
			measureInbox + ".cutover.>",
		},
		Subscribe: []string{measureInbox + ".>"},
	}
}

// measureInbox is the measurement job's own inbox prefix.
const measureInbox = "_INBOX_envoy_measure"

// streamConfigurationCalls are the JetStream API calls that make a stream, change its
// configuration or remove it: a create, an update, a delete, and a restore, which makes the stream
// a snapshot carries with that snapshot's configuration.
var streamConfigurationCalls = []string{
	jsAPI + "STREAM.CREATE.>",
	jsAPI + "STREAM.UPDATE.>",
	jsAPI + "STREAM.DELETE.>",
	jsAPI + "STREAM.RESTORE.>",
}

// ForCallout is p as the auth service answers it: no publish entry can reach a stream create,
// update, delete or restore (streamConfigurationCalls), and every other subject p allows it still
// allows, in lists of its own. An entry those calls contain (a create of one stream, or of every
// stream) is dropped; an entry that reaches one of them and other subjects too (a wildcard such as
// $JS.API.> or >) stays, and each call it reaches is added to the publish deny list, which
// nats-server applies after the allow list. internal/cipher/natscallout answers every connection
// but the operator's through it, and infra/core renders every static user beside a callout through
// it, so no client but the operator can make, change or remove a stream: a create or an update
// carries a configuration of the caller's choosing, and a source or a republish in it reads every
// stream of the account, since nats-server checks a republish destination only against the
// stream's own subjects. Every stream is made at deploy time by the operator instead (envoy-infra
// streams), and a start beside the auth service reads its streams and makes none
// (bus.StreamsOperatorMadeEnvVar).
//
// A bucket's writer (KVWrite) keeps its stream info, its direct and stream gets, its key writes,
// its watchers' consumer calls and their flow-control replies; the listener's grant keeps the
// notification stream's info and stored-message get. Measure holds no stream call to remove. No
// grant of this package holds a wildcard that reaches one of the calls, so none of them is answered
// with a deny. The static sets stay as they are: a hub that runs no auth service renders them, and
// its programs make their own streams.
func ForCallout(p Permissions) Permissions {
	var publish []string
	if p.Publish != nil {
		publish = make([]string, 0, len(p.Publish))
	}
	deny := slices.Clone(p.PublishDeny)
	for _, entry := range p.Publish {
		if slices.ContainsFunc(streamConfigurationCalls, func(call string) bool { return subjectWithin(entry, call) }) {
			continue
		}
		publish = append(publish, entry)
		for _, call := range streamConfigurationCalls {
			if subjectsCollide(entry, call) && !slices.Contains(deny, call) {
				deny = append(deny, call)
			}
		}
	}
	return Permissions{
		Publish:       publish,
		Subscribe:     slices.Clone(p.Subscribe),
		PublishDeny:   deny,
		SubscribeDeny: slices.Clone(p.SubscribeDeny),
	}
}

// subjectWithin reports whether every subject a matches, b matches too: a is b or narrower, both
// subjects or wildcards ("*" one token, ">" the rest).
func subjectWithin(a, b string) bool {
	aTokens, bTokens := strings.Split(a, "."), strings.Split(b, ".")
	for index, bToken := range bTokens {
		if bToken == ">" {
			return index < len(aTokens)
		}
		if index >= len(aTokens) || aTokens[index] == ">" {
			return false
		}
		if bToken != "*" && (aTokens[index] == "*" || aTokens[index] != bToken) {
			return false
		}
	}
	return len(aTokens) == len(bTokens)
}

// subjectsCollide reports whether one subject can match both a and b, two subjects or wildcards
// ("*" one token, ">" the rest), as nats-server's SubjectsCollide reads them.
func subjectsCollide(a, b string) bool {
	aTokens, bTokens := strings.Split(a, "."), strings.Split(b, ".")
	for index := 0; index < len(aTokens) && index < len(bTokens); index++ {
		aToken, bToken := aTokens[index], bTokens[index]
		if aToken == ">" || bToken == ">" {
			return true
		}
		if aToken != "*" && bToken != "*" && aToken != bToken {
			return false
		}
	}
	return len(aTokens) == len(bTokens)
}

// OperatorPhase is the phase of the hold row in the broker's Postgres (public.nats_hold), which
// decides the operator's grant (Operator). The row holds one of the three phases below, and the
// auth service answers the operator with OperatorPhaseReleased's grant whenever it cannot read the
// row, so an unknown phase never widens the grant. The writer hold's commands (envoy-infra hold)
// use this type for the row's phase, since the infra module cannot be imported here.
type OperatorPhase string

// The hold row's three phases: no hold, a hold being prepared while every client is still
// admitted, and a hold set, under which every client but the operator is refused.
const (
	OperatorPhaseReleased OperatorPhase = "released"
	OperatorPhasePrepare  OperatorPhase = "prepare"
	OperatorPhaseHeld     OperatorPhase = "held"
)

// ReplyRoot is the one subject token every reply prefix of Envoy's own clients sits under, so a
// render that moves JetStream into an account of its own and exports only the reply roots to the
// clients' account exports this one token, and no client's prefix needs an export of its own.
const ReplyRoot = "_INBOX_envoy"

// OperatorInbox is the inbox prefix of every operator connection (nats.CustomInboxPrefix), the
// one subscribe of every phase's grant, so the operator reads no other client's replies. It sits
// under ReplyRoot.
const OperatorInbox = ReplyRoot + "._operator"

// HoldBarrierStream is the hold's barrier stream, which the held operator creates, lists until it
// is present, and deletes: a stream rather than a consumer of the notification stream, because at
// a full consumer cap an unnamed consumer's create is refused. Its name is literal, so the held
// grant names it exactly; envoy-infra's streams census and its hold take it from here.
const HoldBarrierStream = "ENVOY_HOLD_BARRIER"

// sessionConsumerRecordsBucket is the bucket that records each listener session consumer's name
// (envoy-infra consumer-names), which the operator reads outside a hold, message by message, and
// snapshots and rewrites inside one.
const sessionConsumerRecordsBucket = "envoy_session_consumers"

// measureStream is the measurement job's stream, the one stream no client depends on, which the
// operator creates only inside a hold (envoy-infra streams update): a create carries a
// configuration of the caller's choosing, whose republish or source can write any subject.
const measureStream = "ENVOY_MEASURE"

// copiedBuckets are the buckets envoy-infra buckets copies: their infos are read outside a hold,
// and their streams are snapshotted, made, changed, removed and restored only inside one, since a
// snapshot delivers its chunks to a subject its requester chooses.
func copiedBuckets() []string {
	return []string{bus.InterestBucket, bus.RoleClaimBucket, bus.SessionBucket, bus.KnownSessionBucket, bus.CIStateBucket}
}

// otherProtectedBuckets are the protected buckets that are neither copied buckets nor the session
// consumers' records. The prepare operator reads their infos, and the held operator makes, changes
// and restores them.
func otherProtectedBuckets() []string {
	return []string{kvStream + bus.LegionAgentBucket, kvStream + bus.DPoPProofBucket, kvStream + bus.WebhookRedeliveryBucket}
}

// otherDeclaredStreams are the protected streams that are not copied buckets and not the session
// consumers' records: the notification stream and the other protected buckets. The held operator
// makes, changes and restores them.
func otherDeclaredStreams() []string {
	return append([]string{bus.Stream}, otherProtectedBuckets()...)
}

// operatorReleased is the operator's publish list outside a hold: JetStream's account info; the
// infos of the notification stream, the session consumers' records and each copied bucket;
// consumer-names status's reads, the records read message by message with STREAM.MSG.GET and the
// notification stream's consumer names and infos; and the info of ENVOY_MEASURE. It names no
// subject that writes. Its one backup is that walk of the session consumers' records
// (internal/bus/kvrecords): a records-only copy, each message's sequence, subject, headers, time
// and data, which a restore writes again, so it cannot reproduce the stream's last_seq. A snapshot
// (STREAM.SNAPSHOT) and a stream create are held's: nats-server checks a snapshot's deliver subject
// for its syntax alone (jetstream_api.go, jsStreamSnapshotRequest) and sends every chunk to it from
// the stream's own sender, and a create carries a republish or a source of the caller's choosing,
// so either writes any subject, a protected bucket's key among them.
//
// Naming no write subject does not keep the grant from writing. The boundary is one bus, a grant
// per connection, and every client in $G: any grant that holds a JetStream API request can have the
// server store a JetStream answer on any subject a stream in $G captures, the subject it names as
// the request's reply. The principals that can choose the stored bytes, by writing a KV key and
// then DIRECT.GETting it with that reply, are the writers of a bucket by design (today Listener,
// ListenerHost, Dispatch, and Host, which retires), through a key of a bucket they write. Measure
// can copy a listener lease message, with its headers, onto any captured subject, and Daemon can
// have a notification it may pull stored, under the notification's own subject, in any stream that
// captures the pull's reply subject. Every other principal can store only a server JSON document,
// this list's among them; Operator(held) writes keys by design. Session grants hold no $JS.API.
// publish. So no record the hold trusts for completion is in NATS (the broker's cutover record in
// Postgres decides), and every reader of a bucket value decodes strictly and fails closed on a
// value that is not an exact record of its key's kind.
func operatorReleased() []string {
	publish := []string{
		jsAPI + "INFO",
		jsAPI + "STREAM.INFO." + bus.Stream,
		jsAPI + "STREAM.INFO." + kvStream + sessionConsumerRecordsBucket,
	}
	for _, bucket := range copiedBuckets() {
		publish = append(publish, jsAPI+"STREAM.INFO."+kvStream+bucket)
	}
	return append(publish,
		jsAPI+"STREAM.MSG.GET."+kvStream+sessionConsumerRecordsBucket,
		jsAPI+"CONSUMER.NAMES."+bus.Stream,
		jsAPI+"CONSUMER.INFO."+bus.Stream+".*",
		jsAPI+"STREAM.INFO."+measureStream,
	)
}

// operatorPrepare is released's list, then the consumer update the hold's threshold raise sends on
// the notification stream, and the infos of every other protected bucket, which the hold's
// preflight reads before the row turns held. nats-server has one subject for a consumer's create
// and its update, so this admits a named consumer's create too: consumer-names prepare sends only
// updates, which the server refuses for a consumer that does not exist, and the hold's approval
// states the residual.
func operatorPrepare() []string {
	publish := append(operatorReleased(), jsAPI+"CONSUMER.CREATE."+bus.Stream+".*")
	for _, stream := range otherProtectedBuckets() {
		publish = append(publish, jsAPI+"STREAM.INFO."+stream)
	}
	return publish
}

// operatorHeld is prepare's list, then the writes the hold makes while every other client is
// refused: the barrier stream's create and delete; the stream and consumer lists the writer census
// reads; the retired consumer names' delete; the create of ENVOY_MEASURE (envoy-infra streams
// update); the session consumers' records, their bucket's create, snapshot and restore; each copied
// bucket's create, update, delete, snapshot and restore with its chunks, and its per-bucket barrier
// key; envoy-infra streams update's creates and updates of every protected stream; the census's
// removals of every undeclared stream and consumer; and the snapshot and restore of every other
// protected stream. Every snapshot is here, since its requester chooses where its chunks go.
func operatorHeld() []string {
	publish := append(operatorPrepare(),
		jsAPI+"STREAM.CREATE."+HoldBarrierStream,
		jsAPI+"STREAM.DELETE."+HoldBarrierStream,
		jsAPI+"STREAM.LIST",
		jsAPI+"STREAM.NAMES",
		jsAPI+"CONSUMER.LIST.*",
		jsAPI+"CONSUMER.DELETE."+bus.Stream+".*",
		jsAPI+"STREAM.CREATE."+measureStream,
		kvSubjects+sessionConsumerRecordsBucket+".>",
		jsAPI+"STREAM.CREATE."+kvStream+sessionConsumerRecordsBucket,
		jsAPI+"STREAM.SNAPSHOT."+kvStream+sessionConsumerRecordsBucket,
		"$JS.SNAPSHOT.ACK."+kvStream+sessionConsumerRecordsBucket+".>",
		jsAPI+"STREAM.RESTORE."+kvStream+sessionConsumerRecordsBucket,
		"$JS.SNAPSHOT.RESTORE."+kvStream+sessionConsumerRecordsBucket+".*",
	)
	for _, bucket := range copiedBuckets() {
		stream := kvStream + bucket
		publish = append(publish,
			jsAPI+"STREAM.CREATE."+stream,
			jsAPI+"STREAM.UPDATE."+stream,
			jsAPI+"STREAM.DELETE."+stream,
			jsAPI+"STREAM.SNAPSHOT."+stream,
			"$JS.SNAPSHOT.ACK."+stream+".>",
			jsAPI+"STREAM.RESTORE."+stream,
			"$JS.SNAPSHOT.RESTORE."+stream+".*",
			kvSubjects+bucket+".hold.barrier.*",
		)
	}
	for _, stream := range otherDeclaredStreams() {
		publish = append(publish, jsAPI+"STREAM.CREATE."+stream)
	}
	for _, stream := range otherDeclaredStreams() {
		publish = append(publish, jsAPI+"STREAM.UPDATE."+stream)
	}
	// ENVOY_MEASURE's update, which envoy-infra streams update sends to raise its copies: no grant
	// outside a hold holds a stream update, whose republish could write any subject.
	publish = append(publish, jsAPI+"STREAM.UPDATE."+measureStream)
	publish = append(publish, jsAPI+"STREAM.DELETE.*", jsAPI+"CONSUMER.DELETE.*.*")
	for _, stream := range otherDeclaredStreams() {
		publish = append(publish,
			jsAPI+"STREAM.SNAPSHOT."+stream,
			"$JS.SNAPSHOT.ACK."+stream+".>",
			jsAPI+"STREAM.RESTORE."+stream,
			"$JS.SNAPSHOT.RESTORE."+stream+".*",
		)
	}
	return publish
}

// Operator is the grant of the deployment's one operator identity (the BROKER_NATS_SERVICES entry
// of kind operator), one function of the hold row's phase: OperatorPhaseReleased's outside a hold,
// OperatorPhasePrepare's while a hold is prepared, and OperatorPhaseHeld's, the full list, while a
// hold is set. Each phase's list holds the one before it. A phase outside the three is answered
// with released's, the least. Every phase subscribes the operator's own inbox prefix
// (OperatorInbox) alone, and names only $JS. and OperatorInbox subjects, the session
// consumers' records' $KV. keys and the copied buckets' $KV. barrier keys, so NamesSystemSubject
// is false for each. The server installs a connection's permissions at its authorization, so a
// process that changes the phase reconnects to be given the new phase's grant.
//
// Two residuals of prepare's consumer create on the notification stream, which held keeps. A push
// consumer whose deliver subject is a protected bucket's key subject stores a foreign message in
// that bucket's stream only while some client holds a subscription on exactly that subject: the
// server activates a push consumer only on such a subscription (nats-server v2.15.1
// server/sublist.go:186-193), and delivers its messages under their own subjects to every matching
// subscriber, the bucket's stream among them (server/stream.go:8620-8621). No
// grant here subscribes a $KV. subject (TestNoGrantSubscribesAKeySubject); a hub's static admin
// user with > would, which is the admin's trust. And the prepare operator can read every
// notification through a push consumer delivering to its own inbox, which held already allows the
// same identity by a snapshot.
func Operator(phase OperatorPhase) Permissions {
	var publish []string
	switch phase {
	case OperatorPhasePrepare:
		publish = operatorPrepare()
	case OperatorPhaseHeld:
		publish = operatorHeld()
	default:
		publish = operatorReleased()
	}
	return Permissions{Publish: publish, Subscribe: []string{OperatorInbox + ".>"}}
}

// holdProbeSubject is the hold probe's one subject, which nothing else uses.
const holdProbeSubject = "_ENVOY_HOLD_PROBE.none"

// HoldProbe is the grant of the writer hold's probe (the BROKER_NATS_SERVICES entry of kind probe):
// publish and subscribe of one subject nothing else uses. The probe only shows whether the hub
// admits a connection; both directions are non-empty because the auth service refuses an empty
// direction as open (OpenDirection).
func HoldProbe() Permissions {
	return Permissions{Publish: []string{holdProbeSubject}, Subscribe: []string{holdProbeSubject}}
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
