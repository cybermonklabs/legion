package claudebridge

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// requestWindow is how long after a model request's end the bridge may hold its report, and how
// long after the end of the call that put a request back it is due again: a quarter of the minute
// the provider's headroom reads (dispatch://INFRA-5899, spec, "The model requests", modelRequests'
// doc).
const requestWindow = 15 * time.Second

// requestSendMargin is how much sooner than requestWindow after an end the window is due: room
// for the window's own work before the report leaves (modelRequests' doc).
const requestSendMargin = 250 * time.Millisecond

// requestDueAfter is how long after a fresh request's end the window is due for it,
// requestSendMargin short of requestWindow (modelRequests' doc).
const requestDueAfter = requestWindow - requestSendMargin

// requestReportTimeout bounds one report's call, as reportTimeout bounds a turn outcome's: a call
// that reaches it fails (modelRequests' doc).
const requestReportTimeout = 10 * time.Second

// requestLeftDueAfter is how long after a flush the put-back requests it leaves past its one chunk
// of them are due (modelRequests' doc): requestReportTimeout and then requestWindow, when the chunk
// the flush took in their place is due again had its call failed at its timeout. So they make no
// flush of their own before that chunk could go again, and the calls open at once stay flat
// through a long stall. Oh My Pi waits as long, MODEL_REQUEST_LEFT_DUE_AFTER_MS in
// packages/pi-legion/src/model-requests.ts.
const requestLeftDueAfter = requestReportTimeout + requestWindow

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
// and a line of another session closes nothing. A window closes every open request too, so it
// takes a request however long the tool it called runs (windowDueLocked). No close can know that
// no block of the request follows: Claude Code writes a tool's result while the request's next
// tool call is still being written (testdata/background-during-prompt.jsonl:7,10,11), and a block
// can take longer than requestWindow to write. So a block of a closed request opens it again, with
// that block's end: a request not yet sent is taken back from what is pending, and one already
// sent goes again under the same id with the later end, which the daemon moves the request to
// (record.Store.KeepModelRequests). No block is left out of its request's end.
//
// When a request is sent. These are the rules of both hosts' reporters
// (dispatch://INFRA-5899/artifact/reporter-rules-md@v7), as the bridge keeps them, stated here
// once. Every other comment of the bridge's model requests that names when a request is sent, put
// back, dropped or due points here and restates none of it, or defines a constant: it names what
// the constant is and points here. A test's comment describes its own row's scenario.
//
//   - Words. A fresh request is one no call put back. A put-back request is one a call put back
//     (pendingRequest's taken, post): every request of a call that failed, and each request an
//     answer did not keep. A call fails on every ending but an answer and a 4xx other than 408 or
//     429, whoever sent the 4xx (call): the bridge cannot encode the report or build its request,
//     the connection is refused or drops, the call reaches requestReportTimeout (10 s), the answer
//     cannot be read, or the answer is a 5xx, a 408 or a 429, from the daemon or a proxy, whatever
//     its body. A report is one call, a chunk of at most claim.MaxModelRequests (200) requests, all
//     fresh or all put back; a report on its way is a call that has not ended yet.
//   - A fresh request, open or pending, is due requestDueAfter after its end, requestSendMargin
//     (250 ms) short of requestWindow (15 s), on the bridge's clock, and one the bridge reads after
//     that moment is due at once. One window serves the bridge, whatever session a request ran in,
//     armed for the earliest moment due (windowDueLocked): the first request arms it, and a request
//     whose own moment comes before the window armed arms it again (armLocked). A window that
//     fires before it is due, since a block moved the oldest end on, is armed again; one that is
//     due closes every open request and flushes (flushLocked), as a turn's `result` does. A flush
//     sends every fresh request pending at once, whatever report is on its way, in chunks of at
//     most 200, each a call of its own that waits for no answer.
//   - A put-back request. S1 (dispatch://INFRA-5899/spec) does not bound it: the retry rule that
//     follows does. It is due requestWindow after the end of the call that put it back
//     (windowDueLocked), not requestDueAfter, whose margin is room for a fresh request's report
//     before S1's bound, and only a due put-back request makes a flush. Before it is due, it may
//     ride in the one chunk of put-back requests of a flush that something else made: a fresh
//     request's moment, a turn's `result`, or another put-back request that is due. So a daemon
//     that fails, or that answers and keeps nothing, is not called in a loop: a call's end only
//     arms the window. Each flush carries at most one chunk of put-back requests, the oldest end
//     first, and the put-back requests it leaves past that chunk are due requestLeftDueAfter (25 s)
//     after that flush, which keeps the calls open at once flat through a long stall. No chunk of
//     put-back requests leaves while another is on its way (retrying): until that chunk's call
//     ends, the other put-back requests make the window due for nothing and no flush takes them;
//     its end, by its answer or its timeout, arms the window for them again (post), at once where
//     one is due.
//   - Answers. Reports cross, and their answers arrive in either order: each answer puts back only
//     its own report's requests, and the daemon takes a request again by its id and keeps the later
//     end (record.Store.KeepModelRequests), so a crossing changes nothing it decides.
//   - Dropped. A report answered with a 4xx other than 408 or 429, by the daemon or a proxy, is
//     dropped with a line, and its requests are not put back. A request the contract's
//     per-request schema refuses (claim.ModelRequestProblem) is dropped where the bridge reads it
//     (assistant), with a line naming its id and the reason, so it never shares a report and the
//     session's other requests still leave. A request older than the hour the host keeps
//     (claim.ModelRequestsSpan) is dropped with a line at a flush.
//   - A turn's `result` closes every open request and flushes: every pending fresh request leaves
//     at once, and put-back requests follow their rule.
//   - The bridge has no send at its exit. When Claude Code exits or the bridge stops, every
//     request it holds, open or pending, put-back requests included, is lost with the process, and
//     so is every report on its way that the exit cuts short. A turn's `result` flushes, so what
//     an exit loses is the put-back requests pending and what a turn cut short left open.
//   - A stall or an outage of the daemon. Fresh requests still leave by their moments and each call
//     ends by its timeout, so the calls open at once are no more than the fresh chunks of the last
//     10 s and one chunk of put-back requests, whatever the length of the stall.
//
// Every report names the launch's boot token. Nothing in the bridge's loop waits on a report.
type modelRequests struct {
	*daemonPoster
	// windowAfter is the timer the window runs on: the bridge's clock's, or a test's
	// (Config.requestWindows), which moves the window alone.
	windowAfter func(time.Duration) <-chan time.Time

	mu sync.Mutex
	// open is each session's open request, by its session.
	open map[string]*claim.ModelRequest
	// pending holds requests not on their way, until they are kept or dropped. posting is how
	// many reports are on their way, and retrying whether a chunk of put-back requests is one of
	// them (modelRequests' doc).
	pending  []pendingRequest
	posting  int
	retrying bool
	// A firing with a different window number was canceled by a result, or by a window due before
	// it (armLocked). windowDue is when the window armed is due to fire.
	window, windows uint64
	windowDue       time.Time
}

