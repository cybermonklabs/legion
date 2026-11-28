package gcp_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

const (
	// dispatchFrontDoorURL is the front door's name as an origin: Dispatch's canonical address on
	// the tailnet, and a served one beside publicOrigin.
	dispatchFrontDoorURL = "https://example-dispatch.example-tailnet.ts.net"
	// dispatchServedOrigin is a second address Dispatch serves, on a host Google takes in a
	// redirect URI, so it serves beside browser sign-in.
	dispatchServedOrigin = "https://b.example.com"
	// dispatchFormerOrigin is an address Dispatch served before, which needs no Google callback.
	dispatchFormerOrigin = "https://old.example.test"
)

// dispatchFrontDoorPublicOriginArgs is the default fixture, the tailnet inputs and browser
// sign-in, with publicOrigin beside them: the canonical address a load balancer of the
// deployment's own serves, and the front door's name a served one.
func dispatchFrontDoorPublicOriginArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.PublicOrigin = dispatchPublicOrigin
	return args
}

// editedDispatchArgs is base's fixture with edit applied to it.
func editedDispatchArgs(base func() *gcp.DispatchArgs, edit func(*gcp.DispatchArgs)) func() *gcp.DispatchArgs {
	return func() *gcp.DispatchArgs {
		args := base()
		edit(args)
		return args
	}
}

// appOriginEnv is the app's envoy-dispatch environment from DISPATCH_SERVER_URL up to
// DISPATCH_LISTEN_HOST, which follows Dispatch's addresses and its proxy ranges where neither
// parentStatusRule nor legionStatusScope is set, as in every fixture here, each variable as
// NAME=value, in the order the pod template lists them.
func appOriginEnv(t *testing.T, m *pulumitest.Mocks) []string {
	t.Helper()
	var lines []string
	for _, item := range podContainer(t, deploymentPodTemplate(t, m, "example-dispatch"), "envoy-dispatch")["env"].([]any) {
		entry := item.(map[string]any)
		name := entry["name"].(string)
		if name == "DISPATCH_LISTEN_HOST" {
			require.NotEmpty(t, lines, "DISPATCH_SERVER_URL comes before DISPATCH_LISTEN_HOST")
			return lines
		}
		if name == "DISPATCH_SERVER_URL" || len(lines) > 0 {
			lines = append(lines, name+"="+entry["value"].(string))
		}
	}
	t.Fatal("the app's environment carries no DISPATCH_LISTEN_HOST")
	return nil
}

