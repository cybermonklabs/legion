package natscallout

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/natsgrant"
)

const (
	listenerAccount = "system:serviceaccount:example-ns:example-listener"
	daemonAccount   = "system:serviceaccount:example-ns:example-daemon"
	measureAccount  = "envoy-measure@example-project.iam.gserviceaccount.com"
)

// Each entry is read as <kind>/<machine id or ->/<* or ->=<account>, the account written as
// BROKER_API_ACCOUNTS writes one.
func TestParseServicesReadsEachEntry(t *testing.T) {
	services, err := ParseServices("listener/dev-stack/*=" + listenerAccount + "\n daemon/-/-=" + daemonAccount +
		" measure/-/*=google:" + measureAccount + " dispatch/example-dispatch/*=system:serviceaccount:dispatch:dispatch")
	require.NoError(t, err)
	require.Equal(t, []ServiceEntry{
		{Kind: KindListener, MachineID: "dev-stack", Account: listenerAccount, AllProjects: true},
		{Kind: KindDaemon, Account: daemonAccount},
		{Kind: KindMeasure, Account: measureAccount, Google: true, AllProjects: true},
		{Kind: KindDispatch, MachineID: "example-dispatch", Account: "system:serviceaccount:dispatch:dispatch", AllProjects: true},
	}, services)
	none, err := ParseServices("  ")
	require.NoError(t, err)
	require.Empty(t, none)
}

// The measurement job's grant holds a KV read, which no project scope can hold, and it reads no
// project's messages, so its entry takes the whole grant and is refused at the broker's start
// naming the entry when it asks for its exchange projects.
func TestParseServicesRefusesProjectsForMeasure(t *testing.T) {
	_, err := ParseServices("measure/-/-=google:" + measureAccount)
	require.ErrorContains(t, err, `"measure/-/-=google:`+measureAccount+`"`)
	require.ErrorContains(t, err, "measure")
	services, err := ParseServices("measure/-/*=google:" + measureAccount)
	require.NoError(t, err)
	require.True(t, services[0].AllProjects)
}

// Every entry the broker could not serve is refused at start, naming the entry.
func TestParseServicesRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"an unknown kind":                       "relay/-/*=" + daemonAccount,
		"a dispatch entry with no machine id":   "dispatch/-/*=" + daemonAccount,
		"a listener entry with no machine id":   "listener/-/*=" + listenerAccount,
		"a daemon entry with a machine id":      "daemon/host-a/-=" + daemonAccount,
		"a machine id that is no subject token": "listener/a.b/*=" + listenerAccount,
		"a scope that is neither":               "daemon/-/PROJ=" + daemonAccount,
		"no account":                            "daemon/-/-",
		"too few parts":                         "daemon/-=" + daemonAccount,
		"an account of neither form":            "daemon/-/-=example-daemon",
		"a namespace's default account":         "daemon/-/-=system:serviceaccount:example-ns:default",
		"a Google default account":              "measure/-/*=google:123-compute@developer.gserviceaccount.com",
		"a Google service agent":                "measure/-/*=google:service-123@gcp-sa-pubsub.iam.gserviceaccount.com",
		"an account given twice":                "daemon/-/-=" + daemonAccount + " listener/host-a/*=" + daemonAccount,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseServices(raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), "BROKER_NATS_SERVICES")
		})
	}
}

// The deployment's one operator identity and the writer hold's one probe are entries of their own
// kinds, each listed once, with no machine id and the whole grant: the hold and the buckets
// command reach no project's messages, and no project scope can hold a stream create. A second
// entry of either kind, a machine id, or the scope - is refused, naming the entry.
func TestParseServicesReadsTheOperatorKind(t *testing.T) {
	const operator = "system:serviceaccount:envoy-operator:envoy-operator"
	const probe = "system:serviceaccount:envoy-operator:envoy-hold-probe"
	services, err := ParseServices("operator/-/*=" + operator + " probe/-/*=" + probe)
	require.NoError(t, err)
	require.Equal(t, []ServiceEntry{
		{Kind: KindOperator, Account: operator, AllProjects: true},
		{Kind: KindProbe, Account: probe, AllProjects: true},
	}, services)
	require.Equal(t, "operator/"+operator, services[0].name())
	require.Equal(t, natsgrant.HoldProbe(), serviceGrant(services[1]))
	for raw, names := range map[string]string{
		"operator/m1/*=" + operator: "operator/m1/*=" + operator,
		"operator/-/-=" + operator:  "operator/-/-=" + operator,
		"operator/-/*=" + operator + " operator/-/*=system:serviceaccount:envoy-operator:second": "operator/-/*=system:serviceaccount:envoy-operator:second",
		"probe/m1/*=" + probe: "probe/m1/*=" + probe,
		"probe/-/-=" + probe:  "probe/-/-=" + probe,
		"probe/-/*=" + probe + " probe/-/*=system:serviceaccount:envoy-operator:second": "probe/-/*=system:serviceaccount:envoy-operator:second",
	} {
		_, err := ParseServices(raw)
		require.ErrorContains(t, err, strconv.Quote(names), "ParseServices(%q)", raw)
	}
}
