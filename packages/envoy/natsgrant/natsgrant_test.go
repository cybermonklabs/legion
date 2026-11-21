package natsgrant_test

import (
	"encoding/json"
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

// Every set this package builds is, element for element and in order, the set infra/core built at
// the merge base, so moving them here rewrites no hub's rendered authorization.
func TestEachGrantMatchesTheSetItReplaces(t *testing.T) {
	var base mergeBase
	readJSON(t, "testdata/merge-base-grants.json", &base)
	require.Equal(t, []string{"example-host-a", "example-dispatch", "example-hub"}, base.MachineIDs)
	for _, id := range base.MachineIDs {
		for name, pair := range map[string]struct {
			want set
			got  natsgrant.Permissions
		}{
			"Listener":     {base.Listener[id], natsgrant.Listener(id)},
			"ListenerHost": {base.ListenerHost[id], natsgrant.ListenerHost(id)},
			"Dispatch":     {base.Dispatch[id], natsgrant.Dispatch(id)},
			"Host":         {base.Host[id], natsgrant.Host(id)},
		} {
			requireSet(t, pair.want, pair.got, name+"("+strconv.Quote(id)+")")
		}
	}
	requireSet(t, base.SessionPattern, natsgrant.SessionPattern(), "SessionPattern()")
	requireSet(t, base.Daemon, natsgrant.Daemon(), "Daemon()")
	require.NotEmpty(t, base.DispatchDurables)
	require.Equal(t, base.DispatchDurables, natsgrant.DispatchDurables())
}

// Measure is the measurement job's grant as the deployment repository deploys it
// (envoy-cluster/hub.go, hubMeasurePublish and hubMeasureSubscribe, once the job's inbox moved
// under the reply root). The copy there goes when the job moves to the auth service, and this test
// with it.
func TestMeasureMatchesTheDeployedList(t *testing.T) {
	var deployed set
	readJSON(t, "testdata/deployment-measure-grant.json", &deployed)
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

// The measurement job's grant admits its inbox under the reply root alone, now that no job sets
// the prefix it used before, _INBOX_envoy_measure. Each row is the subscription nats.js 2.29.3
// makes for its requests, the prefix, a nuid and *, whose answers come on the prefix, the nuid and
// a token (protocol.js, the mux's SubscriptionImpl; muxsubscription.js, init). The grant admits no
// other client's replies: nats.go's _INBOX., another prefix under the root, or another token
// directly under it. The controls are the grant before this change, which admits the old prefix,
// and a deny entry over the inbox, which the helper must read as a refusal.
func TestMeasureSubscribesItsInboxUnderTheReplyRootAlone(t *testing.T) {
	rows := []struct {
		name, subscription string
		admitted           bool
	}{
		{"the inbox under the reply root", "_INBOX_envoy._measure.Fq3kZ.*", true},
		{"the prefix the job used before it moved", "_INBOX_envoy_measure.Fq3kZ.*", false},
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
	t.Run("control: the grant before this change admits the old prefix", func(t *testing.T) {
		before := natsgrant.Permissions{Subscribe: []string{"_INBOX_envoy_measure.>", "_INBOX_envoy._measure.>"}}
		require.True(t, subscribes(t, before, "_INBOX_envoy_measure.Fq3kZ.*"))
	})
	t.Run("control: a deny entry over the inbox refuses it", func(t *testing.T) {
		denied := natsgrant.Measure()
		denied.SubscribeDeny = []string{"_INBOX_envoy._measure.*.*"}
		require.False(t, subscribes(t, denied, "_INBOX_envoy._measure.Fq3kZ.*"))
	})
}

// Every reply prefix the auth service grants sits under one token, ReplyRoot, which a render that
// puts JetStream in an account of its own exports to the clients' account as ReplyRoot+".>": a
// push or ordered consumer's deliveries and a stream's republish reach a client only through a
// subject that account exports. Each row is one kind's inbox prefix and its grant: the prefix is
// under the root, the root's export reaches a reply under it, the grant subscribes it, and every
// subscribe of the grant is under the root.
func TestEveryReplyPrefixIsUnderTheReplyRoot(t *testing.T) {
	require.Equal(t, "_INBOX_envoy", natsgrant.ReplyRoot)
	export := natsgrant.ReplyRoot + ".>"
	rows := []struct {
		name, prefix string
		grant        natsgrant.Permissions
	}{
		{"measure", natsgrant.MeasureInbox, natsgrant.Measure()},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			subscription, reply := row.prefix+".Fq3kZ.*", row.prefix+".Fq3kZ.7"
			require.True(t, strings.HasPrefix(row.prefix, natsgrant.ReplyRoot+"."),
				"%s's prefix %q is not under the reply root %q", row.name, row.prefix, natsgrant.ReplyRoot)
			require.True(t, natsserver.SubjectMatchesFilter(reply, export),
				"the reply root's export %s does not reach %s's reply %q", export, row.name, reply)
			require.True(t, subscribes(t, row.grant, subscription),
				"%s's grant does not admit its own inbox's subscription %q", row.name, subscription)
			for _, entry := range row.grant.Subscribe {
				require.True(t, strings.HasPrefix(entry, natsgrant.ReplyRoot+"."),
					"%s subscribes %q, outside the reply root %q", row.name, entry, natsgrant.ReplyRoot)
			}
		})
	}
}

// A session's grant names its own subjects and no other session's, and an id that is not one
// subject token is refused, because a dot, a wildcard or a space in it would widen the grant.
func TestSessionGrantNamesOnlyItsOwnSession(t *testing.T) {
	grant, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	require.Equal(t, natsgrant.Permissions{
		Publish:   []string{"_INBOX.>", contracts.AgentStreamFramesSubject("ses_a")},
		Subscribe: []string{bus.DeliverSubject("ses_a"), contracts.AgentStreamControlSubject("ses_a")},
	}, grant)
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
	session, err := natsgrant.Session("ses_a")
	require.NoError(t, err)
	grants := map[string]natsgrant.Permissions{
		"SessionPattern()": natsgrant.SessionPattern(),
		"Session(ses_a)":   session,
		"Daemon()":         natsgrant.Daemon(),
		"Measure()":        natsgrant.Measure(),
		"KVRead":           {Publish: natsgrant.KVRead(bus.InterestBucket)},
		"KVWrite":          {Publish: natsgrant.KVWrite(bus.InterestBucket)},
	}
	for _, id := range []string{"example-host-a", "example-dispatch", "example-hub"} {
		grants["Listener("+id+")"] = natsgrant.Listener(id)
		grants["ListenerHost("+id+")"] = natsgrant.ListenerHost(id)
		grants["Dispatch("+id+")"] = natsgrant.Dispatch(id)
		grants["Host("+id+")"] = natsgrant.Host(id)
	}
	for name, grant := range grants {
		require.NotEmpty(t, slices.Concat(grant.Publish, grant.Subscribe), name)
		entry, found := natsgrant.NamesSystemSubject(grant)
		require.False(t, found, "%s names %q", name, entry)
	}
}
