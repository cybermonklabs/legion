package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// workspaceBundleWorkingCopiesVersion is the version of the migration this file tests.
const workspaceBundleWorkingCopiesVersion = 20261117204629

// 20261117204629 adds workspace_bundles.working_copies. A row the release before saved takes the
// default, the empty object, and its take answers the bundle with no working copy of a task, so
// its restore is the issue's working copy alone. The way back the migration's header gives drops
// the column and the version's row, and Migrate then adds the column again: the control is the
// read after the way back, which fails while the column is gone.
func TestMigrationWorkspaceBundleWorkingCopiesKeepsARowOfTheReleaseBefore(t *testing.T) {
	ctx := context.Background()
	st := emptyStore(t)
	migrateThrough(t, st, workspaceBundleWorkingCopiesVersion-1)
	at := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	if _, err := st.Pool().Exec(ctx, `insert into workspace_bundles (issue, release_id, source_pod_uid, bundle, size_bytes, at, head, tips, bookmarks)
		values ('INFRA-945', 11, 'pod-of-the-release-before', 'PACK', 4, $1, 'f2f134541b30dfa5160a5d1ce5e17b76c967c661', '[]', '{}')`, at); err != nil {
		t.Fatalf("save a bundle as the release before saves one: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('INFRA-945', 'INFRA-945', 'INFRA', 'root', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record INFRA-945: %v", err)
	}
	read, take, err := st.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-that-replaces-it", at.Add(time.Minute))
	if err != nil || take != runtime.WorkspaceBundleSend {
		t.Fatalf("the take answered %v (%v), want the bundle sent", take, err)
	}
	if read.Head != "f2f134541b30dfa5160a5d1ce5e17b76c967c661" || len(read.WorkingCopies) != 0 {
		t.Fatalf("the bundle of the release before reads head %s and working copies %v, want its head and none", read.Head, read.WorkingCopies)
	}

	if _, err := st.Pool().Exec(ctx, `alter table workspace_bundles drop column if exists working_copies;
		delete from schema_version where version = 20261117204629`); err != nil {
		t.Fatalf("run the migration's way back: %v", err)
	}
	columns := func() int {
		t.Helper()
		var n int
		if err := st.Pool().QueryRow(ctx, `select count(*) from information_schema.columns
			where table_name = 'workspace_bundles' and column_name = 'working_copies'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := columns(); n != 0 {
		t.Fatalf("after the way back workspace_bundles has %d working_copies column(s), want none", n)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate again after the way back: %v", err)
	}
	if n := columns(); n != 1 {
		t.Fatalf("the roll-forward after the way back left %d working_copies column(s), want one", n)
	}
}
