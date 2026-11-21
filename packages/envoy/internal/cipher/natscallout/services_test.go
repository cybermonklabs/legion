package natscallout

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/natsgrant"
)

const (
	listenerAccount        = "system:serviceaccount:example-ns:example-listener"
	webhookListenerAccount = "system:serviceaccount:example-ns:example-webhook-listener"
	daemonAccount          = "system:serviceaccount:example-ns:example-daemon"
	measureAccount         = "envoy-measure@example-project.iam.gserviceaccount.com"
)

// Each entry is read as <kind>/<machine id or ->/<* or ->=<account>, the account written as
// BROKER_API_ACCOUNTS writes one.
func TestParseServicesReadsEachEntry(t *testing.T) {
	services, err := ParseServices("listener/dev-stack/*=" + listenerAccount + "\n daemon/-/-=" + daemonAccount +
		" measure/-/*=google:" + measureAccount + " dispatch/example-dispatch/*=system:serviceaccount:dispatch:dispatch" +
		" webhook-listener/example-webhook/*=" + webhookListenerAccount)
	require.NoError(t, err)
	require.Equal(t, []ServiceEntry{
		{Kind: KindListener, MachineID: "dev-stack", Account: listenerAccount, AllProjects: true},
		{Kind: KindDaemon, Account: daemonAccount},
		{Kind: KindMeasure, Account: measureAccount, Google: true, AllProjects: true},
		{Kind: KindDispatch, MachineID: "example-dispatch", Account: "system:serviceaccount:dispatch:dispatch", AllProjects: true},
		{Kind: KindWebhookListener, MachineID: "example-webhook", Account: webhookListenerAccount, AllProjects: true},
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
		"a scope that is neither":                     "daemon/-/PROJ=" + daemonAccount,
		"no account":                                  "daemon/-/-",
		"too few parts":                               "daemon/-=" + daemonAccount,
		"an account of neither form":                  "daemon/-/-=example-daemon",
		"a namespace's default account":               "daemon/-/-=system:serviceaccount:example-ns:default",
		"a Google default account":                    "measure/-/*=google:123-compute@developer.gserviceaccount.com",
		"a Google service agent":                      "measure/-/*=google:service-123@gcp-sa-pubsub.iam.gserviceaccount.com",
		"an account given twice":                      "daemon/-/-=" + daemonAccount + " listener/host-a/*=" + daemonAccount,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseServices(raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), "BROKER_NATS_SERVICES")
		})
	}
}

// Check l16 of dispatch://INFRA-3589 (R18) at the auth service: a listener whose machine id is a
// fully qualified host name is listed under that id, as the listener starts with it
// (bus.CheckListenerMachineID), and the grant the service answers it with names the durable and
// the queue group the listener creates, each with its dots escaped into one subject token
// (bus.ListenerDurable, bus.ListenerQueue). cmd/listener's test of this name runs that listener.
func TestADottedMachineIDStartsAndDelivers(t *testing.T) {
	const machineID = "host.example.internal"
	for _, kind := range []Kind{KindListener, KindWebhookListener, KindDispatch} {
		t.Run(string(kind), func(t *testing.T) {
			services, err := ParseServices(string(kind) + "/" + machineID + "/*=" + listenerAccount)
			require.NoError(t, err)
			require.Equal(t, []ServiceEntry{{Kind: kind, MachineID: machineID, Account: listenerAccount, AllProjects: true}}, services)
			require.Equal(t, string(kind)+"/"+machineID, services[0].name())
			grant := serviceGrant(services[0])
			durable, queue := bus.ListenerDurable(machineID), bus.ListenerQueue(machineID)
			require.False(t, strings.ContainsAny(durable, ".*> "), "the durable %q is not one token", durable)
			require.Contains(t, grant.Publish, "$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS."+durable)
			require.Contains(t, grant.Publish, "$JS.ACK.ENVOY_NOTIFICATIONS."+durable+".>")
			require.Contains(t, grant.Subscribe, "notifications.role.> "+queue)
			_, found := natsgrant.NamesSystemSubject(grant)
			require.False(t, found)
		})
	}
}
