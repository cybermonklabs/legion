// result_grant_test.go drives `agent-secrets result-grant -- COMMAND` as a GitHub job runs it: the
// built binary, a stand-in for the runner's OIDC token endpoint, and a stand-in broker whose token
// endpoint answers the run's trade. The child is `env`, which prints the environment the command
// was given.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/oauthwire"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/dpopproof/dpopprooftest"
)

// The grant the stand-in broker answers: its placeholder, its id and the one host its entry binds.
const (
	resultPlaceholder = "agent-secrets-placeholder-result-QqRrSsTtUuVvWwXxYyZz0123456789ab"
	resultGrantID     = "result:00000000-0000-4000-8000-000000000001"
	resultHost        = "dispatch.example.test"
)

// runnerRequestToken is the bearer the stand-in runner hands the job in ACTIONS_ID_TOKEN_REQUEST_TOKEN
// and wants back on its token request: a value of this test's own, built rather than written out so
// that no line reads as a credential.
var runnerRequestToken = strings.Repeat("r", 24)

// resultBroker is the stand-in broker: what each trade sent, and the answer it gives.
type resultBroker struct {
	*httptest.Server
	mu     sync.Mutex
	forms  []url.Values
	proofs []string
}

func startResultBroker(t *testing.T, status int, answer map[string]any) *resultBroker {
	t.Helper()
	b := &resultBroker{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/oauth/token" || r.ParseForm() != nil {
			http.Error(w, "the stand-in broker answers its token endpoint alone", http.StatusNotFound)
			return
		}
		b.mu.Lock()
		b.forms = append(b.forms, r.PostForm)
		b.proofs = append(b.proofs, r.Header.Get("DPoP"))
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(b.Close)
	return b
}

// placeholderGrant is the token endpoint's answer to a result entry's run (C8), with an airlock
// authority whose certificate the command's trust bundle can hold, its entry binding hosts, or
// resultHost alone when none is given.
func placeholderGrant(t *testing.T, hosts ...string) map[string]any {
	t.Helper()
	if len(hosts) == 0 {
		hosts = []string{resultHost}
	}
	authority, err := proxyrulestest.NewAuthority("result-grant test")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"access_token": resultPlaceholder, "issued_token_type": oauthwire.TokenTypeAirlockPlaceholder, "token_type": "N_A", "expires_in": 900,
		oauthwire.ExtraGrantID: resultGrantID,
		oauthwire.ExtraAirlock: oauthwire.AirlockRelease{
			ProxyURL: "http://127.0.0.1:13381", CACertificate: string(authority.CertificatePEM()), ProxyPassword: resultPlaceholder,
			Names: map[string]oauthwire.AirlockName{oauthwire.AirlockResultsName: {Value: resultPlaceholder, Slots: map[string]string{"value": resultPlaceholder}, Hosts: hosts}},
		},
	}
}

// runnerTokenEndpoint is the stand-in for the runner's OIDC token endpoint, which answers the
// bearer it gave the job with a run token. The scheme is read in any letter case, as HTTP reads
// it: GitHub's own example spells it `bearer`, as the client sends it.
func runnerTokenEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || token != runnerRequestToken {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "the-runs-oidc-token"})
	}))
	t.Cleanup(server.Close)
	return server
}

// runResultGrant runs the built `agent-secrets result-grant -- env` against broker as a GitHub
// job, with extra on top, and answers its stdout, its stderr and its exit status.
func runResultGrant(t *testing.T, broker *resultBroker, extra ...string) (string, string, int) {
	t.Helper()
	runner := runnerTokenEndpoint(t)
	command := exec.Command(buildAgentSecrets(t), "result-grant", "--", "env")
	command.Env = append([]string{
		"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "XDG_RUNTIME_DIR=" + t.TempDir(),
		"AGENT_SECRETS_URL=" + broker.URL, "AGENT_SECRETS_MACHINE_IDENTITY=github-actions",
		"AGENT_SECRETS_MACHINE_AUDIENCE=example-broker", "ACTIONS_ID_TOKEN_REQUEST_URL=" + runner.URL,
		cipherclient.ActionsIDTokenRequestTokenVar + "=" + runnerRequestToken, "DISPATCH_URL=https://" + resultHost,
	}, extra...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0
	case errors.As(err, &exit):
		return stdout.String(), stderr.String(), exit.ExitCode()
	default:
		t.Fatalf("run agent-secrets result-grant: %v", err)
		return "", "", 0
	}
}

// childVar is the value the child's environment, as `env` printed it, gives name, and whether
// it gives one.
func childVar(stdout, name string) (string, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		if value, ok := strings.CutPrefix(line, name+"="); ok {
			return value, true
		}
	}
	return "", false
}

