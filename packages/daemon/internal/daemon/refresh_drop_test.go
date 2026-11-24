package daemon

import (
	"fmt"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
