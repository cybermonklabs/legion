package daemon

//ci:reads packages/pi-legion/scripts/skill-scenarios/testdata/f2/sessions/p10-production-check.jsonl TestTeardownEffectsTranscriptF2 reads fixture F2 through ../../../pi-legion

import (
	"bufio"
	"encoding/json"
	"errors"
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
// the reason of a harness failure and its typed cause, `fault`), the seeded Dispatch records the
// replay started from, each call replayed with the recorded changes applied before it, the state it
// ran in, the stand-in's answers and the real daemon's, and the client's answer in the run and in
// the replay; then the changes recorded after the last call, which the replay did not apply, and the
// calls it did not replay, which follow the first call the real daemon answered otherwise. Every
// list is an array, never null (teardownEffectsNewReplayRecord).
type teardownEffectsReplayRecord struct {
	ID          string                      `json:"id"`
	Verdict     string                      `json:"verdict"`
	Reason      string                      `json:"reason,omitempty"`
	Fault       *teardownEffectsFault       `json:"fault,omitempty"`
	Seed        []teardownEffectsChange     `json:"seed"`
	Calls       []teardownEffectsReplayCall `json:"calls"`
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
	return teardownEffectsReplayRecord{ID: id, Seed: []teardownEffectsChange{}, Calls: []teardownEffectsReplayCall{}, After: []teardownEffectsChange{}, NotReplayed: []string{}}
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
