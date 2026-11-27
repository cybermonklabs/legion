package admit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// Admission reads the priority Dispatch resolves (C1 and C2 of dispatch://INFRA-5636 v13, J1 and J3
// of dispatch://INFRA-5793): `resolved_priority`, the issue's own priority else the nearest
// ancestor's that has one, beside `priority`, the issue's own, with `priority_from`, the issue it
// comes from. A P0 issue for admission is one Dispatch resolves to P0, directly or by inheritance;
// where Dispatch sends no `resolved_priority` only an issue's own P0 is, and no Legion tree fallback
// makes one. Every resolved value is taken from p0Priorities, never a sample of it.

// outside is the issue an inherited priority comes from in these cases: an ancestor in another
// project, which the daemon records nothing of.
const outside = "OTHER-5"

// resolvedFrom is the resolved priority Dispatch sends for an issue whose own is none: priority,
// coming from outside, or a present null.
func resolvedFrom(priority record.Priority) record.Resolved {
	if priority == record.NoPriority {
		return record.Resolved{Known: true}
	}
	return record.Resolved{Priority: priority, From: outside, Known: true}
}

// resolvedPayload is the `resolved_priority` and `priority_from` of an event's payload for
// resolved: both absent where Dispatch resolves nothing, else the level, null for none.
func resolvedPayload(resolved record.Resolved) string {
	if !resolved.Known {
		return ""
	}
	level, from := "null", "null"
	if l := resolved.Priority.Level(); l != nil {
		level = fmt.Sprint(*l)
	}
	if resolved.From != "" {
		from = fmt.Sprintf("%q", resolved.From)
	}
	return fmt.Sprintf(`,"resolved_priority":%s,"priority_from":%s`, level, from)
}

// resolvedEvent is a Dispatch issue event of key with no priority of its own, decoded by the
// daemon's own decoder (intake.DecodeDispatchEvent) from the bytes Dispatch writes: resolved is
// its resolved priority as resolvedPayload writes it, and changed its `changed`.
func resolvedEvent(t *testing.T, key, status, rank string, seq int64, resolved record.Resolved, changed ...string) intake.DispatchIssue {
	t.Helper()
	names, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"key":%q,"status":%q,"title":%q,"parent":null,"rank":%q,"priority":null%s,"labels":[%q],"resolved_repo":"acme/widgets","blocked_by":[],"claim":null,"changed":%s}`,
		key, status, key, rank, resolvedPayload(resolved), dispatch.LegionLabel, names)
	event := fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":%d,"actor":{"kind":"user","id":"human"},"notify":true,"type":"issue.updated","created_at":"2026-09-23T11:00:00Z","payload":%s}`,
		seq, key, seq, payload)
	fact, unread, err := intake.DecodeDispatchEvent(testProject, key, []byte(event))
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode the event of %s: %v, unread %v", key, err, unread)
	}
	return fact.(intake.DispatchIssue)
}

// inheritedQueued records key as a root handed to Legion that never ran, with no priority of its
// own and the resolved priority resolved.
func inheritedQueued(t *testing.T, pool *pgxpool.Pool, key, rank string, resolved record.Resolved) {
	t.Helper()
	p0Queued(t, pool, key, rank, record.NoPriority, func(issue *record.Issue) { issue.Resolved, issue.LastDispatchSeq = resolved, 1 })
}

// inheritedHolder records key as a started root holding a slot, with no priority of its own and the
// resolved priority resolved.
func inheritedHolder(t *testing.T, pool *pgxpool.Pool, key, rank string, resolved record.Resolved) {
	t.Helper()
	seedSlottedInPhase(t, pool, testProject, key, rank, phase.Admitted, func(issue *record.Issue) { issue.Resolved, issue.LastDispatchSeq = resolved, 1 })
}

// stateBody is the state the projection gives now, as `legion state --json` writes it.
func stateBody(t *testing.T, pool *pgxpool.Pool) map[string]any {
	t.Helper()
	var body map[string]any
	inTx(t, pool, func(tx pgx.Tx) {
		state, err := projection.Project(context.Background(), projection.Inputs{Tx: tx, Store: record.NewStore(), Project: testProject, Now: fixedNow})
		if err != nil {
			t.Fatalf("project the state: %v", err)
		}
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("write the state: %v", err)
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("read the state back: %v", err)
		}
	})
	return body
}

