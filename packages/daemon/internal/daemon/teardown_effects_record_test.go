package daemon

//ci:reads packages/pi-legion/scripts/skill-scenarios/testdata/f2/sessions/p10-production-check.jsonl TestTeardownEffectsTranscriptF2 reads fixture F2 through ../../../pi-legion
//ci:reads packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json TestTeardownEffectsPromisedControls reads T0's example cases through ../../../pi-legion

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// teardownEffectsRecord is the lane D interchange record. Null is meaningful, and the
// three lists are encoded as arrays even if no tool call reached the daemon.
type teardownEffectsRecord struct {
	ID              string                      `json:"id"`
	Answers         []teardownEffectsAnswer     `json:"answers"`
	PhaseBefore     string                      `json:"phaseBefore"`
	PhaseAfter      string                      `json:"phaseAfter"`
	ProductionCheck *teardownEffectsCheck       `json:"productionCheck"`
	HandoffRecords  []teardownEffectsHandoff    `json:"handoffRecords"`
	Diverged        []teardownEffectsDifference `json:"diverged"`
}

type teardownEffectsStatus struct {
	Status int     `json:"status"`
	Code   *string `json:"code"`
}

type teardownEffectsAnswer struct {
	Seq int       `json:"seq"`
	At  time.Time `json:"at"`
	By  string    `json:"by"`
	Op  string    `json:"op"`
	teardownEffectsStatus
	Body json.RawMessage `json:"body"`
}

type teardownEffectsCheck struct {
	WakeAt       *time.Time              `json:"wakeAt"`
	CheckWakeRow *teardownEffectsWakeRow `json:"checkWakeRow"`
}

type teardownEffectsWakeRow struct {
	DueAt time.Time `json:"dueAt"`
}

type teardownEffectsHandoff struct {
	Phase   string `json:"phase"`
	Version int    `json:"version"`
	ID      string `json:"id"`
}

// teardownEffectsDifference is one request the real daemon answered otherwise than the stand-in
// answered the role (plan T4, lane D step 3), in call order: `seq` is the place of the daemon's
// real answer among the record's answers, `route` the request's method and path, `call` the
// original legion call's tool call id, and `outcomeSeq` the outcome.json action that records the
// stand-in's answer, so a reader can grade the original through that call with the real answer.
type teardownEffectsDifference struct {
	Seq        int                   `json:"seq"`
	Route      string                `json:"route"`
	Call       string                `json:"call"`
	OutcomeSeq int                   `json:"outcomeSeq"`
	Standin    teardownEffectsStatus `json:"standin"`
	Daemon     teardownEffectsStatus `json:"daemon"`
}

// teardownEffectsReplayRecord is replay.json, written beside effects.json for every replayed run,
// and alone where the replay is a harness failure: the verdict (`ok`, `DIVERGED` or `HARNESS`, with
// the reason of a harness failure and its typed cause, `fault`), the seeded Dispatch records the
// replay started from, each call replayed with the recorded changes applied before it, the state it
// ran in, the stand-in's answers and the real daemon's, and the client's answer in the run and in
// the replay; then the changes the replay made after its last call (`end`: a control's trace makes
// every action it holds, so the actions after its last daemon call too; a run's replay makes none),
// the changes recorded after the last call, which the replay did not apply (`after`: a run's), and
// the calls it did not replay, which follow the first call the real daemon answered otherwise.
// Every list is an array, never null (teardownEffectsNewReplayRecord).
type teardownEffectsReplayRecord struct {
	ID          string                      `json:"id"`
	Verdict     string                      `json:"verdict"`
	Reason      string                      `json:"reason,omitempty"`
	Fault       *teardownEffectsFault       `json:"fault,omitempty"`
	Seed        []teardownEffectsChange     `json:"seed"`
	Calls       []teardownEffectsReplayCall `json:"calls"`
	End         []teardownEffectsChange     `json:"end"`
	After       []teardownEffectsChange     `json:"after"`
	NotReplayed []string                    `json:"notReplayed"`
}

// teardownEffectsFault is the cause of a HARNESS verdict, typed so that a reader never parses the
// reason's text (plan v51 T6 step 4). `role-write` is a Dispatch write of the role's own session
// (`session`, whose legion calls the run holds, is its `author`) that the replay does not make,
// before the later legion call `call`: the record (`source`, its event type `event`) and the
// outcome.json action that scores it (`outcomeSeq`, at its case time `at`). `harness-record` is
// every other cause: a change the records cannot place or join, a write of another author or one
// whose record the readback lacks, a seeded record the replay cannot make, a failed setup.
type teardownEffectsFault struct {
	Kind       string                `json:"kind"`
	Source     string                `json:"source,omitempty"`
	Event      string                `json:"event,omitempty"`
	At         *time.Time            `json:"at,omitempty"`
	OutcomeSeq int                   `json:"outcomeSeq,omitempty"`
	Author     *teardownEffectsActor `json:"author,omitempty"`
	Session    string                `json:"session,omitempty"`
	Call       string                `json:"call,omitempty"`
}

// teardownEffectsNewReplayRecord is the replay.json of run id before anything is replayed, with
// every list an empty array, so a record written at any point of the replay encodes no null list.
func teardownEffectsNewReplayRecord(id string) teardownEffectsReplayRecord {
	return teardownEffectsReplayRecord{ID: id, Seed: []teardownEffectsChange{}, Calls: []teardownEffectsReplayCall{}, End: []teardownEffectsChange{}, After: []teardownEffectsChange{}, NotReplayed: []string{}}
}

// teardownEffectsStopped is the reason of the receipt a replay writes where it stopped before its
// verdict: the failure's own words are in the test log.
const teardownEffectsStopped = "the replay stopped before its verdict; the test log holds the failure"

// teardownEffectsCreateNew writes value to path as indented JSON, and never over a file that
// exists: a replay's record is never written over another's.
func teardownEffectsCreateNew(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(data, '\n'))
	return err
}

// teardownEffectsStoppedReceipt writes at path the receipt of a replay that failed before it wrote
// its own (failed, and not written): HARNESS, of the harness's own records, with record as far as
// the replay got. It writes nothing for a replay that did not fail or that wrote its receipt, and
// nothing over a receipt that exists.
func teardownEffectsStoppedReceipt(path string, record teardownEffectsReplayRecord, failed, written bool) error {
	if !failed || written {
		return nil
	}
	record.Verdict, record.Reason, record.Fault = "HARNESS", teardownEffectsStopped, &teardownEffectsFault{Kind: "harness-record"}
	return teardownEffectsCreateNew(path, record)
}

type teardownEffectsReplayCall struct {
	Call       string                     `json:"call"`
	Op         string                     `json:"op"`
	At         time.Time                  `json:"at"`
	Before     []teardownEffectsChange    `json:"before"`
	State      teardownEffectsState       `json:"state"`
	Standin    []teardownEffectsAnswerRef `json:"standin"`
	Daemon     []teardownEffectsAnswerRef `json:"daemon"`
	Client     teardownEffectsClientRef   `json:"client"`
	PhaseAfter string                     `json:"phaseAfter"`
}

// teardownEffectsAnswerRef is one answer to a call's request: a stand-in's, with the outcome.json
// action that records it, or the real daemon's, with its place among effects.json's answers.
type teardownEffectsAnswerRef struct {
	OutcomeSeq int    `json:"outcomeSeq,omitempty"`
	Seq        *int   `json:"seq,omitempty"`
	Route      string `json:"route"`
	teardownEffectsStatus
}

type teardownEffectsClientRef struct {
	Run    teardownEffectsClientAnswer `json:"run"`
	Replay teardownEffectsClientAnswer `json:"replay"`
}

type teardownEffectsClientAnswer struct {
	IsError bool   `json:"isError"`
	Text    string `json:"text"`
}

// teardownEffectsState is what a call ran against once the changes before it were applied: the
// issue's phase, the tree the daemon holds it in (its own key for a root, its parent's for a child)
// and whether that tree runs, its parent in Dispatch ("" for none), the commit its workspace stands
// on (`@-`), the head of its branch on the remote ("" where the remote holds no branch) and each
// pull request the GitHub fixture holds.
type teardownEffectsState struct {
	Phase    string                     `json:"phase"`
	Tree     string                     `json:"tree"`
	TreeRuns bool                       `json:"treeRuns"`
	Parent   string                     `json:"parent"`
	Standing string                     `json:"standing"`
	Branch   string                     `json:"branch"`
	Pulls    []teardownEffectsPullState `json:"pulls"`
}

type teardownEffectsPullState struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   string `json:"head"`
}

// teardownEffectsCall is one legion call of a run's transcript, its time, and the result the role
// was given: when it came back, its text and whether it is an error.
type teardownEffectsCall struct {
	ID, Op, Session string
	Args            map[string]any
	At, ResultAt    time.Time
	Result          string
	IsError         bool
}

// teardownEffectsReadCalls reads the full arguments on assistant message blocks, not the
// truncated tool execution logs. Calls in one message keep block order; across transcripts
// they are merged by timestamp, with file order breaking ties.
func teardownEffectsReadCalls(dir string) ([]teardownEffectsCall, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no session transcripts in %s", dir)
	}
	var calls []teardownEffectsCall
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		got, err := teardownEffectsTranscript(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		calls = append(calls, got...)
	}
	slices.SortStableFunc(calls, func(a, b teardownEffectsCall) int { return a.At.Compare(b.At) })
	return calls, nil
}

