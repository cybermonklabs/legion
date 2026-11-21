package gcp_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/testport"
)

// listenerLoopback is the address the session listener's probes ask, on the pod's own loopback.
const listenerLoopback = "http://127.0.0.1:9020"

// The session listener's startup probe passes only on a scrape that holds envoy_active_interests'
// sample line, which the listener writes only once its /v1 gate is open and its interest list can
// be served. The rendered script runs under the shell shellCommand names, with an httptest
// server's URL in place of the pod's loopback. It exits 0 for a /metrics body holding the sample
// line, and 1, grep's status when it selects no line, for a body holding only the gauge's "# HELP"
// and "# TYPE" lines, which every scrape carries, for a 503 whose body holds the sample line, so
// the status alone fails it, and for no server at all. Each server counts the scrapes it answered,
// so a run that never reached it cannot stand for one that did. The test needs curl and grep on
// this machine, as the listener's image carries them, and fails, never skips, without one.
func TestClusteredNatsHubListenerStartupProbePassesOnlyOnTheSample(t *testing.T) {
	for _, tool := range []string{"curl", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH; the listener's image carries it, and this test fails rather than skips", tool)
		}
	}
	template := deploymentPodTemplate(t, runClusteredHub(t, clusteredArgs()), clusteredListenerName)
	probe := podContainer(t, template, "listener")["startupProbe"].(map[string]any)
	command := probe["exec"].(map[string]any)["command"].([]any)
	require.Len(t, command, 3, "the startup probe is a shell's -c of one script")
	require.Equal(t, []any{"/bin/sh", "-c"}, command[:2], "the startup probe runs its script under /bin/sh")
	script := command[2].(string)
	require.Equal(t, 1, strings.Count(script, listenerLoopback), "the script names the listener's loopback once: %s", script)

	described := "# HELP envoy_active_interests Number of active interest subscriptions\n# TYPE envoy_active_interests gauge\n"
	sampled := described + "envoy_active_interests 3\n"
	for _, row := range []struct {
		name   string
		status int
		body   string
		exit   int
	}{
		{"the sample line", http.StatusOK, sampled, 0},
		{"only the HELP and TYPE lines", http.StatusOK, described, 1},
		{"a 503", http.StatusServiceUnavailable, sampled, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			var scrapes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/metrics" {
					http.NotFound(w, r)
					return
				}
				scrapes.Add(1)
				w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
				w.WriteHeader(row.status)
				fmt.Fprint(w, row.body)
			}))
			defer server.Close()
			exit, stderr := runStartupScript(t, strings.ReplaceAll(script, listenerLoopback, server.URL))
			require.Equal(t, row.exit, exit, "the script's exit for %s (stderr %q)", row.name, stderr)
			require.Equal(t, int32(1), scrapes.Load(), "the script scraped the server's /metrics once")
		})
	}
	t.Run("no server", func(t *testing.T) {
		// A port the test holds and nothing listens on, so curl's dial is refused and no other
		// process can take the port and answer the scrape (testport.Refused).
		closed := "http://" + testport.Refused(t)
		exit, stderr := runStartupScript(t, strings.ReplaceAll(script, listenerLoopback, closed))
		require.Equal(t, 1, exit, "the script's exit with nothing listening at %s (stderr %q)", closed, stderr)
	})
}

// runStartupScript runs script under the shell shellCommand names, with this machine's PATH alone
// in its environment, so no proxy setting reaches curl, and returns its exit status and what it
// wrote to stderr. A shell that does not start fails the test.
func runStartupScript(t *testing.T, script string) (int, string) {
	t.Helper()
	cmd := shellCommand(t, "-c", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), stderr.String()
	default:
		t.Fatalf("run the startup probe's script: %v", err)
		return 0, ""
	}
}
