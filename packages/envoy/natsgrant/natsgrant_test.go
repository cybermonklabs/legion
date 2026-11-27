package natsgrant_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/natsgrant"
)

// set is one grant as the fixtures write it.
type set struct {
	Publish   []string `json:"publish"`
	Subscribe []string `json:"subscribe"`
}

// mergeBase is testdata/merge-base-grants.json: every set infra/core returned at 545b2116, before
// the sets moved here, for each machine id the fixture names. It was written once, by a throwaway
// program in the infra module calling core.ListenerPermissions, core.ListenerHostPermissions,
// core.DispatchPermissions, core.HostPermissions, core.SessionPermissions, core.DaemonPermissions
// and core.DispatchDurables at that commit, and is never regenerated: it is the record of what the
// hub rendered before this package existed.
type mergeBase struct {
	MachineIDs       []string       `json:"machine_ids"`
	Listener         map[string]set `json:"listener"`
	ListenerHost     map[string]set `json:"listener_host"`
	Dispatch         map[string]set `json:"dispatch"`
	Host             map[string]set `json:"host"`
	SessionPattern   set            `json:"session_pattern"`
	Daemon           set            `json:"daemon"`
	DispatchDurables []string       `json:"dispatch_durables"`
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, into))
}

// requireSet compares got with want element for element, after refusing an empty fixture set: an
// empty want would compare equal to an empty grant and prove nothing.
func requireSet(t *testing.T, want set, got natsgrant.Permissions, name string) {
	t.Helper()
	require.NotEmpty(t, want.Publish, "the fixture holds no publish list for %s", name)
	require.Equal(t, want.Publish, got.Publish, "%s publish", name)
	require.Equal(t, want.Subscribe, got.Subscribe, "%s subscribe", name)
}

// withPublish is s with entries after its publish list, in a list of its own.
func withPublish(s set, entries []string) set {
	return set{Publish: slices.Concat(s.Publish, entries), Subscribe: s.Subscribe}
}

// knownSessionPurge is the one entry every set built on listenerGrant has gained since the merge
// base the fixture records: the known-session store's purge of its replaced chunks' markers
// (internal/kvchunk, session.KnownSessions.purgeChunkMarkers), right after the interest bucket's
// purge.
const (
	interestPurge     = "$JS.API.STREAM.PURGE.KV_envoy_interests"
	knownSessionPurge = "$JS.API.STREAM.PURGE.KV_envoy_known_sessions"
)

// sinceMergeBase is a set the fixture records with knownSessionPurge added after its one
// interestPurge, which it must hold, so a fixture set the addition does not reach fails here rather
// than compare unchanged.
func sinceMergeBase(t *testing.T, base set, name string) set {
	t.Helper()
	at := slices.Index(base.Publish, interestPurge)
	require.NotEqual(t, -1, at, "the fixture's %s holds no %s to add %s after", name, interestPurge, knownSessionPurge)
	require.NotContains(t, base.Publish, knownSessionPurge, "the fixture's %s already holds %s", name, knownSessionPurge)
	base.Publish = slices.Insert(slices.Clone(base.Publish), at+1, knownSessionPurge)
	return base
}

// Every set this package builds is, element for element and in order, the set infra/core built at
// the merge base, so moving them here rewrites no hub's rendered authorization, with the one entry
// added since (sinceMergeBase), and but for the Legion daemons' task bucket, which came after that
// commit: the listener's two sets end their publish list with the bucket's reads
// (listenerTaskBucketReads), and the daemon's with its calls on the bucket
// (daemonTaskBucketCalls). Host, Dispatch and the session's set gain nothing of it.
func TestEachGrantIsTheSetItReplacesPlusTheTaskBucket(t *testing.T) {
	var base mergeBase
	readJSON(t, "testdata/merge-base-grants.json", &base)
	require.Equal(t, []string{"example-host-a", "example-dispatch", "example-hub"}, base.MachineIDs)
	for _, id := range base.MachineIDs {
		for name, pair := range map[string]struct {
			want set
			got  natsgrant.Permissions
		}{
			"Listener":     {withPublish(base.Listener[id], listenerTaskBucketReads), natsgrant.Listener(id)},
			"ListenerHost": {withPublish(base.ListenerHost[id], listenerTaskBucketReads), natsgrant.ListenerHost(id)},
			"Dispatch":     {base.Dispatch[id], natsgrant.Dispatch(id)},
			"Host":         {base.Host[id], natsgrant.Host(id)},
		} {
			name := name + "(" + strconv.Quote(id) + ")"
			requireSet(t, sinceMergeBase(t, pair.want, name), pair.got, name)
		}
	}
	requireSet(t, base.SessionPattern, natsgrant.SessionPattern(), "SessionPattern()")
	requireSet(t, withPublish(base.Daemon, daemonTaskBucketCalls), natsgrant.Daemon(), "Daemon()")
	require.NotEmpty(t, base.DispatchDurables)
	require.Equal(t, base.DispatchDurables, natsgrant.DispatchDurables())
}

// Measure is the measurement user's grant as the deployment renders it once its stream calls and
// envoy-measure.> are removed and its cutover inbox is added, which is not yet deployed: eight
// JetStream reads and its own cutover inbox, under the job's inbox before the move. To it this test
// adds the transitional entries of the job's inbox under the reply root, the subscribe and the
// cutover inbox there, which the deployment repository's copy adds in its own change while the
// job's inbox moves (TestMeasureSubscribesItsInboxUnderBothRoots). The copy there goes when the job
// moves to the auth service, and this test with it.
func TestMeasureMatchesTheDeployedList(t *testing.T) {
	var deployed set
	readJSON(t, "testdata/deployment-measure-grant.json", &deployed)
	require.Equal(t, []string{"_INBOX_envoy_measure.>"}, deployed.Subscribe,
		"the fixture is the list before the move, to which this test adds the moved inbox")
	deployed.Subscribe = append(deployed.Subscribe, "_INBOX_envoy._measure.>")
	deployed.Publish = append(deployed.Publish, "_INBOX_envoy._measure.cutover.>")
	requireSet(t, deployed, natsgrant.Measure(), "Measure()")
}

