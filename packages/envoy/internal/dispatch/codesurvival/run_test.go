package codesurvival

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
)

// shaPattern finds a 40-character hex string that no longer run of hex digits holds: a SHA, and
// never a piece of a 64-digit digest.
var shaPattern = regexp.MustCompile(`(?:^|[^0-9a-f])([0-9a-f]{40})(?:[^0-9a-f]|$)`)

// The only SHAs the five tables hold are those R3 allows: a change's, the remover's of a stored
// pair, and a commit's that blocks a day while it blocks it. The branch's 40 days hold changes,
// commits that merged no pull request and remove no measured line (days 6 and 20), one that does
// remove one (day 9), and a commit whose patch neither route gives whole (day 4), which blocks days
// 0 to 4; days 5 to 7 are measured, and a change of day 8 removes a line of day 6's. After the
// run, every row of the five tables is read as text and every 40-character hex string in it is
// collected: each must be a change of a measured day, a remover of a stored pair that merged a
// pull request, or the blocking commit, and none is a commit that merged no pull request.
func TestTheOnlySHAsStoredAreChangesRemoversAndBlockedCommits(t *testing.T) {
	t.Parallel()
	w := newWriter(t)
	var silent, remover, blocking, pairRemover string
	w.days(0, 40, func(int) int { return 1 }, func(n int) {
		at := day(n).Add(6 * time.Hour)
		switch n {
		case 4:
			w.add("src/blocked-a.txt", 3, at, "blocked")
			w.add("src/blocked-b.txt", 3, at, "blocked")
			blocking = w.h.Push("a commit whose patch is withheld", at)
		case 6, 20:
			w.add("NOTES.md", 1, at, "note")
			silent = w.h.Push("notes", at)
		case 8:
			w.dropLast("src/file1.txt", 1)
			pairRemover = w.h.Land(w.pull(), at)
		case 9:
			w.dropLast("src/file0.txt", 1)
			remover = w.h.Push("tidy", at)
		}
	})
	branch := w.h.Branch()
	r := widgetsRig(t, branch)
	r.github.Refuse(blocking, codesurvivaltest.Refusal{Status: 406, Body: "diff too large", Withheld: 1})
	r.passAt(day(40).Add(time.Hour))

	states := r.days(widgets)
	for n := range 8 {
		want := survival.DayMeasured
		if n <= 4 {
			want = survival.DayBlocked
		}
		if states[day(n)] != want {
			t.Fatalf("day %d is %q, want %q", n, states[day(n)], want)
		}
	}
	var reason string
	if err := r.database.Pool.QueryRow(r.ctx, `select blocked_reason from code_days where repo = $1 and day = $2`, widgets, day(2)).Scan(&reason); err != nil ||
		reason != "diff refused (406), files[] withheld 1 of 2 files" {
		t.Fatalf("the blocked day's reason is %q (%v), want the refusal and the count named", reason, err)
	}

	pulls := map[string]bool{}
	for _, pull := range branch.Pulls {
		pulls[pull.SHA] = true
	}
	changes := map[string]bool{}
	for _, sha := range commitsOn(branch, day(5), day(8)) {
		if pulls[sha] {
			changes[sha] = true
		}
	}
	var stored []string
	for _, row := range r.dump(resultTables...) {
		for _, match := range shaPattern.FindAllStringSubmatch(row, -1) {
			stored = append(stored, match[1])
		}
	}
	if len(stored) == 0 {
		t.Fatal("the five tables hold no SHA at all")
	}
	var removers []string
	rows, err := r.database.Pool.Query(r.ctx, `select remover_sha from code_pairs where repo = $1`, widgets)
	if err != nil {
		t.Fatalf("read the removers: %v", err)
	}
	if removers, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatalf("read the removers: %v", err)
	}
	if !slices.Contains(removers, pairRemover) {
		t.Fatalf("the pairs' removers are %v, want the change of day 8 that removed a line of day 6's", removers)
	}
	seenBlocking, seenChanges := false, map[string]bool{}
	for _, sha := range stored {
		switch {
		case sha == silent || sha == remover:
			t.Errorf("the tables hold %s, a commit that merged no pull request", sha)
		case sha == blocking:
			seenBlocking = true
		case changes[sha]:
			seenChanges[sha] = true
		case slices.Contains(removers, sha) && pulls[sha]:
		default:
			t.Errorf("the tables hold %s, which is no change of a measured day, no remover of a stored pair and no blocking commit", sha)
		}
	}
	if !seenBlocking || len(seenChanges) != len(changes) {
		t.Errorf("the tables hold the blocking commit %t and %d of the %d changes of the measured days", seenBlocking, len(seenChanges), len(changes))
	}
	t.Logf("%d SHAs read from the five tables: %d changes, %d pairs' removers, the blocking commit", len(stored), len(seenChanges), len(removers))
}