// issueView is key's issue in body.
func issueView(t *testing.T, body map[string]any, key string) map[string]any {
	t.Helper()
	view, ok := body["issues"].(map[string]any)[key].(map[string]any)
	if !ok {
		t.Fatalf("the state carries no issue %s", key)
	}
	return view
}

// The decoder reads `resolved_priority` apart from `priority`: an absent field is unknown, a present
// null is no priority, and a level comes with the issue `priority_from` names; `changed` naming the
// resolved priority alone marks the event as one that changed nothing else.
func TestAnIssueEventCarriesTheResolvedPriorityApartFromItsOwn(t *testing.T) {
	if got := resolvedEvent(t, "LEGION-9", "todo", "C", 2, record.Resolved{}, "title"); got.Resolved.Known || got.ResolvedOnly {
		t.Fatalf("an event with no resolved_priority decoded to %+v, resolved only %t; want it unknown and not resolved only", got.Resolved, got.ResolvedOnly)
	}
	for _, priority := range p0Priorities {
		want := resolvedFrom(priority)
		got := resolvedEvent(t, "LEGION-9", "todo", "C", 2, want, "resolved_priority")
		if got.Resolved != want || got.Priority != record.NoPriority || !got.ResolvedOnly {
			t.Fatalf("the event of resolved %s decoded to own %s, resolved %+v, resolved only %t; want own none, %+v and resolved only",
				priorityName(priority), priorityName(got.Priority), got.Resolved, got.ResolvedOnly, want)
		}
	}
	if got := resolvedEvent(t, "LEGION-9", "todo", "C", 2, resolvedFrom(record.P0), "resolved_priority", "title"); got.ResolvedOnly {
		t.Fatal("an event whose changed names a title beside the resolved priority decoded as resolved only, want an edit of the issue")
	}
}

// Each path from Dispatch stores each resolved value it carries, with its source: an issue event, a
// boot-listing row, a detail read and the issue an accepted status write answers. The record held
// another resolved value from another issue before, so a path that kept it reads that one; a detail
// read applies as a view only to an issue the workflow records nothing of (intake's
// settleObserved), so its case records the issue from it.
func TestEveryPathFromDispatchStoresEachResolvedPriority(t *testing.T) {
	for _, priority := range p0Priorities {
		before := record.Resolved{Priority: record.P2, From: "OTHER-9", Known: true}
		if priority == record.P2 {
			before.Priority = record.P3
		}
		want := resolvedFrom(priority)
		t.Run(priorityName(priority), func(t *testing.T) {
			check := func(t *testing.T, pool *pgxpool.Pool, path string) {
				t.Helper()
				if got := issue(t, pool, "LEGION-9"); got.Resolved != want || got.Priority != record.NoPriority {
					t.Fatalf("%s's resolved %+v stored as own %s and resolved %+v", path, want, priorityName(got.Priority), got.Resolved)
				}
			}
			t.Run("an issue event", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				inheritedQueued(t, pool, "LEGION-9", "C", before)
				apply(t, pool, admission, "event", resolvedEvent(t, "LEGION-9", "todo", "C", 2, want, "resolved_priority"), priorityEngine())
				check(t, pool, "the event")
			})
			t.Run("a boot-listing row", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				inheritedQueued(t, pool, "LEGION-9", "C", before)
				reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C",
					HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets", Resolved: &dispatch.ResolvedPriority{Priority: level(want.Priority), From: want.From}}}, 2, 2, true)
				check(t, pool, "the listing")
			})
			t.Run("a detail read", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				applyFact(t, pool, admission, "detail", intake.ObservedIssue(dispatch.Issue{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C",
					HandedOver: true, LastSeq: 2, ResolvedRepo: "acme/widgets", Resolved: &dispatch.ResolvedPriority{Priority: level(want.Priority), From: want.From}}))
				check(t, pool, "the detail")
			})
			t.Run("a status write's returned issue", func(t *testing.T) {
				pool := migratedPool(t)
				admission := quiet(t, 1)
				p0Holder(t, pool, "LEGION-1", "A", record.P1)
				inheritedHolder(t, pool, "LEGION-9", "C", before)
				applyFact(t, pool, admission, "own-status-write", intake.OwnStatusWrite(dispatch.Issue{Key: "LEGION-9", Title: "LEGION-9", Status: "in_progress",
					Rank: "C", HandedOver: true, LastSeq: 2, ResolvedRepo: "acme/widgets", Resolved: &dispatch.ResolvedPriority{Priority: level(want.Priority), From: want.From}}, "", ""))
				check(t, pool, "the status write")
			})
		})
	}
}

