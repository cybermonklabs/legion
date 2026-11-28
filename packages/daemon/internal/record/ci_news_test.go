package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// ciNewsIssue is the issue the prompts here are for, and ciNewsHead the head they are at.
const (
	ciNewsIssue = "LEGION-208"
	ciNewsHead  = "c0de000000000000000000000000000000000001"
)

// prompt is a prompt to read Dispatch for the reviewer of task a at generation 3, with snapshots.
func prompt(snapshots ...CIStateSnapshot) CINews {
	return CINews{Purpose: CINewsState, Role: "legion-legion-legion-208-reviewer", Generation: 3, Task: "a", Repo: "acme/widgets",
		Head: ciNewsHead, Reader: &CIReader{Phase: phase.Reviewing, Entry: 4}, States: snapshots}
}

// ciNewsRows is every queued ci_news row of ciNewsIssue, decoded, in the order queued.
func ciNewsRows(t *testing.T, tx pgx.Tx) []CINews {
	t.Helper()
	rows, err := tx.Query(context.Background(), `select id, kind, issue, payload from outbox where issue = $1 order by id`, ciNewsIssue)
	must(t, err)
	defer rows.Close()
	var news []CINews
	for rows.Next() {
		var row OutboxRow
		must(t, rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload))
		if row.Kind != OutboxKindCINews {
			t.Fatalf("row %d of %s is of kind %s, want ci_news", row.ID, ciNewsIssue, row.Kind)
		}
		decoded, err := DecodeOutboxPayload(row)
		must(t, err)
		news = append(news, decoded.(CINews))
	}
	must(t, rows.Err())
	return news
}

// A prompt to read Dispatch carries no verdict a later state can overtake: a check's state yes
// queued for the reviewer, and the same check's no at a higher sequence before the row is
// delivered, join into one row of kind ci_news and purpose state for the reviewer, its reader and
// head X, its set of checks kept, whose snapshot is the no. The body comes from ReadPromptBody,
// which takes the head and the names of the checks and no state; the test reads no body.
func TestAQueuedPromptCarriesNoVerdict(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	yes := CIStateSnapshot{Check: "k-ci", State: "yes", Reason: "pass", CICase: "success", Roles: []string{"tester"}, Seq: 5}
	no := CIStateSnapshot{Check: "k-ci", State: "no", Reason: "fail", CICase: "failure", Roles: []string{"tester"}, Seq: 6}
	queue := func(t *testing.T) []CINews {
		t.Helper()
		records, st := NewStore(), migratedStore(t)
		ctx := context.Background()
		var news []CINews
		inTx(t, st, func(tx pgx.Tx) {
			first := prompt(yes)
			first.Checks = &[]string{"k-ci"}
			if joined, err := records.EnqueueReadPrompt(ctx, tx, ciNewsIssue, first, now); err != nil || joined {
				t.Fatalf("the first prompt = joined %t, %v; want a row of its own", joined, err)
			}
			if joined, err := records.EnqueueReadPrompt(ctx, tx, ciNewsIssue, prompt(no), now.Add(time.Second)); err != nil || !joined {
				t.Fatalf("the second prompt = joined %t, %v; want it joined to the first", joined, err)
			}
			news = ciNewsRows(t, tx)
		})
		if len(news) != 1 {
			t.Fatalf("queued prompts = %+v, want one row", news)
		}
		got := news[0]
		if got.Purpose != CINewsState || got.Role != "legion-legion-legion-208-reviewer" || got.Head != ciNewsHead ||
			!reflect.DeepEqual(got.Reader, &CIReader{Phase: phase.Reviewing, Entry: 4}) || got.Checks == nil || !reflect.DeepEqual(*got.Checks, []string{"k-ci"}) {
			t.Fatalf("the joined prompt = %+v, want the reviewer's state prompt at %s with its reader and its set kept", got, ciNewsHead)
		}
		return news
	}
	t.Run("the higher sequence's snapshot is the one held", func(t *testing.T) {
		news := queue(t)
		if !reflect.DeepEqual(news[0].States, []CIStateSnapshot{no}) {
			t.Errorf("the joined prompt's snapshots = %+v, want the no at sequence 6 alone", news[0].States)
		}
	})
}

