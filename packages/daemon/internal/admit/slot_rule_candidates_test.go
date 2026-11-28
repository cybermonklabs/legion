package admit

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// keptRule is the rule the repository keeps under name (record.SlotRuleNamed), and fails t where it
// keeps none.
func keptRule(t *testing.T, name string) record.SlotRule {
	t.Helper()
	rule, kept := record.SlotRuleNamed(name)
	if !kept {
		t.Fatalf("the repository keeps no rule named %q", name)
	}
	return rule
}

// keptWaitGraces is each grace a rule the repository keeps names (record.SlotRule.WaitGrace), in
// the order of record.SlotRules, which the rows of the grace's reader take one each as their own, so
// that each kept rule with a grace makes a row's choice the row's way
// (TestEveryKeptRuleTakesTheReadersRows); none where the repository keeps no such rule, which leaves
// no row that moves the grace and fails TestTheChoicesOfARuleAreTheReadersRows.
func keptWaitGraces() []time.Duration {
	var graces []time.Duration
	for _, rule := range record.SlotRules() {
		if rule.WaitGrace > 0 && !slices.Contains(graces, rule.WaitGrace) {
			graces = append(graces, rule.WaitGrace)
		}
	}
	return graces
}

// firstSlotTables is a replay's rule tables as far as the first slots go (RuleTables): the moment
// of each issue's first slot in its generation, by key, held as data and added to the facts of a
// pass under a rule that places the line's returned part by it (record.LineReturnedByFirstSlot,
// record.LineFacts.FirstSlots). The daemon keeps no such moment and runs no such rule; a replay
// keeps them in a table of its own database (the replay's ruleTables), whose keeping that
// package's tests read.
type firstSlotTables map[string]time.Time

func (tables firstSlotTables) ReadRuleFacts(_ context.Context, _ pgx.Tx, _ []record.Issue, facts *record.LineFacts) error {
	if facts.Rule.OrMain().Order == record.LineReturnedByFirstSlot {
		facts.FirstSlots = tables
	}
	return nil
}

func (firstSlotTables) KeepCovers(context.Context, pgx.Tx, map[string]string) error { return nil }

// graceWaiter, graceBlocker and graceQueued are the issues of the grace's test: a root holding the
// one slot in implementing, an issue Legion is not given that it is linked to, and a root handed
// to Legion that waits for the slot.
const graceWaiter, graceBlocker, graceQueued = "ACME-1", "ACME-2", "ACME-3"

// graceWorld seeds the grace's test under a cap of one and answers its store, a link of the waiter
// to the issues blockers name on the mirror (none takes the link off), and a pass at a moment, a
// controller tick through the daemon's own chain of handlers under rule on a clock standing there.
func graceWorld(t *testing.T, rule record.SlotRule) (*pgxpool.Pool, func(blockers ...string), func(at time.Time, id string)) {
	t.Helper()
	pool := migratedPool(t)
	acmeRoot(t, pool, graceWaiter, phase.Implementing, "in_progress", "a", nil, nil)
	takeSlot(t, pool, graceWaiter)
	seedMirrorIn(t, pool, acmeProject, graceBlocker, "in_progress")
	acmeQueuedRoot(t, pool, graceQueued, "b")
	admission := ruledAdmission(t, 1, rule, silent())
	now := fixedNow
	admission.now = func() time.Time { return now }
	seq := int64(1)
	link := func(blockers ...string) {
		seq++
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutDispatchIssue(context.Background(), tx, record.DispatchReading{Key: graceWaiter,
				Project: acmeProject, Seq: seq, Blockers: record.Carry(blockers)}); err != nil {
				t.Fatalf("link %s to %v on the mirror: %v", graceWaiter, blockers, err)
			}
		})
	}
	pass := func(at time.Time, id string) {
		now = at
		tickThrough(t, pool, admission, id)
	}
	return pool, link, pass
}

// graceWord is what the grace's test reads after a pass: whether the waiter holds the slot and
// carries the mark of a release for a wait, whether the queued root's tree stands open, which is
// the slot the waiter gave back taken, and the start of the wait its slot marks.
func graceWord(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	wait := "no slot of " + graceWaiter + " marks a wait"
	for _, slot := range slots(t, pool) {
		if slot.Issue == graceWaiter && slot.WaitsSince != nil {
			wait = "the slot of " + graceWaiter + " marks a wait since " + slot.WaitsSince.UTC().Format("15:04:05")
		}
	}
	return slotWord(t, pool, graceWaiter) + "; " + markWord(t, pool, graceWaiter) + "; " + treeWord(t, pool, graceQueued) + "; " + wait
}

