package claudebridge

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// requestWindow is how long after a model request's end the bridge may hold its report: a quarter
// of the minute the provider's headroom reads, so a pane sends at most four reports a minute
// (dispatch://INFRA-5899, spec, "The model requests").
const requestWindow = 15 * time.Second

// requestReportTimeout bounds one report's call, as reportTimeout bounds a turn outcome's: a
// daemon that takes the connection and never answers costs the report its window, and the
// requests go again under the same ids.
const requestReportTimeout = 10 * time.Second

// refusedStatus is the HTTP status a limit refuses a model request with. A failure of any other
// status — an overloaded provider's 529 among them — is no limit's refusal, and is not reported.
const refusedStatus = 429

// modelRequests is the bridge as the Claude Code backend's producer of model requests (POST
// /legion/v1/claims/model-requests): each request Claude Code made for the role's session or for
// a subagent the role started is reported with the moment it ended, its uncached input tokens
// and, for one a limit refused, who refused it, for the provider's headroom and refusals. The Oh
// My Pi plugin's half is packages/pi-legion/src/model-requests.ts.
//
// What the stream says of a request, measured on Claude Code 2.1.311 in this bridge's print mode
// against an Anthropic-shaped stand-in (testdata/subagent-turn.jsonl, testdata/api-retry-answered.jsonl)
// and on 2.1.310 (testdata/api-error-429.jsonl, testdata/background-turn.jsonl):
//
//   - an answered request is one or more `assistant` lines with the same `message.id`, one for each
//     block of its content, each carrying the request's `message.usage` and the moment that block
//     was written (`timestamp`). The request ended with its last line, which is the line before
//     the first line that is not one of its own, so a request is closed there (read) and counted
//     once, by its id, however many lines it took;
//   - a subagent's lines are the same, with `parent_tool_use_id` naming the Task call and
//     `agent_id` the subagent, which is the session reported for them; the role's own lines carry
//     `parent_tool_use_id` null and the session's id;
//   - each attempt Claude Code retries is a `system` `api_retry` line with its `error_status`,
//     and no text of the refusal (its `error` is Claude Code's own category, `rate_limit`), so a
//     refused retry is unknown: never classed by what a later attempt said;
//   - the last attempt of a request refused for good is Claude Code's synthetic answer (model
//     `<synthetic>`) with `api_error_status` and the refusal's text as its content, classed by
//     that text: the own pattern first, the provider's second, else unknown.
//
// A retry line and a synthetic answer name no model, so they are reported under the model the
// session's `init` line names. Uncached input is `input_tokens + cache_creation_input_tokens`,
// never the cache reads; a refused attempt counts none the stream names.
//
// One pending batch: the first request pending arms a window of requestWindow, which a request
// that joins it moves nothing of; every `result`, the turn's end among them, sends what is pending
// at once and drops the window; requests that end while a report is on its way wait for a report
// of their own; and a request the daemon's answer does not name as held goes again under the same
// id once a window has passed, while a report the daemon refuses (a 4xx) is dropped with a line,
// as a turn outcome's is. Every report names the launch's boot token alone, as the turn outcome
// does: the bridge holds no other credential. Nothing of the bridge's loop waits on a report.
type modelRequests struct {
	url        string
	bootToken  string
	generation uint64
	client     *http.Client
	clock      Clock
	log        *log.Logger
	// own and provider are the project's two refusal patterns, nil where the launch set none.
	own, provider *regexp.Regexp

	// The rest above mu is the loop's alone: the request whose lines are being read, the ids
	// counted already, and the model the session's init line names.
	open    *claim.ModelRequest
	counted map[string]bool
	model   string

	mu sync.Mutex
	// pending is every request no report has carried to a daemon that holds it; sending is a
	// report on its way; due is a send that fell due meanwhile, made once it is answered.
	pending []claim.ModelRequest
	sending bool
	due     bool
	// window is the number of the window armed, 0 where none is: a firing whose number is not it
	// is one dropped since.
	window, windows uint64
}

// startModelRequests is the reporter for cfg, or nil when the pane names no daemon, boot token
// file or generation, as startTurnReports is: a bridge run by hand reports to nobody.
func startModelRequests(cfg Config, logger *log.Logger) *modelRequests {
	if cfg.DaemonURL == "" || cfg.BootTokenFile == "" || cfg.Generation == 0 {
		logger.Printf("reporting no model requests: this pane names %s", missingReportInputs(cfg))
		return nil
	}
	token, err := os.ReadFile(cfg.BootTokenFile)
	if err != nil {
		logger.Printf("reporting no model requests: the boot token the daemon would recognise this claim by could not be read: %v", err)
		return nil
	}
	return &modelRequests{
		url:        strings.TrimSuffix(cfg.DaemonURL, "/") + "/legion/v1/claims/model-requests",
		bootToken:  strings.TrimSpace(string(token)),
		generation: cfg.Generation,
		client:     &http.Client{},
		clock:      cfg.clock(),
		log:        logger,
		own:        refusalPattern(runtime.OwnRefusalVariable, cfg.OwnRefusal, logger),
		provider:   refusalPattern(runtime.ProviderRefusalVariable, cfg.ProviderRefusal, logger),
		counted:    map[string]bool{},
	}
}

