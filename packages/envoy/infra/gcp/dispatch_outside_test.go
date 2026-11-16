package gcp_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// dispatchSharedHostname is sharedHostname as a consumer sets it, and dispatchSharedOrigin the
	// origin the app is told it is reached at through that node: https, the node's name on the
	// fixture's tailnet, no port and no path.
	dispatchSharedHostname = "dispatch-shared"
	dispatchSharedOrigin   = "https://dispatch-shared.example-tailnet.ts.net"
	// dispatchOutsideGolden is the app's pod with both outside inputs set.
	dispatchOutsideGolden = "dispatch-pod-with-outside-sign-in.golden"
)

// dispatchOutsideArgs is the default fixture, sign-in on, with both outside inputs set.
func dispatchOutsideArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.SharedHostname = dispatchSharedHostname
	args.SignInOutside = true
	return args
}

// appEnvNames is the names of the app's envoy-dispatch container's environment, in the order the
// pod template lists them.
func appEnvNames(t *testing.T, m *pulumitest.Mocks) []string {
	t.Helper()
	var names []string
	for _, item := range podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch")["env"].([]any) {
		names = append(names, item.(map[string]any)["name"].(string))
	}
	return names
}

// With both inputs set the app's container carries the two settings outside sign-in reads, each
// next to the setting it extends: DISPATCH_SIGNIN_OUTSIDE=1 right after DISPATCH_SIGNIN_DOMAIN,
// whose accounts it admits beside the domain's own people, and DISPATCH_SHARED_ORIGIN right after
// DISPATCH_SERVER_URL, the canonical origin it is the second of. Nothing else moves: the listener's
// and the front door's pods, the front door's serve configuration and the registration set are
// the defaults' own, since the shared node is the deployment repository's and not built here.
func TestDispatchPodWithOutsideSignInRendersBothSettings(t *testing.T) {
	m, err := runDispatch(t, dispatchOutsideArgs(), true)
	require.NoError(t, err)

	requirePodGolden(t, m, "example-dispatch", dispatchOutsideGolden)
	env := appEnv(t, m)
	require.Equal(t, "1", env["DISPATCH_SIGNIN_OUTSIDE"])
	require.Equal(t, dispatchSharedOrigin, env["DISPATCH_SHARED_ORIGIN"])
	names := appEnvNames(t, m)
	for _, pair := range []struct{ after, name string }{
		{"DISPATCH_SIGNIN_DOMAIN", "DISPATCH_SIGNIN_OUTSIDE"},
		{"DISPATCH_SERVER_URL", "DISPATCH_SHARED_ORIGIN"},
	} {
		at := slices.Index(names, pair.after)
		require.GreaterOrEqual(t, at, 0, "the app carries %s", pair.after)
		require.Less(t, at+1, len(names), "%s is not the last variable", pair.after)
		require.Equal(t, pair.name, names[at+1], "%s comes right after %s", pair.name, pair.after)
	}

	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
	require.Equal(t, readGolden(t, "dispatch-serve.golden"), serveConfig(t, m))
	require.Equal(t, readGolden(t, "dispatch-registrations.golden"), dispatchRegistrations(m))
}

// The two inputs are independent: each renders its own setting, right after the setting it
// extends, and never the other's, so a deployment can serve the shared origin before it admits
// anyone from outside, a tailnet Dispatch with no browser sign-in included. The origin is the
// name the consumer gave, on its tailnet.
func TestDispatchOutsideInputsEachRenderTheirOwnSetting(t *testing.T) {
	const origin = "https://example-shared.example-tailnet.ts.net"
	for _, row := range []struct {
		name                 string
		args                 func() *gcp.DispatchArgs
		present, after, gone string
		value                string
	}{
		{"signInOutside alone", func() *gcp.DispatchArgs {
			a := dispatchArgs()
			a.SignInOutside = true
			return a
		}, "DISPATCH_SIGNIN_OUTSIDE", "DISPATCH_SIGNIN_DOMAIN", "DISPATCH_SHARED_ORIGIN", "1"},
		{"sharedHostname alone", func() *gcp.DispatchArgs {
			a := dispatchArgs()
			a.SharedHostname = "example-shared"
			return a
		}, "DISPATCH_SHARED_ORIGIN", "DISPATCH_SERVER_URL", "DISPATCH_SIGNIN_OUTSIDE", origin},
		{"sharedHostname with no browser sign-in", func() *gcp.DispatchArgs {
			a := dispatchTailnetWithoutSignInArgs()
			a.SharedHostname = "example-shared"
			return a
		}, "DISPATCH_SHARED_ORIGIN", "DISPATCH_SERVER_URL", "DISPATCH_SIGNIN_OUTSIDE", origin},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			env := appEnv(t, m)
			require.Equal(t, row.value, env[row.present])
			require.NotContains(t, env, row.gone)
			names := appEnvNames(t, m)
			at := slices.Index(names, row.after)
			require.GreaterOrEqual(t, at, 0, "the app carries %s", row.after)
			require.Less(t, at+1, len(names), "%s is not the last variable", row.after)
			require.Equal(t, row.present, names[at+1], "%s comes right after %s", row.present, row.after)
		})
	}
}