// With a Dispatch release that resolves no priority, only an issue's own P0 makes it P0: a member
// with none of its own under a P0 tree root waits at a full cap, its event carrying no
// `resolved_priority`, while a root whose own priority is P0 starts outside the cap.
func TestWithNoResolvedPriorityOnlyAnIssuesOwnP0StartsIt(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	p0Tree(t, pool, "LEGION-A", "B", record.P0, true)
	p0Member(t, pool, "LEGION-A", "LEGION-A1", "B1", phase.Admitted, "todo", record.NoPriority)
	p0Queued(t, pool, "LEGION-9", "C", record.P0, nil)

	tick(t, pool, admission, "tick")

	if holdsSlot(t, pool, "LEGION-A1") {
		t.Fatal("LEGION-A1, with no priority of its own and no resolved_priority, holds a slot under its P0 tree root: a Legion tree fallback made it P0")
	}
	if !holdsSlot(t, pool, "LEGION-9") {
		t.Fatal("LEGION-9, at its own P0 with no resolved_priority, holds no slot: an older Dispatch's explicit P0 no longer starts outside the cap")
	}
}

// A present null is no priority and replaces the inherited P0 before it, on an event and on the
// boot listing: the holder that inherited P0 counts against the cap from then on, and the waiting
// issue that inherited it no longer starts outside it.
func TestAPresentNullReplacesAnInheritedP0(t *testing.T) {
	t.Run("a holder's event", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		inheritedHolder(t, pool, "LEGION-1", "A", resolvedFrom(record.P0))
		p0Queued(t, pool, "LEGION-9", "C", record.P2, nil)
		tick(t, pool, admission, "tick")
		assertRowHolders(t, pool, "LEGION-1", "LEGION-9")

		apply(t, pool, admission, "null", resolvedEvent(t, "LEGION-1", "in_progress", "A", 2, record.Resolved{Known: true}, "resolved_priority"), priorityEngine())
		if got := issue(t, pool, "LEGION-1"); got.Resolved != (record.Resolved{Known: true}) || got.StartsNow() {
			t.Fatalf("LEGION-1 after the present null holds %+v, starts now %t; want a known none that starts nothing", got.Resolved, got.StartsNow())
		}
		p0Queued(t, pool, "LEGION-10", "D", record.P3, nil)
		tick(t, pool, admission, "after")
		if holdsSlot(t, pool, "LEGION-10") {
			t.Fatal("LEGION-10 took a slot: the holder whose inherited P0 a present null replaced still takes none of the cap's")
		}
	})
	t.Run("a boot-listing row", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		inheritedQueued(t, pool, "LEGION-9", "C", resolvedFrom(record.P0))
		reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C",
			HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets", Resolved: &dispatch.ResolvedPriority{}}}, 2, 2, true)
		if got := issue(t, pool, "LEGION-9"); got.Resolved != (record.Resolved{Known: true}) {
			t.Fatalf("LEGION-9 after the listing's present null holds %+v, want a known none", got.Resolved)
		}
		if holdsSlot(t, pool, "LEGION-9") {
			t.Fatal("LEGION-9 holds a slot at a full cap after its inherited P0 was replaced by a present null")
		}
	})
}

