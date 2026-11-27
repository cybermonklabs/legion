package natsgrant

import (
	"slices"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The two lanes of notifications.> that one user alone publishes. listenerGrant's publish deny
// names both. Only the grant of a lane's own publisher takes that lane off its deny (publishing):
//   - Dispatch's lane: Dispatch's user (Dispatch). envoy-dispatch's outbox publishes every issue,
//     document and project event there. The Legion daemon trusts an event there as Dispatch's.
//   - GitHub's lane: the user of the listener that receives GitHub's webhook (WebhookListener,
//     WebhookListenerHost). That listener's webhook route and its CI summary loop publish every
//     GitHub event. The Legion daemon applies a merge and a deciding review from an event there.
//
// No other program publishes either lane. A listener without GitHub's webhook publishes neither,
// and Dispatch asks GitHub to send a missed delivery again rather than publish it itself. So the
// deny refuses a direct publish on either lane by a session on a host. A caller of a listener's
// /v1/messages/publish publishes with the listener's user, so the deny covers that caller too,
// except on the listener whose user publishes that lane: the one beside envoy-dispatch, which
// shares Dispatch's user, and the webhook listener. No grant can refuse that caller there, so the
// publish route refuses a topic under either lane by name on every listener, and publishes
// nothing (cmd/listener/api.go, publisherLanes).
// The deny is in PublishDeny, so a hub's static user (packages/envoy/infra/core renders it) and a
// connection the auth service admits (internal/cipher/natscallout encodes it) both carry it.
//
// The deny alone does not stop a session on a shared host user from posing as Dispatch or as
// GitHub. listenerGrant allows the stream's create and update ($JS.API.STREAM.CREATE and
// STREAM.UPDATE on ENVOY_NOTIFICATIONS), so every grant built on it but Edge holds both: Listener,
// ListenerHost, WebhookListener, WebhookListenerHost, Dispatch and Host. Edge takes both off its
// list. No session shares the user of the first five. Host is the grant of the user that a host's
// listener and every session on the host share. Such a session can update the stream to add a
// subject transform, or a Sources entry with transforms, and then publish on a subject the deny
// allows. The stream stores that message under Dispatch's lane or GitHub's, and the Legion daemon
// trusts it there. No other session can: SessionPattern and Session hold no stream call, so
// neither a session of a hub's shared session user nor a session the auth service admits can
// change the stream. The gap stays open until the sessions of a host connect through the auth
// service and hold Session in place of the shared host user (dispatch://INFRA-840).
const (
	dispatchLane = bus.DispatchTopicPrefix + ">"
	githubLane   = contracts.GithubTopicPrefix + ">"
)

// WebhookListener is the grant of the listener that receives GitHub's webhook (its ENVOY_WEBHOOKS
// names github), as machine machineID: Listener with GitHub's lane (githubLane) taken off its
// publish deny. That listener's webhook route and CI summary loop publish every GitHub event, and
// no other listener may. The set still denies Dispatch's lane. The auth service answers a service
// of the kind webhook-listener with it (internal/cipher/natscallout).
func WebhookListener(machineID string) Permissions {
	return publishing(Listener(machineID), githubLane)
}

// WebhookListenerHost is the grant of the user of the listener that receives GitHub's webhook, as
// machine machineID, when that user is the listener's own static user, one no session shares:
// ListenerHost with GitHub's lane taken off its publish deny. The set still denies Dispatch's lane.
// A gcp:NatsHub user with listenerMachineId and githubWebhook holds it
// (packages/envoy/infra/core, WebhookListenerHostPermissions).
func WebhookListenerHost(machineID string) Permissions {
	return publishing(ListenerHost(machineID), githubLane)
}

// publishing is grant with lane taken off its publish deny: the grant of lane's one publisher.
// listenerGrant builds the deny afresh at each call, so this changes no other grant.
func publishing(grant Permissions, lane string) Permissions {
	grant.PublishDeny = slices.DeleteFunc(grant.PublishDeny, func(entry string) bool { return entry == lane })
	return grant
}
