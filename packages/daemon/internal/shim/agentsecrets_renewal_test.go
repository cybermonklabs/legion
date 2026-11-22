package shim_test

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The waits of `agent-secrets renew` this test is bounded by, as this tree's
// packages/envoy/internal/broker/leasetime declares them (an internal package of another module,
// so they are restated here rather than imported).
const (
	// renewalUnanswered is how long the renewer waits on a renewal that gets no answer: its HTTP
	// client's whole-call timeout, leasetime.RenewClientTimeout.
	renewalUnanswered = 30 * time.Second
	// renewalRetry is its first wait after that failure, leasetime.RenewBackoffInitial.
	renewalRetry = 2 * time.Second
	// renewalSlack is the rest of the bound: the renewer's start and the scheduler. Measured on
	// 2026-11-16, the second renewal reached a broker that never answered 32 s after the first.
	renewalSlack = 6 * time.Second
)

// R4's end for the secrets broker: a renewal the broker accepts and never answers is followed by
// another renewal, so the pod's lease is not left to lapse behind one hung call. The real
// `agent-secrets`, built from this tree, runs under the real shim as a pod's renewer does, against
// a broker that accepts every connection and answers nothing.
//
// What makes the second renewal is the renewer's own client timeout, not the shim: the renewer
// is one process that renews on its own schedule and says nothing when a renewal succeeds, so the
// shim cannot tell a hung renewer from a healthy one, and a shim-side bound would kill healthy
// renewers on a timer and be a second renewal writer. So the shim must not restart it here: the
// renewer started once and never exited.
//
// The test takes about 32 seconds of wall time, because that is the renewer's own behaviour it
// proves: 30 s for the unanswered renewal to time out and 2 s of backoff before the next. Those
// waits are constants of the binary, and a binary rebuilt with shorter ones would not be the
// renewer a pod runs.
func TestARenewalTheBrokerNeverAnswersIsMadeAgain(t *testing.T) {
	const enrollment = "enr-unanswered"
	renewer := buildAgentSecrets(t)
	brokerURL, renewals := silentBroker(t, enrollment)

	path := socketPath(t)
	stub := listen(t, path)
	o := newOMP(t)
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "keys")
	if err := os.Mkdir(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("eyJhbGciOiJSUzI1NiJ9.e30.sig\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config(t, path, o)
	cfg.AgentSecrets = &shim.AgentSecrets{KeyDir: keyDir, TokenFile: tokenFile, Binary: renewer}
	// The renewer's identity is this test's key directory alone: an AGENT_SECRETS_* variable
	// inherited from the machine running the test could name a person's own session instead.
	cfg.Env = append(withoutAgentSecrets(cfg.Env), "AGENT_SECRETS_KEY_DIR="+keyDir, "AGENT_SECRETS_URL="+brokerURL)
	r := run(t, cfg, newClock())
	peer := stub.accept(t)
	peer.open(t)

	peer.send(t, shimwire.AgentSecretsEnrollment{ID: "req-1", EnrollmentID: enrollment})
	peer.expect(t, shimwire.AgentSecretsEnrollmentResult{ID: "req-1", OK: true})

	first := awaitRenewal(t, renewals, waitLimit, "the renewer's first renewal")
	bound := renewalUnanswered + renewalRetry + renewalSlack
	second := awaitRenewal(t, renewals, bound, "a second renewal, after the first got no answer,")
	gap := second.Sub(first)
	if gap < renewalUnanswered-time.Second {
		t.Fatalf("the second renewal came %s after the first, before the first could have timed out (%s): it is not the renewal made again", gap, renewalUnanswered)
	}
	t.Logf("the second renewal reached the broker %s after the first, which got no answer", gap.Round(100*time.Millisecond))
	if r.log.count("renewer started") != 1 || r.log.count("renewer exited") != 0 {
		t.Fatalf("the shim restarted the renewer; the renewal must be made again by the renewer itself. log:\n%s", r.log)
	}
	r.stop(t)
}

// buildAgentSecrets builds `agent-secrets` from this tree's packages/envoy, the binary a pod's
// worker image carries beside the shim. Its source is `cmd/cipher`, which the worker image builds
// into `agent-secrets` (worker.Dockerfile).
func buildAgentSecrets(t *testing.T) string {
	t.Helper()
	envoy, err := filepath.Abs(filepath.Join("..", "..", "..", "envoy"))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "agent-secrets")
	build := exec.Command("go", "build", "-o", out, "./cmd/cipher")
	build.Dir = envoy
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/cipher in %s: %v\n%s", envoy, err, output)
	}
	return out
}

// silentBroker is a secrets broker that accepts every connection and answers nothing: a front
// door with nothing behind it. Each renewal of enrollment that reaches it is sent on the channel,
// stamped with when its request line arrived.
func silentBroker(t *testing.T, enrollment string) (string, <-chan time.Time) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	renewals := make(chan time.Time, 16)
	renewal := "POST /v1/enrollments/" + enrollment + "/renew "
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
			go func() {
				lines := bufio.NewReader(conn)
				for {
					line, err := lines.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, renewal) {
						renewals <- time.Now()
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	return "http://" + ln.Addr().String(), renewals
}

func awaitRenewal(t *testing.T, renewals <-chan time.Time, bound time.Duration, what string) time.Time {
	t.Helper()
	select {
	case at := <-renewals:
		return at
	case <-time.After(bound):
		t.Fatalf("%s did not reach the broker within %s", what, bound)
		return time.Time{}
	}
}

// withoutAgentSecrets is env without its AGENT_SECRETS_* variables.
func withoutAgentSecrets(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, "AGENT_SECRETS_") {
			kept = append(kept, entry)
		}
	}
	return kept
}