func teardownEffectsTranscript(r io.Reader) ([]teardownEffectsCall, error) {
	var calls []teardownEffectsCall
	var session string
	byID := map[string]int{}
	err := teardownEffectsLines(r, func(line []byte) error {
		var entry struct {
			Type, ID  string
			Timestamp time.Time
			Message   struct {
				Role, ToolCallID string
				Content          json.RawMessage
				IsError          bool
			}
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			return err
		}
		if entry.Type == "session" {
			session = entry.ID
			return nil
		}
		if entry.Type != "message" {
			return nil
		}
		switch entry.Message.Role {
		case "toolResult":
			if i, ok := byID[entry.Message.ToolCallID]; ok {
				var blocks []struct{ Type, Text string }
				if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil {
					return fmt.Errorf("result of legion call %s: %w", entry.Message.ToolCallID, err)
				}
				var text strings.Builder
				for _, block := range blocks {
					if block.Type == "text" {
						text.WriteString(block.Text)
					}
				}
				calls[i].ResultAt, calls[i].Result, calls[i].IsError = entry.Timestamp, text.String(), entry.Message.IsError
			}
		case "assistant":
			var blocks []struct {
				Type, Name, ID string
				Arguments      json.RawMessage
			}
			if err := json.Unmarshal(entry.Message.Content, &blocks); err != nil {
				return err
			}
			for _, block := range blocks {
				if block.Type != "toolCall" || strings.TrimLeft(block.Name, "_") != "legion" {
					continue
				}
				var args map[string]any
				if err := json.Unmarshal(block.Arguments, &args); err != nil {
					return fmt.Errorf("legion call %s: %w", block.ID, err)
				}
				op, _ := args["op"].(string)
				_, duplicate := byID[block.ID]
				if session == "" || block.ID == "" || duplicate || entry.Timestamp.IsZero() {
					return fmt.Errorf("legion call %q: missing session, id, or time, or repeated id", block.ID)
				}
				byID[block.ID] = len(calls)
				calls = append(calls, teardownEffectsCall{ID: block.ID, Op: op, Args: args, Session: session, At: entry.Timestamp})
			}
		}
		return nil
	})
	return calls, err
}

