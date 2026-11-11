package blockers

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// placedAt is the one moment the generated records hang their slots, approvals and answers on;
// no rule read here compares two of them.
var placedAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

// shape is one generated project: a tree root, one member under it, and a second root whose
// Dispatch chain may reach the first. Each field is one choice the records can make that the
// waiting line reads, and the generator below runs their whole product.
type shape struct {
	// rootStands is the phase the tree's root stands in: admitted while its architect writes
	// the design, at work once its tree runs, or done and closed.
	rootStands phase.Phase
	// labelled is whether the root carries the legion label (record.Issue.HandedOver). A
	// person takes it off to pause a root, and Legion starts nothing for it then.
	labelled bool
	// gate is the root's design gate: "none" for a root that has registered nothing, "open"
	// for one a person's approval stands on, and "revised" for one whose architect has moved
	// the document past that approval.
	gate string
	// lifecycle is whether the tree's lifecycle stands open under the workflow.
	lifecycle bool
	// slotted is whether the root holds one of the cap's slots.
	slotted bool
	// memberSlotted is whether the member holds one.
	memberSlotted bool
	// asking is whether the root has handed a question to a person and has no answer yet.
	asking bool
	// linked is whether a Dispatch blocked_by link of the member names an unfinished issue.
	linked bool
	// chained is whether the second root's recorded Dispatch chain reaches the first.
	chained bool
}

func (s shape) String() string {
	return fmt.Sprintf("root %s labelled=%t gate=%s lifecycle=%t slotted=%t memberSlotted=%t asking=%t linked=%t chained=%t",
		s.rootStands, s.labelled, s.gate, s.lifecycle, s.slotted, s.memberSlotted, s.asking, s.linked, s.chained)
}

const (
	shapeRoot   = "LEGION-10"
	shapeMember = "LEGION-11"
	shapeOther  = "LEGION-20"
)

// records is the project's issue records, slots, mirror rows and line facts as this shape
// writes them.
func (s shape) records() ([]record.Issue, []record.Slot, map[string]Row, record.LineFacts) {
	status := map[phase.Phase]string{phase.Admitted: "todo", phase.Implementing: "in_progress", phase.Done: "done"}
	root := record.Issue{Key: shapeRoot, Tree: shapeRoot, Project: "LEGION", Rank: "A",
		Status: status[s.rootStands], Phase: s.rootStands, HandedOver: s.labelled}
	if s.asking && s.rootStands == phase.Admitted {
		root.Phase, root.Status = phase.AwaitingPerson, "in_progress"
		root.Awaiting = &record.Awaiting{Ask: "ask-1", Since: record.EventMark{Seq: 3}}
	}
	member := record.Issue{Key: shapeMember, Tree: shapeRoot, Parent: record.ParentOf(shapeRoot),
		Project: "LEGION", Rank: "B", Status: "todo", Phase: phase.Admitted}
	other := record.Issue{Key: shapeOther, Tree: shapeOther, Project: "LEGION", Rank: "C",
		Status: "todo", Phase: phase.Admitted, HandedOver: true}
	if s.chained {
		other.Ancestry = []string{shapeRoot}
	}
	issues := []record.Issue{root, member, other}

	var slots []record.Slot
	if s.slotted {
		slots = append(slots, record.Slot{Issue: shapeRoot, Index: 0, AdmittedAt: placedAt})
	}
	if s.memberSlotted {
		slots = append(slots, record.Slot{Issue: shapeMember, Index: 1, AdmittedAt: placedAt})
	}

	approved := 1
	gate := record.DesignGate{Issue: shapeRoot, Step: record.DesignStepSpec, LatestVersion: 1,
		ApprovedVersion: &approved, ApprovedMark: record.EventMark{Seq: 7, At: placedAt}, EverOpened: true}
	gates := map[string]record.TreeGate{}
	switch s.gate {
	case "open":
		gates[shapeRoot] = record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: gate}}
	case "revised":
		gate.LatestVersion = 2
		gates[shapeRoot] = record.TreeGate{Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: gate}}
	}
	lifecycles := map[string]treelifecycle.Lifecycle{}
	if s.lifecycle {
		lifecycles[shapeRoot] = treelifecycle.Lifecycle{Authority: treelifecycle.AuthorityWorkflow}
	}
	rows := map[string]Row{}
	if s.linked {
		rows[shapeMember] = Row{Status: "todo", Blockers: []string{"LEGION-99"}}
		rows["LEGION-99"] = Row{Status: "in_progress"}
	}
	facts := record.LineFacts{Lifecycles: lifecycles, Gates: gates}
	// What JudgeLine fills in from the mirror before it builds the line; there is no store
	// here, and the children-first wait reads one, so no shape writes an open child.
	facts.Held = HeldIssues(rows, issues)
	return issues, slots, rows, facts
}

