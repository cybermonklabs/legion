package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A chain the copy cannot read to its top is read again from Dispatch, outside any decision,
// until it is known (R16 of dispatch://INFRA-3320, decision 20 of dispatch://INFRA-4730): a turn
// that leaves it unknown puts the key off by a backoff that doubles from a second, a 404 of a
// link is logged once however often it is read again, and the turn that reads the chain whole
// drops the key. Two causes are read again: a row the copy lacks, which Dispatch serves only
// later, and a stale row whose parent makes the chain 33 links long, which Dispatch serves moved.
// Every link is read each turn, the rows the copy holds included, since a stale row is a present
// row whose value is wrong.
func TestAnUnknownChainIsReadAgainUntilKnown(t *testing.T) {
	t.Parallel()
	t.Run("a missing row", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{}
		w, logs := rereadRuntime(t, pool, client)
		const key, parent = "CAPTURE-10", "CAPTURE-11"
		seedRereadIssue(t, pool, key)
		seedRereadRow(t, pool, key, parent, 1)
		client.serve(dispatch.Issue{Key: key, Project: "CAPTURE", Status: "in_progress", Parent: new(parent), LastSeq: 2})
		if rereadKnown(t, pool, key) {
			t.Fatal("the chain of an issue whose parent has no row in the copy reads known before any re-read")
		}

		start := time.Now()
		pending := map[string]*chainReread{key: {due: start, delay: chainRereadFirst, logged: map[string]bool{}}}
		for turn, at := range []time.Time{start, start.Add(chainRereadFirst)} {
			next := w.rereadDue(context.Background(), pending, at)
			held := pending[key]
			if held == nil || rereadKnown(t, pool, key) {
				t.Fatalf("turn %d: the chain is known or dropped while Dispatch serves no %s", turn, parent)
			}
			if want := at.Add(chainRereadFirst << turn); !next.Equal(want) || !held.due.Equal(want) {
				t.Fatalf("turn %d: next read at %s (held %s), want %s: the backoff doubles from a second", turn, next, held.due, want)
			}
		}
		if asked, want := client.asked(), []string{key, parent, key, parent}; !slices.Equal(asked, want) {
			t.Fatalf("Dispatch was asked %v, want %v: each turn reads the issue and then its missing link", asked, want)
		}
		if logged := strings.Count(logs.String(), "is no issue Dispatch serves"); logged != 1 {
			t.Fatalf("the 404 of %s was logged %d times over two turns, want once:\n%s", parent, logged, logs.String())
		}

		client.serve(dispatch.Issue{Key: parent, Project: "CAPTURE", Status: "in_progress", LastSeq: 2})
		if next := w.rereadDue(context.Background(), pending, start.Add(3*chainRereadFirst)); !next.IsZero() || len(pending) != 0 {
			t.Fatalf("after Dispatch serves %s, the re-read holds %v and is next due at %s, want nothing held", parent, pending, next)
		}
		if !rereadKnown(t, pool, key) {
			t.Fatalf("the chain of %s is still unknown after a re-read that read every link", key)
		}
	})

	t.Run("a stale row that makes a chain 33 links long", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		client := &tickDispatch{}
		w, _ := rereadRuntime(t, pool, client)
		// CAPTURE-200 stands under 201, 201 under 202, and so on to 232, whose stale row names
		// 233, the 33rd link. Dispatch has since moved 232 to the top, so the chain is 32 links.
		key := rereadKey(200)
		seedRereadIssue(t, pool, key)
		for i := 200; i <= 232; i++ {
			seedRereadRow(t, pool, rereadKey(i), rereadKey(i+1), 1)
			served := dispatch.Issue{Key: rereadKey(i), Project: "CAPTURE", Status: "in_progress", LastSeq: 2}
			if i < 232 {
				served.Parent = new(rereadKey(i + 1))
			}
			client.serve(served)
		}
		seedRereadRow(t, pool, rereadKey(233), "", 1)
		if rereadKnown(t, pool, key) {
			t.Fatal("a chain 33 links long reads known before any re-read")
		}

		pending := map[string]*chainReread{key: {due: time.Now(), delay: chainRereadFirst, logged: map[string]bool{}}}
		if next := w.rereadDue(context.Background(), pending, time.Now()); !next.IsZero() || len(pending) != 0 {
			t.Fatalf("after one re-read the re-read holds %v, next due %s, want the chain known and dropped", pending, next)
		}
		if !rereadKnown(t, pool, key) {
			t.Fatalf("the chain of %s is still unknown after the re-read of every link", key)
		}
		var want []string
		for i := 200; i <= 232; i++ {
			want = append(want, rereadKey(i))
		}
		if asked := client.asked(); !slices.Equal(asked, want) {
			t.Fatalf("Dispatch was asked %v, want every present row of the chain read once, %v", asked, want)
		}
	})
}