// refusalPattern is pattern compiled as every Go reader of it does (config.SharedPattern), nil
// where the launch set none. One the loader would not have taken is said once and classes nothing.
func refusalPattern(variable, pattern string, logger *log.Logger) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	compiled, err := config.SharedPattern(pattern)
	if err != nil {
		logger.Printf("%s does not compile, so it classes no refusal: %v", variable, err)
		return nil
	}
	return compiled
}

// classOf is who refused a request whose refusal said text: the own pattern first, the
// provider's second, else unknown.
func (r *modelRequests) classOf(text string) claim.RefusalClass {
	switch {
	case r.own != nil && r.own.MatchString(text):
		return claim.RefusalOwn
	case r.provider != nil && r.provider.MatchString(text):
		return claim.RefusalProvider
	}
	return claim.RefusalUnknown
}

// streamLine is what read takes of one stream-json line of Claude Code's.
type streamLine struct {
	kind, subtype string
	message       json.RawMessage
	// parent is the Task call a subagent's line serves, "" on the role's own; agent the
	// subagent's id; session the session's id; uuid the line's own id.
	parent, agent, session, uuid string
	// timestamp is when the line was written, "" where it names none; model the model an init
	// line names.
	timestamp, model string
	// apiErrorStatus is a synthetic answer's (and a result's) api_error_status, errorStatus a
	// retry line's error_status, each 0 where the line names none.
	apiErrorStatus, errorStatus int
}

// read acts on one line of the stream, on the bridge's loop: it closes the request whose lines
// were being read at the first line that is not one of its own, opens the next, reports each
// refused attempt, and at a `result` sends what is pending.
func (r *modelRequests) read(line streamLine) {
	if r == nil {
		return
	}
	switch {
	case line.kind == "assistant":
		r.assistant(line)
		return
	case line.kind == "system" && line.subtype == "init":
		if line.model != "" {
			r.model = line.model
		}
	case line.kind == "system" && line.subtype == "api_retry" && line.errorStatus == refusedStatus:
		r.close()
		r.refused(line, "", claim.RefusalUnknown)
		return
	}
	r.close()
	if line.kind == "result" {
		r.flush()
	}
}

// assistant reads one assistant line: a line of the request open, the first line of the next, or
// a synthetic answer, which is the last attempt of a refused request.
func (r *modelRequests) assistant(line streamLine) {
	var m struct {
		ID      string         `json:"id"`
		Model   string         `json:"model"`
		Content []contentBlock `json:"content"`
		Usage   *struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(line.message, &m)
	if m.Model == shimwire.SyntheticModel {
		r.close()
		if line.apiErrorStatus == refusedStatus {
			var texts []string
			for _, block := range m.Content {
				if block.Type == "text" {
					texts = append(texts, block.Text)
				}
			}
			r.refused(line, m.ID, r.classOf(strings.Join(texts, "\n")))
		}
		return
	}
	if r.open != nil && r.open.ID == m.ID {
		r.open.EndedAt = endOf(line)
		return
	}
	r.close()
	if m.ID == "" || m.Model == "" || r.counted[m.ID] {
		return
	}
	request := &claim.ModelRequest{ID: m.ID, Session: sessionOfLine(line), Model: m.Model, EndedAt: endOf(line)}
	if m.Usage != nil {
		request.UncachedInputTokens = new(m.Usage.Input + m.Usage.CacheWrite)
	}
	r.open = request
}

// close counts the request whose lines were being read, which has ended, and queues its report.
func (r *modelRequests) close() {
	if r.open == nil {
		return
	}
	request := *r.open
	r.open = nil
	r.counted[request.ID] = true
	r.enqueue(request)
}

// refused queues the report of one refused attempt, under the line's own id (`uuid`), else the
// synthetic answer's message id, else one of the bridge's own: a refused attempt is one line, and
// counts once.
func (r *modelRequests) refused(line streamLine, messageID string, class claim.RefusalClass) {
	model := r.model
	if model == "" {
		r.log.Printf("a refused model request goes unreported: the session's init line named no model")
		return
	}
	at := endOf(line)
	id := cmp.Or(line.uuid, messageID, fmt.Sprintf("refused-%s-%d", sessionOfLine(line), at.UnixNano()))
	if r.counted[id] {
		return
	}
	r.counted[id] = true
	r.enqueue(claim.ModelRequest{ID: id, Session: sessionOfLine(line), Model: model, EndedAt: at, Refusal: class})
}

// sessionOfLine is the session a line's request ran in: the subagent's where the line serves a
// Task call, else the session's own.
func sessionOfLine(line streamLine) string {
	if line.parent != "" && line.agent != "" {
		return line.agent
	}
	return line.session
}

// endOf is when a line was written, as the line says, and the bridge's own time for a line that
// says none (a retry line).
func endOf(line streamLine) time.Time {
	if at, err := time.Parse(time.RFC3339Nano, line.timestamp); err == nil {
		return at
	}
	return time.Now().UTC()
}

// enqueue adds request to the pending batch, arming the window if none is.
func (r *modelRequests) enqueue(request claim.ModelRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, request)
	r.armLocked()
}

