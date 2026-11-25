package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A look's task names the check's bound as the check has it, in main's form. Main said it as
// "<boundText> since the issue closed": whole days and whole hours as such, and any other duration
// as Go writes it (boundText). `production_check_hours` takes any positive number of hours, a
// decimal among them. A bound of whole days is named in whole days (bound_days); any other is
// named as Go writes the duration (bound_duration), with no bound in days beside it, exact to the
// nanosecond. The task named such a bound in decimal hours (bound_hours), which names 1.5µs
// (0.000000000416…) only nearly and a bound of a few nanoseconds as a long run of zeros. A check
// bound at its own open names its bound of 0s.
func TestALookTaskNamesABoundThatIsNotWholeDaysAsAGoDuration(t *testing.T) {
	issue := record.Issue{Key: "LEGION-208", Title: "A production check"}
	for _, tc := range []struct {
		bound          time.Duration
		days, duration string
	}{
		{bound: 14 * 24 * time.Hour, days: "14"},
		{bound: 36 * time.Hour, duration: "36h0m0s"},
		{bound: 12 * time.Hour, duration: "12h0m0s"},
		{bound: 90 * time.Minute, duration: "1h30m0s"},
		{bound: 18 * time.Minute, duration: "18m0s"},
		{bound: 90*time.Second - time.Nanosecond, duration: "1m29.999999999s"},
		{bound: 1500 * time.Nanosecond, duration: "1.5µs"},
		{bound: 4 * time.Nanosecond, duration: "4ns"},
		{bound: 0, duration: "0s"},
	} {
		t.Run(tc.bound.String(), func(t *testing.T) {
			check := record.ProductionCheck{Issue: issue.Key, Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43",
				OpenedAt: checkOpened, BoundAt: checkOpened.Add(tc.bound), Looks: 1, Role: claim.RoleTester}
			read := readTaskOf(t, checkTask(issue, check, wakeMerge, "", false))
			if read.fields["bound_days"] != tc.days || read.fields["bound_duration"] != tc.duration {
				t.Errorf("a bound of %s is named as bound_days %q and bound_duration %q, want %q and %q", tc.bound,
					read.fields["bound_days"], read.fields["bound_duration"], tc.days, tc.duration)
			}
			if hours, named := read.fields["bound_hours"]; named {
				t.Errorf("a bound of %s is also named in decimal hours (bound_hours %q)", tc.bound, hours)
			}
			if want := checkOpened.Add(tc.bound).UTC().Format(time.RFC3339Nano); read.fields["last_look"] != want {
				t.Errorf("the last look is named %q, want %q", read.fields["last_look"], want)
			}
		})
	}
}

// Every look of a check names the one bound the check has, the merge's look as the bound's. The
// record keeps OpenedAt and BoundAt in whole microseconds, Postgres's timestamptz precision, and
// `production_check_hours` takes a bound that is no whole count of them (0.000000000001 hours
// reads as 4ns, config's readUnit). Before, the merge's look named the bound as the engine
// computed it and every later look as the record read it back: 1.5µs, then 1µs, and 4ns, then no
// bound and no last look at all. The check's bound is what the record keeps, so every look names
// that one.
func TestEveryLookOfACheckNamesTheBoundTheRecordKeeps(t *testing.T) {
	for _, tc := range []struct {
		bound time.Duration
		named string
	}{
		{bound: 1500 * time.Nanosecond, named: "1µs"},
		{bound: 4 * time.Nanosecond, named: "0s"},
		{bound: 90*time.Minute + 1500*time.Nanosecond, named: "1h30m0.000001s"},
	} {
		t.Run(tc.bound.String(), func(t *testing.T) {
			rig := newCheckRig(t)
			cfg := rig.engine.cfg
			cfg.ProductionCheckBound = tc.bound
			rig.engine = New(record.NewStore(), cfg, rig.engine.log)
			rig.apply = applyFacts(t, rig.pool, rig.engine)
			seedMergingChild(t, rig.pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			rig.apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge-43"})
			rig.clock.now = checkOpened.Add(tc.bound + time.Minute)
			rig.apply("pending", intake.CheckReport{Issue: "LEGION-209", Look: 1, Verdict: "pending"})
			rig.apply("bound", intake.CheckBound{Issue: "LEGION-209"})
			starts := superviseRowsOf(t, rig.pool, "LEGION-209", "start")
			if len(starts) != 2 || starts[1].CheckLook != 2 {
				t.Fatalf("start rows = %+v, want the merge's look and the bound's", starts)
			}
			lastLook := checkOpened.Add(tc.bound).Truncate(time.Microsecond).UTC().Format(time.RFC3339Nano)
			for _, start := range starts {
				read := readTaskOf(t, start.Task)
				if read.fields["bound_duration"] != tc.named || read.fields["last_look"] != lastLook {
					t.Errorf("look %d names bound_duration %q and last_look %q, want %q and %q, the bound the record keeps",
						start.CheckLook, read.fields["bound_duration"], read.fields["last_look"], tc.named, lastLook)
				}
			}
		})
	}
}
