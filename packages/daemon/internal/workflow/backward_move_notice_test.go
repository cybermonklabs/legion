package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A worker's own move back tells the architect that it is that move whatever its words. Main's
// notice said "moved back to <to> by the <role>" before the role's words (noticeFor at d46ec4e0),
// and the record said it only through the reason block, which a move whose role gave no words
// leaves out. Such a notice named the role, the phase and the phase it moved to alone: what a
// tester's fail, or a reviewer's request for changes, names when its completion carries no
// summary. A worker's move back says so (moved_back: true), with words or none; a completion that
// moves the issue to the same earlier phase does not.
func TestAWorkersMoveBackSaysSoWithNoWords(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		trigger TriggerKind
		from    phase.Phase
		handoff record.PhaseRow
		reason  string
		back    bool
	}{
		{name: "a tester's move back with no words", trigger: TriggerBackward, from: phase.Testing, back: true},
		{name: "a reviewer's move back with no words", trigger: TriggerBackward, from: phase.Reviewing, back: true},
		{name: "a tester's move back with its words", trigger: TriggerBackward, from: phase.Testing, reason: "The fuzz suite hangs.", back: true},
		{name: "a tester's fail", trigger: TriggerTesterFailed, from: phase.Testing,
			handoff: record.PhaseRow{Role: claim.RoleTester, Verdict: "fail"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice, err := record.NoticeOf(NoticeFor("LEGION-208", tc.trigger, tc.from, phase.Implementing, tc.handoff, tc.reason, now))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(notice)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			back, said := payload["moved_back"].(bool)
			if tc.back && (!said || !back) {
				t.Errorf("the phase-finished notice of %s is %s, want moved_back: true: nothing else tells it from a completion that moves the issue back", tc.name, raw)
			}
			if !tc.back && said {
				t.Errorf("the phase-finished notice of %s is %s, want no moved_back: the worker completed its phase", tc.name, raw)
			}
		})
	}
}
