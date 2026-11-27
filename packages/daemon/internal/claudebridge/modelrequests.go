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
// of the minute the provider's headroom reads (dispatch://INFRA-5899, spec, "The model requests").
// One window serves the whole bridge, whatever session a request ran in: a window that sends
// sends every request the bridge holds in one report. It is due at the earlier of two times
// (windowDueLocked): the window's flush, requestWindow - requestSendMargin after the oldest end
// among the requests the bridge holds but no sooner than that after the last flush; and the
// deadline of the oldest request no flush has taken yet, requestWindow - requestSendMargin after
// its own end. So each request leaves within requestWindow of its end on the bridge's clock, and
// one the bridge reads only after its deadline leaves at once. A deadline comes before the
// window's flush only for a request that ended before the last flush, which the bridge read only
// after it: such a request adds a report. Without one, a pane's windows send at most one report
// each 14.75 s, about four a minute. A turn's end sends what is left at once (modelRequests).
const requestWindow = 15 * time.Second

// requestSendMargin is how much sooner than requestWindow after an end the window is due: room
// for the window's own work before the report leaves, so a request the bridge read by its
// deadline leaves within requestWindow of its end.
const requestSendMargin = 250 * time.Millisecond

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
// and a line of another session closes nothing. A window closes every open request too, so a
// request is reported within requestWindow of its last block however long the tool it called
// runs. No close can know that no block of the request follows: Claude Code writes a tool's result
// while the request's next tool call is still being written
// (testdata/background-during-prompt.jsonl:7,10,11), and a block can take longer than
// requestWindow to write. So a block of a closed request opens it again, with that block's end: a
// request not yet sent is taken back from what is pending, and one already sent goes again under
// the same id with the later end, which the daemon moves the request to
// (record.Store.KeepModelRequests). No block is left out of its request's end.
//
// One window serves the bridge, whatever session a request ran in. It is due requestSendMargin
// before requestWindow from the oldest end of a request open or pending, no sooner than
// requestWindow - requestSendMargin after the last flush, but no later than requestSendMargin
// before requestWindow from the end of a request no flush has taken yet (windowDueLocked). The
// first request arms it, and a request whose own deadline comes before the window armed arms it
// again for that deadline. A window that fires before it is due, since a block moved the oldest
// end on, is armed again for that end; one that is due closes every open request and sends what
// is pending in one report (flushLocked), as a result does at once. Overflow drains on an answer
// that keeps at least one request. Otherwise a failed or incomplete report can retry at the next
// window or result; only IDs the daemon did not keep go again. Requests past the keep window and
// reports the daemon refuses (a 4xx) are dropped with a line. Every report names the launch's boot
// token. Nothing in the bridge's loop waits on a report.
type modelRequests struct {
	*daemonPoster
	// windowAfter is the timer the window runs on: the bridge's clock's, or a test's
	// (Config.requestWindows), which moves the window alone.
	windowAfter func(time.Duration) <-chan time.Time

	mu sync.Mutex
	// open is each session's open request, by its session.
	open map[string]*claim.ModelRequest
	// pending holds requests until they are kept or dropped. due records a window or result
	// that called for another send while one was underway.
	pending []pendingRequest
	sending bool
	due     bool
	// flushed is when the bridge last closed every open request and sent what was pending, at a
	// window or a result (flushLocked), on the bridge's clock.
	flushed time.Time
	// A firing with a different window number was canceled by a result, or by a request due
	// before it (assistant). windowDue is when the window armed is due to fire.
	window, windows uint64
	windowDue       time.Time
}

// pendingRequest is a request the bridge holds closed. taken is whether a flush took it
// (flushLocked): it waits behind a report on its way or for the room a full report left, or goes
// again after a report that failed, at a window's flush and never by a deadline of its own
// (windowDueLocked).
type pendingRequest struct {
	claim.ModelRequest
	taken bool
}

