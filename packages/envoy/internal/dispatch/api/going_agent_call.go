package api

// going_agent_call.go runs the agent's calls (agentwire.CallRequest): at most goingCallSlots at
// once in the server, each on a context of its own bounded by goingCallTime, so a call whose agent
// died runs to its end, and each under a recover that answers a panic of its route as an error and
// logs it with its stack, so the server serves on; the row of a write whose call panicked is kept
// failed first (recordRowPanicked). Each call is made as the run's person, resolved from the run's
// grant at the call (goingGrantCaller), after its lease is checked.
//
// A call of kind route runs a tool's rows through the server's own route table in-process
// (goingInProcess), through a writer that keeps at most agentwire.CallBodyLimit of the answer and
// marks the rest cut. A write tool runs only as the approved row of the run's plan the call names
// (going_plan_run.go). Each request a write sends carries a landing (goingRunLanding) whose step, in
// the route's own transaction, checks the lease under the run's row locked for share and writes
// the request's going_landings row, keyed by the run, the step, the call and the request's number
// within the call: so the change and the record that it landed commit together or not at all,
// and a stale owner's write is refused and rolled back. A second try of a call reads the rows of
// its key: a request that landed is answered from its row and not sent again, and the next one is
// sent. A second try while the first still runs waits for it (goingCallKeys), at most
// goingCallTime, then reads its rows or fails as unknown; across server processes the key's
// primary key holds the same line, since a second insert of a request's row waits on the first's
// and fails once it commits, which rolls back the second try's change with it.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/agentwire"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// goingCallSlots is how many calls the server runs for its agents at once; the next one
	// waits for a free slot.
	goingCallSlots = 16
	// goingCallTime bounds one call in the server, and the wait of a second try of a call key for
	// its first.
	goingCallTime = 60 * time.Second
)

// goingCallKey is the idempotency key of one call of a run: the run, the step and the call's
// number within it.
type goingCallKey struct {
	run        string
	step, call int
}

// goingCallKeys holds each call key while a try of it runs in this server.
type goingCallKeys struct {
	mu   sync.Mutex
	held map[goingCallKey]chan struct{}
}

// lock holds key, waiting up to wait for a try that holds it to end, and answers the release;
// false where the wait, or ctx, ended first.
func (k *goingCallKeys) lock(ctx context.Context, key goingCallKey, wait time.Duration) (func(), bool) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		k.mu.Lock()
		if k.held == nil {
			k.held = map[goingCallKey]chan struct{}{}
		}
		running, busy := k.held[key]
		if !busy {
			done := make(chan struct{})
			k.held[key] = done
			k.mu.Unlock()
			return func() {
				k.mu.Lock()
				delete(k.held, key)
				k.mu.Unlock()
				close(done)
			}, true
		}
		k.mu.Unlock()
		select {
		case <-running:
		case <-timer.C:
			return nil, false
		case <-ctx.Done():
			return nil, false
		}
	}
}

// goingCallResult is a call's answer, or its refusal.
type goingCallResult struct {
	answer  agentwire.CallAnswer
	refusal *agentwire.Error
}

// call runs one call of a run: it takes a slot, waiting for one while the agent waits, and runs
// the call on a goroutine of its own, counted until it ends, which holds the slot until then. The
// agent is answered when the call ends; an agent that stops waiting leaves the call to end on its
// own.
func (a *AgentListener) call(ctx context.Context, request agentwire.CallRequest) (agentwire.CallAnswer, *agentwire.Error) {
	if refusal := goingRunID(request.Run, request.LeaseToken); refusal != nil {
		return agentwire.CallAnswer{}, refusal
	}
	switch request.Kind {
	case agentwire.KindRoute, agentwire.KindGitHub, agentwire.KindWeb, agentwire.KindPlan, agentwire.KindReview, agentwire.KindBlock, agentwire.KindLine, agentwire.KindWords:
	default:
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "a call is of kind route, github, web, plan, review, block, line or words, not %q", request.Kind)
	}
	select {
	case a.slots <- struct{}{}:
	case <-ctx.Done():
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallFailed, "the agent stopped waiting for a slot")
	}
	done := make(chan goingCallResult, 1)
	a.calls.goDetached(func() {
		defer func() { <-a.slots }()
		running, cancel := context.WithTimeout(context.WithoutCancel(a.s.lifetime), a.callTime)
		defer cancel()
		done <- a.runCall(running, request)
	})
	select {
	case result := <-done:
		return result.answer, result.refusal
	case <-ctx.Done():
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallFailed, "the agent stopped waiting for the call")
	}
}

// goingCallRun is the run a call is made for, as its row stands at the call: its kind is
// agentwire.RunNewIssue for a new-issue run, "" for a run of its person's words, and its mode is
// going_runs.mode, ask or auto_review, "" for a run stored with none.
type goingCallRun struct {
	id, login, kind, level, mode string
	used                         agentwire.Budget
}

