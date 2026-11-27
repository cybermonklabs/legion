package daemon

import (
	"encoding/json"
	"fmt"
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
// T4, lane D step 3): the route and the status must match, and a refusal's code too. An accepted
// answer's body is not compared, since the stand-in's simpler records say less than the daemon's.
func teardownEffectsSameAnswer(a, b teardownEffectsHTTP) bool {
	if a.Status != b.Status || a.Method != b.Method || a.Path != b.Path {
		return false
	}
	if a.Status < 400 {
		return true
	}
	return teardownEffectsRefusalCode(a.Answer) == teardownEffectsRefusalCode(b.Answer)
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

// teardownEffectsGitHubRow is one row of calls.jsonl (gh-standin.ts). `Done`, when the call ended,
// and `Pull`, the change gh-standin kept to a pull request, are absent from captures made before
// gh-standin recorded them.
type teardownEffectsGitHubRow struct {
	At    time.Time                    `json:"at"`
	Done  *time.Time                   `json:"done"`
	As    string                       `json:"as"`
	Argv  []string                     `json:"argv"`
	Files map[string]string            `json:"files"`
	Stdin *string                      `json:"stdin"`
	Body  *string                      `json:"body"`
	Exit  int                          `json:"exit"`
	Pull  *teardownEffectsPullMutation `json:"pull"`
}

// teardownEffectsPullMutation is the change gh-standin kept to a pull request: `open` (the number,
// branch, base and head commit it gave the new one), `edit` (the body it gave each pull request it
// answers with) or `review` (the review it added).
type teardownEffectsPullMutation struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	Numbers     []int  `json:"numbers"`
	Head        string `json:"head"`
	Base        string `json:"base"`
	SHA         string `json:"sha"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Draft       bool   `json:"draft"`
	State       string `json:"state"`
	Commit      string `json:"commit"`
	SubmittedAt string `json:"submittedAt"`
}

// teardownEffectsChange is one recorded change to Git, GitHub, Dispatch or the role's workspace:
// where it was recorded, when on the case's clock, and the outcome.json action that records it (0
// for a seeded record or a move of the workspace, which no outcome holds; the workspace record's
// line and jj operation are its provenance).
type teardownEffectsChange struct {
	Kind       string    `json:"kind"`
	Source     string    `json:"source"`
	What       string    `json:"what"`
	At         time.Time `json:"at"`
	OutcomeSeq int       `json:"outcomeSeq,omitempty"`
	from, to   time.Time
	fault      string
	push       *teardownEffectsPush
	github     *teardownEffectsGitHubRow
	event      *teardownEffectsEvent
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

type teardownEffectsOutcomeAction struct {
	Seq  int       `json:"seq"`
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	Op   string    `json:"op"`
}

// teardownEffectsOrder places a run's recorded changes and stand-in answers against its legion
// calls (calls, from teardownEffectsReadCalls). clock is the case's start (clock.txt) and launched
// the wall-clock instant of the launch (clock-real.txt), as outcome.ts moves records onto the case's
// timeline. An error is a harness failure: it names the record that cannot be placed or joined.
func teardownEffectsOrder(dir string, calls []teardownEffectsCall, clock, launched time.Time) (teardownEffectsPlan, error) {
	var plan teardownEffectsPlan
	caseAt := func(t time.Time) time.Time { return clock.Add(t.Sub(launched)).UTC() }
	if err := teardownEffectsDecode(filepath.Join(dir, "dispatch-readback.json"), &plan.Readback); err != nil {
		return plan, err
	}
	var outcome struct {
		Actions []teardownEffectsOutcomeAction `json:"actions"`
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

	// Each outcome.json action is taken once, by kind and case time, in its own order.
	type key struct {
		kind string
		at   int64
	}
	pending := map[key][]teardownEffectsOutcomeAction{}
	for _, action := range outcome.Actions {
		k := key{action.Kind, action.At.UnixNano()}
		pending[k] = append(pending[k], action)
	}
	take := func(kind string, at time.Time) (int, bool) {
		k := key{kind, caseAt(at).UnixNano()}
		actions := pending[k]
		if len(actions) == 0 {
			return 0, false
		}
		pending[k] = actions[1:]
		return actions[0].Seq, true
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
		c := teardownEffectsChange{Kind: "github", Source: fmt.Sprintf("calls.jsonl:%d", n+1), What: action + ": gh " + strings.Join(row.Argv, " "), from: row.At, github: &row}
		if row.Done != nil {
			c.to = *row.Done
		}
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
			if fault := teardownEffectsDispatchFault(&plan.Readback, e); fault != "" {
				return plan, fmt.Errorf("the case's starting state holds %s (%s), which the replay cannot seed: %s", e.Type, c.Source, fault)
			}
			plan.Seed = append(plan.Seed, c)
			continue
		}
		if e.Actor.Kind == "system" {
			// Dispatch's own write: the replayed write that called for it makes it again. outcome.json
			// holds it too, so it is taken here.
			if _, ok := take("dispatch", e.CreatedAt); !ok {
				return plan, fmt.Errorf("%s (%s) has no action in outcome.json: the records disagree", c.Source, e.Type)
			}
			continue
		}
		c.fault = teardownEffectsDispatchFault(&plan.Readback, e)
		changes = append(changes, c)
	}

	for i := range changes {
		c := &changes[i]
		c.At = caseAt(c.from)
		seq, ok := take(c.Kind, c.from)
		if !ok {
			return plan, fmt.Errorf("%s (%s at %s) has no action in outcome.json: the records disagree, so its place is not established", c.Source, c.What, c.At.Format(time.RFC3339Nano))
		}
		c.OutcomeSeq = seq
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
		if c.to.IsZero() {
			if len(calls) > 0 && !c.from.After(calls[len(calls)-1].ResultAt) {
				return plan, fmt.Errorf("%s (%s) records when the call began and not when it ended, so the legion calls after it cannot be placed against it", c.Source, c.What)
			}
			plan.After = append(plan.After, *c)
			continue
		}
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
			return plan, fmt.Errorf("%s (%s) precedes legion call %d (%s): %s", c.Source, c.What, place+1, calls[place].ID, c.fault)
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
			seq, ok := take("daemon", line.At)
			if !ok && scored {
				return plan, fmt.Errorf("daemon.log:%d (%s) has no action in outcome.json: missing provenance", n+1, line.route())
			}
			plan.Steps[in[0]].Standin = append(plan.Steps[in[0]].Standin, teardownEffectsStandin{line, seq})
			continue
		}
		if len(in) != 1 || !teardownEffectsReadOp(calls[in[0]].Op) {
			continue
		}
		step := &plan.Steps[in[0]]
		if slices.ContainsFunc(step.Standin, func(s teardownEffectsStandin) bool { return s.Method == "GET" && s.Path == line.Path }) {
			continue
		}
		seq, _ := take("daemon", line.At)
		step.Standin = append(step.Standin, teardownEffectsStandin{line, seq})
	}
	// Every push, every Dispatch write and every daemon write outcome.json scores has a record
	// placed above: an action no record matches is a fact the replay would miss.
	for k, actions := range pending {
		for _, action := range actions {
			if k.kind == "push" || k.kind == "dispatch" || k.kind == "daemon" && !teardownEffectsReadOp(action.Op) {
				return plan, fmt.Errorf("outcome.json action %d (%s %s at %s) has no record the replay can place: a missing ordering fact", action.Seq, action.Kind, action.Op, action.At.Format(time.RFC3339Nano))
			}
		}
	}
	return plan, nil
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
func teardownEffectsDispatchFault(b *teardownEffectsReadback, e teardownEffectsEvent) string {
	text := func(key string) string { s, _ := e.Payload[key].(string); return s }
	switch e.Type {
	case "handoff.recorded":
		if e.Actor.Kind != "session" {
			return "a handoff written by a " + e.Actor.Kind
		}
		if _, _, _, ok := b.handoffBody(text("handoff_id")); !ok {
			return "its record " + text("handoff_id") + " is not in the readback"
		}
	case "comment.created":
		for _, field := range []string{"reply_to", "ask_id", "anchor", "artifact_id", "block_id"} {
			if v, ok := e.Payload[field]; ok && v != nil && v != "" {
				return "a reply, an answer on a question or an anchored comment (" + field + ")"
			}
		}
		if _, ok := e.Payload["body"].(string); !ok {
			return "its event carries no body"
		}
	case "result.recorded":
		if e.Actor.Kind != "session" {
			return "a result recorded by a " + e.Actor.Kind
		}
		if _, _, ok := b.result(text("result_id")); !ok {
			return "its result " + text("result_id") + " is not in the readback"
		}
	case "ask.opened":
		for _, field := range []string{"block_id", "anchor", "artifact_id"} {
			if v, ok := e.Payload[field]; ok && v != nil && v != "" {
				return "a question of a document (" + field + ")"
			}
		}
		found := false
		for _, ask := range b.Asks {
			found = found || ask.ID == text("id")
		}
		if !found {
			return "its question " + text("id") + " is not in the readback"
		}
	case "artifact.created":
		found := false
		for _, d := range b.Documents {
			if d.ID == e.ArtifactID {
				found = d.Kind == "doc"
			}
		}
		if !found {
			return "the readback holds no document " + e.ArtifactID
		}
		for _, later := range b.Events {
			changed := later.Type == "artifact.version" || later.Type == "artifact.human_edited" || later.Type == "artifact.renamed" || later.Type == "artifact.removed"
			if later.Seq > e.Seq && later.ArtifactID == e.ArtifactID && changed {
				return "the readback holds only the document's latest text, and " + later.Type + " changed it after it was made"
			}
		}
	default:
		return "the replay writes no " + e.Type
	}
	return ""
}

var (
	teardownEffectsPullsPath   = regexp.MustCompile(`^repos/[^/]+/[^/]+/pulls$`)
	teardownEffectsPullPath    = regexp.MustCompile(`^repos/[^/]+/[^/]+/pulls/\d+$`)
	teardownEffectsReviewsPath = regexp.MustCompile(`^repos/[^/]+/[^/]+/pulls/\d+/reviews$`)
	teardownEffectsMergePath   = regexp.MustCompile(`^repos/[^/]+/[^/]+/pulls/\d+/merge$`)
)

// teardownEffectsGitHubAction is the change a gh call made to the GitHub lane R kept: `open`,
// `edit`, `review`, `merge` or `close`, and "" for a call that changed nothing it kept (a read, a
// comment, a call gh-standin answered from a fixed route). A row that names its end is
// gh-standin's newer record, whose `pull` is the change it kept; an older row is read from its
// arguments. A merge or a close is read from its arguments in both, as the daemon stand-in reads it.
func teardownEffectsGitHubAction(row teardownEffectsGitHubRow) (string, error) {
	if row.Pull != nil {
		return row.Pull.Action, nil
	}
	command, method, path, fields, err := teardownEffectsGitHubRequest(row)
	if err != nil {
		return "", err
	}
	switch {
	case command == "gh pr merge" || method == "PUT" && teardownEffectsMergePath.MatchString(path):
		return "merge", nil
	case command == "gh pr close":
		return "close", nil
	case row.Done != nil:
		return "", nil
	case command == "gh pr create" || method == "POST" && teardownEffectsPullsPath.MatchString(path):
		return "open", nil
	case (command == "gh pr edit" || method == "PATCH" && teardownEffectsPullPath.MatchString(path)) && fields["body"] != nil:
		return "edit", nil
	case command == "gh pr review" || method == "POST" && teardownEffectsReviewsPath.MatchString(path):
		return "review", nil
	}
	return "", nil
}

// teardownEffectsGitHubRequest reads a gh call's arguments as the request it makes: a `gh pr`
// command with its number and fields, or a `gh api` method, path and fields, each body or field a
// file names read from what the call was handed (`files`, `stdin`). A `gh api` call that sends
// fields and names no method is a POST, as gh makes it.
func teardownEffectsGitHubRequest(row teardownEffectsGitHubRow) (command, method, path string, fields map[string]any, err error) {
	fields = map[string]any{}
	args := row.Argv
	read := func(name string) string {
		if name == "-" && row.Stdin != nil {
			return *row.Stdin
		}
		return row.Files[name]
	}
	if len(args) < 2 {
		return "", "", "", fields, nil
	}
	api := args[0] == "api"
	switch {
	case args[0] == "pr":
		command = "gh pr " + args[1]
		args = args[2:]
	case api:
		args = args[1:]
	default:
		return "", "", "", fields, nil
	}
	// The flags that take no value, of gh api and of each gh pr command that changes a pull request.
	booleans := map[string][]string{
		"api":          {"--paginate", "--slurp", "--silent", "--include", "-i", "--verbose"},
		"gh pr create": {"--draft", "-d", "--fill", "-f", "--fill-first", "--fill-verbose", "--web", "-w", "--dry-run", "--no-maintainer-edit"},
		"gh pr review": {"--approve", "-a", "--request-changes", "-r", "--comment", "-c"},
		"gh pr merge":  {"--auto", "--disable-auto", "--merge", "-m", "--squash", "-s", "--rebase", "-r", "--admin", "--delete-branch", "-d"},
		"gh pr close":  {"--delete-branch", "-d"},
		"gh pr ready":  {"--undo"},
	}
	mode := command
	if api {
		mode = "api"
	}
	data := false
	for len(args) > 0 {
		flag := args[0]
		args = args[1:]
		if key, value, ok := strings.Cut(flag, "="); ok && strings.HasPrefix(key, "--") {
			flag = key
			args = append([]string{value}, args...)
		} else if api && len(flag) > 2 && strings.HasPrefix(flag, "-X") {
			flag, args = "-X", append([]string{flag[2:]}, args...)
		}
		if !strings.HasPrefix(flag, "-") {
			if api && path == "" {
				path = strings.TrimPrefix(flag, "/")
			} else if n, ok := teardownEffectsPullNumber(flag); ok && !api && fields["number"] == nil {
				fields["number"] = float64(n)
			}
			continue
		}
		if slices.Contains(booleans[mode], flag) {
			switch {
			case mode == "gh pr review" && (flag == "--approve" || flag == "-a"):
				fields["event"] = "APPROVE"
			case mode == "gh pr review" && (flag == "--request-changes" || flag == "-r"):
				fields["event"] = "REQUEST_CHANGES"
			case mode == "gh pr review" && (flag == "--comment" || flag == "-c"):
				fields["event"] = "COMMENT"
			case mode == "gh pr create" && (flag == "--draft" || flag == "-d"):
				fields["draft"] = true
			}
			continue
		}
		if len(args) == 0 {
			continue
		}
		value := args[0]
		args = args[1:]
		switch {
		case api && (flag == "-X" || flag == "--method"):
			method = strings.ToUpper(value)
		case api && flag == "--input":
			data = true
			if err := json.Unmarshal([]byte(read(value)), &fields); err != nil {
				return "", "", "", fields, fmt.Errorf("read the request input of gh %s: %w", strings.Join(row.Argv, " "), err)
			}
		case api && (flag == "-f" || flag == "-F" || flag == "--raw-field" || flag == "--field"):
			if key, text, ok := strings.Cut(value, "="); ok {
				data = true
				if name, ok := strings.CutPrefix(text, "@"); ok && (flag == "-F" || flag == "--field") {
					text = read(name)
				}
				fields[key] = text
			}
		case !api && (flag == "--head" || flag == "-H"):
			fields["head"] = value
		case !api && (flag == "--base" || flag == "-B"):
			fields["base"] = value
		case !api && (flag == "--title" || flag == "-t"):
			fields["title"] = value
		case !api && (flag == "--body" || flag == "-b"):
			fields["body"] = value
		case !api && (flag == "--body-file" || flag == "-F"):
			fields["body"] = read(value)
		}
	}
	if api && method == "" {
		method = "GET"
		if data {
			method = "POST"
		}
	}
	if row.Body != nil {
		fields["body"] = *row.Body
	}
	return command, method, path, fields, nil
}

// teardownEffectsPullNumber is the pull request a `gh pr` selector names by number, or by its URL.
func teardownEffectsPullNumber(selector string) (int, bool) {
	if _, tail, ok := strings.Cut(selector, "/pull/"); ok {
		selector = tail
	}
	n, err := strconv.Atoi(selector)
	return n, err == nil && n > 0
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
	if pull := second.Before[1].github.Pull; pull == nil || pull.Number != 1 || pull.SHA != head || pull.Head != "legion/ACME-12" {
		t.Fatalf("pull request change %+v, want #1 opened on legion/ACME-12 at %s", pull, head)
	}
	if len(second.Standin) != 1 || second.Standin[0].route() != "POST /legion/v1/handoff/complete" || second.Standin[0].Status != 200 || second.Standin[0].OutcomeSeq != 5 || second.Call.ID != "toolu_05published_accepted" {
		t.Fatalf("second call %s answered %+v, want the stand-in's 200 to its completion, outcome action 5", second.Call.ID, second.Standin)
	}
}

// A changed or missing ordering fact is a harness failure, named, and never a placement: each row
// changes or removes one record of the control.
func TestTeardownEffectsOrderRefusesUnplacedFacts(t *testing.T) {
	t.Parallel()
	const push, pushCase = "2026-11-27T00:07:05.361170980Z", "2030-01-07T15:00:27.372005949Z"
	const commit = `"start":"2026-11-27T00:06:48.698+00:00","end":"2026-11-27T00:06:48.726+00:00"`
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
			dir := filepath.Join(t.TempDir(), "run")
			if err := os.CopyFS(dir, os.DirFS(teardownEffectsLocalOnlyHead)); err != nil {
				t.Fatal(err)
			}
			for _, edit := range row.edits {
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
			if row.remove != "" {
				if err := os.Remove(filepath.Join(dir, row.remove)); err != nil {
					t.Fatal(err)
				}
			}
			_, err := teardownEffectsOrderOf(t, dir)
			if err == nil || !strings.Contains(err.Error(), row.want) {
				t.Fatalf("placed the run with error %v, want one naming %q", err, row.want)
			}
		})
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

// gh's arguments are read as the request gh makes; a newer gh-standin row says itself which change
// it kept, so a call that opened nothing in lane R opens nothing in the replay.
func TestTeardownEffectsGitHubRequest(t *testing.T) {
	t.Parallel()
	done := time.Date(2026, 11, 26, 20, 0, 1, 0, time.UTC)
	for _, row := range []struct {
		name                  string
		row                   teardownEffectsGitHubRow
		command, method, path string
		fields                map[string]any
		action                string
	}{
		{"a pull request opened with gh pr create", teardownEffectsGitHubRow{Argv: []string{"pr", "create", "-H", "legion/ACME-12", "-B", "main", "-t", "T", "--fill", "-b", "B"}}, "gh pr create", "", "", map[string]any{"head": "legion/ACME-12", "base": "main", "title": "T", "body": "B"}, "open"},
		{"a pull request opened through gh api fields", teardownEffectsGitHubRow{Argv: []string{"api", "repos/acme/widgets/pulls", "-f", "head=acme:legion/ACME-13", "-f", "base=main", "--jq", ".number"}}, "", "POST", "repos/acme/widgets/pulls", map[string]any{"head": "acme:legion/ACME-13", "base": "main"}, "open"},
		{"a body edited through gh api", teardownEffectsGitHubRow{Argv: []string{"api", "-XPATCH", "/repos/acme/widgets/pulls/3", "-F", "body=@body.md"}, Files: map[string]string{"body.md": "new"}}, "", "PATCH", "repos/acme/widgets/pulls/3", map[string]any{"body": "new"}, "edit"},
		{"an approval", teardownEffectsGitHubRow{Argv: []string{"pr", "review", "73", "--approve", "--body", "Fine at B."}}, "gh pr review", "", "", map[string]any{"number": float64(73), "event": "APPROVE", "body": "Fine at B."}, "review"},
		{"a merge by URL", teardownEffectsGitHubRow{Argv: []string{"pr", "merge", "https://github.com/acme/widgets/pull/9", "--squash", "--delete-branch"}, Done: &done}, "gh pr merge", "", "", map[string]any{"number": float64(9)}, "merge"},
		{"a read", teardownEffectsGitHubRow{Argv: []string{"pr", "view", "--json", "number"}}, "gh pr view", "", "", map[string]any{}, ""},
		{"a creation gh-standin kept nothing of", teardownEffectsGitHubRow{Argv: []string{"pr", "create", "--title", "T"}, Done: &done}, "gh pr create", "", "", map[string]any{"title": "T"}, ""},
	} {
		command, method, path, fields, err := teardownEffectsGitHubRequest(row.row)
		action, actionErr := teardownEffectsGitHubAction(row.row)
		if err != nil || actionErr != nil || command != row.command || method != row.method || path != row.path || !reflect.DeepEqual(fields, row.fields) || action != row.action {
			t.Errorf("%s: %q %q %q %v %v, action %q %v; want %q %q %q %v, action %q", row.name, command, method, path, fields, err, action, actionErr, row.command, row.method, row.path, row.fields, row.action)
		}
	}
}
