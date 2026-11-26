package codesurvival

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
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
// issue's change for GAMMA, which has no default repository, on its day alone.
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
	if days := r.evalDays("GAMMA"); !slices.Equal(days, []time.Time{day(1)}) {
		t.Errorf("GAMMA holds rows of %v, want of its change's day 1 alone", days)
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
// no default repository and no change reads "no data" with no row (k16, R11): DELTA, whose issue's
// change was measured in sharedRepo, holds that day's rows with repo_unreadable, its window reads
// no number, and refusedRepo is stored unreadable with its reason; EPSILON holds no row, so its
// window reads no data, and its settings name no default repository.
func TestAProjectWithNoDefaultRepositoryOrAnUnreadableOneNamesWhy(t *testing.T) {
	t.Parallel()
	r := r11Rig(t)
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
	if read := r.read("EPSILON", "all", day(0), day(8)); read.survival.Share != nil || read.status.WaitingDays != 8 {
		t.Errorf("EPSILON reads %s with %d days waiting, want no data and every day waiting", value(read.survival.Share), read.status.WaitingDays)
	}
	settings, err := delivery.GetProjectSettings(r.ctx, r.database.Pool, "EPSILON")
	if err != nil || settings.DefaultRepo != nil {
		t.Errorf("EPSILON's settings name the default repository %v (%v), want none", settings.DefaultRepo, err)
	}
}

// The k9 seed: one repository, two projects of it with different groups, and the changes that tell
// the group rules apart.
const (
	k9Repo   = "acme/k9"
	k9Prefix = "k9seed/"
	k9Login  = "k9-seed-app[bot]"
)

// k9Groups are project ONE's groups, a group by k9's prefix and login and one with no rule after
// it; and TWO's, a group by k9's login alone.
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
	r.clock.set(k9Pass)
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
	if err != nil || !stood.Built() || stood.GroupsDigest != groupsDigest(oneGroups) {
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
	go r.regroup.Run(ctx, wake)

	regrouped := []delivery.CodeGroup{{Name: "prefixed", BranchPrefixes: []string{k9Prefix}}, {Name: "everyone"}}
	want := groupsDigest(regrouped)
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
	for project, want := range map[string][]string{"ONE": {"survival:rest", "survival:seeded"}, "TWO": {"survival:bots"}} {
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
				if series.Listed && series.Series != "status" {
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
