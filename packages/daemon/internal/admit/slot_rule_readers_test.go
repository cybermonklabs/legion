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
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// acmeProject is the Dispatch project the tests of this package that record their issues under a
// project of their own record them under.
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
// that changes that row's choice alone, given to the engine (workflow.Config.SlotRule), whose rule
// the admission pass reads — and reads back the outcome the choice decides: a slot held or given
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

// slotRuleReaders is the table of the readers of the slot rule, one row for each, but the grace's
// reader, which has one row for each grace a kept rule names (keptWaitGraces), so each kept rule
// with a grace finds a row whose choice it makes the row's way.
//
// "Holds no slot because its phase keeps none" is the rule's complement over the working phases
// (record.SlotRule.InSlotlessPhase), and its two readers are rows here. The questions that sound
// like it but are the workflow's own are no rows, since no slot rule decides them:
//
//   - admit's readSlotHolders reads "holds no slot": the repair of a worker with no task reaches an
//     issue in a phase that keeps none because such an issue holds no row to be found by, and an
//     issue whose phase keeps one is reached through its row or waits in the line for one. A row.
//   - projection's atWorkInSlotlessPhase (why.go) reads "holds no slot": it finds the issue at work
//     that `admission.active` does not name because its phase keeps no slot, and serves it no
//     answer. A row.
//   - classify.MergeComesNext reads "the merge is the next step on the task's own line": the line
//     of the issue's plan says it, whatever slot the issue holds. No row.
//   - classify.ConflictNeedsMergeUnder, record.SlotRule.MergeForwardAsker and workflow's
//     mergeForwardOnEntry read the rule's conflict path past the review (record.SlotRule.Conflict):
//     under the daemon's own a conflict in accepting or retro waits for merging and one in merging
//     asks the merger (cybermonklabs/legion#4979), and under another path a conflict in accepting,
//     retro or merging moves the issue back or asks the implementer at once. Rows: the four rows of
//     the conflict path past the review below, one for each path, each a confirmed conflict in each
//     of the three phases (pastReviewConflictOutcome).
//   - workflow's mergeForwardLanded (merge_forward.go) reads the issue's own walk: a merge forward
//     that lands under an issue whose reviewer step already ran asks the reviewer to read it,
//     because the issue stands on a review of the older head. The task's line decides it, and no
//     rule does. No row.
//   - workflow's startHeldRetro and beneathRetro (retro_hold.go) read the workflow: whether the
//     parent is still in the workflow (record.InWorkflow) and whether its task's last step is the
//     owner's acceptance. The hold before the retro stands in place of a phase that keeps no slot
//     under the daemon's own rule, and under a rule that keeps one there the parent holds it as
//     any held issue holding one does (the KeepsSlotIn row of held above). No row.
//
// The return of a re-admitted tree's members (cybermonklabs/legion#5303) adds or changes readers
// beside the rule that read none of its choices, so none of them is a row:
//
//   - record.Waiting's hold of the members of a tree whose root waits for the promotion that opens
//     it (record.AwaitsPromotion, read once for the line and for record.FirstPhaseStartsNow), and
//     of a root whose label a person took off while its members are still owed
//     (record.Issue.MembersOwed): the root's design gate, its tree's lifecycle and the mark decide
//     it, and the slot row it reads says only whether the root was promoted already, whichever
//     phases a rule keeps a slot in. No row.
//   - record.HeldReasonOf's tree_closed word for those members (reopensAtPromotion) reads the same
//     gate and lifecycle with no slot term. No row.
//   - blockers.waitsOf, blockers.CoverWaits, record.LineFacts.IdleReturns and the stalled-tree
//     measure (lineHolds.waitsByDesign) read the queued-root rule over the root's tree lifecycle
//     beside its gate (record.QueuedRoot): which roots wait for their tree to be opened, not which
//     phases keep a slot. No row.
//   - admit's releaseTreeSlots frees every row of a re-admitted root's tree and clears each mark
//     of a release for a wait: the rows releaseSlots frees for a tree that lingers under every
//     rule, since record.LineFacts.NeedsSlot answers false under a root that lingers whatever
//     phases the rule keeps a slot in. No row.
//   - admit's returnMembersOfRunningTrees and promotedByEarlierRelease read the mark, the tree's
//     lifecycle and gate (record.MembersReturnOwed), a slot row, the mark of a release for a wait
//     and whether the root stands past admitted, which every phase past admitted answers alike
//     under every rule; the earlier release it names ran the daemon's own. No row.
//
// The line by priority first (cybermonklabs/legion#5365) adds one choice of the rule and readers
// beside it:
//
//   - record.Waiting's band by priority reads the rule's choice (record.SlotRule.PriorityFirst),
//     on in the daemon's own rule and off in the rules the recorded windows ran, and sets each
//     entry's priority (record.LineEntry.Priority) under a rule that bands the line and
//     NoPriority under one that does not. A row.
//   - projection's waiting line (Project's band counts, the wire's `priority` and
//     waitingLineReasonUnder) reads the same choice through the rule the state is read under:
//     the counts of a band and the priority of each member come off the entries, and the
//     sentence names the priority that placed a member only under a rule that bands the line.
//     A row.
//   - R10's start outside the cap (record.SlotRule.StartsNow) is read through one seam in record
//     (record.SlotRule.StartsOutsideCap and record.SlotRule.CapCounted, over record.HoldersOf), by four
//     readers, each with rows under main, which starts a P0 outside the cap since
//     cybermonklabs/legion#5488, and under before-p0, the kept rule that changes that choice alone:
//     admit's capLets, in the promotion and in lineCanMove, which lets a P0 entry through under
//     the P0 bound and no other through but under the cap; promoteHolds, whose count of the
//     holders leaves a P0 holder out of the cap's and whose line of each start and refusal names
//     it; lineCanMove, whose count of the holders is the promotion's; and slotFree, which counts
//     neither a P0 holder nor a P0 candidate against the cap. The
//     intake's decode, admit's record of each observation and the engine's write of a fact carry
//     the priority to the record and read no rule. No row.
func slotRuleReaders() []slotRuleReader {
	var rows []slotRuleReader
	for _, stands := range phase.All {
		rule := flippingSlotIn(stands)
		const key = "ACME-10"
		keeps := func(rule record.SlotRule) string {
			if slices.Contains(rule.KeepsSlotIn, stands) && record.InWorkflow(record.Issue{Phase: stands, Status: statusIn(stands)}) {
				return key + " holds a slot"
			}
			return key + " holds no slot"
		}
		rows = append(rows, slotRuleReader{
			reader:  "record.SlotRule.PhaseKeepsSlot through record.SlotRule.KeepsSlot: " + string(stands),
			changes: []string{"KeepsSlotIn"},
			rule:    rule,
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				var waiting func(*record.Issue)
				if stands == phase.AwaitingChildren {
					// The phase's shape check holds its held start beside it, and an open child
					// keeps the parent waiting through the pass, which would otherwise take it back.
					waiting = func(issue *record.Issue) {
						issue.ChildrenWait = &record.ChildrenWait{Start: phase.Implementing, Task: "Implement " + key + ".",
							Reason: "a child is open", Since: fixedNow.Add(-time.Hour)}
					}
					openChild(t, pool, "ACME-11", key)
				}
				acmeRoot(t, pool, key, stands, statusIn(stands), "a", nil, waiting)
				takeSlot(t, pool, key)
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return slotWord(t, pool, key)
			},
			main: keeps(record.MainRule), changed: keeps(rule),
			why: "a started issue keeps its slot in a phase the rule keeps one in, and gives it back in any other",
		})
	}
	for _, grace := range keptWaitGraces() {
		rows = append(rows, slotRuleReader{
			reader:  "blockers.waitsOf: a wait within a grace of " + grace.String(),
			changes: []string{"WaitGrace"},
			rule:    ruleWith("main with a wait grace of "+grace.String(), func(rule *record.SlotRule) { rule.WaitGrace = grace }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const waiter, blocker = "ACME-75", "ACME-76"
				acmeRoot(t, pool, waiter, phase.Implementing, "in_progress", "a", []string{blocker}, nil)
				takeSlot(t, pool, waiter)
				seedMirrorIn(t, pool, acmeProject, blocker, "in_progress")
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return slotWord(t, pool, waiter) + "; " + markWord(t, pool, waiter)
			},
			main:    "ACME-75 holds no slot; ACME-75 carries the mark of a release for a wait",
			changed: "ACME-75 holds a slot; ACME-75 carries no mark of a release for a wait",
			why: "a started issue that waits on another issue gives its slot back at the pass that finds the wait, and under a rule " +
				"with a grace keeps it while the wait is younger than the grace",
		})
	}
	rows = append(rows,
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
				releaseForWaitAt(t, pool, marked, fixedNow.Add(-time.Hour))
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
			reader:  "record.HeldReasonOf: the word of a stopped task",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Accepting),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const key = "ACME-45"
				acmeRoot(t, pool, key, phase.Held, "needs_review", "a", nil, func(root *record.Issue) {
					root.Hold = &record.Hold{From: phase.Accepting, Reason: record.HoldTaskStopped}
				})
				approvedGate(t, pool, key)
				tickThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "the pass")
				_, held, _ := lineUnder(t, pool, rule)
				return heldWord(held, key)
			},
			main:    "ACME-45 is held as task_stopped",
			changed: "ACME-45 is held as unclassified",
			why: "a task stopped where its issue keeps no slot reads task_stopped, and one stopped where its phase keeps a slot " +
				"stands in the active set, so an issue holding none there is no word's",
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
				carriedRootForProject(t, pool, acmeProject, carried, phase.Accepting, "b")
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
				applyThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "api", "sign-off", signOffOf(t, accepted))
				return phaseWord(t, pool, accepted) + "; " + startsWord(accepted, startsOfForProject(t, pool, acmeProject, accepted)) + "; " +
					owedWord(t, pool, accepted)
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
			reader:  "the admission pass's stop of a started parent: a wait that stops the phase",
			changes: []string{"WaitStopsPhase"},
			rule:    ruleWith("main with no wait that stops the phase", func(rule *record.SlotRule) { rule.WaitStopsPhase = false }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const parent, child = "ACME-90", "ACME-91"
				acmeRoot(t, pool, parent, phase.Implementing, "in_progress", "a", nil, nil)
				approvedGate(t, pool, parent)
				takeSlot(t, pool, parent)
				openChild(t, pool, child, parent)
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return phaseWord(t, pool, parent) + "; " + slotWord(t, pool, parent) + "; " + stopWord(t, pool, parent, claim.RoleImplementer)
			},
			main:    "ACME-90 stands in awaiting_children; ACME-90 holds no slot; the implementer of ACME-90 is stopped",
			changed: "ACME-90 stands in implementing; ACME-90 holds a slot; the implementer of ACME-90 goes on",
			why: "a started parent with an open Dispatch child moves into awaiting_children, which keeps no slot, and its worker's task is retired, " +
				"under a rule whose wait stops the phase (cybermonklabs/legion#4592); under a rule whose wait stops nothing its phase goes on",
		},
		slotRuleReader{
			reader:  "record.SlotRule.StartedParents through record.HeldReasonOf: awaiting_children",
			changes: []string{"WaitStopsPhase"},
			rule:    ruleWith("main with no wait that stops the phase", func(rule *record.SlotRule) { rule.WaitStopsPhase = false }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const parent, child = "ACME-100", "ACME-101"
				acmeRoot(t, pool, parent, phase.Implementing, "in_progress", "a", nil, nil)
				approvedGate(t, pool, parent)
				takeSlot(t, pool, parent)
				openChild(t, pool, child, parent)
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				_, held, facts := lineUnder(t, pool, rule)
				return heldWord(held, parent) + "; " + childWaitWord(facts, parent)
			},
			main:    "ACME-100 is held as awaiting_children; ACME-100 waits on no open child",
			changed: "ACME-100 is held as unclassified; ACME-100 waits on no open child",
			why: "a started parent's open children are judged apart from the children-first wait of a first phase (record.SlotRule.StartedParents), " +
				"and the parent the admission pass moved into awaiting_children by them is held by that word, under a rule whose wait stops the phase alone",
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
			reader:  "record.Waiting's order of the line: returned work by first slot",
			changes: []string{"Order"},
			rule:    ruleWith("main with returned work by first slot", func(rule *record.SlotRule) { rule.Order = record.LineReturnedByFirstSlot }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, returnedFirst, startedFirst = "ACME-115", "ACME-116", "ACME-117"
				seedSlottedInPhase(t, pool, acmeProject, holder, "a", phase.Admitted, nil)
				acmeRoot(t, pool, returnedFirst, phase.Implementing, "in_progress", "b", nil, nil)
				oweMemberStart(t, pool, returnedFirst, returnedFirst, phase.Implementing)
				acmeRoot(t, pool, startedFirst, phase.Implementing, "in_progress", "c", nil, nil)
				oweMemberStart(t, pool, startedFirst, startedFirst, phase.Implementing)
				admission := ruledAdmission(t, 2, rule, silent())
				admission.UseRuleTables(firstSlotTables{returnedFirst: fixedNow.Add(-time.Hour), startedFirst: fixedNow.Add(-3 * time.Hour)})
				tickThrough(t, pool, admission, "the pass")
				return slotWord(t, pool, returnedFirst) + "; " + slotWord(t, pool, startedFirst)
			},
			main:    "ACME-116 holds a slot; ACME-117 holds no slot",
			changed: "ACME-116 holds no slot; ACME-117 holds a slot",
			why: "the issues whose work came back take a free slot in the order their work came back, or by the moment of their " +
				"first slot under a rule that orders them so",
		},
		slotRuleReader{
			reader:  "record.Waiting's order of the line: work in progress before the trees not yet opened",
			changes: []string{"Order"},
			rule:    ruleWith("main with work in progress first", func(rule *record.SlotRule) { rule.Order = record.LineReworkFirst }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const root, child, queued = "ACME-190", "ACME-191", "ACME-192"
				// A started tree whose root ranks behind a tree that never opened: its child that
				// has not started is work in progress, and the queued root is new work.
				acmeRoot(t, pool, root, phase.Accepting, "needs_review", "c", nil, nil)
				approvedGate(t, pool, root)
				acmeChild(t, pool, child, root, phase.Admitted, "todo", "b")
				acmeQueuedRoot(t, pool, queued, "a")
				line, _, _ := lineUnder(t, pool, rule)
				return lineWord(line)
			},
			main:    "the line is [ACME-192 ACME-191]",
			changed: "the line is [ACME-191 ACME-192]",
			why: "the issues that have not started stand by their tree root's rank, or, under a rule that burns down work in " +
				"progress first, an issue of a tree already started stands ahead of every tree not yet opened",
		},
		slotRuleReader{
			reader:  "record.Waiting's order of the line: work that came back by first slot, under work in progress first",
			changes: []string{"Order"},
			rule:    ruleWith("main with work in progress first", func(rule *record.SlotRule) { rule.Order = record.LineReworkFirst }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const cameBackFirst, startedFirst = "ACME-193", "ACME-194"
				// The root ranked first on the board is the first whose work came back; the other
				// took its first slot two hours before it.
				acmeRoot(t, pool, cameBackFirst, phase.Implementing, "in_progress", "a", nil, nil)
				oweMemberStart(t, pool, cameBackFirst, cameBackFirst, phase.Implementing)
				acmeRoot(t, pool, startedFirst, phase.Implementing, "in_progress", "b", nil, nil)
				oweMemberStart(t, pool, startedFirst, startedFirst, phase.Implementing)
				line, _, _ := lineUnderTables(t, pool, rule, orderTables{firstSlots: map[string]time.Time{
					cameBackFirst: fixedNow.Add(-time.Hour), startedFirst: fixedNow.Add(-3 * time.Hour)}})
				return lineWord(line)
			},
			main:    "the line is [ACME-193 ACME-194]",
			changed: "the line is [ACME-194 ACME-193]",
			why: "the issues whose work came back stand oldest first, or, under a rule that burns down work in progress first, " +
				"by the moment of their first slot",
		},
		slotRuleReader{
			reader:  "record.Waiting's order of the line: work in progress in the order it arrived",
			changes: []string{"Order"},
			rule:    ruleWith("main with work in progress first", func(rule *record.SlotRule) { rule.Order = record.LineReworkFirst }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const ranked, rankedChild, started, laterChild, earlierChild = "ACME-195", "ACME-196", "ACME-197", "ACME-198", "ACME-199"
				// Two started trees, each with children that have not started. The tree ranked
				// first on the board took its first slot after the other; in the other tree, the
				// child ranked second arrived first.
				acmeRoot(t, pool, ranked, phase.Accepting, "needs_review", "a", nil, nil)
				approvedGate(t, pool, ranked)
				acmeChild(t, pool, rankedChild, ranked, phase.Admitted, "todo", "a")
				acmeRoot(t, pool, started, phase.Accepting, "needs_review", "b", nil, nil)
				approvedGate(t, pool, started)
				acmeChild(t, pool, laterChild, started, phase.Admitted, "todo", "b")
				acmeChild(t, pool, earlierChild, started, phase.Admitted, "todo", "c")
				line, _, _ := lineUnderTables(t, pool, rule, orderTables{
					firstSlots: map[string]time.Time{ranked: fixedNow.Add(-time.Hour), started: fixedNow.Add(-3 * time.Hour)},
					arrivals: map[string]time.Time{rankedChild: fixedNow.Add(-30 * time.Minute), laterChild: fixedNow.Add(-time.Hour),
						earlierChild: fixedNow.Add(-2 * time.Hour)}})
				return lineWord(line)
			},
			main:    "the line is [ACME-196 ACME-198 ACME-199]",
			changed: "the line is [ACME-199 ACME-198 ACME-196]",
			why: "the issues that have not started stand by their tree root's rank and then their own, or, under a rule that " +
				"burns down work in progress first, each started tree by the moment its root first took a slot and each issue " +
				"by the moment it arrived",
		},
		slotRuleReader{
			// The two choices of rework-first-keep together: a slot kept past the review puts an
			// issue there that gave its slot back for a wait in the returned part, and work in
			// progress first puts a started tree's child ahead of a tree not yet opened, so each
			// choice alone gives an outcome of its own, which the reverted runs of
			// TestTheChoicesOfARuleAreTheReadersRows tell apart.
			reader:  "record.Waiting's order of the line: work in progress first, with the slot kept past the review",
			changes: []string{"KeepsSlotIn", "Order"},
			rule: ruleWith("main with work in progress first and a slot past the review", func(rule *record.SlotRule) {
				rule.Order = record.LineReworkFirst
				rule.KeepsSlotIn = append(rule.KeepsSlotIn, phase.Accepting, phase.Retro, phase.Merging)
			}),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const waiter, root, child, queued = "ACME-204", "ACME-205", "ACME-206", "ACME-207"
				// A root in accepting that gave its slot back for a wait, and a started tree whose
				// child has not started, ranked behind a tree that never opened.
				acmeRoot(t, pool, waiter, phase.Accepting, "needs_review", "c", nil, nil)
				takeSlot(t, pool, waiter)
				releaseForWaitAt(t, pool, waiter, fixedNow.Add(-time.Hour))
				acmeRoot(t, pool, root, phase.Accepting, "needs_review", "b", nil, nil)
				approvedGate(t, pool, root)
				acmeChild(t, pool, child, root, phase.Admitted, "todo", "b")
				acmeQueuedRoot(t, pool, queued, "a")
				line, _, _ := lineUnder(t, pool, rule)
				return lineWord(line)
			},
			main:    "the line is [ACME-207 ACME-206]",
			changed: "the line is [ACME-204 ACME-206 ACME-207]",
			why: "an issue past the review needs no slot and stands in no place of the line, and the issues that have not started " +
				"stand by their tree root's rank; under a rule that keeps the slot past the review and burns down work in progress " +
				"first, an issue there that gave its slot back for a wait stands in the returned part, and a started tree's child " +
				"stands ahead of every tree not yet opened",
		},
		slotRuleReader{
			reader:  "record.Waiting's order of the line: one queue in arrival order",
			changes: []string{"Order"},
			rule:    ruleWith("main with one queue in arrival order", func(rule *record.SlotRule) { rule.Order = record.LineArrival }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const ranked, earliest, cameBack = "ACME-201", "ACME-202", "ACME-203"
				// Two roots that never ran and one whose work came back. The root ranked second
				// arrived first, and the root ranked first arrived last.
				acmeQueuedRoot(t, pool, ranked, "a")
				acmeQueuedRoot(t, pool, earliest, "b")
				acmeRoot(t, pool, cameBack, phase.Implementing, "in_progress", "c", nil, nil)
				oweMemberStart(t, pool, cameBack, cameBack, phase.Implementing)
				line, _, _ := lineUnderTables(t, pool, rule, orderTables{arrivals: map[string]time.Time{
					ranked: fixedNow.Add(-time.Hour), earliest: fixedNow.Add(-3 * time.Hour), cameBack: fixedNow.Add(-2 * time.Hour)}})
				return lineWord(line)
			},
			main:    "the line is [ACME-203 ACME-201 ACME-202]",
			changed: "the line is [ACME-202 ACME-203 ACME-201]",
			why: "the issues whose work came back stand before every issue that has not started, and those stand by rank, or, " +
				"under one queue in arrival order, each issue stands by the moment it arrived, whatever its part and its rank",
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
				seedClaimForProject(t, claimStore(t, pool), acmeProject, awaiting, awaiting, claim.RoleArchitect, supervise.StateIdle)
				carriedRootForProject(t, pool, acmeProject, carried, phase.Accepting, "b")
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
			// The row under main of the conflict path past the review. A rule that names no conflict
			// path runs the daemon's own, so both runs give cybermonklabs/legion#4979's answers, which
			// each of the three rows after it changes.
			reader:  "the conflict path past the review: the daemon's own",
			changes: []string{"Conflict"},
			rule:    ruleWith("main with no conflict path named", func(rule *record.SlotRule) { rule.Conflict = "" }),
			outcome: pastReviewConflictOutcome,
			main:    pastReviewOnMain,
			changed: pastReviewOnMain,
			why: "under the daemon's own conflict path a conflict in accepting or in retro is recorded and asks nobody, " +
				"and one in merging asks the merger to merge the base forward (cybermonklabs/legion#4979)",
		},
		slotRuleReader{
			reader:  "the conflict path past the review: move back by conflict",
			changes: []string{"Conflict"},
			rule:    ruleWith("main with a conflict that moves back", func(rule *record.SlotRule) { rule.Conflict = record.ConflictMovesBack }),
			outcome: pastReviewConflictOutcome,
			main:    pastReviewOnMain,
			changed: pastReviewWords(pastReviewMovedBack, string(workflow.TriggerConflict), string(workflow.TriggerConflict),
				string(workflow.TriggerConflict)),
			why: "a conflict past the review waits for merging or asks the merger under the daemon's own path, and moves the issue " +
				"back to implementing by conflict from accepting, retro and merging under the path of 958173da",
		},
		slotRuleReader{
			reader:  "the conflict path past the review: move back by checks_red",
			changes: []string{"Conflict"},
			rule:    ruleWith("main with a conflict that moves back by red checks", func(rule *record.SlotRule) { rule.Conflict = record.ConflictChecksRed }),
			outcome: pastReviewConflictOutcome,
			main:    pastReviewOnMain,
			changed: pastReviewWords(pastReviewMovedBack, string(workflow.TriggerChecksRed), string(workflow.TriggerChecksRed),
				string(workflow.TriggerChecksRed)),
			why: "a conflict past the review waits for merging or asks the merger under the daemon's own path, and moves the issue " +
				"back to implementing by checks_red from accepting, retro and merging under the path of 9542c6fb",
		},
		slotRuleReader{
			reader:  "the conflict path past the review: the implementer merges forward at once",
			changes: []string{"Conflict"},
			rule: ruleWith("main with the implementer asked to merge forward past the review", func(rule *record.SlotRule) {
				rule.Conflict = record.ConflictImplementerMergesForward
			}),
			outcome: pastReviewConflictOutcome,
			main:    pastReviewOnMain,
			changed: pastReviewWords([]pastReviewEnd{{stands: phase.Accepting, asked: claim.RoleImplementer},
				{stands: phase.Retro, asked: claim.RoleImplementer}, {stands: phase.Merging, asked: claim.RoleImplementer}}),
			why: "a conflict past the review waits for merging or asks the merger under the daemon's own path, and under the path " +
				"of bafd3618 keeps its phase in accepting, retro and merging while the implementer is asked to merge the base " +
				"forward at once, with no supervise start",
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
			reader:  "record.LineFacts.InSlotlessPhase in readSlotHolders",
			changes: []string{"KeepsSlotIn"},
			rule:    keepingSlotIn(phase.Retro),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const root, member = "ACME-170", "ACME-171"
				seedTreeAndMemberForProject(t, pool, acmeProject, root, member, phase.Retro, 1, false)
				token := putWorkerClaimForProject(t, pool, acmeProject, root, member, claim.RoleImplementer, 1, supervise.StateSuspended,
					supervise.SuspendedByDaemon)
				inTx(t, pool, func(tx pgx.Tx) {
					if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: member,
						Role: claim.RoleImplementer, Claim: token, Entry: seededEntry, EntryTask: seededEntryTask(member),
						EntryRaisedAt: fixedNow}); err != nil {
						t.Fatalf("record the implementer's phases row of %s: %v", member, err)
					}
				})
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				return startsWord(member, startsOfForProject(t, pool, acmeProject, member))
			},
			main:    "ACME-171 has a start of the implementer carrying a task, which waits for no slot and a lane may run",
			changed: "ACME-171 has no start",
			why: "the repair of a worker with no task reaches an issue that holds no slot because its phase keeps none; " +
				"an issue whose phase keeps a slot it does not hold waits in the line for one, and the repair leaves it there",
		},
		slotRuleReader{
			reader:  "record.LineFacts.InSlotlessPhase in projection's atWorkInSlotlessPhase",
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
		slotRuleReader{
			// The records carry priorities through a store that sets them (prioritized), which the
			// fixtures write none of.
			reader:  "record.Waiting's band of the line by priority",
			changes: []string{"PriorityFirst"},
			rule:    ruleWith("main with the line by no priority", func(rule *record.SlotRule) { rule.PriorityFirst = false }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const cameBack, ranked, root, child, none = "ACME-210", "ACME-211", "ACME-212", "ACME-213", "ACME-214"
				// A root at P1 whose work came back; a root at P2 that never ran, ranked ahead of a
				// started tree whose root, at P0, stands past its review; that root's child at
				// admitted, with no priority of its own, which takes its root's; and a root with no
				// priority that never ran, ranked first of all.
				acmeRoot(t, pool, cameBack, phase.Implementing, "in_progress", "d", nil, nil)
				oweMemberStart(t, pool, cameBack, cameBack, phase.Implementing)
				acmeQueuedRoot(t, pool, ranked, "b")
				acmeRoot(t, pool, root, phase.Accepting, "needs_review", "c", nil, nil)
				approvedGate(t, pool, root)
				acmeChild(t, pool, child, root, phase.Admitted, "todo", "c")
				acmeQueuedRoot(t, pool, none, "a")
				priorities := prioritized{Store: record.NewStore(), of: map[string]record.Priority{cameBack: record.P1, ranked: record.P2, root: record.P0}}
				line, _, _ := lineUnderStore(t, pool, priorities, rule, nil)
				// Each entry's priority, none where no priority placed it, and its place within
				// its band and part (record.LineEntry.Priority, PartPlace).
				placed := make([]string, 0, len(line))
				for _, entry := range line {
					priority := entry.Priority.String()
					if entry.Priority == record.NoPriority {
						priority = "none"
					}
					placed = append(placed, fmt.Sprintf("%s %s %d", entry.Issue.Key, priority, entry.PartPlace))
				}
				return lineWord(line) + ", placed [" + strings.Join(placed, ", ") + "]"
			},
			main: "the line is [ACME-213 ACME-210 ACME-211 ACME-214], " +
				"placed [ACME-213 P0 1, ACME-210 P1 1, ACME-211 P2 1, ACME-214 none 1]",
			changed: "the line is [ACME-210 ACME-214 ACME-211 ACME-213], " +
				"placed [ACME-210 none 1, ACME-214 none 1, ACME-211 none 2, ACME-213 none 3]",
			why: "each issue stands in the band of its own priority, else its tree root's, P0 first and an issue with neither last, " +
				"and in the daemon's own order within one band, its place counted within its band and part; under a rule that " +
				"bands no priority, no priority places an issue, the issues whose work came back stand first and then the issues " +
				"that have not started by their tree root's rank, each place counted within its part",
		},
		slotRuleReader{
			reader:  "projection's waiting line: the band by priority",
			changes: []string{"PriorityFirst"},
			rule:    ruleWith("main with the line by no priority", func(rule *record.SlotRule) { rule.PriorityFirst = false }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				// Two roots that never ran, the one at P2 ranked ahead of the one at P1, each with
				// its priority in the store's own column, which the state reads.
				const ranked, urgent = "ACME-215", "ACME-216"
				acmeQueuedRoot(t, pool, ranked, "a")
				acmeQueuedRoot(t, pool, urgent, "b")
				inTx(t, pool, func(tx pgx.Tx) {
					for key, level := range map[string]int{ranked: 2, urgent: 1} {
						if _, err := tx.Exec(context.Background(), `update issues set priority = $2 where key = $1`, key, level); err != nil {
							t.Fatalf("set the priority of %s: %v", key, err)
						}
					}
				})
				return waitingLineWord(t, pool, rule, urgent)
			},
			main:    "ACME-216 stands at place 1 of 1, placed at P1, and its sentence names P1",
			changed: "ACME-216 stands at place 2 of 2, placed at no priority, and its sentence names no priority",
			why: "the state counts a member's place within the band of the priority that placed it and says so, under a rule that " +
				"bands the line; under one that does not, it counts within the member's part and names no priority",
		},
		slotRuleReader{
			reader:  "admit's capLets in the promotion: a P0 starts outside the cap",
			changes: []string{"StartsNow"},
			rule:    keptSlotRule("before-p0"),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, ranked, urgent, root, child = "ACME-220", "ACME-221", "ACME-222", "ACME-223", "ACME-224"
				// The holder fills the cap of one. A root at P1 that never ran is ranked first and
				// one at P0 second; and a started tree whose root is at P0 has a child at admitted
				// with no priority of its own.
				acmeRoot(t, pool, holder, phase.Implementing, "in_progress", "a", nil, nil)
				takeSlot(t, pool, holder)
				acmeQueuedRoot(t, pool, ranked, "b")
				acmeQueuedRoot(t, pool, urgent, "c")
				acmeRoot(t, pool, root, phase.Accepting, "needs_review", "d", nil, nil)
				approvedGate(t, pool, root)
				acmeChild(t, pool, child, root, phase.Admitted, "todo", "d")
				admission := prioritizedAdmission(t, 1, rule, map[string]record.Priority{ranked: record.P1, urgent: record.P0, root: record.P0})
				tickThrough(t, pool, admission, "the pass")
				return openedWord(t, pool, ranked, urgent) + "; " + slotWord(t, pool, child) + "; " + slotWord(t, pool, holder)
			},
			main:    "the trees open of [ACME-221 ACME-222] are [ACME-222]; ACME-224 holds no slot; ACME-220 holds a slot",
			changed: "the trees open of [ACME-221 ACME-222] are []; ACME-224 holds no slot; ACME-220 holds a slot",
			why: "under main, which starts a P0 outside the cap, an issue whose own priority is P0 starts whatever the slots in use, " +
				"and one that takes P0 from its tree root alone, or is P1, waits; under a rule that starts none there, a full cap lets " +
				"no start through that needs a slot",
		},
		slotRuleReader{
			reader:  "admit's capLets in lineCanMove: a P0 starts outside the cap",
			changes: []string{"StartsNow"},
			rule:    keptSlotRule("before-p0"),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const awaiting, urgent = "ACME-225", "ACME-226"
				registerControllerFor(t, pool, acmeProject)
				acmeRoot(t, pool, awaiting, phase.AwaitingPerson, "in_progress", "a", nil, nil)
				takeSlot(t, pool, awaiting)
				seedClaimForProject(t, claimStore(t, pool), acmeProject, awaiting, awaiting, claim.RoleArchitect, supervise.StateIdle)
				acmeQueuedRoot(t, pool, urgent, "b")
				settled := settleMovements(t, pool, awaiting)
				admission := prioritizedAdmission(t, 1, rule, map[string]record.Priority{urgent: record.P0})
				admission.UseStalledTreeNotice(deploymentBound(t, ""))
				admission.now = func() time.Time { return settled.Add(7 * time.Hour) }
				tickThrough(t, pool, admission, "the measure")
				return stalledWord(stalledRoots(t, pool))
			},
			main:    "the controller is told of no stopped tree",
			changed: "the controller is told that [ACME-225] stopped",
			why: "the stall measure's free-slot term asks the cap what the promotion asks it: under main, which starts a P0 outside " +
				"the cap, a root at P0 starts and the line moves; under a rule that starts none there, a root that never ran is held " +
				"by a full cap and the line is stopped",
		},
		slotRuleReader{
			// The holders' count of the stall measure: the one holder is a P0 root that waits on
			// a person, and the root that waits in the line is at P1, so only the count decides.
			reader:  "admit's lineCanMove: the cap counts no P0 holder",
			changes: []string{"StartsNow"},
			rule:    keptSlotRule("before-p0"),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const awaiting, ranked = "ACME-227", "ACME-228"
				registerControllerFor(t, pool, acmeProject)
				acmeRoot(t, pool, awaiting, phase.AwaitingPerson, "in_progress", "a", nil, nil)
				takeSlot(t, pool, awaiting)
				seedClaimForProject(t, claimStore(t, pool), acmeProject, awaiting, awaiting, claim.RoleArchitect, supervise.StateIdle)
				acmeQueuedRoot(t, pool, ranked, "b")
				settled := settleMovements(t, pool, awaiting)
				admission := prioritizedAdmission(t, 1, rule, map[string]record.Priority{awaiting: record.P0, ranked: record.P1})
				admission.UseStalledTreeNotice(deploymentBound(t, ""))
				admission.now = func() time.Time { return settled.Add(7 * time.Hour) }
				tickThrough(t, pool, admission, "the measure")
				return stalledWord(stalledRoots(t, pool))
			},
			main:    "the controller is told of no stopped tree",
			changed: "the controller is told that [ACME-227] stopped",
			why: "the stall measure counts the holders the cap counts: under main, which starts a P0 outside the cap, a P0 holder " +
				"fills none of it, so the root at P1 starts and the line moves; under a rule that starts none there, a holder that " +
				"waits on a person fills the cap of one and the line is stopped",
		},
		slotRuleReader{
			// The holders' count of the promotion and the P0 bound: a P0 holder fills the cap of
			// one under before-p0, and two P0 roots and one at P1 wait, under a bound of two.
			reader:  "admit's promoteHolds: the cap counts the holders below P0, and the P0 bound the rest",
			changes: []string{"StartsNow"},
			rule:    keptSlotRule("before-p0"),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, first, ranked, second = "ACME-240", "ACME-241", "ACME-242", "ACME-243"
				acmeRoot(t, pool, holder, phase.Implementing, "in_progress", "a", nil, nil)
				takeSlot(t, pool, holder)
				acmeQueuedRoot(t, pool, first, "b")
				acmeQueuedRoot(t, pool, ranked, "c")
				acmeQueuedRoot(t, pool, second, "d")
				var logged bytes.Buffer
				admission := ruledAdmission(t, 1, rule, slog.New(slog.NewJSONHandler(&logged, nil)))
				admission.store = prioritized{Store: admission.store, of: map[string]record.Priority{holder: record.P0, first: record.P0,
					ranked: record.P1, second: record.P0}}
				admission.UseP0Bound(new(2))
				tickThrough(t, pool, admission, "the pass")
				return openedWord(t, pool, first, ranked, second) + "; " + p0LinesWord(t, &logged)
			},
			main: "the trees open of [ACME-241 ACME-242 ACME-243] are [ACME-241 ACME-242]; the pass logs [" +
				"admission starts a P0 issue outside the cap: ACME-241, 2 P0 holders, 0 other holders, cap 1, bound 2; " +
				"admission holds a P0 issue at the P0 bound: ACME-243, 2 P0 holders, 0 other holders, cap 1, bound 2]",
			changed: "the trees open of [ACME-241 ACME-242 ACME-243] are []; the pass logs no P0 line",
			why: "under main, which starts a P0 outside the cap, the cap counts no P0 holder, so the root at P1 starts, and the P0 " +
				"bound counts the P0 holders, so the first P0 root starts outside the cap and the second waits at the bound, each " +
				"logged with the holders, the cap and the bound; under a rule that starts none there, a holder fills the cap and no " +
				"root starts",
		},
		slotRuleReader{
			// The read of a free slot the controller is woken by: a P0 holder and a P0 root in the
			// line at a cap of one.
			reader:  "admit's slotFree: a P0 holder and a P0 candidate take none of the cap's slots",
			changes: []string{"StartsNow"},
			rule:    keptSlotRule("before-p0"),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const holder, urgent = "ACME-245", "ACME-246"
				acmeRoot(t, pool, holder, phase.Implementing, "in_progress", "a", nil, nil)
				takeSlot(t, pool, holder)
				acmeQueuedRoot(t, pool, urgent, "b")
				admission := prioritizedAdmission(t, 1, rule, map[string]record.Priority{holder: record.P0, urgent: record.P0})
				if slotFreeNow(t, pool, admission) {
					return "a slot of the cap stands free"
				}
				return "no slot of the cap stands free"
			},
			main:    "a slot of the cap stands free",
			changed: "no slot of the cap stands free",
			why: "under main, which starts a P0 outside the cap, neither the P0 holder nor the P0 root in the line counts against " +
				"the cap, so its one slot stands free; under a rule that starts none there, the holder fills the cap of one and the " +
				"root in the line would take a slot",
		},
		slotRuleReader{
			reader:  "admit's releaseIdleSlots: a holder idle while its children work",
			changes: []string{"IdleReturn"},
			rule:    ruleWith("main with an idle return of an hour", func(rule *record.SlotRule) { rule.IdleReturn = time.Hour }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const parent, child, queued = "ACME-230", "ACME-231", "ACME-232"
				// The parent, in implementing, has held its slot for three hours, and its
				// implementer's last turn ended two hours ago; its child, in implementing, holds
				// the other slot of the cap of two; a root that never ran waits.
				acmeRoot(t, pool, parent, phase.Implementing, "in_progress", "a", nil, nil)
				approvedGate(t, pool, parent)
				takeSlotAt(t, pool, parent, fixedNow.Add(-3*time.Hour))
				idleClaim(t, pool, parent, parent, claim.RoleImplementer, fixedNow.Add(-150*time.Minute), fixedNow.Add(-2*time.Hour))
				acmeChild(t, pool, child, parent, phase.Implementing, "in_progress", "a")
				takeSlotAt(t, pool, child, fixedNow.Add(-time.Hour))
				acmeQueuedRoot(t, pool, queued, "b")
				tickThrough(t, pool, ruledAdmission(t, 2, rule, silent()), "the pass")
				line, _, _ := lineUnder(t, pool, rule)
				return slotWord(t, pool, parent) + "; " + markWord(t, pool, parent) + "; " + treeWord(t, pool, queued) + "; " + waitsWord(line, parent)
			},
			main: "ACME-230 holds a slot; ACME-230 carries no mark of a release for a wait; the tree of ACME-232 is not open; " +
				"ACME-230 stands in no place of the line",
			changed: "ACME-230 holds no slot; ACME-230 carries the mark of a release for a wait; the tree of ACME-232 is open; " +
				"ACME-230 stands in the line waiting on [ACME-231]",
			why: "a parent past admitted keeps its slot while its children work; under a rule with an idle return, a holder that waits " +
				"on its children with no turn for that long gives its slot back to work that can start, and stands in the line at " +
				"the mark of that release, passed over while its children work",
		},
		slotRuleReader{
			reader:  "admit's releaseIdleSlots: a root idle while it waits on a person",
			changes: []string{"IdleReturn"},
			rule:    ruleWith("main with an idle return of an hour", func(rule *record.SlotRule) { rule.IdleReturn = time.Hour }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const awaiting, queued = "ACME-235", "ACME-236"
				// The root waits on a person and has held the only slot for three hours; its
				// architect's last turn ended two hours ago; a root that never ran waits.
				acmeRoot(t, pool, awaiting, phase.AwaitingPerson, "in_progress", "a", nil, nil)
				takeSlotAt(t, pool, awaiting, fixedNow.Add(-3*time.Hour))
				idleClaim(t, pool, awaiting, awaiting, claim.RoleArchitect, fixedNow.Add(-150*time.Minute), fixedNow.Add(-2*time.Hour))
				acmeQueuedRoot(t, pool, queued, "b")
				tickThrough(t, pool, ruledAdmission(t, 1, rule, silent()), "the pass")
				line, _, _ := lineUnder(t, pool, rule)
				return slotWord(t, pool, awaiting) + "; " + markWord(t, pool, awaiting) + "; " + treeWord(t, pool, queued) + "; " +
					waitsWord(line, awaiting)
			},
			main: "ACME-235 holds a slot; ACME-235 carries no mark of a release for a wait; the tree of ACME-236 is not open; " +
				"ACME-235 stands in no place of the line",
			changed: "ACME-235 holds no slot; ACME-235 carries the mark of a release for a wait; the tree of ACME-236 is open; " +
				"ACME-235 stands in no place of the line",
			why: "a root waiting on a person keeps its slot; under a rule with an idle return, one with no turn for that long gives it " +
				"back to work that can start, and stands in no line until a person answers it",
		},
		slotRuleReader{
			reader:  "record.Waiting: an answered root keeps the place of its idle release",
			changes: []string{"IdleReturn"},
			rule:    ruleWith("main with an idle return of an hour", func(rule *record.SlotRule) { rule.IdleReturn = time.Hour }),
			outcome: func(t *testing.T, rule record.SlotRule) string {
				t.Helper()
				pool := migratedPool(t)
				const answered, waited = "ACME-237", "ACME-238"
				// The root gave its slot back an hour ago while it waited on a person, who answered
				// it ten minutes from now; the other issue gave its slot back for a wait that has
				// since ended, now.
				answeredAt := fixedNow.Add(10 * time.Minute)
				acmeRoot(t, pool, answered, phase.AwaitingPerson, "in_progress", "b", nil, func(root *record.Issue) {
					root.Awaiting.AnsweredAt, root.Awaiting.AnsweredBy, root.Awaiting.AnsweredDetail = &answeredAt, record.AnsweredByAsk, "ask-"+answered
				})
				takeSlot(t, pool, answered)
				releaseForWaitAt(t, pool, answered, fixedNow.Add(-time.Hour))
				acmeRoot(t, pool, waited, phase.Implementing, "in_progress", "a", nil, nil)
				takeSlot(t, pool, waited)
				releaseForWaitAt(t, pool, waited, fixedNow)
				line, _, _ := lineUnder(t, pool, rule)
				return lineWord(line)
			},
			main:    "the line is [ACME-238 ACME-237]",
			changed: "the line is [ACME-237 ACME-238]",
			why: "an answered root stands in the returned part at its answer; under a rule with an idle return, one that gave its slot " +
				"back while it waited keeps the place of that release, ahead of every slot given back after it",
		},
	)
	return append(rows, signalRuleReaders()...)
}

