package daemon

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A request for the replacement of a pod a project's pause holds opens no round, as the sweep
// opens none over such a pod (openPodUpdateUnderLimits, the opening step both take): the pause
// kept the pod and the work on its disk for the roles it stopped. Here the pause has ended and its
// hold still stands, which the resume leaves on a pod none of whose roles runs yet, and the
// route's own PROJECT_PAUSED check therefore passes the request through. The daemon refuses it
// 409 POD_HELD_BY_PAUSE, naming the pod, and posts nothing: no claim records a round, no opening
// is charged to the project's limits, and no ask is queued.
//
// The negative control is the opening step's pause branch undone (openPodUpdateUnderLimits's
// clock.held()): the request opens the round over the held pod and is answered 200.
func TestPodUpdateTheControllerAsksForOverAPodAPauseHoldsIsRefusedAndPostsNothing(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	route := newPodUpdateRoute(t, rig.s, rig.sup, quietLogger())
	pause := openLegionPause(t, rig.pool, podUpdateStart.Add(-time.Hour))
	holdLegionPod(t, rig.st, pause, podUpdateIssue, podUpdateStart.Add(-time.Hour))
	endLegionPause(t, rig.pool, pause, podUpdateStart.Add(-time.Minute))

	refusal := route.refused(podUpdateIssue, http.StatusConflict, "POD_HELD_BY_PAUSE")
	if !strings.Contains(refusal, "a project's pause holds the pod "+podUpdatePod+" of "+podUpdateIssue) {
		t.Errorf("the refusal says %q, want it to name the pause's hold of pod %s", refusal, podUpdatePod)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		rig.settle(role)
		if c := rig.claimOf(role); c.UpdateRound != "" {
			t.Errorf("the %s records round %q after the refused request, want none", role, c.UpdateRound)
		}
	}
	if opened, inFlight := charged(rig.s); !opened.IsZero() || len(inFlight) != 0 {
		t.Errorf("the refused request charged an opening at %v, in flight %v, want nothing charged", opened, inFlight)
	}
	if asks := rig.rows(record.OutboxKindUpdateAsk); len(asks) != 0 {
		t.Errorf("the refused request queued %d asks, want none", len(asks))
	}
}
