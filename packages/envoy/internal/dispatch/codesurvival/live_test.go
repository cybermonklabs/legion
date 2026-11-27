//go:build codesurvivallive

package codesurvival

// live_test.go is check k15 of dispatch://INFRA-3206: the server's own run (Run.Pass) measured
// against real GitHub over 28 days of cybermonklabs/legion (TestLiveMeasurementOfTwentyEightDays),
// and against the double's large history (TestLiveSizeBoundOnTheDouble). No unit run builds them:
// the first needs a read token of the repository and spends about 4,800 requests of the
// installation's budget, which every Legion agent shares, and the second moves 2 GiB of patches.
// Both drive the pass through the loopback App proxy of
// docs/solutions/testing/prove-a-size-dependent-github-read-on-a-scratch-branch-and-drive-dispatch-against-real-github-through-a-loopback-app-proxy-INFRA-390.md,
// with the App client built as cmd/dispatch's boot builds it (bootClient).
//
// The live run, under the secrets broker, whose proxy puts the read token into each call to
// api.github.com, so no process of the run holds it; start it only when the installation's
// remaining budget is above 9,500 (GET /rate_limit, which the test also reads first):
//
//	agent-secrets GITHUB_READ --target cybermonklabs/legion --reason '…' -- sh -c 'cd packages/envoy && \
//	  CODESURVIVAL_LIVE_TOKEN="Bearer $GITHUB_READ" DISPATCH_TEST_DATABASE_URL=… SURVIVAL_REF_DIR=<dir> \
//	  go test -tags codesurvivallive -timeout 45m -run "^TestLiveMeasurementOfTwentyEightDays$" -v ./internal/dispatch/codesurvival/'
//
// The double's run, with no network and no token:
//
//	DISPATCH_TEST_DATABASE_URL=… go test -tags codesurvivallive -timeout 45m \
//	  -run '^TestLiveSizeBoundOnTheDouble$' -v ./internal/dispatch/codesurvival/

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/codesurvival/codesurvivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival"
	"github.com/sjawhar/envoy/internal/dispatch/delivery/survival/survivaltest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// liveTokenEnv names the Authorization value the proxy forwards each read under: "Bearer " and
// the broker's placeholder for the read token, which the broker's proxy replaces.
const liveTokenEnv = "CODESURVIVAL_LIVE_TOKEN"

// The live run's repository, its project, GitHub's API, and the run's shape (k15; plan,
// Departure 2): the start day and the clock, so the pass measures the 28 days from 2026-09-09 and
// reads patches through 2026-11-05.
const (
	liveRepo    = survivaltest.ReferenceRepo
	liveProject = "INFRA"
	liveGitHub  = "https://api.github.com"
	liveDays    = 28
)

var (
	liveStartDay = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	liveClock    = time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)
)

// liveGroups is the project's code groups as INFRA's delivery settings name them, the reference's
// (survivaltest.ReferenceGroups): Legion's changes, by the head-branch prefix legion/ or one of
// Legion's two implement Apps, then every other change.
var liveGroups = func() []delivery.CodeGroup {
	var groups []delivery.CodeGroup
	for _, group := range survivaltest.ReferenceGroups() {
		groups = append(groups, delivery.CodeGroup{Name: group.Name, BranchPrefixes: group.BranchPrefixes, AuthorLogins: group.AuthorLogins})
	}
	return groups
}()

// The budget and the bounds k15 holds the live run to.
const (
	// liveBudgetFloor is the remaining budget the live run starts only above: k15's 8,000, and
	// the run's own pre-check at a limit of 15,000, a quarter of it, 3,750, plus its estimate in
	// a database that holds no pull request yet, about 6,150 (two per commit of the range, and
	// the list's pages).
	liveBudgetFloor = 9900
	// livePace is how many reads a second the proxy forwards at most: under the 900 points a
	// minute GitHub allows the REST API before its secondary limit, which would stop the pass
	// with nothing stored and the requests it made spent.
	livePace = 14
	// heapBound is R4's bound on the live heap after garbage collection.
	heapBound = 1 << 30
	// liveHeapGuard is the live heap past which the live run is cut short, so it never threatens
	// the machine it runs on; a run that passes heapBound has failed already.
	liveHeapGuard = 2 << 30
	// attributeBudget is how long the live test replays k5 with one commit of D at a time, to
	// name the commits of D that move each change the plain comparison finds; its log says how
	// many of D it had time for.
	attributeBudget = 10 * time.Minute
	// commitPages is the most pages of a pull request's commit messages attribution reads
	// (maxCommitPages in delivery/intake.go), which R12 allows each merged pull request.
	commitPages = 5
)

// The double's large history (k15): 2,048 commits over 58 days, each the commit a pull request
// landed as and each a patch of exactly 1 MiB that inserts its lines at the top of one of 32
// files; the patches add 4,000,000 lines in all. The commits of one file share one patch body, so
// the double holds 32 MiB of patches, not 2 GiB.
const (
	sizeDays     = 58
	sizeMeasured = 28
	sizeCommits  = 2048
	sizeFiles    = 32
	sizePatch    = 1 << 20
	sizeLines    = 4_000_000
	// doublePeriod is how often a run on the double samples the live heap: often enough to cut
	// the negative control short soon after it passes heapBound.
	doublePeriod = 10 * time.Millisecond
	// keepCap is the most patch bytes the negative control keeps, and controlMemoryLimit the
	// soft memory limit it runs under, so the heap it grows stays well inside the machine's.
	keepCap            = 5 << 28
	controlMemoryLimit = 3 << 29
)

// liveHeapMetric is the live heap after the last garbage collection.
const liveHeapMetric = "/gc/heap/live:bytes"

// TestLiveMeasurementOfTwentyEightDays is k15's live run: the server's own pass over 28 days of
// cybermonklabs/legion, read from api.github.com through the loopback App proxy, against a
// database of the test's own that holds k5's issues and the project's code groups. It records the
// requests by route, the bytes, the seconds, the peak patch bytes held, the peak lines followed,
// the live heap after garbage collection (sampled each second) and the remaining budget. The
// proxy parses each patch the run reads as it passes (survival.ParseDiff, or survival.FilesPatch
// of a commit's files[] pages) and keeps its file diffs alone, never its text; D is the commits of
// the range whose GitHub patch so parsed differs from git's (lg/git/ of SURVIVAL_REF_DIR). The pass
// condition: the run's output equals k5's replay of the same days with GitHub's patch in place of
// git's for each commit of D, change for change (added, alive, removed_by_non_pr) and pair for
// pair (remover, lines). It also logs the plain comparison with k5's replay over git's patches
// alone: each change that differs, and the commits of D that move it, each replayed alone. It
// fails when a patch of the range was not read exactly once, when attribution read more than five
// pages of a merged pull request, above the patch-bytes bound or 1 GiB, at any change or pair
// that differs from the replay with D, and at zero changes.
func TestLiveMeasurementOfTwentyEightDays(t *testing.T) {
	requireLiveHeapMetric(t)
	authorization := os.Getenv(liveTokenEnv)
	if authorization == "" {
		t.Fatalf("%s is unset: run this test under agent-secrets GITHUB_READ with %s=\"Bearer $GITHUB_READ\", as the head of live_test.go says", liveTokenEnv, liveTokenEnv)
	}
	if os.Getenv(storetest.DatabaseURLEnv) == "" {
		t.Fatalf("%s is unset: the live run loads k5's issues into a database of its own on that server", storetest.DatabaseURLEnv)
	}
	liveMeasurement(t, liveGitHub, authorization, livePace)
}