// The app's environment names Dispatch's canonical address in DISPATCH_SERVER_URL, its other
// served addresses in DISPATCH_SERVED_ORIGINS and its former ones in DISPATCH_FORMER_ORIGINS, the
// two lists right after it, each comma-joined in the order given with one trailing slash trimmed,
// and each only where it holds an address: the variables before DISPATCH_LISTEN_HOST are exactly
// these. With the tailnet inputs the canonical address is the front door's name, and a second
// served address beside it leaves it so; with publicOrigin beside them publicOrigin is canonical
// and the front door's name the first served address; without the tailnet inputs publicOrigin is
// canonical, and in the sandbox the app's own Service, each with the lists after it. Google's
// redirect rule holds a served address only with browser sign-in on, so a tailnet Dispatch
// without it serves a host Google would refuse.
func TestDispatchRendersServedAndFormerOrigins(t *testing.T) {
	bothLists := func(a *gcp.DispatchArgs) {
		a.ServedOrigins = []string{dispatchServedOrigin}
		a.FormerOrigins = []string{dispatchFormerOrigin}
	}
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
		want []string
	}{
		{"the tailnet inputs alone", dispatchArgs, []string{"DISPATCH_SERVER_URL=" + dispatchFrontDoorURL}},
		{"the tailnet inputs and a former address", dispatchArgsWith(func(a *gcp.DispatchArgs) {
			a.FormerOrigins = []string{dispatchFormerOrigin}
		}), []string{"DISPATCH_SERVER_URL=" + dispatchFrontDoorURL, "DISPATCH_FORMER_ORIGINS=" + dispatchFormerOrigin}},
		{"the tailnet inputs and two more served addresses, the canonical one kept", dispatchArgsWith(func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{dispatchServedOrigin, "https://c.example.com:8443/"}
			a.FormerOrigins = []string{dispatchFormerOrigin, "http://older.example.test:8080"}
		}), []string{
			"DISPATCH_SERVER_URL=" + dispatchFrontDoorURL,
			"DISPATCH_SERVED_ORIGINS=" + dispatchServedOrigin + ",https://c.example.com:8443",
			"DISPATCH_FORMER_ORIGINS=" + dispatchFormerOrigin + ",http://older.example.test:8080",
		}},
		{"the tailnet inputs and publicOrigin", dispatchFrontDoorPublicOriginArgs, []string{
			"DISPATCH_SERVER_URL=" + dispatchPublicOrigin,
			"DISPATCH_SERVED_ORIGINS=" + dispatchFrontDoorURL,
		}},
		{"the tailnet inputs, publicOrigin and both lists", editedDispatchArgs(dispatchFrontDoorPublicOriginArgs, bothLists), []string{
			"DISPATCH_SERVER_URL=" + dispatchPublicOrigin,
			"DISPATCH_SERVED_ORIGINS=" + dispatchFrontDoorURL + "," + dispatchServedOrigin,
			"DISPATCH_FORMER_ORIGINS=" + dispatchFormerOrigin,
		}},
		{"publicOrigin without the tailnet inputs", editedDispatchArgs(dispatchPublicOriginArgs, bothLists), []string{
			"DISPATCH_SERVER_URL=" + dispatchPublicOrigin,
			"DISPATCH_SERVED_ORIGINS=" + dispatchServedOrigin,
			"DISPATCH_FORMER_ORIGINS=" + dispatchFormerOrigin,
		}},
		{"the sandbox", editedDispatchArgs(dispatchSandboxArgs, func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{"http://b.example.test:8766"}
			a.FormerOrigins = []string{dispatchFormerOrigin}
		}), []string{
			"DISPATCH_SERVER_URL=" + dispatchSandboxURL,
			"DISPATCH_SERVED_ORIGINS=http://b.example.test:8766",
			"DISPATCH_FORMER_ORIGINS=" + dispatchFormerOrigin,
		}},
		{"the tailnet inputs with no browser sign-in, on a host Google would refuse", editedDispatchArgs(dispatchTailnetWithoutSignInArgs, func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{"https://b.example.test"}
		}), []string{"DISPATCH_SERVER_URL=" + dispatchFrontDoorURL, "DISPATCH_SERVED_ORIGINS=https://b.example.test"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			require.Equal(t, row.want, appOriginEnv(t, m))
		})
	}
}

