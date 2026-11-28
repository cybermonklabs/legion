package admit

import (
	"bytes"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// R19 of dispatch://INFRA-3320 and the architect's ruling
// (dispatch://INFRA-4730/comment/ada915ba-c94a-4876-8d6e-6cc1a1423f32): a todo on a child the
// daemon has not recorded under a parent in Legion's work starts the child only when a person or
// the daemon's own release_children wrote it, the writer being the transition of Dispatch's event
// log that wrote the todo (intake.DispatchIssue.Transition) where the preparation read one. A todo
// written by the parent's implementer, whose session holds a claim on the child's Dispatch chain,
// records nothing and starts nothing; until the repair is removed (dispatch://INFRA-4733), it
// queues a set-back to the status Dispatch showed before the write at the write's sequence and
// logs one line, once however often the todo is read again, or logs and queues no row when
// Dispatch showed no status before it. A person's later todo starts the child whatever actor the
// fact's own event names, and a todo no transition proves starts nothing and queues nothing.
func TestUnrecordedChildTodoByWriter(t *testing.T) {
	const parent, implementer = "LEGION-650", "ses-parent-implementer"
	agent := ownerClaim{tree: parent, issue: parent, role: claim.RoleImplementer, session: implementer, state: supervise.StateWorking}

	t.Run("parent implementer writes todo over backlog", func(t *testing.T) {
		const child = "LEGION-651"
		pool := migratedPool(t)
		var logs bytes.Buffer
		admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(&logs, nil)))
		seedRunning(t, pool, parent, "")
		seedSessionClaims(t, pool, agent)
		written := &intake.StatusTransition{Seq: 4, StatusBefore: "backlog", Session: implementer}
		applyMirrored(t, pool, admission, "implementer-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 4,
			Type: "issue.updated", Status: "todo", Title: child, Parent: parent, Rank: child, ActorSession: implementer, Transition: written}, admission.engine)
		tick(t, pool, admission, "after-implementer-todo")
		// A person's rename after the todo: Dispatch's view at its sequence, whose writer is still the
		// implementer's todo.
		applyMirrored(t, pool, admission, "person-rename", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 5,
			Status: "todo", Title: child + " renamed", Parent: parent, Rank: child, Observed: true, Transition: written}, admission.engine)
		tick(t, pool, admission, "after-person-rename")

		assertNotStarted(t, pool, child)
		if stored := maybeIssue(t, pool, child); stored != nil {
			t.Errorf("%s after the implementer's todo = %+v, want nothing recorded", child, stored)
		}
		if line, _ := passJudgement(t, pool, admission); slices.Contains(line, child) {
			t.Errorf("waiting line = %v, want no %s", line, child)
		}
		wantWrites := []record.StatusWrite{record.ReassertStatusWrite("backlog", "todo", 4)}
		if got := payloadsOf[record.StatusWrite](rowsFor(effects(t, pool), child)); !reflect.DeepEqual(got, wantWrites) {
			t.Errorf("status writes queued for %s = %+v, want %+v", child, got, wantWrites)
		}
		if lines := strings.Count(logs.String(), "sets it back"); lines != 1 {
			t.Errorf("set-back log lines = %d, want 1:\n%s", lines, logs.String())
		}
	})

	t.Run("a person writes todo after the implementer", func(t *testing.T) {
		const child = "LEGION-655"
		pool := migratedPool(t)
		admission := quiet(t, 3)
		seedRunning(t, pool, parent, "")
		seedSessionClaims(t, pool, agent)
		applyMirrored(t, pool, admission, "person-todo-after-agent", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 7,
			Type: "issue.updated", Status: "todo", Title: child, Parent: parent, Rank: child, ActorSession: implementer,
			Transition: &intake.StatusTransition{Seq: 6, StatusBefore: "backlog"}}, admission.engine)
		tick(t, pool, admission, "after-person-todo-after-agent")

		if stored := maybeIssue(t, pool, child); stored == nil || !record.InLegionsWork(*stored) || stored.Phase == phase.Admitted {
			t.Errorf("%s after a person's todo = %+v, want it recorded and started in Legion's work", child, stored)
		}
		for _, write := range payloadsOf[record.StatusWrite](rowsFor(effects(t, pool), child)) {
			if write.Kind == record.ReassertStatusRow {
				t.Errorf("%s has the set-back %+v queued, want none over a person's todo", child, write)
			}
		}
	})

	t.Run("a todo whose writer nothing proves", func(t *testing.T) {
		const child = "LEGION-656"
		pool := migratedPool(t)
		var logs bytes.Buffer
		admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(&logs, nil)))
		seedRunning(t, pool, parent, "")
		seedSessionClaims(t, pool, agent)
		applyMirrored(t, pool, admission, "unproved-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 3,
			Status: "todo", Title: child, Parent: parent, Rank: child, Observed: true, Transition: &intake.StatusTransition{}}, admission.engine)
		tick(t, pool, admission, "after-unproved-todo")

		assertNotStarted(t, pool, child)
		if stored := maybeIssue(t, pool, child); stored != nil {
			t.Errorf("%s after a todo no transition proves = %+v, want nothing recorded", child, stored)
		}
		if got := payloadsOf[record.StatusWrite](rowsFor(effects(t, pool), child)); len(got) != 0 {
			t.Errorf("status writes queued for %s = %+v, want none: no write is known to undo", child, got)
		}
		if lines := strings.Count(logs.String(), "proves no writer"); lines != 1 {
			t.Errorf("no-writer log lines = %d, want 1:\n%s", lines, logs.String())
		}
	})

	t.Run("parent implementer writes todo with no status before it", func(t *testing.T) {
		const child = "LEGION-652"
		pool := migratedPool(t)
		var logs bytes.Buffer
		admission := newAdmission(t, 3, slog.New(slog.NewTextHandler(&logs, nil)))
		seedRunning(t, pool, parent, "")
		seedSessionClaims(t, pool, agent)
		applyMirrored(t, pool, admission, "implementer-created-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 1,
			Type: "issue.created", Status: "todo", Title: child, Parent: parent, Rank: child, ActorSession: implementer}, admission.engine)
		tick(t, pool, admission, "after-implementer-created-todo")

		assertNotStarted(t, pool, child)
		if stored := maybeIssue(t, pool, child); stored != nil {
			t.Errorf("%s after the implementer's todo with no prior status = %+v, want nothing recorded", child, stored)
		}
		if got := payloadsOf[record.StatusWrite](rowsFor(effects(t, pool), child)); len(got) != 0 {
			t.Errorf("status writes queued for %s = %+v, want none when Dispatch showed no status before it", child, got)
		}
		if lines := strings.Count(logs.String(), "no status before it"); lines != 1 {
			t.Errorf("no-prior-status log lines = %d, want 1:\n%s", lines, logs.String())
		}
	})

	t.Run("person writes todo", func(t *testing.T) {
		const child = "LEGION-653"
		pool := migratedPool(t)
		admission := quiet(t, 3)
		seedRunning(t, pool, parent, "")
		seedSessionClaims(t, pool, agent)
		applyMirrored(t, pool, admission, "person-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2,
			Type: "issue.updated", Status: "todo", Title: child, Parent: parent, Rank: child}, admission.engine)
		tick(t, pool, admission, "after-person-todo")

		if stored := maybeIssue(t, pool, child); stored == nil || !record.InLegionsWork(*stored) || stored.Phase == phase.Admitted {
			t.Errorf("%s after a person's todo = %+v, want it recorded and started in Legion's work", child, stored)
		}
	})

	t.Run("daemon release_children writes todo", func(t *testing.T) {
		const child = "LEGION-654"
		pool := migratedPool(t)
		admission := quiet(t, 3)
		seedRunning(t, pool, parent, "")
		seedSessionClaims(t, pool, agent)
		written := dispatch.Issue{Key: child, Title: child, Status: "todo", Parent: new(parent), Rank: child, LastSeq: 2, ResolvedRepo: "acme/widgets"}
		event := intake.OwnStatusWrite(written)
		fact := intake.ObservedIssue(written)
		fact.Delivered = &event
		applyAsTheDaemon(t, pool, admission, "outbox", "release-children-todo", fact, admission.engine)
		tick(t, pool, admission, "after-release-children-todo")

		if stored := maybeIssue(t, pool, child); stored == nil || !record.InLegionsWork(*stored) || stored.Phase == phase.Admitted {
			t.Errorf("%s after release_children's todo = %+v, want it recorded and started in Legion's work", child, stored)
		}
	})
}
