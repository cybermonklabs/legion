package gcp_test

import (
	"slices"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

// dispatchResourceName is one registration of the component's, by its type and name.
type dispatchResourceName struct{ typ, name string }

// dispatchNetworkKept are the resources of the network half outside clusterOnly that register
// whatever regionalNetwork says: the Network the component's own cluster and an existing one both
// run on, and the private services range and peering the Cloud SQL instance takes its private
// address through. dispatchNetworkRegional are the ones regionalNetwork decides: the Subnetwork,
// Router and RouterNat in region. Each is named as the default golden holds it.
var (
	dispatchNetworkKept = []dispatchResourceName{
		{networkType, "example-dispatch-network"},
		{globalAddressType, "example-dispatch-services"},
		{connectionType, "example-dispatch-services"},
	}
	dispatchNetworkRegional = []dispatchResourceName{
		{subnetworkType, "example-dispatch"},
		{routerType, "example-dispatch"},
		{routerNatType, "example-dispatch"},
	}
)

// existingClusterRegionalNetworkArgs is existingClusterArgs with regionalNetwork set to value.
func existingClusterRegionalNetworkArgs(value bool) *gcp.DispatchArgs {
	args := existingClusterArgs()
	args.RegionalNetwork = pulumi.BoolRef(value)
	return args
}

// With existingCluster and regionalNetwork false the component registers exactly what the default
// inputs register, less its own Cluster and the regional part of its network: no Subnetwork,
// Router or RouterNat registers. The comparison is of the whole registration set, so any of the
// three, or any resource the default does not register, is a line it does not hold.
func TestDispatchRegionalNetworkFalseRegistersNoRegionalNetwork(t *testing.T) {
	m, err := runDispatch(t, existingClusterRegionalNetworkArgs(false), true)
	require.NoError(t, err)
	dropped := slices.Concat([]dispatchResourceName{{clusterType, "example-dispatch"}}, dispatchNetworkRegional)
	require.Equal(t, dispatchGoldenWithout(t, dropped...), dispatchRegistrations(m))
}

// regionalNetwork true is its value when unset, so beside existingCluster it registers exactly what
// existingCluster with the input unset registers: the same registration set, and the network half,
// the regional part with the rest, registered as it is there, each resource a same step from that
// run to this one (dispatchPlanBetween).
func TestDispatchRegionalNetworkTrueRegistersWhatUnsetDoes(t *testing.T) {
	unset, err := runDispatch(t, existingClusterArgs(), true)
	require.NoError(t, err)
	m, err := runDispatch(t, existingClusterRegionalNetworkArgs(true), true)
	require.NoError(t, err)
	require.Equal(t, dispatchRegistrations(unset), dispatchRegistrations(m))
	same := map[dispatchResourceName]string{}
	for _, r := range slices.Concat(dispatchNetworkKept, dispatchNetworkRegional) {
		same[r] = stepSame
	}
	requireDispatchPlan(t, dispatchPlanBetween(t, unset, m), same)
}

// regionalNetwork false without existingCluster would leave the component's own cluster no
// subnetwork to run on, so the resolver refuses it, as a core.InputError naming regionalNetwork,
// rather than dropping it; NewDispatch then fails naming the component and regionalNetwork before
// anything registers, so nothing beyond the program's three providers exists. regionalNetwork true
// without existingCluster is the default path, and registers exactly the default golden.
func TestDispatchRefusesRegionalNetworkFalseWithoutAnExistingCluster(t *testing.T) {
	const refusal = "regionalNetwork false leaves the component's own cluster no subnetwork: set existingCluster, or unset regionalNetwork"
	args := dispatchArgs()
	args.RegionalNetwork = pulumi.BoolRef(false)
	_, err := gcp.ResolveDispatch(args)
	require.EqualError(t, err, refusal)
	var refused *core.InputError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "regionalNetwork", refused.Input)

	m, err := runDispatch(t, args, true)
	require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+refusal)
	require.ElementsMatch(t, []string{gcpProviderType, randomProviderType, tailscaleProviderType}, m.Types(),
		"the program's own three providers registered and nothing of the component's")

	args.RegionalNetwork = pulumi.BoolRef(true)
	m, err = runDispatch(t, args, true)
	require.NoError(t, err)
	require.Equal(t, readGolden(t, "dispatch-registrations.golden"), dispatchRegistrations(m))
}
