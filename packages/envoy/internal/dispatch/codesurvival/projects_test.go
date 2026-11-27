package codesurvival

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/fold"
	"github.com/sjawhar/envoy/internal/dispatch/evals"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The repositories of k16: the one two projects share, and the one the double refuses the App.
const (
	sharedRepo  = "acme/shared"
	refusedRepo = "acme/refused"
)

// everyChange is a project's one code group, which takes every change.
var everyChange = []delivery.CodeGroup{{Name: "all"}}

// r11Rig is k16's five projects and their shared repository, measured: ALPHA and BETA, whose
// default repository is sharedRepo; GAMMA, with no default repository; DELTA, whose default
// repository the double refuses the App; and EPSILON, with no default repository and no change.
// sharedRepo's first five days each land one change: of ALPHA-1 (an issue of no repository of its
// own, so ALPHA's), of GAMMA-1 (whose own repository is sharedRepo), of DELTA-1 (whose own
// repository is sharedRepo), naming DELTA-2 (whose repository is DELTA's default, refusedRepo, so
// attribution drops it), and naming no issue. The pass measures days 0 to 7.
func r11Rig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	for _, p := range []struct{ key, repo string }{{"ALPHA", sharedRepo}, {"BETA", sharedRepo}, {"GAMMA", ""}, {"DELTA", refusedRepo}, {"EPSILON", ""}} {
		r.project(p.key, p.repo, everyChange)
	}
	r.issue("ALPHA", 1, "")
	r.issue("GAMMA", 1, sharedRepo)
	r.issue("DELTA", 1, sharedRepo)
	r.issue("DELTA", 2, "")
	w := newWriter(t)
	for n, title := range []string{"ALPHA-1: one", "GAMMA-1: two", "DELTA-1: three", "DELTA-2: four", "five"} {
		at := day(n).Add(10 * time.Hour)
		w.add("src/file.txt", 4, at, title)
		pull := w.pull()
		pull.Title = title
		w.h.Land(pull, at)
	}
	for n := 5; n < 10; n++ {
		at := day(n).Add(10 * time.Hour)
		w.add("src/notes.txt", 1, at, "note")
		w.h.Push("note", at)
	}
	r.github.Serve(sharedRepo, w.h.Branch())
	r.github.Unreadable(refusedRepo)
	r.passAt(day(40).Add(time.Hour))
	return r
}

