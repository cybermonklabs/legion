package admit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// acmeProject is the Dispatch project every reading of this file and of pass_record_test.go
// records its issues under.
const acmeProject = "ACME"

// slotRuleReader is one row of TestEveryReaderOfTheSlotRuleTakesTheRule: one reader of the slot
// rule, the choice of the rule it reads, and the outcome that choice decides.
type slotRuleReader struct {
	// reader names the reader and, where it reads more than one, the choice this row is about.
	reader string
	// changes is the fields of record.SlotRule that rule moves off record.MainRule, by name, which
	// TestTheChoicesOfARuleAreTheReadersRows reads.
	changes []string
	// rule is record.MainRule with this row's choice changed and nothing else.
	rule record.SlotRule
	// outcome seeds one store of its own, runs the admission pass under the rule it is handed
	// through the daemon's own chain of handlers (Handlers), and reads back what the row's choice
	// decides, as a sentence.
	outcome func(t *testing.T, rule record.SlotRule) string
	// main and changed are the sentences outcome gives under record.MainRule and under rule.
	main, changed string
	// why is what the choice decides, which a failure names.
	why string
}

// Every reader of the slot rule reads the rule its pass was built with, and none reads the
// daemon's own in its place. The table holds one row for each reader: each row seeds a store
// through this package's fixtures, runs one admission pass through intake.ApplyFact with the
// daemon's own chain of handlers (Handlers) — once under record.MainRule and once under a rule
// that changes that row's choice alone, given to the admission (UseSlotRule) and to the engine
// (workflow.Config.SlotRule) — and reads back the outcome the choice decides: a slot held or given
// back, a start written or owed, a place in the line, a start the cap lets through, a phase moved
// or kept. A reader that read the daemon's rule in place of its pass's would give the main
// outcome under both rules, and its row fails.
//
// The choice of which phases keep a slot is read once for each phase of phase.All, taken as data,
// each row under a rule that flips that phase alone: its outcome is whether the issue keeps its
// slot, which is the rule's own answer for that phase wherever the record holds the issue in the
// workflow (record.InWorkflow), and a done issue is out of it whatever the rule says.
func TestEveryReaderOfTheSlotRuleTakesTheRule(t *testing.T) {
	for _, row := range slotRuleReaders() {
		t.Run(row.reader, func(t *testing.T) {
			for _, run := range []struct {
				rule record.SlotRule
				want string
			}{{record.MainRule, row.main}, {row.rule, row.changed}} {
				t.Run(run.rule.Name, func(t *testing.T) {
					if got := row.outcome(t, run.rule); got != run.want {
						t.Fatalf("under the rule %q: %s\nwant %s\n(%s)", run.rule.Name, got, run.want, row.why)
					}
				})
			}
		})
	}
}

// Each choice of a slot rule has a reader whose outcome it moves: for every field of
// record.SlotRule but its name and its commit, some row of the table above changes that field and
// gives another outcome under its rule than under the same rule with that field set back to the
// daemon's own. So a choice added to the rule later fails here until a row reads it, and a row
// whose reader ignores its choice moves nothing and covers nothing.
//
// Each row names the fields its rule changes, as data, and the names are held to the rule itself:
// a row's rule differs from record.MainRule in exactly the fields it names.
func TestTheChoicesOfARuleAreTheReadersRows(t *testing.T) {
	rows := slotRuleReaders()
	for _, row := range rows {
		named := slices.Sorted(slices.Values(row.changes))
		if moved := choicesMoved(row.rule); !slices.Equal(moved, named) {
			t.Errorf("the row %q names the choices %v, and its rule %q moves %v off record.MainRule", row.reader, named, row.rule.Name, moved)
		}
	}
	for _, choice := range slotRuleChoices() {
		moved := false
		for _, row := range rows {
			if !slices.Contains(row.changes, choice) {
				continue
			}
			without := withMainChoice(row.rule, choice)
			var under, back string
			t.Run(row.reader+"/"+row.rule.Name, func(t *testing.T) { under = row.outcome(t, row.rule) })
			t.Run(row.reader+"/"+without.Name, func(t *testing.T) { back = row.outcome(t, without) })
			if under != "" && under != back {
				moved = true
				break
			}
		}
		if !moved {
			t.Errorf("record.SlotRule.%s is a choice no row of TestEveryReaderOfTheSlotRuleTakesTheRule moves the outcome of: "+
				"each row that changes it gives the same outcome with it set back to record.MainRule's, so no reader takes it", choice)
		}
	}
}

// slotRuleChoices is every field of record.SlotRule that is a choice of the rule, by name: each
// field but the rule's name and the commit it comes from, in the order the type declares them.
func slotRuleChoices() []string {
	var choices []string
	for field := range reflect.TypeFor[record.SlotRule]().Fields() {
		if field.Name != "Name" && field.Name != "Commit" {
			choices = append(choices, field.Name)
		}
	}
	return choices
}

// choicesMoved is the choices of rule that differ from record.MainRule's, sorted by name.
func choicesMoved(rule record.SlotRule) []string {
	value, daemon := reflect.ValueOf(rule), reflect.ValueOf(record.MainRule)
	var moved []string
	for _, choice := range slotRuleChoices() {
		if !reflect.DeepEqual(value.FieldByName(choice).Interface(), daemon.FieldByName(choice).Interface()) {
			moved = append(moved, choice)
		}
	}
	slices.Sort(moved)
	return moved
}

// withMainChoice is rule with its choice set back to record.MainRule's, under a name of its own.
func withMainChoice(rule record.SlotRule, choice string) record.SlotRule {
	value := reflect.ValueOf(&rule).Elem()
	value.FieldByName(choice).Set(reflect.ValueOf(record.MainRule).FieldByName(choice))
	rule.Name += ", " + choice + " as main's"
	return rule
}

