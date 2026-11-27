package claudebridge

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
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

var errRequestReportTimeout = fmt.Errorf("the daemon gave no answer within the report's bound of %s", requestReportTimeout)

// modelRequests is the bridge as the Claude Code backend's producer of model requests (POST
// /legion/v1/claims/model-requests): each request Claude Code answered for the role's session or
// for a subagent the role started is reported with the moment it ended and its uncached input
// tokens, for the provider's headroom. The Oh My Pi plugin's half is
// packages/pi-legion/src/model-requests.ts.
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
//   - an attempt Claude Code retries is a `system` `api_retry` line, and the last attempt of a
//     request refused for good is Claude Code's synthetic answer (model `<synthetic>`). Neither is
//     an answered request, so neither is reported: a retry the next attempt answered is reported
//     as the answer alone.
//
// Uncached input is `input_tokens + cache_creation_input_tokens`, never the cache reads.
//
// The first request arms a window of requestWindow. A result sends any pending requests now.
// Overflow drains on an answer that keeps at least one request. Otherwise a failed or incomplete
// report can retry at the next window or result; only IDs the daemon did not keep go again.
// Requests past the keep window and reports the daemon refuses (a 4xx) are dropped with a line.
// Every report names the launch's boot token. Nothing in the bridge's loop waits on a report.
type modelRequests struct {
	*daemonPoster

	mu      sync.Mutex
	open    *claim.ModelRequest
	counted map[string]bool
	// pending holds requests until they are kept or dropped. due records a window or result
	// that called for another send while one was underway.
	pending []claim.ModelRequest
	sending bool
	due     bool
	// A firing with a different window number was canceled by a result.
	window, windows uint64
}

func startModelRequests(poster *daemonPoster) *modelRequests {
	return &modelRequests{daemonPoster: poster, counted: map[string]bool{}}
}

// streamLine is what read takes of one stream-json line of Claude Code's.
type streamLine struct {
	kind    string
	message json.RawMessage
	// parent is the Task call a subagent's line serves, "" on the role's own; agent the
	// subagent's id; session the session's id.
	parent, agent, session string
	// timestamp is when the line was written, "" where it names none.
	timestamp string
}

// read acts on one line of the stream, on the bridge's loop: it closes the request whose lines
// were being read at the first line that is not one of its own, opens the next, and at a `result`
// sends what is pending.
func (r *modelRequests) read(line streamLine) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if line.kind == "assistant" {
		r.assistant(line)
		return
	}
	r.closeLocked()
	if line.kind == "result" {
		r.window = 0
		r.sendLocked()
	}
}

// assistant reads the request's metadata. The synthetic answer Claude Code gives a request it
// could not have answered is no request. The caller holds r.mu.
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

// sessionOfLine is the session a line's request ran in: the subagent's where the line serves a
// Task call, else the session's own.
func sessionOfLine(line streamLine) string {
	if line.parent != "" && line.agent != "" {
		return line.agent
	}
	return line.session
}

// endOf is when a line was written, as the line says, and the bridge's own time for a line that
// says none.
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
	cutoff := r.clock.Now().Add(-claim.ModelRequestsSpan)
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
// report the daemon refused, which no retry passes.
func (r *modelRequests) call(batch []claim.ModelRequest) (map[string]struct{}, bool) {
	body, err := json.Marshal(claim.ModelRequestsRequest{
		BootCaller: claim.BootCaller{BootToken: r.bootToken},
		Generation: r.generation,
		Requests:   batch,
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
