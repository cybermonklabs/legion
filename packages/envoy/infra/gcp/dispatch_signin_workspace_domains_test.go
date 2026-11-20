package gcp_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

// dispatchWorkspaceDomains is signInWorkspaceDomains as a consumer sets it: the other domains the
// Workspace owns beside signInDomain (example.com in the default fixture).
var dispatchWorkspaceDomains = []string{"example.net", "example.org"}

// withWorkspaceDomains is base with signInWorkspaceDomains set to domains.
func withWorkspaceDomains(base func() *gcp.DispatchArgs, domains ...string) func() *gcp.DispatchArgs {
	return func() *gcp.DispatchArgs {
		args := base()
		args.SignInWorkspaceDomains = domains
		return args
	}
}

// With the sign-in inputs set, signInWorkspaceDomains renders DISPATCH_SIGNIN_WORKSPACE_DOMAINS,
// its entries comma-joined in the order given, as Dispatch reads them: right after
// DISPATCH_SIGNIN_DOMAIN, the domain they stand beside, or after DISPATCH_SIGNIN_OUTSIDE when
// signInOutside is set. An internationalized entry renders as the consumer wrote it, in Unicode or
// as its A-labels, since Dispatch takes both. That an unset list renders nothing in any mode is
// TestDispatchPodWithoutOutsideInputsIsUnchanged's: every committed pod golden is unchanged.
func TestDispatchSignInWorkspaceDomainsRenderWithSignIn(t *testing.T) {
	for _, row := range []struct {
		name, after, value string
		args               func() *gcp.DispatchArgs
	}{
		{"beside the front door", "DISPATCH_SIGNIN_DOMAIN", "example.net,example.org", withWorkspaceDomains(dispatchArgs, dispatchWorkspaceDomains...)},
		{"beside publicOrigin", "DISPATCH_SIGNIN_DOMAIN", "example.net,example.org", withWorkspaceDomains(dispatchPublicOriginArgs, dispatchWorkspaceDomains...)},
		{"beside passkey sign-in", "DISPATCH_SIGNIN_DOMAIN", "example.net,example.org", withWorkspaceDomains(dispatchPasskeyArgs, dispatchWorkspaceDomains...)},
		{"beside signInOutside", "DISPATCH_SIGNIN_OUTSIDE", "example.net,example.org", withWorkspaceDomains(dispatchOutsideArgs, dispatchWorkspaceDomains...)},
		{"one entry", "DISPATCH_SIGNIN_DOMAIN", "example.net", withWorkspaceDomains(dispatchArgs, "example.net")},
		{"internationalized entries", "DISPATCH_SIGNIN_DOMAIN", "bücher.example,xn--bcher-kva.example.net",
			withWorkspaceDomains(dispatchArgs, "bücher.example", "xn--bcher-kva.example.net")},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			require.Equal(t, row.value, appEnv(t, m)["DISPATCH_SIGNIN_WORKSPACE_DOMAINS"])
			requireRightAfter(t, m, row.after, "DISPATCH_SIGNIN_WORKSPACE_DOMAINS")
		})
	}
}

// Each entry is held to the rule Dispatch holds a DISPATCH_SIGNIN_WORKSPACE_DOMAINS entry to, so a
// bad one fails the preview rather than the pod's boot: lowercase, taken by IDNA lookup, and then
// a DNS name of two labels or more, each of at most 63 characters, at most 253 in all. These are
// taken; TestDispatchRefusesASignInWorkspaceDomainsItCannotServe holds the refusals.
func TestDispatchSignInWorkspaceDomainsTakeEveryDomainDispatchTakes(t *testing.T) {
	for _, domain := range []string{
		"example.net",
		"1example.net",
		"a.b.example.net",
		strings.Repeat("a", 63) + ".example.net",
		"abc--d.example.net",
		"xn--bcher-kva.example",
		"bücher.example",
	} {
		t.Run(domain, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(withWorkspaceDomains(dispatchArgs, domain)())
			require.NoError(t, err)
		})
	}
}

