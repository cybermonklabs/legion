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
	// dispatchOutsideGolden is the app's pod with both outside inputs set.
	dispatchOutsideGolden = "dispatch-pod-with-outside-sign-in.golden"
)

// dispatchOutsideArgs is the default fixture, sign-in on, with both outside inputs set.
func dispatchOutsideArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.OutsideOrigin = dispatchOutsideOrigin
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

// requireRightAfter holds name to the variable right after after in the app's environment.
func requireRightAfter(t *testing.T, m *pulumitest.Mocks, after, name string) {
	t.Helper()
	names := appEnvNames(t, m)
	at := slices.Index(names, after)
	require.GreaterOrEqual(t, at, 0, "the app carries %s", after)
	require.Less(t, at+1, len(names), "%s is not the last variable", after)
	require.Equal(t, name, names[at+1], "%s comes right after %s", name, after)
}

// With both inputs set the app's container carries the two settings outside sign-in reads, each
// next to the setting it extends: DISPATCH_SIGNIN_OUTSIDE=1 right after DISPATCH_SIGNIN_DOMAIN,
// whose accounts it admits beside the domain's own people, and DISPATCH_OUTSIDE_ORIGIN right after
// DISPATCH_SERVER_URL, the canonical origin it is the second of. Nothing else moves: the listener's
// and the front door's pods, the front door's serve configuration and the registration set are
// the defaults' own, since what serves the second origin is the deployment's and not built here.
func TestDispatchPodWithOutsideSignInRendersBothSettings(t *testing.T) {
	m, err := runDispatch(t, dispatchOutsideArgs(), true)
	require.NoError(t, err)

	requirePodGolden(t, m, "example-dispatch", dispatchOutsideGolden)
	requireRightAfter(t, m, "DISPATCH_SIGNIN_DOMAIN", "DISPATCH_SIGNIN_OUTSIDE")
	requireRightAfter(t, m, "DISPATCH_SERVER_URL", "DISPATCH_OUTSIDE_ORIGIN")

	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
	require.Equal(t, readGolden(t, "dispatch-serve.golden"), serveConfig(t, m))
	require.Equal(t, readGolden(t, "dispatch-registrations.golden"), dispatchRegistrations(m))
}

// The two inputs are independent: each renders its own setting, right after the setting it
// extends, and never the other's, so a deployment can serve the second origin before it admits
// anyone from outside, a tailnet Dispatch with no browser sign-in included. The origin is the one
// the consumer gave, its port kept and a trailing slash trimmed, as Dispatch trims one.
func TestDispatchOutsideInputsEachRenderTheirOwnSetting(t *testing.T) {
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
		}, "DISPATCH_SIGNIN_OUTSIDE", "DISPATCH_SIGNIN_DOMAIN", "DISPATCH_OUTSIDE_ORIGIN", "1"},
		{"outsideOrigin alone", func() *gcp.DispatchArgs {
			a := dispatchArgs()
			a.OutsideOrigin = "https://example-outside.example.net"
			return a
		}, "DISPATCH_OUTSIDE_ORIGIN", "DISPATCH_SERVER_URL", "DISPATCH_SIGNIN_OUTSIDE", "https://example-outside.example.net"},
		{"outsideOrigin with a port and a trailing slash", func() *gcp.DispatchArgs {
			a := dispatchArgs()
			a.OutsideOrigin = "https://example-outside.example.net:8443/"
			return a
		}, "DISPATCH_OUTSIDE_ORIGIN", "DISPATCH_SERVER_URL", "DISPATCH_SIGNIN_OUTSIDE", "https://example-outside.example.net:8443"},
		{"outsideOrigin with no browser sign-in", func() *gcp.DispatchArgs {
			a := dispatchTailnetWithoutSignInArgs()
			a.OutsideOrigin = "https://example-outside.example.net"
			return a
		}, "DISPATCH_OUTSIDE_ORIGIN", "DISPATCH_SERVER_URL", "DISPATCH_SIGNIN_OUTSIDE", "https://example-outside.example.net"},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			env := appEnv(t, m)
			require.Equal(t, row.value, env[row.present])
			require.NotContains(t, env, row.gone)
			requireRightAfter(t, m, row.after, row.present)
		})
	}
}

