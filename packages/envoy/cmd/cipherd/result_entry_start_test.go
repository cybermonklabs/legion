// result_entry_start_test.go drives the REAL compiled broker's start check of the results profile
// DISPATCH_RESULTS_TOKEN (config.ResultsEntryRules, beside config.ProxyMintedRules, the check of
// every dpop block) on a broker with no result entries of its own: such a broker still serves
// every result grant of the database it shares, so it is held to the same profile as one with
// result entries. The helpers are result_write_test.go's and main_test.go's.
package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/proxyrules/proxyrulestest"
)

// otherResultsHost is a host the misconfigured profiles below bind beside, or in place of,
// resultsBoundHost: a name under the same parent, which a CI step could own.
const otherResultsHost = "evil.ci-results.test"

// withoutSettings is env with every entry of the named settings taken out.
func withoutSettings(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if name, _, _ := strings.Cut(entry, "="); !slices.Contains(names, name) {
			out = append(out, entry)
		}
	}
	return out
}

// resultsProfilesBinding is resultsProfiles with DISPATCH_RESULTS_TOKEN's proxy block binding hosts
// (a JSON list's inside) and the extra profiles after it.
func resultsProfilesBinding(hosts string, extra ...string) string {
	profiles := append([]string{`{"name": "DISPATCH_RESULTS_TOKEN", "proxy": {"hosts": [` + hosts + `], "headers": ["Authorization"],
	"allow": [{"method": "POST", "pathPrefix": "/api/v1/projects/"}], "kind": "dpop"}}`}, extra...)
	return `{"profiles": [` + strings.Join(profiles, ", ") + `]}`
}

