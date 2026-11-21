package core_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// listenerBasePublish is the publish grant every user that runs a listener holds for
// example-host-a, written out by hand from Envoy's names as they stand: every notification topic
// (the stream's subjects, the role lanes, every exception lane and any ad-hoc topic) but the two
// lanes listenerDeny names, the stream (its stored-message get, `bus.Client.NextStoredMessage`)
// and its own durable, then each bucket in turn, then the interest bucket's purge.
// HostPermissions and DispatchPermissions are built on it, so neither holds a session consumer's
// call. TestSessionPermissions's exact list holds none of it: no session gets the stored-message
// read or a consumer.
var listenerBasePublish = []string{
	"notifications.>",
	"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS",
	"$JS.API.STREAM.CREATE.ENVOY_NOTIFICATIONS",
	"$JS.API.STREAM.UPDATE.ENVOY_NOTIFICATIONS",
	"$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS",
	"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.listener-example-host-a",
	"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.listener-example-host-a",
	"$JS.ACK.ENVOY_NOTIFICATIONS.listener-example-host-a.>",
	"$JS.API.INFO",
	"$JS.API.STREAM.INFO.KV_envoy_interests",
	"$JS.API.STREAM.CREATE.KV_envoy_interests",
	"$JS.API.STREAM.UPDATE.KV_envoy_interests",
	"$JS.API.DIRECT.GET.KV_envoy_interests.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_interests",
	"$KV.envoy_interests.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_interests.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_interests.>",
	"$JS.FC.KV_envoy_interests.>",
	"$JS.API.STREAM.INFO.KV_envoy_roles",
	"$JS.API.STREAM.CREATE.KV_envoy_roles",
	"$JS.API.STREAM.UPDATE.KV_envoy_roles",
	"$JS.API.DIRECT.GET.KV_envoy_roles.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_roles",
	"$KV.envoy_roles.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_roles.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_roles.>",
	"$JS.FC.KV_envoy_roles.>",
	"$JS.API.STREAM.INFO.KV_envoy_sessions",
	"$JS.API.STREAM.CREATE.KV_envoy_sessions",
	"$JS.API.STREAM.UPDATE.KV_envoy_sessions",
	"$JS.API.DIRECT.GET.KV_envoy_sessions.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_sessions",
	"$KV.envoy_sessions.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_sessions.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_sessions.>",
	"$JS.FC.KV_envoy_sessions.>",
	"$JS.API.STREAM.INFO.KV_envoy_known_sessions",
	"$JS.API.STREAM.CREATE.KV_envoy_known_sessions",
	"$JS.API.STREAM.UPDATE.KV_envoy_known_sessions",
	"$JS.API.DIRECT.GET.KV_envoy_known_sessions.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_known_sessions",
	"$KV.envoy_known_sessions.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_known_sessions.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_known_sessions.>",
	"$JS.FC.KV_envoy_known_sessions.>",
	"$JS.API.STREAM.INFO.KV_legion_agents",
	"$JS.API.STREAM.CREATE.KV_legion_agents",
	"$JS.API.STREAM.UPDATE.KV_legion_agents",
	"$JS.API.DIRECT.GET.KV_legion_agents.>",
	"$JS.API.STREAM.MSG.GET.KV_legion_agents",
	"$KV.legion_agents.>",
	"$JS.API.CONSUMER.CREATE.KV_legion_agents.>",
	"$JS.API.CONSUMER.DELETE.KV_legion_agents.>",
	"$JS.FC.KV_legion_agents.>",
	"$JS.API.STREAM.INFO.KV_envoy_ci_state",
	"$JS.API.STREAM.CREATE.KV_envoy_ci_state",
	"$JS.API.STREAM.UPDATE.KV_envoy_ci_state",
	"$JS.API.DIRECT.GET.KV_envoy_ci_state.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_ci_state",
	"$KV.envoy_ci_state.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_ci_state.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_ci_state.>",
	"$JS.FC.KV_envoy_ci_state.>",
	"$JS.API.STREAM.INFO.KV_envoy_dpop_proofs",
	"$JS.API.STREAM.CREATE.KV_envoy_dpop_proofs",
	"$JS.API.STREAM.UPDATE.KV_envoy_dpop_proofs",
	"$JS.API.DIRECT.GET.KV_envoy_dpop_proofs.>",
	"$JS.API.STREAM.MSG.GET.KV_envoy_dpop_proofs",
	"$KV.envoy_dpop_proofs.>",
	"$JS.API.CONSUMER.CREATE.KV_envoy_dpop_proofs.>",
	"$JS.API.CONSUMER.DELETE.KV_envoy_dpop_proofs.>",
	"$JS.FC.KV_envoy_dpop_proofs.>",
	"$JS.API.STREAM.PURGE.KV_envoy_interests",
}

