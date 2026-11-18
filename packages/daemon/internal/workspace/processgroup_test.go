package workspace

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// hangingServer accepts an HTTPS connection and never writes a byte of response: git's
// git-remote-https helper sits forever waiting for a reply unless something outside curl's own
// retry logic ends it. asked receives once the first request has reached the server, which is
// that helper up and waiting.
func hangingServer(t *testing.T) (server *httptest.Server, asked <-chan struct{}) {
	t.Helper()
	reached := make(chan struct{}, 1)
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	t.Cleanup(func() {
		// Close alone blocks until every in-flight handler returns, which a connection this
		// server never answers would never do on its own: sever it first.
		server.CloseClientConnections()
		server.Close()
	})
	return server, reached
}

// trustHangingServerCA makes the fetch's own clone (which reads no config file, GIT_CONFIG_GLOBAL
// and GIT_CONFIG_NOSYSTEM both pinned) trust server's self-signed certificate: GIT_SSL_CAINFO is
// an environment variable git's http transport reads directly, independent of any config file.
func trustHangingServerCA(t *testing.T, server *httptest.Server) {
	t.Helper()
	caFile := filepath.Join(t.TempDir(), "server-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
}

// hangingRemoteRunner retargets the fetch's clone at a local server, through the production
// runner (recordingRunner's), against a local TLS server that accepts a connection and never
// answers.
type hangingRemoteRunner struct {
	remote string
	run    Runner
}

func (r *hangingRemoteRunner) Timeout() time.Duration { return r.run.Timeout() }

func (r *hangingRemoteRunner) Run(ctx context.Context, command Command) (Result, error) {
	if commandWith(command.Argv, "git", "clone", "--bare", "--quiet", "https://github.com/acme/widgets") {
		rewritten := append([]string(nil), command.Argv...)
		for i, arg := range rewritten {
			if arg == "https://github.com/acme/widgets" {
				rewritten[i] = r.remote
			}
		}
		command.Argv = rewritten
	}
	return r.run.Run(ctx, command)
}

// git's https transport spawns git-remote-https, a helper holding the same stdout/stderr pipes as
// the git process exec.CommandContext starts directly: killing only that direct child when the
// deadline fires leaves the helper, now reparented, holding the pipe open, and Wait (so Run, so
// Fetch) never returns. Against a server that accepts the connection and never answers, the
// clone's timeout, on a fake clock that the row moves to it once the helper's request has reached
// the server and never past the grace that follows, must still end Fetch, with an error naming the
// timeout, and no git process of this test still running.
func TestFetchKillsGitsWholeProcessTreeWhenItsTimeoutFires(t *testing.T) {
	run := newLocalRunner(t)
	clock := clocktest.New(stopMoment)
	run.runner = execRunner{timeout: run.timeout, tools: testTools(t), clock: clock}
	server, asked := hangingServer(t)
	trustHangingServerCA(t, server)
	req := fetchRequest(t)

	fetched := inFlight(func() error {
		_, err := Fetch(context.Background(), &hangingRemoteRunner{remote: server.URL + "/widgets.git", run: run}, req)
		return err
	})
	testwait.Receive(t, "the clone's git-remote-https to reach the server", asked)
	// The clone is the one command running, so its timeout is the one deadline armed.
	armed := clock.AwaitArmed(t, "the clone's timeout", 1)
	clock.Advance(armed[0].Sub(clock.Now()))
	err := testwait.Receive(t, "Fetch to return once the clone's timeout stopped its whole process group", fetched)

	if err == nil {
		t.Fatal("Fetch: no error, want the clone's own timeout to end it")
	}
	if !strings.Contains(err.Error(), "command timed out") {
		t.Errorf("Fetch error = %q, want it to name the timeout (\"command timed out\")", err)
	}
	if leftover := gitProcessesUnder(t, server.URL); len(leftover) != 0 {
		t.Errorf("git processes still running after Fetch returned: %v", leftover)
	}
}

// gitProcessesUnder scans /proc for a process whose command line names marker (server.URL, the
// one local address the fetch's clone and git-remote-https both reach): a git or git-remote-https
// process orphaned by a timeout that killed only its parent would still show up this way. A
// request's own CredentialDir never appears in any git argv — the credential helper reaches git
// through GIT_CONFIG_VALUE_1 (clone.go), not a path — so scanning for it finds nothing whether or
// not a process leaked.
func gitProcessesUnder(t *testing.T, marker string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("/proc unavailable: %v", err)
	}
	var found []string
	for _, entry := range entries {
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		joined := strings.Join(args, " ")
		if strings.Contains(joined, marker) {
			found = append(found, fmt.Sprintf("pid %s: %s", entry.Name(), joined))
		}
	}
	return found
}
