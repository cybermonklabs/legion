// Package registrywrite is the daemon's writer to the Envoy listener's Legion agent registry: the
// three PUTs the agent registry mirror makes (internal/agenthistory's Mirror), each with its
// listenerwire body, and what each came to (Outcome). It imports the standard library,
// listenerwire and redact alone, so the listener's client test
// (packages/envoy/listenerwire/clients_test.go) reads it, and nothing else of the daemon, when it
// holds the writer to the listener's own types.
// When a write is tried again, and how often, is the mirror's.
package registrywrite

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/listenerwire"

	"github.com/sjawhar/legion/daemon/internal/redact"
)

const (
	// requestTimeout bounds one write of a Writer built with no client of its own.
	requestTimeout = 10 * time.Second
	// refusalBodyLimit bounds how much of an answer's body a log line carries.
	refusalBodyLimit = 4096
	// listenerServer is how a write's error names the Envoy listener when its address does not
	// parse (redact.RequestError). envoy_url can carry a user name and a password, and a dial's or
	// a resolver's error names the host, which is a password's tail when it holds an unencoded "@":
	// every write that got no answer is logged through redact.RequestError, as notify's requests
	// are.
	listenerServer = "the Envoy listener"
)

// Outcome is what one write came to.
type Outcome int

const (
	// Sent is a 2xx: the listener took the write.
	Sent Outcome = iota
	// NotFound is a 404: a listener that predates the route.
	NotFound
	// Refused is a 400 or 409; a 500 with the code listenerwire.CodeMalformedRecord, a stored
	// entry of the write's session that the listener cannot decode and answers so however often it
	// is asked; or a write that could not be encoded or built: the listener will never take what
	// was sent. A batch of several entries refused for a malformed record holds one such entry,
	// which the mirror finds by sending each entry alone, as it does after a 400 or 409.
	Refused
	// Failed is everything else, a transport failure, every 503 and every other 500 among them:
	// worth trying again.
	Failed
)

// The registry's routes, as each log line names them.
const (
	batchRoute   = "PUT /v1/legion/agents"
	entryRoute   = "PUT /v1/legion/agents/sessions/{id}"
	projectRoute = "PUT /v1/legion/projects/{project}"
)

// Writer writes to the Envoy listener's Legion agent registry: its base URL and `/v1` bearer
// (envoy_url, envoy_token_file, as notify.HTTPPublisher holds them; a token "" sends none), the
// HTTP client, the logger, which routes have answered 404 already, each logged once, and which
// sessions' writes the listener refused for a stored entry it cannot decode
// (listenerwire.CodeMalformedRecord), each logged once at ERROR. The daemon exports no metrics, so
// that line's "count" attribute is the count: how many sessions' writes the listener has refused
// so, each session once however often its write is refused, and a batch of several entries never,
// since its own entries are each sent alone after it and the bad one is counted there. A Writer is
// for one goroutine at a time: the mirror's Run.
type Writer struct {
	baseURL, token string
	client         *http.Client
	log            *slog.Logger
	logged404      map[string]bool
	// loggedMalformed holds each session a malformed record refused a write of; how many it holds
	// is the ERROR line's count.
	loggedMalformed map[string]bool
}

// New builds a Writer for the listener at baseURL. A nil client is one with a ten-second timeout.
func New(baseURL, token string, client *http.Client, log *slog.Logger) *Writer {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return &Writer{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: client, log: log, logged404: map[string]bool{}, loggedMalformed: map[string]bool{}}
}

// Batch PUTs a batch of entries to /v1/legion/agents. A batch of one entry is that entry's write:
// a malformed record refusing it is its session's (put).
func (w *Writer) Batch(ctx context.Context, batch listenerwire.LegionAgentBatch) Outcome {
	session := ""
	if len(batch.Entries) == 1 {
		session = batch.Entries[0].SessionID
	}
	return w.put(ctx, batchRoute, "/v1/legion/agents", session, batch)
}

