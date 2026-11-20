// broker_values_line_test.go drives every broker-answer value this CLI prints to a terminal -
// request and grant ids, a request's state, who decided it and the broker's sentence about it, the
// approver a pending request waits on, the ids enroll, a pod's enrolment and a sign-in print, a
// pod refusal's code, and the cells of the machine and grant tables - with a control character in
// it, each of which must print on one line. The ids register prints are driven in
// hostmode_linux_test.go (TestRegisterPrintsTheBrokersEnrollmentOnOneLine), beside register's
// other tests, since register takes the helper's answer only on Linux.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/brokeraddr"
	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/brokerclient"
)

// forged is a broker-answer value that, printed as it came, would end the line it is on, start a
// line of its own and then rewrite it.
const forged = "v-1\nagent-secrets: FORGED\x1b[2K"

// quotedForged is forged as each line must print it.
var quotedForged = strconv.Quote(forged)

// everyLineIsOne fails for a line of out that holds a control character inside it, and for out
// that does not name the forged value Go-quoted at least want times.
func everyLineIsOne(t *testing.T, what, out string, want int) {
	t.Helper()
	for _, line := range strings.SplitAfter(out, "\n") {
		if strings.ContainsAny(strings.TrimSuffix(line, "\n"), "\r\n\x1b") {
			t.Errorf("%s holds a line with a control character inside it: %q (all of it: %q)", what, line, out)
		}
	}
	if got := strings.Count(out, quotedForged); got < want {
		t.Errorf("%s names the forged value Go-quoted %d times, want %d: %q", what, got, want, out)
	}
}

// forgedBroker answers each route with the body the test gives it, and 404 for any other (the
// settings call among them, so a source names the broker alone).
func forgedBroker(t *testing.T, bodies map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for route, body := range bodies {
			method, path, _ := strings.Cut(route, " ")
			if r.Method == method && (r.URL.Path == path || strings.HasSuffix(path, "/") && strings.HasPrefix(r.URL.Path, path)) {
				_ = json.NewEncoder(w).Encode(body)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "error": "not found"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AGENT_SECRETS_URL", srv.URL)
	t.Setenv("AGENT_SECRETS_KEY_DIR", newKeyDir(t))
	t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
	t.Setenv("AGENT_SECRETS_WAIT", "")
	return srv
}

// TestTheRequestFormsPrintTheBrokersValuesOnOneLine: request, status, self and the exec form print
// a request's id and state, its grant id, who decided it, the broker's sentence about it and the
// approver it waits on, a session's enrollment, kind and operator and its grants' ids, Go-quoted
// where the broker's answer holds a control character.
func TestTheRequestFormsPrintTheBrokersValuesOnOneLine(t *testing.T) {
	t.Run("request, pending", func(t *testing.T) {
		forgedBroker(t, map[string]any{"POST /v1/requests": map[string]any{
			"request_id": forged, "state": "pending", "approver": forged, "approve_url": "https://broker.example/approve/r-1", "detail": forged,
		}})
		var stdout, stderr bytes.Buffer
		if code := run([]string{"request", "--reason", "need it", "SOME_SECRET"}, &stdout, &stderr); code != exitPending {
			t.Fatalf("request = %d (stderr %q), want %d", code, stderr.String(), exitPending)
		}
		// request_id, detail and the approver.
		everyLineIsOne(t, "request's stdout", stdout.String(), 3)
	})
	t.Run("request, a state the broker made up", func(t *testing.T) {
		forgedBroker(t, map[string]any{"POST /v1/requests": map[string]any{"request_id": "req-1", "state": forged}})
		var stdout, stderr bytes.Buffer
		run([]string{"request", "--reason", "need it", "SOME_SECRET"}, &stdout, &stderr)
		everyLineIsOne(t, "request's stdout", stdout.String(), 1)
	})
	t.Run("request, a lifetime the broker did not give", func(t *testing.T) {
		forgedBroker(t, map[string]any{"POST /v1/requests": map[string]any{
			"request_id": forged, "state": "granted", "grant_id": forged, "record_id": "record-1",
		}})
		var stdout, stderr bytes.Buffer
		if code := run([]string{"request", "--reason", "need it", "--ttl", "60s", "SOME_SECRET"}, &stdout, &stderr); code != 1 {
			t.Fatalf("request --ttl = %d (stderr %q), want 1", code, stderr.String())
		}
		// The request id and the grant id the refusal names.
		everyLineIsOne(t, "request --ttl's stderr", stderr.String(), 2)
	})
	t.Run("status", func(t *testing.T) {
		forgedBroker(t, map[string]any{"GET /v1/requests/": map[string]any{
			"state": "granted", "grant_id": forged, "decision": map[string]any{"by": forged, "at": time.Now()}, "detail": forged,
		}})
		var stdout, stderr bytes.Buffer
		if code := run([]string{"status", "req-1"}, &stdout, &stderr); code != 0 {
			t.Fatalf("status = %d (stderr %q), want 0", code, stderr.String())
		}
		// grant_id, decided_by and detail.
		everyLineIsOne(t, "status's stdout", stdout.String(), 3)
	})
	t.Run("status, pending", func(t *testing.T) {
		forgedBroker(t, map[string]any{"GET /v1/requests/": map[string]any{"state": forged}})
		var stdout, stderr bytes.Buffer
		run([]string{"status", "req-1"}, &stdout, &stderr)
		everyLineIsOne(t, "status's stdout", stdout.String(), 1)
	})
	t.Run("self", func(t *testing.T) {
		forgedBroker(t, map[string]any{"GET /v1/enrollments/self": map[string]any{
			"enrollment_id": forged, "kind": forged, "operator": forged, "lease_expires_at": time.Now(),
			"grants": []any{
				map[string]any{"grant_id": forged, "request_id": forged},
				map[string]any{"grant_id": forged, "request_id": forged, "expires_at": time.Now()},
			},
		}})
		var stdout, stderr bytes.Buffer
		if code := run([]string{"self"}, &stdout, &stderr); code != 0 {
			t.Fatalf("self = %d (stderr %q), want 0", code, stderr.String())
		}
		// The enrollment, its kind and operator, and each grant's two ids.
		everyLineIsOne(t, "self's stdout", stdout.String(), 7)
	})
	t.Run("the exec form, denied", func(t *testing.T) {
		forgedBroker(t, map[string]any{"POST /v1/requests": map[string]any{"request_id": forged, "state": "denied", "detail": forged}})
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--reason", "need it", "SOME_SECRET", "--", "true"}, &stdout, &stderr); code != exitDenied {
			t.Fatalf("the exec form = %d (stderr %q), want %d", code, stderr.String(), exitDenied)
		}
		// The request id and the broker's sentence on the denied line.
		everyLineIsOne(t, "the exec form's stderr", stderr.String(), 2)
	})
	t.Run("the exec form, still pending", func(t *testing.T) {
		forgedBroker(t, map[string]any{"POST /v1/requests": map[string]any{
			"request_id": forged, "state": "pending", "approver": forged, "approve_url": "https://broker.example/approve/r-1",
		}})
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--wait", "0s", "--reason", "need it", "SOME_SECRET", "--", "true"}, &stdout, &stderr); code != exitPending {
			t.Fatalf("the exec form = %d (stderr %q), want %d", code, stderr.String(), exitPending)
		}
		// The request id on the waiting line, the approver, and the id twice on the line that
		// says nothing was run.
		everyLineIsOne(t, "the exec form's stderr", stderr.String(), 4)
	})
}

