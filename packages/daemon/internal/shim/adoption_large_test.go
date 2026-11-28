package shim_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// largeCheckoutEnv names the bare git repository TestAdoptionOnALargeCheckout provisions its
// workspace from, one holding main. CI sets no such variable; the builds Job of
// scripts/e2e/adopt-large-checkout-job.sh does, over a repository of 342,000 files it writes.
const largeCheckoutEnv = "LEGION_TEST_LARGE_CHECKOUT"

// adoptionBound is R4's bound on one adoption of a fresh working copy of a checkout of 342,000
// files (dispatch://INFRA-3916, check c-adopt-large-checkout).
const adoptionBound = 10 * time.Second

// largeCheckoutBudget is the budget, in milliseconds, each adoption of TestAdoptionOnALargeCheckout
// is given: the daemon's default slow_command_timeout_seconds, 300, so an adoption past
// adoptionBound still ends and its time is read.
const largeCheckoutBudget = 300_000

// largeCheckoutTime begins each line of TestAdoptionOnALargeCheckout that gives one elapsed time,
// as `adopt-large-checkout time: <what> <seconds> s`, which the Job's verdict reads.
const largeCheckoutTime = "adopt-large-checkout time:"

// R4 on a large checkout: each adoption of a fresh working copy ends within 10 s on a checkout of
// 342,000 files. The workspace is an issue pod's, provisioned by workspace.Provision from the bare
// repository LEGION_TEST_LARGE_CHECKOUT names (provisionPodWorkspace), and each adoption is the
// frame through the real shim, as the daemon sends it before a phase task, so runAdoption runs
// with its freshness check, with the first jj on PATH: once right after the checkout, three times
// warm under a new identity each time, so each metaedit rewrites @'s author, and once on the `jj
// new` branch, after a describe of @ that keeps its tree. Each time is from the frame's send to
// the shim's answer. Then main's two commands before this change, the adoption's pair with no
// --ignore-working-copy, which snapshot the working copy, run on the same workspace as the
// baseline: their times are reported, not judged.
func TestAdoptionOnALargeCheckout(t *testing.T) {
	remote := os.Getenv(largeCheckoutEnv)
	if remote == "" {
		t.Skip(largeCheckoutEnv + " is unset: this test runs only where " + largeCheckoutEnv + " names a directory holding a bare git repository")
	}
	jj := fixturegit.CompiledJJ(t)
	inspect := fixturegit.Isolated(t, t.TempDir())
	if bare, err := inspect.Try(remote, "git", "rev-parse", "--is-bare-repository"); err != nil || strings.TrimSpace(bare) != "true" {
		t.Fatalf("%s=%s, which names no directory holding a bare git repository: git rev-parse --is-bare-repository there answered %q (%v)", largeCheckoutEnv, remote, strings.TrimSpace(bare), err)
	}
	files := strings.Count(inspect.Raw(remote, "git", "ls-tree", "-r", "-z", "--name-only", "main"), "\x00")
	commits := inspect.Run(remote, "git", "rev-list", "--count", "main")
	t.Logf("adopt-large-checkout: %s holds %d tracked files on main, over %s commits; %s", remote, files, commits, inspect.Run(remote, jj, "--version"))

	start := time.Now()
	// The runner's budget bounds the provision's commands whose time does not follow the
	// repository's size; the clone and the checkout keep their own bounds.
	w := provisionPodWorkspace(t, jj, remote, 10*time.Minute)
	t.Logf("%s provision %.3f s", largeCheckoutTime, time.Since(start).Seconds())

	bin := t.TempDir()
	if err := os.Symlink(jj, filepath.Join(bin, "jj")); err != nil {
		t.Fatal(err)
	}
	adopt := func(what, user string) {
		t.Helper()
		p, shim := w.startShim(bin)
		start := time.Now()
		got := w.askAdoption(p, user, largeCheckoutBudget)
		took := time.Since(start)
		t.Logf("%s %s %.3f s", largeCheckoutTime, what, took.Seconds())
		if !got.OK || len(got.EditsCommits) != 0 {
			t.Fatalf("the adoption %s answered %#v, want a successful adoption of a fresh working copy, which names no commit; log:\n%s", what, got, shim.log)
		}
		if took > adoptionBound {
			t.Errorf("the adoption %s took %s, past R4's bound of %s", what, took, adoptionBound)
		}
	}
	field := func(user, revision, template string) string {
		t.Helper()
		return w.as(user, "jj", "--ignore-working-copy", "log", "-r", revision, "--no-graph", "-T", template)
	}

	adopt("adoption-cold", "implementer")
	for i, user := range []string{"tester", "reviewer", "merger"} {
		adopt("adoption-warm-"+strconv.Itoa(i+1), user)
		if author := field(user, "@", "author.name()"); author != user {
			t.Fatalf("@'s author after %s's adoption is %q, want %q: the metaedit did not run", user, author, user)
		}
	}
	w.as("merger", "jj", "--ignore-working-copy", "describe", "-m", "feat: the merger's change")
	described := field("merger", "@", "commit_id")
	adopt("adoption-jj-new", "architect")
	if parent := field("architect", "@-", "commit_id"); parent != described {
		t.Fatalf("after the adoption of a described @, @'s parent is %s, want the described commit %s: jj new did not run", parent, described)
	}

	// main's describedArgs and adoptionArgs, before this change gave each --ignore-working-copy.
	baseline := func(what string, argv ...string) time.Duration {
		t.Helper()
		start := time.Now()
		w.as("planner", argv...)
		took := time.Since(start)
		t.Logf("%s %s %.3f s", largeCheckoutTime, what, took.Seconds())
		return took
	}
	read := baseline("baseline-log", "jj", "log", "-r", `@ ~ description(exact:"")`, "--no-graph", "-T", "commit_id", "-R", w.dir)
	change := baseline("baseline-metaedit", "jj", "metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", w.dir)
	t.Logf("%s baseline-pair %.3f s", largeCheckoutTime, (read + change).Seconds())
}
