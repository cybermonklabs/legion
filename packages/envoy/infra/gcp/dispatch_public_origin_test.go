package gcp_test

import (
	"strings"
	"testing"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// dispatchPublicOrigin is the https origin a deployment's own entrance serves Dispatch at
	// when it has no front door, on a host Google takes in a redirect URI.
	dispatchPublicOrigin = "https://dispatch.example.com"
	// dispatchIAPAudience is the audience IAP signs for a load balancer's backend service:
	// /projects/<project number>/global/backendServices/<backend service id>.
	dispatchIAPAudience = "/projects/123456789012/global/backendServices/1234567890123456789"
	// dispatchDirectoryAccount is the keyless service account Dispatch reads the Workspace
	// Directory through behind the IAP door, and dispatchDirectoryAdmin the Workspace user those
	// reads act as.
	dispatchDirectoryAccount = "dispatch-directory@example-project.iam.gserviceaccount.com"
	dispatchDirectoryAdmin   = "directory-reader@example.com"
	// The app's pod at publicOrigin with Google sign-in, and behind the IAP door.
	dispatchPublicOriginGolden = "dispatch-pod-with-public-origin.golden"
	dispatchIAPGolden          = "dispatch-pod-with-iap.golden"
)

// dispatchPublicOriginArgs is a Dispatch with no front door served at publicOrigin with Google
// sign-in: the sandbox's inputs, the sign-in group and publicOrigin, and no allowedNamespaces,
// since the deployment's entrance admits its own traffic.
func dispatchPublicOriginArgs() *gcp.DispatchArgs {
	args := dispatchSandboxArgs()
	args.AllowedNamespaces = nil
	args.PublicOrigin = dispatchPublicOrigin
	for _, in := range dispatchSandboxSignInGroup {
		in.set(args)
	}
	return args
}

// dispatchIAPArgs is a Dispatch behind the IAP door: served at publicOrigin, with iapAudience, the
// domain whose accounts may use Dispatch and the two inputs of its Workspace Directory reads, and no
// Google client.
func dispatchIAPArgs() *gcp.DispatchArgs {
	args := dispatchSandboxArgs()
	args.AllowedNamespaces = nil
	args.PublicOrigin = dispatchPublicOrigin
	args.IapAudience = pulumi.String(dispatchIAPAudience)
	args.SignInDomain = "example.com"
	args.DirectoryAccount = pulumi.String(dispatchDirectoryAccount)
	args.DirectoryAdmin = pulumi.String(dispatchDirectoryAdmin)
	return args
}

// At publicOrigin Dispatch tells a reader it is at that origin, runs Google sign-in there with
// the tailnet inputs unset, and builds no front door: the registrations are the sandbox's own,
// so the origin changes the app's environment and nothing else the component declares, and the
// program needs no tailscale provider. The control is the same inputs without publicOrigin,
// which the sandbox refuses for the sign-in inputs, so publicOrigin is what admits them.
func TestDispatchServesAtItsPublicOrigin(t *testing.T) {
	args := dispatchPublicOriginArgs()
	settings, err := gcp.ResolveDispatch(args)
	require.NoError(t, err)
	require.Equal(t, dispatchPublicOrigin, settings.ServerURL)

	_, err = runDispatchWithProviders(t, "example-dispatch", args, newGCPProvider, newRandomProvider)
	require.NoError(t, err, "a Dispatch at publicOrigin declares no Tailscale Service, so it needs no tailscale provider")
	// The registration golden was taken from a program declaring all three providers, so this run
	// declares them too.
	m, err := runDispatch(t, args, true)
	require.NoError(t, err)
	require.Equal(t, dispatchSandboxGolden(t), dispatchRegistrations(m))
	requirePodGolden(t, m, "example-dispatch", dispatchPublicOriginGolden)
	env := appEnv(t, m)
	require.Equal(t, dispatchPublicOrigin, env["DISPATCH_SERVER_URL"])
	require.Equal(t, "example-client-id", env["DISPATCH_SIGNIN_CLIENT_ID"])
	require.NotContains(t, env, "DISPATCH_IDENTITY", "sign-in is Dispatch's default identity, so no mode is named")
	require.Empty(t, policyIngressRules(t, m, "example-dispatch", 0), "with no allowedNamespaces the deployment's entrance admits its own traffic")

	control := dispatchPublicOriginArgs()
	control.PublicOrigin = ""
	_, err = gcp.ResolveDispatch(control)
	require.ErrorContains(t, err, "signInClientId configures browser sign-in, which needs an https origin, the tailnet inputs' or publicOrigin")
}

