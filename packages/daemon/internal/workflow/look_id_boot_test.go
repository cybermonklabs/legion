package workflow

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// lookIDForm is the form of every look id: crypto/rand's Text, 26 characters of the base32
// alphabet.
var lookIDForm = regexp.MustCompile(`^[A-Z2-7]{26}$`)

// repairOpenChecks runs the boot's pass over the open production checks (Engine.RepairOpenChecks)
// in a transaction of its own, as the daemon's boot runs it before any fact, and answers how many
// looks it gave an id and how many checks it gave a generation.
func repairOpenChecks(t *testing.T, pool *pgxpool.Pool, engine *Engine) (looks, generations int) {
	t.Helper()
	if err := store.Transact(t.Context(), pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		looks, generations, err = engine.RepairOpenChecks(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("repair the open checks at the boot: %v", err)
	}
	return looks, generations
}

// putProductionCheck writes check as the record holds it.
func putProductionCheck(t *testing.T, pool *pgxpool.Pool, check record.ProductionCheck) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutProductionCheck(t.Context(), tx, check) })
}

// outboxKindsAfter is the kind of every outbox row queued after the row after, in queue order.
func outboxKindsAfter(t *testing.T, pool *pgxpool.Pool, after int64) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select kind from outbox where id > $1 order by id", after)
	if err != nil {
		t.Fatalf("read the rows queued after row %d: %v", after, err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect the rows queued after row %d: %v", after, err)
	}
	return kinds
}

// lookBootOpened is when the check of each row of the boot's look tests opened, and
// lookBootWoken when its look 2 started, under an older daemon: the boot comes half an hour into
// that look.
var (
	lookBootOpened = checkOpened
	lookBootWoken  = checkOpened.Add(26 * time.Hour)
)

// lookBootRoot is a running root, LEGION-208, whose child LEGION-209 merged sjawhar/legion#43.
func lookBootRoot() record.Issue {
	return record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION",
		Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1}
}

// lookBootChild is LEGION-209 done at generation 3, the generation its merge closed it at.
func lookBootChild() record.Issue {
	parent := "LEGION-208"
	return record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION",
		Title: "child", Parent: &parent, Phase: phase.Done, Generation: 3, Status: "done", DispatchStatus: "done", Rank: "V", LastDispatchSeq: 2}
}

// lookBootCheck is LEGION-209's check as an older daemon leaves it: open on the merge of #43 at
// generation 3, the tester's, at its look 2, open since lookBootWoken and woken by the wake the
// pending report of look 1 named, with no look id at all.
func lookBootCheck() record.ProductionCheck {
	opened, woken := lookBootOpened, lookBootWoken
	return record.ProductionCheck{Issue: "LEGION-209", Project: "LEGION", Tree: "LEGION-208", Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
		Role: claim.RoleTester, OpenedAt: opened, BoundAt: opened.Add(checkBound), Looks: 2, LookOpen: true, LookWakeAt: &woken,
		LastWakeKind: wakeNamed, LastWakeDetail: "production does not carry merge-43 yet", LastWakeAt: &woken, OpenedGeneration: 3}
}

// lookBootOlderTask is the task of look 2 as a daemon before cybermonklabs/legion#5463 wrote it:
// it names the look and no check instance, so it gives no check_opened_at, and no look id.
const lookBootOlderTask = "Production check of LEGION-209: child.\nRepository sjawhar/legion, pull request #43, merge commit merge-43. " +
	"Look 2, woken by the wake named at the last look — production does not carry merge-43 yet.\n" +
	"Do what the deployment instructions say to reach production and read whether production carries the merge. " +
	"Then report with the legion tool's handoff_complete and a verdict, committing nothing."

