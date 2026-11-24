package api

import (
	"context"
	"encoding/json"
	"errors"
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
// registers that session. A broker that does not answer is a 503, which a run again can change, and
// one that refuses the question for good (a 4xx) a 502 that says a run again answers the same,
// unless the broker is rolling (review of #4355); each is logged as an operator request that
// failed (side review of #4355). An answer too long to read is a 409 too, a carry the release
// cannot list. None frees anything. An unknown claim is a 404.
//
// The controls (the pull request records each run): the 502 and the 503 writing no log line fail
// the log rows.
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

	broker.failCarried(errors.New("dial tcp: connection refused"))
	if outage := h.operator(http.MethodPost, route, nil); outage.Code != http.StatusServiceUnavailable || !strings.Contains(outage.Body.String(), "run it again") {
		t.Fatalf("the release while the broker does not answer = %d %s, want 503 saying to run it again", outage.Code, outage.Body)
	}
	if lines := h.serverLog.lines("api: an operator request failed", "release-names", "did not answer"); len(lines) != 1 {
		t.Fatalf("the 503 logged %q, want one failed-request line", h.serverLog.lines("release-names"))
	}
	broker.failCarried(refusedQuestion{})
	if refusal := h.operator(http.MethodPost, route, nil); refusal.Code != http.StatusBadGateway ||
		!strings.Contains(refusal.Body.String(), "answers the same") || !strings.Contains(refusal.Body.String(), "while the broker rolls") {
		t.Fatalf("the release the broker refuses for good = %d %s, want 502 saying a run again answers the same unless the broker is rolling", refusal.Code, refusal.Body)
	}
	if lines := h.serverLog.lines("api: an operator request failed", "release-names", "refused the question"); len(lines) != 1 {
		t.Fatalf("the 502 logged %q, want one failed-request line", h.serverLog.lines("release-names"))
	}
	broker.failCarried(unlistedCarry{})
	if unlisted := h.operator(http.MethodPost, route, nil); unlisted.Code != http.StatusConflict || !strings.Contains(unlisted.Body.String(), "more enrollments than it can list") {
		t.Fatalf("the release whose answer is too long to read = %d %s, want 409 saying the broker carries more than it can list", unlisted.Code, unlisted.Body)
	}
	broker.failCarried(nil)
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

// refusedQuestion is the broker's refusal of a question a retry cannot change, as the daemon's
// broker client answers a 4xx (supervise.PermanentError).
type refusedQuestion struct{}

func (refusedQuestion) Error() string   { return "broker answered 403 SERVICE_LOGIN_REQUIRED" }
func (refusedQuestion) Permanent() bool { return true }

// unlistedCarry is the broker client's answer that was too long to read whole, as the machine
// sees it (supervise.UnlistedCarryError).
type unlistedCarry struct{}

func (unlistedCarry) Error() string {
	return "the broker's answer is longer than the 8388608 bytes the daemon reads of one, so it names more enrollments than it can list"
}
func (unlistedCarry) UnlistedCarry() bool { return true }

// releaseOnlyStore is a store that holds no claim by boot token hash and whose one claim write is
// an operator's release (supervise.Store.ReleaseHolds), which answers err, or takes the write when
// err is nil.
type releaseOnlyStore struct {
	supervise.Store
	err error
}

func (releaseOnlyStore) ClaimByBootTokenHash(context.Context, []byte) (supervise.Claim, bool, error) {
	return supervise.Claim{}, false, nil
}

func (s releaseOnlyStore) ReleaseHolds(context.Context, supervise.Claim, []supervise.Hold) ([]supervise.Hold, error) {
	return nil, s.err
}

// An operator's release writes the claim (supervise.Store.ReleaseHolds) and so is one of the
// claim writes BootTokens.Recording remembers a launch by: a release the store took records the
// boot token hash the claim carries, as a PutClaim of it would, and one the store refused records
// nothing, since the claim was not written (review of #4355).
//
// The control (the pull request records the run): recordingStore without its ReleaseHolds, so the
// embedded store's is promoted and nothing is recorded: the token is unknown after a release the
// store took.
func TestAReleaseRecordsTheBootTokenItsClaimCarries(t *testing.T) {
	const boot = "a-release-boot-token"
	c := supervise.Claim{Token: claim.Token("legion-legion-legion-903-implementer"), Generation: 3, BootTokenHash: supervise.HashBootToken(boot)}
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		err   error
		known bool
	}{
		{name: "refused", err: errors.New("the database refused the claim write"), known: false},
		{name: "taken", known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := releaseOnlyStore{err: tc.err}
			tokens := NewBootTokens(store)
			if _, err := tokens.Recording(store).ReleaseHolds(ctx, c, nil); (err != nil) != (tc.err != nil) {
				t.Fatalf("the release = %v, want error %v", err, tc.err)
			}
			launch, known, err := tokens.Resolve(ctx, boot)
			if err != nil || known != tc.known {
				t.Fatalf("the release's boot token resolves known=%v, %v; want known=%v", known, err, tc.known)
			}
			if known && (launch.Claim != c.Token || launch.Generation != c.Generation) {
				t.Fatalf("the release's boot token resolves to %+v, want %s at generation %d", launch, c.Token, c.Generation)
			}
		})
	}
}
