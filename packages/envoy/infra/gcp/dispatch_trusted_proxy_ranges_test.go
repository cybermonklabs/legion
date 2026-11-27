package gcp_test

import (
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/testnats"
)

// dispatchGoogleProxyRanges is what DISPATCH_TRUSTED_PROXY_RANGES renders with signInPasskey and no
// trustedProxyRanges: Google's external Application Load Balancer's proxy ranges, IPv4 then IPv6.
const dispatchGoogleProxyRanges = "35.191.0.0/16,130.211.0.0/22,2600:2d00:1:1::/64"

// With signInPasskey, DISPATCH_TRUSTED_PROXY_RANGES renders right after DISPATCH_SIGNIN_PASSKEY:
// Google's three ranges when trustedProxyRanges is unset, beside the front door and beside
// publicOrigin alike, and the entries trustedProxyRanges names, comma-joined in the order given,
// when it is set. Dispatch refuses to start passkey sign-in without the setting.
//
// Control: render DISPATCH_SIGNIN_PASSKEY alone, and every row fails naming the missing variable.
func TestDispatchRendersTheTrustedProxyRangesWithPasskeySignIn(t *testing.T) {
	publicOriginWithPasskey := func() *gcp.DispatchArgs {
		args := dispatchPublicOriginArgs()
		args.OutsideOrigin = dispatchOutsideOrigin
		args.SignInPasskey = true
		return args
	}
	named := func() *gcp.DispatchArgs {
		args := dispatchPasskeyArgs()
		args.TrustedProxyRanges = []string{"192.0.2.0/24", "2001:db8:100::/48", "35.191.0.0/16"}
		return args
	}
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
		want string
	}{
		{"unset, beside the front door", dispatchPasskeyArgs, dispatchGoogleProxyRanges},
		{"unset, beside publicOrigin", publicOriginWithPasskey, dispatchGoogleProxyRanges},
		{"set", named, "192.0.2.0/24,2001:db8:100::/48,35.191.0.0/16"},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			require.Equal(t, row.want, appEnv(t, m)["DISPATCH_TRUSTED_PROXY_RANGES"])
			requireRightAfter(t, m, "DISPATCH_SIGNIN_PASSKEY", "DISPATCH_TRUSTED_PROXY_RANGES")
		})
	}
}

// Without signInPasskey, DISPATCH_TRUSTED_PROXY_RANGES renders in no mode, so a deployment that sets
// none of the outside inputs renders what it rendered before (TestDispatchPodWithoutOutsideInputsIsUnchanged
// holds every committed golden to that).
func TestDispatchTrustedProxyRangesAbsentWithoutPasskeySignIn(t *testing.T) {
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
	}{
		{"the default", dispatchArgs},
		{"outsideOrigin with sign-in, no passkey", dispatchOutsideArgs},
		{"publicOrigin", dispatchPublicOriginArgs},
		{"the IAP door", dispatchIAPArgs},
		{"sandbox", dispatchSandboxArgs},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			require.NotContains(t, appEnv(t, m), "DISPATCH_TRUSTED_PROXY_RANGES")
		})
	}
}

// trustedProxyRanges is refused by its own name, as a core.InputError naming it, before anything
// registers: without signInPasskey, since Dispatch reads it only for the passkey begins' bound; with
// an entry that is not a CIDR, one not written as the network it names, and a /0, which would trust
// every peer's X-Forwarded-For. A seed in an entry is refused as in every plain input. clusterOnly
// refuses it as it refuses every input that configures Dispatch
// (TestDispatchClusterOnlyRefusesEveryDispatchInput).
//
// Control: drop the check that trustedProxyRanges needs signInPasskey, and the first row registers
// Dispatch and fails.
func TestDispatchRefusesTrustedProxyRangesItCannotUse(t *testing.T) {
	seed, _ := testnats.User(t)
	with := func(base func() *gcp.DispatchArgs, ranges ...string) func() *gcp.DispatchArgs {
		return func() *gcp.DispatchArgs {
			args := base()
			args.TrustedProxyRanges = ranges
			return args
		}
	}
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
		want string
	}{
		{"without signInPasskey", with(dispatchOutsideArgs, "192.0.2.0/24"),
			"trustedProxyRanges needs signInPasskey: Dispatch reads it only to bound the passkey ceremonies' begins per client address; set signInPasskey, or unset trustedProxyRanges"},
		{"not a CIDR", with(dispatchPasskeyArgs, "35.191.0.0/16", "not-a-cidr"),
			`trustedProxyRanges[1] "not-a-cidr" must be a CIDR, such as 130.211.0.0/22`},
		{"not written as its network", with(dispatchPasskeyArgs, "192.0.2.10/24"),
			`trustedProxyRanges[0] "192.0.2.10/24" must be written as the network it names, 192.0.2.0/24`},
		{"every IPv4 address", with(dispatchPasskeyArgs, "0.0.0.0/0"),
			`trustedProxyRanges[0] "0.0.0.0/0" trusts every peer's X-Forwarded-For, so any caller would pick the address its begins are bounded by: name the front doors' own ranges, such as 130.211.0.0/22`},
		{"every IPv6 address", with(dispatchPasskeyArgs, "35.191.0.0/16", "::/0"),
			`trustedProxyRanges[1] "::/0" trusts every peer's X-Forwarded-For, so any caller would pick the address its begins are bounded by: name the front doors' own ranges, such as 130.211.0.0/22`},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "trustedProxyRanges", refused.Input)
			m, err := runDispatch(t, row.args(), true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
			require.NotContains(t, m.Types(), "kubernetes:apps/v1:Deployment", "nothing of Dispatch's registers")
		})
	}
	t.Run("a seed in an entry", func(t *testing.T) {
		_, err := gcp.ResolveDispatch(with(dispatchPasskeyArgs, "192.0.2.0/24", strings.ToLower(seed))())
		// The message names the entry by its path; the core.InputError names the list.
		require.EqualError(t, err, "trustedProxyRanges[1] holds an nkey seed (not echoed)")
		requireInputError(t, err, "trustedProxyRanges")
	})
}

// trustedProxyRanges reaches the pod over the wire a consumer's SDK sends it on, through the
// plugin's own Construct, in a preview: a plain list of strings beside signInPasskey's boolean.
func TestConstructRendersTrustedProxyRangesInAPreview(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["outsideOrigin"] = property.New(dispatchOutsideOrigin)
	inputs["signInPasskey"] = property.New(true)
	inputs["trustedProxyRanges"] = property.New(property.NewArray([]property.Value{property.New("192.0.2.0/24"), property.New("2001:db8:100::/48")}))
	m, _, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
	require.Equal(t, "192.0.2.0/24,2001:db8:100::/48", appEnv(t, m)["DISPATCH_TRUSTED_PROXY_RANGES"])
	requireRightAfter(t, m, "DISPATCH_SIGNIN_PASSKEY", "DISPATCH_TRUSTED_PROXY_RANGES")
}
