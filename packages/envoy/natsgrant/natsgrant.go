// Package natsgrant is the one table of NATS grants Envoy's clients hold: the sets a hub renders
// into its static authorization (packages/envoy/infra/core wraps each as the rules a User takes) and
// the grants the auth service answers a connection with (internal/cipher/natscallout). Both read
// the same functions, so a hub's static user and a connection the auth service admits for the same
// kind hold the same lists, built from the names internal/bus and internal/contracts export.
//
// Each call returns lists of its own, so a caller may narrow a grant without changing another's.
package natsgrant

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
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
//   - for the listener state bucket (bus.ListenerStateBucket), the calls the listener makes on it
//     and no wider one (ListenerState): the bucket's stream info, create and update, and a direct
//     get and a write of each key family it uses, which are the machine's own keys alone - its
//     delivery lease (bus.ListenerLeaseKey), the clean-hand-back marks of its stopping pods
//     (bus.ListenerHandBackPrefix) and its delivery ledger (bus.ListenerLedgerPrefix), so one
//     machine's user cannot write or delete another machine's lease, marks or ledger by a key
//     write, nor request a read of one by a direct get - beside every session's last-seen record
//     (bus.LastSeenPrefix), which is read across machines and so stays fleet-wide. What every
//     listener user of the fleet can still do to the whole bucket is ListenerState's to say. A
//     listener whose grant lacks this bucket does not exit: it logs an ERROR naming the bucket
//     and delivers without a lease, receiving from its durable only while no other subscriber
//     holds it;
//   - the purge of the interest bucket, which its delete-marker collection sends, and of the
//     known-session bucket, which the purge of its replaced chunks' markers sends. NATS grants a
//     purge by stream alone (the subject a purge filters to is in its request), so each reaches
//     the whole bucket; a user that holds the bucket's writes can already delete or roll up every
//     key of it, so neither removes a record that user could not.
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
	publish = append(publish, ListenerState(machineID)...)
	publish = append(publish, jsAPI+"STREAM.PURGE."+kvStream+bus.InterestBucket, jsAPI+"STREAM.PURGE."+kvStream+bus.KnownSessionBucket)
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

// edgeInboxPrefix begins the inbox prefix of every edge listener (EdgeInboxPrefix).
const edgeInboxPrefix = "_INBOX_envoy_edge_"

// EdgeInboxPrefix is the inbox prefix of every connection the edge listener of machine machineID
// makes (cmd/listener in edge mode, nats.CustomInboxPrefix): _INBOX_envoy_edge_<machine id>. So
// its request replies and its push consumers' deliveries arrive under a prefix of its own, which
// Edge subscribes in place of every reply inbox, and one edge machine is delivered no other
// machine's replies. Edge refuses a machine id that is not one subject token, so no machine's
// prefix covers another's.
func EdgeInboxPrefix(machineID string) string {
	return edgeInboxPrefix + machineID
}

// EdgeRelayReplyPrefix is the receipt family of the edge listener of machine machineID:
// bus.RelayReplyPrefix followed by the machine id, under which its session relay names a relay
// frame's receipt subject (that family, then a nuid, the session id and a token). Edge subscribes
// only its own machine's family, so no edge machine subscribes another machine's receipts, and
// since it can
// change no stream's subjects, no stream it reads captures them. It publishes every edge
// machine's (bus.RelayReplyPrefix): the listener serving a session's relay socket may be another
// edge machine than the one holding the session's consumer, and it publishes the receipt on the
// owner's family. So one edge machine can forge a receipt for another machine's frame, which adds
// nothing past the reach Edge already leaves it: it can update or delete any consumer of the
// stream. Edge refuses a machine id that is not one subject token, so no machine's family covers
// another's.
func EdgeRelayReplyPrefix(machineID string) string {
	return bus.RelayReplyPrefix + machineID
}