// ruleWith is record.MainRule under name with change applied to it: the one way a row builds its
// rule, so each rule is the daemon's own moved by exactly what its change says. The phases are a
// copy, so no change reaches record.MainRule's own.
func ruleWith(name string, change func(*record.SlotRule)) record.SlotRule {
	rule := record.MainRule
	rule.Name = name
	rule.KeepsSlotIn = slices.Clone(record.MainRule.KeepsSlotIn)
	change(&rule)
	return rule
}

// keepingSlotIn is record.MainRule with a slot kept in stands as well.
func keepingSlotIn(stands phase.Phase) record.SlotRule {
	return ruleWith("main with a slot in "+string(stands), func(rule *record.SlotRule) {
		rule.KeepsSlotIn = append(rule.KeepsSlotIn, stands)
	})
}

// keepingNoSlotIn is record.MainRule with no slot kept in stands.
func keepingNoSlotIn(stands phase.Phase) record.SlotRule {
	return ruleWith("main with no slot in "+string(stands), func(rule *record.SlotRule) {
		rule.KeepsSlotIn = slices.DeleteFunc(rule.KeepsSlotIn, func(kept phase.Phase) bool { return kept == stands })
	})
}

// flippingSlotIn is record.MainRule with the one choice of whether stands keeps a slot flipped.
func flippingSlotIn(stands phase.Phase) record.SlotRule {
	if slices.Contains(record.MainRule.KeepsSlotIn, stands) {
		return keepingNoSlotIn(stands)
	}
	return keepingSlotIn(stands)
}

// statusIn is the Dispatch status an issue standing in stands shows, as the rows of
// phases_up_to_review_test.go give it.
func statusIn(stands phase.Phase) string {
	switch stands {
	case phase.Testing:
		return "testing"
	case phase.Reviewing, phase.Accepting:
		return "needs_review"
	case phase.Retro, phase.Merging:
		return "retro"
	case phase.Done:
		return "done"
	default:
		return "in_progress"
	}
}

