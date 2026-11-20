package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roundMembership is where one claim stands in the two sets of a pod update round: the claims the
// round asks, and the claims it waits for before its bound.
type roundMembership int

const (
	// inBoth is a claim the round asks, now or once its agent is ready, and waits for until it
	// answers.
	inBoth roundMembership = iota + 1
	// inNeither is a claim the round does not ask and does not wait for.
	inNeither
	// answeredIt is a claim the round asks that has answered. It is in the ask set and out of the
	// wait set. The same claim with its answer cleared is in the wait set.
	answeredIt
)

// roundShape is one shape of claim and the membership the round's two sets must give it.
type roundShape struct {
	name  string
	claim supervise.Claim
	want  roundMembership
}

// One predicate gives the claims a pod update round asks and the claims it waits for, so a claim
// is in both sets or in neither (dispatch://INFRA-4109 R2). A claim in the wait set and not in
// the ask set holds the pod until the round's bound for an answer nobody asked for: that is how
// a claim the daemon suspended held BET-1182's, OSSV-300's and OSSV-313's pods to their bounds.
// A claim the daemon suspended (daemon, exit, released, unknown) is in neither set, because it
// runs no process and no turn in the pod (R1). An operator's suspension is in both, as
// dispatch://INFRA-2981 R5 made it.
//
// Each row is one shape of claim, with the membership the rule gives it. The ask side is
// roundAsks: a claim the round asks, or asks once its agent is ready (askRound reads it through
// unaskedClaims and SaidReady). The wait side is every reading that holds the replacement for a
// claim before the bound: waitingClaims, heldByOperator and readRoundEnd's holding. Each reading
// is taken over the claim alone, and again beside a worker that answered and records a process in
// the round's pod, so the two group shapes must agree. For a claim that answered, the wait side
// is read once more with the answer cleared, so both sides of that row are compared.
//
// The last check walks supervise.States() and supervise.SuspendSources(). A state or a source
// added to supervise without a row here fails it.
func TestEveryClaimShapeIsInBothRoundSetsOrNeither(t *testing.T) {
	const round = podUpdateRound
	opened := podUpdateStart
	now := opened.Add(time.Minute)
	// clock is the round read a minute after it opened, long before its bound, with no pause
	// holding its pod.
	clock := roundClock{now: now, grace: podUpdateGrace}
	answeredAt := opened.Add(30 * time.Second)
	token := mustClaimToken(t, podUpdateIssue, claim.RoleImplementer)

	member := func(state supervise.ClaimState, change ...func(*supervise.Claim)) supervise.Claim {
		c := supervise.Claim{Token: token, Project: podUpdateProject, Tree: podUpdateIssue, Issue: podUpdateIssue,
			Role: claim.RoleImplementer, State: state, Session: "ses-implementer",
			UpdateRound: round, UpdateReasons: podUpdateDrift, UpdateOpenedAt: &opened}
		for _, set := range change {
			set(&c)
		}
		return c
	}
	suspendedBy := func(source supervise.SuspendSource) supervise.Claim {
		return member(supervise.StateSuspended, func(c *supervise.Claim) { c.SuspendedBy = source })
	}

	rows := []roundShape{
		{name: "queued", claim: member(supervise.StateQueued), want: inBoth},
		{name: "launch_uncertain", claim: member(supervise.StateLaunchUncertain), want: inBoth},
		{name: "launching", claim: member(supervise.StateLaunching), want: inBoth},
		{name: "shim_connected", claim: member(supervise.StateShimConnected), want: inBoth},
		{name: "registered", claim: member(supervise.StateRegistered), want: inBoth},
		{name: "ready", claim: member(supervise.StateReady), want: inBoth},
		{name: "working", claim: member(supervise.StateWorking), want: inBoth},
		{name: "idle", claim: member(supervise.StateIdle), want: inBoth},
		{name: "failed", claim: member(supervise.StateFailed), want: inNeither},
		{name: "retired", claim: member(supervise.StateRetired), want: inNeither},
		{name: "suspended by operator", claim: suspendedBy(supervise.SuspendedByOperator), want: inBoth},
		{name: "suspended by daemon", claim: suspendedBy(supervise.SuspendedByDaemon), want: inNeither},
		{name: "suspended by exit", claim: suspendedBy(supervise.SuspendedByExit), want: inNeither},
		{name: "suspended by released", claim: suspendedBy(supervise.SuspendedByReleased), want: inNeither},
		{name: "suspended by unknown", claim: suspendedBy(supervise.SuspendedByUnknown), want: inNeither},
		{name: "an operator suspension held over a running turn", claim: member(supervise.StateWorking, func(c *supervise.Claim) {
			c.SuspensionHeld, c.SuspensionHeldBy = true, supervise.SuspendedByOperator
		}), want: inBoth},
		{name: "a daemon suspension held over a running turn", claim: member(supervise.StateWorking, func(c *supervise.Claim) {
			c.SuspensionHeld, c.SuspensionHeldBy = true, supervise.SuspendedByDaemon
		}), want: inBoth},
		{name: "no session", claim: member(supervise.StateQueued, func(c *supervise.Claim) { c.Session = "" }), want: inNeither},
		{name: "left the round's pod", claim: member(supervise.StateReady, func(c *supervise.Claim) { c.UpdateLeftRound = round }), want: inNeither},
		{name: "answered", claim: member(supervise.StateReady, func(c *supervise.Claim) {
			c.UpdateTaskedAt, c.UpdateSignalledAt = &opened, &answeredAt
		}), want: answeredIt},
		{name: "records another round", claim: member(supervise.StateReady, func(c *supervise.Claim) {
			c.UpdateRound = supervise.PodRound(podUpdatePreviousPod)
		}), want: inBoth},
		{name: "records no round", claim: member(supervise.StateReady, func(c *supervise.Claim) {
			c.UpdateRound, c.UpdateReasons, c.UpdateOpenedAt = "", "", nil
		}), want: inBoth},
	}

	// answeredWorker is a worker beside the row's claim: it answered the round and records a
	// process in the round's pod, so the round has a process to replace the pod through.
	testerToken := mustClaimToken(t, podUpdateIssue, claim.RoleTester)
	answeredWorker := supervise.Claim{Token: testerToken, Project: podUpdateProject, Tree: podUpdateIssue, Issue: podUpdateIssue,
		Role: claim.RoleTester, State: supervise.StateReady, Session: "ses-tester",
		Locator:     podUpdateLocator(testerToken, podUpdateIssue, podUpdatePod),
		UpdateRound: round, UpdateReasons: podUpdateDrift, UpdateOpenedAt: &opened,
		UpdateTaskedAt: &opened, UpdateSignalledAt: &answeredAt}

	// waitsFor reads the wait side of c over both group shapes, and fails the test where the two
	// shapes disagree.
	waitsFor := func(t *testing.T, c supervise.Claim) bool {
		t.Helper()
		var read []bool
		for _, group := range [][]supervise.Claim{{c}, {c, answeredWorker}} {
			named := func(claims []supervise.Claim) bool {
				return slices.ContainsFunc(claims, func(of supervise.Claim) bool { return of.Token == c.Token })
			}
			unanswered, _ := readRoundEnd(group, round, clock).holding()
			waiting, held, holding := named(waitingClaims(group, round)), named(heldByOperator(group, round)), named(unanswered)
			t.Logf("over %d claims: waitingClaims %t, heldByOperator %t, holding %t", len(group), waiting, held, holding)
			read = append(read, waiting || held || holding)
		}
		if read[0] != read[1] {
			t.Errorf("the wait side over the claim alone is %t, and beside an answered worker in the pod %t; want one answer", read[0], read[1])
		}
		return read[0]
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			asks, waits := roundAsks(row.claim, round), waitsFor(t, row.claim)
			switch row.want {
			case inBoth:
				if !asks || !waits {
					t.Errorf("asked %t, waited for %t; want the claim in both sets", asks, waits)
				}
			case inNeither:
				if asks || waits {
					t.Errorf("asked %t, waited for %t; want the claim in neither set", asks, waits)
				}
			case answeredIt:
				if !asks || waits {
					t.Errorf("asked %t, waited for %t; want the answered claim asked and not waited for", asks, waits)
				}
				unanswered := row.claim
				unanswered.UpdateSignalledAt = nil
				if !waitsFor(t, unanswered) {
					t.Errorf("with its answer cleared, the claim is not waited for; want it waited for, as the round asks it")
				}
			default:
				t.Fatalf("the row gives no membership")
			}
		})
	}

	for _, state := range supervise.States() {
		if !slices.ContainsFunc(rows, func(row roundShape) bool { return row.claim.State == state && row.claim.Session != "" }) {
			t.Errorf("no row holds a claim with a session in state %s: add a row for the state", state)
		}
	}
	for _, source := range supervise.SuspendSources() {
		if !slices.ContainsFunc(rows, func(row roundShape) bool {
			return row.claim.State == supervise.StateSuspended && row.claim.SuspendedBy == source
		}) {
			t.Errorf("no row holds a claim suspended by %s: add a row for the source", source)
		}
	}
}
