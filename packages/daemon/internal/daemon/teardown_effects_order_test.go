package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Lane D's replay order (plan T4, lane D step 1): the run starts from the case's starting state, and
// before each recorded legion call only the Git, GitHub, Dispatch and workspace changes the run
// recorded before that call are applied, in their recorded order. Every place comes from a record's
// own time: a push from its line of pushes.log (the remote's post-receive hook, after the ref
// moved), a GitHub change from its row of calls.jsonl (gh-standin's `at` and `done`), a Dispatch
// write from its event in dispatch-readback.json (`created_at`), a move of the role's workspace
// from the jj operation of workspace-ops.jsonl that made it (its start and end), a legion call
// from its transcript (its assistant message and its tool result), and the stand-in's answer from
// its line of daemon.log. Where the records cannot place a change a call needs, or cannot pair a
// request with its call, the replay is a harness failure, never a behavioral result. Nothing is
// placed from a refusal's text, a handoff's body or a final ref.

// teardownEffectsHTTP is one request on a pane's hop to a daemon and its answer: a line of the
// daemon stand-in's daemon.log in lane R, or a request the replay's proxy saw reach the real daemon.
type teardownEffectsHTTP struct {
	At           time.Time `json:"at"`
	Method, Path string
	Status       int
	Body, Answer json.RawMessage
}

func (x teardownEffectsHTTP) status() teardownEffectsStatus {
	var a struct{ Code *string }
	_ = json.Unmarshal(x.Answer, &a)
	return teardownEffectsStatus{Status: x.Status, Code: a.Code}
}

func (x teardownEffectsHTTP) route() string { return x.Method + " " + x.Path }

// teardownEffectsToolRoute reports whether path is a route the legion tool calls: boot,
// registration, grants and the pane's lifecycle reports are no tool answers.
func teardownEffectsToolRoute(path string) bool {
	if !strings.HasPrefix(path, "/legion/v1/") || path == "/legion/v1/grants" {
		return false
	}
	switch path {
	case "/legion/v1/claims/register", "/legion/v1/claims/ready", "/legion/v1/claims/exit", "/legion/v1/claims/turn-outcome", "/legion/v1/claims/activity":
		return false
	}
	return true
}

// teardownEffectsReadOp reports whether op is a legion read whose GET is the call's own; any other
// GET inside a call is the Legion extension's own check of the record (outcome.ts daemonReads).
func teardownEffectsReadOp(op string) bool { return op == "read_record" || op == "read_state" }

// teardownEffectsScoredWrites are the daemon routes a legion tool write reaches, the ones
// outcome.ts scores (its daemonOps). A write on one of them that no call sent is unpaired; another
// POST outside every call is the pane's own traffic.
var teardownEffectsScoredWrites = map[string]bool{
	"/legion/v1/handoff/complete": true, "/legion/v1/issues/status": true, "/legion/v1/claims/ready-for-update": true,
	"/legion/v1/claims/fresh-session": true, "/legion/v1/claims/compact": true, "/legion/v1/project/pause": true,
	"/legion/v1/project/resume": true, "/legion/v1/intake/replay": true, "/legion/v1/intake/discard": true,
	"/legion/v1/gates/register": true, "/legion/v1/waves/release": true, "/legion/v1/phase/backward": true,
	"/legion/v1/phase/retry": true, "/legion/v1/signoff": true, "/legion/v1/roots/close": true,
	"/legion/v1/children/rerun": true, "/legion/v1/plans/register": true, "/legion/v1/tasks/stop": true,
}

// teardownEffectsSameAnswer reports whether the real daemon answered a request as the stand-in
// did. A run diverges on an acceptance against a refusal, or one refusal against another (plan
// T4, lane D step 3): the route must match, any two acceptances (a status below 400) are the same
// answer, and two refusals (400 or more, a 5xx among them) are the same only with the same status
// and code. An accepted answer's body is not compared, since the stand-in's simpler records say
// less than the daemon's.
func teardownEffectsSameAnswer(a, b teardownEffectsHTTP) bool {
	if a.Method != b.Method || a.Path != b.Path {
		return false
	}
	if a.Status < 400 || b.Status < 400 {
		return a.Status < 400 && b.Status < 400
	}
	return a.Status == b.Status && teardownEffectsRefusalCode(a.Answer) == teardownEffectsRefusalCode(b.Answer)
}

// teardownEffectsRefusalCode is the `code` of a refusal's JSON body, or the whole body where it is
// no JSON object naming one.
func teardownEffectsRefusalCode(body []byte) string {
	var refusal struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &refusal) != nil || refusal.Code == "" {
		return string(body)
	}
	return refusal.Code
}

// teardownEffectsActor is a Dispatch actor as an event or a record names it.
type teardownEffectsActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// teardownEffectsEvent is one event of dispatch-readback.json (seed.ts case-readback): Dispatch's
// own envelope, with the actor that wrote it and the time Dispatch committed it.
type teardownEffectsEvent struct {
	Seq        int64                `json:"seq"`
	Type       string               `json:"type"`
	CreatedAt  time.Time            `json:"created_at"`
	ArtifactID string               `json:"artifact_id"`
	Actor      teardownEffectsActor `json:"actor"`
	Payload    map[string]any       `json:"payload"`
}

// teardownEffectsReadback is the part of dispatch-readback.json the replay reads: the event log
// with its seed cursor, and the records whose bodies the events do not carry.
type teardownEffectsReadback struct {
	SeedThrough int64                  `json:"seedThrough"`
	Events      []teardownEffectsEvent `json:"events"`
	Documents   []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"documents"`
	Handoffs []struct {
		ID      string               `json:"id"`
		Phase   string               `json:"phase"`
		Version int                  `json:"version"`
		Body    json.RawMessage      `json:"body"`
		Author  teardownEffectsActor `json:"author"`
	} `json:"handoffs"`
	Asks []struct {
		ID       string          `json:"id"`
		Question string          `json:"question"`
		Options  json.RawMessage `json:"options"`
	} `json:"asks"`
	Results struct {
		Checks []struct {
			ID      string           `json:"block_id"`
			Results []map[string]any `json:"results"`
		} `json:"checks"`
	} `json:"results"`
}

// handoffBody is the stored body of the handoff record id: Dispatch keeps it as a string, and an
// older readback held it as the object.
func (b *teardownEffectsReadback) handoffBody(id string) (string, int, string, bool) {
	for _, h := range b.Handoffs {
		if h.ID != id {
			continue
		}
		var text string
		if json.Unmarshal(h.Body, &text) != nil {
			text = string(h.Body)
		}
		return text, h.Version, h.Phase, true
	}
	return "", 0, "", false
}

// result is the recorded result id, with the block id of its check.
func (b *teardownEffectsReadback) result(id string) (map[string]any, string, bool) {
	for _, check := range b.Results.Checks {
		for _, result := range check.Results {
			if result["id"] == id {
				return result, check.ID, true
			}
		}
	}
	return nil, "", false
}

// teardownEffectsPush is one line of pushes.log: the ref and the heads it moved between.
type teardownEffectsPush struct {
	Ref, Old, New string
}

// teardownEffectsGitHubRow is one row of calls.jsonl (gh-standin.ts): when the call began (`at`)
// and ended (`done`), its arguments and its exit status, and the change it made to a pull request or
// a review (`pull`), which gh-standin writes for every call it took that made one and for no other.
// What a call did is read from `pull` alone, and never from its arguments: a row with no `pull` is a
// read, or a write the stand-in refused, and changes nothing.
type teardownEffectsGitHubRow struct {
	At   time.Time                    `json:"at"`
	Done *time.Time                   `json:"done"`
	As   string                       `json:"as"`
	Argv []string                     `json:"argv"`
	Exit int                          `json:"exit"`
	Pull *teardownEffectsPullMutation `json:"pull"`
}

// teardownEffectsPullMutation is one change to a pull request or a review, as gh-standin.ts keeps it
// (`pull`) and names it (`action`), each naming the pull request it changed (`number`): `open` (the
// number, branch, base, head commit, title, body and draft it gave the new one); `edit` (the pull
// requests in `numbers`, the body each has after the edit, and the title and base the edit set, nil
// where it set none); `review` (the review it added: its state, commit, body and time);
// `review-comment` (a comment on the diff, or a reply under one) and `comment` (a comment on the pull
// request), each with its `body`; `merge` (its `method`, and `auto` for one that waits for the
// requirements) and `disable-auto`; `close` and `reopen` (with the `comment` the command added);
// `ready` and `draft`; `enqueue` and `dequeue` (the merge queue's GraphQL mutations, by the pull
// request's node `id`); and, by their own names, any other `gh pr` command, any other GraphQL
// `mutation` (its `name`) and any other REST write under a pull request (`request`, its `method`
// and `path`).
type teardownEffectsPullMutation struct {
	Action      string  `json:"action"`
	Number      int     `json:"number"`
	Numbers     []int   `json:"numbers"`
	Head        string  `json:"head"`
	Base        *string `json:"base"`
	SHA         string  `json:"sha"`
	Title       *string `json:"title"`
	Body        string  `json:"body"`
	Draft       bool    `json:"draft"`
	State       string  `json:"state"`
	Commit      string  `json:"commit"`
	SubmittedAt string  `json:"submittedAt"`
	Method      string  `json:"method"`
	Auto        bool    `json:"auto"`
	Comment     string  `json:"comment"`
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Path        string  `json:"path"`

	// A trace's alone (teardownEffectsTraceChange), for teardownEffectsScratch.resolve: a gh pr
	// command that names no pull request, and an edit that names no body.
	onBranch, keepBody bool
}

// teardownEffectsChange is one recorded change to Git, GitHub, Dispatch or the role's workspace:
// where it was recorded, when on the case's clock, and the outcome.json action that records it (0
// for a seeded record or a move of the workspace, which no outcome holds; the workspace record's
// line and jj operation are its provenance). Refused is the refusal of a write the replay made as
// a refused write, which changed nothing (a trace's request-approval,
// teardownEffectsApprovalRefusal); nil for every change the replay made.
type teardownEffectsChange struct {
	Kind       string                  `json:"kind"`
	Source     string                  `json:"source"`
	What       string                  `json:"what"`
	At         time.Time               `json:"at"`
	OutcomeSeq int                     `json:"outcomeSeq,omitempty"`
	Refused    *teardownEffectsRefusal `json:"refused,omitempty"`
	from, to   time.Time
	fault      string
	push       *teardownEffectsPush
	pull       *teardownEffectsPullMutation
	event      *teardownEffectsEvent
	// unsupported says fault is the kind of write itself, which the replay never makes
	// (teardownEffectsDispatchFault), and not a record the readback lacks or a foreign author.
	unsupported bool
	// workspace is the commit the role's workspace stood on (`@-`) once the move was made.
	workspace string
}

// teardownEffectsOperation is one line of workspace-ops.jsonl (git-world.ts recordWorkspace): one
// jj operation of the run's workspace, when it began and ended on the host's clock, and the
// commits `@-` named in its view.
type teardownEffectsOperation struct {
	ID          string    `json:"id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Description string    `json:"description"`
	StandingOn  []string  `json:"standingOn"`
}

// teardownEffectsNoCommit is Git's and jj's id of no commit: a push's old head on a new branch, a
// deleted branch's new head, and jj's root commit.
const teardownEffectsNoCommit = "0000000000000000000000000000000000000000"

// teardownEffectsStandin is the stand-in's answer to one of a call's requests, with the
// outcome.json action that records it: the original call as lane R scored it.
type teardownEffectsStandin struct {
	teardownEffectsHTTP
	OutcomeSeq int
}

// teardownEffectsStep is one recorded legion call, the changes applied before it, and the
// stand-in's answers to the requests it sent.
type teardownEffectsStep struct {
	Call    teardownEffectsCall
	Before  []teardownEffectsChange
	Standin []teardownEffectsStandin
}

// teardownEffectsPlan is a run's replay order: the seeded Dispatch records the replay starts from,
// each call with what precedes it, and the changes after the last call, which no call saw.
type teardownEffectsPlan struct {
	Readback teardownEffectsReadback
	Seed     []teardownEffectsChange
	Steps    []teardownEffectsStep
	After    []teardownEffectsChange
}

// teardownEffectsOutcomeAction is the part of an outcome.json action the order reads: its place,
// kind and case time, a daemon action's op, and a GitHub action's `change`, the pull request change
// gh-standin kept of the call (absent from a record written before outcome.ts kept it), by its name.
type teardownEffectsOutcomeAction struct {
	Seq    int       `json:"seq"`
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
	Op     string    `json:"op"`
	Change *struct {
		Action string `json:"action"`
	} `json:"change"`
}