// pendingRequest is a request the bridge holds closed. taken is whether it is a put-back request,
// and again when a put-back request is due (modelRequests' doc).
type pendingRequest struct {
	claim.ModelRequest
	taken bool
	again time.Time
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
// line's session, opens the next, and at a `result` flushes (flushLocked).
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
		// The turn is over.
		r.flushLocked()
	default:
		r.closeLocked(sessionOfLine(line))
	}
}

// assistant reads the request's metadata. The synthetic answer Claude Code gives a request it
// could not have answered is no request, and a request claim.ModelRequestProblem refuses is
// dropped here (modelRequests' doc). The caller holds r.mu.
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
	request := claim.ModelRequest{ID: m.ID, Session: session, Model: m.Model, EndedAt: end}
	if m.Usage != nil {
		request.UncachedInputTokens = new(m.Usage.Input + m.Usage.CacheWrite)
	}
	if problem := claim.ModelRequestProblem(request); problem != "" {
		r.log.Printf("dropped model request %q: the daemon refuses it: %s", m.ID, problem)
		return
	}
	// A block of a request closed already opens it again (modelRequests' doc).
	if i := slices.IndexFunc(r.pending, func(p pendingRequest) bool { return p.ID == m.ID }); i >= 0 {
		request.EndedAt = later(r.pending[i].EndedAt, end)
		r.pending = slices.Delete(r.pending, i, i+1)
	}
	r.open[session] = &request
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

// windowDueLocked is when the window is due, and whether a request the bridge holds makes it due,
// by the rules of modelRequests' doc. The caller holds r.mu.
func (r *modelRequests) windowDueLocked() (time.Time, bool) {
	var due time.Time
	for _, open := range r.open {
		due = earlier(due, open.EndedAt.Add(requestDueAfter))
	}
	for _, request := range r.pending {
		switch {
		case !request.taken:
			due = earlier(due, request.EndedAt.Add(requestDueAfter))
		case !r.retrying:
			due = earlier(due, request.again)
		}
	}
	return due, !due.IsZero()
}

