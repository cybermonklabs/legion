package gcp_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
)

// signInDomain is held to the rule Dispatch holds DISPATCH_SIGNIN_DOMAIN to at boot
// (cmd/dispatch checkDomainShape, under Google sign-in and under iap identity alike: a lowercase
// DNS name of two labels or more, each of at most 63 characters, at most 253 in all), so a domain
// Dispatch would refuse fails the preview, not the pod's boot. Beside signInPasskey the domain is
// also the one the passkey rule treats as the Workspace's, so a boot refusal there takes outside
// sign-in down with it.
func TestDispatchRefusesASignInDomainDispatchWouldRefuse(t *testing.T) {
	for _, mode := range []struct {
		name string
		args func() *gcp.DispatchArgs
	}{
		{"beside passkey sign-in", dispatchPasskeyArgs},
		{"beside iapAudience", dispatchIAPArgs},
	} {
		for name, domain := range map[string]string{
			"a label of 64 characters": strings.Repeat("a", 64) + ".example.com",
			"more than 253 characters": strings.Repeat(strings.Repeat("a", 62)+".", 4) + "example",
		} {
			t.Run(mode.name+"/"+name, func(t *testing.T) {
				args := mode.args()
				args.SignInDomain = domain
				_, err := gcp.ResolveDispatch(args)
				require.Error(t, err, "Dispatch refuses DISPATCH_SIGNIN_DOMAIN of %d characters at boot", len(domain))
				var refused *core.InputError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, "signInDomain", refused.Input)
			})
		}
	}
}

// A whitespace-only signInClientId is no client: Dispatch trims DISPATCH_SIGNIN_CLIENT_ID and
// refuses the boot without one ("Google sign-in needs ... DISPATCH_SIGNIN_CLIENT_ID ... missing"),
// so the component refuses it, as refuseDispatchSecret refuses a blank signInClientSecret.
func TestDispatchRefusesABlankSignInClientId(t *testing.T) {
	args := dispatchPasskeyArgs()
	args.SignInClientId = " "
	_, err := gcp.ResolveDispatch(args)
	require.Error(t, err, "Dispatch refuses a blank DISPATCH_SIGNIN_CLIENT_ID at boot")
	var refused *core.InputError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "signInClientId", refused.Input)
}