// liveMeasurement is the body of TestLiveMeasurementOfTwentyEightDays against upstream, GitHub's
// API, read under authorization through the proxy at most pace reads a second (no bound for 0).
func liveMeasurement(t *testing.T, upstream, authorization string, pace int) {
	ref := survivaltest.OpenReference(t, os.Getenv(survivaltest.ReferenceDirEnv))
	r := newRig(t)
	proxy := newLiveProxy(t, upstream, authorization, pace)
	proxy.capture = true
	before := proxy.budget(t, r.ctx)
	t.Logf("the installation's budget before the run: %d of %d remaining, %d used, resets at %s",
		before.Remaining, before.Limit, before.Used, time.Unix(before.Reset, 0).UTC().Format(time.RFC3339))
	if before.Remaining <= liveBudgetFloor {
		t.Fatalf("the installation's remaining budget is %d of %d: the live run starts only above %d", before.Remaining, before.Limit, liveBudgetFloor)
	}
	r.run.Client = bootClient(t, r.database.Pool, proxy.url)
	issues, projects := r.seedLive(t, ref)
	if err := PutRepo(r.ctx, r.database.Pool, Repo{Repo: liveRepo, StartDay: liveStartDay, State: RepoReadable}); err != nil {
		t.Fatalf("set the start day of %s: %v", liveRepo, err)
	}
	t.Logf("%d issues of %d projects loaded; %s's start day %s, the run's clock %s", issues, projects, liveRepo,
		liveStartDay.Format(time.DateOnly), liveClock.Format(time.RFC3339))

	run := r.passThrough(proxy, liveClock, time.Second, liveHeapGuard)
	after := proxy.budget(t, r.ctx)
	t.Logf("the installation's budget after the run: %d of %d remaining, %d used, resets at %s",
		after.Remaining, after.Limit, after.Used, time.Unix(after.Reset, 0).UTC().Format(time.RFC3339))
	line := run.line
	if line == nil {
		t.Fatalf("the pass logged no line: it was cut short after %s (live heap after GC peaked at %d bytes, guard %d, cut %t); %d reads forwarded",
			run.took.Round(time.Second), run.heap, liveHeapGuard, run.aborted, len(run.reads))
	}
	t.Logf("the run's line: %v", line)
	if line["stopped"] != stoppedNone {
		t.Fatalf("the pass stopped at %v and stored nothing: %v", line["stopped"], line)
	}
	if figure(line, "days_measured") != liveDays {
		t.Errorf("the pass measured %d days, want %d: %v", figure(line, "days_measured"), liveDays, line)
	}

	statuses, err := DayStatuses(r.ctx, r.database.Pool, liveRepo, liveStartDay, liveStartDay.AddDate(0, 0, liveDays-1))
	if err != nil {
		t.Fatalf("read the days of %s: %v", liveRepo, err)
	}
	var selected, measured []time.Time
	for _, status := range statuses {
		switch status.State {
		case survival.DayMeasured:
			measured = append(measured, status.Day)
			selected = append(selected, status.Day)
		case survival.DayBlocked:
			selected = append(selected, status.Day)
		}
	}
	if len(selected) != liveDays || !selected[0].Equal(liveStartDay) {
		t.Fatalf("the run stored %d days from %v as measured or blocked, want the %d from %s", len(selected), selected, liveDays, liveStartDay.Format(time.DateOnly))
	}
	changes, err := Changes(r.ctx, r.database.Pool, liveRepo, measured)
	if err != nil {
		t.Fatalf("read the changes of %s: %v", liveRepo, err)
	}
	pairs, err := Pairs(r.ctx, r.database.Pool, liveRepo, measured)
	if err != nil {
		t.Fatalf("read the pairs of %s: %v", liveRepo, err)
	}

	// k5's side: git's patch of each commit of the range (lg/git/), as the reference tests read
	// them, and the run's own, as the proxy parsed each one it forwarded; D is the commits whose
	// two patches differ.
	chain := liveReferenceChain(t, ref)
	span := survival.ReplayRange(chain, selected[0], selected[len(selected)-1])
	if len(span) == 0 {
		t.Fatalf("k5's chain holds no commit of the replay range of %s to %s", selected[0].Format(time.DateOnly), selected[len(selected)-1].Format(time.DateOnly))
	}
	reader := &survivaltest.PatchReader{}
	git := make(map[string]survival.Patch, len(span))
	order := make(map[string]int, len(span))
	for index, commit := range span {
		files, err := gitPatch(reader, ref, commit.SHA)
		if err != nil {
			t.Fatalf("read git's patch of %s: %v", commit.SHA, err)
		}
		git[commit.SHA], order[commit.SHA] = survival.Patch{Files: files}, index
	}
	github, unparsed := proxy.githubPatches()
	if len(unparsed) > 0 {
		t.Errorf("the proxy could not parse %d patches the run read: %s", len(unparsed), liveFirst(unparsed, 5))
	}
	differs := map[string]string{}
	var uncaptured []string
	for _, commit := range span {
		patch, ok := github[commit.SHA]
		if !ok {
			uncaptured = append(uncaptured, commit.SHA)
			continue
		}
		if difference := patchDifference(patch, git[commit.SHA].Files); difference != "" {
			differs[commit.SHA] = difference
		}
	}
	if len(uncaptured) > 0 {
		t.Errorf("the proxy parsed no patch of %d commits of the range: %s", len(uncaptured), liveFirst(uncaptured, 5))
	}
	replay := func(fromGitHub func(sha string) bool) survival.MeasureOutput {
		t.Helper()
		out, err := survival.Measure(survival.MeasureInput{Days: selected, Chain: span, Patch: func(i int) (survival.Patch, error) {
			if sha := span[i].SHA; fromGitHub(sha) {
				return github[sha], nil
			}
			return git[span[i].SHA], nil
		}})
		if err != nil {
			t.Fatalf("replay the %d days: %v", len(selected), err)
		}
		return out
	}
	inD := func(sha string) bool {
		_, ok := differs[sha]
		return ok
	}
	plain, withD := replay(func(string) bool { return false }), replay(inD)

	// The reads: each patch of the range answered once, through the diff or the compare route,
	// or through files[] in place of a refused diff.
	counts, others := countsOf(run.reads)
	answered, tries, files := map[string]int{}, map[string]int{}, map[string]int{}
	var largest int64
	for _, read := range run.reads {
		if sha, ok := read.Patch(); ok {
			tries[sha]++
			if read.Status == http.StatusOK {
				answered[sha]++
				largest = max(largest, read.Bytes)
			}
		} else if liveRoute(read) == RouteFiles {
			files[filesSHA(read)]++
		}
	}
	inSpan := map[string]survival.Commit{}
	merged, unattributed := 0, 0
	issueOf := map[string]string{}
	for _, issue := range ref.PullIssues(t) {
		issueOf[issue.MergeSHA] = issue.Issue
	}
	for _, commit := range span {
		inSpan[commit.SHA] = commit
		if commit.Change.PR != 0 {
			merged++
			if issueOf[commit.SHA] == "" {
				unattributed++
			}
		}
	}
	var unread, twice, outside []string
	retries := 0
	for _, commit := range span {
		switch n := answered[commit.SHA]; {
		case n == 0 && files[commit.SHA] == 0:
			unread = append(unread, commit.SHA)
		case n > 1:
			twice = append(twice, fmt.Sprintf("%s (%d times)", commit.SHA, n))
		}
		retries += max(tries[commit.SHA]-1, 0)
	}
	for sha := range tries {
		if _, ok := inSpan[sha]; !ok {
			outside = append(outside, sha)
		}
	}
	if len(unread) > 0 || len(twice) > 0 || len(outside) > 0 {
		t.Errorf("of the %d patches of the range, %d were not read, %d were answered more than once, and %d patches of no commit of the range were read: %s; %s; %s",
			len(span), len(unread), len(twice), len(outside), liveFirst(unread, 5), liveFirst(twice, 5), liveFirst(outside, 5))
	}
	if len(others) > 0 {
		t.Errorf("the run sent %d reads of no route it names: %s", len(others), liveFirst(others, 5))
	}
	if counts.Diff != int(figure(line, "patches_diff")) || counts.Compare != int(figure(line, "patches_compare")) || counts.Files != int(figure(line, "patches_files")) ||
		counts.Attribution != int(figure(line, "requests_attribution")) || counts.Requests() != int(figure(line, "requests")) {
		t.Errorf("the proxy forwarded %+v (%d requests), which the run's line does not count: %v", counts, counts.Requests(), line)
	}
	if counts.Attribution > commitPages*merged {
		t.Errorf("attribution read %d pages of commit messages for the %d merged pull requests of the range, past %d each", counts.Attribution, merged, commitPages)
	}
	held, bound := figure(line, "peak_patch_bytes"), 8*largest+streamBuffer
	if held > bound {
		t.Errorf("the run held %d patch bytes at once, past the bound of %d (8 × its largest patch of %d + 64 MiB)", held, bound, largest)
	}
	if run.heap > heapBound {
		t.Errorf("the live heap after GC reached %d bytes, past 1 GiB", run.heap)
	}

	// The pass condition: the run's output equals k5's replay with GitHub's patch of each commit
	// of D in place of git's, change for change and pair for pair.
	got, want, bare := countsByChange(changes, pairs), countsByChange(withD.Changes, withD.Pairs), countsByChange(plain.Changes, plain.Pairs)
	if len(got) == 0 || len(want) == 0 {
		t.Fatalf("the run stored %d changes of the %d measured days and k5's replay with D read %d: zero changes compared", len(got), len(measured), len(want))
	}
	unequal := differingChanges(got, want, order)
	if len(unequal) > 0 {
		entries := make([]string, len(unequal))
		for index, sha := range unequal {
			entries[index] = sha + ": " + describeCounts(got, want, sha)
		}
		t.Errorf("%d changes of the run differ from k5's replay with GitHub's patch of each commit of D in place:\n%s", len(unequal), strings.Join(entries, "\n"))
	}
	var facts []string
	replayed := map[string]survival.Change{}
	for _, change := range withD.Changes {
		replayed[change.SHA] = change
	}
	for _, g := range changes {
		if w, ok := replayed[g.SHA]; ok && (g.PR != w.PR || g.HeadRef != w.HeadRef || g.Author != w.Author || !g.Day.Equal(w.Day)) {
			facts = append(facts, fmt.Sprintf("%s: the run's PR #%d %q by %s on %s, k5's PR #%d %q by %s on %s", g.SHA,
				g.PR, g.HeadRef, g.Author, g.Day.Format(time.DateOnly), w.PR, w.HeadRef, w.Author, w.Day.Format(time.DateOnly)))
		}
	}

	// The plain comparison: each change of the run that differs from k5's replay over git's
	// patches alone, and the commits of D that move it, each replayed alone in place of git's.
	moved := differingChanges(got, bare, order)
	isMoved := map[string]bool{}
	for _, sha := range moved {
		isMoved[sha] = true
	}
	movers := map[string][]string{}
	tried, attributing := 0, time.Now()
	for _, commit := range span {
		if !inD(commit.SHA) || time.Since(attributing) > attributeBudget {
			continue
		}
		tried++
		alone := replay(func(sha string) bool { return sha == commit.SHA })
		for _, sha := range differingChanges(countsByChange(alone.Changes, alone.Pairs), bare, order) {
			if isMoved[sha] {
				movers[sha] = append(movers[sha], commit.SHA[:12])
			}
		}
	}

	t.Logf("k15, the live run of %s: the 28 days %s to %s (%d measured, %d blocked), clock %s, patches through %s",
		liveRepo, selected[0].Format(time.DateOnly), selected[len(selected)-1].Format(time.DateOnly), len(measured), len(selected)-len(measured),
		liveClock.Format(time.RFC3339), span[len(span)-1].CommittedAt.Format(time.RFC3339))
	t.Logf("requests by route: repository %d, chain %d, pulls %d, attribution %d, diff %d, compare %d, files %d; %d in all (the run's line: %d; k15's estimate about 4,800, and at most %d by R12: the lists, %d patches and %d pages for each of the %d merged pull requests of the range, %d of which sources 3 and 4 leave unattributed in k5's files); %d tries of a patch asked again",
		counts.Repository, counts.Chain, counts.Pulls, counts.Attribution, counts.Diff, counts.Compare, counts.Files, counts.Requests(), figure(line, "requests"),
		counts.Repository+counts.Chain+counts.Pulls+counts.Files+retries+len(span)+commitPages*merged, len(span), commitPages, merged, unattributed, retries)
	t.Logf("bytes %d through the proxy, of which the routes the run counts bytes on (chain, pulls, diff, compare, files) %d (the run's line: %d); seconds %.1f (the pass: %s); the proxy forwarded at most %d reads a second (0: no bound) and held %d of them for their turn, %s in all",
		counts.Bytes, countedBytes(run.reads), figure(line, "bytes"), line["seconds"], run.took.Round(time.Second), pace, proxy.paced, proxy.pacing.Round(time.Second))
	t.Logf("patches: %d in the range, each answered once: %t; largest patch %d bytes; peak patch bytes held %d (bound %d: 8 × %d + 64 MiB)",
		len(span), len(unread)+len(twice)+len(outside) == 0, largest, held, bound, largest)
	t.Logf("peak lines followed %d; live heap after GC, sampled each second, peaked at %d bytes (%.1f MiB; bound 1 GiB), the parsed patches the proxy kept included; remaining budget %d of %d (the run's line)",
		figure(line, "peak_lines"), run.heap, float64(run.heap)/(1<<20), figure(line, "remaining"), figure(line, "limit"))
	t.Logf("D: %d of the %d commits of the range have a GitHub patch that differs from git's", len(differs), len(span))
	for _, commit := range span {
		if difference, ok := differs[commit.SHA]; ok {
			t.Logf("D: %s (%s, PR #%d): %s", commit.SHA, commit.CommittedAt.Format(time.DateOnly), commit.Change.PR, difference)
		}
	}
	t.Logf("plain k5, over git's patches alone: %d of the run's %d changes differ (k5 holds %d); %d of the %d commits of D replayed alone in %s",
		len(moved), len(got), len(bare), tried, len(differs), time.Since(attributing).Round(time.Second))
	for _, sha := range moved {
		by := "no commit of D alone"
		if len(movers[sha]) > 0 {
			by = "D's " + strings.Join(movers[sha], ", ")
		}
		t.Logf("plain k5: %s (PR #%d, %s): %s; moved by %s", sha, inSpan[sha].Change.PR, inSpan[sha].CommittedAt.Format(time.DateOnly), describeCounts(got, bare, sha), by)
	}
	t.Logf("equality with k5's replay with D: %d changes compared (the run %d, the replay %d), %d pairs in the run; %d differ; %d changes with other pull request facts",
		len(got), len(got), len(want), len(pairs), len(unequal), len(facts))
	for _, entry := range facts {
		t.Logf("facts: %s", entry)
	}
}

