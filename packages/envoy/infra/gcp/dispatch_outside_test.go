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
	for after, name := range map[string]string{"DISPATCH_SIGNIN_DOMAIN": "DISPATCH_SIGNIN_OUTSIDE", "DISPATCH_SERVER_URL": "DISPATCH_SHARED_ORIGIN"} {
		at := slices.Index(names, after)
		require.GreaterOrEqual(t, at, 0, "the app carries %s", after)
		require.Less(t, at+1, len(names), "%s is not the last variable", after)
		require.Equal(t, name, names[at+1], "%s comes right after %s", name, after)
	}

	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
	require.Equal(t, readGolden(t, "dispatch-serve.golden"), serveConfig(t, m))
	require.Equal(t, readGolden(t, "dispatch-registrations.golden"), dispatchRegistrations(m))
}

// The two inputs are independent: each renders its own setting and never the other's, so a
// deployment can serve the shared origin to its own people before it admits anyone from outside.
func TestDispatchOutsideInputsEachRenderTheirOwnSetting(t *testing.T) {
	for _, row := range []struct {
		name          string
		set           func(*gcp.DispatchArgs)
		present, gone string
		value         string
	}{
		{"signInOutside alone", func(a *gcp.DispatchArgs) { a.SignInOutside = true }, "DISPATCH_SIGNIN_OUTSIDE", "DISPATCH_SHARED_ORIGIN", "1"},
		{"sharedHostname alone", func(a *gcp.DispatchArgs) { a.SharedHostname = dispatchSharedHostname }, "DISPATCH_SHARED_ORIGIN", "DISPATCH_SIGNIN_OUTSIDE", dispatchSharedOrigin},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchArgs()
			row.set(args)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			env := appEnv(t, m)
			require.Equal(t, row.value, env[row.present])
			require.NotContains(t, env, row.gone)
		})
	}
}

// dispatchPodGoldens is every committed golden of the app's pod rendered with the outside inputs
// unset, with the fixture it was committed for.
var dispatchPodGoldens = []struct {
	golden string
	args   func(*testing.T) *gcp.DispatchArgs
}{
	{"dispatch-pod.golden", func(*testing.T) *gcp.DispatchArgs { return dispatchArgs() }},
	{"dispatch-pod-with-broker-and-app.golden", dispatchBrokerAndAppArgs},
	{"dispatch-pod-with-azure-key.golden", dispatchArgsWith(setAzureKey)},
	{"dispatch-pod-with-azure-federation.golden", dispatchArgsWith(setAzureFederation)},
	{"dispatch-pod-with-azure-answerer.golden", dispatchArgsWith(setAzureChat)},
	{"dispatch-pod-with-chat-endpoint.golden", dispatchArgsWith(setChatEndpoint)},
}

// dispatchArgsWith is the default fixture with set applied to it.
func dispatchArgsWith(set func(*gcp.DispatchArgs)) func(*testing.T) *gcp.DispatchArgs {
	return func(*testing.T) *gcp.DispatchArgs {
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
	committed, err := filepath.Glob("testdata/dispatch-pod*.golden")
	require.NoError(t, err)
	require.NotEmpty(t, committed, "the glob found the committed pod goldens")
	covered := []string{"testdata/" + dispatchOutsideGolden}
	for _, row := range dispatchPodGoldens {
		covered = append(covered, "testdata/"+row.golden)
		t.Run(row.golden, func(t *testing.T) {
			m, err := runDispatch(t, row.args(t), true)
			require.NoError(t, err)
			requirePodGolden(t, m, "example-dispatch", row.golden)
		})
	}
	require.ElementsMatch(t, committed, covered, "every committed pod golden has its fixture in dispatchPodGoldens")
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
				"which no OAuth provider takes as a redirect origin, so no browser could sign in; unset the sign-in inputs, or set the tailnet inputs",
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
