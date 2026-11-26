package daemon

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The queue a relaunch after an end from outside the pod waits in (dispatch://INFRA-5682 R2), on
// the rig of message_wake_test.go (priorityWakeRig): such an end charges no death, though its
// decision writes the mark a death's does (supervise.Claim.DeathCharged), so its relaunch waits
// in the second queue, as a displaced relaunch does, on one daemon and at the boot's pass.

// observeOutside tells c's machine that the runtime found its process gone with a pod it read as
// ended by something outside it (runtime.PodEndNotOurs), which the machine continues in the
// claim's own session and charges nothing (supervise's endedFromOutside). That decision writes
// the claim, so the store then holds the end decided from outside the pod.
func (r *priorityWakeRig) observeOutside(c supervise.Claim) {
	r.t.Helper()
	m, _ := r.sup.Machine(c.Token)
	if err := m.Handle(r.t.Context(), supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *c.Locator, Kind: runtime.Gone, At: r.clock.Now(), EndReading: runtime.PodEndNotOurs,
	}}); err != nil {
		r.t.Fatal(err)
	}
}

// owedWork is a claim whose task the boot finds owed since at (bootOwing).
type owedWork struct {
	token claim.Token
	at    time.Time
}

// bootOwing stops the rig's supervisor and starts a new one over the same database, as restart
// does, with each claim of owed holding a task queued at its moment, so the owed order of the
// boot's pass is the test's alone. It runs that pass over the launches the first supervisor left
// unfinished and the ends its claims record decided (recordedDeaths), and answers the claims the
// boot read.
func (r *priorityWakeRig) bootOwing(owed ...owedWork) []supervise.Claim {
	r.t.Helper()
	r.sup.stop()
	for i, w := range owed {
		if err := r.st.PutDelivery(r.t.Context(), w.token, supervise.Delivery{ID: fmt.Sprintf("owed-%d", i), Task: "Owed work", QueuedAt: w.at}); err != nil {
			r.t.Fatal(err)
		}
	}
	r.st = outboxTreeStore(r.t, r.pool)
	claims, err := r.st.Claims(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	r.open()
	unfinished, err := r.sup.restore(r.t.Context(), claims)
	if err != nil {
		r.t.Fatal(err)
	}
	s := supervision{supervisor: r.sup, log: quietLogger()}
	if refused := s.launchUnfinished(r.t.Context(), waitingLaunches(unfinished), recordedDeaths(claims)...); len(refused) != 0 {
		r.t.Fatalf("boot relaunches refused: %v", refused)
	}
	return claims
}

// wantQueueLines is the wake line of each claim of want saying the queue want names for it,
// `message` for the first and `other` for the second.
func (r *priorityWakeRig) wantQueueLines(want map[claim.Token]string) {
	r.t.Helper()
	for token, queue := range want {
		if line, ok := wakeLineOf(r.t, r.logged, token); !ok || line["queue"] != queue {
			r.t.Fatalf("the relaunch of %s wrote %v; want the %s queue", token, line, queue)
		}
	}
}

// R2: with four relaunches after ends from outside the pod paced, the relaunch of a role whose
// own process died runs at the earliest turn still held, and the four run after it in their
// order, each from the other queue and charged nothing: an end from outside the pod charges no
// death and takes no place in the first queue, as a displaced relaunch takes none
// (TestADeathRelaunchGoesAheadOfDisplacedRelaunches). Negative control: supervise.WakeQueue
// reading Claim.DeathCharged in place of Claim.chargedDeath puts all five in the first queue,
// where the death's relaunch, raised last, runs last.
func TestADeathRelaunchGoesAheadOfRelaunchesAfterEndsFromOutsideThePod(t *testing.T) {
	r := newPriorityWakeRig(t)
	var outside []claim.Token
	for range 4 {
		c := r.seed(supervise.StateWorking)
		r.observeOutside(c)
		outside = append(outside, c.Token)
	}
	dead := r.seed(supervise.StateWorking)
	r.observe(dead, false)
	launches := r.run(5)
	wantWakeOrder(t, launches, append([]claim.Token{dead.Token}, outside...))
	if !launches[0].at.Equal(wakeOrigin.Add(testWakeInterval)) {
		t.Fatalf("charged relaunch ran at %s; want the earliest held turn", launches[0].at)
	}
	want := map[claim.Token]string{dead.Token: "message"}
	for _, token := range outside {
		want[token] = "other"
		if m, _ := r.sup.Machine(token); m.Claim().Budgets != (supervise.Budgets{}) {
			t.Fatalf("the relaunch after the end from outside the pod of %s left budgets %+v; want nothing charged", token, m.Claim().Budgets)
		}
	}
	r.wantQueueLines(want)
}

// R2 at the boot's pass: a claim the previous daemon left over an end from outside the pod that
// it decided, as the store holds one after that decision's write, is placed by the boot in the
// other queue, in the order its work was owed among that queue's places: behind the rule resume
// owed before it, ahead of the one owed after it, and behind the recorded charged death, whose
// relaunch alone waits in the first queue though its work is owed last of all. Negative control:
// supervise.WakeQueue reading Claim.DeathCharged in place of Claim.chargedDeath places the end
// from outside the pod in the first queue, where its work owed before the death's runs it first.
func TestARestartPlacesTheRelaunchAfterAnEndFromOutsideThePodInTheSecondQueue(t *testing.T) {
	r := newPriorityWakeRig(t)
	early := r.start(pods.WakeChecks)
	outside := r.seed(supervise.StateWorking)
	r.observeOutside(outside)
	late := r.start(pods.WakeChecks)
	dead := r.seed(supervise.StateWorking)
	r.observe(dead, false)

	claims := r.bootOwing(
		owedWork{early.Token, wakeOrigin.Add(-3 * time.Hour)},
		owedWork{outside.Token, wakeOrigin.Add(-2 * time.Hour)},
		owedWork{late.Token, wakeOrigin.Add(-time.Hour)},
		owedWork{dead.Token, wakeOrigin.Add(-time.Minute)},
	)

	i := slices.IndexFunc(claims, func(c supervise.Claim) bool { return c.Token == outside.Token })
	if i < 0 {
		t.Fatalf("the boot read no claim %s", outside.Token)
	}
	if c := claims[i]; !c.DeathCharged || !c.EndedWork.FromOutside || c.EndedWork.Generation != c.Generation {
		t.Fatalf("the store holds %s decided %v with the record %+v at generation %d; want the end decided from outside the pod",
			c.Token, c.DeathCharged, c.EndedWork, c.Generation)
	}
	wantWakeOrder(t, r.run(4), []claim.Token{dead.Token, early.Token, outside.Token, late.Token})
	r.wantQueueLines(map[claim.Token]string{dead.Token: "message", early.Token: "other", outside.Token: "other", late.Token: "other"})
}