// A trailing slash is trimmed, as Dispatch trims one, and a port that is not https's default is
// kept: the value rendered is the origin as a browser writes it.
func TestDispatchPublicOriginIsRenderedAsABrowserWritesIt(t *testing.T) {
	args := dispatchPublicOriginArgs()
	args.PublicOrigin = "https://dispatch.example.com:8443/"
	settings, err := gcp.ResolveDispatch(args)
	require.NoError(t, err)
	require.Equal(t, "https://dispatch.example.com:8443", settings.ServerURL)
}

// outsideOrigin, which a Dispatch without a front door used to refuse, is taken beside
// publicOrigin as it is beside the front door: the second origin follows the first in the app's
// environment.
func TestDispatchPublicOriginTakesAnOutsideOrigin(t *testing.T) {
	args := dispatchPublicOriginArgs()
	args.OutsideOrigin = dispatchOutsideOrigin
	m, err := runDispatchWithProviders(t, "example-dispatch", args, newGCPProvider, newRandomProvider)
	require.NoError(t, err)
	requireRightAfter(t, m, "DISPATCH_SERVER_URL", "DISPATCH_OUTSIDE_ORIGIN")
	require.Equal(t, dispatchOutsideOrigin, appEnv(t, m)["DISPATCH_OUTSIDE_ORIGIN"])
}