// conflictOutcome is the outcome of the two rows of the conflict path in testing: a root in testing
// holding its slot, whose open pull request's head GitHub reads conflicting twice a minute apart
// against a base that requires a check the head has not passed, so the conflict is confirmed and
// needs the merge (workflow's mergeability, classify.ConflictNeedsMergeUnder). The engine's own log
// says which move it made ("workflow: phase changed").
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

// pastReviewIssues is the issues of the rows of the conflict path past the review, one in each
// phase past the review of the default steps, with the number of each one's pull request.
var pastReviewIssues = []struct {
	key    string
	number int
	stands phase.Phase
}{{"ACME-155", 155, phase.Accepting}, {"ACME-156", 156, phase.Retro}, {"ACME-157", 157, phase.Merging}}

// pastReviewEnd is how one issue of pastReviewIssues ends in a row of the conflict path past the
// review: the phase it stands in, the role the record says was asked to merge the base forward
// (record.MergeForward.Asked, "" for nobody) and the role of each supervise start written for it.
type pastReviewEnd struct {
	stands  phase.Phase
	asked   claim.Role
	started []claim.Role
}

// pastReviewOnMain is the outcome of the conflict path past the review under the daemon's own
// path (cybermonklabs/legion#4979): the issues in accepting and in retro keep their phase and
// nobody is asked, and the merger is asked for the issue in merging, with no supervise start and
// no phase move.
var pastReviewOnMain = pastReviewWords([]pastReviewEnd{{stands: phase.Accepting}, {stands: phase.Retro},
	{stands: phase.Merging, asked: claim.RoleMerger}})

