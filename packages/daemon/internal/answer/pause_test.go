package answer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// A run the project's pause comes during makes no model call and no gh call after it, and Answer
// answers ErrPaused, with no fault, no turns and what the calls made so far cost: paused before
// the first model call, no call at all; as a call asking for gh ends, that gh call does not run;
// during the gh call, the call finishes and no next model call starts; during one of two gh calls
// a model call asked for, which agentloop runs one at a time in either order, that call finishes,
// the other does not run, and no next model call starts; as a reply the form refuses ends, it is
// not asked for again. Each stand-in holds the reply a next call would get, so a run that went on
// would make it.
func TestAPauseEndsTheRunBeforeItsNextModelCallOrGhCall(t *testing.T) {
	const answered = `{"kind":"answered","text":"PR 7 is open.","needs":""}`
	asksForGH := func() []reply {
		return []reply{
			anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5}, anthropicGH("toolu_1", "pr", "view", "7")),
			anthropicStream("end_turn", anthropicUsage{Input: 12, Output: 6}, anthropicText(answered)),
		}
	}
	refusedFirst := []reply{
		anthropicStream("end_turn", anthropicUsage{Input: 10, Output: 5}, anthropicText(`{"kind":"not_answered","text":"I cannot tell from here.","needs":""}`)),
		anthropicStream("end_turn", anthropicUsage{Input: 12, Output: 6}, anthropicText(answered)),
	}
	asksForTwoGH := []reply{
		anthropicStream("tool_use", anthropicUsage{Input: 10, Output: 5}, anthropicGH("toolu_1", "pr", "view", "7"), anthropicGH("toolu_2", "pr", "checks", "7")),
		anthropicStream("end_turn", anthropicUsage{Input: 12, Output: 6}, anthropicText(answered)),
	}
	for _, c := range []struct {
		name    string
		replies []reply
		// pausedFrom is the number of model calls made from which the project reads paused, -1
		// for none; inGH pauses it during the first gh call that runs instead.
		pausedFrom        int
		inGH              bool
		wantCalls, wantGH int
	}{
		{name: "paused before the first model call", replies: asksForGH(), pausedFrom: 0},
		{name: "paused as a model call asking for gh ends", replies: asksForGH(), pausedFrom: 1, wantCalls: 1},
		{name: "paused during the gh call", replies: asksForGH(), pausedFrom: -1, inGH: true, wantCalls: 1, wantGH: 1},
		{name: "paused during one of two gh calls a model call asked for", replies: asksForTwoGH, pausedFrom: -1, inGH: true, wantCalls: 1, wantGH: 1},
		{name: "paused as a reply the form refuses ends", replies: refusedFirst, pausedFrom: 1, wantCalls: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newStandIn(t, c.replies...)
			var pausedInGH atomic.Bool
			gh := &stubGH{}
			if c.inGH {
				gh.onCall = func(int) { pausedInGH.Store(true) }
			}
			req := anthropicRequest(t, s, gh, 3)
			req.Paused = func() bool {
				return pausedInGH.Load() || (c.pausedFrom >= 0 && len(s.requests()) >= c.pausedFrom)
			}
			reply, err := Answer(context.Background(), req)
			if !errors.Is(err, ErrPaused) || errors.Is(err, context.Canceled) {
				t.Fatalf("Answer = %+v, %v; want ErrPaused", reply, err)
			}
			if reply.Fault != "" || len(reply.Append.Turns) != 0 {
				t.Errorf("reply %+v; want no fault and no turns", reply)
			}
			if made := len(s.requests()); made != c.wantCalls || reply.Calls != c.wantCalls {
				t.Errorf("%d model calls made, the reply counts %d; want %d", made, reply.Calls, c.wantCalls)
			}
			if calls := gh.made(); len(calls) != c.wantGH {
				t.Errorf("gh calls %+v; want %d", calls, c.wantGH)
			}
			if (reply.CostUSD > 0) != (c.wantCalls > 0) {
				t.Errorf("the reply costs %v after %d model calls; want what they cost", reply.CostUSD, c.wantCalls)
			}
		})
	}
}