// Entry PUTs one session's entry to /v1/legion/agents/sessions/<session_id>.
func (w *Writer) Entry(ctx context.Context, entry listenerwire.LegionAgentEntry) Outcome {
	return w.put(ctx, entryRoute, "/v1/legion/agents/sessions/"+url.PathEscape(entry.SessionID), entry.SessionID, entry)
}

// Answering PUTs the project's answering record to /v1/legion/projects/<legion_project>.
func (w *Writer) Answering(ctx context.Context, project string, beat listenerwire.LegionProjectRequest) Outcome {
	return w.put(ctx, projectRoute, "/v1/legion/projects/"+url.PathEscape(project), "", beat)
}

// put PUTs body to path, logging what is not a success: a route's first 404, a refusal, and every
// other failure. session is the one session whose entry body carries, "" for a batch of several
// and for the project's record. A 500 with the code listenerwire.CodeMalformedRecord is Refused:
// for a session's own write it is logged at ERROR the first time, naming the session's key, the
// listener's cause and the count (Writer), so the mirror drops that one entry and goes on with the
// rest; for any other write it is logged as a 400 or 409 is. Every other 500, and every 503, is
// Failed, to be sent again.
func (w *Writer) put(ctx context.Context, route, path, session string, body any) Outcome {
	encoded, err := json.Marshal(body)
	if err != nil {
		w.log.Error("legion daemon could not encode an agent registry write", "route", route, "error", err)
		return Refused
	}
	target := w.baseURL + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target, bytes.NewReader(encoded))
	if err != nil {
		w.log.Error("legion daemon could not build an agent registry write", "route", route, "error", redact.RequestError(listenerServer, target, err))
		return Refused
	}
	request.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		request.Header.Set("Authorization", "Bearer "+w.token)
	}
	response, err := w.client.Do(request)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("legion daemon could not write to the agent registry; trying again", "route", route, "error", redact.RequestError(listenerServer, target, err))
		}
		return Failed
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
	status := response.StatusCode
	cause, malformed := malformedRecord(status, answer)
	switch {
	case status >= 200 && status < 300:
		return Sent
	case status == http.StatusNotFound:
		if !w.logged404[route] {
			w.logged404[route] = true
			w.log.Warn("the Envoy listener answers 404 on "+route+": it predates the Legion agent registry; the daemon tries again each minute",
				"route", route, "answer", strings.TrimSpace(string(answer)))
		}
		return NotFound
	case malformed && session != "":
		if !w.loggedMalformed[session] {
			w.loggedMalformed[session] = true
			w.log.Error("the Envoy listener holds an agent registry entry it cannot decode; the daemon does not send its write again and goes on with the rest of the mirror",
				"route", route, "path", path, "session", session, "key", "session."+session, "code", listenerwire.CodeMalformedRecord,
				"count", len(w.loggedMalformed), "cause", cause)
		}
		return Refused
	case status == http.StatusBadRequest || status == http.StatusConflict || malformed:
		w.log.Warn("the Envoy listener refused an agent registry write; it is not sent again", "route", route, "path", path,
			"status", status, "answer", strings.TrimSpace(string(answer)))
		return Refused
	default:
		w.log.Warn("the Envoy listener failed an agent registry write; trying again", "route", route, "path", path,
			"status", status, "answer", strings.TrimSpace(string(answer)))
		return Failed
	}
}

// malformedRecord is the cause answer names and true when it is the listener's 500 for a stored
// entry it cannot decode, which carries the code listenerwire.CodeMalformedRecord; a 500 with no
// code or another one, a body that does not decode, and every other status are false.
func malformedRecord(status int, answer []byte) (string, bool) {
	if status != listenerwire.FaultStatus {
		return "", false
	}
	var refusal listenerwire.APIError
	if err := json.Unmarshal(answer, &refusal); err != nil || refusal.Code != listenerwire.CodeMalformedRecord {
		return "", false
	}
	return refusal.Error, true
}