// slotRuleReaders is the table of the readers of the slot rule, one row for each.
//
// record.PastReview is read by four callers, and each one reads either "holds no slot", which is
// the rule's complement and a row here, or "past the review" in the workflow's sense, which no
// slot rule decides and which is no row:
//
//   - admit's readSlotHolders reads "holds no slot": the repair of a worker with no task reaches an
//     issue past its review because such an issue holds no row to be found by, and an issue whose
//     phase keeps one is reached through its row or waits in the line for one. A row.
//   - projection's atWorkPastReview (why.go) reads "holds no slot": it finds the issue at work
//     that `admission.active` does not name because its phase keeps no slot, and serves it no
//     answer. A row.
//   - classify.ConflictNeedsMerge reads "past the review": the next step of such an issue is its
//     merge, and GitHub merges no head that conflicts with its base, so the merge forward is asked
//     at once (record.ConflictMergeNext) whatever slot the issue holds. No row.
//   - workflow's mergeForwardOnEntry and mergeForwardLanded (merge_forward.go) read "past the
//     review": a move into accepting, retro or merging at a conflicting head asks for the merge
//     because the merge comes next, and a merge forward that lands under an issue past its review
//     asks the reviewer to read it because the issue stands on a review of the older head. No
//     row; the conflict path's own choice is the conflict rows below.
func slotRuleReaders() []slotRuleReader {
	var rows []slotRuleReader
	for _, stands := range phase.All {
		rule := flippingSlotIn(stands)
		keeps := func(rule record.SlotRule) string {
			const key = "ACME-10"
			if slices.Contains(rule.KeepsSlotIn, stands) && record.InWorkflow(record.Issue{Phase: stands, Status: statusIn(stands)}) {
				return key + " holds a slot"
			}
			return key + " holds no slot"
		}
		rows = append(rows, slotRuleReader{
			reader:  "record.PhaseKeepsSlot through record.KeepsSlot: " + string(stands),
			changes: []string{"KeepsSlotIn"},
			rule:    rule,
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const key = "ACME-10"
				acmeRoot(t, pool, key, stands, statusIn(stands), "a", nil, nil)
				takeSlot(t, pool, key)
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return slotWord(t, pool, key)
			},
			main: keeps(record.MainRule), changed: keeps(rule),
			why: "a started issue keeps its slot in a phase the rule keeps one in, and gives it back in any other",
		})
	}
	return append(rows,
		slotRuleReader{
			reader:  "record.NeedsSlot in releaseSlots: the mark of a release for a wait",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Accepting),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, marked = "ACME-20", "ACME-21"
				acmeRoot(t, pool, holder, phase.Implementing, "in_progress", "a", nil, nil)
				takeSlot(t, pool, holder)
				acmeRoot(t, pool, marked, phase.Accepting, "needs_review", "b", nil, nil)
				takeSlot(t, pool, marked)
				releaseForWait(t, pool, marked)
				tickThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "the pass")
				return slotWord(t, pool, marked) + "; " + markWord(t, pool, marked)
			},
			main:    "ACME-21 holds no slot; ACME-21 carries no mark of a release for a wait",
			changed: "ACME-21 holds no slot; ACME-21 carries the mark of a release for a wait",
			why: "an issue released for a wait whose phase needs no slot loses the mark that stands it in the line, " +
				"and one whose phase keeps a slot keeps the mark while the cap is full",
		},
		slotRuleReader{
			reader:  "record.NeedsSlot in record.Waiting",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingNoSlotIn(phase.Implementing),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, returning = "ACME-30", "ACME-31"
				seedSlottedInPhase(t, pool, acmeProject, holder, "a", phase.Admitted, nil)
				acmeRoot(t, pool, returning, phase.Implementing, "in_progress", "b", nil, nil)
				oweMemberStart(t, pool, returning, returning, phase.Implementing)
				tickThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "the pass")
				line, _, _ := lineUnder(t, pool, rule)
				return lineWord(line)
			},
			main:    "the line is [ACME-31]",
			changed: "the line is []",
			why:     "the line offers a returning issue a slot only where its phase keeps one",
		},
		slotRuleReader{
			reader:  "record.HeldReasonOf",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Accepting),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const key = "ACME-40"
				acmeRoot(t, pool, key, phase.Accepting, "needs_review", "a", nil, nil)
				approvedGate(t, pool, key)
				tickThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "the pass")
				_, held, _ := lineUnder(t, pool, rule)
				return heldWord(held, key)
			},
			main:    "ACME-40 is held as at_work",
			changed: "ACME-40 is held as unclassified",
			why:     "an issue standing in a phase that keeps no slot is at work, and one whose phase keeps a slot it holds none of is no word's",
		},
		slotRuleReader{
			reader:  "admit's slotFor",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Accepting),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, carried = "ACME-50", "ACME-51"
				seedSlottedInPhase(t, pool, acmeProject, holder, "a", phase.Admitted, nil)
				acmeCarriedRoot(t, pool, carried, phase.Accepting, "b")
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return slotWord(t, pool, carried) + "; " + treeWord(t, pool, carried)
			},
			main:    "ACME-51 holds no slot; the tree of ACME-51 is open",
			changed: "ACME-51 holds a slot; the tree of ACME-51 is open",
			why:     "a promotion takes a slot for a candidate whose phase keeps one, and none for one whose phase keeps none",
		},
		slotRuleReader{
			reader:  "workflow's startOrOwe mark and the SQL that reads it (outboxInOrder, OwedStarts)",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Retro),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, accepted = "ACME-60", "ACME-61"
				seedSlottedInPhase(t, pool, acmeProject, holder, "a", phase.Admitted, nil)
				acmeRoot(t, pool, accepted, phase.Accepting, "needs_review", "b", nil, nil)
				approvedGate(t, pool, accepted)
				applyThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "api", "sign-off", intake.SignOff{Issue: accepted})
				return phaseWord(t, pool, accepted) + "; " + startsWord(accepted, acmeStarts(t, pool, accepted)) + "; " + owedWord(t, pool, accepted)
			},
			main: "ACME-61 stands in retro; ACME-61 has a start of the implementer carrying a task, which waits for no slot " +
				"and a lane may run; the project owes ACME-61 no start that waits for a slot",
			changed: "ACME-61 stands in retro; ACME-61 has a start of the implementer carrying a task, which waits for a slot " +
				"and no lane may run; the project owes ACME-61 a start that waits for a slot",
			why: "the start of a phase that keeps a slot is marked for one, which holds it unclaimable while the issue holds no slot " +
				"(outboxInOrder) and puts it in the line (OwedStarts); EnqueueStart matches a later start on that same mark",
		},
		slotRuleReader{
			reader:  "blockers.waitsOf: the link wait",
			changes: []string{"LinkWaitGivesBack"},
			rule:    ruleWith("main with a link wait that keeps the slot", func(rule *record.SlotRule) { rule.LinkWaitGivesBack = false }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const waiter, blocker = "ACME-70", "ACME-71"
				acmeRoot(t, pool, waiter, phase.Implementing, "in_progress", "a", []string{blocker}, nil)
				takeSlot(t, pool, waiter)
				seedMirrorIn(t, pool, acmeProject, blocker, "in_progress")
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return slotWord(t, pool, waiter) + "; " + markWord(t, pool, waiter)
			},
			main:    "ACME-70 holds no slot; ACME-70 carries the mark of a release for a wait",
			changed: "ACME-70 holds a slot; ACME-70 carries no mark of a release for a wait",
			why:     "a started issue that waits on an unfinished blocked_by link gives its slot back for the wait only under a rule whose link wait does",
		},
		slotRuleReader{
			reader:  "blockers.waitsOf: the children wait",
			changes: []string{"ChildrenWaitGivesBack"},
			rule:    ruleWith("main with a children wait that keeps the slot", func(rule *record.SlotRule) { rule.ChildrenWaitGivesBack = false }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const parent, child = "ACME-80", "ACME-81"
				acmeRoot(t, pool, parent, phase.Admitted, "in_progress", "a", nil, nil)
				approvedGate(t, pool, parent)
				takeSlot(t, pool, parent)
				openChild(t, pool, child, parent)
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return slotWord(t, pool, parent) + "; " + markWord(t, pool, parent)
			},
			main:    "ACME-80 holds no slot; ACME-80 carries the mark of a release for a wait",
			changed: "ACME-80 holds a slot; ACME-80 carries no mark of a release for a wait",
			why:     "a started parent that waits on its open children gives its slot back for the wait only under a rule whose children wait does",
		},
		slotRuleReader{
			reader:  "the release for a wait: a wait that stops the phase",
			changes: []string{"WaitStopsPhase"},
			rule:    ruleWith("main with a wait that stops the phase", func(rule *record.SlotRule) { rule.WaitStopsPhase = true }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const waiter, blocker = "ACME-90", "ACME-91"
				acmeRoot(t, pool, waiter, phase.Implementing, "in_progress", "a", []string{blocker}, nil)
				takeSlot(t, pool, waiter)
				seedMirrorIn(t, pool, acmeProject, blocker, "in_progress")
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return slotWord(t, pool, waiter) + "; " + stopWord(t, pool, waiter, claim.RoleImplementer)
			},
			main:    "ACME-90 holds no slot; the implementer of ACME-90 goes on",
			changed: "ACME-90 holds no slot; the implementer of ACME-90 is stopped",
			why: "a phase goes on while its issue waits, and only its next start waits, except under a rule whose wait stops the phase: " +
				"there the pass that releases the issue for its wait stops the worker of its phase (a supervise suspend or release of that role)",
		},
		slotRuleReader{
			reader:  "record.ChildWaitsOf",
			changes: []string{"WaitStopsPhase"},
			rule:    ruleWith("main with a wait that stops the phase", func(rule *record.SlotRule) { rule.WaitStopsPhase = true }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const parent, child = "ACME-100", "ACME-101"
				acmeRoot(t, pool, parent, phase.Implementing, "in_progress", "a", nil, nil)
				approvedGate(t, pool, parent)
				takeSlot(t, pool, parent)
				openChild(t, pool, child, parent)
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				_, _, facts := lineUnder(t, pool, rule)
				return childWaitWord(facts, parent) + "; " + slotWord(t, pool, parent)
			},
			main:    "ACME-100 waits on no open child; ACME-100 holds a slot",
			changed: "ACME-100 waits on its open children [ACME-101]; ACME-100 holds no slot",
			why: "a parent whose first phase has not started waits on its open children; under a rule whose wait stops the phase, " +
				"a started parent waits on them in whatever phase it stands, and gives its slot back for that wait",
		},
		slotRuleReader{
			reader:  "record.Waiting's order of the line: returned work by rank",
			changes: []string{"Order"},
			rule:    ruleWith("main with returned work by rank", func(rule *record.SlotRule) { rule.Order = record.LineReturnedByRank }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, older, newer = "ACME-110", "ACME-111", "ACME-112"
				seedSlottedInPhase(t, pool, acmeProject, holder, "a", phase.Admitted, nil)
				acmeRoot(t, pool, older, phase.Implementing, "in_progress", "c", nil, nil)
				oweMemberStart(t, pool, older, older, phase.Implementing)
				acmeRoot(t, pool, newer, phase.Implementing, "in_progress", "b", nil, nil)
				oweMemberStart(t, pool, newer, newer, phase.Implementing)
				tickThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "the pass")
				line, _, _ := lineUnder(t, pool, rule)
				return lineWord(line)
			},
			main:    "the line is [ACME-111 ACME-112]",
			changed: "the line is [ACME-112 ACME-111]",
			why:     "the issues whose work came back stand oldest first, or by rank under a rule that orders them so",
		},
		slotRuleReader{
			reader:  "record.Waiting's order of the line: what started work waits on first",
			changes: []string{"Order"},
			rule:    ruleWith("main with blockers first", func(rule *record.SlotRule) { rule.Order = record.LineBlockersFirst }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const waiter, ranked, blocker = "ACME-120", "ACME-121", "ACME-122"
				acmeRoot(t, pool, waiter, phase.Implementing, "in_progress", "a", []string{blocker}, nil)
				takeSlot(t, pool, waiter)
				acmeQueuedRoot(t, pool, ranked, "b")
				acmeQueuedRoot(t, pool, blocker, "c")
				tickThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "the pass")
				line, _, _ := lineUnder(t, pool, rule)
				return openedWord(t, pool, ranked, blocker) + "; " + lineWord(line)
			},
			main:    "the trees open of [ACME-121 ACME-122] are [ACME-121]; the line is [ACME-120 ACME-122]",
			changed: "the trees open of [ACME-121 ACME-122] are [ACME-122]; the line is [ACME-120 ACME-121]",
			why: "the issues that have not started stand by rank, or with the issues started work waits on first under a rule " +
				"that orders them so, and the slot the waiter gave back goes to the first of them",
		},
		slotRuleReader{
			reader:  "admit's capLets in the promotion",
			changes: []string{"Take"},
			rule:    ruleWith("main with a cover", func(rule *record.SlotRule) { rule.Take = record.TakeCover }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, blocker = "ACME-130", "ACME-131"
				// The holder is a root whose architect writes its design, which keeps its row
				// whatever it waits on (blockers.waitsOf), so under the daemon's own take the cap
				// stays full and a cover is the one choice that starts what it waits on.
				acmeRoot(t, pool, holder, phase.Admitted, "in_progress", "a", []string{blocker}, nil)
				takeSlot(t, pool, holder)
				acmeQueuedRoot(t, pool, blocker, "b")
				tickThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "the pass")
				return treeWord(t, pool, blocker)
			},
			main:    "the tree of ACME-131 is not open",
			changed: "the tree of ACME-131 is open",
			why:     "a full cap lets no start through that needs a slot, save one a cover lets start on the slot of a holder that waits on it",
		},
		slotRuleReader{
			reader:  "admit's capLets in lineCanMove",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Accepting),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const awaiting, carried = "ACME-140", "ACME-141"
				registerControllerFor(t, pool, acmeProject)
				acmeRoot(t, pool, awaiting, phase.AwaitingPerson, "in_progress", "a", nil, nil)
				takeSlot(t, pool, awaiting)
				acmeClaim(t, claimStore(t, pool), awaiting, awaiting, claim.RoleArchitect, supervise.StateIdle)
				acmeCarriedRoot(t, pool, carried, phase.Accepting, "b")
				settled := settleMovements(t, pool, awaiting)
				admission := ruledAdmission(t, 1, rule, silent())
				admission.UseStalledTreeNotice(deploymentBound(t, ""))
				admission.now = func() time.Time { return settled.Add(7 * time.Hour) }
				tickThrough(t, pool, admission, "the measure")
				return stalledWord(stalledRoots(t, pool))
			},
			main:    "the controller is told of no stopped tree",
			changed: "the controller is told that [ACME-140] stopped",
			why: "the stall measure's free-slot term asks the cap what the promotion asks it: a carried root whose phase keeps no slot " +
				"opens under a full cap and the line moves, and one whose phase keeps a slot is held by the cap and the line is stopped",
		},
		slotRuleReader{
			reader:  "the conflict path: move back by conflict",
			changes: []string{"Conflict"},
			rule:    ruleWith("main with a conflict that moves back", func(rule *record.SlotRule) { rule.Conflict = record.ConflictMovesBack }),
			outcome: conflictOutcome,
			main:    "ACME-150 stands in testing; no phase move was logged",
			changed: "ACME-150 stands in implementing; the phase moved by conflict",
			why:     "a confirmed conflict keeps the phase and merges the base forward, or moves the issue back to implementing by the rule's trigger",
		},
		slotRuleReader{
			reader:  "the conflict path: move back by checks_red",
			changes: []string{"Conflict"},
			rule:    ruleWith("main with a conflict that moves back by red checks", func(rule *record.SlotRule) { rule.Conflict = record.ConflictChecksRed }),
			outcome: conflictOutcome,
			main:    "ACME-150 stands in testing; no phase move was logged",
			changed: "ACME-150 stands in implementing; the phase moved by checks_red",
			why:     "a confirmed conflict keeps the phase and merges the base forward, or moves the issue back to implementing by the rule's trigger",
		},
		slotRuleReader{
			// The point inside merging where the slot goes back is a choice only of a rule that
			// keeps a slot in merging, so this row's rule keeps one there and gives it back at the
			// merger's ready mark: the two choices together, which the reverted runs of
			// TestTheChoicesOfARuleAreTheReadersRows tell apart.
			reader:  "the release at the merger's ready mark",
			changes: []string{"KeepsSlotIn", "ReleaseAtReady"},
			rule: ruleWith("main with a slot in merging until the ready mark", func(rule *record.SlotRule) {
				rule.KeepsSlotIn, rule.ReleaseAtReady = append(rule.KeepsSlotIn, phase.Merging), true
			}),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const ready, merging = "ACME-160", "ACME-161"
				acmeRoot(t, pool, ready, phase.Merging, "retro", "a", nil, nil)
				takeSlot(t, pool, ready)
				mergerReady(t, pool, ready)
				acmeRoot(t, pool, merging, phase.Merging, "retro", "b", nil, nil)
				takeSlot(t, pool, merging)
				tickThrough(t, pool, ruledAdmission(t, 3, rule, silent()), "the pass")
				return slotWord(t, pool, ready) + "; " + slotWord(t, pool, merging)
			},
			main:    "ACME-160 holds no slot; ACME-161 holds no slot",
			changed: "ACME-160 holds no slot; ACME-161 holds a slot",
			why: "merging keeps no slot under the daemon's own rule; under a rule that keeps one there until the merger's ready mark, " +
				"the issue whose merger has completed its merging gives it back and the one whose merger has not keeps it",
		},
		slotRuleReader{
			reader:  "record.PastReview in readSlotHolders",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Retro),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const root, member = "ACME-170", "ACME-171"
				acmeTreeAndMember(t, pool, root, member, phase.Retro)
				token := acmeWorkerClaim(t, pool, root, member, claim.RoleImplementer, supervise.StateSuspended, supervise.SuspendedByDaemon)
				inTx(t, pool, func(tx pgx.Tx) {
					if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: member,
						Role: claim.RoleImplementer, Claim: token, Entry: seededEntry, EntryTask: seededEntryTask(member),
						EntryRaisedAt: fixedNow}); err != nil {
						t.Fatalf("record the implementer's phases row of %s: %v", member, err)
					}
				})
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return startsWord(member, acmeStarts(t, pool, member))
			},
			main:    "ACME-171 has a start of the implementer carrying a task, which waits for no slot and a lane may run",
			changed: "ACME-171 has no start",
			why: "the repair of a worker with no task reaches an issue that holds no slot because its phase keeps none; " +
				"an issue whose phase keeps a slot it does not hold waits in the line for one, and the repair leaves it there",
		},
		slotRuleReader{
			reader:  "record.PastReview in projection's atWorkPastReview",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Retro),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const key = "ACME-180"
				acmeRoot(t, pool, key, phase.Retro, "retro", "a", nil, nil)
				approvedGate(t, pool, key)
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return whyWord(t, pool, rule, key)
			},
			main:    "ACME-180 is served no why",
			changed: "ACME-180 is served why unlisted",
			why: "an issue at work in a phase that keeps no slot is in none of the lists that place an issue and is served no answer; " +
				"one whose phase keeps a slot it does not hold is unlisted",
		},
	)
}

