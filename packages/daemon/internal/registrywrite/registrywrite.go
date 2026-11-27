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
	// Refused is a 400 or 409, or a write that could not be encoded or built: the listener will
	// never take what was sent.
	Refused
	// Failed is everything else, a transport failure among them: worth trying again.
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
// HTTP client, the logger, and which routes have answered 404 already, each logged once. A Writer
// is for one goroutine at a time: the mirror's Run.
type Writer struct {
	baseURL, token string
	client         *http.Client
	log            *slog.Logger
	logged404      map[string]bool
}

// New builds a Writer for the listener at baseURL. A nil client is one with a ten-second timeout.
func New(baseURL, token string, client *http.Client, log *slog.Logger) *Writer {
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	return &Writer{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: client, log: log, logged404: map[string]bool{}}
}

// Batch PUTs a batch of entries to /v1/legion/agents.
func (w *Writer) Batch(ctx context.Context, batch listenerwire.LegionAgentBatch) Outcome {
	return w.put(ctx, batchRoute, "/v1/legion/agents", batch)
}

// Entry PUTs one session's entry to /v1/legion/agents/sessions/<session_id>.
func (w *Writer) Entry(ctx context.Context, entry listenerwire.LegionAgentEntry) Outcome {
	return w.put(ctx, entryRoute, "/v1/legion/agents/sessions/"+url.PathEscape(entry.SessionID), entry)
}

// Answering PUTs the project's answering record to /v1/legion/projects/<legion_project>.
func (w *Writer) Answering(ctx context.Context, project string, beat listenerwire.LegionProjectRequest) Outcome {
	return w.put(ctx, projectRoute, "/v1/legion/projects/"+url.PathEscape(project), beat)
}

// put PUTs body to path, logging what is not a success: a route's first 404, a refusal, and every
// other failure.
func (w *Writer) put(ctx context.Context, route, path string, body any) Outcome {
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
	switch status := response.StatusCode; {
	case status >= 200 && status < 300:
		return Sent
	case status == http.StatusNotFound:
		if !w.logged404[route] {
			w.logged404[route] = true
			w.log.Warn("the Envoy listener answers 404 on "+route+": it predates the Legion agent registry; the daemon tries again each minute",
				"route", route, "answer", strings.TrimSpace(string(answer)))
		}
		return NotFound
	case status == http.StatusBadRequest || status == http.StatusConflict:
		w.log.Warn("the Envoy listener refused an agent registry write; it is not sent again", "route", route, "path", path,
			"status", status, "answer", strings.TrimSpace(string(answer)))
		return Refused
	default:
		w.log.Warn("the Envoy listener failed an agent registry write; trying again", "route", route, "path", path,
			"status", status, "answer", strings.TrimSpace(string(answer)))
		return Failed
	}
}