// subscribes reports whether grant lets a client hold a plain subscription, one with no queue
// group, to subject, as nats-server 2.15.1 decides it (server/client.go, canSubscribeInternal and
// loadMsgDenyFilterIfNeeded): an allow entry covers it, and no deny entry overlaps it. A deny entry
// that matches the subscription refuses it, and one that only overlaps it drops the deliveries it
// covers; this helper reads any overlap as a refusal, the stricter answer. An entry with a queue
// group admits or denies that group alone, which this helper does not model, so it fails the test
// on one rather than answer for it.
func subscribes(t *testing.T, grant natsgrant.Permissions, subject string) bool {
	t.Helper()
	for _, entry := range slices.Concat(grant.Subscribe, grant.SubscribeDeny) {
		require.Len(t, strings.Fields(entry), 1,
			"subscribe entry %q carries a queue group, which subscribes does not model", entry)
	}
	if slices.ContainsFunc(grant.SubscribeDeny, func(deny string) bool { return natsserver.SubjectsCollide(subject, deny) }) {
		return false
	}
	return slices.ContainsFunc(grant.Subscribe, func(allow string) bool { return natsserver.SubjectMatchesFilter(subject, allow) })
}

// While the measurement job's inbox moves under the reply root, its grant admits it under both:
// the deployed job sets nats.js's inboxPrefix to _INBOX_envoy_measure, and the moved one to
// MeasureInbox. Each row is the subscription nats.js 2.29.3 makes for its requests, the prefix, a
// nuid and *, whose answers come on the prefix, the nuid and a token (protocol.js, the mux's
// SubscriptionImpl; muxsubscription.js, init). The grant admits no other client's replies:
// nats.go's _INBOX., another prefix under the root, or another token directly under it. The
// control is the grant before this change, which refuses the moved inbox, and a deny entry over the
// moved inbox, which the helper must read as a refusal.
func TestMeasureSubscribesItsInboxUnderBothRoots(t *testing.T) {
	rows := []struct {
		name, subscription string
		admitted           bool
	}{
		{"the deployed job's inbox", "_INBOX_envoy_measure.Fq3kZ.*", true},
		{"the inbox under the reply root", "_INBOX_envoy._measure.Fq3kZ.*", true},
		{"nats.go's inbox, where other clients' replies arrive", "_INBOX.Fq3kZ.*", false},
		{"the operator's prefix under the reply root", "_INBOX_envoy._operator.Fq3kZ.*", false},
		{"another token directly under the reply root", "_INBOX_envoy.Fq3kZ.*", false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			require.Equal(t, row.admitted, subscribes(t, natsgrant.Measure(), row.subscription),
				"Measure() admits a subscription to %q", row.subscription)
		})
	}
	t.Run("control: the grant before this change refuses the moved inbox", func(t *testing.T) {
		before := natsgrant.Permissions{Subscribe: []string{"_INBOX_envoy_measure.>"}}
		require.True(t, subscribes(t, before, "_INBOX_envoy_measure.Fq3kZ.*"))
		require.False(t, subscribes(t, before, "_INBOX_envoy._measure.Fq3kZ.*"))
	})
	t.Run("control: a deny entry over the moved inbox refuses it", func(t *testing.T) {
		denied := natsgrant.Measure()
		denied.SubscribeDeny = []string{"_INBOX_envoy._measure.*.*"}
		require.False(t, subscribes(t, denied, "_INBOX_envoy._measure.Fq3kZ.*"))
		require.True(t, subscribes(t, denied, "_INBOX_envoy_measure.Fq3kZ.*"))
	})
}

