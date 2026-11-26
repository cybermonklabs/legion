// edge_server_test.go holds the broker's edge server (newEdgeServer) to its idle timeout, and its
// access log (edgeHandler, edgeAccessLog) to its line: one at INFO for every request a handler of
// the edge is handed, the 404 and 405 its mux answers and a refused login among them, naming the
// method, the path, the protocol, the status the broker answered with, the User-Agent and
// X-Forwarded-For, and nothing else the request carries. The access log's first rows drive the
// server main builds over a mux of their own; the last drives the REAL compiled broker with its
// edge, on a real Postgres.
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/answer"
	"github.com/sjawhar/envoy/internal/cipher/ciphertest"
	"github.com/sjawhar/envoy/internal/cipher/store/storetest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
)

// TestTheEdgeKeepsAnIdleConnectionLongerThanItsLoadBalancer: the edge's server keeps an idle
// connection open longer than the 600 seconds Google's external Application Load Balancer keeps
// its own connection to a backend idle, which that balancer's documentation says a backend's
// keepalive must exceed, since the balancer sends no POST again on another connection; the edge
// serves on the API server's read and write timeouts, and wraps its routes in its handler chain.
//
// The named revert: give the edge the API server's two-minute idle timeout, as before, and this
// fails.
func TestTheEdgeKeepsAnIdleConnectionLongerThanItsLoadBalancer(t *testing.T) {
	const balancerKeepalive = 600 * time.Second
	apiServer := &http.Server{ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: writeTimeout, IdleTimeout: 2 * time.Minute}
	edge := newEdgeServer(http.NewServeMux(), nil, slog.Default(), apiServer)
	if edge.IdleTimeout <= balancerKeepalive {
		t.Errorf("the edge's idle timeout is %s, want more than the load balancer's %s backend keepalive", edge.IdleTimeout, balancerKeepalive)
	}
	if edge.ReadHeaderTimeout != apiServer.ReadHeaderTimeout || edge.ReadTimeout != apiServer.ReadTimeout || edge.WriteTimeout != apiServer.WriteTimeout {
		t.Errorf("the edge's read and write timeouts are %s, %s and %s, want the API server's %s, %s and %s",
			edge.ReadHeaderTimeout, edge.ReadTimeout, edge.WriteTimeout, apiServer.ReadHeaderTimeout, apiServer.ReadTimeout, apiServer.WriteTimeout)
	}
	if edge.Handler == nil {
		t.Error("the edge's server has no handler")
	}
}

// edgeLogFields is every field an access-log line holds: the three the broker's JSON handler
// writes on every line (logging.NewCloudHandler), and the six the line names.
var edgeLogFields = []string{"time", "severity", "message", "method", "path", "protocol", "status", "user_agent", "x_forwarded_for"}

// edgeLogRig is the edge's server as main builds it (newEdgeServer) over a mux of the test's own,
// serving a socket of its own, with the default logger, which the answer recorder writes its
// refusal lines on, and the access log both writing the broker's own JSON lines into log.
type edgeLogRig struct {
	url string
	log *logtest.Buffer
}

// newEdgeLogRig serves mux on the edge's server, as main serves the edge's routes.
func newEdgeLogRig(t *testing.T, mux http.Handler) *edgeLogRig {
	t.Helper()
	log := &logtest.Buffer{}
	logtest.SetDefault(t, logging.NewCloudHandler(log))
	server := httptest.NewUnstartedServer(nil)
	server.Config = newEdgeServer(mux, nil, slog.Default(), &http.Server{})
	server.Start()
	t.Cleanup(server.Close)
	return &edgeLogRig{url: server.URL, log: log}
}

// lines are the access-log lines written so far, in order.
func (r *edgeLogRig) lines() []map[string]any {
	return recordsWith(r.log.String(), edgeRequestMessage, nil)
}