// TestTheTablesPrintTheBrokersValuesOnOneLine: a cell of the grant and machine tables is the
// broker's value as it came where it holds no control character, and Go-quoted where it holds
// one, a tab among them, which would split the cell.
func TestTheTablesPrintTheBrokersValuesOnOneLine(t *testing.T) {
	slot := forged
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	plain := OperatorGrant{GrantID: "grant-1", Granted: "approved", Names: []string{"A", "B"},
		Approver: new("ada@example.com"), DecidedBy: new("judge:model-1"),
		Enrollment: OperatorGrantEnrollment{Kind: "session", RuntimeID: "h:1:1", Operator: "ada@example.com"}, ExpiresAt: &at}
	if got, want := grantRow(brokeraddr.New("https://broker.example"), plain), []string{"grant-1", "A,B", "approved", "ada@example.com", "judge:model-1", "session/h:1:1", "ada@example.com", "2026-11-17T09:00:00Z"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("grantRow of plain values = %q, want %q", got, want)
	}
	tabbed := "a\tb"
	row := grantRow(brokeraddr.New("https://broker.example"), OperatorGrant{GrantID: forged, Granted: forged, Names: []string{forged}, Approver: &slot, DecidedBy: &tabbed,
		Enrollment: OperatorGrantEnrollment{Kind: forged, RuntimeID: "h:1:1", Operator: forged, Slot: &slot}})
	for i, cell := range row {
		if strings.ContainsAny(cell, "\t\r\n\x1b") {
			t.Errorf("grantRow cell %d (%s) = %q, which holds a control character", i, grantColumns[i], cell)
		}
	}
	if row[0] != quotedForged || row[4] != strconv.Quote(tabbed) {
		t.Errorf("grantRow = %q, want the grant id %s and the decider %s", row, quotedForged, strconv.Quote(tabbed))
	}

	if got, want := machineRow(brokeraddr.New("https://broker.example"), OperatorMachine{CredentialID: "lcred-1", Host: "box", ApprovedBy: "ada@example.com", IssuedAt: at, ExpiresAt: at}),
		[]string{"lcred-1", "box", "ada@example.com", "2026-11-17T09:00:00Z", "2026-11-17T09:00:00Z", "ok"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("machineRow of plain values = %q, want %q", got, want)
	}
	cells := machineRow(brokeraddr.New("https://broker.example"), OperatorMachine{CredentialID: forged, Host: forged, ApprovedBy: tabbed, IssuedAt: at, ExpiresAt: at, Expired: true})
	if cells[0] != quotedForged || cells[1] != quotedForged || cells[2] != strconv.Quote(tabbed) || cells[5] != "expired" {
		t.Errorf("machineRow = %q, want the credential and the host %s and the approver %s", cells, quotedForged, strconv.Quote(tabbed))
	}
}