// sessionConsumerPublish are the session consumers' calls (create with and without a filter
// subject, info, delete, pull and acknowledgements on any consumer of the stream, and the stream's
// consumer list), which ListenerPermissions alone adds to the base grant.
var sessionConsumerPublish = []string{
	"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.*",
	"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.*.>",
	"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.*",
	"$JS.API.CONSUMER.DELETE.ENVOY_NOTIFICATIONS.*",
	"$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.*",
	"$JS.API.CONSUMER.LIST.ENVOY_NOTIFICATIONS",
	"$JS.ACK.ENVOY_NOTIFICATIONS.*.>",
}

// listenerPublish is the listener's own publish grant: the base grant, then the session consumers'
// calls.
var listenerPublish = append(append([]string{}, listenerBasePublish...), sessionConsumerPublish...)

// listenerSubscribe is the listener's subscribe grant for example-host-a: reply inboxes, then the
// role lanes in the machine's queue group and in the one every listener shares.
var listenerSubscribe = []string{"_INBOX.>", "notifications.role.> envoy-listener-example-host-a", "notifications.role.> envoy-roles"}

// listenerDeny is the publish deny of every set built on the base grant: Dispatch's lane, which
// Dispatch's user alone publishes, and GitHub's lane, which the user of the listener that receives
// GitHub's webhook alone publishes. Each of those two sets takes its own lane off.
var listenerDeny = []string{"notifications.dispatch.>", "notifications.github.>"}

func TestListenerPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish:   core.Rules{Allow: listenerPublish, Deny: listenerDeny},
		Subscribe: core.Rules{Allow: listenerSubscribe},
	}, core.ListenerPermissions("example-host-a"))
}

// The listener-host set is the listener's own, with the subscribe to every notification topic
// after the role lanes, which no reader is left for: a user no session shares, so it holds
// none of the session's publish entries.
func TestListenerHostPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish:   core.Rules{Allow: listenerPublish, Deny: listenerDeny},
		Subscribe: core.Rules{Allow: append(append([]string{}, listenerSubscribe...), "notifications.>")},
	}, core.ListenerHostPermissions("example-host-a"))
}

// The webhook listener's set is the listener-host set with GitHub's lane taken off its deny: the
// listener that receives GitHub's webhook publishes every GitHub event, and still no Dispatch event.
func TestWebhookListenerHostPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish:   core.Rules{Allow: listenerPublish, Deny: []string{"notifications.dispatch.>"}},
		Subscribe: core.Rules{Allow: append(append([]string{}, listenerSubscribe...), "notifications.>")},
	}, core.WebhookListenerHostPermissions("example-host-a"))
}

// TestOnlyDispatchMayPublishItsLane (lanes_test.go) reads the webhook listener's set through
// webhookListenerGrant, which is set here, so that its file names no symbol a commit before that
// set lacks: there it builds, and fails.
func init() { webhookListenerGrant = core.WebhookListenerHostPermissions }

// The one set a listener may hold with consumer delivery and the plain notifications.>
// subscribe both, which no queue group narrows. The plain host set, which a host's sessions
// share, carries that subscribe and no consumer call, so a listener connecting as it refuses
// every registration that asks for consumer delivery by name and writes nothing.
func TestListenerHostPermissionsHoldTheConsumerCallsAndEveryTopic(t *testing.T) {
	grant := core.ListenerHostPermissions("example-host-a")
	require.Subset(t, grant.Publish.Allow, sessionConsumerPublish)
	require.Contains(t, grant.Subscribe.Allow, "notifications.>")
	host := core.HostPermissions("example-host-a")
	require.Contains(t, host.Subscribe.Allow, "notifications.>")
	for _, entry := range sessionConsumerPublish {
		require.NotContains(t, host.Publish.Allow, entry, "the host set publishes %q", entry)
	}
}

