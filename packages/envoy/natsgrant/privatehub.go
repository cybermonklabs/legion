package natsgrant

import (
	"slices"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The names two programs must spell alike for a hub rendered with private-project support. One
// side writes each and the auth service (internal/cipher/natscallout) reads it, so both take it
// from here: PrivateProjectsServerTag, which a hub's renderer (packages/envoy/infra/core) writes
// into the hub's configuration; HubAdmissionAudience, for which gcp:Dispatch projects Dispatch's
// token and which Dispatch's proof client presents; and PrivacyRequiredSuffix, which a listener
// puts at the end of its connections' client names. A package this one imports, internal/bus and
// internal/contracts among them, and natscred, which internal/bus imports, cannot import it back,
// so such a package takes the name from its caller. The auth service decides its private-hub
// admission on all three; the hub writer's tag, Dispatch's projection and proof client, and the
// listener's required mode are later work.
const (
	// PrivateProjectsServerTag is the server_tags entry a hub's nats-server configuration carries
	// when, and only when, the hub is rendered with private-project support. nats-server copies a
	// server's tags into every authorization request it sends the auth service
	// (server/auth_callout.go, claim.Server.Tags), lowercased and trimmed as it reads them (jwt's
	// TagList.Add), so this value is lowercase and has no space to trim. The hub writer renders it,
	// and the auth service decides its private-hub admission rules on it. A hub rendered private
	// whose tags, so normalized, do not hold it, the entry missing or misspelled, reads to the auth
	// service as one without private-project support: Dispatch's hub-admission proof fails there,
	// and the auth service refuses there the credentials that only a hub rendered private may
	// admit, Dispatch's own included once a project is private.
	PrivateProjectsServerTag = "envoy-private-projects"

	// HubAdmissionAudience is the audience of the projected Kubernetes ServiceAccount token
	// Dispatch presents on its hub-admission proof connection: gcp:Dispatch projects it into the
	// envoy-dispatch container alone. The auth service admits a token of this audience only for
	// Dispatch's own account (its dispatch entry of BROKER_NATS_SERVICES), and only on a request
	// carrying PrivateProjectsServerTag. It must differ from BROKER_NATS_K8S_AUDIENCE, at which the
	// auth service admits a service's Kubernetes token on every hub (PlatformTokens): a proof token
	// projected for that audience is admitted as an ordinary service connection whether or not the
	// hub carries PrivateProjectsServerTag, and one projected for an audience the auth service
	// takes no NATS token at is refused on every hub.
	HubAdmissionAudience = "envoy-hub-admission"

	// PrivacyRequiredSuffix is the suffix a listener built in required privacy mode puts at the end
	// of the NATS client name of every connection its credential opens; its leading space keeps it
	// a word of its own. That is each of the listener's bus clients (the stream client, the role
	// lane and the session consumers, all three of which internal/bus names today) and the renewal
	// probe natscred dials with a later token, whose name keeps the connection's name last ("probe "
	// then that name), so the suffix stays last. internal/bus and natscred cannot import this
	// package, so the listener passes the suffix in. nats-server copies a client name unchanged
	// into the authorization request (server/auth_callout.go, ConnectOptions.Name in
	// fillConnectOpts and ClientInformation.Name in fillClientInfo). On a request carrying
	// PrivateProjectsServerTag the auth service refuses a connection it would answer with a
	// listener's grant, Dispatch's listener's included, whose client name does not end with this
	// exact suffix. The name is the client's own claim: it keeps an older or all-public listener
	// build off a private hub, and proves nothing about who holds a listener's credential.
	PrivacyRequiredSuffix = " privacy-required"
)

// hubAdmissionPingToken is the last token of HubAdmissionPing: a literal, which no reply inbox
// nats.go makes under the prefix can be, since each of those ends in a 22-character NUID.
const hubAdmissionPingToken = "hub-admission"

// HubAdmissionPing is the one subject Dispatch's hub-admission proof connection may publish to:
// a subject under its own reply prefix, InboxPrefix(identity), which the same connection
// subscribes. A publish on it, which the server delivers back to that connection's own
// subscription, is the proof's round trip, and reaches no other client; the grant lets the
// connection publish nothing else, a reply included.
func HubAdmissionPing(identity string) string {
	return InboxPrefix(identity) + "." + hubAdmissionPingToken
}

// HubAdmission is the grant the auth service answers Dispatch's hub-admission proof with
// (HubAdmissionAudience), on a hub rendered private alone: subscribe its own reply prefix,
// InboxPrefix(identity)+".>", and publish only HubAdmissionPing(identity). It holds no project
// subject, no JetStream or KV call and no system subject, so the connection proves that the hub
// admitted Dispatch through the auth service and can do nothing else.
func HubAdmission(identity string) Permissions {
	return Permissions{
		Publish:   []string{HubAdmissionPing(identity)},
		Subscribe: []string{InboxPrefix(identity) + ".>"},
	}
}

// DispatchListener is the grant of Dispatch's listener Deployment once it runs as an account of
// its own (the auth service's dispatch-listener kind), as machine machineID: the listener grant
// (listenerGrant), which is Listener less the session consumers' calls (sessionConsumerCalls),
// element for element the listener part Dispatch's own grant is built on. So that listener keeps
// its durable and its role lane, and refuses a registration that asks for consumer delivery 503
// no_grant (bus.ErrConsumerNotGranted), writing nothing of the session it names, as the listener
// beside envoy-dispatch does on Dispatch's user today.
func DispatchListener(machineID string) Permissions {
	return listenerGrant(machineID)
}

// DispatchApp is the grant the auth service answers Dispatch's own account with on a hub rendered
// private (PrivateProjectsServerTag), as machine machineID: Dispatch less what only a listener
// uses, which is the listener durable's (bus.ListenerDurable) info, create and acknowledgements,
// and the two role-lane queue subscriptions. It keeps every other entry, in Dispatch's order: the
// publish of every notification topic, the inbox subscription, the stream's calls, the KV calls on
// each listener bucket (the role and session buckets Dispatch's outbox reads among them), Dispatch's
// own durables, the webhook-redelivery bucket and the agent stream's subjects. So a listener left
// on Dispatch's app account binds no role lane on such a hub, and relays nothing there. Dispatch
// itself stays as it is, the static set a hub renders, and the auth service answers it on a hub
// not rendered private.
func DispatchApp(machineID string) Permissions {
	durable := bus.ListenerDurable(machineID)
	listenerOnly := []string{
		jsAPI + "CONSUMER.INFO." + bus.Stream + "." + durable,
		jsAPI + "CONSUMER.CREATE." + bus.Stream + "." + durable,
		jsAck + bus.Stream + "." + durable + ".>",
		contracts.RoleTopicPrefix + "> " + bus.ListenerQueue(machineID),
		contracts.RoleTopicPrefix + "> " + bus.RoleLaneQueue,
	}
	grant := Dispatch(machineID)
	without := func(entry string) bool { return slices.Contains(listenerOnly, entry) }
	grant.Publish = slices.DeleteFunc(grant.Publish, without)
	grant.Subscribe = slices.DeleteFunc(grant.Subscribe, without)
	return grant
}