// conflictOutcome is the outcome of both rows of the conflict path: a root in testing holding its
// slot, whose open pull request's head GitHub reads conflicting twice a minute apart against a base
// that requires a check the head has not passed, so the conflict is confirmed and needs the merge
// (workflow's mergeability, classify.ConflictNeedsMerge). The engine's own log says which move it
// made ("workflow: phase changed").
func conflictOutcome(t *testing.T, rule record.SlotRule) string {
	t.Helper()
	pool := migratedPool(t)
	const key = "ACME-150"
	acmeRoot(t, pool, key, phase.Testing, "testing", "a", nil, nil)
	takeSlot(t, pool, key)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: key,
			Repo: "acme/widgets", Number: 150, Branch: "legion/" + key, HeadSHA: "head", Base: "main", Failing: []string{}}); err != nil {
			t.Fatalf("seed the pull request of %s: %v", key, err)
		}
	})
	var logged bytes.Buffer
	admission := ruledAdmission(t, 2, rule, slog.New(slog.NewJSONHandler(&logged, nil)))
	for _, after := range []time.Duration{0, time.Minute} {
		applyThrough(t, pool, admission, "github", "conflict-"+after.String(), intake.PullRequestMergeability{Repo: "acme/widgets",
			Number: 150, Base: "main", HeadSHA: "head", Mergeable: record.MergeabilityConflicting, MergeState: record.MergeStateDirty,
			ReadAt: fixedNow.Add(after), Rules: &record.BaseRules{Required: []string{"ci"}}})
	}
	return phaseWord(t, pool, key) + "; " + movesWord(phaseMoves(t, &logged))
}