func teardownEffectsLines(r io.Reader, visit func([]byte) error) error {
	b := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, err := b.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) != 0 {
			if problem := visit(line); problem != nil {
				return fmt.Errorf("line %d: %w", n, problem)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// teardownEffectsComplete is the daemon route a handoff_complete reaches.
const teardownEffectsComplete = "/legion/v1/handoff/complete"

// The shell validates case files with heldout-case.ts. These projections only read the
// fields needed to set up or exercise the live daemon; they do not define the case schema.
type teardownEffectsCase struct {
	ID, Kind, Role, Task string
	World                teardownEffectsWorld
	Actions              []teardownEffectsCaseAction
	Traces               struct{ Good, Bad teardownEffectsTrace }
}

// teardownEffectsCaseAction is one correct next action of a case: the routes that take it, and the
// effect it drives (heldout-case.ts CaseAction).
type teardownEffectsCaseAction struct {
	Name   string
	Routes []teardownEffectsRoute
	Effect struct {
		Kind, Answer, PhaseAfter, Outbox string
		WakeAt                           struct{ After, Before time.Time }
	}
}

// teardownEffectsRoute is one way to take a case action (heldout-case.ts Route): an action of its
// kind whose fields hold Match, and whose time at each Between field falls inside its window.
type teardownEffectsRoute struct {
	Kind    string
	Match   map[string]any
	Between []struct {
		Field         string
		After, Before time.Time
	}
}

// teardownEffectsTrace is a case's known-good or known-bad trace: an outcome record.
type teardownEffectsTrace struct {
	ID      string
	Actions []teardownEffectsTraceAction
}

// teardownEffectsTraceAction is one scored action of a trace (heldout-case.ts OutcomeAction).
type teardownEffectsTraceAction struct {
	Kind, Op, Command, Method, Path, Target, Ref, Commit string
	Seq                                                  int
	At                                                   time.Time
	Args, Fields                                         map[string]any
	Answer                                               struct {
		Code *string
		Body any
	}
	// Change is a GitHub action's change as gh-standin kept it, where outcome.ts recorded one
	// (teardownEffectsTraceChange); a frozen trace holds none.
	Change *teardownEffectsPullMutation
}

type teardownEffectsWorld struct {
	Clock    time.Time
	Dispatch struct {
		Issue     struct{ Key, Title, Status, Spec, Parent string }
		Documents []struct{ Name, Body string }
		Handoffs  []struct {
			Phase, Author string
			Version       int
			Body          json.RawMessage
		}
	}
	Git *struct {
		Commits []struct {
			ID, Description string
			Parents         []string
		}
		Remote struct {
			Name     string
			Branches []struct{ Name, Head string }
		}
		Workspace struct {
			StandingOn string
			LocalOnly  []string
		}
	}
	GitHub *struct {
		Repository   string
		PullRequests []teardownEffectsPull
	}
	Daemon *struct {
		Phase phase.Phase
		Step  *struct {
			Place int
			Name  string
		}
		Task            map[string]string
		ProductionCheck *struct {
			Look              int
			Last              bool
			OpenedAt, BoundAt time.Time
			MergeCommit       string
		}
	}
}

// teardownEffectsPull is one pull request of the scratch GitHub, as the case's world gives it
// (heldout-case.ts PullRequest), and, once a change armed it, its auto-merge and its place in the
// merge queue (teardownEffectsScratch.change), which no world records.
type teardownEffectsPull struct {
	Number                                int
	State, Base, Title, Body, MergeCommit string
	Draft                                 bool
	Head                                  struct{ Ref, SHA string }
	Reviews                               []teardownEffectsReview
	AutoMerge, Queued                     bool `json:"-"`
}

// teardownEffectsReview is one review of a pull request: the world's, or one a change added.
type teardownEffectsReview struct {
	Author, State, Commit, Body string
	At                          time.Time
}

func teardownEffectsParams(op string, args map[string]any) map[string]any {
	params := make(map[string]any, len(args)+1)
	for k, v := range args {
		params[k] = v
	}
	params["op"] = op
	return params
}
func teardownEffectsString(fields map[string]any, key string) string {
	text, _ := fields[key].(string)
	return text
}

// teardownEffectsTraces are the traces of case c that lane D's --controls replays, in its order:
// the known-good trace, and the known-bad trace where the case has one. An A case is a task agent's
// run in a plain session, under no daemon, and lane D replays neither of its traces.
func teardownEffectsTraces(c teardownEffectsCase) []string {
	switch {
	case c.Kind == "A":
		return nil
	case c.Traces.Bad.ID == "":
		return []string{"good"}
	}
	return []string{"good", "bad"}
}

// trace is c's trace that name names: good or bad.
func (c teardownEffectsCase) trace(name string) teardownEffectsTrace {
	if name == "bad" {
		return c.Traces.Bad
	}
	return c.Traces.Good
}

// teardownEffectsControlDir is where --controls writes the two records of a trace's replay that a
// run's replay writes, replay.json and effects.json: <out>/<case id>/<good or bad>. The
// heldout-reader.ts function controlAt reads them there.
func teardownEffectsControlDir(out, caseID, trace string) string {
	return filepath.Join(out, caseID, trace)
}

// The checks of a control (teardownEffectsControl). A trace's own check, replayed, holds when the
// replay made every action of the trace, each daemon answer was the one the trace records, and the
// trace's replay.json (verdict ok) and effects.json are written. Each other check is one of plan T4's
// lane D controls, fixture row F3 among them, and one action of a known trace shows it. The effect
// check holds a case action's daemon effect on the known-good trace's action that takes it.
const (
	teardownEffectsReplayed      = "replayed"
	teardownEffectsWakeRefused   = "pending-without-wake-refused"
	teardownEffectsWakeAccepted  = "valid-pending-names-wake"
	teardownEffectsAccepted      = "pushed-with-open-pull-request-accepted"
	teardownEffectsCommentNoWake = "dispatch-comment-names-no-wake"
	teardownEffectsLocalOnly     = "local-only-implementer-refused"
	teardownEffectsNoPullRequest = "pushed-without-pull-request-refused"
	teardownEffectsEffect        = "effect"
)

// teardownEffectsChecks are the checks a case file of --controls must show at least once: the four
// answers daemon-answers.json captures, the refusal of a head only the workspace holds, and a
// comment that names no wake.
var teardownEffectsChecks = []string{teardownEffectsWakeRefused, teardownEffectsWakeAccepted, teardownEffectsAccepted, teardownEffectsCommentNoWake, teardownEffectsLocalOnly, teardownEffectsNoPullRequest}

// teardownEffectsControl is one control --controls promises: the check Check, on the trace Trace
// (good or bad) of the case Case, at the trace's action Seq (0 for the whole trace). An effect
// check also names the case action, Action, whose effect that trace action must drive.
type teardownEffectsControl struct {
	Case, Trace, Check, Action string
	Seq                        int
}

func (c teardownEffectsControl) String() string {
	what := c.Check
	if c.Check == teardownEffectsEffect {
		what = "the effect of " + c.Action
	}
	if c.Seq == 0 {
		return fmt.Sprintf("%s %s trace: %s", c.Case, c.Trace, what)
	}
	return fmt.Sprintf("%s %s trace, action %d: %s", c.Case, c.Trace, c.Seq, what)
}

// teardownEffectsPromised is every control --controls promises for cases, in the order it makes
// them (plan T4, lane D step 4, and fixture row F3):
//   - each trace lane D replays (teardownEffectsTraces), replayed;
//   - on a known-good trace, each handoff_complete that the trace records refused
//     CHECK_WAKE_REQUIRED (pending-without-wake-refused), each taken pending that names wake_at
//     (valid-pending-names-wake), and, in an implementer case in implementing, each taken
//     completion (pushed-with-open-pull-request-accepted);
//   - on any trace of a case whose production check is on a look that is not the last, each
//     Dispatch comment (dispatch-comment-names-no-wake);
//   - on a known-good trace, each case action whose effect is the daemon's (answered or
//     check-wake), at the trace's last action that takes it;
//   - once, on the first case teardownEffectsPublishable names, its known-good completion made
//     before the trace pushes (local-only-implementer-refused) and after the push but before the
//     pull request (pushed-without-pull-request-refused).
//
// It also returns an error that names each problem: a known-good trace that takes no case action
// whose effect the case names, or that answers one otherwise than the case says; a daemon action
// whose seq no receipt can name; and a check of teardownEffectsChecks that no case shows.
func teardownEffectsPromised(cases []teardownEffectsCase) ([]teardownEffectsControl, error) {
	var promised []teardownEffectsControl
	var problems []string
	published := false
	for _, c := range cases {
		daemon := c.World.Daemon
		implementing := c.Role == "implementer" && daemon != nil && daemon.Phase == phase.Implementing
		looking := daemon != nil && daemon.ProductionCheck != nil && !daemon.ProductionCheck.Last
		for _, name := range teardownEffectsTraces(c) {
			trace := c.trace(name)
			promised = append(promised, teardownEffectsControl{Case: c.ID, Trace: name, Check: teardownEffectsReplayed})
			for _, a := range trace.Actions {
				if a.Kind == "daemon" && a.Seq < 1 {
					problems = append(problems, fmt.Sprintf("%s's %s trace holds a daemon action at seq %d, and a receipt names an action by a seq of 1 or more", c.ID, name, a.Seq))
				}
				at := teardownEffectsControl{Case: c.ID, Trace: name, Seq: a.Seq}
				switch {
				case a.Kind == "dispatch" && a.Command == "comment" && looking:
					at.Check = teardownEffectsCommentNoWake
				case name != "good" || a.Kind != "daemon" || a.Op != "handoff_complete":
				case a.Answer.Code != nil && *a.Answer.Code == "CHECK_WAKE_REQUIRED":
					at.Check = teardownEffectsWakeRefused
				case a.Answer.Code != nil:
				case a.Args["verdict"] == "pending" && a.Args["wake_at"] != nil:
					at.Check = teardownEffectsWakeAccepted
				case implementing:
					at.Check = teardownEffectsAccepted
				}
				if at.Check != "" {
					promised = append(promised, at)
				}
			}
			if name != "good" {
				continue
			}
			for _, action := range c.Actions {
				if action.Effect.Kind != "answered" && action.Effect.Kind != "check-wake" {
					continue
				}
				taker, ok := teardownEffectsTaker(action, trace)
				if !ok {
					problems = append(problems, fmt.Sprintf("%s's known-good trace takes no action %s, whose effect the case names", c.ID, action.Name))
					continue
				}
				if answer := teardownEffectsAnswerName(taker.Answer.Code); action.Effect.Kind == "answered" && answer != action.Effect.Answer {
					problems = append(problems, fmt.Sprintf("%s's known-good trace takes its action %s at action %d, which it records answered %s, and the case says %s", c.ID, action.Name, taker.Seq, answer, action.Effect.Answer))
					continue
				}
				promised = append(promised, teardownEffectsControl{Case: c.ID, Trace: "good", Check: teardownEffectsEffect, Action: action.Name, Seq: taker.Seq})
			}
		}
		if seq, ok := teardownEffectsPublishable(c); ok && !published {
			published = true
			promised = append(promised, teardownEffectsControl{Case: c.ID, Trace: "good", Check: teardownEffectsLocalOnly, Seq: seq},
				teardownEffectsControl{Case: c.ID, Trace: "good", Check: teardownEffectsNoPullRequest, Seq: seq})
		}
	}
	for _, check := range teardownEffectsChecks {
		if !slices.ContainsFunc(promised, func(c teardownEffectsControl) bool { return c.Check == check }) {
			problems = append(problems, "no case shows the check "+check)
		}
	}
	if len(problems) > 0 {
		return promised, errors.New(strings.Join(problems, "; "))
	}
	return promised, nil
}

// teardownEffectsPublishable is the seq of the completion of case c's known-good trace that
// --controls makes before the trace publishes its head, where c can show both refusals: an
// implementer case in implementing whose workspace stands on a commit only it holds, and whose
// known-good trace writes a handoff, pushes the issue branch and makes a completion the daemon
// takes (the first such completion).
func teardownEffectsPublishable(c teardownEffectsCase) (int, bool) {
	daemon, git := c.World.Daemon, c.World.Git
	if c.Kind == "A" || c.Role != "implementer" || daemon == nil || daemon.Phase != phase.Implementing || git == nil || !slices.Contains(git.Workspace.LocalOnly, git.Workspace.StandingOn) {
		return 0, false
	}
	branch := "legion/" + c.World.Dispatch.Issue.Key
	wrote, pushed, seq := false, false, 0
	for _, a := range c.Traces.Good.Actions {
		switch {
		case a.Kind == "dispatch" && a.Command == "handoff-write":
			wrote = true
		case a.Kind == "push" && strings.TrimPrefix(a.Ref, "refs/heads/") == branch:
			pushed = true
		case a.Kind == "daemon" && a.Op == "handoff_complete" && a.Answer.Code == nil && seq == 0:
			seq = a.Seq
		}
	}
	return seq, wrote && pushed && seq > 0
}

// teardownEffectsAnswerName is an answer as a case action's effect names it: ok for a request the
// daemon took (code nil), and otherwise the refusal code the trace records.
func teardownEffectsAnswerName(code *string) string {
	if code == nil {
		return "ok"
	}
	return *code
}

// teardownEffectsTaker is the last action of trace that takes the case action, by one of its routes
// (teardownEffectsTakes): a corrected retry after a refusal the case allows comes after the refused
// attempt, so the last taking is the one that drives the action's effect.
func teardownEffectsTaker(action teardownEffectsCaseAction, trace teardownEffectsTrace) (teardownEffectsTraceAction, bool) {
	for _, a := range slices.Backward(trace.Actions) {
		if slices.ContainsFunc(action.Routes, func(route teardownEffectsRoute) bool { return teardownEffectsTakes(route, a) }) {
			return a, true
		}
	}
	return teardownEffectsTraceAction{}, false
}

// teardownEffectsTakes reports whether the trace action a takes route, as heldout-case.ts's Route
// defines it: an action of the route's kind whose fields hold the route's match (each named field
// equal; an object holds a match's members, recursively), and whose time at each `between` field (a
// dot path, as args.wake_at) falls strictly inside its window.
func teardownEffectsTakes(route teardownEffectsRoute, a teardownEffectsTraceAction) bool {
	if route.Kind != a.Kind {
		return false
	}
	fields := teardownEffectsFieldsOf(a)
	if !teardownEffectsHolds(fields, map[string]any(route.Match)) {
		return false
	}
	for _, window := range route.Between {
		var at any = fields
		for part := range strings.SplitSeq(window.Field, ".") {
			object, _ := at.(map[string]any)
			at = object[part]
		}
		text, _ := at.(string)
		when, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || !when.After(window.After) || !when.Before(window.Before) {
			return false
		}
	}
	return true
}

// teardownEffectsFieldsOf is a trace action's fields as a route matches them: the fields
// heldout-case.ts's DETAILS gives its kind, each as the case file holds it.
func teardownEffectsFieldsOf(a teardownEffectsTraceAction) map[string]any {
	fields := map[string]any{}
	set := func(key, value string) {
		if value != "" {
			fields[key] = value
		}
	}
	switch a.Kind {
	case "daemon":
		var code any
		if a.Answer.Code != nil {
			code = *a.Answer.Code
		}
		fields["args"], fields["answer"] = a.Args, map[string]any{"code": code, "body": a.Answer.Body}
		set("op", a.Op)
	case "dispatch":
		fields["fields"] = a.Fields
		set("command", a.Command)
		set("target", a.Target)
	case "github":
		fields["fields"] = a.Fields
		set("command", a.Command)
		set("method", a.Method)
		set("path", a.Path)
	case "push":
		set("ref", a.Ref)
		set("commit", a.Commit)
	}
	return fields
}

// teardownEffectsHolds reports whether have holds want: an object holds each member of want,
// recursively, and any other value is equal to it.
func teardownEffectsHolds(have, want any) bool {
	members, ok := want.(map[string]any)
	if !ok {
		return reflect.DeepEqual(have, want)
	}
	object, ok := have.(map[string]any)
	if !ok {
		return false
	}
	for key, member := range members {
		if !teardownEffectsHolds(object[key], member) {
			return false
		}
	}
	return true
}

// teardownEffectsOutcomeCode is an answer as an outcome record names it (outcome.ts daemonAnswer):
// its refusal code, HTTP_<status> for a refusal that names none, and "" for an acceptance.
func teardownEffectsOutcomeCode(s teardownEffectsStatus) string {
	switch {
	case s.Code != nil:
		return *s.Code
	case s.Status >= 400:
		return "HTTP_" + strconv.Itoa(s.Status)
	}
	return ""
}

// teardownEffectsTraceAnswer holds the requests a replayed call of a trace's daemon action sent
// (replayed, as the replay's proxy saw them reach the real daemon) to the answer that action, at
// seq, records (code, as outcome.json records one: nil for an acceptance). The call must send one
// request it is judged on: a scored write, or the GET of a read op (teardownEffectsKept). A trace
// records no status, so the stand-in answer that replay.json gives the action is the real daemon's
// own status and code, which is the same answer. Any other answer is an error: the trace is not
// what the real daemon answers, and the control is a harness failure.
func teardownEffectsTraceAnswer(seq int, op string, code *string, replayed []teardownEffectsHTTP) (teardownEffectsAnswerRef, error) {
	var judged []teardownEffectsHTTP
	for _, i := range teardownEffectsKept(op, replayed) {
		if x := replayed[i]; x.Method == "GET" || teardownEffectsScoredWrites[x.Path] {
			judged = append(judged, x)
		}
	}
	if len(judged) != 1 {
		var routes []string
		for _, x := range judged {
			routes = append(routes, x.route())
		}
		return teardownEffectsAnswerRef{}, fmt.Errorf("the trace's action %d (%s) records one daemon answer, and the replayed call sent %d requests it is judged on: [%s]", seq, op, len(judged), strings.Join(routes, ", "))
	}
	got := judged[0]
	want := ""
	if code != nil {
		want = *code
	}
	if have := teardownEffectsOutcomeCode(got.status()); have != want {
		return teardownEffectsAnswerRef{}, fmt.Errorf("the trace's action %d (%s) records the answer %s, and the real daemon answered %s %s", seq, op, teardownEffectsAnswerName(code), got.route(), teardownEffectsStatusText(got.status()))
	}
	return teardownEffectsAnswerRef{OutcomeSeq: seq, Route: got.route(), teardownEffectsStatus: got.status()}, nil
}

// teardownEffectsTraceWrite is the trace's Dispatch write a as the run replay's writer makes one
// (applyDispatch): the event of its kind, by the role's session, and a readback that holds the
// record the event names. A handoff's head is mapped to its commit (sha) and takes the version
// that version gives its phase; a result's commit is mapped too. It writes what the run replay
// writes: a handoff, an issue comment, a result and a question. Any other write is an error, a
// document among them (Dispatch's artifact.created event names no text, so a trace holds none),
// and so is a write on another issue than key.
func teardownEffectsTraceWrite(key, session string, a teardownEffectsTraceAction, sha func(string) string, version func(phase string) int) (teardownEffectsEvent, teardownEffectsReadback, error) {
	var readback teardownEffectsReadback
	author := teardownEffectsActor{Kind: "session", ID: session}
	id := fmt.Sprintf("trace-action-%d", a.Seq)
	event := teardownEffectsEvent{Seq: int64(a.Seq), CreatedAt: a.At, Actor: author}
	fields := maps.Clone(a.Fields)
	if fields == nil {
		fields = map[string]any{}
	}
	if a.Target != key {
		return event, readback, fmt.Errorf("the trace's Dispatch write %s is on %s, and the replay writes on %s alone", a.Command, a.Target, key)
	}
	var records map[string]any
	switch a.Command {
	case "handoff-write":
		word := teardownEffectsString(fields, "phase")
		body, _ := fields["body"].(map[string]any)
		if body == nil {
			return event, readback, fmt.Errorf("the trace's handoff write at action %d holds no body object", a.Seq)
		}
		body = maps.Clone(body)
		if head := teardownEffectsString(body, "head"); head != "" {
			body["head"] = sha(head)
		}
		text, err := json.Marshal(body)
		if err != nil {
			return event, readback, err
		}
		event.Type, event.Payload = "handoff.recorded", map[string]any{"handoff_id": id}
		records = map[string]any{"handoffs": []any{map[string]any{"id": id, "phase": word, "version": version(word), "body": string(text), "author": author}}}
	case "result":
		commit := teardownEffectsString(fields, "commit_sha")
		if commit == "" {
			commit = teardownEffectsString(fields, "commit")
		}
		fields["id"], fields["commit_sha"] = id, sha(commit)
		event.Type, event.Payload = "result.recorded", map[string]any{"result_id": id}
		records = map[string]any{"results": map[string]any{"checks": []any{map[string]any{"block_id": teardownEffectsString(fields, "check"), "results": []any{fields}}}}}
	case "comment":
		event.Type, event.Payload = "comment.created", fields
	case "ask":
		fields["id"] = id
		event.Type, event.Payload = "ask.opened", fields
		records = map[string]any{"asks": []any{map[string]any{"id": id, "question": fields["question"], "options": fields["options"]}}}
	case "artifact":
		return event, readback, fmt.Errorf("the trace's document write at action %d holds no text: Dispatch's artifact.created event names none, so the replay cannot make it", a.Seq)
	default:
		return event, readback, fmt.Errorf("the replay makes no Dispatch write of the command %s (the trace's action %d)", a.Command, a.Seq)
	}
	if records != nil {
		data, err := json.Marshal(records)
		if err == nil {
			err = json.Unmarshal(data, &readback)
		}
		if err != nil {
			return event, readback, fmt.Errorf("the readback of the trace's action %d: %w", a.Seq, err)
		}
	}
	return event, readback, nil
}

// teardownEffectsTurned is one legion call as a replay made it: its record in replay.json, the
// requests the real daemon answered, what the tool answered, the handoff records its requests
// named, and the issue's production check just before the call and just after it. A check of a
// Dispatch comment (dispatch-comment-names-no-wake) has no call: its record holds the phase before
// the comment and after it, and before and after hold the check.
type teardownEffectsTurned struct {
	call          teardownEffectsReplayCall
	replies       []teardownEffectsHTTP
	client        teardownEffectsAnswered
	handoffs      []teardownEffectsHandoff
	before, after *teardownEffectsCheck
}

// teardownEffectsShows is nil where turned, the replay of the trace action that control names,
// shows control's check, and otherwise says what it shows instead. c is the control's case.
func teardownEffectsShows(control teardownEffectsControl, c teardownEffectsCase, turned teardownEffectsTurned) error {
	var a teardownEffectsTraceAction
	for _, each := range c.trace(control.Trace).Actions {
		if each.Seq == control.Seq {
			a = each
		}
	}
	client, after := turned.client, turned.after
	moved := turned.call.PhaseAfter != turned.call.State.Phase
	unchanged := func() error {
		if moved || !teardownEffectsSameCheck(turned.before, after) {
			return fmt.Errorf("the phase moved from %s to %s, or the production check from %+v to %+v", turned.call.State.Phase, turned.call.PhaseAfter, turned.before, after)
		}
		return nil
	}
	// answered holds the call's one completion to status and code, and the tool's answer to an
	// error exactly where the daemon refused it.
	answered := func(status int, code string) error {
		if _, err := teardownEffectsOneAnswer(turned.replies, status, code); err != nil {
			return err
		}
		if client.isError != (status >= 400) {
			return fmt.Errorf("the tool answered %q (error %t)", client.text, client.isError)
		}
		return nil
	}
	switch control.Check {
	case teardownEffectsWakeRefused:
		if err := answered(400, "CHECK_WAKE_REQUIRED"); err != nil {
			return err
		}
		if !strings.Contains(client.text, "CHECK_WAKE_REQUIRED") {
			return fmt.Errorf("the tool answered %q, which names no CHECK_WAKE_REQUIRED", client.text)
		}
		return unchanged()
	case teardownEffectsWakeAccepted:
		if err := answered(200, ""); err != nil {
			return err
		}
		wake, err := time.Parse(time.RFC3339Nano, teardownEffectsString(a.Args, "wake_at"))
		if err != nil || moved || after == nil || after.WakeAt == nil || after.CheckWakeRow == nil || !after.WakeAt.Equal(wake) || !after.CheckWakeRow.DueAt.Equal(wake) {
			return fmt.Errorf("the production check is %+v and the phase moved from %s to %s, and both the wake and the check_wake row are due at the report's wake_at %q with the phase unmoved", after, turned.call.State.Phase, turned.call.PhaseAfter, teardownEffectsString(a.Args, "wake_at"))
		}
	case teardownEffectsAccepted:
		if err := answered(200, ""); err != nil {
			return err
		}
		if !moved || len(turned.handoffs) != 1 {
			return fmt.Errorf("the phase moved from %s to %s with %d handoff records named, and a taken completion moves it with its one handoff record", turned.call.State.Phase, turned.call.PhaseAfter, len(turned.handoffs))
		}
	case teardownEffectsLocalOnly:
		remote := "origin"
		if git := c.World.Git; git != nil && git.Remote.Name != "" {
			remote = git.Remote.Name
		}
		branch := "legion/" + c.World.Dispatch.Issue.Key + "@" + remote
		if !client.isError || !strings.Contains(client.text, "is not on "+branch) || len(teardownEffectsCompletions(turned.replies)) != 0 {
			return fmt.Errorf("the tool answered %q (error %t) with %d completions sent, and it refuses a head that is not on %s before any completion reaches the daemon", client.text, client.isError, len(teardownEffectsCompletions(turned.replies)), branch)
		}
		return unchanged()
	case teardownEffectsNoPullRequest:
		if err := answered(409, "HANDOFF_NO_PULL_REQUEST"); err != nil {
			return err
		}
		return unchanged()
	case teardownEffectsCommentNoWake:
		return unchanged()
	case teardownEffectsEffect:
		i := slices.IndexFunc(c.Actions, func(each teardownEffectsCaseAction) bool { return each.Name == control.Action })
		if i < 0 {
			return fmt.Errorf("the case has no action %s", control.Action)
		}
		effect := c.Actions[i].Effect
		switch effect.Kind {
		case "answered":
			// The answer itself is the trace's (teardownEffectsTraceAnswer), which
			// teardownEffectsPromised holds to the case's.
			if turned.call.PhaseAfter != effect.PhaseAfter {
				return fmt.Errorf("the phase after the call is %s, and the case says %s", turned.call.PhaseAfter, effect.PhaseAfter)
			}
		case "check-wake":
			if after == nil || after.WakeAt == nil || after.CheckWakeRow == nil || !after.WakeAt.After(effect.WakeAt.After) || !after.WakeAt.Before(effect.WakeAt.Before) || !after.CheckWakeRow.DueAt.Equal(*after.WakeAt) {
				return fmt.Errorf("the production check is %+v, and the case names a wake inside (%s, %s) with the check_wake row due then", after, effect.WakeAt.After.Format(time.RFC3339), effect.WakeAt.Before.Format(time.RFC3339))
			}
		}
	default:
		return fmt.Errorf("no check %s", control.Check)
	}
	return nil
}

// teardownEffectsCompletions are the handoff_complete requests of replies.
func teardownEffectsCompletions(replies []teardownEffectsHTTP) []teardownEffectsHTTP {
	var found []teardownEffectsHTTP
	for _, x := range replies {
		if x.Path == teardownEffectsComplete {
			found = append(found, x)
		}
	}
	return found
}

// teardownEffectsOneAnswer is the one handoff_complete request of replies, where the daemon answered
// it status with the refusal code code ("" for none).
func teardownEffectsOneAnswer(replies []teardownEffectsHTTP, status int, code string) (teardownEffectsHTTP, error) {
	found := teardownEffectsCompletions(replies)
	if len(found) != 1 || found[0].Status != status || teardownEffectsOutcomeCode(teardownEffectsStatus{Code: found[0].status().Code}) != code {
		var answers []string
		for _, x := range found {
			answers = append(answers, teardownEffectsStatusText(x.status()))
		}
		return teardownEffectsHTTP{}, fmt.Errorf("the daemon answered %d completions [%s], and the check needs one answered %d %s", len(found), strings.Join(answers, ", "), status, code)
	}
	return found[0], nil
}

// teardownEffectsSameCheck reports whether two reads of a production check hold the same wake and
// the same check_wake row.
func teardownEffectsSameCheck(a, b *teardownEffectsCheck) bool {
	if a == nil || b == nil {
		return a == b
	}
	same := func(x, y *time.Time) bool { return x == nil && y == nil || x != nil && y != nil && x.Equal(*y) }
	var rowA, rowB *time.Time
	if a.CheckWakeRow != nil {
		rowA = &a.CheckWakeRow.DueAt
	}
	if b.CheckWakeRow != nil {
		rowB = &b.CheckWakeRow.DueAt
	}
	return same(a.WakeAt, b.WakeAt) && same(rowA, rowB)
}

// teardownEffectsMissed names each control of promised that did not execute (executed holds the ones
// that did), in promised's order.
func teardownEffectsMissed(promised []teardownEffectsControl, executed map[teardownEffectsControl]bool) []string {
	var missed []string
	for _, control := range promised {
		if !executed[control] {
			missed = append(missed, control.String())
		}
	}
	return missed
}

// teardownEffectsStatusText is an answer's status and refusal code, as a verdict line names it.
func teardownEffectsStatusText(s teardownEffectsStatus) string {
	if s.Code == nil {
		return strconv.Itoa(s.Status)
	}
	return strconv.Itoa(s.Status) + " " + *s.Code
}

func TestTeardownEffectsRecordJSONRoundTrip(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/teardown-effects/effects-example.json")
	if err != nil {
		t.Fatal(err)
	}
	var record teardownEffectsRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Answers) != 2 || record.Answers[0].Code == nil || *record.Answers[0].Code != "CHECK_WAKE_REQUIRED" || record.ProductionCheck == nil || record.ProductionCheck.CheckWakeRow == nil {
		t.Fatalf("wrong decoded effect: %+v", record)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the effect: %s", encoded)
	}
}

// replay.json as heldout-reader-receipt.ts's ReplayRecord reads it, byte for byte: every list an
// array however little the replay reached, `reason` and `fault` on a HARNESS verdict alone, and a
// role's write named by its record, its outcome.json action and case time, its author, the role's
// session and the call it preceded (the fault teardownEffectsOrder gives the local-only-head control
// whose result event is made a status write). testdata/teardown-effects/replay-example.json is the
// three records as replay.json encodes each, in one array; heldout-reader.test.ts reads that file.
func TestTeardownEffectsReplayRecordJSON(t *testing.T) {
	t.Parallel()
	const id, role = "1b815eb3-4c9a-4174-b231-5c3d864f1622", "7d1e5a12-0c05-4a12-9b12-a5120c05a512"
	ok := teardownEffectsNewReplayRecord(id)
	ok.Verdict = "ok"
	at := time.Date(2030, 1, 7, 15, 0, 40, 260834969, time.UTC)
	write := teardownEffectsNewReplayRecord(id)
	write.Verdict, write.Reason = "HARNESS", "dispatch-readback.json event 6 (issue.updated) precedes legion call 2 (toolu_05published_accepted): the replay writes no issue.updated"
	write.Fault = &teardownEffectsFault{Kind: "role-write", Source: "dispatch-readback.json event 6", Event: "issue.updated", At: &at, OutcomeSeq: 4, Author: &teardownEffectsActor{Kind: "session", ID: role}, Session: role, Call: "toolu_05published_accepted"}
	records := teardownEffectsNewReplayRecord(id)
	setup := fmt.Errorf("boot changed ACME-12 from implementing to planning")
	records.Verdict, records.Reason, records.Fault = "HARNESS", setup.Error(), teardownEffectsFaultOf(setup)
	got, err := json.MarshalIndent([]teardownEffectsReplayRecord{ok, write, records}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/teardown-effects/replay-example.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(append(got, '\n')) != string(want) {
		t.Fatalf("encoded the records\n%s\nwant testdata/teardown-effects/replay-example.json\n%s", got, want)
	}
}

// A replay that fails before it writes its receipt leaves one HARNESS receipt of the harness's own
// records, whose reason sends the reader to the test log; none where the replay did not fail or
// wrote its own, and none over a receipt that exists.
func TestTeardownEffectsStoppedReceipt(t *testing.T) {
	t.Parallel()
	const id = "1b815eb3-4c9a-4174-b231-5c3d864f1622"
	path := filepath.Join(t.TempDir(), id, "replay.json")
	record := teardownEffectsNewReplayRecord(id)
	for _, row := range []struct{ failed, written bool }{{false, false}, {true, true}, {false, true}} {
		if err := teardownEffectsStoppedReceipt(path, record, row.failed, row.written); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed %t, written %t: wrote %s (%v)", row.failed, row.written, path, err)
		}
	}
	if err := teardownEffectsStoppedReceipt(path, record, true, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got teardownEffectsReplayRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Verdict != "HARNESS" || got.Reason != teardownEffectsStopped || got.Fault == nil || *got.Fault != (teardownEffectsFault{Kind: "harness-record"}) || !strings.Contains(string(data), `"notReplayed": []`) {
		t.Fatalf("wrote %s, want the HARNESS receipt of the harness's own records with every list an array", data)
	}
	if err := teardownEffectsStoppedReceipt(path, record, true, false); !errors.Is(err, os.ErrExist) {
		t.Fatalf("wrote a second receipt over the first: %v", err)
	}
}

func TestTeardownEffectsTranscript(t *testing.T) {
	t.Parallel()
	f, err := os.Open("testdata/teardown-effects/session-synthetic.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	calls, err := teardownEffectsTranscript(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("read %d legion calls, want 2", len(calls))
	}
	for i, want := range []struct {
		id, at  string
		wake    any
		isError bool
		result  string
	}{
		{"toolu_02legion_refused", "2030-01-08T12:03:10Z", nil, true, "400 CHECK_WAKE_REQUIRED: a pending report names wakeAt"},
		{"toolu_03legion_ok", "2030-01-08T12:03:40Z", "2030-01-08T12:30:00Z", false, "{}"},
	} {
		got := calls[i]
		if got.ID != want.id || got.Op != "handoff_complete" || got.Session != "11111111-2222-4333-8444-555555555555" || got.At.Format(time.RFC3339) != want.at || got.ResultAt.Sub(got.At) != time.Second || got.Args["wake_at"] != want.wake ||
			got.IsError != want.isError || !strings.HasPrefix(got.Result, want.result) {
			t.Fatalf("call %d: %+v, want %s at %s (wake %v), answered %q (error %t)", i, got, want.id, want.at, want.wake, want.result, want.isError)
		}
	}
}

// Fixture F2 is a real lane R transcript (P10's production-check look, captured by the T4 proof's
// lane R Job; packages/pi-legion/scripts/skill-scenarios/testdata/f2/f2.command). Its one legion
// call is the pending report, read whole from the assistant message with the session the header
// names and the time its result came back.
func TestTeardownEffectsTranscriptF2(t *testing.T) {
	t.Parallel()
	f, err := os.Open("../../../pi-legion/scripts/skill-scenarios/testdata/f2/sessions/p10-production-check.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	calls, err := teardownEffectsTranscript(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("read %d legion calls, want 1: %+v", len(calls), calls)
	}
	got := calls[0]
	if got.ID != "toolu_0145VcE1ybv5hPDBo9UkaRbD" || got.Op != "handoff_complete" || got.Session != "01a217c6-05b8-71f0-8c27-f4db9cca186b" ||
		got.At.Format(time.RFC3339Nano) != "2026-11-26T12:15:29.293Z" || got.ResultAt.Format(time.RFC3339Nano) != "2026-11-26T12:15:29.31Z" ||
		got.Args["verdict"] != "pending" || got.Args["check_opened_at"] != "2030-01-08T11:58:00Z" || got.Args["wake_at"] != "2030-01-08T13:00:00Z" {
		t.Fatalf("read %+v, want the pending report with its check, wake and times", got)
	}
	if summary, _ := got.Args["summary"].(string); !strings.HasPrefix(summary, "Look 1 (woken by the merge)") {
		t.Fatalf("summary %q is not the call's whole argument", summary)
	}
}

// teardownEffectsExample is T0's public example case file, which --controls runs in the proof Job.
const teardownEffectsExample = "../../../pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json"

// teardownEffectsExampleCases are the example's cases as --controls reads them, each case changed
// by edit first (as JSON objects, before they are read), where edit is not nil.
func teardownEffectsExampleCases(t *testing.T, edit func(cases []map[string]any) []map[string]any) []teardownEffectsCase {
	t.Helper()
	data, err := os.ReadFile(teardownEffectsExample)
	if err != nil {
		t.Fatal(err)
	}
	var file struct{ Cases []map[string]any }
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		file.Cases = edit(file.Cases)
	}
	encoded, err := json.Marshal(file.Cases)
	if err != nil {
		t.Fatal(err)
	}
	var cases []teardownEffectsCase
	if err := json.Unmarshal(encoded, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

// teardownEffectsMember is the member of a JSON value at path: an object's member by its name, an
// array's by its place.
func teardownEffectsMember(value any, path ...any) any {
	for _, step := range path {
		switch at := step.(type) {
		case string:
			object, _ := value.(map[string]any)
			value = object[at]
		case int:
			array, _ := value.([]any)
			value = array[at]
		}
	}
	return value
}

// For T0's example, --controls promises each trace replayed, each check one of its actions shows,
// each case effect at the known-good action that takes it, and the completion made before the
// trace publishes its head. A known-good trace that holds any daemon operation is judged by the
// case's own effects, and a case file that cannot show a control is refused with each problem
// named.
func TestTeardownEffectsPromisedControls(t *testing.T) {
	t.Parallel()
	names := func(promised []teardownEffectsControl) []string {
		var got []string
		for _, control := range promised {
			got = append(got, control.String())
		}
		return got
	}
	promised, err := teardownEffectsPromised(teardownEffectsExampleCases(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"P05 good trace: replayed",
		"P05 good trace, action 5: pushed-with-open-pull-request-accepted",
		"P05 good trace, action 5: the effect of complete",
		"P05 bad trace: replayed",
		"P05 good trace, action 5: local-only-implementer-refused",
		"P05 good trace, action 5: pushed-without-pull-request-refused",
		"P10 good trace: replayed",
		"P10 good trace, action 2: pending-without-wake-refused",
		"P10 good trace, action 3: valid-pending-names-wake",
		"P10 good trace, action 3: the effect of report-pending",
		"P10 bad trace: replayed",
		"P10 bad trace, action 1: dispatch-comment-names-no-wake",
	}
	if got := names(promised); !slices.Equal(got, want) {
		t.Fatalf("promised\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A known-good trace of sign_off and set_status is judged by the case's own effect: the sign-off
	// answered ok, at the action that takes it. No operation is refused for being no completion.
	signOff := func(cases []map[string]any) []map[string]any {
		other := map[string]any{}
		for key, value := range cases[0] {
			other[key] = value
		}
		answer := map[string]any{"code": nil, "body": map[string]any{}}
		other["id"] = "X01"
		other["traces"] = map[string]any{"good": map[string]any{"id": "0b9c2f1e-5d4a-4c3b-8a2e-9f1d0c3b2a10", "actions": []any{
			map[string]any{"kind": "daemon", "seq": 1, "at": "2030-01-07T15:01:00Z", "op": "sign_off", "args": map[string]any{"summary": "Signed off."}, "answer": answer},
			map[string]any{"kind": "daemon", "seq": 2, "at": "2030-01-07T15:02:00Z", "op": "set_status", "args": map[string]any{"issue": "ACME-12", "status": "blocked"}, "answer": answer},
		}}}
		other["actions"] = []any{
			map[string]any{"name": "sign-off", "routes": []any{map[string]any{"kind": "daemon", "match": map[string]any{"op": "sign_off"}}}, "effect": map[string]any{"kind": "answered", "answer": "ok", "phaseAfter": "done"}},
			map[string]any{"name": "park", "routes": []any{map[string]any{"kind": "daemon", "match": map[string]any{"op": "set_status"}}}, "effect": map[string]any{"kind": "unchanged"}},
		}
		return append(cases, other)
	}
	signed, err := teardownEffectsPromised(teardownEffectsExampleCases(t, signOff))
	if err != nil {
		t.Fatalf("a known-good trace of sign_off and set_status: %v", err)
	}
	if got := names(signed)[len(want):]; !slices.Equal(got, []string{"X01 good trace: replayed", "X01 good trace, action 1: the effect of sign-off"}) {
		t.Fatalf("the sign_off case promised %q", got)
	}

	// An A case runs in a plain session, and lane D replays none of it.
	agent, err := teardownEffectsPromised(teardownEffectsExampleCases(t, func(cases []map[string]any) []map[string]any {
		other := map[string]any{}
		for key, value := range cases[0] {
			other[key] = value
		}
		other["id"], other["kind"] = "A01", "A"
		return append(cases, other)
	}))
	if err != nil || !slices.Equal(names(agent), want) {
		t.Fatalf("an A case promised %q, %v", names(agent)[len(want):], err)
	}

	for _, row := range []struct {
		name  string
		edit  func(cases []map[string]any) []map[string]any
		names []string
	}{
		{"no Dispatch comment on a look that is not the last", func(cases []map[string]any) []map[string]any {
			teardownEffectsMember(cases[1], "traces", "bad").(map[string]any)["actions"] = []any{}
			return cases
		}, []string{teardownEffectsCommentNoWake}},
		{"a known-good trace that takes no action whose effect the case names", func(cases []map[string]any) []map[string]any {
			good := teardownEffectsMember(cases[1], "traces", "good").(map[string]any)
			good["actions"] = good["actions"].([]any)[:2]
			return cases
		}, []string{"P10", "report-pending", teardownEffectsWakeAccepted}},
		{"a known-good trace that answers an action otherwise than the case says", func(cases []map[string]any) []map[string]any {
			teardownEffectsMember(cases[0], "actions", 4, "effect").(map[string]any)["answer"] = "HANDOFF_NO_PULL_REQUEST"
			return cases
		}, []string{"P05", "complete", "action 5"}},
		{"a workspace that stands on a pushed commit", func(cases []map[string]any) []map[string]any {
			teardownEffectsMember(cases[0], "world", "git", "workspace").(map[string]any)["localOnly"] = []any{}
			return cases
		}, []string{teardownEffectsLocalOnly, teardownEffectsNoPullRequest}},
		{"a daemon action no receipt can name", func(cases []map[string]any) []map[string]any {
			teardownEffectsMember(cases[0], "traces", "bad", "actions", 4).(map[string]any)["seq"] = 0
			return cases
		}, []string{"P05", "bad", "seq 0"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, err := teardownEffectsPromised(teardownEffectsExampleCases(t, row.edit))
			if err == nil {
				t.Fatal("promised the controls with no problem named")
			}
			for _, name := range row.names {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("%v does not name %s", err, name)
				}
			}
		})
	}
}

// A replayed call of a trace's daemon action is held to the answer the trace records: the same
// acceptance, or the same refusal in outcome terms, which replay.json names with the daemon's own
// status. Any other answer, or a call that sent no request or two that it is judged on, is an error.
func TestTeardownEffectsTraceAnswer(t *testing.T) {
	t.Parallel()
	http := func(method, path string, status int, answer string) teardownEffectsHTTP {
		return teardownEffectsHTTP{Method: method, Path: path, Status: status, Answer: json.RawMessage(answer)}
	}
	complete := func(status int, answer string) teardownEffectsHTTP {
		return http("POST", teardownEffectsComplete, status, answer)
	}
	refusal := func(code string) string { return `{"code":"` + code + `","error":"refused"}` }
	check := http("GET", "/legion/v1/issues/ACME-12/record", 200, `{}`)
	code := func(s string) *string { return &s }
	for _, row := range []struct {
		name     string
		op       string
		code     *string
		replayed []teardownEffectsHTTP
		want     teardownEffectsAnswerRef
		fault    bool
	}{
		{name: "an acceptance, beside the extension's own check", op: "handoff_complete", replayed: []teardownEffectsHTTP{check, complete(200, `{}`)},
			want: teardownEffectsAnswerRef{OutcomeSeq: 5, Route: "POST " + teardownEffectsComplete, teardownEffectsStatus: teardownEffectsStatus{Status: 200}}},
		{name: "a refusal, named with the daemon's status", op: "handoff_complete", code: code("CHECK_WAKE_REQUIRED"), replayed: []teardownEffectsHTTP{complete(400, refusal("CHECK_WAKE_REQUIRED"))},
			want: teardownEffectsAnswerRef{OutcomeSeq: 5, Route: "POST " + teardownEffectsComplete, teardownEffectsStatus: teardownEffectsStatus{Status: 400, Code: code("CHECK_WAKE_REQUIRED")}}},
		{name: "a refusal that names no code", op: "handoff_complete", code: code("HTTP_409"), replayed: []teardownEffectsHTTP{complete(409, `"conflict"`)},
			want: teardownEffectsAnswerRef{OutcomeSeq: 5, Route: "POST " + teardownEffectsComplete, teardownEffectsStatus: teardownEffectsStatus{Status: 409}}},
		{name: "a sign-off", op: "sign_off", replayed: []teardownEffectsHTTP{http("POST", "/legion/v1/signoff", 200, `{}`)},
			want: teardownEffectsAnswerRef{OutcomeSeq: 5, Route: "POST /legion/v1/signoff", teardownEffectsStatus: teardownEffectsStatus{Status: 200}}},
		{name: "a status write", op: "set_status", replayed: []teardownEffectsHTTP{check, http("POST", "/legion/v1/issues/status", 200, `{}`)},
			want: teardownEffectsAnswerRef{OutcomeSeq: 5, Route: "POST /legion/v1/issues/status", teardownEffectsStatus: teardownEffectsStatus{Status: 200}}},
		{name: "a read, its route's first GET", op: "read_state", replayed: []teardownEffectsHTTP{http("GET", "/legion/v1/state", 200, `{}`), http("GET", "/legion/v1/state", 200, `{}`)},
			want: teardownEffectsAnswerRef{OutcomeSeq: 5, Route: "GET /legion/v1/state", teardownEffectsStatus: teardownEffectsStatus{Status: 200}}},
		{name: "the daemon took what the trace records refused", op: "handoff_complete", code: code("CHECK_WAKE_REQUIRED"), replayed: []teardownEffectsHTTP{complete(200, `{}`)}, fault: true},
		{name: "the daemon refused what the trace records taken", op: "handoff_complete", replayed: []teardownEffectsHTTP{complete(409, refusal("HANDOFF_NO_PULL_REQUEST"))}, fault: true},
		{name: "another refusal", op: "handoff_complete", code: code("CHECK_WAKE_REQUIRED"), replayed: []teardownEffectsHTTP{complete(400, refusal("HANDOFF_STALE_ENTRY"))}, fault: true},
		{name: "no request reached the daemon", op: "handoff_complete", replayed: []teardownEffectsHTTP{check}, fault: true},
		{name: "two requests", op: "handoff_complete", replayed: []teardownEffectsHTTP{complete(200, `{}`), complete(200, `{}`)}, fault: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			got, err := teardownEffectsTraceAnswer(5, row.op, row.code, row.replayed)
			if row.fault {
				if err == nil || !strings.Contains(err.Error(), "action 5") {
					t.Fatalf("judged %+v, %v; want an error that names action 5", got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, row.want) {
				t.Fatalf("judged %+v, %v; want %+v", got, err, row.want)
			}
		})
	}
}

// A trace's Dispatch write is made by the run replay's own writer: a handoff, an issue comment, a
// result and a question each become the event and the readback record that writer takes, its
// commits mapped; a write the run replay does not make is refused, by the writer's own rule or here.
func TestTeardownEffectsTraceWrite(t *testing.T) {
	t.Parallel()
	const session = "11111111-2222-4333-8444-555555555555"
	sha := func(id string) string { return "sha-of-" + id }
	version := func(phase string) int { return map[string]int{"implement": 2}[phase] }
	at := time.Date(2030, 1, 7, 15, 4, 5, 0, time.UTC)
	write := func(command, target string, fields map[string]any) teardownEffectsTraceAction {
		return teardownEffectsTraceAction{Kind: "dispatch", Command: command, Target: target, Seq: 4, At: at, Fields: fields}
	}
	made := func(a teardownEffectsTraceAction) (teardownEffectsEvent, teardownEffectsReadback) {
		t.Helper()
		e, readback, err := teardownEffectsTraceWrite("ACME-12", session, a, sha, version)
		if err != nil {
			t.Fatalf("%s: %v", a.Command, err)
		}
		if fault, _ := teardownEffectsDispatchFault(&readback, e); fault != "" || e.Actor != (teardownEffectsActor{Kind: "session", ID: session}) {
			t.Fatalf("%s: the run replay's writer refuses %+v: %s", a.Command, e, fault)
		}
		return e, readback
	}

	e, readback := made(write("handoff-write", "ACME-12", map[string]any{"phase": "implement", "body": map[string]any{"head": "c1", "summary": "Done."}}))
	body, version2, phase, ok := readback.handoffBody(teardownEffectsString(e.Payload, "handoff_id"))
	if e.Type != "handoff.recorded" || !ok || phase != "implement" || version2 != 2 || body != `{"head":"sha-of-c1","summary":"Done."}` {
		t.Fatalf("the handoff write made %+v, the record %s at version %d of %s", e, body, version2, phase)
	}
	e, readback = made(write("result", "ACME-12", map[string]any{"check": "k1", "role": "implementer", "verdict": "pass", "commit": "c1", "run": "go test ./..."}))
	result, block, ok := readback.result(teardownEffectsString(e.Payload, "result_id"))
	if e.Type != "result.recorded" || !ok || block != "k1" || result["commit_sha"] != "sha-of-c1" || result["verdict"] != "pass" || result["run"] != "go test ./..." {
		t.Fatalf("the result made %+v, the record %v of check %s", e, result, block)
	}
	if e, _ := made(write("comment", "ACME-12", map[string]any{"body": "Look 1 is pending."})); e.Type != "comment.created" || e.Payload["body"] != "Look 1 is pending." {
		t.Fatalf("the comment made %+v", e)
	}
	if e, readback := made(write("ask", "ACME-12", map[string]any{"question": "Which?", "options": []any{"a", "b"}})); e.Type != "ask.opened" || len(readback.Asks) != 1 || readback.Asks[0].Question != "Which?" || string(readback.Asks[0].Options) != `["a","b"]` {
		t.Fatalf("the question made %+v, %+v", e, readback.Asks)
	}

	// A reply is a comment the run replay's writer does not make.
	e, readback, err := teardownEffectsTraceWrite("ACME-12", session, write("comment", "ACME-12", map[string]any{"body": "Yes.", "reply_to": "c-1"}), sha, version)
	if fault, _ := teardownEffectsDispatchFault(&readback, e); err != nil || fault == "" {
		t.Fatalf("a reply made %+v (%v), and the writer takes it", e, err)
	}
	for _, refused := range []teardownEffectsTraceAction{
		write("artifact", "ACME-12", map[string]any{"artifact": map[string]any{"name": "notes.md"}}),
		write("issue-update", "ACME-12", map[string]any{"status": "blocked"}),
		write("comment", "ACME-10", map[string]any{"body": "On the parent."}),
		write("handoff-write", "ACME-12", map[string]any{"phase": "implement"}),
	} {
		if _, _, err := teardownEffectsTraceWrite("ACME-12", session, refused, sha, version); err == nil {
			t.Fatalf("made the write %s on %s %v", refused.Command, refused.Target, refused.Fields)
		}
	}
}

// Each check holds on the replay of the trace action that shows it, and fails, saying what the
// replay showed, where the answer, the phase, the wake or the check_wake row is another.
func TestTeardownEffectsShows(t *testing.T) {
	t.Parallel()
	cases := teardownEffectsExampleCases(t, nil)
	p05, p10 := cases[0], cases[1]
	complete := func(status int, answer string) []teardownEffectsHTTP {
		return []teardownEffectsHTTP{{Method: "POST", Path: teardownEffectsComplete, Status: status, Answer: json.RawMessage(answer)}}
	}
	instant := func(text string) *time.Time {
		at, err := time.Parse(time.RFC3339, text)
		if err != nil {
			t.Fatal(err)
		}
		return &at
	}
	wake := func(at, due string) *teardownEffectsCheck {
		check := &teardownEffectsCheck{}
		if at != "" {
			check.WakeAt = instant(at)
		}
		if due != "" {
			check.CheckWakeRow = &teardownEffectsWakeRow{DueAt: *instant(due)}
		}
		return check
	}
	turn := func(before, after string, replies []teardownEffectsHTTP, client teardownEffectsAnswered, checks ...*teardownEffectsCheck) teardownEffectsTurned {
		turned := teardownEffectsTurned{call: teardownEffectsReplayCall{State: teardownEffectsState{Phase: before}, PhaseAfter: after}, replies: replies, client: client}
		turned.before, turned.after = checks[0], checks[1]
		return turned
	}
	ok := teardownEffectsAnswered{text: "{}"}
	refused := func(code string) teardownEffectsAnswered {
		return teardownEffectsAnswered{isError: true, text: "400 " + code + ": refused"}
	}
	wakeRequired := `{"code":"CHECK_WAKE_REQUIRED","error":"refused"}`
	noPullRequest := `{"code":"HANDOFF_NO_PULL_REQUEST","error":"refused"}`
	none := wake("", "")
	due := wake("2030-01-08T12:30:00Z", "2030-01-08T12:30:00Z")
	control := func(c teardownEffectsCase, trace string, seq int, check, action string) teardownEffectsControl {
		return teardownEffectsControl{Case: c.ID, Trace: trace, Seq: seq, Check: check, Action: action}
	}
	accepted := turn("implementing", "testing", complete(200, `{}`), ok, nil, nil)
	accepted.handoffs = []teardownEffectsHandoff{{Phase: "implement", Version: 1, ID: "h1"}}
	unmoved := accepted
	unmoved.call.PhaseAfter = "implementing"
	localOnly := teardownEffectsAnswered{isError: true, text: "the commit this workspace stands on, 2e74250b, is not on legion/ACME-12@origin"}
	for _, row := range []struct {
		name    string
		control teardownEffectsControl
		c       teardownEffectsCase
		turned  teardownEffectsTurned
		holds   bool
	}{
		{"a pending without wake_at refused, and nothing written", control(p10, "good", 2, teardownEffectsWakeRefused, ""), p10, turn("done", "done", complete(400, wakeRequired), refused("CHECK_WAKE_REQUIRED"), none, none), true},
		{"a refused pending that wrote a wake", control(p10, "good", 2, teardownEffectsWakeRefused, ""), p10, turn("done", "done", complete(400, wakeRequired), refused("CHECK_WAKE_REQUIRED"), none, due), false},
		{"a pending without wake_at taken", control(p10, "good", 2, teardownEffectsWakeRefused, ""), p10, turn("done", "done", complete(200, `{}`), ok, none, none), false},
		{"a pending with wake_at taken, its wake and row due then", control(p10, "good", 3, teardownEffectsWakeAccepted, ""), p10, turn("done", "done", complete(200, `{}`), ok, none, due), true},
		{"a taken pending with no check_wake row", control(p10, "good", 3, teardownEffectsWakeAccepted, ""), p10, turn("done", "done", complete(200, `{}`), ok, none, wake("2030-01-08T12:30:00Z", "")), false},
		{"a taken completion that moves the phase with its handoff", control(p05, "good", 5, teardownEffectsAccepted, ""), p05, accepted, true},
		{"a taken completion that leaves the phase", control(p05, "good", 5, teardownEffectsAccepted, ""), p05, unmoved, false},
		{"a local-only head refused by the tool", control(p05, "good", 5, teardownEffectsLocalOnly, ""), p05, turn("implementing", "implementing", nil, localOnly, nil, nil), true},
		{"a local-only head whose completion reached the daemon", control(p05, "good", 5, teardownEffectsLocalOnly, ""), p05, turn("implementing", "implementing", complete(409, noPullRequest), localOnly, nil, nil), false},
		{"a pushed head with no pull request refused", control(p05, "good", 5, teardownEffectsNoPullRequest, ""), p05, turn("implementing", "implementing", complete(409, noPullRequest), teardownEffectsAnswered{isError: true, text: "409 HANDOFF_NO_PULL_REQUEST: no open pull request"}, nil, nil), true},
		{"a pushed head with no pull request taken", control(p05, "good", 5, teardownEffectsNoPullRequest, ""), p05, accepted, false},
		{"a comment that leaves the check", control(p10, "bad", 1, teardownEffectsCommentNoWake, ""), p10, turn("done", "done", nil, teardownEffectsAnswered{}, none, none), true},
		{"a comment that wrote a wake", control(p10, "bad", 1, teardownEffectsCommentNoWake, ""), p10, turn("done", "done", nil, teardownEffectsAnswered{}, none, due), false},
		{"the case's answered effect: the phase it names", control(p05, "good", 5, teardownEffectsEffect, "complete"), p05, accepted, true},
		{"the case's answered effect: another phase", control(p05, "good", 5, teardownEffectsEffect, "complete"), p05, unmoved, false},
		{"the case's check-wake effect: a wake in its window", control(p10, "good", 3, teardownEffectsEffect, "report-pending"), p10, turn("done", "done", complete(200, `{}`), ok, none, due), true},
		{"the case's check-wake effect: a wake past its window", control(p10, "good", 3, teardownEffectsEffect, "report-pending"), p10, turn("done", "done", complete(200, `{}`), ok, none, wake("2030-01-10T12:00:00Z", "2030-01-10T12:00:00Z")), false},
		{"the case's check-wake effect: a row due at another time", control(p10, "good", 3, teardownEffectsEffect, "report-pending"), p10, turn("done", "done", complete(200, `{}`), ok, none, wake("2030-01-08T12:30:00Z", "2030-01-08T13:00:00Z")), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if err := teardownEffectsShows(row.control, row.c, row.turned); (err == nil) != row.holds {
				t.Fatalf("%s: %v, want it to hold: %t", row.control, err, row.holds)
			}
		})
	}
}

// --controls names each control it promised that did not execute, in the order it promised them.
func TestTeardownEffectsMissed(t *testing.T) {
	t.Parallel()
	promised, err := teardownEffectsPromised(teardownEffectsExampleCases(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	executed := map[teardownEffectsControl]bool{}
	for _, control := range promised {
		executed[control] = control.Case == "P10"
	}
	executed[teardownEffectsControl{Case: "P10", Trace: "good", Check: teardownEffectsReplayed}] = false
	want := []string{
		"P05 good trace: replayed",
		"P05 good trace, action 5: pushed-with-open-pull-request-accepted",
		"P05 good trace, action 5: the effect of complete",
		"P05 bad trace: replayed",
		"P05 good trace, action 5: local-only-implementer-refused",
		"P05 good trace, action 5: pushed-without-pull-request-refused",
		"P10 good trace: replayed",
	}
	if got := teardownEffectsMissed(promised, executed); !slices.Equal(got, want) {
		t.Fatalf("named %q, want %q", got, want)
	}
	if got := teardownEffectsMissed(promised, map[teardownEffectsControl]bool{}); len(got) != len(promised) {
		t.Fatalf("with none executed, named %d of %d", len(got), len(promised))
	}
}

// teardownEffectsControls is the evidence --controls wrote of T0's example in the proof Job (its
// four trace directories; testdata/teardown-effects/controls.command says how it was captured).
const teardownEffectsControls = "testdata/teardown-effects/controls"

// Each trace --controls promises to replay has the two records of a run's replay at
// teardownEffectsControlDir, in the Go driver's own encoding and under the trace's own id: the
// verdict ok, each daemon action of the trace joined to its call's answer in order, and each other
// action of the trace made before a call or after the last one, but a staging action and a GitHub
// read, which change nothing. Each GitHub action of the example records its request and no change,
// and is read as the change gh-standin keeps of that request (teardownEffectsTraceChange): P05's
// gh pr create opens a pull request, and P10's gh pr edit edits #77. heldout-reader.test.ts reads
// the same records as the controls of a lane D batch.
func TestTeardownEffectsControlRecords(t *testing.T) {
	t.Parallel()
	cases := teardownEffectsExampleCases(t, nil)
	promised, err := teardownEffectsPromised(cases)
	if err != nil {
		t.Fatal(err)
	}
	replayed := 0
	github := map[string]string{}
	for _, control := range promised {
		if control.Check != teardownEffectsReplayed {
			continue
		}
		replayed++
		c := cases[slices.IndexFunc(cases, func(each teardownEffectsCase) bool { return each.ID == control.Case })]
		trace := c.trace(control.Trace)
		dir := teardownEffectsControlDir(teardownEffectsControls, control.Case, control.Trace)
		var receipt teardownEffectsReplayRecord
		var effects teardownEffectsRecord
		for name, into := range map[string]any{"replay.json": &receipt, "effects.json": &effects} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("%s: %v", control, err)
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(into); err != nil {
				t.Fatalf("%s/%s: %v", dir, name, err)
			}
			encoded, err := json.MarshalIndent(into, "", "  ")
			if err != nil || string(append(encoded, '\n')) != string(data) {
				t.Fatalf("%s/%s is not the driver's own encoding of its record: %v", dir, name, err)
			}
		}
		if receipt.ID != trace.ID || effects.ID != trace.ID || receipt.Verdict != "ok" || len(effects.Diverged) != 0 || len(receipt.After) != 0 || len(receipt.NotReplayed) != 0 {
			t.Fatalf("%s: replay.json of %s (%s) and effects.json of %s, with %d differences, for the trace %s", control, receipt.ID, receipt.Verdict, effects.ID, len(effects.Diverged), trace.ID)
		}
		var joined, made, daemon, changes []int
		for _, call := range receipt.Calls {
			for _, ref := range call.Standin {
				joined = append(joined, ref.OutcomeSeq)
			}
			for _, change := range call.Before {
				made = append(made, change.OutcomeSeq)
			}
		}
		for _, change := range receipt.End {
			made = append(made, change.OutcomeSeq)
		}
		for _, a := range trace.Actions {
			switch {
			case a.Kind == "daemon":
				daemon = append(daemon, a.Seq)
			case a.Kind == "staging", a.Kind == "github" && teardownEffectsTraceRead(a):
			default:
				changes = append(changes, a.Seq)
			}
			if a.Kind != "github" {
				continue
			}
			change, err := teardownEffectsTraceChange(a)
			if err != nil || change == nil || a.Change != nil {
				t.Fatalf("%s: action %d reads as the change %+v (%v), and the example records a request the replay makes and no change", control, a.Seq, change, err)
			}
			github[fmt.Sprintf("%s %s %d", control.Case, control.Trace, a.Seq)] = fmt.Sprintf("%s %v", change.Action, change.Numbers)
		}
		if !slices.Equal(joined, daemon) || !slices.Equal(made, changes) {
			t.Fatalf("%s: the calls answer the actions %v and make the changes %v, and the trace holds the daemon actions %v and the changes %v", control, joined, made, daemon, changes)
		}
	}
	if replayed != 4 {
		t.Fatalf("--controls promises %d trace replays of the example, and the evidence holds 4", replayed)
	}
	if want := map[string]string{"P05 good 2": "open []", "P05 bad 4": "open []", "P10 good 1": "edit [77]"}; !maps.Equal(github, want) {
		t.Fatalf("the example's GitHub actions read as %v, want %v", github, want)
	}
}
