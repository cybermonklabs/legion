package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
)

// TestEachLevelSetsItsBudgetsAndEffort: Fast, Standard and Deep each bound a run's model calls,
// its working time and each call's output tokens, and ask each call for their effort (R10, the
// spec's Design): a run that has made its level's calls, or worked its level's time, is spent, and
// one a call or a millisecond short is not; the last call it may make is offered no tool, so it
// must answer; and the request a step sends the chat model carries the level's output tokens and
// effort, as the fake model reads them off the wire.
func TestEachLevelSetsItsBudgetsAndEffort(t *testing.T) {
	want := map[string]struct {
		calls  int
		time   time.Duration
		tokens int
		effort string
	}{
		"fast":     {6, 90 * time.Second, 4000, "low"},
		"standard": {24, 5 * time.Minute, 8000, "medium"},
		"deep":     {80, 20 * time.Minute, 16000, "high"},
	}
	fake := agenttest.New(t, func(*agenttest.Request) agenttest.Reply { return agenttest.Reply{Text: "An answer."} })
	sent := func(t *testing.T, level Level) *agenttest.Request {
		t.Helper()
		before := fake.Count()
		request := level.request(fake.Model(), agentwire.Budget{})
		request.Input = []agentloop.Message{{Role: agentloop.User, Blocks: []agentloop.Block{{Kind: agentloop.TextBlock, Text: "What is new?"}}}}
		if result, err := agentloop.Run(context.Background(), request); err != nil || result.Fault != "" {
			t.Fatalf("a step of the level: %+v, %v", result, err)
		}
		requests := fake.Requests()
		if len(requests) != before+1 {
			t.Fatalf("the step made %d model calls, want 1", len(requests)-before)
		}
		return requests[len(requests)-1]
	}
	for _, name := range []string{"fast", "standard", "deep"} {
		level, w := Levels[name], want[name]
		t.Run(fmt.Sprintf("%s makes %d model calls", name, w.calls), func(t *testing.T) {
			if reason := level.spent(name, agentwire.Budget{Calls: w.calls - 1}); reason != "" {
				t.Errorf("after %d calls the run is spent: %q", w.calls-1, reason)
			}
			if reason := level.spent(name, agentwire.Budget{Calls: w.calls}); !strings.HasPrefix(reason, ReasonCallBudget) {
				t.Errorf("after %d calls the run is spent with %q, want %s", w.calls, reason, ReasonCallBudget)
			}
			if bounds := level.request(agentloop.Model{}, agentwire.Budget{Calls: w.calls - 1}).Bounds; bounds.Calls != 1 {
				t.Errorf("the run's last call is given %d calls, want 1, so it is offered no tool and answers", bounds.Calls)
			}
		})
		t.Run(fmt.Sprintf("%s works %s", name, w.time), func(t *testing.T) {
			short := agentwire.Budget{WorkingMS: (w.time - time.Millisecond).Milliseconds()}
			if reason := level.spent(name, short); reason != "" {
				t.Errorf("a millisecond short of %s the run is spent: %q", w.time, reason)
			}
			if reason := level.spent(name, agentwire.Budget{WorkingMS: w.time.Milliseconds()}); !strings.HasPrefix(reason, ReasonTimeBudget) {
				t.Errorf("after %s the run is spent with %q, want %s", w.time, reason, ReasonTimeBudget)
			}
			if bounds := level.request(agentloop.Model{}, short).Bounds; bounds.Time != time.Millisecond {
				t.Errorf("a millisecond short of %s, the step is given %s, want the millisecond left", w.time, bounds.Time)
			}
		})
		t.Run(fmt.Sprintf("%s writes %d output tokens a call", name, w.tokens), func(t *testing.T) {
			if got := sent(t, level).Field("max_tokens"); got != float64(w.tokens) {
				t.Errorf("a %s step asks for max_tokens %v, want %d", name, got, w.tokens)
			}
		})
		t.Run(fmt.Sprintf("%s asks for %s effort", name, w.effort), func(t *testing.T) {
			if got := sent(t, level).Field("reasoning_effort"); got != w.effort {
				t.Errorf("a %s step asks for reasoning_effort %v, want %s", name, got, w.effort)
			}
		})
	}
}

// TestTheAgentPacesItsShareOfTheQuota: the agent paces its model calls to 75% of the chat
// deployment's tokens a minute, the server's short answers taking the rest: of a quota of 60,000
// a minute, 45,000 tokens go at once, and the next 750 wait the second 750 tokens of 45,000 a
// minute take.
func TestTheAgentPacesItsShareOfTheQuota(t *testing.T) {
	pacer := newPacer(60_000)
	now := time.Unix(1_700_000_000, 0)
	if _, wait, ok := pacer.delay(now, 45_000); !ok || wait != 0 {
		t.Fatalf("45,000 tokens of a 60,000 quota wait %v (ok %t), want none", wait, ok)
	}
	if _, wait, ok := pacer.delay(now, 750); !ok || wait < 990*time.Millisecond || wait > 1010*time.Millisecond {
		t.Fatalf("750 tokens past the share wait %v (ok %t), want 1 s", wait, ok)
	}
	if newPacer(0) != nil {
		t.Error("a deployment that names no quota is paced")
	}
}