// teardownEffectsOrder places a run's recorded changes and stand-in answers against its legion
// calls (calls, from teardownEffectsReadCalls). clock is the case's start (clock.txt) and launched
// the wall-clock instant of the launch (clock-real.txt), as outcome.ts moves records onto the case's
// timeline. An error is a harness failure: it names the record that cannot be placed or joined. A
// Dispatch write the replay does not make, of the session whose legion call it precedes, is a
// *teardownEffectsRoleWriteError, which names the write (teardownEffectsFaultOf).
func teardownEffectsOrder(dir string, calls []teardownEffectsCall, clock, launched time.Time) (teardownEffectsPlan, error) {
	var plan teardownEffectsPlan
	caseAt := func(t time.Time) time.Time { return clock.Add(t.Sub(launched)).UTC() }
	if err := teardownEffectsDecode(filepath.Join(dir, "dispatch-readback.json"), &plan.Readback); err != nil {
		return plan, err
	}
	// outcome.json's actions, and the Dispatch writes of actors other than the role, which it keeps
	// apart (`otherWrites`, absent from a record written before it did).
	var outcome struct {
		Actions     []teardownEffectsOutcomeAction `json:"actions"`
		OtherWrites []struct {
			At time.Time `json:"at"`
		} `json:"otherWrites"`
	}
	if err := teardownEffectsDecode(filepath.Join(dir, "outcome.json"), &outcome); err != nil {
		return plan, err
	}
	standin, err := teardownEffectsJSONLines[teardownEffectsHTTP](filepath.Join(dir, "daemon.log"))
	if err != nil {
		return plan, err
	}
	rows, err := teardownEffectsJSONLines[teardownEffectsGitHubRow](filepath.Join(dir, "calls.jsonl"))
	if err != nil {
		return plan, err
	}
	pushes, err := teardownEffectsReadPushes(filepath.Join(dir, "pushes.log"))
	if err != nil {
		return plan, err
	}
	for i, call := range calls {
		if call.ResultAt.IsZero() {
			return plan, fmt.Errorf("legion call %d (%s) has no recorded result, so what the role was answered is unknown", i+1, call.ID)
		}
		plan.Steps = append(plan.Steps, teardownEffectsStep{Call: call})
	}

	// Each outcome.json action is taken once, by kind and case time, in its own order; a Dispatch
	// write no action records is taken from the other actors' writes, with no action of its own.
	type key struct {
		kind string
		at   int64
	}
	pending := map[key][]teardownEffectsOutcomeAction{}
	for _, action := range outcome.Actions {
		k := key{action.Kind, action.At.UnixNano()}
		pending[k] = append(pending[k], action)
	}
	others := map[int64]int{}
	for _, write := range outcome.OtherWrites {
		others[write.At.UnixNano()]++
	}
	take := func(kind string, at time.Time) (teardownEffectsOutcomeAction, bool) {
		k := key{kind, caseAt(at).UnixNano()}
		actions := pending[k]
		if len(actions) == 0 {
			if kind == "dispatch" && others[k.at] > 0 {
				others[k.at]--
				return teardownEffectsOutcomeAction{}, true
			}
			return teardownEffectsOutcomeAction{}, false
		}
		pending[k] = actions[1:]
		return actions[0], true
	}

	var changes []teardownEffectsChange
	for _, p := range pushes {
		changes = append(changes, teardownEffectsChange{Kind: "push", Source: fmt.Sprintf("pushes.log:%d", p.line), What: fmt.Sprintf("%s %s..%s", p.Ref, p.Old, p.New), from: p.at, to: p.at, push: &p.teardownEffectsPush})
	}
	for n, row := range rows {
		if row.As != "gh" {
			continue
		}
		action, err := teardownEffectsGitHubAction(row)
		if err != nil {
			return plan, fmt.Errorf("calls.jsonl:%d: %w", n+1, err)
		}
		if action == "" {
			continue
		}
		c := teardownEffectsChange{Kind: "github", Source: fmt.Sprintf("calls.jsonl:%d", n+1), What: action + ": gh " + strings.Join(row.Argv, " "), from: row.At, to: *row.Done, pull: row.Pull}
		if row.Exit != 0 {
			if row.Pull == nil {
				continue
			}
			c.fault = fmt.Sprintf("gh-standin kept a pull request change (%s) from a call that exited %d, which the daemon stand-in did not count", row.Pull.Action, row.Exit)
		}
		changes = append(changes, c)
	}
	seedThrough := plan.Readback.SeedThrough
	for _, e := range plan.Readback.Events {
		if strings.HasSuffix(e.Type, ".delivery") {
			continue
		}
		event := e
		c := teardownEffectsChange{Kind: "dispatch", Source: fmt.Sprintf("dispatch-readback.json event %d", e.Seq), What: e.Type, At: caseAt(e.CreatedAt), from: e.CreatedAt, to: e.CreatedAt, event: &event}
		if e.Seq <= seedThrough {
			// The case's starting state: the boot creates the issue and sets its status, and
			// Dispatch writes its own records again where the seeded writes call for them.
			if e.Actor.Kind == "system" || e.Type == "issue.created" || e.Type == "issue.updated" || e.Type == "issue.closed" {
				continue
			}
			if fault, _ := teardownEffectsDispatchFault(&plan.Readback, e); fault != "" {
				return plan, fmt.Errorf("the case's starting state holds %s (%s), which the replay cannot seed: %s", e.Type, c.Source, fault)
			}
			plan.Seed = append(plan.Seed, c)
			continue
		}
		if e.Actor.Kind == "system" {
			// Dispatch's own write: the replayed write that called for it makes it again. outcome.json
			// holds it too, among the other actors' writes (or, in a record written before it kept
			// them apart, among the actions), so it is taken here.
			if _, ok := take("dispatch", e.CreatedAt); !ok {
				return plan, fmt.Errorf("%s (%s) has no action in outcome.json: the records disagree", c.Source, e.Type)
			}
			continue
		}
		c.fault, c.unsupported = teardownEffectsDispatchFault(&plan.Readback, e)
		changes = append(changes, c)
	}

	for i := range changes {
		c := &changes[i]
		c.At = caseAt(c.from)
		action, ok := take(c.Kind, c.from)
		if !ok {
			return plan, fmt.Errorf("%s (%s at %s) has no action in outcome.json: the records disagree, so its place is not established", c.Source, c.What, c.At.Format(time.RFC3339Nano))
		}
		if c.pull != nil && action.Change != nil && action.Change.Action != c.pull.Action {
			return plan, fmt.Errorf("%s (%s) is outcome.json action %d, which records the change %s: the records disagree on what the call did", c.Source, c.What, action.Seq, action.Change.Action)
		}
		c.OutcomeSeq = action.Seq
	}
	moves, err := teardownEffectsWorkspaceMoves(dir, launched)
	if err != nil {
		return plan, err
	}
	for _, c := range moves {
		c.At = caseAt(c.from)
		changes = append(changes, c)
	}

	// Each change goes before the first call that started after it ended. One that touches a call's
	// window may or may not have been seen by that call.
	var applied []*teardownEffectsChange
	for i := range changes {
		c := &changes[i]
		place := -1
		for n, call := range calls {
			if !c.from.After(call.ResultAt) && !c.to.Before(call.At) {
				return plan, fmt.Errorf("%s (%s) falls inside legion call %d (%s), so whether that call saw it is unknown", c.Source, c.What, n+1, call.ID)
			}
			if place < 0 && c.to.Before(call.At) {
				place = n
			}
		}
		if place < 0 {
			plan.After = append(plan.After, *c)
			continue
		}
		if c.fault != "" {
			err := fmt.Errorf("%s (%s) precedes legion call %d (%s): %s", c.Source, c.What, place+1, calls[place].ID, c.fault)
			own := teardownEffectsActor{Kind: "session", ID: calls[place].Session}
			if c.unsupported && c.event != nil && c.event.Actor == own && c.OutcomeSeq > 0 {
				at := c.At
				return plan, &teardownEffectsRoleWriteError{err: err, fault: teardownEffectsFault{Kind: "role-write", Source: c.Source, Event: c.What, At: &at, OutcomeSeq: c.OutcomeSeq, Author: &own, Session: own.ID, Call: calls[place].ID}}
			}
			return plan, err
		}
		plan.Steps[place].Before = append(plan.Steps[place].Before, *c)
		if c.Kind == "push" || c.Kind == "github" {
			applied = append(applied, c)
		}
	}
	// A push and a GitHub change that overlap have no order: a pull request opened while its branch
	// moved has either head.
	for i, a := range applied {
		for _, b := range applied[i+1:] {
			if !a.to.Before(b.from) && !b.to.Before(a.from) {
				return plan, fmt.Errorf("%s (%s) and %s (%s) overlap, so their order is unknown", a.Source, a.What, b.Source, b.What)
			}
		}
	}
	for i := range plan.Steps {
		slices.SortStableFunc(plan.Steps[i].Before, func(a, b teardownEffectsChange) int { return a.from.Compare(b.from) })
	}

	// Each request the stand-in answered inside a call is that call's; a scored write it answered
	// outside every call, or inside more than one, is unpaired. Another POST outside every call is
	// the pane's own traffic, and a GET outside a read op's call the extension's own check.
	for n, line := range standin {
		if !teardownEffectsToolRoute(line.Path) {
			continue
		}
		var in []int
		for i, call := range calls {
			if !line.At.Before(call.At) && !line.At.After(call.ResultAt) {
				in = append(in, i)
			}
		}
		if line.Method != "GET" {
			scored := teardownEffectsScoredWrites[line.Path]
			if len(in) == 0 && !scored {
				continue
			}
			if len(in) != 1 {
				return plan, fmt.Errorf("daemon.log:%d (%s at %s) falls inside %d legion calls: an unpaired request", n+1, line.route(), line.At.Format(time.RFC3339Nano), len(in))
			}
			action, ok := take("daemon", line.At)
			if !ok && scored {
				return plan, fmt.Errorf("daemon.log:%d (%s) has no action in outcome.json: missing provenance", n+1, line.route())
			}
			plan.Steps[in[0]].Standin = append(plan.Steps[in[0]].Standin, teardownEffectsStandin{line, action.Seq})
			continue
		}
		if len(in) != 1 || !teardownEffectsReadOp(calls[in[0]].Op) {
			continue
		}
		step := &plan.Steps[in[0]]
		if slices.ContainsFunc(step.Standin, func(s teardownEffectsStandin) bool { return s.Method == "GET" && s.Path == line.Path }) {
			continue
		}
		action, _ := take("daemon", line.At)
		step.Standin = append(step.Standin, teardownEffectsStandin{line, action.Seq})
	}
	// Every push, every Dispatch write and every daemon write outcome.json scores has a record
	// placed above: an action no record matches is a fact the replay would miss. So has every GitHub
	// action whose `change` says gh-standin kept a change of it: calls.jsonl's `pull` is the one record
	// the replay makes a GitHub change from.
	for k, actions := range pending {
		for _, action := range actions {
			if k.kind == "push" || k.kind == "dispatch" || k.kind == "daemon" && !teardownEffectsReadOp(action.Op) {
				return plan, fmt.Errorf("outcome.json action %d (%s %s at %s) has no record the replay can place: a missing ordering fact", action.Seq, action.Kind, action.Op, action.At.Format(time.RFC3339Nano))
			}
			if k.kind == "github" && action.Change != nil {
				return plan, fmt.Errorf("outcome.json action %d (github %s at %s) records a change gh-standin kept, and calls.jsonl holds no `pull` of that call: a missing mutation record", action.Seq, action.Change.Action, action.At.Format(time.RFC3339Nano))
			}
		}
	}
	return plan, nil
}

// teardownEffectsRoleWriteError is teardownEffectsOrder's error where a write the role made causes
// the HARNESS verdict: a Dispatch write of the role's own session, which outcome.json scores and the
// replay does not make, before a later legion call of that session.
type teardownEffectsRoleWriteError struct {
	err   error
	fault teardownEffectsFault
}

func (e *teardownEffectsRoleWriteError) Error() string { return e.err.Error() }
func (e *teardownEffectsRoleWriteError) Unwrap() error { return e.err }

// teardownEffectsFaultOf is the typed cause replay.json gives the HARNESS verdict err makes: the
// role's write where teardownEffectsOrder named one, and the harness's own records for every other
// error. Nothing is read from err's text.
func teardownEffectsFaultOf(err error) *teardownEffectsFault {
	var write *teardownEffectsRoleWriteError
	if errors.As(err, &write) {
		fault := write.fault
		return &fault
	}
	return &teardownEffectsFault{Kind: "harness-record"}
}

