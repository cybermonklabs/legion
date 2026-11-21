package natscallout

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	listenerAccount = "system:serviceaccount:example-ns:example-listener"
	daemonAccount   = "system:serviceaccount:example-ns:example-daemon"
	measureAccount  = "envoy-measure@example-project.iam.gserviceaccount.com"
	// privacyMeasureAccount is the account plan C's privacy measurement Job runs as.
	privacyMeasureAccount = "system:serviceaccount:example-ns:example-privacy-measure"
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

// The privacy measurement reads GET /v1/project-privacy, which answers every private project, so
// its entry takes the whole grant and is refused at the broker's start naming the entry when it
// asks for its exchange projects. It names the run by a machine id, as a listener does.
func TestParseServicesRefusesProjectsForPrivacyMeasure(t *testing.T) {
	_, err := ParseServices("privacy-measure/project-privacy-measure/-=" + privacyMeasureAccount)
	require.ErrorContains(t, err, `"privacy-measure/project-privacy-measure/-=`+privacyMeasureAccount+`"`)
	require.ErrorContains(t, err, "every private project")
	services, err := ParseServices("privacy-measure/project-privacy-measure/*=" + privacyMeasureAccount)
	require.NoError(t, err)
	require.Equal(t, []ServiceEntry{{Kind: KindPrivacyMeasure, MachineID: "project-privacy-measure", Account: privacyMeasureAccount, AllProjects: true}}, services)
}

// Every entry the broker could not serve is refused at start, naming the entry.
func TestParseServicesRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"an unknown kind":                            "relay/-/*=" + daemonAccount,
		"a dispatch entry with no machine id":        "dispatch/-/*=" + daemonAccount,
		"a listener entry with no machine id":        "listener/-/*=" + listenerAccount,
		"a privacy-measure entry with no machine id": "privacy-measure/-/*=" + privacyMeasureAccount,
		"a daemon entry with a machine id":           "daemon/host-a/-=" + daemonAccount,
		"a machine id that is no subject token":      "listener/a.b/*=" + listenerAccount,
		"a scope that is neither":                    "daemon/-/PROJ=" + daemonAccount,
		"no account":                                 "daemon/-/-",
		"too few parts":                              "daemon/-=" + daemonAccount,
		"an account of neither form":                 "daemon/-/-=example-daemon",
		"a namespace's default account":              "daemon/-/-=system:serviceaccount:example-ns:default",
		"a Google default account":                   "measure/-/*=google:123-compute@developer.gserviceaccount.com",
		"a Google service agent":                     "measure/-/*=google:service-123@gcp-sa-pubsub.iam.gserviceaccount.com",
		"an account given twice":                     "daemon/-/-=" + daemonAccount + " listener/host-a/*=" + daemonAccount,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseServices(raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), "BROKER_NATS_SERVICES")
		})
	}
}
