package natsgrant_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
)

// listenerDeny is the publish deny of every grant built on the listener grant: Dispatch's lane,
// which Dispatch's user alone publishes, and GitHub's lane, which the user of the listener that
// receives GitHub's webhook alone publishes. Each of those grants takes its own lane off.
var listenerDeny = []string{"notifications.dispatch.>", "notifications.github.>"}

// publishes reports whether a user holding grant may publish subject, as nats-server judges it: an
// allow entry matches the subject, and no deny entry does.
func publishes(grant natsgrant.Permissions, subject string) bool {
	matches := func(patterns []string) bool {
		return slices.ContainsFunc(patterns, func(pattern string) bool { return bus.SubjectContains(pattern, subject) })
	}
	return matches(grant.Publish) && !matches(grant.PublishDeny)
}

// Check l5 of dispatch://INFRA-3589, over natsgrant's own grants, which a hub renders and the auth
// service answers a connection with: only Dispatch's NATS user may publish directly on Dispatch's
// lane, and only the user of the listener that receives GitHub's webhook may publish directly on
// GitHub's lane. The rows are infra/core's lanes_test.go rows, each set named by the natsgrant
// grant core wraps, then the rows of WebhookListener, the webhook listener's grant as the auth
// service answers it. A session on a shared host user can still have a message stored under either
// lane, by a stream update that adds a subject transform, until the host's sessions connect through
// the auth service (dispatch://INFRA-840); this test checks the deny alone. Every other grant built
// on the listener grant still publishes the rest of notifications.>, a role lane among them.
// Dispatch's user may not publish the listener's resume lane, and still publishes the kept lane,
// the wake topic and a session's agent subject. The listener sets keep the resume and kept lanes.
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
		grant   func(string) natsgrant.Permissions
		subject string
		publish bool
	}{
		{"Listener may not publish Dispatch's lane", natsgrant.Listener, dispatch, false},
		{"Listener may publish a role lane", natsgrant.Listener, role, true},
		{"ListenerHost may not publish Dispatch's lane", natsgrant.ListenerHost, dispatch, false},
		{"ListenerHost may publish a role lane", natsgrant.ListenerHost, role, true},
		{"Host may not publish Dispatch's lane", natsgrant.Host, dispatch, false},
		{"Host may publish a role lane", natsgrant.Host, role, true},
		{"Dispatch may publish Dispatch's lane", natsgrant.Dispatch, dispatch, true},
		{"the webhook listener's grant may publish GitHub's lane", natsgrant.WebhookListenerHost, github, true},
		{"the webhook listener's grant may not publish Dispatch's lane", natsgrant.WebhookListenerHost, dispatch, false},
		{"Listener may not publish GitHub's lane", natsgrant.Listener, github, false},
		{"ListenerHost may not publish GitHub's lane", natsgrant.ListenerHost, github, false},
		{"Dispatch may not publish GitHub's lane", natsgrant.Dispatch, github, false},
		{"Host may not publish GitHub's lane", natsgrant.Host, github, false},
		{"Dispatch may not publish the resume lane", natsgrant.Dispatch, resume, false},
		{"Dispatch may publish the kept lane, which Dispatch publishes", natsgrant.Dispatch, kept, true},
		{"Dispatch may publish the wake topic, which Dispatch publishes", natsgrant.Dispatch, wake, true},
		{"Dispatch may publish a session's agent subject, on which Dispatch queues a person's message", natsgrant.Dispatch, agent, true},
		{"Listener may publish the resume lane", natsgrant.Listener, resume, true},
		{"Listener may publish the kept lane", natsgrant.Listener, kept, true},
		{"ListenerHost may publish the resume lane", natsgrant.ListenerHost, resume, true},
		{"ListenerHost may publish the kept lane", natsgrant.ListenerHost, kept, true},
		{"WebhookListener, the auth service's webhook listener grant, may publish GitHub's lane", natsgrant.WebhookListener, github, true},
		{"WebhookListener may not publish Dispatch's lane", natsgrant.WebhookListener, dispatch, false},
		{"WebhookListener may publish the resume lane", natsgrant.WebhookListener, resume, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			grant := row.grant(machine)
			if got := publishes(grant, row.subject); got != row.publish {
				t.Fatalf("publishes %s = %t, want %t (publish deny %q)", row.subject, got, row.publish, grant.PublishDeny)
			}
		})
	}
}

