package gcp_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// appDrain is the app pod's drain as rendered: its terminationGracePeriodSeconds, and the seconds
// its envoy-dispatch container's preStop hook sleeps before the kubelet signals it.
func appDrain(t *testing.T, m *pulumitest.Mocks) (grace, wait float64) {
	t.Helper()
	template := deploymentPodTemplate(t, m, "example-dispatch")
	return template["spec"].(map[string]any)["terminationGracePeriodSeconds"].(float64), preStopSeconds(t, podContainer(t, template, "envoy-dispatch"))
}

// The app pod's preStop wait and its grace period are preStopSeconds and
// terminationGracePeriodSeconds, 5 and 35 seconds when unset. The kubelet spends the wait and then
// the server's 30-second shutdown inside the one grace period, so a grace period of exactly the
// wait plus 30 is taken, and one second less is refused, set or not, naming
// terminationGracePeriodSeconds; a wait under 1 second is refused, and so is one that overflows
// once those 30 seconds are added, which would wrap below every grace period. Each refusal comes
// before anything registers. The listener's and the front door's pods keep their own drain
// whatever the two inputs hold.
func TestDispatchDrainInputsRenderAndRefuse(t *testing.T) {
	drain := func(preStop, grace *int) func() *gcp.DispatchArgs {
		return editedDispatchArgs(dispatchArgs, func(a *gcp.DispatchArgs) {
			a.PreStopSeconds, a.TerminationGracePeriodSeconds = preStop, grace
		})
	}
	for _, row := range []struct {
		name        string
		args        func() *gcp.DispatchArgs
		grace, wait float64
	}{
		{"unset", drain(nil, nil), 35, 5},
		{"a longer wait and a grace period of exactly the wait and the shutdown", drain(pulumi.IntRef(20), pulumi.IntRef(50)), 50, 20},
		{"the shortest wait and a grace period of exactly it and the shutdown", drain(pulumi.IntRef(1), pulumi.IntRef(31)), 31, 1},
		{"a longer grace period alone", drain(nil, pulumi.IntRef(60)), 60, 5},
		{"a wait well inside a long grace period", drain(pulumi.IntRef(10), pulumi.IntRef(120)), 120, 10},
	} {
		t.Run(row.name, func(t *testing.T) {
			m, err := runDispatch(t, row.args(), true)
			require.NoError(t, err)
			grace, wait := appDrain(t, m)
			require.Equal(t, row.grace, grace, "terminationGracePeriodSeconds")
			require.Equal(t, row.wait, wait, "the preStop sleep")
			requirePodGolden(t, m, "example-dispatch-listener", "dispatch-listener-pod.golden")
			requirePodGolden(t, m, "example-dispatch-tailnet", "dispatch-tailnet-pod.golden")
		})
	}

	short := func(grace, preStop int) string {
		least := preStop + 30
		return fmt.Sprintf("terminationGracePeriodSeconds %d is under %d, preStopSeconds %d plus the 30 seconds envoy-dispatch's shutdown takes: "+
			"the kubelet spends the wait and then the shutdown inside the one grace period, so it would kill the server mid-shutdown with a write "+
			"still waiting on a lock; set terminationGracePeriodSeconds to at least %d, or leave both unset for 5 and 35", grace, least, preStop, least)
	}
	noWait := func(preStop int) string {
		return fmt.Sprintf("preStopSeconds %d is below 1: envoy-dispatch keeps answering for this wait while its pod's removal from routing reaches "+
			"every proxy, and with none a request still sent there finds the server gone; leave it unset for 5", preStop)
	}
	for _, row := range []struct {
		name, input string
		args        func() *gcp.DispatchArgs
		want        string
	}{
		{"a grace period one second under the wait and the shutdown", "terminationGracePeriodSeconds", drain(pulumi.IntRef(20), pulumi.IntRef(49)), short(49, 20)},
		{"a grace period one second under the default wait and the shutdown", "terminationGracePeriodSeconds", drain(nil, pulumi.IntRef(34)), short(34, 5)},
		{"a longer wait with the default grace period", "terminationGracePeriodSeconds", drain(pulumi.IntRef(10), nil), short(35, 10)},
		{"a negative grace period", "terminationGracePeriodSeconds", drain(nil, pulumi.IntRef(-1)), short(-1, 5)},
		{"no wait", "preStopSeconds", drain(pulumi.IntRef(0), nil), noWait(0)},
		{"a negative wait", "preStopSeconds", drain(pulumi.IntRef(-5), pulumi.IntRef(60)), noWait(-5)},
		{"a wait that overflows with the shutdown", "preStopSeconds", drain(pulumi.IntRef(math.MaxInt), pulumi.IntRef(math.MaxInt)),
			fmt.Sprintf("preStopSeconds %d overflows a count of seconds once the 30 seconds envoy-dispatch's shutdown takes are added to it, "+
				"so no grace period could hold it; leave it unset for 5", math.MaxInt)},
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
}