// send sends method target, relative to the rig's server, with headers and body, each request on
// a connection of its own, and answers the status, or 0 when no answer came. A connection of its
// own because net/http's client sends a GET again on a fresh connection when a reused one closes
// before any answer, as it does when the handler panics, and that second request writes a line too.
func (r *edgeLogRig) send(t *testing.T, method, target string, headers http.Header, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, r.url+target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range headers {
		req.Header[name] = values
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// TestTheEdgeAccessLogNamesTheRequestsFieldsAndNothingElse: one request to the edge, carrying a
// query string, a Referer, an Authorization header, a DPoP proof, a cookie and a body, each
// holding a planted value of its own that the handler reads, writes one line at INFO that names
// the method, the path, the protocol, the status, the User-Agent and X-Forwarded-For as sent, and
// holds no other field. No planted value is anywhere in what the whole chain logged.
//
// The named reverts: log r.RequestURI or r.URL.String() as the path, and the query's planted value
// is in the line; log a header the line does not name, and its planted value is; drop the access
// log from edgeHandler, and the request writes no line.
func TestTheEdgeAccessLogNamesTheRequestsFieldsAndNothingElse(t *testing.T) {
	const (
		userAgent    = "agent-secrets/1.2.3 (edge access log row)"
		forwardedFor = "198.51.100.7, 203.0.113.70, 35.191.0.1"
	)
	planted := map[string]string{
		"the query string":         "planted-query-6c1d",
		"the Referer":              "planted-referer-41a9",
		"the Authorization header": "planted-authorization-9e07",
		"the DPoP proof":           "planted-dpop-0b5f",
		"the cookie":               "planted-cookie-d382",
		"the body":                 "planted-body-7aa4",
	}
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cookie, _ := r.Cookie("session")
		seen = []string{r.URL.RawQuery, r.Referer(), r.Header.Get("Authorization"), r.Header.Get("DPoP"), cookie.String(), string(body)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"device_code":"a code"}`)
	})
	rig := newEdgeLogRig(t, mux)
	status := rig.send(t, http.MethodPost, "/v1/oauth/device_authorization?login_hint="+planted["the query string"], http.Header{
		"User-Agent":      {userAgent},
		"X-Forwarded-For": {forwardedFor},
		"Referer":         {"https://referer.example/" + planted["the Referer"]},
		"Authorization":   {"Bearer " + planted["the Authorization header"]},
		"Dpop":            {planted["the DPoP proof"]},
		"Cookie":          {"session=" + planted["the cookie"]},
		"Content-Type":    {"application/x-www-form-urlencoded"},
	}, url.Values{"login_hint": {planted["the body"]}}.Encode())
	if status != http.StatusOK {
		t.Fatalf("the device authorization answered %d, want 200", status)
	}
	// The control: each planted value reached the handler, so its absence from the log below is
	// the line leaving it out and not the request never carrying it.
	for what, value := range planted {
		if !slices.ContainsFunc(seen, func(s string) bool { return strings.Contains(s, value) }) {
			t.Fatalf("the handler never read %s's planted value %q; it read %q", what, value, seen)
		}
	}

	lines := rig.lines()
	if len(lines) != 1 {
		t.Fatalf("the request wrote %d access-log lines, want 1:\n%s", len(lines), rig.log)
	}
	line := lines[0]
	for _, row := range []struct {
		field string
		want  any
	}{
		{"severity", "INFO"},
		{"method", http.MethodPost},
		{"path", "/v1/oauth/device_authorization"},
		{"protocol", "HTTP/1.1"},
		{"status", float64(http.StatusOK)},
		{"user_agent", userAgent},
		{"x_forwarded_for", forwardedFor},
	} {
		if line[row.field] != row.want {
			t.Errorf("the line's %s is %#v, want %#v: %v", row.field, line[row.field], row.want, line)
		}
	}
	for field := range line {
		if !slices.Contains(edgeLogFields, field) {
			t.Errorf("the line holds the field %s, which an access-log line does not name: %v", field, line)
		}
	}
	for what, value := range planted {
		if strings.Contains(rig.log.String(), value) {
			t.Errorf("the log holds %s's planted value %q:\n%s", what, value, rig.log)
		}
	}
}

// TestTheEdgeAccessLogWritesALineForEveryAnswer: every request a handler of the edge is handed
// writes one line, in order, with the status the broker answered with, read once the handler has
// returned: a path the edge serves no route for (the mux's 404), a method its login path does not
// take (the mux's 405), a login a gate refuses (a 400 the answer recorder holds until the handler
// returns, and writes no refusal line for), the health check, and a handler that panics, which
// sends nothing and is written as a 500 marked aborted.
//
// The named reverts: write the line before next has returned, and the refused login's line says
// 200; drop the access log from edgeHandler, and no row writes a line; let the line go unwritten
// when the handler panics, and the last row writes none.
func TestTheEdgeAccessLogWritesALineForEveryAnswer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		// As the edge's gate refuses a person of the Workspace: before any bucket, quiet.
		answer.FromContext(r.Context()).NoteQuiet()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_request","code":"EDGE_WORKSPACE_LOGIN"}`)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /panics", func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})
	rig := newEdgeLogRig(t, mux)
	rows := []struct {
		name, method, target, path string
		status                     int
		aborted                    bool
	}{
		{"a path the edge serves no route for", http.MethodGet, "/v1/settings?token=planted-404-query", "/v1/settings", http.StatusNotFound, false},
		{"a method a login path does not take", http.MethodPut, "/v1/oauth/device_authorization", "/v1/oauth/device_authorization", http.StatusMethodNotAllowed, false},
		{"a login a gate refuses", http.MethodPost, "/v1/oauth/device_authorization", "/v1/oauth/device_authorization", http.StatusBadRequest, false},
		{"the health check", http.MethodGet, "/healthz", "/healthz", http.StatusOK, false},
		{"a handler that panics", http.MethodGet, "/panics", "/panics", http.StatusInternalServerError, true},
	}
	for _, row := range rows {
		want := row.status
		if row.aborted {
			want = 0
		}
		if got := rig.send(t, row.method, row.target, http.Header{"X-Forwarded-For": {testEdgeClientForwarding}}, ""); got != want {
			t.Fatalf("%s: answered %d, want %d (0 for no answer)", row.name, got, want)
		}
	}
	// Each line is written before the answer is sent, the panicking handler's before net/http
	// closes its connection, so every line is in the log by now.
	lines := rig.lines()
	if len(lines) != len(rows) {
		t.Fatalf("%d requests wrote %d access-log lines, want one each:\n%s", len(rows), len(lines), rig.log)
	}
	for i, row := range rows {
		line := lines[i]
		if line["method"] != row.method || line["path"] != row.path || line["status"] != float64(row.status) ||
			line["x_forwarded_for"] != testEdgeClientForwarding || line["severity"] != "INFO" {
			t.Errorf("%s: the line is %v, want %s %s with status %d at INFO", row.name, line, row.method, row.path, row.status)
		}
		if aborted, _ := line["aborted"].(bool); aborted != row.aborted {
			t.Errorf("%s: the line's aborted is %v, want %v: %v", row.name, line["aborted"], row.aborted, line)
		}
	}
	if strings.Contains(rig.log.String(), "planted-404-query") {
		t.Errorf("the log holds the 404's query string:\n%s", rig.log)
	}
}

// TestTheEdgeAccessLogCutsWhatTheCallerWrote: the method, the path and the User-Agent keep their
// first 256 bytes and X-Forwarded-For its last 256, whose end holds the entries the load balancer
// appended after what the caller wrote; X-Forwarded-For sent as several header lines is written
// as one, its lines in order joined by ", "; and a request that sends some 600 KB in those fields
// writes a line well within one Cloud Logging entry.
//
// The named reverts: cut X-Forwarded-For from its start, and the balancer's entries are lost;
// write any of the four fields uncut, and the line passes the bound.
func TestTheEdgeAccessLogCutsWhatTheCallerWrote(t *testing.T) {
	const entryLimit = 256 << 10
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	rig := newEdgeLogRig(t, mux)
	// The caller's text: not one byte repeated, so its first 256 bytes are no other 256 of it.
	sent := strings.Repeat("abcdefghij", 20000)
	forwarded := sent + ", " + testEdgeClientForwarding
	method := "M" + strings.ToUpper(sent[:1000])
	rig.send(t, http.MethodGet, "/"+sent, http.Header{"User-Agent": {sent}, "X-Forwarded-For": {forwarded}}, "")
	rig.send(t, method, "/healthz", nil, "")
	rig.send(t, http.MethodGet, "/healthz", http.Header{"X-Forwarded-For": {"198.51.100.9", testEdgeClientForwarding}}, "")

	lines := rig.lines()
	if len(lines) != 3 {
		t.Fatalf("three requests wrote %d access-log lines, want 3:\n%.2000s", len(lines), rig.log)
	}
	for _, row := range []struct {
		name, field string
		line        int
		want        string
	}{
		{"the path", "path", 0, ("/" + sent)[:256]},
		{"the User-Agent", "user_agent", 0, sent[:256]},
		{"X-Forwarded-For", "x_forwarded_for", 0, forwarded[len(forwarded)-256:]},
		{"the method", "method", 1, method[:256]},
		{"X-Forwarded-For sent as two lines", "x_forwarded_for", 2, "198.51.100.9, " + testEdgeClientForwarding},
	} {
		if got := lines[row.line][row.field]; got != row.want {
			t.Errorf("%s: the line's %s is %.80q, want %.80q", row.name, row.field, got, row.want)
		}
	}
	if forwarded := lines[0]["x_forwarded_for"].(string); !strings.HasSuffix(forwarded, ", "+testEdgeClientForwarding) {
		t.Errorf("the cut X-Forwarded-For %q lost the entries the balancer appended, %q", forwarded, testEdgeClientForwarding)
	}
	for line := range strings.Lines(rig.log.String()) {
		if len(line) >= entryLimit {
			t.Errorf("a line is %d bytes, want under the %d of one Cloud Logging entry", len(line), entryLimit)
		}
	}
}

// TestTheEdgeAccessLogPassesTheWriterOn: behind the access log a handler still flushes, through
// the answer recorder, so its caller reads what it wrote before the handler returns, and still
// reaches the server's own writer through http.ResponseController; its line is written once it
// has returned.
//
// The named reverts: drop edgeAnswer's Flush, and the caller reads nothing until the handler
// returns, so the handler gives up waiting for that read; drop its Unwrap, and SetWriteDeadline
// answers http.ErrNotSupported.
func TestTheEdgeAccessLogPassesTheWriterOn(t *testing.T) {
	read := make(chan struct{})
	released := make(chan bool, 1)
	controlled := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /streams", func(w http.ResponseWriter, _ *http.Request) {
		controlled <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Minute))
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-read:
			released <- true
		case <-time.After(5 * time.Second):
			released <- false
		}
		_, _ = io.WriteString(w, "second\n")
	})
	rig := newEdgeLogRig(t, mux)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rig.url+"/streams", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("first\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first\n" {
		t.Fatalf("read %q, %v; want the flushed first line", first, err)
	}
	close(read)
	if !<-released {
		t.Fatal("the caller read the first line only once the handler had returned: its flush did not reach the server")
	}
	rest, _ := io.ReadAll(response.Body)
	if string(rest) != "second\n" {
		t.Fatalf("read %q after the first line, want %q", rest, "second\n")
	}
	if err := <-controlled; err != nil {
		t.Errorf("SetWriteDeadline through the access log = %v, want nil", err)
	}
	if lines := rig.lines(); len(lines) != 1 || lines[0]["status"] != float64(http.StatusOK) || lines[0]["path"] != "/streams" {
		t.Errorf("the streamed answer wrote %v, want one line naming /streams with status 200", lines)
	}
}