// TestTheBrokersValuesPrintNothingBesideACredentialedAddress: request, status, self, the exec
// form, a --ttl the broker did not give, and the grant and machine tables print each value of the
// broker's answer as the address lets it print (brokeraddr.Address.Words): beside an
// AGENT_SECRETS_URL that carries a credential, which whoever answers there can repeat in any
// value, each prints as brokeraddr.WordsNotPrinted. Control: beside a clean address the same
// values print, so the check sees them.
func TestTheBrokersValuesPrintNothingBesideACredentialedAddress(t *testing.T) {
	planted := "hunter2pw-" + "brokervalue-" + "51c7"
	credentialed := func(t *testing.T, srv *httptest.Server) {
		t.Setenv("AGENT_SECRETS_URL", strings.Replace(srv.URL, "http://", "http://demouser7q:demopass9z-not-a-secret@", 1))
	}
	for _, row := range []struct {
		name   string
		bodies map[string]any
		args   []string
		want   string
	}{
		{"request, pending", map[string]any{"POST /v1/requests": map[string]any{
			"request_id": planted, "state": "pending", "approver": planted, "approve_url": "https://broker.example/approve/" + planted, "detail": planted,
		}}, []string{"request", "--reason", "need it", "SOME_SECRET"}, "request_id: " + brokeraddr.WordsNotPrinted},
		{"status", map[string]any{"GET /v1/requests/": map[string]any{
			"state": "granted", "grant_id": planted, "decision": map[string]any{"by": planted, "at": time.Now()}, "detail": planted,
		}}, []string{"status", "req-1"}, "grant_id: " + brokeraddr.WordsNotPrinted},
		{"self", map[string]any{"GET /v1/enrollments/self": map[string]any{
			"enrollment_id": planted, "kind": planted, "operator": planted, "lease_expires_at": time.Now(),
			"grants": []any{map[string]any{"grant_id": planted, "request_id": planted}},
		}}, []string{"self"}, "enrollment_id: " + brokeraddr.WordsNotPrinted},
		{"the exec form, denied", map[string]any{"POST /v1/requests": map[string]any{"request_id": planted, "state": "denied", "detail": planted}},
			[]string{"--reason", "need it", "SOME_SECRET", "--", "true"}, "was denied: " + brokeraddr.WordsNotPrinted},
		{"a lifetime the broker did not give", map[string]any{"POST /v1/requests": map[string]any{
			"request_id": planted, "state": "granted", "grant_id": planted, "record_id": "record-1",
		}}, []string{"request", "--reason", "need it", "--ttl", "60s", "SOME_SECRET"}, "the broker gave request " + brokeraddr.WordsNotPrinted},
	} {
		for _, clean := range []bool{false, true} {
			name := row.name + ", beside a credentialed address"
			if clean {
				name = "control: " + row.name + ", beside a clean address"
			}
			t.Run(name, func(t *testing.T) {
				srv := forgedBroker(t, row.bodies)
				if !clean {
					credentialed(t, srv)
				}
				var stdout, stderr bytes.Buffer
				run(row.args, &stdout, &stderr)
				out := stdout.String() + stderr.String()
				if clean {
					if !strings.Contains(out, planted) {
						t.Fatalf("the output %q does not print the broker's values beside a clean address", out)
					}
					return
				}
				wantNamedNotPrinted(t, "the output", out, []string{row.want}, planted, "demopass9z")
			})
		}
	}
	t.Run("the grant and machine tables", func(t *testing.T) {
		base := brokeraddr.New("http://demouser7q:demopass9z-not-a-secret@broker.example")
		at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
		grant := grantRow(base, OperatorGrant{GrantID: planted, Granted: planted, Names: []string{planted}, Approver: &planted,
			Enrollment: OperatorGrantEnrollment{Kind: planted, RuntimeID: planted, Operator: planted}, ExpiresAt: &at})
		machine := machineRow(base, OperatorMachine{CredentialID: planted, Host: planted, ApprovedBy: planted, IssuedAt: at, ExpiresAt: at})
		wantNamedNotPrinted(t, "the rows", strings.Join(grant, "|")+"|"+strings.Join(machine, "|"), []string{brokeraddr.WordsNotPrinted}, planted)
	})
}