// Each publicOrigin the component cannot serve is refused naming it before anything registers:
// one weborigin.Parse refuses, one that is not https, one beside the tailnet inputs, whose front
// door's name is then Dispatch's origin, one whose host Google refuses in a redirect URI while
// browser sign-in is on, and a second origin on its own host. What no Dispatch without a front
// door can make good is refused beside it too, as in the sandbox.
func TestDispatchRefusesAPublicOriginItCannotServe(t *testing.T) {
	with := func(edit func(*gcp.DispatchArgs)) func() *gcp.DispatchArgs {
		return func() *gcp.DispatchArgs {
			args := dispatchPublicOriginArgs()
			edit(args)
			return args
		}
	}
	for _, row := range []struct {
		name, input string
		args        func() *gcp.DispatchArgs
		want        string
	}{
		{"no scheme", "publicOrigin", with(func(a *gcp.DispatchArgs) { a.PublicOrigin = "dispatch.example.com" }),
			"publicOrigin: must be an absolute http or https URL with no path, user, query or fragment, as an origin is (not echoed)"},
		{"a path", "publicOrigin", with(func(a *gcp.DispatchArgs) { a.PublicOrigin = "https://dispatch.example.com/dispatch" }),
			"publicOrigin: must be an absolute http or https URL with no path, user, query or fragment, as an origin is (not echoed)"},
		{"http", "publicOrigin", with(func(a *gcp.DispatchArgs) { a.PublicOrigin = "http://dispatch.example.com" }),
			"publicOrigin must be an https origin: Dispatch's session cookie, browser sign-in and IAP need TLS at it, as they do at the front door's name"},
		{"beside the tailnet inputs", "publicOrigin", func() *gcp.DispatchArgs {
			args := dispatchArgs()
			args.PublicOrigin = dispatchPublicOrigin
			return args
		}, "publicOrigin is refused beside the tailnet inputs: the front door serves Dispatch at <hostname>.<tailnetDnsName>, which is then its origin, " +
			"and a second origin beside that one is outsideOrigin; unset publicOrigin, or unset the tailnet inputs"},
		{"an IP address with browser sign-in", "publicOrigin", with(func(a *gcp.DispatchArgs) { a.PublicOrigin = "https://10.0.0.1" }),
			"publicOrigin's host 10.0.0.1 is an IP address, which Google refuses as the host of an OAuth redirect URI, so browser sign-in at it could not complete: name the origin by a DNS name"},
		{"a single label with browser sign-in", "publicOrigin", with(func(a *gcp.DispatchArgs) { a.PublicOrigin = "https://dispatch" }),
			"publicOrigin's host dispatch is a single label, with no top-level domain on the public suffix list, which Google requires of an OAuth redirect URI's host, so browser sign-in at it could not complete"},
		{"outsideOrigin on publicOrigin's host", "outsideOrigin", with(func(a *gcp.DispatchArgs) { a.OutsideOrigin = "https://dispatch.example.com:8443" }),
			"outsideOrigin's host dispatch.example.com is publicOrigin's: a second origin needs a host of its own"},
		{"outsideOrigin over http", "outsideOrigin", with(func(a *gcp.DispatchArgs) { a.OutsideOrigin = "http://dispatch-outside.example.com" }),
			"outsideOrigin must be an https origin: browser sign-in at it needs TLS, as publicOrigin's does"},
		{"natsInCluster unset", "natsInCluster", with(func(a *gcp.DispatchArgs) { a.NatsInCluster = nil }),
			"natsInCluster is required without the tailnet inputs: the pods reach a tailnet NATS hub only through the front door's SOCKS5 proxy, and no front door is built, so the hub must run in the cluster"},
		{"frontDoorReplicas", "frontDoorReplicas", with(func(a *gcp.DispatchArgs) { a.FrontDoorReplicas = pulumi.IntRef(2) }),
			"frontDoorReplicas sizes a front door, which is not built without the tailnet inputs: unset it, or set the tailnet inputs"},
		{"frontDoorPdbMinAvailable", "frontDoorPdbMinAvailable", with(func(a *gcp.DispatchArgs) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(1) }),
			"frontDoorPdbMinAvailable covers a front door, which is not built without the tailnet inputs: unset it, or set the tailnet inputs"},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			requireInputError(t, err, row.input)
		})
	}
	// Without browser sign-in Google's redirect rule does not apply: a deployment that serves
	// agents alone at an address Google would not take still resolves.
	args := dispatchPublicOriginArgs()
	for _, in := range dispatchSandboxSignInGroup {
		switch in.input {
		case "signInClientId":
			args.SignInClientId = ""
		case "signInClientSecret":
			args.SignInClientSecret = nil
		case "signInDomain":
			args.SignInDomain = ""
		}
	}
	args.PublicOrigin = "https://10.0.0.1"
	_, err := gcp.ResolveDispatch(args)
	require.NoError(t, err)
}

