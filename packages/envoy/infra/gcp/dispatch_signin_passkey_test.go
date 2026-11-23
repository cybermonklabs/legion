package gcp_test

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

// dispatchPasskeyGolden is the app's pod with outsideOrigin and signInPasskey set.
const dispatchPasskeyGolden = "dispatch-pod-with-passkey-sign-in.golden"

// dispatchPasskeyArgs is the default fixture, sign-in on, with the second origin and passkey
// sign-in at it.
func dispatchPasskeyArgs() *gcp.DispatchArgs {
	args := dispatchArgs()
	args.OutsideOrigin = dispatchOutsideOrigin
	args.SignInPasskey = true
	return args
}

// With signInPasskey and outsideOrigin set the app's container carries DISPATCH_SIGNIN_PASSKEY=1
// right after DISPATCH_OUTSIDE_ORIGIN, the origin whose host a passkey is made for. Nothing else
// moves: the listener's and the front door's pods, the front door's serve configuration and the
// registration set are the defaults' own.
func TestDispatchPodWithPasskeySignInRendersTheSetting(t *testing.T) {
	m, err := runDispatch(t, dispatchPasskeyArgs(), true)
	require.NoError(t, err)

	requirePodGolden(t, m, "example-dispatch", dispatchPasskeyGolden)
	require.Equal(t, "1", appEnv(t, m)["DISPATCH_SIGNIN_PASSKEY"])
	requireRightAfter(t, m, "DISPATCH_SERVER_URL", "DISPATCH_OUTSIDE_ORIGIN")
	requireRightAfter(t, m, "DISPATCH_OUTSIDE_ORIGIN", "DISPATCH_SIGNIN_PASSKEY")

	requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
	requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
	require.Equal(t, readGolden(t, "dispatch-serve.golden"), serveConfig(t, m))
	require.Equal(t, readGolden(t, "dispatch-registrations.golden"), dispatchRegistrations(m))
}

// signInPasskey is taken wherever outsideOrigin is taken with browser sign-in on: beside the front
// door and beside publicOrigin. Each renders the setting right after the second origin. Beside
// signInOutside it is refused (TestDispatchRefusesASignInPasskeyItCannotServe).
func TestDispatchSignInPasskeyRendersWhereverOutsideOriginIsTaken(t *testing.T) {
	withPasskey := func(base func() *gcp.DispatchArgs) func() *gcp.DispatchArgs {
		return func() *gcp.DispatchArgs {
			args := base()
			args.OutsideOrigin = dispatchOutsideOrigin
			args.SignInPasskey = true
			return args
		}
	}
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
	}{
		{"beside the front door", withPasskey(dispatchArgs)},
		{"beside publicOrigin", withPasskey(dispatchPublicOriginArgs)},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			env := appEnv(t, m)
			require.Equal(t, "1", env["DISPATCH_SIGNIN_PASSKEY"])
			require.Equal(t, dispatchOutsideOrigin, env["DISPATCH_OUTSIDE_ORIGIN"])
			requireRightAfter(t, m, "DISPATCH_OUTSIDE_ORIGIN", "DISPATCH_SIGNIN_PASSKEY")
		})
	}
}

// Unset, signInPasskey renders nothing in any mode, with outsideOrigin set or not. That every
// committed pod golden still renders byte for byte as committed is
// TestDispatchPodWithoutOutsideInputsIsUnchanged's.
func TestDispatchSignInPasskeyAbsentWhenUnset(t *testing.T) {
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
	}{
		{"the default", dispatchArgs},
		{"outside sign-in by Google", dispatchOutsideArgs},
		{"tailnet without sign-in", dispatchTailnetWithoutSignInArgs},
		{"publicOrigin", dispatchPublicOriginArgs},
		{"the IAP door", dispatchIAPArgs},
		{"sandbox", dispatchSandboxArgs},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			require.NotContains(t, appEnv(t, m), "DISPATCH_SIGNIN_PASSKEY")
		})
	}
}

