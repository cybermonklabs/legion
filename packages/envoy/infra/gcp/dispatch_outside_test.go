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
)

const (
	// dispatchOutsideOrigin is outsideOrigin as a consumer sets it: a host of its own, on no
	// tailnet in particular, since the component names no way in.
	dispatchOutsideOrigin = "https://dispatch-outside.example.com"
	// dispatchOutsideGolden is the app's pod with outsideOrigin set.
	dispatchOutsideGolden = "dispatch-pod-with-outside-origin.golden"
)

// dispatchOutsideArgs is the default fixture, sign-in on, with outsideOrigin set.
func dispatchOutsideArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.OutsideOrigin = dispatchOutsideOrigin
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

// requireRightAfter holds name to the variable right after after in the app's environment.
func requireRightAfter(t *testing.T, m *pulumitest.Mocks, after, name string) {
	t.Helper()
	names := appEnvNames(t, m)
	at := slices.Index(names, after)
	require.GreaterOrEqual(t, at, 0, "the app carries %s", after)
	require.Less(t, at+1, len(names), "%s is not the last variable", after)
	require.Equal(t, name, names[at+1], "%s comes right after %s", name, after)
}

// With outsideOrigin set the app's container carries DISPATCH_OUTSIDE_ORIGIN right after
// DISPATCH_SERVER_URL, the canonical origin it is the second of. Nothing else moves: the listener's
// and the front door's pods, the front door's serve configuration and the registration set are
// the defaults' own, since what serves the second origin is the deployment's and not built here.
func TestDispatchPodWithOutsideOriginRendersItsSetting(t *testing.T) {
	m, err := runDispatch(t, dispatchOutsideArgs(), true)
	require.NoError(t, err)

	requirePodGolden(t, m, "example-dispatch", dispatchOutsideGolden)
	requireRightAfter(t, m, "DISPATCH_SERVER_URL", "DISPATCH_OUTSIDE_ORIGIN")

	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
	require.Equal(t, readGolden(t, "dispatch-serve.golden"), serveConfig(t, m))
	require.Equal(t, readGolden(t, "dispatch-registrations.golden"), dispatchRegistrations(m))
}

// outsideOrigin renders DISPATCH_OUTSIDE_ORIGIN right after DISPATCH_SERVER_URL, with browser
// sign-in and without it, a tailnet Dispatch with no browser sign-in included. The origin is the
// one the consumer gave, its port kept and a trailing slash trimmed, as Dispatch trims one.
func TestDispatchOutsideOriginRendersItsOwnSetting(t *testing.T) {
	for _, row := range []struct {
		name  string
		args  func() *gcp.DispatchArgs
		value string
	}{
		{"outsideOrigin", func() *gcp.DispatchArgs {
			a := dispatchArgs()
			a.OutsideOrigin = "https://example-outside.example.net"
			return a
		}, "https://example-outside.example.net"},
		{"outsideOrigin with a port and a trailing slash", func() *gcp.DispatchArgs {
			a := dispatchArgs()
			a.OutsideOrigin = "https://example-outside.example.net:8443/"
			return a
		}, "https://example-outside.example.net:8443"},
		{"outsideOrigin with no browser sign-in", func() *gcp.DispatchArgs {
			a := dispatchTailnetWithoutSignInArgs()
			a.OutsideOrigin = "https://example-outside.example.net"
			return a
		}, "https://example-outside.example.net"},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			require.Equal(t, row.value, appEnv(t, m)["DISPATCH_OUTSIDE_ORIGIN"])
			requireRightAfter(t, m, "DISPATCH_SERVER_URL", "DISPATCH_OUTSIDE_ORIGIN")
		})
	}
}

// These hosts, each with lowercase DNS labels whose "--" is not at their third and fourth
// characters and a top-level domain on the public suffix list, are ones the second origin may
// have with browser sign-in on: one character, a leading digit, 63 characters, a "--" further in,
// and a name on the front door's own tailnet other than the front door's. The refusals are in
// TestDispatchRefusesOutsideInputsItCannotServe.
func TestDispatchOutsideOriginTakesEveryPlainHost(t *testing.T) {
	for _, origin := range []string{
		"https://a.example.com",
		"https://1outside.example.com",
		"https://" + strings.Repeat("a", 63) + ".example.com",
		"https://abc--d.example.com",
		"https://xn--bcher-kva.example.com",
		"https://example-outside.example-tailnet.ts.net",
		"https://dispatch-outside.example.com:8443",
	} {
		t.Run(origin, func(t *testing.T) {
			args := dispatchArgs()
			args.OutsideOrigin = origin
			_, err := gcp.ResolveDispatch(args)
			require.NoError(t, err)
		})
	}
}

// Without browser sign-in no Google client lists the second origin's callback, so the component
// takes a host Google would refuse there, an IP address or a single label, as weborigin.Parse does.
func TestDispatchOutsideOriginTakesAnyHostWithoutBrowserSignIn(t *testing.T) {
	for _, origin := range []string{"https://10.0.0.1", "https://dispatch", "https://dispatch-outside.example"} {
		t.Run(origin, func(t *testing.T) {
			args := dispatchTailnetWithoutSignInArgs()
			args.OutsideOrigin = origin
			_, err := gcp.ResolveDispatch(args)
			require.NoError(t, err)
		})
	}
}