// silent is a logger that writes nowhere, for the rows that read records and outbox rows alone.
func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ruledAdmission is an admission of cap over acmeProject whose pass and whose engine run rule:
// the pass reads it (UseSlotRule), and so do the engine's table and the slot mark of each start it
// writes (workflow.Config.SlotRule). It is newAdmissionForProject with the rule given to both, on
// the same fixed clock, the same repositories and the deployment's single design step. log is
// where both write.
func ruledAdmission(t *testing.T, cap int, rule record.SlotRule, log *slog.Logger) *Admission {
	t.Helper()
	engine := workflow.New(record.NewStore(), workflow.Config{Project: acmeProject, Repositories: testRepositories,
		Steps: config.DesignGate("").Steps(), Clock: func() time.Time { return fixedNow }, SlotRule: rule}, log)
	admission := New(record.NewStore(), engine, cap, acmeProject, log)
	admission.now = func() time.Time { return fixedNow }
	admission.UseSlotRule(rule)
	return admission
}

// applyThrough applies fact as source's event id through the daemon's own chain of handlers
// (Handlers): the mirror, then the engine, then admission, whose pass runs in the fact's one
// transaction.
func applyThrough(t *testing.T, pool *pgxpool.Pool, admission *Admission, source, id string, fact intake.Fact) {
	t.Helper()
	result, err := intake.ApplyFact(context.Background(), pool, source, id, fact,
		Handlers(record.NewStore(), acmeProject, admission.engine, admission)...)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply %s = %+v, %v", id, result.Refusal, err)
	}
}

