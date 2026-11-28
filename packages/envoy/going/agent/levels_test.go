package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/agentloop"
	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
)

// TestEachLevelSetsItsBudgetsAndEffort: Fast, Standard and Deep each bound a run's model calls,
// its working time and each call's output tokens, reserve each call's reasoning tokens in the
// pacer, and ask each call for their effort (R10, the spec's Design): a run that has made its
// level's calls, or worked its level's time, is spent, and one a call or a millisecond short is
// not; the last call it may make is offered no tool, so it must answer; the pacer reserves for a
// call a token for each charsPerToken characters of its prompt, the level's output tokens and its
// reasoning tokens; and the request a step sends the chat model carries the level's output tokens
// and effort, as the fake model reads them off the wire.
func TestEachLevelSetsItsBudgetsAndEffort(t *testing.T) {
	want := map[string]struct {
		calls     int
		time      time.Duration
		tokens    int
		reasoning int
		effort    string
	}{
		"fast":     {6, 90 * time.Second, 1000, 353, "low"},
		"standard": {24, 5 * time.Minute, 2500, 865, "medium"},
		"deep":     {80, 20 * time.Minute, 5000, 1967, "high"},
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
		t.Run(fmt.Sprintf("%s reserves %d reasoning tokens a call", name, w.reasoning), func(t *testing.T) {
			const prompt = 40 * charsPerToken
			if got := level.estimate(prompt); got != 40+w.tokens+w.reasoning {
				t.Errorf("the pacer reserves %d tokens for a %s call of %d prompt characters, want 40 for the prompt, %d output and %d reasoning",
					got, name, prompt, w.tokens, w.reasoning)
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
// a minute, a call of 45,000 tokens goes at once, and the next, of 750, waits the second its share
// takes to earn them back, which a call with half a second left is refused for. The pacer's clock
// stands still, so the share earns nothing back between them.
func TestTheAgentPacesItsShareOfTheQuota(t *testing.T) {
	now := time.Now()
	pacer := going.NewPacer(60_000, quotaShare, func() time.Time { return now })
	var charges []going.Charge
	draw := func(ctx context.Context, tokens int) error {
		model := pacer.Paced(agentloop.Model{Endpoint: agentloop.Endpoint{Token: func(context.Context) (string, error) { return "a token", nil }}},
			func() int { return tokens }, func(charge going.Charge) { charges = append(charges, charge) })
		_, err := model.Endpoint.Token(ctx)
		return err
	}
	if err := draw(t.Context(), 45_000); err != nil || len(charges) != 1 || charges[0] != (going.Charge{Tokens: 45_000, Drawn: 45_000}) {
		t.Fatalf("45,000 tokens of a 60,000 quota answered %v and were charged %+v, want them drawn at once", err, charges)
	}
	short, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if err := draw(short, 750); !errors.Is(err, going.ErrQuotaPastDeadline) {
		t.Errorf("750 tokens past the share with half a second left answered %v, want %v: they wait a second", err, going.ErrQuotaPastDeadline)
	}
}

// TestATimeStopSaysWhenTheModelWasBusy: a run stopped at its level's working time whose pacer
// held its model calls for more than half of it says so, and for how long, after the bound's code
// the web reads; a run the pacer held for half of it or less says the time alone.
func TestATimeStopSaysWhenTheModelWasBusy(t *testing.T) {
	const idle = "TIME_BUDGET: this run worked the 1m30s a fast run may; Continue starts a follow-up with a new budget"
	for _, c := range []struct {
		name string
		used agentwire.Budget
		want string
	}{
		{
			name: "held for 52 s of its 90 s", used: agentwire.Budget{WorkingMS: 90_400, PaceMS: 52_300},
			want: "TIME_BUDGET: this run worked the 1m30s a fast run may, and the model was busy for 52s of it; Continue starts a follow-up with a new budget",
		},
		{name: "held for half of its 90 s", used: agentwire.Budget{WorkingMS: 90_000, PaceMS: 45_000}, want: idle},
		{name: "never held", used: agentwire.Budget{WorkingMS: 90_000}, want: idle},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Levels["fast"].spent("fast", c.used); got != c.want {
				t.Errorf("the run that spent %+v stopped with %q, want %q", c.used, got, c.want)
			}
		})
	}
}