// Every DNS label but an internationalized one is a name the shared node may take: one character,
// a leading digit, 63 characters, a "--" inside. The refusals are in
// TestDispatchRefusesOutsideInputsItCannotServe.
func TestDispatchSharedHostnameTakesEveryPlainDNSLabel(t *testing.T) {
	for _, name := range []string{"a", "1shared", strings.Repeat("a", 63), "ab--cd"} {
		t.Run(name, func(t *testing.T) {
			args := dispatchArgs()
			args.SharedHostname = name
			_, err := gcp.ResolveDispatch(args)
			require.NoError(t, err)
		})
	}
}

// With both inputs unset, a tailnet Dispatch with no browser sign-in and a sandbox render neither
// setting, as the goldens of a deployment with sign-in on show for that mode
// (TestDispatchPodWithoutOutsideInputsIsUnchanged).
func TestDispatchOutsideSettingsAbsentInEveryModeWhenUnset(t *testing.T) {
	for name, args := range map[string]func() *gcp.DispatchArgs{
		"tailnet without sign-in": dispatchTailnetWithoutSignInArgs,
		"sandbox":                 dispatchSandboxArgs,
	} {
		t.Run(name, func(t *testing.T) {
			m, err := runDispatch(t, args(), true)
			require.NoError(t, err)
			env := appEnv(t, m)
			require.NotContains(t, env, "DISPATCH_SIGNIN_OUTSIDE")
			require.NotContains(t, env, "DISPATCH_SHARED_ORIGIN")
		})
	}
}

// dispatchArgsWith is the default fixture with set applied to it.
func dispatchArgsWith(set func(*gcp.DispatchArgs)) func() *gcp.DispatchArgs {
	return func() *gcp.DispatchArgs {
		args := dispatchArgs()
		set(args)
		return args
	}
}

// With both inputs unset nothing renders differently: every committed golden of the app's pod
// renders byte for byte as it was committed, none of them regenerated for this change, and the
// table is held to the goldens on disk, so a pod golden added later without its fixture here
// fails rather than escaping the check.
func TestDispatchPodWithoutOutsideInputsIsUnchanged(t *testing.T) {
	// Every committed golden of the app's pod rendered with the outside inputs unset, with the
	// fixture it was committed for.
	goldens := []struct {
		golden string
		args   func() *gcp.DispatchArgs
	}{
		{"dispatch-pod.golden", dispatchArgs},
		{"dispatch-pod-with-broker-and-app.golden", func() *gcp.DispatchArgs { return dispatchBrokerAndAppArgs(t) }},
		{"dispatch-pod-with-azure-key.golden", dispatchArgsWith(setAzureKey)},
		{"dispatch-pod-with-azure-federation.golden", dispatchArgsWith(setAzureFederation)},
		{"dispatch-pod-with-azure-answerer.golden", dispatchArgsWith(setAzureChat)},
		{"dispatch-pod-with-chat-endpoint.golden", dispatchArgsWith(setChatEndpoint)},
	}
	committed, err := filepath.Glob("testdata/dispatch-pod*.golden")
	require.NoError(t, err)
	require.NotEmpty(t, committed, "the glob found the committed pod goldens")
	covered := []string{"testdata/" + dispatchOutsideGolden}
	for _, row := range goldens {
		covered = append(covered, "testdata/"+row.golden)
		t.Run(row.golden, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			requirePodGolden(t, m, "example-dispatch", row.golden)
		})
	}
	require.ElementsMatch(t, committed, covered, "every committed pod golden has its fixture in this test's table")
}