// Behind the IAP door the app names its person from IAP's signed header: DISPATCH_IDENTITY=iap,
// DISPATCH_IAP_AUDIENCE and DISPATCH_SIGNIN_DOMAIN, the domain whose accounts may use Dispatch,
// then DISPATCH_DIRECTORY_ACCOUNT and DISPATCH_DIRECTORY_ADMIN, which its Workspace Directory reads
// sign as and act as, in the place the sign-in variables hold, right after the Secret's keys and
// before DISPATCH_SERVER_URL, which is publicOrigin. No other sign-in variable renders and the
// Secret holds no client secret, since Dispatch runs no Google sign-in of its own; the component
// declares what the sandbox declares, and nothing of a front door.
func TestDispatchRendersTheIAPDoor(t *testing.T) {
	_, err := runDispatchWithProviders(t, "example-dispatch", dispatchIAPArgs(), newGCPProvider, newRandomProvider)
	require.NoError(t, err, "the IAP door declares no Tailscale Service, so it needs no tailscale provider")
	m, err := runDispatch(t, dispatchIAPArgs(), true)
	require.NoError(t, err)
	requirePodGolden(t, m, "example-dispatch", dispatchIAPGolden)
	env := appEnv(t, m)
	require.Equal(t, "iap", env["DISPATCH_IDENTITY"])
	require.Equal(t, dispatchIAPAudience, env["DISPATCH_IAP_AUDIENCE"])
	require.Equal(t, "example.com", env["DISPATCH_SIGNIN_DOMAIN"])
	require.Equal(t, dispatchDirectoryAccount, env["DISPATCH_DIRECTORY_ACCOUNT"])
	require.Equal(t, dispatchDirectoryAdmin, env["DISPATCH_DIRECTORY_ADMIN"])
	require.Equal(t, dispatchPublicOrigin, env["DISPATCH_SERVER_URL"])
	requireRightAfter(t, m, "DISPATCH_SIGNING_KEY", "DISPATCH_IDENTITY")
	requireRightAfter(t, m, "DISPATCH_IDENTITY", "DISPATCH_IAP_AUDIENCE")
	requireRightAfter(t, m, "DISPATCH_IAP_AUDIENCE", "DISPATCH_SIGNIN_DOMAIN")
	requireRightAfter(t, m, "DISPATCH_SIGNIN_DOMAIN", "DISPATCH_DIRECTORY_ACCOUNT")
	requireRightAfter(t, m, "DISPATCH_DIRECTORY_ACCOUNT", "DISPATCH_DIRECTORY_ADMIN")
	requireRightAfter(t, m, "DISPATCH_DIRECTORY_ADMIN", "DISPATCH_SERVER_URL")
	for name := range env {
		if name != "DISPATCH_SIGNIN_DOMAIN" {
			require.False(t, strings.HasPrefix(name, "DISPATCH_SIGNIN_"), "%s configures Google sign-in beside IAP", name)
		}
	}
	require.ElementsMatch(t, []string{"DATABASE_URL", "DISPATCH_AGENT_TOKEN", "DISPATCH_SIGNING_KEY"}, secretDataKeys(dispatchSecretData(t, m)))
	require.Equal(t, dispatchSandboxGolden(t), dispatchRegistrations(m))
}

// The IAP door needs publicOrigin, the origin IAP's load balancer serves, signInDomain, the
// domain Dispatch holds the signed header's hd claim to, and directoryAccount and directoryAdmin,
// the Workspace Directory reads Dispatch checks each person through, and runs beside no Google
// client of Dispatch's own and no second origin: each is refused naming it before
// anything registers, a client input for being sent at all rather than for the rest of the
// sign-in group being missing.
func TestDispatchRefusesWhatTheIAPDoorCannotServe(t *testing.T) {
	const needsOrigin = "iapAudience needs publicOrigin: IAP runs on the deployment's own load balancer, which serves Dispatch at that origin, " +
		"and no request through the tailnet front door or into the sandbox carries IAP's signed header; set publicOrigin, or unset iapAudience"
	clientRefusal := func(input string) string {
		return input + " configures Google sign-in, which iapAudience replaces: IAP signs a person in and Dispatch names them from IAP's signed header, " +
			"so it runs no sign-in of its own; unset " + input + ", or unset iapAudience"
	}
	needsDirectory := func(input string) string {
		return "iapAudience needs " + input + ": behind the IAP door Dispatch checks each person's Google Workspace account through the Directory API, " +
			"signing as directoryAccount and reading as directoryAdmin, and refuses to boot without either; set " + input + ", or unset iapAudience"
	}
	with := func(edit func(*gcp.DispatchArgs)) func() *gcp.DispatchArgs {
		return func() *gcp.DispatchArgs {
			args := dispatchIAPArgs()
			edit(args)
			return args
		}
	}
	for _, row := range []struct {
		name, input string
		args        func() *gcp.DispatchArgs
		want        string
	}{
		{"in the sandbox", "iapAudience", func() *gcp.DispatchArgs {
			args := dispatchSandboxArgs()
			args.IapAudience = pulumi.String(dispatchIAPAudience)
			return args
		}, needsOrigin},
		{"behind the tailnet front door", "iapAudience", func() *gcp.DispatchArgs {
			args := dispatchArgs()
			args.IapAudience = pulumi.String(dispatchIAPAudience)
			return args
		}, needsOrigin},
		{"signInClientId", "signInClientId", with(func(a *gcp.DispatchArgs) { a.SignInClientId = "example-client-id" }), clientRefusal("signInClientId")},
		{"signInClientSecret", "signInClientSecret", with(func(a *gcp.DispatchArgs) { a.SignInClientSecret = pulumi.String("example-client-secret") }), clientRefusal("signInClientSecret")},
		{"outsideOrigin", "outsideOrigin", with(func(a *gcp.DispatchArgs) { a.OutsideOrigin = dispatchOutsideOrigin }),
			"outsideOrigin is refused beside iapAudience: a browser reaches Dispatch through IAP alone, at publicOrigin, and a second origin is for " +
				"outside sign-in, which the IAP door does not run; unset outsideOrigin, or unset iapAudience"},
		{"no signInDomain", "signInDomain", with(func(a *gcp.DispatchArgs) { a.SignInDomain = "" }),
			`signInDomain "" must be a lowercase DNS name with iapAudience: the Workspace domain whose accounts may use Dispatch, ` +
				"which Dispatch holds IAP's signed header's hd claim to and refuses to boot without"},
		{"an uppercase signInDomain", "signInDomain", with(func(a *gcp.DispatchArgs) { a.SignInDomain = "Example.com" }),
			`signInDomain "Example.com" must be a lowercase DNS name with iapAudience`},
		{"no directoryAccount", "directoryAccount", with(func(a *gcp.DispatchArgs) { a.DirectoryAccount = nil }), needsDirectory("directoryAccount")},
		{"no directoryAdmin", "directoryAdmin", with(func(a *gcp.DispatchArgs) { a.DirectoryAdmin = nil }), needsDirectory("directoryAdmin")},
		{"neither Directory input", "directoryAccount", with(func(a *gcp.DispatchArgs) { a.DirectoryAccount, a.DirectoryAdmin = nil, nil }), needsDirectory("directoryAccount")},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.ErrorContains(t, err, row.want)
			requireInputError(t, err, row.input)
		})
	}
	_, err := gcp.ResolveDispatch(dispatchIAPArgs())
	require.NoError(t, err, "the same door with nothing beside it")
}