// Edge is the grant of an edge listener's own NATS user as machine machineID, serving projects:
// the listener that serves an outside person's sessions, which no session and no other listener
// shares. It is Listener's set with these changes and no other. machineID must be one subject
// token (SessionToken), and projects a non-empty list oauthwire.ValidProjects takes, as
// ENVOY_EDGE_PROJECTS and an edge-listener entry of BROKER_NATS_SERVICES are read: project keys,
// sorted, each once. Anything else is refused naming why before any subject is built: a key or a
// machine id that is no one subject token would widen the subjects it names to other projects' or
// other machines', and an empty list would render a grant of the whole consumer and key-value
// reach serving no project.
//
// Publish, in place of every notification topic, this list and no other notification topic:
//   - the agent subjects (contracts.AgentTopicPrefix): a send;
//   - the role subjects of each project, in the form that names the project
//     (contracts.RoleTopicPrefix + project): a role message;
//   - the relay subjects (bus.DeliverSubjectPrefix): a frame to a session whose consumer it holds;
//   - every edge machine's relay receipts (bus.RelayReplyPrefix), which the listener serving a
//     session's relay socket publishes a frame's receipt on, on the family of the machine that
//     holds the session's consumer (EdgeRelayReplyPrefix): cmd/listener's relay socket
//     (relay_socket.go) publishes the receipt its client sends there;
//   - the resume lane (bus.SessionResumeSubjectPrefix): where a deleted session consumer stopped;
//   - the exception lanes (bus.ExceptionTopicPrefix) of the agent subjects and of each project's
//     role subjects: a failed delivery's report, and an unheld role's refusal.
//
// So a direct publish of the edge's reaches no Dispatch, GitHub, Slack, team or Legion event, no
// other project's role lane and no role lane in the form that names no project, nothing on the
// kept role lane (bus.KeptTopicPrefix, its copies and drain cursors, which only a listener that
// takes the role lane writes), no exception of any other topic and no reply to another client's
// request. A message on a role lane of its own project that names a role of another project is
// the shared listeners' role lane to refuse. It holds no create or update of the stream or of any
// bucket's stream, no direct get of any bucket, and none of the listener state bucket's calls
// (ListenerState), which Listener holds and which include that bucket's stream create, update and
// direct gets, so it cannot change any stream's configuration - its subjects, its limits, or a
// republish. It can, though, delete any stream of its account, through two answers the server
// sends with no body on whatever reply subject a client names, each needing no other client: a
// no_wait pull on a consumer with nothing to deliver is answered a "404 No Messages" status, and a
// synchronous ack an empty reply, so either aimed at $JS.API.STREAM.DELETE.<s> deletes stream <s>
// (the consumer calls below, TestAPullStatusAndAnAckAnswerOnTheAPIDeleteAStream). That reach is
// availability only - no message of any project crosses it - and is accepted for day one until
// the hub's nats-server is raised from 2.15.1 to 2.15.3, which refuses a reply into $JS.API. The
// two routes that would change a stream's configuration are both held off:
//   - A republish carries the stored value to its destination with that value as its body, and a
//     destination under $JS.API. is a request of the account's: on the hub's image an empty value
//     republished to a stream delete deletes the stream, and a non-empty one is refused as that
//     request's body (TestTheEdgeCannotSetABucketToRepublishIntoTheJetStreamAPI).
//   - The stream itself answers a direct get, through the account's own JetStream client, on
//     whatever reply subject the request names, with the stored value as the body. Aimed at
//     $JS.API., the API takes that value as a request of the account's: a value written as a
//     stream update changes that stream, and an empty one aimed at a delete deletes it
//     (TestTheEdgeHoldsNoDirectGetWhoseAnswerReachesTheJetStreamAPI). So the edge reads each
//     bucket with a stream message get (bus.WithStreamMessageReads), whose answer the JetStream API
//     publishes; that answer, a stream info answer and a stored publish's acknowledgement, aimed the
//     same way, take no action there in the same row.
//
// Its start reads the stream and ends, naming it, when the server holds none, and finds each
// bucket it opens (bus.EnsureKeyValue): when the server lacks the interest, role, session or
// spent-proof bucket, or holds it at fewer copies than the deployment's count, the start tries the
// open again by the listener's start rule (cmd/listener/README.md, "Start"), logging at each try
// an error that names the create or raise the server refused, and so waits for the bucket; the
// Legion agent bucket's open logs one error and is retried in the background, and so is the
// known-session bucket's, which raises no bucket it finds. Every other listener of this build
// creates the spent-proof bucket at its start (cmd/listener's ensureSpentProofBucket), so an edge
// listener started before any full listener waits for one. Since it holds no call of the listener
// state bucket, an edge listener opens no delivery lease and delivers as a listener whose grant
// lacks that bucket does (cmd/listener, edge mode): one edge pod runs per machine id. The rest of
// Listener's publish list stays as it is: the stream's info and stored-message get, its durable's
// calls, JetStream's account info, the KV calls of every bucket of bus.ListenerBuckets but its
// stream's create, its update and its direct get, the purges of the interest bucket and of the
// known-session bucket (the edge's known-session store runs the chunk-marker pass too), and the
// session consumers' calls.
//
// Subscribe: its own machine's inbox prefix (EdgeInboxPrefix) in place of every reply inbox, so
// the server delivers it no other client's replies; every session's relay subject
// (bus.DeliverSubject), which the listener serving a session's relay socket subscribes for that
// session; and its own machine's relay receipts (EdgeRelayReplyPrefix), which its session relay
// names its frames' receipts under (cmd/listener's startSessionRelays), so a receipt a relay socket
// publishes for a frame reaches the machine that relayed it, whichever replica serves the socket.
// Neither is a reply inbox, which no session's grant reaches at the edge: an edge session answers
// its frames over its relay socket. It subscribes no role lane, so it
// takes no share of any project's role messages: a shared listener resolves a role's holder and
// forwards the message to the holder's agent subject, which the holder's session consumer
// delivers. Its lists name no agent stream subject. nats-server (2.15.1) admits a subscription to
// notifications.envoy.deliver.> under the allow entry notifications.envoy.deliver.*, and delivers
// it a subject of two tokens past the prefix: no Envoy program publishes one, since
// bus.DeliverSubject names a session by one token.
//
// What the lists do not hold back. TestTheEdgeGrantsConsumerCallsReachPastItsPublishList,
// TestTheEdgeGrantsPushConsumerReachesItsAccountsJetStreamAPI,
// TestAPushConsumersHeartbeatOnTheAPIDeletesAStreamWhileAClientSubscribesIt and
// TestAPullStatusAndAnAckAnswerOnTheAPIDeleteAStream pin these on the hub's image; the
// bucket-stream consumer is read from the server's rule, not run:
//   - The session consumers' calls name the consumer by *, since each session's consumer is named
//     for its session (bus.SessionConsumerName). A NATS wildcard matches a whole token and a
//     consumer name is one token, so no permission can hold these calls to the edge's own
//     consumers: the edge can create, update (a create with the update action), delete, read and
//     pull any consumer of the stream, every other listener's durable, Dispatch's and every
//     session's consumer among them, and so take another listener's push deliveries.
//   - nats-server checks a push consumer's deliver subject against no permission, only that the
//     stream does not carry it, and delivers only where a client subscribes. A delivery is sent
//     under the message's own stream subject, not the deliver subject: each subscription that
//     matches the deliver subject receives it, and it reads as a message of the stream's subject.
//     So through a consumer it creates, the edge can hand any message it stored, or any message of
//     the stream, to any subscriber of any subject the stream does not carry: every project's role
//     lanes and their exception lanes, any reply inbox, and the agent stream subjects. A deliver
//     subject the stream carries is refused as a cycle. The JetStream API's import sees the stream
//     subject, not $JS.API., so a delivery to a stream delete, purge or update, or to a bucket's
//     stream delete, takes no action. A stream whose subjects take the deliver subject - another
//     stream of the account, a key-value bucket's stream among them - stores the delivery under
//     its stream subject, as a message of that stream and never as a key, and counts it toward
//     that stream's limits. A consumer created with an idle heartbeat also sends its heartbeats,
//     empty status messages with no stored subject, to the deliver subject: a heartbeat aimed at a
//     stream delete deletes that stream. Each of these push routes needs some other client to
//     subscribe the deliver subject, since the edge subscribes none a stream takes and no $JS.API.
//     subject, and no Envoy client subscribes one.
//   - The consumer calls the edge keeps also answer two requests directly, needing no subscriber:
//     a no_wait pull on a consumer with nothing to deliver
//     (CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.*) is answered a "404 No Messages" status, and a
//     synchronous ack ($JS.ACK.ENVOY_NOTIFICATIONS.*.>) an empty reply. The server sends each with
//     no body on whatever reply subject the requester names, so one aimed at
//     $JS.API.STREAM.DELETE.<s> deletes stream <s> on the hub's nats-server (2.15.1), with no
//     other client (TestAPullStatusAndAnAckAnswerOnTheAPIDeleteAStream). nats-server 2.15.3, which
//     the hub is to run, refuses a reply into $JS.API.; this is the availability reach accepted for
//     day one.
//   - Its KV calls include a consumer on each bucket's stream (CONSUMER.CREATE.KV_<bucket>.>),
//     and that stream does not carry the notification subjects, so such a consumer can deliver a
//     bucket's message onto the Dispatch, GitHub, Slack, Legion, team, kept, resume and takeover
//     subjects wherever a client subscribes them: the cycle refusal holds only for the
//     notification stream's own consumers.
//   - Today's agent, relay and resume subjects name no project, so it can publish on any
//     session's, and a consumer on any filter reads any stored message.
//   - Its KV calls write every record of the seven buckets of bus.ListenerBuckets: interests,
//     roles, sessions, known sessions, Legion agents, CI state and DPoP proofs.
func Edge(machineID string, projects []string) (Permissions, error) {
	if !SessionToken(machineID) {
		return Permissions{}, fmt.Errorf("natsgrant: the edge's machine id %q is not one subject token", machineID)
	}
	if len(projects) == 0 {
		return Permissions{}, errors.New("natsgrant: the edge serves no project")
	}
	if err := oauthwire.ValidProjects(projects); err != nil {
		return Permissions{}, fmt.Errorf("natsgrant: the edge's projects %q are no list of project keys: %w", projects, err)
	}
	removed := []string{
		bus.NotificationTopicPrefix + ">",
		jsAPI + "STREAM.CREATE." + bus.Stream,
		jsAPI + "STREAM.UPDATE." + bus.Stream,
	}
	for _, bucket := range bus.ListenerBuckets() {
		removed = append(removed, jsAPI+"STREAM.CREATE."+kvStream+bucket, jsAPI+"STREAM.UPDATE."+kvStream+bucket, jsAPI+"DIRECT.GET."+kvStream+bucket+".>")
	}
	removed = append(removed, ListenerState(machineID)...)
	publish := []string{contracts.AgentTopicPrefix + ">"}
	for _, project := range projects {
		publish = append(publish, contracts.RoleTopicPrefix+project+".>")
	}
	publish = append(publish,
		bus.DeliverSubjectPrefix+"*",
		bus.RelayReplyPrefix+">",
		bus.SessionResumeSubjectPrefix+">",
		bus.ExceptionTopicPrefix+contracts.AgentTopicPrefix+">",
	)
	for _, project := range projects {
		publish = append(publish, bus.ExceptionTopicPrefix+contracts.RoleTopicPrefix+project+".>")
	}
	for _, entry := range Listener(machineID).Publish {
		if !slices.Contains(removed, entry) {
			publish = append(publish, entry)
		}
	}
	return Permissions{
		Publish:   publish,
		Subscribe: []string{EdgeInboxPrefix(machineID) + ".>", bus.DeliverSubjectPrefix + "*", EdgeRelayReplyPrefix(machineID) + ".>"},
	}, nil
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

// ListenerState is the listener state bucket's part of listenerGrant for machine machineID: the
// eleven subjects the listener's own calls on bus.ListenerStateBucket send, each no wider than its
// call. Below, <u> is the machine's durable (bus.ListenerDurable), one subject token for any
// listener that binds it, since nats.go refuses a consumer name holding a '.' (nats.go@v1.55.0
// jsm.go, checkConsumerName). Each call is named by its function: paths are packages/envoy's, and
// nats.go's at v1.55.0 where the text says nats.go. The listener opens the bucket once a process,
// on its lease's connection (cmd/listener/lease_bucket.go, openListenerState): at main's start, or,
// for a pod whose grant refused the bucket then, at the try of cmd/listener/main.go's
// joinDeliveryLease that opens it. The lease (main.go, openDeliveryLease, or joinDeliveryLease's
// own), the delivery ledger (main.go, newDeliveryLedger, or leaseLedger at the join), the last-seen
// records (main.go's main, session.WithLastSeenBucket, or session.SessionRegistry.SetLastSeenBucket
// at the join) and the bus client's keeper (main.go's main, bus.Client.KeepBucket) share that
// handle.
//
//   - $JS.API.STREAM.INFO.KV_envoy_listener: the open's lookups and its read of a found bucket
//     (internal/bus/kv_ensure.go, ensureKeyValue: its lookUpBucket, which awaitBucketApplied
//     takes again, and its js.StreamInfo), the copy and leader waits (internal/bus/start_wait.go,
//     awaitStream, from ensureKeyValue's awaitReplicasCurrent and awaitStreamLeader), the handle's
//     open (internal/bus/kv.go, wrapKeyValue), the age raise's read (bus.RaiseMaxAge, from
//     openListenerState), an update's read-back (internal/bus/stream.go, updateStream) and the
//     keeper's raise (kv_ensure.go, raiseReplicas, from internal/bus/keeper.go,
//     bus.Client.keepPass): nats.go's (*js).StreamInfo in jsm.go and jetstream's
//     (*jetStream).Stream in jetstream/jetstream.go.
//   - $JS.API.STREAM.CREATE.KV_envoy_listener: the create of an absent bucket (ensureKeyValue's
//     js.CreateKeyValue, from openListenerState's bus.EnsureKeyValue): nats.go's AddStream in
//     jsm.go, from (*js).CreateKeyValue in nats.go's kv.go.
//   - $JS.API.STREAM.UPDATE.KV_envoy_listener: the copy raise (ensureKeyValue to raiseStream to
//     updateStream), the age raise (openListenerState to bus.RaiseMaxAge to updateStream), the
//     keeper's raise (raiseReplicas to raiseStream) and nats.go's update of a bucket another pod
//     created first (nats.go's CreateKeyValue): UpdateStream in nats.go's jsm.go.
//   - $JS.API.DIRECT.GET.KV_envoy_listener.$KV.envoy_listener.<key>, one subject for each key family
//     below: every Get, a direct get of the key's last message with no body, since every bucket
//     nats.go creates allows direct gets (nats.go's CreateKeyValue sets AllowDirect, (*kvs).get
//     reads by it, and (*js).getMsg in jsm.go sends the get), and the read nats.go's (*kvs).Create
//     makes of its key after a write that failed.
//   - $KV.envoy_listener.<key>, one subject for each key family below: every write, a publish to
//     the key's subject - nats.go's (*kvs).Put, Create and Update (both through (*kvs).update) and
//     Delete, in nats.go's kv.go.
//
// The four key families, each read and written by these calls:
//   - lease.<u> (bus.ListenerLeaseKey), the machine's delivery lease: read by Get in
//     cmd/listener/lease.go's lease.Successor, lease.ownRevision, lease.poll and lease.acquire
//     (its read-back), and after a failed Create; written by Create in lease.acquire and by Update
//     in lease.writeRelease, lease.renew and lease.acquire.
//   - handback.<u>.* (bus.ListenerHandBackPrefix), one token after the prefix, the revision a mark
//     vouches for (cmd/listener/lease_handback.go, lease.handBackKey): written by Put in
//     lease.MarkHandBack, read by Get in lease.HandBackMarked.
//   - delivery.<u>.*.* (bus.ListenerLedgerPrefix), two tokens after the prefix, the digest and the
//     session token (internal/dedupe/kv.go, dedupe.KV.Key): written by Create and Update in
//     dedupe.KV.attempt, Put in dedupe.KV.Record and Delete in dedupe.KV.Clear, read by Get in
//     dedupe.KV.attempt and after a failed Create.
//   - seen.* (bus.LastSeenPrefix), one token after the prefix, the session token
//     (internal/session/lastseen.go, lastSeenKey): written by Put in
//     session.SessionRegistry.recordSeen, read by Get in internal/session/lastseen.go's readSeen,
//     for every session whichever machine registered it.
//
// The listener makes no other call on this bucket. bus.KeyValue offers no Watch, WatchAll,
// History, Keys or ListKeys (its doc, internal/bus/kv.go), and the listener calls none of its
// GetRevision (a direct get on the stream's bare subject), WatchKeys, WatchFilteredKeys and
// ReadHistory (a consumer create) or GetFromLeader (a STREAM.MSG.GET with a request body) on it. So
// this set holds none of the stream message get, consumer create and delete or flow-control
// replies KVWrite grants for every other bucket, and no direct get of a key outside the four
// families: every read but seen.* is of a key this machine writes. The two other calls the open
// needs are listenerGrant's own: JetStream's account info, which a create sends first (nats.go's
// CreateKeyValue), and the reply inboxes every answer arrives on.
//
// What this set holds is a subject's reach, and a NATS grant acts on subjects alone, not on a
// message's headers or on what a stream update changes. So a machine's user cannot write, delete
// or request a read of another machine's keys by a key write or a direct get, and the bucket is
// still no machine's own: every listener user of the fleet can purge it, change its configuration
// and read every answer it sends another machine. A write to the user's own key may carry the
// header Nats-Rollup: all, which the bucket's stream takes, since nats.go creates every bucket
// with AllowRollup set and DenyPurge unset (nats.go's CreateKeyValue), and answers by purging
// every key of every machine (nats-server v2.15.1 server/stream.go,
// processJetStreamMsgWithRollup). The STREAM.UPDATE the raises need carries a whole
// configuration, and no subject narrows it to the field a raise changes, so it can change the
// bucket's limits or any other setting. And the _INBOX.> subscription of listenerGrant receives
// every reply any client gets, another machine's direct-get answer included: the refused direct
// get stops a machine's user asking for another machine's key, not reading the answer. The bucket
// therefore trusts all listener users of the fleet, as that shared _INBOX.> subscription already
// trusts them with every reply. dispatch://INFRA-840's plan replaces the shared reply subscription
// and the update grant.
func ListenerState(machineID string) []string {
	stream := kvStream + bus.ListenerStateBucket
	durable := bus.ListenerDurable(machineID)
	keys := []string{
		bus.ListenerLeaseKey(machineID),
		bus.ListenerHandBackPrefix(durable) + "*",
		bus.ListenerLedgerPrefix(durable) + "*.*",
		bus.LastSeenPrefix + "*",
	}
	calls := []string{
		jsAPI + "STREAM.INFO." + stream,
		jsAPI + "STREAM.CREATE." + stream,
		jsAPI + "STREAM.UPDATE." + stream,
	}
	for _, key := range keys {
		calls = append(calls, jsAPI+"DIRECT.GET."+stream+"."+kvSubjects+bus.ListenerStateBucket+"."+key)
	}
	for _, key := range keys {
		calls = append(calls, kvSubjects+bus.ListenerStateBucket+"."+key)
	}
	return calls
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
// reads the account's JetStream state and the listener's lease keys in KV_envoy_listener, and
// publishes on its own cutover inbox, <inbox prefix>.cutover.>, under which a probe sends a
// numbered sequence and reads it back to count what a restart of the hub's servers loses. It makes,
// changes and removes no stream. Its direct get of the lease keys needs nats-server 2.15.3 or
// newer: on 2.15.1 a direct get of a missing key whose reply subject is
// $JS.API.STREAM.DELETE.<stream> deletes that stream, and 2.15.3 refuses that request. The grant
// before this change held stream creates and updates, and a create or an update carries a
// configuration of the caller's choosing: a source or a republish in it would read every stream of
// the account, and the same grant wrote any KV bucket of the account, since a RePublish whose
// destination is a KV subject (Source envoy-measure.>, Destination
// $KV.envoy_session_consumers.>) is accepted: nats-server checks only that the destination does not
// loop into the stream itself (v2.15.1 stream.go:2479-2514, checkStreamCfgLocked; measured on
// 2.15.1 and 2.10.29), so its publishes became chosen self-keyed records and the bucket's sentinel.
// The streams change (#4687) removes those calls from every callout answer but the held
// operator's, and narrows every static user on a callout hub, so after the cutover the held
// operator is their only holder; ENVOY_MEASURE is the operator's to make (envoy-infra streams
// update). It subscribes only its own inbox prefixes, never _INBOX.>, where the listener's and
// Dispatch's push consumer deliveries and request replies arrive, so it reads no message of another
// client. Its inbox prefixes are two while the job moves under ReplyRoot: legacyMeasureInbox, which
// the deployed job sets today, and MeasureInbox, which it sets once it moves; the cutover inbox is
// under each. The legacy one goes when no measurement job sets it.
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
			legacyMeasureInbox + ".cutover.>",
			MeasureInbox + ".cutover.>",
		},
		Subscribe: []string{legacyMeasureInbox + ".>", MeasureInbox + ".>"},
	}
}