// A space in the machine id is a hyphen in the durable and the queue, as the listener names them.
func TestListenerPermissionsNameTheDurableAndQueueOfAMachineIDWithASpace(t *testing.T) {
	p := core.ListenerPermissions("example host")
	require.Contains(t, p.Publish.Allow, "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.listener-example-host")
	require.Contains(t, p.Publish.Allow, "$JS.ACK.ENVOY_NOTIFICATIONS.listener-example-host.>")
	require.Contains(t, p.Subscribe.Allow, "notifications.role.> envoy-listener-example-host")
}

// A session subscribes to its own relay subject and its agent stream's control subject, and to no
// notification topic that carries a message: a notification reaches it as a relay frame of its own
// consumer, which the listener owning the session publishes there, or not at all.
func TestSessionPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish: core.Rules{Allow: []string{"_INBOX.>", "agentstream.*.frames"}},
		Subscribe: core.Rules{Allow: []string{
			"notifications.envoy.deliver.*",
			"agentstream.*.control",
		}},
	}, core.SessionPermissions())
}

// A user holding SessionPermissions alone publishes a reply and never subscribes to a reply inbox,
// so it may answer a request but not make one: sessions that share one user holding the set would
// otherwise each receive every other session's replies. The rules are read from the rendered block,
// as the server reads them. A host's user subscribes to the inboxes for its listener, which the same
// check finds.
func TestSessionPermissionsRefuseASubscribeToReplyInboxes(t *testing.T) {
	publish, subscribe := renderedRules(t, core.SessionPermissions())
	require.Contains(t, publish, "_INBOX.>", "a session publishes the receipt of each relay frame")
	for _, entry := range subscribe {
		require.False(t, admitsInbox(entry), "the session's subscribe entry %q admits a reply inbox", entry)
	}
	_, host := renderedRules(t, core.HostPermissions("example-host-a"))
	require.True(t, slices.ContainsFunc(host, admitsInbox), "the host's set subscribes to the inboxes, for its listener")
}

// renderedRules renders one user holding p and returns the publish and subscribe allow lists its
// line of the block holds.
func renderedRules(t *testing.T, p core.Permissions) (publish, subscribe []string) {
	t.Helper()
	const key = "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO"
	text, err := core.Authorization{Users: []core.User{{
		Name: "example-session", NKey: key, Publish: &p.Publish, Subscribe: &p.Subscribe, HasSeed: true,
	}}}.Render(nil)
	require.NoError(t, err)
	var user struct {
		Permissions struct {
			Publish, Subscribe struct{ Allow []string }
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, `"nkey": "`+key+`"`) {
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(line)), &user), line)
			return user.Permissions.Publish.Allow, user.Permissions.Subscribe.Allow
		}
	}
	t.Fatalf("the rendered block holds no line for the user:\n%s", text)
	return nil, nil
}

// admitsInbox reports whether a subscribe allow entry, a subject pattern and an optional queue
// group after a space, admits any subject under _INBOX.: its first token is _INBOX, * or >.
func admitsInbox(entry string) bool {
	first, _, _ := strings.Cut(strings.Fields(entry)[0], ".")
	return first == "_INBOX" || first == "*" || first == ">"
}

// The host's user runs a listener and holds every session's entries, and no session consumer's
// call: a listener connecting as it keeps its own durable alone. Its notifications.> subscribe is
// the set's own, and it admits the session's relay subject, which is why the union adds no entry
// for it. It keeps the base grant's publish deny, so no session on the host publishes directly on
// Dispatch's lane or GitHub's. A session on the host can still have a message stored under either
// lane, by a stream update that adds a subject transform, until the host's sessions connect
// through the auth service (dispatch://INFRA-840); this test checks the deny alone.
func TestHostPermissionsAreTheListenersThenTheSessions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish: core.Rules{Allow: append(append([]string{}, listenerBasePublish...), "_INBOX.>", "agentstream.*.frames"), Deny: listenerDeny},
		Subscribe: core.Rules{Allow: append(append([]string{}, listenerSubscribe...),
			"notifications.>", "agentstream.*.control",
		)},
	}, core.HostPermissions("example-host-a"))
}