// pastReviewMovedBack is how each issue of pastReviewIssues ends under a path that moves a
// conflicting head back: in implementing, nobody asked, the implementer's start written.
var pastReviewMovedBack = []pastReviewEnd{{stands: phase.Implementing, started: []claim.Role{claim.RoleImplementer}},
	{stands: phase.Implementing, started: []claim.Role{claim.RoleImplementer}},
	{stands: phase.Implementing, started: []claim.Role{claim.RoleImplementer}}}

// pastReviewWords is the outcome pastReviewConflictOutcome gives where each issue of
// pastReviewIssues ends as ends says, in order, its head held conflicting, and the engine logged
// the phase moves by moves.
func pastReviewWords(ends []pastReviewEnd, moves ...string) string {
	words := make([]string, 0, len(ends)+1)
	for i, end := range ends {
		words = append(words, pastReviewWord(pastReviewIssues[i].key, end, true))
	}
	return strings.Join(append(words, movesWord(moves)), "; ")
}

// pastReviewWord is how key ends in a row of the conflict path past the review (pastReviewEnd),
// and whether the record holds its head conflicting.
func pastReviewWord(key string, end pastReviewEnd, conflicting bool) string {
	head := "its head held conflicting"
	if !conflicting {
		head = "its head not held conflicting"
	}
	asked := "nobody asked to merge forward"
	if end.asked != "" {
		asked = "the " + string(end.asked) + " asked to merge forward"
	}
	started := "no supervise start"
	if len(end.started) > 0 {
		roles := make([]string, 0, len(end.started))
		for _, role := range end.started {
			roles = append(roles, string(role))
		}
		started = "a supervise start of the " + strings.Join(roles, ", the ")
	}
	return fmt.Sprintf("%s stands in %s, %s, %s, %s", key, end.stands, head, asked, started)
}