// An ancestor's change of priority or of parent reaches a member as an update of the member itself,
// whose `changed` names the resolved priority alone (J3 of dispatch://INFRA-5793), from an ancestor
// in another project. The pass that applies it reads the new value — a member moved into a P0 chain
// starts at that pass at a full cap, one moved out keeps its slot and counts against the cap — and
// the update is no work of the member's own, so the holder's movement mark stays where it was, read
// with an engine whose clock stands an hour past that mark: the store moves a mark forward only
// (record's MarkIssueMoved), so a clock behind it could not show a mark the update wrote.
func TestAMemberMovedIntoOrOutOfAP0ChainWithNoEditOfItsOwn(t *testing.T) {
	t.Run("into", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		inheritedQueued(t, pool, "LEGION-9", "C", resolvedFrom(record.P2))
		tick(t, pool, admission, "tick")
		assertRowHolders(t, pool, "LEGION-1")

		apply(t, pool, admission, "into", resolvedEvent(t, "LEGION-9", "todo", "C", 2, resolvedFrom(record.P0), "resolved_priority", "priority_from"), priorityEngine())

		assertRowHolders(t, pool, "LEGION-1", "LEGION-9")
	})
	t.Run("out of", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		inheritedHolder(t, pool, "LEGION-1", "A", resolvedFrom(record.P0))
		p0Holder(t, pool, "LEGION-2", "B", record.P1)
		p0Queued(t, pool, "LEGION-9", "C", record.P2, nil)
		tick(t, pool, admission, "tick")
		assertRowHolders(t, pool, "LEGION-1", "LEGION-2")
		moved := issue(t, pool, "LEGION-1").MovedAt

		later := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
			Linger: time.Hour, Clock: func() time.Time { return moved.Add(time.Hour) }}, nil)
		apply(t, pool, admission, "out", resolvedEvent(t, "LEGION-1", "in_progress", "A", 2, resolvedFrom(record.P2), "resolved_priority"), later)
		if got := issue(t, pool, "LEGION-1").MovedAt; !got.Equal(moved) {
			t.Fatalf("LEGION-1's movement mark moved from %s to %s on an update of its resolved priority alone, want it kept", moved, got)
		}
		assertRowHolders(t, pool, "LEGION-1", "LEGION-2")
		closeTree(t, pool, admission, "second-closed", "LEGION-2", "B")
		assertRowHolders(t, pool, "LEGION-1")
	})
}

// The first boot listing after Dispatch resolves priorities replaces each recorded issue's
// resolved priority at the issue's own sequence: Dispatch's upgrade made the inherited P0 with no
// event of the issue, so the listing is all the daemon reads of it. A root that never ran, which the
// listing re-admits, starts outside the cap at that boot's pass, and a holder, whose observation the
// listing records with nothing else of it changed, leaves the cap's count and gives its slot to the
// line below P0; neither needs an edit of its own.
func TestTheFirstBootListingAfterDispatchResolvesTakesAnInheritedP0AtItsOwnSequence(t *testing.T) {
	t.Run("a root that never ran", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		p0Queued(t, pool, "LEGION-9", "C", record.NoPriority, func(issue *record.Issue) { issue.LastDispatchSeq = 2 })
		tick(t, pool, admission, "before")
		assertRowHolders(t, pool, "LEGION-1")

		reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C",
			HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets", Resolved: &dispatch.ResolvedPriority{Priority: level(record.P0), From: outside}}}, 2, 2, true)

		if got := issue(t, pool, "LEGION-9").Resolved; got != resolvedFrom(record.P0) {
			t.Fatalf("LEGION-9 after the listing at its own sequence holds %+v, want %+v", got, resolvedFrom(record.P0))
		}
		tick(t, pool, admission, "after")
		assertRowHolders(t, pool, "LEGION-1", "LEGION-9")
	})
	t.Run("a holder", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		inheritedHolder(t, pool, "LEGION-1", "A", record.Resolved{})
		holder := issue(t, pool, "LEGION-1")
		// The record stands as the listing shows it but for the resolved priority, so the listing
		// changes that alone: its own sequence, status and fields are the record's.
		holder.LastDispatchSeq, holder.DispatchStatus = 2, holder.Status
		putIssue(t, pool, holder)
		p0Queued(t, pool, "LEGION-9", "C", record.P2, nil)
		tick(t, pool, admission, "before")
		assertRowHolders(t, pool, "LEGION-1")

		reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-1", Title: holder.Title, Status: holder.Status, Rank: holder.Rank,
			HandedOver: holder.HandedOver, LastSeq: 2, ResolvedRepo: "acme/widgets", Resolved: &dispatch.ResolvedPriority{Priority: level(record.P0), From: outside}}}, 2, 2, true)

		if got := issue(t, pool, "LEGION-1").Resolved; got != resolvedFrom(record.P0) {
			t.Fatalf("LEGION-1 after the listing at its own sequence holds %+v, want %+v", got, resolvedFrom(record.P0))
		}
		tick(t, pool, admission, "after")
		assertRowHolders(t, pool, "LEGION-1", "LEGION-9")
	})
}

