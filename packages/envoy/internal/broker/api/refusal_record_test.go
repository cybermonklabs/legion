// refusal_record_test.go drives one refusal through each writer of the API port, behind the
// answer recorder cmd/broker installs outermost (answer.Middleware, which newTestServerWith wraps
// every test server in), and reads the record each one writes, the answer id each answer
// carries, and the count of every answer, refusals and those that write no line alike.
package api_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/broker/api"
	"github.com/sjawhar/envoy/internal/broker/brokertest"
	"github.com/sjawhar/envoy/internal/broker/oauthwire"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// The writers of the API port, each a writer whose refusal no other row writes.
const (
	writerWriteError       = "writeError"
	writerPageRefusal      = "writePageRefusal"
	writerOAuthRefusal     = "oauth.WriteRefusal"
	writerOAuthLibrary     = "the OAuth provider's own writer"
	writerRefuseOrigin     = "refuseOrigin"
	writerPageMessage      = "the approval page's writeMessage"
	writerMuxNotFound      = "the mux's 404"
	writerMuxNotAllowed    = "the mux's 405"
	writersOfTheAPIPort    = 8
	answersThatWriteNoLine = 6
)

// answered is one answer of the broker as its client read it.
type answered struct {
	status int
	header http.Header
	body   []byte
}

// id is the answer id the answer's header carries.
func (a answered) id() string { return a.header.Get(answer.Header) }

// send sends req with client and reads the whole answer.
func send(t *testing.T, client *http.Client, req *http.Request) answered {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer to %s %s: %v", req.Method, req.URL.Path, err)
	}
	return answered{status: resp.StatusCode, header: resp.Header, body: body}
}

// request is a request for target with body and headers.
func request(t *testing.T, method, target string, body io.Reader, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// jsonBody is v as a JSON request body.
func jsonBody(t *testing.T, v any) io.Reader {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw)
}

// sessionCall is a request signed by the session enrollmentID holds key for.
func (ts *testServer) sessionCall(t *testing.T, key *ecdsa.PrivateKey, enrollmentID, method, path string, body any, headers map[string]string) *http.Request {
	t.Helper()
	p, err := proof.Sign(key, enrollmentID, method, ts.URL+path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	all := map[string]string{"Proof": p, "Content-Type": "application/json"}
	for k, v := range headers {
		all[k] = v
	}
	return request(t, method, ts.URL+path, jsonBody(t, body), all)
}

// answersCounted is ts's broker_answers_total sample naming labels, 0 when it has none.
func (ts *testServer) answersCounted(t *testing.T, labels string) int {
	t.Helper()
	w := httptest.NewRecorder()
	ts.registry.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(w.Body.String()) {
		if value, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "broker_answers_total{"+labels+"} "); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("broker_answers_total{%s} is no count: %q", labels, value)
			}
			return n
		}
	}
	return 0
}

// refusalRecords are the records under answer.RefusalMessage that name answerID, each attribute
// by its key, with the level each was written at.
func refusalRecords(capture *logtest.Recorder, answerID string) []map[string]any {
	var found []map[string]any
	for _, record := range capture.Records() {
		if record.Message != answer.RefusalMessage {
			continue
		}
		fields := map[string]any{"level": record.Level}
		record.Attrs(func(a slog.Attr) bool { fields[a.Key] = a.Value.Any(); return true })
		if fields["answer_id"] == answerID {
			found = append(found, fields)
		}
	}
	return found
}

