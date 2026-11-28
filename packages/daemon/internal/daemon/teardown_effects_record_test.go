package daemon

//ci:reads packages/pi-legion/scripts/skill-scenarios/testdata/f2/sessions/p10-production-check.jsonl TestTeardownEffectsTranscriptF2 reads fixture F2 through ../../../pi-legion
//ci:reads packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json TestTeardownEffectsPromisedControls reads T0's example cases through ../../../pi-legion

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
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
// the calls it did not replay, which follow the first call the real daemon answered otherwise. A
// change the replay made as a refused write (a trace's request-approval) carries its refusal
// (`refused`), which effects.json does not hold. Every list is an array, never null
// (teardownEffectsNewReplayRecord).
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
		if len(bytes.TrimSpace(line)) != 0 {
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

// teardownEffectsTrace is a case's known-good or known-bad trace: an outcome record. Documents is
// each document of the case's issue the trace wrote, with the body its recorded edits leave
// (heldout-case.ts OutcomeRecord's `documents`): nil where the trace has no `documents` key, which
// states nothing, and empty where it states that it wrote no document.
type teardownEffectsTrace struct {
	ID        string
	Actions   []teardownEffectsTraceAction
	Documents []teardownEffectsDocument
}

// action is the trace's action at seq: the last of them where more than one has it, and the zero
// action where none has.
func (trace teardownEffectsTrace) action(seq int) teardownEffectsTraceAction {
	var found teardownEffectsTraceAction
	for _, a := range trace.Actions {
		if a.Seq == seq {
			found = a
		}
	}
	return found
}

// teardownEffectsDocument is one entry of a trace's `documents`: a document's file name, the last
// version the trace wrote and that version's text.
type teardownEffectsDocument struct {
	Name    string
	Version int
	Body    string
}

// teardownEffectsWorldIDs is text with each id of the replay's Dispatch that ids names written as
// the world's id (ids: the replay's id to the world's), the other way round of the mapping the
// replay names the world's ids by (teardownEffectsRun.ids), as outcome.ts writes a run's scratch
// ids back as the world's (seed.ts's dispatch-fixture.json `ids`).
func teardownEffectsWorldIDs(text string, ids map[string]string) string {
	for _, given := range slices.Sorted(maps.Keys(ids)) {
		text = strings.ReplaceAll(text, given, ids[given])
	}
	return text
}

// teardownEffectsDocumentFaults names each document a trace states that the replay's Dispatch does
// not hold as stated, after the trace's last write, and each document of the case's issue the
// replay wrote that the trace does not state. held is each document of the issue by its file name,
// with its latest version and that version's text, which is compared with each of the replay's ids
// written as the world's (teardownEffectsWorldIDs). written is the file name of each document of
// the issue the replay wrote. A stated document the replay does not hold, another version, another
// text (named by its first line that differs) and a written document the trace does not state are
// faults, so a trace that states no document fails where the replay wrote one.
func teardownEffectsDocumentFaults(stated []teardownEffectsDocument, held map[string]teardownEffectsDocument, written []string, ids map[string]string) []string {
	var faults []string
	for _, want := range stated {
		got, ok := held[want.Name]
		body := teardownEffectsWorldIDs(got.Body, ids)
		switch {
		case !ok:
			faults = append(faults, fmt.Sprintf("the trace states %s at version %d, and the replay's Dispatch holds no document of that name", want.Name, want.Version))
		case got.Version != want.Version:
			faults = append(faults, fmt.Sprintf("the trace states %s at version %d, and the replay's Dispatch holds version %d", want.Name, want.Version, got.Version))
		case body != want.Body:
			faults = append(faults, fmt.Sprintf("%s version %d: %s", want.Name, want.Version, teardownEffectsFirstDifference(want.Body, body)))
		}
	}
	for _, name := range written {
		if !slices.ContainsFunc(stated, func(want teardownEffectsDocument) bool { return want.Name == name }) {
			faults = append(faults, fmt.Sprintf("the replay wrote %s, and the trace states no such document", name))
		}
	}
	return faults
}

// teardownEffectsFirstDifference names the first line at which the body a trace states (want) and
// the body the replay's Dispatch holds (got) differ, each line quoted, "no line" past a body's end.
func teardownEffectsFirstDifference(want, got string) string {
	a, b := strings.Split(want, "\n"), strings.Split(got, "\n")
	line := func(lines []string, i int) string {
		if i < len(lines) {
			return strconv.Quote(lines[i])
		}
		return "no line"
	}
	for i := range max(len(a), len(b)) {
		if i >= len(a) || i >= len(b) || a[i] != b[i] {
			return fmt.Sprintf("line %d is %s in the body the trace states, and %s in the body the replay's Dispatch holds", i+1, line(a, i), line(b, i))
		}
	}
	return "the bodies are the same"
}

// teardownEffectsTraceAction is one scored action of a trace (heldout-case.ts OutcomeAction).
type teardownEffectsTraceAction struct {
	Kind, Op, Command, Method, Path, Target, Ref, Commit string
	// Tool, Name, Agent, EndedAt, Revision, Source and Paths are the fields of a carried kind alone
	// (teardownEffectsCarriedKinds): an Envoy message's tool, a task agent's name, agent and end, a
	// probe's revision, and a diff read's checkout root and paths, as the case file holds each.
	Tool, Name, Agent, EndedAt, Revision, Source string
	Paths                                        []string
	Seq                                          int
	At                                           time.Time
	Args, Fields                                 map[string]any
	Answer                                       struct {
		Code *string
		Body any
	}
	// Change is a GitHub action's change as gh-standin kept it, where outcome.ts recorded one
	// (teardownEffectsTraceChange); a frozen trace holds none.
	Change *teardownEffectsPullMutation
}

// teardownEffectsWorld is a case's world (heldout-case.ts World) as lane D seeds it, each typed part
// of it (teardownEffectsDaemonParts, teardownEffectsDispatchIssue, teardownEffectsStepRun, the GitHub
// parts) among them. Its decoding (teardown_effects_world_test.go) names in unserved each member of a
// typed part's container that lane D has no seeding for, which start refuses as a setup failure.
type teardownEffectsWorld struct {
	Clock    time.Time
	Dispatch struct {
		Issue struct {
			Key, Title, Status, Spec, Parent string
			// Labels is the issue's labels where the world states them, nil where it states none.
			Labels *[]string
		}
		Documents []struct{ Name, Body string }
		Handoffs  []struct {
			Phase, Author string
			Version       int
			Body          json.RawMessage
		}
		// Comments and Asks are the issue's comments and questions, each named by the world's id,
		// and Results each result recorded against a check of its Verification document.
		Comments []struct {
			ID, Author, Body string
			At               time.Time
		}
		Asks []struct {
			ID, Author, Question, State string
			At                          time.Time
			Options                     []string
			Answer                      *string
		}
		Results []struct {
			Check, Role, Verdict, Commit string
			At                           time.Time
		}
		Issues []teardownEffectsDispatchIssue
		Steps  []teardownEffectsStepRun
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
		Settings     map[string]json.RawMessage
		BranchRules  *[]struct {
			Branch string
			Rules  []json.RawMessage
		}
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
		teardownEffectsDaemonParts
	}
	unserved []string
}

var (
	// teardownEffectsTypedOpener is the opening line of a typed block: at most three spaces, three
	// colons or more, the block's name, `{`, the attributes, and the `}` that ends the line (pmdoc's
	// ParseTypedDirectiveOpen).
	teardownEffectsTypedOpener = regexp.MustCompile(`^( {0,3}:{3,}([A-Za-z0-9_-]+)\{)(.*)\}$`)
	// teardownEffectsCodeFence is the opening line of a fenced code block: at most three spaces,
	// then three backticks or more, or three tildes or more.
	teardownEffectsCodeFence = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	// teardownEffectsAttribute is, in a typed block's attributes, a quoted value, which is skipped
	// whole, or an attribute with a value: the whitespace before it, its name and its value. A
	// quoted value holds no `"`, so no text in the value of an attribute is read as another one.
	teardownEffectsAttribute = regexp.MustCompile(`"[^"]*"|(^|\s)([A-Za-z0-9_-]+)="([^"]*)"`)
	// teardownEffectsBareID is a block id that a pointer names alone (`r1`), in the grammar of the
	// ids seed.ts dispatchBlockRefs writes in a reference. No reference percent-encodes one.
	teardownEffectsBareID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	// teardownEffectsSlugGap is a run of the characters that a document slug does not keep.
	teardownEffectsSlugGap = regexp.MustCompile(`[^a-z0-9]+`)
)

// teardownEffectsBarePointers is body, the text of the case's document name, as the replay writes
// it on the issue key: each bare id of a pointer of a check or a plan task (`serves="r1"`,
// `needs="t1"`) is written as the reference Dispatch writes to that block, as lane R's seed writes
// it (seed.ts dispatchBlockRefs). Dispatch refuses any other pointer (400 INVALID_MARKDOWN). A
// `serves` points at a requirement of key's spec (specBlock). A `needs` points only at a block of
// its own document, under the slug Dispatch gives a new document of name (artifactSlug in
// packages/envoy/internal/dispatch/api/artifacts.go). Only an opening line outside fenced code is
// read, and in it only those pointers. Every other entry, attribute and line is left as it is.
func teardownEffectsBarePointers(key, name, body string) string {
	slug := strings.Trim(teardownEffectsSlugGap.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		slug = "artifact"
	}
	spec := func(id string) string { return specBlock(key, id) }
	own := func(id string) string { return "dispatch://" + key + "/artifact/" + slug + "#b-" + id }
	pointers := map[string]map[string]func(string) string{
		"check":     {"serves": spec},
		"plan_task": {"serves": spec, "needs": own},
	}
	lines := strings.Split(body, "\n")
	fence := ""
	for i, line := range lines {
		if fence != "" {
			rest := strings.TrimRight(line, " \t")
			run := strings.TrimLeft(rest, " ")
			if len(rest)-len(run) <= 3 && len(run) >= len(fence) && strings.Trim(run, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		if opened := teardownEffectsCodeFence.FindStringSubmatch(line); opened != nil {
			fence = opened[1]
			continue
		}
		opener := teardownEffectsTypedOpener.FindStringSubmatch(line)
		if opener == nil || pointers[opener[2]] == nil {
			continue
		}
		attributes := teardownEffectsAttribute.ReplaceAllStringFunc(opener[3], func(written string) string {
			attribute := teardownEffectsAttribute.FindStringSubmatch(written)
			target, ok := pointers[opener[2]][attribute[2]]
			if !ok {
				return written
			}
			entries := strings.Split(attribute[3], " ")
			for j, entry := range entries {
				if teardownEffectsBareID.MatchString(entry) {
					entries[j] = target(entry)
				}
			}
			return attribute[1] + attribute[2] + `="` + strings.Join(entries, " ") + `"`
		})
		lines[i] = opener[1] + attributes + "}"
	}
	return strings.Join(lines, "\n")
}

// teardownEffectsPull is one pull request of the scratch GitHub, as the case's world gives it
// (heldout-case.ts PullRequest): with, where the world states them, GraphQL's merge state of it
// (MergeStateStatus, `CLEAN`), its auto-merge request (AutoMergeRequest, JSON null for none), its
// node id (NodeID), its diff and what a merge of it answers (MergeResult); and, once a change armed
// it, its auto-merge and its place in the merge queue (teardownEffectsScratch.change), which no
// world records.
type teardownEffectsPull struct {
	Number                                int
	State, Base, Title, Body, MergeCommit string
	Draft                                 bool
	Head                                  struct{ Ref, SHA string }
	Reviews                               []teardownEffectsReview
	MergeStateStatus, NodeID, Diff        string
	AutoMergeRequest, MergeResult         json.RawMessage
	AutoMerge, Queued                     bool `json:"-"`
}

// teardownEffectsReview is one review of a pull request: the world's, or one a change added.
type teardownEffectsReview struct {
	Author, State, Commit, Body string
	At                          time.Time
}

func teardownEffectsParams(op string, args map[string]any) map[string]any {
	params := make(map[string]any, len(args)+1)
	maps.Copy(params, args)
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
// run's replay writes, replay.json and effects.json: <out>/<case id>/<good or bad>. out is the
// evidence directory for a case of the case file (--cases), and its supplemental directory
// (teardownEffectsSupplementalDir) for a case of the supplemental file (--supplemental). The
// heldout-reader.ts function controlAt reads the case file's records at the evidence directory.
func teardownEffectsControlDir(out, caseID, trace string) string {
	return filepath.Join(out, caseID, trace)
}

// teardownEffectsSupplementalDir is the directory of the evidence directory that holds the records
// of the supplemental file's cases and the answers their checks capture. A supplemental case is a
// control, not a case: heldout-reader.ts reads only the case file's ids, at the evidence directory
// itself, and a held-out stand-in overlay reads only the evidence directory's daemon-answers.json.
const teardownEffectsSupplementalDir = "supplemental"

// teardownEffectsCapture is a daemon-answers.json: the answers the checks of one case file capture
// (fixture row F3), with the command, the time and the commit that captured them.
type teardownEffectsCapture struct {
	Version  int `json:"version"`
	Captured struct {
		Command string    `json:"command"`
		At      time.Time `json:"at"`
		Commit  string    `json:"commit"`
	} `json:"captured"`
	Answers []teardownEffectsFixtureAnswer `json:"answers"`
}
type teardownEffectsFixtureAnswer struct {
	Name   string          `json:"name"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Match  map[string]any  `json:"match"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// take adds to the captured answers the answer control's check shows, with the request fields
// daemon-standin.ts matches it by: the check report's verdict, wake and look, or the completion's
// commit and handoff record. A check that shows no answer of the daemon adds none.
func (capture *teardownEffectsCapture) take(control teardownEffectsControl, c teardownEffectsCase, turned teardownEffectsTurned) {
	a := c.trace(control.Trace).action(control.Seq)
	found := teardownEffectsCompletions(turned.replies)
	if len(found) != 1 {
		return
	}
	var request struct {
		Commit    string `json:"commit"`
		HandoffID string `json:"handoffId"`
	}
	_ = json.Unmarshal(found[0].Body, &request)
	switch control.Check {
	case teardownEffectsWakeRefused:
		capture.add("check-wake-required", found[0], map[string]any{"verdict": "pending", "wakeAt": nil, "checkOpenedAt": a.Args["check_opened_at"]})
	case teardownEffectsWakeAccepted:
		capture.add("pending-accepted", found[0], map[string]any{"verdict": "pending", "wakeAt": a.Args["wake_at"], "checkOpenedAt": a.Args["check_opened_at"]})
	case teardownEffectsAccepted:
		capture.add("implementer-accepted", found[0], map[string]any{"commit": request.Commit, "handoffId": request.HandoffID})
	case teardownEffectsNoPullRequest:
		capture.add("implementer-no-pull-request", found[0], map[string]any{"commit": request.Commit, "handoffId": request.HandoffID})
	}
}
func (capture *teardownEffectsCapture) add(name string, res teardownEffectsHTTP, match map[string]any) {
	capture.Answers = append(capture.Answers, teardownEffectsFixtureAnswer{Name: name, Path: res.Path, Method: res.Method, Match: match, Status: res.Status, Body: res.Answer})
}

// teardownEffectsControlRun is what one --controls run makes controls of: the cases of its case
// file, then those of its supplemental file where it is given one; every control
// teardownEffectsPromised promises of both together, so the coverage check counts both files; and
// the answers the checks capture, each case file's in a daemon-answers.json of its own (answers,
// by its path).
type teardownEffectsControlRun struct {
	out          string
	cases        []teardownEffectsCase
	supplemental map[string]bool
	promised     []teardownEffectsControl
	answers      map[string]*teardownEffectsCapture
}

// teardownEffectsNewControlRun is the run, with the evidence directory out, of the case file's
// cases and the supplemental file's (none where it is given none). It returns, with the run, the
// error of teardownEffectsPromised on both files' cases together.
func teardownEffectsNewControlRun(out string, cases, supplemental []teardownEffectsCase) (*teardownEffectsControlRun, error) {
	run := &teardownEffectsControlRun{out: out, cases: slices.Concat(cases, supplemental), supplemental: map[string]bool{}, answers: map[string]*teardownEffectsCapture{}}
	for _, c := range supplemental {
		run.supplemental[c.ID] = true
	}
	for _, c := range run.cases {
		if run.answers[run.answersFile(c.ID)] == nil {
			run.answers[run.answersFile(c.ID)] = &teardownEffectsCapture{Version: 1}
		}
	}
	promised, err := teardownEffectsPromised(run.cases)
	run.promised = promised
	return run, err
}

// dir is the directory below which the run writes the records of case id: the evidence directory,
// or its supplemental directory for a supplemental case.
func (run *teardownEffectsControlRun) dir(id string) string {
	if run.supplemental[id] {
		return filepath.Join(run.out, teardownEffectsSupplementalDir)
	}
	return run.out
}

// answersFile is the daemon-answers.json that holds the answers the checks of case id capture.
func (run *teardownEffectsControlRun) answersFile(id string) string {
	return filepath.Join(run.dir(id), "daemon-answers.json")
}

// capture is where the answers the checks of case id capture go.
func (run *teardownEffectsControlRun) capture(id string) *teardownEffectsCapture {
	return run.answers[run.answersFile(id)]
}

// named is control as the run's report names it: a control of a supplemental case says so.
func (run *teardownEffectsControlRun) named(control teardownEffectsControl) string {
	if run.supplemental[control.Case] {
		return teardownEffectsSupplementalDir + " " + control.String()
	}
	return control.String()
}

// teardownEffectsSupplemental reads the supplemental case file at path (--supplemental), whose
// cases a --controls run makes controls of beside the cases of its case file (cases). It answers
// the file's path in the checkout root and its cases. commit is the commit the run records
// (daemon-answers.json's captured.commit). It refuses, before any replay and naming the cause, a
// file that is not public, a file that does not parse or holds no case, a case id that both files
// hold, and a case of the case file whose id is teardownEffectsSupplementalDir, whose records
// would lie among the supplemental ones.
//
// A file is public where commit holds it at its path in root, with the bytes the run reads. So a
// file outside root (a copy under $TMPDIR among them), a file no commit holds and a file changed
// since commit are not. The repository is public, so anyone can read what its commit holds.
func teardownEffectsSupplemental(root, commit, path string, cases []teardownEffectsCase) (string, []teardownEffectsCase, error) {
	file, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}
	checkout, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, err
	}
	rel, err := filepath.Rel(checkout, file)
	if err != nil || !filepath.IsLocal(rel) {
		return "", nil, fmt.Errorf("%s is not public: it is no file of the checkout %s, so no commit of the repository holds it", path, root)
	}
	name := filepath.ToSlash(rel)
	if commit == "" {
		return "", nil, fmt.Errorf("%s is not public: the run names no commit that holds it", name)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", nil, err
	}
	committed, err := exec.Command("git", "-C", checkout, "cat-file", "blob", commit+":"+name).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			err = errors.New(string(bytes.TrimSpace(exit.Stderr)))
		}
		return "", nil, fmt.Errorf("%s is not public: the commit %s holds no such file (%v)", name, commit, err)
	}
	if !bytes.Equal(committed, data) {
		return "", nil, fmt.Errorf("%s is not public: it differs from the file the commit %s holds", name, commit)
	}
	var supplemental struct{ Cases []teardownEffectsCase }
	if err := json.Unmarshal(data, &supplemental); err != nil {
		return "", nil, fmt.Errorf("%s does not parse: %w", name, err)
	}
	if len(supplemental.Cases) == 0 {
		return "", nil, fmt.Errorf("%s holds no case", name)
	}
	var problems []string
	for _, c := range supplemental.Cases {
		if slices.ContainsFunc(cases, func(each teardownEffectsCase) bool { return each.ID == c.ID }) {
			problems = append(problems, fmt.Sprintf("the case file and %s both hold the case %s", name, c.ID))
		}
	}
	if slices.ContainsFunc(cases, func(each teardownEffectsCase) bool { return each.ID == teardownEffectsSupplementalDir }) {
		problems = append(problems, fmt.Sprintf("the case file holds a case %s, and the records of %s go to the directory of that name", teardownEffectsSupplementalDir, name))
	}
	if len(problems) > 0 {
		return "", nil, errors.New(strings.Join(problems, "; "))
	}
	return name, supplemental.Cases, nil
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
	case "envoy":
		fields["fields"] = a.Fields
		set("tool", a.Tool)
		set("target", a.Target)
	case "dispatch-read":
		fields["fields"] = a.Fields
		set("command", a.Command)
		set("target", a.Target)
	case "agent":
		fields["fields"] = a.Fields
		set("agent", a.Agent)
		set("name", a.Name)
		set("endedAt", a.EndedAt)
	case "probe":
		set("target", a.Target)
		set("revision", a.Revision)
	case "diff-read":
		paths := make([]any, len(a.Paths))
		for i, p := range a.Paths {
			paths[i] = p
		}
		fields["paths"] = paths
		set("command", a.Command)
		set("source", a.Source)
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
// that version gives its phase; a result's commit is mapped too. It makes a handoff, a result and
// a question, each on the issue key alone; any other command is an error. Every other Dispatch
// write of a trace goes through the real dispatch CLI, as the role makes it
// (teardownEffectsTraceCommand).
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
	case "ask":
		fields["id"] = id
		event.Type, event.Payload = "ask.opened", fields
		records = map[string]any{"asks": []any{map[string]any{"id": id, "question": fields["question"], "options": fields["options"]}}}
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

// teardownEffectsApprovalCommand is the command of a trace's Dispatch write that asks a person to
// approve a document, `dispatch request-approval`. The replay makes it through the real dispatch
// CLI as the role's session (teardownEffectsTraceCommand), and takes Dispatch's acceptance of it
// as a made write and two refusals of it as refused writes (teardownEffectsApprovalRefusal).
const teardownEffectsApprovalCommand = "request-approval"

// The causes of a refused request-approval (teardownEffectsRefusal).
const (
	// teardownEffectsMissingArtifact: the issue holds no document the request names, so the CLI's
	// own resolver refuses the request before it sends it (resolveArtifact,
	// packages/envoy-client/src/dispatch-commands/document-targets.ts), and Dispatch answers nothing.
	teardownEffectsMissingArtifact = "missing-artifact"
	// teardownEffectsPlanApproval: the document is a plan, and Dispatch refuses the request 409
	// PLAN_APPROVED_BY_REVIEW (requestArtifactApproval, packages/envoy/internal/dispatch/api/reviews.go).
	teardownEffectsPlanApproval = "plan-approval"
)

// teardownEffectsRefusal is the refusal of a trace's request-approval that the replay made again,
// which changed nothing: its cause; the status and the code of Dispatch's answer, null where the
// CLI refused the request before it sent it; and what the CLI printed. replay.json records it on
// the change (`refused`). effects.json, which the reader gives the judge, holds none of it.
type teardownEffectsRefusal struct {
	Cause      string  `json:"cause"`
	Status     *int    `json:"status"`
	Code       *string `json:"code"`
	Diagnostic string  `json:"diagnostic"`
}

// teardownEffectsApprovalRoute is the path of Dispatch's approval request route, which
// `dispatch request-approval` posts to: POST /api/v1/artifacts/{id}/approval-requests.
var teardownEffectsApprovalRoute = regexp.MustCompile(`^/api/v1/artifacts/[^/]+/approval-requests$`)

// teardownEffectsApprovalRefusal is what the real dispatch CLI made of the request-approval c, a
// trace's request of the document c names (`artifact`, the issue's primary document where it
// names none) on its issue: exit is the CLI's exit code, printed is what it printed, asked is the
// number of requests it sent Dispatch, and sent is each of those requests that came back, with
// Dispatch's answer as the recording proxy keeps it (teardownEffectsStartProxy), in order. Where
// Dispatch took the approval request and the CLI exited 0, the request is a made write, and it
// answers no refusal. It takes two refusals besides, and neither changes anything:
//   - plan-approval: the CLI sent one approval request, Dispatch refused it 409
//     PLAN_APPROVED_BY_REVIEW, and the CLI exited 1;
//   - missing-artifact: the CLI sent no approval request and exited 1, and Dispatch's answer to
//     the CLI's last read of the issue lists no document by that reference, as the CLI's resolver
//     matches one: `spec` (or no reference) the issue's primary document, any other reference an
//     id, a slug or a filename.
//
// Every other outcome is an error, which makes the replay HARNESS, so it is never a passing
// control: an acceptance the CLI exited otherwise than 0 on; another write of the CLI's that
// Dispatch took; a request that got no answer, or an answer of 500 or more (a transport or server
// failure); any other refusal; a read of the issue that is no JSON; and a refusal the CLI printed
// nothing of.
func teardownEffectsApprovalRefusal(c teardownEffectsCommand, exit int, printed string, asked int, sent []teardownEffectsHTTP) (*teardownEffectsRefusal, error) {
	key, reference := c.Issue, c.flag("artifact")
	if reference == "" {
		reference = "spec"
	}
	if asked != len(sent) {
		return nil, fmt.Errorf("the CLI sent Dispatch %d requests, and %d came back: a transport failure, not a refusal", asked, len(sent))
	}
	var requests []teardownEffectsHTTP
	var issue *teardownEffectsHTTP
	for i, x := range sent {
		switch {
		case x.Status >= 500:
			return nil, fmt.Errorf("Dispatch answered %s %s: a server failure, not a refusal", x.route(), teardownEffectsStatusText(x.status()))
		case x.Method == "POST" && teardownEffectsApprovalRoute.MatchString(x.Path):
			requests = append(requests, x)
		case x.Status >= 400:
			return nil, fmt.Errorf("Dispatch refused the CLI's %s %s, which is no approval request: a refusal of another cause", x.route(), teardownEffectsStatusText(x.status()))
		case x.Method != "GET":
			return nil, fmt.Errorf("Dispatch took the CLI's %s (%d), a change that a request-approval does not make", x.route(), x.Status)
		case x.Path == "/api/v1/issues/"+key:
			issue = &sent[i]
		}
	}
	refusal := teardownEffectsRefusal{Diagnostic: strings.TrimSpace(printed)}
	switch len(requests) {
	case 0:
		if issue == nil {
			return nil, fmt.Errorf("the CLI sent no approval request, and Dispatch answered no read of %s that it made, so no missing document is shown", key)
		}
		var read struct {
			Artifacts []struct {
				ID, Slug, Name string
				Primary        bool
			}
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}
		var text string
		if json.Unmarshal(issue.Answer, &text) == nil {
			return nil, fmt.Errorf("Dispatch's answer to the CLI's read of %s is no JSON (the recording proxy keeps such an answer as a string)", key)
		}
		if err := json.Unmarshal(issue.Answer, &read); err != nil {
			return nil, fmt.Errorf("Dispatch's answer to the CLI's read of %s: %w", key, err)
		}
		for _, d := range read.Artifacts {
			named := reference == d.ID || reference == d.Slug || reference == d.Name
			if reference == "spec" {
				named = d.Primary || read.PrimaryArtifactID != "" && d.ID == read.PrimaryArtifactID
			}
			if named {
				return nil, fmt.Errorf("the CLI sent no approval request, and %s holds the document %s (%s) that the request names: a refusal of another cause than a missing document", key, d.Slug, d.Name)
			}
		}
		refusal.Cause = teardownEffectsMissingArtifact
	case 1:
		x := requests[0]
		code := teardownEffectsOutcomeCode(x.status())
		if x.Status < 400 {
			if exit != 0 {
				return nil, fmt.Errorf("Dispatch took the approval request (%s %d), and the CLI exited %d", x.route(), x.Status, exit)
			}
			return nil, nil
		}
		if x.Status != 409 || code != "PLAN_APPROVED_BY_REVIEW" {
			return nil, fmt.Errorf("Dispatch refused the approval request %s, which is not its refusal of a plan (409 PLAN_APPROVED_BY_REVIEW)", teardownEffectsStatusText(x.status()))
		}
		refusal.Cause, refusal.Status, refusal.Code = teardownEffectsPlanApproval, &x.Status, &code
	default:
		return nil, fmt.Errorf("the CLI sent %d approval requests, and one request-approval sends one", len(requests))
	}
	if exit != 1 {
		return nil, fmt.Errorf("the CLI exited %d, and it exits 1 on a refusal (%s)", exit, refusal.Cause)
	}
	if refusal.Diagnostic == "" {
		return nil, fmt.Errorf("the CLI printed nothing of its refusal (%s)", refusal.Cause)
	}
	return &refusal, nil
}

// The forms a flag of a dispatch command takes (teardownEffectsFlag), as the CLI derives them from
// each field's kind (packages/envoy-client/src/dispatch-command.ts): `--<name> <value>`, a text
// read from a file (`--<name>-file <path>`), JSON read from a file (`--<name>-json-file <path>`),
// and a boolean, `--<name>` alone, whose name says which value it sends (`force`, `no-force`).
const (
	teardownEffectsPlain = iota
	teardownEffectsText
	teardownEffectsJSON
	teardownEffectsSet
)

// teardownEffectsFlag is one flag of a dispatch command: its name, its value and its form.
type teardownEffectsFlag struct {
	Name, Value string
	Form        int
}

// teardownEffectsCommand is a Dispatch write as the role's `dispatch` CLI makes it: the command,
// its flags in order, the issue it writes on, the version of the document the write must make (0
// where none is stated), and the case's own name for the comment it makes ("" for none), which
// joins the replay's id map once Dispatch gives the comment its id.
type teardownEffectsCommand struct {
	Command, Issue, Name string
	Flags                []teardownEffectsFlag
	Version              int
}

// flag is the value of c's flag name, "" where c has none.
func (c teardownEffectsCommand) flag(name string) string {
	for _, f := range c.Flags {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

// argv is the command and its arguments, each text or JSON value written by file, which answers the
// path it wrote the flag's value to.
func (c teardownEffectsCommand) argv(file func(flag, value string) string) []string {
	args := []string{c.Command}
	for _, f := range c.Flags {
		switch f.Form {
		case teardownEffectsText:
			args = append(args, "--"+f.Name+"-file", file(f.Name, f.Value))
		case teardownEffectsJSON:
			args = append(args, "--"+f.Name+"-json-file", file(f.Name, f.Value))
		case teardownEffectsSet:
			args = append(args, "--"+f.Name)
		default:
			args = append(args, "--"+f.Name, f.Value)
		}
	}
	return args
}

// String is the command as a failure names it, each value read from a file named by its flag.
func (c teardownEffectsCommand) String() string {
	return "dispatch " + strings.Join(c.argv(func(flag, _ string) string { return "<" + flag + ">" }), " ")
}

// teardownEffectsCommandFields is each field a trace's Dispatch write may name, by its command, in
// the order the command's flags are given. Each is the CLI's own field of that command, but where
// the trace names it as Dispatch's event does: a comment's `ask_id` is `--reply-to-ask`, and its
// `id` is the case's own name for it, which the CLI is not given. An upload's and an edit's
// `version` is the version the write makes, which the answer must name and which is not sent.
var teardownEffectsCommandFields = map[string][]string{
	"comment":                      {"body", "reply_to", "ask_id", "turn", "id"},
	"message":                      {"body"},
	"issue-update":                 {"status", "before", "after"},
	"artifact":                     {"name", "content", "summary", "force", "version"},
	"doc-edit":                     {"artifact", "ops", "summary", "version"},
	"plan-approve":                 {"artifact", "version"},
	teardownEffectsApprovalCommand: {"artifact", "summary", "include_verification"},
}

// teardownEffectsDocumentRef is a reference to a document of an issue, which a doc-edit can target
// instead of an issue: dispatch://ACME-118/spec, or dispatch://ACME-118/artifact/<slug>. Its first
// group is the issue and its second the document (`spec`, or `artifact/` and the slug).
var teardownEffectsDocumentRef = regexp.MustCompile(`^dispatch://([A-Z][A-Z0-9]*-\d+)/(spec|artifact/[A-Za-z0-9._-]+)$`)

// teardownEffectsTraceCommand is the trace's Dispatch write a as the role's `dispatch` CLI makes it
// (teardownEffectsCommand), on the case's issue key or on another issue of held, the issues the
// case's world seeds; a doc-edit targets a document of one of them by its reference too. A field
// that names a record by the case's own name is given the id the replay's Dispatch gave it (ids,
// by `comment/<name>`, `ask/<name>` and `artifact/<name>`): a reply's comment, an answer's
// question, a document. Each text the write writes is mapped too: each reference to a comment or a
// question of key by its case name and each document id in a body (teardownEffectsCaseRefs), and,
// in the text of a document (an upload's content, an edit's markdown), each bare `serves` or
// `needs` pointer is first written as the importer writes it in a starting document
// (teardownEffectsBarePointers). Anything else is an error, which makes the replay HARNESS, and
// names the field: a target the world does not hold, a field the command takes no flag of, a name
// the replay's Dispatch holds no record of, and a value of another kind than its flag's.
func teardownEffectsTraceCommand(key string, held []string, a teardownEffectsTraceAction, ids map[string]string) (teardownEffectsCommand, error) {
	where := fmt.Sprintf("the trace's %s at action %d", a.Command, a.Seq)
	fields, ok := teardownEffectsCommandFields[a.Command]
	if !ok {
		return teardownEffectsCommand{}, fmt.Errorf("the replay makes no Dispatch write of the command %s (the trace's action %d)", a.Command, a.Seq)
	}
	for _, field := range slices.Sorted(maps.Keys(a.Fields)) {
		if !slices.Contains(fields, field) {
			return teardownEffectsCommand{}, fmt.Errorf("%s names the field %s, of which dispatch %s takes no flag", where, field, a.Command)
		}
	}
	c := teardownEffectsCommand{Command: a.Command, Issue: a.Target, Flags: []teardownEffectsFlag{{Name: "issue", Value: a.Target}}}
	document := teardownEffectsString(a.Fields, "artifact")
	if m := teardownEffectsDocumentRef.FindStringSubmatch(a.Target); m != nil && a.Command == "doc-edit" {
		c.Issue, c.Flags, document = m[1], []teardownEffectsFlag{{Name: "ref", Value: a.Target}}, strings.TrimPrefix(m[2], "artifact/")
	}
	if !slices.Contains(held, c.Issue) {
		return teardownEffectsCommand{}, fmt.Errorf("%s is on %s, which the case's world does not hold", where, a.Target)
	}
	switch {
	case document != "":
	case a.Command == "doc-edit":
		// An edit that names no document edits the issue's primary one, its spec.
		document = "spec"
	default:
		document = teardownEffectsString(a.Fields, "name")
	}
	for _, field := range fields {
		value, ok := a.Fields[field]
		if !ok {
			continue
		}
		text, isText := value.(string)
		flag := strings.ReplaceAll(field, "_", "-")
		var err error
		switch field {
		case "body", "summary", "name":
			if !isText {
				return c, fmt.Errorf("%s names %s as no text", where, field)
			}
			if field == "body" {
				text = teardownEffectsCaseRefs(text, key, ids)
			}
			c.Flags = append(c.Flags, teardownEffectsFlag{Name: flag, Value: text, Form: teardownEffectsText})
		case "content":
			if !isText {
				return c, fmt.Errorf("%s names %s as no text", where, field)
			}
			text = teardownEffectsCaseRefs(teardownEffectsBarePointers(c.Issue, document, text), key, ids)
			c.Flags = append(c.Flags, teardownEffectsFlag{Name: flag, Value: text, Form: teardownEffectsText})
		case "reply_to", "ask_id", "artifact":
			kind := map[string]string{"reply_to": "comment", "ask_id": "ask", "artifact": "artifact"}[field]
			if !isText || text == "" {
				return c, fmt.Errorf("%s names %s as no text", where, field)
			}
			given, named := ids[kind+"/"+text]
			switch {
			case named:
				text = given
			case kind != "artifact":
				return c, fmt.Errorf("%s names the %s %s by %s, and the replay's Dispatch holds no %s the case names so", where, kind, text, field, kind)
			}
			if field == "ask_id" {
				flag = "reply-to-ask"
			}
			c.Flags = append(c.Flags, teardownEffectsFlag{Name: flag, Value: text})
		case "turn", "status":
			if !isText || text == "" {
				return c, fmt.Errorf("%s names %s as no text", where, field)
			}
			c.Flags = append(c.Flags, teardownEffectsFlag{Name: flag, Value: text})
		case "before", "after":
			if !isText || !slices.Contains(held, text) {
				return c, fmt.Errorf("%s moves %s %s %v on the board, which the case's world does not hold", where, a.Target, field, value)
			}
			c.Flags = append(c.Flags, teardownEffectsFlag{Name: flag, Value: text})
		case "id":
			if !isText || text == "" {
				return c, fmt.Errorf("%s names its id as no text", where)
			}
			c.Name = text
		case "force", "include_verification":
			set, isBool := value.(bool)
			if !isBool {
				return c, fmt.Errorf("%s names %s as no boolean", where, field)
			}
			if !set {
				flag = "no-" + flag
			}
			c.Flags = append(c.Flags, teardownEffectsFlag{Name: flag, Form: teardownEffectsSet})
		case "version":
			number, isNumber := value.(float64)
			if !isNumber || number < 1 || number != float64(int(number)) {
				return c, fmt.Errorf("%s names the version %v, which is no version", where, value)
			}
			if a.Command == "plan-approve" {
				c.Flags = append(c.Flags, teardownEffectsFlag{Name: flag, Value: strconv.Itoa(int(number))})
			} else {
				c.Version = int(number)
			}
		case "ops":
			if text, err = teardownEffectsEditOps(value, c.Issue, document, key, ids); err != nil {
				return c, fmt.Errorf("%s: %w", where, err)
			}
			c.Flags = append(c.Flags, teardownEffectsFlag{Name: flag, Value: text, Form: teardownEffectsJSON})
		}
	}
	return c, nil
}

// teardownEffectsEditOps is a doc-edit's operations as the replay sends them, in JSON: each the
// trace's own, but the text it writes (`markdown`, `with`), whose bare pointers are written as the
// importer writes them in a document of the issue owner by its name or slug (document), and whose
// references to a record the case names are given the replay's ids (teardownEffectsCaseRefs). The
// text an operation finds or anchors on is the document's own, and stays as it is.
func teardownEffectsEditOps(ops any, owner, document, key string, ids map[string]string) (string, error) {
	list, ok := ops.([]any)
	if !ok || len(list) == 0 {
		return "", fmt.Errorf("its ops are %v, and an edit holds a list of one operation or more", ops)
	}
	sent := make([]any, len(list))
	for i, op := range list {
		fields, ok := op.(map[string]any)
		if !ok {
			return "", fmt.Errorf("its operation %d is %v, which is no object", i, op)
		}
		fields = maps.Clone(fields)
		for _, written := range []string{"markdown", "with"} {
			if text, ok := fields[written].(string); ok {
				fields[written] = teardownEffectsCaseRefs(teardownEffectsBarePointers(owner, document, text), key, ids)
			}
		}
		sent[i] = fields
	}
	encoded, err := json.Marshal(sent)
	return string(encoded), err
}

// teardownEffectsCaseRef is a reference to a comment or a question of an issue, by an id or a
// name: its issue, its kind and the id (seed.ts caseDispatchRefs).
var teardownEffectsCaseRef = regexp.MustCompile(`dispatch://([A-Z][A-Z0-9]*-\d+)/(comment|ask)/([A-Za-z0-9][A-Za-z0-9_-]*)`)

// teardownEffectsCaseRefs is text with each reference to a comment or a question of key by a name
// ids holds (`dispatch://ACME-41/comment/c-1`) written with the id the replay's Dispatch gave it,
// and each document id ids holds written as the replay's, as lane R's seed maps the world's names
// in the run's prompt (seed.ts caseDispatchRefs and the gate documents' ids): Dispatch refuses an
// id it did not give. Any other reference, and any other text, is left as it is.
func teardownEffectsCaseRefs(text, key string, ids map[string]string) string {
	text = teardownEffectsCaseRef.ReplaceAllStringFunc(text, func(ref string) string {
		m := teardownEffectsCaseRef.FindStringSubmatch(ref)
		if given, ok := ids[m[2]+"/"+m[3]]; ok && m[1] == key {
			return "dispatch://" + m[1] + "/" + m[2] + "/" + given
		}
		return ref
	})
	for _, named := range slices.Sorted(maps.Keys(ids)) {
		if stated, ok := strings.CutPrefix(named, "artifact/"); ok && stated != "" {
			text = strings.ReplaceAll(text, stated, ids[named])
		}
	}
	return text
}

// teardownEffectsMade is what a dispatch command made: the id of the record its write made (a
// comment's, a message's), and, for an upload or an edit of a document, that document's id and the
// version the write made (0 where it made none).
type teardownEffectsMade struct {
	ID, Document string
	Version      int
}

// teardownEffectsCheckRoute is a request the CLI sends before it writes, which writes nothing: the
// check of a document's text that `dispatch artifact` makes (POST /api/v1/markdown/round-trip and
// file-round-trip).
var teardownEffectsCheckRoute = regexp.MustCompile(`^/api/v1/markdown/`)

// teardownEffectsArtifactRoute is a route of one document: its id is the first group.
var teardownEffectsArtifactRoute = regexp.MustCompile(`^/api/v1/artifacts/([^/]+)/`)

// teardownEffectsCommandMade is what the real dispatch CLI made of c, the role's write: exit is its
// exit code, printed what it printed, asked the number of requests it sent Dispatch, and sent each
// of them that came back, with Dispatch's answer as the recording proxy keeps it, in order. The
// write is made where every request came back, the CLI exited 0, and Dispatch took each write, a
// request that is no read and no check of a text (teardownEffectsCheckRoute). The last such
// write's answer says what it made: the record's id, and, for an upload or an edit, the document
// and the version, which must be the version c names. Every other outcome is an error, which makes
// the replay HARNESS: it names the command, and Dispatch's status and code where Dispatch refused a
// request, or what the CLI printed where it refused the write before Dispatch refused any request.
func teardownEffectsCommandMade(c teardownEffectsCommand, exit int, printed string, asked int, sent []teardownEffectsHTTP) (teardownEffectsMade, error) {
	var made teardownEffectsMade
	if asked != len(sent) {
		return made, fmt.Errorf("%s sent Dispatch %d requests, and %d came back: a transport failure", c, asked, len(sent))
	}
	var writes []teardownEffectsHTTP
	var refused *teardownEffectsHTTP
	for i, x := range sent {
		if x.Status >= 400 && refused == nil {
			refused = &sent[i]
		}
		if x.Method != "GET" && x.Method != "HEAD" && !teardownEffectsCheckRoute.MatchString(x.Path) {
			writes = append(writes, x)
		}
	}
	switch {
	case exit != 0 && refused != nil:
		return made, fmt.Errorf("%s exited %d: Dispatch refused %s %s", c, exit, refused.route(), teardownEffectsStatusText(refused.status()))
	case exit != 0:
		return made, fmt.Errorf("%s exited %d before Dispatch refused any request: %s", c, exit, strings.TrimSpace(printed))
	case len(writes) == 0:
		return made, fmt.Errorf("%s exited 0 and sent Dispatch no write", c)
	}
	for _, x := range writes {
		if x.Status >= 400 {
			return made, fmt.Errorf("%s exited 0, and Dispatch refused %s %s", c, x.route(), teardownEffectsStatusText(x.status()))
		}
	}
	last := writes[len(writes)-1]
	id, document, version, err := teardownEffectsAnswerOf(last.Answer)
	if err != nil {
		return made, fmt.Errorf("%s: Dispatch's answer to %s: %w", c, last.route(), err)
	}
	made.ID = id
	if c.Command == "artifact" || c.Command == "doc-edit" {
		made.Document, made.Version = document, version
		if m := teardownEffectsArtifactRoute.FindStringSubmatch(last.Path); m != nil {
			made.Document = m[1]
		}
	}
	if c.Name != "" && made.ID == "" {
		return made, fmt.Errorf("%s: Dispatch's answer to %s names no id for the comment the case names %s", c, last.route(), c.Name)
	}
	if c.Version != 0 && made.Version != c.Version {
		return made, fmt.Errorf("%s: the trace states that its write makes version %d, and Dispatch's answer to %s names version %d", c, c.Version, last.route(), made.Version)
	}
	return made, nil
}

// teardownEffectsAnswerOf reads what the answer of a Dispatch write names: the id of the record
// it made (its own `id`, else an approval request's `ask.id`), the document it wrote
// (`artifact.id`), and the version it names (`version`, a number or an object holding one as
// `number`; 0 for none, null among them). An answer that is no JSON object is an error.
func teardownEffectsAnswerOf(answer []byte) (id, document string, version int, err error) {
	var read struct {
		ID  string `json:"id"`
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
		Version json.RawMessage `json:"version"`
	}
	if err := json.Unmarshal(answer, &read); err != nil {
		return "", "", 0, fmt.Errorf("no record: %w", err)
	}
	var numbered struct{ Number int }
	if json.Unmarshal(read.Version, &numbered) != nil {
		_ = json.Unmarshal(read.Version, &numbered.Number)
	}
	id = read.ID
	if id == "" {
		id = read.Ask.ID
	}
	return id, read.Artifact.ID, numbered.Number, nil
}

// Observe actual bytes on one HTTP hop: the pane's to the real daemon, where only the tool routes
// are observed (boot, registration, and asynchronous lifecycle calls are not tool answers), or the
// dispatch CLI's to Dispatch, where every request is observed. asked counts each observed request,
// and records holds each that came back, with its answer, so a request that got no answer (a
// transport failure) shows as a gap between the two.
type teardownEffectsProxy struct {
	mu      sync.Mutex
	url     string
	asked   int
	records []teardownEffectsHTTP
}

// teardownEffectsStartProxy starts a proxy to target that records each request observe names. An
// answer is recorded as the JSON its sender wrote: Dispatch gzips an answer of a kilobyte or more to
// a caller that takes gzip, as the dispatch CLI's bun does (compressAnswers in
// packages/envoy/internal/dispatch/api/response_compression.go), and the proxy passes those bytes
// to the caller as they came and records their decompressed text. An answer that is no JSON is
// recorded as a JSON string of its text.
func teardownEffectsStartProxy(t *testing.T, target string, observe func(path string) bool) *teardownEffectsProxy {
	t.Helper()
	dest, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := &teardownEffectsProxy{}
	forward := httputil.NewSingleHostReverseProxy(dest)
	type requestKey struct{}
	forward.ModifyResponse = func(res *http.Response) error {
		sent, ok := res.Request.Context().Value(requestKey{}).(teardownEffectsHTTP)
		if !ok {
			return nil
		}
		answer, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return err
		}
		res.Body = io.NopCloser(bytes.NewReader(answer))
		if strings.EqualFold(res.Header.Get("Content-Encoding"), "gzip") {
			if unzipped, err := gzip.NewReader(bytes.NewReader(answer)); err == nil {
				if text, err := io.ReadAll(unzipped); err == nil {
					answer = text
				}
			}
		}
		if !json.Valid(answer) {
			answer, _ = json.Marshal(string(answer))
		}
		sent.Answer, sent.Status = answer, res.StatusCode
		p.mu.Lock()
		p.records = append(p.records, sent)
		p.mu.Unlock()
		return nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if observe(req.URL.Path) {
			p.mu.Lock()
			p.asked++
			p.mu.Unlock()
			body, err := io.ReadAll(req.Body)
			if err != nil {
				http.Error(w, "cannot observe request", 400)
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), requestKey{}, teardownEffectsHTTP{At: time.Now(), Method: req.Method, Path: req.URL.Path, Body: body}))
		}
		forward.ServeHTTP(w, req)
	}))
	p.url = server.URL
	t.Cleanup(server.Close)
	return p
}

