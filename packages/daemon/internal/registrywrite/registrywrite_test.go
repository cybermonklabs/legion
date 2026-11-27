package registrywrite

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

// listener answers every write with the status and body the test sets for its path, and records
// each write it took.
type listener struct {
	mu      sync.Mutex
	status  map[string]int
	answer  map[string]string
	written []written
}

type written struct {
	method, path, contentType, authorization, body string
}

func newListener(t *testing.T) (*listener, *httptest.Server) {
	t.Helper()
	l := &listener{status: map[string]int{}, answer: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		defer l.mu.Unlock()
		l.written = append(l.written, written{r.Method, r.URL.EscapedPath(), r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(body)})
		status := l.status[r.URL.EscapedPath()]
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(l.answer[r.URL.EscapedPath()]))
	}))
	t.Cleanup(server.Close)
	return l, server
}

func (l *listener) set(path string, status int, answer string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status[path], l.answer[path] = status, answer
}

func (l *listener) writes() []written {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]written(nil), l.written...)
}

// logged is a Writer's log, one line per record.
type logged struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logged) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logged) lines(containing ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		matches := line != ""
		for _, part := range containing {
			matches = matches && strings.Contains(line, part)
		}
		if matches {
			found = append(found, line)
		}
	}
	return found
}

func newWriter(server *httptest.Server, token string) (*Writer, *logged) {
	log := &logged{}
	return New(server.URL+"/", token, nil, slog.New(slog.NewTextHandler(log, nil))), log
}