// pastReviewConflictOutcome is the outcome of the rows of the conflict path past the review: a root
// in each phase past the review (pastReviewIssues), holding no slot, whose open pull request's head
// GitHub reads conflicting twice a minute apart against a base that requires a check the head has
// not passed, so each conflict is confirmed (workflow's mergeability). For each issue it reads where
// the issue stands, whether the record holds its head conflicting, whom the record says was asked to
// merge the base forward (record.MergeForward.Asked) and each supervise start written for it, which
// a move back writes and a merge forward does not; the engine's own log says which moves it made.
func pastReviewConflictOutcome(t *testing.T, rule record.SlotRule) string {
	t.Helper()
	pool := migratedPool(t)
	for i, at := range pastReviewIssues {
		acmeRoot(t, pool, at.key, at.stands, statusIn(at.stands), fmt.Sprintf("a%d", i), nil, nil)
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutPullRequest(context.Background(), tx, record.PullRequest{State: record.PullRequestOpen, Issue: at.key,
				Repo: "acme/widgets", Number: at.number, Branch: "legion/" + at.key, HeadSHA: "head-" + at.key, Base: "main",
				Failing: []string{}}); err != nil {
				t.Fatalf("seed the pull request of %s: %v", at.key, err)
			}
		})
	}
	var logged bytes.Buffer
	admission := ruledAdmission(t, len(pastReviewIssues), rule, slog.New(slog.NewJSONHandler(&logged, nil)))
	for _, at := range pastReviewIssues {
		for _, after := range []time.Duration{0, time.Minute} {
			applyThrough(t, pool, admission, "github", "conflict-"+at.key+"-"+after.String(), intake.PullRequestMergeability{
				Repo: "acme/widgets", Number: at.number, Base: "main", HeadSHA: "head-" + at.key, Mergeable: record.MergeabilityConflicting,
				MergeState: record.MergeStateDirty, ReadAt: fixedNow.Add(after), Rules: &record.BaseRules{Required: []string{"ci"}}})
		}
	}
	words := make([]string, 0, len(pastReviewIssues)+1)
	for _, at := range pastReviewIssues {
		var pr *record.PullRequest
		inTx(t, pool, func(tx pgx.Tx) {
			var err error
			if pr, err = record.NewStore().PullRequest(context.Background(), tx, at.key); err != nil || pr == nil {
				t.Fatalf("read the pull request of %s: %+v, %v", at.key, pr, err)
			}
		})
		end := pastReviewEnd{stands: issue(t, pool, at.key).Phase}
		if pr.MergeForward != nil {
			end.asked = pr.MergeForward.Asked()
		}
		for _, start := range startsOfForProject(t, pool, acmeProject, at.key) {
			end.started = append(end.started, start.role)
		}
		words = append(words, pastReviewWord(at.key, end, pr.ConflictingHead != "" && pr.ConflictingHead == pr.HeadSHA))
	}
	return strings.Join(append(words, movesWord(phaseMoves(t, &logged))), "; ")
}