// A change counts for the project R11 gives it (k16): the change of ALPHA's issue for ALPHA alone;
// the change naming DELTA-2, an issue of another repository, and the change naming none for each
// project whose default repository holds them, ALPHA and BETA, through the repository; and GAMMA's
// issue's change for GAMMA, which has no default repository and so takes its days from sharedRepo,
// the repository its issue's change is in: it holds a row of each day the pass measured there,
// with its one change on day 1 and zero counts on every other day.
func TestAChangeCountsForTheProjectR11GivesIt(t *testing.T) {
	t.Parallel()
	r := r11Rig(t)
	cases := []struct {
		project      string
		changes, via float64
	}{{"ALPHA", 3, 2}, {"BETA", 2, 2}, {"GAMMA", 1, 0}}
	for _, c := range cases {
		read := r.read(c.project, "all", day(0), day(8))
		if read.survival.Changes == nil || *read.survival.Changes != c.changes || *read.survival.ViaRepository != c.via || read.survival.Share == nil {
			t.Errorf("%s counts %s changes, %s through its repository, survival %s; want %g and %g and a number", c.project,
				value(read.survival.Changes), value(read.survival.ViaRepository), value(read.survival.Share), c.changes, c.via)
		}
	}
	if days := r.evalDays("GAMMA"); !slices.Equal(days, daysFrom(0, 8)) {
		t.Errorf("GAMMA holds rows of %v, want of each of %s's measured days 0 to 7", days, sharedRepo)
	}
	for n := range 8 {
		want := "0"
		if n == 1 {
			want = "1"
		}
		if read := r.read("GAMMA", "all", day(n), day(n+1)); value(read.survival.Changes) != want || value(read.survival.ViaRepository) != "0" {
			t.Errorf("GAMMA counts %s changes, %s through a repository, on day %d; want %s and 0", value(read.survival.Changes), value(read.survival.ViaRepository), n, want)
		}
	}
	if read := r.read("GAMMA", "all", day(0), day(8), sharedRepo); read.survival.Changes == nil || *read.survival.Changes != 1 {
		t.Errorf("GAMMA's cut of %s counts %s changes, want its issue's one", sharedRepo, value(read.survival.Changes))
	}
	if read := r.read("ALPHA", "all", day(0), day(8), sharedRepo); read.survival.Changes == nil || *read.survival.Changes != 3 {
		t.Errorf("ALPHA's cut of %s counts %s changes, want 3", sharedRepo, value(read.survival.Changes))
	}
	var issues []string
	rows, err := r.database.Pool.Query(r.ctx, `select coalesce(issue_key, '-') from code_changes where repo = $1 order by day`, sharedRepo)
	if err == nil {
		issues, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if err != nil || !slices.Equal(issues, []string{"ALPHA-1", "GAMMA-1", "DELTA-1", "-", "-"}) {
		t.Fatalf("the stored changes' issues are %v (%v), want attribution's: ALPHA-1, GAMMA-1, DELTA-1 and none twice", issues, err)
	}
}

// A project whose default repository the App cannot read reads "no data" naming it, and one with
// no default repository and no change of its issues in any repository reads "no data" naming the
// missing repository, with no row (k16, R11): DELTA, whose issue's change was measured in
// sharedRepo, holds that day's rows with repo_unreadable, its window reads no number, and
// refusedRepo is stored unreadable with its reason; EPSILON, once the regroup has built it, holds
// no row, so its window reads no number and no measured day, and its settings name no default
// repository.
func TestAProjectWithNoDefaultRepositoryOrAnUnreadableOneNamesWhy(t *testing.T) {
	t.Parallel()
	r := r11Built(t)
	read := r.read("DELTA", "all", day(0), day(8))
	if read.survival.Share != nil || read.survival.Changes != nil || read.status.RepoUnreadable == 0 || read.status.MeasuredDays != 1 {
		t.Errorf("DELTA reads survival %s over %s changes, %d rows naming its repository unreadable and %d measured days; want no data, the row named, one day",
			value(read.survival.Share), value(read.survival.Changes), read.status.RepoUnreadable, read.status.MeasuredDays)
	}
	if days := r.evalDays("DELTA"); !slices.Equal(days, []time.Time{day(2)}) {
		t.Errorf("DELTA holds rows of %v, want of its issue's change's day 2 alone", days)
	}
	if repo := r.repo(refusedRepo); repo.State != RepoUnreadable || repo.Reason == "" || !repo.CheckedOn.Equal(day(40)) {
		t.Errorf("%s is stored as %+v, want unreadable with its reason, checked today", refusedRepo, repo)
	}
	if days := r.evalDays("EPSILON"); len(days) != 0 {
		t.Errorf("EPSILON holds rows of %v, want none", days)
	}
	if read := r.read("EPSILON", "all", day(0), day(8)); read.survival.Share != nil || read.survival.Changes != nil || read.status.MeasuredDays != 0 || read.status.WaitingDays != 8 {
		t.Errorf("EPSILON reads %s over %s changes with %d measured days and %d waiting, want no data, no day measured and every day waiting",
			value(read.survival.Share), value(read.survival.Changes), read.status.MeasuredDays, read.status.WaitingDays)
	}
	if stood, err := ProjectDigest(r.ctx, r.database.Pool, "EPSILON"); err != nil || !stood.Built() {
		t.Errorf("EPSILON stands as %+v (%v), want built", stood, err)
	}
	settings, err := delivery.GetProjectSettings(r.ctx, r.database.Pool, "EPSILON")
	if err != nil || settings.DefaultRepo != nil {
		t.Errorf("EPSILON's settings name the default repository %v (%v), want none", settings.DefaultRepo, err)
	}
}

// r11Projects is the keys of r11Rig's five projects, in key order.
var r11Projects = []string{"ALPHA", "BETA", "DELTA", "EPSILON", "GAMMA"}

// r11Built is r11Rig once a regroup pass, an hour after the run's, has built the one project the
// run did not: EPSILON, which no repository's results count for.
func r11Built(t *testing.T) *rig {
	t.Helper()
	r := r11Rig(t)
	r.clock.Set(day(40).Add(2 * time.Hour))
	if next := r.regroup.Pass(r.ctx); !next.IsZero() {
		t.Fatalf("the regroup pass after k16's run asks to pass again at %s, want no retry", next)
	}
	return r
}

// r11Rows is the stored rows of each of r11Projects' code evals, by project.
func (r *rig) r11Rows() map[string][]string {
	r.t.Helper()
	rows := map[string][]string{}
	for _, key := range r11Projects {
		rows[key] = r.evalRows(key)
	}
	return rows
}

// r11RowsMoved is each of r11Projects but skip whose stored rows of the code evals are not rows'.
func (r *rig) r11RowsMoved(rows map[string][]string, skip string) []string {
	r.t.Helper()
	var moved []string
	for _, key := range r11Projects {
		if key != skip && !slices.Equal(r.evalRows(key), rows[key]) {
			moved = append(moved, key)
		}
	}
	return moved
}

// r11Changes is how many changes each of r11Projects counts over days 0 to 7, and how many of
// those reached it through its default repository, as a log line states them.
func (r *rig) r11Changes() string {
	r.t.Helper()
	counts := make([]string, 0, len(r11Projects))
	for _, key := range r11Projects {
		read := r.read(key, "all", day(0), day(8))
		counts = append(counts, fmt.Sprintf("%s %s (%s through its repository)", key, value(read.survival.Changes), value(read.survival.ViaRepository)))
	}
	return fmt.Sprintf("%d projects: %s", len(r11Projects), strings.Join(counts, ", "))
}

// A clear of a project's default repository rebuilds its rows, and a restore rebuilds them again
// (k16, R11). Once every project of k16's rig is built, BETA's default repository is cleared as
// PUT /api/v1/projects/{key}/default-repository clears it, and the next regroup pass rebuilds BETA
// alone. BETA's two changes name no issue of its own and counted for it only through sharedRepo,
// so it then holds no row and reads "no data" naming its missing default repository, as EPSILON
// does: no number, no measured day, every day waiting, and no default repository in its settings.
// Every other project's rows stand as they were. Once the repository is restored, the next pass
// rebuilds BETA again, and it counts its 2 changes, 2 of them through its repository, on the days
// it held rows of before the clear.
func TestAClearedDefaultRepositoryCountsNoChangeOfItAndARestoreCountsThemAgain(t *testing.T) {
	t.Parallel()
	r := r11Built(t)
	regroups := 0
	r.regroup.afterRows = func() { regroups++ }
	rows := r.r11Rows()
	before, beforeDays := r.read("BETA", "all", day(0), day(8)), r.evalDays("BETA")
	if value(before.survival.Changes) != "2" || value(before.survival.ViaRepository) != "2" || before.survival.Share == nil {
		t.Fatalf("BETA counts %s changes, %s through its repository, survival %s, before the clear; want 2, 2 and a number",
			value(before.survival.Changes), value(before.survival.ViaRepository), value(before.survival.Share))
	}
	t.Logf("before the clear, %s", r.r11Changes())

	r.setDefaultRepo("BETA", "")
	r.clock.Set(day(40).Add(3 * time.Hour))
	if next := r.regroup.Pass(r.ctx); !next.IsZero() || regroups != 1 {
		t.Fatalf("the pass after the clear ran %d regroups and asks to pass again at %s, want BETA's alone and no retry", regroups, next)
	}
	t.Logf("after the clear, %s", r.r11Changes())
	after := r.read("BETA", "all", day(0), day(8))
	if after.survival.Share != nil || after.survival.Changes != nil || after.rework.Share != nil || after.status.MeasuredDays != 0 || after.status.WaitingDays != 8 {
		t.Errorf("BETA reads survival %s over %s changes and rework %s, with %d measured days and %d waiting, after the clear; want no data, no day measured and every day waiting",
			value(after.survival.Share), value(after.survival.Changes), value(after.rework.Share), after.status.MeasuredDays, after.status.WaitingDays)
	}
	if days := r.evalDays("BETA"); len(days) != 0 {
		t.Errorf("BETA holds rows of %v after the clear, want none", days)
	}
	settings, err := delivery.GetProjectSettings(r.ctx, r.database.Pool, "BETA")
	if err != nil || settings.DefaultRepo != nil {
		t.Errorf("BETA's settings name the default repository %v (%v) after the clear, want none", settings.DefaultRepo, err)
	}
	if stood, err := ProjectDigest(r.ctx, r.database.Pool, "BETA"); err != nil || !stood.Built() || stood.GroupsDigest != rowsDigest(project{Groups: everyChange}) {
		t.Errorf("BETA stands as %+v (%v) after the clear, want built under its groups and no default repository", stood, err)
	}
	if moved := r.r11RowsMoved(rows, "BETA"); len(moved) != 0 {
		t.Errorf("the regroup after BETA's clear rewrote the rows of %v", moved)
	}

	r.setDefaultRepo("BETA", sharedRepo)
	regroups = 0
	r.clock.Set(day(40).Add(4 * time.Hour))
	if next := r.regroup.Pass(r.ctx); !next.IsZero() || regroups != 1 {
		t.Fatalf("the pass after the restore ran %d regroups and asks to pass again at %s, want BETA's alone and no retry", regroups, next)
	}
	t.Logf("after the restore, %s", r.r11Changes())
	restored := r.read("BETA", "all", day(0), day(8))
	for _, figure := range []struct {
		name      string
		got, want *float64
	}{
		{"changes", restored.survival.Changes, before.survival.Changes},
		{"changes through its repository", restored.survival.ViaRepository, before.survival.ViaRepository},
		{"survival", restored.survival.Share, before.survival.Share},
		{"rework", restored.rework.Share, before.rework.Share},
	} {
		if value(figure.got) != value(figure.want) {
			t.Errorf("BETA's %s read %s after the restore, want %s as before the clear", figure.name, value(figure.got), value(figure.want))
		}
	}
	if days := r.evalDays("BETA"); !slices.Equal(days, beforeDays) {
		t.Errorf("BETA holds rows of %v after the restore, want of %v as before the clear", days, beforeDays)
	}
	if moved := r.r11RowsMoved(rows, "BETA"); len(moved) != 0 {
		t.Errorf("the regroup after BETA's restore rewrote the rows of %v", moved)
	}
}

// A pass after a save of the same groups under the same default repository rebuilds nothing
// (k16): once every project of k16's rig is built, BETA's groups are saved again as they were and
// its default repository is written again as it was, and the next regroup pass opens no regroup,
// asks for no retry, and leaves every project's rows and code_projects as they were.
func TestAPassAfterASaveOfTheSameGroupsAndRepositoryRebuildsNothing(t *testing.T) {
	t.Parallel()
	r := r11Built(t)
	regroups := 0
	r.regroup.afterRows = func() { regroups++ }
	rows, stood := r.r11Rows(), r.dump("code_projects")

	r.saveGroups("BETA", everyChange)
	r.setDefaultRepo("BETA", sharedRepo)
	r.clock.Set(day(40).Add(3 * time.Hour))
	if next := r.regroup.Pass(r.ctx); !next.IsZero() || regroups != 0 {
		t.Fatalf("the pass after the same save ran %d regroups and asks to pass again at %s, want none and no retry", regroups, next)
	}
	if moved := r.r11RowsMoved(rows, ""); len(moved) != 0 {
		t.Errorf("the pass after the same save rewrote the rows of %v", moved)
	}
	if after := r.dump("code_projects"); !slices.Equal(after, stood) {
		t.Errorf("the pass after the same save left code_projects as %v, want %v", after, stood)
	}
}

// daysFrom is the UTC midnights of days from up to to.
func daysFrom(from, to int) []time.Time {
	var days []time.Time
	for n := from; n < to; n++ {
		days = append(days, day(n))
	}
	return days
}

// lateRepo is HOME's default repository, and the own repository of LATE-1, the one issue of LATE,
// which has no default repository.
const lateRepo = "acme/late"

// A project with no default repository takes its days from the repository its issues' changes are
// in on the run's path too (k16, R11), as the regroup builds them. lateRepo, HOME's default
// repository, lands a commit a day for 22 days, day 10's the one change of LATE-1. The first run
// measures days 0 to 7, and the regroup builds LATE, which no change counts for yet, with no row.
// The second run measures days 8 to 14, LATE's change among them, and LATE then holds a row of each
// of the 15 measured days, the 8 the first run measured included. The third measures days 15 to
// 21, which hold no change of LATE's, and LATE holds a row of each of them too. Every day counts
// LATE's one change on day 10 alone, and zero on every other day.
func TestAProjectWithNoDefaultRepositoryHoldsARowOfEachDayItsIssuesRepositoryMeasured(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.project("HOME", lateRepo, everyChange)
	r.project("LATE", "", everyChange)
	r.issue("LATE", 1, lateRepo)
	w := newWriter(t)
	for n := range 22 {
		at := day(n).Add(10 * time.Hour)
		if n == 10 {
			w.add("src/late.txt", 4, at, "LATE-1: late")
			pull := w.pull()
			pull.Title = "LATE-1: late"
			w.h.Land(pull, at)
			continue
		}
		w.add("src/notes.txt", 1, at, "note")
		w.h.Push("note", at)
	}
	r.github.Serve(lateRepo, w.h.Branch())
	measuredDays := func() int {
		measured := 0
		for _, state := range r.days(lateRepo) {
			if state == survival.DayMeasured {
				measured++
			}
		}
		return measured
	}
	counted := func(through int) {
		t.Helper()
		for n := range through {
			want := "0"
			if n == 10 {
				want = "1"
			}
			if read := r.read("LATE", "all", day(n), day(n+1)); value(read.survival.Changes) != want {
				t.Errorf("LATE counts %s changes on day %d, want %s", value(read.survival.Changes), n, want)
			}
		}
	}

	r.passAt(day(40).Add(time.Hour))
	r.clock.Set(day(40).Add(2 * time.Hour))
	if next := r.regroup.Pass(r.ctx); !next.IsZero() {
		t.Fatalf("the regroup pass after the first run asks to pass again at %s, want no retry", next)
	}
	if measured, days := measuredDays(), r.evalDays("LATE"); measured != 8 || len(days) != 0 {
		t.Fatalf("after the first run %s holds %d measured days and LATE rows of %v; want 8, and none, since no change counts for LATE yet", lateRepo, measured, days)
	}
	if stood, err := ProjectDigest(r.ctx, r.database.Pool, "LATE"); err != nil || !stood.Built() {
		t.Fatalf("LATE stands as %+v (%v) after the regroup, want built", stood, err)
	}

	r.passAt(day(47).Add(time.Hour))
	if measured, days := measuredDays(), r.evalDays("LATE"); measured != 15 || !slices.Equal(days, daysFrom(0, 15)) {
		t.Fatalf("after the second run %s holds %d measured days and LATE rows of %v; want 15, and a row of each of them", lateRepo, measured, days)
	}
	counted(15)

	r.passAt(day(54).Add(time.Hour))
	if measured, days := measuredDays(), r.evalDays("LATE"); measured != 22 || !slices.Equal(days, daysFrom(0, 22)) {
		t.Fatalf("after the third run %s holds %d measured days and LATE rows of %v; want 22, and a row of each of them", lateRepo, measured, days)
	}
	counted(22)
	if read := r.read("LATE", "all", day(0), day(22)); value(read.survival.Changes) != "1" || value(read.survival.ViaRepository) != "0" || read.survival.Share == nil {
		t.Errorf("LATE counts %s changes, %s through a repository, survival %s over days 0 to 21; want 1, 0 and a number",
			value(read.survival.Changes), value(read.survival.ViaRepository), value(read.survival.Share))
	}
}

// The k9 seed: one repository, two projects of it with different groups, and the changes that tell
// the group rules apart.
const (
	k9Repo   = "acme/k9"
	k9Prefix = "k9seed/"
	k9Login  = "k9-seed-app[bot]"
)

// oneGroups are project ONE's groups, a group by k9's prefix and login and one with no rule after
// it; and twoGroups TWO's, a group by k9's login alone.
var (
	oneGroups = []delivery.CodeGroup{{Name: "seeded", BranchPrefixes: []string{k9Prefix}, AuthorLogins: []string{k9Login}}, {Name: "rest"}}
	twoGroups = []delivery.CodeGroup{{Name: "bots", AuthorLogins: []string{k9Login}}}
)

// k9Rig is k9's seed, measured: ONE and TWO share k9Repo, and THREE, also of it, names no group.
// Its changes: one that only k9's login matches (day 0, six lines, all alive), one that only its
// prefix matches (day 1, six lines, three of which a commit with no pull request removes on day
// 2), and one of another author on another branch (day 3). The pass measures days 0 to 7.
func k9Rig(t *testing.T) *rig {
	t.Helper()
	r := k9Seed(t)
	r.passAt(k9Pass)
	return r
}

// k9Pass is when k9Rig's pass runs: days 0 to 7 of its branch wait.
var k9Pass = day(40).Add(time.Hour)

// k9Seed is k9Rig before its pass.
func k9Seed(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.project("ONE", k9Repo, oneGroups)
	r.project("TWO", k9Repo, twoGroups)
	r.project("THREE", k9Repo, nil)
	w := newWriter(t)
	land := func(n int, head, author string) {
		at := day(n).Add(10 * time.Hour)
		w.add("src/k9.txt", 6, at, head)
		w.h.Land(codesurvivaltest.Pull{Number: n + 1, HeadRef: head, Author: author, Title: "change"}, at)
	}
	land(0, "misc/login-only", k9Login)
	land(1, k9Prefix+"prefix-only", "someone")
	w.dropLast("src/k9.txt", 3)
	w.h.Push("tidy", day(2).Add(10*time.Hour))
	land(3, "feature/other", "person")
	for n := 4; n < 10; n++ {
		at := day(n).Add(10 * time.Hour)
		w.add("src/notes.txt", 1, at, "note")
		w.h.Push("note", at)
	}
	r.github.Serve(k9Repo, w.h.Branch())
	return r
}

// phaseHold holds every builder that reaches it, between its row locks and its series writes
// (prepare's afterRows), until the test lets them go.
type phaseHold struct {
	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func (h *phaseHold) arrive() {
	h.mu.Lock()
	h.arrived++
	h.mu.Unlock()
	<-h.release
}

func (h *phaseHold) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.arrived
}

// A run's results and a regroup of the same projects at once take their locks in one order and
// never wait for each other in a cycle (prepare): a pass of k9's repository, whose results count
// for ONE, TWO and THREE, and a regroup of the three, which no build has built yet, start together,
// and each is held between its row locks and its series writes until the other has reached the
// same point or waits for a row the first holds. Both finish: the run measures its 8 days, the
// regroup asks for no retry, nothing logs a failure, and ONE stands built under its groups.
func TestARunAndARegroupOfTheSameProjectsTakeTheirLocksInOneOrder(t *testing.T) {
	t.Parallel()
	r := k9Seed(t)
	hold := &phaseHold{release: make(chan struct{})}
	r.run.afterRows, r.regroup.afterRows = hold.arrive, hold.arrive
	r.clock.Set(k9Pass)
	start, done := make(chan struct{}), make(chan struct{})
	var (
		wg   sync.WaitGroup
		next time.Time
	)
	wg.Go(func() {
		<-start
		r.run.Pass(r.ctx)
	})
	wg.Go(func() {
		<-start
		next = r.regroup.Pass(r.ctx)
	})
	close(start)
	waiting := 0
	testwait.Until(t, "both builders past their row locks, or one waiting for a row the other holds", func() (bool, bool) {
		if err := r.database.Pool.QueryRow(r.ctx, `
			select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Errorf("read the lock waits: %v", err)
			return false, true
		}
		arrived := hold.count()
		return true, arrived >= 2 || (arrived == 1 && waiting >= 1)
	})
	t.Logf("released with %d builders held and %d waiting on a lock", hold.count(), waiting)
	close(hold.release)
	go func() {
		wg.Wait()
		close(done)
	}()
	testwait.Receive(t, "the run and the regroup to finish", done)

	if !next.IsZero() {
		t.Errorf("the regroup asked to pass again at %s, want no retry", next)
	}
	lines := r.logs.lines(t, "codesurvival: run")
	if len(lines) != 1 || lines[0]["stopped"] != stoppedNone || lines[0]["days_measured"] != float64(8) {
		t.Fatalf("the run logged %v, want one line measuring 8 days", lines)
	}
	for _, msg := range []string{"codesurvival: regroup a project's code day rows", "codesurvival: a repository's run failed; it is tried again at the next daily pass"} {
		if failed := r.logs.lines(t, msg); len(failed) != 0 {
			t.Errorf("a builder failed: %v", failed)
		}
	}
	stood, err := ProjectDigest(r.ctx, r.database.Pool, "ONE")
	if err != nil || !stood.Built() || stood.GroupsDigest != rowsDigest(project{DefaultRepo: k9Repo, Groups: oneGroups}) {
		t.Fatalf("ONE stands as %+v (%v), want built under its groups", stood, err)
	}
	if counts := r.groupCounts("ONE", oneGroups); counts["seeded"] != "2" || counts["rest"] != "1" {
		t.Errorf("ONE counts %v, want 2 seeded and 1 more", counts)
	}
}

// groupCounts is project's count of changes in each of groups over days 0 to 7.
func (r *rig) groupCounts(project string, groups []delivery.CodeGroup) map[string]string {
	r.t.Helper()
	counts := map[string]string{}
	for _, group := range groups {
		counts[group.Name] = value(r.read(project, group.Name, day(0), day(8)).survival.Changes)
	}
	return counts
}

// A save of a project's groups regroups its rows with no GitHub request (k9, R3, the day-state
// table's row 12): once ONE and TWO are measured, the double fails every request; the regroup's
// loop runs on a listener of the settings notification, as the server runs it; a save of ONE's
// groups wakes it, and it rebuilds ONE's rows under the new groups, a group by the prefix alone and
// one with no rule, and stores their digest, with no request to GitHub. Each group holds the count
// the seed states, the old group's series has no row left, and TWO's rows stand as they were.
func TestAGroupsSaveRegroupsWithNoGitHubRequest(t *testing.T) {
	t.Parallel()
	r := k9Rig(t)
	if counts := r.groupCounts("ONE", oneGroups); counts["seeded"] != "2" || counts["rest"] != "1" {
		t.Fatalf("ONE counts %v before the save, want 2 seeded (by the login and by the prefix) and 1 more", counts)
	}
	two := r.evalRows("TWO")
	r.github.FailAll(true)
	r.github.Forget()

	ctx, cancel := context.WithCancel(r.ctx)
	t.Cleanup(cancel)
	hub := &pglisten.Hub{Owner: "codesurvival-test", Dial: func(ctx context.Context) (*pgx.Conn, error) {
		return r.database.Pool.Dedicated(ctx, "codesurvival-regroup-test")
	}}
	wake := hub.Wake(delivery.SettingsChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	go hub.Run(ctx)
	testwait.Receive(t, "the listener of the settings notification", connected)
	go r.regroup.Run(ctx, wake, nil)

	regrouped := []delivery.CodeGroup{{Name: "prefixed", BranchPrefixes: []string{k9Prefix}}, {Name: "everyone"}}
	want := rowsDigest(project{DefaultRepo: k9Repo, Groups: regrouped})
	r.saveGroups("ONE", regrouped)
	testwait.Until(t, "the regroup of ONE under the saved groups", func() (bool, bool) {
		stood, err := ProjectDigest(r.ctx, r.database.Pool, "ONE")
		return true, err == nil && stood.Built() && stood.GroupsDigest == want
	})
	cancel()
	if requests := r.github.Requests(); len(requests) != 0 {
		t.Fatalf("the regroup sent GitHub %d requests: %v", len(requests), requests)
	}
	if counts := r.groupCounts("ONE", regrouped); counts["prefixed"] != "1" || counts["everyone"] != "2" {
		t.Errorf("ONE counts %v after the save, want 1 by the prefix and 2 more", counts)
	}
	if read := r.read("ONE", "seeded", day(0), day(8)); read.survival.Changes != nil {
		t.Errorf("ONE's removed group seeded still counts %s changes", value(read.survival.Changes))
	}
	if !slices.Equal(r.evalRows("TWO"), two) {
		t.Error("the regroup of ONE rewrote TWO's rows")
	}
}

// A pass over a project whose code evals the seed has not written opens no regroup transaction
// (the regroup's minute poll): it reads every project's groups, digest and seeding in one
// statement, leaves the project unbuilt, and asks for another pass a minute later, which, once the
// seed has written the evals, builds the project's rows under its groups and asks for none.
func TestAPassOpensNoRegroupOfAProjectTheSeedHasNotReached(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if _, err := r.database.Pool.Exec(r.ctx, `insert into projects (key, name, default_assignee) values ('LATE', 'LATE', 'alice')`); err != nil {
		t.Fatalf("make the project LATE: %v", err)
	}
	r.saveGroups("LATE", everyChange)
	regroups := 0
	r.regroup.afterRows = func() { regroups++ }
	r.clock.Set(k9Pass)
	if next := r.regroup.Pass(r.ctx); !next.Equal(k9Pass.Add(regroupRetry)) {
		t.Errorf("the pass over LATE, whose evals are not seeded, asks to pass again at %s, want a minute later, %s", next, k9Pass.Add(regroupRetry))
	}
	if regroups != 0 {
		t.Errorf("the pass over LATE, whose evals are not seeded, opened %d regroups, want none", regroups)
	}
	if _, err := r.registry.Ensure(r.ctx, "LATE"); err != nil {
		t.Fatalf("seed the evals of LATE: %v", err)
	}
	if next := r.regroup.Pass(r.ctx); !next.IsZero() {
		t.Errorf("the pass after LATE's seed asks to pass again at %s, want no retry", next)
	}
	if regroups != 1 {
		t.Errorf("the pass after LATE's seed opened %d regroups, want 1", regroups)
	}
	if stood, err := ProjectDigest(r.ctx, r.database.Pool, "LATE"); err != nil || !stood.Built() || stood.GroupsDigest != rowsDigest(project{Groups: everyChange}) {
		t.Errorf("LATE stands as %+v (%v), want built under its groups", stood, err)
	}
}

// Two projects' groups read two numbers over one repository (k9): ONE's seeded group, by the
// prefix and the login, holds the login's change, all of whose six lines are alive, and the
// prefix's, three of whose six are not, and reads 9/12; TWO's bots group, by the login alone, holds
// the login's change alone and reads 6/6. The list read holds each project's own group series.
func TestTwoProjectsGroupsReadTwoNumbers(t *testing.T) {
	t.Parallel()
	r := k9Rig(t)
	one, two := r.read("ONE", "seeded", day(0), day(8)), r.read("TWO", "bots", day(0), day(8))
	if value(one.survival.Share) != "0.75" || value(two.survival.Share) != "1" {
		t.Fatalf("ONE's seeded share is %s and TWO's bots share %s, want 0.75 and 1", value(one.survival.Share), value(two.survival.Share))
	}
	for project, want := range map[string][]string{
		"ONE": {fold.GroupSeries(fold.SeriesSurvival, "rest"), fold.GroupSeries(fold.SeriesSurvival, "seeded")},
		"TWO": {fold.GroupSeries(fold.SeriesSurvival, "bots")},
	} {
		page, err := r.registry.Current(r.ctx, []string{project}, "", 200, 0)
		if err != nil {
			t.Fatalf("read the evals of %s: %v", project, err)
		}
		var listed []string
		for _, entry := range page.Items {
			if entry.Definition.EvalID != evals.EvalID(project, evals.SlugCodeSurvival) {
				continue
			}
			for _, series := range entry.Series {
				if series.Listed && series.Series != fold.SeriesStatus {
					listed = append(listed, series.Series)
				}
			}
		}
		slices.Sort(listed)
		if !slices.Equal(listed, want) {
			t.Errorf("%s's survival eval lists the group series %v, want %v", project, listed, want)
		}
	}
}

// A project that names no group reads "no data" naming the setting (R10): THREE, of the measured
// repository, holds its days' status rows, which say it names no group, and no group's row.
func TestAProjectWithNoGroupsReadsNoDataNamingTheSetting(t *testing.T) {
	t.Parallel()
	r := k9Rig(t)
	read := r.read("THREE", "rest", day(0), day(8))
	if read.survival.Share != nil || read.status.Groups == nil || *read.status.Groups != 0 || read.status.MeasuredDays != 8 {
		t.Fatalf("THREE reads %s with groups %v over %d measured days, want no data naming no group over 8 days", value(read.survival.Share), read.status.Groups, read.status.MeasuredDays)
	}
	var groupRows int
	if err := r.database.Pool.QueryRow(r.ctx, `
		select count(*) from eval_days where project_key = 'THREE' and eval_id = any($1) and series <> 'status'`, codeEvalIDs("THREE")).Scan(&groupRows); err != nil || groupRows != 0 {
		t.Fatalf("THREE holds %d rows of a group series (%v), want none", groupRows, err)
	}
}