// TestResultGrantRunsTheCommandWithThePlaceholderAndTheProxy: the run's token is traded at the
// broker's token endpoint for the airlock placeholder, with a DPoP proof the endpoint takes; the
// command runs with the placeholder in DISPATCH_RESULTS_TOKEN, the proxy in HTTPS_PROXY with the
// placeholder as its password, and the trust bundle, and with no machine identity of its own to
// trade.
func TestResultGrantRunsTheCommandWithThePlaceholderAndTheProxy(t *testing.T) {
	broker := startResultBroker(t, http.StatusOK, placeholderGrant(t))
	stdout, stderr, exit := runResultGrant(t, broker)
	if exit != 0 {
		t.Fatalf("result-grant exited %d: %s", exit, stderr)
	}
	broker.mu.Lock()
	forms, proofs := broker.forms, broker.proofs
	broker.mu.Unlock()
	if len(forms) != 1 || forms[0].Get("requested_token_type") != oauthwire.TokenTypeAirlockPlaceholder ||
		forms[0].Get("subject_token") != "the-runs-oidc-token" || forms[0].Get("subject_token_type") != oauthwire.SubjectTokenTypeGitHub {
		t.Fatalf("the broker's token endpoint got %v; want one trade of the run's token for the placeholder type", forms)
	}
	// The key is made in the command's memory and never leaves it, so the test cannot name it.
	if why, _ := dpopprooftest.TradeRefusal(proofs[0], broker.URL+cipherclient.TokenPath); why != "" {
		t.Fatalf("the trade's DPoP proof %q is not one the token endpoint takes: %s", proofs[0], why)
	}
	if got, _ := childVar(stdout, oauthwire.AirlockResultsName); got != resultPlaceholder {
		t.Errorf("the child's %s = %q, want the placeholder", oauthwire.AirlockResultsName, got)
	}
	proxy, _ := childVar(stdout, "HTTPS_PROXY")
	parsed, err := url.Parse(proxy)
	if err != nil || parsed.Host != "127.0.0.1:13381" {
		t.Fatalf("the child's HTTPS_PROXY = %q, want the airlock proxy", proxy)
	}
	if password, _ := parsed.User.Password(); password != resultPlaceholder {
		t.Errorf("the child's HTTPS_PROXY password is not the placeholder")
	}
	if bundle, ok := childVar(stdout, "SSL_CERT_FILE"); !ok || bundle == "" {
		t.Errorf("the child has no trust bundle in SSL_CERT_FILE")
	}
	if _, ok := childVar(stdout, "AGENT_SECRETS_MACHINE_IDENTITY"); ok {
		t.Errorf("the child holds AGENT_SECRETS_MACHINE_IDENTITY, so it could trade the run's identity itself")
	}
}

// TestResultGrantRefusesADispatchURLOutsideTheGrant: a DISPATCH_URL whose host the grant does not
// bind, or that is not https on port 443, would send the command's writes around the proxy with the
// placeholder in them (the child's HTTPS_PROXY covers https alone, and the proxy reads a bound host
// on 443 alone), so nothing runs. A host is bound as the proxy binds one: exactly, or one label or
// more under a `*.` entry, so `evilexample.test` is not under `*.example.test`, and
// `dispatch.example.test` is, which runs.
func TestResultGrantRefusesADispatchURLOutsideTheGrant(t *testing.T) {
	for _, c := range []struct {
		name, dispatchURL, host string
		bound                   []string
	}{
		{"another host", "https://dispatch.elsewhere.test", "dispatch.elsewhere.test", nil},
		{"a name that ends in a wildcard entry's domain without its dot", "https://evilexample.test", "evilexample.test", []string{"*.example.test"}},
		{"the bound host over plain http", "http://" + resultHost, resultHost, nil},
		{"the bound host over plain http on 443", "http://" + resultHost + ":443", resultHost, nil},
		{"the bound host on another port", "https://" + resultHost + ":8443", resultHost, nil},
	} {
		broker := startResultBroker(t, http.StatusOK, placeholderGrant(t, c.bound...))
		stdout, stderr, exit := runResultGrant(t, broker, "DISPATCH_URL="+c.dispatchURL)
		if exit == 0 || strings.Contains(stdout, resultPlaceholder) || !strings.Contains(stderr, c.host) || !strings.Contains(stderr, "around") {
			t.Errorf("%s: result-grant with DISPATCH_URL=%s = exit %d, stdout %q, stderr %q; want a refusal naming the host, and nothing run", c.name, c.dispatchURL, exit, stdout, stderr)
		}
	}
	broker := startResultBroker(t, http.StatusOK, placeholderGrant(t, "*.example.test"))
	if stdout, stderr, exit := runResultGrant(t, broker, "DISPATCH_URL=https://"+resultHost); exit != 0 || !strings.Contains(stdout, oauthwire.AirlockResultsName+"=") {
		t.Errorf("result-grant with DISPATCH_URL=https://%s under a grant bound to *.example.test = exit %d (%s); want the command run", resultHost, exit, stderr)
	}
}

// TestResultGrantMasksThePlaceholderInAGitHubJob: in a GitHub job the placeholder is the proxy's
// password, which a proxy's error can quote, so the first thing printed masks it in the job's log.
func TestResultGrantMasksThePlaceholderInAGitHubJob(t *testing.T) {
	broker := startResultBroker(t, http.StatusOK, placeholderGrant(t))
	stdout, stderr, exit := runResultGrant(t, broker, "GITHUB_ACTIONS=true")
	if exit != 0 || !strings.HasPrefix(stdout, "::add-mask::"+resultPlaceholder+"\n") {
		t.Fatalf("result-grant in a GitHub job = exit %d, stdout %q (%s); want it to start ::add-mask::%s", exit, stdout, stderr, resultPlaceholder)
	}
}

// TestABrokerRefusalRunsNothing: the broker's refusal of the trade is printed, the command never
// runs, and result-grant exits 1. The refusal quotes the run's token it was sent, as a broker's
// error may, and the line printed holds [redacted] in its place and never the token.
func TestABrokerRefusalRunsNothing(t *testing.T) {
	broker := startResultBroker(t, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": "no result entry lists the run of subject token the-runs-oidc-token"})
	stdout, stderr, exit := runResultGrant(t, broker)
	if exit != 1 || strings.Contains(stdout, "PATH=") || !strings.Contains(stderr, "invalid_request") {
		t.Fatalf("result-grant refused by the broker = exit %d, stdout %q, stderr %q; want exit 1, nothing run and the refusal printed", exit, stdout, stderr)
	}
	if strings.Contains(stderr, "the-runs-oidc-token") || !strings.Contains(stderr, "subject token [redacted]") {
		t.Fatalf("the refusal printed = %q; want the run's token as [redacted], and never the token", stderr)
	}
}
