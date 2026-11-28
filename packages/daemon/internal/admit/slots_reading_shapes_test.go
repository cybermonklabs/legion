package admit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The slots reading (spec R6) names two shapes a deployment reported where they stand, under
// every rule the repository keeps. A tree root in implementing whose implementer is idle with no
// pending task, holding its slot for hundreds of hours while its children run, is `idle` under a
// rule whose wait stops no phase and takes no cover (the rules the recorded windows ran): the
// spec's `issue` is a holder that waits on another issue and keeps its slot under the rule, and
// such a rule has no root past admitted wait on its children (record.SlotRule.StartedParents names
// none, and record.SlotRule.ChildrenFirstCandidates reads a parent at admitted alone), so the root
// names no wait (blockers.KeptWaits) and none of the first four states holds for it. Its time is
// the end of its implementer's last turn, the later of that and its slot. And a root in
// awaiting_person, on an ask or on its design gate, holding its slot while it waits on a person, is
// `person`, from the moment its wait began. The children run in their implementers' turns,
// `working`.
//
// The shapes: ACME-1, a root in implementing, holds its slot since 329 hours before the tick; its
// implementer's claim is idle with no task, its newest turn lasting seven seconds and ending two
// hours before the tick; ACME-2 and ACME-3, its children in implementing, hold slots and their
// implementers are in turns since 40 and 25 minutes before; ACME-4, a child of ACME-1 filed in
// backlog, is open and unreleased. ACME-5, a root in awaiting_person on an ask since six hours
// before, and ACME-6, a root in awaiting_person on its design gate's version 1 since two hours
// before, hold slots and have no claim in a turn.
//
// Under a rule whose wait stops the phase under way (record.SlotRule.WaitStopsPhase: the daemon's
// own since cybermonklabs/legion#4592, and every candidate built from it but cover), ACME-4, open,
// holds ACME-1's next start, so the tick's pass moves ACME-1 into awaiting_children (admit's
// childrenWaits) and the release frees its slot, awaiting_children keeping none: 4 of 5 in use,
// ACME-2 and ACME-3 working and ACME-5 and ACME-6 person. ACME-1 holds no slot and stands in no
// line, and it is started work without a slot in all (reading (b): a started issue before its
// review that holds no slot, its wait on its children included) from the tick, the moment it
// entered the wait (record.ChildrenWait.Since), and no wait only for a slot. Under cover, whose
// wait stops no phase and whose
// start may take a cover, ACME-1 keeps its phase and its slot, and ACME-4, an open child of a
// started parent, is a wait a cover reads (blockers.CoverWaits), one ACME-1 keeps its slot through,
// so it is `issue` from this pass, the first that finds the wait: 5 of 5 in use. ACME-4 is not
// released, so no cover forms for it.
//
// Under a rule with an idle return (record.SlotRule.IdleReturn, an hour in every kept rule that
// names one, both built from the daemon's own) ACME-1 moves into awaiting_children as above, and
// ACME-5 and ACME-6, waiting on a person with no turn since they took their slots, give their slots
// back at the tick's own pass (record.LineFacts.IdleReturns). The two children at work hold the
// only slots in use, ACME-1 is started work without a slot in all as above, and ACME-5 and ACME-6
// stand in no line until a person answers them.
func TestTheSlotsReadingNamesAnIdleRootAndARootWaitingOnAPerson(t *testing.T) {
	lastTurnEnd := fixedNow.Add(-2 * time.Hour)
	turnStarts := map[string]time.Time{"ACME-2": fixedNow.Add(-40 * time.Minute), "ACME-3": fixedNow.Add(-25 * time.Minute)}
	askSince, gateSince := fixedNow.Add(-6*time.Hour), fixedNow.Add(-2*time.Hour)
	for _, rule := range record.SlotRules() {
		t.Run(rule.Name, func(t *testing.T) {
			pool := migratedPool(t)
			registerControllerFor(t, pool, acmeProject)
			claims := claimStore(t, pool)
			putClaim := func(key string, state supervise.ClaimState, start, end time.Time) {
				t.Helper()
				if err := claims.PutClaim(context.Background(), supervise.Claim{Token: acmeToken(t, key, claim.RoleImplementer),
					Project: acmeProjectToken(t), Tree: "ACME-1", Issue: key, Role: claim.RoleImplementer, TreeEpoch: 1, Generation: 1,
					State: state, Session: "ses_implementer_" + key, LastTurnStart: start, LastTurnEnd: end}); err != nil {
					t.Fatalf("record the implementer's claim of %s: %v", key, err)
				}
			}

			acmeRoot(t, pool, "ACME-1", phase.Implementing, "in_progress", "a", nil, nil)
			approvedGate(t, pool, "ACME-1")
			takeSlotAt(t, pool, "ACME-1", fixedNow.Add(-329*time.Hour))
			putClaim("ACME-1", supervise.StateIdle, lastTurnEnd.Add(-7*time.Second), lastTurnEnd)
			parent := "ACME-1"
			for i, key := range []string{"ACME-2", "ACME-3"} {
				putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Project: acmeProject, Title: key, Tree: "ACME-1", Parent: &parent, Phase: phase.Implementing, Generation: 1,
					Status: "in_progress", DispatchStatus: "in_progress", Rank: fmt.Sprintf("a%d", i+1), LastDispatchSeq: 1})
				takeSlotAt(t, pool, key, turnStarts[key].Add(-time.Hour))
				putClaim(key, supervise.StateWorking, turnStarts[key], time.Time{})
			}
			openChild(t, pool, "ACME-4", "ACME-1")

			acmeRoot(t, pool, "ACME-5", phase.AwaitingPerson, "in_progress", "b", nil, func(root *record.Issue) {
				root.Awaiting = &record.Awaiting{Ask: "ask-ACME-5", Since: record.EventMark{Seq: 5, At: askSince}}
			})
			takeSlotAt(t, pool, "ACME-5", askSince.Add(-time.Hour))
			acmeRoot(t, pool, "ACME-6", phase.AwaitingPerson, "in_progress", "c", nil, func(root *record.Issue) {
				root.Awaiting = &record.Awaiting{GateVersion: 1, Artifact: "spec-ACME-6", Since: record.EventMark{Seq: 5, At: gateSince}}
			})
			seedGate(t, pool, record.DesignGate{Issue: "ACME-6", ArtifactID: "spec-ACME-6", LatestVersion: 1})
			takeSlotAt(t, pool, "ACME-6", gateSince.Add(-time.Hour))

			tickThrough(t, pool, ruledAdmission(t, 5, rule, silent()), "tick")

			notice, payload := tickNoticeOf(t, pool, acmeProject)
			if notice.Slots == nil {
				t.Fatalf("the tick notice carries no slots reading: %s", payload)
			}
			oldestTurn := turnStarts["ACME-2"]
			working, person := record.StateCount{Count: 2, Oldest: &oldestTurn}, record.StateCount{Count: 2, Oldest: &askSince}
			waitingParent := record.StateCount{Count: 1, Oldest: &fixedNow}
			var want record.SlotsReading
			switch {
			case rule.WaitStopsPhase && rule.IdleReturn > 0:
				want = record.SlotsReading{InUse: 2, Cap: 5, Working: working, StartedWithoutSlot: waitingParent}
			case rule.WaitStopsPhase:
				want = record.SlotsReading{InUse: 4, Cap: 5, Working: working, Person: person, StartedWithoutSlot: waitingParent}
			case rule.Take == record.TakeCover:
				want = record.SlotsReading{InUse: 5, Cap: 5, Working: working, Person: person,
					Issue: record.StateCount{Count: 1, Oldest: &fixedNow}}
			default:
				want = record.SlotsReading{InUse: 5, Cap: 5, Working: working, Person: person,
					Idle: record.StateCount{Count: 1, Oldest: &lastTurnEnd}}
			}
			want.Line = record.SlotsLine(want)
			if got, wanted := slotsReadingSaid(t, *notice.Slots), slotsReadingSaid(t, want); got != wanted {
				t.Fatalf("the tick notice's slots = %s, want %s", got, wanted)
			}
		})
	}
}
