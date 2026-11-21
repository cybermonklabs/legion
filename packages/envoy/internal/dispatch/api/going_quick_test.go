package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/goingtest"
	"github.com/sjawhar/envoy/internal/contracts"
)

// heldAnswerer is a model that answers no question until its test releases it, one question for
// each release, or its run's context ends: it keeps the words of each question it was asked and of
// each whose context ended under it, how many it holds now and the most it ever held at once.
type heldAnswerer struct {
	release chan struct{}

	mu      sync.Mutex
	asked   []string
	ended   []string
	holding int
	most    int
}

func newHeldAnswerer() *heldAnswerer {
	return &heldAnswerer{release: make(chan struct{})}
}

// freeAfter answers every question still held once the test ends, before the server's own
// cleanup, which the test registered earlier and which then waits on none of them.
func (h *heldAnswerer) freeAfter(t *testing.T) {
	t.Cleanup(func() { close(h.release) })
}

func (h *heldAnswerer) Run(ctx context.Context, request going.Request, emit func(going.Event) error) error {
	h.mu.Lock()
	h.asked = append(h.asked, request.Words)
	h.holding++
	h.most = max(h.most, h.holding)
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.holding--
		h.mu.Unlock()
	}()
	select {
	case <-h.release:
		return emit(going.End(0, 0))
	case <-ctx.Done():
		h.mu.Lock()
		h.ended = append(h.ended, request.Words)
		h.mu.Unlock()
		return ctx.Err()
	}
}

func (h *heldAnswerer) Blocks() (blocks, known bool) { return false, false }

// state is what the model was asked, what ended under it, and how many it holds now and held at
// most.
func (h *heldAnswerer) state() (asked, ended []string, holding, most int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked), slices.Clone(h.ended), h.holding, h.most
}

// terminalWithin waits up to within for run's ending, and answers its frames' names and the error
// code it ended with, "" for an end.
func (rig goingRoutesRig) terminalWithin(t *testing.T, run string, within time.Duration) ([]string, string) {
	t.Helper()
	var names []string
	var code string
	waitUntil(t, within, "the ending of run "+run, func() bool {
		stored, _, err := rig.server.readGoingStream(context.Background(), run, 0)
		if err != nil {
			t.Fatalf("read the run's frames: %v", err)
		}
		if len(stored) == 0 || !stored[len(stored)-1].Event.Terminal() {
			return false
		}
		names = frameNames(stored)
		if last := stored[len(stored)-1].Event; last.Name == contracts.GoingEventError {
			code = fmt.Sprintf("%s", last.Data)
		}
		return true
	})
	return names, code
}

