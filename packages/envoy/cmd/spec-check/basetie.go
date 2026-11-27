package main

import (
	"io"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// baseLine starts the first line of the list that changed prints and citations --changed reads,
// "base <object name>": the commit that the paths of the list changed since.
const baseLine = "base "

// objectName is whether s is a full git object name: 40 lower-case hex digits, or 64 in a
// repository of SHA-256. citations names commits so in its own files, since a short or a symbolic
// name can name another commit tomorrow.
func objectName(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}

// isCloneTop is whether repo is the top of its git clone, the one directory from which changed
// names each path and citations reads the ties and the code; when it is not, it has written the
// one line that names the top that git rev-parse --show-toplevel gives (specgraph.GitHistory.Top),
// to pass instead, or why git gives none. A directory below the top would make the two commands
// disagree, since git names each changed path from the top whatever directory the command names.
// The top named through a link is the top, since the two are one directory.
func isCloneTop(stderr io.Writer, repo string) bool {
	top, err := specgraph.GitHistory{Dir: repo}.Top()
	if err != nil {
		say(stderr, "%s: git finds no work tree there: %s", repo, err)
		return false
	}
	repoInfo, repoErr := os.Stat(repo)
	topInfo, topErr := os.Stat(top)
	if repoErr != nil || topErr != nil || !os.SameFile(repoInfo, topInfo) {
		say(stderr, "%s: is not the top of its git clone; pass the top, --repo %s", repo, top)
		return false
	}
	return true
}

// derivedTies is the tie of each check that the .github/scripts/go-test-reads.sh of a repository
// derives (specgraph.GoTies), one check at a time on the first ask, each set of the script's
// arguments asked once. It memoizes across the per-check asks of specgraph.Citations, which GoTies
// cannot, since each ask is a new call of GoTies. specgraph.Citations asks for a check's tie only
// when its test did not run, so a run where each test ran runs no script.
type derivedTies struct {
	reads   specgraph.TestReads
	answers map[string]readsAnswer
}

// readsAnswer is one answer of go-test-reads.sh.
type readsAnswer struct {
	reads []string
	err   error
}

// newDerivedTies is the derived ties of the repository at root, whose go-test-reads.sh runs in
// root, where the root go.work takes the import paths of the repository's modules, with environ
// as its whole environment, nil for the process's own (specgraph.ScriptTestReads).
func newDerivedTies(root string, environ []string) *derivedTies {
	return &derivedTies{reads: specgraph.ScriptTestReads(root, environ), answers: map[string]readsAnswer{}}
}

// tie is the tie of c, Known false with the cause when it cannot be derived.
func (d *derivedTies) tie(c specgraph.Check) specgraph.Tie {
	return specgraph.GoTies([]specgraph.Check{c}, ".", nil, d.once)[c.ID]
}

// once is the script's answer for its arguments, asked the first time only.
func (d *derivedTies) once(workdir string, pkgs, env []string) ([]string, error) {
	key := strings.Join(append(append([]string{workdir}, pkgs...), env...), "\x00")
	answer, asked := d.answers[key]
	if !asked {
		answer.reads, answer.err = d.reads(workdir, pkgs, env)
		d.answers[key] = answer
	}
	return answer.reads, answer.err
}

// baseCheckout is the tie of each check at the base commit of --changed, derived as the ties at the
// head are, by the base's own .github/scripts/go-test-reads.sh, in a checkout of the base commit
// (specgraph.GitHistory.Checkout) that the first ask makes and close removes. A file that the
// change deleted or moved out of a tie is in no tie at the head, so a result carries from the base
// only while no file of the base's tie changed either (specgraph.Citations), which asks only for a
// check whose test did not run and whose tie at the head no changed path reaches: a run where each
// test ran makes no checkout, and citations makes none for a base that is the head, whose tie is
// the one at the head.
//
// The script lists the repository's files with git ls-files (ci_filter_coverage.py's repo_files,
// which ci_job_reads.py's tables read too), and the checkout is no git work tree. So the script
// runs with git's view of the base, the checkout's Environ: GIT_DIR the git directory of the
// clone, GIT_WORK_TREE the checkout's tree and GIT_INDEX_FILE the checkout's index of the base's
// tree, and no other variable GIT_* of the process. Its git ls-files then lists the base's files,
// and no file of the clone's own index or work tree (TestTheBaseTieReadsGitsListOfTheBase).
type baseCheckout struct {
	repo, commit string
	checkout     specgraph.Checkout // the checkout, once made
	err          error              // why the checkout could not be made
	ties         *derivedTies
}

// tie is the tie of c at the base commit, Known false with the cause when it cannot be derived.
func (b *baseCheckout) tie(c specgraph.Check) specgraph.Tie {
	if b.checkout.Dir == "" && b.err == nil {
		if b.checkout, b.err = (specgraph.GitHistory{Dir: b.repo}).Checkout(b.commit); b.err == nil {
			b.ties = newDerivedTies(b.checkout.Tree(), b.checkout.Environ)
		}
	}
	if b.err != nil {
		return specgraph.Tie{Why: b.err.Error()}
	}
	return b.ties.tie(c)
}

// close removes the checkout of the base commit.
func (b *baseCheckout) close() {
	if b.checkout.Dir != "" {
		b.checkout.Remove()
	}
}
