package daemon

import (
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// staleIdleClaims is n idle claims of n issues, each recording a plugin contract this daemon does
// not speak, as the refresh pass finds a fleet the boot restored on a new contract.
func staleIdleClaims(t *testing.T, n int) []supervise.Claim {
	t.Helper()
	project, _ := claim.ProjectToken("legion")
	var claims []supervise.Claim
	for i := range n {
		issue := fmt.Sprintf("LEGION-%d", i+1)
		token, err := claim.NewToken(project, issue, claim.RoleImplementer)
		if err != nil {
			t.Fatal(err)
		}
		claims = append(claims, supervise.Claim{
			Token: token, Project: project, Tree: issue, Issue: issue, Role: claim.RoleImplementer,
			Generation: 1, State: supervise.StateIdle, Session: "ses_" + issue, SessionFile: "/sessions/" + issue + ".jsonl",
			PluginContract: 1, Locator: pluginTestLocator(token),
		})
	}
	return claims
}

// grantedAfter is how many admissions gate grants in a row, at most limit+1, which a test reads
// once every credit it expects back should be back.
func grantedAfter(gate *supervise.RefreshGate, limit int) int {
	granted := 0
	for granted <= limit && gate.Admit() != nil {
		granted++
	}
	return granted
}

// A refresh pass the daemon's stop halts gives back every credit it admitted and could not post.
// The supervisor never removes a machine (supervisor.add is the one writer of its machines), so
// the one way a post the pass makes is not delivered is a stop that halts the supervisor while
// the pass runs (supervisor.post, supervisor.halt). The pass's own release of a dropped post's
// credit (refreshStalePlugins) is what gives that credit back: no machine ever takes it, so the
// gate's correction has no machine to ask about it (supervise.RefreshGate.Correct), and without
// that release the gate holds it for the life of the daemon.
//
// The stop lands where it can in production, between an admission and its post, through the
// pass's test seam (afterRefreshAdmit), which halts the supervisor at the pass's first admission.
// Every post of the pass then goes nowhere, and the gate must be whole again afterwards: rate
// fresh admissions granted and the next refused.
//
// The negative control is the pass's release of a dropped post's credit removed: the gate then
// holds the rate's worth of credits nothing will ever give back, and grants none.
func TestARefreshPassTheStopHaltsGivesBackEveryCreditItCouldNotPost(t *testing.T) {
	t.Parallel()
	const rate = 8
	sup, _ := newPluginSupervisor(t, 24, true)
	claims := staleIdleClaims(t, rate)
	if _, err := sup.restore(t.Context(), claims); err != nil {
		t.Fatalf("restore: %v", err)
	}
	gate := supervise.NewRefreshGate(rate)
	admitted := 0
	s := &supervision{log: quietLogger(), supervisor: sup, claims: claims, plan: plan{pluginRelaunchable: true},
		refreshGate: gate,
		afterRefreshAdmit: func(claim.Token, *supervise.RefreshCredit) {
			admitted++
			if admitted == 1 {
				sup.halt()
			}
		}}

	s.refreshStalePlugins(s.claims, "sweep")

	if admitted != rate {
		t.Fatalf("the pass admitted %d claims, want the rate, %d: every claim is stale and idle, so the gate grants each one",
			admitted, rate)
	}
	if granted := grantedAfter(gate, rate); granted != rate {
		t.Fatalf("the gate granted %d admissions after a pass whose every post the stop dropped, want %d: each dropped post's credit goes back to the gate",
			granted, rate)
	}
}

// A stop that halts supervision after the refresh pass delivered its posts, and before any
// machine decided them, gives back every credit those posts carry. The halt closes every inbox and
// drops what it still holds (supervisor.halt, inbox.close), so a credited post waiting there
// reaches no machine, and the post is that credit's one holder until a machine decides it.
//
// Each machine's feed is held inside a first event that carries no fact (an IssueTurnEnded) until
// the halt has closed its inbox, so every post the pass makes after it waits in that inbox.
//
// The negative control is the release of a dropped post's credit removed from inbox.close: the
// gate then holds the rate's worth of credits nothing will ever give back, and grants none.
func TestAStopAfterTheRefreshPassDeliveredItsPostsGivesBackEveryCreditTheyCarry(t *testing.T) {
	t.Parallel()
	const rate = 8
	sup, _ := newPluginSupervisor(t, 24, true)
	hold := make(chan struct{})
	var holding sync.WaitGroup
	holding.Add(rate)
	var letGoOnce sync.Once
	letGo := func() { letGoOnce.Do(func() { close(hold) }) }
	t.Cleanup(letGo)
	sup.beforeDecision = func(_ claim.Token, ev supervise.Event) {
		if _, first := ev.(supervise.IssueTurnEnded); first {
			holding.Done()
			<-hold
		}
	}
	claims := staleIdleClaims(t, rate)
	if _, err := sup.restore(t.Context(), claims); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, c := range claims {
		if !sup.post(c.Token, supervise.IssueTurnEnded{Claim: c.Token}) {
			t.Fatalf("the event that holds %s's feed was not delivered", c.Token)
		}
	}
	holding.Wait()

	gate := supervise.NewRefreshGate(rate)
	s := &supervision{log: quietLogger(), supervisor: sup, claims: claims, plan: plan{pluginRelaunchable: true},
		refreshGate: gate}
	s.refreshStalePlugins(s.claims, "sweep")
	if credit := gate.Admit(); credit != nil {
		credit.Release()
		t.Fatalf("the gate still grants after the pass, want its %d credits out on the posts waiting in the inboxes", rate)
	}

	sup.halt()
	letGo()
	sup.feeding.Wait()

	if granted := grantedAfter(gate, rate); granted != rate {
		t.Fatalf("the gate granted %d admissions after a stop dropped the pass's %d delivered posts, want %d: each dropped post's credit goes back to the gate",
			granted, rate, rate)
	}
}

// A refresh pass reads each claim it would post as the claim's machine holds it now, not as the
// sweep's snapshot read it, so it spends no credit on a claim that has left the lane since. The
// sweep reads its snapshot before its orphan reconciliation, which under Kubernetes lists every
// Sandbox, and a claim can be suspended or start a turn in between. A credit posted to such a
// claim comes back only as its machine decides the post, and until then the gate counts it out,
// so the pass refuses every claim after it and the lane admits nothing on that credit until a
// later sweep. A machine that is busy with another decision holds the post, and the credit, for
// as long as that decision runs: under Kubernetes a relaunch waits minutes for its pod.
//
// Each case restores two stale claims, the first as its machine holds it now and the second idle,
// and hands the pass a snapshot that reads both idle. The first claim's feed holds each refresh
// post it takes until the pass has returned, as a machine busy with another decision does. The
// gate grants one credit, which must go to the second claim; the claim in a turn is posted with
// no credit, as every claim in a turn is (refreshStalePlugins, holdStaleRelaunch).
//
// The negative control is the pass reading the snapshot's claim for the lane's terms, as main
// did before this change: it posts the first claim the credit and refuses the second.
func TestARefreshPassSpendsNoCreditOnAClaimThatLeftTheLaneSinceItsSnapshot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// now is the first claim as its machine holds it when the pass runs.
		now func(supervise.Claim) supervise.Claim
		// posted is whether the first claim is posted at all, with no credit.
		posted bool
	}{
		{name: "suspended since the snapshot", now: func(c supervise.Claim) supervise.Claim {
			// A suspension lets the process go (supervise's letGo), so the claim records none.
			c.State, c.SuspendedBy, c.Locator = supervise.StateSuspended, supervise.SuspendedByOperator, nil
			return c
		}},
		{name: "in a turn since the snapshot", now: func(c supervise.Claim) supervise.Claim {
			c.State = supervise.StateWorking
			return c
		}, posted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sup, _ := newPluginSupervisor(t, 24, true)
			snapshot := staleIdleClaims(t, 2)
			first, second := snapshot[0].Token, snapshot[1].Token
			hold := make(chan struct{})
			var letGoOnce sync.Once
			letGo := func() { letGoOnce.Do(func() { close(hold) }) }
			t.Cleanup(letGo)
			var (
				mu   sync.Mutex
				took []supervise.StalePlugin
			)
			sup.beforeDecision = func(token claim.Token, ev supervise.Event) {
				stale, ok := ev.(supervise.StalePlugin)
				if !ok || token != first {
					return
				}
				mu.Lock()
				took = append(took, stale)
				mu.Unlock()
				<-hold
			}
			restored := slices.Clone(snapshot)
			restored[0] = tc.now(restored[0])
			if _, err := sup.restore(t.Context(), restored); err != nil {
				t.Fatalf("restore: %v", err)
			}
			var admitted []claim.Token
			s := &supervision{log: quietLogger(), supervisor: sup, claims: snapshot, plan: plan{pluginRelaunchable: true},
				refreshGate: supervise.NewRefreshGate(1),
				afterRefreshAdmit: func(token claim.Token, _ *supervise.RefreshCredit) {
					admitted = append(admitted, token)
				}}

			s.refreshStalePlugins(snapshot, "sweep")

			if !slices.Equal(admitted, []claim.Token{second}) {
				t.Fatalf("the pass admitted %v, want only %s: %s is %s now, so the gate's one credit goes to the idle claim after it",
					admitted, second, first, restored[0].State)
			}
			// The post, if any, waits at the first claim's feed, which took it before the pass
			// returned or takes it now; nothing else posts to that claim here.
			if tc.posted {
				testwait.Eventually(t, "the first claim's feed to take the pass's post", func() bool {
					mu.Lock()
					defer mu.Unlock()
					return len(took) > 0
				})
			}
			letGo()
			sup.halt()
			sup.feeding.Wait()
			mu.Lock()
			defer mu.Unlock()
			want := 0
			if tc.posted {
				want = 1
			}
			if len(took) != want {
				t.Fatalf("the first claim's feed took %d refresh posts, want %d", len(took), want)
			}
			if len(took) == 1 && took[0].Credit != nil {
				t.Fatalf("the claim in a turn was posted a credit, want none: its relaunch waits for the turn's end, which asks the gate itself")
			}
		})
	}
}
