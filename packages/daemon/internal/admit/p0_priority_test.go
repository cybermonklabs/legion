package admit

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The daemon keeps each issue's own Dispatch priority on every path and the state gives it (C1 of
// dispatch://INFRA-5636, which dispatch://INFRA-3576 built and this change proves again at its own
// head), and the state gives the number of P0 holders beside the slots in use and the cap (C4).
// Every priority is taken from the list p0Priorities names, never a sample of it.

// level is priority as Dispatch sends it: 0 to 3, or nil for none.
func level(priority record.Priority) *int { return priority.Level() }

// Each path from Dispatch stores the priority it carries, each of the list: an issue event, a
// boot-listing row and a detail read. The issue is a recorded root waiting at P2, so a path that
// left the stored priority alone would read P2, and the list holds P2 itself only once, where the
// path is read against another issue's P2 record.
func TestEveryPathFromDispatchStoresEachPriority(t *testing.T) {
	for _, priority := range p0Priorities {
		start := record.P2
		if priority == record.P2 {
			start = record.P3
		}
		t.Run(priorityName(priority), func(t *testing.T) {
			t.Run("an issue event", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				p0Queued(t, pool, "LEGION-9", "C", start, func(issue *record.Issue) { issue.LastDispatchSeq = 1 })
				apply(t, pool, admission, "event", priorityEvent("LEGION-9", "todo", "C", 2, priority), priorityEngine())
				if got := issue(t, pool, "LEGION-9").Priority; got != priority {
					t.Fatalf("the event's %s stored as %s", priorityName(priority), priorityName(got))
				}
			})
			t.Run("a boot-listing row", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				p0Queued(t, pool, "LEGION-9", "C", start, func(issue *record.Issue) { issue.LastDispatchSeq = 1 })
				reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C",
					HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets", Priority: level(priority)}}, 2, 2, true)
				if got := issue(t, pool, "LEGION-9").Priority; got != priority {
					t.Fatalf("the listing's %s stored as %s", priorityName(priority), priorityName(got))
				}
			})
			t.Run("a detail read", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				detail := dispatch.Issue{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C", HandedOver: true, LastSeq: 1,
					ResolvedRepo: "acme/widgets", Priority: level(priority)}
				applyFact(t, pool, admission, "detail", intake.ObservedIssue(detail))
				if got := issue(t, pool, "LEGION-9").Priority; got != priority {
					t.Fatalf("the detail's %s stored as %s", priorityName(priority), priorityName(got))
				}
			})
		})
	}
}

// A move from P0 to no priority is applied from what Dispatch sent, whichever path sent it: an
// issue event, and the issue a status write's PATCH returns (intake.OwnStatusWrite), which an API
// route applies in the request. The holder is P0, so a path that kept the stored priority would
// leave P0.
func TestAMoveFromP0ToNoPriorityIsApplied(t *testing.T) {
	t.Run("an issue event", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P0)
		apply(t, pool, admission, "taken-off", priorityEvent("LEGION-1", "in_progress", "A", 2, record.NoPriority), priorityEngine())
		if got := issue(t, pool, "LEGION-1").Priority; got != record.NoPriority {
			t.Fatalf("LEGION-1 after the event that took its P0 off stands at %s, want none", priorityName(got))
		}
	})
	t.Run("a status write's returned issue", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P0)
		written := intake.OwnStatusWrite(dispatch.Issue{Key: "LEGION-1", Title: "LEGION-1", Status: "in_progress", Rank: "A", HandedOver: true,
			LastSeq: 2, ResolvedRepo: "acme/widgets"})
		applyFact(t, pool, admission, "own-status-write", written)
		if got := issue(t, pool, "LEGION-1").Priority; got != record.NoPriority {
			t.Fatalf("LEGION-1 after its status write returned it with no priority stands at %s, want none", priorityName(got))
		}
	})
}

// `legion state --json` gives each issue the priority its record holds, a number from 0 for P0 to
// 3 for P3 and null for none, and `admission.p0Holders` counts the P0 issues holding a slot beside
// `admission.active`, every holder. One holder stands at each priority of the list, and one more P0
// issue waits in the line, which holds no slot and so is no P0 holder.
func TestTheStateGivesEachPriorityAndTheNumberOfP0Holders(t *testing.T) {
	pool := migratedPool(t)
	want := map[string]any{}
	for i, priority := range p0Priorities {
		key := "LEGION-" + string(rune('1'+i))
		p0Holder(t, pool, key, string(rune('A'+i)), priority)
		want[key] = levelOf(priority)
	}
	p0Queued(t, pool, "LEGION-9", "Z", record.P0, nil)
	want["LEGION-9"] = levelOf(record.P0)

	body := stateBody(t, pool)
	issues := body["issues"].(map[string]any)
	for key, priority := range want {
		view := issues[key].(map[string]any)
		got, carried := view["priority"]
		if !carried || got != priority {
			t.Errorf("%s's priority in the state = %v (carried %t), want %v", key, got, carried, priority)
		}
	}
	admission := body["admission"].(map[string]any)
	if active := admission["active"].([]any); len(active) != len(p0Priorities) {
		t.Fatalf("admission.active = %v, want the %d holders", active, len(p0Priorities))
	}
	if got := admission["p0Holders"]; got != float64(1) {
		t.Fatalf("admission.p0Holders = %v, want 1: LEGION-1 alone holds a slot at P0", got)
	}
}