// Every reply inbox a grant of this package subscribes sits under one token, ReplyRoot, which a
// render that puts JetStream in an account of its own exports to the clients' account as
// ReplyRoot+".>": a push or ordered consumer's deliveries and a stream's republish reach a client
// only through a subject that account exports. A reply inbox is a subscribe whose first token
// begins _INBOX, as nats.go's and nats.js's default prefix and every prefix this package names do
// (outsideTheRoot). Each row is one kind's grant, its inbox prefix under the root once its client
// moved, and the inboxes it still subscribes outside the root while its client moves
// (transitional), each named: nats.go's _INBOX.> for the listener's kinds, Dispatch and the daemon,
// the edge listener's own machine's prefix (#4807), and the measurement job's prefix before its
// move. For each row, every reply inbox of the grant is under the root or one the row names; each
// one the row names is one the grant subscribes and one the root's export does not reach; and a
// prefix under the root is reached by the root's export and admitted by the grant. So a kind that
// subscribes any other inbox outside the root fails, naming it. The control is the edge row's grant
// checked without its listing, which names the edge's prefix.
func TestEveryReplyPrefixIsUnderTheReplyRoot(t *testing.T) {
	require.Equal(t, "_INBOX_envoy", natsgrant.ReplyRoot)
	export := natsgrant.ReplyRoot + ".>"
	session, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	rows := []struct {
		name  string
		grant natsgrant.Permissions
		// prefix is the kind's inbox prefix under the root, empty while its client has not moved.
		prefix string
		// transitional are the inboxes a kind subscribes outside the root while its client moves.
		transitional []string
	}{
		{"listener", natsgrant.Listener("m"), "", []string{"_INBOX.>"}},
		{"listener host", natsgrant.ListenerHost("m"), "", []string{"_INBOX.>"}},
		{"dispatch", natsgrant.Dispatch("m"), "", []string{"_INBOX.>"}},
		{"host", natsgrant.Host("m"), "", []string{"_INBOX.>"}},
		{"daemon", natsgrant.Daemon(), "", []string{"_INBOX.>"}},
		{"session pattern", natsgrant.SessionPattern(), "", nil},
		{"session", session, "", nil},
		// The measurement job's prefix before its move shares the root's characters but not its
		// token, so the root's export does not reach it; it goes when no job sets it.
		{"measure", natsgrant.Measure(), natsgrant.MeasureInbox, []string{"_INBOX_envoy_measure.>"}},
		// The edge listener's own machine's prefix shares the root's characters but not its token,
		// so a render with JetStream in an account of its own exports it once per edge machine
		// until the edge listener moves under the root.
		{"edge", edge(t, "m", "PRIV"), "", []string{"_INBOX_envoy_edge_m.>"}},
		// A dotted machine id (R18) is one token of that prefix, spelled as the listener spells it,
		// so the prefix a render exports for it is the spelled one, outside the root as well.
		{"edge with a dotted machine id", edge(t, "host.example.internal", "PRIV"), "", []string{"_INBOX_envoy_edge_host%2Eexample%2Einternal.>"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			require.Empty(t, outsideTheRoot(row.grant, row.transitional),
				"%s subscribes these reply inboxes outside the reply root %q", row.name, natsgrant.ReplyRoot)
			for _, entry := range row.transitional {
				require.Contains(t, row.grant.Subscribe, entry,
					"%s's row names %q, which its grant does not subscribe; drop it from the row", row.name, entry)
				require.False(t, natsserver.SubjectMatchesFilter(entry, export),
					"%s's transitional subscribe %q is under the root; drop it from the row", row.name, entry)
			}
			if row.prefix == "" {
				return
			}
			subscription, reply := row.prefix+".Fq3kZ.*", row.prefix+".Fq3kZ.7"
			require.True(t, strings.HasPrefix(row.prefix, natsgrant.ReplyRoot+"."),
				"%s's prefix %q is not under the reply root %q", row.name, row.prefix, natsgrant.ReplyRoot)
			require.True(t, natsserver.SubjectMatchesFilter(reply, export),
				"the reply root's export %s does not reach %s's reply %q", export, row.name, reply)
			require.True(t, subscribes(t, row.grant, subscription),
				"%s's grant does not admit its own inbox's subscription %q", row.name, subscription)
		})
	}
	t.Run("control: the edge's grant without its listing names the edge's prefix", func(t *testing.T) {
		require.Equal(t, []string{"_INBOX_envoy_edge_m.>"}, outsideTheRoot(edge(t, "m", "PRIV"), nil))
	})
}

// outsideTheRoot returns each reply inbox grant subscribes, a subscribe whose first token begins
// _INBOX, that is not under ReplyRoot and that transitional does not name.
func outsideTheRoot(grant natsgrant.Permissions, transitional []string) []string {
	var outside []string
	for _, entry := range grant.Subscribe {
		if strings.HasPrefix(entry, "_INBOX") && !strings.HasPrefix(entry, natsgrant.ReplyRoot+".") && !slices.Contains(transitional, entry) {
			outside = append(outside, entry)
		}
	}
	return outside
}

// edgeGolden is testdata/edge.golden.json: the edge listener's grant for the machine id and the
// projects it names, which the deployment repository's hub user for an edge listener copies entry
// for entry. It is written by hand and never regenerated, so a change to Edge, or to a name it is
// built from, fails here until the golden and that copy change with it.
type edgeGolden struct {
	MachineID   string   `json:"machine_id"`
	Projects    []string `json:"projects"`
	Publish     []string `json:"publish"`
	Subscribe   []string `json:"subscribe"`
	PublishDeny []string `json:"publish_deny"`
}

// edge is natsgrant.Edge(machineID, projects), which must take the list.
func edge(t *testing.T, machineID string, projects ...string) natsgrant.Permissions {
	t.Helper()
	grant, err := natsgrant.Edge(machineID, projects)
	require.NoError(t, err, "Edge(%q, %v)", machineID, projects)
	return grant
}

// The edge listener's grant is the golden, element for element and in order. Its publish deny is
// the listener's, Dispatch's lane and GitHub's lane (check l5 of dispatch://INFRA-3589), and it has
// no subscribe deny.
func TestTheEdgeGrantIsTheGolden(t *testing.T) {
	var golden edgeGolden
	readJSON(t, "testdata/edge.golden.json", &golden)
	require.Equal(t, "m", golden.MachineID)
	require.Equal(t, []string{"PRIV"}, golden.Projects)
	grant := edge(t, golden.MachineID, golden.Projects...)
	requireSet(t, set{Publish: golden.Publish, Subscribe: golden.Subscribe}, grant, "Edge("+strconv.Quote(golden.MachineID)+", "+strings.Join(golden.Projects, ",")+")")
	require.Equal(t, []string{"notifications.dispatch.>", "notifications.github.>"}, golden.PublishDeny)
	require.Equal(t, golden.PublishDeny, grant.PublishDeny)
	require.Empty(t, grant.SubscribeDeny)
}

