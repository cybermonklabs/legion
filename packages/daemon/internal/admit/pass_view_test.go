package admit

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// lineKeys is the key of each member of line, in its order.
func lineKeys(line []record.LineEntry) []string {
	keys := make([]string, len(line))
	for i, entry := range line {
		keys[i] = entry.Issue.Key
	}
	return keys
}

// slotKeys is the issue of each slot, sorted.
func slotKeys(slots []record.Slot) []string {
	keys := make([]string, len(slots))
	for i, slot := range slots {
		keys[i] = slot.Issue
	}
	slices.Sort(keys)
	return keys
}

// The pass observer is handed each pass's view once its fact commits: the slots as the pass left
// them, the slots it gave out and took back, the project's records and the line it judged, with
// the members its last round promoted out of the line, since they wait in it no more. Under a cap
// of one, a tick whose pass takes the slot of an issue past its review and gives it to the first
// of two queued roots hands over one view: the slot is the promoted root's, the line holds the
// second root alone, and the view names the move each way. A pass whose fact rolls back is never
// seen, and an admission with no observer gathers nothing.
func TestThePassObserverSeesEachPassThatCommits(t *testing.T) {
	log := silent()

	t.Run("a pass that commits", func(t *testing.T) {
		pool := migratedPool(t)
		admission := ruledAdmission(t, 1, record.MainRule, log)
		var views []PassView
		admission.UsePassObserver(func(view PassView) { views = append(views, view) })
		seedSlottedInPhase(t, pool, acmeProject, "ACME-1", "a", phase.Accepting, func(issue *record.Issue) {
			issue.Status, issue.DispatchStatus = "needs_review", "needs_review"
		})
		acmeQueuedRoot(t, pool, "ACME-2", "b")
		acmeQueuedRoot(t, pool, "ACME-3", "c")
		tickThrough(t, pool, admission, "the pass")
		if len(views) != 1 {
			t.Fatalf("the observer saw %d views of one tick's pass, want 1", len(views))
		}
		view := views[0]
		if view.Project != acmeProject || view.Cap != 1 || view.Rule.Name != record.MainRule.Name || !view.At.Equal(fixedNow) {
			t.Errorf("the view is of %s under a cap of %d and the rule %q at %s, want %s, 1 and %q at %s", view.Project, view.Cap,
				view.Rule.Name, view.At, acmeProject, record.MainRule.Name, fixedNow)
		}
		if got := slotKeys(view.Slots); !slices.Equal(got, []string{"ACME-2"}) {
			t.Errorf("the view's slots are held by %v, want [ACME-2]: the pass took ACME-1's and gave it to ACME-2", got)
		}
		if got := lineKeys(view.Line); !slices.Equal(got, []string{"ACME-3"}) {
			t.Errorf("the view's line is %v, want [ACME-3]: the promoted ACME-2 waits in it no more", got)
		}
		wantGiven := []record.SlotGiven{{Issue: "ACME-2", Kind: record.LineQueuedRoot.String()}}
		wantTaken := []record.SlotTaken{{Issue: "ACME-1", Reason: record.TakenPastReview}}
		if !slices.Equal(view.Given, wantGiven) || !slices.Equal(view.Taken, wantTaken) {
			t.Errorf("the view gives %v and takes %v, want %v and %v", view.Given, view.Taken, wantGiven, wantTaken)
		}
		recorded := map[string]bool{}
		for _, issue := range view.Issues {
			recorded[issue.Key] = true
		}
		for _, key := range []string{"ACME-1", "ACME-2", "ACME-3"} {
			if !recorded[key] {
				t.Errorf("the view's records hold no %s", key)
			}
		}
	})

	t.Run("a pass whose fact rolls back", func(t *testing.T) {
		pool := migratedPool(t)
		admission := ruledAdmission(t, 1, record.MainRule, log)
		var views []PassView
		admission.UsePassObserver(func(view PassView) { views = append(views, view) })
		acmeQueuedRoot(t, pool, "ACME-4", "a")
		tickRolledBack(t, pool, admission)
		if len(views) != 0 {
			t.Fatalf("the observer saw %d views of a pass whose fact rolled back, want none: nothing it read or moved was kept", len(views))
		}
	})

	t.Run("an admission with no observer", func(t *testing.T) {
		pool := migratedPool(t)
		admission := ruledAdmission(t, 1, record.MainRule, log)
		acmeQueuedRoot(t, pool, "ACME-5", "a")
		tickThrough(t, pool, admission, "the pass")
		assertSlots(t, pool, []record.Slot{{Issue: "ACME-5", Index: 0, AdmittedAt: fixedNow}})
	})
}
