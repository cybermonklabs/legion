package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// listingDispatch lists summaries for the project.
type listingDispatch struct {
	outboxDispatch
	summaries []dispatch.IssueSummary
}

func (d *listingDispatch) ListIssues(context.Context, string, []string) ([]dispatch.IssueSummary, error) {
	return d.summaries, nil
}

// A record written before the record carried the issue's repository is filled at boot from the
// repository Dispatch resolves the issue to and that repository's default branch, GitHub read once
// per repository. A record Dispatch resolves to none is left as it is, and the issue and the cause
// are logged, while every other record is filled: one record's repository never stops the boot
// (INFRA-4401 D11), and the backfill answers that a record is left, for the refresher to try it
// again.
func TestTheBootFillsRecordsWithNoRepository(t *testing.T) {
	seed := func(t *testing.T) *workflowRuntime {
		t.Helper()
		pool := isolatedOutboxPool(t)
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			for _, key := range []string{"LEGION-7", "LEGION-9", "LEGION-12"} {
				if _, err := tx.Exec(context.Background(), `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
					values ($1, $1, 'LEGION', $1, 'planning', 1, 'in_progress', 'A', 1)`, key); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		github := newRepositoryGitHub(t, "release/v7.1")
		github.fullName = "Acme/Widgets"
		return &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"},
			defaultBranches: newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)}
	}

	t.Run("Dispatch resolves none for some", func(t *testing.T) {
		w := seed(t)
		client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-7"}, {Key: "LEGION-9", ResolvedRepo: "acme/widgets"}}}
		var logs bytes.Buffer
		filled, left, err := backfillRepositories(context.Background(), w.pool, w.records, client, "LEGION", t.TempDir(), w.defaultBranches, ghrepo.Renames{}, slog.New(slog.NewTextHandler(&logs, nil)))
		if err != nil || !filled || !left {
			t.Fatalf("backfill = filled %t, left %t, %v; want LEGION-9 filled, the two Dispatch resolves none for left, and no error", filled, left, err)
		}
		if unfilled := unfilledKeys(t, w); !slices.Equal(slices.Sorted(slices.Values(unfilled)), []string{"LEGION-12", "LEGION-7"}) {
			t.Fatalf("unfilled after the backfill = %v, want LEGION-12 and LEGION-7 left as they are", unfilled)
		}
		for _, key := range []string{"LEGION-7", "LEGION-12"} {
			if !strings.Contains(logs.String(), "issue="+key+" repo=\"\" cause=\"Dispatch resolves no repository for it: set its repository in Dispatch\"") {
				t.Fatalf("backfill log = %s, want %s named as left with its cause", logs.String(), key)
			}
		}
	})

	t.Run("Dispatch resolves every one", func(t *testing.T) {
		w := seed(t)
		github := newRepositoryGitHub(t, "release/v7.1")
		github.fullName = "Acme/Widgets"
		w.defaultBranches = newDefaultBranches(roleTokens{}, nil, github.url, defaultBranchTTL, time.Now)
		client := &listingDispatch{summaries: []dispatch.IssueSummary{
			{Key: "LEGION-7", ResolvedRepo: "acme/widgets"}, {Key: "LEGION-9", ResolvedRepo: "acme/widgets"}, {Key: "LEGION-12", ResolvedRepo: "acme/widgets"},
		}}
		var logs bytes.Buffer
		if _, _, err := backfillRepositories(context.Background(), w.pool, w.records, client, "LEGION", t.TempDir(), w.defaultBranches, ghrepo.Renames{}, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
			t.Fatalf("backfill: %v", err)
		}
		if unfilled := unfilledKeys(t, w); len(unfilled) != 0 {
			t.Fatalf("unfilled after the backfill = %v, want none", unfilled)
		}
		if reads := github.reads.Load(); reads != 1 {
			t.Fatalf("GitHub was read %d times for one repository, want once", reads)
		}
		if !strings.Contains(logs.String(), "legion workflow backfill filled LEGION-12 with Acme/Widgets@release/v7.1") {
			t.Fatalf("backfill log = %s, want each record named", logs.String())
		}
	})
}

func unfilledKeys(t *testing.T, w *workflowRuntime) []string {
	t.Helper()
	var keys []string
	if err := pgx.BeginFunc(context.Background(), w.pool, func(tx pgx.Tx) error {
		unfilled, err := w.records.UnfilledIssues(context.Background(), tx, "LEGION")
		for _, issue := range unfilled {
			keys = append(keys, issue.Key)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return keys
}