// Edge is Listener with the edge's changes and nothing else. In publish: in place of every
// notification topic, the finite list of the edge's own subjects - the agent subjects, each
// project's role subjects, the relay subjects, every edge machine's relay receipts, the resume
// lane, and the exception lanes of the agent subjects and of each project's role subjects - and no
// create or update of the stream or of any bucket's stream, no direct get of any bucket, and none
// of the task bucket's reads (listenerTaskBucketReads). In
// subscribe: its own machine's inbox prefix in place of every
// reply inbox, every session's relay subject and its own machine's relay receipts, and neither
// role-lane queue group. Its publish deny is Listener's. So a change to Listener reaches Edge, and
// the golden above says that it did.
func TestTheEdgeGrantIsTheListenersWithTheEdgesChanges(t *testing.T) {
	for _, id := range []string{"m", "example-hub"} {
		require.Equal(t, "_INBOX_envoy_edge_"+id, natsgrant.EdgeInboxPrefix(id), "EdgeInboxPrefix(%q)", id)
		require.Equal(t, "notifications.envoy.relayreply."+id, natsgrant.EdgeRelayReplyPrefix(id), "EdgeRelayReplyPrefix(%q)", id)
		for _, projects := range [][]string{{"PRIV"}, {"OTHER", "PRIV"}} {
			listener, edge := natsgrant.Listener(id), edge(t, id, projects...)
			require.Equal(t, "notifications.>", listener.Publish[0])
			kept := slices.DeleteFunc(slices.Clone(listener.Publish[1:]), func(entry string) bool {
				return strings.HasPrefix(entry, "$JS.API.STREAM.CREATE.") || strings.HasPrefix(entry, "$JS.API.STREAM.UPDATE.") || strings.HasPrefix(entry, "$JS.API.DIRECT.GET.") || slices.Contains(listenerTaskBucketReads, entry)
			})
			require.Len(t, kept, len(listener.Publish)-1-2*(1+len(bus.ListenerBuckets()))-len(bus.ListenerBuckets())-len(listenerTaskBucketReads), "Listener(%q) holds every notification topic, the create and update of the stream and of each bucket's stream, each bucket's direct get, and the task bucket's reads", id)
			own := []string{"notifications.agent.>"}
			for _, project := range projects {
				own = append(own, "notifications.role."+project+".>")
			}
			own = append(own, "notifications.envoy.deliver.*", "notifications.envoy.relayreply.>", "notifications.envoy.resume.>", "notifications.envoy.exceptions.notifications.agent.>")
			for _, project := range projects {
				own = append(own, "notifications.envoy.exceptions.notifications.role."+project+".>")
			}
			require.Equal(t, append(own, kept...), edge.Publish, "Edge(%q, %v) publish", id, projects)
			require.Equal(t, []string{"_INBOX_envoy_edge_" + id + ".>", "notifications.envoy.deliver.*", "notifications.envoy.relayreply." + id + ".>"}, edge.Subscribe, "Edge(%q, %v) subscribe", id, projects)
			require.Equal(t, listener.PublishDeny, edge.PublishDeny, "Edge(%q, %v) publish deny", id, projects)
		}
	}
}

// Check l16 of dispatch://INFRA-3589 (R18) for the edge: Edge takes a dotted machine id, such as a
// fully qualified host name, and spells it in its inbox prefix and its receipt family as the
// listener spells it in its durable (bus.ListenerSpelling): each dot as %2E, so each name is one
// subject token. An id that holds none of the characters the spelling escapes keeps its prefix
// byte for byte, so a deployed edge's prefix does not move.
func TestADottedEdgeMachineIDIsOneTokenInEachEdgeName(t *testing.T) {
	const id, spelled = "host.example.internal", "host%2Eexample%2Einternal"
	require.Equal(t, "_INBOX_envoy_edge_"+spelled, natsgrant.EdgeInboxPrefix(id))
	require.Equal(t, "notifications.envoy.relayreply."+spelled, natsgrant.EdgeRelayReplyPrefix(id))
	grant := edge(t, id, "PRIV")
	require.Equal(t, []string{"_INBOX_envoy_edge_" + spelled + ".>", "notifications.envoy.deliver.*", "notifications.envoy.relayreply." + spelled + ".>"}, grant.Subscribe)
	require.Contains(t, grant.Publish, "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS."+bus.ListenerDurable(id))
	require.Equal(t, "listener-"+spelled, bus.ListenerDurable(id))
	for _, plain := range []string{"m", "example-hub", "edge_a-1", "EDGE"} {
		require.Equal(t, "_INBOX_envoy_edge_"+plain, natsgrant.EdgeInboxPrefix(plain), "EdgeInboxPrefix(%q)", plain)
		require.Equal(t, "notifications.envoy.relayreply."+plain, natsgrant.EdgeRelayReplyPrefix(plain), "EdgeRelayReplyPrefix(%q)", plain)
	}
}

// Two edge machines subscribe inbox prefixes and receipt families of their own, so neither is
// delivered the other's replies nor reads the other's receipts: neither's subscribe entries match
// a subject the other names. Each publishes the other's receipt, as the listener serving a
// session's relay socket answers on the family of the machine holding the session's consumer. A
// machine whose id begins another's (edge-a, edge-ab) is no exception, since a NATS wildcard
// matches whole tokens, and nor is one whose id is another's and a dotted suffix (edge-a,
// edge-a.internal), since the spelling makes the dotted id one token. The other machine's own
// entries match the same subjects, which shows the matcher finds one.
func TestEachEdgeMachineHoldsItsOwnInboxesAndReceipts(t *testing.T) {
	for _, pair := range [][2]string{{"edge-a", "edge-b"}, {"edge-a", "edge-ab"}, {"edge-a", "edge-a.internal"}, {"edge-a.internal", "edge-a"}} {
		a, b := edge(t, pair[0], "PRIV"), edge(t, pair[1], "PRIV")
		inboxOfB := natsgrant.EdgeInboxPrefix(pair[1]) + ".nuid"
		receiptOfB := natsgrant.EdgeRelayReplyPrefix(pair[1]) + ".nuid"
		admits := func(entries []string, subject string) bool {
			return slices.ContainsFunc(entries, func(entry string) bool { return bus.SubjectContains(entry, subject) })
		}
		require.True(t, admits(b.Subscribe, inboxOfB), "%s does not subscribe its own inbox %s", pair[1], inboxOfB)
		require.False(t, admits(a.Subscribe, inboxOfB), "%s subscribes %s's inbox %s", pair[0], pair[1], inboxOfB)
		require.True(t, admits(b.Subscribe, receiptOfB), "%s does not subscribe its own receipt %s", pair[1], receiptOfB)
		require.False(t, admits(a.Subscribe, receiptOfB), "%s subscribes %s's receipt %s", pair[0], pair[1], receiptOfB)
		require.True(t, admits(a.Publish, receiptOfB), "%s does not publish %s's receipt %s, which the relay socket's answer needs", pair[0], pair[1], receiptOfB)
	}
}