// runCall makes the call: it checks the run's lease, resolves the run's person from its grant,
// and makes the call of its kind as that person. A panic is answered CALL_PANICKED and logged with
// its stack.
func (a *AgentListener) runCall(ctx context.Context, request agentwire.CallRequest) (result goingCallResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("dispatch: a call of a command-bar run panicked", "run", request.Run, "kind", request.Kind,
				"tool", request.Tool, "panic", recovered, "stack", string(debug.Stack()))
			result = goingCallResult{refusal: goingAgentRefusal(agentwire.CodeCallPanicked,
				"the call panicked in the server, which recovered it and serves on: %v", recovered)}
		}
	}()
	run, refusal := a.callRun(ctx, request)
	if refusal != nil {
		return goingCallResult{refusal: refusal}
	}
	// A grant through IAP lasts the run's working budget (goingGrantCaller's budgetEnds), the
	// level's working time less what the run has spent of it.
	budget := agent.Levels[run.level].Time - time.Duration(run.used.WorkingMS)*time.Millisecond
	caller, err := a.s.goingGrantCaller(ctx, run.id, time.Now().Add(budget))
	var ended goingGrantEnded
	switch {
	case errors.As(err, &ended):
		return goingCallResult{refusal: &agentwire.Error{Code: agentwire.CodeGrantEnded, Message: ended.Error(), Reason: ended.Reason}}
	case errors.Is(err, identity.ErrSignInUnavailable):
		return goingCallResult{refusal: goingAgentRefusal(agentwire.CodeSignInUnavailable, "%v", err)}
	case err != nil:
		return goingCallResult{refusal: goingAgentFailed(agentwire.CallCall, err)}
	}
	doer := a.s.goingDoer(caller, a.s.deps.ServerURL).(goingInProcess)
	doer.limit = agentwire.CallBodyLimit
	if hook := a.s.deps.AgentCallHook; len(hook.Rules) > 0 {
		doer.handler = hook.wrap(doer.handler)
	}
	var answer agentwire.CallAnswer
	switch request.Kind {
	case agentwire.KindRoute:
		answer, refusal = a.routeCall(ctx, request, run.login, doer)
	case agentwire.KindGitHub:
		answer, refusal = a.githubRead(ctx, request, doer)
	case agentwire.KindWeb:
		answer, refusal = a.webRead(ctx, request)
	case agentwire.KindPlan:
		answer, refusal = a.planCall(ctx, request, run, doer)
	case agentwire.KindReview:
		answer, refusal = a.reviewCall(ctx, request, run)
	case agentwire.KindBlock:
		answer, refusal = a.blockCall(ctx, request)
	case agentwire.KindLine:
		answer, refusal = a.lineCall(ctx, request, caller)
	case agentwire.KindWords:
		answer, refusal = a.wordsCall(ctx, request, run.login)
	}
	if refusal == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// What a route answered once the call's own time was over is not the call's answer: the
		// call failed, and a write that committed keeps its landing, which a second try answers.
		return goingCallResult{refusal: goingAgentRefusal(agentwire.CodeCallFailed, "the call ran past its %s in the server", a.callTime)}
	}
	return goingCallResult{answer: answer, refusal: refusal}
}

// AgentCallHook is a test hook on the calls the server runs for its agent (Deps.AgentCallHook),
// which an end-to-end test of the agent process sets: its rules, each on the requests of one
// route. The zero hook sets none.
type AgentCallHook struct {
	Rules []AgentCallRule
}

// AgentCallRule is one rule of an AgentCallHook: every request of Route ("METHOD /path") that a
// call sends panics, where Panic is set, or is held Hold, or until the call's own context ends -
// before the route answers it, or, where After is set, once the route answered it and before that
// answer reaches the call, so the route's write has committed and the call has not yet heard it.
type AgentCallRule struct {
	Route string
	Panic bool
	Hold  time.Duration
	After bool
}

// ParseAgentCallHook reads a hook as DISPATCH_TEST_AGENT_CALL_HOOK writes one: its rules, apart by
// ";", each "panic METHOD /path", "hold DURATION METHOD /path" or "after DURATION METHOD /path".
func ParseAgentCallHook(raw string) (AgentCallHook, error) {
	var hook AgentCallHook
	for part := range strings.SplitSeq(raw, ";") {
		rule, err := parseAgentCallRule(part)
		if err != nil {
			return AgentCallHook{}, err
		}
		hook.Rules = append(hook.Rules, rule)
	}
	return hook, nil
}

// parseAgentCallRule reads one rule of ParseAgentCallHook's.
func parseAgentCallRule(raw string) (AgentCallRule, error) {
	fields := strings.Fields(raw)
	var rule AgentCallRule
	switch {
	case len(fields) == 3 && fields[0] == "panic":
		rule.Panic = true
	case len(fields) == 4 && (fields[0] == "hold" || fields[0] == "after"):
		hold, err := time.ParseDuration(fields[1])
		if err != nil || hold <= 0 {
			return AgentCallRule{}, fmt.Errorf("%q is no positive Go duration", fields[1])
		}
		rule.Hold, rule.After = hold, fields[0] == "after"
	default:
		return AgentCallRule{}, errors.New("expected panic METHOD /path, hold DURATION METHOD /path or after DURATION METHOD /path, apart by ;")
	}
	method, path := fields[len(fields)-2], fields[len(fields)-1]
	if method != strings.ToUpper(method) || !strings.HasPrefix(path, "/") || strings.Contains(path, "?") {
		return AgentCallRule{}, errors.New("expected a method and a request path, such as GET /api/v1/search")
	}
	rule.Route = method + " " + path
	return rule, nil
}