// teardownEffectsWorkspaceMoves are the moves of the role's workspace after the launch: each jj
// operation of workspace-ops.jsonl (git-world.ts recordWorkspace) that left `@-` on other commits
// than the operation before it, timed by the operation's own start and end. The workspace must
// stand at the launch where the case's world stands it (its standingOn, through commit-map.json;
// jj's root commit for a world with no Git), or the record is not this run's start. A move that
// began before the launch and ended after it has no place.
func teardownEffectsWorkspaceMoves(dir string, launched time.Time) ([]teardownEffectsChange, error) {
	path := filepath.Join(dir, "workspace-ops.jsonl")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("the run directory holds no workspace-ops.jsonl, the jj operation record of its workspace, so what the workspace stood on at each call is unknown "+
			"(outcome.ts writes it at the end of a run; for an earlier capture, run git-world.ts record on the workspace of its original job archive): %w", err)
	}
	operations, err := teardownEffectsJSONLines[teardownEffectsOperation](path)
	if err != nil {
		return nil, err
	}
	var world struct {
		Git *struct {
			Workspace struct {
				StandingOn string `json:"standingOn"`
			} `json:"workspace"`
		} `json:"git"`
	}
	if err := teardownEffectsDecode(filepath.Join(dir, "world.json"), &world); err != nil {
		return nil, err
	}
	var commits map[string]string
	if err := teardownEffectsDecode(filepath.Join(dir, "commit-map.json"), &commits); err != nil {
		return nil, err
	}
	start := teardownEffectsNoCommit
	if world.Git != nil && world.Git.Workspace.StandingOn != "" {
		if start = commits[world.Git.Workspace.StandingOn]; start == "" {
			return nil, fmt.Errorf("commit-map.json names no commit for the world's standingOn %s", world.Git.Workspace.StandingOn)
		}
	}
	launch := -1
	for n, operation := range operations {
		if !operation.End.After(launched) {
			launch = n
		}
	}
	if launch < 0 {
		return nil, fmt.Errorf("workspace-ops.jsonl holds no operation that ended before the launch (clock-real.txt), so what the workspace stood on at the start is unknown")
	}
	stood := strings.Join(operations[launch].StandingOn, ",")
	if stood != start {
		return nil, fmt.Errorf("workspace-ops.jsonl:%d says the workspace stood on %s at the launch, and the case's world stands it on %s: the record is not this run's start", launch+1, stood, start)
	}
	var moves []teardownEffectsChange
	for n := launch + 1; n < len(operations); n++ {
		operation := operations[n]
		now := strings.Join(operation.StandingOn, ",")
		if now == stood {
			continue
		}
		id := operation.ID
		if len(id) > 12 {
			id = id[:12]
		}
		c := teardownEffectsChange{Kind: "workspace", Source: fmt.Sprintf("workspace-ops.jsonl:%d (jj operation %s)", n+1, id), What: fmt.Sprintf("@- %s..%s: %s", stood, now, operation.Description),
			from: operation.Start, to: operation.End}
		switch {
		case operation.Start.Before(launched):
			return nil, fmt.Errorf("%s (%s) began before the launch and ended after it, so whether the run started on it is unknown", c.Source, c.What)
		case len(operation.StandingOn) != 1:
			c.fault = fmt.Sprintf("the workspace stood on %d commits, which the replay does not rebuild", len(operation.StandingOn))
		default:
			c.workspace = operation.StandingOn[0]
		}
		moves = append(moves, c)
		stood = now
	}
	return moves, nil
}

// teardownEffectsDispatchFault is why the replay cannot write e again ("" when it can): it writes
// a handoff, an issue comment, a result, a question and a document, each as its author did.
// unsupported says the fault is the kind of write itself, one the replay never makes (a status
// write, a claim, a reply, an answer, a question of a document, a file, a document its author
// changed after making it), and not a record the readback lacks or an author the write cannot have.
func teardownEffectsDispatchFault(b *teardownEffectsReadback, e teardownEffectsEvent) (fault string, unsupported bool) {
	text := func(key string) string { s, _ := e.Payload[key].(string); return s }
	switch e.Type {
	case "handoff.recorded":
		if e.Actor.Kind != "session" {
			return "a handoff written by a " + e.Actor.Kind, false
		}
		if _, _, _, ok := b.handoffBody(text("handoff_id")); !ok {
			return "its record " + text("handoff_id") + " is not in the readback", false
		}
	case "comment.created":
		for _, field := range []string{"reply_to", "ask_id", "anchor", "artifact_id", "block_id"} {
			if v, ok := e.Payload[field]; ok && v != nil && v != "" {
				return "a reply, an answer on a question or an anchored comment (" + field + ")", true
			}
		}
		if _, ok := e.Payload["body"].(string); !ok {
			return "its event carries no body", false
		}
	case "result.recorded":
		if e.Actor.Kind != "session" {
			return "a result recorded by a " + e.Actor.Kind, false
		}
		if _, _, ok := b.result(text("result_id")); !ok {
			return "its result " + text("result_id") + " is not in the readback", false
		}
	case "ask.opened":
		for _, field := range []string{"block_id", "anchor", "artifact_id"} {
			if v, ok := e.Payload[field]; ok && v != nil && v != "" {
				return "a question of a document (" + field + ")", true
			}
		}
		found := false
		for _, ask := range b.Asks {
			found = found || ask.ID == text("id")
		}
		if !found {
			return "its question " + text("id") + " is not in the readback", false
		}
	case "artifact.created":
		kind := ""
		for _, d := range b.Documents {
			if d.ID == e.ArtifactID {
				kind = d.Kind
			}
		}
		switch kind {
		case "":
			return "the readback holds no document " + e.ArtifactID, false
		case "doc":
		default:
			return "the readback holds " + e.ArtifactID + " as a " + kind + ", which the replay does not make", true
		}
		for _, later := range b.Events {
			changed := later.Type == "artifact.version" || later.Type == "artifact.human_edited" || later.Type == "artifact.renamed" || later.Type == "artifact.removed"
			if later.Seq > e.Seq && later.ArtifactID == e.ArtifactID && changed {
				return "the readback holds only the document's latest text, and " + later.Type + " changed it after it was made", later.Actor == e.Actor
			}
		}
	default:
		return "the replay writes no " + e.Type, true
	}
	return "", false
}

// teardownEffectsGitHubAction is the change a gh call made to the GitHub lane R kept, as gh-standin
// recorded it (`pull`: `open`, `edit`, `review`, `merge`, `close` and the rest it names), and ""
// for a call that changed nothing: a read, or a write the stand-in refused. Nothing reads the call's
// arguments again. A row that records no end is from a gh-standin that recorded neither its end nor
// its change, so what it did is unknown.
func teardownEffectsGitHubAction(row teardownEffectsGitHubRow) (string, error) {
	if row.Done == nil {
		return "", fmt.Errorf("gh %s records when the call began and not when it ended: gh-standin records both and the change each call made (`done`, `pull`), so this row is from before it did, and what the call did is unknown", strings.Join(row.Argv, " "))
	}
	if row.Pull == nil {
		return "", nil
	}
	return row.Pull.Action, nil
}

// teardownEffectsReviewer is the author gh-standin.ts gives each review it adds.
const teardownEffectsReviewer = "legion-review[bot]"

// teardownEffectsScratch is the replay's scratch GitHub as a pull request change reads and makes it:
// its pull requests by number, the issue's branch (`legion/<issue>`), the repository's default
// branch, and the head a branch has on the replay's remote ("" where it has none).
type teardownEffectsScratch struct {
	pulls        map[int]teardownEffectsPull
	branch, base string
	remote       func(branch string) string
}

// change makes p, one pull request change in gh-standin.ts's names, on the scratch GitHub, by the
// one rule each name has, whether lane R's gh-standin kept the change (a run's `pull`) or a trace
// records it (teardownEffectsTraceChange):
//   - open: a new open pull request, at the commit its branch has on the remote, which must be the
//     commit p names;
//   - edit: each pull request's body, and the title and base the edit set;
//   - review: the review, by gh-standin's reviewer;
//   - merge: merged, its merge commit the head of its base on the remote; with `auto`, auto-merge
//     armed and the pull request still open, as daemon-standin.ts holds it, since the replay runs no
//     requirement that would merge it; disable-auto disarms it;
//   - close and reopen: closed, and open;
//   - ready and draft: out of draft, and into it;
//   - enqueue and dequeue: into the merge queue and out of it, the pull request still open, since
//     the replay's queue merges nothing;
//   - comment and review-comment: quiet, since the scratch GitHub keeps no comment and the daemon
//     reads none: the pull request stays as it was.
//
// It changes nothing, and names the change, where the scratch GitHub cannot hold it: a change of
// a pull request it does not hold, an open at another commit than its branch's or of a branch that
// has an open pull request into that base, a merged or closed pull request merged or enqueued, a
// merged one closed or reopened, and every change it has no rule for: a GraphQL `mutation`, a REST
// `request`, and any other `gh pr` command.
func (s teardownEffectsScratch) change(p teardownEffectsPullMutation) error {
	switch p.Action {
	case "open":
		return s.open(p)
	case "edit":
		return s.edit(p)
	case "review", "merge", "disable-auto", "close", "reopen", "ready", "draft", "enqueue", "dequeue", "comment", "review-comment":
	case "mutation":
		return fmt.Errorf("the GraphQL mutation %s is a change the replay's GitHub has no rule for, so the replay does not make it", p.Name)
	case "request":
		return fmt.Errorf("the REST write %s %s is a change the replay's GitHub has no rule for, so the replay does not make it", p.Method, p.Path)
	default:
		return fmt.Errorf("gh pr %s is a change the replay's GitHub has no rule for, so the replay does not make it", p.Action)
	}
	pull, err := s.held(p.Action, p.Number)
	if err != nil {
		return err
	}
	switch p.Action {
	case "review":
		at, err := time.Parse(time.RFC3339Nano, p.SubmittedAt)
		if err != nil {
			return fmt.Errorf("review of pull request #%d: %w", p.Number, err)
		}
		pull.Reviews = append(pull.Reviews, teardownEffectsReview{Author: teardownEffectsReviewer, State: p.State, Commit: p.Commit, Body: p.Body, At: at})
	case "merge":
		if pull.State != "open" {
			return fmt.Errorf("merge of pull request #%d, which is %s: GitHub merges an open pull request alone", p.Number, pull.State)
		}
		if p.Auto {
			pull.AutoMerge = true
			break
		}
		commit := s.remote(pull.Base)
		if commit == "" {
			return fmt.Errorf("merge of pull request #%d into %s, which the replay's remote does not hold, so the merge has no merge commit", p.Number, pull.Base)
		}
		pull.State, pull.MergeCommit, pull.AutoMerge, pull.Queued = "merged", commit, false, false
	case "disable-auto":
		pull.AutoMerge = false
	case "close", "reopen":
		if pull.State == "merged" {
			return fmt.Errorf("%s of pull request #%d, which is merged: GitHub neither closes nor reopens a merged pull request", p.Action, p.Number)
		}
		pull.State = "open"
		if p.Action == "close" {
			pull.State = "closed"
		}
	case "ready", "draft":
		pull.Draft = p.Action == "draft"
	case "enqueue":
		if pull.State != "open" {
			return fmt.Errorf("enqueue of pull request #%d, which is %s: the merge queue takes an open pull request alone", p.Number, pull.State)
		}
		pull.Queued = true
	case "dequeue":
		pull.Queued = false
	}
	s.pulls[p.Number] = pull
	return nil
}

// held is the pull request number, which the change action names, where the scratch GitHub holds
// it, and otherwise an error that names the change and the number.
func (s teardownEffectsScratch) held(action string, number int) (teardownEffectsPull, error) {
	if pull, ok := s.pulls[number]; ok {
		return pull, nil
	}
	if number == 0 {
		return teardownEffectsPull{}, fmt.Errorf("%s names no pull request the replay's GitHub holds", action)
	}
	return teardownEffectsPull{}, fmt.Errorf("%s of pull request #%d, which the replay's GitHub does not hold", action, number)
}

// open opens the pull request p names (change).
func (s teardownEffectsScratch) open(p teardownEffectsPullMutation) error {
	base := ""
	if p.Base != nil {
		base = *p.Base
	}
	if p.Number < 1 || p.Head == "" || base == "" {
		return fmt.Errorf("open of pull request #%d names no number, no head or no base", p.Number)
	}
	if _, taken := s.pulls[p.Number]; taken {
		return fmt.Errorf("open of pull request #%d, which the replay's GitHub already holds", p.Number)
	}
	if got := s.remote(p.Head); got != p.SHA {
		return fmt.Errorf("open of pull request #%d on %s at %s, and the replay's remote holds %q there: the replay's state is not the run's", p.Number, p.Head, p.SHA, got)
	}
	for _, pull := range s.pulls {
		if pull.State == "open" && pull.Head.Ref == p.Head && pull.Base == base {
			return fmt.Errorf("open of pull request #%d on %s into %s, which pull request #%d is open on: GitHub opens no second", p.Number, p.Head, base, pull.Number)
		}
	}
	pull := teardownEffectsPull{Number: p.Number, State: "open", Base: base, Body: p.Body, Draft: p.Draft}
	if p.Title != nil {
		pull.Title = *p.Title
	}
	pull.Head.Ref, pull.Head.SHA = p.Head, p.SHA
	s.pulls[p.Number] = pull
	return nil
}

// edit sets the body of each pull request p names, and the title and base p sets, where the
// scratch GitHub holds every one of them (change).
func (s teardownEffectsScratch) edit(p teardownEffectsPullMutation) error {
	if len(p.Numbers) == 0 {
		return fmt.Errorf("edit names no pull request")
	}
	edited := map[int]teardownEffectsPull{}
	for _, n := range p.Numbers {
		pull, err := s.held("edit", n)
		if err != nil {
			return err
		}
		pull.Body = p.Body
		if p.Title != nil {
			pull.Title = *p.Title
		}
		if p.Base != nil {
			pull.Base = *p.Base
		}
		edited[n] = pull
	}
	maps.Copy(s.pulls, edited)
	return nil
}