func (p *teardownEffectsProxy) since(from int) []teardownEffectsHTTP {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.records[from:])
}

func (p *teardownEffectsProxy) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records)
}

// observed is the number of requests the proxy observed, and each of them that came back, with its
// answer, in order.
func (p *teardownEffectsProxy) observed() (int, []teardownEffectsHTTP) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked, slices.Clone(p.records)
}

// teardownEffectsApplied is one recorded change a replay makes again, and how it makes it. apply
// answers the refusal of a change the replay made as a refused write (a trace's request-approval,
// teardownEffectsApprovalRefusal), and nil for a change it made.
type teardownEffectsApplied struct {
	teardownEffectsChange
	apply func() (*teardownEffectsRefusal, error)
}

// teardownEffectsMake makes changes again in their order, each with its apply, and answers each as
// replay.json records it: a change made as a refused write carries its refusal, and the changes
// after it are made too. It stops at the first change it cannot make. It then answers the changes
// made before it, and an error that names that change, its record and where, when it was to be
// made.
func teardownEffectsMake(changes []teardownEffectsApplied, where string) ([]teardownEffectsChange, error) {
	made := []teardownEffectsChange{}
	for _, c := range changes {
		refused, err := c.apply()
		if err != nil {
			return made, fmt.Errorf("%s (%s), %s: %w", c.Source, c.What, where, err)
		}
		c.Refused = refused
		made = append(made, c.teardownEffectsChange)
	}
	return made, nil
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
	a := c.trace(control.Trace).action(control.Seq)
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

// missed names each control the run promised that did not execute (executed holds the ones that
// did), in the order it promised them.
func (run *teardownEffectsControlRun) missed(executed map[teardownEffectsControl]bool) []string {
	var missed []string
	for _, control := range run.promised {
		if !executed[control] {
			missed = append(missed, run.named(control))
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
// array however little the replay reached, `reason` and `fault` on a HARNESS verdict alone, a
// role's write named by its record, its outcome.json action and case time, its author, the role's
// session and the call it preceded (the fault teardownEffectsOrder gives the local-only-head control
// whose result event is made a status write), and a trace's request-approval that the replay made
// as a refused write, under its own action and case time, with its refusal: the CLI's own of a
// document the issue does not hold, whose status and code are null, and Dispatch's 409
// PLAN_APPROVED_BY_REVIEW of a plan. A change made as no refused write holds no `refused` key.
// testdata/teardown-effects/replay-example.json is the five records as replay.json encodes each, in
// one array; heldout-reader.test.ts reads that file.
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
	requested := time.Date(2030, 1, 7, 15, 1, 0, 0, time.UTC)
	refused := func(refusal teardownEffectsRefusal) teardownEffectsReplayRecord {
		record := teardownEffectsNewReplayRecord(id)
		record.Verdict = "ok"
		record.End = []teardownEffectsChange{{Kind: "dispatch", Source: "R01 bad trace, action 1", What: teardownEffectsApprovalCommand, At: requested, OutcomeSeq: 1, Refused: &refusal}}
		return record
	}
	status, code := 409, "PLAN_APPROVED_BY_REVIEW"
	missing := refused(teardownEffectsRefusal{Cause: teardownEffectsMissingArtifact, Diagnostic: `document "plan.md" not found by slug; this issue's documents: spec-md (spec.md)`})
	plan := refused(teardownEffectsRefusal{Cause: teardownEffectsPlanApproval, Status: &status, Code: &code,
		Diagnostic: "PLAN_APPROVED_BY_REVIEW: a plan is approved by its plan review, not by a person: record it with dispatch plan-approve (POST /api/v1/artifacts/{id}/plan-reviews)"})
	got, err := json.MarshalIndent([]teardownEffectsReplayRecord{ok, write, records, missing, plan}, "", "  ")
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
	return teardownEffectsCasesAt(t, teardownEffectsExample, edit)
}

// teardownEffectsCasesAt are the cases of the case file at name, read as teardownEffectsExampleCases
// reads the example's.
func teardownEffectsCasesAt(t *testing.T, name string, edit func(cases []map[string]any) []map[string]any) []teardownEffectsCase {
	t.Helper()
	data, err := os.ReadFile(name)
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
		other := maps.Clone(cases[0])
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
		other := maps.Clone(cases[0])
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

// A trace's handoff, result and question are made by the run replay's own writer: each becomes the
// event and the readback record that writer takes, its commits mapped. Any other command is the
// real CLI's (teardownEffectsTraceCommand), and this writer refuses it, as it refuses a write on
// another issue and a write that lacks what its record needs.
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
	if e, readback := made(write("ask", "ACME-12", map[string]any{"question": "Which?", "options": []any{"a", "b"}})); e.Type != "ask.opened" || len(readback.Asks) != 1 || readback.Asks[0].Question != "Which?" || string(readback.Asks[0].Options) != `["a","b"]` {
		t.Fatalf("the question made %+v, %+v", e, readback.Asks)
	}
	for _, refused := range []teardownEffectsTraceAction{
		write("comment", "ACME-12", map[string]any{"body": "Look 1 is pending."}),
		write("artifact", "ACME-12", map[string]any{"name": "notes.md", "content": "Notes.\n"}),
		write("issue-update", "ACME-12", map[string]any{"status": "blocked"}),
		write("ask", "ACME-10", map[string]any{"question": "On the parent?", "options": []any{"yes"}}),
		write("handoff-write", "ACME-12", map[string]any{"phase": "implement"}),
	} {
		if _, _, err := teardownEffectsTraceWrite("ACME-12", session, refused, sha, version); err == nil {
			t.Fatalf("made the write %s on %s %v", refused.Command, refused.Target, refused.Fields)
		}
	}
}

// A synthetic trace: a request-approval Dispatch refused, then a comment and a push. The replay
// records each change under its own source, case time and outcome action, the refused one with its
// refusal and the others with none, and goes on to make the changes after the refusal. A change it
// cannot make stops the replay there: the error names the change and where it stood, and the changes
// made before it are kept.
func TestTeardownEffectsMake(t *testing.T) {
	t.Parallel()
	start := time.Date(2030, 1, 7, 15, 0, 0, 0, time.UTC)
	status, code := 409, "PLAN_APPROVED_BY_REVIEW"
	refusal := &teardownEffectsRefusal{Cause: teardownEffectsPlanApproval, Status: &status, Code: &code, Diagnostic: "PLAN_APPROVED_BY_REVIEW: a plan is approved by its plan review"}
	var made []int
	change := func(seq int, kind, what string, refused *teardownEffectsRefusal, err error) teardownEffectsApplied {
		c := teardownEffectsChange{Kind: kind, Source: fmt.Sprintf("S01 bad trace, action %d", seq), What: what, At: start.Add(time.Duration(seq) * time.Minute), OutcomeSeq: seq}
		return teardownEffectsApplied{teardownEffectsChange: c, apply: func() (*teardownEffectsRefusal, error) {
			made = append(made, seq)
			return refused, err
		}}
	}
	trace := []teardownEffectsApplied{
		change(1, "dispatch", teardownEffectsApprovalCommand, refusal, nil),
		change(2, "dispatch", "comment", nil, nil),
		change(3, "push", "refs/heads/legion/ACME-12 c1", nil, nil),
	}
	got, err := teardownEffectsMake(trace, "before legion call 1 (S01 bad trace, action 4)")
	if err != nil || !slices.Equal(made, []int{1, 2, 3}) || len(got) != 3 {
		t.Fatalf("made the actions %v and recorded %+v, %v; want all three, in their order", made, got, err)
	}
	for i, c := range got {
		want := trace[i].teardownEffectsChange
		if i == 0 {
			want.Refused = refusal
		}
		if !reflect.DeepEqual(c, want) {
			t.Fatalf("recorded change %d as %+v, want %+v", i, c, want)
		}
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(encoded), `"refused"`) != 1 || !strings.Contains(string(encoded), `"at":"2030-01-07T15:01:00Z","outcomeSeq":1,"refused":{"cause":"plan-approval","status":409,"code":"PLAN_APPROVED_BY_REVIEW"`) {
		t.Fatalf("encoded %s, want the refusal on action 1's change alone", encoded)
	}

	made = nil
	failed := change(2, "dispatch", "comment", nil, errors.New("Dispatch POST /api/v1/issues/ACME-12/comments = 500"))
	got, err = teardownEffectsMake([]teardownEffectsApplied{trace[0], failed, trace[2]}, "after the last legion call")
	if !slices.Equal(made, []int{1, 2}) || len(got) != 1 || got[0].Refused != refusal || err == nil ||
		err.Error() != "S01 bad trace, action 2 (comment), after the last legion call: Dispatch POST /api/v1/issues/ACME-12/comments = 500" {
		t.Fatalf("made %v and recorded %+v, %v; want the refused write kept and the comment's failure named", made, got, err)
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

// --controls names each control it promised that did not execute, in the order it promised them,
// and a supplemental case's control that did not execute fails the run as any other does.
func TestTeardownEffectsMissed(t *testing.T) {
	t.Parallel()
	example := teardownEffectsExampleCases(t, nil)
	run, err := teardownEffectsNewControlRun(t.TempDir(), example, nil)
	if err != nil {
		t.Fatal(err)
	}
	executed := map[teardownEffectsControl]bool{}
	for _, control := range run.promised {
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
	if got := run.missed(executed); !slices.Equal(got, want) {
		t.Fatalf("named %q, want %q", got, want)
	}
	if got := run.missed(map[teardownEffectsControl]bool{}); len(got) != len(run.promised) {
		t.Fatalf("with none executed, named %d of %d", len(got), len(run.promised))
	}

	// P10 as a supplemental case: its control that did not execute is missed, and none other.
	supplemented, err := teardownEffectsNewControlRun(t.TempDir(), example[:1], example[1:])
	if err != nil {
		t.Fatal(err)
	}
	for _, control := range supplemented.promised {
		executed[control] = true
	}
	executed[teardownEffectsControl{Case: "P10", Trace: "good", Check: teardownEffectsReplayed}] = false
	if got := supplemented.missed(executed); len(got) != 1 || !strings.HasSuffix(got[0], "P10 good trace: replayed") {
		t.Fatalf("with every control but the supplemental P10's replay executed, named %q", got)
	}
}

// teardownEffectsLookedAtNoComment is T0's example as a case file that shows no Dispatch comment on
// a production-check look that is not the last: P05, and P10 renamed L10 with no action in its
// known-bad trace.
func teardownEffectsLookedAtNoComment(t *testing.T) []teardownEffectsCase {
	t.Helper()
	return teardownEffectsExampleCases(t, func(cases []map[string]any) []map[string]any {
		cases[1]["id"] = "L10"
		teardownEffectsMember(cases[1], "traces", "bad").(map[string]any)["actions"] = []any{}
		return cases
	})
}

// The coverage check counts the controls of the case file and of the supplemental file together.
// A case file that shows no Dispatch comment on a look that is not the last is refused alone, and
// with a supplemental file that shows one (T0's P10) it is promised every control of both files:
// each of its own, then each of P10's, its known-bad comment among them.
func TestTeardownEffectsSupplementalCoverage(t *testing.T) {
	t.Parallel()
	locked := teardownEffectsLookedAtNoComment(t)
	if _, err := teardownEffectsPromised(locked); err == nil || !strings.Contains(err.Error(), teardownEffectsCommentNoWake) {
		t.Fatalf("the case file alone was promised its controls with %v, and it shows no %s", err, teardownEffectsCommentNoWake)
	}
	run, err := teardownEffectsNewControlRun(t.TempDir(), locked, teardownEffectsExampleCases(t, nil)[1:])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, control := range run.promised {
		got = append(got, control.String())
	}
	want := []string{
		"P05 good trace: replayed",
		"P05 good trace, action 5: pushed-with-open-pull-request-accepted",
		"P05 good trace, action 5: the effect of complete",
		"P05 bad trace: replayed",
		"P05 good trace, action 5: local-only-implementer-refused",
		"P05 good trace, action 5: pushed-without-pull-request-refused",
		"L10 good trace: replayed",
		"L10 good trace, action 2: pending-without-wake-refused",
		"L10 good trace, action 3: valid-pending-names-wake",
		"L10 good trace, action 3: the effect of report-pending",
		"L10 bad trace: replayed",
		"P10 good trace: replayed",
		"P10 good trace, action 2: pending-without-wake-refused",
		"P10 good trace, action 3: valid-pending-names-wake",
		"P10 good trace, action 3: the effect of report-pending",
		"P10 bad trace: replayed",
		"P10 bad trace, action 1: dispatch-comment-names-no-wake",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("promised\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A check that neither the case file nor the supplemental file shows is refused, naming the check.
func TestTeardownEffectsSupplementalCoverageRefused(t *testing.T) {
	t.Parallel()
	supplemental := teardownEffectsExampleCases(t, func(cases []map[string]any) []map[string]any {
		teardownEffectsMember(cases[1], "traces", "bad").(map[string]any)["actions"] = []any{}
		return cases[1:]
	})
	_, err := teardownEffectsNewControlRun(t.TempDir(), teardownEffectsLookedAtNoComment(t), supplemental)
	if err == nil || !strings.Contains(err.Error(), "no case shows the check "+teardownEffectsCommentNoWake) {
		t.Fatalf("two files that show no %s were refused with %v", teardownEffectsCommentNoWake, err)
	}
}

// No supplemental record reaches the reader. A supplemental case's records go to
// supplemental/<case id>/<good or bad> and its answers to supplemental/daemon-answers.json, so the
// evidence directory's daemon-answers.json, which a held-out stand-in overlay reads, holds the case
// file's answers alone, and heldout-reader.ts's controlAt, given a case file's id, reaches no
// supplemental record (heldout-reader.test.ts reads such a directory with readControls). Each check
// here takes the one answer its replay showed, a completion whose body names its case.
func TestTeardownEffectsSupplementalLayout(t *testing.T) {
	t.Parallel()
	const out = "/evidence"
	example := teardownEffectsExampleCases(t, nil)
	run, err := teardownEffectsNewControlRun(out, example[:1], example[1:])
	if err != nil {
		t.Fatal(err)
	}
	records := map[string]string{}
	for _, control := range run.promised {
		c := example[slices.IndexFunc(example, func(each teardownEffectsCase) bool { return each.ID == control.Case })]
		if control.Check == teardownEffectsReplayed {
			records[control.Case+" "+control.Trace] = teardownEffectsControlDir(run.dir(c.ID), c.ID, control.Trace)
		}
		reply := teardownEffectsHTTP{Method: http.MethodPost, Path: teardownEffectsComplete, Status: http.StatusOK, Answer: json.RawMessage(`{"case":"` + c.ID + `"}`)}
		run.capture(c.ID).take(control, c, teardownEffectsTurned{replies: []teardownEffectsHTTP{reply}})
	}
	if want := map[string]string{
		"P05 good": "/evidence/P05/good",
		"P05 bad":  "/evidence/P05/bad",
		"P10 good": "/evidence/supplemental/P10/good",
		"P10 bad":  "/evidence/supplemental/P10/bad",
	}; !maps.Equal(records, want) {
		t.Fatalf("the records go to %v, want %v", records, want)
	}
	answers := map[string][]string{}
	for file, capture := range run.answers {
		for _, answer := range capture.Answers {
			answers[file] = append(answers[file], answer.Name+" "+string(answer.Body))
		}
	}
	if want := map[string][]string{
		"/evidence/daemon-answers.json":              {`implementer-accepted {"case":"P05"}`, `implementer-no-pull-request {"case":"P05"}`},
		"/evidence/supplemental/daemon-answers.json": {`check-wake-required {"case":"P10"}`, `pending-accepted {"case":"P10"}`},
	}; !reflect.DeepEqual(answers, want) {
		t.Fatalf("the answers files hold %v, want %v", answers, want)
	}
	// With no supplemental file, the run writes one answers file, at the evidence directory.
	alone, err := teardownEffectsNewControlRun(out, example, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Collect(maps.Keys(alone.answers)); !slices.Equal(got, []string{"/evidence/daemon-answers.json"}) {
		t.Fatalf("with no supplemental file, the answers files are %q", got)
	}
}

// --supplemental refuses, before any replay and naming the cause, a supplemental case file that
// is not public (outside the checkout, held by no commit, or changed since the commit the run
// records), one that does not parse or holds no case, a case id that both files hold, and a case
// of the case file named as the supplemental directory. A public file reads as its path in the
// checkout and its cases.
func TestTeardownEffectsSupplementalRefusals(t *testing.T) {
	t.Parallel()
	example, err := os.ReadFile(teardownEffectsExample)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	git := fixturegit.Isolated(t, t.TempDir())
	git.Run(root, fixturegit.InitWorkTree("", "main")...)
	write := func(dir, name, text string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	public := write(root, "cases/public.json", string(example))
	changed := write(root, "cases/changed.json", string(example))
	broken := write(root, "cases/broken.json", `{"cases": [`)
	empty := write(root, "cases/empty.json", `{"cases": []}`)
	git.Run(root, "git", "add", ".")
	git.Run(root, "git", "commit", "-q", "-m", "cases")
	commit := git.Run(root, "git", "rev-parse", "HEAD")
	write(root, "cases/changed.json", strings.Replace(string(example), "P10", "P11", 1))
	untracked := write(root, "cases/untracked.json", string(example))
	outside := write(t.TempDir(), "P10.json", string(example))
	locked := teardownEffectsExampleCases(t, func(cases []map[string]any) []map[string]any {
		cases[0]["id"], cases[1]["id"] = "L05", "L10"
		return cases
	})

	name, cases, err := teardownEffectsSupplemental(root, commit, public, locked)
	if err != nil || name != "cases/public.json" || len(cases) != 2 || cases[0].ID != "P05" || cases[1].ID != "P10" {
		t.Fatalf("the public file reads as %q with %d cases, %v", name, len(cases), err)
	}
	named := teardownEffectsExampleCases(t, func(cases []map[string]any) []map[string]any {
		cases[0]["id"], cases[1]["id"] = "L05", teardownEffectsSupplementalDir
		return cases
	})
	for _, row := range []struct {
		name, path string
		cases      []teardownEffectsCase
		names      []string
	}{
		{"a file outside the checkout", outside, locked, []string{outside}},
		{"a file no commit holds", untracked, locked, []string{"cases/untracked.json", commit}},
		{"a file changed since the commit", changed, locked, []string{"cases/changed.json", commit}},
		{"a file that does not parse", broken, locked, []string{"cases/broken.json"}},
		{"a file that holds no case", empty, locked, []string{"cases/empty.json"}},
		{"a case id both files hold", public, teardownEffectsExampleCases(t, nil), []string{"P05", "P10"}},
		{"a case of the case file named as the supplemental directory", public, named, []string{teardownEffectsSupplementalDir}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := teardownEffectsSupplemental(root, commit, row.path, row.cases)
			if err == nil {
				t.Fatal("read the supplemental file with no refusal")
			}
			for _, want := range row.names {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("%v does not name %s", err, want)
				}
			}
		})
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
			case teardownEffectsMakesNothing(a):
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

// Catches: a starting document whose check or plan task names a block by its bare id
// (`serves="r1"`, `needs="t1"`), which the real Dispatch refuses (400 INVALID_MARKDOWN), so the
// replay could not write the case's starting state; and a rewrite that reaches a reference, a
// quoted value, another attribute or block, prose or fenced code, or a `needs` outside the
// document's own slug.
func TestTeardownEffectsBarePointers(t *testing.T) {
	t.Parallel()
	authored := strings.Join([]string{
		":::requirement{#r1}",
		"The reader retries.",
		":::",
		"",
		`:::check{#k1 serves="r1 dispatch://ACME-12/spec#b-r2 dispatch://ACME-12/spec#b-r3@v2" surface="cli" run="go test ./... -run r1" negative_control="A check that serves r1 alone fails."}`,
		"It serves r1.",
		":::",
		"",
		`:::plan_task{#t1 serves="r1"}`,
		"First.",
		":::",
		"",
		`:::plan_task{#t2 serves="r1" needs="t1 dispatch://ACME-12/artifact/plan-md#b-t0"}`,
		"After t1.",
		":::",
		"",
		`:::callout{#c1 serves="r1"}`,
		"Not a pointer of a check or a plan task.",
		":::",
		"",
		"```md",
		`:::check{#k9 serves="r9"}`,
		"```",
		"",
		`Prose with serves="r1" needs="t1".`,
	}, "\n")
	want := strings.NewReplacer(
		`{#k1 serves="r1 `, `{#k1 serves="dispatch://ACME-12/spec#b-r1 `,
		`{#t1 serves="r1"}`, `{#t1 serves="dispatch://ACME-12/spec#b-r1"}`,
		`{#t2 serves="r1" needs="t1 `, `{#t2 serves="dispatch://ACME-12/spec#b-r1" needs="dispatch://ACME-12/artifact/plan-md#b-t1 `,
	).Replace(authored)
	if got := teardownEffectsBarePointers("ACME-12", "plan.md", authored); got != want {
		t.Errorf("plan.md as the replay writes it:\n%s\nwant:\n%s", got, want)
	}
	if got := teardownEffectsBarePointers("ACME-12", "plan.md", want); got != want {
		t.Errorf("plan.md written once more:\n%s\nwant it unchanged:\n%s", got, want)
	}
	if got, want := teardownEffectsBarePointers("ACME-12", "Plan v2.md", `:::plan_task{#t2 needs="t1"}`), `:::plan_task{#t2 needs="dispatch://ACME-12/artifact/plan-v2-md#b-t1"}`; got != want {
		t.Errorf("a needs of Plan v2.md reads %s, want %s", got, want)
	}
}

// teardownEffectsCarriedFile is the example with actions of the carried kinds (an Envoy message, a
// Dispatch read, a task agent's start, a probe of production, a diff read) among the actions of its
// traces, in time order, each trace numbered again from 1 (its .command says how it was made).
// P05's completion is action 10 there, and action 5 in the example; P10's two reports are actions 5
// and 7, and 2 and 3; P10's known-bad comment is action 3, and 1.
const teardownEffectsCarriedFile = "testdata/teardown-effects/carried-kinds-cases.json"

// The replay makes none of the carried kinds again, a staging action and a GitHub read as well, and
// makes each push, GitHub change, Dispatch write and daemon call.
func TestTeardownEffectsMakesNothing(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		a    teardownEffectsTraceAction
		want bool
	}{
		{"an Envoy message", teardownEffectsTraceAction{Kind: "envoy", Tool: "publish", Target: "notifications.role.acme.x"}, true},
		{"a Dispatch read", teardownEffectsTraceAction{Kind: "dispatch-read", Command: "handoff-read", Target: "ACME-12"}, true},
		{"a task agent's start", teardownEffectsTraceAction{Kind: "agent", Agent: "task", Name: "ReviewDiff"}, true},
		{"a probe of production", teardownEffectsTraceAction{Kind: "probe", Target: "https://service.example/version", Revision: strings.Repeat("a", 40)}, true},
		{"a diff read", teardownEffectsTraceAction{Kind: "diff-read", Command: "jj diff", Source: "$LEGION_WORKSPACE"}, true},
		{"a staging action", teardownEffectsTraceAction{Kind: "staging"}, true},
		{"a GitHub read", teardownEffectsTraceAction{Kind: "github", Command: "gh pr view"}, true},
		{"a GitHub change", teardownEffectsTraceAction{Kind: "github", Command: "gh pr create", Fields: map[string]any{"head": "legion/ACME-12", "base": "main"}}, false},
		{"a push", teardownEffectsTraceAction{Kind: "push", Ref: "legion/ACME-12"}, false},
		{"a Dispatch write whose command a read is named", teardownEffectsTraceAction{Kind: "dispatch", Command: "handoff-read"}, false},
		{"a daemon call", teardownEffectsTraceAction{Kind: "daemon", Op: "handoff_complete"}, false},
	} {
		if got := teardownEffectsMakesNothing(row.a); got != row.want {
			t.Errorf("%s: the replay makes nothing of it = %t, want %t", row.name, got, row.want)
		}
	}
}

// The carried actions are no control: with them among the actions of every trace, --controls
// promises the same controls as for the example, each check on its own action under that action's
// own seq, and none on an action of a carried kind.
func TestTeardownEffectsCarriedKindsAreNoControl(t *testing.T) {
	t.Parallel()
	promised, err := teardownEffectsPromised(teardownEffectsCasesAt(t, teardownEffectsCarriedFile, nil))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, control := range promised {
		got = append(got, control.String())
	}
	want := []string{
		"P05 good trace: replayed",
		"P05 good trace, action 10: pushed-with-open-pull-request-accepted",
		"P05 good trace, action 10: the effect of complete",
		"P05 bad trace: replayed",
		"P05 good trace, action 10: local-only-implementer-refused",
		"P05 good trace, action 10: pushed-without-pull-request-refused",
		"P10 good trace: replayed",
		"P10 good trace, action 5: pending-without-wake-refused",
		"P10 good trace, action 7: valid-pending-names-wake",
		"P10 good trace, action 7: the effect of report-pending",
		"P10 bad trace: replayed",
		"P10 bad trace, action 3: dispatch-comment-names-no-wake",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("promised\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The same checks as the example's, in the same order: only the seq of each moved.
	example, err := teardownEffectsPromised(teardownEffectsExampleCases(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	checks := func(controls []teardownEffectsControl) (names []string) {
		for _, control := range controls {
			names = append(names, control.Case+" "+control.Trace+" "+control.Check+" "+control.Action)
		}
		return names
	}
	if !slices.Equal(checks(promised), checks(example)) {
		t.Fatalf("the carried file promises the checks\n%s\nand the example\n%s", strings.Join(checks(promised), "\n"), strings.Join(checks(example), "\n"))
	}
	// Every control that names an action names one the replay makes, never one of a carried kind.
	for _, c := range teardownEffectsCasesAt(t, teardownEffectsCarriedFile, nil) {
		carried := 0
		for _, name := range teardownEffectsTraces(c) {
			for _, a := range c.trace(name).Actions {
				if !slices.Contains(teardownEffectsCarriedKinds, a.Kind) {
					continue
				}
				carried++
				for _, control := range promised {
					if control.Case == c.ID && control.Trace == name && control.Seq == a.Seq {
						t.Errorf("%s is a control on %s %s's action %d, a %s action", control, c.ID, name, a.Seq, a.Kind)
					}
				}
			}
		}
		if carried == 0 {
			t.Errorf("%s holds no action of a carried kind", c.ID)
		}
	}
}

// A route names each carried kind and its fields as heldout-case.ts's Route does, and takes an
// action of its own kind alone.
func TestTeardownEffectsTakesCarriedKinds(t *testing.T) {
	t.Parallel()
	cases := teardownEffectsCasesAt(t, teardownEffectsCarriedFile, nil)
	byKind := map[string]teardownEffectsTraceAction{}
	for _, c := range cases {
		for _, a := range c.Traces.Good.Actions {
			if _, seen := byKind[a.Kind]; !seen {
				byKind[a.Kind] = a
			}
		}
	}
	// The window of an agent's end: from 15:00 to 16:00, or, for the second, from 15:30.
	endedWithin := func(after time.Time) []struct {
		Field         string
		After, Before time.Time
	} {
		return []struct {
			Field         string
			After, Before time.Time
		}{{Field: "endedAt", After: after, Before: time.Date(2030, 1, 7, 16, 0, 0, 0, time.UTC)}}
	}
	window := endedWithin(time.Date(2030, 1, 7, 15, 0, 0, 0, time.UTC))
	late := endedWithin(time.Date(2030, 1, 7, 15, 30, 0, 0, time.UTC))
	for _, row := range []struct {
		name  string
		route teardownEffectsRoute
		on    string
		want  bool
	}{
		{"an Envoy publish of its tool and an argument", teardownEffectsRoute{Kind: "envoy", Match: map[string]any{"tool": "publish", "fields": map[string]any{"expects_reply": false}}}, "envoy", true},
		{"an Envoy send, for a publish", teardownEffectsRoute{Kind: "envoy", Match: map[string]any{"tool": "send"}}, "envoy", false},
		{"a Dispatch read of its command and target", teardownEffectsRoute{Kind: "dispatch-read", Match: map[string]any{"command": "read", "target": "ACME-12"}}, "dispatch-read", true},
		{"a Dispatch read of another command", teardownEffectsRoute{Kind: "dispatch-read", Match: map[string]any{"command": "steps"}}, "dispatch-read", false},
		{"a task agent of its kind inside the window of its end", teardownEffectsRoute{Kind: "agent", Match: map[string]any{"agent": "task"}, Between: window}, "agent", true},
		{"a task agent whose end is outside the window", teardownEffectsRoute{Kind: "agent", Match: map[string]any{"name": "ReviewDiff"}, Between: late}, "agent", false},
		{"a Dispatch write route, for a Dispatch read", teardownEffectsRoute{Kind: "dispatch", Match: map[string]any{"command": "read"}}, "dispatch-read", false},
		{"a probe of its address and revision", teardownEffectsRoute{Kind: "probe", Match: map[string]any{"target": "https://service.example/version", "revision": "3b5d7f9a1c2e4a6c8e0b2d4f6a8c1e3b5d7f9a0c"}}, "probe", true},
		{"a probe that answered another revision", teardownEffectsRoute{Kind: "probe", Match: map[string]any{"revision": strings.Repeat("a", 40)}}, "probe", false},
		{"a diff read of its command, checkout and paths", teardownEffectsRoute{Kind: "diff-read", Match: map[string]any{"command": "jj diff", "source": "$LEGION_WORKSPACE", "paths": []any{}}}, "diff-read", true},
		{"a diff read of another path", teardownEffectsRoute{Kind: "diff-read", Match: map[string]any{"paths": []any{"internal/api/doc.go"}}}, "diff-read", false},
	} {
		a, ok := byKind[row.on]
		if !ok {
			t.Fatalf("the carried file holds no %s action", row.on)
		}
		if got := teardownEffectsTakes(row.route, a); got != row.want {
			t.Errorf("%s: takes = %t, want %t (fields %v)", row.name, got, row.want, teardownEffectsFieldsOf(a))
		}
	}
}

// A trace's documents read as the case file holds them, and lane D's control holds each to the
// document the real Dispatch holds after the trace's last write: its latest version and that
// version's text, with each id the replay's Dispatch gave a record the world names written as the
// world's, and a differing text named by its first line that differs. With no id to map, the text
// is compared as Dispatch holds it. A document the replay wrote that the trace does not state is a
// fault too, so a trace that states no document fails where the replay wrote one, and one with no
// `documents` key states nothing.
func TestTeardownEffectsDocumentFaults(t *testing.T) {
	t.Parallel()
	cases := teardownEffectsCasesAt(t, teardownEffectsCarriedFile, func(cases []map[string]any) []map[string]any {
		good := teardownEffectsMember(cases[0], "traces", "good").(map[string]any)
		good["documents"] = []any{map[string]any{"name": "verification.md", "version": 2, "body": "See dispatch://ACME-12/comment/c-1 and art-design.\n"}}
		teardownEffectsMember(cases[0], "traces", "bad").(map[string]any)["documents"] = []any{}
		return cases
	})
	stated := cases[0].Traces.Good.Documents
	if want := []teardownEffectsDocument{{Name: "verification.md", Version: 2, Body: "See dispatch://ACME-12/comment/c-1 and art-design.\n"}}; !reflect.DeepEqual(stated, want) {
		t.Fatalf("the trace's documents read as %+v, want %+v", stated, want)
	}
	if none, unstated := cases[0].Traces.Bad.Documents, cases[1].Traces.Good.Documents; none == nil || len(none) != 0 || unstated != nil {
		t.Fatalf("an empty `documents` reads as %#v and none as %#v, want an empty list and nil", none, unstated)
	}
	given := "See dispatch://ACME-12/comment/0b6c2f1e-0000-4000-8000-000000000001 and 7d2a9b4c-0000-4000-8000-000000000002.\n"
	ids := map[string]string{"0b6c2f1e-0000-4000-8000-000000000001": "c-1", "7d2a9b4c-0000-4000-8000-000000000002": "art-design"}
	held := map[string]teardownEffectsDocument{"verification.md": {Name: "verification.md", Version: 2, Body: given}}
	if faults := teardownEffectsDocumentFaults(stated, held, []string{"verification.md"}, ids); len(faults) != 0 {
		t.Fatalf("the document as stated, with the replay's ids, reads as faults %v", faults)
	}
	// With no ids to map, the replay's ids stay in the text, and the stated body is not the one held.
	if faults := teardownEffectsDocumentFaults(stated, held, nil, map[string]string{}); len(faults) != 1 ||
		!strings.Contains(faults[0], `line 1 is "See dispatch://ACME-12/comment/c-1 and art-design." in the body the trace states, and "See dispatch://ACME-12/comment/0b6c2f1e`) {
		t.Fatalf("with no ids mapped, the faults are %v, want the first line named", faults)
	}
	plain := []teardownEffectsDocument{{Name: "plan.md", Version: 1, Body: "The plan.\n"}}
	if faults := teardownEffectsDocumentFaults(plain, map[string]teardownEffectsDocument{"plan.md": plain[0]}, nil, map[string]string{}); len(faults) != 0 {
		t.Fatalf("a document held as stated, with no ids, reads as faults %v", faults)
	}
	for _, row := range []struct {
		name    string
		stated  []teardownEffectsDocument
		held    map[string]teardownEffectsDocument
		written []string
		want    string
	}{
		{"another version", stated, map[string]teardownEffectsDocument{"verification.md": {Name: "verification.md", Version: 3, Body: given}}, nil, "holds version 3"},
		{"another text", stated, map[string]teardownEffectsDocument{"verification.md": {Name: "verification.md", Version: 2, Body: "Other.\n"}}, nil,
			`verification.md version 2: line 1 is "See dispatch://ACME-12/comment/c-1 and art-design." in the body the trace states, and "Other." in the body the replay's Dispatch holds`},
		{"a text with a line more", plain, map[string]teardownEffectsDocument{"plan.md": {Name: "plan.md", Version: 1, Body: "The plan.\nMore.\n"}}, nil,
			`line 2 is "" in the body the trace states, and "More." in the body the replay's Dispatch holds`},
		{"a text with a line less", []teardownEffectsDocument{{Name: "plan.md", Version: 1, Body: "The plan.\nMore.\n"}}, map[string]teardownEffectsDocument{"plan.md": plain[0]}, nil,
			`line 2 is "More." in the body the trace states, and "" in the body the replay's Dispatch holds`},
		{"no document of the name", stated, map[string]teardownEffectsDocument{}, nil, "holds no document of that name"},
		{"a document written and stated by none", nil, held, []string{"verification.md"}, "the replay wrote verification.md, and the trace states no such document"},
	} {
		if faults := teardownEffectsDocumentFaults(row.stated, row.held, row.written, ids); len(faults) != 1 || !strings.Contains(faults[0], row.want) {
			t.Errorf("%s: faults %v, want one naming %q", row.name, faults, row.want)
		}
	}
	if got := teardownEffectsFirstDifference("a\nb", "a"); got != `line 2 is "b" in the body the trace states, and no line in the body the replay's Dispatch holds` {
		t.Errorf("a body cut short reads as %q, want its missing line named", got)
	}
}

// The case's world holds the typed parts a read answers from (heldout-case.ts: the daemon's claim,
// issue, entry, architect, records, admission and gate; Dispatch's other issues and step runs;
// GitHub's settings, branch rules and a pull request's merge state), and lane D reads each one
// rather than dropping it in silence, as it did before it seeded them: a world with them reads
// otherwise than the world without, part for part (teardown_effects_world_test.go seeds and holds
// them).
func TestTeardownEffectsReadsTheWorldParts(t *testing.T) {
	t.Parallel()
	plain := teardownEffectsExampleCases(t, nil)
	parts := teardownEffectsExampleCases(t, func(cases []map[string]any) []map[string]any {
		world := cases[0]["world"].(map[string]any)
		daemon := world["daemon"].(map[string]any)
		daemon["claim"] = map[string]any{"issue": "ACME-12", "tree": "ACME-10", "session": "ses-acme-12-implementer"}
		daemon["issue"] = map[string]any{"tree": "ACME-10", "parent": "ACME-10", "generation": 2, "lingerUntil": nil}
		daemon["entry"] = map[string]any{"phase": 1, "turn": 2, "owedTask": nil, "movedBack": true}
		daemon["architect"] = map[string]any{"state": "suspended"}
		daemon["records"] = []any{map[string]any{"key": "ACME-13", "tree": "ACME-10", "parent": "ACME-10", "phase": "planning", "status": "in_progress", "generation": 1, "architect": nil}}
		daemon["admission"] = map[string]any{"cap": 3, "active": []any{"ACME-12"}, "waiting": []any{}, "blocked": []any{}}
		daemon["gate"] = []any{map[string]any{"step": "design", "document": "design.md", "current": 2, "approved": 1, "artifactId": "art-design"}}
		dispatch := world["dispatch"].(map[string]any)
		dispatch["issue"].(map[string]any)["labels"] = []any{"legion"}
		dispatch["issues"] = []any{map[string]any{"key": "ACME-13", "status": "todo", "parent": nil, "rank": 1, "labels": []any{}, "blockedBy": []any{}, "claimedBy": nil}}
		dispatch["steps"] = []any{map[string]any{"task": "default", "step": 1, "kind": "step", "role": "planner", "result": "passed", "startedAt": "2030-01-07T14:00:00Z", "endedAt": "2030-01-07T14:30:00Z"}}
		github := world["github"].(map[string]any)
		github["settings"] = map[string]any{"default_branch": "main"}
		github["branchRules"] = []any{map[string]any{"branch": "main", "rules": []any{}}}
		return cases
	})
	if reflect.DeepEqual(plain, parts) {
		t.Fatalf("a world holding every part reads as the world without them: %+v", parts[0].World)
	}
	read := parts[0].World
	daemonParts := read.Daemon.teardownEffectsDaemonParts
	if daemonParts.Claim == nil || daemonParts.Issue == nil || daemonParts.Entry == nil || daemonParts.Architect == nil || len(daemonParts.Records) != 1 || daemonParts.Admission == nil || len(daemonParts.Gate) != 1 ||
		read.Dispatch.Issue.Labels == nil || len(read.Dispatch.Issues) != 1 || len(read.Dispatch.Steps) != 1 || read.GitHub.Settings == nil || read.GitHub.BranchRules == nil || len(read.unserved) != 0 {
		t.Fatalf("a world holding every part reads as %+v, its daemon parts as %+v", read, daemonParts)
	}
}
