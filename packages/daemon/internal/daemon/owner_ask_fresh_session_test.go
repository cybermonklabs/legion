package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// toldMark is the mark of whom a merge-forward ask reached, as the record of the pull request holds
// it (record.MergeForward.Told): the launch and the session of the owner's claim, the one rule of
// told an owner-step's run holds too (record.OwnerTold).
type toldMark struct {
	Launch  uint64 `json:"launch"`
	Session string `json:"session"`
}

// An ask delivered to the owner records whom it reached by the one rule an owner-step's run records
// it by: the launch and the session of the owner's claim as it was read before the publish. On
// `implementer:owner reviewer` at the reviewer's step, the ask reaches the owner at its launch 1.
// Its claim runs in a session, which the mark names beside the launch; or its claim recorded no
// session, and the mark names the launch alone, so the catch-up of that same launch judges the ask
// told by its launch, as it judges an owner-step (workflow's toldThisSession).
func TestAnAskToTheOwnerRecordsWhomItReachedAsAnOwnerStepDoes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		session string
		want    toldMark
	}{
		{name: "a claim that runs in a session", session: ownerHoldSession, want: toldMark{Launch: ownerHoldLaunch, Session: ownerHoldSession}},
		{name: "a claim that recorded no session", want: toldMark{Launch: ownerHoldLaunch}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newOwnerHoldRig(t, phase.Reviewing, record.TaskPlace{Kind: record.PlaceStep, Position: 2}, "implementer:owner", "reviewer")
			r.started(t, "step", 1, true)
			r.relaunched(t, ownerHoldLaunch, tc.session)
			r.askedOf(t, r.owner, phase.Reviewing)
			if err := r.told(t, mergeForwardAsk(r.owner, phase.Reviewing)); err != nil {
				t.Fatalf("the ask to the owner = %v", err)
			}
			if got := r.published(); len(got) != 1 {
				t.Fatalf("the ask published %v, want one: this case proves nothing", got)
			}
			var raw []byte
			if err := r.pool.QueryRow(context.Background(), "select coalesce(merge_forward->'told', 'null'::jsonb) from pull_requests where issue = 'LEGION-1'").Scan(&raw); err != nil {
				t.Fatalf("read the told mark of LEGION-1's merge forward: %v", err)
			}
			var got toldMark
			if err := json.Unmarshal(raw, &got); err != nil || got != tc.want {
				t.Fatalf("the merge forward's told mark is %s (%v), want %+v", raw, err, tc.want)
			}
		})
	}
}