// TestTheBinaryWritesAnAccessLogLineForEachEdgeRequest: the real binary with its edge writes one
// "broker: edge request" line at INFO for each request its edge answers, in order and with each
// answer's status: the health check, a device authorization it takes, one it refuses for naming a
// person of the Workspace (a gate refusal, which writes no refusal line), a path it serves no
// route for and a method its token path does not take. Each line names the method, the path, the
// protocol, the status, the User-Agent and X-Forwarded-For, and no planted value the requests
// carried in a query string, a Referer, an Authorization header, a DPoP proof or a cookie is
// anywhere in the broker's log. A request to the broker's own address writes no such line: nginx
// logs those.
//
// The named reverts: serve the edge without edgeHandler's access log (main), and no edge request
// writes a line; serve the API port behind it too, and its requests write lines.
func TestTheBinaryWritesAnAccessLogLineForEachEdgeRequest(t *testing.T) {
	broker := startBroker(t, brokerOptions{Env: []string{
		"BROKER_MACHINE_LOGIN_URL=" + testMachineLoginURL,
		"BROKER_DATABASE_URL=" + storetest.URL(t),
		"BROKER_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_PUBLIC_URL=http://127.0.0.1:0",
		"BROKER_FAKE_SECRETS_FILE=" + emptySecretsFile(t),
		"BROKER_EDGE_LISTEN_ADDR=127.0.0.1:0",
		"BROKER_EDGE_URL=" + testEdgeURL,
		"BROKER_EDGE_MACHINE_LOGIN_URL=" + testEdgeMachineLoginURL,
		"BROKER_SIGNIN_WORKSPACE_DOMAINS=" + ciphertest.EdgeWorkspaceDomain,
	}})
	edgeAddr, _ := broker.AwaitRecord(t, "broker edge listening", nil)["addr"].(string)
	if edgeAddr == "" {
		t.Fatalf("the edge's line names no addr:\n%s", broker.Log())
	}
	edge := "http://" + edgeAddr
	const userAgent = "agent-secrets/1.2.3 (edge access log binary row)"
	planted := []string{"planted-query-5e21", "planted-referer-c07b", "planted-authorization-3f98", "planted-dpop-a6d4", "planted-cookie-81ce"}
	// Two requests to the broker's own address, before the edge's: neither writes an edge line, so
	// the first of the lines read below is the edge's first request's.
	requireHealthy(t, broker.Addr)
	statusOf(t, http.MethodGet, "http://"+broker.Addr+"/v1/settings?token="+planted[0])
	carrying := map[string]string{
		"X-Forwarded-For": testEdgeClientForwarding,
		"User-Agent":      userAgent,
		"Referer":         "https://referer.example/" + planted[1],
		"DPoP":            planted[3],
		"Cookie":          "session=" + planted[4],
	}
	requireHealthy(t, edgeAddr)
	if status, body := postDeviceAuthorization(t, edge, testEdgeURL, "outside@outside.example", carrying); status != http.StatusOK {
		t.Fatalf("a device authorization at the edge = %d %v, want 200", status, body)
	}
	// The OAuth library reads an Authorization header as the client's own authentication and
	// answers a device authorization carrying a bearer 401 invalid_client, so the refused login
	// carries it: the edge's gate refuses that before the library reads it.
	carrying["Authorization"] = "Bearer " + planted[2]
	if status, body := postDeviceAuthorization(t, edge, testEdgeURL, "ada@"+ciphertest.SignInDomain, carrying); status != http.StatusBadRequest || body["code"] != "EDGE_WORKSPACE_LOGIN" {
		t.Fatalf("a device authorization at the edge naming a person of the Workspace = %d %v, want 400 EDGE_WORKSPACE_LOGIN", status, body)
	}
	if status := statusOf(t, http.MethodGet, edge+"/v1/settings?token="+planted[0]); status != http.StatusNotFound {
		t.Fatalf("GET /v1/settings at the edge = %d, want 404", status)
	}
	if status := statusOf(t, http.MethodDelete, edge+"/v1/oauth/token"); status != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /v1/oauth/token at the edge = %d, want 405", status)
	}

	// Each line is written before its answer is sent, so once the last request's line has
	// arrived every line before it has too.
	broker.AwaitRecord(t, edgeRequestMessage, map[string]any{"method": http.MethodDelete, "status": http.StatusMethodNotAllowed})
	log := broker.Log()
	lines := recordsWith(log, edgeRequestMessage, nil)
	goClient := "Go-http-client/1.1"
	want := []map[string]any{
		{"method": http.MethodGet, "path": "/healthz", "status": float64(http.StatusOK), "user_agent": goClient, "x_forwarded_for": ""},
		{"method": http.MethodPost, "path": "/v1/oauth/device_authorization", "status": float64(http.StatusOK), "user_agent": userAgent, "x_forwarded_for": testEdgeClientForwarding},
		{"method": http.MethodPost, "path": "/v1/oauth/device_authorization", "status": float64(http.StatusBadRequest), "user_agent": userAgent, "x_forwarded_for": testEdgeClientForwarding},
		{"method": http.MethodGet, "path": "/v1/settings", "status": float64(http.StatusNotFound), "user_agent": goClient, "x_forwarded_for": ""},
		{"method": http.MethodDelete, "path": "/v1/oauth/token", "status": float64(http.StatusMethodNotAllowed), "user_agent": goClient, "x_forwarded_for": ""},
	}
	if len(lines) != len(want) {
		t.Fatalf("the edge's %d requests wrote %d %q lines, want one each:\n%s", len(want), len(lines), edgeRequestMessage, log)
	}
	for i, fields := range want {
		fields["severity"], fields["protocol"] = "INFO", "HTTP/1.1"
		if !carries(lines[i], fields) {
			t.Errorf("edge request %d's line is %v, want it to carry %v", i+1, lines[i], fields)
		}
		for field := range lines[i] {
			if !slices.Contains(edgeLogFields, field) {
				t.Errorf("edge request %d's line holds the field %s, which an access-log line does not name: %v", i+1, field, lines[i])
			}
		}
	}
	for _, value := range planted {
		if strings.Contains(log, value) {
			t.Errorf("the broker's log holds the planted value %q:\n%s", value, log)
		}
	}
	if refusals := recordsWith(log, answer.RefusalMessage, map[string]any{"port": answer.PortEdge}); len(refusals) != 0 {
		t.Errorf("the edge wrote refusal lines %v, want none: its 404, 405 and gate refusal write their access-log line alone", refusals)
	}
}