// changeCounts is one change as k15 compares it: the lines it added, those alive at its mark,
// those commits that merged no pull request removed by then, and the lines of each pair, by its
// remover's SHA.
type changeCounts struct {
	added, alive, nonPR int
	pairs               map[string]int
}

// countsByChange is each change of changes, by its SHA, with its pairs among pairs.
func countsByChange(changes []survival.Change, pairs []survival.Pair) map[string]changeCounts {
	out := make(map[string]changeCounts, len(changes))
	for _, change := range changes {
		out[change.SHA] = changeCounts{added: change.Added, alive: change.AliveAtMark, nonPR: change.RemovedByNonPR, pairs: map[string]int{}}
	}
	for _, pair := range pairs {
		if change, ok := out[pair.AdderSHA]; ok {
			change.pairs[pair.RemoverSHA] += pair.Lines
		}
	}
	return out
}

// differingChanges is each change one of a and b holds and the other does not, or holds with
// other counts or pairs, in chain order (order).
func differingChanges(a, b map[string]changeCounts, order map[string]int) []string {
	var out []string
	for _, side := range []map[string]changeCounts{a, b} {
		for sha := range side {
			x, inA := a[sha]
			y, inB := b[sha]
			if (inA != inB || x.added != y.added || x.alive != y.alive || x.nonPR != y.nonPR || !maps.Equal(x.pairs, y.pairs)) && !slices.Contains(out, sha) {
				out = append(out, sha)
			}
		}
	}
	slices.SortFunc(out, func(x, y string) int { return order[x] - order[y] })
	return out
}