// tickThrough is one controller tick through the daemon's own chain of handlers, which is one
// admission pass.
func tickThrough(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string) {
	t.Helper()
	applyThrough(t, pool, admission, "controller", id, intake.ControllerTick{})
}

// acmeRoot records key as the root of a running tree of its own, standing in stands with the
// Dispatch status status: its record, its mirror row naming waits as the issues it is blocked by,
// and its tree lifecycle, open under the workflow. A root waiting on a person carries the ask it
// waits on, as the stored shape requires, and shape is where a row says what else the record
// carries. It is phases_up_to_review_test.go's rowRuleRoot under acmeProject, and like it writes no
// slot: a row that gives the root one takes it with takeSlot.
func acmeRoot(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase, status, rank string, waits []string, shape func(*record.Issue)) {
	t.Helper()
	root := record.Issue{Key: key, Tree: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: acmeProject, Title: key, Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: rank,
		HandedOver: true, LastDispatchSeq: 1}
	if stands == phase.AwaitingPerson {
		root.Awaiting = &record.Awaiting{Ask: "ask-" + key, Since: record.EventMark{Seq: 5}}
	}
	if shape != nil {
		shape(&root)
	}
	putIssue(t, pool, root)
	seedMirrorIn(t, pool, acmeProject, key, status, waits...)
	seedLifecycleForProject(t, pool, acmeProject, key)
}

// acmeQueuedRoot records key as a root handed to Legion that has never run, waiting at todo for
// its tree to be opened, with its mirror row.
func acmeQueuedRoot(t *testing.T, pool *pgxpool.Pool, key, rank string) {
	t.Helper()
	seedWaitingForProject(t, pool, acmeProject, key, rank)
	seedMirrorIn(t, pool, acmeProject, key, "todo")
}

// acmeCarriedRoot is capfull_resume_test.go's carriedRoot under acmeProject: a root another tree
// handed to Legion carrying the phase it ran there, with a tree of its own still to be opened.
func acmeCarriedRoot(t *testing.T, pool *pgxpool.Pool, key string, carries phase.Phase, rank string) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: key, Tree: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: acmeProject, Title: "a carried root", Phase: carries, Generation: 2, Status: "todo", Rank: rank,
		HandedOver: true, HandedOverFrom: "ACME-700"})
	seedMirrorIn(t, pool, acmeProject, key, "todo")
}

// acmeTreeAndMember is slot_repair_test.go's seedTreeAndMember under acmeProject: root waiting in
// accepting with no row of its own, its tree lifecycle open, and the one member of it, key,
// standing in stands.
func acmeTreeAndMember(t *testing.T, pool *pgxpool.Pool, root, key string, stands phase.Phase) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: acmeProject, Title: "the root of " + key, Tree: root, Phase: phase.Accepting, Generation: 1,
		Status: "needs_review", DispatchStatus: "needs_review", Rank: root, HandedOver: true, LastDispatchSeq: 1})
	seedLifecycleForProject(t, pool, acmeProject, root)
	parent := root
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: acmeProject, Title: "the member " + key, Tree: root, Parent: &parent, Phase: stands, Generation: 1,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: key, LastDispatchSeq: 2})
}

