package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// renamePassSummary is the summary line the boot's rename pass logs at every boot.
const renamePassSummary = `msg="dispatch: rewrote renamed repositories' old names"`

// seedRenamedIssue writes an issue whose own repository is sjawhar/legion, and the mapping row
// that renames sjawhar/legion to cybermonklabs/legion.
func seedRenamedIssue(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('INFRA', 'Infra', 'alice');
		insert into repo_projects (repo, project, created_by, renamed_to)
			values ('sjawhar/legion', 'INFRA', '{"kind":"user","id":"alice"}', 'cybermonklabs/legion');
		insert into issues (key, project_key, number, title, created_by, rank, repo)
			values ('INFRA-1', 'INFRA', 1, 'One', '{"kind":"user","id":"alice"}', 'U1', 'sjawhar/legion');
	`); err != nil {
		t.Fatalf("seed the renamed issue: %v", err)
	}
}

// issueRepo reads the own repository of INFRA-1.
func issueRepo(t *testing.T, ctx context.Context, database *store.Store) string {
	t.Helper()
	var repo string
	if err := database.Pool.QueryRow(ctx, `select repo from issues where key = 'INFRA-1'`).Scan(&repo); err != nil {
		t.Fatalf("read INFRA-1's repository: %v", err)
	}
	return repo
}

// The boot reads the pairs and runs the pass over them: the pass writes the new name and logs
// its summary at INFO, with the pair and the row it wrote.
func TestTheBootRenamePassWritesTheNewNameAndLogsItsSummary(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	seedRenamedIssue(t, ctx, database)
	pairs, err := store.ReadRepoRenames(ctx, database.Pool)
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	renameRepositories(ctx, database.Pool, pairs, err, repoRenamePassTimeout)

	if out := logs.String(); !strings.Contains(out, `level=INFO `+renamePassSummary+` pairs=1 rows=1 deleted=0 failures=0 left=0`) {
		t.Errorf("logged, want the summary at INFO with pairs=1 rows=1 deleted=0 failures=0 left=0:\n%s", out)
	}
	if repo := issueRepo(t, ctx, database); repo != "cybermonklabs/legion" {
		t.Errorf("INFRA-1's repository = %s, want cybermonklabs/legion", repo)
	}
}

// A read of the pairs that failed is one failure of the pass. The pass then has no pairs and
// writes nothing, the rows left on an old name are unknown, and the summary is at ERROR.
func TestTheBootRenamePassCountsAFailedReadOfThePairsAsAFailure(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	seedRenamedIssue(t, ctx, database)
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	renameRepositories(ctx, database.Pool, nil, errors.New("read the repository rename pairs: timeout"), repoRenamePassTimeout)

	if out := logs.String(); !strings.Contains(out, `level=ERROR `+renamePassSummary+` pairs=0 rows=0 deleted=0 failures=1 left=unknown`) {
		t.Errorf("logged, want the summary at ERROR with pairs=0 rows=0 deleted=0 failures=1 left=unknown:\n%s", out)
	}
	if repo := issueRepo(t, ctx, database); repo != "sjawhar/legion" {
		t.Errorf("INFRA-1's repository = %s, want sjawhar/legion as it was", repo)
	}
}

// A count of the rows left that fails is one failure of the pass, and the summary says the rows
// left are unknown, at ERROR, while the lines that could write wrote.
func TestTheBootRenamePassLogsLeftUnknownWhenItsCountFails(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	seedRenamedIssue(t, ctx, database)
	// The count reads every table of the pass, so a dropped one fails it, as it fails that
	// table's own line.
	if _, err := database.Pool.Exec(ctx, `drop table ci_reconcile_passes`); err != nil {
		t.Fatalf("drop ci_reconcile_passes: %v", err)
	}
	pairs, err := store.ReadRepoRenames(ctx, database.Pool)
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	renameRepositories(ctx, database.Pool, pairs, err, repoRenamePassTimeout)

	if out := logs.String(); !strings.Contains(out, `level=ERROR `+renamePassSummary+` pairs=1 rows=1 deleted=0 failures=2 left=unknown`) {
		t.Errorf("logged, want the summary at ERROR with pairs=1 rows=1 deleted=0 failures=2 left=unknown:\n%s", out)
	}
	if repo := issueRepo(t, ctx, database); repo != "cybermonklabs/legion" {
		t.Errorf("INFRA-1's repository = %s, want cybermonklabs/legion", repo)
	}
}

// dispatch://INFRA-4411 drops repo_projects. The boot then reads no pairs and no error, and the
// pass writes nothing and logs its summary at INFO with pairs=0.
func TestTheBootRenamePassLogsNoPairsOnceRepoProjectsIsDropped(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	seedRenamedIssue(t, ctx, database)
	if _, err := database.Pool.Exec(ctx, `drop table repo_projects`); err != nil {
		t.Fatalf("drop repo_projects: %v", err)
	}
	pairs, err := store.ReadRepoRenames(ctx, database.Pool)
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	renameRepositories(ctx, database.Pool, pairs, err, repoRenamePassTimeout)

	if out := logs.String(); !strings.Contains(out, `level=INFO `+renamePassSummary+` pairs=0 rows=0 deleted=0 failures=0 left=0`) {
		t.Errorf("logged, want the summary at INFO with pairs=0 rows=0 deleted=0 failures=0 left=0:\n%s", out)
	}
	if repo := issueRepo(t, ctx, database); repo != "sjawhar/legion" {
		t.Errorf("INFRA-1's repository = %s, want sjawhar/legion as it was", repo)
	}
}