// TestEveryWriterOfTheAPIPortWritesOneRefusalRecord drives a refusal through each of the eight
// writers of the API port and reads what each leaves: exactly one record under
// answer.RefusalMessage at WARN, naming the code, every secret name the request named, the
// caller's class and identity, the route and the answer id, which the answer carries in its
// X-Broker-Answer-Id header and, in a JSON body, as answer_id, and on the approval page's HTML
// as text; a caller's X-Correlation-Id; and the refusal counted. A refusal raised in
// authentication names caller_class unauthenticated, and so does a listed caller's lack of a
// route's power, on the decision routes that authenticate for themselves as on every other
// account route. A 500, a 503, a 429 of writeError and of the approval page, and the device
// grant's authorization_pending and slow_down write no record and are counted. No record holds
// the value of a secret the port released, or a refusal's message.
func TestEveryWriterOfTheAPIPortWritesOneRefusalRecord(t *testing.T) {
	capture := &logtest.Recorder{}
	logtest.SetDefault(t, capture)
	var failDeviceLogin, draining atomic.Bool
	ts := newTestServerWith(t, func(d *api.Deps) {
		login := d.DeviceLogin
		d.DeviceLogin = func(ctx context.Context, form url.Values, userCode string) error {
			if failDeviceLogin.Load() {
				return errors.New("the device login's store went away")
			}
			return login(ctx, form, userCode)
		}
		d.Draining = draining.Load
		// One reread an hour from an address, so the second answers writeError's 429.
		d.RereadLimit = &ratelimit.Limit{Every: time.Hour, Burst: 1}
		// The Envoy listener, a listed caller that holds no decision's power.
		d.APIAccounts[listenerAccount] = brokertest.Caller(t, listenerCaller)
	})
	// A server whose page and account routes take one request an hour from an address, so the
	// second answers the approval page's own 429 (Page.WriteRateLimited).
	limited := newTestServerWith(t, func(d *api.Deps) { d.AccountLimit = &ratelimit.Limit{Every: time.Hour, Burst: 1} })

	enrollmentID, key := ts.newSessionEnrollment(t, "session-"+t.Name(), testApprover)
	// WORKER_TOKEN is granted at once and its value released through the port, so the record
	// of the refusal that follows on the same grant has a value to keep out.
	worker := ts.requestAs(t, key, enrollmentID, "WORKER_TOKEN")
	if worker.GrantID == nil {
		t.Fatalf("POST /v1/requests for WORKER_TOKEN = %+v, want an automatic grant", worker)
	}
	if released := send(t, http.DefaultClient, ts.sessionCall(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+*worker.GrantID+"/values", nil, nil)); released.status != http.StatusOK || !bytes.Contains(released.body, []byte("worker-v1")) {
		t.Fatalf("the release of WORKER_TOKEN = %d %s, want 200 with its value", released.status, released.body)
	}
	pending := ts.requestAs(t, key, enrollmentID, "DEEL_API_KEY")
	if pending.RecordID == nil {
		t.Fatalf("POST /v1/requests for DEEL_API_KEY = %+v, want a pending record", pending)
	}
	loginCode := ts.openMachineLogin(t, newSigningKey(t), testApprover, "example-host-refused")
	looked := decode[wireRecord](t, send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/machine-logins/lookup",
		jsonBody(t, map[string]any{"code": loginCode}), map[string]string{"Authorization": "Bearer " + ts.uiToken, "Content-Type": "application/json"})).body)
	const correlation = "a caller's own request id 7d9f"

	drove := map[string]bool{}
	for _, row := range []struct {
		writer, name string
		drive        func(t *testing.T) answered
		status       int
		// code is the code the record names; "" reads it from the JSON body's error member, for
		// the OAuth library's own refusal, whose words are the library's.
		code, route  string
		secrets      []string
		class        string
		caller       string
		correlation  string
		html, isJSON bool
	}{
		{
			writer: writerWriteError, name: "a session's request",
			drive: func(t *testing.T) answered {
				// Signed by a key the session does not hold: the request object opens, so its
				// names are read, and is then refused as not the session's own.
				return send(t, http.DefaultClient, ts.sessionCall(t, key, enrollmentID, http.MethodPost, "/v1/requests",
					map[string]any{"request": signAgentSecretRequest(t, newSigningKey(t), ts.URL, "need it", "DEEL_API_KEY", "SHARED_KEY"), "session_id": nil},
					map[string]string{"X-Correlation-Id": correlation}))
			},
			status: http.StatusBadRequest, code: "REQUEST_INVALID", route: "POST /v1/requests", secrets: []string{"DEEL_API_KEY", "SHARED_KEY"},
			class: answer.CallerSession, caller: enrollmentID, correlation: correlation, isJSON: true,
		},
		{
			writer: writerWriteError, name: "a grant's release",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, ts.sessionCall(t, key, enrollmentID, http.MethodPost, "/v1/grants/"+*worker.GrantID+"/values",
					map[string]any{"github_app": map[string]any{"owner": "ada", "read_only": true}}, nil))
			},
			status: http.StatusBadRequest, code: "NARROWING_NOT_APPLICABLE", route: "POST /v1/grants/{id}/values", secrets: []string{"WORKER_TOKEN"},
			class: answer.CallerSession, caller: enrollmentID, isJSON: true,
		},
		{
			writer: writerWriteError, name: "raised in authentication",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodGet, ts.URL+"/v1/enrollments/self", nil, nil))
			},
			status: http.StatusUnauthorized, code: "PROOF_INVALID", route: "GET /v1/enrollments/self",
			class: answer.CallerUnauthenticated, isJSON: true,
		},
		{
			writer: writerWriteError, name: "a listed account",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodGet, ts.URL+"/v1/pending", nil, map[string]string{"Authorization": "Bearer " + ts.uiToken}))
			},
			status: http.StatusBadRequest, code: "APPROVER_REQUIRED", route: "GET /v1/pending",
			class: answer.CallerAccount, caller: dispatchCaller, isJSON: true,
		},
		{
			writer: writerWriteError, name: "a decision whose handler authenticated its caller",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/credential-requests/"+looked.RecordID+"/approve",
					jsonBody(t, map[string]any{"approver": " "}), map[string]string{"Authorization": "Bearer " + ts.uiToken, "Content-Type": "application/json"}))
			},
			status: http.StatusBadRequest, code: "APPROVER_REQUIRED", route: "POST /v1/credential-requests/{record}/approve",
			class: answer.CallerAccount, caller: dispatchCaller, isJSON: true,
		},
		{
			writer: writerWriteError, name: "a decision by a listed caller without its power",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/credential-requests/"+looked.RecordID+"/approve",
					jsonBody(t, map[string]any{"approver": testApprover}), map[string]string{"Authorization": "Bearer " + ts.accountToken(t, listenerAccount, "", time.Hour), "Content-Type": "application/json"}))
			},
			status: http.StatusForbidden, code: "ACCOUNT_LACKS_POWER", route: "POST /v1/credential-requests/{record}/approve",
			class: answer.CallerUnauthenticated, isJSON: true,
		},
		{
			writer: writerWriteError, name: "a service's token login",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/launcher-credentials/token",
					jsonBody(t, map[string]any{"token": ts.accountToken(t, daemonAccount, uuid.NewString(), time.Hour), "request": "not a request object"}),
					map[string]string{"Content-Type": "application/json"}))
			},
			status: http.StatusBadRequest, code: "REQUEST_INVALID", route: "POST /v1/launcher-credentials/token",
			class: answer.CallerService, caller: "legion-daemon@legion-system/legion-daemon-test", isJSON: true,
		},
		{
			writer: writerPageRefusal, name: "a secret request decided off the broker's page",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/credential-requests/"+*pending.RecordID+"/approve", nil, nil))
			},
			status: http.StatusForbidden, code: "DECIDE_ON_BROKER_PAGE", route: "POST /v1/credential-requests/{record}/approve",
			class: answer.CallerUnauthenticated, isJSON: true,
		},
		{
			writer: writerOAuthRefusal, name: "a service's device login",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, deviceAuthorization(t, ts, newSigningKey(t), "legion-daemon"))
			},
			status: http.StatusBadRequest, code: "SERVICE_LOGS_IN_WITH_TOKEN", route: "POST /v1/oauth/device_authorization",
			class: answer.CallerPublic, isJSON: true,
		},
		{
			writer: writerOAuthLibrary, name: "a device code no login issued",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/oauth/token", strings.NewReader(tokenForm.Encode()),
					map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
			},
			status: http.StatusBadRequest, route: "POST /v1/oauth/token",
			class: answer.CallerPublic, caller: oauthwire.ClientID, isJSON: true,
		},
		{
			writer: writerRefuseOrigin, name: "a decision from another origin",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/approve", strings.NewReader("decision=approve"),
					map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "https://elsewhere.example"}))
			},
			status: http.StatusForbidden, code: "ORIGIN_REFUSED", route: "POST /approve",
			class: answer.CallerUnauthenticated, html: true,
		},
		{
			writer: writerPageMessage, name: "a page for no record id",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodGet, ts.URL+"/approve/not-a-record", nil, nil))
			},
			status: http.StatusBadRequest, code: "RECORD_ID_INPUT", route: "GET /approve/{record}",
			class: answer.CallerUnauthenticated, html: true,
		},
		{
			writer: writerMuxNotFound, name: "a path no route serves",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodGet, ts.URL+"/v1/nowhere", nil, nil))
			},
			status: http.StatusNotFound, code: "HTTP_404", class: answer.CallerPublic,
		},
		{
			writer: writerMuxNotAllowed, name: "a method the route does not take",
			drive: func(t *testing.T) answered {
				return send(t, http.DefaultClient, request(t, http.MethodDelete, ts.URL+"/v1/settings", nil, nil))
			},
			status: http.StatusMethodNotAllowed, code: "HTTP_405", class: answer.CallerPublic,
		},
	} {
		t.Run(row.writer+": "+row.name, func(t *testing.T) {
			got := row.drive(t)
			drove[row.writer] = true
			id := got.id()
			if !idPattern(id) {
				t.Fatalf("%s: the answer carries %s %q, want an answer id (%d %s)", row.writer, answer.Header, id, got.status, got.body)
			}
			if got.status != row.status {
				t.Fatalf("%s: the answer is %d %s, want %d", row.writer, got.status, got.body, row.status)
			}
			var body map[string]any
			if row.isJSON {
				if err := json.Unmarshal(got.body, &body); err != nil {
					t.Fatalf("%s: the refusal is no JSON object: %v: %s", row.writer, err, got.body)
				}
				if body["answer_id"] != id {
					t.Errorf("%s: the body's answer_id is %v, want the header's %s: %s", row.writer, body["answer_id"], id, got.body)
				}
			}
			if row.html && !bytes.Contains(got.body, []byte("<code>"+id+"</code>")) {
				t.Errorf("%s: the refusal page does not print its answer id %s:\n%s", row.writer, id, got.body)
			}
			code := row.code
			if code == "" {
				code, _ = body["error"].(string)
			}
			records := refusalRecords(capture, id)
			if len(records) != 1 {
				t.Fatalf("%s: the refusal wrote %d records under %q, want 1: %v", row.writer, len(records), answer.RefusalMessage, records)
			}
			record := records[0]
			secrets := row.secrets
			if secrets == nil {
				secrets = []string{}
			}
			want := map[string]any{
				"level": slog.LevelWarn, "status": int64(row.status), "code": code, "route": row.route,
				"caller_class": row.class, "caller": row.caller, "secrets": secrets,
			}
			if row.correlation != "" {
				want["correlation_id"] = row.correlation
			} else if sent, ok := record["correlation_id"]; ok {
				t.Errorf("%s: the record names correlation_id %v for a caller that sent none", row.writer, sent)
			}
			for field, value := range want {
				if fmt.Sprint(record[field]) != fmt.Sprint(value) {
					t.Errorf("%s: the record's %s is %v, want %v: %v", row.writer, field, record[field], value, record)
				}
			}
			// The broker's own envelope words its refusal under error, beside its code; an OAuth
			// error's error is its code, and its words are its error_description.
			if message, _ := body["error"].(string); body["code"] != nil && message != "" && strings.Contains(fmt.Sprint(record), message) {
				t.Errorf("%s: the record quotes the refusal's message %q: %v", row.writer, message, record)
			}
			if message, _ := body["error_description"].(string); message != "" && strings.Contains(fmt.Sprint(record), message) {
				t.Errorf("%s: the record quotes the refusal's description %q: %v", row.writer, message, record)
			}
			if n := ts.answersCounted(t, `outcome="refused",code="`+code+`",caller_class="`+row.class+`"`); n < 1 {
				t.Errorf("%s: broker_answers_total counts no refused answer coded %s of class %s", row.writer, code, row.class)
			}
		})
	}
	var missing []string
	for _, writer := range []string{writerWriteError, writerPageRefusal, writerOAuthRefusal, writerOAuthLibrary, writerRefuseOrigin, writerPageMessage, writerMuxNotFound, writerMuxNotAllowed} {
		if !drove[writer] {
			missing = append(missing, writer)
		}
	}
	if len(drove) < writersOfTheAPIPort || len(missing) != 0 {
		t.Fatalf("the table drove %d of the API port's %d writers; it never drove %v", len(drove), writersOfTheAPIPort, missing)
	}

	// The answers that write no record: each is counted under its own outcome, and the 500 keeps
	// the line its writer logs.
	machineKey := newSigningKey(t)
	var deviceCode string
	silent := 0
	for _, row := range []struct {
		name   string
		server *testServer
		drive  func(t *testing.T) answered
		status int
		labels string
	}{
		{
			name: "a 500", server: ts,
			drive: func(t *testing.T) answered {
				failDeviceLogin.Store(true)
				defer failDeviceLogin.Store(false)
				return send(t, http.DefaultClient, deviceAuthorization(t, ts, newSigningKey(t), ""))
			},
			status: http.StatusInternalServerError, labels: `outcome="failed",code="HTTP_500",caller_class="public"`,
		},
		{
			name: "a 503", server: ts,
			drive: func(t *testing.T) answered {
				draining.Store(true)
				defer draining.Store(false)
				return send(t, http.DefaultClient, request(t, http.MethodGet, ts.URL+"/healthz", nil, nil))
			},
			status: http.StatusServiceUnavailable, labels: `outcome="failed",code="HTTP_503",caller_class="public"`,
		},
		{
			name: "a 429 of writeError", server: ts,
			drive: func(t *testing.T) answered {
				if first := send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/secrets/WORKER_TOKEN/reread", nil, nil)); first.status != http.StatusOK {
					t.Fatalf("the first reread = %d %s, want 200", first.status, first.body)
				}
				return send(t, http.DefaultClient, request(t, http.MethodPost, ts.URL+"/v1/secrets/WORKER_TOKEN/reread", nil, nil))
			},
			status: http.StatusTooManyRequests, labels: `outcome="rate_limited",code="RATE_LIMITED",caller_class="public"`,
		},
		{
			name: "a 429 of the approval page", server: limited,
			drive: func(t *testing.T) answered {
				send(t, http.DefaultClient, request(t, http.MethodGet, limited.URL+"/approve/not-a-record", nil, nil))
				return send(t, http.DefaultClient, request(t, http.MethodGet, limited.URL+"/approve/not-a-record", nil, nil))
			},
			status: http.StatusTooManyRequests, labels: `outcome="rate_limited",code="RATE_LIMITED",caller_class="unauthenticated"`,
		},
		{
			name: "authorization_pending", server: ts,
			drive: func(t *testing.T) answered {
				authorized := send(t, http.DefaultClient, deviceAuthorization(t, ts, machineKey, ""))
				var codes map[string]any
				if err := json.Unmarshal(authorized.body, &codes); err != nil || authorized.status != http.StatusOK {
					t.Fatalf("device authorization = %d %s, want 200", authorized.status, authorized.body)
				}
				deviceCode, _ = codes["device_code"].(string)
				return pollToken(t, ts, machineKey, deviceCode)
			},
			status: http.StatusBadRequest, labels: `outcome="pending",code="authorization_pending",caller_class="public"`,
		},
		{
			// Polled again at once, inside the interval the device authorization named.
			name: "slow_down", server: ts,
			drive: func(t *testing.T) answered {
				return pollToken(t, ts, machineKey, deviceCode)
			},
			status: http.StatusBadRequest, labels: `outcome="rate_limited",code="slow_down",caller_class="public"`,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := row.server.answersCounted(t, row.labels)
			got := row.drive(t)
			silent++
			if !idPattern(got.id()) || got.status != row.status {
				t.Fatalf("%s: the answer is %d with id %q: %s, want %d with an answer id", row.name, got.status, got.id(), got.body, row.status)
			}
			if records := refusalRecords(capture, got.id()); len(records) != 0 {
				t.Errorf("%s: wrote %v, want no record", row.name, records)
			}
			if after := row.server.answersCounted(t, row.labels); after != before+1 {
				t.Errorf("%s: broker_answers_total{%s} went from %d to %d, want one more", row.name, row.labels, before, after)
			}
		})
	}
	if silent != answersThatWriteNoLine {
		t.Fatalf("the table drove %d answers that write no record, want %d", silent, answersThatWriteNoLine)
	}
	failed := 0
	for _, record := range capture.Records() {
		if record.Level == slog.LevelError && record.Message == "broker: OAuth request failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("the 500 left %d %q lines, want its writer's one", failed, "broker: OAuth request failed")
	}

	// No record, under any message, holds the value of a secret the port serves, though one was
	// released through it above.
	for _, value := range []string{"worker-v1", "deel-v1", "shared-v1"} {
		for _, record := range capture.Records() {
			text := record.Message
			record.Attrs(func(a slog.Attr) bool { text += " " + a.Key + "=" + fmt.Sprint(a.Value.Any()); return true })
			if strings.Contains(text, value) {
				t.Errorf("a record holds the stored value %q: %s", value, text)
			}
		}
	}
}

