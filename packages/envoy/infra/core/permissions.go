package core

import "github.com/sjawhar/envoy/natsgrant"

// Permissions are one NATS user's publish and subscribe rules, as a User takes them.
type Permissions struct {
	Publish, Subscribe Rules
}

// The sets below are natsgrant's, as the rules a User takes. natsgrant
// (packages/envoy/natsgrant) is the one table of grants, which the auth service also answers
// connections with, so a hub's static user and an admitted connection of the same kind hold the
// same lists; each set's reach is documented there.

// rules wraps a natsgrant grant as the rules of a User: its allow lists, and its deny lists, which
// every set natsgrant builds today leaves empty, so a hub's rendered users are unchanged by them.
func rules(grant natsgrant.Permissions) Permissions {
	return Permissions{
		Publish:   Rules{Allow: grant.Publish, Deny: grant.PublishDeny},
		Subscribe: Rules{Allow: grant.Subscribe, Deny: grant.SubscribeDeny},
	}
}

// ListenerPermissions is the grant envoy-listener needs as machine machineID (natsgrant.Listener),
// the session consumers' calls included.
func ListenerPermissions(machineID string) Permissions {
	return rules(natsgrant.Listener(machineID))
}

// ListenerHostPermissions is the grant of a listener's own NATS user, one no session shares, as
// machine machineID (natsgrant.ListenerHost): ListenerPermissions and a subscribe to every
// notification topic. HostPermissions stays the set of a user a host's sessions share.
func ListenerHostPermissions(machineID string) Permissions {
	return rules(natsgrant.ListenerHost(machineID))
}

// DispatchDurables are the fixed durable consumers of the stream that Dispatch's grant lets it
// bind, beside the listener durable of its machine id (natsgrant.DispatchDurables).
func DispatchDurables() []string {
	return natsgrant.DispatchDurables()
}

// DispatchPermissions is the grant of Dispatch's one NATS user, which envoy-dispatch and the
// envoy-listener beside it share (natsgrant.Dispatch).
func DispatchPermissions(machineID string) Permissions {
	return rules(natsgrant.Dispatch(machineID))
}

// SessionPermissions is the grant any agent session needs, with * for the session id
// (natsgrant.SessionPattern): the user a hub renders for the sessions that share it. Every session
// on that user holds the same set, so one session can read another's relay frames; a session the
// auth service admits holds natsgrant.Session instead, which names its own id alone.
func SessionPermissions() Permissions {
	return rules(natsgrant.SessionPattern())
}

// DaemonPermissions is the NATS grant one Legion daemon needs (natsgrant.Daemon), which the
// daemon's real-NATS proof also reads, so the proof and the hub's rendered user share it.
func DaemonPermissions() Permissions {
	return rules(natsgrant.Daemon())
}

// HostPermissions is the grant of a host's one NATS user, which its listener and every session on
// the host share (natsgrant.Host).
func HostPermissions(machineID string) Permissions {
	return rules(natsgrant.Host(machineID))
}