// ReplyRoot is the subject token Envoy's reply prefixes move under, so that a render that moves
// JetStream into an account of its own exports one reply root to the clients' account rather than
// a prefix for each client: a push or ordered consumer's deliveries and a stream's republish are
// published inside JetStream's account and reach a client only through a subject that account
// exports. A request's reply needs no export, since it comes back through the service import's
// response mapping. The operator's prefix (OperatorInbox) and the measurement job's moved one
// (MeasureInbox) are under it. The listener, Dispatch and the Legion daemon still use nats.go's
// _INBOX. (Listener, ListenerHost, Host, Dispatch and Daemon subscribe _INBOX.>), and the edge
// listener its own machine's prefix, _INBOX_envoy_edge_<machine id> (EdgeInboxPrefix, #4807); until
// each moves under the root, such a render exports those prefixes as well: _INBOX.> once, and
// _INBOX_envoy_edge_<machine id>.> once per edge machine, since that prefix shares the root's
// characters but not its token, so the root's export does not reach it, and a wildcard matches
// whole tokens, so no one export reaches every edge machine's prefix. The measurement job's prefix
// before its move (legacyMeasureInbox) is outside the root too, and goes once no job sets it.
const ReplyRoot = "_INBOX_envoy"

// MeasureInbox is the measurement job's inbox prefix (nats.js's inboxPrefix) under ReplyRoot.
const MeasureInbox = ReplyRoot + "._measure"

// legacyMeasureInbox is the measurement job's inbox prefix before it moved under ReplyRoot, which
// the deployed job still sets. It shares the root's characters but not its token, so ReplyRoot's
// export does not reach it.
const legacyMeasureInbox = "_INBOX_envoy_measure"

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
