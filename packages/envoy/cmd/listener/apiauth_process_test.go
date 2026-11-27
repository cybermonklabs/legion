package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/testexec"
)

// A listener whose ENVOY_OIDC_ACCOUNTS names no account — a list emptied by mistake — refuses to
// start, naming the setting, rather than reading it as unset and admitting every pod of the
// cluster; the same listener with the setting empty starts, as one with it unset does. A refused
// start ends within refusedStartLimit of when the kernel made its process. The limit counts from
// that start, not from the call, so a loaded machine that starts the binary late kills no start
// that would have refused on its own; a start still going with 30 s left of the test binary's
// deadline is killed too.
func TestTheListenerBinaryRefusesAnAccountListThatNamesNoAccount(t *testing.T) {
	t.Parallel()
	issuer := oidctest.New(t)
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	for _, value := range []string{"   ", ", ,"} {
		ctx, cancel := context.WithTimeout(context.Background(), testwait.Remaining(t, 30*time.Second))
		cmd := exec.CommandContext(ctx, binary)
		cmd.Env = []string{
			"PORT=0", "ENVOY_LISTEN_HOST=127.0.0.1", "ENVOY_MACHINE_ID=oidc-accounts-blank", "NATS_URLS=" + uri,
			"ENVOY_OIDC_ISSUER=" + issuer.URL(), "ENVOY_OIDC_AUDIENCE=" + listenerTestAudience, "ENVOY_OIDC_ACCOUNTS=" + value,
		}
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		overran, err := testexec.RunFromStart(t, cmd, refusedStartLimit)
		cancel()
		if overran {
			t.Fatalf("ENVOY_OIDC_ACCOUNTS=%q: the start did not end within %s of when the kernel made its process and was killed, want exit 1 naming the setting:\n%s", value, refusedStartLimit, output.Bytes())
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(output.String(), "ENVOY_OIDC_ACCOUNTS is set but names no account") {
			t.Fatalf("ENVOY_OIDC_ACCOUNTS=%q: the start ended with %v, want exit 1 naming the setting:\n%s", value, err, output.Bytes())
		}
	}
	listener := startListenerProcess(t, binary, uri, "oidc-accounts-empty",
		"ENVOY_OIDC_ISSUER="+issuer.URL(), "ENVOY_OIDC_AUDIENCE="+listenerTestAudience, "ENVOY_OIDC_ACCOUNTS=")
	listener.waitHealthy(t)
}

// refusedStartLimit bounds a start that refuses its settings, from when the kernel made the
// listener's process to its exit: the listener reads its settings before it serves anything.
const refusedStartLimit = 30 * time.Second

// The settings reach the handler the listener binary serves, which the handler's own tests, each
// building its configuration by hand, cannot show: started with ENVOY_OIDC_ACCOUNTS beside a
// local issuer, the binary admits the listed subject's projected token and refuses another's, and
// its own /metrics counts each admission under its credential and route. Started with no list,
// the same binary admits both, as it did before the list existed.
func TestTheListenerBinaryHoldsProjectedTokensToItsListAndCountsThem(t *testing.T) {
	t.Parallel()
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
			if status := call(listenerTestToken); status != http.StatusOK {
				t.Fatalf("the shared token: status %d, want 200", status)
			}

			_, metrics := do(t, mustRequest(t, http.MethodGet, base+"/metrics"))
			for _, want := range []string{
				`envoy_listener_requests_total{credential="oidc",path="/v1/sessions"} ` + strconv.Itoa(tc.wantOIDC),
				`envoy_listener_requests_total{credential="shared_token",path="/v1/sessions"} 1`,
			} {
				if !strings.Contains(metrics, want+"\n") {
					t.Fatalf("/metrics does not carry %s:\n%s", want, metrics)
				}
			}
		})
	}
}