// Edge refuses, naming why and building no subject, a machine id whose prefixes would share
// another machine's: a space, which the spelling writes as a hyphen, and one holding the escape
// character '%' (a%2Eb would spell as a.b does). It also refuses what a BROKER_NATS_SERVICES entry
// refuses (natsgrant.MachineIDToken): a wildcard and an empty id. It takes a dotted id, which the
// spelling makes one token. It refuses a project list it cannot name: none, which would render the
// whole consumer and key-value reach serving no project, or one oauthwire.ValidProjects refuses (a
// wildcard here; its own tests read every rule).
func TestTheEdgeGrantRefusesAMachineOrProjectsItCannotName(t *testing.T) {
	for _, machineID := range []string{"example hub", "*", ">", "", "a%2Eb"} {
		grant, err := natsgrant.Edge(machineID, []string{"PRIV"})
		require.ErrorContains(t, err, strconv.Quote(machineID), "Edge(%q, [PRIV])", machineID)
		require.Equal(t, natsgrant.Permissions{}, grant)
	}
	_, err := natsgrant.Edge("a.b", []string{"PRIV"})
	require.NoError(t, err, "Edge(a.b, [PRIV])")
	for name, projects := range map[string][]string{"no list": nil, "an empty list": {}, "a wildcard": {"*"}} {
		grant, err := natsgrant.Edge("m", projects)
		require.Error(t, err, "Edge(m, %s)", name)
		require.Equal(t, natsgrant.Permissions{}, grant)
	}
	for _, projects := range [][]string{{"PRIV"}, {"OTHER", "PRIV"}} {
		_, err := natsgrant.Edge("m", projects)
		require.NoError(t, err, "Edge(m, %q)", projects)
	}
}

// The edge publishes each of its own lanes and nothing else of the notification topics: no event
// of another source, no Legion wake, no role subject of another project or in the form that names
// none, no kept role lane or drain cursor, no exception lane but its own two, no reply to another
// client's request and no agent stream subject, and it subscribes no agent stream subject either.
// The listener's own grant admits each publish the edge's refuses, which shows the matcher can
// find one.
func TestTheEdgeGrantPublishesNothingOutsideItsLanes(t *testing.T) {
	edge, listener := edge(t, "m", "PRIV"), natsgrant.Listener("m")
	admits := func(entries []string, subject string) bool {
		return slices.ContainsFunc(entries, func(pattern string) bool { return bus.SubjectContains(pattern, subject) })
	}
	for _, subject := range []string{
		"notifications.dispatch.issue.PRIV.PRIV-1.comment.created",
		"notifications.github.acme.pub.pr.1.opened",
		"notifications.slack.example-channel",
		"notifications.legion.wake",
		"notifications.team.deploy-squad",
		"notifications.role.INFRA.x",
		"notifications.role.legion-infra-controller",
		"notifications.envoy.kept.role.x",
		"notifications.envoy.kept.cursor.x",
		"notifications.envoy.exceptions.notifications.github.acme.pub.pr.1.opened",
		"notifications.envoy.exceptions.notifications.role.INFRA.x",
		"notifications.envoy.exceptions.notifications.team.deploy-squad",
		"notifications.envoy.relayreply",
		"$JS.API.STREAM.CREATE.KV_envoy_interests",
		"$JS.API.STREAM.UPDATE.KV_envoy_interests",
		"$JS.API.STREAM.UPDATE.KV_envoy_dpop_proofs",
		"$JS.API.DIRECT.GET.KV_envoy_interests.$KV.envoy_interests.k",
		"$JS.API.DIRECT.GET.KV_envoy_dpop_proofs.$KV.envoy_dpop_proofs.k",
	} {
		require.False(t, admits(edge.Publish, subject), "Edge publishes %s", subject)
		require.True(t, admits(listener.Publish, subject), "Listener does not publish %s, so the matcher finds nothing", subject)
	}
	for _, subject := range []string{
		"notifications.agent.ses_a",
		"notifications.role.PRIV.reviewer",
		"notifications.envoy.deliver.ses_a",
		"notifications.envoy.relayreply.m.t1",
		"notifications.envoy.relayreply.other.t1",
		"notifications.envoy.resume.ses_a",
		"notifications.envoy.exceptions.notifications.agent.ses_a",
		"notifications.envoy.exceptions.notifications.role.PRIV.reviewer",
	} {
		require.True(t, admits(edge.Publish, subject), "Edge does not publish %s", subject)
	}
	for _, entries := range [][]string{edge.Publish, edge.Subscribe} {
		for _, entry := range entries {
			require.False(t, strings.HasPrefix(entry, "agentstream."), "Edge names the agent stream subject %q", entry)
		}
	}
	for _, entry := range edge.Publish {
		require.False(t, strings.HasPrefix(entry, "_INBOX"), "Edge publishes to the reply inboxes %q", entry)
	}
}

