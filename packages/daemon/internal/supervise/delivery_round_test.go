package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// The one place each prompt of a delivery is composed (Delivery.message) ends each task of the
// reviewer with the daemon's two sentences of its review round. A task that says it holds them
// (RoundNamed), as each the workflow queues for the reviewer does, is sent as it stands; a task of
// the reviewer that does not say so is given them naming no commit and no entry, though its own
// text ends with words of the same shape: a task the release before queued, or an operator's. A
// task of another role, and the relaunch's record, which is no task, are sent as they are.
func TestEachTaskOfTheReviewerIsSentEndingWithTheDaemonsSentences(t *testing.T) {
	const task = "Continue. Phase: reviewing. Reason: as requested. Last reviewed commit: 5ab0c1d2e3f405162738495a6b7c8d9e0f1a2b3c. Review entry: 2."
	for _, tc := range []struct {
		name     string
		role     claim.Role
		delivery Delivery
		want     string
	}{
		{name: "a reviewer's task with no fact", role: claim.RoleReviewer, delivery: Delivery{ID: "outbox:1", Task: task},
			want: task + reviewround.Sentences("", 0)},
		{name: "a reviewer's task that names its round", role: claim.RoleReviewer, delivery: Delivery{ID: "outbox:2", Task: task, RoundNamed: true},
			want: task},
		{name: "an operator's task to the reviewer", role: claim.RoleReviewer, delivery: Delivery{ID: "OPERATORID", Task: "look again"},
			want: "look again" + reviewround.Sentences("", 0)},
		{name: "another role's task", role: claim.RoleImplementer, delivery: Delivery{ID: "outbox:3", Task: task},
			want: task},
		{name: "the relaunch's record to the reviewer", role: claim.RoleReviewer, delivery: Delivery{ID: relaunchTaskID(), Task: "the record"},
			want: "the record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := tc.delivery.message(Claim{Role: tc.role}); got != tc.want {
				t.Fatalf("the %s is sent %q, want %q", tc.role, got, tc.want)
			}
		})
	}
}