// silent is a logger that writes nowhere, for the rows that read records and outbox rows alone.
func silent() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ruledAdmission is an admission of cap over acmeProject whose engine runs rule, which the pass
// reads through it (workflow.Engine.SlotRule), as do the engine's table and the slot mark of each
// start it writes (workflow.Config.SlotRule). It is newAdmissionForProject with the rule given to
// the engine, on the same fixed clock, the same repositories and the deployment's single design
// step. log is where both write.
func ruledAdmission(t *testing.T, cap int, rule record.SlotRule, log *slog.Logger) *Admission {
	t.Helper()
	engine := workflow.New(record.NewStore(), workflow.Config{Project: acmeProject, Repositories: testRepositories,
		Steps: config.DesignGate("").Steps(), Clock: func() time.Time { return fixedNow }, SlotRule: rule}, log)
	admission := New(record.NewStore(), engine, cap, acmeProject, log)
	admission.now = func() time.Time { return fixedNow }
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

// acmeChild records key as a member of root's running tree, filed under root, standing in stands
// with the Dispatch status status and the board's rank rank, with its mirror row. It writes no
// slot, as acmeRoot writes none.
func acmeChild(t *testing.T, pool *pgxpool.Pool, key, root string, stands phase.Phase, status, rank string) {
	t.Helper()
	parent := root
	putIssue(t, pool, record.Issue{Key: key, Tree: root, Parent: &parent, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: acmeProject, Title: key, Phase: stands, Generation: 1, Status: status,
		DispatchStatus: status, Rank: rank, LastDispatchSeq: 1})
	seedMirrorIn(t, pool, acmeProject, key, status)
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

// signOffOf is the sign-off of key's own architect, as the route writes it: its claim, the
// generation these fixtures seed (1) and the run of the step the issue stands in, which is the
// zero entry for a root no step record of this fixture holds. The workflow judges all three again
// under the fact lock (intake.ArchitectRequest), so a sign-off that named none of them would be
// refused as one of an architect outside the tree.
func signOffOf(t *testing.T, key string) intake.SignOff {
	t.Helper()
	return intake.SignOff{Issue: key, By: acmeToken(t, key, claim.RoleArchitect), Generation: 1}
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

// releaseForWaitAt takes key's slot back for a wait at the moment at, which the mark carries, as
// the pass's release for a wait does: the row goes and the mark that says why is written with it
// (record.Store.ReleaseSlotForWait). readmitted_members_test.go's releaseForWait is this at the
// pass's own clock.
func releaseForWaitAt(t *testing.T, pool *pgxpool.Pool, key string, at time.Time) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().ReleaseSlotForWait(context.Background(), tx, key, at); err != nil {
			t.Fatalf("release the slot of %s for a wait: %v", key, err)
		}
	})
}

