package replay

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A replay under the rule cover boots the start state the record's rule left onto the rule, as the
// first boot of a release that runs it does (spec v4 R9; FirstBoot): an issue the record had
// released for a wait at the window's start takes its slot back at the start, where the recorded
// rule leaves it without one. The test replays linkDuringImplementingWindow from 10:15, inside
// ACME-1's wait on ACME-3 (an issue Legion is not given, which no cover can start, so ACME-1 is a
// counted waiter), and reads the slots and the mark of a release at the first state the snapshot
// timeline takes, at the window's start: under main ACME-1 holds no slot and carries the mark;
// under cover it holds its slot and carries no mark.
func TestTheFirstBootOfCoverGivesAReleasedWaiterItsSlot(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	window := linkDuringImplementingWindow(t)
	start := clock(10, 15)
	window.Before = Duration(time.Duration(window.Before) + start.Sub(window.From))
	window.From = start
	for _, c := range []struct {
		rule  record.SlotRule
		holds bool
	}{{mainRule(t), false}, {replayedRule(t, "cover"), true}} {
		t.Run(c.rule.Name, func(t *testing.T) {
			t.Parallel()
			read := false
			var first time.Time
			var holds, marked bool
			cfg := RunConfig{Window: window, Rule: c.rule, Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn, Out: t.TempDir()}
			cfg.atSettled = func(ctx context.Context, st *store.Store, at time.Time) error {
				if read {
					return nil
				}
				read, first = true, at
				return store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
					records := record.NewStore()
					slots, err := records.Slots(ctx, tx)
					if err != nil {
						return err
					}
					for _, slot := range slots {
						holds = holds || slot.Issue == "ACME-1"
					}
					issue, err := records.Issue(ctx, tx, "ACME-1")
					if err != nil || issue == nil {
						return err
					}
					marked = issue.ReleasedForWaitAt != nil
					return nil
				})
			}
			if _, err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("replay the window from %s under %s: %v", stamp(start), c.rule.Name, err)
			}
			if !read || !first.Equal(start) {
				t.Fatalf("the snapshot timeline's first state is at %s (read %t), want the window's start %s", stamp(first), read, stamp(start))
			}
			if holds != c.holds || marked == c.holds {
				t.Errorf("at %s ACME-1 holds a slot: %t, carries the mark of a release for a wait: %t; want a slot %t", stamp(start), holds, marked, c.holds)
			}
		})
	}
}
