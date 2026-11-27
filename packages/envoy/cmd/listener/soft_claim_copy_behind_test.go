package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
)

// A soft claim leaves a live holder its role even where the copy of the session bucket a direct
// get reaches has not applied that holder's registration: the holder's session is judged on the
// bucket stream leader's record. Two reads decide it: the holder's liveness when the claimant
// names no predecessor or another one (roleSupersessionVerdict through liveSessionEntry), and
// the declared predecessor's registration, whose client instance decides whether the role
// follows the claimant (bucketSessionEntry). Before, the copy's "no record" read as a holder that
// is gone, and the claim took a live holder's role. Control: a holder whose session the bucket
// has no record of gives the role up through the same copy.
func TestASoftClaimLeavesALiveHolderItsRoleWhileTheSessionsCopyIsBehind(t *testing.T) {
	t.Parallel()
	const (
		holder           = "ses_soft_copy_behind_holder"
		claimantID       = "ses_soft_copy_behind_claimant"
		claimantInstance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
		holderInstance   = "9b8a7c6d5e4f3a2b9b8a7c6d5e4f3a2b"
	)
	for _, tc := range []struct {
		name        string
		predecessor bool
		registered  bool
	}{
		{"the holder's liveness: a live holder the copy answers absent", false, true},
		{"the declared predecessor: a live predecessor of another instance the copy answers absent", true, true},
		{"control: the holder's liveness, a holder the bucket has no record of", false, false},
		{"control: the declared predecessor, a predecessor the bucket has no record of", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimant, observer := trailingListeners(t)
			role := map[bool]string{false: "soft-copy-behind-holder", true: "soft-copy-behind-predecessor"}[tc.predecessor]
			claimRole(t, claimant, holder, role)
			registerInstance(t, claimant.sessions, holder, holderInstance)
			if !tc.registered {
				if err := claimant.sessions.Delete(context.Background(), holder); err != nil {
					t.Fatalf("end %s's session: %v", holder, err)
				}
			}
			registerInstance(t, observer.sessions, claimantID, claimantInstance)
			behindForSession(t, observer, holder)

			body := `{"session_id":"` + claimantID + `","role":"` + role + `","soft":true}`
			if tc.predecessor {
				body = `{"session_id":"` + claimantID + `","role":"` + role + `","soft":true,"previous_session_id":"` + holder + `"}`
			}
			recorder := httptest.NewRecorder()
			roleSetHandler(observer, "test-machine", logging.New("test")).ServeHTTP(recorder,
				httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(body)))
			wantHolder := map[bool]string{true: holder, false: claimantID}[tc.registered]
			switch {
			case tc.registered && (recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"holder":"`+holder+`"`)):
				t.Errorf("soft claim of a live holder's role through a copy behind: status %d %s, want 409 naming %s", recorder.Code, recorder.Body.String(), holder)
			case !tc.registered && recorder.Code != http.StatusOK:
				t.Errorf("soft claim of a gone holder's role: status %d %s, want 200", recorder.Code, recorder.Body.String())
			}
			if got, err := claimant.registry.RoleHolder(role); err != nil || got != wantHolder {
				t.Fatalf("%s's holder after the soft claim = %q, %v; want %q", role, got, err, wantHolder)
			}
		})
	}
}

// A claim is granted to a claimant whose registration the session bucket holds, even where the
// copy of the bucket a direct get reaches has not applied that registration and this listener's
// cache does not hold it either: the claim route reads the claimant's registration from the bucket
// stream's leader (liveSessionEntry with session.SessionRegistry.RefreshFromLeader). A
// registration made through another pod a moment ago is the one this misses. Before, a direct get
// in that place answered "no record" through the copy, and the claim was refused 404 "session is
// not registered" for a session that is. Control: a claimant the bucket has no record of is
// refused 404 through the same copy.
func TestAClaimByAClaimantWhoseRegistrationTheSessionsCopyHasNotAppliedIsGranted(t *testing.T) {
	t.Parallel()
	const claimantInstance = "6d5c4b3a2f1e0d9c6d5c4b3a2f1e0d9c"
	for _, tc := range []struct {
		name       string
		registered bool
	}{
		{"a claimant the copy answers absent", true},
		{"control: a claimant the bucket has no record of", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimantID := map[bool]string{true: "ses_claim_copy_behind_registered", false: "ses_claim_copy_behind_unknown"}[tc.registered]
			const role = "claim-copy-behind"
			claimant, observer := trailingListeners(t)
			if tc.registered {
				registerInstance(t, claimant.sessions, claimantID, claimantInstance)
			}
			behindForSession(t, observer, claimantID)

			recorder := httptest.NewRecorder()
			roleSetHandler(observer, "test-machine", logging.New("test")).ServeHTTP(recorder,
				httptest.NewRequest(http.MethodPost, "/v1/roles/set", strings.NewReader(`{"session_id":"`+claimantID+`","role":"`+role+`"}`)))
			wantHolder := ""
			switch {
			case tc.registered && recorder.Code != http.StatusOK:
				t.Fatalf("claim by a registered claimant through a copy behind and a cache without it: status %d %s, want 200", recorder.Code, recorder.Body.String())
			case tc.registered:
				wantHolder = claimantID
			case recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "session is not registered"):
				t.Fatalf("claim by a claimant the bucket has no record of: status %d %s, want 404 session is not registered", recorder.Code, recorder.Body.String())
			}
			if got, err := claimant.registry.RoleHolder(role); err != nil || got != wantHolder {
				t.Fatalf("%s's holder after the claim = %q, %v; want %q", role, got, err, wantHolder)
			}
		})
	}
}