// Every issue the records call admitted is placed, over every shape the records can take: it
// is in `active`, in `waiting` or in `blocked`, and never in none of them, which is the state
// dispatch://INFRA-2964 reported. `waiting` is exclusive — an issue in it is in neither of the
// others — so no issue both waits for a slot and holds one, and none waits for a slot that
// something holds. The one pair a shape may produce is `active` with `blocked`, and only for an
// issue holding a slot: the slot says the cap counts it among the issues the daemon has
// started, and the row says what its own next phase waits for (Placement).
//
// This is the whole product of the choices the waiting line reads, not the three fixtures the
// first fix was written against: a root's phase, its label, its design gate, its tree's
// lifecycle, the slots the root and its member hold, a question to a person, a Dispatch link
// and a chain into another tree. Each combination is placed, and the invariant is read over the
// records themselves rather than over an expected list, so a shape nobody thought of is judged
// like the rest.
func TestPlaceGivesEveryAdmittedIssueExactlyOnePlacement(t *testing.T) {
	cases := 0
	seen := map[string]int{}
	for _, stands := range []phase.Phase{phase.Admitted, phase.Implementing, phase.Done} {
		for _, labelled := range []bool{true, false} {
			for _, gate := range []string{"none", "open", "revised"} {
				for _, lifecycle := range []bool{true, false} {
					for _, slotted := range []bool{true, false} {
						for _, memberSlotted := range []bool{true, false} {
							for _, asking := range []bool{true, false} {
								for _, linked := range []bool{true, false} {
									for _, chained := range []bool{true, false} {
										cases++
										assertPlacement(t, seen, shape{rootStands: stands, labelled: labelled, gate: gate,
											lifecycle: lifecycle, slotted: slotted, memberSlotted: memberSlotted,
											asking: asking, linked: linked, chained: chained})
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if cases != 3*2*3*2*2*2*2*2*2 {
		t.Fatalf("the generator ran %d shapes, want the whole product", cases)
	}
	// What the product reached, so a later change that stops exercising a placement is a
	// failure here rather than a silent gap. The accepted-ancestor hold is not among them: it
	// needs a member the daemon has accepted with work under it, which this shape does not
	// write, and internal/record's own table covers it.
	for _, placement := range []string{"active", "waiting", "blocked_by", "tree_closed", "design_gate",
		"awaiting_person", "unlabelled", "unreleased_work"} {
		if seen[placement] == 0 {
			t.Errorf("no shape of the product placed an issue as %q: %v", placement, seen)
		}
	}
	// No shape leaves a hold this file cannot name. A row of that kind is still better than
	// the issue vanishing, which is what it is for, but it is a gap in record.waitOf.
	if seen[string(record.WaitUnexplained)] != 0 {
		t.Errorf("%d placements name no hold at all: %v", seen[string(record.WaitUnexplained)], seen)
	}
}

// assertPlacement reads one shape's three lists and holds them to the invariant.
func assertPlacement(t *testing.T, seen map[string]int, s shape) {
	t.Helper()
	issues, slots, rows, facts := s.records()
	line := record.Waiting(issues, slots, facts)
	placed := Place(rows, issues, slots, line, facts)

	active := make([]string, 0, len(slots))
	for _, slot := range slots {
		active = append(active, slot.Issue)
	}
	where := make(map[string][]string, len(issues))
	for _, key := range active {
		where[key] = append(where[key], "active")
		seen["active"]++
	}
	for _, issue := range placed.Waiting {
		where[issue.Key] = append(where[issue.Key], "waiting")
		seen["waiting"]++
	}
	for _, row := range placed.Blocked {
		where[row.Issue] = append(where[row.Issue], "blocked")
		if row.Wait != nil {
			seen[string(row.Wait.Kind)]++
		} else {
			seen["blocked_by"]++
		}
		if len(row.BlockedBy) == 0 && row.Wait == nil {
			t.Errorf("%s: the row for %s says neither what it waits on nor what holds it", s, row.Issue)
		}
		if row.Wait != nil && len(row.BlockedBy) > 0 {
			t.Errorf("%s: the row for %s carries a hold %+v beside Dispatch blockers %v; a row carries one",
				s, row.Issue, *row.Wait, row.BlockedBy)
		}
	}
	slotted := make(map[string]struct{}, len(active))
	for _, key := range active {
		slotted[key] = struct{}{}
	}
	for _, issue := range issues {
		if issue.Phase != phase.Admitted || !record.InWorkflow(issue) {
			continue
		}
		stands := where[issue.Key]
		if len(stands) == 0 {
			t.Fatalf("%s: %s is recorded admitted and stands in none of active, waiting and blocked", s, issue.Key)
		}
		if slices.Contains(stands, "waiting") && len(stands) != 1 {
			t.Fatalf("%s: %s stands in %v: an issue waiting for a slot neither holds one nor is held", s, issue.Key, stands)
		}
		if len(stands) > 1 {
			if _, holds := slotted[issue.Key]; !slices.Equal(stands, []string{"active", "blocked"}) || !holds {
				t.Fatalf("%s: %s stands in %v, want one place, or `active` and `blocked` for an issue holding a slot",
					s, issue.Key, stands)
			}
		}
	}
	// The line itself is untouched: every member of it is still in it, placed somewhere, and
	// the order `waiting` publishes is the line's own.
	for _, entry := range line {
		if len(where[entry.Issue.Key]) == 0 {
			t.Fatalf("%s: %s stands in the line and in none of the three lists", s, entry.Issue.Key)
		}
	}
	queue := make([]string, 0, len(line))
	for _, entry := range line {
		if slices.ContainsFunc(placed.Waiting, func(issue record.Issue) bool { return issue.Key == entry.Issue.Key }) {
			queue = append(queue, entry.Issue.Key)
		}
	}
	for i, issue := range placed.Waiting {
		if queue[i] != issue.Key {
			t.Fatalf("%s: waiting = %v, want the line's own order", s, placed.Waiting)
		}
	}
}

// Place reads the same records the line does and reaches the same issues through either of its
// two callers' reads: the state's pass, which holds every issue of the project, and the
// controller's tick notice, which holds the live ones alone and the records above them
// (record.LineFacts.Parents). A member stranded under a closed root is reported to both.
func TestPlaceReportsAStrandedMemberToBothOfItsCallers(t *testing.T) {
	const root, member = "LEGION-320", "LEGION-321"
	closed := record.Issue{Key: root, Tree: root, Project: "LEGION", Status: "done", Phase: phase.Done, HandedOver: true}
	stranded := record.Issue{Key: member, Tree: root, Parent: record.ParentOf(root), Project: "LEGION",
		Status: "todo", Phase: phase.Admitted}
	want := record.BlockedIssue{Issue: member, Wait: &record.Wait{Kind: record.WaitTreeClosed, Issue: root}}

	for _, tc := range []struct {
		name   string
		issues []record.Issue
		facts  record.LineFacts
	}{
		{name: "the state's pass", issues: []record.Issue{closed, stranded}},
		{name: "the tick notice's pass", issues: []record.Issue{stranded},
			facts: record.LineFacts{Parents: map[string]record.Issue{root: closed}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := record.Waiting(tc.issues, nil, tc.facts)
			placed := Place(map[string]Row{}, tc.issues, nil, line, tc.facts)
			if len(placed.Waiting) != 0 {
				t.Fatalf("waiting = %v, want nothing: this case proves nothing otherwise", placed.Waiting)
			}
			if len(placed.Blocked) != 1 || placed.Blocked[0].Issue != want.Issue ||
				placed.Blocked[0].Wait == nil || *placed.Blocked[0].Wait != *want.Wait {
				t.Fatalf("blocked = %+v, want %s held by the hand-over %s owes it", placed.Blocked, member, root)
			}
		})
	}
}
