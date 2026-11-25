package replay

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

func TestEveryCandidateOfTheDesignIsARule(t *testing.T) {
	t.Parallel()
	skipUntilBuilt(t)
	inventory := []struct{ name, reason string }{
		{"cover", "keep the slot until done, with a cover"},
		{"keep-past-review", "keep the slot past the review"},
		{"blockers-first", "start blockers of started work first"},
		{"returned-by-rank", "order returned work by rank"},
		{"before-p0", "reference rule before P0 starts outside the cap"},
		{"main", "order by priority first, then returned work and rank"},
		{"signals", "admit below P0 while signals are under their limits"},
		{"rework-first", "returned work, then started work by arrival, then rank"},
		{"arrival-order", "pure arrival order"},
		{"idle-return-1h", "give back the slot after an hour without a turn on a person or child wait"},
		{"signals-backoff", "lower the allowance on a project refusal or kill"},
		{"wait-grace", "short grace before a wait gives back a slot"},
		{"wait-grace-1h", "one-hour grace before a wait gives back a slot"},
		{"returned-by-first-slot", "order returned work by its first slot"},
		{"rework-first-keep", "rework first with slots kept past review"},
		// Remove these two rows when main and idle-return-1h take over their P0 behavior.
		{"p0-at-once", "start P0 outside the cap"},
		{"p0-idle", "start P0 outside the cap and return idle slots"},
		{"legion-v19.57.3", "recorded rule of window two"},
		{"legion-v20.42.5", "recorded rule of window one"},
		{"legion-v22.14.1", "recorded rule of window three"},
	}
	printed := map[string]int{}
	for _, rule := range record.SlotRules() {
		printed[rule.Name]++
	}
	for _, entry := range inventory {
		if printed[entry.name] != 1 {
			t.Errorf("legion replay rules prints %q (%s) %d times, want once", entry.name, entry.reason, printed[entry.name])
		}
		delete(printed, entry.name)
	}
	for _, name := range slices.Sorted(maps.Keys(printed)) {
		t.Errorf("legion replay rules prints %q, absent from the inventory", name)
	}
}

// errRolledBack ends a transaction of a test that must leave nothing behind it.
var errRolledBack = errors.New("the test's transaction is rolled back")

// replayStore makes a replay's own database on dsn's server (openReplayDatabase) and answers it
// with the store opened on it, the store closed and the database dropped when the test ends.
func replayStore(t *testing.T, dsn string) (replayDatabase, *store.Store) {
	t.Helper()
	ctx := context.Background()
	db, st, err := openReplayDatabase(ctx, dsn)
	if err != nil {
		t.Fatalf("make a replay's database: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		if err := db.drop(ctx); err != nil {
			t.Errorf("drop the replay's database: %v", err)
		}
	})
	return db, st
}

// inRolledBackTx runs fn in a transaction of st's, which it then rolls back, so fn leaves nothing
// behind it.
func inRolledBackTx(t *testing.T, st *store.Store, fn func(tx pgx.Tx)) {
	t.Helper()
	err := pgx.BeginTxFunc(context.Background(), st.Pool(), pgx.TxOptions{}, func(tx pgx.Tx) error {
		fn(tx)
		return errRolledBack
	})
	if !errors.Is(err, errRolledBack) {
		t.Fatalf("the test's transaction: %v", err)
	}
}