// TestABrokerWithNoResultEntriesHoldsTheResultsEntryToTheDestinationsOneHost: broker B, on the
// database of broker A, which has a result entry and has granted run 7 a placeholder, starts with
// no result entry of its own. With a profile DISPATCH_RESULTS_TOKEN whose proxy block binds
// *.ci-results.test, or resultsBoundHost and evil.ci-results.test, or resultsBoundHost with no
// private destination or with a destination of evil.ci-results.test alone, or with a second
// profile of kind dpop beside it whose host evil.ci-results.test no destination names, B refuses
// to start, naming BROKER_PROFILES_FILE and the profile at fault, and not
// BROKER_EXCHANGE_RESULT_WORKFLOWS or the removed BROKER_AIRLOCK_RULES_FILE, and the stand-in of
// evil.ci-results.test, which B could resolve and dial, receives nothing. Control: B with A's own
// profile, whose proxy block binds resultsBoundHost, and the destination of that host starts,
// takes A's run placeholder (a grant serves only on a broker of its own results entry), and its
// write reaches the stand-in of Dispatch, stored.
func TestABrokerWithNoResultEntriesHoldsTheResultsEntryToTheDestinationsOneHost(t *testing.T) {
	e := newResultsEnvironment(t)
	// The stand-in listens on 127.0.0.2: BROKER_AIRLOCK_TEST_RESOLVE refuses one address given two
	// ports, and Dispatch's stand-in has 127.0.0.1.
	evil, err := proxyrulestest.NewService(proxyrulestest.ServiceOptions{Addr: "127.0.0.2:0", Hosts: []string{otherResultsHost}, Authority: e.dispatch.service.Authority()})
	if err != nil {
		t.Fatalf("start the stand-in of %s: %v", otherResultsHost, err)
	}
	t.Cleanup(evil.Close)
	evil.AnswerWith(func(w http.ResponseWriter, _ *http.Request, _ proxyrulestest.Request) bool {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"stored":true}`)
		return true
	})
	routing := []string{
		"BROKER_AIRLOCK_TEST_RESOLVE=" + resultsBoundHost + "=" + e.dispatch.service.Addr() + " " + otherResultsHost + "=" + evil.Addr(),
		"BROKER_AIRLOCK_TEST_DIAL_ALLOW=" + resultsBoundHost + ":443 " + otherResultsHost + ":443",
	}
	a := e.start(t)
	g := e.grant(t, a, "7")
	binary := buildBroker(t)
	base := append(withoutSettings(e.env, "BROKER_PROFILES_FILE", "BROKER_AIRLOCK_TEST_RESOLVE", "BROKER_AIRLOCK_TEST_DIAL_ALLOW",
		"BROKER_EXCHANGE_RESULT_WORKFLOWS", "BROKER_PROXY_PRIVATE_UPSTREAM"), routing...)
	otherUpstream := "BROKER_PROXY_PRIVATE_UPSTREAM=" + otherResultsHost + "=https://" + otherResultsHost
	secondDPoP := `{"name": "OTHER_TOKEN", "proxy": {"hosts": ["` + otherResultsHost + `"], "headers": ["Authorization"], "allow": [{"method": "POST", "pathPrefix": "/api/"}], "kind": "dpop"}}`
	for _, c := range []struct {
		name, profiles, profile, says string
		upstream                      []string
	}{
		{"a wildcard profile", resultsProfilesBinding(`"*.ci-results.test"`), "DISPATCH_RESULTS_TOKEN", "one exact host", []string{resultsUpstream}},
		{"a two-host profile", resultsProfilesBinding(`"` + resultsBoundHost + `", "` + otherResultsHost + `"`), "DISPATCH_RESULTS_TOKEN", "one exact host", []string{resultsUpstream}},
		{"no private destination", resultsProfilesBinding(`"` + resultsBoundHost + `"`), "DISPATCH_RESULTS_TOKEN", "BROKER_PROXY_PRIVATE_UPSTREAM is unset", nil},
		{"a destination of another host", resultsProfilesBinding(`"` + resultsBoundHost + `"`), "DISPATCH_RESULTS_TOKEN", "which no entry of BROKER_PROXY_PRIVATE_UPSTREAM names", []string{otherUpstream}},
		{"a second dpop profile of a host no destination names", resultsProfilesBinding(`"`+resultsBoundHost+`"`, secondDPoP), "OTHER_TOKEN", "binds " + otherResultsHost + ", which no entry of BROKER_PROXY_PRIVATE_UPSTREAM names", []string{resultsUpstream}},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := append(append(append([]string{}, base...), "BROKER_PROFILES_FILE="+profilesFile(t, c.profiles)), c.upstream...)
			out := refusedWithin(t, binary, append([]string{metricsListenEnv}, env...))
			if !strings.Contains(out, c.profile) || !strings.Contains(out, c.says) {
				t.Fatalf("B with %s was refused, but its output does not name %s and %q: %s", c.name, c.profile, c.says, out)
			}
			if strings.Contains(out, "BROKER_EXCHANGE_RESULT_WORKFLOWS") {
				t.Errorf("B's refusal names BROKER_EXCHANGE_RESULT_WORKFLOWS, which it was not given: %s", out)
			}
			if !strings.Contains(out, "BROKER_PROFILES_FILE") || strings.Contains(out, "BROKER_AIRLOCK_RULES_FILE") {
				t.Errorf("B's refusal does not name BROKER_PROFILES_FILE, whose proxy blocks are the rules, or names the removed BROKER_AIRLOCK_RULES_FILE: %s", out)
			}
			if seen := evil.Requests(); len(seen) != 0 {
				t.Fatalf("the stand-in of %s received %d request(s); want none", otherResultsHost, len(seen))
			}
		})
	}

	// Control: A's own profile and destination start, and B serves A's grant to Dispatch.
	env := append(append([]string{}, base...), "BROKER_PROFILES_FILE="+profilesFile(t, resultsProfiles), resultsUpstream)
	process := startBroker(t, brokerOptions{Binary: binary, Env: env})
	thumbprint, _ := process.AwaitRecord(t, dpopKeyLine, nil)["thumbprint"].(string)
	b := &resultsBroker{brokerProcess: process, proxyAddr: airlockProxyAddr(t, process), thumbprint: thumbprint}
	before := len(e.dispatch.service.Requests())
	answer, body := g.write(t, g.client(t, b))
	if answer.StatusCode != http.StatusCreated || len(e.dispatch.service.Requests()) != before+1 {
		t.Fatalf("control: A's placeholder through B answered %d %q and Dispatch's stand-in received %d request(s); want 201 and one", answer.StatusCode, body, len(e.dispatch.service.Requests())-before)
	}
	if seen := evil.Requests(); len(seen) != 0 {
		t.Fatalf("control: the stand-in of %s received %d request(s); want none", otherResultsHost, len(seen))
	}
}

// refusedWithin runs the broker binary as runRefusedBroker does and answers its output, failing t
// unless it exits non-zero within a minute without logging "broker listening": a broker that
// takes the settings runs on, and is ended at the minute and reported, rather than holding the
// test until its own timeout.
func refusedWithin(t *testing.T, binary string, env []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = append(append(append(os.Environ(), productionEnv(t)...), noAWSEnv(t)...), env...)
	out, err := cmd.CombinedOutput()
	switch {
	case ctx.Err() != nil:
		t.Fatalf("the broker started and ran for a minute; want a refusal at start: %s", out)
	case err == nil:
		t.Fatalf("the broker exited 0; want a refusal at start: %s", out)
	case strings.Contains(string(out), "broker listening"):
		t.Fatalf(`the broker logged "broker listening" before it exited: %s`, out)
	}
	return string(out)
}