// resolve fills what a trace's change p leaves to the state it is made in, at its case time at, as
// gh-standin.ts fills it from its own GitHub: the pull request of a gh pr command that names none
// (the one open pull request of the issue's branch, which gh takes: none, or more than one, is
// refused, as gh refuses it); an open's head (the issue's branch), base (the default branch), number
// (one above every number held) and head commit (its branch's head on the remote: a branch the
// remote does not hold is refused, as GitHub refuses it); an edit's body where it names none (the
// body held); and a review's commit (the head held) and time.
func (s teardownEffectsScratch) resolve(p teardownEffectsPullMutation, at time.Time) (teardownEffectsPullMutation, error) {
	if p.onBranch {
		var open []int
		for n, pull := range s.pulls {
			if pull.Head.Ref == s.branch && pull.State == "open" {
				open = append(open, n)
			}
		}
		if len(open) != 1 {
			return p, fmt.Errorf("%s names no pull request, and %s has %d open pull requests, where gh takes its one: no pull requests found for branch %q", p.Action, s.branch, len(open), s.branch)
		}
		p.Number = open[0]
		if p.Action == "edit" {
			p.Numbers = []int{p.Number}
		}
	}
	switch p.Action {
	case "open":
		if p.Head == "" {
			p.Head = s.branch
		}
		if p.Base == nil {
			base := s.base
			p.Base = &base
		}
		if p.Number == 0 {
			for n := range s.pulls {
				p.Number = max(p.Number, n)
			}
			p.Number++
		}
		if p.SHA == "" {
			if p.SHA = s.remote(p.Head); p.SHA == "" {
				return p, fmt.Errorf("open on %s, which the replay's remote does not hold: GitHub refuses it (Head sha can't be blank)", p.Head)
			}
		}
	case "edit":
		if p.keepBody && len(p.Numbers) > 0 {
			p.Body = s.pulls[p.Numbers[0]].Body
		}
	case "review":
		if p.Commit == "" {
			p.Commit = s.pulls[p.Number].Head.SHA
		}
		if p.SubmittedAt == "" {
			p.SubmittedAt = at.UTC().Format(time.RFC3339Nano)
		}
	}
	p.onBranch, p.keepBody = false, false
	return p, nil
}

var (
	// teardownEffectsRESTPath is a gh api path under a repository's pull requests or issues, as
	// gh-standin.ts reads one: the kind, the number, and what follows.
	teardownEffectsRESTPath = regexp.MustCompile(`^repos/[^/]+/[^/]+/(pulls|issues)(?:/(\d+))?(/[^?]*)?(?:\?.*)?$`)
	// teardownEffectsDiffComments is what follows a pull request's path in a comment on its diff,
	// or in a reply under one.
	teardownEffectsDiffComments = regexp.MustCompile(`^/comments(?:/\d+/replies)?$`)
	// teardownEffectsMutation, teardownEffectsMutationName and teardownEffectsNodeID read a GraphQL
	// query as gh-standin.ts reads it: whether it is a mutation, the mutation's name, and the pull
	// request's node id it names, a literal or a variable.
	teardownEffectsMutation     = regexp.MustCompile(`^\s*mutation\b`)
	teardownEffectsMutationName = regexp.MustCompile(`\{\s*(\w+)\s*\(`)
	teardownEffectsNodeID       = regexp.MustCompile(`\b(?:pullRequestId|id)\s*:\s*(?:\$(\w+)|"([^"]*)")`)
)

// teardownEffectsPRReads are the gh pr commands that read and change nothing on GitHub
// (gh-standin.ts PR_READS).
var teardownEffectsPRReads = []string{"view", "list", "checks", "diff", "status", "checkout"}

// teardownEffectsReads are the verbs of any other gh command that read and change nothing on GitHub.
var teardownEffectsReads = append(slices.Clone(teardownEffectsPRReads), "download", "watch", "search")

// teardownEffectsTraceChange is the change of a trace's GitHub action a, in gh-standin.ts's names:
// the action's own `change`, which outcome.ts records for a call gh-standin took, and otherwise, for
// a frozen trace that records none, the change gh-standin keeps of the same request. The action
// records that request as heldout-case.ts does, a gh command or a gh api method and path, with the
// request's fields as gh-standin's ghRequest reads them, and it is read here as gh-standin reads it.
// It is nil for a call that changes nothing: a gh pr read, a gh command of another group that reads
// (teardownEffectsReads), a REST GET and a GraphQL query. A write that the replay's GitHub has no
// rule for is a change, never a no-op: a REST write that no pull request rule covers is a `request`,
// which teardownEffectsScratch.change refuses by name, and so is a gh command of another group that
// does not read, an error here. A gh pr review that gh refuses for its flags is an error. What the
// call leaves to the state it is made in is left to teardownEffectsScratch.resolve: a number the
// call does not name (0, and onBranch for a gh pr command), an opened pull request's number, base
// and head commit, an edit's body (keepBody), and a review's commit and time. A comment on an issue
// is taken as one on a pull request, which change refuses where the issue is none.
func teardownEffectsTraceChange(a teardownEffectsTraceAction) (*teardownEffectsPullMutation, error) {
	if a.Change != nil {
		recorded := *a.Change
		return &recorded, nil
	}
	fields := a.Fields
	text := func(key string) string { s, _ := fields[key].(string); return s }
	named := func(key string) *string {
		if s, ok := fields[key].(string); ok {
			return &s
		}
		return nil
	}
	set := func(key string) bool { return fields[key] == true }
	opening := func() *teardownEffectsPullMutation {
		head, title := text("head"), text("title")
		if _, branch, owned := strings.Cut(head, ":"); owned {
			head = branch
		}
		return &teardownEffectsPullMutation{Action: "open", Head: head, Base: named("base"), Title: &title, Body: text("body"), Draft: set("draft") || fields["draft"] == "true"}
	}
	editing := func(n int) *teardownEffectsPullMutation {
		p := &teardownEffectsPullMutation{Action: "edit", Number: n, Title: named("title"), Base: named("base"), Body: text("body"), keepBody: named("body") == nil}
		if n > 0 {
			p.Numbers = []int{n}
		}
		return p
	}
	reviewing := func(n int, event string) *teardownEffectsPullMutation {
		state := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED", "COMMENT": "COMMENTED"}[event]
		if state == "" {
			state = "PENDING"
		}
		return &teardownEffectsPullMutation{Action: "review", Number: n, State: state, Commit: text("commit_id"), Body: text("body")}
	}
	if verb, ok := strings.CutPrefix(a.Command, "gh pr "); ok {
		if slices.Contains(teardownEffectsPRReads, verb) {
			return nil, nil
		}
		n := 0
		if number, ok := fields["number"].(float64); ok {
			n = int(number)
		}
		var p *teardownEffectsPullMutation
		switch verb {
		case "create":
			return opening(), nil
		case "edit":
			p = editing(n)
		case "review":
			var events []string
			for _, flag := range [][2]string{{"approve", "APPROVE"}, {"request-changes", "REQUEST_CHANGES"}, {"comment", "COMMENT"}} {
				if set(flag[0]) {
					events = append(events, flag[1])
				}
			}
			if len(events) != 1 {
				return nil, fmt.Errorf("gh pr review sets %d of --approve, --request-changes and --comment, and gh refuses all but one", len(events))
			}
			if events[0] != "APPROVE" && text("body") == "" {
				return nil, fmt.Errorf("gh pr review's %s has no body, and gh refuses it", events[0])
			}
			p = reviewing(n, events[0])
		case "merge":
			p = &teardownEffectsPullMutation{Action: "merge", Number: n, Auto: set("auto")}
			if set("disable-auto") {
				p = &teardownEffectsPullMutation{Action: "disable-auto", Number: n}
			}
			for _, method := range []string{"merge", "squash", "rebase"} {
				if p.Action == "merge" && p.Method == "" && set(method) {
					p.Method = method
				}
			}
		case "ready":
			p = &teardownEffectsPullMutation{Action: "ready", Number: n}
			if set("undo") {
				p.Action = "draft"
			}
		case "comment":
			p = &teardownEffectsPullMutation{Action: "comment", Number: n, Body: text("body")}
		default:
			p = &teardownEffectsPullMutation{Action: verb, Number: n, Comment: text("comment")}
		}
		p.onBranch = n == 0
		return p, nil
	}
	if a.Method == "" || a.Path == "" {
		if a.Command == "" {
			return nil, fmt.Errorf("the trace's GitHub action %d names neither a gh command nor a gh api method and path", a.Seq)
		}
		// The command is `gh <group> <verb>`, or `gh <command>` of one word, which has no verb to read.
		if words := strings.Fields(a.Command); len(words) >= 3 && !slices.Contains(teardownEffectsReads, words[2]) {
			return nil, fmt.Errorf("%s is a change the replay's GitHub has no rule for, so the replay does not make it", a.Command)
		}
		return nil, nil
	}
	method, path := strings.ToUpper(a.Method), strings.TrimPrefix(a.Path, "/")
	if path == "graphql" {
		query := text("query")
		if !teardownEffectsMutation.MatchString(query) {
			return nil, nil
		}
		p := &teardownEffectsPullMutation{Action: "mutation"}
		if name := teardownEffectsMutationName.FindStringSubmatch(query); name != nil {
			p.Name = name[1]
		}
		switch p.Name {
		case "enqueuePullRequest":
			p.Action, p.Name = "enqueue", ""
		case "dequeuePullRequest":
			p.Action, p.Name = "dequeue", ""
		}
		if bound := teardownEffectsNodeID.FindStringSubmatch(query); bound != nil {
			p.ID = bound[2]
			if bound[1] != "" {
				p.ID = text(bound[1])
			}
		}
		// Each pull request the scratch GitHub holds has the node id PR_<number>, as case-world.ts
		// and gh-standin.ts give it.
		if number, ok := strings.CutPrefix(p.ID, "PR_"); ok {
			p.Number, _ = strconv.Atoi(number)
		}
		return p, nil
	}
	rest := teardownEffectsRESTPath.FindStringSubmatch(path)
	if method == "GET" {
		return nil, nil
	}
	if rest == nil {
		return &teardownEffectsPullMutation{Action: "request", Method: method, Path: path}, nil
	}
	kind, numbered, tail := rest[1], rest[2] != "", rest[3]
	n, _ := strconv.Atoi(rest[2])
	pulls := kind == "pulls"
	state := fields["state"]
	switch {
	case pulls && !numbered && tail == "" && method == "POST":
		return opening(), nil
	case pulls && numbered && tail == "" && method == "PATCH" && state != nil && state != "" && state != false:
		p := &teardownEffectsPullMutation{Action: "reopen", Number: n}
		if state == "closed" {
			p.Action = "close"
		}
		return p, nil
	case pulls && numbered && tail == "" && method == "PATCH":
		return editing(n), nil
	case pulls && numbered && tail == "/reviews" && method == "POST":
		return reviewing(n, text("event")), nil
	case pulls && numbered && tail == "/merge" && method == "PUT":
		return &teardownEffectsPullMutation{Action: "merge", Number: n, Method: text("merge_method")}, nil
	case pulls && numbered && teardownEffectsDiffComments.MatchString(tail):
		return &teardownEffectsPullMutation{Action: "review-comment", Number: n, Body: text("body")}, nil
	case !pulls && numbered && tail == "/comments":
		return &teardownEffectsPullMutation{Action: "comment", Number: n, Body: text("body")}, nil
	}
	return &teardownEffectsPullMutation{Action: "request", Number: n, Method: method, Path: path}, nil
}

// teardownEffectsTraceRead reports whether a trace's GitHub action changes no pull request
// (teardownEffectsTraceChange): the replay makes no such call, as it makes no staging action.
func teardownEffectsTraceRead(a teardownEffectsTraceAction) bool {
	change, err := teardownEffectsTraceChange(a)
	return err == nil && change == nil
}

// teardownEffectsAnswered is what the legion tool answered the role: its result's text and
// whether the result is an error.
type teardownEffectsAnswered struct {
	isError bool
	text    string
}

// teardownEffectsKept are the requests of records a call is judged on, by their place: each
// request but a GET, and a read op's first GET of each route (teardownEffectsOrder's rule for the
// stand-in's log).
func teardownEffectsKept(op string, records []teardownEffectsHTTP) []int {
	var kept []int
	seen := map[string]bool{}
	for i, x := range records {
		if x.Method == "GET" {
			if !teardownEffectsReadOp(op) || seen[x.Path] {
				continue
			}
			seen[x.Path] = true
		}
		kept = append(kept, i)
	}
	return kept
}