// signInWorkspaceDomains is refused before anything registers wherever Dispatch could not read it,
// and an entry Dispatch would refuse at boot is refused here, each refusal a core.InputError naming
// signInWorkspaceDomains as the schema does, an entry's refusal naming the entry by its index in
// its message. Dispatch reads the list only with Google sign-in, so it is refused without the
// sign-in inputs (the clause naming each one still unset), in the sandbox, and beside iapAudience,
// which replaces Google sign-in. clusterOnly refuses it as it refuses every input that configures
// Dispatch (TestDispatchClusterOnlyRefusesEveryDispatchInput), and a seed in an entry is refused
// with every other plain input's (TestDispatchRefusesAnNKeySeedInAPlainInput).
func TestDispatchRefusesASignInWorkspaceDomainsItCannotServe(t *testing.T) {
	const (
		needsSignIn = "signInWorkspaceDomains needs browser sign-in: it names the other domains of the Google Workspace that signInDomain names, " +
			"which Dispatch reads only with Google sign-in; set the sign-in inputs, or unset signInWorkspaceDomains " +
			"(the sign-in inputs are set together or not at all; unset: signInClientId, signInClientSecret, signInDomain)"
		inSandbox = "signInWorkspaceDomains configures browser sign-in, which needs an https origin, the tailnet inputs' or publicOrigin: without either " +
			"Dispatch is reached at an in-cluster http:// address, which no OAuth provider takes as a redirect origin, so no browser could sign in; " +
			"unset signInWorkspaceDomains, or set the tailnet inputs or publicOrigin"
		besideIAP = "signInWorkspaceDomains configures Google sign-in, which iapAudience replaces: IAP signs a person in and Dispatch names them from " +
			"IAP's signed header, so it runs no sign-in of its own; unset signInWorkspaceDomains, or unset iapAudience"
	)
	notDomain := func(index int, value string) string {
		return fmt.Sprintf("signInWorkspaceDomains[%d] %q must be a lowercase domain name that IDNA lookup takes, such as example.net or bücher.example, "+
			"of two labels or more, each of at most 63 characters, and at most 253 characters in all, "+
			"as Dispatch requires of each DISPATCH_SIGNIN_WORKSPACE_DOMAINS entry", index, value)
	}
	publicOriginWithoutSignIn := func() *gcp.DispatchArgs {
		args := dispatchPublicOriginArgs()
		args.SignInClientId, args.SignInClientSecret, args.SignInDomain = "", nil, ""
		return args
	}
	tooLong := strings.Repeat(strings.Repeat("a", 63)+".", 4) + "example"
	for _, row := range []struct {
		name, input, want string
		args              func() *gcp.DispatchArgs
	}{
		{"beside the front door with no Google sign-in", "signInWorkspaceDomains", needsSignIn,
			withWorkspaceDomains(dispatchTailnetWithoutSignInArgs, dispatchWorkspaceDomains...)},
		{"beside publicOrigin with no Google sign-in", "signInWorkspaceDomains", needsSignIn,
			withWorkspaceDomains(publicOriginWithoutSignIn, dispatchWorkspaceDomains...)},
		{"in a sandbox", "signInWorkspaceDomains", inSandbox, withWorkspaceDomains(dispatchSandboxArgs, dispatchWorkspaceDomains...)},
		{"beside iapAudience", "signInWorkspaceDomains", besideIAP, withWorkspaceDomains(dispatchIAPArgs, dispatchWorkspaceDomains...)},
		{"an empty entry", "signInWorkspaceDomains", "signInWorkspaceDomains[1] is empty: each entry is a domain the Workspace owns",
			withWorkspaceDomains(dispatchArgs, "example.net", "")},
		{"an uppercase entry", "signInWorkspaceDomains", notDomain(0, "Example.net"), withWorkspaceDomains(dispatchArgs, "Example.net")},
		{"a single label", "signInWorkspaceDomains", notDomain(0, "example"), withWorkspaceDomains(dispatchArgs, "example")},
		{"two domains in one entry", "signInWorkspaceDomains", notDomain(0, "example.net,example.org"),
			withWorkspaceDomains(dispatchArgs, "example.net,example.org")},
		{"an address", "signInWorkspaceDomains", notDomain(0, "person@example.net"), withWorkspaceDomains(dispatchArgs, "person@example.net")},
		{"an origin", "signInWorkspaceDomains", notDomain(0, "https://example.net"), withWorkspaceDomains(dispatchArgs, "https://example.net")},
		{"a trailing dot", "signInWorkspaceDomains", notDomain(0, "example.net."), withWorkspaceDomains(dispatchArgs, "example.net.")},
		{"a label of 64 characters", "signInWorkspaceDomains", notDomain(0, strings.Repeat("a", 64)+".example.net"),
			withWorkspaceDomains(dispatchArgs, strings.Repeat("a", 64)+".example.net")},
		{"more than 253 characters", "signInWorkspaceDomains", notDomain(0, tooLong), withWorkspaceDomains(dispatchArgs, tooLong)},
		{"a fake A-label", "signInWorkspaceDomains", notDomain(0, "xn--a.example.net"), withWorkspaceDomains(dispatchArgs, "xn--a.example.net")},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, row.input, refused.Input)
			m, err := runDispatch(t, row.args(), true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
			require.NotContains(t, m.Types(), "kubernetes:apps/v1:Deployment", "nothing of Dispatch's registers")
		})
	}
}

// signInWorkspaceDomains reaches the pod over the wire a consumer's SDK sends it on, through the
// plugin's own Construct, in a preview: a plain list of strings, decoded and rendered as the
// direct path renders it.
func TestConstructRendersSignInWorkspaceDomainsInAPreview(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["signInWorkspaceDomains"] = property.New(property.NewArray([]property.Value{property.New("example.net"), property.New("example.org")}))
	m, _, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
	require.Equal(t, "example.net,example.org", appEnv(t, m)["DISPATCH_SIGNIN_WORKSPACE_DOMAINS"])
	requireRightAfter(t, m, "DISPATCH_SIGNIN_DOMAIN", "DISPATCH_SIGNIN_WORKSPACE_DOMAINS")
}
