package gcp_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// trustedProxyRanges renders DISPATCH_TRUSTED_PROXY_RANGES wherever it is set, in every mode that
// runs the app, beside whatever else is set: the entries comma-joined in the order given, right
// after the last of Dispatch's address settings, DISPATCH_SERVER_URL or the served and former
// lists after it. Unset, no variable renders in any mode. The input changes no other object: the
// declaration and every registration but the app's Deployment carry what the same mode carries
// without it, and that Deployment differs by the one variable alone.
//
// Control: render nothing for the input, and every row that sets it fails on the missing
// variable; render it while it is unset, and every unset row fails; render it into the listener
// or the front door too, and requireOnlyTheAppEnvDiffers fails on that object.
func TestDispatchRendersTheTrustedProxyRangesInEachMode(t *testing.T) {
	ranges := []string{"10.129.0.0/23", "2001:db8:100::/48", "192.0.2.7/32"}
	const joined = "10.129.0.0/23,2001:db8:100::/48,192.0.2.7/32"
	withLists := dispatchArgsWith(func(a *gcp.DispatchArgs) {
		a.ServedOrigins = []string{dispatchServedOrigin}
		a.FormerOrigins = []string{dispatchFormerOrigin}
	})
	for _, row := range []struct {
		name        string
		args        func() *gcp.DispatchArgs
		set         []string
		want, after string
	}{
		{"set beside the front door", dispatchArgs, ranges, joined, "DISPATCH_SERVER_URL"},
		{"set beside the front door with no browser sign-in", dispatchTailnetWithoutSignInArgs, ranges, joined, "DISPATCH_SERVER_URL"},
		{"set beside the front door and both lists", withLists, ranges, joined, "DISPATCH_FORMER_ORIGINS"},
		{"set beside the front door and publicOrigin", dispatchFrontDoorPublicOriginArgs, ranges, joined, "DISPATCH_SERVED_ORIGINS"},
		{"set beside publicOrigin", dispatchPublicOriginArgs, ranges, joined, "DISPATCH_SERVER_URL"},
		{"set behind the IAP door", dispatchIAPArgs, ranges, joined, "DISPATCH_SERVER_URL"},
		{"set in the sandbox", dispatchSandboxArgs, ranges, joined, "DISPATCH_SERVER_URL"},
		{"one range", dispatchArgs, []string{"35.191.0.0/16"}, "35.191.0.0/16", "DISPATCH_SERVER_URL"},
		{"unset beside the front door", dispatchArgs, nil, "", ""},
		{"unset beside the front door with no browser sign-in", dispatchTailnetWithoutSignInArgs, nil, "", ""},
		{"unset beside the front door and both lists", withLists, nil, "", ""},
		{"unset beside the front door and publicOrigin", dispatchFrontDoorPublicOriginArgs, nil, "", ""},
		{"unset beside publicOrigin", dispatchPublicOriginArgs, nil, "", ""},
		{"unset behind the IAP door", dispatchIAPArgs, nil, "", ""},
		{"unset in the sandbox", dispatchSandboxArgs, nil, "", ""},
		{"sent empty in the sandbox", dispatchSandboxArgs, []string{}, "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := row.args()
			args.TrustedProxyRanges = row.set
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			if row.want == "" {
				require.NotContains(t, appEnv(t, m), "DISPATCH_TRUSTED_PROXY_RANGES")
				return
			}
			require.Equal(t, row.want, appEnv(t, m)["DISPATCH_TRUSTED_PROXY_RANGES"])
			requireRightAfter(t, m, row.after, "DISPATCH_TRUSTED_PROXY_RANGES")
			base, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			requireOnlyTheAppEnvDiffers(t, base, m, "DISPATCH_TRUSTED_PROXY_RANGES")
		})
	}
}

// requireOnlyTheAppEnvDiffers holds changed to base's render but for the app's environment: the
// same declaration, every registration but the app's Deployment carrying the same values, and that
// Deployment's spec the same once the variables names lists are taken out of its envoy-dispatch
// container's environment in both.
func requireOnlyTheAppEnvDiffers(t *testing.T, base, changed *pulumitest.Mocks, names ...string) {
	t.Helper()
	require.Equal(t, dispatchRegistrations(base), dispatchRegistrations(changed), "the same objects are declared")
	for _, registration := range base.Registrations() {
		if registration.Type == deploymentType && registration.Name == "example-dispatch" {
			continue
		}
		require.Equal(t, registration.Inputs, changed.Named(t, registration.Type, registration.Name).Inputs,
			"%s %s carries the same values", registration.Type, registration.Name)
	}
	require.Equal(t, appSpecWithout(t, base, names...), appSpecWithout(t, changed, names...), "the app's Deployment differs in %v alone", names)
}

