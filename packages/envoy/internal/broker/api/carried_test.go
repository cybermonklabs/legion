package api_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// wireCarried is POST /v1/enrollments/carried's answer.
type wireCarried struct {
	Carried []struct {
		SessionID    string    `json:"session_id"`
		EnrollmentID string    `json:"enrollment_id"`
		State        string    `json:"state"`
		EndsAt       time.Time `json:"ends_at"`
	} `json:"carried"`
}

// askCarried posts body to POST /v1/enrollments/carried under the launcher credential
// credentialID, signed with key.
func (ts *testServer) askCarried(t *testing.T, key *ecdsa.PrivateKey, credentialID string, body any) (int, []byte) {
	t.Helper()
	return ts.launcher(t, key, credentialID, http.MethodPost, "/v1/enrollments/carried", body)
}

// TestTheCarriedRouteAnswersTheCallersOwnPodEnrollments drives the route over real HTTP: a
// daemon's token login enrols a pod of a worker account under session S, and asking about S and
// an id nothing names answers that one enrollment, live, ending at the lease its row holds, which
// is the lease end the enrolment itself answered (both to the microsecond Postgres keeps:
// enroll.Service.Create truncates what it answers to it). Another daemon's login asking about S is
// answered `{"carried": []}`, an empty list rather than null.
//
// The control (the pull request records the run): Create answering its lease end to the
// nanosecond again fails the comparison with the enrolment's own answer.
func TestTheCarriedRouteAnswersTheCallersOwnPodEnrollments(t *testing.T) {
	ts := newTestServer(t)
	daemon, daemonKey := ts.mintServiceLauncherCredential(t)
	other, otherKey := ts.mintServiceLauncherCredentialAs(t, otherDaemonAccount)
	const session = "ses_carried_route"
	podUID := uuid.NewString()
	status, body := ts.launcher(t, daemonKey, daemon, http.MethodPost, "/v1/enrollments", map[string]any{
		"kind": "pod", "runtime_id": podUID, "slot": "implementer-g1", "thumbprint": thumbprintOf(t, newSigningKey(t)),
		"pod_token": ts.podToken(t, workerAccount, podUID), "session_id": session,
	})
	if status != http.StatusCreated {
		t.Fatalf("enrol the pod = %d %s, want 201", status, body)
	}
	enrolled := decode[wireEnrolled](t, body).EnrollmentID
	answeredLease := decode[struct {
		LeaseExpiresAt time.Time `json:"lease_expires_at"`
	}](t, body).LeaseExpiresAt

	status, body = ts.askCarried(t, daemonKey, daemon, map[string]any{"session_ids": []string{session, "ses_nobody", session}})
	if status != http.StatusOK {
		t.Fatalf("POST /v1/enrollments/carried as the enrolling daemon = %d %s, want 200", status, body)
	}
	got := decode[wireCarried](t, body).Carried
	lease := ts.column(t, `select (extract(epoch from lease_expires_at) * 1000000)::bigint::text from enrollments where id=$1`, enrolled)
	if len(got) != 1 || got[0].SessionID != session || got[0].EnrollmentID != enrolled || got[0].State != "live" ||
		len(lease) != 1 || strconv.FormatInt(got[0].EndsAt.UnixMicro(), 10) != lease[0] {
		t.Fatalf("the enrolling daemon was answered %s, want its one enrollment %s of %s, live, ending at its lease end %v (Unix microseconds)",
			body, enrolled, session, lease)
	}
	if !got[0].EndsAt.Equal(answeredLease) {
		t.Fatalf("the route answers the lease ending %s, and the enrolment answered %s: want one time, the row's own",
			got[0].EndsAt.Format(time.RFC3339Nano), answeredLease.Format(time.RFC3339Nano))
	}

	status, body = ts.askCarried(t, otherKey, other, map[string]any{"session_ids": []string{session}})
	if status != http.StatusOK || !strings.Contains(string(body), `"carried":[]`) {
		t.Fatalf("POST /v1/enrollments/carried as another daemon = %d %s, want 200 {\"carried\":[]}", status, body)
	}
}

// TestTheCarriedRouteRefusesALoginThatProvedNoAccount: a person's own machine login carries no
// subject and launched nothing the route could answer for, so it is 403 SERVICE_LOGIN_REQUIRED,
// and the route says so before it reads the body: a body the route would refuse 400 is 403 too
// (side review of #4355), so such a caller never has a list decoded.
//
// The control (the pull request records the run): the caller checked after the body is read
// answers the list of numbers 400 INVALID_SESSION_IDS.
func TestTheCarriedRouteRefusesALoginThatProvedNoAccount(t *testing.T) {
	ts := newTestServer(t)
	credentialID, key := ts.mintLauncherCredential(t, testApprover, "example-host-devbox")
	for _, c := range []struct {
		name string
		body any
	}{
		{"one id", map[string]any{"session_ids": []string{"ses_carried_route"}}},
		{"a list of numbers", map[string]any{"session_ids": []int{1}}},
	} {
		status, body := ts.askCarried(t, key, credentialID, c.body)
		if status != http.StatusForbidden || decode[wireError](t, body).Code != "SERVICE_LOGIN_REQUIRED" {
			t.Fatalf("POST /v1/enrollments/carried with %s under a person's machine login = %d %.200s, want 403 SERVICE_LOGIN_REQUIRED", c.name, status, body)
		}
	}
}