// The url output a consumer reads is Dispatch's canonical address, the address DISPATCH_SERVER_URL
// names in the same render, over the wire a consumer's SDK reads it on: the front door's name with
// the tailnet inputs, a second served address beside it changing nothing; publicOrigin beside the
// tailnet inputs and without them; and the app's own Service in the sandbox.
func TestDispatchUrlIsTheCanonicalAddress(t *testing.T) {
	withoutTheTailnet := func(inputs map[string]property.Value) {
		for _, in := range dispatchSandboxTailnetGroup {
			delete(inputs, in.input)
		}
		inputs["natsUrl"] = property.New(inClusterNatsURL)
	}
	for _, row := range []struct {
		name string
		edit func(map[string]property.Value)
		want string
	}{
		{"the tailnet inputs alone", func(map[string]property.Value) {}, dispatchFrontDoorURL},
		{"the tailnet inputs and a served address", func(inputs map[string]property.Value) {
			inputs["servedOrigins"] = stringArray(dispatchServedOrigin)
		}, dispatchFrontDoorURL},
		{"the tailnet inputs and publicOrigin", func(inputs map[string]property.Value) {
			inputs["publicOrigin"] = property.New(dispatchPublicOrigin)
			inputs["servedOrigins"] = stringArray(dispatchServedOrigin)
		}, dispatchPublicOrigin},
		{"publicOrigin without the tailnet inputs", func(inputs map[string]property.Value) {
			withoutTheTailnet(inputs)
			inputs["publicOrigin"] = property.New(dispatchPublicOrigin)
			inputs["servedOrigins"] = stringArray(dispatchServedOrigin)
		}, dispatchPublicOrigin},
		{"the sandbox", func(inputs map[string]property.Value) {
			withoutTheTailnet(inputs)
			for _, in := range dispatchSandboxSignInGroup {
				delete(inputs, in.input)
			}
			inputs["allowedNamespaces"] = stringArray("legion")
			inputs["servedOrigins"] = stringArray("http://b.example.test:8766")
		}, dispatchSandboxURL},
	} {
		t.Run(row.name, func(t *testing.T) {
			inputs := dispatchConstructBaseInputs()
			row.edit(inputs)
			m, response, err := constructDispatch(t, inputs, false)
			require.NoError(t, err)
			url, ok := response.State.GetOk("url")
			require.True(t, ok, "url is in the state")
			require.Equal(t, row.want, url.AsString())
			require.Equal(t, row.want, appEnv(t, m)["DISPATCH_SERVER_URL"], "the url output and DISPATCH_SERVER_URL are one address")
		})
	}
}

