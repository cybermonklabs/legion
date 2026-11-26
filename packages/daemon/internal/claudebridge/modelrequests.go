package claudebridge

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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

// requestKeepWindow bounds the age of a report: the daemon reads an hour of request history.
const requestKeepWindow = time.Hour

var errRequestReportTimeout = fmt.Errorf("the daemon gave no answer within the report's bound of %s", requestReportTimeout)

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
//   - an answered request is one or more `assistant` lines with the same `message.id`, one for
//     each content block. Each carries the request's `usage` and the time the block was written.
//     The bridge counts it once by its id, closing it on the next unrelated line or at the window;
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
// The first request arms a window of requestWindow. A result sends any pending requests now.
// Overflow drains on an answer that keeps at least one request. Otherwise a failed or incomplete
// report can retry at the next window or result; only IDs the daemon did not keep go again.
// Requests past the keep window and reports the daemon refuses (a 4xx) are dropped with a line.
// Every report names the launch's boot token. Nothing in the bridge's loop waits on a report.
type modelRequests struct {
	*daemonPoster
	// own and provider are the project's two refusal patterns, nil where the launch set none.
	own, provider *regexp.Regexp

	mu      sync.Mutex
	open    *claim.ModelRequest
	counted map[string]bool
	model   string
	// pending holds requests until they are kept or dropped. due records a window or result
	// that called for another send while one was underway.
	pending []claim.ModelRequest
	sending bool
	due     bool
	// A firing with a different window number was canceled by a result.
	window, windows uint64
}

func startModelRequests(cfg Config, poster *daemonPoster) *modelRequests {
	return &modelRequests{
		daemonPoster: poster,
		own:          refusalPattern(runtime.OwnRefusalVariable, cfg.OwnRefusal, poster.log),
		provider:     refusalPattern(runtime.ProviderRefusalVariable, cfg.ProviderRefusal, poster.log),
		counted:      map[string]bool{},
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
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case line.kind == "assistant":
		r.assistant(line)
		return
	case line.kind == "system" && line.subtype == "init":
		if line.model != "" {
			r.model = line.model
		}
	case line.kind == "system" && line.subtype == "api_retry" && line.errorStatus == http.StatusTooManyRequests:
		r.closeLocked()
		r.refused(line, "", claim.RefusalUnknown)
		return
	}
	r.closeLocked()
	if line.kind == "result" {
		r.window = 0
		r.sendLocked()
	}
}

// assistant reads the request's metadata, or the refusal text of a synthetic answer.
// The caller holds r.mu.
func (r *modelRequests) assistant(line streamLine) {
	var m struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(line.message, &m)
	if m.Model == shimwire.SyntheticModel {
		r.closeLocked()
		if line.apiErrorStatus == http.StatusTooManyRequests {
			r.refused(line, m.ID, r.classOf(textOf(blocksOf(line.message))))
		}
		return
	}
	if r.open != nil && r.open.ID == m.ID {
		r.open.EndedAt = endOf(line)
		return
	}
	r.closeLocked()
	if m.ID == "" || m.Model == "" || r.counted[m.ID] {
		return
	}
	request := &claim.ModelRequest{ID: m.ID, Session: sessionOfLine(line), Model: m.Model, EndedAt: endOf(line)}
	if m.Usage != nil {
		request.UncachedInputTokens = new(m.Usage.Input + m.Usage.CacheWrite)
	}
	r.open = request
	r.armLocked()
}

// closeLocked queues the open request, which already armed a window when it opened.
func (r *modelRequests) closeLocked() {
	if r.open == nil {
		return
	}
	request := *r.open
	r.open = nil
	r.counted[request.ID] = true
	r.pending = append(r.pending, request)
}

// refused queues the report of one refused attempt, under the line's own id (`uuid`), else the
// synthetic answer's message id, else one of the bridge's own: a refused attempt is one line, and
// counts once. The caller holds r.mu.
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
	r.pending = append(r.pending, claim.ModelRequest{ID: id, Session: sessionOfLine(line), Model: model, EndedAt: at, Refusal: class})
	r.armLocked()
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

