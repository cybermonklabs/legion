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
