package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// credentialURL is a URL on host with redacttest's markers in its user information and in its
// query, as a deployment can write a setting that dev sign-in reads.
func credentialURL(host string) string {
	return "http://" + redacttest.User + ":" + redacttest.Password + "@" + host + "/api?access" + "_token=" + redacttest.Token
}

// TestADevSignInLineNamesNoCredentialAURLItReadsCarries: dev sign-in reads two URLs a deployment
// can write with a credential in its user information or its query, and each line or refusal that
// names one names it as the URL rule names a configured URL (redact.URL), never as written:
//   - api-base: the refusal of an App key that would sign calls to a DISPATCH_GITHUB_API_BASE
//     on another host than this machine's (devSignInLoadedFence);
//   - origin: the warning a dev sign-in server starts with, which names the dashboard origin,
//     envoy.json's dispatch.serverUrl, held only to an http(s) URL with a host and, under the flag,
//     to a loopback host (routes.DevSignInOrigin), so its user information and query pass. The
//     warning is written by main, so the row runs this test binary as the server (TestMain's
//     DISPATCH_MEMORY_TEST_SERVE) and reads its standard error up to the warning.
//
// The twin shows the fence takes the origin and the URLs as written hold the markers. Captures
// dispatch.dev-signin-url/<case>.
func TestADevSignInLineNamesNoCredentialAURLItReadsCarries(t *testing.T) {
	origin := strings.Replace(credentialURL("127.0.0.1:8799"), "/api", "", 1)
	t.Run("twin", func(t *testing.T) {
		for _, raw := range []string{credentialURL("github.example"), origin} {
			if held := redacttest.Held(raw); len(held) != len(redacttest.Secrets) {
				t.Errorf("%q holds %v; want every marker, so the rows can see it printed", raw, held)
			}
		}
		if err := devSignInLoadedFence(devSignInBoot(t, nil), origin, nil, appCredentialSource{}); err != nil {
			t.Errorf("the fence refused the origin %q: %v; want it taken, so the warning names it", origin, err)
		}
	})
	t.Run("api-base", func(t *testing.T) {
		key := &auth.AppConfig{ClientID: "Iv1.app", PEM: "app private key"}
		err := devSignInLoadedFence(devSignInBoot(t, map[string]string{"DISPATCH_GITHUB_API_BASE": credentialURL("github.example")}), devSignInOrigin, key, appCredentialSource{})
		if err == nil {
			t.Fatal("the fence took an App key signing calls to a remote base; want it refused")
		}
		redacttest.RequireNoPiece(t, "dispatch.dev-signin-url/api-base", err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
		if !strings.Contains(err.Error(), "DISPATCH_GITHUB_API_BASE=") || !strings.Contains(err.Error(), "DISPATCH_APP_PEM_B64") {
			t.Errorf("the refusal is %q; want it to name DISPATCH_GITHUB_API_BASE and DISPATCH_APP_PEM_B64", err)
		}
	})
	t.Run("origin", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, "config", "legion"), 0o700); err != nil {
			t.Fatal(err)
		}
		envoyJSON := `{"dispatch":{"serverUrl":"` + origin + `"}}`
		if err := os.WriteFile(filepath.Join(home, "config", "legion", "envoy.json"), []byte(envoyJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		server := exec.CommandContext(ctx, os.Args[0])
		server.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
			memoryTestServeEnv + "=1",
			// No database answers on port 1: the server stops once its boot dials it, after the
			// warning.
			"DATABASE_URL=postgres://postgres:x@127.0.0.1:1/dispatch_e2e?sslmode=disable",
			"DISPATCH_AGENT_TOKEN=x",
			"DISPATCH_LISTEN_HOST=127.0.0.1",
			"DISPATCH_DEV_SIGNIN=1",
			"DISPATCH_NATS_DISABLED=1",
		}
		said, _ := server.CombinedOutput()
		const warning = "dispatch: dev sign-in mounted"
		var line string
		for _, l := range strings.Split(string(said), "\n") {
			if strings.Contains(l, warning) {
				line = l
				break
			}
		}
		if line == "" {
			t.Fatalf("the server wrote no %q line; its output:\n%s", warning, said)
		}
		redacttest.RequireNoPiece(t, "dispatch.dev-signin-url/origin", line, redacttest.User, redacttest.Password, redacttest.Token)
		if want := "origin="; !strings.Contains(line, want) {
			t.Errorf("the warning is %q; want it to name the origin (%q)", line, want)
		}
		if !strings.Contains(line, redact.NotPrinted) {
			t.Errorf("the warning is %q; want the origin as redact.URL names it, %q", line, redact.NotPrinted)
		}
	})
}