// TestTheBootGivesEachOpenLookWithoutItsOwnIDAnIDAndATask holds the R6 inventory of
// dispatch://INFRA-3518: every kind of open look with no look id of its own gets one at the boot
// (Engine.RepairOpenChecks), made for that look (26 base32 characters, stored with the look and
// the check instance, so CurrentLookID names it), and one start row of the same look: the
// check's role, the look's number and the check's opening time, the issue's generation, the
// look id, marked as the new task of an open look (record.SuperviseRequest.LookReissue), and the
// task checkTask writes now from the record's last wake and its last mark, which gives
// check_opened_at and the look id. The look keeps its number, the check stays open, nothing else
// of the check changes, and no other row is queued. The kinds: a look with no id; an id made for
// the previous look; an id made for a check a later merge replaced; a look whose task an older
// daemon wrote with no check_opened_at, whose start row stays as it was beside the new one; a
// last look, whose new task says it is the last; and the look of an issue that runs again, a
// child re-entered at a later generation, whose row carries that generation (the outbox's
// checkLookHolds finishes it while the run holds the issue). The controls change nothing and
// queue nothing: a look with its own id, and a check with no open look. The engine's journal
// carries no look id and no task.
func TestTheBootGivesEachOpenLookWithoutItsOwnIDAnIDAndATask(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		child func(record.Issue) record.Issue
		check func(record.ProductionCheck) record.ProductionCheck
		// older is the task of a start row of the look an older daemon queued, "" for none.
		older string
		// reissues says the boot gives the look an id and a new task; false for a control.
		reissues bool
		// says is what the new task says beside check_opened_at and the look id.
		says []string
	}{
		{name: "no id", reissues: true, says: []string{"Look 2, woken by the wake named at the last look — production does not carry merge-43 yet."}},
		{name: "an id made for the previous look", reissues: true, check: func(c record.ProductionCheck) record.ProductionCheck {
			c.LookID, c.LookIDLook, c.LookIDOpenedAt = "PREVIOUSLOOKIDABCDEFGHIJKL", 1, c.OpenedAt
			return c
		}},
		{name: "an id made for a check that a later merge replaced", reissues: true, check: func(c record.ProductionCheck) record.ProductionCheck {
			c.LookID, c.LookIDLook, c.LookIDOpenedAt = "REPLACEDCHECKSLOOKIDABCDEF", 2, c.OpenedAt.Add(-24*time.Hour)
			return c
		}},
		{name: "a task an older daemon wrote with no check_opened_at", reissues: true, older: lookBootOlderTask},
		{name: "a last look", reissues: true, check: func(c record.ProductionCheck) record.ProductionCheck {
			c.Last, c.LastWakeKind, c.LastWakeDetail = true, wakeBound, "2 days since the issue closed"
			return c
		}, says: []string{"Look 2, woken by the bound: 2 days since the issue closed.", "This is the last look: no look follows."}},
		{name: "an issue that runs again", reissues: true, child: func(c record.Issue) record.Issue {
			c.Phase, c.Status, c.DispatchStatus, c.Generation, c.LastDispatchSeq = phase.Admitted, "todo", "todo", 4, 3
			return c
		}},
		{name: "control: a look with its own id", check: func(c record.ProductionCheck) record.ProductionCheck {
			c.LookID, c.LookIDLook, c.LookIDOpenedAt = "OWNLOOKIDOFLOOKTWOABCDEFGH", 2, c.OpenedAt
			return c
		}},
		{name: "control: a check with no open look", check: func(c record.ProductionCheck) record.ProductionCheck {
			wake := lookBootWoken.Add(2 * time.Hour)
			c.LookOpen, c.WakeAt = false, &wake
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCheckRig(t)
			rig.clock.now = lookBootWoken.Add(30 * time.Minute)
			child, seeded := lookBootChild(), lookBootCheck()
			if tc.child != nil {
				child = tc.child(child)
			}
			if tc.check != nil {
				seeded = tc.check(seeded)
			}
			seedIssue(t, rig.pool, lookBootRoot())
			seedIssue(t, rig.pool, child)
			putProductionCheck(t, rig.pool, seeded)
			if tc.older != "" {
				seedRecord(t, rig.pool, func(tx pgx.Tx) error {
					return rig.engine.enqueueAt(t.Context(), tx, child.Key, record.SuperviseRequest{Op: "start", Tree: child.Tree, Role: seeded.Role,
						Generation: child.Generation, CheckLook: seeded.Looks, Task: tc.older}, lookBootWoken)
				})
			}
			before := productionCheck(t, rig.pool, child.Key)
			olderStarts := lookStarts(t, rig.pool, child.Key)
			after := lastOutboxID(t, rig.pool)
			mark := rig.logged.Len()

			looks, generations := repairOpenChecks(t, rig.pool, rig.engine)
			check := productionCheck(t, rig.pool, child.Key)
			queued := outboxKindsAfter(t, rig.pool, after)
			if !tc.reissues {
				if looks != 0 || generations != 0 || !reflect.DeepEqual(check, before) || len(queued) != 0 {
					t.Fatalf("the boot gave %d looks an id and %d checks a generation, left the check %+v and queued %q; want nothing changed: %+v",
						looks, generations, check, queued, before)
				}
				return
			}
			if looks != 1 || generations != 0 {
				t.Fatalf("the boot gave %d looks an id and %d checks a generation, want the open look an id and no check a generation", looks, generations)
			}
			id := check.CurrentLookID()
			if !lookIDForm.MatchString(id) || id == before.LookID || check.LookIDLook != before.Looks || !check.LookIDOpenedAt.Equal(before.OpenedAt) {
				t.Fatalf("the look's id after the boot = %q made for look %d of the check opened %s (stored %q), want an id of its own, 26 base32 characters, made for look %d of the check opened %s",
					id, check.LookIDLook, check.LookIDOpenedAt, check.LookID, before.Looks, before.OpenedAt)
			}
			kept := *before
			kept.LookID, kept.LookIDLook, kept.LookIDOpenedAt = check.LookID, check.LookIDLook, check.LookIDOpenedAt
			if !reflect.DeepEqual(*check, kept) || !check.Open() || !check.LookOpen || check.Looks != before.Looks {
				t.Fatalf("the check after the boot = %+v, want it open at look %d with its look open, as it was but for the look id: %+v", check, before.Looks, kept)
			}
			if len(queued) != 1 || queued[0] != string(record.OutboxKindSupervise) {
				t.Fatalf("rows the boot queued = %q, want one start row of the look", queued)
			}
			starts := lookStarts(t, rig.pool, child.Key)
			if len(starts) != len(olderStarts)+1 || !slices.EqualFunc(starts[:len(olderStarts)], olderStarts, func(a, b record.SuperviseRequest) bool { return reflect.DeepEqual(a, b) }) {
				t.Fatalf("the look's start rows after the boot = %+v, want those before (%+v) and one more", starts, olderStarts)
			}
			start := starts[len(starts)-1]
			if start.Op != "start" || start.Role != before.Role || start.Tree != child.Tree || start.Generation != child.Generation || start.Phase != "" ||
				start.CheckLook != before.Looks || !start.CheckOpenedAt.Equal(before.OpenedAt) || start.LookID != id || !start.LookReissue {
				t.Fatalf("the boot's start row = %+v, want the %s's start of look %d of the check opened %s at generation %d, giving the look's id, marked a reissue",
					start, before.Role, before.Looks, before.OpenedAt, child.Generation)
			}
			openedAt := before.OpenedAt.UTC().Format(time.RFC3339Nano)
			if want := checkTask(child, *check, id, before.LastWakeKind, before.LastWakeDetail, before.Last); start.Task != want {
				t.Fatalf("the boot's task =\n%s\nwant checkTask's text of now:\n%s", start.Task, want)
			}
			for _, line := range append([]string{"your report names it with check_opened_at `" + openedAt + "`.", "This look has the look id `" + id + "`."}, tc.says...) {
				if !strings.Contains(start.Task, line) {
					t.Fatalf("the boot's task =\n%s\nwant it to say %q", start.Task, line)
				}
			}
			if tc.older != "" && (len(olderStarts) != 1 || olderStarts[0].Task != tc.older || olderStarts[0].LookID != "" || start.Task == tc.older) {
				t.Fatalf("the older start row = %+v, want it kept with the older task and no look id beside the boot's own task", olderStarts)
			}
			journal := rig.logged.String()[mark:]
			for _, secret := range []string{id, checkTaskLead} {
				if strings.Contains(journal, secret) {
					t.Fatalf("the engine's journal of the boot carries %q, a look id or a task:\n%s", secret, journal)
				}
			}
		})
	}
}

