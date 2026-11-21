package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
)

// A listener whose ENVOY_OIDC_ACCOUNTS names no account — a list emptied by mistake — refuses to
// start, naming the setting, rather than reading it as unset and admitting every pod of the
// cluster; the same listener with the setting empty starts, as one with it unset does.
func TestTheListenerBinaryRefusesAnAccountListThatNamesNoAccount(t *testing.T) {
	issuer := oidctest.New(t)
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	for _, value := range []string{"   ", ", ,"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, binary)
		cmd.Env = []string{
			"PORT=0", "ENVOY_LISTEN_HOST=127.0.0.1", "ENVOY_MACHINE_ID=oidc-accounts-blank", "NATS_URLS=" + uri,
			"ENVOY_OIDC_ISSUER=" + issuer.URL(), "ENVOY_OIDC_AUDIENCE=" + listenerTestAudience, "ENVOY_OIDC_ACCOUNTS=" + value,
		}
		output, err := cmd.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "ENVOY_OIDC_ACCOUNTS is set but names no account") {
			t.Fatalf("ENVOY_OIDC_ACCOUNTS=%q: the start ended with %v, want exit 1 naming the setting:\n%s", value, err, output)
		}
	}
	listener := startListenerProcess(t, binary, uri, "oidc-accounts-empty",
		"ENVOY_OIDC_ISSUER="+issuer.URL(), "ENVOY_OIDC_AUDIENCE="+listenerTestAudience, "ENVOY_OIDC_ACCOUNTS=")
	listener.waitHealthy(t)
}

// The settings reach the handler the listener binary serves, which the handler's own tests, each
// building its configuration by hand, cannot show: started with ENVOY_OIDC_ACCOUNTS beside a
// local issuer, the binary admits the listed subject's projected token and refuses another's, and
// its own /metrics counts each admission under its credential and route. Started with no list,
// the same binary admits both, as it did before the list existed.
func TestTheListenerBinaryHoldsProjectedTokensToItsListAndCountsThem(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "listener-process-key")
	listed := issuer.Mint(t, key, issuer.Claims(listenerListedSubject, listenerTestAudience))
	unlisted := issuer.Mint(t, key, issuer.Claims(listenerUnlistedSubject, listenerTestAudience))
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	pair := []string{"ENVOY_OIDC_ISSUER=" + issuer.URL(), "ENVOY_OIDC_AUDIENCE=" + listenerTestAudience}

	for _, tc := range []struct {
		name, machineID string
		env             []string
		wantUnlisted    int
		wantOIDC        int
	}{
		{name: "held to ENVOY_OIDC_ACCOUNTS", machineID: "oidc-accounts-held", env: append([]string{"ENVOY_OIDC_ACCOUNTS=" + listenerListedSubject}, pair...),
			wantUnlisted: http.StatusUnauthorized, wantOIDC: 1},
		// Written with commas, the list holds the listed subject itself, not "<subject>,", so its
		// caller is admitted.
		{name: "held to a list written with commas", machineID: "oidc-accounts-commas",
			env:          append([]string{"ENVOY_OIDC_ACCOUNTS=" + listenerListedSubject + ", system:serviceaccount:example:other"}, pair...),
			wantUnlisted: http.StatusUnauthorized, wantOIDC: 1},
		{name: "with no list, as before", machineID: "oidc-accounts-unset", env: pair,
			wantUnlisted: http.StatusOK, wantOIDC: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := startListenerProcess(t, binary, uri, tc.machineID, tc.env...)
			listener.waitHealthy(t)
			base := "http://127.0.0.1:" + strconv.Itoa(listener.port)
			call := func(bearer string) int {
				t.Helper()
				request := mustRequest(t, http.MethodGet, base+"/v1/sessions")
				request.Header.Set("Authorization", "Bearer "+bearer)
				status, _ := do(t, request)
				return status
			}

			if status := call(listed); status != http.StatusOK {
				t.Fatalf("the listed subject's token: status %d, want 200\n%s", status, listener.Output.String())
			}
			if status := call(unlisted); status != tc.wantUnlisted {
				t.Fatalf("the unlisted subject's token: status %d, want %d\n%s", status, tc.wantUnlisted, listener.Output.String())
			}

			_, metrics := do(t, mustRequest(t, http.MethodGet, base+"/metrics"))
			if want := `envoy_listener_requests_total{credential="oidc",path="/v1/sessions"} ` + strconv.Itoa(tc.wantOIDC); !strings.Contains(metrics, want+"\n") {
				t.Fatalf("/metrics does not carry %s:\n%s", want, metrics)
			}
		})
	}
}

