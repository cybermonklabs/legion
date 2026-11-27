package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/ciphertoken/ciphertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// scrapeMetrics is the Prometheus text the registry answers a scrape with.
func scrapeMetrics(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(recorder.Body)
	return string(body)
}

// metricSample is the value of the sample named name in a scrape, and whether the scrape holds one.
func metricSample(t *testing.T, scrape, name string) (float64, bool) {
	t.Helper()
	match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + ` (\S+)$`).FindStringSubmatch(scrape)
	if match == nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		t.Fatalf("the sample %s reads %q: %v", name, match[1], err)
	}
	return value, true
}

// The process's metrics registry carries the broker revocations owed, so an operator sees one the
// broker never confirms: envoy_dispatch_broker_revocations_owed counts them, and
// envoy_dispatch_broker_revocation_oldest_owed_seconds is how long the oldest has been owed since its
// replacing commit, written only while one is owed. Each series reads the table when a scrape asks
// for it, so what it counts is the deployment's, whichever process recorded it.
func TestTheBrokerRevocationMetricsCountTheOwedAndAgeTheOldest(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	registry := metrics.New()
	api.RegisterBrokerRevocationMetrics(registry, database.Pool)
	scrape := scrapeMetrics(t, registry)
	if owed, ok := metricSample(t, scrape, "envoy_dispatch_broker_revocations_owed"); !ok || owed != 0 {
		t.Fatalf("with nothing owed, the scrape reads owed %v (%t), want 0:\n%s", owed, ok, scrape)
	}
	if _, ok := metricSample(t, scrape, "envoy_dispatch_broker_revocation_oldest_owed_seconds"); ok {
		t.Fatalf("with nothing owed, the scrape holds an oldest owed age, want none:\n%s", scrape)
	}

	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil, store.WithBrokerRevocations("https://accounts.google.com"))
	for _, subject := range []string{firstHolder, secondHolder} {
		if err := people.SignInSubject(ctx, reassignedEmail, subject, "refresh", "", model.MemberFull); err != nil {
			t.Fatal(err)
		}
	}
	// A revocation recorded ten minutes ago: its recording time moves only when it is recorded
	// again, so the fixture writes it with the guard off (storetest.TestOnlyWriteBrokerRevocations).
	storetest.TestOnlyWriteBrokerRevocations(t, database, `update broker_revocations set recorded_at = recorded_at - interval '10 minutes'`)
	scrape = scrapeMetrics(t, registry)
	if owed, ok := metricSample(t, scrape, "envoy_dispatch_broker_revocations_owed"); !ok || owed != 1 {
		t.Fatalf("with one owed, the scrape reads owed %v (%t), want 1:\n%s", owed, ok, scrape)
	}
	if oldest, ok := metricSample(t, scrape, "envoy_dispatch_broker_revocation_oldest_owed_seconds"); !ok || oldest < 600 || oldest > 660 {
		t.Fatalf("with one owed for ten minutes, the scrape reads its age %v (%t), want about 600 seconds:\n%s", oldest, ok, scrape)
	}
}

// runClearBrokerRevocation runs `envoy-dispatch clear-broker-revocation` with args on database,
// answering its exit code and what it printed.
func runClearBrokerRevocation(t *testing.T, database *store.Store, args ...string) (int, string) {
	t.Helper()
	env := envGetter(map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()})
	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), append([]string{"clear-broker-revocation"}, args...), env, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// `envoy-dispatch clear-broker-revocation` settles a revocation the broker never confirms, so the
