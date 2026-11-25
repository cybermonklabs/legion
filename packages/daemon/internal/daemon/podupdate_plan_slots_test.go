package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// planSlotsRuntime is the fake runtime with the plan's slots of an issue pod
// (runtime.PlanSlotsDrift): it answers slots, or err where it is set, and counts its calls.
type planSlotsRuntime struct {
	*fake.Runtime
	mu    sync.Mutex
	slots []string
	err   error
	asked []claim.Token
}

func (r *planSlotsRuntime) PodSlotsDrift(_ context.Context, k runtime.Known) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, k.Claim)
	return slices.Clone(r.slots), r.err
}

// The sweep reads an issue's plan once for the issue's pod, not once for each claim in it: the
// pod's other reasons come from PodDrift of each claim, and the slots the plan names from one
// question of the pod (runtime.PlanSlotsDrift), asked through the first claim that records a
// process. A plan that cannot be read costs that sweep the slots reason alone: the reading stays
// podRead with the pod's other reasons, so an outage of the store neither holds a round that the
// configuration opens nor reads the pod as unread, and the failed read is logged as a warning.
func TestTheSweepReadsAnIssuesPlanSlotsOnceForItsPod(t *testing.T) {
	const issue = "LEGION-208"
	image := "image ghcr.io/cybermonklabs/legion-worker@sha256:old, now ghcr.io/cybermonklabs/legion-worker@sha256:new"
	slotsReason := "slots architect, planner, implementer, tester, reviewer, merger, now architect, planner, implementer, tester, reviewer, merger, implementer-2"
	var group []supervise.Claim
	for _, agent := range []claim.Agent{"implementer", "implementer-2", "reviewer"} {
		token := agentClaimToken(t, issue, agent)
		group = append(group, supervise.Claim{Token: token, Locator: &runtime.Locator{Claim: token, Sandbox: &runtime.SandboxLocator{Name: "legion-legion-208", PodUID: "pod-1"}}})
	}
	for _, tc := range []struct {
		name   string
		err    error
		want   []string
		warned bool
	}{
		{name: "the plan read", want: []string{image, slotsReason}},
		{name: "the plan unread", err: errors.New("the database is not answering"), want: []string{image}, warned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &planSlotsRuntime{Runtime: fake.NewRuntime(), slots: []string{slotsReason}, err: tc.err}
			for _, c := range group {
				rt.ScriptPodDrift(c.Token, image)
			}
			var logs strings.Builder
			s := &supervision{runtime: rt, log: slog.New(slog.NewTextHandler(&logs, nil))}

			reasons, reading := s.groupDrift(context.Background(), issue, group)
			if reading != podRead {
				t.Fatalf("the reading = %d, want podRead (%d): the pod was read", reading, podRead)
			}
			if !slices.Equal(reasons, tc.want) {
				t.Errorf("the reasons = %q, want %q", reasons, tc.want)
			}
			if !slices.Equal(rt.asked, []claim.Token{group[0].Token}) {
				t.Errorf("the plan's slots were asked through %v, want once, through %s", rt.asked, group[0].Token)
			}
			if got := len(rt.CallsOf("PodDrift")); got != len(group) {
				t.Errorf("PodDrift was asked %d times, want once for each of the %d claims", got, len(group))
			}
			if warned := strings.Contains(logs.String(), "level=WARN") && strings.Contains(logs.String(), "lacks a slot of its plan"); warned != tc.warned {
				t.Errorf("the failed plan read warned = %v, want %v; logs:\n%s", warned, tc.warned, logs.String())
			}
		})
	}
}
