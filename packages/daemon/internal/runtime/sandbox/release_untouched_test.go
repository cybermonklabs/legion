package sandbox

import (
	"errors"
	"testing"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A release whose read of the issue's Sandbox fails after authorize answered that the pod goes has
// changed nothing: the Sandbox still runs its pod, and the failure says so
// (runtime.PodUntouchedError), which ends the release's record that its teardown took the pod (the
// daemon's releaseIssue). The controls are a failure of the write that sets the Sandbox Suspended,
// which may have been applied though it failed, and a failure once the release has set the Sandbox
// Suspended — here its delete — which say no such thing, since the pod may be going.
func TestAReleaseThatFailsBeforeItTouchesThePodSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verb      string
		untouched bool
		mode      string
	}{
		{name: "the Sandbox's read fails", verb: "get", untouched: true, mode: modeRunning},
		{name: "control: the write that suspends the Sandbox fails", verb: "patch", mode: modeRunning},
		{name: "control: the Sandbox's delete fails once it is Suspended", verb: "delete", mode: modeSuspended},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(workerSpec(t))
			name := loc.Sandbox.Name
			g.dyn.PrependReactor(tc.verb, "sandboxes", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
				return true, nil, errors.New("the API did not answer")
			})

			err := g.r.ReleaseIssue(g.ctx, testTree, testTree, authorized)

			var untouched *runtime.PodUntouchedError
			if err == nil || errors.As(err, &untouched) != tc.untouched {
				t.Fatalf("the release = %v, untouched %t; want a failure, untouched %t", err, errors.As(err, &untouched), tc.untouched)
			}
			s := g.sandbox(name)
			if s == nil {
				t.Fatalf("the Sandbox %s is gone, want it standing", name)
			}
			if s.mode() != tc.mode {
				t.Fatalf("the Sandbox %s is %s after the failed release, want %s", name, s.mode(), tc.mode)
			}
		})
	}
}
