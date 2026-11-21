// priority_expected_test.go drives the precondition of PATCH /api/v1/priorities/{id}
// (priorities.go, patchPriorityInput.Expected) against the database store/storetest's Open
// clones from this package's migrated template, through the handler the neighbouring tests drive
// (newTestHandlerWithStore, dispatchRequest).
//
// What it reads is the write a reader's edit makes as they type: its title or description,
// with the value the edit started from beside it. Where somebody else wrote the attribute in
// between, the write is refused 409 PRIORITY_CHANGED and states nothing — no tuple, so no act in
// the history, and no `priority.updated` — rather than writing over that change unseen. A write
// whose value is still the one stored lands, a write naming no expected writes as it always has,
// and two writes whose edits started from one value cannot both land.
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/prioritystore"
)

func TestPriorityPatchExpected(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()
	const id = "ship-the-daemon"
	made := createTestPriority(t, handler, map[string]any{
		"id": id, "title": "Ship the daemon", "description": "# Why\n\nThe daemon runs every workspace.",
	}, "alice")

	// priorityUpdates counts the priority.updated events the store holds, which is what each
	// write that lands appends one of (priorityEvent).
	priorityUpdates := func(t *testing.T) int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx,
			`select count(*) from events where type = 'priority.updated'`).Scan(&count); err != nil {
			t.Fatalf("count the priority.updated events: %v", err)
		}
		return count
	}
	// changed reads a write the precondition refused: 409 PRIORITY_CHANGED, its message naming
	// each value stored now that it quotes, and the priority, its history and the event log
	// exactly as they were before it. It answers the message.
	changed := func(t *testing.T, what string, body map[string]any, quotes ...string) string {
		t.Helper()
		before := readTestPriority(t, handler, id)
		acts, events := len(readPriorityHistory(t, handler, id)), priorityUpdates(t)
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/"+id, body, "alice")
		code, text := refusalOf(t, response)
		if response.Code != http.StatusConflict || code != codePriorityChanged {
			t.Fatalf("%s: status=%d code=%s error=%.300q, want 409 %s", what, response.Code, code, text, codePriorityChanged)
		}
		for _, quote := range quotes {
			if !strings.Contains(text, quote) {
				t.Fatalf("%s is refused with %.600q, want it to name %.300s", what, text, quote)
			}
		}
		if after := readTestPriority(t, handler, id); after.Title != before.Title || after.Description != before.Description {
			t.Fatalf("%s moved the priority from title %q description %q to %q %q, want it as it was",
				what, before.Title, before.Description, after.Title, after.Description)
		}
		if got := len(readPriorityHistory(t, handler, id)); got != acts {
			t.Fatalf("%s left %d acts in the history, want the %d it held", what, got, acts)
		}
		if got := priorityUpdates(t); got != events {
			t.Fatalf("%s appended %d priority.updated events, want none", what, got-events)
		}
		return text
	}
	// lands reads a write that is written: 200 with the priority as it now stands, one act in
	// the history for each attribute it names, and one priority.updated.
	lands := func(t *testing.T, what string, body map[string]any, attributes int, login string) model.Priority {
		t.Helper()
		acts, events := len(readPriorityHistory(t, handler, id)), priorityUpdates(t)
		written := patchTestPriority(t, handler, id, body, login)
		if got := len(readPriorityHistory(t, handler, id)); got != acts+attributes {
			t.Fatalf("%s left %d acts in the history, want the %d it held and %d more", what, got, acts, attributes)
		}
		if got := priorityUpdates(t); got != events+1 {
			t.Fatalf("%s appended %d priority.updated events, want one", what, got-events)
		}
		return written
	}

	t.Run("a write naming no expected writes as before", func(t *testing.T) {
		// Bob rewrites both attributes while Alice's edit, which started from what the create
		// wrote, is still open. His write names no expected, so it lands whatever is stored.
		written := lands(t, "a rename naming no expected", map[string]any{
			"title": "Ship the daemon everywhere", "description": "Every pod runs it.",
		}, 2, "bob")
		if written.Title != "Ship the daemon everywhere" || written.Description != "Every pod runs it." {
			t.Fatalf("the write naming no expected answered %+v, want the title and description it wrote", written)
		}
	})

	t.Run("a stale expected is refused and writes nothing", func(t *testing.T) {
		changed(t, "a rename from the title Bob wrote over", map[string]any{
			"title": "Ship it", "expected": map[string]any{"title": made.Title},
		}, `title is now "Ship the daemon everywhere"`)
		changed(t, "a description from the one Bob wrote over", map[string]any{
			"description": "Alice's reasons", "expected": map[string]any{"description": made.Description},
		}, `description is now "Every pod runs it."`)
		// One attribute whose value is still the one stored does not carry the other: the
		// whole write is refused, naming the one that moved.
		if text := changed(t, "a write whose description is stale and title is not", map[string]any{
			"title": "Ship it", "description": "Alice's reasons",
			"expected": map[string]any{"title": "Ship the daemon everywhere", "description": made.Description},
		}, `description is now "Every pod runs it."`); strings.Contains(text, "title is now") {
			t.Fatalf("the refusal %q names the title, whose expected is the one stored", text)
		}

		// A stored value past the quote's bound is quoted to it, with a trailing ellipsis.
		long := strings.Repeat("a", priorityChangedQuoteMax) + "TAIL"
		lands(t, "a long description", map[string]any{"description": long}, 1, "bob")
		changed(t, "a description from before the long one", map[string]any{
			"description": "Alice's reasons", "expected": map[string]any{"description": "Every pod runs it."},
		}, `description is now "`+strings.Repeat("a", priorityChangedQuoteMax)+`…"`)
	})

	t.Run("a matching expected writes", func(t *testing.T) {
		stored := readTestPriority(t, handler, id)
		renamed := lands(t, "a rename from the title stored", map[string]any{
			"title": "Ship it", "expected": map[string]any{"title": stored.Title},
		}, 1, "alice")
		if renamed.Title != "Ship it" || renamed.Description != stored.Description {
			t.Fatalf("the rename answered title %q description %q, want %q and the description it left alone",
				renamed.Title, renamed.Description, "Ship it")
		}
		described := lands(t, "a description from the one stored", map[string]any{
			"description": "Alice's reasons", "expected": map[string]any{"description": stored.Description},
		}, 1, "alice")
		if described.Description != "Alice's reasons" {
			t.Fatalf("the description write answered %q, want the one it wrote", described.Description)
		}
		if got := readTestPriority(t, handler, id); got.Title != "Ship it" || got.Description != "Alice's reasons" {
			t.Fatalf("the priority reads back title %q description %q, want what the two writes wrote", got.Title, got.Description)
		}
	})

	t.Run("an expected beside no write of its attribute is refused", func(t *testing.T) {
		for name, body := range map[string]map[string]any{
			"title":       {"owner": "carol", "expected": map[string]any{"title": "Ship it"}},
			"description": {"title": "Ship it now", "expected": map[string]any{"description": "Alice's reasons"}},
		} {
			before := readTestPriority(t, handler, id)
			response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/"+id, body, "alice")
			code, text := refusalOf(t, response)
			if response.Code != http.StatusBadRequest || code != codePriorityInput {
				t.Fatalf("expected.%s with no %s written: status=%d code=%s error=%.300q, want 400 %s",
					name, name, response.Code, code, text, codePriorityInput)
			}
			if !strings.Contains(text, "expected."+name) || !strings.Contains(text, "writes no "+name) {
				t.Fatalf("expected.%s with no %s written is refused with %q, want it to say the write writes no %s",
					name, name, text, name)
			}
			if after := readTestPriority(t, handler, id); after.Title != before.Title ||
				after.Description != before.Description || after.Owner != before.Owner {
				t.Fatalf("the refused write moved the priority from %+v to %+v", before, after)
			}
		}
		missing := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/no-such-priority",
			map[string]any{"title": "Anything", "expected": map[string]any{"title": "Nothing"}}, "alice")
		if code, text := refusalOf(t, missing); missing.Code != http.StatusNotFound || code != codePriorityNotFound {
			t.Fatalf("an expected on an id no priority carries: status=%d code=%s error=%q, want 404 %s",
				missing.Code, code, text, codePriorityNotFound)
		}
	})

	t.Run("two writes from one value do not both land", func(t *testing.T) {
		// A transaction of the test's own holds the priority's row, so each write reaches the
		// row and waits there before either has compared; when the row comes free, one write
		// takes it and writes, and the other compares against what that one wrote.
		from := readTestPriority(t, handler, id).Title
		acts, events := len(readPriorityHistory(t, handler, id)), priorityUpdates(t)
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the transaction that holds the row: %v", err)
		}
		release := sync.OnceFunc(func() { _ = holder.Rollback(ctx) })
		if _, err := holder.Exec(ctx, `select 1 from priority_nodes where kind = $1 and id = $2 for update`,
			prioritystore.KindPriority, id); err != nil {
			release()
			t.Fatalf("hold the priority's row: %v", err)
		}
		titles := []string{"Ship it from Alice", "Ship it from Bob"}
		logins := []string{"alice", "bob"}
		responses := make([]*httptest.ResponseRecorder, len(titles))
		var writes sync.WaitGroup
		t.Cleanup(func() {
			release()
			writes.Wait()
		})
		for index, title := range titles {
			writes.Go(func() {
				responses[index] = dispatchRequest(t, handler, http.MethodPatch, "/api/v1/priorities/"+id,
					map[string]any{"title": title, "expected": map[string]any{"title": from}}, logins[index])
			})
		}
		// Each write is waiting when the backend running it waits on a lock with a statement
		// of the priority's row in hand, whichever of the row's statements that is.
		deadline := time.Now().Add(20 * time.Second)
		for {
			var waiting int
			if err := database.Pool.QueryRow(ctx, `
				select count(*) from pg_stat_activity
				where datname = current_database() and pid <> pg_backend_pid()
					and wait_event_type = 'Lock' and query like '%priority_nodes%'
			`).Scan(&waiting); err != nil {
				t.Fatalf("read the writes waiting on the row: %v", err)
			}
			if waiting == len(titles) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d of the %d writes wait on the priority's row 20s on", waiting, len(titles))
			}
			time.Sleep(10 * time.Millisecond)
		}
		release()
		writes.Wait()

		winner := -1
		for index, response := range responses {
			if response.Code == http.StatusOK {
				if winner >= 0 {
					t.Fatalf("both writes from the title %q landed: %s and %s", from,
						responses[winner].Body.String(), response.Body.String())
				}
				winner = index
			}
		}
		if winner < 0 {
			t.Fatalf("neither write from the title %q landed: %s and %s", from,
				responses[0].Body.String(), responses[1].Body.String())
		}
		loser := responses[1-winner]
		code, text := refusalOf(t, loser)
		if loser.Code != http.StatusConflict || code != codePriorityChanged {
			t.Fatalf("the second write: status=%d code=%s error=%q, want 409 %s", loser.Code, code, text, codePriorityChanged)
		}
		if want := `title is now "` + titles[winner] + `"`; !strings.Contains(text, want) {
			t.Fatalf("the second write is refused with %q, want it to name %s", text, want)
		}
		if got := readTestPriority(t, handler, id).Title; got != titles[winner] {
			t.Fatalf("the title reads %q, want %q, which the write that landed wrote", got, titles[winner])
		}
		if got := len(readPriorityHistory(t, handler, id)); got != acts+1 {
			t.Fatalf("the two writes left %d acts in the history, want the %d it held and one more", got, acts)
		}
		if got := priorityUpdates(t); got != events+1 {
			t.Fatalf("the two writes appended %d priority.updated events, want one", got-events)
		}
	})
}
