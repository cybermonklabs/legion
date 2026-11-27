package core_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
)

// A listener whose machine id is a fully qualified host name starts and delivers, and the grant
// envoy-infra builds for it lets it do so: the machine-id rule accepts the id, and every grant of a
// user that runs a listener names the durable and the queue group the listener creates
// (bus.ListenerDurable, bus.ListenerQueue), each a name NATS takes and one token of its subject.
// cmd/listener's test of this name runs that listener.
func TestADottedMachineIDStartsAndDelivers(t *testing.T) {
	const machineID = "host.example.internal"
	require.NoError(t, core.CheckMachineID("machineId", machineID))
	require.NoError(t, core.ListenerConfig{MachineID: machineID, Port: core.DefaultListenerPort}.Check())

	durable, queue := bus.ListenerDurable(machineID), bus.ListenerQueue(machineID)
	// nats-server refuses these in a consumer name (server/jetstream.go isValidAssetName), and a
	// dot or a wildcard would split or widen the subject token that carries the name.
	require.False(t, strings.ContainsAny(durable, " \t\r\n\f.*>\\/"), "the listener's durable %q is no name NATS takes", durable)
	require.False(t, strings.ContainsAny(queue, " \t\r\n\f.*>"), "the listener's queue group %q is not one token", queue)

	for _, row := range []struct {
		name  string
		grant core.Permissions
	}{
		{"ListenerPermissions", core.ListenerPermissions(machineID)},
		{"ListenerHostPermissions", core.ListenerHostPermissions(machineID)},
		{"HostPermissions", core.HostPermissions(machineID)},
		{"DispatchPermissions", core.DispatchPermissions(machineID)},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, entry := range []string{
				"$JS.API.CONSUMER.INFO.ENVOY_NOTIFICATIONS." + durable,
				"$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS." + durable,
				"$JS.ACK.ENVOY_NOTIFICATIONS." + durable + ".>",
			} {
				require.Contains(t, row.grant.Publish.Allow, entry)
			}
			require.Contains(t, row.grant.Subscribe.Allow, "notifications.role.> "+queue)
		})
	}
}
