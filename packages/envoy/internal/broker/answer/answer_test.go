package answer_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/answer"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/metrics"
)

// idPattern is an answer id: 16 random bytes in hex.
var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// rig is one recorded handler: the logger its lines go to and the registry it counts on.
type rig struct {
	log      *logtest.Recorder
	registry *metrics.Registry
	handler  http.Handler
}

// newRig records next as cmd/broker does, behind a handler that hands the mux a request of its
// own (as withRequestDeadline does) through answer.WithContext.
func newRig(t *testing.T, next http.Handler) *rig {
	t.Helper()
	r := &rig{log: &logtest.Recorder{}, registry: metrics.New()}
	logtest.SetDefault(t, r.log)
	deadline := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		next.ServeHTTP(w, answer.WithContext(req, ctx))
	})
	r.handler = answer.Middleware(deadline, answer.NewAnswers(r.registry))
	return r
}

// serve answers req and returns the answer as a client reads it.
func (r *rig) serve(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.handler.ServeHTTP(w, req)
	return w
}

// lines are the refusal lines written so far, each attribute by its key.
func (r *rig) lines(t *testing.T) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, record := range r.log.Records() {
		if record.Message != answer.RefusalMessage {
			continue
		}
		if record.Level != slog.LevelWarn {
			t.Errorf("a refusal line is at %v, want WARN", record.Level)
		}
		line := map[string]any{}
		record.Attrs(func(a slog.Attr) bool { line[a.Key] = a.Value.Any(); return true })
		lines = append(lines, line)
	}
	return lines
}

// count is the sample of broker_answers_total naming labels, 0 when the scrape holds none.
func (r *rig) count(t *testing.T, labels string) string {
	t.Helper()
	w := httptest.NewRecorder()
	r.registry.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(w.Body.String()) {
		if value, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "broker_answers_total{"+labels+"} "); ok {
			return value
		}
	}
	return "0"
}

// writeJSON answers status with the body text, as the broker's JSON writers do.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body+"\n")
}