// dispatchDirectoryGroup is the two inputs of Dispatch's Workspace Directory reads, which the IAP
// door needs and which nothing else takes. Written here alone, as the tailnet and sign-in groups
// are, so the cluster-only table takes them from here.
var dispatchDirectoryGroup = []dispatchInput{
	{"directoryAccount", func(a *gcp.DispatchArgs) { a.DirectoryAccount = pulumi.String(dispatchDirectoryAccount) }},
	{"directoryAdmin", func(a *gcp.DispatchArgs) { a.DirectoryAdmin = pulumi.String(dispatchDirectoryAdmin) }},
}

// directoryAccount and directoryAdmin configure the Workspace Directory reads Dispatch makes only
// behind the IAP door, so either one without iapAudience is refused naming it before anything
// registers, in each mode that has no IAP door: the sandbox, publicOrigin with Google sign-in, and
// the tailnet front door. Each mode alone resolves, so the input is what is refused.
func TestDispatchRefusesADirectoryInputWithoutTheIAPDoor(t *testing.T) {
	for _, mode := range []struct {
		name string
		args func() *gcp.DispatchArgs
	}{
		{"the sandbox", dispatchSandboxArgs},
		{"publicOrigin with Google sign-in", dispatchPublicOriginArgs},
		{"the tailnet front door", dispatchArgs},
	} {
		_, err := gcp.ResolveDispatch(mode.args())
		require.NoError(t, err, "%s alone resolves", mode.name)
		for _, in := range dispatchDirectoryGroup {
			t.Run(mode.name+", "+in.input, func(t *testing.T) {
				args := mode.args()
				in.set(args)
				_, err := gcp.ResolveDispatch(args)
				require.EqualError(t, err, in.input+" is refused without iapAudience: Dispatch reads the Google Workspace Directory only behind the IAP door, "+
					"and refuses the setting at boot outside it; unset "+in.input+", or set iapAudience")
			})
		}
	}
}

