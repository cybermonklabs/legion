package gcp_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

var (
	// dispatchLoadBalancerRanges are Google's proxy and health-check ranges, the sources a
	// Google Cloud load balancer reaches a backend's pods from.
	dispatchLoadBalancerRanges = []string{"35.191.0.0/16", "130.211.0.0/22"}
	// dispatchNEGName is a standalone network endpoint group's name.
	dispatchNEGName = "example-dispatch-neg"
)

// dispatchEntranceArgs are dispatchPublicOriginArgs with the deployment's load balancer named:
// the ranges it reaches the app from and the endpoint group it takes as its backend.
func dispatchEntranceArgs() *gcp.DispatchArgs {
	args := dispatchPublicOriginArgs()
	args.LoadBalancerSourceRanges = dispatchLoadBalancerRanges
	args.NetworkEndpointGroupName = dispatchNEGName
	return args
}

// appServiceAnnotations are the annotations on the app's Service, nil for none.
func appServiceAnnotations(t *testing.T, args *gcp.DispatchArgs) map[string]any {
	t.Helper()
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	metadata := m.Named(t, "kubernetes:core/v1:Service", "example-dispatch").Inputs.Mappable()["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	return annotations
}

// The ranges render as ipBlock peers on 8766 alone, exactly the ranges given in their order, in
// the app's own NetworkPolicy, which therefore is the one policy selecting the Dispatch pods; the
// allowedNamespaces rule stays beside them. Control: the same arguments with no ranges render no
// such rule, so the rule is the input's.
func TestDispatchAdmitsTheLoadBalancersRanges(t *testing.T) {
	args := dispatchEntranceArgs()
	args.AllowedNamespaces = []string{"legion"}
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	rules := policyIngressRules(t, m, "example-dispatch", 2)
	require.Equal(t, []any{map[string]any{"port": float64(8766)}}, rules[1].(map[string]any)["ports"])
	var cidrs []string
	for _, peer := range rules[1].(map[string]any)["from"].([]any) {
		fields := peer.(map[string]any)
		require.Len(t, fields, 1, "a range's peer is an ipBlock alone, with no selector beside it")
		block := fields["ipBlock"].(map[string]any)
		require.Len(t, block, 1, "no except list")
		cidrs = append(cidrs, block["cidr"].(string))
	}
	require.Equal(t, dispatchLoadBalancerRanges, cidrs)
	require.Equal(t, []any{map[string]any{"port": float64(8766)}}, rules[0].(map[string]any)["ports"])
	require.NotContains(t, rules[0].(map[string]any)["from"].([]any)[0].(map[string]any), "ipBlock", "the namespaces' rule names no range")

	control := dispatchEntranceArgs()
	control.AllowedNamespaces = []string{"legion"}
	control.LoadBalancerSourceRanges = nil
	m, err = runDispatch(t, control, true)
	require.NoError(t, err)
	policyIngressRules(t, m, "example-dispatch", 1)
}

// The app's Service carries cloud.google.com/neg naming the endpoint group and the port 8766, with
// the group pre-provisioned in every zone of the region (`"zones":["*"]`), and nothing else beside
// it: the annotation is exactly that JSON, so a field GKE does not read cannot slip in. Control:
// without the name it carries no annotation at all.
func TestDispatchNamesItsNetworkEndpointGroup(t *testing.T) {
	annotations := appServiceAnnotations(t, dispatchEntranceArgs())
	require.Len(t, annotations, 1)
	value := annotations["cloud.google.com/neg"].(string)
	require.Equal(t, `{"exposed_ports":{"8766":{"name":"`+dispatchNEGName+`"}},"zones":["*"]}`, value)
	var neg struct {
		ExposedPorts map[string]map[string]string `json:"exposed_ports"`
		Zones        []string                     `json:"zones"`
	}
	require.NoError(t, json.Unmarshal([]byte(value), &neg))
	require.Equal(t, map[string]map[string]string{"8766": {"name": dispatchNEGName}}, neg.ExposedPorts)
	require.Equal(t, []string{"*"}, neg.Zones)

	control := dispatchEntranceArgs()
	control.NetworkEndpointGroupName = ""
	require.Empty(t, appServiceAnnotations(t, control))
}

// Each input is refused without publicOrigin, by its own name, in every mode that has none: on
// the tailnet and in the sandbox. A range that is no CIDR, is written other than as its network,
// or is /0, which admits every address past the load balancer and IAP, and a name that is no
// Compute Engine resource name, are refused too, and so is an oidcIssuer with an empty query or
// fragment. Control: the arguments every row edits resolve.
func TestDispatchRefusesAnEntranceItHasNoOriginFor(t *testing.T) {
	needs := func(input string) string {
		return input + " needs publicOrigin: it describes the deployment's own load balancer, which serves Dispatch at that origin, " +
			"and a Dispatch reached through the tailnet front door or inside its cluster has none; set publicOrigin, or unset " + input
	}
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
		want string
	}{
		{"ranges on the tailnet", func() *gcp.DispatchArgs {
			a := dispatchArgs()
			a.LoadBalancerSourceRanges = dispatchLoadBalancerRanges
			return a
		}, needs("loadBalancerSourceRanges")},
		{"a group on the tailnet", func() *gcp.DispatchArgs {
			a := dispatchArgs()
			a.NetworkEndpointGroupName = dispatchNEGName
			return a
		}, needs("networkEndpointGroupName")},
		{"ranges in the sandbox", func() *gcp.DispatchArgs {
			a := dispatchSandboxArgs()
			a.LoadBalancerSourceRanges = dispatchLoadBalancerRanges
			return a
		}, needs("loadBalancerSourceRanges")},
		{"a group in the sandbox", func() *gcp.DispatchArgs {
			a := dispatchSandboxArgs()
			a.NetworkEndpointGroupName = dispatchNEGName
			return a
		}, needs("networkEndpointGroupName")},
		{"a range that is no CIDR", func() *gcp.DispatchArgs {
			a := dispatchEntranceArgs()
			a.LoadBalancerSourceRanges = []string{"35.191.0.0"}
			return a
		}, `loadBalancerSourceRanges[0] "35.191.0.0" must be a CIDR, such as 130.211.0.0/22`},
		{"a range written as a host in it", func() *gcp.DispatchArgs {
			a := dispatchEntranceArgs()
			a.LoadBalancerSourceRanges = []string{"35.191.0.0/16", "130.211.1.2/22"}
			return a
		}, `loadBalancerSourceRanges[1] "130.211.1.2/22" must be written as the network it names, 130.211.0.0/22`},
		{"the whole IPv4 space", func() *gcp.DispatchArgs {
			a := dispatchEntranceArgs()
			a.LoadBalancerSourceRanges = []string{"35.191.0.0/16", "0.0.0.0/0"}
			return a
		}, `loadBalancerSourceRanges[1] "0.0.0.0/0" admits every address to 8766, past the load balancer and IAP alike`},
		{"the whole IPv6 space", func() *gcp.DispatchArgs {
			a := dispatchEntranceArgs()
			a.LoadBalancerSourceRanges = []string{"::/0"}
			return a
		}, `loadBalancerSourceRanges[0] "::/0" admits every address to 8766, past the load balancer and IAP alike`},
		{"an issuer with an empty query", func() *gcp.DispatchArgs {
			a := dispatchEntranceArgs()
			a.OidcAudience, a.OidcIssuer = dispatchOidcAudience, dispatchOidcIssuer+"?"
			return a
		}, `oidcIssuer "` + dispatchOidcIssuer + `?" must be an https URL with a host and no user, query or fragment`},
		{"an issuer with an empty fragment", func() *gcp.DispatchArgs {
			a := dispatchEntranceArgs()
			a.OidcAudience, a.OidcIssuer = dispatchOidcAudience, dispatchOidcIssuer+"#"
			return a
		}, `oidcIssuer "` + dispatchOidcIssuer + `#" must be an https URL with a host and no user, query or fragment`},
		{"a group name with an uppercase letter", func() *gcp.DispatchArgs {
			a := dispatchEntranceArgs()
			a.NetworkEndpointGroupName = "Example-neg"
			return a
		}, `networkEndpointGroupName "Example-neg" must match`},
		{"a group name ending in a hyphen", func() *gcp.DispatchArgs {
			a := dispatchEntranceArgs()
			a.NetworkEndpointGroupName = "example-neg-"
			return a
		}, `networkEndpointGroupName "example-neg-" must match`},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.ErrorContains(t, err, row.want)
		})
	}
	withIssuer := dispatchEntranceArgs()
	withIssuer.OidcAudience, withIssuer.OidcIssuer = dispatchOidcAudience, dispatchOidcIssuer
	withIssuer.LoadBalancerSourceRanges = []string{"35.191.0.0/16", "2600:2d00:1:b029::/64", "203.0.113.7/32"}
	for _, args := range []*gcp.DispatchArgs{dispatchArgs(), dispatchSandboxArgs(), dispatchEntranceArgs(), withIssuer} {
		_, err := gcp.ResolveDispatch(args)
		require.NoError(t, err)
	}
}

// With both inputs unset the component renders exactly what it rendered before they existed: the
// registrations and the app's pod match the goldens publicOrigin's own test holds, the app's
// policy has no rule, and its Service no annotation. Control: with both set the registrations
// differ from that golden, so the comparison can see them.
func TestDispatchRendersNoEntranceWithBothUnset(t *testing.T) {
	m, err := runDispatch(t, dispatchPublicOriginArgs(), true)
	require.NoError(t, err)
	require.Equal(t, dispatchSandboxGolden(t), dispatchRegistrations(m))
	requirePodGolden(t, m, "example-dispatch", dispatchPublicOriginGolden)
	require.Empty(t, policyIngressRules(t, m, "example-dispatch", 0))
	require.Empty(t, appServiceAnnotations(t, dispatchPublicOriginArgs()))

	control, err := runDispatch(t, dispatchEntranceArgs(), true)
	require.NoError(t, err)
	policyIngressRules(t, control, "example-dispatch", 1)
	require.NotEmpty(t, appServiceAnnotations(t, dispatchEntranceArgs()))
}