// Every request the run sends is one of the routes "The run" names (k2): the App's installation and
// token, the repository, its branch, the commits list, a commit's patch through the commit route
// for a commit of one parent and through the compare route for the merge commit, the closed pull
// requests and a pull request's commit messages; no request reads files[], since no patch was
// refused.
func TestARunReadsOnlyTheRoutesItNames(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	requests := r.passAt(firstPass)
	if len(requests) == 0 {
		t.Fatal("the run sent no request")
	}
	byRoute := map[string]int{}
	for _, request := range requests {
		route := routeOf(request)
		if route == "" {
			t.Errorf("the run sent %s, which is none of the run's routes", request)
		}
		byRoute[route]++
	}
	parents := map[string]int{}
	for _, commit := range histories.a.Chain {
		parents[commit.SHA] = len(commit.Parents)
	}
	for _, request := range requests {
		sha, ok := request.Patch()
		if !ok {
			continue
		}
		if route, want := routeOf(request), map[bool]string{true: "compare", false: "diff"}[parents[sha] > 1]; route != want {
			t.Errorf("the patch of %s, of %d parents, was read through %s, want %s", sha, parents[sha], route, want)
		}
	}
	for _, route := range []string{"installation", "token", "repository", "branch", "chain", "pulls", "diff", "compare"} {
		if byRoute[route] == 0 {
			t.Errorf("the run sent no request on the route %s: %v", route, byRoute)
		}
	}
	if byRoute["compare"] != 1 || byRoute["files"] != 0 {
		t.Errorf("the run read %d patches through compare and %d pages of files[], want the merge commit's alone and none", byRoute["compare"], byRoute["files"])
	}
	t.Logf("%d requests by route: %v", len(requests), byRoute)
}

