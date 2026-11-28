package daemon

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
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

// roundShape is one shape of claim and the membership the round's two sets must give it. holds is
// what a project's pause did to the issue's pod when the round is read, nil for no pause.
type roundShape struct {
	name  string
	claim supervise.Claim
	holds podHolds
	want  roundMembership
}

// One predicate gives the claims a pod update round asks and the claims it waits for
// (waitingClaims), so a claim is in both sets or in neither (dispatch://INFRA-4109 R2). A claim
// in the wait set and not in the ask set holds the pod until the round's bound for an answer
// nobody asked for: that is how a claim the daemon suspended held BET-1182's, OSSV-300's and
// OSSV-313's pods to their bounds. A claim the daemon suspended (daemon, exit, released, unknown,
// project_release) is in neither set, because it runs no process and no turn in the pod (R1). A
// suspension that holds (the operator's or the controller's) is in both, as dispatch://INFRA-2981
// R5 made the operator's.
//
// A claim a project's pause stopped is suspended released, and it is the pause's, not R1's,
// while the pause still owes its role a launch: the pause kept the pod for the work on its disk
// (decision 12 of dispatch://INFRA-3759). The store reads what the pause owes role by role
// (store.PausePodHold.Owed), and the daemon reads it for the claim's own role (podHolds.owes). So
// the claim is in both sets under a hold that stands, and under a hold that ended at the resume,
// wherever the hold owes its role a launch. A hold that owes its role nothing keeps it in neither
// set. No clock decides it: a let-out before the round opened changes nothing. The pause keeps no
// other source in the round: a claim the daemon's own rule suspended is in neither set under a
// hold that stands too.
//
// Each row is one shape of claim, with the membership the rule gives it, read at the round's
// clock over the pause's holds the row names. The ask side is roundAsks: a claim the round asks,
// or asks once its agent is ready (askRound reads it through unaskedClaims and SaidReady). The
// wait side is the readings that hold the replacement for a claim before the bound:
// waitingClaims, heldClaims and readRoundEnd's holding. The replacement's hold (holding) also
// holds a claim that a project's pause still keeps and that the round does not ask (R8,
// unansweredClaims). That claim is in neither of the round's two sets, and this table's wait side
// would read it as waited for. No row of this table has that shape:
// TestEveryClaimShapeOfARoleThePauseOwesALaunchIsReadByOneRule reads it. Each reading is taken
// over the claim alone, and again beside a worker that answered and records a process in the
// round's pod, so the two group shapes must agree. For a claim that answered, the wait side is
// read once more with the answer cleared, so both sides of that row are compared.
//
// The last check walks supervise.States() and supervise.SuspendSources(). A state or a source
// added to supervise without a row here fails it.
func TestEveryClaimShapeIsInBothRoundSetsOrNeither(t *testing.T) {
	t.Parallel()
	const round = podUpdateRound
	opened := podUpdateStart
	// now is the round read a minute after it opened, long before its bound.
	now := opened.Add(time.Minute)
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
	// pauseHold is one hold of the issue's pod by a pause of LEGION, put on it an hour before the
	// round opened; change ends it.
	pauseHold := func(change ...func(*store.PausePodHold)) podHolds {
		hold := store.PausePodHold{ID: 1, Pause: 1, Project: "LEGION", Issue: podUpdateIssue, HeldAt: opened.Add(-time.Hour)}
		for _, set := range change {
			set(&hold)
		}
		return podHolds{hold}
	}
	// resumed is a hold the resume ended a minute before the round opened.
	resumed := func(hold *store.PausePodHold) {
		hold.EndedAt, hold.End = opened.Add(-time.Minute), store.PausePodHoldResumed
	}
	// letOutBeforeTheOpening is the latest let-out of a launch that the hold's pause held for a
	// role of the pod, at the hold's end, which is before the round opened.
	letOutBeforeTheOpening := func(hold *store.PausePodHold) {
		hold.LetOut = hold.EndedAt
	}
	// owingTheClaim is a hold whose pause still owes the row's claim's role, the implementer, a
	// launch (store.PausePodHold.Owed).
	owingTheClaim := func(hold *store.PausePodHold) {
		hold.Owed = []claim.Role{claim.RoleImplementer}
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
		{name: "suspended by controller", claim: suspendedBy(supervise.SuspendedByController), want: inBoth},
		{name: "suspended by daemon", claim: suspendedBy(supervise.SuspendedByDaemon), want: inNeither},
		{name: "suspended by exit", claim: suspendedBy(supervise.SuspendedByExit), want: inNeither},
		{name: "suspended by released", claim: suspendedBy(supervise.SuspendedByReleased), want: inNeither},
		{name: "suspended by unknown", claim: suspendedBy(supervise.SuspendedByUnknown), want: inNeither},
		// The project release's stop is the daemon's own, and the daemon's next boot is what starts
		// the claim again: the round neither asks it nor waits for it.
		{name: "suspended by project_release", claim: suspendedBy(supervise.SuspendedByProjectRelease), want: inNeither},
		// The pause's stop takes a held wake of each role it stops (store.PodRecords.TakePodWake,
		// pods.WakePause), so while its hold stands the stopped role has an open resume of that
		// pause, whose clock is the moment of the stop.
		{name: "suspended by released under a pause hold that stands", claim: suspendedBy(supervise.SuspendedByReleased),
			holds: pauseHold(owingTheClaim), want: inBoth},
		{name: "suspended by released under an ended pause hold that still owes its role a launch", claim: suspendedBy(supervise.SuspendedByReleased),
			holds: pauseHold(resumed, owingTheClaim), want: inBoth},
		// No clock decides whether the pause keeps the claim: a let-out before the round opened
		// leaves the claim the pause's while the hold still owes its role a launch.
		{name: "suspended by released under an ended pause hold that still owes its role a launch let out before the round opened",
			claim: suspendedBy(supervise.SuspendedByReleased), holds: pauseHold(resumed, letOutBeforeTheOpening, owingTheClaim), want: inBoth},
		// The store reads that the hold owes the role nothing (store.PausePodHold.Owed): the role
		// became ready since the pause stopped it, or the pause held no launch of it.
		{name: "suspended by released under an ended pause hold that owes its role nothing", claim: suspendedBy(supervise.SuspendedByReleased),
			holds: pauseHold(resumed), want: inNeither},
		{name: "suspended by released under a pause hold its pod's deletion ended", claim: suspendedBy(supervise.SuspendedByReleased),
			holds: pauseHold(func(hold *store.PausePodHold) {
				hold.EndedAt, hold.End, hold.LetOut = opened.Add(-time.Minute), store.PausePodHoldDeleted, opened.Add(-time.Minute)
			}, owingTheClaim), want: inNeither},
		{name: "suspended by daemon under a pause hold that stands", claim: suspendedBy(supervise.SuspendedByDaemon),
			holds: pauseHold(owingTheClaim), want: inNeither},
		// A pause's hold keeps a claim the pause stopped, which is suspended released. The project
		// release takes over such a suspension (supervise's suspensionRank), and the claim is then
		// the release's: in neither set, as the daemon's own is under a hold.
		{name: "suspended by project_release under a pause hold that stands", claim: suspendedBy(supervise.SuspendedByProjectRelease),
			holds: pauseHold(owingTheClaim), want: inNeither},
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

	// waitsFor reads the wait side of c at clock over both group shapes, and fails the test where
	// the two shapes disagree.
	waitsFor := func(t *testing.T, c supervise.Claim, clock roundClock) bool {
		t.Helper()
		var read []bool
		for _, group := range [][]supervise.Claim{{c}, {c, answeredWorker}} {
			named := func(claims []supervise.Claim) bool {
				return slices.ContainsFunc(claims, func(of supervise.Claim) bool { return of.Token == c.Token })
			}
			// The background hold is a wait on the pod's processes, not on an answer, and it never
			// names a suspended or failed claim (backgroundClaims): the answer side is what R2 sets.
			unanswered, _, _ := readRoundEnd(group, round, clock).holding()
			waiting, held, holding := named(waitingClaims(group, round, clock)), named(heldClaims(group, round, clock)), named(unanswered)
			t.Logf("over %d claims: waitingClaims %t, heldClaims %t, holding %t", len(group), waiting, held, holding)
			read = append(read, waiting || held || holding)
		}
		if read[0] != read[1] {
			t.Errorf("the wait side over the claim alone is %t, and beside an answered worker in the pod %t; want one answer", read[0], read[1])
		}
		return read[0]
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			clock := roundClock{now: now, grace: podUpdateGrace, holds: row.holds}
			asks, waits := roundAsks(row.claim, round, clock), waitsFor(t, row.claim, clock)
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
				if !waitsFor(t, unanswered, clock) {
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

// owedLaunchState is one claim state of the table of
// TestEveryClaimShapeOfARoleThePauseOwesALaunchIsReadByOneRule, with the answers that the rule
// gives it written out.
type owedLaunchState struct {
	state supervise.ClaimState
	// by is the source of a suspension, "" for a claim that is not suspended.
	by supervise.SuspendSource
	// bare and located are whether the round keeps the pod for the claim that has not answered,
	// with no locator and with a locator in the round's pod.
	bare, located bool
	// sets is whether the claim with a session that has not answered is in both of the round's
	// sets. Otherwise it is in neither. answeredAsked is whether the ask set holds the claim with a
	// session once it answered. An answered claim is in no wait.
	sets, answeredAsked bool
}

// dispatch://INFRA-4109 review round 5: one rule says whether a project's pause keeps the pod of a
// round for a claim (podHolds.owes), and each reader takes it from that rule. The readers are what
// holds the pod of a round with no process in it, read over every claim of the issue
// (roundPodEnds), the same reading at a launch into the pod with the claim as the launching claim
// (launchRoundPodEnds, roundPodEndsAtLaunch), the round's two sets (roundAsks, waitingClaims,
// through roundClock.passesOver), and what holds a replacement through a process before the
// round's bound (readRoundEnd's unanswered, unansweredClaims; spec v19 R8). Review rounds 3, 4
// and 5 each found a claim state that the keep left out, so this table names every state.
//
// Each row is one state of supervise.States(), and a suspended claim has one row for each source
// of supervise.SuspendSources(). The pause's hold of the pod ended at the resume, before the round
// opened, and it still owes the claim's role, the implementer, a launch. The claim is a member of
// the round that no ask reached. The other member is a tester that answered and that the daemon
// then suspended, so no process runs in the pod. Each row is read with and without a locator in
// the round's pod, with and without a session, and with and without an answer of the round. The
// row gives the answers for a claim that has not answered. A session changes no keep: a launch
// that forgot its role's lost session records no session. A claim with no session is in neither
// set, but where the pause keeps the pod for it, it still holds a replacement through a process
// before the bound: the round's end holds the replacement for the claim exactly where the round
// waits for it or the pause keeps the pod for it. An answered claim keeps nothing and is in no
// wait.
//
// The keep is read from the reason that each reading answers (a project's pause owes the
// implementer a launch), because a suspension that holds holds the pod for a reason of its own.
//
// The last check walks supervise.States() and supervise.SuspendSources(). A state or a source
// added to supervise without a row here fails it.
func TestEveryClaimShapeOfARoleThePauseOwesALaunchIsReadByOneRule(t *testing.T) {
	t.Parallel()
	const round = podUpdateRound
	opened := podUpdateStart
	// now is the round read a minute after it opened, long before its bound.
	now := opened.Add(time.Minute)
	answeredAt := opened.Add(30 * time.Second)
	token := mustClaimToken(t, podUpdateIssue, claim.RoleImplementer)
	clock := roundClock{now: now, grace: podUpdateGrace, holds: podHolds{{
		ID: 1, Pause: 1, Project: "LEGION", Issue: podUpdateIssue, HeldAt: opened.Add(-time.Hour),
		EndedAt: opened.Add(-time.Minute), End: store.PausePodHoldResumed, Owed: []claim.Role{claim.RoleImplementer},
	}}}
	tester := supervise.Claim{Token: mustClaimToken(t, podUpdateIssue, claim.RoleTester), Project: podUpdateProject,
		Tree: podUpdateIssue, Issue: podUpdateIssue, Role: claim.RoleTester, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByDaemon, Session: "ses-tester", UpdateRound: round, UpdateReasons: podUpdateDrift,
		UpdateOpenedAt: &opened, UpdateTaskedAt: &opened, UpdateSignalledAt: &answeredAt}

	rows := []owedLaunchState{
		{state: supervise.StateQueued, bare: true, sets: true, answeredAsked: true},
		{state: supervise.StateLaunchUncertain, bare: true, sets: true, answeredAsked: true},
		{state: supervise.StateLaunching, bare: true, sets: true, answeredAsked: true},
		{state: supervise.StateShimConnected, bare: true, sets: true, answeredAsked: true},
		{state: supervise.StateRegistered, bare: true, sets: true, answeredAsked: true},
		{state: supervise.StateReady, bare: true, sets: true, answeredAsked: true},
		{state: supervise.StateWorking, bare: true, sets: true, answeredAsked: true},
		{state: supervise.StateIdle, bare: true, sets: true, answeredAsked: true},
		// The pause's own stop. Once the claim answered, the pause owes it nothing, and the round
		// passes over it as R1 passes over a released claim.
		{state: supervise.StateSuspended, by: supervise.SuspendedByReleased, bare: true, located: true, sets: true},
		{state: supervise.StateSuspended, by: supervise.SuspendedByDaemon},
		{state: supervise.StateSuspended, by: supervise.SuspendedByExit},
		{state: supervise.StateSuspended, by: supervise.SuspendedByUnknown},
		// The project release's stop keeps nothing for the pause: the claim is not released, and a
		// suspended claim is gone (supervise.Claim.Gone), so the round passes over it as R1 passes
		// over the daemon's own.
		{state: supervise.StateSuspended, by: supervise.SuspendedByProjectRelease},
		// A suspension that holds (the operator's or the controller's) holds the pod for its
		// holder, never for the pause.
		{state: supervise.StateSuspended, by: supervise.SuspendedByOperator, sets: true, answeredAsked: true},
		{state: supervise.StateSuspended, by: supervise.SuspendedByController, sets: true, answeredAsked: true},
		{state: supervise.StateFailed},
		{state: supervise.StateRetired},
	}

	// keepsForThePause is whether a reading of the pod keeps it for the claim's owed launch.
	keepsForThePause := func(ends bool, why string) bool {
		return !ends && strings.Contains(why, fmt.Sprintf("owes the %s a launch", claim.RoleImplementer))
	}
	isClaim := func(of supervise.Claim) bool { return of.Token == token }
	for _, row := range rows {
		for _, located := range []bool{false, true} {
			for _, session := range []bool{true, false} {
				for _, answered := range []bool{false, true} {
					c := supervise.Claim{Token: token, Project: podUpdateProject, Tree: podUpdateIssue, Issue: podUpdateIssue,
						Role: claim.RoleImplementer, State: row.state, SuspendedBy: row.by,
						UpdateRound: round, UpdateReasons: podUpdateDrift, UpdateOpenedAt: &opened}
					shape := []string{string(row.state), "no locator", "no session", "unanswered"}
					if row.by != "" {
						shape[0] += " by " + string(row.by)
					}
					if located {
						c.Locator = podUpdateLocator(token, podUpdateIssue, podUpdatePod)
						shape[1] = "a locator"
					}
					if session {
						c.Session = "ses-implementer"
						shape[2] = "a session"
					}
					if answered {
						c.UpdateTaskedAt, c.UpdateSignalledAt = &opened, &answeredAt
						shape[3] = "answered"
					}
					t.Run(strings.Join(shape, ", "), func(t *testing.T) {
						group := []supervise.Claim{c, tester}
						keep := row.bare
						if located {
							keep = row.located
						}
						keep = keep && !answered
						if got := keepsForThePause(roundPodEnds(group, round, clock)); got != keep {
							t.Errorf("the sweep's reading keeps the pod for the claim's owed launch: %t, want %t", got, keep)
						}
						if got := keepsForThePause(launchRoundPodEnds(group, c, round, clock)); got != keep {
							t.Errorf("the reading at the claim's own launch keeps the pod for its owed launch: %t, want %t", got, keep)
						}
						wantAsks, wantWaits := session && row.sets, session && row.sets
						if answered {
							wantAsks, wantWaits = session && row.answeredAsked, false
						}
						if got := roundAsks(c, round, clock); got != wantAsks {
							t.Errorf("the round asks the claim: %t, want %t", got, wantAsks)
						}
						if got := slices.ContainsFunc(waitingClaims(group, round, clock), isClaim); got != wantWaits {
							t.Errorf("the round waits for the claim: %t, want %t", got, wantWaits)
						}
						unanswered, _, _ := readRoundEnd(group, round, clock).holding()
						if got, want := slices.ContainsFunc(unanswered, isClaim), wantWaits || keep; got != want {
							t.Errorf("the round's end holds the replacement through a process for the claim: %t, want %t", got, want)
						}
					})
				}
			}
		}
	}

	for _, state := range supervise.States() {
		if !slices.ContainsFunc(rows, func(row owedLaunchState) bool { return row.state == state }) {
			t.Errorf("no row holds a claim in state %s: add a row for the state", state)
		}
	}
	for _, source := range supervise.SuspendSources() {
		if !slices.ContainsFunc(rows, func(row owedLaunchState) bool {
			return row.state == supervise.StateSuspended && row.by == source
		}) {
			t.Errorf("no row holds a claim suspended by %s: add a row for the source", source)
		}
	}
}