// The auth service's guard passes the edge's grant: it opens neither direction and names no system
// subject. The same check on a copy with no subscribe list finds that direction open.
func TestTheEdgeGrantOpensNoDirection(t *testing.T) {
	grant := edge(t, "m", "PRIV")
	direction, open := natsgrant.OpenDirection(grant)
	require.False(t, open, "Edge opens its %s direction", direction)
	entry, named := natsgrant.NamesSystemSubject(grant)
	require.False(t, named, "Edge names %q", entry)
	grant.Subscribe = nil
	direction, open = natsgrant.OpenDirection(grant)
	require.True(t, open)
	require.Equal(t, "subscribe", direction)
}

// A session's grant names its own subjects and no other session's - its replies too, under
// _INBOX.*.<id>.>, where the shared pattern publishes every _INBOX. subject - and an id that is not
// one subject token is refused, because a dot, a wildcard or a space in it would widen the grant.
func TestSessionGrantNamesOnlyItsOwnSession(t *testing.T) {
	grant, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	require.Equal(t, natsgrant.Permissions{
		Publish:   []string{"_INBOX.*.ses_a.>", contracts.AgentStreamFramesSubject("ses_a")},
		Subscribe: []string{bus.DeliverSubject("ses_a"), contracts.AgentStreamControlSubject("ses_a")},
	}, grant)
	require.Equal(t, bus.SessionReplies("ses_a"), grant.Publish[0])
	require.Equal(t, "_INBOX.>", natsgrant.SessionPattern().Publish[0], "the shared pattern keeps every reply inbox")
	for _, id := range []string{"a.b", "*", ">", "", "a b", "a\tb", "a\x00b"} {
		_, err := natsgrant.Session(id)
		require.ErrorContains(t, err, strconv.Quote(id), "Session(%q)", id)
		require.False(t, natsgrant.SessionToken(id), "SessionToken(%q)", id)
	}
	require.True(t, natsgrant.SessionToken("ses_a"))
}

// The read-only bucket grant reads a bucket and watches it, and writes nothing: no key, no stream
// create or update.
func TestKVReadIsReadOnly(t *testing.T) {
	read := natsgrant.KVRead("b")
	require.Equal(t, []string{
		"$JS.API.STREAM.INFO.KV_b",
		"$JS.API.DIRECT.GET.KV_b.>",
		"$JS.API.STREAM.MSG.GET.KV_b",
		"$JS.API.CONSUMER.CREATE.KV_b.>",
		"$JS.API.CONSUMER.DELETE.KV_b.>",
		"$JS.FC.KV_b.>",
	}, read)
	for _, entry := range read {
		require.NotEqual(t, "$KV.b.>", entry)
		require.NotContains(t, entry, "STREAM.CREATE")
		require.NotContains(t, entry, "STREAM.UPDATE")
	}
	require.Subset(t, natsgrant.KVWrite("b"), read, "the write grant holds every read call")
	require.Contains(t, natsgrant.KVWrite("b"), "$KV.b.>")
}

// NamesSystemSubject finds an entry that reaches a system subject, the auth service's own
// authorization subject among them, in either list and with or without a queue group, whatever
// whitespace stands before the queue group: nats-server splits an entry with strings.Fields
// (server/client.go, splitSubjectQueue), so a tab or a no-break space separates it as a space
// does. And it finds none in any grant this package returns.
func TestNamesSystemSubject(t *testing.T) {
	for _, entry := range []string{
		"$SYS.REQ.USER.AUTH",
		"$SYS.>",
		">",
		"*.REQ.USER.AUTH",
		"*.>",
		"$SYS.REQ.USER.AUTH envoy-auth",
		">\tenvoy-auth",
		"$SYS.REQ.USER.AUTH\tenvoy-auth",
		">\u00a0envoy-auth",
	} {
		for _, p := range []natsgrant.Permissions{
			{Publish: []string{"_INBOX.>", entry}},
			{Subscribe: []string{"notifications.envoy.deliver.ses_a", entry}},
		} {
			got, found := natsgrant.NamesSystemSubject(p)
			require.True(t, found, "%q in %v", entry, p)
			require.Equal(t, entry, got)
		}
	}
	grants := grantTable(t)
	grants["KVRead"] = natsgrant.Permissions{Publish: natsgrant.KVRead(bus.InterestBucket)}
	grants["KVWrite"] = natsgrant.Permissions{Publish: natsgrant.KVWrite(bus.InterestBucket)}
	for name, grant := range grants {
		require.NotEmpty(t, slices.Concat(grant.Publish, grant.Subscribe), name)
		entry, found := natsgrant.NamesSystemSubject(grant)
		require.False(t, found, "%s names %q", name, entry)
	}
}

// grantTable is every grant this package builds, keyed by the call that builds it: the function's
// name, with its argument in parentheses where it takes one. TestTheGrantTableNamesEveryGrant holds
// it to the declarations of the package's non-test files, so a new grant function fails that test
// until it is listed here, and every test that walks the table walks the new grant too.
func grantTable(t *testing.T) map[string]natsgrant.Permissions {
	t.Helper()
	session, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	grants := map[string]natsgrant.Permissions{
		"SessionPattern()":   natsgrant.SessionPattern(),
		"Session(ses_a)":     session,
		"Daemon()":           natsgrant.Daemon(),
		"Measure()":          natsgrant.Measure(),
		"HoldProbe()":        natsgrant.HoldProbe(),
		"Operator(released)": natsgrant.Operator(natsgrant.OperatorPhaseReleased),
		"Operator(prepare)":  natsgrant.Operator(natsgrant.OperatorPhasePrepare),
		"Operator(held)":     natsgrant.Operator(natsgrant.OperatorPhaseHeld),
	}
	for _, id := range []string{"example-host-a", "example-dispatch", "example-hub"} {
		grants["Listener("+id+")"] = natsgrant.Listener(id)
		grants["WebhookListener("+id+")"] = natsgrant.WebhookListener(id)
		grants["WebhookListenerHost("+id+")"] = natsgrant.WebhookListenerHost(id)
		grants["ListenerHost("+id+")"] = natsgrant.ListenerHost(id)
		grants["Dispatch("+id+")"] = natsgrant.Dispatch(id)
		grants["Host("+id+")"] = natsgrant.Host(id)
		grants["Edge("+id+", PRIV)"] = edge(t, id, "PRIV")
	}
	return grants
}

