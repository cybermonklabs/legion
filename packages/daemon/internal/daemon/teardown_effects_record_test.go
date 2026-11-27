package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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
// the reason of a harness failure), the seeded Dispatch records the replay started from, each call
// replayed with the recorded changes applied before it, the state it ran in, the stand-in's answers
// and the real daemon's, and the client's answer in the run and in the replay; then the changes
// recorded after the last call, which the replay did not apply, and the calls it did not replay,
// which follow the first call the real daemon answered otherwise.
type teardownEffectsReplayRecord struct {
	ID          string                      `json:"id"`
	Verdict     string                      `json:"verdict"`
	Reason      string                      `json:"reason,omitempty"`
	Seed        []teardownEffectsChange     `json:"seed"`
	Calls       []teardownEffectsReplayCall `json:"calls"`
	After       []teardownEffectsChange     `json:"after"`
	NotReplayed []string                    `json:"notReplayed"`
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