// No two of Dispatch's addresses share a host, compared by name at any port, as Dispatch compares
// them when it starts and refuses every such pair: the canonical address, the front door's name,
// each served address and each former address. Each pair is refused before anything registers, as
// a core.InputError naming the input taken later, in the order the front door's name,
// publicOrigin, the sandbox's own Service, servedOrigins and formerOrigins. The same addresses,
// each on a host of its own, resolve.
func TestDispatchRefusesOverlappingOrigins(t *testing.T) {
	const ownHost = ": each address of Dispatch's needs a host of its own"
	frontDoorHost := strings.TrimPrefix(dispatchFrontDoorURL, "https://")
	for _, row := range []struct {
		name, input string
		args        func() *gcp.DispatchArgs
		want        string
	}{
		{"publicOrigin on the front door's host", "publicOrigin", dispatchArgsWith(func(a *gcp.DispatchArgs) {
			a.PublicOrigin = dispatchFrontDoorURL + ":8443"
		}), "publicOrigin's host " + frontDoorHost + " is the front door's" + ownHost},
		{"a served address on the front door's host", "servedOrigins", dispatchArgsWith(func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{dispatchFrontDoorURL + ":8443"}
		}), "servedOrigins[0]'s host " + frontDoorHost + " is the front door's" + ownHost},
		{"a served address on the front door's host beside publicOrigin", "servedOrigins", editedDispatchArgs(dispatchFrontDoorPublicOriginArgs, func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{dispatchFrontDoorURL}
		}), "servedOrigins[0]'s host " + frontDoorHost + " is the front door's" + ownHost},
		{"a served address on publicOrigin's host beside the front door", "servedOrigins", editedDispatchArgs(dispatchFrontDoorPublicOriginArgs, func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{"https://dispatch.example.com:8443"}
		}), "servedOrigins[0]'s host dispatch.example.com is publicOrigin's" + ownHost},
		{"a served address on publicOrigin's host", "servedOrigins", editedDispatchArgs(dispatchPublicOriginArgs, func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{"https://dispatch.example.com:8443"}
		}), "servedOrigins[0]'s host dispatch.example.com is publicOrigin's" + ownHost},
		{"a served address on the sandbox's own host", "servedOrigins", editedDispatchArgs(dispatchSandboxArgs, func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{"http://example-dispatch.dispatch.svc.cluster.local:9000"}
		}), "servedOrigins[0]'s host example-dispatch.dispatch.svc.cluster.local is the app's own Service's" + ownHost},
		{"two served addresses on one host", "servedOrigins", dispatchArgsWith(func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{dispatchServedOrigin, dispatchServedOrigin + ":8443"}
		}), "servedOrigins[1]'s host b.example.com is servedOrigins[0]'s" + ownHost},
		{"a former address on the front door's host", "formerOrigins", dispatchArgsWith(func(a *gcp.DispatchArgs) {
			a.FormerOrigins = []string{dispatchFrontDoorURL + ":8443"}
		}), "formerOrigins[0]'s host " + frontDoorHost + " is the front door's" + ownHost},
		{"a former address on publicOrigin's host beside the front door", "formerOrigins", editedDispatchArgs(dispatchFrontDoorPublicOriginArgs, func(a *gcp.DispatchArgs) {
			a.FormerOrigins = []string{"https://dispatch.example.com:8443"}
		}), "formerOrigins[0]'s host dispatch.example.com is publicOrigin's" + ownHost},
		{"a former address on a served address's host", "formerOrigins", dispatchArgsWith(func(a *gcp.DispatchArgs) {
			a.ServedOrigins = []string{dispatchServedOrigin}
			a.FormerOrigins = []string{"http://b.example.com"}
		}), "formerOrigins[0]'s host b.example.com is servedOrigins[0]'s" + ownHost},
		{"two former addresses on one host", "formerOrigins", dispatchArgsWith(func(a *gcp.DispatchArgs) {
			a.FormerOrigins = []string{dispatchFormerOrigin, "http://old.example.test:8080"}
		}), "formerOrigins[1]'s host old.example.test is formerOrigins[0]'s" + ownHost},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			requireInputError(t, err, row.input)
			m, err := runDispatch(t, row.args(), true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
			require.NotContains(t, m.Types(), deploymentType, "nothing of Dispatch's registers")
		})
	}
	everyAddress := func(a *gcp.DispatchArgs) {
		a.ServedOrigins = []string{dispatchServedOrigin, "https://c.example.com"}
		a.FormerOrigins = []string{dispatchFormerOrigin, "https://older.example.test"}
	}
	for _, args := range []func() *gcp.DispatchArgs{
		dispatchArgsWith(everyAddress),
		editedDispatchArgs(dispatchFrontDoorPublicOriginArgs, everyAddress),
		editedDispatchArgs(dispatchPublicOriginArgs, everyAddress),
	} {
		_, err := gcp.ResolveDispatch(args())
		require.NoError(t, err, "each address on a host of its own")
	}
}

