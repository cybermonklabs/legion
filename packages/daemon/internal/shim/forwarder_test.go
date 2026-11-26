package shim_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// TestTheForwarderStartsOnceTheKeyExistsAndStopsWithTheShim: a shim told its role's GitHub
// forwarder starts it once keygen has written the role's key, in the shim's own environment with
// the role's key directory, and at the shim's end sends it SIGTERM and waits for it to finish,
// which is when the forwarder revokes its grants.
//
// The named revert: drop startForwarder's call from identity, and the forwarder never starts.
func TestTheForwarderStartsOnceTheKeyExistsAndStopsWithTheShim(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	cfg, keyDir, _ := agentSecretsConfig(t, path, o)
	log := filepath.Join(t.TempDir(), "forwarder.log")
	script := filepath.Join(t.TempDir(), "forwarder")
	body := `#!/bin/sh
trap 'kill "$pid" 2>/dev/null; printf "stopped\n" >>"` + log + `"; exit 0' TERM
key=no; [ -f "$AGENT_SECRETS_KEY_DIR/key.pem" ] && key=yes
printf 'started key=%s args=%s\n' "$key" "$*" >>"` + log + `"
sleep 300 >/dev/null 2>&1 </dev/null & pid=$!
wait "$pid"
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubForwarder = []string{script, "--role", "tester"}
	r := run(t, cfg, newClock())
	peer := stub.accept(t)
	peer.open(t)

	started := waitFor(t, log, "started")
	if !strings.Contains(started, "started key=yes args=--role tester") {
		t.Fatalf("the forwarder logged %q; want it started with its flags once %s/key.pem existed", started, keyDir)
	}
	r.stop(t)
	if stopped := waitFor(t, log, "stopped"); !strings.Contains(stopped, "stopped") {
		t.Fatalf("the forwarder logged %q; want its SIGTERM handled before the shim returned", stopped)
	}
	if lines := strings.Count(mustRead(t, log), "started"); lines != 1 {
		t.Fatalf("the forwarder started %d times; want once", lines)
	}
}

// TestTheForwarderIsNotStartedAgainDuringAStop: once the stop began (the shim SIGTERMed the agent,
// which outlives its SIGTERM, as a Claude Code role's save does), a forwarder that exits — role-init
// signals every process of the role at the stop's start — is not started again: a new one would
// ask for a fresh grant while the role is going away.
//
// The named revert: restart the forwarder while the shim's loop alone is live, and the shim says
// it restarts it.
func TestTheForwarderIsNotStartedAgainDuringAStop(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t, "FAKE_OMP_IGNORE_SIGTERM=1")
	cfg, _, _ := agentSecretsConfig(t, path, o)
	log := filepath.Join(t.TempDir(), "forwarder.log")
	script := filepath.Join(t.TempDir(), "forwarder")
	body := `#!/bin/sh
trap 'kill "$pid" 2>/dev/null; printf "stopped\n" >>"` + log + `"; exit 0' TERM
printf 'started %s\n' "$$" >>"` + log + `"
sleep 300 >/dev/null 2>&1 </dev/null & pid=$!
wait "$pid"
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubForwarder = []string{script}
	clock := newClock()
	r := run(t, cfg, clock)
	t.Cleanup(clock.release)
	peer := stub.accept(t)
	peer.open(t)
	started := waitFor(t, log, "started")
	pid, err := strconv.Atoi(strings.Fields(strings.TrimSpace(started))[1])
	if err != nil {
		t.Fatalf("the forwarder logged %q; want its pid", started)
	}

	r.cancel()
	r.log.await(t, "sending SIGTERM to the wrapped process", 1)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFor(t, log, "stopped")
	r.log.await(t, "forwarder exited", 1)
	if strings.Contains(r.log.String(), "restarting") {
		t.Fatalf("the shim restarts the forwarder during the stop:\n%s", r.log.String())
	}
	clock.release()
	r.wait(t)
	if lines := strings.Count(mustRead(t, log), "started"); lines != 1 {
		t.Fatalf("the forwarder started %d times; want once", lines)
	}
}

// TestAStaleEnrollmentIsRemovedBeforeKeygen: a kept pod's key directory outlives a generation, so
// it may hold the enrollment id of the previous generation's key. The shim removes it before
// keygen makes the new key, so the forwarder never pairs the old id with the new key, and waits for
// the daemon's new enrollment instead.
//
// The named revert: drop the removal from identity, and the old id is still there after keygen.
func TestAStaleEnrollmentIsRemovedBeforeKeygen(t *testing.T) {
	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	cfg, keyDir, _ := agentSecretsConfig(t, path, o)
	stale := filepath.Join(keyDir, "enrollment")
	if err := os.WriteFile(stale, []byte("enrollment-of-the-previous-generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := run(t, cfg, newClock())
	peer := stub.accept(t)
	if _, ok := peer.next(t).(shimwire.Hello2); !ok {
		t.Fatal("the shim's first frame is no hello2")
	}
	if _, err := os.Stat(filepath.Join(keyDir, "key.pem")); err != nil {
		t.Fatalf("keygen wrote no key before the hello: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("the previous generation's enrollment id is still in the key directory after keygen (%v); want it removed", err)
	}
	r.stop(t)
}

// waitFor waits up to ten seconds for the file at path to hold want, and answers what it holds.
func waitFor(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), want) {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return mustRead(t, path)
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