// Every host whose labels are lowercase DNS labels without "--" as their third and fourth
// characters is one the second origin may have: one character, a leading digit, 63 characters, a
// "--" further in, and a name on the front door's own tailnet other than the front door's. The
// refusals are in TestDispatchRefusesOutsideInputsItCannotServe.
func TestDispatchOutsideOriginTakesEveryPlainHost(t *testing.T) {
	for _, origin := range []string{
		"https://a.example.com",
		"https://1outside.example.com",
		"https://" + strings.Repeat("a", 63) + ".example.com",
		"https://abc--d.example.com",
		"https://xn--bcher-kva.example.com",
		"https://example-outside.example-tailnet.ts.net",
		"https://10.0.0.1",
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
			require.NotContains(t, env, "DISPATCH_OUTSIDE_ORIGIN")
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
// registers: outside sign-in with no browser sign-in to extend, either input in a sandbox, and a
// second origin that is no https origin, whose host has a label no public certificate authority
// issues for, or that is the front door's own. An nkey seed in outsideOrigin is refused with every
// other plain input's (TestDispatchRefusesAnNKeySeedInAPlainInput), and clusterOnly refuses both
// as it refuses every input that configures Dispatch
// (TestDispatchClusterOnlyRefusesEveryDispatchInput).
func TestDispatchRefusesOutsideInputsItCannotServe(t *testing.T) {
	// The web-origin refusals are weborigin.Parse's, prefixed with the input's name; that
	// package's own test holds its whole table, and these rows hold that the component asks it.
	const notOrigin = "outsideOrigin: must be an absolute http or https URL with no path, user, query or fragment, as an origin is (not echoed)"
	reserved := func(label string) string {
		return `outsideOrigin: host label "` + label + `" has "--" as its third and fourth characters, a reserved form (RFC 5890 section 2.3.1) public certificate authorities refuse to issue for`
	}
	fakeALabel := func(label string) string {
		return `outsideOrigin: host label "` + label + `" is an xn-- label the IDNA lookup profile refuses, a Fake A-label (RFC 5890 section 2.3.1)`
	}
	notCanonical := func(canonical string) string {
		return "outsideOrigin: must be written as a browser writes this origin, " + canonical + ": a lowercase scheme and no default port"
	}
	badPort := func(port string) string {
		return `outsideOrigin: port "` + port + `" is not a port 1-65535 written without a leading zero`
	}
	notIPv4 := func(host string) string {
		return "outsideOrigin: host " + host + " ends in a number and is no IPv4 address in its canonical form, which a browser refuses or rewrites"
	}
	notLabel := func(label string) string {
		return `outsideOrigin: host label "` + label + `" is not a lowercase DNS label`
	}
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
			name: "outsideOrigin in a sandbox",
			args: func() *gcp.DispatchArgs {
				args := dispatchSandboxArgs()
				args.OutsideOrigin = dispatchOutsideOrigin
				return args
			},
			want: "outsideOrigin needs the tailnet inputs: without them the component builds the sandbox, whose own origin is an in-cluster http:// address " +
				"where browser sign-in is refused, and outside sign-in is what a second origin is for; unset outsideOrigin, or set the tailnet inputs",
		},
		{"outsideOrigin over http", withOrigin("http://dispatch-outside.example.com"), "outsideOrigin must be an https origin: browser sign-in at it needs TLS, as the front door's does"},
		{"outsideOrigin with no scheme", withOrigin("dispatch-outside.example.com"), notOrigin},
		{"outsideOrigin with a path", withOrigin("https://dispatch-outside.example.com/dispatch"), notOrigin},
		{"outsideOrigin with a query", withOrigin("https://dispatch-outside.example.com?a=b"), notOrigin},
		{"outsideOrigin with a fragment", withOrigin("https://dispatch-outside.example.com#top"), notOrigin},
		{"outsideOrigin with a user", withOrigin("https://example-user@dispatch-outside.example.com"), notOrigin},
		{"outsideOrigin with an upper-case label", withOrigin("https://Dispatch-Outside.example.com"), notLabel("Dispatch-Outside")},
		{"outsideOrigin with an empty label", withOrigin("https://dispatch-outside..example.com"), notLabel("")},
		{"outsideOrigin with a label longer than 63", withOrigin("https://" + strings.Repeat("a", 64) + ".example.com"), notLabel(strings.Repeat("a", 64))},
		{"outsideOrigin with a leading hyphen", withOrigin("https://-outside.example.com"), notLabel("-outside")},
		{"outsideOrigin with a trailing hyphen", withOrigin("https://outside-.example.com"), notLabel("outside-")},
		{"outsideOrigin with a reserved label", withOrigin("https://ab--cd.example.com"), reserved("ab--cd")},
		{"outsideOrigin with a reserved label further up", withOrigin("https://outside.ab--cd.com"), reserved("ab--cd")},
		{"outsideOrigin with an xn-- label the IDNA lookup profile refuses", withOrigin("https://xn--a.example.com"), fakeALabel("xn--a")},
		// Dispatch compares the Origin a browser sends with the rendered string, so a string other
		// than the browser's own form of the origin, or one no browser opens, is refused rather than
		// rewritten (WHATWG URL's origin serialization, as Node's new URL(v).origin prints it).
		{"outsideOrigin with :443", withOrigin("https://dispatch-outside.example.com:443"), notCanonical("https://dispatch-outside.example.com")},
		{"outsideOrigin with an upper-case scheme", withOrigin("HTTPS://dispatch-outside.example.com"), notCanonical("https://dispatch-outside.example.com")},
		{"outsideOrigin with an empty port", withOrigin("https://dispatch-outside.example.com:"), badPort("")},
		{"outsideOrigin with a leading-zero port", withOrigin("https://dispatch-outside.example.com:0443"), badPort("0443")},
		{"outsideOrigin with port 0", withOrigin("https://dispatch-outside.example.com:0"), badPort("0")},
		{"outsideOrigin with a port above 65535", withOrigin("https://dispatch-outside.example.com:65536"), badPort("65536")},
		{"outsideOrigin with a five-nines port", withOrigin("https://dispatch-outside.example.com:99999"), badPort("99999")},
		{"outsideOrigin with a short IPv4", withOrigin("https://127.1"), notIPv4("127.1")},
		{"outsideOrigin with a hex IPv4 octet", withOrigin("https://0x7f.0.0.1"), notIPv4("0x7f.0.0.1")},
		{"outsideOrigin with a whole-number IPv4", withOrigin("https://2130706433"), notIPv4("2130706433")},
		{"outsideOrigin with an octal IPv4 octet", withOrigin("https://010.0.0.1"), notIPv4("010.0.0.1")},
		{"outsideOrigin with five numeric labels", withOrigin("https://1.2.3.4.5"), notIPv4("1.2.3.4.5")},
		{"outsideOrigin with a numeric top label", withOrigin("https://example.123"), notIPv4("example.123")},
		{"outsideOrigin with a hex top label", withOrigin("https://a.example.0x1"), notIPv4("a.example.0x1")},
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
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			require.NotContains(t, err.Error(), "example-user", "the refusal echoes no user, which could carry a credential")
		})
	}
}

// The two inputs reach the pod over the wire a consumer's SDK sends them on, through the plugin's
// own Construct, in a preview: outsideOrigin as a plain string and signInOutside as a plain
// boolean, decoded and rendered as the direct path renders them.
func TestConstructRendersTheOutsideInputsInAPreview(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["outsideOrigin"] = property.New(dispatchOutsideOrigin)
	inputs["signInOutside"] = property.New(true)
	m, _, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
	env := appEnv(t, m)
	require.Equal(t, "1", env["DISPATCH_SIGNIN_OUTSIDE"])
	require.Equal(t, dispatchOutsideOrigin, env["DISPATCH_OUTSIDE_ORIGIN"])
}
