package cireconcile

import (
	"fmt"
	"strings"
	"testing"
)

// absentRow is a repository the App is installed on that GitHub then answers 404 for. It sorts
// before the harness's repository, so a pass reads it first and has to go on from it.
const absentRow = "acme/absent"

// renamedAwayRow is the name the harness's repository was renamed away from, which a rename pair
// records (seedRenamePair) and which a row the boot's rename pass missed can still name.
// The GitHub stand-in knows no repository under it, so every read of it is a 404, as GitHub
// answers for a name a repository no longer has.
const renamedAwayRow = "acme/widgets-before"

// uninstalledRow is a repository the App is not installed on: the GitHub stand-in knows no
// repository under it, so its installation lookup answers 404, as GitHub answers for a
// repository the App was never installed on. It sorts before the harness's repository, so a pass
// reads it first and has to go on from it.
const uninstalledRow = "acme/elsewhere"

// installationPath is the lookup a pass asks GitHub whether the App is installed on a
// repository with.
func installationPath(repository string) string {
	return "/repos/" + repository + "/installation"
}

// A project's default repository GitHub does not answer for is skipped, counted and named, and
// the pass goes on to the repositories after it: one repository GitHub has stopped answering
// about must not cost every other repository its pass.
func TestAProjectDefaultGitHubDoesNotAnswerIsSkipped(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(absentRow)
	h.seedProjectDefault(rowRepository)
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
	if got := number(t, line, "defaults_skipped"); got != 1 {
		t.Errorf("the pass line reports %d repositories skipped, want 1: %v", got, line)
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

// A project's default repository that was renamed away is reconciled under the name the
// repository is called today, as is an issue's own repository under that old name, so the pass
// asks GitHub nothing about the old name and skips nothing for it. GitHub answers 404 for the
// installation of a name a repository was renamed away from, so a pass reading the stored name
// holds no token for it and logs `ci reconcile skipped a repository it holds no token for` on
// every pass for ever, about a name no one can read by design. The boot's rename pass writes the
// new name into both, so the old name stands only where that pass failed, and this read is what
// holds then. The default that names no rename is reconciled exactly as it was: its head settles
// on the checks GitHub holds.
func TestARenamedProjectDefaultIsReconciledUnderTheNameItHasNow(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
	h.seedRenamePair(renamedAwayRow, rowRepository)
	h.seedProjectDefault(renamedAwayRow)
	h.issueInRepository(renamedAwayRow)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-05T03:01:00Z"),
	)
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after a pass over a project default renamed away and the project default it names")
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
	if got := number(t, line, "defaults_skipped"); got != 0 {
		t.Errorf("the pass line reports %d repositories skipped, want none: %v", got, line)
	}
	if got := number(t, line, "heads"); got != 1 {
		t.Errorf("the pass read %d heads, want the one head of the repository both names name: %v", got, line)
	}
	if asked := h.queriesFor(gitHubRepository); asked != 1 {
		t.Errorf("%s was queried %d times, want once for the two defaults and the issue naming it", gitHubRepository, asked)
	}
}

// A repository only an issue names that the App is not installed on is no skipped project
// default. An issue's own repository is any `owner/name`, so one the App is not on is expected,
// and it stays in the order for as long as an issue names it, closed or not. A pass that counted
// it in defaults_skipped and named it at WARN would do both at every pass for ever, and neither
// would mean a project default to mend any longer. So it is counted apart and named once at
// INFO, and GitHub is asked about it once rather than once a pass: githubapp keeps only an
// installation it found, so the pass holds this answer itself.
func TestAnIssuesRepositoryWithoutTheAppIsNoSkippedDefaultAndIsAskedAboutOnce(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(rowRepository)
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
		if got := number(t, line, "defaults_skipped"); got != 0 {
			t.Errorf("pass %d reports %d repositories skipped, want none, since no project's default names %s: %v", pass+1, got, uninstalledRow, line)
		}
		if got := number(t, line, "heads"); got != 1 {
			t.Errorf("pass %d read %d heads, want the one head of the project's default repository: %v", pass+1, got, line)
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

// A project's default repository the App is not installed on is a fault to mend. It is named at
// WARN and counted in defaults_skipped at every pass the fault stands for, whether or not an
// issue names it too, and it is no part of the count of repositories only an issue names.
// GitHub is asked about it once all the same: the answer is held whoever names the repository.
func TestAProjectDefaultWithoutTheAppIsSkippedAtEveryPassAndAskedAboutOnce(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault(uninstalledRow)
	h.issueInRepository(uninstalledRow)
	h.seedProjectDefault(rowRepository)
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
		t.Fatalf("the project's default repository was named at WARN %d times, want once a pass for %d passes:\n%s", len(warned), passes, h.logs.String())
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
		if got := number(t, line, "defaults_skipped"); got != 1 {
			t.Errorf("pass %d reports %d repositories skipped, want the 1 the App is missing from: %v", pass+1, got, line)
		}
		if got := number(t, line, "heads"); got != 1 {
			t.Errorf("pass %d read %d heads, want the one head of the other project's default repository: %v", pass+1, got, line)
		}
		if got := number(t, line, "repositories_without_app"); got != 0 {
			t.Errorf("pass %d reports %d repositories without the App, want none, since a project's default names this one: %v", pass+1, got, line)
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
	h.seedProjectDefault(rowRepository)
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

	// The listener boots and the bucket arrives with it, on its own clock: its store makes the
	// bucket here on a fixture's connection (startListener), as `envoy-infra buckets` makes a
	// deployment's before its listener starts.
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

// A stored name no repository GitHub could hold is skipped before the pass asks GitHub anything
// about it, counted and named at WARN under the name the pass reads (splitRepo,
// refs.SplitGitHubRepo): project defaults stored as `acme/..`, `acme/.git` and `acme/`, as a
// write before the routes held a name to GitHub's names or direct SQL could leave them, an
// issue's own `Zeta/..`, and the rename pair's new name `acme/...git`, which the default
// `oldco/tools` is read under.
//
// The negative control is splitRepo reading a name as two non-empty halves, as it did, which
// takes `acme/..`, `acme/.git`, `Zeta/..` and `acme/...git` and asks GitHub about
// `/repos/acme/../installation`.
func TestARefusedStoredNameIsSkippedAndNamed(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.seedProjectDefault("acme/..")
	h.seedProjectDefault("acme/.git")
	h.seedProjectDefault("acme/")
	h.issueInRepository("Zeta/..")
	h.seedRenamePair("oldco/tools", "acme/...git")
	h.seedProjectDefault("oldco/tools")

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
		if _, named := skipped[row]; !named {
			t.Errorf("no WARN names %s:\n%s", row, h.logs.String())
		}
	}
	if got := number(t, h.passLine(), "defaults_skipped"); got != len(refused) {
		t.Errorf("the pass line reports %d repositories skipped, want %d", got, len(refused))
	}
}
