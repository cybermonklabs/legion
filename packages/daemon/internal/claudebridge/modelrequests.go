package claudebridge

import (
	"cmp"
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
//     each content block. Each carries the request's `usage` and the time the block was written,
//     and none says it is the request's last (`stop_reason` is null on each). The bridge counts it
//     once by its id, ended at its last block;
//   - a subagent's lines are the same, with `parent_tool_use_id` naming the Task call and
//     `agent_id` the subagent, which is the session reported for them; the role's own lines carry
//     `parent_tool_use_id` null and the session's id. The lines of a subagent and of the role, or
//     of two subagents, interleave;
//   - an attempt Claude Code retries is a `system` `api_retry` line, and the last attempt of a
//     request refused for good is Claude Code's synthetic answer (model `<synthetic>`). Neither is
//     an answered request, so neither is reported: a retry the next attempt answered is reported
//     as the answer alone.
//
// Uncached input is `input_tokens + cache_creation_input_tokens`, never the cache reads.
//
// Each session (sessionOfLine) has at most one open request, the one its lines are of: a session
// runs one request at a time, so a later line of the session, or the turn's `result`, closes it,
// and a line of another session closes nothing. A request whose newest line is requestWindow old
// on the bridge's clock is closed too, so a request is reported within requestWindow of its last
// block however long the tool it called runs. Neither close can know that no block of the request
// follows: Claude Code writes a tool's result while the request's next tool call is still being
// written (testdata/background-during-prompt.jsonl:7,10,11), and a block can take longer than
// requestWindow to write. So a block of a closed request opens it again, with that block's end: a
// request not yet sent is taken back from what is pending, and one already sent goes again under
// the same id with the later end, which the daemon moves the request to
// (record.Store.KeepModelRequests). No block is left out of its request's end.
//
// The first request arms a window, which fires when the oldest newest line of an open request is
// requestWindow old, or requestWindow after it was armed where none is open. It closes each open
// request that is due, sends what is pending, and arms the next window while a request is open. A
// result sends any pending requests now. Overflow drains on an answer that keeps at least one
// request. Otherwise a failed or incomplete report can retry at the next window or result; only
// IDs the daemon did not keep go again. Requests past the keep window and reports the daemon
// refuses (a 4xx) are dropped with a line. Every report names the launch's boot token. Nothing in
// the bridge's loop waits on a report.
type modelRequests struct {
	*daemonPoster

	mu sync.Mutex
	// open is each session's open request, by its session; counted every id closed once.
	open    map[string]*openRequest
	counted map[string]bool
	// pending holds requests until they are kept or dropped. due records a window or result
	// that called for another send while one was underway.
	pending []claim.ModelRequest
	sending bool
	due     bool
	// A firing with a different window number was canceled by a result.
	window, windows uint64
}

// openRequest is a session's open request, and when the bridge read its newest line, on the
// bridge's clock.
type openRequest struct {
	claim.ModelRequest
	seen time.Time
}

func startModelRequests(poster *daemonPoster) *modelRequests {
	return &modelRequests{daemonPoster: poster, open: map[string]*openRequest{}, counted: map[string]bool{}}
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

// read acts on one line of the stream, on the bridge's loop: it closes the open request of the
// line's session, opens the next, and at a `result` closes every open request and sends what is
// pending.
func (r *modelRequests) read(line streamLine) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch line.kind {
	case "assistant":
		r.assistant(line)
	case "result":
		// The turn is over, and each request a host reports is reported by its turn's end.
		r.closeOpenLocked(func(*openRequest) bool { return true })
		r.window = 0
		r.sendLocked()
	default:
		r.closeLocked(sessionOfLine(line))
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
	session := sessionOfLine(line)
	if m.Model == shimwire.SyntheticModel {
		r.closeLocked(session)
		return
	}
	end := endOf(line)
	if open := r.open[session]; open != nil && open.ID == m.ID {
		open.EndedAt, open.seen = later(open.EndedAt, end), r.clock.Now()
		return
	}
	r.closeLocked(session)
	if m.ID == "" || m.Model == "" {
		return
	}
	request := claim.ModelRequest{ID: m.ID, Session: session, Model: m.Model, EndedAt: end}
	if m.Usage != nil {
		request.UncachedInputTokens = new(m.Usage.Input + m.Usage.CacheWrite)
	}
	// A block of a request closed already (modelRequests' doc): the request opens again with this
	// block's end, taken back from what is pending where it was not sent yet, and sent again
	// under its id where it was.
	if r.counted[m.ID] {
		if i := slices.IndexFunc(r.pending, func(p claim.ModelRequest) bool { return p.ID == m.ID }); i >= 0 {
			request.EndedAt = later(r.pending[i].EndedAt, end)
			r.pending = slices.Delete(r.pending, i, i+1)
		}
	}
	r.open[session] = &openRequest{ModelRequest: request, seen: r.clock.Now()}
	r.armLocked()
}

// closeLocked queues the open request of session, if it has one, which already armed a window
// when it opened. The caller holds r.mu.
func (r *modelRequests) closeLocked(session string) {
	open := r.open[session]
	if open == nil {
		return
	}
	delete(r.open, session)
	r.counted[open.ID] = true
	r.pending = append(r.pending, open.ModelRequest)
}

// closeOpenLocked queues each open request that is due, in the order they ended, so a report lists
// the requests of its sessions as they happened. The caller holds r.mu.
func (r *modelRequests) closeOpenLocked(due func(*openRequest) bool) {
	var sessions []string
	for session, open := range r.open {
		if due(open) {
			sessions = append(sessions, session)
		}
	}
	slices.SortFunc(sessions, func(a, b string) int {
		return cmp.Or(r.open[a].EndedAt.Compare(r.open[b].EndedAt), cmp.Compare(r.open[a].ID, r.open[b].ID))
	})
	for _, session := range sessions {
		r.closeLocked(session)
	}
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

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// armLocked arms a window for what is open or pending, if none is armed: it fires when the oldest
// newest line of an open request is requestWindow old, or requestWindow from now where no request
// is open. The caller holds r.mu.
func (r *modelRequests) armLocked() {
	if r.window != 0 || (len(r.pending) == 0 && len(r.open) == 0) {
		return
	}
	now := r.clock.Now()
	wait := requestWindow
	for _, open := range r.open {
		wait = min(wait, open.seen.Add(requestWindow).Sub(now))
	}
	r.windows++
	r.window = r.windows
	window, fires := r.window, r.clock.After(max(wait, 0))
	go func() {
		<-fires
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.window != window {
			return
		}
		r.window = 0
		now := r.clock.Now()
		r.closeOpenLocked(func(open *openRequest) bool { return !now.Before(open.seen.Add(requestWindow)) })
		r.sendLocked()
		if len(r.open) != 0 {
			r.armLocked()
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
// the same id, but for one a block of which opened it again while the report was on its way
// (assistant): that request is open or pending already, with the later end.
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
			} else if !r.queuedLocked(request) {
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

// queuedLocked says whether request's id is open or pending, and moves that entry's end to
// request's where request's is later. The caller holds r.mu.
func (r *modelRequests) queuedLocked(request claim.ModelRequest) bool {
	if open := r.open[request.Session]; open != nil && open.ID == request.ID {
		open.EndedAt = later(open.EndedAt, request.EndedAt)
		return true
	}
	if i := slices.IndexFunc(r.pending, func(p claim.ModelRequest) bool { return p.ID == request.ID }); i >= 0 {
		r.pending[i].EndedAt = later(r.pending[i].EndedAt, request.EndedAt)
		return true
	}
	return false
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