// acmeWorkerClaim is slot_repair_test.go's putWorkerClaim under acmeProject, at generation 1: the
// claim of key's role on the tree root, in state and suspended by by, and its token.
func acmeWorkerClaim(t *testing.T, pool *pgxpool.Pool, root, key string, role claim.Role, state supervise.ClaimState,
	by supervise.SuspendSource,
) claim.Token {
	t.Helper()
	token := acmeToken(t, key, role)
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
		serving_generation, last_start_row, launch_failures, prompt_failures, prompt_retires, uncertain_streak, suspended_by)
		values ($1, $2, $3, $4, $5, 1, 'ses_worker', '/sessions/ses_worker.jsonl', $6, 1, 0, 0, 0, 0, 0, $7)`,
		string(token), acmeProjectToken(t), root, key, string(role), string(state), string(by)); err != nil {
		t.Fatalf("record the %s %s claim of %s: %v", state, role, key, err)
	}
	return token
}

// acmeClaim is tree_stalled_test.go's seedClaim under acmeProject: the role claim of issue in
// state, with no step of its own, written through the daemon's own store.
func acmeClaim(t *testing.T, records *store.Store, tree, issue string, role claim.Role, state supervise.ClaimState) {
	t.Helper()
	if err := records.PutClaim(context.Background(), supervise.Claim{Token: acmeToken(t, issue, role), Project: acmeProjectToken(t),
		Tree: tree, Issue: issue, Role: role, TreeEpoch: 1, Generation: 1, State: state}); err != nil {
		t.Fatalf("record the %s claim of %s in state %s: %v", role, issue, state, err)
	}
}

// acmeProjectToken is acmeProject as the claim-keyed tables spell it (claim.ProjectToken).
func acmeProjectToken(t *testing.T) string {
	t.Helper()
	project, err := claim.ProjectToken(acmeProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", acmeProject, err)
	}
	return project
}

// acmeToken is the claim token of key's role under acmeProject.
func acmeToken(t *testing.T, key string, role claim.Role) claim.Token {
	t.Helper()
	token, err := claim.NewToken(acmeProjectToken(t), key, role)
	if err != nil {
		t.Fatalf("the %s token of %s: %v", role, key, err)
	}
	return token
}

// approvedGate registers root's design gate with a person's approval on its document's current
// version, so the gate stands open and has opened (record.TreeGate.EverOpened).
func approvedGate(t *testing.T, pool *pgxpool.Pool, root string) {
	t.Helper()
	seedGate(t, pool, record.DesignGate{Issue: root, ArtifactID: "spec-" + root, LatestVersion: 1, ApprovedVersion: new(1)})
}

// openChild writes what Dispatch says of a child of parent that the daemon has never recorded:
// filed in backlog and not released, which is work of the parent's still to happen
// (record.ChildOpen).
func openChild(t *testing.T, pool *pgxpool.Pool, key, parent string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		reading := record.DispatchReading{Key: key, Project: acmeProject, Seq: 1, Status: record.Carry("backlog"),
			Parent: record.Carry(parent), Blockers: record.Carry([]string(nil))}
		if err := record.NewStore().PutDispatchIssue(context.Background(), tx, reading); err != nil {
			t.Fatalf("seed the mirror of %s: %v", key, err)
		}
	})
}

// releaseForWait takes key's slot back for a wait, as the pass's release for a wait does: the row
// goes and the mark that says why is written with it (record.Store.ReleaseSlotForWait).
func releaseForWait(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().ReleaseSlotForWait(context.Background(), tx, key, fixedNow.Add(-time.Hour)); err != nil {
			t.Fatalf("release the slot of %s for a wait: %v", key, err)
		}
	})
}

// mergerReady records the merger's ready mark on key: its completion of merging, which records the
// head it stands on on the merger's phases row (record.PhaseRow.HandoffCommit) while the issue
// stays in merging for the merge itself — the READY head the cutover repair reads
// (workflow's cutoverMergerRepair).
func mergerReady(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: key, Role: claim.RoleMerger,
			Claim: acmeToken(t, key, claim.RoleMerger), HandoffCommit: "ready-" + key, CompletedAt: fixedNow}); err != nil {
			t.Fatalf("record the merger's completion of %s: %v", key, err)
		}
	})
}

// lineUnder is the waiting line of acmeProject as a pass under rule judges it, read after the pass
// from the stored state as the pass reads it — the live records, the line facts with rule on them
// (record.LineFacts.Rule), and blockers.JudgeLine over this project's own slots — with the rows it
// leaves behind, each with its word, and the facts with every judgement filled.
func lineUnder(t *testing.T, pool *pgxpool.Pool, rule record.SlotRule) ([]record.LineEntry, []record.HeldIssue, record.LineFacts) {
	t.Helper()
	var line []record.LineEntry
	var held []record.HeldIssue
	var facts record.LineFacts
	inTx(t, pool, func(tx pgx.Tx) {
		records, ctx := record.NewStore(), context.Background()
		issues, err := records.LiveIssues(ctx, tx, acmeProject)
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			t.Fatalf("list the slots: %v", err)
		}
		if facts, err = record.ReadLineFacts(ctx, tx, records, acmeProject, issues, config.DesignGate("").Steps()); err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		facts.Rule = rule
		if _, facts, line, held, err = blockers.JudgeLine(ctx, tx, records, acmeProject, issues, ownSlots(issues, slots), facts, nil, fixedNow); err != nil {
			t.Fatalf("judge the waiting line: %v", err)
		}
	})
	return line, held, facts
}

// acmeStarts is owed_start_test.go's startsOf under acmeProject: every supervise start row of
// issue, oldest first, with whether a lane may run it now (record.Store.DueRows, over the outbox's
// own predicate outboxInOrder).
func acmeStarts(t *testing.T, pool *pgxpool.Pool, issue string) []midPhaseStart {
	t.Helper()
	var starts []midPhaseStart
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		due, err := record.NewStore().DueRows(ctx, tx, acmeProject, issue, []record.OutboxKind{record.OutboxKindSupervise}, fixedNow)
		if err != nil {
			t.Fatalf("list what a lane may run for %s: %v", issue, err)
		}
		runnable := make(map[int64]bool, len(due))
		for _, row := range due {
			runnable[row.ID] = true
		}
		rows, err := tx.Query(ctx, `select id, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
			coalesce(payload->>'needsSlot', '') = 'true'
			from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
		if err != nil {
			t.Fatalf("list the starts of %s: %v", issue, err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var start midPhaseStart
			if err := rows.Scan(&id, &start.role, &start.task, &start.needsSlot); err != nil {
				t.Fatalf("scan a start of %s: %v", issue, err)
			}
			start.claimable = runnable[id]
			starts = append(starts, start)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the starts of %s: %v", issue, err)
		}
	})
	return starts
}

// acmeTreeOpen says whether key's tree lifecycle stands open under the workflow's authority, as
// capfull_resume_test.go's treeOpen does under testProject.
func acmeTreeOpen(t *testing.T, pool *pgxpool.Pool, key string) bool {
	t.Helper()
	var open bool
	inTx(t, pool, func(tx pgx.Tx) {
		lifecycles, err := record.NewStore().TreeLifecycles(context.Background(), tx, acmeProjectToken(t), []string{key})
		if err != nil {
			t.Fatalf("read the tree lifecycles: %v", err)
		}
		life, found := lifecycles[key]
		open = found && life.Authority == treelifecycle.AuthorityWorkflow
	})
	return open
}

