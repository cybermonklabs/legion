package cireconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// absentRow is a repository the App is installed on that GitHub then answers 404 for. It sorts
// before the mapped repository, so a pass reads it first and has to go on from it.
const absentRow = "acme/absent"

// renamedAwayRow is the name the mapped repository was renamed away from, held by a mapping of
// its own as repo_projects holds both names of one repository. The GitHub stand-in knows no
// repository under it, so every read of it is a 404, as GitHub answers for a name a repository
// no longer has.
const renamedAwayRow = "acme/widgets-before"

// uninstalledRow is a repository the App is not installed on: the GitHub stand-in knows no
// repository under it, so its installation lookup answers 404, as GitHub answers for a
// repository the App was never installed on. It sorts before the mapped repository, so a pass
// reads it first and has to go on from it.
const uninstalledRow = "acme/elsewhere"

// installationPath is the lookup a pass asks GitHub whether the App is installed on a
// repository with.
func installationPath(repository string) string {
	return "/repos/" + repository + "/installation"
}

// A mapping GitHub does not answer for is skipped, counted and named, and the pass goes on to
// the repositories after it: one repository GitHub has stopped answering about must not cost
// every other repository its pass.
func TestAMappingGitHubDoesNotAnswerIsSkipped(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(absentRow)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.hideRepository(absentRow)
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after a pass over a repository GitHub did not answer for")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the second repository's head settled on its one passing check", settled)
	}
	line := h.passLine()
	if got := number(t, line, "mappings_skipped"); got != 1 {
		t.Errorf("the pass line reports %d mappings skipped, want 1: %v", got, line)
	}
	if got := number(t, line, "heads"); got != 1 {
		t.Errorf("the pass read %d heads, want the one head of the repository GitHub answered for: %v", got, line)
	}
	skipped := h.lines("ci reconcile skipped a repository GitHub did not answer for")
	if len(skipped) != 1 {
		t.Fatalf("the skipped repository was named %d times, want once:\n%s", len(skipped), h.logs.String())
	}
	if skipped[0]["repository"] != absentRow {
		t.Fatalf("the WARN names repository %v, want %s", skipped[0]["repository"], absentRow)
	}
}

// A mapping whose repository was renamed away is reconciled under the name the repository is
// called today, so the pass asks GitHub nothing about the old name and skips nothing for it.
// GitHub answers 404 for the installation of a name a repository was renamed away from, so a
// pass reading that row's own name holds no token for it and logs `ci reconcile skipped a
// repository it holds no token for` on every pass for ever, about a name no one can read by
// design. The mapping that names no rename is reconciled exactly as it was: its head settles on
// the checks GitHub holds.
func TestARenamedMappingIsReconciledUnderTheNameItHasNow(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.mapRenamedRepository(renamedAwayRow, rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after a pass over a mapping renamed away and the mapping it names")
	if settled.Number != pullNumber || settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head of %s settled on its one passing check", settled, gitHubRepository)
	}
	if skipped := h.lines("ci reconcile skipped a repository it holds no token for"); len(skipped) != 0 {
		t.Fatalf("the pass skipped %d repositories it holds no token for, want none:\n%s", len(skipped), h.logs.String())
	}
	for _, request := range h.github.received() {
		if strings.Contains(strings.ToLower(request.url), renamedAwayRow) {
			t.Errorf("the pass asked GitHub about the name the repository was renamed away from: %s %s", request.method, request.url)
		}
	}
	line := h.passLine()
	if got := number(t, line, "mappings_skipped"); got != 0 {
		t.Errorf("the pass line reports %d mappings skipped, want none: %v", got, line)
	}
	if got := number(t, line, "heads"); got != 1 {
		t.Errorf("the pass read %d heads, want the one head of the repository both rows name: %v", got, line)
	}
	if asked := h.queriesFor(gitHubRepository); asked != 1 {
		t.Errorf("%s was queried %d times, want once for two rows naming it", gitHubRepository, asked)
	}
}