// armLocked arms a window for what is pending, if none is armed. The caller holds r.mu.
func (r *modelRequests) armLocked() {
	if r.window != 0 || (len(r.pending) == 0 && r.open == nil) {
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
			r.closeLocked()
			r.sendLocked()
		}
	}()
}

// sendLocked sends the oldest pending requests, up to claim.MaxModelRequests, unless a report is
// on its way, in which case they go once it is answered. The caller holds r.mu.
func (r *modelRequests) sendLocked() {
	if len(r.pending) == 0 {
		return
	}
	if r.sending {
		r.due = true
		return
	}
	cutoff := r.clock.Now().Add(-requestKeepWindow)
	r.pending = slices.DeleteFunc(r.pending, func(request claim.ModelRequest) bool {
		if !request.EndedAt.Before(cutoff) {
			return false
		}
		r.log.Printf("dropped model request %q ended at %s: past the daemon's keep window", request.ID, request.EndedAt)
		return true
	})
	if len(r.pending) == 0 {
		return
	}
	n := min(len(r.pending), claim.MaxModelRequests)
	batch := slices.Clone(r.pending[:n])
	r.pending = slices.Delete(r.pending, 0, n)
	r.sending = true
	go r.post(batch, len(r.pending) != 0)
}

// post makes one report and puts back every request of it the daemon did not say it holds, under
// the same id.
func (r *modelRequests) post(batch []claim.ModelRequest, overflow bool) {
	kept, retry := r.call(batch)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sending = false
	progress := false
	if retry {
		var unheld []claim.ModelRequest
		for _, request := range batch {
			if _, ok := kept[request.ID]; ok {
				progress = true
			} else {
				unheld = append(unheld, request)
			}
		}
		if len(unheld) != 0 {
			r.pending = append(unheld, r.pending...)
		}
	}
	// Overflow is not itself permission to retry a failed batch without waiting.
	if r.due || (progress && (overflow || len(r.pending) > claim.MaxModelRequests)) {
		r.due = false
		r.sendLocked()
	}
	r.armLocked()
}

// call posts batch and answers the ids the daemon holds, and whether the rest go again: not for a
// report the daemon refused, which no retry passes. Every batch says that its host reports each
// 429 its HTTP transport retries, since Claude Code writes an `api_retry` line for each such
// attempt and the bridge reports each as a refused request of its own (refused).
func (r *modelRequests) call(batch []claim.ModelRequest) (map[string]struct{}, bool) {
	body, err := json.Marshal(claim.ModelRequestsRequest{
		BootCaller:              claim.BootCaller{BootToken: r.bootToken},
		Generation:              r.generation,
		Requests:                batch,
		RetriedRefusalsReported: true,
	})
	if err != nil {
		r.log.Printf("could not encode a report of %d model requests: %v", len(batch), err)
		return nil, false
	}
	// The answer echoes IDs from this JSON body and no other request fields, so even a full
	// batch of long IDs fits inside the body size. Keep room for ordinary refusals too.
	bound := max(int64(len(body)), int64(maxAnswerBytes))
	response, attempted, err := r.postJSON("/legion/v1/claims/model-requests", body, requestReportTimeout, errRequestReportTimeout, bound)
	switch {
	case !attempted:
		r.log.Printf("could not report %d model requests: %v", len(batch), err)
		return nil, false
	case response.statusCode == 0:
		r.log.Printf("could not report %d model requests; they go again: %v", len(batch), err)
		return nil, true
	case response.statusCode/100 == 4:
		r.log.Printf("the daemon refused a report of %d model requests; they go unreported: %s %s", len(batch), response.status, strings.TrimSpace(string(response.body)))
		return nil, false
	case response.statusCode/100 != 2 || err != nil:
		r.log.Printf("the daemon did not take a report of %d model requests; they go again: %s %s", len(batch), response.status, strings.TrimSpace(string(response.body)))
		return nil, true
	}
	var decoded claim.ModelRequestsResponse
	if err := json.Unmarshal(response.body, &decoded); err != nil {
		r.log.Printf("the daemon's answer to a report of %d model requests could not be read; they go again: %v", len(batch), err)
		return nil, true
	}
	kept := make(map[string]struct{}, len(decoded.Kept))
	for _, id := range decoded.Kept {
		kept[id] = struct{}{}
	}
	return kept, true
}