// Each write is a PUT of its listenerwire body, as JSON, to its route, with the listener's bearer
// when the daemon has one and none when it has not.
func TestEachWriteIsAPutOfItsBodyToItsRoute(t *testing.T) {
	l, server := newListener(t)
	writer, _ := newWriter(server, "envoy-bearer")
	entry := listenerwire.LegionAgentEntry{SessionID: "ses-1", LegionProject: "acme", Issue: "ACME-7", Role: "tester",
		RoleToken: "legion-acme-acme-7-tester", Title: "Legion tester · ACME-7", Harness: "omp", StartedAt: "2026-10-20T09:00:00.000Z"}
	answering := true
	ctx := context.Background()
	for _, outcome := range []Outcome{
		writer.Batch(ctx, listenerwire.LegionAgentBatch{Entries: []listenerwire.LegionAgentEntry{entry}}),
		writer.Entry(ctx, entry),
		writer.Answering(ctx, "acme", listenerwire.LegionProjectRequest{Answering: &answering, UpdatedAt: "2026-10-26T12:00:00.000Z"}),
	} {
		if outcome != Sent {
			t.Fatalf("a write the listener took came to %v, want Sent", outcome)
		}
	}
	entryJSON := `{"session_id":"ses-1","legion_project":"acme","issue":"ACME-7","role":"tester","role_token":"legion-acme-acme-7-tester","title":"Legion tester · ACME-7","harness":"omp","started_at":"2026-10-20T09:00:00.000Z"}`
	want := []written{
		{http.MethodPut, "/v1/legion/agents", "application/json", "Bearer envoy-bearer", `{"entries":[` + entryJSON + `]}`},
		{http.MethodPut, "/v1/legion/agents/sessions/ses-1", "application/json", "Bearer envoy-bearer", entryJSON},
		{http.MethodPut, "/v1/legion/projects/acme", "application/json", "Bearer envoy-bearer", `{"answering":true,"updated_at":"2026-10-26T12:00:00.000Z"}`},
	}
	got := l.writes()
	if len(got) != len(want) {
		t.Fatalf("the listener took %d writes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("write %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	none, _ := newWriter(server, "")
	if outcome := none.Entry(ctx, entry); outcome != Sent {
		t.Fatalf("a write with no bearer came to %v, want Sent", outcome)
	}
	if last := l.writes()[len(want)]; last.authorization != "" {
		t.Errorf("a daemon with no bearer sent Authorization %q, want none", last.authorization)
	}
}

// What a write comes to is its answer's status: a 2xx is Sent; a 404, a listener that predates
// the route, is NotFound, logged once for each route however often it answers so; a 400 or 409 is
// Refused, logged with the status and the listener's answer, bounded to 4096 bytes; any other
// status is Failed, logged the same way; and a write no listener answered is Failed, logged unless
// its own context ended.
func TestAWritesOutcomeIsItsAnswersStatus(t *testing.T) {
	l, server := newListener(t)
	writer, log := newWriter(server, "envoy-bearer")
	ctx := context.Background()
	entry := func(id string) listenerwire.LegionAgentEntry { return listenerwire.LegionAgentEntry{SessionID: id} }

	for _, tc := range []struct {
		status int
		want   Outcome
	}{
		{http.StatusOK, Sent},
		{http.StatusNoContent, Sent},
		{http.StatusNotFound, NotFound},
		{http.StatusBadRequest, Refused},
		{http.StatusConflict, Refused},
		{http.StatusRequestEntityTooLarge, Failed},
		{http.StatusInternalServerError, Failed},
		{http.StatusServiceUnavailable, Failed},
	} {
		id := "ses-" + http.StatusText(tc.status)
		id = strings.ReplaceAll(id, " ", "-")
		l.set("/v1/legion/agents/sessions/"+id, tc.status, "")
		if got := writer.Entry(ctx, entry(id)); got != tc.want {
			t.Errorf("a write answered %d came to %v, want %v", tc.status, got, tc.want)
		}
	}

	// A route's 404 is logged once, however often it answers so; another route's is its own.
	l.set("/v1/legion/agents", http.StatusNotFound, "404 page not found")
	l.set("/v1/legion/projects/acme", http.StatusNotFound, "404 page not found")
	for range 3 {
		if got := writer.Batch(ctx, listenerwire.LegionAgentBatch{}); got != NotFound {
			t.Fatalf("a batch answered 404 came to %v, want NotFound", got)
		}
	}
	if got := writer.Answering(ctx, "acme", listenerwire.LegionProjectRequest{}); got != NotFound {
		t.Fatalf("a beat answered 404 came to %v, want NotFound", got)
	}
	for route, want := range map[string]int{
		"PUT /v1/legion/agents/sessions/{id}": 1,
		"PUT /v1/legion/agents":               1,
		"PUT /v1/legion/projects/{project}":   1,
	} {
		if lines := log.lines("the Envoy listener answers 404 on " + route + ": it predates the Legion agent registry; the daemon tries again each minute"); len(lines) != want {
			t.Errorf("%s's 404 was logged %d times, want %d:\n%s", route, len(lines), want, strings.Join(log.lines(), "\n"))
		}
	}

	// A refusal names the status and the listener's answer, cut at 4096 bytes.
	long := "refused: " + strings.Repeat("x", 5000)
	l.set("/v1/legion/agents/sessions/ses-long", http.StatusConflict, long)
	if got := writer.Entry(ctx, entry("ses-long")); got != Refused {
		t.Fatalf("a write answered 409 came to %v, want Refused", got)
	}
	refusals := log.lines("the Envoy listener refused an agent registry write; it is not sent again", "path=/v1/legion/agents/sessions/ses-long", "status=409")
	if len(refusals) != 1 || !strings.Contains(refusals[0], long[:4096]) || strings.Contains(refusals[0], long[:4097]) {
		t.Errorf("the 409 was logged as %q, want one line naming its answer's first 4096 bytes", refusals)
	}
	if lines := log.lines("the Envoy listener failed an agent registry write; trying again", "status=500"); len(lines) != 1 {
		t.Errorf("the 500 was logged %d times, want once:\n%s", len(lines), strings.Join(log.lines(), "\n"))
	}

	// No listener answers: Failed, and logged unless the write's own context ended.
	server.Close()
	if got := writer.Entry(ctx, entry("ses-gone")); got != Failed {
		t.Fatalf("a write no listener answered came to %v, want Failed", got)
	}
	if lines := log.lines("legion daemon could not write to the agent registry; trying again"); len(lines) != 1 {
		t.Errorf("the transport failure was logged %d times, want once", len(lines))
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got := writer.Entry(cancelled, entry("ses-cancelled")); got != Failed {
		t.Fatalf("a write whose context ended came to %v, want Failed", got)
	}
	if lines := log.lines("legion daemon could not write to the agent registry; trying again"); len(lines) != 1 {
		t.Errorf("a write whose context ended was logged as a failure (%d lines), want no line of its own", len(lines))
	}
}

// The listener's 500 for a stored entry it cannot decode carries the code
// listenerwire.CodeMalformedRecord, and a write it answers so is Refused, so the mirror drops that
// one entry and goes on with the rest. A session's own write, one PUT of its entry or a batch of
// that entry alone, is logged at ERROR the first time, naming the session's key, the listener's
// cause and the count, the daemon's only count of them, and is counted once however often the
// listener refuses it; another session is a line and a count of its own. A batch of several
// entries is Refused and logged as a 400 or 409 is, and counted never: the mirror sends each of its
// entries alone, and the bad one is counted there. The controls are each other answer, a 500 with
// no code, with another code or with a body that is not JSON, and a 503 with no code or this one:
// each is Failed, logged as a write to try again, and counted never, so the batch's bad entry sent
// alone after them is the third. A writer that read no code fails the first half; one that refused
// every 500, or read the code on any status, fails the controls.
func TestAStoredEntryTheListenerCannotDecodeIsRefusedLoggedOnceAndCounted(t *testing.T) {
	l, server := newListener(t)
	writer, log := newWriter(server, "envoy-bearer")
	ctx := context.Background()
	entry := func(id string) listenerwire.LegionAgentEntry { return listenerwire.LegionAgentEntry{SessionID: id} }
	answer := func(code, message string) string {
		encoded, err := json.Marshal(listenerwire.APIError{Code: code, Error: message})
		if err != nil {
			t.Fatalf("encode the answer: %v", err)
		}
		return string(encoded)
	}
	const head = "envoy: the legion agent registry holds a record it cannot decode: "
	cause := func(id string) string {
		return "malformed legion agent registry record: legion agent " + id + ": invalid character 'o' in literal null (expecting 'u')"
	}
	errorLines := func(id string) []string {
		return log.lines("level=ERROR", "the Envoy listener holds an agent registry entry it cannot decode", "session="+id+" ", "key=session."+id+" ")
	}

	// ses-bad's write as a batch of itself alone, as the mirror sends a refused batch's entries,
	// then twice as its own PUT: Refused each time, logged and counted the first.
	l.set("/v1/legion/agents", listenerwire.FaultStatus, answer(listenerwire.CodeMalformedRecord, head+"wrote 0 of 1 legion agents, then entries[0]: "+cause("ses-bad")))
	if got := writer.Batch(ctx, listenerwire.LegionAgentBatch{Entries: []listenerwire.LegionAgentEntry{entry("ses-bad")}}); got != Refused {
		t.Fatalf("a batch of ses-bad alone answered 500 %s came to %v, want Refused", listenerwire.CodeMalformedRecord, got)
	}
	l.set("/v1/legion/agents/sessions/ses-bad", listenerwire.FaultStatus, answer(listenerwire.CodeMalformedRecord, head+cause("ses-bad")))
	for range 2 {
		if got := writer.Entry(ctx, entry("ses-bad")); got != Refused {
			t.Fatalf("ses-bad's write answered 500 %s came to %v, want Refused", listenerwire.CodeMalformedRecord, got)
		}
	}
	if lines := errorLines("ses-bad"); len(lines) != 1 || !strings.Contains(lines[0], "count=1 ") || !strings.Contains(lines[0], cause("ses-bad")) {
		t.Errorf("ses-bad was logged at ERROR as %q, want one line naming its key, the count 1 and the listener's cause:\n%s", lines, strings.Join(log.lines(), "\n"))
	}

	// Another session is its own line and its own count.
	l.set("/v1/legion/agents/sessions/ses-worse", listenerwire.FaultStatus, answer(listenerwire.CodeMalformedRecord, head+cause("ses-worse")))
	if got := writer.Entry(ctx, entry("ses-worse")); got != Refused {
		t.Fatalf("ses-worse's write answered 500 %s came to %v, want Refused", listenerwire.CodeMalformedRecord, got)
	}
	if lines := errorLines("ses-worse"); len(lines) != 1 || !strings.Contains(lines[0], "count=2 ") || !strings.Contains(lines[0], cause("ses-worse")) {
		t.Errorf("ses-worse was logged at ERROR as %q, want one line naming its key, the count 2 and the listener's cause:\n%s", lines, strings.Join(log.lines(), "\n"))
	}

	// A batch of several: Refused, logged as a refusal, counted never.
	l.set("/v1/legion/agents", listenerwire.FaultStatus, answer(listenerwire.CodeMalformedRecord, head+"wrote 0 of 2 legion agents, then entries[1]: "+cause("ses-third")))
	if got := writer.Batch(ctx, listenerwire.LegionAgentBatch{Entries: []listenerwire.LegionAgentEntry{entry("ses-ok"), entry("ses-third")}}); got != Refused {
		t.Fatalf("a batch of two answered 500 %s came to %v, want Refused, so the mirror sends each alone", listenerwire.CodeMalformedRecord, got)
	}
	if lines := log.lines("level=WARN", "the Envoy listener refused an agent registry write", "path=/v1/legion/agents status=500"); len(lines) != 1 {
		t.Errorf("the batch of two was logged as %q, want one refusal line:\n%s", lines, strings.Join(log.lines(), "\n"))
	}
	if lines := log.lines("level=ERROR"); len(lines) != 2 {
		t.Errorf("ERROR lines after the batch of two = %d, want ses-bad's and ses-worse's alone:\n%s", len(lines), strings.Join(lines, "\n"))
	}

	// The controls: every other 500, and a 503 whatever its code, is Failed.
	for _, tc := range []struct {
		id, answer string
		status     int
	}{
		{"ses-plain", `{"error":"internal error"}`, listenerwire.FaultStatus},
		{"ses-other-code", answer(listenerwire.CodeMaybeStored, head+cause("ses-other-code")), listenerwire.FaultStatus},
		{"ses-not-json", "internal error", listenerwire.FaultStatus},
		{"ses-unavailable", answer("", "envoy: the legion agent registry did not answer: nats: timeout"), http.StatusServiceUnavailable},
		{"ses-unavailable-code", answer(listenerwire.CodeMalformedRecord, head+cause("ses-unavailable-code")), http.StatusServiceUnavailable},
	} {
		path := "/v1/legion/agents/sessions/" + tc.id
		l.set(path, tc.status, tc.answer)
		if got := writer.Entry(ctx, entry(tc.id)); got != Failed {
			t.Errorf("%s's write answered %d %s came to %v, want Failed: worth trying again", tc.id, tc.status, tc.answer, got)
		}
		if lines := log.lines("level=WARN", "the Envoy listener failed an agent registry write; trying again", "path="+path+" "); len(lines) != 1 {
			t.Errorf("%s's %d was logged as %q, want one line of a write to try again:\n%s", tc.id, tc.status, lines, strings.Join(log.lines(), "\n"))
		}
		if lines := errorLines(tc.id); len(lines) != 0 {
			t.Errorf("%s's %d was logged at ERROR as a record the listener cannot decode: %q", tc.id, tc.status, lines)
		}
	}

	// The batch's bad entry sent alone, as the mirror sends it: its line counts it third, after
	// ses-bad and ses-worse, so neither the batch of two nor any control was counted.
	l.set("/v1/legion/agents/sessions/ses-third", listenerwire.FaultStatus, answer(listenerwire.CodeMalformedRecord, head+cause("ses-third")))
	if got := writer.Entry(ctx, entry("ses-third")); got != Refused {
		t.Fatalf("ses-third's write answered 500 %s came to %v, want Refused", listenerwire.CodeMalformedRecord, got)
	}
	if lines := errorLines("ses-third"); len(lines) != 1 || !strings.Contains(lines[0], "count=3 ") || !strings.Contains(lines[0], cause("ses-third")) {
		t.Errorf("ses-third was logged at ERROR as %q, want one line naming its key, the count 3 and the listener's cause:\n%s", lines, strings.Join(log.lines(), "\n"))
	}
}

// An envoy_url whose made-up password holds an unencoded "@" leaves the password's tail where the
// host goes, and the resolver's error names that host: the line a write no listener answered is
// logged with holds no part of the tail, in its ASCII form or in the `xn--` form a tail that is not
// ASCII is dialed by. A password holding a "/" ends the authority there, so url.Parse reads its
// head as the port and no write is built: that line, whose error quotes the URL whole, holds no
// part of the password either.
func TestAWritesLoggedErrorHoldsNoPartOfThePassword(t *testing.T) {
	t.Parallel()
	passwordAt := "legion:p" + "@"
	for name, tc := range map[string]struct {
		address, line string
		want          Outcome
	}{
		"an ASCII tail": {"http://" + passwordAt + "madeuptail0123.invalid",
			"legion daemon could not write to the agent registry; trying again", Failed},
		"a tail that is not ASCII": {"http://" + passwordAt + "madeuptail0123\u00f6rd.invalid",
			"legion daemon could not write to the agent registry; trying again", Failed},
		"a head read as the port": {"http://legion:madeuptail0123/x" + "@listener.invalid",
			"legion daemon could not build an agent registry write", Refused},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &logged{}
			writer := New(tc.address, "envoy-bearer", nil, slog.New(slog.NewTextHandler(log, nil)))
			if got := writer.Entry(context.Background(), listenerwire.LegionAgentEntry{SessionID: "ses-x"}); got != tc.want {
				t.Fatalf("the write came to %v, want %v", got, tc.want)
			}
			lines := log.lines(tc.line)
			if len(lines) != 1 {
				t.Fatalf("the write was logged as %q, want one line %q", log.lines(), tc.line)
			}
			if lower := strings.ToLower(lines[0]); strings.Contains(lower, "madeuptail") || strings.Contains(lower, "xn--") {
				t.Errorf("the line %q holds part of the password", lines[0])
			}
		})
	}
}