// A run over 60 days that hold one merge commit keeps no text of a patch (k2): after it, every
// table of the database is dumped as `select t::text from <table> t`, and every large object, and
// every bytea in them that starts 1f 8b is decompressed; nothing holds any of 50 added lines of at
// least 40 bytes from 50 patches the double served, or any of ten 200-byte pieces of ten of them.
// Every file the run wrote under its HOME, its TMPDIR and its working directory, each a directory of
// its own for the run, is searched the same way. It runs alone, since it moves the process's
// environment and working directory.
func TestARunStoresNoTextOfAPatch(t *testing.T) {
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	var lines, pieces [][]byte
	for _, commit := range histories.a.Chain {
		patch := histories.a.Patches[commit.SHA]
		if commit.CommittedAt.Before(day(58)) && len(lines) < 50 {
			for _, line := range bytes.Split(patch, []byte("\n")) {
				if len(line) >= 41 && line[0] == '+' && !bytes.HasPrefix(line, []byte("+++")) {
					lines = append(lines, line[1:])
					break
				}
			}
		}
		if at := bytes.Index(patch, []byte("\n@@")); at >= 0 && len(patch) >= at+201 && len(pieces) < 10 && len(lines)%5 == 0 {
			pieces = append(pieces, patch[at+1:at+201])
		}
	}
	if len(lines) != 50 || len(pieces) != 10 {
		t.Fatalf("the branch gives %d added lines and %d pieces, want 50 and 10", len(lines), len(pieces))
	}
	home, temp, work := scratchDir(t), scratchDir(t), scratchDir(t)
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", temp)
	t.Chdir(work)
	r.passAt(firstPass)
	if measured := len(r.measuredAt(widgets)); measured != 28 {
		t.Fatalf("the run measured %d days, want 28", measured)
	}

	found := func(where string, text []byte) {
		for _, needle := range append(slices.Clone(lines), pieces...) {
			if bytes.Contains(text, needle) {
				t.Errorf("%s holds the patch text %q", where, needle)
			}
		}
	}
	gzipped := regexp.MustCompile(`\\x(1f8b[0-9a-f]*)`)
	rows, err := r.database.Pool.Query(r.ctx, `
		select quote_ident(schemaname) || '.' || quote_ident(tablename) from pg_tables
		where schemaname not in ('pg_catalog', 'information_schema') order by 1`)
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	values, codeRows := 0, 0
	for _, table := range tables {
		dumped, err := r.database.Pool.Query(r.ctx, `select t::text from `+table+` t`)
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		texts, err := pgx.CollectRows(dumped, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		for _, text := range texts {
			values++
			if slices.Contains(resultTables, strings.TrimPrefix(table, "public.")) {
				codeRows++
			}
			found(table, []byte(text))
			for _, match := range gzipped.FindAllStringSubmatch(text, -1) {
				if raw, err := hex.DecodeString(match[1]); err == nil {
					found(table+" (gunzipped)", gunzip(raw))
				}
			}
		}
	}
	objects, err := r.database.Pool.Query(r.ctx, `select lo_get(oid) from pg_largeobject_metadata`)
	if err != nil {
		t.Fatalf("read the large objects: %v", err)
	}
	largeObjects, err := pgx.CollectRows(objects, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("read the large objects: %v", err)
	}
	for _, object := range largeObjects {
		found("a large object", object)
		found("a large object (gunzipped)", gunzip(object))
	}
	if len(tables) == 0 || codeRows == 0 {
		t.Fatalf("the dump read %d tables and %d rows of the results tables, want some of each", len(tables), codeRows)
	}
	files := 0
	for _, dir := range []string{home, temp, work} {
		if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			found(path, data)
			found(path+" (gunzipped)", gunzip(data))
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	t.Logf("searched %d tables, %d values, %d large objects and %d files the run wrote for 50 lines and 10 pieces", len(tables), values, len(largeObjects), files)
}

// scratchDir is a directory of the test's own, made before the test moves TMPDIR and removed when
// it ends.
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "codesurvival-")
	if err != nil {
		t.Fatalf("make a scratch directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// gunzip is data decompressed where it is gzip, and nothing otherwise.
func gunzip(data []byte) []byte {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	out, _ := io.ReadAll(reader)
	return out
}

// osExecImporters is every package codesurvival depends on that imports os/exec: three packages of
// Google's cloud authentication, which internal/cipherclient reaches through delivery's bus
// (delivery → internal/bus → natscred → internal/cipherclient → cloud.google.com/go/auth/credentials),
// and which run a credential process only for a workload identity that names one. No package of
// this module among the dependencies imports it.
var osExecImporters = []string{
	"cloud.google.com/go/auth/credentials/internal/externalaccount",
	"cloud.google.com/go/auth/internal/transport/cert",
	"github.com/googleapis/enterprise-certificate-proxy/client",
}

// A run starts no process (R2): it measures its 28 days with PATH an empty directory, where no git
// can be found, and among the packages codesurvival depends on, the only ones that import os/exec
// are the three of osExecImporters, none of this module. It runs alone, since it moves PATH.
func TestARunStartsNoProcess(t *testing.T) {
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	list := exec.Command(goTool, "list", "-deps", "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}", ".")
	var stderr bytes.Buffer
	list.Stderr = &stderr
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, stderr.String())
	}
	var importers []string
	packages := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		packages++
		if slices.Contains(fields[1:], "os/exec") {
			importers = append(importers, fields[0])
		}
	}
	slices.Sort(importers)
	if packages == 0 || !slices.Equal(importers, osExecImporters) {
		t.Errorf("of the %d packages codesurvival depends on, %v import os/exec, want exactly %v", packages, importers, osExecImporters)
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := exec.LookPath("git"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("git is found with PATH an empty directory: %v", err)
	}
	r.passAt(firstPass)
	if measured := len(r.measuredAt(widgets)); measured != 28 {
		t.Fatalf("with no PATH the run measured %d days, want 28", measured)
	}
	t.Logf("%d packages read; os/exec imported by %v; the run measured its 28 days with no PATH", packages, importers)
}

// The first daily pass sets the start day and measures 28 days (k3): the branch's 60 days begin on
// d0, its first commit, so that is the start day, later than survival.StartDay's, and days 0 to 27
// wait and are measured. It reads the patch of every commit of the replay range, days 0 to 27 and
// the 30 after them, once, and of no other commit; the 30 days after the measured ones hold their
// fingerprints alone, and the windows of the measured days read numbers.
func TestTheFirstRunMeasuresTwentyEightDaysAndReadsEachPatchOnce(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	requests := r.passAt(firstPass)
	repo := r.repo(widgets)
	if !repo.StartDay.Equal(day(0)) || !survival.StartDay(firstPass).Before(day(0)) {
		t.Fatalf("the start day is %s (survival.StartDay %s), want the first commit's day %s", repo.StartDay, survival.StartDay(firstPass), day(0))
	}
	if !repo.CheckedOn.Equal(day(60)) || !repo.LastMeasuredOn.Equal(day(60)) || repo.State != RepoReadable || repo.Branch != codesurvivaltest.BranchName {
		t.Fatalf("the repository's row is %+v, want it readable on main, checked and measured on %s", repo, day(60))
	}
	states := r.days(widgets)
	for n := -3; n < 62; n++ {
		want := survival.DayState("")
		switch {
		case n >= 0 && n < 28:
			want = survival.DayMeasured
		case n >= 28 && n < 58:
			want = survival.DayReplayed
		}
		if states[day(n)] != want {
			t.Errorf("day %d is %q, want %q", n, states[day(n)], want)
		}
	}
	reads, want := patchReads(requests), commitsOn(histories.a, day(0), day(58))
	if len(reads) != len(want) {
		t.Errorf("the run read the patches of %d commits, want the %d of days 0 to 57", len(reads), len(want))
	}
	for _, sha := range want {
		if reads[sha] != 1 {
			t.Errorf("the patch of %s was read %d times, want once", sha, reads[sha])
		}
	}
	read := r.read(widgetsProject, "agents", day(0), day(28))
	if read.survival.Share == nil || read.rework.Share == nil || read.status.MeasuredDays != 28 {
		t.Fatalf("the window of the 28 days reads survival %s and rework %s over %d measured days, want two numbers over 28", value(read.survival.Share), value(read.rework.Share), read.status.MeasuredDays)
	}
	t.Logf("28 days measured from %s; %d patches read once each; the agents' survival %s and rework %s", repo.StartDay.Format(time.DateOnly), len(reads), value(read.survival.Share), value(read.rework.Share))
}

// A pass one day after the first reads no patch (k3, the day-state table's row 2): one more day
// waits, fewer than seven, so the pass reads the chain, finds no day to measure and stores that it
// checked the repository, and writes no day row.
func TestARunOneDayLaterReadsNoPatch(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	r.passAt(firstPass)
	rows, days := r.evalRows(widgetsProject), r.dump("code_changes", "code_pairs", "code_days")
	requests := r.passAt(nextDay)
	byRoute := map[string]int{}
	for _, request := range requests {
		byRoute[routeOf(request)]++
	}
	if byRoute["chain"] == 0 || byRoute["pulls"] != 0 || byRoute["diff"]+byRoute["compare"]+byRoute["files"] != 0 {
		t.Fatalf("the pass a day later sent %v, want the chain read and no pull request or patch read", byRoute)
	}
	if !slices.Equal(r.evalRows(widgetsProject), rows) || !slices.Equal(r.dump("code_changes", "code_pairs", "code_days"), days) {
		t.Fatal("the pass a day later wrote a result or a day row")
	}
	if repo := r.repo(widgets); !repo.CheckedOn.Equal(day(61)) || !repo.LastMeasuredOn.Equal(day(60)) {
		t.Fatalf("the repository's row after the pass a day later is %+v, want it checked on %s and measured on %s", repo, day(61), day(60))
	}
}

// A pass a week after the first measures the seven days that wait (k3, the day-state table's row
// 3): days 28 to 34, reading the patches of those days and the 30 after them, each once.
func TestARunSevenDaysLaterMeasuresSevenDays(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	r.passAt(firstPass)
	r.passAt(nextDay)
	requests := r.passAt(weekLater)
	measured := r.measuredAt(widgets)
	for n := 28; n < 35; n++ {
		if !measured[day(n)].Equal(weekLater) {
			t.Errorf("day %d was measured at %s, want by the pass at %s", n, measured[day(n)], weekLater)
		}
	}
	if len(measured) != 35 {
		t.Errorf("%d days are measured, want 35", len(measured))
	}
	reads, want := patchReads(requests), commitsOn(histories.a, day(28), day(65))
	if len(reads) != len(want) {
		t.Errorf("the pass read the patches of %d commits, want the %d of days 28 to 59", len(reads), len(want))
	}
	for _, sha := range want {
		if reads[sha] != 1 {
			t.Errorf("the patch of %s was read %d times, want once", sha, reads[sha])
		}
	}
	if read := r.read(widgetsProject, "agents", day(28), day(35)); read.survival.Share == nil || read.status.MeasuredDays != 7 {
		t.Fatalf("the window of days 28 to 34 reads %s over %d measured days, want a number over 7", value(read.survival.Share), read.status.MeasuredDays)
	}
}

// k3's runs, in order: the first, one a day later, one six days after that, one a week later that
// meets the budget floor halfway through its patches, and the one after the reset. Each day is
// measured by the run due to measure it and never again: its measured_at is that run's moment, a
// day measured before keeps its day rows as they were written, no run reads a patch twice, and the
// run that met the floor wrote nothing.
func TestNoMeasuredDayIsMeasuredAgain(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	reset := twoWeeks.Add(30 * time.Minute)
	runs := []struct {
		at       time.Time
		before   func()
		measures []int
	}{
		{at: firstPass, measures: span(0, 28)},
		{at: nextDay},
		{at: weekLater, measures: span(28, 35)},
		{at: twoWeeks, before: func() {
			r.github.SetBudgetAfter(12, codesurvivaltest.Budget{Limit: 5000, Remaining: 1000, Reset: reset})
		}},
		{at: reset.Add(10 * time.Minute), before: func() { r.github.SetBudget(codesurvivaltest.DefaultBudget) }, measures: span(35, 42)},
	}
	measuredBy := map[time.Time]time.Time{}
	var earlier []string
	for index, run := range runs {
		if run.before != nil {
			run.before()
		}
		requests := r.passAt(run.at)
		for sha, reads := range patchReads(requests) {
			if reads != 1 {
				t.Errorf("run %d read the patch of %s %d times", index, sha, reads)
			}
		}
		for _, n := range run.measures {
			measuredBy[day(n)] = run.at
		}
		measured := r.measuredAt(widgets)
		if len(measured) != len(measuredBy) {
			t.Errorf("after run %d, %d days are measured, want %d", index, len(measured), len(measuredBy))
		}
		for measuredDay, at := range measuredBy {
			if !measured[measuredDay].Equal(at) {
				t.Errorf("after run %d, %s was measured at %s, want by the run at %s", index, measuredDay.Format(time.DateOnly), measured[measuredDay], at)
			}
		}
		rows := r.evalRows(widgetsProject)
		for _, row := range earlier {
			if !slices.Contains(rows, row) {
				t.Errorf("run %d rewrote or deleted the day row %q", index, row)
			}
		}
		earlier = rows
	}
}

// span is the numbers from from up to to.
func span(from, to int) []int {
	var numbers []int
	for n := from; n < to; n++ {
		numbers = append(numbers, n)
	}
	return numbers
}

// budgetRig is a rig after k3's first three runs, with days 0 to 34 measured, and the patches the
// run a week later reads (days 35 to 59).
func budgetRig(t *testing.T) (*rig, []string) {
	t.Helper()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	r.passAt(firstPass)
	r.passAt(nextDay)
	r.passAt(weekLater)
	return r, commitsOn(histories.a, day(35), day(72))
}

// A remaining budget below a quarter of the limit stops the run and writes no result (k3, R12):
// the double serves a remaining 1,000 of 5,000 once 12 of the 25 patches of the run a week later
// were read. The run stops: no day of 35 to 41 is measured, no row of the results tables or of the
// code evals moves, the repository waits for the reset (retry_at) and is not checked today, and the
// line says it stopped at the budget.
func TestABudgetBelowAQuarterStopsTheRunAndWritesNoResult(t *testing.T) {
	t.Parallel()
	r, patches := budgetRig(t)
	reset := twoWeeks.Add(30 * time.Minute)
	results, rows := r.dump("code_changes", "code_pairs", "code_days", "code_projects"), r.evalRows(widgetsProject)
	r.github.SetBudgetAfter(12, codesurvivaltest.Budget{Limit: 5000, Remaining: 1000, Reset: reset})
	requests := r.passAt(twoWeeks)
	if read := len(patchReads(requests)); read < 12 || read >= len(patches) {
		t.Errorf("the run read %d patches, want it stopped after the 12th and before the last of %d", read, len(patches))
	}
	if !slices.Equal(r.dump("code_changes", "code_pairs", "code_days", "code_projects"), results) || !slices.Equal(r.evalRows(widgetsProject), rows) {
		t.Fatal("the run that met the budget floor wrote a result or a day row")
	}
	repo := r.repo(widgets)
	if !repo.RetryAt.Equal(reset) || !repo.CheckedOn.Equal(day(67)) {
		t.Fatalf("after the budget stop the repository's row is %+v, want it to wait for %s and still checked on %s", repo, reset, day(67))
	}
	lines := r.logs.lines(t, "codesurvival: run")
	if last := lines[len(lines)-1]; last["stopped"] != stoppedBudget || last["remaining"] != float64(1000) || last["days_measured"] != float64(0) {
		t.Fatalf("the stopped run logged %v, want stopped at the budget with 1000 remaining and no day measured", last)
	}
}

// The run after the reset measures the days the budget stop left (k3): a pass before retry_at
// sends nothing, and the first pass after it, the same UTC day, measures days 35 to 41, reading
// each of their patches once.
func TestTheNextRunAfterTheResetMeasuresTheSameDays(t *testing.T) {
	t.Parallel()
	r, patches := budgetRig(t)
	reset := twoWeeks.Add(30 * time.Minute)
	r.github.SetBudgetAfter(12, codesurvivaltest.Budget{Limit: 5000, Remaining: 1000, Reset: reset})
	r.passAt(twoWeeks)
	if requests := r.passAt(reset.Add(-time.Minute)); len(requests) != 0 {
		t.Fatalf("a pass before the reset sent %d requests, want none", len(requests))
	}
	r.github.SetBudget(codesurvivaltest.DefaultBudget)
	after := reset.Add(10 * time.Minute)
	requests := r.passAt(after)
	measured := r.measuredAt(widgets)
	for n := 35; n < 42; n++ {
		if !measured[day(n)].Equal(after) {
			t.Errorf("day %d was measured at %s, want by the pass after the reset at %s", n, measured[day(n)], after)
		}
	}
	reads := patchReads(requests)
	for _, sha := range patches {
		if reads[sha] != 1 {
			t.Errorf("the patch of %s was read %d times by the pass after the reset, want once", sha, reads[sha])
		}
	}
	if repo := r.repo(widgets); !repo.RetryAt.IsZero() || !repo.CheckedOn.Equal(day(74)) {
		t.Fatalf("after the measurement the repository's row is %+v, want no retry and checked on %s", repo, day(74))
	}
}

// A run whose requests would take the remaining budget below a quarter of the limit does not start
// reading (R12): with 1,300 of 5,000 remaining, above the floor of 1,250 by itself, the 65 requests
// the pass a week later counts before it reads (a page of pull requests, and the patch and the
// commit messages of each of the 32 commits of its range, which attribution has not judged) would
// cross the floor, so it reads no pull request and no patch, writes nothing, and waits for the
// reset.
func TestARunThatWouldCrossTheFloorDoesNotStart(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	r.passAt(firstPass)
	r.passAt(nextDay)
	results := r.dump("code_changes", "code_pairs", "code_days", "code_projects")
	reset := weekLater.Add(time.Hour)
	r.github.SetBudget(codesurvivaltest.Budget{Limit: 5000, Remaining: 1300, Reset: reset})
	requests := r.passAt(weekLater)
	for _, request := range requests {
		if route := routeOf(request); route == "pulls" || route == "attribution" || route == "diff" || route == "compare" || route == "files" {
			t.Errorf("the run that would cross the floor sent %s", request)
		}
	}
	if !slices.Equal(r.dump("code_changes", "code_pairs", "code_days", "code_projects"), results) {
		t.Fatal("the run that would cross the floor wrote a result")
	}
	if repo := r.repo(widgets); !repo.RetryAt.Equal(reset) {
		t.Fatalf("the repository waits for %s, want the reset %s", repo.RetryAt, reset)
	}
	lines := r.logs.lines(t, "codesurvival: run")
	if last := lines[len(lines)-1]; last["stopped"] != stoppedBudget || last["patches_diff"] != float64(0) {
		t.Fatalf("the run logged %v, want stopped at the budget with no patch read", last)
	}
}

// Each pass of a repository logs one line (plan, "The log line") with every field, and its counts
// are the requests the double was sent: the first run's measured days, its patches by route, its
// requests (every one but the App's installation and token), its attribution reads, the budget the
// answers named, and how it stopped; and the pass a day later's line, which measured nothing.
func TestEachRunLogsItsLine(t *testing.T) {
	t.Parallel()
	histories := sixtyDays(t)
	r := widgetsRig(t, histories.a)
	first := r.passAt(firstPass)
	r.passAt(nextDay)
	lines := r.logs.lines(t, "codesurvival: run")
	if len(lines) != 2 {
		t.Fatalf("two passes logged %d lines, want 2", len(lines))
	}
	fields := []string{"repo", "days_measured", "days_marked", "patches_diff", "patches_compare", "patches_files", "requests", "bytes",
		"seconds", "peak_patch_bytes", "peak_lines", "requests_attribution", "limit", "remaining", "stopped"}
	for index, line := range lines {
		for _, field := range fields {
			if _, ok := line[field]; !ok {
				t.Errorf("line %d holds no %s: %v", index, field, line)
			}
		}
	}
	byRoute := map[string]int{}
	for _, request := range first {
		byRoute[routeOf(request)]++
	}
	requests := len(first) - byRoute["installation"] - byRoute["token"]
	want := map[string]any{
		"repo": widgets, "days_measured": float64(28), "days_marked": float64(0), "patches_diff": float64(byRoute["diff"]),
		"patches_compare": float64(1), "patches_files": float64(0), "requests": float64(requests),
		"requests_attribution": float64(byRoute["attribution"]), "limit": float64(5000), "remaining": float64(5000), "stopped": stoppedNone,
	}
	for field, value := range want {
		if lines[0][field] != value {
			t.Errorf("the first run's %s is %v, want %v", field, lines[0][field], value)
		}
	}
	if lines[0]["bytes"].(float64) <= 0 || lines[0]["peak_patch_bytes"].(float64) <= 0 || lines[0]["peak_lines"].(float64) <= 0 ||
		lines[0]["peak_patch_bytes"].(float64) > lines[0]["bytes"].(float64) {
		t.Errorf("the first run's bytes %v, peak patch bytes %v and peak lines %v, want each above 0 and the peak within the bytes", lines[0]["bytes"], lines[0]["peak_patch_bytes"], lines[0]["peak_lines"])
	}
	if lines[1]["days_measured"] != float64(0) || lines[1]["patches_diff"] != float64(0) || lines[1]["stopped"] != stoppedNone {
		t.Errorf("the pass a day later logged %v, want no day measured, no patch read and no stop", lines[1])
	}
	t.Logf("the first run's line: %v", lines[0])
}