// idPattern reports whether id is an answer id: 16 random bytes in hex.
func idPattern(id string) bool {
	if len(id) != 32 {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) })
}

// deviceAuthorization is a device authorization request for a machine login key signs, naming
// service (none for a person's machine), bound to key by dpop_jkt.
func deviceAuthorization(t *testing.T, ts *testServer, key *ecdsa.PrivateKey, service string) *http.Request {
	t.Helper()
	jkt, err := proof.Thumbprint(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"client_id": {oauthwire.ClientID}, "scope": {"machine"}, "dpop_jkt": {jkt},
		"request": {signMachineLoginRequest(t, key, ts.URL, testApprover, "example-host-devbox", service)},
	}
	return request(t, http.MethodPost, ts.URL+"/v1/oauth/device_authorization", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}

// pollToken asks the token endpoint for deviceCode's tokens with a DPoP proof of key, as a
// machine polls while its person decides.
func pollToken(t *testing.T, ts *testServer, key *ecdsa.PrivateKey, deviceCode string) answered {
	t.Helper()
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}, "client_id": {oauthwire.ClientID}}
	client := &http.Client{Transport: brokerclient.NewDPoPTransport(key, http.DefaultTransport)}
	return send(t, client, request(t, http.MethodPost, ts.URL+"/v1/oauth/token", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
}