// wrap is inner with the hook's rules in front of their routes: a request of a rule's route
// panics, or is held, before its route or after it, logged as it is held and as it is released, so
// a test reads when each call entered and left the hold.
func (h AgentCallHook) wrap(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at := slices.IndexFunc(h.Rules, func(rule AgentCallRule) bool { return rule.Route == r.Method+" "+r.URL.Path })
		if at < 0 {
			inner.ServeHTTP(w, r)
			return
		}
		rule := h.Rules[at]
		if rule.Panic {
			panic("the test hook DISPATCH_TEST_AGENT_CALL_HOOK panics " + rule.Route)
		}
		if rule.After {
			answered := &goingHeldAnswer{header: http.Header{}, status: http.StatusOK}
			inner.ServeHTTP(answered, r)
			if !rule.hold(r, "dispatch: a test hook holds the answer of a call of the agent", "dispatch: a test hook released the answer of a call of the agent") {
				return
			}
			maps.Copy(w.Header(), answered.header)
			w.WriteHeader(answered.status)
			_, _ = w.Write(answered.body.Bytes())
			return
		}
		if !rule.hold(r, "dispatch: a test hook holds a call of the agent", "dispatch: a test hook released a call of the agent") {
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// hold holds r for the rule's Hold or until r's context ends, logging held as it starts and
// released as it ends, and reports whether r's context is still live.
func (rule AgentCallRule) hold(r *http.Request, held, released string) bool {
	slog.Info(held, "route", rule.Route, "hold", rule.Hold)
	timer := time.NewTimer(rule.Hold)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
	}
	slog.Info(released, "route", rule.Route)
	return r.Context().Err() == nil
}

// goingHeldAnswer is a route's answer an after rule of an AgentCallHook holds before the call
// hears it: its header, its status and its body.
type goingHeldAnswer struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *goingHeldAnswer) Header() http.Header         { return a.header }
func (a *goingHeldAnswer) WriteHeader(status int)      { a.status = status }
func (a *goingHeldAnswer) Write(p []byte) (int, error) { return a.body.Write(p) }

// callRun reads the call's run and refuses a call whose lease token is not the run's, or whose
// run no longer works. A withdrawn run (goingRunWithdrawerOf) is stopped with its withdrawer's
// reason and refused as one that no longer works, so it reads nothing more, and writes nothing
// more of what it may quote.
func (a *AgentListener) callRun(ctx context.Context, request agentwire.CallRequest) (goingCallRun, *agentwire.Error) {
	run := goingCallRun{id: request.Run}
	var token *string
	var state *string
	var used []byte
	var withdrawer goingRunWithdrawer
	err := a.s.deps.Store.Pool.QueryRow(ctx, `
		select author_login, coalesce(kind, ''), coalesce(level, ''), coalesce(mode, ''), budget_used, lease_token::text, state, `+goingRunWithdrawerOf("r.id")+`
		from going_runs r where id = $1::uuid
	`, request.Run).Scan(&run.login, &run.kind, &run.level, &run.mode, &used, &token, &state, &withdrawer)
	if errors.Is(err, pgx.ErrNoRows) {
		return goingCallRun{}, goingStaleLease(request.Run)
	}
	if err != nil {
		return goingCallRun{}, goingAgentFailed(agentwire.CallCall, err)
	}
	if token == nil || *token != request.LeaseToken || state == nil || *state != "working" {
		return goingCallRun{}, goingStaleLease(request.Run)
	}
	if withdrawer != goingRunNotWithdrawn {
		if _, err := a.s.stopGoingRun(ctx, request.Run, withdrawer.reason()); err != nil {
			return goingCallRun{}, goingAgentFailed(agentwire.CallCall, err)
		}
		return goingCallRun{}, goingStaleLease(request.Run)
	}
	if err := json.Unmarshal(used, &run.used); err != nil {
		return goingCallRun{}, goingAgentFailed(agentwire.CallCall, err)
	}
	return run, nil
}

// goingLandedRequest is one request of a call key that landed, as its going_landings row holds
// it: what was sent, the status and body its route recorded, 0 and null where it recorded none,
// and the delivery recovery point it committed, nil for none, which the undo of a suggestion reads
// the suggestion's comment from (goingUndoer.suggestion).
type goingLandedRequest struct {
	method, path, digest string
	status               int
	body                 json.RawMessage
	recovery             json.RawMessage
}

// sends reports whether the row records the very request spec sends.
func (l goingLandedRequest) sends(spec goingSend) bool {
	return l.method == spec.method && l.path == spec.path && l.digest == spec.digest()
}

// answer is the call's answer from the row: the recorded status and body, replayed.
func (l goingLandedRequest) answer() agentwire.CallAnswer {
	answer := agentwire.CallAnswer{Status: l.status, Replayed: true}
	if len(l.body) > 0 && string(l.body) != "null" {
		answer.Body, answer.Cut = l.body, len(l.body) > agentwire.CallBodyLimit
		if answer.Cut {
			answer.Body = answer.Body[:agentwire.CallBodyLimit]
		}
		answer.ContentType = "application/json"
	}
	return answer
}