// The listener takes no shared token and serves no open /v1 off loopback, so the three settings
// that asked for either refuse its start, each naming the variable and what replaces it: a
// non-empty ENVOY_API_TOKEN names the credentials a caller presents instead, and a non-empty
// ENVOY_API_ALLOW_UNAUTHENTICATED or ENVOY_V1_OPEN_WITH_TOKEN_CHECKS names the loopback bind that
// is the only open /v1 left and the credentials an off-loopback listener verifies. The listener
// here binds loopback, where it would otherwise start, so the refusal is the setting's and not the
// bind's. All three set empty, as a render that writes a variable it no longer fills does, the
// same listener starts.
func TestSharedTokenVariablesRefuseTheStart(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, tc := range []struct {
		setting string
		names   []string
	}{
		{setting: "ENVOY_API_TOKEN=1",
			names: []string{"ENVOY_API_TOKEN", "ENVOY_OIDC_ISSUER", "ENVOY_OIDC_GOOGLE_AUDIENCE", "ENVOY_AGENT_SECRETS_URL"}},
		{setting: "ENVOY_API_ALLOW_UNAUTHENTICATED=1",
			names: []string{"ENVOY_API_ALLOW_UNAUTHENTICATED", "loopback", "ENVOY_OIDC_ISSUER", "ENVOY_OIDC_GOOGLE_AUDIENCE", "ENVOY_AGENT_SECRETS_URL"}},
		{setting: "ENVOY_V1_OPEN_WITH_TOKEN_CHECKS=true",
			names: []string{"ENVOY_V1_OPEN_WITH_TOKEN_CHECKS", "loopback", "ENVOY_OIDC_ISSUER", "ENVOY_OIDC_GOOGLE_AUDIENCE", "ENVOY_AGENT_SECRETS_URL"}},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			cmd := exec.Command(binary)
			cmd.Env = []string{
				"PORT=0", "ENVOY_LISTEN_HOST=127.0.0.1", "ENVOY_MACHINE_ID=shared-token-refused",
				"NATS_URLS=nats://127.0.0.1:1", tc.setting,
			}
			output := refusedStart(t, cmd).Output.String()
			for _, name := range tc.names {
				if !strings.Contains(output, name) {
					t.Errorf("%s: the refusal does not name %s:\n%s", tc.setting, name, output)
				}
			}
		})
	}
	t.Run("all set empty", func(t *testing.T) {
		startListenerProcess(t, binary, testnats.URL(t), "shared-token-empty",
			"ENVOY_API_TOKEN=", "ENVOY_API_ALLOW_UNAUTHENTICATED=", "ENVOY_V1_OPEN_WITH_TOKEN_CHECKS=").waitHealthy(t)
	})
}

