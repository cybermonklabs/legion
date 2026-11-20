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

// Measure is the measurement job's grant as the deployment repository deploys it today
// (envoy-cluster/hub.go, hubMeasurePublish and hubMeasureSubscribe at fe26eb31). The copy there
// goes when the job moves to the auth service, and this test with it.
func TestMeasureMatchesTheDeployedList(t *testing.T) {
	var deployed set
	readJSON(t, "testdata/deployment-measure-grant.json", &deployed)
	requireSet(t, deployed, natsgrant.Measure(), "Measure()")
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
// it to natsgrant.go's declarations, so a new grant function fails that test until it is listed
// here, and every test that walks the table walks the new grant too.
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
		grants["ListenerHost("+id+")"] = natsgrant.ListenerHost(id)
		grants["Dispatch("+id+")"] = natsgrant.Dispatch(id)
		grants["Host("+id+")"] = natsgrant.Host(id)
	}
	return grants
}

// Every exported function natsgrant.go declares that returns Permissions, read from the source by
// go/ast, has a row in grantTable, and every row names one of them: a grant added to the package
// is walked by every test that reads the table, or this test names it.
func TestTheGrantTableNamesEveryGrant(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "natsgrant.go", nil, 0)
	require.NoError(t, err)
	declared := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil {
			continue
		}
		if result, ok := fn.Type.Results.List[0].Type.(*ast.Ident); ok && result.Name == "Permissions" {
			declared[fn.Name.Name] = true
		}
	}
	require.NotEmpty(t, declared, "natsgrant.go declares no grant function: the reading enumerated nothing")
	listed := map[string]bool{}
	for call := range grantTable(t) {
		name, _, _ := strings.Cut(call, "(")
		require.True(t, declared[name], "grantTable lists %s, which natsgrant.go does not declare as a grant", call)
		listed[name] = true
	}
	for name := range declared {
		require.True(t, listed[name], "natsgrant.go declares the grant %s, which grantTable does not list", name)
	}
}

// The buckets envoy-infra buckets copies, whose backup the operator takes outside a hold and whose
// streams it makes, changes, removes and restores only inside one.
var copiedBuckets = []string{"envoy_interests", "envoy_roles", "envoy_sessions", "envoy_known_sessions", "envoy_ci_state"}

// operatorReleased is Operator(OperatorPhaseReleased)'s publish list as the plan writes it.
func operatorReleased() []string {
	publish := []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO.ENVOY_NOTIFICATIONS",
		"$JS.API.STREAM.INFO.KV_envoy_session_consumers",
	}
	for _, b := range copiedBuckets {
		publish = append(publish, "$JS.API.STREAM.INFO.KV_"+b)
	}
	for _, b := range copiedBuckets {
		publish = append(publish, "$JS.API.STREAM.SNAPSHOT.KV_"+b, "$JS.SNAPSHOT.ACK.KV_"+b+".>")
	}
	return append(publish,
		"$JS.API.STREAM.MSG.GET.KV_envoy_session_consumers",
		"$JS.API.CONSUMER.NAMES.ENVOY_NOTIFICATIONS",
		"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS.*",
		"$JS.API.STREAM.CREATE.ENVOY_MEASURE",
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

// operatorHeld is Operator(OperatorPhaseHeld)'s publish list as the plan writes it.
func operatorHeld() []string {
	publish := append(operatorPrepare(),
		"$JS.API.STREAM.CREATE.ENVOY_HOLD_BARRIER",
		"$JS.API.STREAM.DELETE.ENVOY_HOLD_BARRIER",
		"$JS.API.STREAM.LIST",
		"$JS.API.STREAM.NAMES",
		"$JS.API.CONSUMER.LIST.*",
		"$JS.API.CONSUMER.DELETE.ENVOY_NOTIFICATIONS.*",
		"$KV.envoy_session_consumers.>",
		"$JS.API.STREAM.CREATE.KV_envoy_session_consumers",
		"$JS.API.STREAM.RESTORE.KV_envoy_session_consumers",
		"$JS.SNAPSHOT.RESTORE.KV_envoy_session_consumers.*",
	)
	for _, b := range copiedBuckets {
		publish = append(publish,
			"$JS.API.STREAM.CREATE.KV_"+b,
			"$JS.API.STREAM.UPDATE.KV_"+b,
			"$JS.API.STREAM.DELETE.KV_"+b,
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
	inbox := []string{"_INBOX_envoy_operator.>"}
	for phase, want := range map[natsgrant.OperatorPhase][]string{
		natsgrant.OperatorPhaseReleased: operatorReleased(),
		natsgrant.OperatorPhasePrepare:  operatorPrepare(),
		natsgrant.OperatorPhaseHeld:     operatorHeld(),
	} {
		got := natsgrant.Operator(phase)
		for _, entry := range got.Publish {
			require.Contains(t, want, entry, "Operator(%s) publishes %q, which the plan's list does not hold", phase, entry)
		}
		require.Equal(t, natsgrant.Permissions{Publish: want, Subscribe: inbox}, got, "Operator(%s)", phase)
	}
	require.Equal(t, natsgrant.Operator(natsgrant.OperatorPhaseReleased), natsgrant.Operator("paused"),
		"a phase outside the three is answered with the released grant")
	require.Equal(t, natsgrant.Permissions{
		Publish:   []string{"_ENVOY_HOLD_PROBE.none"},
		Subscribe: []string{"_ENVOY_HOLD_PROBE.none"},
	}, natsgrant.HoldProbe())
	require.Equal(t, "_INBOX_envoy_operator", natsgrant.OperatorInbox)
	require.Equal(t, "ENVOY_HOLD_BARRIER", natsgrant.HoldBarrierStream)
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