// The merger's ready mark of the release at 9542c6fb reaches the admission pass through its facts
// under the rule that reads it alone, and only for the visit to merging it fell in; and the covers
// a pass keeps read back as it kept them (ruleTables, design 10.10). Under each rule the repository
// keeps, in a replay's own database, an issue stands in merging at generation 1 holding the
// merger's phase entry 3, and a ReadyMark goes through the replay's handler: under legion-v19.57.3
// the pass's facts carry the mark (record.LineFacts.Ready) and the issue keeps no slot in merging
// (record.LineFacts.KeepsSlot), and once the merger's entry moves on, as the next visit to merging
// moves it, the mark reads nothing; under every other rule the handler writes no mark. Under cover
// the covers kept are the covers read, and a second keep replaces the first; under every other rule
// the facts carry none.
func TestTheReadyMarkAndTheCoversReadThroughTheFacts(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	ctx := context.Background()
	_, st := replayStore(t, dsn)
	records := record.NewStore()
	issue := record.Issue{Key: "ACME-1", Repo: builtRepository, DefaultBranch: "main", GitHubRepo: builtRepository, Tree: "ACME-1",
		Project: builtProject, Title: "ACME-1", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "a0", LastDispatchSeq: 1}
	merger := func(entry int64) record.PhaseRow {
		return record.PhaseRow{Issue: issue.Key, Role: claim.RoleMerger, Claim: "acme-1-merger", Entry: entry}
	}
	read := func(tx pgx.Tx, rule record.SlotRule) record.LineFacts {
		t.Helper()
		facts := record.LineFacts{Rule: rule}
		if err := (ruleTables{}).ReadRuleFacts(ctx, tx, []record.Issue{issue}, &facts); err != nil {
			t.Fatalf("under the rule %q, read the facts the rule keeps: %v", rule.Name, err)
		}
		return facts
	}
	for _, rule := range record.SlotRules() {
		t.Run(rule.Name, func(t *testing.T) {
			inRolledBackTx(t, st, func(tx pgx.Tx) {
				if err := records.PutIssue(ctx, tx, issue); err != nil {
					t.Fatalf("record %s: %v", issue.Key, err)
				}
				if err := records.PutPhase(ctx, tx, merger(3)); err != nil {
					t.Fatalf("record the merger's phases row of %s: %v", issue.Key, err)
				}
				handler := replayHandler{clock: func() time.Time { return clock(9, 0) }, rule: rule}
				if _, err := handler.Apply(ctx, tx, ReadyMark{Issue: issue.Key, Generation: issue.Generation}); err != nil {
					t.Fatalf("apply the ready mark of %s: %v", issue.Key, err)
				}
				var marks int
				if err := tx.QueryRow(ctx, `select count(*) from replay_ready_marks`).Scan(&marks); err != nil {
					t.Fatalf("count the ready marks: %v", err)
				}
				reads, want := rule.Name == "legion-v19.57.3", 0
				if reads {
					want = 1
				}
				if marks != want {
					t.Errorf("the handler wrote %d ready marks, want %d", marks, want)
				}
				facts := read(tx, rule)
				if _, ready := facts.Ready[issue.Key]; ready != reads {
					t.Errorf("the facts carry the ready mark of %s: %t, want %t", issue.Key, ready, reads)
				}
				if keeps := facts.KeepsSlot(issue); keeps != (rule.PhaseKeepsSlot(phase.Merging) && !reads) {
					t.Errorf("%s keeps its slot in merging after the mark: %t", issue.Key, keeps)
				}
				if err := records.PutPhase(ctx, tx, merger(4)); err != nil {
					t.Fatalf("move the merger's phase entry of %s on: %v", issue.Key, err)
				}
				if _, ready := read(tx, rule).Ready[issue.Key]; ready {
					t.Errorf("the ready mark of %s's merger entry 3 reads at its entry 4", issue.Key)
				}

				first, second := map[string]string{"ACME-2": "ACME-3", "ACME-4": "ACME-5"}, map[string]string{"ACME-2": "ACME-3"}
				for _, kept := range []map[string]string{first, second} {
					if err := (ruleTables{}).KeepCovers(ctx, tx, kept); err != nil {
						t.Fatalf("keep the covers %v: %v", kept, err)
					}
					covers := read(tx, rule).Covers
					if rule.Take != record.TakeCover {
						if covers != nil {
							t.Errorf("the facts carry the covers %v under a rule that takes none", covers)
						}
						continue
					}
					if !maps.Equal(covers, kept) {
						t.Errorf("the covers read are %v, and the pass kept %v", covers, kept)
					}
				}
			})
		})
	}
}