// Each served and former address is an origin written as a browser writes it, since Dispatch
// compares a request's Origin with it as a string and refuses to start on any other spelling:
// weborigin.Parse's refusals are prefixed with the entry's path, named as the list, and none
// echoes a user, which could carry a credential. With browser sign-in on, a served address must be
// https, as publicOrigin must, and its host one Google takes in a redirect URI, since the Google
// client lists its callback; a former address serves no sign-in, and neither does a served one
// without browser sign-in or behind the IAP door, so those take http and a host Google would
// refuse.
func TestDispatchRefusesAServedOrFormerAddressItCannotServe(t *testing.T) {
	notOrigin := func(entry string) string {
		return entry + ": must be an absolute http or https URL with no path, user, query or fragment, as an origin is (not echoed)"
	}
	served := func(base func() *gcp.DispatchArgs, entries ...string) func() *gcp.DispatchArgs {
		return editedDispatchArgs(base, func(a *gcp.DispatchArgs) { a.ServedOrigins = entries })
	}
	former := func(entries ...string) func() *gcp.DispatchArgs {
		return dispatchArgsWith(func(a *gcp.DispatchArgs) { a.FormerOrigins = entries })
	}
	const offTheList = "servedOrigins[0]'s host b.example.test ends in test, no top-level domain on the public suffix list, " +
		"which Google requires of an OAuth redirect URI's host, so browser sign-in at it could not complete"
	for _, row := range []struct {
		name, input string
		args        func() *gcp.DispatchArgs
		want        string
	}{
		{"a served address with no scheme", "servedOrigins", served(dispatchArgs, "b.example.com"), notOrigin("servedOrigins[0]")},
		{"a served address with a path", "servedOrigins", served(dispatchArgs, "https://b.example.com/dispatch"), notOrigin("servedOrigins[0]")},
		{"a served address of another scheme", "servedOrigins", served(dispatchArgs, "ftp://b.example.com"), notOrigin("servedOrigins[0]")},
		{"an empty served address after a good one", "servedOrigins", served(dispatchArgs, dispatchServedOrigin, ""), notOrigin("servedOrigins[1]")},
		{"a served address with an upper-case host", "servedOrigins", served(dispatchArgs, "https://B.example.com"),
			`servedOrigins[0]: host label "B" is not a lowercase DNS label`},
		{"a served address with its scheme's default port", "servedOrigins", served(dispatchArgs, "https://b.example.com:443"),
			"servedOrigins[0]: must be written as a browser writes this origin, https://b.example.com: a lowercase scheme and no default port"},
		{"a served address with an upper-case scheme", "servedOrigins", served(dispatchArgs, "HTTPS://b.example.com"),
			"servedOrigins[0]: must be written as a browser writes this origin, https://b.example.com: a lowercase scheme and no default port"},
		{"a former address with a query", "formerOrigins", former("https://old.example.test?from=x"), notOrigin("formerOrigins[0]")},
		{"a former address with a user", "formerOrigins", former("https://example-user@old.example.test"), notOrigin("formerOrigins[0]")},
		{"an http served address with browser sign-in", "servedOrigins", served(dispatchArgs, "https://c.example.com", "http://b.example.com"),
			"servedOrigins[1] must be an https origin with browser sign-in on: Dispatch's session cookie and the Google sign-in need TLS at each address it serves, as they do at publicOrigin"},
		{"an http served address beside publicOrigin without the front door", "servedOrigins", served(dispatchPublicOriginArgs, "http://b.example.com"),
			"servedOrigins[0] must be an https origin with browser sign-in on: Dispatch's session cookie and the Google sign-in need TLS at each address it serves, as they do at publicOrigin"},
		{"a served IP address with browser sign-in", "servedOrigins", served(dispatchArgs, "https://10.0.0.1"),
			"servedOrigins[0]'s host 10.0.0.1 is an IP address, which Google refuses as the host of an OAuth redirect URI, so browser sign-in at it could not complete: name the origin by a DNS name"},
		{"a served host off the public suffix list with browser sign-in", "servedOrigins", served(dispatchArgs, "https://b.example.test"), offTheList},
		{"the same beside publicOrigin without the front door", "servedOrigins", served(dispatchPublicOriginArgs, "https://b.example.test"), offTheList},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			require.NotContains(t, err.Error(), "example-user", "the refusal echoes no user, which could carry a credential")
			requireInputError(t, err, row.input)
		})
	}
	for _, args := range []func() *gcp.DispatchArgs{
		former("https://10.0.0.1", "https://old.example.test/"),
		served(dispatchTailnetWithoutSignInArgs, "https://10.0.0.2", "https://b.example.test"),
		served(dispatchIAPArgs, "https://b.example.test"),
	} {
		_, err := gcp.ResolveDispatch(args())
		require.NoError(t, err)
	}
}