// A repository only an issue names that the App is not installed on is no skipped mapping. An
// issue's own repository is any `owner/name`, so one the App is not on is expected, and it
// stays in the order for as long as an issue names it, closed or not. A pass that counted it
// in mappings_skipped and named it at WARN would do both at every pass for ever, and neither
// would mean a mapping to mend any longer. So it is counted apart and named once at INFO, and
// GitHub is asked about it once rather than once a pass: githubapp keeps only an installation
// it found, so the pass holds this answer itself.
func TestAnIssuesRepositoryWithoutTheAppIsNoSkippedMappingAndIsAskedAboutOnce(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.issueInRepository(uninstalledRow)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)

	const passes = 3
	for range passes {
		h.pass()
	}

	lines := h.lines("ci reconcile pass")
	if len(lines) != passes {
		t.Fatalf("%d passes logged %d pass lines:\n%s", passes, len(lines), h.logs.String())
	}
	for pass, line := range lines {
		if got := number(t, line, "mappings_skipped"); got != 0 {
			t.Errorf("pass %d reports %d mappings skipped, want none, since no mapping names %s: %v", pass+1, got, uninstalledRow, line)
		}
		if got := number(t, line, "heads"); got != 1 {
			t.Errorf("pass %d read %d heads, want the one head of the mapped repository: %v", pass+1, got, line)
		}
	}
	if warned := h.lines("ci reconcile skipped a repository it holds no token for"); len(warned) != 0 {
		t.Errorf("the passes named a repository at WARN %d times, want never:\n%s", len(warned), h.logs.String())
	}
	if asked := h.github.countPath(installationPath(uninstalledRow)); asked != 1 {
		t.Errorf("GitHub was asked %d times whether the App is installed on %s, want once for %d passes", asked, uninstalledRow, passes)
	}
	named := h.lines("ci reconcile passed over a repository the App is not installed on")
	if len(named) != 1 {
		t.Fatalf("the repository was named %d times, want once, by the pass that asked GitHub:\n%s", len(named), h.logs.String())
	}
	if named[0]["repository"] != uninstalledRow || named[0]["level"] != "INFO" {
		t.Errorf("the line names repository %v at %v, want %s at INFO", named[0]["repository"], named[0]["level"], uninstalledRow)
	}
	for pass, line := range lines {
		if got := number(t, line, "repositories_without_app"); got != 1 {
			t.Errorf("pass %d reports %d repositories without the App, want 1: %v", pass+1, got, line)
		}
	}
}

// A mapped repository the App is not installed on is still a mapping to mend. It is named at
// WARN and counted in mappings_skipped at every pass the fault stands for, whether or not an
// issue names it too, and it is no part of the count of repositories only an issue names.
// GitHub is asked about it once all the same: the answer is held whoever names the repository.
func TestAMappedRepositoryWithoutTheAppIsSkippedAtEveryPassAndAskedAboutOnce(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(uninstalledRow)
	h.issueInRepository(uninstalledRow)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)

	const passes = 2
	for range passes {
		h.pass()
	}

	if asked := h.github.countPath(installationPath(uninstalledRow)); asked != 1 {
		t.Errorf("GitHub was asked %d times whether the App is installed on %s, want once for %d passes", asked, uninstalledRow, passes)
	}
	warned := h.lines("ci reconcile skipped a repository it holds no token for")
	if len(warned) != passes {
		t.Fatalf("the mapped repository was named at WARN %d times, want once a pass for %d passes:\n%s", len(warned), passes, h.logs.String())
	}
	for pass, line := range warned {
		if line["repository"] != uninstalledRow || line["level"] != "WARN" {
			t.Errorf("pass %d names repository %v at %v, want %s at WARN", pass+1, line["repository"], line["level"], uninstalledRow)
		}
		if text := fmt.Sprint(line["error"]); !strings.Contains(text, "not installed") {
			t.Errorf("pass %d gives the error %q, want GitHub's answer that the App is not installed", pass+1, text)
		}
	}
	lines := h.lines("ci reconcile pass")
	if len(lines) != passes {
		t.Fatalf("%d passes logged %d pass lines:\n%s", passes, len(lines), h.logs.String())
	}
	for pass, line := range lines {
		if got := number(t, line, "mappings_skipped"); got != 1 {
			t.Errorf("pass %d reports %d mappings skipped, want the 1 the App is missing from: %v", pass+1, got, line)
		}
		if got := number(t, line, "heads"); got != 1 {
			t.Errorf("pass %d read %d heads, want the one head of the other mapped repository: %v", pass+1, got, line)
		}
		if got := number(t, line, "repositories_without_app"); got != 0 {
			t.Errorf("pass %d reports %d repositories without the App, want none, since a mapping names this one: %v", pass+1, got, line)
		}
	}
}