// idleClaim records the claim of key's role in tree as an idle agent's: no turn under way and no
// task, its last turn begun at start and ended at end, the two moments the holder state and the
// idle return read a claim's turns by (record.HolderClaim).
func idleClaim(t *testing.T, pool *pgxpool.Pool, key, tree string, role claim.Role, start, end time.Time) {
	t.Helper()
	if err := claimStore(t, pool).PutClaim(context.Background(), supervise.Claim{Token: acmeToken(t, key, role), Project: acmeProjectToken(t),
		Tree: tree, Issue: key, Role: role, TreeEpoch: 1, Generation: 1, State: supervise.StateIdle, Session: "ses_" + string(role) + "_" + key,
		LastTurnStart: start, LastTurnEnd: end}); err != nil {
		t.Fatalf("record the %s claim of %s: %v", role, key, err)
	}
}

// prioritized is the daemon's own store with each issue's own Dispatch priority set as of names it
// on every record the store reads of a project's live issues (record.Store.LiveIssues, the read the
// admission pass and the line take their records from), and none where of names none. The
// fixtures of this file write no priority, so a row whose rule reads one hands its pass the
// records with the priorities it names here, as the daemon's store reads them back from Dispatch's
// level (record.Issue.Priority).
type prioritized struct {
	record.Store
	of map[string]record.Priority
}

