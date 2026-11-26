package answer

import (
	"context"
	"strings"
	"testing"
)

// The last model call offers no tool and asks for nothing again when its reply breaks the form's
// rules, so a not_answered or needs_work reply naming no need there ends the answer malformed
// and the asker never hears what is needed (proof run 44848088186, A4 under the files store:
// the planner's last call replied not_answered with needs ""). The message that call carries is
// the one place the model can be told the rule in time, so it names it: needs, never empty for
// not_answered and needs_work.
func TestTheLastCallsMessageNamesTheNeedsRule(t *testing.T) {
	s := newStandIn(t, anthropicStream("end_turn", anthropicUsage{Input: 2, Output: 3},
		anthropicText(`{"kind":"not_answered","text":"I cannot tell from here.","needs":"My working copy."}`)))
	reply, err := Answer(context.Background(), anthropicRequest(t, s, &stubGH{}, 1))
	if err != nil || reply.Fault != "" || reply.Kind != NotAnswered {
		t.Fatalf("reply %+v, %v; want not_answered on the one call", reply, err)
	}
	got := s.requests()
	if len(got) != 1 {
		t.Fatalf("%d model calls, want 1", len(got))
	}
	sent := messages(t, got[0])
	newest := blocks(sent[len(sent)-1])
	text, _ := newest[len(newest)-1]["text"].(string)
	for _, word := range []string{"needs", "not_answered", "needs_work"} {
		if !strings.Contains(text, word) {
			t.Errorf("the last call's message is %q; want it to name the needs rule (%q missing): needs is never empty for not_answered and needs_work", text, word)
		}
	}
}
