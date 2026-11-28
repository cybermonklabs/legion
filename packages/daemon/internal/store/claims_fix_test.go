package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The heads a fix's task names: the head the fix's working copy is made at, and the head the
// task's working copy moves to first; and the fix's own branch, which its working copy is made at
// where a new pod finds it on origin.
const (
	fixDeliveryFrom   = "c0de000000000000000000000000000000000001"
	fixDeliveryMove   = "c0de000000000000000000000000000000000002"
	fixDeliveryBranch = "legion-beside/LEGION-209/4/default/fix-1/implementer"
)

// fixDeliveryClaim is an idle implementer's claim the store holds, at the plugin contract the
// machine below is built for, so nothing but its pending task decides what a send does.
func fixDeliveryClaim(t *testing.T, store *Store) supervise.Claim {
	t.Helper()
	c := tmuxClaim("legion-LEGION-209-implementer")
	c.PluginContract = 24
	if err := store.PutClaim(context.Background(), c); err != nil {
		t.Fatalf("put claim: %v", err)
	}
	return c
}

// fixDelivery is a fix's task as the claim holds it pending: fix 1, the head its working copy is
// made at, the fix's own branch and the head the task's working copy moves to.
func fixDelivery(id string) supervise.Delivery {
	return supervise.Delivery{ID: id, Task: "Fix the CI check k-ci, which failed at " + fixDeliveryFrom + ".",
		Phase: phase.Implementing, Generation: 4, Entry: 2, QueuedAt: at(1), Fix: 1, FixFrom: fixDeliveryFrom, MoveTo: fixDeliveryMove,
		FixBranch: fixDeliveryBranch}
}

// adoptionAfterARestart builds a machine from the claim the store reads back, as a daemon's boot
// does, and has the sweep find its process alive, which sends the task it holds pending. It answers
// the adoption that send asked the runtime for, the fix's commit, its branch and the move it named,
// and holds that the task's prompt went out behind it.
func adoptionAfterARestart(t *testing.T, store *Store) runtime.Adoption {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c := onlyClaim(t, store)
	rt, conns, conn := fake.NewRuntime(), fake.NewConns(), fake.NewConn()
	conns.Register(c.Token, conn)
	m, err := supervise.NewMachine(ctx, supervise.Deps{
		Runtime: rt, Conns: conns, Store: store, Clock: supervise.RealClock{},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2},
		Timeouts: supervise.Timeouts{Boot: time.Minute, RegistrationIntervals: 2, RPC: 10 * time.Second,
			Probe: 10 * time.Second, Stop: 10 * time.Second, LaunchBackoff: time.Second},
		PluginContract: c.PluginContract,
		RefreshGate:    supervise.NewRefreshGate(0),
		Identity: func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
			return runtime.GitIdentity{Name: "legion-implementer[bot]", Email: "7+legion-implementer[bot]@users.noreply.github.com"}, nil
		},
	}, c)
	if err != nil {
		t.Fatalf("build the machine from the read claim: %v", err)
	}
	if err := m.Handle(ctx, supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *c.Locator, Kind: runtime.Alive, At: time.Now()}}); err != nil {
		t.Fatalf("observe the claim's process alive: %v", err)
	}
	m.Wait()
	adoptions := rt.CallsOf("AdoptWorkingCopy")
	if len(adoptions) != 1 {
		t.Fatalf("the read claim's send asked %d adoptions (%+v), want one", len(adoptions), adoptions)
	}
	if prompts := conn.Prompts(); len(prompts) != 1 || prompts[0].DeliveryID != c.Pending.ID {
		t.Fatalf("the read claim's send prompted %+v, want its pending task %s once", prompts, c.Pending.ID)
	}
	return adoptions[0].Adoption
}

// A claim's pending fix's task keeps its fix, the head its working copy is made at, the fix's own
// branch and the head the task's working copy moves to through the store: the write records the
// four with the delivery, in its insert and when it replaces the row, and the read hands them back,
// so a daemon restarted while the fix's task waits sends the same adoption, naming the fix's
// commit, its branch and the move. The control is the same delivery with the four columns left at
// their defaults, as a write that names none of them leaves them: it reads back with no fix, and
// the restarted send names none, which is the copy a restart would lose.
func TestAFixDeliveryKeepsItsCopyThroughARead(t *testing.T) {
	ctx := context.Background()
	t.Run("the read delivery holds the fix, and the restarted send adopts its copy", func(t *testing.T) {
		store := migratedStore(t)
		c := fixDeliveryClaim(t, store)
		// The second write replaces the row under a rotated id, as a refused prompt's re-put does.
		for _, put := range []supervise.Delivery{fixDelivery("outbox:12"), fixDelivery("outbox:12-r1")} {
			if err := store.PutDelivery(ctx, c.Token, put); err != nil {
				t.Fatalf("put the delivery %s: %v", put.ID, err)
			}
			got := onlyClaim(t, store).Pending
			if got == nil || got.ID != put.ID || got.Fix != 1 || got.FixFrom != fixDeliveryFrom || got.MoveTo != fixDeliveryMove || got.FixBranch != fixDeliveryBranch {
				t.Fatalf("the delivery read back = %+v, want %s of fix 1 from %s on %s moving to %s", got, put.ID, fixDeliveryFrom, fixDeliveryBranch, fixDeliveryMove)
			}
		}
		want := runtime.Adoption{FixFrom: fixDeliveryFrom, MoveTo: fixDeliveryMove, FixBranch: fixDeliveryBranch}
		if got := adoptionAfterARestart(t, store); got != want {
			t.Fatalf("the restarted send's adoption named %+v, want %+v", got, want)
		}
	})

	t.Run("control: the four columns left at their defaults read back as no fix, and the send names none", func(t *testing.T) {
		store := migratedStore(t)
		c := fixDeliveryClaim(t, store)
		if err := store.PutDelivery(ctx, c.Token, fixDelivery("outbox:12")); err != nil {
			t.Fatalf("put the delivery: %v", err)
		}
		if _, err := store.pool.Exec(ctx, `update pending_task_deliveries set fix = default, fix_from = default, move_to = default,
			fix_branch = default where claim_token = $1`, string(c.Token)); err != nil {
			t.Fatalf("leave the four columns at their defaults: %v", err)
		}
		got := onlyClaim(t, store).Pending
		if got == nil || got.ID != "outbox:12" || got.Fix != 0 || got.FixFrom != "" || got.MoveTo != "" || got.FixBranch != "" {
			t.Fatalf("the delivery read back = %+v, want outbox:12 with no fix, no fix head, no branch and no move: the loss this control asserts", got)
		}
		if adoption := adoptionAfterARestart(t, store); adoption != (runtime.Adoption{}) {
			t.Fatalf("the restarted send's adoption named %+v, want no member: the loss this control asserts", adoption)
		}
	})
}
