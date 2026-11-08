package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/api"
)

// podLogin is one pod of daemonAccount logging its service in with its own projected token and
// its own key (POST /v1/launcher-credentials/token), which is the call that moves the account's
// one launcher_credentials row onto that pod: its key_thumbprint and its pod_uid. It answers the
// credential's id, the same id for every pod of the account on the schema this release ships
// (IssueTokenLoginTx's subject-only conflict target).
func (ts *testServer) podLogin(t *testing.T, key *ecdsa.PrivateKey, podUID string) string {
	t.Helper()
	status, body := ts.tokenLogin(t, ts.accountToken(t, daemonAccount, podUID, time.Hour),
		signMachineLoginRequest(t, key, ts.URL, "", "cluster.example", "legion-daemon"))
	if status != http.StatusOK {
		t.Fatalf("token login of pod %s = %d %s, want 200", podUID, status, body)
	}
	return decode[wireTokenLogin](t, body).CredentialID
}

// TestALauncherProofEndsNothingUnderAKeyTheRowNoLongerHolds closes the race between the two reads
// of a launcher credential every proof-authenticated request makes: the proof's own lookup of the
// row's pinned key (proof.Verifier.LookupLauncher, enroll.Service.AuthenticateLauncher), and the
// read that hands the handler the credential the request acts under. The pods of one
// ServiceAccount share one credential row, and a login moves that row's key_thumbprint and
// pod_uid, so a sibling pod logging in between the two reads handed the caller the sibling's
// PodUID — and Revoke's own-account branch, which asks whether the credential's pod uid is the
// enrollment's runtime id, then judged the request as that sibling and ended its enrollment.
//
// The sibling's login runs inside the verifier's lookup, which is where the race lives: the row
// holds the caller's key when the proof is verified and the sibling's by the time the credential
// is read. The request is now refused 401 LAUNCHER_INVALID, since the key it proved holds no row,
// and the sibling's enrollment stays live and renews.
func TestALauncherProofEndsNothingUnderAKeyTheRowNoLongerHolds(t *testing.T) {
	// armed is false for every proof of the setup below and true for the one request this test
	// turns on, so the sibling logs in once and at that one moment.
	var armed atomic.Bool
	siblingLoggedIn := make(chan error, 1)
	var siblingLogin func() error
	ts := newTestServerWith(t, func(d *api.Deps) {
		pinned := d.Proof.LookupLauncher
		d.Proof.LookupLauncher = func(ctx context.Context, id string) (string, bool, error) {
			thumbprint, live, err := pinned(ctx, id)
			if armed.CompareAndSwap(true, false) {
				siblingLoggedIn <- siblingLogin()
			}
			return thumbprint, live, err
		}
	})

	sessionKey, siblingKey, callerKey := newSigningKey(t), newSigningKey(t), newSigningKey(t)
	siblingUID, callerUID := "pod-uid-sibling-"+uuid.NewString(), "pod-uid-caller-"+uuid.NewString()

	// The sibling pod logs in and enrols its own session, the enrollment the caller must not
	// reach: a pod of the login's own account, which Revoke's own-account branch judges by the
	// credential's pod uid.
	credentialID := ts.podLogin(t, siblingKey, siblingUID)
	status, body := ts.launcher(t, siblingKey, credentialID, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": siblingUID, "thumbprint": thumbprintOf(t, sessionKey),
		"pod_token": ts.podToken(t, daemonAccount, siblingUID),
	})
	if status != http.StatusCreated {
		t.Fatalf("the sibling pod enrolling itself = %d %s, want 201", status, body)
	}
	enrollmentID := decode[wireEnrolled](t, body).EnrollmentID

	// The caller is a second pod of that account. Its login takes the shared row over, which is
	// why its own proof verifies at all.
	if again := ts.podLogin(t, callerKey, callerUID); again != credentialID {
		t.Fatalf("the two pods of %s hold credentials %s and %s; this test turns on the one row they share", daemonAccount, credentialID, again)
	}

	// The sibling pod logs in again, as a restarted pod does, while the caller's proof is being
	// verified: the row goes back to the sibling's key and pod uid between the two reads. It is
	// built here, outside the handler's goroutine, so the hook only sends the request.
	login, err := json.Marshal(map[string]any{
		"token":   ts.accountToken(t, daemonAccount, siblingUID, time.Hour),
		"request": signMachineLoginRequest(t, siblingKey, ts.URL, "", "cluster.example", "legion-daemon"),
	})
	if err != nil {
		t.Fatalf("marshal the sibling's login: %v", err)
	}
	siblingLogin = func() error {
		resp, err := http.Post(ts.URL+"/v1/launcher-credentials/token", "application/json", bytes.NewReader(login))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("the sibling's login mid-request = %d, want 200", resp.StatusCode)
		}
		return nil
	}

	armed.Store(true)
	status, body = ts.launcher(t, callerKey, credentialID, http.MethodDelete, "/v1/enrollments/"+enrollmentID, nil)
	if err := <-siblingLoggedIn; err != nil {
		t.Fatalf("the sibling's login mid-request: %v", err)
	}
	if status != http.StatusUnauthorized || decode[wireError](t, body).Code != "LAUNCHER_INVALID" {
		t.Fatalf("DELETE /v1/enrollments/{id} by a key the row no longer holds = %d %s, want 401 LAUNCHER_INVALID", status, body)
	}
	// The race did happen: the row the caller's id names is the sibling's again, which is the
	// identity the old read by id alone would have handed the handler.
	if rows := ts.column(t, `select pod_uid from launcher_credentials where id=$1`, credentialID); !slices.Equal(rows, []string{siblingUID}) {
		t.Fatalf("the shared row's pod uid = %v, want the sibling's %s: the login mid-request did not move it", rows, siblingUID)
	}
	if status, body := ts.session(t, sessionKey, enrollmentID, http.MethodPost, "/v1/enrollments/"+enrollmentID+"/renew",
		map[string]any{"pod_token": ts.podToken(t, daemonAccount, siblingUID)}); status != http.StatusOK {
		t.Fatalf("the sibling's renew after the refused DELETE = %d %s, want 200: its enrollment must still be live", status, body)
	}
}
