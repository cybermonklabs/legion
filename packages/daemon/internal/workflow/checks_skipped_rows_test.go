package workflow

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A settlement of the wake rig's head (LEGION-208 implementing, its pull request #42 at head1, its
// implementer released), weighed against the one standing before it, in every row of three things
// (dispatch://INFRA-6293 R6):
//
//   - the word the settlement standing before gives unit: success, failure, cancelled, skipped, or
//     none, unit being a name new to the head;
//   - the change to unit: the same run; a newer run with the same word; a newer run with another
//     word, the next one of success, failure, cancelled, skipped, success; and, for a name new to
//     the head, a run of each of the four words;
//   - the skipped run of approval beside it: none; a run of approval, a skipped name new to the
//     head; or a newer run of approval, which the settlement standing before named skipped.
//
// Beside them stands build, the same run concluded success in both settlements. Each row says, as
// R6 gives it and not as the code under test computes it, whether the roles are told (tellChecks
// queues a CI-settled role message) and whether the released implementer is woken
// (wakeOnSettlement queues its start row and records its resume). A settlement whose only change
// is skipped runs, each new to the head or a newer run of its name, beside newer runs that stay
// success, tells nobody and wakes nobody. A newer run of a check told failed or cancelled is a
// change whatever skipped runs stand beside it, and wakes the implementer, as does a settlement
// whose only change is newer runs that keep their word, with no skipped run among them, and one in
// which no run changed. A change of unit's word, or a name new to the head that is not skipped, is
// told and wakes. A row marked "line unchanged" keeps every word and adds no name, so its line is
// the one told already and tellChecks queues nothing, whatever its wake.
//
// In each row the settlement standing before is applied first, and wakes the implementer; the pod
// then comes back and is released again, as in TestWakeOnSettlementOfAnOldHeadWakesNobody, which
// says why, so the row's settlement meets a released implementer.
//
// Negative controls: with a rule that takes a newer run of a told check as no change whatever its
// word, the four rows in which unit stays failure or cancelled under a newer run beside a skipped
// run of approval fail, told and woken by nobody beside a name new to the head and woken by nobody
// beside a newer run; without the rule's turning away of a run of a check new to the head that is
// not skipped, the two rows of a new run concluded success beside a skipped run of approval fail,
// told and woken by nobody (a new run concluded failure or cancelled is still turned away, its
// word being no success); and with a newer run that stays success counted as a skipped change, the
// row of a newer run of unit that stays success with no skipped run beside wakes nobody.
func TestEachChangeOfACheckBesideSkippedRunsTellsAndWakesAsR6Says(t *testing.T) {
	t.Parallel()
	// The change to unit: its run as told, a newer run of it, or a run of it where the settlement
	// standing before named no unit.
	const (
		sameRun  = "the same run"
		newerRun = "a newer run"
		newRun   = "a run new to the head"
	)
	// The skipped run of approval beside unit's: none; one where the settlement standing before
	// named no approval; or a newer run of approval, which that settlement named skipped.
	const (
		alone       = "with no skipped run beside"
		besideNew   = "beside a skipped name new to the head"
		besideNewer = "beside a newer run of a told skipped name"
	)
	// ran adds to fact a run of name of id with GitHub's word for it, as intake's decoder reads it,
	// and names it in the settlement's failed or cancelled group where the word is failure or
	// cancelled.
	ran := func(fact *intake.PullRequestChecks, name string, id int64, word string) {
		switch word {
		case "skipped":
			fact.CheckRuns = append(fact.CheckRuns, skippedRun(name, id))
			return
		case "failure":
			fact.Failing = append(fact.Failing, name)
		case "cancelled":
			fact.Cancelled = append(fact.Cancelled, name)
		}
		fact.CheckRuns = append(fact.CheckRuns, wordedRun(name, id, word))
	}
	for _, row := range []struct {
		// before is the word the settlement standing before gives unit, "" where it names none.
		before string
		change string
		// after is the word the row's settlement gives unit.
		after  string
		beside string
		// told is whether a role is told the row's settlement, woken whether it wakes the
		// released implementer.
		told, woken bool
	}{
		{"success", sameRun, "success", alone, false, true},      // line unchanged; no run changed
		{"success", newerRun, "success", alone, false, true},     // line unchanged; no skipped run among the changes
		{"success", newerRun, "failure", alone, true, true},      // unit's word changed
		{"failure", sameRun, "failure", alone, false, true},      // line unchanged; no run changed
		{"failure", newerRun, "failure", alone, false, true},     // line unchanged; a newer failed run
		{"failure", newerRun, "cancelled", alone, true, true},    // unit's word changed
		{"cancelled", sameRun, "cancelled", alone, false, true},  // line unchanged; no run changed
		{"cancelled", newerRun, "cancelled", alone, false, true}, // line unchanged; a newer cancelled run
		{"cancelled", newerRun, "skipped", alone, true, true},    // a skipped run replaces a run told by another word
		{"skipped", sameRun, "skipped", alone, false, true},      // line unchanged; no run changed
		{"skipped", newerRun, "skipped", alone, false, false},    // line unchanged; its only change is a skipped run
		{"skipped", newerRun, "success", alone, true, true},      // unit's word changed
		{"", newRun, "success", alone, true, true},               // a name new to the head that is not skipped
		{"", newRun, "failure", alone, true, true},               // a name new to the head that is not skipped
		{"", newRun, "cancelled", alone, true, true},             // a name new to the head that is not skipped
		{"", newRun, "skipped", alone, false, false},             // its only change is a skipped name new to the head

		{"success", sameRun, "success", besideNew, false, false},     // its only change is a skipped name new to the head
		{"success", newerRun, "success", besideNew, false, false},    // skipped beside a newer run that stays success
		{"success", newerRun, "failure", besideNew, true, true},      // unit's word changed
		{"failure", sameRun, "failure", besideNew, false, false},     // its only change is a skipped name new to the head
		{"failure", newerRun, "failure", besideNew, true, true},      // a newer failed run is a change
		{"failure", newerRun, "cancelled", besideNew, true, true},    // unit's word changed
		{"cancelled", sameRun, "cancelled", besideNew, false, false}, // its only change is a skipped name new to the head
		{"cancelled", newerRun, "cancelled", besideNew, true, true},  // a newer cancelled run is a change
		{"cancelled", newerRun, "skipped", besideNew, true, true},    // a skipped run replaces a run told by another word
		{"skipped", sameRun, "skipped", besideNew, false, false},     // its only change is a skipped name new to the head
		{"skipped", newerRun, "skipped", besideNew, false, false},    // its only changes are skipped runs
		{"skipped", newerRun, "success", besideNew, true, true},      // unit's word changed
		{"", newRun, "success", besideNew, true, true},               // a name new to the head that is not skipped
		{"", newRun, "failure", besideNew, true, true},               // a name new to the head that is not skipped
		{"", newRun, "cancelled", besideNew, true, true},             // a name new to the head that is not skipped
		{"", newRun, "skipped", besideNew, false, false},             // its only changes are skipped names new to the head

		{"success", sameRun, "success", besideNewer, false, false},     // line unchanged; its only change is a skipped run
		{"success", newerRun, "success", besideNewer, false, false},    // line unchanged; skipped beside a newer run that stays success
		{"success", newerRun, "failure", besideNewer, true, true},      // unit's word changed
		{"failure", sameRun, "failure", besideNewer, false, false},     // line unchanged; its only change is a skipped run
		{"failure", newerRun, "failure", besideNewer, false, true},     // line unchanged; a newer failed run is a change
		{"failure", newerRun, "cancelled", besideNewer, true, true},    // unit's word changed
		{"cancelled", sameRun, "cancelled", besideNewer, false, false}, // line unchanged; its only change is a skipped run
		{"cancelled", newerRun, "cancelled", besideNewer, false, true}, // line unchanged; a newer cancelled run is a change
		{"cancelled", newerRun, "skipped", besideNewer, true, true},    // a skipped run replaces a run told by another word
		{"skipped", sameRun, "skipped", besideNewer, false, false},     // line unchanged; its only change is a skipped run
		{"skipped", newerRun, "skipped", besideNewer, false, false},    // line unchanged; its only changes are skipped runs
		{"skipped", newerRun, "success", besideNewer, true, true},      // unit's word changed
		{"", newRun, "success", besideNewer, true, true},               // a name new to the head that is not skipped
		{"", newRun, "failure", besideNewer, true, true},               // a name new to the head that is not skipped
		{"", newRun, "cancelled", besideNewer, true, true},             // a name new to the head that is not skipped
		{"", newRun, "skipped", besideNewer, false, false},             // its only changes are skipped runs
	} {
		before := "told " + row.before
		if row.before == "" {
			before = "no unit told"
		}
		t.Run(fmt.Sprintf("%s, %s concluded %s, %s", before, row.change, row.after, row.beside), func(t *testing.T) {
			t.Parallel()
			standing := settlement("head1")
			standing.CheckRuns = []record.AttemptRun{wordedRun("build", 1, "success")}
			if row.before != "" {
				ran(&standing, "unit", 2, row.before)
			}
			if row.beside == besideNewer {
				ran(&standing, "approval", 3, "skipped")
			}
			next := laterSettlement("head1", 2)
			next.CheckRuns = []record.AttemptRun{wordedRun("build", 1, "success")}
			if row.change == sameRun {
				ran(&next, "unit", 2, row.after)
			} else {
				ran(&next, "unit", 20, row.after)
			}
			if row.beside != alone {
				ran(&next, "approval", 30, "skipped")
			}

			rig := newWakeRig(t)
			rig.apply("standing", standing)
			oneWake(t, rig.pool, claim.RoleImplementer, pods.WakeChecks, "head1")
			outboxRan(t, rig.pool)
			seedRelease(t, rig.pool, "implementer", wakeAt.Add(time.Minute))
			claimSuspendedBy(t, rig.pool, claim.RoleImplementer, "released")
			queued := roleMessageCount(t, rig.pool)

			rig.apply("next", next)
			if pr := probePullRequest(t, rig.pool); pr.Generation != 2 {
				t.Fatalf("the pull request keeps generation %d, want 2: the settlement is recorded", pr.Generation)
			}
			if told := roleMessageCount(t, rig.pool) - queued; (told > 0) != row.told {
				t.Fatalf("the settlement queued %d role messages, want a role told %t", told, row.told)
			}
			starts, resumes := wakeStarts(t, rig.pool), wakeResumes(t, rig.pool)
			if !row.woken {
				if len(starts) != 0 || len(resumes) != 1 {
					t.Fatalf("the settlement left supervise start rows %+v and pod resumes %v, want none and the first wake's alone", starts, resumes)
				}
				return
			}
			if len(starts) != 1 || starts[0].Role != claim.RoleImplementer || !strings.Contains(starts[0].Task, "head1") {
				t.Fatalf("the settlement left supervise start rows %+v, want one of the implementer naming head1", starts)
			}
			if len(resumes) != 2 || resumes[1] != [2]string{string(claim.RoleImplementer), pods.WakeChecks} {
				t.Fatalf("the settlement left pod resumes %v, want the first wake's and a %s wake of the implementer", resumes, pods.WakeChecks)
			}
		})
	}
}
