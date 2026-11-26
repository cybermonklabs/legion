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

type teardownEffectsDifference struct {
	Seq     int                   `json:"seq"`
	Standin teardownEffectsStatus `json:"standin"`
	Daemon  teardownEffectsStatus `json:"daemon"`
}

type teardownEffectsCall struct {
	ID, Op, Session string
	Args            map[string]any
	At, ResultAt    time.Time
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
				calls[i].ResultAt = entry.Timestamp
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
		id, at string
		wake   any
	}{
		{"toolu_02legion_refused", "2030-01-08T12:03:10Z", nil},
		{"toolu_03legion_ok", "2030-01-08T12:03:40Z", "2030-01-08T12:30:00Z"},
	} {
		got := calls[i]
		if got.ID != want.id || got.Op != "handoff_complete" || got.Session != "11111111-2222-4333-8444-555555555555" || got.At.Format(time.RFC3339) != want.at || got.ResultAt.Sub(got.At) != time.Second || got.Args["wake_at"] != want.wake {
			t.Fatalf("call %d: %+v, want %s at %s (wake %v)", i, got, want.id, want.at, want.wake)
		}
	}
}