// The open /v1 is a loopback listener's alone. On loopback a listener with no credential serves a
// request that presents none, and so does one with its broker settings, which verifies a broker
// token when one is presented and still serves a bare request. Off loopback, bound by
// ENVOY_LISTEN_HOST or on an inherited ENVOY_LISTEN_FD socket, a listener whose broker settings are
// its only credential starts, and answers a request that presents nothing 401. Each listener is the
// binary as a deployment starts it, so the bind main reads is the one apiAuth judges.
//
// Control: requiresCredential as before this change (the bearer kinds alone); the off-loopback
// listener serves the bare request, and the test fails naming the status.
func TestOpenV1OnlyOnLoopback(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	accountFile := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountFile, []byte("listener-account-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The broker is never asked: no request below presents a broker token.
	brokerSettings := []string{
		"ENVOY_AGENT_SECRETS_URL=http://" + testport.Refused(t),
		"ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=" + accountFile,
		"ENVOY_PUBLIC_URL=http://envoy-session-listener.example:9020",
	}
	bare := func(t *testing.T, port int) int {
		t.Helper()
		status, _ := do(t, mustRequest(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/sessions"))
		return status
	}
	// offLoopback waits for a listener started off loopback to log its address and to report
	// healthy at loopback on that port, which a socket on every interface answers, and answers it.
	offLoopback := func(t *testing.T, process *cmdtest.Process) *listenerProcess {
		t.Helper()
		addr := listeningAddr(t, process)
		host, portText, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("the listener logged that it listens on %q, which is no address: %v", addr, err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || isLoopbackHost(host) {
			t.Fatalf("the listener logged that it listens on %q, want an address off loopback:\n%s", addr, process.Output.String())
		}
		listener := &listenerProcess{Process: process, port: port}
		listener.waitHealthz(t)
		return listener
	}

	for _, tc := range []struct {
		name  string
		start func(t *testing.T) *listenerProcess
		want  int
	}{
		{name: "on loopback with no credential, a bare request is served", want: http.StatusOK,
			start: func(t *testing.T) *listenerProcess {
				listener := startListenerProcess(t, binary, uri, "open-v1-loopback")
				listener.waitHealthy(t)
				return listener
			}},
		{name: "on loopback with the broker settings, a bare request is still served", want: http.StatusOK,
			start: func(t *testing.T) *listenerProcess {
				listener := startListenerProcess(t, binary, uri, "open-v1-loopback-broker", brokerSettings...)
				listener.waitHealthy(t)
				return listener
			}},
		{name: "off loopback by ENVOY_LISTEN_HOST with the broker settings alone, a bare request answers 401", want: http.StatusUnauthorized,
			start: func(t *testing.T) *listenerProcess {
				cmd := exec.Command(binary)
				cmd.Env = append([]string{"PORT=0", "ENVOY_LISTEN_HOST=0.0.0.0", "ENVOY_MACHINE_ID=open-v1-wildcard-host", "NATS_URLS=" + uri}, brokerSettings...)
				return offLoopback(t, cmdtest.Start(t, "the listener", cmd))
			}},
		{name: "off loopback on an inherited ENVOY_LISTEN_FD socket with the broker settings alone, a bare request answers 401", want: http.StatusUnauthorized,
			start: func(t *testing.T) *listenerProcess {
				wildcard, err := net.Listen("tcp4", "0.0.0.0:0")
				if err != nil {
					t.Fatalf("listen on every interface: %v", err)
				}
				socket, err := wildcard.(*net.TCPListener).File()
				_ = wildcard.Close()
				if err != nil {
					t.Fatalf("take the listening socket's descriptor: %v", err)
				}
				t.Cleanup(func() { _ = socket.Close() })
				cmd := exec.Command(binary)
				cmd.Env = append([]string{"ENVOY_MACHINE_ID=open-v1-wildcard-fd", "NATS_URLS=" + uri, "ENVOY_LISTEN_FD=3"}, brokerSettings...)
				cmd.ExtraFiles = []*os.File{socket}
				return offLoopback(t, cmdtest.Start(t, "the listener", cmd))
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := tc.start(t)
			if status := bare(t, listener.port); status != tc.want {
				t.Fatalf("GET /v1/sessions with no credential: status %d, want %d\n%s", status, tc.want, listener.Output.String())
			}
		})
	}
}
