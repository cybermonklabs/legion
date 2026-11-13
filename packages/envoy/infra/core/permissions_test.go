package core_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/legiontasks"
)

// listenerBasePublish is the publish grant every user that runs a listener holds for
// example-host-a, written out by hand from Envoy's names as they stand: every notification topic
// (the stream's subjects, the role lanes, every exception lane and any ad-hoc topic), the stream
// (its stored-message get, `bus.Client.NextStoredMessage`) and its own durable, then each bucket
// in turn, then the interest bucket's purge. HostPermissions and DispatchPermissions are built on
// it, so neither holds a session consumer's call. TestSessionPermissions's exact list holds none
// of it: no session gets the stored-message read or a consumer.
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

// taskBucketReadPublish are the reads of the Legion daemons' task bucket that the listener's task
// relay makes (its stream's info, its direct and stored-message gets, its watch's consumer create
// and delete, and their flow-control replies), which ListenerPermissions alone adds after the
// session consumers' calls: no create, update or write of the bucket, whose one writer is the
// daemon.
var taskBucketReadPublish = []string{
	"$JS.API.STREAM.INFO.KV_legion_tasks",
	"$JS.API.DIRECT.GET.KV_legion_tasks.>",
	"$JS.API.STREAM.MSG.GET.KV_legion_tasks",
	"$JS.API.CONSUMER.CREATE.KV_legion_tasks.>",
	"$JS.API.CONSUMER.DELETE.KV_legion_tasks.>",
	"$JS.FC.KV_legion_tasks.>",
}

// listenerPublish is the listener's own publish grant: the base grant, then the session consumers'
// calls, then the task bucket's reads.
var listenerPublish = append(append(append([]string{}, listenerBasePublish...), sessionConsumerPublish...), taskBucketReadPublish...)

// listenerSubscribe is the listener's subscribe grant for example-host-a: reply inboxes, then the
// role lanes in the machine's queue group and in the one every listener shares.
var listenerSubscribe = []string{"_INBOX.>", "notifications.role.> envoy-listener-example-host-a", "notifications.role.> envoy-roles"}

func TestListenerPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish:   core.Rules{Allow: listenerPublish},
		Subscribe: core.Rules{Allow: listenerSubscribe},
	}, core.ListenerPermissions("example-host-a"))
}