// Under the rule returned-by-first-slot the line's returned part stands by the moment of each
// issue's first slot in its current generation (record.LineReturnedByFirstSlot), which the replay
// keeps in a table of its own database through its rule tables (ruleTables): each pass's read of its
// facts keeps the slot each holder holds as its generation's first where the table holds none, and
// reads the first slots back into the facts (record.LineFacts.FirstSlots). In a replay's own
// database, three roots of the project stand in implementing in trees that run. ACME-1 takes a slot
// at 09:00, ACME-2 at 09:20 and ACME-3 at 09:30, each read by a pass; ACME-2 gives its slot back
// for a wait at 10:00, ACME-3 at 10:10 and ACME-1 at 10:20, and each wait has ended by the pass at
// 10:30. Under main the returned part stands by the moment each gave its slot back, ACME-1 last
// behind the two that came back before it; under returned-by-first-slot ACME-1, first admitted,
// stands ahead of them. ACME-1 then takes a slot again at 10:40, which leaves its first slot of
// the generation at 09:00; its next generation reads no first slot until a pass reads the slot
// it holds in it. Under main no pass keeps any.
func TestTheFirstSlotsPlaceTheReturnedPart(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	ctx := context.Background()
	_, st := replayStore(t, dsn)
	records := record.NewStore()
	token, err := claim.ProjectToken(builtProject)
	if err != nil {
		t.Fatal(err)
	}
	root := func(key, rank string, generation uint64) record.Issue {
		return record.Issue{Key: key, Repo: builtRepository, DefaultBranch: "main", GitHubRepo: builtRepository, Tree: key, Project: builtProject,
			Title: key, Phase: phase.Implementing, Generation: generation, Status: "in_progress", DispatchStatus: "in_progress", Rank: rank,
			HandedOver: true, LastDispatchSeq: 1}
	}
	for _, name := range []string{record.MainRule.Name, "returned-by-first-slot"} {
		rule := replayedRule(t, name)
		byFirstSlot := name != record.MainRule.Name
		t.Run(name, func(t *testing.T) {
			inRolledBackTx(t, st, func(tx pgx.Tx) {
				must := func(err error, what string) {
					t.Helper()
					if err != nil {
						t.Fatalf("%s: %v", what, err)
					}
				}
				for i, key := range []string{"ACME-1", "ACME-2", "ACME-3"} {
					must(records.PutIssue(ctx, tx, root(key, fmt.Sprintf("a%d", i+1), 1)), "record "+key)
					_, err := records.OpenTreeLifecycle(ctx, tx, token, key, treelifecycle.AuthorityWorkflow)
					must(err, "open the tree of "+key)
				}
				// pass reads the facts as a pass under rule reads them (record.ReadLineFactsUnder and
				// the replay's rule tables) and judges the line at now over them.
				pass := func(now time.Time) ([]record.LineEntry, record.LineFacts) {
					t.Helper()
					issues, err := records.LiveIssues(ctx, tx, builtProject)
					must(err, "read the live issues")
					slots, err := records.Slots(ctx, tx)
					must(err, "read the slots")
					facts, err := record.ReadLineFactsUnder(ctx, tx, records, builtProject, issues, nil, rule)
					must(err, "read the line facts")
					must((ruleTables{}).ReadRuleFacts(ctx, tx, issues, &facts), "read the facts the rule keeps")
					_, facts, line, _, err := blockers.JudgeLine(ctx, tx, records, builtProject, issues, slots, facts, nil, now)
					must(err, "judge the line")
					return line, facts
				}
				slot := func(key string, index int, now time.Time) {
					t.Helper()
					must(records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: index, AdmittedAt: now}), "give "+key+" a slot")
					pass(now)
				}
				slot("ACME-1", 0, clock(9, 0))
				slot("ACME-2", 1, clock(9, 20))
				slot("ACME-3", 2, clock(9, 30))
				for _, release := range []struct {
					key string
					at  time.Time
				}{{"ACME-2", clock(10, 0)}, {"ACME-3", clock(10, 10)}, {"ACME-1", clock(10, 20)}} {
					must(records.ReleaseSlotForWait(ctx, tx, release.key, release.at), "release "+release.key+" for a wait")
				}
				line, facts := pass(clock(10, 30))
				order, firsts := lineWordOf(line), "the first slots are "+firstSlotsWord(facts.FirstSlots)
				want, wantFirsts := "[ACME-2 ACME-3 ACME-1]", "the first slots are none"
				if byFirstSlot {
					want, wantFirsts = "[ACME-1 ACME-2 ACME-3]", "the first slots are ACME-1 09:00, ACME-2 09:20, ACME-3 09:30"
				}
				if order != want || firsts != wantFirsts {
					t.Errorf("the line at 10:30 is %s and %s, want %s and %s", order, firsts, want, wantFirsts)
				}
				for _, entry := range line {
					if entry.Part != record.PartReturned {
						t.Errorf("%s stands in the part %q, want the returned part", entry.Issue.Key, entry.Part)
					}
				}

				slot("ACME-1", 0, clock(10, 40))
				if _, facts := pass(clock(10, 45)); byFirstSlot && !facts.FirstSlots["ACME-1"].Equal(clock(9, 0)) {
					t.Errorf("after ACME-1's slot again at 10:40 its first slot reads %v, want 09:00", facts.FirstSlots["ACME-1"])
				}
				must(records.PutIssue(ctx, tx, root("ACME-1", "a1", 2)), "move ACME-1 to its next generation")
				issues, err := records.LiveIssues(ctx, tx, builtProject)
				must(err, "read the live issues")
				read, err := readFirstSlots(ctx, tx, issues)
				must(err, "read the first slots before a pass")
				if first, kept := read["ACME-1"]; kept {
					t.Errorf("ACME-1's next generation reads the first slot %v before any pass read a slot of it", first)
				}
				if _, facts := pass(clock(11, 0)); byFirstSlot && !facts.FirstSlots["ACME-1"].Equal(clock(10, 40)) {
					t.Errorf("ACME-1's next generation reads the first slot %v, want the slot it holds, 10:40", facts.FirstSlots["ACME-1"])
				}
				var count int
				must(tx.QueryRow(ctx, `select count(*) from replay_first_slots`).Scan(&count), "count the first slots kept")
				if want := map[bool]int{false: 0, true: 4}[byFirstSlot]; count != want {
					t.Errorf("the table keeps %d first slots under %s, want %d", count, name, want)
				}
			})
		})
	}
}