// signInPasskey is refused by its own name, as a core.InputError naming it, before anything
// registers wherever Dispatch could not serve passkey sign-in: without outsideOrigin, the origin
// Dispatch serves it at and whose host a passkey is made for; without browser sign-in, since a
// project owner signed in through Google makes the invitation a passkey is enrolled with, the
// refusal naming each sign-in input still unset; beside signInOutside, since passkey sign-in
// replaces the Google sign-in of people from outside that signInOutside turns on; beside
// iapAudience, where IAP signs a person in, outsideOrigin set or not; and in a sandbox, which
// serves no https origin at all. clusterOnly refuses it as it refuses every input that configures
// Dispatch (TestDispatchClusterOnlyRefusesEveryDispatchInput).
func TestDispatchRefusesASignInPasskeyItCannotServe(t *testing.T) {
	const (
		needsOrigin = "signInPasskey needs outsideOrigin: Dispatch serves passkey sign-in at that origin alone, and a passkey is made for its host; " +
			"set outsideOrigin, or unset signInPasskey"
		besideIAP = "signInPasskey is refused beside iapAudience: IAP signs a person in and Dispatch names them from IAP's signed header, " +
			"so it runs no passkey sign-in of its own; unset signInPasskey, or unset iapAudience"
		inSandbox = "signInPasskey needs outsideOrigin, which needs the tailnet inputs or publicOrigin: without either the component builds the sandbox, " +
			"whose own origin is an in-cluster http:// address where browser sign-in is refused; unset signInPasskey, or set the tailnet inputs or publicOrigin"
		needsSignIn = "signInPasskey needs browser sign-in: a project owner, signed in through Google, makes the invitation a person from outside enrolls " +
			"a passkey with, so without it nobody could invite anyone; set the sign-in inputs, or unset signInPasskey " +
			"(the sign-in inputs are set together or not at all; unset: signInClientId, signInClientSecret, signInDomain)"
		besideOutside = "signInPasskey is refused beside signInOutside: passkey sign-in replaces the Google sign-in of people from outside the company " +
			"that signInOutside turns on; unset signInOutside, or unset signInPasskey"
	)
	with := func(base func() *gcp.DispatchArgs, edit func(*gcp.DispatchArgs)) func() *gcp.DispatchArgs {
		return func() *gcp.DispatchArgs {
			args := base()
			args.SignInPasskey = true
			edit(args)
			return args
		}
	}
	alone := func(*gcp.DispatchArgs) {}
	withOrigin := func(a *gcp.DispatchArgs) { a.OutsideOrigin = dispatchOutsideOrigin }
	// publicOrigin's fixture with its sign-in inputs unset: a Dispatch at publicOrigin that runs no
	// browser sign-in, which the component takes without signInPasskey.
	publicOriginWithoutSignIn := func() *gcp.DispatchArgs {
		args := dispatchPublicOriginArgs()
		args.SignInClientId, args.SignInClientSecret, args.SignInDomain = "", nil, ""
		return args
	}
	for _, row := range []struct {
		name string
		args func() *gcp.DispatchArgs
		want string
	}{
		{"without outsideOrigin beside the front door", with(dispatchArgs, alone), needsOrigin},
		{"without outsideOrigin beside the front door with no Google sign-in", with(dispatchTailnetWithoutSignInArgs, alone), needsOrigin},
		{"without outsideOrigin beside publicOrigin", with(dispatchPublicOriginArgs, alone), needsOrigin},
		{"with outsideOrigin beside the front door with no Google sign-in", with(dispatchTailnetWithoutSignInArgs, withOrigin), needsSignIn},
		{"with outsideOrigin beside publicOrigin with no Google sign-in", with(publicOriginWithoutSignIn, withOrigin), needsSignIn},
		{"beside signInOutside", with(dispatchOutsideArgs, alone), besideOutside},
		{"beside iapAudience", with(dispatchIAPArgs, alone), besideIAP},
		{"beside iapAudience with outsideOrigin", with(dispatchIAPArgs, withOrigin), besideIAP},
		{"in a sandbox", with(dispatchSandboxArgs, alone), inSandbox},
		{"in a sandbox with outsideOrigin", with(dispatchSandboxArgs, withOrigin), inSandbox},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.EqualError(t, err, row.want)
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "signInPasskey", refused.Input)
			m, err := runDispatch(t, row.args(), true)
			require.ErrorContains(t, err, "envoy-infra:gcp:Dispatch example-dispatch: "+row.want)
			require.NotContains(t, m.Types(), "kubernetes:apps/v1:Deployment", "nothing of Dispatch's registers")
		})
	}
}

// signInPasskey reaches the pod over the wire a consumer's SDK sends it on, through the plugin's
// own Construct, in a preview: a plain boolean beside outsideOrigin's plain string, decoded and
// rendered as the direct path renders it.
func TestConstructRendersSignInPasskeyInAPreview(t *testing.T) {
	inputs := dispatchConstructBaseInputs()
	inputs["outsideOrigin"] = property.New(dispatchOutsideOrigin)
	inputs["signInPasskey"] = property.New(true)
	m, _, err := constructDispatch(t, inputs, true)
	require.NoError(t, err)
	env := appEnv(t, m)
	require.Equal(t, "1", env["DISPATCH_SIGNIN_PASSKEY"])
	require.Equal(t, dispatchOutsideOrigin, env["DISPATCH_OUTSIDE_ORIGIN"])
	requireRightAfter(t, m, "DISPATCH_OUTSIDE_ORIGIN", "DISPATCH_SIGNIN_PASSKEY")
}