func (s prioritized) LiveIssues(ctx context.Context, tx pgx.Tx, project string) ([]record.Issue, error) {
	issues, err := s.Store.LiveIssues(ctx, tx, project)
	for i := range issues {
		issues[i].Priority = s.of[issues[i].Key]
	}
	return issues, err
}

// prioritizedAdmission is ruledAdmission over a store that reads each issue's priority as
// priorities names it (prioritized).
func prioritizedAdmission(t *testing.T, cap int, rule record.SlotRule, priorities map[string]record.Priority) *Admission {
	t.Helper()
	admission := ruledAdmission(t, cap, rule, silent())
	admission.store = prioritized{Store: admission.store, of: priorities}
	return admission
}

// keptSlotRule is the rule the repository keeps under name (record.SlotRuleNamed), which a row
// reads its choice under where a kept rule changes that choice alone, and panics where it keeps
// none, since the table is built before any test runs.
func keptSlotRule(name string) record.SlotRule {
	rule, kept := record.SlotRuleNamed(name)
	if !kept {
		panic("the repository keeps no slot rule named " + name)
	}
	return rule
}

// p0LinesWord is the lines of R10's starts a pass logged to logged as JSON (P0StartLine,
// P0BoundRefusalLine), each once, in the order they were written, with what each names: the issue,
// the P0 holders, the other holders, the cap and the bound.
func p0LinesWord(t *testing.T, logged *bytes.Buffer) string {
	t.Helper()
	var said []string
	for line := range strings.Lines(logged.String()) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("read the log line %q: %v", line, err)
		}
		if entry["msg"] != P0StartLine && entry["msg"] != P0BoundRefusalLine {
			continue
		}
		word := fmt.Sprintf("%s: %v, %v P0 holders, %v other holders, cap %v, bound %v", entry["msg"], entry["issue"],
			entry["p0_holders"], entry["other_holders"], entry["cap"], entry["p0_bound"])
		if !slices.Contains(said, word) {
			said = append(said, word)
		}
	}
	if len(said) == 0 {
		return "the pass logs no P0 line"
	}
	return "the pass logs [" + strings.Join(said, "; ") + "]"
}