// TestTheShortAnswerAndJevAreBoundedInTheServer: the short answer of R3 and Jev's choice of R2,
// which stay in the server, each keep their bounds there (R15, R16): one question in flight for
// each person, a newer one ending the one before; 32 at once in the server, the 33rd waiting, its
// Jev call too, until one is answered; a stream past 4 MiB fails the answer; and an answer still
// unanswered at 30 s fails, the 30 s bounding the whole loop of its model calls, not each call.
func TestTheShortAnswerAndJevAreBoundedInTheServer(t *testing.T) {
	t.Run("one in flight for each person", func(t *testing.T) {
		held := newHeldAnswerer()
		rig := newGoingRoutesRig(t, held)
		held.freeAfter(t)
		first := askRun(t, rig, "alice", map[string]any{"words": "the words alice typed"})
		waitUntil(t, 10*time.Second, "alice's first question to reach the model", func() bool { asked, _, _, _ := held.state(); return len(asked) == 1 })
		askRun(t, rig, "bob", map[string]any{"words": "the words bob typed"})
		askRun(t, rig, "alice", map[string]any{"words": "the words alice typed next"})
		waitUntil(t, 10*time.Second, "alice's first question to end", func() bool { _, ended, _, _ := held.state(); return len(ended) == 1 })
		waitUntil(t, 10*time.Second, "the two newer questions to reach the model", func() bool { asked, _, _, _ := held.state(); return len(asked) == 3 })
		asked, ended, holding, _ := held.state()
		if ended[0] != "the words alice typed" || holding != 2 {
			t.Fatalf("the model was asked %q, ended %q and holds %d, want alice's first question ended and bob's and alice's next held", asked, ended, holding)
		}
		var cancelled bool
		if err := rig.database.Pool.QueryRow(context.Background(), `select cancelled_at is not null from going_requests where id = $1::uuid`, first).Scan(&cancelled); err != nil || !cancelled {
			t.Errorf("alice's first question's record reads cancelled %t (%v), want cancelled, as its author's cancel leaves it", cancelled, err)
		}
		time.Sleep(200 * time.Millisecond)
		if _, ended, _, _ := held.state(); len(ended) != 1 {
			t.Errorf("questions %q ended, want only alice's first", ended)
		}
	})

	t.Run("the 33rd waits", func(t *testing.T) {
		held := newHeldAnswerer()
		jev := newDecisionStandIn(t)
		jev.answer(t, decidedAnswer{action: going.ActionNone, confidence: 0.9, probabilities: map[string]float64{going.ActionNone: 0.95},
			about: going.ContextNone, display: going.DisplayProse, displayConfidence: 0.9})
		handler, database, deps, _ := newRegisteredTestServer(t, testServerOptions{answerer: held, decider: jev.decider(t, staticToken)})
		rig := goingRoutesRig{handler: handler, server: directServer(deps), database: database}
		held.freeAfter(t)
		for asker := range goingQuickSlots + 1 {
			askRun(t, rig, fmt.Sprintf("asker-%02d", asker), map[string]any{"words": fmt.Sprintf("the question of asker %02d", asker)})
		}
		waitUntil(t, 20*time.Second, "32 questions to reach the model", func() bool { _, _, holding, _ := held.state(); return holding == goingQuickSlots })
		time.Sleep(time.Second)
		if asked, _, _, _ := held.state(); len(asked) != goingQuickSlots || jev.calls.Load() != goingQuickSlots {
			t.Fatalf("with 32 questions held the model was asked %d and Jev %d, want 32 each: the 33rd waits, its Jev call too", len(asked), jev.calls.Load())
		}
		held.release <- struct{}{}
		waitUntil(t, 10*time.Second, "the 33rd question to reach the model once one was answered", func() bool { asked, _, _, _ := held.state(); return len(asked) == goingQuickSlots+1 })
		if _, _, _, most := held.state(); most != goingQuickSlots || jev.calls.Load() != goingQuickSlots+1 {
			t.Errorf("the model held %d questions at most and Jev was asked %d times, want 32 at most and 33", most, jev.calls.Load())
		}
	})

	t.Run("a 4 MiB stream fails", func(t *testing.T) {
		words := strings.Repeat("word ", (answerStreamBytes+64<<10)/5)
		answerer, model := newGoingModelAnswerer(t, goingtest.Reply{Text: words, Chunk: 32 << 10})
		rig := newGoingRoutesRig(t, answerer)
		names, code := rig.terminalWithin(t, askRun(t, rig, "alice", map[string]any{"words": "stream past the limit"}), 30*time.Second)
		if !strings.Contains(code, contracts.GoingErrorAnswerFailed) || model.Calls() != 1 {
			t.Fatalf("a stream past 4 MiB ended %v (%s) after %d model calls, want %s after the one", names, code, model.Calls(), contracts.GoingErrorAnswerFailed)
		}

		within := strings.Repeat("word ", (answerStreamBytes-512<<10)/5)
		answerer, model = newGoingModelAnswerer(t, goingtest.Reply{Text: within, Chunk: 32 << 10}, goingtest.Reply{Final: &goingtest.Final{Answer: "A short answer."}})
		rig = newGoingRoutesRig(t, answerer)
		if names, code := rig.terminalWithin(t, askRun(t, rig, "alice", map[string]any{"words": "stream within the limit"}), 30*time.Second); code != "" {
			t.Errorf("a stream within 4 MiB ended %v (%s) after %d model calls, want it answered", names, code, model.Calls())
		}
	})

	t.Run("30 s fails", func(t *testing.T) {
		answerer, _ := newGoingModelAnswerer(t, goingtest.Reply{Silent: true})
		rig := newGoingRoutesRig(t, answerer)
		asked := time.Now()
		names, code := rig.terminalWithin(t, askRun(t, rig, "alice", map[string]any{"words": "never answered"}), goingQuickTime+goingStreamGrace+5*time.Second)
		took := time.Since(asked)
		if !strings.Contains(code, contracts.GoingErrorAnswerDidNotArrive) {
			t.Fatalf("an answer the model never wrote ended %v (%s), want %s", names, code, contracts.GoingErrorAnswerDidNotArrive)
		}
		if took < 29*time.Second || took > 30*time.Second+goingStreamGrace {
			t.Errorf("the answer failed %s after it was asked, want at its 30 s", took)
		}
	})

	t.Run("a loop of calls each under 30 s fails at 30 s", func(t *testing.T) {
		// Each call streams for 18 s, two pauses of 9 s between its frames: the search call ends
		// 18 s in, and the answer call would end 36 s in.
		const pause = 9 * time.Second
		answerer, model := newGoingModelAnswerer(t,
			goingtest.Reply{ToolCalls: []goingtest.ToolCall{{ID: "call_1", Name: contracts.GoingSearchTool, Arguments: `{"q":"pelicans"}`}}, Chunk: 1 << 10, Pause: pause},
			goingtest.Reply{Final: &goingtest.Final{Answer: "An answer that comes too late."}, Chunk: 1 << 10, Pause: pause},
		)
		rig := newGoingRoutesRig(t, answerer)
		asked := time.Now()
		names, code := rig.terminalWithin(t, askRun(t, rig, "alice", map[string]any{"words": "search, then answer slowly"}), goingQuickTime+goingStreamGrace+5*time.Second)
		took := time.Since(asked)
		if !strings.Contains(code, contracts.GoingErrorAnswerDidNotArrive) || model.Calls() != 2 {
			t.Fatalf("a loop of two 18 s calls ended %v (%s) after %d model calls, want %s in the second", names, code, model.Calls(), contracts.GoingErrorAnswerDidNotArrive)
		}
		if took < 29*time.Second || took > 30*time.Second+goingStreamGrace {
			t.Errorf("the loop failed %s after it was asked, want at its 30 s", took)
		}
	})
}

// answerStreamBytes is going's answerStreamLimit, 4 MiB, which the stream rows cross and keep
// within.
const answerStreamBytes = 4 << 20
