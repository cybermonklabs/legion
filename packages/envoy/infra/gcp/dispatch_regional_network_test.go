package gcp_test

import (
	"slices"
	"strings"
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
		{"gcp:compute/globalAddress:GlobalAddress", "example-dispatch-services"},
		{"gcp:servicenetworking/connection:Connection", "example-dispatch-services"},
	}
	dispatchNetworkRegional = []dispatchResourceName{
		{"gcp:compute/subnetwork:Subnetwork", "example-dispatch"},
		{"gcp:compute/router:Router", "example-dispatch"},
		{"gcp:compute/routerNat:RouterNat", "example-dispatch"},
	}
)

// existingClusterRegionalNetworkArgs is existingClusterArgs with regionalNetwork set to value.
func existingClusterRegionalNetworkArgs(value bool) *gcp.DispatchArgs {
	args := existingClusterArgs()
	args.RegionalNetwork = pulumi.BoolRef(value)
	return args
}

// dispatchNoRegionalNetworkGolden is the whole declaration of a component on an existing cluster
// with regionalNetwork false, taken from the default mode's registration golden: every line of it
// less the lines of Cluster, Subnetwork, Router and RouterNat example-dispatch, the component's own
// cluster and the regional part of its network. Reading it from the golden rather than writing it
// out again holds the two to one declaration: a resource the default renames or reparents moves
// this expectation with it.
func dispatchNoRegionalNetworkGolden(t *testing.T) string {
	t.Helper()
	dropped := []string{"gcp:container/cluster:Cluster example-dispatch "}
	for _, regional := range dispatchNetworkRegional {
		dropped = append(dropped, regional.typ+" "+regional.name+" ")
	}
	var kept strings.Builder
	count := 0
	for _, line := range strings.SplitAfter(readGolden(t, "dispatch-registrations.golden"), "\n") {
		if slices.ContainsFunc(dropped, func(prefix string) bool { return strings.HasPrefix(line, prefix) }) {
			count++
			continue
		}
		kept.WriteString(line)
	}
	require.Equal(t, len(dropped), count, "the golden holds one line each for the component's own cluster, subnetwork, router and Cloud NAT")
	return kept.String()
}

// With existingCluster and regionalNetwork false the component registers exactly what the default
// inputs register, less its own Cluster and the regional part of its network: no Subnetwork,
// Router or RouterNat registers. The comparison is of the whole registration set, so any of the
// three, or any resource the default does not register, is a line it does not hold. The Network,
// the GlobalAddress and the Connection register as they do beside existingCluster with
// regionalNetwork unset, each a same step from that run to this one (dispatchPlanBetween), so the
// Cloud SQL instance keeps its private address while the region empties.
func TestDispatchRegionalNetworkFalseRegistersNoRegionalNetwork(t *testing.T) {
	m, err := runDispatch(t, existingClusterRegionalNetworkArgs(false), true)
	require.NoError(t, err)
	require.Equal(t, dispatchNoRegionalNetworkGolden(t), dispatchRegistrations(m))

	unset, err := runDispatch(t, existingClusterArgs(), true)
	require.NoError(t, err)
	same := map[dispatchResourceName]string{}
	for _, r := range dispatchNetworkKept {
		same[r] = stepSame
	}
	requireDispatchPlan(t, dispatchPlanBetween(t, unset, m), same)
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
