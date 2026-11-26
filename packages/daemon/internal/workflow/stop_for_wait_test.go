package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// ruleEngine is testEngine under rule: the engine reads its rule from its Config alone
// (Config.SlotRule), so the guard on every phase start and the admission pass's stop read the rule
// the test names.
func ruleEngine(rule record.SlotRule) *Engine {
	return New(record.NewStore(), Config{
		Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), ReviewRoundCap: 3, Linger: time.Hour,
		ProductionCheckRole: claim.RoleTester, ReviewAppLogin: testReviewApp, Repositories: testRepositories, SlotRule: rule,
		Clock: func() time.Time { return cwAt },
	}, nil)
}

// R2: a wait that stops the phase under way (record.SlotRule.WaitStopsPhase) is
// cybermonklabs/legion#4592's, and the rule value carries it. Under the daemon's own rule an open
// Dispatch child holds the start the tester's pass raises and the parent moves into
// awaiting_children holding it, and the admission pass's stop of a parent in the middle of its
// phase moves it too; under each rule a recorded window ran, whose release came before #4592, the
// child holds nothing: the move starts the reviewer and the stop moves nothing. At admitted, the
// architect's own phase, the stop moves no parent under either rule, since an open child holds no
// first phase of a started parent (record.OpenChildHolds).
func TestAWaitThatStopsThePhaseIsTheRulesChoice(t *testing.T) {
	rules := []record.SlotRule{record.MainRule}
	for _, name := range []string{"legion-v20.42.5", "legion-v19.57.3", "legion-v22.14.1"} {
		rule, kept := record.SlotRuleNamed(name)
		if !kept {
			t.Fatalf("the repository keeps no rule %s", name)
		}
		rules = append(rules, rule)
	}
	pool := migratedPool(t)
	for i, rule := range rules {
		engine := ruleEngine(rule)
		stops := rule.WaitStopsPhase
		if stops != (rule.Name == record.MainRule.Name) {
			t.Fatalf("the rule %s stops the phase under way: %t, want only main's to", rule.Name, stops)
		}
		t.Run(rule.Name+": the tester's pass", func(t *testing.T) {
			key, child, number := fmt.Sprintf("LEGION-%d", 600+i), fmt.Sprintf("LEGION-%d", 900+i), 600+i
			cwSeedParent(t, pool, key, phase.Testing, number)
			seedMirrorChild(t, pool, child, "todo", key)

			cwTransition(t, pool, engine, key, TriggerTesterPassed, phase.Reviewing,
				record.PhaseRow{Issue: key, Role: claim.RoleTester, Summary: "the checks passed", Verdict: "pass"}, "")

			got := storedIssue(t, pool, key)
			switch {
			case stops && (got.Phase != phase.AwaitingChildren || got.ChildrenWait == nil || got.ChildrenWait.Start != phase.Reviewing):
				t.Fatalf("%s under %s = phase %s, held start %+v; want awaiting_children holding reviewing, since %s is open",
					key, rule.Name, got.Phase, got.ChildrenWait, child)
			case !stops && (got.Phase != phase.Reviewing || got.ChildrenWait != nil):
				t.Fatalf("%s under %s = phase %s, held start %+v; want reviewing and nothing held: the rule's wait stops no phase",
					key, rule.Name, got.Phase, got.ChildrenWait)
			}
			if told := noticesOfKind(t, pool, key, AwaitingChildrenNoticeKind); (len(told) == 1) != stops {
				t.Fatalf("awaiting-children notices of %s under %s = %+v, want one exactly where the rule's wait stops the phase", key, rule.Name, told)
			}
		})
		t.Run(rule.Name+": the admission pass's stop", func(t *testing.T) {
			implementing := cwSeedParent(t, pool, fmt.Sprintf("LEGION-%d", 620+i), phase.Implementing, 620+i)
			admitted := cwSeedParent(t, pool, fmt.Sprintf("LEGION-%d", 640+i), phase.Admitted, 640+i)
			open := []record.OpenChild{{Key: fmt.Sprintf("LEGION-%d", 960+i), Status: "todo"}}
			moved := map[string]bool{}
			if err := store.Transact(engine.OpenFact(t.Context()), pool, func(ctx context.Context, tx pgx.Tx) error {
				for _, parent := range []record.Issue{implementing, admitted} {
					stopped, err := engine.StopForChildren(ctx, tx, parent, open, nil)
					if err != nil {
						return err
					}
					moved[parent.Key] = stopped
				}
				return engine.SettleFact(ctx, tx)
			}); err != nil {
				t.Fatalf("the stops under %s: %v", rule.Name, err)
			}
			if moved[admitted.Key] || storedIssue(t, pool, admitted.Key).Phase != phase.Admitted {
				t.Fatalf("the stop of %s at admitted under %s moved it, want it left: no open child holds a first phase", admitted.Key, rule.Name)
			}
			if got := storedIssue(t, pool, implementing.Key); moved[implementing.Key] != stops || (got.Phase == phase.AwaitingChildren) != stops {
				t.Fatalf("the stop of %s in implementing under %s moved it %t, phase %s; want a move into awaiting_children exactly where the rule's wait stops the phase",
					implementing.Key, rule.Name, moved[implementing.Key], got.Phase)
			}
		})
	}
}