// teardownEffectsJudge sets the replayed call, the requests the replay's proxy saw it send
// (replayed, whose answers are the effect record's answers from seq on) and what the tool answered
// (client), against the run's call, the stand-in's answers to it and what the role was answered.
// A request the real daemon answered otherwise than the stand-in is a difference, joined to the
// original call by its outcome.json action. A request one side sent and the other did not, a
// request naming another commit or handoff record, or a client answer that differs where no
// request was sent means the replay did not reproduce the run's state: an error, a harness failure.
func teardownEffectsJudge(index int, call teardownEffectsCall, standin []teardownEffectsStandin, replayed []teardownEffectsHTTP, client teardownEffectsAnswered, seq int, same func(run, replay teardownEffectsHTTP) error) ([]teardownEffectsDifference, error) {
	name := fmt.Sprintf("legion call %d (%s, %s)", index+1, call.Op, call.ID)
	kept := teardownEffectsKept(call.Op, replayed)
	if len(standin) == 0 && len(kept) == 0 {
		if client.isError != call.IsError || client.isError && !teardownEffectsSameText(call.Result, client.text) {
			return nil, fmt.Errorf("%s sent no request in the run or the replay, and the tool answered the run %q (error %t) and the replay %q (error %t): the replay's state is not the run's", name, call.Result, call.IsError, client.text, client.isError)
		}
		return nil, nil
	}
	routes := func(n int, at func(int) teardownEffectsHTTP) string {
		var all []string
		for i := range n {
			all = append(all, at(i).route())
		}
		return "[" + strings.Join(all, ", ") + "]"
	}
	ran := routes(len(standin), func(i int) teardownEffectsHTTP { return standin[i].teardownEffectsHTTP })
	sent := routes(len(kept), func(i int) teardownEffectsHTTP { return replayed[kept[i]] })
	if ran != sent {
		return nil, fmt.Errorf("%s sent %s in the run and %s in the replay: an unpaired request", name, ran, sent)
	}
	for k, i := range kept {
		if err := same(standin[k].teardownEffectsHTTP, replayed[i]); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	var differences []teardownEffectsDifference
	for k, i := range kept {
		run, got := standin[k], replayed[i]
		if teardownEffectsSameAnswer(run.teardownEffectsHTTP, got) {
			continue
		}
		if run.OutcomeSeq == 0 {
			return nil, fmt.Errorf("%s: the stand-in's answer to %s has no outcome.json action, so the difference cannot be joined to the original call: missing provenance", name, run.route())
		}
		differences = append(differences, teardownEffectsDifference{Seq: seq + i, Route: got.route(), Call: call.ID, OutcomeSeq: run.OutcomeSeq, Standin: run.status(), Daemon: got.status()})
	}
	return differences, nil
}

// teardownEffectsSameText reports whether two tool answers say the same: equal, or one holding the
// other where a harness wraps the tool's own text.
func teardownEffectsSameText(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a == b || a != "" && b != "" && (strings.Contains(a, b) || strings.Contains(b, a))
}

// teardownEffectsSameRequest holds the facts a request takes from the state the call ran in: the
// commit and the handoff record it names. The run's record id is resolved in the readback
// (runHandoff), the replay's in what the replay wrote (replayHandoff); both must name the same
// phase's record at the same version.
func teardownEffectsSameRequest(run, replay teardownEffectsHTTP, runHandoff, replayHandoff func(string) (teardownEffectsHandoff, bool)) error {
	var a, b struct {
		Commit         string `json:"commit"`
		HandoffID      string `json:"handoffId"`
		HandoffVersion int    `json:"handoffVersion"`
	}
	_ = json.Unmarshal(run.Body, &a)
	_ = json.Unmarshal(replay.Body, &b)
	if a.Commit != b.Commit {
		return fmt.Errorf("the run's %s named commit %q and the replay's %q: the replay's state is not the run's", run.route(), a.Commit, b.Commit)
	}
	if a.HandoffVersion != b.HandoffVersion || (a.HandoffID == "") != (b.HandoffID == "") {
		return fmt.Errorf("the run's %s named handoff %q version %d and the replay's %q version %d: the replay's state is not the run's", run.route(), a.HandoffID, a.HandoffVersion, b.HandoffID, b.HandoffVersion)
	}
	if a.HandoffID == "" {
		return nil
	}
	ran, ok := runHandoff(a.HandoffID)
	if !ok {
		return fmt.Errorf("the run's %s named handoff %s, which the readback does not hold: missing provenance", run.route(), a.HandoffID)
	}
	sent, ok := replayHandoff(b.HandoffID)
	if !ok {
		return fmt.Errorf("the replay's %s named handoff %s, which the replay did not write", run.route(), b.HandoffID)
	}
	if ran.Phase != sent.Phase || ran.Version != sent.Version {
		return fmt.Errorf("the run's %s named the %s handoff version %d and the replay's the %s handoff version %d: the replay's state is not the run's", run.route(), ran.Phase, ran.Version, sent.Phase, sent.Version)
	}
	return nil
}

type teardownEffectsPushLine struct {
	teardownEffectsPush
	at   time.Time
	line int
}

// teardownEffectsReadPushes reads pushes.log: `<time> <ref> <old> <new>` a line.
func teardownEffectsReadPushes(path string) ([]teardownEffectsPushLine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pushes []teardownEffectsPushLine
	hex := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for n, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 4 || !hex.MatchString(parts[2]) || !hex.MatchString(parts[3]) {
			return nil, fmt.Errorf("pushes.log:%d is no push: %q", n+1, line)
		}
		at, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return nil, fmt.Errorf("pushes.log:%d: %w", n+1, err)
		}
		pushes = append(pushes, teardownEffectsPushLine{teardownEffectsPush{Ref: parts[1], Old: parts[2], New: parts[3]}, at, n + 1})
	}
	return pushes, nil
}