// The start state is read as a pass reads it, in a transaction that may write nothing
// (driver.startView, admit.Admission.View), so the tables of the replay's own that a pass under a
// rule keeps through its read of the facts (ruleTables) are kept by the passes and the start state
// alone, and a read that may write nothing reads what they kept. Each rule the repository keeps
// whose replay keeps first slots (readsFirstSlots) replays the fixture window send-back-at-full-cap
// through Run, its start state's view included, and the run ends with no error, where a keep in
// that view would fail it with Postgres's refusal of a write in a read-only transaction (SQLSTATE
// 25006). And in a replay's own database, where a pass under the rule read ACME-1 holding a slot
// from 09:00 and ACME-2 has taken a slot at 09:30 since, a read of the facts in a read-only
// transaction keeps no row in any table of the replay's own, and reads the first slot of ACME-1
// alone, the one the pass kept, and, under a rule that places by arrival (readsArrivals), the
// arrival of ACME-1 alone, the moment its record was written, which the pass kept.
func TestTheStartViewKeepsNothingAndReadsWhatThePassesKept(t *testing.T) {
	t.Parallel()
	dsn := replayDSN(t)
	ctx := context.Background()
	window, _ := readFixture(t, "send-back-at-full-cap")
	records := record.NewStore()
	root := func(key, rank string) record.Issue {
		return record.Issue{Key: key, Repo: builtRepository, DefaultBranch: "main", GitHubRepo: builtRepository, Tree: key, Project: builtProject,
			Title: key, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: rank,
			HandedOver: true, LastDispatchSeq: 1}
	}
	one, two := root("ACME-1", "a1"), root("ACME-2", "a2")
	replayed := 0
	for _, rule := range record.SlotRules() {
		if !readsFirstSlots(rule) {
			continue
		}
		replayed++
		t.Run(rule.Name, func(t *testing.T) {
			t.Parallel()
			if _, err := Run(ctx, RunConfig{Window: window, Rule: rule, Branch: MainBranch, Seed: 1, Slowdown: 1, PGDSN: dsn,
				Out: t.TempDir()}); err != nil {
				t.Fatalf("the replay of send-back-at-full-cap under %s failed: %v", rule.Name, err)
			}

			_, st := replayStore(t, dsn)
			in := func(options pgx.TxOptions, what string, fn func(tx pgx.Tx) error) {
				t.Helper()
				if err := pgx.BeginTxFunc(ctx, st.Pool(), options, fn); err != nil {
					t.Fatalf("%s: %v", what, err)
				}
			}
			read := func(tx pgx.Tx, issues ...record.Issue) (record.LineFacts, error) {
				facts := record.LineFacts{Rule: rule}
				err := (ruleTables{}).ReadRuleFacts(ctx, tx, issues, &facts)
				return facts, err
			}
			var written time.Time
			in(pgx.TxOptions{}, "record ACME-1 holding a slot from 09:00 and read the facts as a pass", func(tx pgx.Tx) error {
				if err := records.PutIssue(ctx, tx, one); err != nil {
					return err
				}
				recorded, err := records.Issue(ctx, tx, one.Key)
				if err != nil {
					return err
				}
				written = recorded.MovedAt
				if err := records.PutSlot(ctx, tx, record.Slot{Issue: one.Key, Index: 0, AdmittedAt: clock(9, 0)}); err != nil {
					return err
				}
				_, err = read(tx, one)
				return err
			})
			in(pgx.TxOptions{}, "record ACME-2 taking a slot at 09:30", func(tx pgx.Tx) error {
				if err := records.PutIssue(ctx, tx, two); err != nil {
					return err
				}
				return records.PutSlot(ctx, tx, record.Slot{Issue: two.Key, Index: 1, AdmittedAt: clock(9, 30)})
			})
			kept := replayRows(t, st)
			var facts record.LineFacts
			in(pgx.TxOptions{AccessMode: pgx.ReadOnly}, "read the facts in a read-only transaction", func(tx pgx.Tx) error {
				var err error
				facts, err = read(tx, one, two)
				return err
			})
			if after := replayRows(t, st); !maps.Equal(after, kept) {
				t.Errorf("the read in a read-only transaction left the replay's tables holding %v rows, and they held %v before it", after, kept)
			}
			if got := firstSlotsWord(facts.FirstSlots); got != "ACME-1 09:00" {
				t.Errorf("the read in a read-only transaction reads the first slots %s, want the one the pass kept, ACME-1 09:00", got)
			}
			arrived, hasArrival := facts.Arrivals[one.Key]
			if readsArrivals(rule) != (len(facts.Arrivals) == 1 && hasArrival && arrived.Equal(written)) {
				t.Errorf("under %s the read in a read-only transaction reads the arrivals %v, and the pass kept ACME-1's at %v under a rule "+
					"that places by arrival and none under any other", rule.Name, facts.Arrivals, written)
			}
		})
	}
	if replayed == 0 {
		t.Fatal("the repository keeps no rule whose replay keeps first slots, so nothing was replayed")
	}
}