// address's broker tokens recording no account are not refused for good, and settles only what the
// operator saw: a full member's address passes to another Workspace account and no pass reaches
// the broker. The command given the address alone (in any case) lists the revocation owed, naming
// the replaced account, its generation and the address's session generation at the recording
// (`none` for a row holding none, as one an earlier head recorded), and settles nothing.
// Meanwhile the first holder takes the address back and a third account takes it from them, so
// the first holder's revocation is
// owed again under generation 2. The command given what the operator's list printed settles
// nothing then, exits 1 and says so; given the generations owed now it settles them, without
// telling the broker anything. A broker token of the address recording no account, its machine
// login made before the third holder's sign-in recorded its account, is refused 401
// BROKER_TOKEN_RECORDS_NO_ACCOUNT while they are owed and still once they are cleared: the clear
// changes nothing Dispatch admits, which holds each token to its own account or creation time.
// Listed again it finds nothing owed and exits 1, and it exits 2 for anything but one address and
// well-formed names.
func TestClearBrokerRevocationSettlesOnlyWhatTheOperatorSaw(t *testing.T) {
	s := newOutsideSignIn(t, nil)
	broker := newFakeBroker(t)
	handler, _ := s.brokerRouter(t, broker)
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	s.workspaceSignIn(t, handler, reassignedEmail, secondHolder)

	code, out := runClearBrokerRevocation(t, s.database, strings.ToUpper(reassignedEmail))
	if code != 0 || !strings.Contains(out, "clear-broker-revocation: "+reassignedEmail+" owes "+firstHolder+":1") || !strings.Contains(out, "nothing cleared") {
		t.Fatalf("clear-broker-revocation %s: exit %d, want 0, the revocation owed listed and nothing cleared:\n%s", reassignedEmail, code, out)
	}
	// The line names the address's session generation at the recording, which distinguishes rows
	// an operator settles exactly as the list printed them.
	if generation := s.brokerRevocationSessionGeneration(t, reassignedEmail, firstHolder); !strings.Contains(out, fmt.Sprintf(", session generation %d)", generation)) {
		t.Fatalf("clear-broker-revocation %s lists:\n%s\nwant the owed row's session generation, %d, on its line", reassignedEmail, out, generation)
	}
	s.nullBrokerRevocationSessionGeneration(t, reassignedEmail, firstHolder)
	if _, out := runClearBrokerRevocation(t, s.database, reassignedEmail); !strings.Contains(out, " owes "+firstHolder+":1 ") || !strings.Contains(out, ", session generation none)") {
		t.Fatalf("clear-broker-revocation %s, its row holding no session generation, lists:\n%s\nwant it printed with session generation none", reassignedEmail, out)
	}
	s.restoreBrokerRevocationSessionGeneration(t, reassignedEmail, firstHolder)
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || done {
		t.Fatalf("after the list the revocation of %s reads recorded %t, done %t; want it still owed", reassignedEmail, recorded, done)
	}
	s.workspaceSignIn(t, handler, reassignedEmail, firstHolder)
	s.workspaceSignIn(t, handler, reassignedEmail, "third-holder")
	if code, out := runClearBrokerRevocation(t, s.database, reassignedEmail, firstHolder+":1"); code != 1 || !strings.Contains(out, firstHolder+":1 is not owed") {
		t.Fatalf("clear-broker-revocation of what the list printed, owed again since: exit %d, want 1 and it named not owed:\n%s", code, out)
	}
	if code, out := runClearBrokerRevocation(t, s.database, reassignedEmail); code != 0 ||
		!strings.Contains(out, " owes "+secondHolder+":1 ") || !strings.Contains(out, " owes "+firstHolder+":2 ") {
		t.Fatalf("after a clear of the generation the operator saw, the list of %s answered exit %d, want %s:1 and %s:2 both still owed:\n%s",
			reassignedEmail, code, secondHolder, firstHolder, out)
	}
	machine := ciphertokentest.NewCaller(t)
	const machineToken = "token-of-a-machine-recording-no-account"
	broker.holds(&fakeCredential{id: "login-unnamed", person: reassignedEmail, tokens: []string{machineToken}, createdAt: time.Now().Add(-time.Hour)}, machine)
	requireBrokerTokenRecordsNoAccount(t, "GET /api/v1/whoami with a broker token recording no account made before the recording, the revocations owed",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", machineToken))

	code, out = runClearBrokerRevocation(t, s.database, reassignedEmail, secondHolder+":1", firstHolder+":2")
	if code != 0 || !strings.Contains(out, reassignedEmail+" "+secondHolder+":1 cleared") || !strings.Contains(out, reassignedEmail+" "+firstHolder+":2 cleared") {
		t.Fatalf("clear-broker-revocation of the generations owed now: exit %d, want 0 and both cleared:\n%s", code, out)
	}
	if recorded, done := s.brokerRevocation(t, reassignedEmail); !recorded || !done {
		t.Fatalf("after clear-broker-revocation the revocations of %s read recorded %t, done %t; want every one settled", reassignedEmail, recorded, done)
	}
	if got := broker.calls(); len(got) != 0 {
		t.Fatalf("clear-broker-revocation told the broker %q, want nothing: it settles without the broker", got)
	}
	s.settledAgo(t, reassignedEmail, time.Hour)
	requireBrokerTokenRecordsNoAccount(t, "GET /api/v1/whoami with the broker token recording no account, the revocations cleared an hour ago",
		brokerTokenRequest(t, handler, machine, http.MethodGet, "/api/v1/whoami", "", machineToken))

	if code, out := runClearBrokerRevocation(t, s.database, reassignedEmail); code != 1 || !strings.Contains(out, "no broker revocation of "+reassignedEmail+" is owed") {
		t.Fatalf("clear-broker-revocation listing an address with nothing owed: exit %d, want 1 and nothing owed named:\n%s", code, out)
	}
	for _, args := range [][]string{nil, {""}, {reassignedEmail, firstHolder}, {reassignedEmail, firstHolder + ":0"}, {reassignedEmail, ":1"}, {reassignedEmail, firstHolder + ":x"}} {
		if code, out := runClearBrokerRevocation(t, s.database, args...); code != 2 {
			t.Fatalf("clear-broker-revocation %q: exit %d, want 2:\n%s", args, code, out)
		}
	}
}