// Every exported function a non-test .go file of the package declares that returns Permissions,
// read from the source by go/ast, has a row in grantTable, and every row names one of them: a grant
// added to the package, in natsgrant.go or in a file beside it such as lanes.go, is walked by every
// test that reads the table, or this test names it.
func TestTheGrantTableNamesEveryGrant(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	files := token.NewFileSet()
	declared := map[string]string{}
	for _, entry := range entries {
		path := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, path, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			if result, ok := fn.Type.Results.List[0].Type.(*ast.Ident); ok && result.Name == "Permissions" {
				declared[fn.Name.Name] = path
			}
		}
	}
	require.NotEmpty(t, declared, "the package's files declare no grant function: the reading enumerated nothing")
	listed := map[string]bool{}
	for call := range grantTable(t) {
		name, _, _ := strings.Cut(call, "(")
		require.NotEmpty(t, declared[name], "grantTable lists %s, which no file of the package declares as a grant", call)
		listed[name] = true
	}
	for name, path := range declared {
		require.True(t, listed[name], "%s declares the grant %s, which grantTable does not list", path, name)
	}
}

// The buckets envoy-infra buckets copies, whose infos the operator reads outside a hold and whose
// streams it snapshots, makes, changes, removes and restores only inside one.
var copiedBuckets = []string{"envoy_interests", "envoy_roles", "envoy_sessions", "envoy_known_sessions", "envoy_ci_state"}

// operatorReleased is Operator(OperatorPhaseReleased)'s publish list: reads only, no snapshot and
// no stream create, each of which writes a subject its requester chooses.
func operatorReleased() []string {
	publish := []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS",
		"$JS.API.STREAM.INFO.KV_envoy_session_consumers",
	}
	for _, b := range copiedBuckets {
		publish = append(publish, "$JS.API.STREAM.INFO.KV_"+b)
	}
	return append(publish,
		"$JS.API.STREAM.MSG.GET.KV_envoy_session_consumers",
		"$JS.API.CONSUMER.NAMES.ENVOY_NOTIFICATIONS",
		"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.*",
		"$JS.API.STREAM.INFO.ENVOY_MEASURE",
	)
}

// operatorPrepare is Operator(OperatorPhasePrepare)'s publish list as the plan writes it.
func operatorPrepare() []string {
	return append(operatorReleased(),
		"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.*",
		"$JS.API.STREAM.INFO.KV_legion_agents",
		"$JS.API.STREAM.INFO.KV_envoy_dpop_proofs",
		"$JS.API.STREAM.INFO.KV_envoy_webhook_redelivery",
	)
}

// operatorHeld is Operator(OperatorPhaseHeld)'s publish list as the plan writes it, with every
// snapshot and the create of ENVOY_MEASURE, which released held before.
func operatorHeld() []string {
	publish := append(operatorPrepare(),
		"$JS.API.STREAM.CREATE.ENVOY_HOLD_BARRIER",
		"$JS.API.STREAM.DELETE.ENVOY_HOLD_BARRIER",
		"$JS.API.STREAM.LIST",
		"$JS.API.STREAM.NAMES",
		"$JS.API.CONSUMER.LIST.*",
		"$JS.API.CONSUMER.DELETE.ENVOY_NOTIFICATIONS.*",
		"$JS.API.STREAM.CREATE.ENVOY_MEASURE",
		"$KV.envoy_session_consumers.>",
		"$JS.API.STREAM.CREATE.KV_envoy_session_consumers",
		"$JS.API.STREAM.SNAPSHOT.KV_envoy_session_consumers",
		"$JS.SNAPSHOT.ACK.KV_envoy_session_consumers.>",
		"$JS.API.STREAM.RESTORE.KV_envoy_session_consumers",
		"$JS.SNAPSHOT.RESTORE.KV_envoy_session_consumers.*",
	)
	for _, b := range copiedBuckets {
		publish = append(publish,
			"$JS.API.STREAM.CREATE.KV_"+b,
			"$JS.API.STREAM.UPDATE.KV_"+b,
			"$JS.API.STREAM.DELETE.KV_"+b,
			"$JS.API.STREAM.SNAPSHOT.KV_"+b,
			"$JS.SNAPSHOT.ACK.KV_"+b+".>",
			"$JS.API.STREAM.RESTORE.KV_"+b,
			"$JS.SNAPSHOT.RESTORE.KV_"+b+".*",
			"$KV."+b+".hold.barrier.*",
		)
	}
	publish = append(publish,
		"$JS.API.STREAM.CREATE.ENVOY_NOTIFICATIONS",
		"$JS.API.STREAM.CREATE.KV_legion_agents",
		"$JS.API.STREAM.CREATE.KV_envoy_dpop_proofs",
		"$JS.API.STREAM.CREATE.KV_envoy_webhook_redelivery",
		"$JS.API.STREAM.UPDATE.ENVOY_NOTIFICATIONS",
		"$JS.API.STREAM.UPDATE.KV_legion_agents",
		"$JS.API.STREAM.UPDATE.KV_envoy_dpop_proofs",
		"$JS.API.STREAM.UPDATE.KV_envoy_webhook_redelivery",
		"$JS.API.STREAM.DELETE.*",
		"$JS.API.CONSUMER.DELETE.*.*",
	)
	for _, s := range []string{"ENVOY_NOTIFICATIONS", "KV_legion_agents", "KV_envoy_dpop_proofs", "KV_envoy_webhook_redelivery"} {
		publish = append(publish,
			"$JS.API.STREAM.SNAPSHOT."+s,
			"$JS.SNAPSHOT.ACK."+s+".>",
			"$JS.API.STREAM.RESTORE."+s,
			"$JS.SNAPSHOT.RESTORE."+s+".*",
		)
	}
	return publish
}