// An event older than the newest read the record took leaves that read's resolved priority: a boot
// listing at sequence 5 gave an inherited P0, and the event at sequence 4 that the stream delivers
// after it, with P2, changes nothing.
func TestAnOlderEventLeavesANewerReadsResolvedPriority(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	inheritedQueued(t, pool, "LEGION-9", "C", resolvedFrom(record.P2))
	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{{Key: "LEGION-9", Title: "LEGION-9", Status: "todo", Rank: "C",
		HandedOver: handed, LastSeq: 5, ResolvedRepo: "acme/widgets", Resolved: &dispatch.ResolvedPriority{Priority: level(record.P0), From: outside}}}, 5, 5, true)
	if got := issue(t, pool, "LEGION-9").Resolved; got != resolvedFrom(record.P0) {
		t.Fatalf("LEGION-9 after the listing at sequence 5 holds %+v, want %+v", got, resolvedFrom(record.P0))
	}
	apply(t, pool, admission, "older", resolvedEvent(t, "LEGION-9", "todo", "C", 4, resolvedFrom(record.P2), "resolved_priority"), priorityEngine())
	if got := issue(t, pool, "LEGION-9").Resolved; got != resolvedFrom(record.P0) {
		t.Fatalf("LEGION-9 after the older event holds %+v, want the newer read's %+v", got, resolvedFrom(record.P0))
	}
}

// `legion state --json` gives the issue's own priority and the resolved one apart: own null,
// resolved 0 from the issue it comes from; an issue Dispatch resolved nothing for carries no
// resolved key at all; and an inherited P0 holder is a P0 holder.
func TestTheStateGivesTheOwnAndTheResolvedPriorityApart(t *testing.T) {
	pool := migratedPool(t)
	inheritedHolder(t, pool, "LEGION-1", "A", resolvedFrom(record.P0))
	p0Holder(t, pool, "LEGION-2", "B", record.P1)
	body := stateBody(t, pool)

	first := issueView(t, body, "LEGION-1")
	if priority, carried := first["priority"]; !carried || priority != nil {
		t.Errorf("LEGION-1's priority = %v (carried %t), want null, its own", priority, carried)
	}
	if first["resolvedPriority"] != float64(0) || first["priorityFrom"] != outside {
		t.Errorf("LEGION-1's resolvedPriority and priorityFrom = %v and %v, want 0 and %s", first["resolvedPriority"], first["priorityFrom"], outside)
	}
	second := issueView(t, body, "LEGION-2")
	if _, carried := second["resolvedPriority"]; carried {
		t.Errorf("LEGION-2 carries a resolvedPriority of %v, want none: Dispatch resolved nothing for it", second["resolvedPriority"])
	}
	if second["priority"] != float64(1) {
		t.Errorf("LEGION-2's priority = %v, want 1", second["priority"])
	}
	if got := body["admission"].(map[string]any)["p0Holders"]; got != float64(1) {
		t.Fatalf("admission.p0Holders = %v, want 1: LEGION-1 holds its slot at an inherited P0", got)
	}
}