// TestTheEnrollmentIDsPrintOnOneLine: the enrollment enroll and a pod's enrolment print, which the
// broker made, prints as it came where it holds no control character and Go-quoted where it holds
// one. TestRegisterPrintsTheBrokersEnrollmentOnOneLine does the same for register.
func TestTheEnrollmentIDsPrintOnOneLine(t *testing.T) {
	t.Run("enroll", func(t *testing.T) {
		sock, _ := fakeHelperByOp(t, map[string]helper.Response{"enroll-session-key": {OK: true, EnrollmentID: forged}})
		t.Setenv("AGENT_SECRETS_HELPER_SOCK", sock)
		t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
		var stdout, stderr bytes.Buffer
		if code := cmdEnroll([]string{"--helper", "--runtime-id", "box:1:1", "--thumbprint", "thumbprint-1"}, &stdout, &stderr); code != 0 {
			t.Fatalf("enroll = %d (stderr %q), want 0", code, stderr.String())
		}
		everyLineIsOne(t, "enroll's stdout", stdout.String(), 1)
	})
	t.Run("a pod's enrolment", func(t *testing.T) {
		tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
		out := t.TempDir()
		broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/launcher-credentials/token" {
				_ = json.NewEncoder(w).Encode(map[string]any{"credential_id": "credential-for-the-pod", "expires_at": time.Now().Add(time.Hour)})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"enrollment_id": forged, "lease_expires_at": time.Now().Add(time.Hour)})
		}))
		defer broker.Close()
		t.Setenv("AGENT_SECRETS_URL", broker.URL)
		t.Setenv("AGENT_SECRETS_KEY_DIR", out)
		t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
		var stdout, stderr bytes.Buffer
		if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 0 {
			t.Fatalf("enroll --pod = %d (stderr %q), want 0", code, stderr.String())
		}
		everyLineIsOne(t, "enroll --pod's stdout", stdout.String(), 1)
	})
	t.Run("a pod's refused enrolment", func(t *testing.T) {
		tokenFile := writePodToken(t, fakePodToken(t, testPodAccount, testPodUID))
		out := t.TempDir()
		broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/launcher-credentials/token" {
				_ = json.NewEncoder(w).Encode(map[string]any{"credential_id": "credential-for-the-pod", "expires_at": time.Now().Add(time.Hour)})
				return
			}
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": forged, "error": "this machine login enrols its own pod alone"})
		}))
		defer broker.Close()
		t.Setenv("AGENT_SECRETS_URL", broker.URL)
		t.Setenv("AGENT_SECRETS_KEY_DIR", out)
		t.Setenv("AGENT_SECRETS_HELPER_SOCK", "")
		var stdout, stderr bytes.Buffer
		if code := cmdEnroll([]string{"--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out}, &stdout, &stderr); code != 1 {
			t.Fatalf("enroll --pod = %d (stderr %q), want 1", code, stderr.String())
		}
		everyLineIsOne(t, "enroll --pod's stderr", stderr.String(), 1)
	})
	t.Run("a sign-in", func(t *testing.T) {
		broker := newLoginBroker(t)
		broker.enrollmentID = forged
		issuer := loginIssuer(t, "ada@example.com")
		t.Setenv("AGENT_SECRETS_KEY_DIR", t.TempDir())
		t.Setenv(brokerclient.SignInDirVar, filepath.Join(t.TempDir(), "signins"))
		t.Setenv(brokerclient.BrokerURLVar, broker.server.URL)
		t.Setenv(brokerclient.SessionVar, "")
		forceMetadataMachine(t)
		var stdout bytes.Buffer
		if err := login(context.Background(), &stdout, &approvingWriter{t: t, issuer: issuer}); err != nil {
			t.Fatalf("login: %v", err)
		}
		everyLineIsOne(t, "login's stdout", stdout.String(), 1)
	})
}