// A prompt joins only a pending prompt of its complete work identity: a stale unleased row of the
// owner at X with entry 0, of the generation before, takes in no change of the new generation's
// owner at the same X and entry 0, which gets a row of its own. Nor does a row of another task,
// another reader phase or another fix whose other members match. A change of the same complete
// identity joins once, and a matching row a lane leases joins nothing: the change gets a row of its
// own. The delivery of these rows is TestAStalePromptCannotAbsorbANewReadersChangeAtDelivery's.
func TestAStalePromptCannotAbsorbANewReadersChange(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	owner := func(generation uint64, task string, reader CIReader) CINews {
		return CINews{Purpose: CINewsState, Role: "legion-legion-legion-208-architect", Generation: generation, Task: task, Repo: "acme/widgets",
			Head: ciNewsHead, Reader: &reader, States: []CIStateSnapshot{{Check: "k-ci", State: "no", Seq: int64(generation)}}}
	}
	accepting := CIReader{Phase: phase.Accepting}
	cases := []struct {
		name          string
		stale, change CINews
		leased, joins bool
	}{
		{name: "the owner of the generation before", stale: owner(1, "a", accepting), change: owner(2, "a", accepting)},
		{name: "another task", stale: owner(2, "a", accepting), change: owner(2, "b", accepting)},
		{name: "another reader phase", stale: owner(2, "a", CIReader{Phase: phase.Reviewing}), change: owner(2, "a", CIReader{Phase: phase.Testing})},
		{name: "another fix", stale: owner(2, "a", CIReader{Fix: 1, Entry: 5}), change: owner(2, "a", CIReader{Fix: 2, Entry: 5})},
		{name: "the same complete identity", stale: owner(2, "a", accepting), change: owner(2, "a", accepting), joins: true},
		{name: "the same identity under a lease", stale: owner(2, "a", accepting), change: owner(2, "a", accepting), leased: true},
	}
	play := func(t *testing.T, stale, change CINews, leased bool) (bool, []CINews) {
		t.Helper()
		records, st := NewStore(), migratedStore(t)
		ctx := context.Background()
		var joined bool
		var news []CINews
		inTx(t, st, func(tx pgx.Tx) {
			if j, err := records.EnqueueReadPrompt(ctx, tx, ciNewsIssue, stale, now); err != nil || j {
				t.Fatalf("the stale prompt = joined %t, %v; want a row of its own", j, err)
			}
			if leased {
				if _, err := tx.Exec(ctx, `update outbox set lease_token = 'lane-1', lease_until = $1 where issue = $2`, now.Add(time.Minute), ciNewsIssue); err != nil {
					t.Fatalf("lease the stale prompt: %v", err)
				}
			}
			var err error
			joined, err = records.EnqueueReadPrompt(ctx, tx, ciNewsIssue, change, now.Add(time.Second))
			must(t, err)
			news = ciNewsRows(t, tx)
		})
		return joined, news
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			joined, news := play(t, tc.stale, tc.change, tc.leased)
			if tc.joins {
				if !joined || len(news) != 1 {
					t.Fatalf("the change = joined %t with %d rows, want it joined to the one row", joined, len(news))
				}
				return
			}
			if joined || len(news) != 2 {
				t.Fatalf("the change = joined %t with %d rows, want a row of its own beside the stale one", joined, len(news))
			}
			if news[0].Generation != tc.stale.Generation || news[0].Task != tc.stale.Task || !reflect.DeepEqual(news[0].Reader, tc.stale.Reader) ||
				!reflect.DeepEqual(news[0].States, tc.stale.States) {
				t.Errorf("the stale row after the change = %+v, want it as queued: %+v", news[0], tc.stale)
			}
			if news[1].Generation != tc.change.Generation || news[1].Task != tc.change.Task || !reflect.DeepEqual(news[1].Reader, tc.change.Reader) {
				t.Errorf("the change's row = %+v, want the change's own work: %+v", news[1], tc.change)
			}
		})
	}
}