// directoryAccount and directoryAdmin may be other resources' outputs or secret configuration, so
// each is judged once it resolves: a blank or whitespace-only value, which Dispatch reads as unset
// and so refuses to boot behind the IAP door without, and one holding an nkey seed, whole or inside
// an address, are refused naming the input and never the value. The documented values pass
// (TestDispatchRendersTheIAPDoor).
func TestDispatchRefusesADirectoryInputOnceItResolves(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, in := range []struct {
		input string
		edit  func(*gcp.DispatchArgs, pulumi.StringInput)
	}{
		{"directoryAccount", func(a *gcp.DispatchArgs, v pulumi.StringInput) { a.DirectoryAccount = v }},
		{"directoryAdmin", func(a *gcp.DispatchArgs, v pulumi.StringInput) { a.DirectoryAdmin = v }},
	} {
		blank := in.input + " is blank: Dispatch reads it as unset and refuses to boot behind the IAP door without it"
		for _, row := range []struct{ name, value, want string }{
			{"empty", "", blank},
			{"whitespace only", " \n\t", blank},
			{"a seed", seed, in.input + " holds an nkey seed (not echoed)"},
			{"a lowercase seed inside an address", strings.ToLower(seed) + "@example.com", in.input + " holds an nkey seed (not echoed)"},
		} {
			t.Run(in.input+", "+row.name, func(t *testing.T) {
				args := dispatchIAPArgs()
				in.edit(args, pulumi.String(row.value))
				_, err := runDispatchWithProviders(t, "example-dispatch", args, newGCPProvider, newRandomProvider)
				require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
				require.NotContains(t, strings.ToUpper(err.Error()), seed)
			})
		}
	}
}

// The audience may be another resource's output, so it is judged once it resolves: any value but
// the form IAP signs for a backend service is refused naming iapAudience, an App Engine
// application's among them, since Dispatch compares the token's aud with it as a string and would
// admit nobody. A seed is refused naming the input and never the value. The documented form
// passes (TestDispatchRendersTheIAPDoor).
func TestDispatchRefusesAnIAPAudienceOfAnotherFormOnceItResolves(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range []struct{ name, audience, want string }{
		{"empty", "", `iapAudience "" must be the audience IAP signs for a load balancer's backend service`},
		{"an App Engine application's", "/projects/123456789012/apps/example-project", `iapAudience "/projects/123456789012/apps/example-project" must be`},
		{"a backend service by name", "/projects/123456789012/global/backendServices/example-dispatch", "must be the audience IAP signs"},
		{"a project id for its number", "/projects/example-project/global/backendServices/1234567890123456789", "must be the audience IAP signs"},
		{"a trailing slash", dispatchIAPAudience + "/", "must be the audience IAP signs"},
		{"a seed", "/projects/123456789012/global/backendServices/" + seed, "iapAudience holds an nkey seed (not echoed)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			args := dispatchIAPArgs()
			args.IapAudience = pulumi.String(row.audience)
			_, err := runDispatchWithProviders(t, "example-dispatch", args, newGCPProvider, newRandomProvider)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: ")
			require.ErrorContains(t, err, row.want)
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
			requireInputError(t, err, "iapAudience")
		})
	}
}

// iapAudience reaches the plugin over the wire a consumer's SDK uses, and may be another
// resource's output, the backend service's own id built into the audience: the app's Deployment
// renders it and waits for that resource, and under preview, where the backend service is not
// created yet and its id unknown, the component still constructs.
func TestConstructTakesTheIAPAudienceFromAnotherResource(t *testing.T) {
	inputs := dispatchIAPConstructInputs()
	inputs["iapAudience"] = property.New(dispatchIAPAudience).WithDependencies([]resource.URN{upstream})
	m, response, err := constructDispatchWithProviders(t, inputs, false, dispatchIAPConstructProviders())
	require.NoError(t, err)
	url, ok := response.State.GetOk("url")
	require.True(t, ok)
	require.Equal(t, dispatchPublicOrigin, url.AsString())
	require.Equal(t, dispatchIAPAudience, appEnv(t, m)["DISPATCH_IAP_AUDIENCE"])
	require.Contains(t, m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch").RPC.GetDependencies(), string(upstream),
		"the app waits for the resource the audience comes from")

	preview := dispatchIAPConstructInputs()
	preview["iapAudience"] = property.New(property.Computed).WithDependencies([]resource.URN{upstream})
	_, _, err = constructDispatchWithProviders(t, preview, true, dispatchIAPConstructProviders())
	require.NoError(t, err)
}

