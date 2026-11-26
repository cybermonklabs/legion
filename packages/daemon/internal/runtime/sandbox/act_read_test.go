package sandbox

// GPT-6.1 Sol's point 2 on #5285 at fa9deb04 and the Legion reviewer's point A, which
// the spec's architect ruled fixes inside R7: the read a sweep makes of a recorded replacement
// before it forgets the record (runtime.ActReader) takes the pod's launch turn first, and answers
// ErrTurnBusy without deciding anything while a launch or a replacement holds it. Under the turn
// it reads the Sandbox and the pod from the API server: a store whose feed fails holds what it last
// heard, a Sandbox Running over the pod after its suspension landed among it, and a record forgotten
// on that read would lose the notice of what the suspension took. The control is the pod running as
// the Sandbox's own, which is an act not made.

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// readAct is what ReadAct answers of the pod-taking act on the pod of loc's Sandbox, and whether it
// called decide.
func readAct(g *rig, pod string) (runtime.ActState, bool, error) {
	g.t.Helper()
	state, decided := runtime.ActUncertain, false
	err := g.r.ReadAct(g.ctx, testTree, runtime.PodAct(pod), func(_ context.Context, read runtime.ActState) error {
		state, decided = read, true
		return nil
	})
	return state, decided, err
}

func TestAReplacementsRecordIsReadUnderItsPodsTurnFromTheAPIServer(t *testing.T) {
	t.Run("a launch holds the pod's launch turn", func(t *testing.T) {
		g := newRig(t, nil)
		loc := g.spawn(workerSpec(t))
		release, err := g.r.lockPod(g.ctx, loc.Sandbox.Name)
		if err != nil {
			t.Fatalf("take the pod's launch turn: %v", err)
		}
		defer release()
		if _, decided, err := readAct(g, loc.Sandbox.PodUID); !errors.Is(err, runtime.ErrTurnBusy) || decided {
			t.Fatalf("ReadAct under a held launch turn answered %v and decided=%t, want ErrTurnBusy and nothing decided", err, decided)
		}
	})
	t.Run("the stores' feeds fail, and the Sandbox was suspended over the pod since", func(t *testing.T) {
		g := newRig(t, nil, withBreakableFeeds())
		loc := g.spawn(workerSpec(t))
		g.refuseFeeds(errors.New("the api server refuses the watch"))
		testwait.Eventually(t, "the stores' feeds to fail", func() bool { return g.r.sandboxFeed.check() != nil && g.r.podFeed.check() != nil })
		if _, err := g.dyn.Resource(sandboxGVR).Namespace(testNamespace).Patch(g.ctx, loc.Sandbox.Name, types.MergePatchType,
			[]byte(`{"spec":{"operatingMode":"`+modeSuspended+`"}}`), metav1.PatchOptions{}); err != nil {
			t.Fatalf("suspend the Sandbox through the API: %v", err)
		}
		if s, _ := g.r.storedSandbox(loc.Sandbox.Name); s == nil || s.mode() != modeRunning {
			t.Fatalf("the Sandbox store holds %+v, want the Running copy it last heard, or this case proves nothing", s)
		}
		state, decided, err := readAct(g, loc.Sandbox.PodUID)
		if err != nil || !decided || state != runtime.ActMade {
			t.Fatalf("ReadAct answered %v, decided=%t, state %d; want the act made, as the API server holds the Sandbox Suspended", err, decided, state)
		}
	})
	t.Run("control: the pod runs as the Sandbox's own", func(t *testing.T) {
		g := newRig(t, nil)
		loc := g.spawn(workerSpec(t))
		state, decided, err := readAct(g, loc.Sandbox.PodUID)
		if err != nil || !decided || state != runtime.ActNotMade {
			t.Fatalf("ReadAct answered %v, decided=%t, state %d; want the act not made", err, decided, state)
		}
	})
}
