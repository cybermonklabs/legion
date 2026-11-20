package gcp_test

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

// Every refusal of the sandbox (refuseDispatchSandbox, and refuseDispatchWithoutFrontDoor, which
// it and publicOrigin share), of the IAP door (refuseDispatchIAP), of signInOutside without
// browser sign-in, and of the sign-in group (refuseDispatchSignInGroup) is a core.InputError
// naming the input to correct, as the package's rule has every refusal be, so a caller finds it
// with errors.As rather than by parsing the message. The messages themselves are the other tests'
// (TestDispatchSandboxRefusesWhatItDoesNotBuild, TestDispatchRefusesWhatTheIAPDoorCannotServe,
// TestDispatchRefusesOutsideInputsItCannotServe, TestDispatchPartialTailnetGroupNamesEveryMissingInput);
// each row here also holds that the message names the input, so a row cannot pass on some other
// refusal.
func TestDispatchModeAndSignInRefusalsAreInputErrors(t *testing.T) {
	with := func(base func() *gcp.DispatchArgs, edit func(*gcp.DispatchArgs)) func() *gcp.DispatchArgs {
		return func() *gcp.DispatchArgs {
			args := base()
			edit(args)
			return args
		}
	}
	publicOriginWithoutSignIn := func() *gcp.DispatchArgs {
		args := dispatchPublicOriginArgs()
		args.SignInClientId, args.SignInClientSecret, args.SignInDomain = "", nil, ""
		return args
	}
	type row struct {
		name, input string
		args        func() *gcp.DispatchArgs
	}
	var rows []row
	// The sandbox refuses each sign-in input for being sent at all.
	for _, in := range dispatchSandboxSignInGroup {
		rows = append(rows, row{"sandbox " + in.input, in.input, with(dispatchSandboxArgs, in.set)})
	}
	rows = append(rows, []row{
		{"sandbox signInOutside", "signInOutside", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.SignInOutside = true })},
		{"sandbox outsideOrigin", "outsideOrigin", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.OutsideOrigin = dispatchOutsideOrigin })},
		{"sandbox allowedNamespaces empty", "allowedNamespaces", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.AllowedNamespaces = nil })},
		// What any Dispatch without a front door cannot make good, in the sandbox and at publicOrigin.
		{"sandbox natsInCluster unset", "natsInCluster", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.NatsInCluster = nil })},
		{"sandbox frontDoorReplicas", "frontDoorReplicas", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.FrontDoorReplicas = pulumi.IntRef(2) })},
		{"sandbox frontDoorPdbMinAvailable", "frontDoorPdbMinAvailable", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.FrontDoorPdbMinAvailable = pulumi.IntRef(1) })},
		{"publicOrigin natsInCluster false", "natsInCluster", with(dispatchPublicOriginArgs, func(a *gcp.DispatchArgs) { a.NatsInCluster = pulumi.BoolRef(false) })},
		// The IAP door.
		{"iapAudience in the sandbox", "iapAudience", with(dispatchSandboxArgs, func(a *gcp.DispatchArgs) { a.IapAudience = pulumi.String(dispatchIAPAudience) })},
		{"iapAudience behind the front door", "iapAudience", with(dispatchArgs, func(a *gcp.DispatchArgs) { a.IapAudience = pulumi.String(dispatchIAPAudience) })},
		{"iapAudience beside signInClientId", "signInClientId", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) { a.SignInClientId = "example-client-id" })},
		{"iapAudience beside signInClientSecret", "signInClientSecret", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) {
			a.SignInClientSecret = pulumi.String("example-client-secret")
		})},
		{"iapAudience beside signInOutside", "signInOutside", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) { a.SignInOutside = true })},
		{"iapAudience beside outsideOrigin", "outsideOrigin", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) { a.OutsideOrigin = dispatchOutsideOrigin })},
		{"iapAudience without signInDomain", "signInDomain", with(dispatchIAPArgs, func(a *gcp.DispatchArgs) { a.SignInDomain = "" })},
		// signInOutside without browser sign-in to extend.
		{"signInOutside beside the front door with no Google sign-in", "signInOutside",
			with(dispatchTailnetWithoutSignInArgs, func(a *gcp.DispatchArgs) { a.SignInOutside = true })},
		{"signInOutside beside publicOrigin with no Google sign-in", "signInOutside",
			with(publicOriginWithoutSignIn, func(a *gcp.DispatchArgs) { a.SignInOutside = true })},
		// The sign-in group set in part.
		{"sign-in group without signInClientId", "signInClientId", with(dispatchArgs, func(a *gcp.DispatchArgs) { a.SignInClientId = "" })},
		{"sign-in group with an uppercase signInDomain", "signInDomain", with(dispatchArgs, func(a *gcp.DispatchArgs) { a.SignInDomain = "Example.com" })},
		{"sign-in group without signInClientSecret", "signInClientSecret", with(dispatchArgs, func(a *gcp.DispatchArgs) { a.SignInClientSecret = nil })},
	}...)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := gcp.ResolveDispatch(row.args())
			require.ErrorContains(t, err, row.input, "the refusal names the input")
			var refused *core.InputError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, row.input, refused.Input)
		})
	}
}