// directoryAccount and directoryAdmin reach the plugin over the wire a consumer's SDK uses, and may
// be other resources' outputs: the account as the export of the program that declares it, the
// delegated user as secret configuration. The app's Deployment renders both, waits for the
// resource each comes from, and keeps the delegated user secret; under preview, where both are
// unknown, the component still constructs.
func TestConstructTakesTheDirectoryInputsFromAnotherResource(t *testing.T) {
	account := resource.URN("urn:pulumi:s::p::pulumi:pulumi:StackReference::directory-signer")
	inputs := dispatchIAPConstructInputs()
	inputs["iapAudience"] = property.New(dispatchIAPAudience)
	inputs["directoryAccount"] = property.New(dispatchDirectoryAccount).WithDependencies([]resource.URN{account})
	inputs["directoryAdmin"] = property.New(dispatchDirectoryAdmin).WithSecret(true).WithDependencies([]resource.URN{upstream})
	m, _, err := constructDispatchWithProviders(t, inputs, false, dispatchIAPConstructProviders())
	require.NoError(t, err)
	env := appEnv(t, m)
	require.Equal(t, dispatchDirectoryAccount, env["DISPATCH_DIRECTORY_ACCOUNT"])
	require.Equal(t, "<secret>", env["DISPATCH_DIRECTORY_ADMIN"], "the delegated user stays secret in the rendered pod")
	dependencies := m.Named(t, "kubernetes:apps/v1:Deployment", "example-dispatch").RPC.GetDependencies()
	require.Contains(t, dependencies, string(account), "the app waits for the resource the account comes from")
	require.Contains(t, dependencies, string(upstream), "the app waits for the resource the delegated user comes from")

	preview := dispatchIAPConstructInputs()
	preview["iapAudience"] = property.New(dispatchIAPAudience)
	preview["directoryAccount"] = property.New(property.Computed).WithDependencies([]resource.URN{account})
	preview["directoryAdmin"] = property.New(property.Computed).WithSecret(true).WithDependencies([]resource.URN{upstream})
	_, _, err = constructDispatchWithProviders(t, preview, true, dispatchIAPConstructProviders())
	require.NoError(t, err)
}

// dispatchIAPConstructInputs are dispatchIAPArgs as a consumer's SDK sends them, less iapAudience,
// which each test sends its own way: no input of the tailnet group, signInDomain alone of the
// sign-in group, publicOrigin, the two Directory inputs, and the NATS hub in the pods' own cluster.
func dispatchIAPConstructInputs() map[string]property.Value {
	inputs := dispatchConstructInputs()
	for _, group := range [][]dispatchInput{dispatchSandboxTailnetGroup, dispatchSandboxSignInGroup} {
		for _, in := range group {
			delete(inputs, in.input)
		}
	}
	inputs["signInDomain"] = property.New("example.com")
	inputs["publicOrigin"] = property.New(dispatchPublicOrigin)
	inputs["directoryAccount"] = property.New(dispatchDirectoryAccount)
	inputs["directoryAdmin"] = property.New(dispatchDirectoryAdmin)
	inputs["natsUrl"] = property.New(inClusterNatsURL)
	inputs["natsSeedSecret"] = property.New("projects/example-project/secrets/example-nats-seed")
	return inputs
}

// dispatchIAPConstructProviders are the providers a program with no front door declares: no
// tailscale provider, since nothing of the tailnet is built.
func dispatchIAPConstructProviders() map[tokens.Package]p.ProviderReference {
	providers := dispatchConstructProviders()
	delete(providers, "tailscale")
	return providers
}
