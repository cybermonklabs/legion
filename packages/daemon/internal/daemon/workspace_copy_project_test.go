package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A claim of the daemon's own project whose issue another project records — an operator's spawn
// of that issue, on a database the two projects' daemons share — is copied as every live claim of
// the project is (runningCopyClaim), and the store refuses both of the copy's writes
// (store.ErrIssueNotRecorded): the spawn's copy would replace the running copy of the project that
// records the issue, and its empty bundle would delete it. The cadence logs the refusal at Info
// under its own reason, with the issue, the role and the pod, writes no row and drops none, and
// owes no retry, since a retry would be refused the same way.
func TestACopyOfAnIssueTheProjectDoesNotRecordIsLoggedAndWritesNothing(t *testing.T) {
	empty := copyRigBundle
	empty.Bundle, empty.Head, empty.Tips = nil, "", nil
	for _, tc := range []struct {
		name string
		// held is whether the issue holds a running copy before the spawn's copy, written while the
		// daemon's project still recorded the issue; answer is what the spawn's bundler answers.
		held   bool
		answer runtime.WorkspaceBundle
	}{
		{name: "a copy of unpushed work writes no row", answer: copyRigBundle},
		{name: "a workspace that bundled to nothing drops no row", held: true, answer: empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCopyRig(t)
			ctx := context.Background()
			var before heldCopy
			if tc.held {
				rig.copyWorkspace(t)
				before, _ = rig.facts(t)
			}
			// Another project records the issue from here on, so the rig's claim stands for an
			// operator's spawn of it under the daemon's own project.
			if _, err := rig.pool.Exec(ctx, `update issues set project = 'OTHER' where key = $1`, rig.issue.Key); err != nil {
				t.Fatalf("record %s as OTHER's issue: %v", rig.issue.Key, err)
			}
			answer := tc.answer
			answer.At = time.Now().UTC()
			rig.bundler.answers(answer, nil)

			rig.w.takeRunningCopy(ctx, rig.issue.Key, nil)

			line := rig.awaitLine(t, runningCopyUnsavedLine)
			if line["reason"] != runningCopyNotRecorded || line["level"] != "INFO" || line["issue"] != rig.issue.Key ||
				line["role"] != string(claim.RoleImplementer) || line["pod"] != releasedPodUID || line["bytes"] != float64(answer.Size()) {
				t.Fatalf("the spawn's copy was logged as %v, want it at INFO under reason %s naming %s, the implementer, the pod %s and %d bytes",
					line, runningCopyNotRecorded, rig.issue.Key, releasedPodUID, answer.Size())
			}
			if after, held := rig.facts(t); held != tc.held || after.Size != before.Size || !after.At.Equal(before.At) {
				t.Fatalf("after the spawn's copy the issue holds %+v (held %t), want %+v (held %t) as it was", after, held, before, tc.held)
			}
			if owed := rig.w.copies.retries(); len(owed) != 0 {
				t.Fatalf("the refused copy left retries of %v owed, want none: a retry would be refused the same way", owed)
			}
		})
	}
}