// Narrowing the session's set to its relay subject does not take notifications.> from the host's
// or the listener-host's: both name it themselves. It is the reach the sessions sharing a host
// user keep until dispatch://INFRA-840 gives each session a credential of its own, and the host
// listener's own reach over the topics it serves.
func TestHostPermissionsKeepTheSubscribeToEveryTopicTheSessionsLost(t *testing.T) {
	require.NotContains(t, core.SessionPermissions().Subscribe.Allow, "notifications.>")
	require.Contains(t, core.HostPermissions("example-host-a").Subscribe.Allow, "notifications.>")
	require.Contains(t, core.ListenerHostPermissions("example-host-a").Subscribe.Allow, "notifications.>")
}

// Dispatch's user is built on the same base grant, so it holds its own durables' per-durable
// entries and no session consumer's call. Its publish deny is the base grant's with Dispatch's
// lane taken off, envoy-dispatch publishing every event there and nothing on GitHub's lane, then
// the listener's resume lane, which only a listener holding the session consumers' calls writes.
func TestDispatchPermissions(t *testing.T) {
	dispatchPublish := append(append([]string{}, listenerBasePublish...),
		"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.delivery-events",
		"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.>",
		"$JS.API.CONSUMER.DELETE.ENVOY_NOTIFICATIONS.delivery-events",
		"$JS.ACK.ENVOY_NOTIFICATIONS.delivery-events.>",
		"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.delivery-jobs",
		"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-jobs.>",
		"$JS.API.CONSUMER.DELETE.ENVOY_NOTIFICATIONS.delivery-jobs",
		"$JS.ACK.ENVOY_NOTIFICATIONS.delivery-jobs.>",
		"$JS.API.STREAM.INFO.KV_envoy_webhook_redelivery",
		"$JS.API.STREAM.CREATE.KV_envoy_webhook_redelivery",
		"$JS.API.STREAM.UPDATE.KV_envoy_webhook_redelivery",
		"$JS.API.DIRECT.GET.KV_envoy_webhook_redelivery.>",
		"$JS.API.STREAM.MSG.GET.KV_envoy_webhook_redelivery",
		"$KV.envoy_webhook_redelivery.>",
		"$JS.API.CONSUMER.CREATE.KV_envoy_webhook_redelivery.>",
		"$JS.API.CONSUMER.DELETE.KV_envoy_webhook_redelivery.>",
		"$JS.FC.KV_envoy_webhook_redelivery.>",
		"agentstream.*.control",
	)
	want := core.Permissions{
		Publish: core.Rules{Allow: dispatchPublish, Deny: []string{"notifications.github.>", "notifications.envoy.resume.>"}},
		Subscribe: core.Rules{Allow: append(append([]string{}, listenerSubscribe...),
			"agentstream.*.frames",
		)},
	}
	got := core.DispatchPermissions("example-host-a")
	require.Equal(t, want, got)
}

// The session consumers' calls belong to the sets of a user no session shares, the listener's and
// the listener-host: a user built on the same base grant, Dispatch's or a host's, holds none of
// them, nor does a session's, so a listener connecting as one of those users keeps its own durable
// alone and serves its sessions over core NATS.
func TestOnlyTheListenersOwnSetsHoldTheSessionConsumersCalls(t *testing.T) {
	require.Subset(t, core.ListenerPermissions("example-host-a").Publish.Allow, sessionConsumerPublish)
	require.Subset(t, core.ListenerHostPermissions("example-host-a").Publish.Allow, sessionConsumerPublish)
	require.Subset(t, core.WebhookListenerHostPermissions("example-host-a").Publish.Allow, sessionConsumerPublish)
	for name, p := range map[string]core.Permissions{
		"host":     core.HostPermissions("example-host-a"),
		"dispatch": core.DispatchPermissions("example-host-a"),
		"session":  core.SessionPermissions(),
	} {
		for _, entry := range sessionConsumerPublish {
			require.NotContains(t, p.Publish.Allow, entry, "the %s set publishes %q", name, entry)
		}
	}
}

// nats-server takes a repeated entry, so a duplicate would be noise in every rendered block; none of
// the sets holds one, the union included.
func TestNoSetRepeatsAnEntry(t *testing.T) {
	sets := map[string]core.Permissions{
		"listener":              core.ListenerPermissions("example-host-a"),
		"listener host":         core.ListenerHostPermissions("example-host-a"),
		"webhook listener host": core.WebhookListenerHostPermissions("example-host-a"),
		"session":               core.SessionPermissions(),
		"host":                  core.HostPermissions("example-host-a"),
		"daemon":                core.DaemonPermissions(),
		"dispatch":              core.DispatchPermissions("example-host-a"),
	}
	for name, p := range sets {
		for direction, entries := range map[string][]string{
			"publish": p.Publish.Allow, "publish deny": p.Publish.Deny, "subscribe": p.Subscribe.Allow,
		} {
			seen := map[string]bool{}
			for _, entry := range entries {
				require.False(t, seen[entry], "%s %s repeats %q", name, direction, entry)
				seen[entry] = true
			}
		}
	}
}