// armLocked arms the window to fire when it is due (windowDueLocked), where a request makes it due
// and no window is armed for that time or sooner: a request read late, or a put-back request a
// chunk on its way held back, can be due before the window armed. The caller holds r.mu.
func (r *modelRequests) armLocked() {
	if due, held := r.windowDueLocked(); held && (r.window == 0 || due.Before(r.windowDue)) {
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

// flushLocked is a flush (modelRequests' doc): it closes every open request, cancels the window
// armed and sends (sendLocked); a later request, or a call's end, arms the next window
// (armLocked). The caller holds r.mu.
func (r *modelRequests) flushLocked() {
	r.closeOpenLocked()
	r.window = 0
	r.sendLocked()
}

// sendLocked sends what a flush sends, drops what it drops and leaves the rest pending, as
// modelRequests' doc says. The caller holds r.mu.
func (r *modelRequests) sendLocked() {
	if len(r.pending) == 0 {
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
	var untaken []claim.ModelRequest
	var taken []pendingRequest
	for _, request := range r.pending {
		if request.taken {
			taken = append(taken, request)
		} else {
			untaken = append(untaken, request.ModelRequest)
		}
	}
	for chunk := range slices.Chunk(untaken, claim.MaxModelRequests) {
		r.posting++
		go r.post(chunk, false)
	}
	if r.retrying {
		r.pending = taken
		return
	}
	slices.SortStableFunc(taken, func(a, b pendingRequest) int { return a.EndedAt.Compare(b.EndedAt) })
	retry := taken[:min(len(taken), claim.MaxModelRequests)]
	left := taken[len(retry):]
	leftDue := r.clock.Now().Add(requestLeftDueAfter)
	for i := range left {
		left[i].again = later(left[i].again, leftDue)
	}
	r.pending = left
	if len(retry) == 0 {
		return
	}
	batch := make([]claim.ModelRequest, len(retry))
	for i, request := range retry {
		batch[i] = request.ModelRequest
	}
	r.posting++
	r.retrying = true
	go r.post(batch, true)
}

// post makes one report, and puts back under the same id each of its requests that modelRequests'
// doc makes a put-back request, due as that doc says, but for one a block of which opened it again
// while the report was on its way (assistant), or an earlier report's answer already put back:
// that request is open or pending already, and takes the later end and due (queuedLocked). Its end
// clears retrying where batch is a chunk of put-back requests, and arms the window (armLocked).
func (r *modelRequests) post(batch []claim.ModelRequest, retrying bool) {
	kept, retry := r.call(batch)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posting--
	if retrying {
		r.retrying = false
	}
	if retry {
		again := r.clock.Now().Add(requestWindow)
		var unheld []pendingRequest
		for _, request := range batch {
			if _, ok := kept[request.ID]; !ok && !r.queuedLocked(request, again) {
				unheld = append(unheld, pendingRequest{ModelRequest: request, taken: true, again: again})
			}
		}
		if len(unheld) != 0 {
			r.pending = append(unheld, r.pending...)
		}
	}
	r.armLocked()
}

// queuedLocked says whether request's id is open or pending, and moves that entry's end to
// request's where request's is later, and a pending put-back entry's due to again where again is
// later. The caller holds r.mu.
func (r *modelRequests) queuedLocked(request claim.ModelRequest, again time.Time) bool {
	if open := r.open[request.Session]; open != nil && open.ID == request.ID {
		open.EndedAt = later(open.EndedAt, request.EndedAt)
		return true
	}
	if i := slices.IndexFunc(r.pending, func(p pendingRequest) bool { return p.ID == request.ID }); i >= 0 {
		r.pending[i].EndedAt = later(r.pending[i].EndedAt, request.EndedAt)
		if r.pending[i].taken {
			r.pending[i].again = later(r.pending[i].again, again)
		}
		return true
	}
	return false
}

// call posts batch and answers the ids the daemon holds, and whether the rest go again, as
// modelRequests' doc says of each ending of a call.
func (r *modelRequests) call(batch []claim.ModelRequest) (map[string]struct{}, bool) {
	body, err := json.Marshal(claim.ModelRequestsRequest{
		BootCaller: claim.BootCaller{BootToken: r.bootToken},
		Generation: r.generation,
		Requests:   batch,
	})
	if err != nil {
		r.log.Printf("could not encode a report of %d model requests; they go again: %v", len(batch), err)
		return nil, true
	}
	// The answer echoes IDs from this JSON body and no other request fields, so even a full
	// batch of long IDs fits inside the body size. Keep room for ordinary refusals too.
	bound := max(int64(len(body)), int64(maxAnswerBytes))
	// A request the bridge could not build was never attempted, and answers no status, as a call
	// that never reached the daemon.
	response, _, err := r.postJSON("/legion/v1/claims/model-requests", body, requestReportTimeout, errRequestReportTimeout, bound)
	switch {
	case response.statusCode == 0:
		r.log.Printf("could not report %d model requests; they go again: %v", len(batch), err)
		return nil, true
	case response.statusCode/100 == 4 && response.statusCode != http.StatusRequestTimeout && response.statusCode != http.StatusTooManyRequests:
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
