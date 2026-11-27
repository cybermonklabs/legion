package config

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/api"
)

const (
	listenerAccount        = "system:serviceaccount:example-ns:example-listener"
	webhookListenerAccount = "system:serviceaccount:example-ns:example-webhook-listener"
	daemonAccount          = "system:serviceaccount:example-ns:example-daemon"
	measureAccount         = "envoy-measure@example-project.iam.gserviceaccount.com"
	edgeAccount            = "system:serviceaccount:example-ns:example-edge-listener"
)

// Each entry is read as <kind>/<machine id or ->/<* or ->=<account>, the account written as
// BROKER_API_ACCOUNTS writes one, and keeps its own text for the refusals that name it.
func TestParseNATSServicesReadsEachEntry(t *testing.T) {
	listener, daemon := "listener/dev-stack/*="+listenerAccount, "daemon/-/-="+daemonAccount
	measure, dispatch := "measure/-/*=google:"+measureAccount, "dispatch/example-dispatch/*=system:serviceaccount:dispatch:dispatch"
	webhook := "webhook-listener/example-webhook/*=" + webhookListenerAccount
	services, err := ParseNATSServices(listener + "\n " + daemon + " " + measure + " " + dispatch + " " + webhook)
	require.NoError(t, err)
	require.Equal(t, []NATSService{
		{Kind: NATSListener, MachineID: "dev-stack", Account: listenerAccount, AllProjects: true, Text: listener},
		{Kind: NATSDaemon, Account: daemonAccount, Text: daemon},
		{Kind: NATSMeasure, Account: measureAccount, Google: true, AllProjects: true, Text: measure},
		{Kind: NATSDispatch, MachineID: "example-dispatch", Account: "system:serviceaccount:dispatch:dispatch", AllProjects: true, Text: dispatch},
		{Kind: NATSWebhookListener, MachineID: "example-webhook", Account: webhookListenerAccount, AllProjects: true, Text: webhook},
	}, services)
	none, err := ParseNATSServices("  ")
	require.NoError(t, err)
	require.Empty(t, none)
}

// An edge listener's entry names its machine id and, in place of a scope, the projects it serves,
// comma separated: edge-listener/<machine id>/<KEY>[,<KEY>…]=<account>. The projects are read
// sorted, each once, and no other kind's entry carries any.
func TestParseNATSServicesReadsAnEdgeListenersProjects(t *testing.T) {
	edge, listener := "edge-listener/edge-a/PRIV,OTHER="+edgeAccount, "listener/host-a/*="+listenerAccount
	services, err := ParseNATSServices(edge + " " + listener)
	require.NoError(t, err)
	require.Equal(t, []NATSService{
		{Kind: NATSEdgeListener, MachineID: "edge-a", Account: edgeAccount, Projects: []string{"OTHER", "PRIV"}, Text: edge},
		{Kind: NATSListener, MachineID: "host-a", Account: listenerAccount, AllProjects: true, Text: listener},
	}, services)
}

// EdgeListeners is each edge listener's entry as the broker's API reads it, its account and its
// projects, and no entry of another kind: a listener's account is no edge listener, whatever
// projects its own grant reaches.
func TestEdgeListenersAreTheEdgeListenerEntries(t *testing.T) {
	services, err := ParseNATSServices("listener/host-a/*=" + listenerAccount + " edge-listener/edge-a/PRIV=" + edgeAccount +
		" edge-listener/edge-g/OTHER=google:example-edge@example-project.iam.gserviceaccount.com daemon/-/-=" + daemonAccount)
	require.NoError(t, err)
	require.Equal(t, []api.EdgeListener{
		{Account: edgeAccount, Projects: []string{"PRIV"}},
		{Account: "example-edge@example-project.iam.gserviceaccount.com", Google: true, Projects: []string{"OTHER"}},
	}, NATSCallout{Services: services}.EdgeListeners())
	require.Empty(t, NATSCallout{}.EdgeListeners())
}

// The measurement job's grant holds a KV read, which no project scope can hold, and it reads no
// project's messages, so its entry takes the whole grant and is refused at the broker's start
// naming the entry when it asks for its exchange projects.
func TestParseNATSServicesRefusesProjectsForMeasure(t *testing.T) {
	_, err := ParseNATSServices("measure/-/-=google:" + measureAccount)
	require.ErrorContains(t, err, `"measure/-/-=google:`+measureAccount+`"`)
	require.ErrorContains(t, err, "measure")
	services, err := ParseNATSServices("measure/-/*=google:" + measureAccount)
	require.NoError(t, err)
	require.True(t, services[0].AllProjects)
}

