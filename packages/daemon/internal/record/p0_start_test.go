package record

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// keptRule is the rule SlotRules keeps under name, and fails t where it keeps none.
func keptRule(t *testing.T, name string) SlotRule {
	t.Helper()
	rule, kept := SlotRuleNamed(name)
	if !kept {
		t.Fatalf("the repository keeps no rule named %q", name)
	}
	return rule
}

// An issue starts outside the cap only under a rule whose field says so (StartsNow), and only by
// its own Dispatch priority: a P0 root's child with no priority of its own takes P0 for its place
// in the line (PriorityOf) and still starts under the cap, and so does an issue at P1 or with no
// priority. A rule that names none is the daemon's own, whose field is on since
// cybermonklabs/legion#5488.
func TestAnIssueStartsOutsideTheCapByItsOwnP0UnderARuleThatSaysSo(t *testing.T) {
	root := withPriority(linePartIssue("ACME-1", "ACME-1", "in_progress", phase.Implementing, "a"), P0)
	child := linePartIssue("ACME-2", "ACME-1", "todo", phase.Admitted, "b")
	urgent := withPriority(linePartIssue("ACME-3", "ACME-3", "todo", phase.Admitted, "c"), P0)
	high := withPriority(linePartIssue("ACME-4", "ACME-4", "todo", phase.Admitted, "d"), P1)
	none := linePartIssue("ACME-5", "ACME-5", "todo", phase.Admitted, "e")
	if PriorityOf(child, root) != P0 {
		t.Fatalf("the child takes %v from its P0 root for its place in the line, want P0: the premise is a child placed at P0", PriorityOf(child, root))
	}
	for _, tc := range []struct {
		rule SlotRule
		want map[string]bool
	}{
		{MainRule, map[string]bool{"ACME-1": true, "ACME-2": false, "ACME-3": true, "ACME-4": false, "ACME-5": false}},
		{keptRule(t, "before-p0"), map[string]bool{"ACME-1": false, "ACME-2": false, "ACME-3": false, "ACME-4": false, "ACME-5": false}},
		{SlotRule{}, map[string]bool{"ACME-1": true, "ACME-2": false, "ACME-3": true, "ACME-4": false, "ACME-5": false}},
	} {
		for _, issue := range []Issue{root, child, urgent, high, none} {
			if got := tc.rule.StartsOutsideCap(issue); got != tc.want[issue.Key] {
				t.Errorf("under the rule %q, %s at its own priority %q starts outside the cap: %t, want %t",
					tc.rule.Name, issue.Key, issue.Priority, got, tc.want[issue.Key])
			}
		}
	}
}

// The cap counts every slot holder but the ones that started outside it, and a P0 bound counts
// those: under a rule that starts a P0 outside the cap, a holder whose own priority is P0 is a P0
// holder, and a holder below P0, a P0 root's child with none of its own among them, is the cap's.
// A slot whose issue the project's records do not hold is counted nowhere, as HoldersOf, the one
// count of the holders, counts it (cybermonklabs/legion#5488). Under a rule that starts no P0
// outside the cap, every holder is the cap's. Counting the project's holders one by one
// (SlotHolders.Add) gives what the count of the slots gives.
func TestTheCapCountsTheHoldersBelowP0AndTheP0BoundTheRest(t *testing.T) {
	root := withPriority(linePartIssue("ACME-1", "ACME-1", "in_progress", phase.Implementing, "a"), P0)
	child := linePartIssue("ACME-2", "ACME-1", "in_progress", phase.Implementing, "b")
	urgent := withPriority(linePartIssue("ACME-3", "ACME-3", "in_progress", phase.Implementing, "c"), P0)
	high := withPriority(linePartIssue("ACME-4", "ACME-4", "in_progress", phase.Implementing, "d"), P1)
	waiting := withPriority(linePartIssue("ACME-6", "ACME-6", "todo", phase.Admitted, "f"), P0)
	issues := []Issue{root, child, urgent, high, waiting}
	own := []Slot{{Issue: "ACME-1", Index: 1}, {Issue: "ACME-2", Index: 2}, {Issue: "ACME-3", Index: 3}, {Issue: "ACME-4", Index: 4},
		{Issue: "ACME-7", Index: 5}}
	for _, tc := range []struct {
		rule SlotRule
		want SlotHolders
	}{
		{MainRule, SlotHolders{Capped: 2, P0: 2}},
		{keptRule(t, "before-p0"), SlotHolders{Capped: 4}},
		{keptRule(t, "legion-v20.42.5"), SlotHolders{Capped: 4}},
	} {
		if got := heldUnder(tc.rule, HoldersOf(issues, own)); got != tc.want {
			t.Errorf("under the rule %q the holders count %+v, want %+v", tc.rule.Name, got, tc.want)
		}
		var added SlotHolders
		for _, issue := range []Issue{root, child, urgent, high} {
			added.Add(issue)
		}
		if got := heldUnder(tc.rule, added); got != tc.want {
			t.Errorf("under the rule %q the holders added one by one count %+v, want %+v as HoldersOf counts them", tc.rule.Name, got, tc.want)
		}
		if empty := heldUnder(tc.rule, HoldersOf(issues, nil)); empty != (SlotHolders{}) {
			t.Errorf("under the rule %q no slot counts %+v holders, want none", tc.rule.Name, empty)
		}
	}
	lowered := withPriority(urgent, P2)
	if got := heldUnder(MainRule, HoldersOf([]Issue{root, child, lowered, high}, own[:4])); got != (SlotHolders{Capped: 3, P0: 1}) {
		t.Errorf("with ACME-3 lowered to P2 while it holds its slot, the holders count %+v under main, want 3 of the cap's and 1 P0", got)
	}
}

// heldUnder is held as the cap and a P0 bound count it under rule: the holders the cap counts
// (SlotRule.CapCounted), and the rest, the P0 holders that started outside it.
func heldUnder(rule SlotRule, held SlotHolders) SlotHolders {
	capped := rule.CapCounted(held)
	return SlotHolders{Capped: capped, P0: held.Total() - capped}
}