// describeCounts says how the run's change sha (run) and a replay's (replay) differ.
func describeCounts(run, replay map[string]changeCounts, sha string) string {
	x, inRun := run[sha]
	y, inReplay := replay[sha]
	switch {
	case !inRun:
		return fmt.Sprintf("no change of the run's; the replay's added %d alive %d removed_by_non_pr %d, %d pairs", y.added, y.alive, y.nonPR, len(y.pairs))
	case !inReplay:
		return fmt.Sprintf("the run's added %d alive %d removed_by_non_pr %d, %d pairs; no change of the replay's", x.added, x.alive, x.nonPR, len(x.pairs))
	}
	text := fmt.Sprintf("the run's added %d alive %d removed_by_non_pr %d, the replay's %d %d %d", x.added, x.alive, x.nonPR, y.added, y.alive, y.nonPR)
	var removers []string
	for _, side := range []map[string]int{x.pairs, y.pairs} {
		for remover := range side {
			if x.pairs[remover] != y.pairs[remover] && !slices.Contains(removers, remover) {
				removers = append(removers, remover)
			}
		}
	}
	slices.Sort(removers)
	for index, remover := range removers {
		removers[index] = fmt.Sprintf("%s %d/%d", remover[:12], x.pairs[remover], y.pairs[remover])
	}
	if len(removers) > 0 {
		text += "; pairs differing (remover, the run's lines/the replay's): " + strings.Join(removers, ", ")
	}
	return text
}

// countedBytes is the bytes of reads on the routes the run counts bytes on, those it reads
// through its own get: the chain, the pulls list and the patches (Counts.Bytes); the repository's
// reads and attribution's pages go through the App client and delivery, which count none.
func countedBytes(reads []forwardedRead) int64 {
	var total int64
	for _, read := range reads {
		switch liveRoute(read) {
		case RouteChain, RoutePulls, RouteDiff, RouteCompare, RouteFiles:
			total += read.Bytes
		}
	}
	return total
}

// TestLiveSizeBoundOnTheDouble is k15's large run: the same pass, through the same proxy, against
// the double serving 58 days of 2,048 patches of 1 MiB that add 4,000,000 lines. The run reads each
// patch once, holds at most eight times its largest patch plus 64 MiB of patch bytes, follows the
// lines of the 28 measured days' changes, and keeps its live heap after garbage collection at most
// 1 GiB. The negative controls hold every patch until the replay starts: the double's first patch
// waits at the proxy until every other was answered, with the stream's buffer lifted. The stream
// then holds every patch, past the patch-bytes bound; and with each held patch also kept as the
// double answered it, the live heap passes 1 GiB, which cuts the pass short.
func TestLiveSizeBoundOnTheDouble(t *testing.T) {
	requireLiveHeapMetric(t)
	branch, tracked := sizeBoundBranch(t)
	bound := int64(8*sizePatch + streamBuffer)
	doubleRig := func(t *testing.T) (*rig, *liveProxy) {
		t.Helper()
		r := newRig(t)
		r.github.Serve(widgets, branch)
		// An installation's 15,000: the run's estimate of 4,117 requests leaves more than a
		// quarter of it.
		r.github.SetBudget(codesurvivaltest.Budget{Limit: 15000, Remaining: 15000, Reset: time.Unix(0, 0).UTC()})
		r.project(widgetsProject, widgets, testGroups)
		r.issue(widgetsProject, 1, "")
		proxy := newLiveProxy(t, r.github.URL(), "Bearer codesurvival-double", 0)
		r.run.Client = bootClient(t, r.database.Pool, proxy.url)
		return r, proxy
	}
	now := day(sizeDays + 2)

	t.Run("the run", func(t *testing.T) {
		r, proxy := doubleRig(t)
		run := r.passThrough(proxy, now, doublePeriod, heapBound)
		if run.line == nil {
			t.Fatalf("the pass logged no line: it was cut short after %s, the live heap after GC past 1 GiB (%t, %d bytes)", run.took.Round(time.Second), run.aborted, run.heap)
		}
		line := run.line
		reads := patchReads(requestsOf(run.reads))
		once := 0
		for _, n := range reads {
			if n == 1 {
				once++
			}
		}
		held := figure(line, "peak_patch_bytes")
		counts, others := countsOf(run.reads)
		switch {
		case line["stopped"] != stoppedNone || figure(line, "days_measured") != sizeMeasured:
			t.Errorf("the pass stopped %v with %d days measured, want none and %d: %v", line["stopped"], figure(line, "days_measured"), sizeMeasured, line)
		case len(reads) != sizeCommits || once != sizeCommits:
			t.Errorf("the run read %d patches, %d of them once, want each of the %d once", len(reads), once, sizeCommits)
		case figure(line, "peak_lines") != int64(tracked):
			t.Errorf("the run followed %d lines at its peak, want the %d the measured days' changes added", figure(line, "peak_lines"), tracked)
		case len(others) > 0:
			t.Errorf("the run sent %d reads of no route it names: %s", len(others), liveFirst(others, 5))
		}
		if held > bound {
			t.Errorf("the run held %d patch bytes at once, past the bound of %d (8 × 1 MiB + 64 MiB)", held, bound)
		}
		if run.heap > heapBound {
			t.Errorf("the live heap after GC reached %d bytes, past 1 GiB", run.heap)
		}
		t.Logf("the run on the double: %d patches of %d bytes read once each in %s (the line's seconds %.1f); requests by route: repository %d, chain %d, pulls %d, attribution %d, diff %d, compare %d, files %d, %d in all; %d bytes; peak patch bytes held %d (bound %d); peak lines followed %d (the %d the 28 days' changes added); live heap after GC peaked at %d bytes (%.1f MiB; bound 1 GiB)",
			len(reads), sizePatch, run.took.Round(time.Second), line["seconds"], counts.Repository, counts.Chain, counts.Pulls, counts.Attribution, counts.Diff,
			counts.Compare, counts.Files, counts.Requests(), counts.Bytes, held, bound, figure(line, "peak_lines"), tracked, run.heap, float64(run.heap)/(1<<20))
	})

	t.Run("negative control: the stream holds every patch until the replay starts", func(t *testing.T) {
		r, proxy := doubleRig(t)
		r.run.buffer = 1 << 62
		proxy.hold = newPatchHold(branch.Chain[0].SHA, sizeCommits-1, false)
		run := r.passThrough(proxy, now, doublePeriod, heapBound)
		if run.aborted {
			t.Logf("the control FAILS the bound as it must: the live heap after GC passed 1 GiB (%d bytes) and cut the pass short after %s", run.heap, run.took.Round(time.Second))
			return
		}
		if run.line == nil {
			t.Fatalf("the pass logged no line after %s, and the live heap after GC peaked at %d bytes", run.took.Round(time.Second), run.heap)
		}
		held := figure(run.line, "peak_patch_bytes")
		// Up to streamWorkers - 1 workers may still be parsing when the proxy lets the first
		// patch go.
		if floor := int64(sizeCommits-streamWorkers) * sizePatch; held <= bound || held < floor {
			t.Errorf("the stream held %d patch bytes at its peak, want at least %d (past the bound of %d)", held, floor, bound)
		}
		t.Logf("the control FAILS the patch-bytes bound as it must: the stream held %d patch bytes at once (bound %d); stopped %v after %s; the live heap after GC peaked at %d bytes (%.1f MiB; past 1 GiB: %t), since the stream holds each patch as the replay reads it (survival.Patch: its files and hunks, not its text)",
			held, bound, run.line["stopped"], run.took.Round(time.Second), run.heap, float64(run.heap)/(1<<20), run.heap > heapBound)
	})

	t.Run("negative control: the held patches kept as answered pass 1 GiB", func(t *testing.T) {
		r, proxy := doubleRig(t)
		r.run.buffer = 1 << 62
		proxy.hold = newPatchHold(branch.Chain[0].SHA, sizeCommits-1, true)
		defer debug.SetMemoryLimit(debug.SetMemoryLimit(controlMemoryLimit))
		run := r.passThrough(proxy, now, doublePeriod, heapBound)
		keptCount, keptBytes := proxy.hold.keptSoFar()
		if !run.aborted || run.heap <= heapBound {
			t.Errorf("with every patch held until the replay starts, the live heap after GC peaked at %d bytes and the pass was cut short: %t; want past 1 GiB", run.heap, run.aborted)
		}
		t.Logf("the control FAILS the bound as it must: with each held patch kept as the double answered it (%d patches, %d bytes, when the pass was cut), the live heap after GC passed 1 GiB, %d bytes (%.1f MiB), and cut the pass short after %s",
			keptCount, keptBytes, run.heap, float64(run.heap)/(1<<20), run.took.Round(time.Second))
	})
}

