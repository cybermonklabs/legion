package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A pod no role process runs in, kept for its disk, is told to the issue's architect once for each
// pod (R6 of #5285; Sol's point 3 and the Legion reviewer's points 3 and 14 on it).
// Whether the architect was told of a pod is a fact of the decline row of its own, apart from the
// work a read found and the role the read went through, so each row below is a sequence of
// declines of one issue's release, read by the pod's launcher (Kept) or through a role, and
// whether each queued its message:
//
//   - another_pod_with_the_same_work: a new pod whose disk holds the same work (a kept volume, say)
//     is a pod the architect was not told of;
//   - a_failed_architect_read_then_the_launcher: a read through the architect's own shim that
//     failed told nobody, so the launcher's decline of that pod tells the architect;
//   - a_role_read_then_the_launcher: a role told of its work through its own read is not the
//     architect told of the pod; once the architect is told, a later read through a role that
//     comes back and the launcher's read after it tell the architect nothing more;
//   - repeated_unread_retries (control): a launcher that cannot read, asked again and again, tells
//     the architect once;
//   - a_pod_the_runtime_could_not_name_then_its_uid: a decline whose pod the runtime could not
//     name, then the same pod by its uid, tell once; another named pod tells again.
func TestTheArchitectIsToldOnceForEachKeptPod(t *testing.T) {
	t.Parallel()
	type read struct {
		pod    string
		role   claim.Role
		kept   bool
		unread bool
		told   bool
	}
	launcher := func(pod string, told bool) read {
		return read{pod: pod, role: claim.RoleArchitect, kept: true, told: told}
	}
	launcherUnread := func(pod string, told bool) read {
		return read{pod: pod, role: claim.RoleArchitect, kept: true, unread: true, told: told}
	}
	for _, row := range []struct {
		name  string
		reads []read
	}{
		{"another_pod_with_the_same_work", []read{launcher("pod-a", true), launcher("pod-b", true), launcher("pod-b", false)}},
		{"a_failed_architect_read_then_the_launcher", []read{
			{pod: "pod-a", role: claim.RoleArchitect, unread: true}, launcherUnread("pod-a", true), launcher("pod-a", false)}},
		{"a_role_read_then_the_launcher", []read{
			{pod: "pod-a", role: claim.RoleImplementer, told: true}, launcher("pod-a", true),
			{pod: "pod-a", role: claim.RoleImplementer, told: true}, launcher("pod-a", false)}},
		{"repeated_unread_retries", []read{
			launcherUnread("pod-a", true), launcherUnread("pod-a", false), launcherUnread("pod-a", false), launcher("pod-a", false)}},
		{"a_pod_the_runtime_could_not_name_then_its_uid", []read{
			launcherUnread("", true), launcherUnread("pod-a", false), launcher("pod-a", false), launcherUnread("pod-b", true)}},
	} {
		t.Run(row.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			at := time.Now().UTC().Truncate(time.Millisecond)
			told := 0
			for i, r := range row.reads {
				at = at.Add(time.Minute)
				decline := IdleReleaseDecline{At: at, Role: r.role, Reason: pods.DeclineUnpushedWork, Head: "abc", Commits: 1,
					PodUID: r.pod, Kept: r.kept}
				if r.unread {
					decline.Reason, decline.Head, decline.Commits = pods.DeclineWorkspaceUnread, "", 0
				}
				if r.kept || !r.unread {
					// Every notice row names the run and the phase entry it is queued for
					// (record.Fence); the issue's first is the one this test's issue stands at.
					message, err := record.NewOutboxRow(rootIssue, record.Fenced(record.Notice{Kind: record.WorkspaceKeptNotice, Role: claim.RoleArchitect,
						Reason: "the pod is kept"}, record.FirstFence), at)
					if err != nil {
						t.Fatal(err)
					}
					decline.Message = &IdleReleaseRow{Kind: string(message.Kind), Payload: message.Payload}
				}
				got, err := st.DeclineLeftPodClose(ctx, "legion", rootIssue, decline)
				if err != nil {
					t.Fatalf("decline %d (%+v): %v", i+1, r, err)
				}
				if got != r.told {
					t.Fatalf("decline %d (%+v) queued its message = %t, want %t", i+1, r, got, r.told)
				}
				if got {
					told++
				}
			}
			var queued int
			if err := st.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'notice' and payload->>'kind' = 'workspace-kept'`,
				rootIssue).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if queued != told {
				t.Fatalf("outbox rows of %s = %d, want the %d messages the declines said they queued", rootIssue, queued, told)
			}
		})
	}
}