func teardownEffectsDecode(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func teardownEffectsJSONLines[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []T
	if err := teardownEffectsLines(f, func(line []byte) error {
		var item T
		if err := json.Unmarshal(line, &item); err != nil {
			return err
		}
		out = append(out, item)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

func teardownEffectsReadTime(t *testing.T, path string) time.Time {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return at
}

// The public development control (testdata/teardown-effects/local-only-head, on the T0 example's
// P05 world, whose issue is a child; its .command says how each file was made): the implementer
// commits on its workspace with jj, writes its handoff at that commit, which only its workspace
// holds, and completes, which the tool refuses; then pushes that commit, opens the pull request and
// records k1's result there, and completes again, which the stand-in took.
const teardownEffectsLocalOnlyHead = "testdata/teardown-effects/local-only-head"

func teardownEffectsOrderOf(t *testing.T, dir string) (teardownEffectsPlan, error) {
	t.Helper()
	calls, err := teardownEffectsReadCalls(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	return teardownEffectsOrder(dir, calls, teardownEffectsReadTime(t, filepath.Join(dir, "clock.txt")), teardownEffectsReadTime(t, filepath.Join(dir, "clock-real.txt")))
}

type teardownEffectsPlaced struct {
	Kind, What string
	OutcomeSeq int
}

func teardownEffectsPlacedOf(changes []teardownEffectsChange) []teardownEffectsPlaced {
	placed := []teardownEffectsPlaced{}
	for _, c := range changes {
		placed = append(placed, teardownEffectsPlaced{c.Kind, c.What, c.OutcomeSeq})
	}
	return placed
}

// Each call gets exactly the changes recorded before it, in their recorded order, each joined to
// its record: the role's jj commit (its operation in workspace-ops.jsonl) and its handoff before the
// refused completion, and the push, the pull request and the result before the accepted one, each
// of the last four joined to the outcome.json action that scores it. Every action outcome.json
// scores is placed once, so the replay's provenance is the run's.
func TestTeardownEffectsOrderLocalOnlyHead(t *testing.T) {
	t.Parallel()
	plan, err := teardownEffectsOrderOf(t, teardownEffectsLocalOnlyHead)
	if err != nil {
		t.Fatal(err)
	}
	const start, head = "92e496e0dbed653a0675d987ce34492368592cbc", "d9585820bbbfe46ceb0a69284276fa120eefd9c5"
	if got, want := teardownEffectsPlacedOf(plan.Seed), []teardownEffectsPlaced{{"dispatch", "artifact.created", 0}, {"dispatch", "handoff.recorded", 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("seeded %+v, want %+v", got, want)
	}
	if len(plan.Steps) != 2 || len(plan.After) != 0 {
		t.Fatalf("placed %d calls and %d changes after them, want 2 and none", len(plan.Steps), len(plan.After))
	}
	first, second := plan.Steps[0], plan.Steps[1]
	if first.Call.ID != "toolu_01local_only_refused" || !first.Call.IsError || !strings.Contains(first.Call.Result, head+", is not on legion/ACME-12@origin") || len(first.Standin) != 0 {
		t.Fatalf("first call %+v with stand-in answers %+v, want the tool's own refusal and no request", first.Call, first.Standin)
	}
	if got, want := teardownEffectsPlacedOf(first.Before), []teardownEffectsPlaced{
		{"workspace", "@- " + start + ".." + head + ": commit 3797d8d1fd89561fed2d1f3e01a45f1e97975df4", 0},
		{"dispatch", "handoff.recorded", 1},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("before the first call %+v, want %+v", got, want)
	}
	if move := first.Before[0]; move.workspace != head || move.Source != "workspace-ops.jsonl:6 (jj operation a8a30e0ab996)" || move.At.Format(time.RFC3339Nano) != "2030-01-07T15:00:10.708834969Z" {
		t.Fatalf("workspace move %+v, want the move onto %s of jj operation a8a30e0ab996, at its start on the case's clock", move, head)
	}
	want := []teardownEffectsPlaced{
		{"push", "refs/heads/legion/ACME-12 0000000000000000000000000000000000000000.." + head, 2},
		{"github", "open: gh pr create --head legion/ACME-12 --base main --title feat(daemon): step records carry experiment and variant (ACME-12) --body Dispatch: ACME-12", 3},
		{"dispatch", "result.recorded", 4},
	}
	if got := teardownEffectsPlacedOf(second.Before); !reflect.DeepEqual(got, want) {
		t.Fatalf("before the second call %+v, want %+v", got, want)
	}
	if pull := second.Before[1].pull; pull == nil || pull.Number != 1 || pull.SHA != head || pull.Head != "legion/ACME-12" {
		t.Fatalf("pull request change %+v, want #1 opened on legion/ACME-12 at %s", pull, head)
	}
	if len(second.Standin) != 1 || second.Standin[0].route() != "POST /legion/v1/handoff/complete" || second.Standin[0].Status != 200 || second.Standin[0].OutcomeSeq != 5 || second.Call.ID != "toolu_05published_accepted" {
		t.Fatalf("second call %s answered %+v, want the stand-in's 200 to its completion, outcome action 5", second.Call.ID, second.Standin)
	}
}

// teardownEffectsPullRecord is the pull request change calls.jsonl's one row of the control holds:
// the open gh-standin kept of its gh pr create.
const teardownEffectsPullRecord = `,"pull":{"action":"open","number":1,"head":"legion/ACME-12","base":"main","sha":"d9585820bbbfe46ceb0a69284276fa120eefd9c5","title":"feat(daemon): step records carry experiment and variant (ACME-12)","body":"Dispatch: ACME-12","draft":false}`

// teardownEffectsEditedRun is a copy of the control in a directory of the test's own, each edit
// (a file of it, a text the file holds and the text to put there) made once, and the file remove
// (none where "") removed.
func teardownEffectsEditedRun(t *testing.T, edits [][3]string, remove string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.CopyFS(dir, os.DirFS(teardownEffectsLocalOnlyHead)); err != nil {
		t.Fatal(err)
	}
	for _, edit := range edits {
		path := filepath.Join(dir, edit[0])
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), edit[1]) {
			t.Fatalf("%s holds no %q", edit[0], edit[1])
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(data), edit[1], edit[2], 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if remove != "" {
		if err := os.Remove(filepath.Join(dir, remove)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A changed or missing ordering fact is a harness failure, named, and never a placement: each row
// changes or removes one record of the control.
func TestTeardownEffectsOrderRefusesUnplacedFacts(t *testing.T) {
	t.Parallel()
	const push, pushCase = "2026-11-27T00:07:05.361170980Z", "2030-01-07T15:00:27.372005949Z"
	const commit = `"start":"2026-11-27T00:06:48.698+00:00","end":"2026-11-27T00:06:48.726+00:00"`
	// outcome.json's GitHub action, as outcome.ts records it once it keeps the change gh-standin kept.
	changed := func(action string) [3]string {
		return [3]string{"outcome.json", `"command": "gh pr create",`, `"command": "gh pr create", "change": {"action": "` + action + `", "number": 1},`}
	}
	for _, row := range []struct {
		name   string
		edits  [][3]string // file, old, new
		remove string
		want   string
	}{
		{name: "a push inside the completion's call", edits: [][3]string{{"pushes.log", push, "2026-11-27T00:07:27.200000000Z"}, {"outcome.json", pushCase, "2030-01-07T15:00:49.210834969Z"}}, want: "falls inside legion call 2 (toolu_05published_accepted)"},
		{name: "a push the records lack", edits: [][3]string{{"pushes.log", push + " refs/heads/legion/ACME-12 0000000000000000000000000000000000000000 d9585820bbbfe46ceb0a69284276fa120eefd9c5\n", ""}}, want: "outcome.json action 2 (push  at " + pushCase + ") has no record the replay can place"},
		{name: "a push moved off the time outcome.json scores", edits: [][3]string{{"pushes.log", push, "2026-11-27T00:07:10.000000000Z"}}, want: "pushes.log:1 (refs/heads/legion/ACME-12"},
		{name: "a pull request change with no end", edits: [][3]string{{"calls.jsonl", `"done":"2026-11-27T00:07:13.872Z",`, ""}}, want: "records when the call began and not when it ended"},
		{name: "a pull request change outcome.json records and calls.jsonl lacks", edits: [][3]string{{"calls.jsonl", teardownEffectsPullRecord, ""}, changed("open")}, want: "outcome.json action 3 (github open at 2030-01-07T15:00:35.840834969Z) records a change gh-standin kept, and calls.jsonl holds no `pull` of that call: a missing mutation record"},
		{name: "a pull request change outcome.json records otherwise", edits: [][3]string{changed("merge")}, want: "is outcome.json action 3, which records the change merge: the records disagree on what the call did"},
		{name: "a pull request opened while its branch moved", edits: [][3]string{{"calls.jsonl", `"at":"2026-11-27T00:07:13.830Z"`, `"at":"2026-11-27T00:07:05.300Z"`}, {"calls.jsonl", `"done":"2026-11-27T00:07:13.872Z"`, `"done":"2026-11-27T00:07:05.500Z"`}, {"outcome.json", "2030-01-07T15:00:35.840834969Z", "2030-01-07T15:00:27.310834969Z"}}, want: "overlap, so their order is unknown"},
		{name: "a completion answered outside every call", edits: [][3]string{{"daemon.log", "2026-11-27T00:07:27.350Z", "2026-11-27T00:07:30.000Z"}}, want: "falls inside 0 legion calls: an unpaired request"},
		{name: "a Dispatch write the replay cannot make", edits: [][3]string{{"dispatch-readback.json", `"type": "result.recorded"`, `"type": "message.created"`}}, want: "precedes legion call 2 (toolu_05published_accepted): the replay writes no message.created"},
		{name: "a call whose result the transcript lacks", edits: [][3]string{{"sessions/7d1e5a12-0c05-4a12-9b12-a5120c05a512.jsonl", `"toolCallId":"toolu_05published_accepted"`, `"toolCallId":"toolu_05_elsewhere"`}}, want: "legion call 2 (toolu_05published_accepted) has no recorded result"},
		{name: "a seeded document changed after the seed", edits: [][3]string{{"dispatch-readback.json", `"id": 9005,`, `"id": 9010, "issue_key": "ACME-12", "artifact_id": "art-acme-12-verification", "seq": 7, "actor": {"kind": "session", "id": "rig-seed"}, "created_at": "2026-11-27T00:07:20.000000Z", "type": "artifact.version", "payload": {}}, {"id": 9005,`}}, want: "which the replay cannot seed"},
		{name: "a workspace move inside the refused call", edits: [][3]string{{"workspace-ops.jsonl", commit, `"start":"2026-11-27T00:06:55.100+00:00","end":"2026-11-27T00:06:55.200+00:00"`}}, want: "workspace-ops.jsonl:6 (jj operation a8a30e0ab996) (@- 92e496e0dbed653a0675d987ce34492368592cbc..d9585820bbbfe46ceb0a69284276fa120eefd9c5: commit 3797d8d1fd89561fed2d1f3e01a45f1e97975df4) falls inside legion call 1 (toolu_01local_only_refused)"},
		{name: "a workspace move that began before the launch", edits: [][3]string{{"workspace-ops.jsonl", commit, `"start":"2026-11-27T00:06:37.900+00:00","end":"2026-11-27T00:06:48.726+00:00"`}}, want: "began before the launch and ended after it"},
		{name: "a workspace record of another start", edits: [][3]string{{"workspace-ops.jsonl", `"description":"fetch from git remote(s) origin","standingOn":["92e496e0dbed653a0675d987ce34492368592cbc"]`, `"description":"fetch from git remote(s) origin","standingOn":["3d669591dcafe9170992a26efe256208e7d658bf"]`}}, want: "workspace-ops.jsonl:4 says the workspace stood on 3d669591dcafe9170992a26efe256208e7d658bf at the launch, and the case's world stands it on 92e496e0dbed653a0675d987ce34492368592cbc"},
		{name: "a run with no record of its workspace", remove: "workspace-ops.jsonl", want: "the run directory holds no workspace-ops.jsonl"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := teardownEffectsOrderOf(t, teardownEffectsEditedRun(t, row.edits, row.remove))
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("placed the run with error %v, want one naming %q", err, row.want)
			}
		})
	}
}

// What a GitHub call did is calls.jsonl's `pull` alone: a row with none (a read, or a write the
// stand-in refused) changes nothing, and the open a row keeps is placed where outcome.json records
// that change. A Dispatch write of an actor other than the role is placed by outcome.json's
// `otherWrites`, with no action of its own and its own author, and one that neither the actions nor
// `otherWrites` hold is refused.
func TestTeardownEffectsOrderAuditRecords(t *testing.T) {
	t.Parallel()
	second := func(t *testing.T, edits ...[3]string) []teardownEffectsChange {
		t.Helper()
		plan, err := teardownEffectsOrderOf(t, teardownEffectsEditedRun(t, edits, ""))
		if err != nil {
			t.Fatal(err)
		}
		return plan.Steps[1].Before
	}
	const head = "d9585820bbbfe46ceb0a69284276fa120eefd9c5"
	push := teardownEffectsPlaced{"push", "refs/heads/legion/ACME-12 0000000000000000000000000000000000000000.." + head, 2}
	result := teardownEffectsPlaced{"dispatch", "result.recorded", 4}

	// The row with no `pull`, beside an outcome.json that records no change of it: no GitHub change.
	if got, want := teardownEffectsPlacedOf(second(t, [3]string{"calls.jsonl", teardownEffectsPullRecord, ""})), []teardownEffectsPlaced{push, result}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a row with no pull placed %+v before the second call, want %+v", got, want)
	}
	// The open, joined to the outcome.json action that records it as gh-standin named it.
	recorded := second(t, [3]string{"outcome.json", `"command": "gh pr create",`, `"command": "gh pr create", "change": {"action": "open", "number": 1},`})
	if len(recorded) != 3 || recorded[1].Kind != "github" || recorded[1].OutcomeSeq != 3 || recorded[1].pull == nil || recorded[1].pull.Action != "open" {
		t.Fatalf("placed %+v before the second call, want the open at outcome.json action 3", teardownEffectsPlacedOf(recorded))
	}

	// A reviewer's comment after the seed, between the result and the second call: event 7 of the
	// readback, which outcome.json keeps among the other actors' writes at its case time.
	const reviewer = "ses-acme-12-reviewer"
	comment := [3]string{"dispatch-readback.json", `"id": 9005,`, `"id": 9007, "issue_key": "ACME-12", "seq": 7, "actor": {"kind": "session", "id": "` + reviewer + `"}, "created_at": "2026-11-27T00:07:20.000000Z", "type": "comment.created", "payload": {"body": "The branch reads well."}}, {"id": 9005,`}
	other := [3]string{"outcome.json", `"actions": [`, `"otherWrites": [{"at": "2030-01-07T15:00:42.010834969Z", "command": "comment.created", "target": "ACME-12", "fields": {"body": "The branch reads well."}, "actor": {"kind": "session", "id": "` + reviewer + `"}}], "actions": [`}
	placed := second(t, comment, other)
	if got, want := teardownEffectsPlacedOf(placed), []teardownEffectsPlaced{push, {"github", teardownEffectsPlacedOf(recorded)[1].What, 3}, result, {"dispatch", "comment.created", 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("placed %+v before the second call, want %+v", got, want)
	}
	if e := placed[3].event; e == nil || e.Actor != (teardownEffectsActor{Kind: "session", ID: reviewer}) || e.Payload["body"] != "The branch reads well." {
		t.Fatalf("the reviewer's comment is placed as %+v, want it written by %s", e, reviewer)
	}
	_, err := teardownEffectsOrderOf(t, teardownEffectsEditedRun(t, [][3]string{comment}, ""))
	if err == nil || !strings.Contains(err.Error(), "dispatch-readback.json event 7 (comment.created at 2030-01-07T15:00:42.010834969Z) has no action in outcome.json") {
		t.Fatalf("placed a write no action or other write records, with error %v", err)
	}
}

// A HARNESS verdict is typed by its cause, never read from its reason. A Dispatch write the replay
// does not make (a status write, a reply), of the session whose legion call it precedes and scored
// by outcome.json, is the role's write, named by its record, its outcome.json action and case time,
// its author and that call. The same write by another session, a handoff by a person, a write whose
// record the readback lacks or whose event carries no body, and a seeded write the replay cannot
// make are the harness's own records. Each row changes one event of the control.
func TestTeardownEffectsOrderTypesTheRoleWrite(t *testing.T) {
	t.Parallel()
	const role = "7d1e5a12-0c05-4a12-9b12-a5120c05a512"
	at := time.Date(2030, 1, 7, 15, 0, 40, 260834969, time.UTC)
	roleWrite := func(event string) *teardownEffectsFault {
		return &teardownEffectsFault{Kind: "role-write", Source: "dispatch-readback.json event 6", Event: event, At: &at, OutcomeSeq: 4, Author: &teardownEffectsActor{Kind: "session", ID: role}, Session: role, Call: "toolu_05published_accepted"}
	}
	records := &teardownEffectsFault{Kind: "harness-record"}
	payload := func(event map[string]any) map[string]any { p, _ := event["payload"].(map[string]any); return p }
	for _, row := range []struct {
		name  string
		seq   float64
		edit  func(event map[string]any)
		want  string
		fault *teardownEffectsFault
	}{
		{"the role's status write", 6, func(e map[string]any) { e["type"] = "issue.updated" }, "precedes legion call 2 (toolu_05published_accepted): the replay writes no issue.updated", roleWrite("issue.updated")},
		{"the role's reply", 6, func(e map[string]any) {
			e["type"], payload(e)["reply_to"], payload(e)["body"] = "comment.created", "cmt-1", "Done."
		}, "a reply, an answer on a question or an anchored comment (reply_to)", roleWrite("comment.created")},
		{"another session's status write", 6, func(e map[string]any) {
			e["type"], e["actor"] = "issue.updated", map[string]any{"kind": "session", "id": "ses-acme-12-tester"}
		}, "the replay writes no issue.updated", records},
		{"a person's handoff", 5, func(e map[string]any) { e["actor"] = map[string]any{"kind": "user", "id": "usr-acme"} }, "precedes legion call 1 (toolu_01local_only_refused): a handoff written by a user", records},
		{"the role's result the readback lacks", 6, func(e map[string]any) { payload(e)["result_id"] = "res-missing" }, "its result res-missing is not in the readback", records},
		{"the role's comment whose event carries no body", 6, func(e map[string]any) { e["type"] = "comment.created" }, "its event carries no body", records},
		{"a seeded write the replay cannot make", 2, func(e map[string]any) { e["type"] = "message.created" }, "which the replay cannot seed: the replay writes no message.created", records},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "run")
			if err := os.CopyFS(dir, os.DirFS(teardownEffectsLocalOnlyHead)); err != nil {
				t.Fatal(err)
			}
			teardownEffectsEditEvent(t, filepath.Join(dir, "dispatch-readback.json"), row.seq, row.edit)
			_, err := teardownEffectsOrderOf(t, dir)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("placed the run with error %v, want one naming %q", err, row.want)
			}
			got, _ := json.Marshal(teardownEffectsFaultOf(err))
			want, _ := json.Marshal(row.fault)
			if string(got) != string(want) {
				t.Fatalf("typed the HARNESS cause %s, want %s", got, want)
			}
		})
	}
}

// teardownEffectsEditEvent changes, with edit, the one event of the readback at path whose seq is
// seq.
func teardownEffectsEditEvent(t *testing.T, path string, seq float64, edit func(event map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var readback map[string]any
	if err := json.Unmarshal(data, &readback); err != nil {
		t.Fatal(err)
	}
	events, _ := readback["events"].([]any)
	edited := 0
	for _, e := range events {
		if event, ok := e.(map[string]any); ok && event["seq"] == seq {
			edit(event)
			edited++
		}
	}
	if edited != 1 {
		t.Fatalf("%s holds %d events of seq %v, want 1", path, edited, seq)
	}
	if data, err = json.Marshal(readback); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A request both sides sent and the real daemon answered otherwise is a difference, joined to the
// original call and its outcome.json action; anything that shows the replay's state is not the
// run's (a request one side sent alone, a request naming other state, a differing answer of the
// tool where neither reached a daemon, or a difference with no outcome.json action) is an error.
func TestTeardownEffectsJudge(t *testing.T) {
	t.Parallel()
	refusal := func(code string) string { return `{"code":"` + code + `","error":"refused"}` }
	complete := func(status int, answer string) teardownEffectsHTTP {
		return teardownEffectsHTTP{Method: "POST", Path: "/legion/v1/handoff/complete", Status: status, Body: json.RawMessage(`{"commit":"c","handoffId":"h","handoffVersion":1}`), Answer: json.RawMessage(answer)}
	}
	check := teardownEffectsHTTP{Method: "GET", Path: "/legion/v1/issues/ACME-12/record", Status: 200, Answer: json.RawMessage(`{}`)}
	reached := teardownEffectsCall{ID: "toolu_x", Op: "handoff_complete", Result: "{}"}
	refused := teardownEffectsCall{ID: "toolu_y", Op: "handoff_complete", IsError: true, Result: "the commit this workspace stands on, c, is not on legion/ACME-12@origin"}
	ok := teardownEffectsAnswered{text: "{}"}
	same := func(teardownEffectsHTTP, teardownEffectsHTTP) error { return nil }
	code := func(s string) *string { return &s }
	for _, row := range []struct {
		name     string
		call     teardownEffectsCall
		standin  []teardownEffectsStandin
		replayed []teardownEffectsHTTP
		client   teardownEffectsAnswered
		same     func(teardownEffectsHTTP, teardownEffectsHTTP) error
		want     []teardownEffectsDifference
		fault    string
	}{
		{name: "the same acceptance, beside the extension's own check", call: reached, standin: []teardownEffectsStandin{{complete(200, `{}`), 5}}, replayed: []teardownEffectsHTTP{check, complete(200, `{}`)}, client: ok, same: same},
		{name: "two acceptances of different statuses", call: reached, standin: []teardownEffectsStandin{{complete(200, `{}`), 5}}, replayed: []teardownEffectsHTTP{complete(201, `{"id":"r1"}`)}, client: ok, same: same},
		{name: "the real daemon refused what the stand-in took", call: reached, standin: []teardownEffectsStandin{{complete(200, `{}`), 5}}, replayed: []teardownEffectsHTTP{check, complete(409, refusal("HANDOFF_NO_PULL_REQUEST"))}, client: teardownEffectsAnswered{isError: true}, same: same,
			want: []teardownEffectsDifference{{Seq: 8, Route: "POST /legion/v1/handoff/complete", Call: "toolu_x", OutcomeSeq: 5, Standin: teardownEffectsStatus{Status: 200}, Daemon: teardownEffectsStatus{Status: 409, Code: code("HANDOFF_NO_PULL_REQUEST")}}}},
		{name: "one refusal against another", call: reached, standin: []teardownEffectsStandin{{complete(400, refusal("CHECK_WAKE_REQUIRED")), 3}}, replayed: []teardownEffectsHTTP{complete(400, refusal("HANDOFF_STALE_ENTRY"))}, client: ok, same: same,
			want: []teardownEffectsDifference{{Seq: 7, Route: "POST /legion/v1/handoff/complete", Call: "toolu_x", OutcomeSeq: 3, Standin: teardownEffectsStatus{Status: 400, Code: code("CHECK_WAKE_REQUIRED")}, Daemon: teardownEffectsStatus{Status: 400, Code: code("HANDOFF_STALE_ENTRY")}}}},
		{name: "the run's call reached the daemon and the replay's did not", call: reached, standin: []teardownEffectsStandin{{complete(200, `{}`), 5}}, client: teardownEffectsAnswered{isError: true}, same: same, fault: "an unpaired request"},
		{name: "the replay's call reached the daemon and the run's did not", call: refused, replayed: []teardownEffectsHTTP{complete(200, `{}`)}, client: ok, same: same, fault: "an unpaired request"},
		{name: "the tool refused both, in its own words", call: refused, client: teardownEffectsAnswered{isError: true, text: "Error: " + refused.Result}, same: same},
		{name: "the tool refused both, for different state", call: refused, client: teardownEffectsAnswered{isError: true, text: "the newest implement handoff of ACME-12 (version 2) names d"}, same: same, fault: "the replay's state is not the run's"},
		{name: "the tool refused the run and took the replay", call: refused, client: ok, same: same, fault: "the replay's state is not the run's"},
		{name: "a request naming other state", call: reached, standin: []teardownEffectsStandin{{complete(200, `{}`), 5}}, replayed: []teardownEffectsHTTP{complete(200, `{}`)}, client: ok,
			same: func(teardownEffectsHTTP, teardownEffectsHTTP) error { return fmt.Errorf("another handoff") }, fault: "another handoff"},
		{name: "a difference no outcome action records", call: reached, standin: []teardownEffectsStandin{{complete(200, `{}`), 0}}, replayed: []teardownEffectsHTTP{complete(409, refusal("HANDOFF_NO_PULL_REQUEST"))}, client: ok, same: same, fault: "missing provenance"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := teardownEffectsJudge(1, row.call, row.standin, row.replayed, row.client, 7, row.same)
			if row.fault != "" {
				if err == nil || !strings.Contains(err.Error(), row.fault) {
					t.Fatalf("judged %+v, %v; want an error naming %q", got, err, row.fault)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, row.want) {
				t.Fatalf("judged %+v, %v; want %+v", got, err, row.want)
			}
		})
	}
}

// A request's state facts are its commit and the handoff record it names, by phase and version:
// Dispatch gives the replay's records ids of their own.
func TestTeardownEffectsSameRequest(t *testing.T) {
	t.Parallel()
	request := func(commit, id string, version int) teardownEffectsHTTP {
		body, _ := json.Marshal(map[string]any{"commit": commit, "handoffId": id, "handoffVersion": version, "grantId": id + "-grant"})
		return teardownEffectsHTTP{Method: "POST", Path: "/legion/v1/handoff/complete", Body: body}
	}
	records := map[string]teardownEffectsHandoff{
		"run-1":    {Phase: "implement", Version: 1, ID: "run-1"},
		"replay-1": {Phase: "implement", Version: 1, ID: "replay-1"},
		"replay-2": {Phase: "implement", Version: 2, ID: "replay-2"},
	}
	resolve := func(id string) (teardownEffectsHandoff, bool) { found, ok := records[id]; return found, ok }
	for _, row := range []struct {
		name        string
		run, replay teardownEffectsHTTP
		fault       string
	}{
		{"the same record and commit under new ids", request("c1", "run-1", 1), request("c1", "replay-1", 1), ""},
		{"another commit", request("c1", "run-1", 1), request("c2", "replay-1", 1), `named commit "c1" and the replay's "c2"`},
		{"another version", request("c1", "run-1", 1), request("c1", "replay-2", 2), "version 1 and the replay's"},
		{"a record the readback lacks", request("c1", "run-9", 1), request("c1", "replay-1", 1), "missing provenance"},
	} {
		err := teardownEffectsSameRequest(row.run, row.replay, resolve, resolve)
		if row.fault == "" && err != nil || row.fault != "" && (err == nil || !strings.Contains(err.Error(), row.fault)) {
			t.Errorf("%s: %v, want %q", row.name, err, row.fault)
		}
	}
}

// A gh-standin row says itself which change it kept, and nothing reads its arguments: a row with no
// end is from a gh-standin that recorded no change, so what its call did is unknown, and a row with
// an end and no `pull` (a read, or a write the stand-in refused) changed nothing, whatever its
// arguments say.
func TestTeardownEffectsGitHubAction(t *testing.T) {
	t.Parallel()
	done := time.Date(2026, 11, 26, 20, 0, 1, 0, time.UTC)
	for _, row := range []struct {
		name   string
		row    teardownEffectsGitHubRow
		action string
		fault  bool
	}{
		{"a creation from before gh-standin recorded its end", teardownEffectsGitHubRow{Argv: []string{"pr", "create", "-H", "legion/ACME-12"}}, "", true},
		{"a read", teardownEffectsGitHubRow{Argv: []string{"pr", "view", "--json", "number"}, Done: &done}, "", false},
		{"a merge the stand-in refused", teardownEffectsGitHubRow{Argv: []string{"pr", "merge", "9", "--squash"}, Done: &done, Exit: 1}, "", false},
		{"a creation gh-standin kept nothing of", teardownEffectsGitHubRow{Argv: []string{"pr", "create", "--title", "T"}, Done: &done}, "", false},
		{"a merge gh-standin kept", teardownEffectsGitHubRow{Argv: []string{"pr", "merge", "-s"}, Done: &done, Pull: &teardownEffectsPullMutation{Action: "merge", Number: 9, Method: "squash"}}, "merge", false},
	} {
		action, err := teardownEffectsGitHubAction(row.row)
		if (err != nil) != row.fault || action != row.action {
			t.Errorf("%s: action %q, %v; want %q (an error: %t)", row.name, action, err, row.action, row.fault)
		}
	}
}

// teardownEffectsTestScratch is a scratch GitHub of two pull requests, #7 open on legion/ACME-12 (the
// issue's branch) into main and #8 merged, and a remote whose legion/ACME-12, legion/ACME-13 and
// main stand at h7, h13 and m1.
func teardownEffectsTestScratch() teardownEffectsScratch {
	open := teardownEffectsPull{Number: 7, State: "open", Base: "main", Title: "T", Body: "B"}
	open.Head.Ref, open.Head.SHA = "legion/ACME-12", "h7"
	merged := teardownEffectsPull{Number: 8, State: "merged", Base: "main", MergeCommit: "m0"}
	merged.Head.Ref, merged.Head.SHA = "legion/ACME-11", "h8"
	heads := map[string]string{"legion/ACME-12": "h7", "legion/ACME-13": "h13", "main": "m1"}
	return teardownEffectsScratch{pulls: map[int]teardownEffectsPull{7: open, 8: merged}, branch: "legion/ACME-12", base: "main", remote: func(branch string) string { return heads[branch] }}
}

// Each change in gh-standin's names is made by its one rule, a run's and a trace's alike, and one
// the scratch GitHub cannot hold is refused by name and changes nothing: a comment is quiet, a merge
// that waits for the requirements and the merge queue leave the pull request open, and a GraphQL
// mutation, a REST write and a gh pr command the replay has no rule for are never taken as made.
func TestTeardownEffectsPullChange(t *testing.T) {
	t.Parallel()
	text := func(s string) *string { return &s }
	const at = "2030-01-07T15:05:00Z"
	when, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	unchanged := func(pulls map[int]teardownEffectsPull) bool {
		return reflect.DeepEqual(pulls, teardownEffectsTestScratch().pulls)
	}
	type change = teardownEffectsPullMutation
	type pulls = map[int]teardownEffectsPull
	for _, row := range []struct {
		name   string
		before []change
		change change
		fault  string
		want   func(pulls) bool
	}{
		{name: "an open at its branch's head", change: change{Action: "open", Number: 9, Head: "legion/ACME-13", Base: text("main"), SHA: "h13", Title: text("T9"), Body: "B9", Draft: true}, want: func(p pulls) bool {
			pull := p[9]
			return pull.State == "open" && pull.Head.Ref == "legion/ACME-13" && pull.Head.SHA == "h13" && pull.Base == "main" && pull.Title == "T9" && pull.Body == "B9" && pull.Draft
		}},
		{name: "an open at another commit than its branch's", change: change{Action: "open", Number: 9, Head: "legion/ACME-13", Base: text("main"), SHA: "h0"}, fault: `the replay's remote holds "h13" there`},
		{name: "an open of a number held", change: change{Action: "open", Number: 8, Head: "legion/ACME-13", Base: text("main"), SHA: "h13"}, fault: "open of pull request #8, which the replay's GitHub already holds"},
		{name: "a second open of a branch into its base", change: change{Action: "open", Number: 9, Head: "legion/ACME-12", Base: text("main"), SHA: "h7"}, fault: "which pull request #7 is open on: GitHub opens no second"},
		{name: "an edit of the body alone", change: change{Action: "edit", Numbers: []int{7}, Body: "B2"}, want: func(p pulls) bool { return p[7].Body == "B2" && p[7].Title == "T" && p[7].Base == "main" }},
		{name: "an edit of the title and the base", change: change{Action: "edit", Numbers: []int{7}, Body: "B", Title: text("T2"), Base: text("release")}, want: func(p pulls) bool { return p[7].Title == "T2" && p[7].Base == "release" }},
		{name: "an edit of one pull request held and one not", change: change{Action: "edit", Numbers: []int{7, 99}, Body: "B2"}, fault: "edit of pull request #99, which the replay's GitHub does not hold"},
		{name: "a review", change: change{Action: "review", Number: 7, State: "APPROVED", Commit: "h7", Body: "Fine.", SubmittedAt: at}, want: func(p pulls) bool {
			return reflect.DeepEqual(p[7].Reviews, []teardownEffectsReview{{Author: teardownEffectsReviewer, State: "APPROVED", Commit: "h7", Body: "Fine.", At: when}})
		}},
		{name: "a review at no time", change: change{Action: "review", Number: 7, State: "APPROVED"}, fault: "review of pull request #7"},
		{name: "a merge", change: change{Action: "merge", Number: 7, Method: "squash"}, want: func(p pulls) bool { return p[7].State == "merged" && p[7].MergeCommit == "m1" }},
		{name: "a merge that waits for the requirements", change: change{Action: "merge", Number: 7, Method: "squash", Auto: true}, want: func(p pulls) bool { return p[7].State == "open" && p[7].AutoMerge }},
		{name: "auto-merge disarmed", before: []change{{Action: "merge", Number: 7, Auto: true}}, change: change{Action: "disable-auto", Number: 7}, want: func(p pulls) bool { return p[7].State == "open" && !p[7].AutoMerge }},
		{name: "a merge of a merged pull request", change: change{Action: "merge", Number: 8}, fault: "merge of pull request #8, which is merged: GitHub merges an open pull request alone"},
		{name: "a close", change: change{Action: "close", Number: 7, Comment: "Superseded."}, want: func(p pulls) bool { return p[7].State == "closed" }},
		{name: "a reopen of a closed pull request", before: []change{{Action: "close", Number: 7}}, change: change{Action: "reopen", Number: 7}, want: func(p pulls) bool { return p[7].State == "open" }},
		{name: "a reopen of a merged pull request", change: change{Action: "reopen", Number: 8}, fault: "GitHub neither closes nor reopens a merged pull request"},
		{name: "a pull request into draft", change: change{Action: "draft", Number: 7}, want: func(p pulls) bool { return p[7].Draft }},
		{name: "a draft made ready", before: []change{{Action: "draft", Number: 7}}, change: change{Action: "ready", Number: 7}, want: func(p pulls) bool { return !p[7].Draft }},
		{name: "an enqueue", change: change{Action: "enqueue", Number: 7, ID: "PR_7"}, want: func(p pulls) bool { return p[7].State == "open" && p[7].Queued }},
		{name: "a dequeue", before: []change{{Action: "enqueue", Number: 7}}, change: change{Action: "dequeue", Number: 7}, want: func(p pulls) bool { return p[7].State == "open" && !p[7].Queued }},
		{name: "an enqueue of a merged pull request", change: change{Action: "enqueue", Number: 8}, fault: "the merge queue takes an open pull request alone"},
		{name: "an enqueue of a node id no pull request has", change: change{Action: "enqueue", ID: "PR_kw"}, fault: "enqueue names no pull request the replay's GitHub holds"},
		{name: "a comment, which is quiet", change: change{Action: "comment", Number: 7, Body: "Pushed."}, want: unchanged},
		{name: "a comment on the diff, which is quiet", change: change{Action: "review-comment", Number: 7, Body: "Done."}, want: unchanged},
		{name: "a comment on what is no pull request held", change: change{Action: "comment", Number: 99, Body: "Pushed."}, fault: "comment of pull request #99, which the replay's GitHub does not hold"},
		{name: "another GraphQL mutation", change: change{Action: "mutation", Name: "addLabelsToLabelable", Number: 7}, fault: "the GraphQL mutation addLabelsToLabelable is a change the replay's GitHub has no rule for"},
		{name: "another REST write", change: change{Action: "request", Number: 7, Method: "DELETE", Path: "repos/acme/widgets/pulls/7/requested_reviewers"}, fault: "the REST write DELETE repos/acme/widgets/pulls/7/requested_reviewers is a change the replay's GitHub has no rule for"},
		{name: "another gh pr command", change: change{Action: "lock", Number: 7}, fault: "gh pr lock is a change the replay's GitHub has no rule for"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := teardownEffectsTestScratch()
			for _, p := range row.before {
				if err := s.change(p); err != nil {
					t.Fatal(err)
				}
			}
			kept := maps.Clone(s.pulls)
			err := s.change(row.change)
			if row.fault != "" {
				if err == nil || !strings.Contains(err.Error(), row.fault) {
					t.Fatalf("made %+v with error %v, want one naming %q", row.change, err, row.fault)
				}
				if !reflect.DeepEqual(s.pulls, kept) {
					t.Fatalf("the refused %+v changed the pull requests from %+v to %+v", row.change, kept, s.pulls)
				}
				return
			}
			if err != nil || !row.want(s.pulls) {
				t.Fatalf("made %+v (%v), and the pull requests are %+v", row.change, err, s.pulls)
			}
		})
	}
}

// A frozen trace's GitHub action, which records the request and no change, is read as gh-standin
// reads that request, and what the request leaves to the state is filled from the scratch GitHub:
// the issue branch's one open pull request, an open's number and head commit, an edit's body and a
// review's commit and time. A read changes nothing. A write that no rule covers is a `request`,
// which the scratch GitHub refuses by name (TestTeardownEffectsPullChange), and a gh command of
// another group that writes is an error: neither is a no-op. A trace's own change is taken as it
// is, and a request gh or GitHub refuses is an error.
func TestTeardownEffectsTraceChange(t *testing.T) {
	t.Parallel()
	at := time.Date(2030, 1, 7, 15, 4, 0, 0, time.UTC)
	text := func(s string) *string { return &s }
	type change = teardownEffectsPullMutation
	type fields = map[string]any
	gh := func(command string, f fields) teardownEffectsTraceAction {
		return teardownEffectsTraceAction{Kind: "github", Seq: 2, At: at, Command: command, Fields: f}
	}
	api := func(method, path string, f fields) teardownEffectsTraceAction {
		return teardownEffectsTraceAction{Kind: "github", Seq: 2, At: at, Method: method, Path: path, Fields: f}
	}
	own := gh("gh pr merge", fields{})
	own.Change = &change{Action: "merge", Number: 7, Method: "merge"}
	const reviewed = "2030-01-07T15:04:00Z"
	for _, row := range []struct {
		name   string
		action teardownEffectsTraceAction
		want   *change
		fault  string
	}{
		{"gh pr create of a branch named with its owner", gh("gh pr create", fields{"head": "acme:legion/ACME-13", "base": "main", "title": "T9", "body": "B9", "draft": true}), &change{Action: "open", Number: 9, Head: "legion/ACME-13", Base: text("main"), SHA: "h13", Title: text("T9"), Body: "B9", Draft: true}, ""},
		{"gh pr create that names no head and no base", gh("gh pr create", fields{"title": "T"}), &change{Action: "open", Number: 9, Head: "legion/ACME-12", Base: text("main"), SHA: "h7", Title: text("T")}, ""},
		{"gh pr create on a branch the remote does not hold", gh("gh pr create", fields{"head": "legion/ACME-14"}), nil, "open on legion/ACME-14, which the replay's remote does not hold"},
		{"a gh api POST that opens a pull request", api("POST", "/repos/acme/widgets/pulls", fields{"head": "legion/ACME-13", "base": "main", "title": "T9"}), &change{Action: "open", Number: 9, Head: "legion/ACME-13", Base: text("main"), SHA: "h13", Title: text("T9")}, ""},
		{"gh pr edit of the pull request it names", gh("gh pr edit", fields{"number": float64(7), "body": "B2"}), &change{Action: "edit", Number: 7, Numbers: []int{7}, Body: "B2"}, ""},
		{"gh pr edit of the issue branch's title alone", gh("gh pr edit", fields{"title": "T2"}), &change{Action: "edit", Number: 7, Numbers: []int{7}, Title: text("T2"), Body: "B"}, ""},
		{"a gh api PATCH of a body", api("PATCH", "repos/acme/widgets/pulls/7", fields{"body": "B2"}), &change{Action: "edit", Number: 7, Numbers: []int{7}, Body: "B2"}, ""},
		{"a gh api PATCH of the state", api("PATCH", "repos/acme/widgets/pulls/7", fields{"state": "closed"}), &change{Action: "close", Number: 7}, ""},
		{"gh pr review approving the issue branch's", gh("gh pr review", fields{"approve": true, "body": "Fine."}), &change{Action: "review", Number: 7, State: "APPROVED", Commit: "h7", Body: "Fine.", SubmittedAt: reviewed}, ""},
		{"gh pr review with two verdicts", gh("gh pr review", fields{"number": float64(7), "approve": true, "comment": true, "body": "x"}), nil, "gh pr review sets 2 of --approve"},
		{"gh pr review asking for changes with no body", gh("gh pr review", fields{"number": float64(7), "request-changes": true}), nil, "has no body, and gh refuses it"},
		{"a gh api POST of a review at a commit", api("POST", "repos/acme/widgets/pulls/7/reviews", fields{"event": "REQUEST_CHANGES", "commit_id": "c1", "body": "No."}), &change{Action: "review", Number: 7, State: "CHANGES_REQUESTED", Commit: "c1", Body: "No.", SubmittedAt: reviewed}, ""},
		{"gh pr merge that waits for the requirements", gh("gh pr merge", fields{"number": float64(7), "squash": true, "auto": true}), &change{Action: "merge", Number: 7, Method: "squash", Auto: true}, ""},
		{"gh pr merge --disable-auto", gh("gh pr merge", fields{"disable-auto": true}), &change{Action: "disable-auto", Number: 7}, ""},
		{"a gh api PUT of a merge", api("PUT", "repos/acme/widgets/pulls/7/merge", fields{"merge_method": "rebase"}), &change{Action: "merge", Number: 7, Method: "rebase"}, ""},
		{"gh pr ready --undo", gh("gh pr ready", fields{"undo": true}), &change{Action: "draft", Number: 7}, ""},
		{"gh pr close with a comment", gh("gh pr close", fields{"number": float64(7), "comment": "Superseded."}), &change{Action: "close", Number: 7, Comment: "Superseded."}, ""},
		{"gh pr comment", gh("gh pr comment", fields{"body": "Pushed."}), &change{Action: "comment", Number: 7, Body: "Pushed."}, ""},
		{"a reply under a comment on the diff", api("POST", "repos/acme/widgets/pulls/7/comments/5/replies", fields{"body": "Done."}), &change{Action: "review-comment", Number: 7, Body: "Done."}, ""},
		{"a comment on the pull request's issue", api("POST", "repos/acme/widgets/issues/7/comments", fields{"body": "Pushed."}), &change{Action: "comment", Number: 7, Body: "Pushed."}, ""},
		{"the merge queue's enqueue, by a variable", api("POST", "graphql", fields{"query": "mutation($pr:ID!){enqueuePullRequest(input:{pullRequestId:$pr}){mergeQueueEntry{position}}}", "pr": "PR_7"}), &change{Action: "enqueue", Number: 7, ID: "PR_7"}, ""},
		{"the merge queue's dequeue, by a literal", api("POST", "graphql", fields{"query": `mutation{dequeuePullRequest(input:{id:"PR_7"}){mergeQueueEntry{position}}}`}), &change{Action: "dequeue", Number: 7, ID: "PR_7"}, ""},
		{"another GraphQL mutation", api("POST", "graphql", fields{"query": `mutation{addLabelsToLabelable(input:{labelableId:"PR_7",labelIds:["L1"]}){clientMutationId}}`}), &change{Action: "mutation", Name: "addLabelsToLabelable"}, ""},
		{"another REST write under a pull request", api("DELETE", "repos/acme/widgets/pulls/7/requested_reviewers", fields{"reviewers": []any{"bob"}}), &change{Action: "request", Number: 7, Method: "DELETE", Path: "repos/acme/widgets/pulls/7/requested_reviewers"}, ""},
		{"a trace's own change", own, &change{Action: "merge", Number: 7, Method: "merge"}, ""},
		{"gh pr view", gh("gh pr view", fields{"number": float64(7)}), nil, ""},
		{"gh pr checks", gh("gh pr checks", fields{}), nil, ""},
		{"a gh api GET of a pull request", api("GET", "repos/acme/widgets/pulls/7", fields{}), nil, ""},
		{"a GraphQL query", api("POST", "graphql", fields{"query": "query{viewer{login}}"}), nil, ""},
		{"a gh command of another group that reads", gh("gh run view", fields{"log": true}), nil, ""},
		{"a gh command of another group that writes", gh("gh issue comment", fields{"body": "Pushed."}), nil, "gh issue comment is a change the replay's GitHub has no rule for"},
		{"a gh api write that is not under a pull request", api("POST", "repos/acme/widgets/statuses/h7", fields{"state": "success"}), &change{Action: "request", Method: "POST", Path: "repos/acme/widgets/statuses/h7"}, ""},
		{"a gh api write under a pull request's issue that is no comment", api("POST", "repos/acme/widgets/issues/7/labels", fields{"labels": []any{"x"}}), &change{Action: "request", Number: 7, Method: "POST", Path: "repos/acme/widgets/issues/7/labels"}, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := teardownEffectsTraceChange(row.action)
			if err == nil && got != nil {
				var resolved change
				resolved, err = teardownEffectsTestScratch().resolve(*got, at)
				got = &resolved
			}
			if row.fault != "" {
				if err == nil || !strings.Contains(err.Error(), row.fault) {
					t.Fatalf("read %+v with error %v, want one naming %q", got, err, row.fault)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, row.want) {
				t.Fatalf("read %+v (%v), want %+v", got, err, row.want)
			}
			if read := teardownEffectsTraceRead(row.action); read != (row.want == nil) {
				t.Fatalf("teardownEffectsTraceRead says %t, and the action changes %+v", read, row.want)
			}
		})
	}

	// A gh pr command that names no pull request, where the issue's branch has no open one.
	s := teardownEffectsTestScratch()
	delete(s.pulls, 7)
	merge, err := teardownEffectsTraceChange(gh("gh pr merge", fields{"squash": true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolve(*merge, at); err == nil || !strings.Contains(err.Error(), `no pull requests found for branch "legion/ACME-12"`) {
		t.Fatalf("resolved a merge of the issue branch's pull request where it has none, with error %v", err)
	}
}