// A loop that two late moves made is a chain of present rows, each of which names the other as
// its parent: the copy holds every row, so only a read of the rows it holds can show it wrong. The
// re-read reads each one again, and Dispatch's answers, newer than the late moves' rows, end the
// loop; a late reading below what the re-read wrote, applied after it, changes nothing, which is
// INFRA-4729's per-value fence (record.PutDispatchIssue).
func TestALoopOfPresentRowsIsReadRightAgain(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	client := &tickDispatch{}
	w, _ := rereadRuntime(t, pool, client)
	const key, parent = "CAPTURE-300", "CAPTURE-301"
	seedRereadIssue(t, pool, key)
	seedRereadRow(t, pool, key, parent, 2)
	seedRereadRow(t, pool, parent, key, 2)
	if rereadKnown(t, pool, key) {
		t.Fatal("a loop of two present rows reads as a known chain")
	}
	client.serve(dispatch.Issue{Key: key, Project: "CAPTURE", Status: "in_progress", Parent: new(parent), LastSeq: 3})
	client.serve(dispatch.Issue{Key: parent, Project: "CAPTURE", Status: "in_progress", LastSeq: 3})

	pending := map[string]*chainReread{key: {due: time.Now(), delay: chainRereadFirst, logged: map[string]bool{}}}
	if next := w.rereadDue(context.Background(), pending, time.Now()); !next.IsZero() || len(pending) != 0 {
		t.Fatalf("after one re-read the re-read holds %v, next due %s, want the loop read right and dropped", pending, next)
	}
	if asked, want := client.asked(), []string{key, parent}; !slices.Equal(asked, want) {
		t.Fatalf("Dispatch was asked %v, want both present rows read, %v", asked, want)
	}
	if !rereadKnown(t, pool, key) {
		t.Fatal("the chain is still unknown after the re-read of both rows of the loop")
	}

	// The second late move arrives after the re-read, at the sequence it was made: below the
	// re-read's reading, so it writes nothing.
	seedRereadRow(t, pool, parent, key, 2)
	if !rereadKnown(t, pool, key) {
		t.Fatal("a late reading below the re-read's made the loop again")
	}
}

// A re-read stops at a parent of another project, as the chain does (R19 of
// dispatch://INFRA-3320): CAPTURE-400's chain is unknown for the row of CAPTURE-401 the copy
// lacks, and Dispatch serves CAPTURE-401 under OTHER-5, an issue at work in project OTHER. The
// re-read asks Dispatch for the issue and that link alone, writes no row of the other project into
// the copy, and leaves the chain known and the key dropped.
func TestAReReadStopsAtAParentInAnotherProject(t *testing.T) {
	t.Parallel()
	pool := isolatedOutboxPool(t)
	client := &tickDispatch{}
	w, _ := rereadRuntime(t, pool, client)
	const key, parent, foreign = "CAPTURE-400", "CAPTURE-401", "OTHER-5"
	seedRereadIssue(t, pool, key)
	seedRereadRow(t, pool, key, parent, 1)
	client.serve(dispatch.Issue{Key: key, Project: "CAPTURE", Status: "in_progress", Parent: new(parent), LastSeq: 2})
	client.serve(dispatch.Issue{Key: parent, Project: "CAPTURE", Status: "in_progress", Parent: new(foreign), LastSeq: 2})
	client.serve(dispatch.Issue{Key: foreign, Project: "OTHER", Status: "in_progress", LastSeq: 2})
	if rereadKnown(t, pool, key) {
		t.Fatal("the chain of an issue whose parent has no row in the copy reads known before any re-read")
	}

	pending := map[string]*chainReread{key: {due: time.Now(), delay: chainRereadFirst, logged: map[string]bool{}}}
	if next := w.rereadDue(context.Background(), pending, time.Now()); !next.IsZero() || len(pending) != 0 {
		t.Fatalf("after one re-read the re-read holds %v, next due %s, want the chain known and dropped", pending, next)
	}
	if asked, want := client.asked(), []string{key, parent}; !slices.Equal(asked, want) {
		t.Fatalf("Dispatch was asked %v, want %v: %s is of another project, where the chain ends", asked, want, foreign)
	}
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from dispatch_issues where key = $1`, foreign).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the copy holds %d rows of %s (%v), want none", rows, foreign, err)
	}
	if !rereadKnown(t, pool, key) {
		t.Fatalf("the chain of %s is still unknown after the re-read", key)
	}
}

// rereadRuntime is tickRuntime with a logger the test reads.
func rereadRuntime(t *testing.T, pool *store.Pool, client dispatch.Client) (*workflowRuntime, *syncBuffer) {
	t.Helper()
	w := tickRuntime(t, pool, client)
	logs := &syncBuffer{}
	w.log = slog.New(slog.NewTextHandler(logs, nil))
	return w, logs
}

func rereadKey(n int) string { return fmt.Sprintf("CAPTURE-%d", n) }

// seedRereadIssue records key as an issue at work, whose chain decides it.
func seedRereadIssue(t *testing.T, pool *store.Pool, key string) {
	t.Helper()
	seedTickIssue(t, pool, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "CAPTURE",
		Title: key, Tree: key, Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", LastDispatchSeq: 1,
	})
}

// seedRereadRow writes the copy's row of key at seq, under parent ("" for none).
func seedRereadRow(t *testing.T, pool *store.Pool, key, parent string, seq int64) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: key, Project: "CAPTURE", Seq: seq,
			Status: record.Carry("in_progress"), Parent: record.Carry(parent)})
	}); err != nil {
		t.Fatalf("seed the copy's row of %s: %v", key, err)
	}
}

// rereadKnown is whether the copy holds key's chain known, as every decision reads it.
func rereadKnown(t *testing.T, pool *store.Pool, key string) bool {
	t.Helper()
	var known bool
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		chains, err := record.ReadChainsOf(context.Background(), tx, record.NewStore(), "CAPTURE", []string{key})
		known = err == nil && chains.Chain(key).Known()
		return err
	}); err != nil {
		t.Fatalf("read the chain of %s: %v", key, err)
	}
	return known
}