// Under the rule wait-grace a wait on another issue gives the slot back only once it has lasted
// the grace (record.SlotRule.WaitGrace): the pass that finds a slot holder waiting marks the
// wait's start on its slot (record.Slot.WaitsSince) and keeps its slot, so a wait that ends inside
// the grace frees no slot, writes no mark of a release for a wait and puts the holder in no place
// of the line; and the first pass at or after the wait's start plus the grace gives the slot back
// as the daemon's own rule gives it back at once, the mark of the release at that pass's moment,
// where the holder stands in the line's returned part. Under a cap of one, ACME-1 holds the slot in
// implementing and ACME-3, a root handed to Legion, waits for it. A person links ACME-1 to ACME-2
// at 12:00 and takes the link off at 12:01, and links it again at 12:10, which the passes at
// 12:14:59 and 12:15 read, the grace being five minutes. Under main the first pass, at 12:00,
// gives the slot back and ACME-3 takes it.
func TestAWaitGivesItsSlotBackOnlyOnceItHasLastedTheGrace(t *testing.T) {
	t.Run("wait-grace", func(t *testing.T) {
		rule := keptRule(t, "wait-grace")
		if rule.WaitGrace != 5*time.Minute {
			t.Fatalf("the rule wait-grace keeps a grace of %s, and this test is worked for five minutes", rule.WaitGrace)
		}
		pool, link, pass := graceWorld(t, rule)
		step := func(at time.Time, id, want string) {
			t.Helper()
			pass(at, id)
			if got := graceWord(t, pool); got != want {
				t.Fatalf("after the pass at %s:\n got %s\nwant %s", at.Format("15:04:05"), got, want)
			}
		}
		link(graceBlocker)
		step(fixedNow, "linked", "ACME-1 holds a slot; ACME-1 carries no mark of a release for a wait; the tree of ACME-3 is not open; "+
			"the slot of ACME-1 marks a wait since 12:00:00")
		link()
		step(fixedNow.Add(time.Minute), "unlinked", "ACME-1 holds a slot; ACME-1 carries no mark of a release for a wait; "+
			"the tree of ACME-3 is not open; no slot of ACME-1 marks a wait")
		if line, _, _ := lineUnder(t, pool, rule); lineWord(line) != "the line is [ACME-3]" {
			t.Fatalf("after the wait of a minute %s, want ACME-1 in no place of it", lineWord(line))
		}

		again := fixedNow.Add(10 * time.Minute)
		link(graceBlocker)
		step(again, "linked-again", "ACME-1 holds a slot; ACME-1 carries no mark of a release for a wait; the tree of ACME-3 is not open; "+
			"the slot of ACME-1 marks a wait since 12:10:00")
		step(again.Add(rule.WaitGrace-time.Second), "inside-the-grace", "ACME-1 holds a slot; ACME-1 carries no mark of a release for a wait; "+
			"the tree of ACME-3 is not open; the slot of ACME-1 marks a wait since 12:10:00")
		end := again.Add(rule.WaitGrace)
		step(end, "at-the-grace-end", "ACME-1 holds no slot; ACME-1 carries the mark of a release for a wait; the tree of ACME-3 is open; "+
			"no slot of ACME-1 marks a wait")
		if mark := issue(t, pool, graceWaiter).ReleasedForWaitAt; mark == nil || !mark.Equal(end) {
			t.Fatalf("ACME-1's release for its wait is marked at %v, want the grace's end, %s", mark, end.Format("15:04:05"))
		}
		line, _, _ := lineUnder(t, pool, rule)
		if len(line) != 1 || line[0].Issue.Key != graceWaiter || line[0].Part != record.PartReturned || !line[0].Since.Equal(end) {
			t.Fatalf("after the grace %s (%+v), want ACME-1 alone, in the returned part since the grace's end", lineWord(line), line)
		}
	})
	t.Run("main", func(t *testing.T) {
		pool, link, pass := graceWorld(t, record.MainRule)
		link(graceBlocker)
		pass(fixedNow, "linked")
		if got, want := graceWord(t, pool), "ACME-1 holds no slot; ACME-1 carries the mark of a release for a wait; the tree of ACME-3 is open; "+
			"no slot of ACME-1 marks a wait"; got != want {
			t.Fatalf("after the pass that finds the wait:\n got %s\nwant %s", got, want)
		}
	})
}