// sizeBoundBranch is the double's large history and how many lines the changes of its 28
// measured days add: commit i is on day i × 58 / 2,048, lands pull request i+1 (the agents' on an
// even i) naming widgetsProject's issue 1, and inserts its lines at the top of file i mod 32,
// whose patch every commit of that file shares.
func sizeBoundBranch(t *testing.T) (*codesurvivaltest.Branch, int) {
	t.Helper()
	perFile := sizeCommits / sizeFiles
	bodies, linesOf := make([][]byte, sizeFiles), make([]int, sizeFiles)
	for file := range sizeFiles {
		linesOf[file] = sizeLines / sizeCommits
		if file < sizeLines%sizeCommits/perFile {
			linesOf[file]++
		}
		bodies[file] = insertPatch(fmt.Sprintf("big/file%02d.txt", file), linesOf[file], sizePatch)
		parsed, err := survival.ParseDiff(bodies[file])
		if err != nil || len(bodies[file]) != sizePatch || len(parsed) != 1 || len(parsed[0].Hunks) != 1 || parsed[0].Hunks[0].Added != linesOf[file] {
			t.Fatalf("the patch of file %d is %d bytes and reads %+v (%v), want %d bytes adding %d lines in one hunk", file, len(bodies[file]), parsed, err, sizePatch, linesOf[file])
		}
	}
	var (
		chain   []codesurvivaltest.Commit
		pulls   []codesurvivaltest.Pull
		patches = make(map[string][]byte, sizeCommits)
		onDay   = map[int]int{}
	)
	added, tracked := 0, 0
	for i := range sizeCommits {
		n := i * sizeDays / sizeCommits
		at := day(n).Add(10*time.Hour + time.Duration(onDay[n])*time.Minute)
		onDay[n]++
		commit := codesurvivaltest.Commit{SHA: fmt.Sprintf("%040x", 0x5172e000+i), CommittedAt: at}
		if i > 0 {
			commit.Parents = []string{chain[i-1].SHA}
		}
		chain = append(chain, commit)
		patches[commit.SHA] = bodies[i%sizeFiles]
		pull := codesurvivaltest.Pull{Number: i + 1, HeadRef: fmt.Sprintf("feature/%d", i+1), Author: "someone",
			Title: fmt.Sprintf("%s-1: change %d", widgetsProject, i+1), MergedAt: at, SHA: commit.SHA}
		if i%2 == 0 {
			pull.HeadRef, pull.Author = fmt.Sprintf("agent/%d", i+1), "agent-app[bot]"
		}
		pulls = append(pulls, pull)
		added += linesOf[i%sizeFiles]
		if n < sizeMeasured {
			tracked += linesOf[i%sizeFiles]
		}
	}
	if added != sizeLines || len(onDay) != sizeDays {
		t.Fatalf("the history adds %d lines over %d days, want %d over %d", added, len(onDay), sizeLines, sizeDays)
	}
	return codesurvivaltest.NewBranch(chain, patches, pulls), tracked
}