// Each call returns lists of its own, so a caller that edits one, as a consumer narrowing a grant
// does, changes no later call's.
func TestASetIsTheCallersToChange(t *testing.T) {
	first := core.HostPermissions("example-host-a")
	first.Publish.Allow[0] = "edited"
	first.Publish.Deny[0] = "edited"
	first.Subscribe.Allow[0] = "edited"
	second := core.HostPermissions("example-host-a")
	require.Equal(t, "notifications.>", second.Publish.Allow[0])
	require.Equal(t, "notifications.dispatch.>", second.Publish.Deny[0])
	require.Equal(t, "_INBOX.>", second.Subscribe.Allow[0])
	session := core.SessionPermissions()
	session.Publish.Allow[0] = "edited"
	require.Equal(t, "_INBOX.>", core.SessionPermissions().Publish.Allow[0])
	daemon := core.DaemonPermissions()
	daemon.Publish.Allow[0] = "edited"
	daemon.Subscribe.Allow[0] = "edited"
	require.Equal(t, "$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS", core.DaemonPermissions().Publish.Allow[0])
	require.Equal(t, "notifications.envoy.exceptions.notifications.role.>", core.DaemonPermissions().Subscribe.Allow[0])
	dispatch := core.DispatchPermissions("example-host-a")
	dispatch.Publish.Allow[0] = "edited"
	dispatch.Publish.Deny[0] = "edited"
	dispatch.Subscribe.Allow[0] = "edited"
	again := core.DispatchPermissions("example-host-a")
	require.Equal(t, "notifications.>", again.Publish.Allow[0])
	require.Equal(t, []string{"notifications.github.>", "notifications.envoy.resume.>"}, again.Publish.Deny)
	require.Equal(t, "_INBOX.>", again.Subscribe.Allow[0])
	listenerHost := core.ListenerHostPermissions("example-host-a")
	listenerHost.Publish.Allow[0] = "edited"
	listenerHost.Publish.Deny[0] = "edited"
	listenerHost.Subscribe.Allow[0] = "edited"
	own := core.ListenerHostPermissions("example-host-a")
	require.Equal(t, "notifications.>", own.Publish.Allow[0])
	require.Equal(t, listenerDeny, own.Publish.Deny)
	require.Equal(t, "_INBOX.>", own.Subscribe.Allow[0])
	// The webhook listener's set takes GitHub's lane off a deny of its own: the next call of
	// either listener-host set still has its whole deny.
	webhook := core.WebhookListenerHostPermissions("example-host-a")
	webhook.Publish.Deny[0] = "edited"
	require.Equal(t, []string{"notifications.dispatch.>"}, core.WebhookListenerHostPermissions("example-host-a").Publish.Deny)
	require.Equal(t, listenerDeny, core.ListenerHostPermissions("example-host-a").Publish.Deny)
}

// A set drops into a User as its rules, and the authorization renders it.
func TestASetIsAUsersRules(t *testing.T) {
	p := core.HostPermissions("example-host-a")
	a := core.Authorization{Users: []core.User{{
		Name:      "example-host-a",
		NKey:      "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
		Publish:   &p.Publish,
		Subscribe: &p.Subscribe,
		HasSeed:   true,
	}}}
	_, err := a.Render(nil)
	require.NoError(t, err)
}

// The expected daemon grant is written out literally rather than built from Envoy's subject names,
// so a change to the grant or to the names it is built from fails here until reviewed.
func TestDaemonPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish: core.Rules{Allow: []string{
			"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS",
			"$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS",
			"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>",
			"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>",
			"$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>",
			"$JS.ACK.ENVOY_NOTIFICATIONS.>",
		}},
		Subscribe: core.Rules{Allow: []string{
			"notifications.envoy.exceptions.notifications.role.>",
			"_INBOX.>",
		}},
	}, core.DaemonPermissions())
}
