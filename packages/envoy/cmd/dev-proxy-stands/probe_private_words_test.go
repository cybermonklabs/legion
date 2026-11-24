package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The child process of TestAPrivateProbePrintsNoWordOfItsProxy: what it runs (twin, net/http's own
// GET, or refused, probe-private), and the file it writes what that printed to.
const (
	privateProbeChildEnv = "DEV_PROXY_STANDS_PRIVATE_PROBE_CHILD"
	privateProbeOutEnv   = "DEV_PROXY_STANDS_PRIVATE_PROBE_OUT"
)

// TestAPrivateProbePrintsNoWordOfItsProxy: probe-private's GET of the private address goes through
// the proxy its environment names, as any Go program's does, and whatever answers there refuses
// the CONNECT with a reason phrase that quotes a URL with the probe's credentials
// (redacttest.StatusLineServer). net/http's error holds that phrase as it came (the twin);
// probe-private prints its failure by its class (refused). net/http reads the environment's proxy
// once per process (ProxyFromEnvironment), so each runs in a child process of this test binary,
// with the proxy in its environment and no NO_PROXY that covers the address. Captures
// dev-proxy-stands.probe-private-words/refused.
func TestAPrivateProbePrintsNoWordOfItsProxy(t *testing.T) {
	proxy := redacttest.StatusLineServer(t, http.StatusForbidden, "", "")
	printed := func(t *testing.T, what string) string {
		t.Helper()
		out := filepath.Join(t.TempDir(), "printed")
		child := exec.Command(os.Args[0], "-test.run=^TestAPrivateProbeChild$", "-test.count=1", "-test.v")
		child.Env = append(os.Environ(), "HTTPS_PROXY="+proxy, "https_proxy="+proxy, "NO_PROXY=none.invalid", "no_proxy=none.invalid",
			privateProbeChildEnv+"="+what, privateProbeOutEnv+"="+out)
		output, err := child.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--- PASS: TestAPrivateProbeChild (") {
			t.Fatalf("the child that runs %s did not pass (%v):\n%s", what, err, output)
		}
		text, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return string(text)
	}
	t.Run("twin", func(t *testing.T) {
		if text := printed(t, "twin"); !strings.Contains(text, redacttest.Token) {
			t.Fatalf("net/http's own error holds no word of the proxy's phrase, so the row cannot see one: %q", text)
		}
	})
	t.Run("refused", func(t *testing.T) {
		text := printed(t, "refused")
		redacttest.RequireNone(t, "dev-proxy-stands.probe-private-words/refused", text)
		if !strings.HasPrefix(text, "error after ") || !strings.HasSuffix(text, "ms: "+redact.ClassRequestFailed+"\n") {
			t.Errorf("probe-private printed %q, want the failure by its class", text)
		}
	})
}

// TestAPrivateProbeChild is the child process of TestAPrivateProbePrintsNoWordOfItsProxy, run only
// by it: it writes what its GET printed to the file the parent names.
func TestAPrivateProbeChild(t *testing.T) {
	what := os.Getenv(privateProbeChildEnv)
	if what == "" {
		t.Skip("a child process of TestAPrivateProbePrintsNoWordOfItsProxy, run only by it")
	}
	var text string
	switch what {
	case "twin":
		answer, err := (&http.Client{Timeout: 8 * time.Second}).Get(privateAddress)
		if err == nil {
			_ = answer.Body.Close()
			t.Fatalf("the proxy's refusal answered %s with no error", answer.Status)
		}
		text = fmt.Sprint(err)
	case "refused":
		var stdout, stderr strings.Builder
		if code := run(context.Background(), []string{"probe-private"}, &stdout, &stderr); code != 0 {
			t.Fatalf("dev-airlock-stands probe-private exited %d; stderr: %s", code, stderr.String())
		}
		text = stdout.String()
	default:
		t.Fatalf("no child case %q", what)
	}
	if err := os.WriteFile(os.Getenv(privateProbeOutEnv), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