// phaseMoves is the trigger of each phase move the engine logged, in order, read from the JSON
// lines it wrote ("workflow: phase changed").
func phaseMoves(t *testing.T, logged *bytes.Buffer) []string {
	t.Helper()
	var moves []string
	for _, line := range bytes.Split(logged.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode the log line %q: %v", line, err)
		}
		if entry["msg"] == "workflow: phase changed" {
			moves = append(moves, fmt.Sprint(entry["trigger"]))
		}
	}
	return moves
}

// slotWord says whether key holds one of the cap's slots.
func slotWord(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	if holdsSlot(t, pool, key) {
		return key + " holds a slot"
	}
	return key + " holds no slot"
}

// markWord says whether key carries the mark of a release for a wait (record.Issue.ReleasedForWaitAt).
func markWord(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	if issue(t, pool, key).ReleasedForWaitAt != nil {
		return key + " carries the mark of a release for a wait"
	}
	return key + " carries no mark of a release for a wait"
}

// phaseWord is the phase key's record stands in.
func phaseWord(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	return key + " stands in " + string(issue(t, pool, key).Phase)
}

// treeWord says whether key's tree stands open.
func treeWord(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	if acmeTreeOpen(t, pool, key) {
		return "the tree of " + key + " is open"
	}
	return "the tree of " + key + " is not open"
}

// openedWord is which of keys have a tree standing open.
func openedWord(t *testing.T, pool *pgxpool.Pool, keys ...string) string {
	t.Helper()
	var open []string
	for _, key := range keys {
		if acmeTreeOpen(t, pool, key) {
			open = append(open, key)
		}
	}
	return "the trees open of [" + strings.Join(keys, " ") + "] are [" + strings.Join(open, " ") + "]"
}

// lineWord is the keys of a waiting line, in its order.
func lineWord(line []record.LineEntry) string {
	keys := make([]string, 0, len(line))
	for _, entry := range line {
		keys = append(keys, entry.Issue.Key)
	}
	return "the line is [" + strings.Join(keys, " ") + "]"
}

// heldWord is the word the rows a judgement left behind give key: the first row of key, as the
// held list keeps it (record.HeldList).
func heldWord(held []record.HeldIssue, key string) string {
	for _, row := range held {
		if row.Issue == key {
			return key + " is held as " + string(row.Reason)
		}
	}
	return key + " is held by no word"
}

// childWaitWord is the open children the children-first judgement of facts says key waits on.
func childWaitWord(facts record.LineFacts, key string) string {
	var open []string
	for _, child := range facts.ChildWaits[key].Open {
		open = append(open, child.Key)
	}
	if len(open) == 0 {
		return key + " waits on no open child"
	}
	return key + " waits on its open children [" + strings.Join(open, " ") + "]"
}

// startsWord is the supervise starts key holds, each with the role it starts, whether it carries a
// task, whether it waits for a slot and whether a lane may run it now.
func startsWord(key string, starts []midPhaseStart) string {
	if len(starts) == 0 {
		return key + " has no start"
	}
	described := make([]string, 0, len(starts))
	for _, start := range starts {
		task, slot, lane := "no task", "waits for no slot", "a lane may run"
		if start.task != "" {
			task = "a task"
		}
		if start.needsSlot {
			slot = "waits for a slot"
		}
		if !start.claimable {
			lane = "no lane may run"
		}
		described = append(described, fmt.Sprintf("a start of the %s carrying %s, which %s and %s", start.role, task, slot, lane))
	}
	return key + " has " + strings.Join(described, "; ")
}

// owedWord says whether the project owes key a start that waits for a slot
// (record.Store.OwedStarts, the read the line's returned part is built from).
func owedWord(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	var owes bool
	inTx(t, pool, func(tx pgx.Tx) {
		owed, err := record.NewStore().OwedStarts(context.Background(), tx, acmeProject)
		if err != nil {
			t.Fatalf("list the starts %s owes: %v", acmeProject, err)
		}
		owes = slices.ContainsFunc(owed, func(start record.OwedStart) bool { return start.Issue == key })
	})
	if owes {
		return "the project owes " + key + " a start that waits for a slot"
	}
	return "the project owes " + key + " no start that waits for a slot"
}

// stopWord says whether the outbox holds a stop of role on key: a supervise suspend or release of
// that role, the two acts that stop a worker's process and keep its session.
func stopWord(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) string {
	t.Helper()
	var stops int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' in ('suspend', 'release') and payload->>'role' = $2`, key, string(role)).Scan(&stops); err != nil {
		t.Fatalf("count the stops of the %s of %s: %v", role, key, err)
	}
	if stops > 0 {
		return "the " + string(role) + " of " + key + " is stopped"
	}
	return "the " + string(role) + " of " + key + " goes on"
}

// movesWord is the triggers of the phase moves the engine logged.
func movesWord(moves []string) string {
	if len(moves) == 0 {
		return "no phase move was logged"
	}
	return "the phase moved by " + strings.Join(moves, ", ")
}

// stalledWord is the roots the tree-stalled notices name.
func stalledWord(roots []string) string {
	if len(roots) == 0 {
		return "the controller is told of no stopped tree"
	}
	return "the controller is told that [" + strings.Join(roots, " ") + "] stopped"
}

// whyWord is the answer of `why` the state, read under rule, serves key (projection.Project).
func whyWord(t *testing.T, pool *pgxpool.Pool, rule record.SlotRule, key string) string {
	t.Helper()
	var state string
	inTx(t, pool, func(tx pgx.Tx) {
		projected, err := projection.Project(context.Background(), projection.Inputs{Tx: tx, Store: record.NewStore(),
			Project: acmeProject, Steps: config.DesignGate("").Steps(), Now: fixedNow, SlotRule: rule})
		if err != nil {
			t.Fatalf("project the state: %v", err)
		}
		if why := projected.Issues[key].Why; why != nil {
			state = string(why.State)
		}
	})
	if state == "" {
		return key + " is served no why"
	}
	return key + " is served why " + state
}
