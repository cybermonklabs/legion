package core_test

import (
	"slices"
	"testing"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// webhookListenerGrant is the grant of the user of the listener that receives GitHub's webhook.
// permissions_test.go sets it, so this file names no grant a commit before that one lacks: there
// this file builds, and the rows that need the grant fail.
var webhookListenerGrant func(machineID string) core.Permissions

// publishes reports whether a user holding r may publish subject, as nats-server judges it: an
// allow entry matches the subject, and no deny entry does.
func publishes(r core.Rules, subject string) bool {
	matches := func(patterns []string) bool {
		return slices.ContainsFunc(patterns, func(pattern string) bool { return bus.SubjectContains(pattern, subject) })
	}
	return matches(r.Allow) && !matches(r.Deny)
}

// Check l5 of dispatch://INFRA-3589: only Dispatch's NATS user may publish directly on Dispatch's
// lane, and only the user of the listener that receives GitHub's webhook may publish directly on
// GitHub's lane. The Legion daemon trusts an event on either lane as its source's. The grants are
// natsgrant's: this test reads them through core's wrapper, as a hub renders them, and
// natsgrant's own TestOnlyDispatchMayPublishItsLane holds the same rows over natsgrant. A session
// on a shared host user can still have a message stored under either lane, by a stream update that
// adds a subject transform, until the host's sessions connect through the auth service
// (dispatch://INFRA-840); this test checks the deny alone. Every other set built on the listener
// grant still publishes the rest of notifications.>, a role lane among them. Dispatch's user may
// not publish the listener's resume lane, which only a listener holding the session consumers'
// calls writes; it still publishes the kept lane and the wake topic, as the outbox and the
// listener beside envoy-dispatch do, and a session's agent subject, on which Dispatch queues a
// person's message for a session no store of the listener's holds
// (internal/dispatch/api/message_queue_publish.go). The listener sets keep the resume and kept
// lanes.
func TestOnlyDispatchMayPublishItsLane(t *testing.T) {
	const (
		machine  = "example-host-a"
		dispatch = "notifications.dispatch.issue.X"
		github   = "notifications.github.X"
		role     = "notifications.role.X"
		resume   = "notifications.envoy.resume.X"
		kept     = "notifications.envoy.kept.role.X"
		wake     = "notifications.legion.wake"
		agent    = "notifications.agent.ses_X"
	)
	for _, row := range []struct {
		name    string
		grant   func(string) core.Permissions
		subject string
		publish bool
	}{
		{"ListenerPermissions may not publish Dispatch's lane", core.ListenerPermissions, dispatch, false},
		{"ListenerPermissions may publish a role lane", core.ListenerPermissions, role, true},
		{"ListenerHostPermissions may not publish Dispatch's lane", core.ListenerHostPermissions, dispatch, false},
		{"ListenerHostPermissions may publish a role lane", core.ListenerHostPermissions, role, true},
		{"HostPermissions may not publish Dispatch's lane", core.HostPermissions, dispatch, false},
		{"HostPermissions may publish a role lane", core.HostPermissions, role, true},
		{"DispatchPermissions may publish Dispatch's lane", core.DispatchPermissions, dispatch, true},
		{"the webhook listener's grant may publish GitHub's lane", webhookListenerGrant, github, true},
		{"the webhook listener's grant may not publish Dispatch's lane", webhookListenerGrant, dispatch, false},
		{"ListenerPermissions may not publish GitHub's lane", core.ListenerPermissions, github, false},
		{"ListenerHostPermissions may not publish GitHub's lane", core.ListenerHostPermissions, github, false},
		{"DispatchPermissions may not publish GitHub's lane", core.DispatchPermissions, github, false},
		{"HostPermissions may not publish GitHub's lane", core.HostPermissions, github, false},
		{"DispatchPermissions may not publish the resume lane", core.DispatchPermissions, resume, false},
		{"DispatchPermissions may publish the kept lane, which Dispatch publishes", core.DispatchPermissions, kept, true},
		{"DispatchPermissions may publish the wake topic, which Dispatch publishes", core.DispatchPermissions, wake, true},
		{"DispatchPermissions may publish a session's agent subject, on which Dispatch queues a person's message", core.DispatchPermissions, agent, true},
		{"ListenerPermissions may publish the resume lane", core.ListenerPermissions, resume, true},
		{"ListenerPermissions may publish the kept lane", core.ListenerPermissions, kept, true},
		{"ListenerHostPermissions may publish the resume lane", core.ListenerHostPermissions, resume, true},
		{"ListenerHostPermissions may publish the kept lane", core.ListenerHostPermissions, kept, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.grant == nil {
				t.Fatal("core has no grant for the listener that receives GitHub's webhook")
			}
			rules := row.grant(machine).Publish
			if got := publishes(rules, row.subject); got != row.publish {
				t.Fatalf("publishes %s = %t, want %t (publish deny %q)", row.subject, got, row.publish, rules.Deny)
			}
		})
	}
}