// startModelRequests is the bridge's producer of model requests, its window on windows, or on the
// bridge's clock where windows is nil (Config.requestWindows).
func startModelRequests(poster *daemonPoster, windows func(time.Duration) <-chan time.Time) *modelRequests {
	if windows == nil {
		windows = poster.clock.After
	}
	return &modelRequests{daemonPoster: poster, windowAfter: windows, open: map[string]*claim.ModelRequest{}}
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
		r.flushLocked()
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
	end := r.endOf(line)
	if open := r.open[session]; open != nil && open.ID == m.ID {
		open.EndedAt = later(open.EndedAt, end)
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
	if i := slices.IndexFunc(r.pending, func(p pendingRequest) bool { return p.ID == m.ID }); i >= 0 {
		request.EndedAt = later(r.pending[i].EndedAt, end)
		r.pending = slices.Delete(r.pending, i, i+1)
	}
	r.open[session] = &request
	// A request that ended before the last flush, which the bridge read only after it, can be due
	// before the window armed: the window is armed again for its deadline (windowDueLocked).
	if due := request.EndedAt.Add(requestWindow - requestSendMargin); r.window != 0 && due.Before(r.windowDue) {
		r.armAtLocked(due)
		return
	}
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
	r.pending = append(r.pending, pendingRequest{ModelRequest: *open})
}

// closeOpenLocked queues every open request, in the order they ended, so a report lists the
// requests of its sessions as they happened. The caller holds r.mu.
func (r *modelRequests) closeOpenLocked() {
	var sessions []string
	for session := range r.open {
		sessions = append(sessions, session)
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

// endOf is when a line was written, as the line says, and for a line that says none the bridge's
// clock's time, the clock the window runs on (windowDueLocked).
func (r *modelRequests) endOf(line streamLine) time.Time {
	if at, err := time.Parse(time.RFC3339Nano, line.timestamp); err == nil {
		return at
	}
	return r.clock.Now().UTC()
}

// later is the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// earlier is the earlier of a and b, b where a is zero, none yet.
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// windowDueLocked is when the window is due, and whether the bridge holds a request, open or
// pending. It is due at the earlier of two times. One is the window's flush: requestWindow -
// requestSendMargin after the oldest end among the requests held, but no sooner than that after
// the last flush, so the flushes send one report each that long at most and a report the daemon
// failed goes again no sooner. The other is the deadline of the oldest request no flush has taken
// yet (pendingRequest; every open request is one): requestWindow - requestSendMargin after its
// own end, so each request goes within requestWindow of its end, and one the bridge reads only
// after its deadline goes at once. A deadline comes before the window's flush only for a request
// that ended before the last flush, which the bridge read only after it. The caller holds r.mu.
func (r *modelRequests) windowDueLocked() (time.Time, bool) {
	if len(r.open) == 0 && len(r.pending) == 0 {
		return time.Time{}, false
	}
	// oldest is the oldest end among the requests held, and owed among those no flush has taken.
	var oldest, owed time.Time
	for _, open := range r.open {
		oldest, owed = earlier(oldest, open.EndedAt), earlier(owed, open.EndedAt)
	}
	for _, request := range r.pending {
		oldest = earlier(oldest, request.EndedAt)
		if !request.taken {
			owed = earlier(owed, request.EndedAt)
		}
	}
	return earlier(owed, later(oldest, r.flushed)).Add(requestWindow - requestSendMargin), true
}

// armLocked arms the window, if none is armed and the bridge holds a request, to fire when it is
// due (windowDueLocked). The caller holds r.mu.
func (r *modelRequests) armLocked() {
	if r.window != 0 {
		return
	}
	if due, held := r.windowDueLocked(); held {
		r.armAtLocked(due)
	}
}

// armAtLocked arms the window to fire at due, in place of any window armed. When it fires, it
// arms nothing where the bridge holds nothing any more, is armed again for when the window is due
// where that is still to come, and else flushes. The caller holds r.mu.
func (r *modelRequests) armAtLocked(due time.Time) {
	r.windows++
	r.window, r.windowDue = r.windows, due
	window, fires := r.window, r.windowAfter(max(due.Sub(r.clock.Now()), 0))
	go func() {
		<-fires
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.window != window {
			return
		}
		r.window = 0
		due, held := r.windowDueLocked()
		if !held {
			return
		}
		// A block moved the oldest end on since the window was armed.
		if r.clock.Now().Before(due) {
			r.armAtLocked(due)
			return
		}
		r.flushLocked()
	}()
}

// flushLocked closes every open request and sends what is pending, at a window that is due or at
// a turn's result, and cancels the window armed: the next request arms the next. Each request it
// leaves pending it has taken (pendingRequest). The caller holds r.mu.
func (r *modelRequests) flushLocked() {
	r.closeOpenLocked()
	for i := range r.pending {
		r.pending[i].taken = true
	}
	r.window = 0
	r.flushed = r.clock.Now()
	r.sendLocked()
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
	r.pending = slices.DeleteFunc(r.pending, func(request pendingRequest) bool {
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
	batch := make([]claim.ModelRequest, n)
	for i, request := range r.pending[:n] {
		batch[i] = request.ModelRequest
	}
	r.pending = slices.Delete(r.pending, 0, n)
	r.sending = true
	go r.post(batch, len(r.pending) != 0)
}

// post makes one report and puts back every request of it the daemon did not say it holds, under
// the same id and taken (pendingRequest), but for one a block of which opened it again while the
// report was on its way (assistant): that request is open or pending already, with the later end.
func (r *modelRequests) post(batch []claim.ModelRequest, overflow bool) {
	kept, retry := r.call(batch)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sending = false
	progress := false
	if retry {
		var unheld []pendingRequest
		for _, request := range batch {
			if _, ok := kept[request.ID]; ok {
				progress = true
			} else if !r.queuedLocked(request) {
				unheld = append(unheld, pendingRequest{ModelRequest: request, taken: true})
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
	if i := slices.IndexFunc(r.pending, func(p pendingRequest) bool { return p.ID == request.ID }); i >= 0 {
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
