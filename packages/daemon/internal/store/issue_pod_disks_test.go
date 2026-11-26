package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// keptPodProject is the token of the project whose pods start in these tests, and keptPodStore is
// a migrated store whose issues table records INFRA-945 under that project: a pod is a project's
// to record only where the project records its issue (StartKeptPod, projectRecordsIssue).
const keptPodProject = "infra"

func keptPodStore(t *testing.T) *Store {
	t.Helper()
	s := migratedStore(t)
	if _, err := s.Pool().Exec(context.Background(), `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('INFRA-945', 'INFRA-945', 'INFRA', 'root', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record INFRA-945: %v", err)
	}
	return s
}

// podDisks is the disk of every pod of issue that the store records (issue_pod_disks), in the
// order of the pod uids, each as "<pod uid>/<kept volume>/<mark>": the kept volume as when its
// Sandbox was made, to the second (keptVolume), and the mark "marked" where the disk holds work
// from before the pod, else "unmarked". It is what the update round reads one pod at a time
// (PodHoldsEarlierWork), read here whole.
func podDisks(t *testing.T, s *Store, issue string) []string {
	t.Helper()
	return rowsAsText(t, s, `select pod_uid || '/'
		|| to_char(kept_since at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"') || '/'
		|| case when holds_earlier_work then 'marked' else 'unmarked' end
		from issue_pod_disks where issue = '`+issue+`' order by pod_uid`)
}

// keptVolume is the kept volume of a Sandbox made at made, as podDisks renders it.
func keptVolume(made time.Time) string { return made.UTC().Format(time.RFC3339) }

// wantPodDisks holds the disks of the pods of issue to want, read whole (podDisks), and holds the
// read the update round makes of each pod (PodHoldsEarlierWork) to agree with it: true for each
// pod of want that is marked, false for each that is not and for each pod of other, which the
// store does not record.
func wantPodDisks(t *testing.T, s *Store, issue, when string, want []string, other ...string) {
	t.Helper()
	if got := podDisks(t, s, issue); !slices.Equal(got, want) {
		t.Fatalf("%s the disks of the pods of %s are %v, want %v", when, issue, got, want)
	}
	read := func(pod string, marked bool) {
		t.Helper()
		if holds, err := s.PodHoldsEarlierWork(context.Background(), issue, pod); err != nil || holds != marked {
			t.Fatalf("%s the read of whether the disk of %s of %s holds work from before it answered %t (%v), want %t", when, pod, issue, holds, err, marked)
		}
	}
	for _, row := range want {
		pod, _, _ := strings.Cut(row, "/")
		read(pod, strings.HasSuffix(row, "/marked"))
	}
	for _, pod := range other {
		read(pod, false)
	}
}

// Each start of a role in a pod whose Sandbox owns a kept volume records that pod with its volume
// (StartKeptPod), and the record marks the pod as holding work from before it where an earlier pod
// of the issue mounted the same volume: a kept volume outlives its pod, so what the roles left
// unpushed in that earlier pod is on the later pod's disk. A later pod of a volume never marks an
// earlier one, so the first pod of a volume is never marked: pod-a stays unmarked whatever pod of
// its volume comes after it. A pod of another volume is not marked by it, and neither is a pod of
// another issue of the project whose volume was made in the same second. No later write clears a
// mark: not a later start in a marked pod.
func TestEachPodOfAKeptVolumeIsMarkedWhereAnEarlierPodOfTheIssueMountedIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := keptPodStore(t)
	if _, err := s.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('INFRA-946', 'INFRA-946', 'INFRA', 'root', 'implementing', 1, 'in_progress', 'B', 1)`); err != nil {
		t.Fatalf("record INFRA-946 as INFRA's issue: %v", err)
	}
	first := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	second := first.Add(3 * time.Hour)
	starts := func(issue, pod string, kept time.Time) {
		t.Helper()
		if err := s.StartKeptPod(ctx, keptPodProject, issue, pod, kept); err != nil {
			t.Fatalf("a role's start in %s of %s, on the kept volume made %s: %v", pod, issue, kept, err)
		}
	}
	// The first pod of a volume is unmarked, at its first start and at the next.
	starts("INFRA-945", "pod-a", first)
	starts("INFRA-945", "pod-a", first)
	wantPodDisks(t, s, "INFRA-945", "after two starts in the first pod of a volume,", []string{"pod-a/" + keptVolume(first) + "/unmarked"})
	// The next pod of that volume holds what pod-a's roles left unpushed; pod-a still holds
	// nothing from before it. A pod of a new volume, after a reset or a change of the disk's
	// shape, holds nothing of the first volume. A pod of another issue of the project is no pod of
	// this issue's volume, whenever its own was made.
	starts("INFRA-945", "pod-b", first)
	starts("INFRA-945", "pod-c", second)
	starts("INFRA-946", "pod-x", first)
	wantPodDisks(t, s, "INFRA-945", "after the starts in the next pods,", []string{
		"pod-a/" + keptVolume(first) + "/unmarked",
		"pod-b/" + keptVolume(first) + "/marked",
		"pod-c/" + keptVolume(second) + "/unmarked",
	})
	wantPodDisks(t, s, "INFRA-946", "after a start in a pod of another issue,", []string{"pod-x/" + keptVolume(first) + "/unmarked"})
	// A later start in the marked pod keeps its mark and its volume.
	starts("INFRA-945", "pod-b", first)
	wantPodDisks(t, s, "INFRA-945", "after a later start in the marked pod,", []string{
		"pod-a/" + keptVolume(first) + "/unmarked",
		"pod-b/" + keptVolume(first) + "/marked",
		"pod-c/" + keptVolume(second) + "/unmarked",
	})
}

// A role's start in a pod of a kept volume records the pod only for an issue the asking project
// records: legion's pod of INFRA-945, an INFRA record, writes nothing, and nor does INFRA's pod of
// an issue another project records or of one no project records, each of which is another daemon's
// or none's. A start that names no project, no pod, or no creation of the pod's Sandbox has
// nothing to write the record by and is refused, writing nothing: a zero creation would name no
// volume.
func TestAKeptPodIsRecordedOnlyForAnIssueItsProjectRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := keptPodStore(t)
	if _, err := s.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('OTHER-8', 'OTHER-8', 'OTHER', 'root', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record OTHER-8 as OTHER's issue: %v", err)
	}
	kept := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	if err := s.StartKeptPod(ctx, "legion", "INFRA-945", "pod-of-legions-spawn", kept); err != nil {
		t.Fatalf("legion's start in a pod of INFRA-945: %v", err)
	}
	for _, issue := range []string{"OTHER-8", "NOBODY-3"} {
		if err := s.StartKeptPod(ctx, keptPodProject, issue, "pod-of-infras-spawn", kept); err != nil {
			t.Fatalf("INFRA's start in a pod of %s: %v", issue, err)
		}
		wantPodDisks(t, s, issue, "after INFRA's start,", nil, "pod-of-infras-spawn")
	}
	for _, refused := range []struct {
		what, project, pod string
		kept               time.Time
	}{
		{"a start that named no project", "", "pod-after-the-release", kept},
		{"a start that named no pod", keptPodProject, "", kept},
		{"a start that named no creation of the Sandbox", keptPodProject, "pod-after-the-release", time.Time{}},
	} {
		if err := s.StartKeptPod(ctx, refused.project, "INFRA-945", refused.pod, refused.kept); err == nil {
			t.Fatalf("%s was answered nil, want it refused", refused.what)
		}
	}
	wantPodDisks(t, s, "INFRA-945", "after the starts that are not INFRA's and the refused starts,", nil,
		"pod-of-legions-spawn", "pod-after-the-release")

	if err := s.StartKeptPod(ctx, keptPodProject, "INFRA-945", "pod-after-the-release", kept); err != nil {
		t.Fatalf("INFRA's start in a pod of INFRA-945: %v", err)
	}
	wantPodDisks(t, s, "INFRA-945", "after INFRA's start,", []string{"pod-after-the-release/" + keptVolume(kept) + "/unmarked"}, "pod-of-legions-spawn")
}