// landings reads the rows of key, by the number of each request; a number with no row is nil.
func (s *server) goingCallLandings(ctx context.Context, key goingCallKey) ([]*goingLandedRequest, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select request, method, path, body_digest, coalesce((answer->>'status')::int, 0), answer->'body'
		from going_landings
		where run_id = $1::uuid and phase = 'do' and step = $2 and call = $3
		order by request
	`, key.run, key.step, key.call)
	if err != nil {
		return nil, fmt.Errorf("read the landings of call %d of step %d: %w", key.call, key.step, err)
	}
	defer rows.Close()
	var landed []*goingLandedRequest
	for rows.Next() {
		var index int
		var request goingLandedRequest
		var body []byte
		if err := rows.Scan(&index, &request.method, &request.path, &request.digest, &request.status, &body); err != nil {
			return nil, err
		}
		request.body = body
		for len(landed) <= index {
			landed = append(landed, nil)
		}
		landed[index] = &request
	}
	return landed, rows.Err()
}

// routeCall makes a call of kind route: the tool's rows, by its name and arguments, through the
// route table in-process as the run's person (login). A write runs only as the row of the run's
// plan it names, held to it by rowGate under the call key's lock, with the row's arguments as
// rowGate answers them, each new issue of the plan they name by its key (resolveNewIssues), posted
// on the issue the row names where it is a send to an agent session (going_messages.go), and what
// it came to is kept on that row (recordRow), a panic of its route as a failure of the server's
// (recordRowPanicked), which then goes on to runCall. A second try of the call key whose own
// request landed answers that landing before the call is prepared again: a translation that reads
// would read what the landing wrote, and build another request - a set_issue_state's next seq
// over the list it added to - or refuse - a close of the issue it closed. A second try of the key
// sends only what its earlier tries did not land, and reads the row's item again before it sends
// the rest of a call that landed partly (rowItemChanged); a row's try under a key that has landed
// nothing takes over what an earlier try of the row landed before the row waited again
// (adoptLandings), before a translation that reads what of its first requests landed refuses, so
// the refusal says what landed. A refusal of the tool's after only requests it sends before its
// own (a close's reason) landed is the call's refusal, which says they stand. A row's create of an
// issue refused 409 POSSIBLE_DUPLICATE as a near copy of issues its own plan made, and of no other,
// is sent again with force as the call's next request, with a landing of its own under the call's
// key, so a second try of the key whose forced create landed answers that landing
// (forcedSiblingCopy). A read of an uploaded file's version answers what its file's words make of
// the route's answer (goingUploadedFile.told), and the roster read the agents list with the claims
// of each session and the gone sessions that hold one (goingRosterTold).
func (a *AgentListener) routeCall(ctx context.Context, request agentwire.CallRequest, login string, doer goingInProcess) (answer agentwire.CallAnswer, refusal *agentwire.Error) {
	key := goingCallKey{run: request.Run, step: request.Step, call: request.Call}
	release, held := a.keys.lock(ctx, key, a.callKeyWait)
	if !held {
		return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallUnknown,
			"an earlier try of call %d of step %d still runs, and whether it landed is unknown", request.Call, request.Step)
	}
	defer release()
	// answered is the path of the request whose answer the call answers, "" while none answered.
	// Each comment and issue message that answer carries the words of is recorded as the run's
	// before the agent is given it (recordGoingRunPosts), which withdraws the run once one of them
	// is deleted; a call whose posts cannot be recorded gives the agent nothing. The record is
	// deferred ahead of a write's own, so a write that landed is kept as landed whatever comes of
	// this, and a second try answers its landing and records again.
	answered := ""
	defer func() {
		if refusal != nil || answered == "" {
			return
		}
		if err := a.s.recordGoingRunPosts(ctx, request.Run, answered, answer.Body); err != nil {
			answer, refusal = agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, fmt.Errorf("record the posts its answer carries: %w", err))
		}
	}()
	var arguments map[string]any
	if len(request.Arguments) > 0 {
		if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
			return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeBadRequest, "the call's arguments are no JSON object: %v", err)
		}
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	landed, err := a.s.goingCallLandings(ctx, key)
	if err != nil {
		return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
	}
	var row goingPlanRowStored
	var specs []goingSend
	if _, writes := a.s.going.Lookup(request.Tool); writes {
		var gated *agentwire.Error
		row, gated = a.rowGate(ctx, request, login, doer, landed)
		if gated != nil {
			return agentwire.CallAnswer{}, gated
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				a.recordRowPanicked(ctx, request, specs)
				panic(recovered)
			}
			a.recordRow(ctx, request, answer, refusal)
		}()
		if arguments, err = goingRowArguments(row.arguments); err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
		}
	}
	if last := goingLastLanded(landed); last != nil && goingOwnLanded(a.s.going, request.Tool, *last) {
		// The call's own request, its last, landed under this key, so the whole call did: this
		// try answers that landing and prepares nothing.
		answered = last.path
		return last.answer(), nil
	}
	// resumed is this key's landings, with what an earlier try of the row landed of first, the
	// requests the call sends before its own, taken over (adoptLandings).
	var resumeRefusal *agentwire.Error
	resumed := func(first []goingSend) []*goingLandedRequest {
		taken, refused := a.adoptLandings(ctx, request, key, row, first, landed)
		if refused != nil {
			resumeRefusal = refused
			return landed
		}
		landed = taken
		return landed
	}
	specs, write, refusal := a.routeSends(ctx, request, arguments, doer, resumed, row)
	if resumeRefusal != nil {
		return agentwire.CallAnswer{}, resumeRefusal
	}
	if refusal != nil {
		return agentwire.CallAnswer{}, refusal
	}
	if write && len(specs) > 0 {
		if resumed(specs[:len(specs)-1]); resumeRefusal != nil {
			return agentwire.CallAnswer{}, resumeRefusal
		}
		if row.state != "approved" && goingPartlyLanded(specs, landed) {
			// A second try of the key that finds part of the call landed writes the rest: the
			// row's item is read again first, and a row whose item changed waits again, saying
			// what stands.
			if refusal := a.rowItemChanged(ctx, request, row, login, doer, goingLandedWords(specs, landed)); refusal != nil {
				return agentwire.CallAnswer{}, refusal
			}
		}
	}
	// len(specs) is read on each pass: a create sent again with force is one more request of the
	// call (forcedSiblingCopy).
	for index := 0; index < len(specs); index++ {
		spec := specs[index]
		if index < len(landed) && landed[index] != nil {
			if !landed[index].sends(spec) {
				return agentwire.CallAnswer{}, goingAgentRefusal(agentwire.CodeCallChanged,
					"request %d of this call landed as %s %s, and this try sends %s %s: a call key names one call", index,
					landed[index].method, landed[index].path, spec.method, spec.path)
			}
			answer, answered = landed[index].answer(), landed[index].path
			continue
		}
		landing := goingRunLanding{key: key, token: request.LeaseToken, request: index, method: spec.method, path: spec.path,
			digest: spec.digest(), issue: goingIssueOfPath(spec.path)}
		var sent *http.Request
		if write {
			sent, err = spec.build(context.WithValue(ctx, goingLandingKey{}, landing), "")
		} else {
			sent, err = spec.build(ctx, "")
		}
		if err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
		}
		recorded := doer.serve(sent)
		answer, answered = goingCallAnswerOf(recorded), spec.path
		if spec.file != nil {
			answer = spec.file.told(answer)
		}
		if spec.roster {
			if answer, err = a.s.goingRosterTold(ctx, login, answer); err != nil {
				return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
			}
		}
		if answer.Status < http.StatusMultipleChoices || !write {
			if answer.Status >= http.StatusMultipleChoices {
				return answer, nil
			}
			continue
		}
		// A write answered a refusal: it may still have landed - a route that commits and then
		// answers 5xx, or a second try in another process whose row came first - as its row says.
		rows, err := a.s.goingCallLandings(ctx, key)
		if err != nil {
			return agentwire.CallAnswer{}, goingAgentFailed(agentwire.CallCall, err)
		}
		if index < len(rows) && rows[index] != nil && rows[index].sends(spec) {
			if answer.Status >= http.StatusInternalServerError {
				answer, answered = rows[index].answer(), spec.path
			}
			continue
		}
		if _, stale := a.callRun(ctx, request); stale != nil {
			return agentwire.CallAnswer{}, stale
		}
		// A create refused as a near copy of issues its own plan made, and of no other, is sent
		// again with force, as the call's next request (forcedSiblingCopy).
		forced, again, failed := a.forcedSiblingCopy(ctx, request, spec, answer)
		if failed != nil {
			return agentwire.CallAnswer{}, failed
		}
		if !again {
			return answer, nil
		}
		specs = append(specs, forced)
	}
	return answer, nil
}

// goingLastLanded is the last request of a call key that landed, nil for none.
func goingLastLanded(landed []*goingLandedRequest) *goingLandedRequest {
	for index := len(landed) - 1; index >= 0; index-- {
		if landed[index] != nil {
			return landed[index]
		}
	}
	return nil
}

// goingOwnLanded reports whether landed, a request a call of tool landed, is the call's own: one a
// row of the tool sends (goingRecordedCall), and not one the call sends before its own, such as a
// close's reason.
func goingOwnLanded(catalogue GoingCatalogue, tool string, landed goingLandedRequest) bool {
	_, own := goingRecordedCall(catalogue, tool, goingWriteRequest{Method: landed.method, Path: landed.path})
	return own
}

// goingCallAnswerOf is a call's answer from what its route answered in-process.
func goingCallAnswerOf(recorded *recordedAnswer) agentwire.CallAnswer {
	status := recorded.status
	if status == 0 {
		status = http.StatusOK
	}
	return agentwire.CallAnswer{Status: status, Body: recorded.body.Bytes(), Cut: recorded.cut,
		ContentType: recorded.header.Get("Content-Type")}
}

// routeSends is the requests a route call sends, and whether the tool writes. A write tool is one
// of the catalogue (GoingCaller), whose row of the run's plan rowGate held the call to; it is
// prepared as the plan prepared it, a send to an agent session on the issue its row names
// (sendIssue), its translation's reads made through doer, and told what of the requests it sends
// first landed before (resumed, which answers the call key's landings for those requests). An
// upload whose row's approval showed the document or file of its name, and whose read before
// finds it as shown (rowUpload), trades the create alone its translation carries for the version
// that read found (goingCondition.ExpectVersion), and names the token of the version it replaces
// where the read answered one, so it lands as that artifact's next version or not at all. The
// create of a new-issue run carries the route its row's approval names (newIssueRoute), which no
// argument of the tool holds. A read tool sends one request (goingReadSends), an item the call
// names none of the run's page's where its tool reads the page's (goingPageArguments). Any other
// name is refused UNKNOWN_TOOL.
func (a *AgentListener) routeSends(ctx context.Context, request agentwire.CallRequest, arguments map[string]any, doer goingInProcess,
	resumed func(first []goingSend) []*goingLandedRequest, row goingPlanRowStored,
) ([]goingSend, bool, *agentwire.Error) {
	if _, writes := a.s.going.Lookup(request.Tool); writes {
		caller := GoingCaller{Catalogue: a.s.going, Doer: doer, knownSession: a.s.goingKnownSession, sendIssue: row.sendIssue(),
			resumeAfter: func(specs []goingSend) []goingWriteRequest { return goingLandedBefore(resumed(specs), specs) }}
		prepared, err := caller.prepare(ctx, request.Tool, arguments)
		if err != nil {
			return nil, true, goingCallRefusal(request.Tool, err)
		}
		if route := row.newIssueRoute(); route != "" && prepared.call.body != nil {
			prepared.call.body["route"] = route
		}
		if _, uploads := goingUploadNames[request.Tool]; uploads && request.Row != nil {
			upload, refusal := a.rowUpload(ctx, request, row, prepared, caller.reader())
			if refusal != nil {
				return nil, true, refusal
			}
			if upload.Over {
				prepared.call.condition = &goingCondition{ExpectVersion: &goingArtifactVersion{Artifact: upload.Artifact, Version: upload.Version}}
				if upload.Base != "" && prepared.call.body != nil {
					prepared.call.body["precondition"] = map[string]any{"document": upload.Base}
				}
			}
		}
		specs, err := prepared.call.requests(prepared.route)
		if err != nil {
			return nil, true, goingAgentFailed(agentwire.CallCall, fmt.Errorf("encode %s: %w", request.Tool, err))
		}
		return specs, true, nil
	}
	if _, reads := goingReadTools[request.Tool]; reads {
		named, err := a.s.goingPageArguments(ctx, request.Run, request.Tool, arguments)
		if err != nil {
			return nil, false, goingAgentFailed(agentwire.CallCall, err)
		}
		specs, err := goingReadSends(ctx, request.Tool, named, goingReader{doer: doer}, a.s.deps.ServerURL)
		if err != nil {
			return nil, false, goingCallRefusal(request.Tool, err)
		}
		return specs, false, nil
	}
	return nil, false, goingAgentRefusal(agentwire.CodeUnknownTool, "%q is no tool of this server", request.Tool)
}

// goingCallRefusal is a route call's refusal before any request: the tool's schema or translation
// refusing it, or a read its translation made refused. Its message is the refusal's own words,
// which a plan's row shows its person (recordRow); the tool, and a refused read's status and code,
// are logged. Any other error is the server's own failure.
func goingCallRefusal(tool string, err error) *agentwire.Error {
	var arguments GoingArgumentError
	var refused goingRefusal
	switch {
	case errors.As(err, &arguments):
		return goingAgentRefusal(agentwire.CodeCallRefused, "%s", arguments.Reason)
	case errors.As(err, &refused):
		slog.Info("dispatch: a command-bar call was refused before it was sent", "tool", tool, "status", refused.Status,
			"code", refused.Code, "message", refused.Message)
		return goingAgentRefusal(agentwire.CodeCallRefused, "%s", cmp.Or(refused.Message, strings.ToLower(http.StatusText(refused.Status))))
	}
	return goingAgentFailed(agentwire.CallCall, fmt.Errorf("%s: %w", tool, err))
}

// goingLandedBefore is what of specs, the requests a call sends first, its key's earlier tries
// landed, in order, as a translation reads what of the write already stands
// (goingCallInput.resumes).
func goingLandedBefore(landed []*goingLandedRequest, specs []goingSend) []goingWriteRequest {
	var before []goingWriteRequest
	for index, spec := range specs {
		if index >= len(landed) || landed[index] == nil || !landed[index].sends(spec) {
			break
		}
		before = append(before, goingWriteRequest{Method: spec.method, Path: spec.path, BodySHA256: spec.digest(),
			Outcome: goingWriteOutcome{OK: true, Status: landed[index].status}})
	}
	return before
}

// goingIssueOfPath is the issue a request's path addresses, "" for a path under no issue.
func goingIssueOfPath(path string) string {
	rest, under := strings.CutPrefix(path, "/api/v1/issues/")
	if !under {
		return ""
	}
	key, _, _ := strings.Cut(rest, "/")
	if unescaped, err := url.PathUnescape(key); err == nil {
		key = unescaped
	}
	return key
}

// goingReadSends is the one request a read tool sends for arguments (goingReadCall). serverURL is
// the server's own address, under which a dashboard URL names an item.
func goingReadSends(ctx context.Context, tool string, arguments map[string]any, read goingReader, serverURL string) ([]goingSend, error) {
	routes, err := goingReadToolRoutes(tool)
	if err != nil {
		return nil, err
	}
	route, call, err := goingReadCall(ctx, tool, routes, arguments, read, serverURL)
	if err != nil {
		var untranslatable goingUntranslatable
		if errors.As(err, &untranslatable) {
			return nil, GoingArgumentError{Action: tool, Reason: string(untranslatable)}
		}
		return nil, err
	}
	return call.requests(route)
}

// goingReadCall is the row of routes, the rows of the read tool named tool, that a call of it
// reads, and what that row's route takes of the call: what the tool's Choose made of it after its
// read, where it has one; else what its Pick made of it (goingReadRow); else the call translated
// by the row's own translation where it has one, and otherwise with each argument the path does
// not take sent in the query of a GET, a list as one parameter per item, or in the JSON body of
// any other method (goingTranslateRead).
func goingReadCall(ctx context.Context, tool string, routes []GoingRoute, arguments map[string]any, read goingReader, serverURL string) (GoingRoute, goingCall, error) {
	if choose := goingReadTools[tool].Choose; choose != nil {
		row, call, err := choose(ctx, read, arguments, serverURL)
		if err != nil {
			return GoingRoute{}, goingCall{}, err
		}
		route, err := goingPickedRoute(tool, routes, row)
		return route, call, err
	}
	route, call, picked, err := goingReadRow(tool, routes, arguments, serverURL)
	if err != nil || picked {
		return route, call, err
	}
	translate := route.translate
	if translate == nil {
		translate = goingTranslateRead(route)
	}
	call, err = translate(ctx, read, goingCallInput{arguments: arguments, path: call.path})
	return route, call, err
}

// goingReadRow is the row of routes, the rows of the read tool named tool, that a call of it
// reads, chosen without a read: the row the tool's Pick picks, where it has one, with what that
// row's route takes of the call (picked); and otherwise its first row whose guard the arguments
// satisfy and every one of whose placeholders they fill (chooseGoingRoute), with the value of each
// placeholder as the call's path, for the row's translation to take.
func goingReadRow(tool string, routes []GoingRoute, arguments map[string]any, serverURL string) (GoingRoute, goingCall, bool, error) {
	if pick := goingReadTools[tool].Pick; pick != nil {
		row, call, err := pick(arguments, serverURL)
		if err != nil {
			return GoingRoute{}, goingCall{}, true, err
		}
		route, err := goingPickedRoute(tool, routes, row)
		return route, call, true, err
	}
	route, values, chosen := chooseGoingRoute(GoingAction{Action: tool, Routes: routes}, arguments, "")
	if !chosen {
		return GoingRoute{}, goingCall{}, false, GoingArgumentError{Action: tool, Reason: goingNoRouteReason(GoingAction{Routes: routes})}
	}
	return route, goingCall{path: values}, false, nil
}

// goingPickedRoute is the row of routes, the rows of the read tool named tool, that row names as
// "<METHOD> <pattern>", and an error where it names none of them.
func goingPickedRoute(tool string, routes []GoingRoute, row string) (GoingRoute, error) {
	at := slices.IndexFunc(routes, func(route GoingRoute) bool { return route.Method+" "+route.Pattern == row })
	if at < 0 {
		return GoingRoute{}, fmt.Errorf("%s picked %s, which is no row of its own", tool, row)
	}
	return routes[at], nil
}

// GoingReadRoute is the row of the table, as "<METHOD> <pattern>", that a call of the read tool
// named tool with arguments reads: the row the server picks for it (goingReadRow), with no read
// made and nothing sent. A tool whose row hangs on what its item is names the row its Pick picks:
// dispatch_doc_read names the row of a document, where the server sends a read of an uploaded
// file to one of its versions once it has read the file's record (goingChooseDocRead). serverURL
// is the server's own address, under which a dashboard URL names an item. The arguments are read
// as they are written, with no run's page to name an item they leave out (goingPageArguments). It
// is false for a tool that is no read tool of the server, and for a call the server refuses before
// it picks a row.
func GoingReadRoute(tool string, arguments map[string]any, serverURL string) (string, bool) {
	if _, reads := goingReadTools[tool]; !reads {
		return "", false
	}
	routes, err := goingReadToolRoutes(tool)
	if err != nil {
		return "", false
	}
	route, _, _, err := goingReadRow(tool, routes, arguments, serverURL)
	if err != nil {
		return "", false
	}
	return route.Method + " " + route.Pattern, true
}

// goingReadToolRoutes is the rows of read tool name: the rows its entry names, or the routes its
// spec binds, each with its translation. A row that binds no argument of a call (its Path is nil)
// takes no call, and is left out.
func goingReadToolRoutes(name string) ([]GoingRoute, error) {
	tool := goingReadTools[name]
	named := tool.Routes
	if len(named) == 0 {
		for _, spec := range contracts.DispatchTools {
			if spec.Name == name {
				named, _ = goingToolRows(spec, nil, nil)
				break
			}
		}
	}
	routes := make([]GoingRoute, 0, len(named))
	for _, row := range named {
		if row.Path == nil {
			continue
		}
		routes = append(routes, GoingRoute{Method: row.Method, Pattern: row.Pattern, path: row.Path,
			whenArgument: row.WhenArgument, whenValue: row.WhenValue, translate: row.Translate})
	}
	if len(routes) == 0 {
		return nil, GoingArgumentError{Action: name, Reason: "no row of this read binds a call's arguments, so it takes no call"}
	}
	return routes, nil
}

// goingTranslateRead is the translation of a read row that names none of its own: the path as
// chooseGoingRoute filled it, and every other argument in the query of a GET, a list as one
// parameter per item, or in the JSON body of any other method.
func goingTranslateRead(route GoingRoute) goingTranslator {
	return func(_ context.Context, _ goingReader, call goingCallInput) (goingCall, error) {
		translated := goingCall{path: call.path}
		taken := map[string]bool{}
		for _, argument := range route.path {
			taken[argument] = true
		}
		for name, value := range call.arguments {
			if taken[name] || value == nil {
				continue
			}
			if route.Method != http.MethodGet {
				if translated.body == nil {
					translated.body = map[string]any{}
				}
				translated.body[name] = value
				continue
			}
			items, many := value.([]any)
			if !many {
				items = []any{value}
			}
			for _, item := range items {
				text, isText := goingArgumentText(item)
				if !isText {
					return goingCall{}, goingUntranslatable(fmt.Sprintf("%s is sent in the query, which takes no %T", name, item))
				}
				if translated.query == nil {
					translated.query = url.Values{}
				}
				translated.query.Add(name, text)
			}
		}
		if route.Method != http.MethodGet && translated.body == nil {
			translated.body = map[string]any{}
		}
		return translated, nil
	}
}

// goingRunLanding is the landing of one request a run's call sends: its call key, its number
// within the call, what it sends, the lease the call is made under, and the issue its path
// addresses, "" for none.
type goingRunLanding struct {
	key                  goingCallKey
	token                string
	request              int
	method, path, digest string
	issue                string
}

// errGoingStaleLease is the commit step's refusal of a write whose run is no longer held under its
// call's lease: the route's transaction rolls back, so the stale owner's change is not made.
var errGoingStaleLease = errors.New("the run is no longer held under this call's lease, so its change was not made")

// goingTransactionWrote reports whether tx wrote: it holds the ROW EXCLUSIVE lock an INSERT,
// UPDATE, DELETE or MERGE takes on a table but request_keys, going_runs and going_landings, whose
// rows record a request and its landing and are no change of a route's own.
func goingTransactionWrote(ctx context.Context, tx pgx.Tx) (bool, error) {
	var wrote bool
	if err := tx.QueryRow(ctx, `
		select txid_current_if_assigned() is not null and exists (
			select 1 from pg_locks l join pg_class c on c.oid = l.relation
			where l.pid = pg_backend_pid() and l.locktype = 'relation' and l.mode = 'RowExclusiveLock'
			  and c.relkind in ('r', 'p') and c.oid not in ('request_keys'::regclass, 'going_runs'::regclass, 'going_landings'::regclass)
		)
	`).Scan(&wrote); err != nil {
		return false, fmt.Errorf("record a run's request landed: %w", err)
	}
	return wrote, nil
}

// step is the commit step a route's transaction takes while the request runs (running). A
// transaction that wrote (goingTransactionWrote) checks, under the run's row locked for share,
// that the call's lease token is the run's and the run works, and refuses otherwise, rolling the
// change back; it adds the issue the request addresses to the run's issues, or the issue a create
// made, as its route recorded it; and it writes the request's going_landings row with what its
// route recorded it answers (goingRecordAnswer) and the delivery recovery point it committed
// (goingRecordRecovery). A transaction that only read records nothing. A route that commits
// several transactions writes the row with the first and checks the lease with each.
func (l goingRunLanding) step(running *atomic.Bool, sink *goingAnswerSink) store.BeforeCommit {
	var recorded atomic.Bool
	return func(ctx context.Context, tx pgx.Tx) error {
		if !running.Load() {
			return nil
		}
		wrote, err := goingTransactionWrote(ctx, tx)
		if err != nil || !wrote {
			return err
		}
		status, body, answered := sink.recorded()
		issue := l.issue
		if issue == "" && answered && l.method == http.MethodPost && l.path == "/api/v1/issues" {
			var made struct {
				Key string `json:"key"`
			}
			if json.Unmarshal(body, &made) == nil {
				issue = made.Key
			}
		}
		if issue != "" {
			// Locked for the update first where it adds the issue, so this transaction never holds
			// the row for share while it waits to update it.
			if _, err := tx.Exec(ctx, `
				update going_runs set issues = array_append(issues, $2)
				where id = $1::uuid and lease_token = $3::uuid and state = 'working' and not ($2 = any(issues))
			`, l.key.run, issue, l.token); err != nil {
				return fmt.Errorf("add the issue a run's request landed on: %w", err)
			}
		}
		held, err := goingLeaseHeld(ctx, tx, l.key.run, l.token)
		if err != nil {
			return fmt.Errorf("check a run's lease: %w", err)
		}
		if !held {
			return errGoingStaleLease
		}
		if recorded.Load() {
			return nil
		}
		answer, err := json.Marshal(struct {
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		}{status, goingJSONOrNullRaw(body)})
		if err != nil {
			return err
		}
		var recovery any
		if point := sink.recovery.Load(); point != nil {
			recovery = string(*point)
		}
		if _, err := tx.Exec(ctx, `
			insert into going_landings (run_id, phase, step, call, request, method, path, body_digest, answer, recovery)
			values ($1::uuid, 'do', $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb)
		`, l.key.run, l.key.step, l.key.call, l.request, l.method, l.path, l.digest, string(answer), recovery); err != nil {
			return fmt.Errorf("record a run's request landed: %w", err)
		}
		recorded.Store(true)
		return nil
	}
}