// An answer that is no refusal carries its own id in its header, its body as the handler wrote
// it, and is counted served under HTTP_<status>; it writes no line.
func TestEveryAnswerCarriesAnIDOfItsOwn(t *testing.T) {
	r := newRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"served":true}`)
	}))
	first := r.serve(httptest.NewRequest(http.MethodGet, "/", nil))
	second := r.serve(httptest.NewRequest(http.MethodGet, "/", nil))
	for _, w := range []*httptest.ResponseRecorder{first, second} {
		if id := w.Header().Get(answer.Header); !idPattern.MatchString(id) {
			t.Errorf("%s = %q, want 32 hex digits", answer.Header, id)
		}
		if w.Code != http.StatusOK || w.Body.String() != "{\"served\":true}\n" {
			t.Errorf("answer = %d %q, want 200 and the body as written", w.Code, w.Body)
		}
	}
	if first.Header().Get(answer.Header) == second.Header().Get(answer.Header) {
		t.Errorf("two answers carry the same id %s", first.Header().Get(answer.Header))
	}
	if lines := r.lines(t); len(lines) != 0 {
		t.Errorf("answers that refused nothing wrote %v", lines)
	}
	if got := r.count(t, `outcome="served",code="HTTP_200",caller_class="public"`); got != "2" {
		t.Errorf("served answers counted %s, want 2", got)
	}

	// A handler that writes nothing answers 200 all the same, with its id.
	empty := newRig(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if w := empty.serve(httptest.NewRequest(http.MethodGet, "/", nil)); w.Code != http.StatusOK || !idPattern.MatchString(w.Header().Get(answer.Header)) {
		t.Errorf("an empty answer = %d with id %q, want 200 with one", w.Code, w.Header().Get(answer.Header))
	}
}

// Each refusal writes one line at WARN naming its code - read from the body's code, else its
// error, else the code its writer noted, else HTTP_<status> - and the answer id its header
// carries, and a JSON object body gains answer_id, its length dropped since the body grew. The
// line holds no word of the refusal's message.
func TestARefusalWritesOneLineNamingItsCode(t *testing.T) {
	const message = "the words the caller reads"
	driven := 0
	for _, row := range []struct {
		name     string
		status   int
		header   string
		body     string
		noted    string
		wantCode string
		// wantBody is the body the client reads, with ID standing for the answer id.
		wantBody string
	}{
		{"the broker's error object", http.StatusForbidden, "application/json", `{"code":"NOT_YOURS","error":"` + message + `"}`, "", "NOT_YOURS",
			`{"code":"NOT_YOURS","error":"` + message + `","answer_id":"ID"}`},
		{"an OAuth error", http.StatusBadRequest, "application/json;charset=UTF-8", `{"error":"invalid_grant","error_description":"` + message + `"}`, "", "invalid_grant",
			`{"error":"invalid_grant","error_description":"` + message + `","answer_id":"ID"}`},
		{"a body's code over the one noted", http.StatusBadRequest, "application/json", `{"code":"FROM_BODY","error":"` + message + `"}`, "NOTED", "FROM_BODY",
			`{"code":"FROM_BODY","error":"` + message + `","answer_id":"ID"}`},
		{"an empty object", http.StatusBadRequest, "application/json", `{ }`, "", "HTTP_400", `{ "answer_id":"ID"}`},
		{"an object that names its answer already", http.StatusConflict, "application/json", `{"code":"TAKEN","answer_id":"another"}`, "", "TAKEN",
			`{"code":"TAKEN","answer_id":"another"}`},
		{"a page whose writer noted its code", http.StatusForbidden, "text/html; charset=utf-8", "<p>" + message + "</p>", "ORIGIN_REFUSED", "ORIGIN_REFUSED",
			"<p>" + message + "</p>"},
		{"text with no code", http.StatusNotFound, "text/plain; charset=utf-8", "404 page not found", "", "HTTP_404", "404 page not found"},
		{"a code that is free text", http.StatusBadRequest, "application/json", `{"code":"not a code","error":"` + message + `"}`, "", "HTTP_400",
			`{"code":"not a code","error":"` + message + `","answer_id":"ID"}`},
		{"JSON text that is no object", http.StatusBadRequest, "application/json", `["NOT_AN_OBJECT"]`, "", "HTTP_400", `["NOT_AN_OBJECT"]`},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			r := newRig(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rec := answer.FromContext(req.Context())
				rec.NoteCaller(answer.CallerSession, "enrollment-1")
				rec.NoteSecrets("FIRST_SECRET", "SECOND_SECRET")
				if row.noted != "" {
					answer.FromWriter(w).NoteCode(row.noted)
				}
				w.Header().Set("Content-Type", row.header)
				w.Header().Set("Content-Length", "1")
				w.WriteHeader(row.status)
				_, _ = io.WriteString(w, row.body+"\n")
			}))
			w := r.serve(httptest.NewRequest(http.MethodPost, "/v1/somewhere", nil))
			id := w.Header().Get(answer.Header)
			if !idPattern.MatchString(id) {
				t.Fatalf("%s = %q, want 32 hex digits", answer.Header, id)
			}
			if want := strings.ReplaceAll(row.wantBody, "ID", id) + "\n"; w.Code != row.status || w.Body.String() != want {
				t.Errorf("answer = %d %q, want %d %q", w.Code, w.Body, row.status, want)
			}
			if got := w.Header().Get("Content-Length"); got != "" {
				t.Errorf("the answer keeps the handler's Content-Length %s", got)
			}
			lines := r.lines(t)
			if len(lines) != 1 {
				t.Fatalf("one refusal wrote %d lines, want 1: %v", len(lines), lines)
			}
			line := lines[0]
			want := map[string]any{
				"answer_id": id, "status": int64(row.status), "code": row.wantCode, "method": http.MethodPost, "route": "",
				"caller_class": answer.CallerSession, "caller": "enrollment-1", "secrets": []string{"FIRST_SECRET", "SECOND_SECRET"},
			}
			for key, value := range want {
				if got, err := json.Marshal(line[key]); err != nil || string(got) != mustJSON(t, value) {
					t.Errorf("the line's %s = %v, want %v (line %v)", key, line[key], value, line)
				}
			}
			if len(line) != len(want) {
				t.Errorf("the line carries %v, want exactly the fields %v", line, want)
			}
			for key, value := range line {
				if strings.Contains(mustJSON(t, value), message) {
					t.Errorf("the line's %s quotes the refusal's message: %v", key, value)
				}
			}
			if got := r.count(t, `outcome="refused",code="`+row.wantCode+`",caller_class="session"`); got != "1" {
				t.Errorf("the refusal counted %s, want 1", got)
			}
		})
	}
	if driven != 9 {
		t.Fatalf("the table drove %d refusals, want 9", driven)
	}
}

// mustJSON is value as JSON text.
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	text, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

// A 429, the OAuth device grant's slow_down and authorization_pending, a 500 and a 503 each write
// no line and are counted under their own outcome; each still carries its id.
func TestTheAnswersThatWriteNoLineAreCounted(t *testing.T) {
	driven := 0
	for _, row := range []struct {
		name, body, labels string
		status             int
	}{
		{"a 429", `{"code":"RATE_LIMITED","error":"too many"}`, `outcome="rate_limited",code="RATE_LIMITED"`, http.StatusTooManyRequests},
		{"slow_down", `{"error":"slow_down"}`, `outcome="rate_limited",code="slow_down"`, http.StatusBadRequest},
		{"authorization_pending", `{"error":"authorization_pending"}`, `outcome="pending",code="authorization_pending"`, http.StatusBadRequest},
		{"a 500", `{"code":"INTERNAL","error":"read failed"}`, `outcome="failed",code="HTTP_500"`, http.StatusInternalServerError},
		{"a 503", `{"code":"DATABASE_UNAVAILABLE","error":"ping failed"}`, `outcome="failed",code="HTTP_503"`, http.StatusServiceUnavailable},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			r := newRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, row.status, row.body)
			}))
			w := r.serve(httptest.NewRequest(http.MethodPost, "/", nil))
			if w.Code != row.status || !idPattern.MatchString(w.Header().Get(answer.Header)) {
				t.Errorf("answer = %d with id %q, want %d with one", w.Code, w.Header().Get(answer.Header), row.status)
			}
			if lines := r.lines(t); len(lines) != 0 {
				t.Errorf("%s wrote %v, want no line", row.name, lines)
			}
			if got := r.count(t, row.labels+`,caller_class="public"`); got != "1" {
				t.Errorf("%s counted %s under %s, want 1", row.name, got, row.labels)
			}
		})
	}
	if driven != 5 {
		t.Fatalf("the table drove %d answers, want 5", driven)
	}
}

// A code a writer noted for the refusal it was about to write names no other answer: a writer
// that notes one and then answers 500 instead, as the approval page does when its template
// fails, is counted failed under HTTP_500, and writes no line.
func TestACodeNotedForARefusalNamesNoOtherAnswer(t *testing.T) {
	r := newRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		answer.FromWriter(w).NoteCode("RECORD_ID_INPUT")
		http.Error(w, "the approval page failed to render", http.StatusInternalServerError)
	}))
	if w := r.serve(httptest.NewRequest(http.MethodGet, "/", nil)); w.Code != http.StatusInternalServerError {
		t.Fatalf("answer = %d, want 500", w.Code)
	}
	if lines := r.lines(t); len(lines) != 0 {
		t.Errorf("a 500 wrote %v, want no line", lines)
	}
	if got := r.count(t, `outcome="failed",code="HTTP_500",caller_class="public"`); got != "1" {
		t.Errorf("the 500 counted %s under HTTP_500, want 1", got)
	}
	if got := r.count(t, `outcome="failed",code="RECORD_ID_INPUT",caller_class="public"`); got != "0" {
		t.Errorf("the 500 counted %s under the code noted for a refusal, want 0", got)
	}
}

// The line names the pattern of the route the mux matched, read from the request the mux was
// handed through a middleware between it and the recorder, and an empty route for the mux's own
// 404 and 405. A caller's X-Correlation-Id is kept as its printable ASCII, at most 256 bytes.
func TestTheLineNamesTheRouteAndTheCallersCorrelationID(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/things/{name}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, `{"code":"THING_INVALID","error":"no"}`)
	})
	r := newRig(t, mux)
	sent := "abc\x01\tdef" + strings.Repeat("x", 300)
	for _, call := range []struct{ method, path, correlation string }{
		{http.MethodPost, "/v1/things/one", sent},
		{http.MethodGet, "/v1/nothing", ""},
		{http.MethodGet, "/v1/things/one", ""},
	} {
		req := httptest.NewRequest(call.method, call.path, nil)
		if call.correlation != "" {
			req.Header.Set("X-Correlation-Id", call.correlation)
		}
		r.serve(req)
	}
	lines := r.lines(t)
	if len(lines) != 3 {
		t.Fatalf("three refusals wrote %d lines: %v", len(lines), lines)
	}
	wantCorrelation := ("abcdef" + strings.Repeat("x", 300))[:256]
	for i, want := range []map[string]any{
		{"route": "POST /v1/things/{name}", "code": "THING_INVALID", "status": int64(400), "correlation_id": wantCorrelation},
		{"route": "", "code": "HTTP_404", "status": int64(404)},
		{"route": "", "code": "HTTP_405", "status": int64(405)},
	} {
		for key, value := range want {
			if lines[i][key] != value {
				t.Errorf("line %d's %s = %v, want %v", i, key, lines[i][key], value)
			}
		}
		if _, ok := want["correlation_id"]; !ok {
			if got, sent := lines[i]["correlation_id"]; sent {
				t.Errorf("line %d carries correlation_id %v for a caller that sent none", i, got)
			}
		}
	}
}

// A refusal's line lists the secret names its request named in the request's order, up to 32 KiB
// of names, and says how many the request named (secrets_count) and, when it left names out, how
// many it left out (secrets_omitted). Cloud Logging refuses an entry over 256 KiB, and GKE drops a
// jsonPayload entry over that size, so a line that listed every name of a request naming
// thousands would leave that refusal with no record at all (ca-refusal-record, Verification v9).
// A request below the bound still lists every name, with its count and no secrets_omitted. The
// line is read as the broker writes it, through its own JSON handler, so its size is the entry's.
//
// The named revert: list every name the request noted, with no bound and no counts (fbaa5626);
// the row of 4,000 names then writes a line of about a MiB, and this test fails.
func TestARefusalsLineBoundsTheNamesItLists(t *testing.T) {
	const (
		nameBytes  = 256
		listBudget = 32 << 10
		entryLimit = 256 << 10
	)
	named := make([]string, 4000)
	for i := range named {
		prefix := fmt.Sprintf("NAME_%04d_", i)
		named[i] = prefix + strings.Repeat("X", nameBytes-len(prefix))
	}
	driven := 0
	for _, row := range []struct {
		name  string
		names []string
	}{
		{"4,000 names of 256 bytes", named},
		{"three names", named[:3]},
	} {
		t.Run(row.name, func(t *testing.T) {
			driven++
			out := &logtest.Buffer{}
			logtest.SetDefault(t, logging.NewCloudHandler(out))
			handler := answer.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				answer.FromContext(req.Context()).NoteSecrets(row.names...)
				writeJSON(w, http.StatusBadRequest, `{"code":"NAMES_REFUSED","error":"no"}`)
			}), nil)
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/requests", nil))

			var refusals []string
			for line := range strings.Lines(out.String()) {
				if strings.Contains(line, `"message":"`+answer.RefusalMessage+`"`) {
					refusals = append(refusals, line)
				}
			}
			if len(refusals) != 1 {
				t.Fatalf("one refusal wrote %d lines under %q, want 1", len(refusals), answer.RefusalMessage)
			}
			text := refusals[0]
			if len(text) >= entryLimit {
				t.Fatalf("the refusal's line is %d bytes, want under the %d bytes of one Cloud Logging entry", len(text), entryLimit)
			}
			var line struct {
				Secrets  []string `json:"secrets"`
				Count    *int     `json:"secrets_count"`
				Omitted  *int     `json:"secrets_omitted"`
				Severity string   `json:"severity"`
			}
			if err := json.Unmarshal([]byte(text), &line); err != nil {
				t.Fatalf("the refusal's line is not one JSON object: %v", err)
			}
			if line.Count == nil || *line.Count != len(row.names) {
				t.Fatalf("secrets_count = %v, want %d, the names the request named", line.Count, len(row.names))
			}
			if len(line.Secrets) == 0 || len(line.Secrets) > len(row.names) || !slices.Equal(line.Secrets, row.names[:len(line.Secrets)]) {
				t.Fatalf("the line lists %d names, want the first names the request named, in its order", len(line.Secrets))
			}
			listed := 0
			for _, name := range line.Secrets {
				listed += len(name)
			}
			if listed > listBudget {
				t.Fatalf("the line lists %d bytes of names, want at most %d", listed, listBudget)
			}
			left := len(row.names) - len(line.Secrets)
			switch {
			case left == 0 && line.Omitted != nil:
				t.Fatalf("a line that lists every name carries secrets_omitted %d", *line.Omitted)
			case left > 0 && (line.Omitted == nil || *line.Omitted != left):
				t.Fatalf("secrets_omitted = %v, want %d, the names the line left out", line.Omitted, left)
			case left > 0 && listed+2*nameBytes <= listBudget:
				t.Fatalf("the line left %d names out but lists only %d bytes of the %d it may", left, listed, listBudget)
			}
			if len(row.names) <= 3 && left != 0 {
				t.Fatalf("a request of %d names below the bound lists %d of them", len(row.names), len(line.Secrets))
			}
		})
	}
	if driven != 2 {
		t.Fatalf("the table drove %d rows, want 2", driven)
	}
}

// unwrapping is a writer a handler wraps the recorder in, as http.ResponseController expects.
type unwrapping struct{ http.ResponseWriter }

func (u unwrapping) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// A handler reaches the recorder through its request's context and through a writer that wraps
// it, and one that runs behind none notes nothing and does not fail.
func TestAHandlerReachesTheRecorderItRunsBehind(t *testing.T) {
	r := newRig(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		wrapped := unwrapping{w}
		if answer.FromWriter(wrapped) != answer.FromContext(req.Context()) || answer.FromWriter(wrapped) == nil {
			t.Error("the writer and the context reach different recorders")
		}
		answer.FromWriter(wrapped).NoteCode("NOTED_THROUGH_A_WRAPPER")
		wrapped.WriteHeader(http.StatusForbidden)
	}))
	w := r.serve(httptest.NewRequest(http.MethodGet, "/", nil))
	if lines := r.lines(t); len(lines) != 1 || lines[0]["code"] != "NOTED_THROUGH_A_WRAPPER" || lines[0]["answer_id"] != w.Header().Get(answer.Header) {
		t.Errorf("lines = %v, want one naming the noted code and the answer's id", lines)
	}

	none := answer.FromWriter(httptest.NewRecorder())
	if none != nil || answer.FromContext(context.Background()) != nil {
		t.Fatal("a request no middleware records reached a recorder")
	}
	none.NoteCaller(answer.CallerAccount, "dispatch")
	none.NoteSecrets("A_SECRET")
	none.NoteCode("A_CODE")
	if none.ID() != "" {
		t.Errorf("no recorder answers the id %q", none.ID())
	}
}

// A refusal streamed with io.Copy, from a reader that is no io.WriterTo so the copy reaches the
// recorder's ReadFrom, is held and gains its answer_id as one written with Write does; a handler
// that flushes its refusal sends it at once, and the line still names its code from what it had
// written. A connection a handler takes is the handler's, the answer counted, once the handler
// has returned, as the 101 an upgrade is.
func TestARecordedAnswerStillStreamsFlushesAndHijacks(t *testing.T) {
	copied := newRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_, _ = io.Copy(w, struct{ io.Reader }{strings.NewReader(`{"code":"GONE"}`)})
	}))
	w := copied.serve(httptest.NewRequest(http.MethodGet, "/", nil))
	if want := `{"code":"GONE","answer_id":"` + w.Header().Get(answer.Header) + `"}`; w.Body.String() != want {
		t.Errorf("a copied refusal = %q, want %q", w.Body, want)
	}

	flushed := newRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"code":"STREAMED",`)
		w.(http.Flusher).Flush()
		// The server's writer holds what was written before the flush, sent on by it.
		inner, _ := answer.FromWriter(w).Unwrap().(*httptest.ResponseRecorder)
		if inner == nil || !inner.Flushed || inner.Code != http.StatusForbidden || inner.Body.String() != `{"code":"STREAMED",` {
			t.Error("the flush did not send the refusal written so far to the server's writer")
		}
		_, _ = io.WriteString(w, `"error":"the rest"}`)
	}))
	w = flushed.serve(httptest.NewRequest(http.MethodGet, "/", nil))
	if !w.Flushed || w.Code != http.StatusForbidden || w.Body.String() != `{"code":"STREAMED","error":"the rest"}` {
		t.Errorf("a flushed refusal = %d %q (flushed %v), want 403 as written, flushed", w.Code, w.Body, w.Flushed)
	}
	if lines := flushed.lines(t); len(lines) != 1 || lines[0]["code"] != "HTTP_403" {
		t.Errorf("lines = %v, want one naming HTTP_403, the code of a body cut at its flush", lines)
	}

	hijacked := newRig(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Errorf("hijack through the recorder: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: close\r\n\r\n")
		_ = buffered.Flush()
	}))
	// returned is closed once the recorder has returned, so it has counted the answer.
	returned := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer close(returned)
		hijacked.handler.ServeHTTP(w, req)
	}))
	defer server.Close()
	conn, err := (&net.Dialer{}).Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || status != "HTTP/1.1 101 Switching Protocols\r\n" {
		t.Fatalf("the hijacked connection answered %q, %v", status, err)
	}
	<-returned
	if got := hijacked.count(t, `outcome="served",code="HTTP_101",caller_class="public"`); got != "1" {
		t.Errorf("a hijacked answer counted %s served HTTP_101 answers, want 1", got)
	}
}

// A handler that panics is counted failed, and its panic goes on to the server.
func TestAPanickingHandlerIsCountedFailed(t *testing.T) {
	r := newRig(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Errorf("the panic reached the server as %v, want http.ErrAbortHandler", recovered)
			}
		}()
		r.serve(httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if got := r.count(t, `outcome="failed",code="HTTP_500",caller_class="public"`); got != "1" {
		t.Errorf("a panicking handler counted %s failed answers, want 1", got)
	}
}
