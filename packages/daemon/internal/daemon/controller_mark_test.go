package daemon

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Out of reach a second time within 6 hours, the daemon's controller is not relaunched, and
// read_state names it as unreachable (R5): the controller's locator carries its claim's mark, and
// carries none while the claim is reachable.
func TestTheControllersLocatorCarriesItsClaimsMark(t *testing.T) {
	project, err := claim.ProjectToken("legion")
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 11, 12, 18, 20, 0, 0, time.UTC)
	controller := supervise.Claim{Token: claim.ControllerToken(project), Role: claim.RoleController,
		Reach: supervise.Reach{Session: "ses_controller", Since: since}}
	worker := supervise.Claim{Token: "legion-legion-legion-209-implementer", Role: claim.RoleImplementer,
		Reach: supervise.Reach{Session: "ses_implementer", Since: since.Add(time.Hour)}}

	locator := &api.ControllerLocator{SessionID: "ses_controller"}
	markControllerLocator(locator, project, []supervise.Claim{worker, controller})
	if locator.Unreachable == nil || !locator.Unreachable.Since.Equal(since) {
		t.Fatalf("the locator's mark = %+v, want the controller claim's since %s", locator.Unreachable, since)
	}

	reachable := &api.ControllerLocator{SessionID: "ses_controller"}
	controller.Reach = supervise.Reach{Session: "ses_controller"}
	markControllerLocator(reachable, project, []supervise.Claim{worker, controller})
	if reachable.Unreachable != nil {
		t.Fatalf("the locator of a reachable controller = %+v, want no mark", reachable.Unreachable)
	}
	markControllerLocator(nil, project, []supervise.Claim{controller})
}