// appSpecWithout is the app's Deployment spec, as deploymentSpec renders it, with the variables
// names lists taken out of its envoy-dispatch container's environment.
func appSpecWithout(t *testing.T, m *pulumitest.Mocks, names ...string) map[string]any {
	t.Helper()
	spec := deploymentSpec(t, m, "example-dispatch")
	found := false
	for _, item := range spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any) {
		container := item.(map[string]any)
		if container["name"] != "envoy-dispatch" {
			continue
		}
		found = true
		kept := []any{}
		for _, entry := range container["env"].([]any) {
			if !slices.Contains(names, entry.(map[string]any)["name"].(string)) {
				kept = append(kept, entry)
			}
		}
		container["env"] = kept
	}
	require.True(t, found, "the app's pod runs envoy-dispatch")
	return spec
}

// trustedProxyRanges is refused by its own name, as a core.InputError naming it, before anything
// registers, in every mode that takes it, on each entry Dispatch refuses to start on
// (parseTrustedProxyRanges in packages/envoy/cmd/dispatch/main.go): one that is not a CIDR, an
// IPv4-mapped IPv6 prefix, which no peer's address is ever inside, one not written as the network
// it names, and a range of length 0, IPv4's or IPv6's, which would trust every peer's
// X-Forwarded-For. The refusal names the entry by its index. A seed in an entry is refused as in
// every plain input, and clusterOnly refuses the input as it refuses every input that configures
// Dispatch (TestDispatchClusterOnlyRefusesEveryDispatchInput).
//
// Control: drop any one of the four checks, and its rows register Dispatch and fail.
func TestDispatchRefusesTrustedProxyRangesItCannotUse(t *testing.T) {
	seed, _ := testnats.User(t)
	with := func(base func() *gcp.DispatchArgs, ranges ...string) func() *gcp.DispatchArgs {
		return editedDispatchArgs(base, func(a *gcp.DispatchArgs) { a.TrustedProxyRanges = ranges })
	}
	const everyPeer = " trusts every peer's X-Forwarded-For, so any caller would pick the address it is logged and rate limited by: " +
		"name the proxies' own ranges, such as 130.211.0.0/22"
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
		want string
	}{
		{"not a CIDR", with(dispatchArgs, "not-a-cidr"),
			`trustedProxyRanges[0] "not-a-cidr" must be a CIDR, such as 130.211.0.0/22`},
		{"not a CIDR after a good one, beside publicOrigin", with(dispatchPublicOriginArgs, "10.129.0.0/23", "not-a-cidr"),
			`trustedProxyRanges[1] "not-a-cidr" must be a CIDR, such as 130.211.0.0/22`},
		{"an address with no prefix length, in the sandbox", with(dispatchSandboxArgs, "192.0.2.1"),
			`trustedProxyRanges[0] "192.0.2.1" must be a CIDR, such as 130.211.0.0/22`},
		{"an empty entry", with(dispatchArgs, ""),
			`trustedProxyRanges[0] "" must be a CIDR, such as 130.211.0.0/22`},
		{"an IPv4-mapped IPv6 prefix", with(dispatchArgs, "::ffff:10.129.0.0/119"),
			`trustedProxyRanges[0] "::ffff:10.129.0.0/119" is an IPv4-mapped IPv6 prefix, which no peer's address is ever inside, since Dispatch reads each peer's address as the IPv4 address it holds: write it as an IPv4 prefix, such as 130.211.0.0/22`},
		{"not written as its network", with(dispatchArgs, "10.0.0.1/8"),
			`trustedProxyRanges[0] "10.0.0.1/8" must be written as the network it names, 10.0.0.0/8`},
		{"an IPv6 range not written as its network", with(dispatchFrontDoorPublicOriginArgs, "2001:db8:100::1/48"),
			`trustedProxyRanges[0] "2001:db8:100::1/48" must be written as the network it names, 2001:db8:100::/48`},
		{"every IPv4 address", with(dispatchArgs, "0.0.0.0/0"), `trustedProxyRanges[0] "0.0.0.0/0"` + everyPeer},
		{"every IPv4 address, in the sandbox", with(dispatchSandboxArgs, "0.0.0.0/0"), `trustedProxyRanges[0] "0.0.0.0/0"` + everyPeer},
		{"every IPv6 address after a good one", with(dispatchArgs, "10.129.0.0/23", "::/0"), `trustedProxyRanges[1] "::/0"` + everyPeer},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			requireInputError(t, err, "trustedProxyRanges")
			m, err := runDispatch(t, row.args(), true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
			require.NotContains(t, m.Types(), deploymentType, "nothing of Dispatch's registers")
		})
	}
	t.Run("a seed in an entry", func(t *testing.T) {
		_, err := gcp.ResolveDispatch(with(dispatchArgs, "192.0.2.0/24", strings.ToLower(seed))())
		// The message names the entry by its path; the core.InputError names the list.
		require.EqualError(t, err, "trustedProxyRanges[1] holds an nkey seed (not echoed)")
		requireInputError(t, err, "trustedProxyRanges")
	})
}
