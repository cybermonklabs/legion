package gcp_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
)

// dispatchSwitchModes is each mode that runs Dispatch: a tailnet Dispatch with browser sign-in and
// without it, and the sandbox. defaults is that args are the defaults' own, whose listener and
// front door pods have committed goldens.
var dispatchSwitchModes = []struct {
	name     string
	args     func() *gcp.DispatchArgs
	defaults bool
}{
	{"tailnet with sign-in", dispatchArgs, true},
	{"tailnet without sign-in", dispatchTailnetWithoutSignInArgs, false},
	{"sandbox", dispatchSandboxArgs, false},
}

// requireSwitchInputRendersOn holds an on-or-off input of gcp:Dispatch to one rule, in each mode
// of dispatchSwitchModes. Unset, the app carries no variable, so a deployment that leaves it unset
// renders the pod it always had: each committed golden of the app's pod renders byte for byte
// (TestDispatchPodWithoutOutsideInputsIsUnchanged). Set (set turns it on), it renders variable=on
// on the app's container, right after DISPATCH_SERVER_URL. Only envoy-dispatch reads the setting,
// so the listener's and the front door's pods are the defaults' own.
func requireSwitchInputRendersOn(t *testing.T, variable string, set func(*gcp.DispatchArgs)) {
	t.Helper()
	for _, mode := range dispatchSwitchModes {
		t.Run(mode.name, func(t *testing.T) {
			unset, err := runDispatch(t, mode.args(), true)
			require.NoError(t, err)
			require.NotContains(t, appEnv(t, unset), variable)

			args := mode.args()
			set(args)
			m, err := runDispatch(t, args, true)
			require.NoError(t, err)
			require.Equal(t, "on", appEnv(t, m)[variable])
			requireRightAfter(t, m, "DISPATCH_SERVER_URL", variable)
			if mode.defaults {
				requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
				requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
			}
		})
	}
}

// parentStatusRule true renders DISPATCH_PARENT_STATUS_RULE=on on the app's container, right
// after DISPATCH_SERVER_URL, in each mode that runs Dispatch, and unset renders the pod it always
// had (requireSwitchInputRendersOn).
func TestTheParentStatusRuleInputRendersTheSetting(t *testing.T) {
	requireSwitchInputRendersOn(t, "DISPATCH_PARENT_STATUS_RULE", func(a *gcp.DispatchArgs) { a.ParentStatusRule = true })
}
