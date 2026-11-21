package writerhold

import (
	"context"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// refusedProbe dials url as the hold's probe does, a CONNECT the rig's auth service refuses, and
// returns the dial's error.
func refusedProbe(ctx context.Context, url string) error {
	nc, err := natsgo.Connect(url, natsgo.UserInfo("service", "holdtest-probe-token"), natsgo.Name("hold probe"),
		natsgo.Timeout(10*time.Second), natsgo.NoReconnect())
	if err == nil {
		nc.Close()
	}
	return err
}

// bindings records the binding checks a restart asked for, each passed.
type bindings struct {
	mu    sync.Mutex
	pods  []string
	moved []bool
}

func (b *bindings) bind(_ context.Context, pod string, step1, now Varz) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pods = append(b.pods, pod)
	b.moved = append(b.moved, step1.ServerID != now.ServerID)
	return nil
}

// restartOf is the restart of the rig's hub from declared, with the refused probe and binds.
func (r *calloutRig) restartOf(declared Declared, binds *bindings, limit time.Duration) Restart {
	return Restart{Hub: r.hub, Declared: declared, Probe: refusedProbe, Bind: binds.bind, Limit: limit, Retry: 50 * time.Millisecond}
}

// TestHoldAServerThatKeepsItsId shows step 4 stops the hold on a declared server that keeps its
// step-1 id across its pod's eviction, as one whose nats-server never restarted would: the pod is
// deleted and made again, but the process, and with it any work a client left inside it, is the
// same. The restart stops there, after the server before it, naming the pod and the id, and evicts
// no later pod. Its control is the same schedule with the id check left out, under which the
// restart passes all three pods while envoy-nats-1's server keeps its id: the danger. Every pod
// that restarts passes, its exit recorded from its old container (137 read as forced, never
// graceful), its binding check asked with its step-1 and new /varz. It fails when step 4 accepts a
// server's old id, or does not read the restarted server's own /varz.
func TestHoldAServerThatKeepsItsId(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	ctx := rowContext(t)
	r.kube.KeepProcess(1)
	r.kube.SetExitCode(137)

	declared, err := Discover(ctx, r.hub)
	require.NoError(t, err)
	binds := &bindings{}
	control := r.restartOf(declared, binds, 2*time.Minute)
	control.keptIDs = true
	restarted, err := control.Run(ctx, r.caller)
	require.NoError(t, err, "control: with the id check left out, a server that did not restart passes")
	require.Len(t, restarted, 3)
	require.Equal(t, declared[1].Varz.ServerID, restarted[1].Varz.ServerID, "the kept server answers its step-1 id")
	for i, one := range restarted {
		require.Equal(t, holdtest.Pod(i), one.Pod)
		require.True(t, one.Exit.Forced(), "%s: %s", one.Pod, one.Exit)
		require.False(t, one.Exit.Graceful())
		require.Contains(t, one.Exit.String(), "forced")
	}
	require.Equal(t, []string{holdtest.Pod(0), holdtest.Pod(1), holdtest.Pod(2)}, binds.pods)
	require.Equal(t, []bool{true, false, true}, binds.moved)

	r.kube.SetExitCode(0)
	declared, err = Discover(ctx, r.hub)
	require.NoError(t, err)
	evicted := len(r.kube.Evicted())
	restarted, err = r.restartOf(declared, &bindings{}, 2*time.Minute).Run(ctx, r.caller)
	requireRefusal(t, err, StepProof, "pod "+holdtest.Pod(1), declared[1].Varz.ServerID, "kept its step-1 id")
	require.Len(t, restarted, 1, "the restart passed the pod before the kept server")
	require.True(t, restarted[0].Exit.Graceful(), "%s", restarted[0].Exit)
	require.Equal(t, []string{holdtest.Pod(0), holdtest.Pod(1)}, r.kube.Evicted()[evicted:], "no pod after the kept server was evicted")
}

// TestHoldARestartPastItsLimit shows an eviction the disruption budget refuses (429) is sent
// again until the pod's limit, and past it the hold stops as UNKNOWN, with the row still held and
// no pod evicted. Its control is the budget spent for a moment: the refused eviction is sent again,
// accepted once the budget frees, and the restart passes, its exit read as graceful. It fails when
// the restart deletes a pod in place of an eviction (ignoring the budget), gives up at the first
// refusal, or waits past its limit.
func TestHoldARestartPastItsLimit(t *testing.T) {
	r := newCalloutRig(t, holdAwareName)
	ctx := rowContext(t)
	declared, err := Discover(ctx, r.hub)
	require.NoError(t, err)

	r.kube.SpendBudget(true)
	type result struct {
		restarted []Restarted
		err       error
	}
	done := make(chan result, 1)
	go func() {
		restarted, err := r.restartOf(declared, &bindings{}, 2*time.Minute).Run(ctx, r.caller)
		done <- result{restarted, err}
	}()
	testwait.Eventually(t, "the budget refused the first eviction twice", func() bool { return r.kube.Refused() >= 2 })
	r.kube.SpendBudget(false)
	control := <-done
	require.NoError(t, control.err, "control: an eviction the budget refused for a moment is sent again and accepted")
	require.Len(t, control.restarted, 3)
	for _, one := range control.restarted {
		require.True(t, one.Exit.Graceful(), "%s: %s", one.Pod, one.Exit)
		require.Contains(t, one.Exit.String(), "graceful")
	}

	declared, err = Discover(ctx, r.hub)
	require.NoError(t, err)
	r.kube.SpendBudget(true)
	refused, evicted := r.kube.Refused(), len(r.kube.Evicted())
	limit := 2 * time.Second
	started := time.Now()
	restarted, err := r.restartOf(declared, &bindings{}, limit).Run(ctx, r.caller)
	requireUnknown(t, err, StepRestart, "the disruption budget refused pod "+holdtest.Pod(0)+"'s eviction until its limit", "the row stays held")
	require.Empty(t, restarted)
	require.Greater(t, r.kube.Refused()-refused, 1, "the refused eviction was sent again")
	require.Len(t, r.kube.Evicted(), evicted, "no pod was evicted past the budget")
	require.Less(t, time.Since(started), limit+time.Minute, "the restart waited past its limit")
	after, err := Discover(ctx, r.hub)
	require.NoError(t, err)
	for i := range after {
		require.Equal(t, declared[i].Varz.ServerID, after[i].Varz.ServerID, "a server restarted past the budget")
	}
}