// The record of the disks of an issue's pods stands until the issue's work is over. The issue's
// close deletes the record of every pod of that issue, marked or not, and of that issue alone
// (DeleteIssuePodDisks, which the outbox's forgetIdleRelease runs), and a delete that finds nothing
// finishes.
func TestTheDisksOfAnIssuesPodsStayRecordedUntilTheIssuesWorkIsOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := keptPodStore(t)
	seedRunningRoot(t, s, 1)
	kept := time.Date(2026, 11, 23, 9, 30, 0, 0, time.UTC)
	starts := func(project, issue, pod string) {
		t.Helper()
		if err := s.StartKeptPod(ctx, project, issue, pod, kept); err != nil {
			t.Fatalf("a role's start in %s of %s: %v", pod, issue, err)
		}
	}
	starts(keptPodProject, "INFRA-945", "pod-a")
	starts(keptPodProject, "INFRA-945", "pod-b")
	starts("legion", rootIssue, "pod-a")
	starts("legion", rootIssue, "pod-b")
	both := []string{"pod-a/" + keptVolume(kept) + "/unmarked", "pod-b/" + keptVolume(kept) + "/marked"}
	wantPodDisks(t, s, "INFRA-945", "after the starts,", both)

	for range 2 {
		if err := s.DeleteIssuePodDisks(ctx, "INFRA-945"); err != nil {
			t.Fatalf("delete the record of the disks of the pods of INFRA-945: %v", err)
		}
		wantPodDisks(t, s, "INFRA-945", "after the issue's work is over,", nil, "pod-a", "pod-b")
		wantPodDisks(t, s, rootIssue, "after the work of another issue is over,", both)
	}
}