// insertPatch is a patch of exactly size bytes, as git writes one, that inserts lines lines at the
// top of path, each its own, all of one length but for a byte.
func insertPatch(path string, lines, size int) []byte {
	var patch bytes.Buffer
	patch.Grow(size)
	fmt.Fprintf(&patch, "diff --git a/%s b/%s\nindex 1111111..2222222 100644\n--- a/%s\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, path, path, path, lines)
	rest := size - patch.Len()
	width, longer := rest/lines, rest%lines
	for i := range lines {
		length := width
		if i < longer {
			length++
		}
		start := patch.Len()
		fmt.Fprintf(&patch, "+%s line %07d ", path, i)
		for patch.Len()-start < length-1 {
			patch.WriteByte('.')
		}
		patch.WriteByte('\n')
	}
	return patch.Bytes()
}

// liveProxy is the loopback App proxy of the INFRA-390 solution: an httptest server that answers
// the App's installation lookup and token mint itself, since GitHub never checks a JWT this test
// signs, and forwards every other read of a repository (a /repos/ path, or a /repositories/ path a
// Link header of GitHub's names) to upstream under authorization, with its path, query and Accept
// unchanged, through http.ProxyFromEnvironment. It writes its own origin in place of upstream's
// in a Link header, so the client follows each next page through it, and it records every read it
// forwarded. With pace set it forwards one read a tick; with capture it keeps the replay's form of
// each patch it forwards (githubPatches); hold is the negative control's.
type liveProxy struct {
	url           string
	upstream      string
	authorization string
	client        *http.Client
	pace          *time.Ticker
	hold          *patchHold
	capture       bool

	mu        sync.Mutex
	forwarded []forwardedRead
	// paced is how many reads waited more than 10 ms for their turn, and pacing how long all
	// of them waited.
	paced  int
	pacing time.Duration
	// parsed is the replay's form of each patch forwarded with capture, by its commit's SHA, and
	// unparsed each answer that did not parse.
	parsed   map[string]survival.Patch
	unparsed []string
}

// forwardedRead is one read the proxy forwarded: the request, upstream's status and the bytes of
// its answer.
type forwardedRead struct {
	codesurvivaltest.Request
	Status int
	Bytes  int64
}

// proxyAgent is the User-Agent of the proxy's reads, which GitHub requires.
const proxyAgent = "dispatch-codesurvival-live-test"

// livePermissions is what the proxy says the App's installation and its token may read.
var livePermissions = map[string]string{"contents": "read", "pull_requests": "read", "metadata": "read"}

// newLiveProxy starts the proxy in front of upstream; it stops when the test ends. perSecond is
// the most reads it forwards a second, none for 0.
func newLiveProxy(t *testing.T, upstream, authorization string, perSecond int) *liveProxy {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.MaxIdleConnsPerHost = 2 * streamWorkers
	p := &liveProxy{upstream: strings.TrimSuffix(upstream, "/"), authorization: authorization, client: &http.Client{Transport: transport},
		parsed: map[string]survival.Patch{}}
	if perSecond > 0 {
		p.pace = time.NewTicker(time.Second / time.Duration(perSecond))
		t.Cleanup(p.pace.Stop)
	}
	server := httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(server.Close)
	t.Cleanup(transport.CloseIdleConnections)
	t.Cleanup(func() {
		if p.hold != nil {
			p.hold.stop()
		}
	})
	p.url = server.URL
	return p
}

func (p *liveProxy) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "app" && parts[1] == "installations" && parts[3] == "access_tokens":
		proxyJSON(w, http.StatusCreated, map[string]any{"token": "codesurvival-live-proxy", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "permissions": livePermissions})
	case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "repos" && parts[3] == "installation":
		proxyJSON(w, http.StatusOK, map[string]any{"id": 1, "app_slug": "codesurvival-live", "permissions": livePermissions})
	case r.Method == http.MethodGet && len(parts) >= 3 && (parts[0] == "repos" || parts[0] == "repositories"):
		p.forward(w, r)
	default:
		proxyJSON(w, http.StatusNotFound, map[string]string{"message": "the loopback App proxy forwards only reads of a repository"})
	}
}

// forward sends r to upstream under the proxy's authorization and writes upstream's answer back.
func (p *liveProxy) forward(w http.ResponseWriter, r *http.Request) {
	read := forwardedRead{Request: codesurvivaltest.Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Accept: r.Header.Get("Accept")}}
	sha, patch := read.Patch()
	if patch && p.hold != nil && !p.hold.turn(r.Context(), sha) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if p.pace != nil {
		waited := time.Now()
		select {
		case <-p.pace.C:
		case <-r.Context().Done():
			return
		}
		if wait := time.Since(waited); wait > 10*time.Millisecond {
			p.mu.Lock()
			p.paced, p.pacing = p.paced+1, p.pacing+wait
			p.mu.Unlock()
		}
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.upstream+r.URL.RequestURI(), nil)
	if err != nil {
		http.Error(w, "the loopback App proxy could not build the read: "+err.Error(), http.StatusBadGateway)
		return
	}
	out.Header.Set("Accept", read.Accept)
	out.Header.Set("Authorization", p.authorization)
	out.Header.Set("User-Agent", proxyAgent)
	if etag := r.Header.Get("If-None-Match"); etag != "" {
		out.Header.Set("If-None-Match", etag)
	}
	response, err := p.client.Do(out)
	if err != nil {
		read.Status = http.StatusBadGateway
		p.record(read)
		http.Error(w, "the loopback App proxy could not reach upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		if key == "Link" {
			values = slices.Clone(values)
			for index := range values {
				values[index] = strings.ReplaceAll(values[index], p.upstream, p.url)
			}
		}
		w.Header()[key] = values
	}
	w.WriteHeader(response.StatusCode)
	read.Status = response.StatusCode
	ok := response.StatusCode == http.StatusOK
	keep := patch && p.hold != nil && p.hold.keep && ok
	capture := p.capture && ok && (patch || liveRoute(read) == RouteFiles)
	if keep || capture {
		body, _ := io.ReadAll(response.Body)
		read.Bytes = int64(len(body))
		_, _ = w.Write(body)
		if keep {
			p.hold.kept(body)
		}
		if capture {
			p.parse(read, body)
		}
	} else {
		read.Bytes, _ = io.Copy(w, response.Body)
	}
	p.record(read)
	if patch && p.hold != nil && ok {
		p.hold.answered(sha)
	}
}

func (p *liveProxy) record(read forwardedRead) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.forwarded = append(p.forwarded, read)
}

// reads is every read the proxy forwarded so far, in order.
func (p *liveProxy) reads() []forwardedRead {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.forwarded)
}

// parse keeps the replay's form of one answer forwarded with capture, and none of its text: a
// diff answer's file diffs (survival.ParseDiff), or a files[] page's, read as the run reads the
// pages (survival.FilesPatch, which reads each entry alone), added to its commit's earlier pages';
// the first page starts the commit's patch again, in place of a diff the run refused for passing
// its limit.
func (p *liveProxy) parse(read forwardedRead, body []byte) {
	if sha, ok := read.Patch(); ok {
		files, err := survival.ParseDiff(body)
		p.mu.Lock()
		defer p.mu.Unlock()
		if err != nil {
			p.unparsed = append(p.unparsed, fmt.Sprintf("%s: %v", sha, err))
			return
		}
		p.parsed[sha] = survival.Patch{Files: files}
		return
	}
	sha := filesSHA(read)
	var page struct {
		Files []survival.FilesEntry `json:"files"`
	}
	err := json.Unmarshal(body, &page)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.unparsed = append(p.unparsed, fmt.Sprintf("%s's files[]: %v", sha, githubapp.ShownErr(err)))
		return
	}
	files, withheld := survival.FilesPatch(page.Files)
	earlier := p.parsed[sha]
	if read.Query.Get("page") == "1" {
		earlier = survival.Patch{}
	}
	p.parsed[sha] = survival.Patch{Files: append(earlier.Files, files...), Withheld: earlier.Withheld + withheld}
}

// githubPatches is each patch the proxy forwarded with capture, by its commit's SHA, in the
// replay's form, with the files a files[] read withheld; and each answer that did not parse.
func (p *liveProxy) githubPatches() (map[string]survival.Patch, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.parsed), slices.Clone(p.unparsed)
}

// filesSHA is the commit a files[] read names, the last part of its path, whether a /repos/ or a
// /repositories/ path.
func filesSHA(read forwardedRead) string {
	parts := strings.Split(read.Path, "/")
	return parts[len(parts)-1]
}

