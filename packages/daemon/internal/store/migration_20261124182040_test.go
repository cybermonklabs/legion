package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// podDisksVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const podDisksVersion = 20261124182040

// 20261124182040 adds issue_pod_disks, the disks of the pods of an issue's kept volume in which a
// role started, and changes no table that the release before reads. A role's start in a pod of a
// kept volume records that pod with its volume. The table's key is the issue and the pod, and it
// refuses a row with no pod, with no kept volume or with no mark, which no write makes. The
// negative control is the table itself, which is not there before the migration.
func TestTheDisksOfAnIssuesPodsArriveWith20261124182040(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, podDisksVersion-1)
	exec := func(statement string, args ...any) error {
		t.Helper()
		_, err := store.Pool().Exec(ctx, statement, args...)
		return err
	}
	if err := exec(`insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('INFRA-945', 'INFRA-945', 'INFRA', 'root', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record INFRA-945: %v", err)
	}
	const disks = `select issue || '/' || pod_uid || '/' || to_char(kept_since at time zone 'UTC', 'HH24:MI:SS') || '/' || holds_earlier_work
		from issue_pod_disks order by issue, pod_uid`
	if err := exec(disks); err == nil || !strings.Contains(err.Error(), "issue_pod_disks") {
		t.Fatalf("before the migration a read of the disks of the pods = %v, want the table to be missing", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the disks of the pods: %v", err)
	}
	if got := rowsAsText(t, store, disks); len(got) != 0 {
		t.Fatalf("after the migration the table holds %v, want it empty", got)
	}
	// A role's start in a pod of a kept volume records that pod with its volume.
	at := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	if err := store.StartKeptPod(ctx, "infra", "INFRA-945", "pod-of-a-kept-volume", at); err != nil {
		t.Fatalf("this release's start in a pod of a kept volume: %v", err)
	}
	if got, want := rowsAsText(t, store, disks), []string{"INFRA-945/pod-of-a-kept-volume/09:30:00/false"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after this release's start the table holds %v, want %v", got, want)
	}

	// One row per issue and pod: the same pod of another issue is a row of its own, the same pair
	// again is refused by the key, and a row with no pod, with no kept volume, or with no mark is
	// refused.
	const insert = `insert into issue_pod_disks (issue, pod_uid, kept_since, holds_earlier_work) values ($1, $2, $3, $4)`
	if err := exec(insert, "OTHER-8", "pod-of-a-kept-volume", at, true); err != nil {
		t.Fatalf("the same pod of another issue: %v", err)
	}
	for _, refused := range []struct {
		what  string
		issue string
		pod   string
		kept  *time.Time
		mark  *bool
	}{
		{"the same issue and pod again", "INFRA-945", "pod-of-a-kept-volume", &at, new(true)},
		{"a row with no pod", "INFRA-945", "", &at, new(true)},
		{"a row with no kept volume", "INFRA-945", "pod-of-a-later-pod", nil, new(true)},
		{"a row with no mark", "INFRA-945", "pod-of-a-later-pod", &at, nil},
	} {
		if err := exec(insert, refused.issue, refused.pod, refused.kept, refused.mark); err == nil {
			t.Fatalf("%s was written, want it refused", refused.what)
		}
	}
	if got, want := rowsAsText(t, store, disks), []string{"INFRA-945/pod-of-a-kept-volume/09:30:00/false",
		"OTHER-8/pod-of-a-kept-volume/09:30:00/true"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the table holds %v, want %v", got, want)
	}
}
