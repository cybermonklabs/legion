package daemon

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// dispatch://INFRA-3583 R6: the release asks each running role of a pod with the text built for
// that role. Both roles run in one pod with unpushed work, so the release asks both of them in one
// ask, and each role is asked once, with the text supervise.ReleaseTask builds for that role's
// own claim, the release's round, the issue and the release's bound. The reviewer creates no
// commit and the implementer does (roles/reviewer.md), so their texts differ, and a release that
// builds one role's text for both fails here. Supervise's
// TestReleaseTaskTellsOnlyARoleThatCommitsToCommitAndPush checks what each text says.
func TestProjectReleaseAsksEachRoleOfAPodWithItsOwnText(t *testing.T) {
	t.Parallel()
	plan := idleRigPlan{TakeTurn: true, Unpushed: &idleRigReadUnpushed{Commits: 1}, AnswerRelease: true}
	rig := newIdleRigWith(t, idleRigOptions{},
		idleRigSeed{name: "pair", roles: []claim.Role{claim.RoleImplementer, claim.RoleReviewer}, plan: &plan})
	pair := rig.issue("pair")
	rig.launchAll()

	rig.startRelease("45s")
	release, _ := rig.awaitReleaseFinished()
	line := releaseLine(t, release, pair.pod)
	if line.Outcome != api.ReleaseDeletedLost || len(line.Asked) != 2 {
		t.Fatalf("the pod's line is %+v, want deleted_lost with both roles asked", line)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleReviewer} {
		asks := rig.releaseAsks(pair.key, role)
		if len(asks) != 1 {
			t.Fatalf("the %s was asked %d times, want once; its messages: %+v", role, len(asks),
				rig.agents.role(pair.key, string(role)).Messages)
		}
		// The daemon publishes the text as a JSON string (notify's publish), and the listener
		// relays that payload as it came, so the ask is the text once it is decoded.
		var text string
		if err := json.Unmarshal([]byte(asks[0].Text), &text); err != nil {
			t.Fatalf("decode the release's ask to the %s, %q: %v", role, asks[0].Text, err)
		}
		// release.BoundAt is the bound the daemon built the asks with: the run's bound and the
		// release's GET both read the release's stored row, and the view's JSON keeps every digit.
		if want := supervise.ReleaseTask(role, projectReleaseRound(release.ID), pair.key, release.BoundAt); text != want {
			t.Errorf("the release's ask to the %s is:\n%s\nwant the text built for its own role:\n%s", role, text, want)
		}
	}
}