// The listener-host set is the listener's own, with the subscribe to every notification topic
// after the role lanes, which no reader is left for: a user no session shares, so it holds
// none of the session's publish entries.
func TestListenerHostPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish:   core.Rules{Allow: listenerPublish},
		Subscribe: core.Rules{Allow: append(append([]string{}, listenerSubscribe...), "notifications.>")},
	}, core.ListenerHostPermissions("example-host-a"))
}

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
// for it.
func TestHostPermissionsAreTheListenersThenTheSessions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish: core.Rules{Allow: append(append([]string{}, listenerBasePublish...), "_INBOX.>", "agentstream.*.frames")},
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
// entries and no session consumer's call.
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
		Publish: core.Rules{Allow: dispatchPublish},
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
		"listener":      core.ListenerPermissions("example-host-a"),
		"listener host": core.ListenerHostPermissions("example-host-a"),
		"session":       core.SessionPermissions(),
		"host":          core.HostPermissions("example-host-a"),
		"daemon":        core.DaemonPermissions(),
		"dispatch":      core.DispatchPermissions("example-host-a"),
	}
	for name, p := range sets {
		for direction, allow := range map[string][]string{"publish": p.Publish.Allow, "subscribe": p.Subscribe.Allow} {
			seen := map[string]bool{}
			for _, entry := range allow {
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
	first.Subscribe.Allow[0] = "edited"
	second := core.HostPermissions("example-host-a")
	require.Equal(t, "notifications.>", second.Publish.Allow[0])
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
	dispatch.Subscribe.Allow[0] = "edited"
	again := core.DispatchPermissions("example-host-a")
	require.Equal(t, "notifications.>", again.Publish.Allow[0])
	require.Equal(t, "_INBOX.>", again.Subscribe.Allow[0])
	listenerHost := core.ListenerHostPermissions("example-host-a")
	listenerHost.Publish.Allow[0] = "edited"
	listenerHost.Subscribe.Allow[0] = "edited"
	own := core.ListenerHostPermissions("example-host-a")
	require.Equal(t, "notifications.>", own.Publish.Allow[0])
	require.Equal(t, "_INBOX.>", own.Subscribe.Allow[0])
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
// so a change to the grant or to the names it is built from fails here until reviewed: the
// notification stream's calls, then JetStream's account info and the task bucket's, which the
// daemon creates, reads, writes and watches.
func TestDaemonPermissions(t *testing.T) {
	require.Equal(t, core.Permissions{
		Publish: core.Rules{Allow: []string{
			"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS",
			"$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS",
			"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.>",
			"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.>",
			"$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.>",
			"$JS.ACK.ENVOY_NOTIFICATIONS.>",
			"$JS.API.INFO",
			"$JS.API.STREAM.INFO.KV_legion_tasks",
			"$JS.API.STREAM.CREATE.KV_legion_tasks",
			"$JS.API.STREAM.UPDATE.KV_legion_tasks",
			"$JS.API.DIRECT.GET.KV_legion_tasks.>",
			"$JS.API.STREAM.MSG.GET.KV_legion_tasks",
			"$KV.legion_tasks.>",
			"$JS.API.CONSUMER.CREATE.KV_legion_tasks.>",
			"$JS.API.CONSUMER.DELETE.KV_legion_tasks.>",
			"$JS.FC.KV_legion_tasks.>",
		}},
		Subscribe: core.Rules{Allow: []string{
			"notifications.envoy.exceptions.notifications.role.>",
			"_INBOX.>",
		}},
	}, core.DaemonPermissions())
}

// taskBucketCalls are subjects of every call a client makes on the task bucket, one of each: its
// stream's info, create and update, a key's direct and stored-message gets, a key's write or
// delete, a watch's consumer create and delete, and a flow-control reply.
var taskBucketCalls = []string{
	"$JS.API.STREAM.INFO.KV_legion_tasks",
	"$JS.API.STREAM.CREATE.KV_legion_tasks",
	"$JS.API.STREAM.UPDATE.KV_legion_tasks",
	"$JS.API.DIRECT.GET.KV_legion_tasks.$KV.legion_tasks.legion-example-infra-1-implementer",
	"$JS.API.STREAM.MSG.GET.KV_legion_tasks",
	"$KV.legion_tasks.legion-example-infra-1-implementer",
	"$JS.API.CONSUMER.CREATE.KV_legion_tasks.example.$KV.legion_tasks.>",
	"$JS.API.CONSUMER.DELETE.KV_legion_tasks.example",
	"$JS.FC.KV_legion_tasks.example.1.2",
}

// subjectAdmits reports whether pattern, a grant's entry with any queue group after a space cut
// off, admits subject as NATS matches them: token by token, `*` taking any one token and a last
// `>` the one or more that are left.
func subjectAdmits(pattern, subject string) bool {
	want, got := strings.Split(strings.Fields(pattern)[0], "."), strings.Split(subject, ".")
	for i, token := range want {
		if token == ">" && i == len(want)-1 {
			return len(got) > i
		}
		if i >= len(got) || (token != "*" && token != got[i]) {
			return false
		}
	}
	return len(want) == len(got)
}

// requireNoTaskBucketGrant requires that no entry of p names the task bucket or its stream and
// that no publish entry admits any call on the bucket, a wildcard's included.
func requireNoTaskBucketGrant(t *testing.T, p core.Permissions) {
	t.Helper()
	for _, entry := range append(append([]string{}, p.Publish.Allow...), p.Subscribe.Allow...) {
		require.NotContains(t, entry, "legion_tasks", "the entry %q names the task bucket", entry)
	}
	for _, entry := range p.Publish.Allow {
		for _, call := range taskBucketCalls {
			require.False(t, subjectAdmits(entry, call), "the publish entry %q admits %q", entry, call)
		}
	}
}

// The matcher the no-grant tests read every entry with admits what NATS admits, so a set passes
// them for holding no entry that reaches the bucket and not for a matcher that admits nothing.
func TestTheTaskBucketMatcherAdmitsWhatNATSAdmits(t *testing.T) {
	for _, c := range []struct {
		pattern, subject string
		want             bool
	}{
		{"$KV.legion_tasks.>", "$KV.legion_tasks.legion-example-infra-1-implementer", true},
		{"$KV.>", "$KV.legion_tasks.legion-example-infra-1-implementer", true},
		{">", "$KV.legion_tasks.legion-example-infra-1-implementer", true},
		{"$KV.*.*", "$KV.legion_tasks.legion-example-infra-1-implementer", true},
		{"$JS.API.STREAM.INFO.KV_legion_tasks", "$JS.API.STREAM.INFO.KV_legion_tasks", true},
		{"$JS.API.STREAM.INFO.* envoy-roles", "$JS.API.STREAM.INFO.KV_legion_tasks", true},
		{"$JS.API.STREAM.INFO.KV_legion_tasks.>", "$JS.API.STREAM.INFO.KV_legion_tasks", false},
		{"$KV.envoy_roles.>", "$KV.legion_tasks.legion-example-infra-1-implementer", false},
		{"$KV.*", "$KV.legion_tasks.legion-example-infra-1-implementer", false},
		{"$JS.API.STREAM.INFO.KV_legion_tasks.x", "$JS.API.STREAM.INFO.KV_legion_tasks", false},
	} {
		require.Equal(t, c.want, subjectAdmits(c.pattern, c.subject), "%q admits %q", c.pattern, c.subject)
	}
}

// The listener's two sets hold the task bucket's reads and none of its writes: the daemon is the
// bucket's one writer.
func TestListenerPermissionsReadTheTaskBucketAndWriteNothingOfIt(t *testing.T) {
	for name, p := range map[string]core.Permissions{
		"listener":      core.ListenerPermissions("example-host-a"),
		"listener host": core.ListenerHostPermissions("example-host-a"),
	} {
		require.Subset(t, p.Publish.Allow, taskBucketReadPublish, "the %s set", name)
		for _, call := range []string{
			"$JS.API.STREAM.CREATE.KV_legion_tasks",
			"$JS.API.STREAM.UPDATE.KV_legion_tasks",
			"$KV.legion_tasks.legion-example-infra-1-implementer",
		} {
			for _, entry := range p.Publish.Allow {
				require.False(t, subjectAdmits(entry, call), "the %s set's entry %q admits %q", name, entry, call)
			}
		}
	}
}

// A host's user is shared by its sessions, so it holds nothing of the task bucket: no session reads
// another claim's value, and the listener connecting as it relays no task.
func TestHostPermissionsHoldNoTaskBucketGrant(t *testing.T) {
	requireNoTaskBucketGrant(t, core.HostPermissions("example-host-a"))
}

// Dispatch's user, which envoy-dispatch and the listener beside it share, holds nothing of the task
// bucket.
func TestDispatchPermissionsHoldNoTaskBucketGrant(t *testing.T) {
	requireNoTaskBucketGrant(t, core.DispatchPermissions("example-host-a"))
}

// A session reads its value from the relay frame its listener publishes, never from the bucket.
func TestSessionPermissionsHoldNoTaskBucketGrant(t *testing.T) {
	requireNoTaskBucketGrant(t, core.SessionPermissions())
}

// The task bucket is the daemon's, not one of the buckets the listener keeps and raises, so the
// listener grant every user that runs a listener holds grants it nothing.
func TestListenerBucketsHoldNoTaskBucket(t *testing.T) {
	require.NotContains(t, bus.ListenerBuckets(), legiontasks.Bucket)
}