// Each outside input the component cannot serve is refused by its own name before anything
// registers: outside sign-in with no browser sign-in to extend, a shared node in a sandbox, which
// is on no tailnet, a shared node whose name is no DNS label or is the front door's own, and an
// nkey seed, which no input takes. clusterOnly refuses both as it refuses every input that
// configures Dispatch (TestDispatchClusterOnlyRefusesEveryDispatchInput).
func TestDispatchRefusesOutsideInputsItCannotServe(t *testing.T) {
	seed, _ := testnats.User(t)
	lower := strings.ToLower(seed)
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
		want string
	}{
		{
			name: "signInOutside without the sign-in inputs",
			args: func() *gcp.DispatchArgs {
				args := dispatchTailnetWithoutSignInArgs()
				args.SignInOutside = true
				return args
			},
			want: "signInOutside needs browser sign-in: it lets Google accounts outside signInDomain sign in beside the domain's own people " +
				"(the sign-in inputs are set together or not at all; unset: signInClientId, signInClientSecret, signInDomain)",
		},
		{
			name: "signInOutside in a sandbox",
			args: func() *gcp.DispatchArgs {
				args := dispatchSandboxArgs()
				args.SignInOutside = true
				return args
			},
			want: "signInOutside configures browser sign-in, which needs the tailnet inputs: without them Dispatch is reached at an in-cluster http:// address, " +
				"which no OAuth provider takes as a redirect origin, so no browser could sign in; unset signInOutside, or set the tailnet inputs",
		},
		{
			name: "sharedHostname without the tailnet inputs",
			args: func() *gcp.DispatchArgs {
				args := dispatchSandboxArgs()
				args.SharedHostname = dispatchSharedHostname
				return args
			},
			want: "sharedHostname names a node on the tailnet, which needs the tailnet inputs: without them Dispatch is on no tailnet, " +
				"so no node of it can be shared; unset sharedHostname, or set the tailnet inputs",
		},
		{
			name: "sharedHostname in upper case",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = "Dispatch-Shared"
				return args
			},
			want: `sharedHostname "Dispatch-Shared" must be a DNS label: it is the shared node's machine name on the tailnet`,
		},
		{
			name: "sharedHostname holding a dot",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = "dispatch.shared"
				return args
			},
			want: `sharedHostname "dispatch.shared" must be a DNS label: it is the shared node's machine name on the tailnet`,
		},
		{
			name: "sharedHostname longer than a label",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = strings.Repeat("a", 64)
				return args
			},
			want: `sharedHostname "` + strings.Repeat("a", 64) + `" must be a DNS label: it is the shared node's machine name on the tailnet`,
		},
		{
			name: "sharedHostname with a leading hyphen",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = "-shared"
				return args
			},
			want: `sharedHostname "-shared" must be a DNS label: it is the shared node's machine name on the tailnet`,
		},
		{
			name: "sharedHostname with a trailing hyphen",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = "shared-"
				return args
			},
			want: `sharedHostname "shared-" must be a DNS label: it is the shared node's machine name on the tailnet`,
		},
		{
			// A browser refuses a malformed A-label as a host (https://xn--a.<tailnet> is no URL),
			// and the component decodes no punycode, so it refuses every A-label, a well-formed one
			// too: the operator names this node, and plain letters serve.
			name: "sharedHostname a malformed internationalized label",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = "xn--a"
				return args
			},
			want: `sharedHostname "xn--a" is an internationalized (xn--) label, which this component does not check: name the shared node in plain letters, digits and hyphens`,
		},
		{
			name: "sharedHostname a well-formed internationalized label",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = "xn--bcher-kva"
				return args
			},
			want: `sharedHostname "xn--bcher-kva" is an internationalized (xn--) label, which this component does not check: name the shared node in plain letters, digits and hyphens`,
		},
		{
			name: "sharedHostname equal to hostname",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = args.Hostname
				return args
			},
			want: `sharedHostname "example-dispatch" is hostname: the front door's Tailscale Service holds example-dispatch.example-tailnet.ts.net, ` +
				"so the shared node needs a name of its own",
		},
		{
			name: "sharedHostname holding an nkey seed",
			args: func() *gcp.DispatchArgs {
				args := dispatchArgs()
				args.SharedHostname = lower
				return args
			},
			want: "sharedHostname holds an nkey seed (not echoed)",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
		})
	}
	// Both inputs as a consumer sends them, over the default fixture, resolve.
	_, err := gcp.ResolveDispatch(dispatchOutsideArgs())
	require.NoError(t, err)
}

// The two inputs reach the pod over the wire a consumer's SDK sends them on, through the plugin's
// own Construct, in a preview: sharedHostname as a plain string and signInOutside as a plain
// boolean, decoded and rendered as the direct path renders them.
func TestConstructRendersTheOutsideInputsInAPreview(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["sharedHostname"] = property.New(dispatchSharedHostname)
	inputs["signInOutside"] = property.New(true)
	m, _, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
	env := appEnv(t, m)
	require.Equal(t, "1", env["DISPATCH_SIGNIN_OUTSIDE"])
	require.Equal(t, dispatchSharedOrigin, env["DISPATCH_SHARED_ORIGIN"])
}