// The P0 start reads Dispatch's resolved priority and never the Legion tree's: under a running tree
// root at P2, a member that inherits P0 from outside starts at a full cap, and under a tree root at
// P0, a member whose own P1 Dispatch resolves to P1 waits in the P1 band, behind no P0 issue, and
// takes no slot.
func TestAnInheritedP0StartsAndAnOwnP1UnderAP0RootDoesNot(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	p0Tree(t, pool, "LEGION-A", "B", record.P2, true)
	p0Member(t, pool, "LEGION-A", "LEGION-A1", "B1", phase.Admitted, "todo", record.NoPriority)
	p0Tree(t, pool, "LEGION-B", "C", record.P0, true)
	p0Member(t, pool, "LEGION-B", "LEGION-B1", "C1", phase.Admitted, "todo", record.P1)
	inTx(t, pool, func(tx pgx.Tx) {
		for key, resolved := range map[string]record.Resolved{
			"LEGION-A": {Priority: record.P2, Known: true}, "LEGION-A1": resolvedFrom(record.P0),
			"LEGION-B": {Priority: record.P0, Known: true}, "LEGION-B1": {Priority: record.P1, Known: true},
		} {
			stored, err := record.NewStore().Issue(context.Background(), tx, key)
			if err != nil || stored == nil {
				t.Fatalf("read %s: %v", key, err)
			}
			stored.Resolved = resolved
			if err := record.NewStore().PutIssue(context.Background(), tx, *stored); err != nil {
				t.Fatalf("write %s: %v", key, err)
			}
		}
	})
	p0Queued(t, pool, "LEGION-9", "D", record.P1, func(issue *record.Issue) { issue.Resolved = record.Resolved{Priority: record.P1, Known: true} })

	tick(t, pool, admission, "tick")

	if !holdsSlot(t, pool, "LEGION-A1") {
		t.Fatal("LEGION-A1, which Dispatch resolves to P0 from outside its P2 tree, holds no slot at a full cap")
	}
	if holdsSlot(t, pool, "LEGION-B1") {
		t.Fatal("LEGION-B1, at its own P1 under a P0 tree root, holds a slot: the tree root's P0 started it")
	}
	body := stateBody(t, pool)
	line := issueView(t, body, "LEGION-B1")["waitingLine"].(map[string]any)
	if line["priority"] != float64(1) {
		t.Fatalf("LEGION-B1 waits at the line's priority %v, want 1: an issue Dispatch resolves below P0 entered the P0 band", line["priority"])
	}
	if reason, _ := line["reason"].(string); !strings.Contains(reason, "its own priority") {
		t.Fatalf("LEGION-B1's reason is %q, want it to name its own priority", reason)
	}
}

// The waiting line's reason names the issue Dispatch's `priority_from` names for an inherited
// priority, never the Legion tree root.
func TestTheReasonNamesPriorityFromAndNotTheTreeRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	p0Holder(t, pool, "LEGION-1", "A", record.P1)
	inheritedQueued(t, pool, "LEGION-9", "C", resolvedFrom(record.P2))
	tick(t, pool, admission, "tick")

	reason, _ := issueView(t, stateBody(t, pool), "LEGION-9")["waitingLine"].(map[string]any)["reason"].(string)
	if !strings.Contains(reason, "the priority of "+outside) || strings.Contains(reason, "tree root") {
		t.Fatalf("LEGION-9's reason is %q, want it to name %s, the issue its priority comes from, and no tree root", reason, outside)
	}
}

// The P0 bound counts an inherited P0 holder as it counts an explicit one: at a bound of 1 that an
// inherited P0 holder fills, an explicit P0 issue waits, and at a bound of 1 an explicit P0 holder
// fills, an inherited P0 issue waits.
func TestTheP0BoundCountsAnInheritedP0AsAnExplicitOne(t *testing.T) {
	t.Run("an inherited holder", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		admission.UseP0Bound(new(1))
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		inheritedHolder(t, pool, "LEGION-2", "B", resolvedFrom(record.P0))
		p0Queued(t, pool, "LEGION-9", "C", record.P0, nil)
		tick(t, pool, admission, "tick")
		if holdsSlot(t, pool, "LEGION-9") {
			t.Fatal("LEGION-9, an explicit P0, holds a slot at a bound of 1 an inherited P0 holder fills")
		}
	})
	t.Run("an inherited candidate", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		admission.UseP0Bound(new(1))
		p0Holder(t, pool, "LEGION-1", "A", record.P1)
		p0Holder(t, pool, "LEGION-2", "B", record.P0)
		inheritedQueued(t, pool, "LEGION-9", "C", resolvedFrom(record.P0))
		tick(t, pool, admission, "tick")
		if holdsSlot(t, pool, "LEGION-9") {
			t.Fatal("LEGION-9, an inherited P0, holds a slot at a bound of 1 an explicit P0 holder fills")
		}
		if line := waitingLine(t, pool, testProject, nil); len(line) == 0 || line[0] != "LEGION-9" {
			t.Fatalf("the waiting line is %v, want LEGION-9 first: an inherited P0 at the bound waits first", line)
		}
	})
}
