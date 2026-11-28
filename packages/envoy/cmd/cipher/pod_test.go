//go:build unix

// packages/envoy/cmd/cipher/pod_test.go
//
// The key `enroll --pod` enrols, against the real broker (ciphertest.NewRig: real handlers, real
// Postgres). It skips without BROKER_TEST_DATABASE_URL, as every rig test does.
package main

import (
	"context"
	"path/filepath"
	"strings"
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