// TestTheCarriedRouteRefusesSessionIDsOutsideItsBounds: an empty list, 4097 ids, an id of 256
// bytes, no list at all and a body that is no such object are each 400 INVALID_SESSION_IDS.
func TestTheCarriedRouteRefusesSessionIDsOutsideItsBounds(t *testing.T) {
	ts := newTestServer(t)
	daemon, key := ts.mintServiceLauncherCredential(t)
	tooMany := make([]string, 4097)
	for i := range tooMany {
		tooMany[i] = "ses_" + uuid.NewString()
	}
	for _, c := range []struct {
		name string
		body any
	}{
		{"an empty list", map[string]any{"session_ids": []string{}}},
		{"4097 ids", map[string]any{"session_ids": tooMany}},
		{"an id of 256 bytes", map[string]any{"session_ids": []string{strings.Repeat("x", 256)}}},
		{"no list", map[string]any{}},
		{"a list of numbers", map[string]any{"session_ids": []int{1}}},
		{"a field the route does not take", map[string]any{"session_ids": []string{"ses_a"}, "subject": "someone"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, body := ts.askCarried(t, key, daemon, c.body)
			if status != http.StatusBadRequest || decode[wireError](t, body).Code != "INVALID_SESSION_IDS" {
				t.Fatalf("POST /v1/enrollments/carried with %s = %d %s, want 400 INVALID_SESSION_IDS", c.name, status, body)
			}
		})
	}
}

// TestTheCarriedRouteAnswersTheLongestListItsBoundsAllow: 4,096 ids of 255 bytes are inside the
// route's bounds, so they are answered 200 however they are written: plainly, a body longer than
// the 1 MiB every other JSON route caps its body at, and with every byte escaped as JSON's
// longest escape (\u00XX), the longest compact body the bounds allow. A body one escaped byte past
// that, of 4,096 ids so the count does not refuse it first, is still 413 REQUEST_TOO_LARGE, so
// the route keeps a cap of its own; a list of 4,097 ids is refused at its last entry
// (TestTheCarriedRouteRefusesSessionIDsOutsideItsBounds).
func TestTheCarriedRouteAnswersTheLongestListItsBoundsAllow(t *testing.T) {
	ts := newTestServer(t)
	daemon, key := ts.mintServiceLauncherCredential(t)
	list := func(n int, id string) map[string]any {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = id
		}
		return map[string]any{"session_ids": ids}
	}
	size := func(body any) int {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal the body: %v", err)
		}
		return len(b)
	}
	// json.Marshal writes U+0001 as \u0001, six bytes for each byte of the id.
	plain, escaped := strings.Repeat("x", 255), strings.Repeat("\x01", 255)
	for _, c := range []struct {
		name string
		body map[string]any
		size int
	}{
		{"4096 plain ids of 255 bytes", list(4096, plain), 4096*(255+3) - 1 + len(`{"session_ids":[]}`)},
		{"4096 ids of 255 bytes, each byte escaped", list(4096, escaped), 4096*(6*255+3) - 1 + len(`{"session_ids":[]}`)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := size(c.body); got != c.size || got <= 1<<20 {
				t.Fatalf("the body is %d bytes, want %d, more than 1 MiB", got, c.size)
			}
			status, body := ts.askCarried(t, key, daemon, c.body)
			if status != http.StatusOK || !strings.Contains(string(body), `"carried":[]`) {
				t.Fatalf("POST /v1/enrollments/carried with %s = %d %.200s, want 200 {\"carried\":[]}", c.name, status, body)
			}
		})
	}
	t.Run("a body one escaped byte past the longest list", func(t *testing.T) {
		past := list(4096, escaped)
		past["session_ids"].([]string)[4095] = escaped + "\x01"
		status, body := ts.askCarried(t, key, daemon, past)
		if status != http.StatusRequestEntityTooLarge || decode[wireError](t, body).Code != "REQUEST_TOO_LARGE" {
			t.Fatalf("POST /v1/enrollments/carried with a body one escaped byte past the longest list = %d %.200s, want 413 REQUEST_TOO_LARGE", status, body)
		}
	})
}