// liveBudget is the installation's core budget as GET /rate_limit answers it.
type liveBudget struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Used      int   `json:"used"`
	Reset     int64 `json:"reset"`
}

// budget reads the installation's core budget from upstream's GET /rate_limit, which GitHub does
// not count against it, under the proxy's authorization.
func (p *liveProxy) budget(t *testing.T, ctx context.Context) liveBudget {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.upstream+"/rate_limit", nil)
	if err != nil {
		t.Fatalf("build the budget read: %v", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", p.authorization)
	request.Header.Set("User-Agent", proxyAgent)
	response, err := p.client.Do(request)
	if err != nil {
		t.Fatalf("read the installation's budget: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("GET /rate_limit answered %d: %s (%v)", response.StatusCode, githubapp.ShownBody(body), err)
	}
	var answer struct {
		Resources struct {
			Core liveBudget `json:"core"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Resources.Core.Limit == 0 {
		t.Fatalf("GET /rate_limit answered no core budget: %s (%v)", githubapp.ShownBody(body), err)
	}
	return answer.Resources.Core
}

func proxyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// patchHold is the negative controls' hold. The proxy holds the read of first's patch, the patch
// the replay needs first, until others other patch reads were answered, so the stream, its
// buffer lifted, holds every other patch before the replay starts. With keep, the proxy also
// keeps each patch answer's bytes, as upstream answered them, until first's is answered, up to
// keepCap. stop ends the hold at once and lets the kept bytes go.
type patchHold struct {
	first  string
	others int
	keep   bool

	ready    chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	mu        sync.Mutex
	count     int
	held      [][]byte
	keptCount int
	keptBytes int64
	released  bool
}

func newPatchHold(first string, others int, keep bool) *patchHold {
	return &patchHold{first: first, others: others, keep: keep, ready: make(chan struct{}), stopped: make(chan struct{})}
}

// turn waits, for first's patch, until the others were answered; it reports false where the hold
// stopped or the read's context ended first.
func (h *patchHold) turn(ctx context.Context, sha string) bool {
	if sha != h.first {
		return true
	}
	select {
	case <-h.ready:
		return true
	case <-h.stopped:
		return false
	case <-ctx.Done():
		return false
	}
}

// answered counts one patch answered; first's lets the kept bytes go.
func (h *patchHold) answered(sha string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sha == h.first {
		h.held, h.released = nil, true
		return
	}
	if h.count++; h.count == h.others {
		close(h.ready)
	}
}

// kept keeps one patch answer's bytes until first's is answered.
func (h *patchHold) kept(body []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released || h.keptBytes >= keepCap {
		return
	}
	h.held = append(h.held, body)
	h.keptCount++
	h.keptBytes += int64(len(body))
}

// keptSoFar is how many patch answers, and how many bytes, the hold has kept in all.
func (h *patchHold) keptSoFar() (int, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.keptCount, h.keptBytes
}

func (h *patchHold) stop() {
	h.stopOnce.Do(func() { close(h.stopped) })
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held, h.released = nil, true
}

// bootClient is the server's App client as cmd/dispatch's boot builds it, for a throwaway App
// whose key the test makes: the App read from DISPATCH_APP_CLIENT_ID and DISPATCH_APP_PEM_B64
// (auth.LoadAppFromEnv, as loadAppCredentials reads it), and githubapp.New with
// DISPATCH_GITHUB_API_BASE, set to base, and the rate limits of the server's database
// (api.NewDeps).
func bootClient(t *testing.T, pool *store.Pool, base string) *githubapp.Client {
	t.Helper()
	_, encoded := githubapptest.Key(t)
	t.Setenv("DISPATCH_APP_CLIENT_ID", "Iv1.codesurvival-live")
	t.Setenv("DISPATCH_APP_PEM_B64", base64.StdEncoding.EncodeToString([]byte(encoded)))
	t.Setenv("DISPATCH_GITHUB_API_BASE", base)
	app, err := auth.LoadAppFromEnv(os.Getenv)
	if err != nil || app == nil {
		t.Fatalf("load the App from the environment: %v", err)
	}
	client, err := githubapp.New(app, strings.TrimSpace(os.Getenv("DISPATCH_GITHUB_API_BASE")), githubapp.WithLimits(ratelimit.New(pool)))
	if err != nil || client == nil {
		t.Fatalf("the App client: %v", err)
	}
	return client
}

// seedLive loads k5's issues (lg/issues.tsv: each key, its project and its own repository) into
// the rig's database as TestReferenceIssuesAreAttributionsSourcesThreeAndFour loads them, with no
// link, under their projects; liveProject, made first, has liveRepo as its default repository and
// liveGroups as its code groups. It answers how many issues and projects it loaded.
func (r *rig) seedLive(t *testing.T, ref survivaltest.Reference) (issues, projects int) {
	t.Helper()
	r.project(liveProject, liveRepo, liveGroups)
	loaded := ref.Issues(t)
	keys, owners, repos := make([]string, len(loaded)), make([]string, len(loaded)), make([]string, len(loaded))
	numbers := make([]int32, len(loaded))
	for at, issue := range loaded {
		keys[at], owners[at], numbers[at], repos[at] = issue.Key, issue.Project, issue.Number, issue.Repo
	}
	named := slices.Compact(slices.Sorted(slices.Values(slices.Clone(owners))))
	for _, project := range named {
		if _, err := r.database.Pool.Exec(r.ctx, `insert into projects (key, name, default_assignee) values ($1, $1, 'codesurvival-live') on conflict (key) do nothing`, project); err != nil {
			t.Fatalf("make the project %s: %v", project, err)
		}
	}
	tag, err := r.database.Pool.Exec(r.ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank, repo)
		select u.key, u.project, u.number, u.key, 'todo', '{"kind":"system","id":"codesurvival-live"}'::jsonb, u.key, nullif(u.repo, '')
		from unnest($1::text[], $2::text[], $3::int[], $4::text[]) as u(key, project, number, repo)`, keys, owners, numbers, repos)
	if err != nil || tag.RowsAffected() != int64(len(keys)) {
		t.Fatalf("load the %d issues of lg/issues.tsv: %v (%d loaded)", len(keys), err, tag.RowsAffected())
	}
	return len(keys), len(named)
}

// liveReferenceChain is k5's chain of the repository as Measure reads it: lg/commits.txt, each
// commit with the facts of the merged pull request of lg/pulls.json it landed as and the issue
// lg/pr_issue.tsv gives it.
func liveReferenceChain(t *testing.T, ref survivaltest.Reference) []survival.Commit {
	t.Helper()
	merged := map[string]survivaltest.Pull{}
	for _, pull := range ref.Pulls(t) {
		if pull.Merged() {
			merged[pull.MergeSHA] = pull
		}
	}
	issues := map[string]string{}
	for _, issue := range ref.PullIssues(t) {
		issues[issue.MergeSHA] = issue.Issue
	}
	var chain []survival.Commit
	for _, commit := range ref.Chain(t) {
		next := survival.Commit{SHA: commit.SHA, Parents: commit.Parents, CommittedAt: commit.CommittedAt}
		if pull, ok := merged[commit.SHA]; ok {
			next.Change = survival.ChangeFacts{PR: pull.Number, HeadRef: pull.HeadRef, Author: pull.Author, IssueKey: issues[commit.SHA]}
		}
		chain = append(chain, next)
	}
	if len(chain) == 0 {
		t.Fatalf("lg/commits.txt of %s holds no commit", ref.Dir)
	}
	return chain
}

// gitPatch is git's patch of sha from the reference directory, read through reader and parsed.
func gitPatch(reader *survivaltest.PatchReader, ref survivaltest.Reference, sha string) ([]survival.FileDiff, error) {
	text, err := reader.Read(ref.PatchPath(sha))
	if err != nil {
		return nil, err
	}
	return survival.ParseDiff(text)
}

// patchDifference says how a commit's patch as GitHub served it differs from git's, as the
// replay reads both (the files' paths, kinds and zero-context hunks, in any order of files), and
// is "" where they are the same.
func patchDifference(github survival.Patch, git []survival.FileDiff) string {
	type side struct {
		file          survival.FileDiff
		added, killed int
	}
	index := func(files []survival.FileDiff) map[string]side {
		out := map[string]side{}
		for _, file := range files {
			entry := side{file: file}
			for _, hunk := range file.Hunks {
				entry.added += hunk.Added
				entry.killed += hunk.Removed
			}
			out[file.OldPath+"\x00"+file.NewPath] = entry
		}
		return out
	}
	ours, theirs := index(github.Files), index(git)
	var notes []string
	if github.Withheld > 0 {
		notes = append(notes, fmt.Sprintf("GitHub withheld %d files", github.Withheld))
	}
	for key, g := range ours {
		w, ok := theirs[key]
		name := strings.ReplaceAll(strings.Trim(key, "\x00"), "\x00", " → ")
		switch {
		case !ok:
			notes = append(notes, fmt.Sprintf("%s is GitHub's alone (+%d −%d)", name, g.added, g.killed))
		case g.file.Status != w.file.Status || g.file.Binary != w.file.Binary || g.file.Submodule != w.file.Submodule || !slices.Equal(g.file.Hunks, w.file.Hunks):
			notes = append(notes, fmt.Sprintf("%s: GitHub's %s +%d −%d in %d hunks, git's %s +%d −%d in %d", name, g.file.Status, g.added, g.killed, len(g.file.Hunks),
				w.file.Status, w.added, w.killed, len(w.file.Hunks)))
		}
	}
	for key, w := range theirs {
		if _, ok := ours[key]; !ok {
			notes = append(notes, fmt.Sprintf("%s is git's alone (+%d −%d)", strings.ReplaceAll(strings.Trim(key, "\x00"), "\x00", " → "), w.added, w.killed))
		}
	}
	slices.Sort(notes)
	return strings.Join(notes, "; ")
}

// liveRoute is the route of the run a forwarded read is one of, "" for none: a read of a
// /repositories/{id}/ path, which GitHub's Link headers name, by the route of its /repos/ form.
func liveRoute(read forwardedRead) Route {
	parts := strings.Split(strings.TrimPrefix(read.Path, "/"), "/")
	var rest []string
	switch {
	case len(parts) >= 3 && parts[0] == "repos":
		rest = parts[3:]
	case len(parts) >= 2 && parts[0] == "repositories":
		rest = parts[2:]
	default:
		return ""
	}
	diff := read.Accept == codesurvivaltest.DiffMediaType
	switch {
	case len(rest) == 0, len(rest) >= 4 && rest[0] == "git" && rest[1] == "ref" && rest[2] == "heads":
		return RouteRepository
	case len(rest) == 1 && rest[0] == "commits":
		return RouteChain
	case len(rest) == 2 && rest[0] == "commits" && diff:
		return RouteDiff
	case len(rest) == 2 && rest[0] == "commits":
		return RouteFiles
	case len(rest) == 2 && rest[0] == "compare" && diff:
		return RouteCompare
	case len(rest) == 1 && rest[0] == "pulls":
		return RoutePulls
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "commits":
		return RouteAttribution
	}
	return ""
}

// countsOf is reads counted by route as the run's Counts counts them, and each read of no route.
func countsOf(reads []forwardedRead) (Counts, []string) {
	var counts Counts
	var others []string
	for _, read := range reads {
		route := liveRoute(read)
		if route == "" {
			others = append(others, read.String())
			continue
		}
		counts.add(route, 1)
		counts.Bytes += read.Bytes
	}
	return counts, others
}

// requestsOf is the requests of reads, as the double records them.
func requestsOf(reads []forwardedRead) []codesurvivaltest.Request {
	out := make([]codesurvivaltest.Request, len(reads))
	for index, read := range reads {
		out[index] = read.Request
	}
	return out
}

// liveRun is what one pass through the proxy recorded: the run's log line (nil where the pass
// was cut short and logged none), the reads the proxy forwarded, the most live heap after GC the
// sampler read, whether the sampler cut the pass short past its bound, and how long it took.
type liveRun struct {
	line    map[string]any
	reads   []forwardedRead
	heap    uint64
	aborted bool
	took    time.Duration
}

// passThrough makes one run pass with the clock at now, the live heap after GC sampled every
// period from a collection before it; the first reading past abortAbove cuts the pass short and
// stops the proxy's hold.
func (r *rig) passThrough(proxy *liveProxy, now time.Time, period time.Duration, abortAbove uint64) liveRun {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	logged := len(r.logs.lines(r.t, "codesurvival: run"))
	r.clock.Set(now)
	runtime.GC()
	stop := sampleLiveHeap(period, abortAbove, func() {
		cancel()
		if proxy.hold != nil {
			proxy.hold.stop()
		}
	})
	started := time.Now()
	r.run.Pass(ctx)
	took := time.Since(started)
	heap, aborted := stop()
	run := liveRun{reads: proxy.reads(), heap: heap, aborted: aborted, took: took}
	if lines := r.logs.lines(r.t, "codesurvival: run"); len(lines) > logged {
		run.line = lines[len(lines)-1]
	}
	return run
}

// sampleLiveHeap reads the live heap after garbage collection every period until its stop, which
// answers the most it read and whether a reading passed abortAbove; the first that did called
// abort, once.
func sampleLiveHeap(period time.Duration, abortAbove uint64, abort func()) (stop func() (peak uint64, aborted bool)) {
	sample := []metrics.Sample{{Name: liveHeapMetric}}
	var most atomic.Uint64
	var over atomic.Bool
	keep := func() {
		metrics.Read(sample)
		value := sample[0].Value.Uint64()
		for was := most.Load(); value > was && !most.CompareAndSwap(was, value); was = most.Load() {
		}
		if value > abortAbove && over.CompareAndSwap(false, true) {
			abort()
		}
	}
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		for {
			keep()
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() (uint64, bool) {
		close(done)
		<-finished
		keep()
		return most.Load(), over.Load()
	}
}

// requireLiveHeapMetric fails t where this runtime has no live heap metric to sample.
func requireLiveHeapMetric(t *testing.T) {
	t.Helper()
	sample := []metrics.Sample{{Name: liveHeapMetric}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		t.Fatalf("%s is no uint64 metric of this runtime", liveHeapMetric)
	}
}

// figure is the number key names in a run's log line.
func figure(line map[string]any, key string) int64 {
	value, _ := line[key].(float64)
	return int64(value)
}

// liveFirst is the first n of items, sorted, for a failure's message.
func liveFirst(items []string, n int) string {
	items = slices.Sorted(slices.Values(items))
	if len(items) > n {
		items = append(items[:n:n], fmt.Sprintf("… and %d more", len(items)-n))
	}
	return strings.Join(items, ", ")
}