// No bucket yet, no pass: a pass that runs before the listener has made envoy_ci_state binds no
// writer, says so and reads GitHub nothing -- there is nowhere to put what it would read -- and
// the pass after the listener has made the bucket records the head and settles it.
//
// Each pass binds once and keeps nothing: the bus dials a replacement when nats.go gives up on
// a connection, and a writer held from an earlier pass would write through the connection it
// gave up on, so every later pass would read GitHub in full and record nothing until a restart.
func TestNoBucketYetNoPass(t *testing.T) {
	h := newHarnessBeforeTheListener(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	// A pass runs on the test's own goroutine, so a plain count is read and written by one.
	binds := 0
	bind := h.reconciler.Open
	h.reconciler.Open = func() (Writer, error) {
		binds++
		return bind()
	}

	h.pass()

	if bound := h.lines("ci reconcile pass bound no CI record writer"); len(bound) != 1 {
		t.Fatalf("the pass logged %d lines saying it bound no writer, want one:\n%s", len(bound), h.logs.String())
	}
	if lines := h.lines("ci reconcile pass"); len(lines) != 0 {
		t.Fatalf("a pass that bound no writer logged a pass line: %v", lines)
	}
	if sent := h.github.count(); sent != 0 {
		t.Fatalf("a pass that bound no writer sent GitHub %d requests: %+v", sent, h.github.received())
	}
	if binds != 1 {
		t.Fatalf("the pass called Open %d times, want once", binds)
	}

	// The listener boots and makes the bucket, as it does on its own clock.
	h.startListener()
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after the listener made the bucket")
	if settled.SHA != headSHA || settled.Passed.Count != 1 {
		t.Fatalf("settlement = %+v, want the head settled on its one passing check", settled)
	}
	state, _, ok := h.record()
	if !ok {
		t.Fatal("the head has no record after the pass that bound the writer")
	}
	if _, held := state.Checks["ci-ok"]; !held {
		t.Fatalf("the record holds %+v, want ci-ok", state.Checks)
	}

	h.pass()

	if binds != 3 {
		t.Fatalf("Open was called %d times over three passes, want once a pass: a pass that kept the writer it bound would write through a connection the client may have replaced", binds)
	}
}

// A mapped name no repository GitHub could hold is skipped, counted and named at WARN, with the
// fields Reconciler.MappingRepairs answers for that name beside it. In cmd/dispatch those fields
// name the settings route's request that mends the mapping (api.MappingRepairs), and that
// function's own test (api/repo_project_repairs_test.go) sends each request it names through the
// route. Here a stand-in answers fields that name the row it was asked about, so this test holds
// what the pass does with them: it asks once about each refused mapped name, under the name the
// pass reads, which for the mapping `oldco/tools` is its renamed_to `acme/...git`, and each WARN
// carries the fields answered for its own name.
//
// The negative control is a pass that asks MappingRepairs nothing, whose WARNs carry neither
// field.
func TestARefusedMappedNameIsNamedWithItsMappingRepairs(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	var asked []string
	h.reconciler.MappingRepairs = func(_ context.Context, row string) []any {
		asked = append(asked, row)
		return []any{"removed_by", "the removal of " + row, "mended_by", "the rename of " + row}
	}
	h.mapRepository("acme/..")
	h.mapRepository("acme/.git")
	h.mapRepository("acme/")
	h.mapRepository("Zeta/..")
	h.mapRenamedRepository("oldco/tools", "acme/...git")

	h.pass()

	refused := []string{"acme/..", "acme/.git", "acme/", "Zeta/..", "acme/...git"}
	skipped := map[string]map[string]any{}
	for _, line := range h.lines("ci reconcile skipped a repository whose name is not an owner/name GitHub allows") {
		skipped[fmt.Sprint(line["repository"])] = line
	}
	if len(skipped) != len(refused) {
		t.Fatalf("the pass named %d refused names, want %q:\n%s", len(skipped), refused, h.logs.String())
	}
	for _, row := range refused {
		line := skipped[row]
		for field, want := range map[string]string{"removed_by": "the removal of " + row, "mended_by": "the rename of " + row} {
			if got := line[field]; got != want {
				t.Errorf("the WARN for %s names %s %v, want %q: %v", row, field, got, want, line)
			}
		}
	}
	slices.Sort(asked)
	if want := slices.Sorted(slices.Values(refused)); !slices.Equal(asked, want) {
		t.Errorf("MappingRepairs was asked about %q, want each refused mapped name once: %q", asked, want)
	}
	if got := number(t, h.passLine(), "mappings_skipped"); got != len(refused) {
		t.Errorf("the pass line reports %d mappings skipped, want %d", got, len(refused))
	}
}
