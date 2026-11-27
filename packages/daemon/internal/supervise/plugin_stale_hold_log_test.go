package supervise

// The stale-plugin hold of sendPending (staleRefusesClaim) keeps a claim's task back for as many
// sweeps as the refresh lane takes to reach the claim, and on OSSV-537 it held the implementer's
// task from 11:46Z to 12:02Z and wrote no line, so the hold was read only by elimination
// (dispatch://INFRA-3916 R3). The hold now writes one line the first time it keeps a delivery
// back, naming the delivery, the contract the claim's process registered on and the daemon's own,
// and no line again for that delivery at the sweeps that ask again. A delivery that takes its
// place is told in a line of its own. The hold itself is unchanged: nothing is sent
// (plugin_refresh_hold_test.go).
//
// The negative control is the gate's line removed, which leaves the hold silent again.

import (
	"log/slog"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// staleHoldLine is the hold's line as an operator reads it: whole, so a reword turns this test red
// rather than leaving a reader of the log looking for words that are gone.
const staleHoldLine = "supervise: the process speaks another plugin contract, so this daemon refuses its every legion call; the task waits for the process the refresh lane's relaunch starts"

func TestATaskHeldForAStalePluginIsLoggedOnce(t *testing.T) {
	h := newHarness(t)
	h.reach(StateIdle)
	// The walk to idle delivers a phase task and works it, so one prompt is already sent.
	sent := len(h.prompts())
	registered := h.claim().PluginContract
	// The daemon this claim's process registered with is gone; this one speaks the next contract
	// and can relaunch onto its plugin, so it refuses every `legion` call of the claim's process.
	h.m.deps.PluginContract, h.m.deps.PluginRelaunchable = refreshContract, true
	if registered == refreshContract {
		t.Fatalf("the claim registered on contract %d, the daemon's own: this test is of a stale claim", registered)
	}
	// wantHeld holds the log to one line for each delivery the hold kept back, in order, each naming
	// its delivery and both contracts, after the claim's agent was sent nothing more.
	wantHeld := func(deliveries ...string) {
		t.Helper()
		h.wantPrompts(sent)
		records := h.records.of(staleHoldLine)
		if len(records) != len(deliveries) {
			t.Fatalf("%q was written %d times, want once for each of the held deliveries %v", staleHoldLine, len(records), deliveries)
		}
		for i, r := range records {
			if r.level != slog.LevelInfo {
				t.Fatalf("%q is %s, want INFO", staleHoldLine, r.level)
			}
			for _, want := range []struct{ key, value string }{
				{"claim", string(h.token)},
				{"delivery", deliveries[i]},
				{"pluginContract", strconv.Itoa(registered)},
				{"daemonApiVersion", strconv.Itoa(refreshContract)},
			} {
				if got, ok := r.attr(want.key); !ok || got.String() != want.value {
					t.Fatalf("line %d of %q carries %s %v (present %v), want %s", i+1, staleHoldLine, want.key, got, ok, want.value)
				}
			}
		}
	}

	h.must(RequestDeliver{Claim: h.token, Task: "implement the plan", ID: "outbox:42", Row: 42,
		Phase: phase.Implementing, Generation: 7, Entry: 3})
	for range 3 {
		h.observe(runtime.Alive) // the sweep, which asks the hold again
	}
	first := h.pending().ID
	wantHeld(first)

	// The workflow stops the phase that task was queued for, which retires it however long it was
	// held (RetireStoppedTask), and the return's own start takes its place: the hold keeps that
	// task back too, and tells it in a line of its own.
	if retired, err := h.m.RetireStoppedTask(h.ctx, phase.Implementing, 7, 3); err != nil || !retired {
		t.Fatalf("the retirement of the held task = %t, %v; want it retired", retired, err)
	}
	h.must(RequestDeliver{Claim: h.token, Task: "carry the implementation on after the wait", ID: "outbox:43", Row: 43,
		Phase: phase.Implementing, Generation: 7, Entry: 4})
	for range 3 {
		h.observe(runtime.Alive)
	}
	second := h.pending().ID
	if second == first {
		t.Fatalf("the claim holds %s again, want the return's own task in its place", first)
	}
	wantHeld(first, second)
}