// Every entry the broker could not serve is refused at start, naming the entry.
func TestParseNATSServicesRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"an unknown kind":                             "relay/-/*=" + daemonAccount,
		"a dispatch entry with no machine id":         "dispatch/-/*=" + daemonAccount,
		"a listener entry with no machine id":         "listener/-/*=" + listenerAccount,
		"a webhook-listener entry with no machine id": "webhook-listener/-/*=" + webhookListenerAccount,
		"a daemon entry with a machine id":            "daemon/host-a/-=" + daemonAccount,
		"an empty machine id":                         "listener//*=" + listenerAccount,
		"a machine id holding a control character":    "listener/a\x01b/*=" + listenerAccount,
		"a machine id holding a wildcard":             "listener/a*b/*=" + listenerAccount,
		"a machine id holding a space":                "listener/a\u00a0b/*=" + listenerAccount,
		"a machine id holding the escape character":   "listener/a%2Eb/*=" + listenerAccount,
		"a machine id whose durable name is too long": "listener/" + strings.Repeat("a", 250) + "/*=" + listenerAccount,
		"an edge machine id holding the escape":       "edge-listener/a%2Eb/PRIV=" + edgeAccount,
		"a scope that is neither":                     "daemon/-/PROJ=" + daemonAccount,
		"no account":                                  "daemon/-/-",
		"too few parts":                               "daemon/-=" + daemonAccount,
		"an account of neither form":                  "daemon/-/-=example-daemon",
		"a namespace's default account":               "daemon/-/-=system:serviceaccount:example-ns:default",
		"a Google default account":                    "measure/-/*=google:123-compute@developer.gserviceaccount.com",
		"a Google service agent":                      "measure/-/*=google:service-123@gcp-sa-pubsub.iam.gserviceaccount.com",
		"an account given twice":                      "daemon/-/-=" + daemonAccount + " listener/host-a/*=" + daemonAccount,
		"an edge listener with no machine id":         "edge-listener/-/PRIV=" + edgeAccount,
		"an edge listener naming no project key":      "edge-listener/edge-a/priv=" + edgeAccount,
		"an edge listener naming a project twice":     "edge-listener/edge-a/PRIV,PRIV=" + edgeAccount,
		"an edge listener naming an empty project":    "edge-listener/edge-a/PRIV,=" + edgeAccount,
		"an edge listener naming seventeen projects":  "edge-listener/edge-a/AA,AB,AC,AD,AE,AF,AG,AH,AI,AJ,AK,AL,AM,AN,AO,AP,AQ=" + edgeAccount,
		"projects for a listener":                     "listener/host-a/PRIV=" + listenerAccount,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseNATSServices(raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), "BROKER_NATS_SERVICES")
		})
	}
}

// Check l16 of dispatch://INFRA-3589 (R18) at the setting: each kind of entry that names a machine
// takes a dotted machine id, as the listener starts with one (bus.CheckListenerMachineID), and
// keeps that id whole. natscallout's TestADottedMachineIDStartsAndDelivers checks the grant the auth
// service answers such an entry with, and its TestADottedEdgeMachineIDIsGrantedItsSpelledPrefix
// the edge listener's.
func TestParseNATSServicesTakesADottedMachineID(t *testing.T) {
	const machineID = "host.example.internal"
	for kind, scope := range map[NATSServiceKind]string{NATSListener: "*", NATSWebhookListener: "*", NATSDispatch: "*", NATSEdgeListener: "PRIV"} {
		t.Run(string(kind), func(t *testing.T) {
			services, err := ParseNATSServices(string(kind) + "/" + machineID + "/" + scope + "=" + listenerAccount)
			require.NoError(t, err)
			require.Len(t, services, 1)
			require.Equal(t, machineID, services[0].MachineID)
			require.Equal(t, string(kind)+"/"+machineID, services[0].Name())
		})
	}
}

// The deployment's one operator identity and the writer hold's one probe are entries of their own
// kinds, each listed once, with no machine id and the whole grant: the hold and the buckets
// command reach no project's messages, and no project scope can hold a stream create. A second
// entry of either kind, a machine id, or the scope - is refused, naming the entry.
func TestParseNATSServicesReadsTheOperatorKind(t *testing.T) {
	const operator = "system:serviceaccount:envoy-operator:envoy-operator"
	const probe = "system:serviceaccount:envoy-operator:envoy-hold-probe"
	operatorText, probeText := "operator/-/*="+operator, "probe/-/*="+probe
	services, err := ParseNATSServices(operatorText + " " + probeText)
	require.NoError(t, err)
	require.Equal(t, []NATSService{
		{Kind: NATSOperator, Account: operator, AllProjects: true, Text: operatorText},
		{Kind: NATSProbe, Account: probe, AllProjects: true, Text: probeText},
	}, services)
	require.Equal(t, "operator/"+operator, services[0].Name())
	for raw, names := range map[string]string{
		"operator/m1/*=" + operator: "operator/m1/*=" + operator,
		"operator/-/-=" + operator:  "operator/-/-=" + operator,
		"operator/-/*=" + operator + " operator/-/*=system:serviceaccount:envoy-operator:second": "operator/-/*=system:serviceaccount:envoy-operator:second",
		"probe/m1/*=" + probe: "probe/m1/*=" + probe,
		"probe/-/-=" + probe:  "probe/-/-=" + probe,
		"probe/-/*=" + probe + " probe/-/*=system:serviceaccount:envoy-operator:second": "probe/-/*=system:serviceaccount:envoy-operator:second",
	} {
		_, err := ParseNATSServices(raw)
		require.ErrorContains(t, err, strconv.Quote(names), "ParseNATSServices(%q)", raw)
	}
}

// An edge listener's entry that gives it every project (*) or its exchange's projects (-) is
// refused by its own rule, naming the form an edge entry takes, before its scope is read as a
// list of project keys: an edge listener serves only the projects its entry names.
func TestAnEdgeEntryOfEveryOrTheExchangeProjectsIsRefusedNamingWhy(t *testing.T) {
	for _, scope := range []string{allProjectsScope, exchangeScope} {
		_, err := ParseNATSServices("edge-listener/edge-a/" + scope + "=" + edgeAccount)
		require.ErrorContains(t, err, "an edge listener serves only the projects its entry names", "scope %q", scope)
	}
}
