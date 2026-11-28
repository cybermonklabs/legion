//go:build unix

// packages/envoy/cmd/cipher/pod_test.go
//
// The key `enroll --pod` enrols, against the real broker (ciphertest.NewRig: real handlers, real
// Postgres). It skips without BROKER_TEST_DATABASE_URL, as every rig test does.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
)

// TestEnrollPodProvesTheKeyItKeeps: a role's key directory already holds the key.pem `agent-secrets
// keygen` wrote before the role's first hello, and `enroll --pod` enrols that key rather than one
// of its own, so the enrollment the broker makes names that key's thumbprint. A role's trade
// proves the same file (cipherclient.MachineKey), and the broker holds a pod session's trade to
// the key its registration names, so a role whose enrolment and trade proved two keys would be
// refused the trade.
func TestEnrollPodProvesTheKeyItKeeps(t *testing.T) {
	rig := ciphertest.NewRig(t)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	out := filepath.Join(t.TempDir(), "keys")

	var stdout, stderr strings.Builder
	if code := cmdKeygen([]string{"--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("keygen = exit %d, stderr %q", code, stderr.String())
	}
	kept := strings.TrimSpace(stdout.String())

	tokenFile := writePodToken(t, rig.MintAccountPodToken(t, ciphertest.DaemonAccount, uuid.NewString(), time.Hour))
	stdout.Reset()
	stderr.Reset()
	if code := cmdEnrollPod("legion-daemon", tokenFile, out, podProjects{}, &stdout, &stderr); code != 0 {
		t.Fatalf("enroll --pod over keygen's directory = exit %d, stderr %q", code, stderr.String())
	}
	id := strings.TrimSpace(stdout.String())

	var enrolled string
	if err := rig.Store.Pool.QueryRow(context.Background(), `select thumbprint from enrollments where id=$1 and revoked_at is null`, id).
		Scan(&enrolled); err != nil {
		t.Fatalf("read the enrollment %q the broker made: %v", id, err)
	}
	if enrolled != kept {
		t.Fatalf("enroll --pod's enrollment %s names the key of thumbprint %s, want the key.pem keygen wrote, %s", id, enrolled, kept)
	}
}

// TestEnrollPodAgainstTheBrokerWaitsOutAnUnreadableKeySetsRetryAfter: enroll --pod against the
// real broker while the key set of the issuer that signed the pod's token cannot be read. The
// broker answers the token login 503 ISSUER_UNAVAILABLE with Retry-After: 5, the spacing of its
// reads of a key set that failed, and the command waits it out: its next login leaves no sooner
// than 5 s after the broker answered the first, so it reaches the broker no sooner than 5 s after
// the first did. The set is served again 6 s after it failed, and the command completes: the
// broker holds the enrollment it printed, and no refusal is left in enrollment.error. It runs on
// the wall clock, which the broker's spacing of its reads of the set runs on too.
func TestEnrollPodAgainstTheBrokerWaitsOutAnUnreadableKeySetsRetryAfter(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process (t.Setenv), and it replaces http.DefaultTransport, which the command's broker client sends through (newClient leaves its Transport unset).
	rig := ciphertest.NewRig(t)
	t.Setenv("AGENT_SECRETS_URL", rig.URL)
	out := filepath.Join(t.TempDir(), "keys")
	tokenFile := writePodToken(t, rig.MintAccountPodToken(t, ciphertest.DaemonAccount, uuid.NewString(), time.Hour))

	// Each token login the command sends: when it left, when its answer came back, and the
	// answer's status.
	type login struct {
		sent, answered time.Time
		status         int
	}
	var mu sync.Mutex
	var logins []login
	base := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.String() != rig.URL+"/v1/launcher-credentials/token" {
			return base.RoundTrip(r)
		}
		sent := time.Now()
		resp, err := base.RoundTrip(r)
		each := login{sent: sent, answered: time.Now()}
		if err == nil {
			each.status = resp.StatusCode
		}
		mu.Lock()
		logins = append(logins, each)
		mu.Unlock()
		return resp, err
	})
	t.Cleanup(func() { http.DefaultTransport = base })

	rig.FailPodKeys(http.StatusServiceUnavailable)
	restore := time.AfterFunc(6*time.Second, func() { rig.FailPodKeys(0) })
	t.Cleanup(func() { restore.Stop() })
	var stdout, stderr strings.Builder
	if code := cmdEnrollPod("legion-daemon", tokenFile, out, podProjects{}, &stdout, &stderr); code != 0 {
		t.Fatalf("enroll --pod through an unreadable key set = exit %d, stderr %q; want it to wait the outage out and enrol", code, stderr.String())
	}

	mu.Lock()
	sentLogins := append([]login(nil), logins...)
	mu.Unlock()
	if len(sentLogins) < 2 || sentLogins[0].status != http.StatusServiceUnavailable || sentLogins[len(sentLogins)-1].status != http.StatusOK {
		t.Fatalf("the command's token logins = %+v; want the broker's 503 first, and its 200 last", sentLogins)
	}
	for i, each := range sentLogins[:len(sentLogins)-1] {
		if each.status != http.StatusServiceUnavailable {
			t.Fatalf("token login %d was answered %d before the last, want 503 while the key set could not be read", i+1, each.status)
		}
	}
	if gap := sentLogins[1].sent.Sub(sentLogins[0].answered); gap < 5*time.Second {
		t.Fatalf("the second token login left %s after the broker answered the first 503 with Retry-After: 5, want no sooner than 5 s", gap)
	}
	id := strings.TrimSpace(stdout.String())
	var thumbprint string
	if err := rig.Store.Pool.QueryRow(context.Background(), `select thumbprint from enrollments where id=$1 and revoked_at is null`, id).
		Scan(&thumbprint); err != nil {
		t.Fatalf("read the enrollment %q the command printed: %v", id, err)
	}
	if _, err := os.Stat(filepath.Join(out, enrollmentErrorFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s is left in the key directory (%v); want no refusal recorded once the command enrolled", enrollmentErrorFile, err)
	}
}