// waitsWord says where key stands in a waiting line: in no place of it, or in it with what its
// entry waits on (record.LineEntry.Waits), which the promotion passes it over for.
func waitsWord(line []record.LineEntry, key string) string {
	for _, entry := range line {
		if entry.Issue.Key == key {
			return key + " stands in the line waiting on [" + strings.Join(entry.Waits, " ") + "]"
		}
	}
	return key + " stands in no place of the line"
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
	return lineUnderTables(t, pool, rule, nil)
}

// lineUnderTables is lineUnder with what a rule the daemon does not run records read in beside the
// record from tables (RuleTables.ReadRuleFacts), as a pass given those tables reads it; nil tables
// read nothing, as the daemon's own pass reads nothing.
func lineUnderTables(t *testing.T, pool *pgxpool.Pool, rule record.SlotRule, tables RuleTables) ([]record.LineEntry, []record.HeldIssue, record.LineFacts) {
	t.Helper()
	return lineUnderStore(t, pool, record.NewStore(), rule, tables)
}

// lineUnderStore is lineUnderTables over the records records reads.
func lineUnderStore(t *testing.T, pool *pgxpool.Pool, records record.Store, rule record.SlotRule, tables RuleTables) ([]record.LineEntry, []record.HeldIssue, record.LineFacts) {
	t.Helper()
	var line []record.LineEntry
	var held []record.HeldIssue
	var facts record.LineFacts
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		issues, err := records.LiveIssues(ctx, tx, acmeProject)
		if err != nil {
			t.Fatalf("list the live issues: %v", err)
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			t.Fatalf("list the slots: %v", err)
		}
		if facts, err = record.ReadLineFactsUnder(ctx, tx, records, acmeProject, issues, config.DesignGate("").Steps(), rule); err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		if tables != nil {
			if err := tables.ReadRuleFacts(ctx, tx, issues, &facts); err != nil {
				t.Fatalf("read what the rule records: %v", err)
			}
		}
		if _, facts, line, held, err = blockers.JudgeLine(ctx, tx, records, acmeProject, issues, record.OwnSlots(issues, slots), facts, nil, fixedNow); err != nil {
			t.Fatalf("judge the waiting line: %v", err)
		}
	})
	return line, held, facts
}

// orderTables is a replay's rule tables as far as the line's moments go (RuleTables): the moment
// of each issue's first slot in its generation and of its arrival in the line, by key, held as data
// and added to the facts of every pass whatever its rule, since a rule that places by neither never
// reads them (record.LineFacts.FirstSlots, record.LineFacts.Arrivals). The daemon keeps neither; a
// replay keeps them in tables of its own database (the replay's ruleTables), whose keeping that
// package's tests read.
type orderTables struct {
	firstSlots, arrivals map[string]time.Time
}

func (tables orderTables) ReadRuleFacts(_ context.Context, _ pgx.Tx, _ []record.Issue, facts *record.LineFacts) error {
	facts.FirstSlots, facts.Arrivals = tables.firstSlots, tables.arrivals
	return nil
}

func (orderTables) KeepCovers(context.Context, pgx.Tx, map[string]string) error { return nil }

// phaseMoves is the trigger of each phase move the engine logged, in order, read from the JSON
// lines it wrote ("workflow: phase changed").
func phaseMoves(t *testing.T, logged *bytes.Buffer) []string {
	t.Helper()
	var moves []string
	for _, line := range loggedLines(t, logged, "workflow: phase changed") {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode the log line %q: %v", line, err)
		}
		moves = append(moves, fmt.Sprint(entry["trigger"]))
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
	if treeOpenForProject(t, pool, acmeProject, key) {
		return "the tree of " + key + " is open"
	}
	return "the tree of " + key + " is not open"
}

// openedWord is which of keys have a tree standing open.
func openedWord(t *testing.T, pool *pgxpool.Pool, keys ...string) string {
	t.Helper()
	var open []string
	for _, key := range keys {
		if treeOpenForProject(t, pool, acmeProject, key) {
			open = append(open, key)
		}
	}
	return "the trees open of [" + strings.Join(keys, " ") + "] are [" + strings.Join(open, " ") + "]"
}

// lineWord is the keys of a waiting line, in its order.
func lineWord(line []record.LineEntry) string {
	return "the line is [" + strings.Join(lineKeys(line), " ") + "]"
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

// startedWaitWord is the open Dispatch children the pass's judgement of the started parents finds
// holding key's next start (record.LineFacts.StartedParentWaits).
func startedWaitWord(facts record.LineFacts, key string) string {
	var open []string
	for _, child := range facts.StartedParentWaits[key].Open {
		open = append(open, child.Key)
	}
	if len(open) == 0 {
		return key + " is held by no open child"
	}
	return key + " is held by its open children [" + strings.Join(open, " ") + "]"
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
// that role, the two acts that stop a worker's process and keep its session, or the retirement of
// the task its claim holds for a phase a move into awaiting_children stopped (retire_task).
func stopWord(t *testing.T, pool *pgxpool.Pool, key string, role claim.Role) string {
	t.Helper()
	var stops int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = $1 and kind = 'supervise'
		and payload->>'op' in ('suspend', 'release', 'retire_task') and payload->>'role' = $2`, key, string(role)).Scan(&stops); err != nil {
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

// waitingLineWord is where the state, read under rule, places key in the waiting line
// (projection.Project, api.WaitingLineView): its place and the length of the part it stands in,
// the priority that placed it, and the priority its sentence names (Reason), if any.
func waitingLineWord(t *testing.T, pool *pgxpool.Pool, rule record.SlotRule, key string) string {
	t.Helper()
	word := key + " stands in no line"
	inTx(t, pool, func(tx pgx.Tx) {
		projected, err := projection.Project(context.Background(), projection.Inputs{Tx: tx, Store: record.NewStore(),
			Project: acmeProject, Steps: config.DesignGate("").Steps(), Now: fixedNow, SlotRule: rule})
		if err != nil {
			t.Fatalf("project the state: %v", err)
		}
		waiting := projected.Issues[key].WaitingLine
		if waiting == nil {
			return
		}
		placed := "no priority"
		if waiting.Priority != nil && record.Priority(*waiting.Priority) != record.NoPriority {
			placed = record.Priority(*waiting.Priority).String()
		}
		names := "no priority"
		for _, priority := range []record.Priority{record.P0, record.P1, record.P2, record.P3} {
			if strings.Contains(waiting.Reason, priority.String()) {
				names = priority.String()
				break
			}
		}
		if names == "no priority" && strings.Contains(waiting.Reason, "priority") {
			names = "a priority of no level: " + waiting.Reason
		}
		word = fmt.Sprintf("%s stands at place %d of %d, placed at %s, and its sentence names %s",
			key, waiting.PartPlace, waiting.PartLength, placed, names)
	})
	return word
}
