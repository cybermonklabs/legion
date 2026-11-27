package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A phase task whose adoption the shim refused twice is sent again by the sweep of a daemon booted
// over the store after the one that met the refusals, once the refusal's cause is gone
// (dispatch://INFRA-3916 R3). On OSSV-537 two adoptions of the implementer's task were refused on
// a git lock, the daemon restarted, and the task reached the implementer only through a direct
// message; the daemon's log showed the retry was not at fault, and this holds that it stays so. A
// refusal arms nothing, the booted daemon restores the task with its refused sends counted on it,
// and its sweep's observation of the live process in a ready claim sends the task, with no wake
// and no message. A third refusal, the shim's answer as the first two were, is counted on the task
// and charges the claim nothing (R7). Each case runs through the daemon's own parts and store, as
// the newsPathRig cases do.
//
// The negative control is an observe in supervise/table.go that skips sendPending for a pending
// task with SendFailures above 0: the sweep that should meet the second refusal sends nothing.
func TestARefusedAdoptionIsSentAgainAfterADaemonRestart(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("%w: jj metaedit: the git lock .git/index.lock was last written 31m ago", runtime.ErrAdoptRefused)
	for _, tc := range []struct {
		name string
		// removed is whether the lock is gone before the booted daemon's sweep.
		removed bool
	}{
		{name: "the cause removed, the task is sent at the next sweep", removed: true},
		{name: "the cause still there, a third refusal is counted and charges nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing})
			r.told(pathRoot, record.AudienceHolder, 4)
			r.told(pathRoot, record.AudienceArchitect, 4)
			r.sup.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
				return runtime.GitIdentity{Name: "legion-tester[bot]", Email: "tester@example.com"}, nil
			}
			token := mustClaimToken(t, pathRoot, claim.RoleTester)
			sweep := func(machine *supervise.Machine) {
				t.Helper()
				if err := machine.Handle(r.ctx, aliveObservation(machine)); err != nil {
					t.Fatalf("the sweep's observation of %s: %v", token, err)
				}
				machine.Wait()
			}

			// The implementer's completion hands testing to the tester, whose start the outbox runs.
			// Its ready sends the phase task, and the sweep after it sends it again: the shim refuses
			// both adoptions on the lock.
			r.rt.FailAdoptWorkingCopy(refusal)
			r.complete(pathRoot)
			if got := r.run(startOf(pathRoot, claim.RoleTester, false)); len(got) != 1 {
				t.Fatalf("the tester's start rows = %d, want the one the implementer's completion wrote", len(got))
			}
			tester, conn := r.connect(pathRoot, claim.RoleTester)
			tester.Wait()
			sweep(tester)
			held := tester.Claim().Pending
			if held == nil || held.SendFailures != 2 || !held.ConfirmedAt.IsZero() {
				t.Fatalf("the tester holds %+v, want its phase task waiting with its two refused sends counted", held)
			}
			if prompted := conn.Prompts(); len(prompted) != 0 {
				t.Fatalf("the tester was prompted %+v, want nothing: the shim refused both adoptions", prompted)
			}
			launches := len(r.rt.CallsOf("Spawn")) + len(r.rt.CallsOf("Resume"))
			mark := r.lastRowID()

			r.restartDaemon()
			restored, booted := r.reconnect(pathRoot, claim.RoleTester, false)
			if got := restored.Claim().Pending; got == nil || got.Task != held.Task || got.SendFailures != 2 {
				t.Fatalf("the booted daemon's tester holds %+v, want the phase task with its two refused sends", got)
			}
			if tc.removed {
				r.rt.FailAdoptWorkingCopy(nil) // the lock is removed
			}
			sweep(restored)

			after := restored.Claim()
			if tc.removed {
				sent := booted.Prompts()
				if len(sent) != 1 || after.Pending == nil || sent[0].DeliveryID != after.Pending.ID || !strings.Contains(sent[0].Message, held.Task) {
					t.Fatalf("the booted daemon prompted %+v while %+v is pending, want the phase task itself, once", sent, after.Pending)
				}
			} else {
				if prompted := booted.Prompts(); len(prompted) != 0 {
					t.Fatalf("the booted daemon prompted %+v, want nothing: the shim refused the adoption again", prompted)
				}
				if after.Pending == nil || after.Pending.SendFailures != 3 {
					t.Fatalf("the booted daemon's tester holds %+v, want the third refused send counted on its task", after.Pending)
				}
				if stored := storedClaimOf(t, r, token); stored.Pending == nil || stored.Pending.SendFailures != 3 || stored.Budgets != (supervise.Budgets{}) {
					t.Fatalf("the store holds the tester as %+v with budgets %+v, want three refused sends on its task and nothing charged",
						stored.Pending, stored.Budgets)
				}
			}
			if after.State != supervise.StateReady || after.Budgets != (supervise.Budgets{}) {
				t.Fatalf("the booted daemon's tester is %s with budgets %+v, want it ready and charged nothing", after.State, after.Budgets)
			}
			// No wake and no message carried the task: no process was started for the claim since the
			// refusals, none waits to start, and nothing was written for the tester to hear.
			if now := len(r.rt.CallsOf("Spawn")) + len(r.rt.CallsOf("Resume")); now != launches {
				t.Fatalf("the runtime was asked for %d launches after the refusals, want none", now-launches)
			}
			if waiting := restored.View().WakeWaiting; waiting != nil {
				t.Fatalf("the booted daemon's tester holds the wake %+v, want none", waiting)
			}
			if rows := r.seenSince(mark, addressedTo(pathRoot, claim.RoleTester, token)); len(rows) != 0 {
				t.Fatalf("the daemon wrote %+v for the tester after the refusals, want nothing: the sweep alone sends its task", rows)
			}
		})
	}
}

// storedClaimOf is token's claim as the rig's store holds it, as the next boot reads it.
func storedClaimOf(t *testing.T, r *newsPathRig, token claim.Token) supervise.Claim {
	t.Helper()
	st, ok := r.sup.deps.Store.(*store.Store)
	if !ok {
		t.Fatalf("the rig's claim store is %T, want the daemon's", r.sup.deps.Store)
	}
	claims, err := st.Claims(r.ctx)
	if err != nil {
		t.Fatalf("read the stored claims: %v", err)
	}
	for _, c := range claims {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("the store holds no claim %s", token)
	return supervise.Claim{}
}
