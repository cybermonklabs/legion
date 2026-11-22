//go:build e2e

// The stand-in secrets broker of check w4 (RIG.md), for its renewal end: an agent-secrets renewal
// the broker does not answer (R4). The shim's renewer is the real `agent-secrets renew`, built from
// the checkout under test, which the shim runs once the rig hands it its enrollment as the
// Kubernetes runtime's enroller does. The stand-in accepts every renewal; the first it holds
// unanswered until the renewer gives up on it, and every later one it answers with a lease, as a
// broker that serves again does. shim/agentsecrets_renewal_test.go's silentBroker answers none.
package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// w4Lease is the lease the stand-in answers a renewal with: `agent-secrets renew` renews again
// after a third of it (leasetime.RenewAfter), so a renewal every 30 s.
const w4Lease = 90 * time.Second

// w4Broker is the stand-in broker: its address, and every renewal that reached it.
type w4Broker struct {
	url string

	mu       sync.Mutex
	renewals []w4Renewal
	stopped  chan struct{}
}

// w4Renewal is one renewal as it reached the stand-in: when, for which enrollment, whether it was
// the one held unanswered, when it ended (its caller gave up, or it was answered), and whether it
// was answered.
type w4Renewal struct {
	at         time.Time
	enrollment string
	held       bool
	ended      time.Time
	answered   bool
}

func newW4Broker(t *testing.T) *w4Broker {
	t.Helper()
	b := &w4Broker{stopped: make(chan struct{})}
	server := httptest.NewServer(b)
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(b.stopped) })
	b.url = server.URL
	return b
}

func (b *w4Broker) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// The whole body is read before anything else, so the server reads the connection on and
	// ends the request's context the moment the renewer closes it.
	_, _ = io.Copy(io.Discard, req.Body)
	req.Body.Close()
	rest, prefixed := strings.CutPrefix(req.URL.Path, "/v1/enrollments/")
	enrollment, renewal := strings.CutSuffix(rest, "/renew")
	if req.Method != http.MethodPost || !prefixed || !renewal || enrollment == "" {
		http.NotFound(w, req)
		return
	}
	b.mu.Lock()
	held := len(b.renewals) == 0
	b.renewals = append(b.renewals, w4Renewal{at: time.Now(), enrollment: enrollment, held: held})
	index := len(b.renewals) - 1
	b.mu.Unlock()
	if held {
		select {
		case <-req.Context().Done():
		case <-b.stopped:
		}
		b.ended(index, false)
		closeUnanswered(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"lease_expires_at": time.Now().Add(w4Lease).UTC().Format(time.RFC3339Nano)})
	b.ended(index, true)
}

func (b *w4Broker) ended(index int, answered bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.renewals[index].ended = time.Now()
	b.renewals[index].answered = answered
}

// received is a copy of every renewal that reached the stand-in, oldest first.
func (b *w4Broker) received() []w4Renewal {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]w4Renewal(nil), b.renewals...)
}

// w4PodEnrollment is what the shim of the renewal's claim is started with: the stand-in's address
// and a key directory in its environment, and the three flags the Kubernetes runtime's launcher
// passes (internal/runtime/sandbox's launcher), with a projected token of the shape a pod holds.
type w4PodEnrollment struct {
	keyDir, tokenFile, binary string
}

// newW4PodEnrollment builds `agent-secrets` from the checkout under test's packages/envoy, the
// binary a pod's worker image carries beside the shim, and writes the key directory and a token.
// Its source is `cmd/cipher` since the broker's commands moved to their Cipher directories, and
// `cmd/agent-secrets` before, where the control's base still keeps it, so the command built is
// whichever of the two the checkout has.
func newW4PodEnrollment(t *testing.T, root string) w4PodEnrollment {
	t.Helper()
	dir := t.TempDir()
	e := w4PodEnrollment{keyDir: filepath.Join(dir, "keys"), tokenFile: filepath.Join(dir, "token"), binary: filepath.Join(dir, "agent-secrets")}
	if err := os.Mkdir(e.keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProofFile(t, e.tokenFile, "eyJhbGciOiJSUzI1NiJ9.e30.sig\n")
	envoy := filepath.Join(root, "packages", "envoy")
	command := "./cmd/cipher"
	if _, err := os.Stat(filepath.Join(envoy, "cmd", "cipher")); err != nil {
		command = "./cmd/agent-secrets"
	}
	build := exec.Command("go", "build", "-o", e.binary, command)
	build.Dir = envoy
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build %s in %s: %v\n%s", command, build.Dir, err, output)
	}
	return e
}

func (e w4PodEnrollment) env(brokerURL string) []string {
	return []string{"AGENT_SECRETS_URL=" + brokerURL, "AGENT_SECRETS_KEY_DIR=" + e.keyDir}
}

func (e w4PodEnrollment) args() []string {
	return []string{"--agent-secrets-key-dir", e.keyDir, "--pod-token-file", e.tokenFile, "--agent-secrets-bin", e.binary}
}

// w4ShimLines is every line of a pane's log naming the agent-secrets renewer, as the shim and the
// renewer wrote them.
func w4ShimLines(t *testing.T, logPath string) []string {
	t.Helper()
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("read the pane's log %s: %v", logPath, err)
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		if line := scanner.Text(); strings.Contains(line, "agent-secrets") {
			lines = append(lines, line)
		}
	}
	return lines
}