// Each grant's deny lists, element for element, for each machine id the merge-base fixture names.
// TestEachGrantMatchesTheSetItReplaces compares the allow lists alone, and the lane deny adds deny
// entries alone, so these are the deny's own assertions: the listener grant's deny in every grant
// built on it, the lane of each lane's one publisher taken off its own, the resume lane added to
// Dispatch's, and no deny in a session's, the daemon's or the measurement job's grant. The webhook
// listener's grants differ from the listener's in that deny alone.
func TestEachGrantDeniesTheLanesItDoesNotPublish(t *testing.T) {
	for _, id := range []string{"example-host-a", "example-dispatch", "example-hub"} {
		for name, row := range map[string]struct {
			grant natsgrant.Permissions
			deny  []string
		}{
			"Listener":            {natsgrant.Listener(id), listenerDeny},
			"ListenerHost":        {natsgrant.ListenerHost(id), listenerDeny},
			"Host":                {natsgrant.Host(id), listenerDeny},
			"WebhookListener":     {natsgrant.WebhookListener(id), []string{"notifications.dispatch.>"}},
			"WebhookListenerHost": {natsgrant.WebhookListenerHost(id), []string{"notifications.dispatch.>"}},
			"Dispatch":            {natsgrant.Dispatch(id), []string{"notifications.github.>", "notifications.envoy.resume.>"}},
		} {
			require.Equal(t, row.deny, row.grant.PublishDeny, "%s(%q) publish deny", name, id)
			require.Empty(t, row.grant.SubscribeDeny, "%s(%q) subscribe deny", name, id)
		}
		for name, pair := range map[string][2]natsgrant.Permissions{
			"WebhookListener":     {natsgrant.Listener(id), natsgrant.WebhookListener(id)},
			"WebhookListenerHost": {natsgrant.ListenerHost(id), natsgrant.WebhookListenerHost(id)},
		} {
			require.Equal(t, pair[0].Publish, pair[1].Publish, "%s(%q) publish", name, id)
			require.Equal(t, pair[0].Subscribe, pair[1].Subscribe, "%s(%q) subscribe", name, id)
		}
	}
	session, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	for name, grant := range map[string]natsgrant.Permissions{
		"SessionPattern()": natsgrant.SessionPattern(),
		"Session(ses_a)":   session,
		"Daemon()":         natsgrant.Daemon(),
		"Measure()":        natsgrant.Measure(),
	} {
		require.Empty(t, grant.PublishDeny, "%s publish deny", name)
		require.Empty(t, grant.SubscribeDeny, "%s subscribe deny", name)
	}
}

// Each call builds its deny lists afresh: a caller that edits one grant's deny changes no later
// call's, and a grant that takes a lane off its own deny (WebhookListenerHost, Dispatch) changes
// no other grant's.
func TestADenyIsTheCallersToChange(t *testing.T) {
	const id = "example-host-a"
	host := natsgrant.Host(id)
	host.PublishDeny[0] = "edited"
	require.Equal(t, listenerDeny, natsgrant.Host(id).PublishDeny)
	dispatch := natsgrant.Dispatch(id)
	dispatch.PublishDeny[0] = "edited"
	require.Equal(t, []string{"notifications.github.>", "notifications.envoy.resume.>"}, natsgrant.Dispatch(id).PublishDeny)
	webhook := natsgrant.WebhookListenerHost(id)
	webhook.PublishDeny[0] = "edited"
	require.Equal(t, []string{"notifications.dispatch.>"}, natsgrant.WebhookListenerHost(id).PublishDeny)
	require.Equal(t, listenerDeny, natsgrant.ListenerHost(id).PublishDeny)
	require.Equal(t, listenerDeny, natsgrant.Listener(id).PublishDeny)
}