// armLocked arms a window for what is pending, if none is armed. The caller holds r.mu.
func (r *modelRequests) armLocked() {
	if r.window != 0 || len(r.pending) == 0 {
		return
	}
	r.windows++
	r.window = r.windows
	window, fires := r.window, r.clock.After(requestWindow)
	go func() {
		<-fires
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.window == window {
			r.window = 0
			r.sendLocked()
		}
	}()
}

// flush sends what is pending now and drops the window.
func (r *modelRequests) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.window = 0
	r.sendLocked()
}

// sendLocked sends the oldest pending requests, up to claim.MaxModelRequests, unless a report is
// on its way, in which case they go once it is answered. The caller holds r.mu.
func (r *modelRequests) sendLocked() {
	if r.sending {
		r.due = true
		return
	}
	if len(r.pending) == 0 {
		return
	}
	n := min(len(r.pending), claim.MaxModelRequests)
	batch := slices.Clone(r.pending[:n])
	r.pending = slices.Delete(r.pending, 0, n)
	r.sending, r.window = true, 0
	go r.post(batch)
}

// post makes one report and puts back every request of it the daemon did not say it holds, under
// the same id.
func (r *modelRequests) post(batch []claim.ModelRequest) {
	kept, retry := r.call(batch)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sending = false
	if retry {
		var unheld []claim.ModelRequest
		for _, request := range batch {
			if !slices.Contains(kept, request.ID) {
				unheld = append(unheld, request)
			}
		}
		r.pending = append(unheld, r.pending...)
	}
	if r.due || len(r.pending) > claim.MaxModelRequests {
		r.due = false
		r.sendLocked()
		return
	}
	r.armLocked()
}

// call posts batch and answers the ids the daemon holds, and whether the rest go again: not for a
// report the daemon refused, which no retry passes.
func (r *modelRequests) call(batch []claim.ModelRequest) ([]string, bool) {
	body, err := json.Marshal(claim.ModelRequestsRequest{
		BootCaller: claim.BootCaller{BootToken: r.bootToken},
		Generation: r.generation,
		Requests:   batch,
	})
	if err != nil {
		r.log.Printf("could not encode a report of %d model requests: %v", len(batch), err)
		return nil, false
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go func() {
		select {
		case <-r.clock.After(requestReportTimeout):
			cancel(fmt.Errorf("the daemon gave no answer within the report's bound of %s", requestReportTimeout))
		case <-ctx.Done():
		}
	}()
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		r.log.Printf("could not report %d model requests: %v", len(batch), err)
		return nil, false
	}
	post.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(post)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = fmt.Errorf("%w (%v)", cause, err)
		}
		r.log.Printf("could not report %d model requests; they go again: %v", len(batch), err)
		return nil, true
	}
	defer func() { _ = response.Body.Close() }()
	answer, err := readAnswer(response)
	switch {
	case response.StatusCode/100 == 4:
		r.log.Printf("the daemon refused a report of %d model requests; they go unreported: %s %s", len(batch), response.Status, strings.TrimSpace(string(answer)))
		return nil, false
	case response.StatusCode/100 != 2 || err != nil:
		r.log.Printf("the daemon did not take a report of %d model requests; they go again: %s %s", len(batch), response.Status, strings.TrimSpace(string(answer)))
		return nil, true
	}
	var decoded claim.ModelRequestsResponse
	if err := json.Unmarshal(answer, &decoded); err != nil {
		r.log.Printf("the daemon's answer to a report of %d model requests could not be read; they go again: %v", len(batch), err)
		return nil, true
	}
	return decoded.Kept, true
}