// replayRows is the number of rows each table of the replay's own (replayTables) holds in st's
// database, by the table's name.
func replayRows(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := st.Pool().Query(ctx, `select table_name from information_schema.tables
		where table_schema = current_schema() and left(table_name, 7) = 'replay_'`)
	if err != nil {
		t.Fatalf("list the replay's tables: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the replay's tables: %v", err)
	}
	if len(names) != len(replayTables) {
		t.Fatalf("the replay's database holds the tables %v, and the replay makes %d", names, len(replayTables))
	}
	counts := make(map[string]int, len(names))
	for _, name := range names {
		var count int
		if err := st.Pool().QueryRow(ctx, `select count(*) from `+pgx.Identifier{name}.Sanitize()).Scan(&count); err != nil {
			t.Fatalf("count the rows of %s: %v", name, err)
		}
		counts[name] = count
	}
	return counts
}

// lineWordOf is the keys of a waiting line, in its order.
func lineWordOf(line []record.LineEntry) string {
	keys := make([]string, 0, len(line))
	for _, entry := range line {
		keys = append(keys, entry.Issue.Key)
	}
	return fmt.Sprint(keys)
}

// firstSlotsWord is the first slots of the facts, by key in order and to the minute, or "none".
func firstSlotsWord(firsts map[string]time.Time) string {
	if len(firsts) == 0 {
		return "none"
	}
	var said []string
	for _, key := range slices.Sorted(maps.Keys(firsts)) {
		said = append(said, key+" "+firsts[key].UTC().Format("15:04"))
	}
	return strings.Join(said, ", ")
}
