package api

// going_rewrite_stop_test.go holds the stop of a run while its agent rewrites a reply it held back
// as long (going/agent's length.go), as the person meets it: the agent's real loop runs in the
// test process, connected to the server over its real socket, and calls a fake chat model
// (going/agent/agenttest) whose rewrite never answers.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
)

// TestAStopDuringTheRewriteShowsTheHeldReplyWhole: a reply the bar draws in 120 words or more is
// held back once, its frame carrying no text, so the bar draws nothing of it, and the next model
// call is its rewrite. A person who stops the run while the rewrite's call works frees its lease,
// so its agent can save nothing more, and the bar never shows nothing: the stop saves the held
// reply shown whole as the run's next step, as the agent shows it where the rewrite draws no reply
// it may show - its text, its count of long words cleared and a note of its text, so a later turn
// reads it as the answer the person saw - with its reply frame, the stopped run's answer: its
// text, no call and the count of the words the bar draws of it that the held frame said, before
// the stop's end frame. A run that withheld a tool result from the model before the reply was held
// back, the content filter having refused a call that held it twice, records it on the held step,
// and the stop's step tells of it as the agent's own does: its body says the notice after the
// text, and its frame, never the held one, how many results were withheld. A run stopped while its
// first model call works, which held no reply back, saves no step and shows no reply.
func TestAStopDuringTheRewriteShowsTheHeldReplyWhole(t *testing.T) {
	t.Parallel()
	rig := newGoingAgentRig(t, nil)
	loop := newGoingLoop(t, rig)
	ctx := context.Background()
	// never is never closed: a call it holds is answered nothing until the agent leaves it.
	never := make(chan struct{})
	words := make([]string, 130)
	for at := range words {
		words[at] = fmt.Sprintf("word%d", at)
	}
	long := strings.Join(words, " ")
	type stored struct {
		name string
		data map[string]any
	}
	type savedStep struct {
		step       float64
		kind, note string
		body       map[string]any
	}
	// stop stops run as its person does once the run made calls model calls, and answers the
	// frames of its events and the steps it saved, in order, once it ended stopped.
	stop := func(t *testing.T, run, asked string, calls int) ([]stored, []savedStep) {
		t.Helper()
		goingEventually(t, fmt.Sprintf("the run's model call %d", calls), func() bool { return len(loop.fake.Asking(asked)) == calls })
		if stopped, err := rig.served.stopGoingRun(ctx, run, goingStoppedByPerson); err != nil || !stopped {
			t.Fatalf("stop the run: %v, %v", stopped, err)
		}
		if record := rig.ended(t, run, time.Minute); record.state != agentwire.StateStopped || record.reason != goingStoppedByPerson {
			t.Fatalf("the run ended %s with %q, want stopped by its person", record.state, record.reason)
		}
		rows, err := rig.database.Pool.Query(ctx, `select name, data from going_run_events where run_id = $1::uuid order by sequence`, run)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (stored, error) {
			var frame stored
			var data []byte
			if err := row.Scan(&frame.name, &data); err != nil {
				return frame, err
			}
			return frame, json.Unmarshal(data, &frame.data)
		})
		if err != nil {
			t.Fatalf("read the frames of run %s: %v", run, err)
		}
		if last := len(frames) - 1; last < 0 || frames[last].name != agentwire.FrameEnd ||
			frames[last].data["state"] != agentwire.StateStopped || frames[last].data["reason"] != goingStoppedByPerson {
			t.Fatalf("run %s's frames are %v, want the last its person's stop", run, frames)
		}
		rows, err = rig.database.Pool.Query(ctx, `select step, kind, note, body from going_run_steps where run_id = $1::uuid order by step`, run)
		if err != nil {
			t.Fatal(err)
		}
		steps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (savedStep, error) {
			var saved savedStep
			var number int
			var body []byte
			if err := row.Scan(&number, &saved.kind, &saved.note, &body); err != nil {
				return saved, err
			}
			saved.step = float64(number)
			return saved, json.Unmarshal(body, &saved.body)
		})
		if err != nil {
			t.Fatalf("read the steps of run %s: %v", run, err)
		}
		return frames, steps
	}
	// requireShown fails t unless run, stopped during its rewrite, holds the held reply's frame,
	// marked, with no text and its count, and, just before the stop's end frame, the stop's frame
	// of the held reply shown whole, the run's answer; and unless the steps it saved end with the
	// held reply and the stop's step of it shown whole, with a note of its text and no long words.
	// It answers the held step, the stop's step and the answer's frame.
	requireShown := func(t *testing.T, run string, frames []stored, steps []savedStep) (savedStep, savedStep, stored) {
		t.Helper()
		var held stored
		for _, frame := range frames {
			if frame.name == goingReplyFrame && frame.data["held_long"] == true && frame.data["text"] == "" {
				held = frame
			}
		}
		step, _ := held.data["step"].(float64)
		drawn, _ := held.data["drawn_words"].(float64)
		if step == 0 || drawn != float64(len(words)) || held.data["withheld"] != nil {
			t.Fatalf("run %s's frames are %v, want the held reply's, marked, with no text, no withheld results and its %d drawn words", run, frames, len(words))
		}
		// The bar's answer is the prose of the last reply that calls nothing (web
		// features/going/run-frames.ts): the stop's frame of the held reply, just before its end.
		var answer stored
		for _, frame := range frames {
			if tools, _ := frame.data["tools"].([]any); frame.name == goingReplyFrame && frame.data["text"] != "" && len(tools) == 0 {
				answer = frame
			}
		}
		labels, _ := answer.data["labels"].([]any)
		if !reflect.DeepEqual(frames[len(frames)-2], answer) || answer.data["step"] != step+1 || answer.data["text"] != long ||
			answer.data["drawn_words"] != drawn || answer.data["held_long"] != nil || answer.data["waits_for_person"] != false || len(labels) != 0 {
			t.Errorf("run %s's answer is %v, of the frames %v; want the held reply whole as step %v with its %v drawn words, just before the stop's end frame",
				run, answer.data, frames, step+1, drawn)
		}
		if len(steps) < 2 {
			t.Fatalf("run %s saved the steps %v, want the held reply and the stop's step of it shown whole", run, steps)
		}
		kept, shown := steps[len(steps)-2], steps[len(steps)-1]
		if kept.step != step || kept.body["text"] != long || kept.body["long_words"] != drawn {
			t.Errorf("run %s's step before the last is %v, want the held reply, step %v, with its %v long words", run, kept, step, drawn)
		}
		text, _ := shown.body["text"].(string)
		if line := strings.TrimSuffix(shown.note, "…"); shown.step != step+1 || shown.kind != agentwire.StepModel || !strings.HasPrefix(text, long) ||
			shown.body["long_words"] != nil || shown.body["withheld"] != nil || line == "" || !strings.HasPrefix(long, line) {
			t.Errorf("run %s's last step is %v, want step %v, the held reply whole with no long words, no record of withheld results and a note of its text, which a later turn reads as the answer",
				run, shown, step+1)
		}
		return kept, shown, answer
	}

	t.Run("a run stopped during its rewrite answers the held reply", func(t *testing.T) {
		const asked = "what did the southern team write about the comet"
		loop.script.set(asked, func(n int) agenttest.Reply {
			if n == 1 {
				return answered(long)
			}
			return agenttest.Reply{Hold: never}
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: asked})
		frames, steps := stop(t, run, asked, 2)
		kept, shown, answer := requireShown(t, run, frames, steps)
		if kept.body["withheld"] != nil || shown.body["text"] != long || answer.data["withheld"] != nil {
			t.Errorf("run %s's held step %v, shown step %v and answer %v tell of withheld results, want none: the run withheld none", run, kept, shown, answer.data)
		}
	})

	t.Run("a run that withheld a result before the held reply tells of it", func(t *testing.T) {
		const asked = "what did the western team write about the comet"
		page := rig.issue(t, "The comet survey")
		// The page's read is step 1. The filter refuses the first model call, and again once it
		// is made again, so the read's result is withheld; the long reply is held back, and its
		// rewrite never answers.
		loop.script.set(asked, func(n int) agenttest.Reply {
			switch n {
			case 1, 2:
				return agenttest.Reply{Status: http.StatusBadRequest, Error: agenttest.FilterRefusal}
			case 3:
				return answered(long)
			}
			return agenttest.Reply{Hold: never}
		})
		run := rig.begin(t, goingLoopRun{login: "alice", words: asked, page: page})
		frames, steps := stop(t, run, asked, 4)
		kept, shown, answer := requireShown(t, run, frames, steps)
		if withheld, _ := kept.body["withheld"].(map[string]any); len(withheld) != 1 || withheld["1"] != "read" {
			t.Errorf("run %s's held step records the withheld results %v, want the page's read, step 1", run, kept.body["withheld"])
		}
		notice := "One tool result of this run, step 1 (read), was withheld from the chat model"
		if text, _ := shown.body["text"].(string); !strings.Contains(strings.TrimPrefix(text, long), notice) {
			t.Errorf("run %s's last step says %q, want the held reply and then the notice %q, which a later turn reads", run, text, notice)
		}
		if answer.data["withheld"] != float64(1) {
			t.Errorf("run %s's answer frame says %v results withheld, want 1, which the bar says under the answer", run, answer.data["withheld"])
		}
	})

	t.Run("a run stopped before it held a reply back shows no reply", func(t *testing.T) {
		const asked = "what did the northern team write about the comet"
		loop.script.set(asked, func(int) agenttest.Reply { return agenttest.Reply{Hold: never} })
		run := rig.begin(t, goingLoopRun{login: "alice", words: asked})
		frames, steps := stop(t, run, asked, 1)
		for _, frame := range frames {
			if frame.name == goingReplyFrame {
				t.Errorf("run %s stored the reply frame %v, want none: it held no reply back", run, frame.data)
			}
		}
		if len(steps) != 0 {
			t.Errorf("run %s saved the steps %v, want none: its one model call was stopped", run, steps)
		}
	})
}