// Operator's three phases and HoldProbe are each exactly the lists the plan writes, entry for
// entry and in order, so a subject added to any phase, or a phase that grants another's entry, fails
// here naming it. Every operator connection subscribes its own inbox prefix alone.
func TestOperatorIsExactly(t *testing.T) {
	inbox := []string{"_INBOX_envoy._operator.>"}
	for phase, want := range map[natsgrant.OperatorPhase][]string{
		natsgrant.OperatorPhaseReleased: operatorReleased(),
		natsgrant.OperatorPhasePrepare:  operatorPrepare(),
		natsgrant.OperatorPhaseHeld:     operatorHeld(),
	} {
		got := natsgrant.Operator(phase)
		for _, entry := range got.Publish {
			require.Contains(t, want, entry, "Operator(%s) publishes %q, which the plan's list does not hold", phase, entry)
		}
		for _, entry := range want {
			require.Contains(t, got.Publish, entry, "Operator(%s) does not publish %q, which the plan's list holds", phase, entry)
		}
		require.Equal(t, natsgrant.Permissions{Publish: want, Subscribe: inbox}, got, "Operator(%s)", phase)
	}
	require.Equal(t, natsgrant.Operator(natsgrant.OperatorPhaseReleased), natsgrant.Operator("paused"),
		"a phase outside the three is answered with the released grant")
	require.Equal(t, natsgrant.Permissions{
		Publish:   []string{"_ENVOY_HOLD_PROBE.none"},
		Subscribe: []string{"_ENVOY_HOLD_PROBE.none"},
	}, natsgrant.HoldProbe())
	require.Equal(t, "_INBOX_envoy._operator", natsgrant.OperatorInbox)
	require.Equal(t, "ENVOY_HOLD_BARRIER", natsgrant.HoldBarrierStream)
}

// The operator's inbox prefix sits under the one reply root, "_INBOX_envoy", as a token of its
// own, so a render that moves JetStream into an account of its own and exports only the reply
// root reaches the operator's replies through that one export. It fails when the operator's prefix
// leaves the root, as "_INBOX_envoy_operator" did, which shares the root's characters but not its
// token.
func TestTheOperatorInboxIsUnderTheReplyRoot(t *testing.T) {
	require.Equal(t, "_INBOX_envoy", natsgrant.ReplyRoot)
	require.True(t, strings.HasPrefix(natsgrant.OperatorInbox, natsgrant.ReplyRoot+"."),
		"OperatorInbox %q is not under the reply root %q", natsgrant.OperatorInbox, natsgrant.ReplyRoot)
	require.True(t, natsserver.SubjectMatchesFilter(natsgrant.OperatorInbox+".x", natsgrant.ReplyRoot+".>"),
		"the reply root's export %s.> does not reach an operator reply", natsgrant.ReplyRoot)
}

// No grant the package builds names the retired hold bucket, KV_envoy_maintenance or its keys:
// the hold lives in the broker's Postgres, and a NATS record of it could be stored over by any
// user with a JetStream API grant.
func TestNoGrantNamesTheRetiredHoldBucket(t *testing.T) {
	for name, grant := range grantTable(t) {
		for _, entry := range slices.Concat(grant.Publish, grant.Subscribe, grant.PublishDeny, grant.SubscribeDeny) {
			require.False(t, strings.Contains(entry, "KV_envoy_maintenance") || strings.Contains(entry, "$KV.envoy_maintenance."),
				"%s names the retired hold bucket in %q", name, entry)
		}
	}
}

// No grant the package builds subscribes a subject any KV bucket's key can have: each subscribe
// entry is held to $KV.> by NATS's own rule for two subjects that one literal subject could both
// match (nats-server's SubjectsCollide), so a $KV.<bucket>.> of any bucket, a $KV.*.>, a *.> or a
// > is caught as a literal key is. A push consumer of ENVOY_NOTIFICATIONS, which the prepare and
// held operator can create, becomes active only while some client subscribes exactly its deliver
// subject (nats-server v2.15.1 server/sublist.go:186-193), and then delivers each message under its
// own subject to every matching subscriber (server/stream.go:8620-8621), the bucket's stream, which
// captures $KV.<bucket>.>, among them: measured on one server, a notification was stored inside
// KV_envoy_sessions. A subscribe of a key subject in any grant would be that client.
func TestNoGrantSubscribesAKeySubject(t *testing.T) {
	const keys = "$KV.>"
	for name, grant := range grantTable(t) {
		for _, entry := range grant.Subscribe {
			fields := strings.Fields(entry)
			require.NotEmpty(t, fields, "%s subscribes a blank entry", name)
			require.False(t, natsserver.SubjectsCollide(fields[0], keys),
				"%s subscribes %q, which a key subject of some bucket (%s) matches", name, entry, keys)
		}
	}
	for _, pattern := range []string{"$KV.envoy_roles.>", "$KV.envoy_sessions.k", "$KV.*.>", ">", "*.>", "$KV.envoy_sessions.*"} {
		require.True(t, natsserver.SubjectsCollide(pattern, keys), "the rule misses %q", pattern)
	}
	for _, pattern := range []string{"_INBOX.>", "notifications.>", "$KV", "KV.envoy_roles.>"} {
		require.False(t, natsserver.SubjectsCollide(pattern, keys), "the rule refuses %q, which no key subject matches", pattern)
	}
}