// With outsideOrigin unset, a tailnet Dispatch with no browser sign-in and a sandbox render no
// second origin, as the goldens of a deployment with sign-in on show for that mode
// (TestDispatchPodWithoutOutsideInputsIsUnchanged).
func TestDispatchOutsideSettingsAbsentInEveryModeWhenUnset(t *testing.T) {
	for name, args := range map[string]func() *gcp.DispatchArgs{
		"tailnet without sign-in": dispatchTailnetWithoutSignInArgs,
		"sandbox":                 dispatchSandboxArgs,
	} {
		t.Run(name, func(t *testing.T) {
			m, err := runDispatch(t, args(), true)
			require.NoError(t, err)
			require.NotContains(t, appEnv(t, m), "DISPATCH_OUTSIDE_ORIGIN")
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

// With outsideOrigin unset nothing renders differently: every committed golden of the app's pod
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
		{dispatchPublicOriginGolden, dispatchPublicOriginArgs},
		{dispatchIAPGolden, dispatchIAPArgs},
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

// outsideOrigin is refused by its own name before anything registers where the component cannot
// serve it: in a sandbox, and as a second origin weborigin.Parse refuses, that is no https origin,
// that is the front door's own, or whose host Google refuses in a redirect URI while browser
// sign-in is on. An nkey seed in outsideOrigin is refused with every other plain input's
// (TestDispatchRefusesAnNKeySeedInAPlainInput), and clusterOnly refuses it as it refuses every
// input that configures Dispatch (TestDispatchClusterOnlyRefusesEveryDispatchInput).
func TestDispatchRefusesOutsideInputsItCannotServe(t *testing.T) {
	// weborigin.Parse's own test holds its whole table; the rows here that expect notOrigin hold
	// that the component asks it and prefixes its refusal with the input's name.
	const notOrigin = "outsideOrigin: must be an absolute http or https URL with no path, user, query or fragment, as an origin is (not echoed)"
	withOrigin := func(origin string) func() *gcp.DispatchArgs {
		return func() *gcp.DispatchArgs {
			args := dispatchArgs()
			args.OutsideOrigin = origin
			return args
		}
	}
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
		want string
	}{
		{
			name: "outsideOrigin in a sandbox",
			args: func() *gcp.DispatchArgs {
				args := dispatchSandboxArgs()
				args.OutsideOrigin = dispatchOutsideOrigin
				return args
			},
			want: "outsideOrigin needs the tailnet inputs or publicOrigin: without either the component builds the sandbox, whose own origin is an in-cluster http:// address " +
				"where browser sign-in is refused, and outside sign-in is what a second origin is for; unset outsideOrigin, or set the tailnet inputs or publicOrigin",
		},
		{"outsideOrigin over http", withOrigin("http://dispatch-outside.example.com"), "outsideOrigin must be an https origin: browser sign-in at it needs TLS, as the front door's does"},
		{"outsideOrigin with no scheme", withOrigin("dispatch-outside.example.com"), notOrigin},
		{"outsideOrigin with a path", withOrigin("https://dispatch-outside.example.com/dispatch"), notOrigin},
		{"outsideOrigin with a query", withOrigin("https://dispatch-outside.example.com?a=b"), notOrigin},
		{"outsideOrigin with a fragment", withOrigin("https://dispatch-outside.example.com#top"), notOrigin},
		{"outsideOrigin with a user", withOrigin("https://example-user@dispatch-outside.example.com"), notOrigin},
		{
			name: "outsideOrigin with the front door's host",
			args: withOrigin("https://example-dispatch.example-tailnet.ts.net"),
			want: "outsideOrigin's host example-dispatch.example-tailnet.ts.net is the front door's: a second origin needs a host of its own",
		},
		{
			name: "outsideOrigin with the front door's host on another port",
			args: withOrigin("https://example-dispatch.example-tailnet.ts.net:8443"),
			want: "outsideOrigin's host example-dispatch.example-tailnet.ts.net is the front door's: a second origin needs a host of its own",
		},
		// With the sign-in inputs set the Google client lists <outsideOrigin>/auth/callback, so a
		// host Google refuses in a redirect URI is refused: a raw IP address, and a host with no
		// top-level domain on the public suffix list (TestDispatchOutsideOriginTakesAnyHostWithoutBrowserSignIn
		// holds them taken without sign-in).
		{
			name: "outsideOrigin an IP address with browser sign-in",
			args: withOrigin("https://10.0.0.1"),
			want: "outsideOrigin's host 10.0.0.1 is an IP address, which Google refuses as the host of an OAuth redirect URI, so browser sign-in at it could not complete: name the origin by a DNS name",
		},
		{
			name: "outsideOrigin a single label with browser sign-in",
			args: withOrigin("https://dispatch"),
			want: "outsideOrigin's host dispatch is a single label, with no top-level domain on the public suffix list, which Google requires of an OAuth redirect URI's host, so browser sign-in at it could not complete",
		},
		{
			name: "outsideOrigin under a top-level domain off the public suffix list with browser sign-in",
			args: withOrigin("https://dispatch-outside.example"),
			want: "outsideOrigin's host dispatch-outside.example ends in example, no top-level domain on the public suffix list, which Google requires of an OAuth redirect URI's host, so browser sign-in at it could not complete",
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			require.NotContains(t, err.Error(), "example-user", "the refusal echoes no user, which could carry a credential")
		})
	}
}

// outsideOrigin reaches the pod over the wire a consumer's SDK sends it on, through the plugin's
// own Construct, in a preview, as a plain string decoded and rendered as the direct path renders it.
func TestConstructRendersTheOutsideOriginInAPreview(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["outsideOrigin"] = property.New(dispatchOutsideOrigin)
	m, _, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
	require.Equal(t, dispatchOutsideOrigin, appEnv(t, m)["DISPATCH_OUTSIDE_ORIGIN"])
}
