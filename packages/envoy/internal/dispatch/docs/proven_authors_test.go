package docs

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A document version's author is proven only when every write merged into it was. The author map
// is keyed by kind and id, so a session that claimed one write and proved another is one author,
// and that author is not proven, whichever write came first, whether the first sat in the
// process's memory or was read back from the pending authors by a process started after it. A
// session that proved both writes stays proven across the same restart, so the mark is kept where
// it is earned and not merely dropped.
func TestAnAuthorIsProvenOnlyWhenEveryWriteMergedIntoItWas(t *testing.T) {
	t.Parallel()
	claimed := model.Actor{Kind: "session", ID: "judge-session"}
	proven := model.Actor{Kind: "session", ID: "judge-session", Proven: true}
	for _, tc := range []struct {
		name          string
		first, second model.Actor
		restart       bool
		want          model.Actor
	}{
		{"a claimed write, then a proven one", claimed, proven, false, claimed},
		{"a proven write, then a claimed one", proven, claimed, false, claimed},
		{"a claimed write, a restart, then a proven one", claimed, proven, true, claimed},
		{"a proven write, a restart, then a claimed one", proven, claimed, true, claimed},
		{"two proven writes across a restart", proven, proven, true, proven},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour // no settlement lists the first write before the second
			seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
			settleCurrentGeneration(t, service, artifactID)
			writeThroughLedger(t, service, artifactID, tc.first, "The first write.\n", withoutVersion)
			if owed := pendingAuthorRows(t, service.store, artifactID); !slices.Equal(owed, []model.Actor{tc.first}) {
				t.Fatalf("pending authors after the first write = %+v, want %+v as written", owed, tc.first)
			}
			versioner := service
			if tc.restart {
				versioner, _ = newTestServiceInstance(t, service.store)
			}
			writeThroughLedger(t, versioner, artifactID, tc.second, "The second write.\n", withNamedVersion)
			if authors := latestVersionAuthors(t, versioner, artifactID); !reflect.DeepEqual(authors, []model.Actor{tc.want}) {
				t.Fatalf("the version lists %+v, want %+v alone", authors, tc.want)
			}
		})
	}
}

// The pending authors keep an author proven only when every row of it is: rows of one author at
// two updates read back as one author, proven only when both are, in either order; and a row
// written again at the same update keeps the mark only when both writes carried it, so a claimed
// write is never lifted to proven by the proven one that rewrites its row, nor the other way.
func TestThePendingAuthorsKeepAnAuthorProvenOnlyWhenEveryRowIs(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	claimed := model.Actor{Kind: "session", ID: "judge-session"}
	proven := model.Actor{Kind: "session", ID: "judge-session", Proven: true}
	ctx := context.Background()
	write := func(authors ...pendingAuthor) {
		t.Helper()
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		if err := upsertPendingAuthors(ctx, tx, artifactID, authors); err != nil {
			t.Fatalf("upsert %+v: %v", authors, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	clearRows := func() {
		t.Helper()
		if _, err := service.store.Pool.Exec(ctx, `delete from doc_pending_authors where artifact_id = $1`, artifactID); err != nil {
			t.Fatalf("clear the pending authors: %v", err)
		}
	}

	for name, rows := range map[string][]pendingAuthor{
		"a proven row, then a claimed one": {{actor: proven, writtenThrough: 4}, {actor: claimed, writtenThrough: 5}},
		"a claimed row, then a proven one": {{actor: claimed, writtenThrough: 4}, {actor: proven, writtenThrough: 5}},
	} {
		clearRows()
		write(rows...)
		owed, err := readPendingAuthors(ctx, service.store.Pool, artifactID)
		if err != nil {
			t.Fatalf("%s: read the pending authors: %v", name, err)
		}
		if want := map[string]model.Actor{actorKey(claimed): claimed}; !reflect.DeepEqual(owed, want) {
			t.Fatalf("%s: the pending authors read %+v, want %+v", name, owed, want)
		}
	}

	clearRows()
	for _, step := range []struct {
		name   string
		writes model.Actor
		stored model.Actor
	}{
		{"a proven row", proven, proven},
		{"the same row proven again", proven, proven},
		{"the same row claimed", claimed, claimed},
		{"the same row proven once more", proven, claimed},
	} {
		write(pendingAuthor{actor: step.writes, writtenThrough: 6})
		if owed := pendingAuthorRows(t, service.store, artifactID); !slices.Equal(owed, []model.Actor{step.stored}) {
			t.Fatalf("after %s the row holds %+v, want %+v", step.name, owed, step.stored)
		}
	}
}