// With the tailnet inputs set, every object of the front door renders the same bytes with and
// without publicOrigin and every new input, so a stack that sets them restarts no front-door pod:
// each front-door resource the default declaration holds carries the same values, its secrets'
// too; serve.json and the front door's pod are the base commit's goldens; the front door's and the
// listener's Deployment specs are the same bytes; and the declaration is the golden one, no object
// added or dropped. Control: the app's own Deployment, whose pods these inputs configure, differs,
// so the comparison sees a change where there is one.
func TestDispatchFrontDoorIsByteEqualWithAndWithoutTheNewInputs(t *testing.T) {
	base, err := runDispatch(t, dispatchArgs(), true)
	require.NoError(t, err)
	args := dispatchFrontDoorPublicOriginArgs()
	args.ServedOrigins = []string{dispatchServedOrigin}
	args.FormerOrigins = []string{dispatchFormerOrigin}
	args.TrustedProxyRanges = []string{"10.129.0.0/23"}
	args.PreStopSeconds, args.TerminationGracePeriodSeconds = pulumi.IntRef(20), pulumi.IntRef(50)
	changed, err := runDispatch(t, args, true)
	require.NoError(t, err)

	for _, object := range dispatchSandboxFrontDoor {
		typ, name, ok := strings.Cut(object, " ")
		require.True(t, ok, "%q names a type and a name", object)
		t.Run(object, func(t *testing.T) {
			want, got := base.Named(t, typ, name).Inputs, changed.Named(t, typ, name).Inputs
			require.Equal(t, prodRenderKey(t, normalizedProperty(resource.NewProperty(want))), prodRenderKey(t, normalizedProperty(resource.NewProperty(got))))
			require.Equal(t, want, got, "the same values, a secret's own included")
		})
	}
	require.Equal(t, readGolden(t, "dispatch-serve.golden"), serveConfig(t, changed))
	requirePodGolden(t, changed, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
	requirePodGolden(t, changed, "example-dispatch-listener", "dispatch-listener-pod.golden")
	for _, deployment := range []string{"example-dispatch-tailnet", "example-dispatch-listener"} {
		require.Equal(t, prodRenderKey(t, deploymentSpec(t, base, deployment)), prodRenderKey(t, deploymentSpec(t, changed, deployment)), "%s's spec", deployment)
	}
	require.Equal(t, readGolden(t, "dispatch-registrations.golden"), dispatchRegistrations(changed))

	require.NotEqual(t, prodRenderKey(t, deploymentSpec(t, base, "example-dispatch")), prodRenderKey(t, deploymentSpec(t, changed, "example-dispatch")),
		"the app's own pods carry the new inputs")
}

// With none of the new inputs set, every object renders as it did before they existed: every
// committed golden of the app's pod renders byte for byte from the fixture it was committed for,
// and the front door's and the listener's pods, serve.json and the whole declaration, the
// sandbox's included, render as their goldens hold. So do they with the new inputs sent at what
// they are when unset (empty lists, a 5-second wait and a 35-second grace period), so a stack
// that adds them at those values restarts no pod. The table is held to the goldens on disk, so a
// pod golden added later without its fixture here fails rather than escaping the check.
func TestDispatchWithNoNewInputRendersTheBaseGoldens(t *testing.T) {
	atTheirDefaults := func(base func() *gcp.DispatchArgs) func() *gcp.DispatchArgs {
		return editedDispatchArgs(base, func(a *gcp.DispatchArgs) {
			a.ServedOrigins, a.FormerOrigins, a.TrustedProxyRanges = []string{}, []string{}, []string{}
			a.PreStopSeconds, a.TerminationGracePeriodSeconds = pulumi.IntRef(5), pulumi.IntRef(35)
		})
	}
	pods := []struct {
		golden string
		args   func() *gcp.DispatchArgs
	}{
		{"dispatch-pod.golden", dispatchArgs},
		{"dispatch-pod-with-broker-and-app.golden", func() *gcp.DispatchArgs { return dispatchBrokerAndAppArgs(t) }},
		{"dispatch-pod-with-verdict-app.golden", func() *gcp.DispatchArgs { return dispatchVerdictAppArgs(t) }},
		{"dispatch-pod-with-azure-key.golden", dispatchArgsWith(setAzureKey)},
		{"dispatch-pod-with-azure-federation.golden", dispatchArgsWith(setAzureFederation)},
		{"dispatch-pod-with-azure-answerer.golden", dispatchArgsWith(setAzureChat)},
		{"dispatch-pod-with-chat-endpoint.golden", dispatchArgsWith(setChatEndpoint)},
		{dispatchOutsideGolden, dispatchOutsideArgs},
		{dispatchPasskeyGolden, dispatchPasskeyArgs},
		{dispatchPublicOriginGolden, dispatchPublicOriginArgs},
		{dispatchIAPGolden, dispatchIAPArgs},
		{dispatchServiceIdentityGolden, dispatchServiceIdentityArgs},
	}
	committed, err := filepath.Glob("testdata/dispatch-pod*.golden")
	require.NoError(t, err)
	require.NotEmpty(t, committed, "the glob found the committed pod goldens")
	var covered []string
	for _, row := range pods {
		covered = append(covered, "testdata/"+row.golden)
		for _, sent := range []struct {
			name string
			args func() *gcp.DispatchArgs
		}{{"unset", row.args}, {"at their defaults", atTheirDefaults(row.args)}} {
			t.Run(row.golden+", the new inputs "+sent.name, func(t *testing.T) {
				m, err := runDispatch(t, sent.args(), true)
				require.NoError(t, err)
				requirePodGolden(t, m, "example-dispatch", row.golden)
			})
		}
	}
	require.ElementsMatch(t, committed, covered, "every committed pod golden has its fixture in this test's table")

	for _, sent := range []struct {
		name             string
		tailnet, sandbox func() *gcp.DispatchArgs
	}{{"unset", dispatchArgs, dispatchSandboxArgs}, {"at their defaults", atTheirDefaults(dispatchArgs), atTheirDefaults(dispatchSandboxArgs)}} {
		t.Run("the other objects, the new inputs "+sent.name, func(t *testing.T) {
			m, err := runDispatch(t, sent.tailnet(), true)
			require.NoError(t, err)
			requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
			requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
			require.Equal(t, readGolden(t, "dispatch-serve.golden"), serveConfig(t, m))
			require.Equal(t, readGolden(t, "dispatch-registrations.golden"), dispatchRegistrations(m))
			sandbox, err := runDispatch(t, sent.sandbox(), true)
			require.NoError(t, err)
			require.Equal(t, dispatchSandboxGolden(t), dispatchRegistrations(sandbox))
		})
	}
}

// Every new input reaches the pod over the wire a consumer's SDK sends it on, through the
// plugin's own Construct, in a preview: publicOrigin beside the tailnet inputs and the two lists
// as plain strings and lists of strings, the proxy ranges without signInPasskey, and the drain as
// two integers, each decoded and rendered as the direct path renders it, with the url output the
// canonical address.
func TestConstructRendersTheNewInputsInAPreview(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["publicOrigin"] = property.New(dispatchPublicOrigin)
	inputs["servedOrigins"] = stringArray(dispatchServedOrigin)
	inputs["formerOrigins"] = stringArray(dispatchFormerOrigin)
	inputs["trustedProxyRanges"] = stringArray("10.129.0.0/23")
	inputs["preStopSeconds"] = property.New(20.0)
	inputs["terminationGracePeriodSeconds"] = property.New(50.0)
	m, response, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
	require.Equal(t, []string{
		"DISPATCH_SERVER_URL=" + dispatchPublicOrigin,
		"DISPATCH_SERVED_ORIGINS=" + dispatchFrontDoorURL + "," + dispatchServedOrigin,
		"DISPATCH_FORMER_ORIGINS=" + dispatchFormerOrigin,
		"DISPATCH_TRUSTED_PROXY_RANGES=10.129.0.0/23",
	}, appOriginEnv(t, m))
	grace, wait := appDrain(t, m)
	require.Equal(t, 50.0, grace)
	require.Equal(t, 20.0, wait)
	url, ok := response.State.GetOk("url")
	require.True(t, ok, "url is in the state")
	require.Equal(t, dispatchPublicOrigin, url.AsString())
}
