package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The operator's release route (`POST /legion/v1/operator/claims/{token}/release-names`, which
// `legion claims release-names` posts) answers the machine's release (supervise.Machine.
// ReleaseNames): while the secrets broker still carries the claim's old session, it is refused 409
// naming that session, its enrollment and when its carry ends, and frees nothing, so another claim
// registering the old session is still refused; once the carry ends it frees the old session's
// names, says the broker answered and which abandoned session it freed, and another claim then
// registers that session. An unknown claim is a 404.
func TestTheReleaseNamesRouteFreesOnlyWhatTheBrokerCarriesNothingOf(t *testing.T) {
	h, broker := sessionHoldHarness(t)
	owner, boot := h.podRole("LEGION-901", claim.RoleImplementer, 1)
	h.readyAt(owner, "ses_first", h.registered(boot, "ses_first"))
	boot = h.freshSession(owner, "ses_first", 2)
	h.readyAt(owner, "ses_second", h.registered(boot, "ses_second"))
	// Another role, so the broker recorder tells the two claims' enrollments apart (enrolledAs).
	other, otherBoot := h.podRole("LEGION-902", claim.RoleTester, 1)
	route := "/legion/v1/operator/claims/" + string(owner) + "/release-names"

	ends := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	broker.carry(supervise.CarriedEnrollment{SessionID: "ses_first", EnrollmentID: "enr-1", State: supervise.CarriedDormant, EndsAt: ends})
	refused := h.operator(http.MethodPost, route, nil)
	if body := refused.Body.String(); refused.Code != http.StatusConflict || !strings.Contains(body, "ses_first") ||
		!strings.Contains(body, "enr-1") || !strings.Contains(body, ends.Format(time.RFC3339)) {
		t.Fatalf("the release while the broker carries ses_first = %d %s, want 409 naming ses_first, enr-1 and %s", refused.Code, body, ends.Format(time.RFC3339))
	}
	h.wantSessionRefused(broker, other, otherBoot, "ses_first", owner)

	broker.endCarry("ses_first")
	recorder := h.operator(http.MethodPost, route, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the release once the broker carries nothing = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var released ReleasedNames
	if err := json.Unmarshal(recorder.Body.Bytes(), &released); err != nil {
		t.Fatalf("decode the release's answer %s: %v", recorder.Body, err)
	}
	if released.Claim != owner || released.Checked != supervise.CheckedByBroker || released.Abandoned != "ses_first" ||
		!slices.Contains(released.Freed, ReleasedName{Kind: string(supervise.HoldSessionID), Name: "ses_first"}) {
		t.Fatalf("the release answered %+v, want %s's ses_first freed, checked %q, abandoned ses_first", released, owner, supervise.CheckedByBroker)
	}
	if slices.Contains(released.Freed, ReleasedName{Kind: string(supervise.HoldSessionID), Name: "ses_second"}) {
		t.Fatalf("the release freed ses_second, the session %s records: %+v", owner, released)
	}
	if got := h.registered(otherBoot, "ses_first"); got.ClaimToken != other {
		t.Fatalf("%s registering the released ses_first answered %+v, want it admitted", other, got)
	}

	unknown := h.operator(http.MethodPost, "/legion/v1/operator/claims/legion-legion-legion-999-implementer/release-names", nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("the release of an unknown claim = %d, want 404; body %s", unknown.Code, unknown.Body)
	}
}