// TestTheBootGivesEveryOpenCheckItsGeneration holds the second half of the boot's pass over the
// open production checks (Engine.RepairOpenChecks): an open check with no generation, one an
// older binary opened, gets one read from its issue as the record holds it before any fact
// (bootGeneration), and nothing else of it changes. A done child's check gets the child's
// generation, so a person's park of the child closes nothing (ranAgain is false). A root in the
// workflow holds such a check only as a root an older binary re-admitted after the check's merge,
// and its check gets the root's generation less one, after one re-admission and after two, which
// is below the root's, so the root's leave with no merge closes it (ranAgain is true). A root that
// older binary re-admitted and that then left with no merge gets its own generation, and keeps its
// check open as that binary left it. A check that has a generation and a closed check are left as
// they are. Each look holds its own id here, so the pass gives no look a new task.
func TestTheBootGivesEveryOpenCheckItsGeneration(t *testing.T) {
	t.Parallel()
	root := func(generation uint64, out bool) record.Issue {
		issue := lookBootRoot()
		issue.Generation, issue.Phase, issue.Status, issue.DispatchStatus = generation, phase.Admitted, "todo", "todo"
		if out {
			until := lookBootWoken.Add(time.Hour)
			issue.Phase, issue.Status, issue.DispatchStatus, issue.LingerUntil = phase.Done, "backlog", "backlog", &until
		}
		return issue
	}
	closed := lookBootWoken.Add(-time.Hour)
	for _, tc := range []struct {
		name  string
		issue record.Issue
		check func(record.ProductionCheck) record.ProductionCheck
		// want is the generation the check holds after the boot, and given whether the boot gave
		// it one; ranAgain is what a leave of the issue reads of it.
		want     uint64
		given    bool
		ranAgain bool
	}{
		{name: "a check of a done child gets the child's generation", issue: lookBootChild(), want: 3, given: true},
		{name: "a check of a root in the workflow after one re-admission gets its generation less one", issue: root(8, false), want: 7, given: true, ranAgain: true},
		{name: "a check of a root in the workflow after two re-admissions gets its generation less one", issue: root(9, false), want: 8, given: true, ranAgain: true},
		{name: "a check of a root an older binary re-admitted that then left with no merge gets the root's generation", issue: root(8, true), want: 8, given: true},
		{name: "control: a check that has a generation is left", issue: lookBootChild(), want: 2, ranAgain: true, check: func(c record.ProductionCheck) record.ProductionCheck {
			c.OpenedGeneration = 2
			return c
		}},
		{name: "control: a closed check is left", issue: lookBootChild(), check: func(c record.ProductionCheck) record.ProductionCheck {
			c.ClosedAt, c.Outcome, c.LookOpen = &closed, record.CheckRecorded, false
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newCheckRig(t)
			rig.clock.now = lookBootWoken.Add(30 * time.Minute)
			seeded := lookBootCheck()
			seeded.Issue, seeded.Tree, seeded.OpenedGeneration = tc.issue.Key, tc.issue.Tree, 0
			seeded.LookID, seeded.LookIDLook, seeded.LookIDOpenedAt = "OWNLOOKIDOFLOOKTWOABCDEFGH", seeded.Looks, seeded.OpenedAt
			if tc.check != nil {
				seeded = tc.check(seeded)
			}
			if tc.issue.Key != "LEGION-208" {
				seedIssue(t, rig.pool, lookBootRoot())
			}
			seedIssue(t, rig.pool, tc.issue)
			putProductionCheck(t, rig.pool, seeded)
			before := productionCheck(t, rig.pool, tc.issue.Key)
			after := lastOutboxID(t, rig.pool)
			mark := rig.logged.Len()

			looks, generations := repairOpenChecks(t, rig.pool, rig.engine)
			check := productionCheck(t, rig.pool, tc.issue.Key)
			want := *before
			want.OpenedGeneration = tc.want
			if given := generations == 1; looks != 0 || given != tc.given || !reflect.DeepEqual(*check, want) {
				t.Fatalf("the boot gave %d looks an id and %d checks a generation, and left the check %+v; want no look and the check given a generation %t: %+v",
					looks, generations, check, tc.given, want)
			}
			if got := ranAgain(tc.issue, *check); got != tc.ranAgain {
				t.Fatalf("%s at generation %d ran again since its check opened at generation %d: %t, want %t", tc.issue.Key, tc.issue.Generation, check.OpenedGeneration, got, tc.ranAgain)
			}
			if queued := outboxKindsAfter(t, rig.pool, after); len(queued) != 0 {
				t.Fatalf("rows the boot queued = %q, want none", queued)
			}
			given := strings.Contains(rig.logged.String()[mark:], `msg="workflow: the boot gave an open production check the generation it opened at" issue=`+tc.issue.Key)
			if given != tc.given {
				t.Fatalf("the engine's journal of the boot names the check given a generation: %t, want %t:\n%s", given, tc.given, rig.logged.String()[mark:])
			}
		})
	}
}
