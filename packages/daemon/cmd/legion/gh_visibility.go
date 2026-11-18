package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// visibilityAnswerBound is the longest `legion gh` takes to answer the shared check's own
// visibility read (answerVisibilityRead), from the moment runGh starts. The installed check gives
// each of its two tries of that read 20 s (public_leak_check.py's vis_of), so the answer, or its
// refusal, comes before the check stops waiting, and no read made for it outlives the check's
// wait. The guard waits for a near rate-limit reset only where its caller's bound has room for
// the wait and the reads after it, and this bound has room for none.
const visibilityAnswerBound = 15 * time.Second

// visibilityClock is the clock answerVisibilityRead's bound runs on, and the guard's clock for
// that answer's reads: clock.Real(), which a test replaces with a clocktest.Fake.
var visibilityClock clock.Clock = clock.Real()

// sharedCheckVisibilityRead is the repository the shared public leak check's own visibility read
// names, and whether argv is that read: exactly leakguard.VisibilityReadArgs of the repository
// PUBLIC_LEAK_CHECK_VISIBILITY_READ names, a name GitHub could serve. runGh decides it before it
// reads anything else of the call, so visibilityAnswerBound covers the whole answer.
func sharedCheckVisibilityRead(argv []string, env map[string]string) (ghrepo.Repository, bool) {
	repo, ok := visibilityReadRepository(env)
	if !ok || !slices.Equal(argv, leakguard.VisibilityReadArgs(env[leakguard.VisibilityReadVar])) {
		return ghrepo.Repository{}, false
	}
	return repo, true
}

// visibilityReadRepository is the repository PUBLIC_LEAK_CHECK_VISIBILITY_READ names, and whether
// it names one GitHub could serve.
func visibilityReadRepository(env map[string]string) (ghrepo.Repository, bool) {
	slug := env[leakguard.VisibilityReadVar]
	if slug == "" {
		return ghrepo.Repository{}, false
	}
	repo, err := ghrepo.Parse(leakguard.VisibilityReadVar, slug)
	return repo, err == nil
}

// answerVisibilityRead answers the shared public leak check's own visibility read of repo
// (sharedCheckVisibilityRead) by the guard's one rule, read fresh (Guard.FreshVisibility): the
// REST read, and where GitHub refuses it for a rate limit, GraphQL with the same token or git's
// ls-remote with no credential, which can answer only public. The guard's reads come back
// through this command, which runs each with the repository's token, or with the caller's
// GH_TOKEN or GITHUB_TOKEN where one is set (ghVisibilityRead). The real gh's read, which the
// check made before, answers nothing while the installation's REST limit is spent, and the check
// then refuses the push. So this is how a theorem-labs push gets through in a spent hour: the
// check reads the visibility of every theorem-labs repository itself, before the guard's own
// read runs.
//
// The read takes no kept answer and keeps none. The check keeps answers of its own, and reads each
// other remote of a push fresh: a remote it reads public widens the commits it counts as held,
// which it does not scan. For that read a false public is the leak, so a public the guard kept
// for a repository made private since must not answer it.
//
// It prints what `gh api ... --jq .visibility` prints, the word public, private or internal and a
// newline, and exits 0. Where no read answers, it prints nothing on standard output, says why on
// standard error (and, where visibilityAnswerBound ended the reads, names that bound in a line of
// its own), and exits 1, which the check reads as an unknown visibility and refuses; the
// check prints its own refusal. A signal that reached this command before or during the reads
// ends it with 128+<signal>, as it ends a call gh was never started for (runGhChild,
// endedBySignal).
func answerVisibilityRead(ctx context.Context, repo ghrepo.Repository, stdout, stderr io.Writer) int {
	if status, ok := signalled(ctx); ok {
		return status
	}
	bounded, cancel := visibilityClock.WithTimeout(ctx, visibilityAnswerBound)
	defer cancel()
	visibility, err := leakguard.New(leakguard.Options{Clock: visibilityClock}).FreshVisibility(bounded, repo.String())
	if status, ok := signalled(ctx); ok {
		return status
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		// The bound says so itself when it is what ended the reads. The guard's own sentence for
		// a read it stopped names the bound it reads from its context's Deadline, and a test's
		// fake clock gives a deadline context that reports its parent's deadline and never its
		// own: there the guard cannot name this bound, so this line names it on every clock.
		if bounded.Err() != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "legion gh: no read answered the visibility of %s within %s, the bound that keeps this answer inside the shared check's own wait\n",
				repo, visibilityAnswerBound)
		}
		return 1
	}
	fmt.Fprintln(stdout, visibility)
	return 0
}

// signalled is the status a signal that cancelled ctx ends this command with (endedBySignal), and
// whether one did.
func signalled(ctx context.Context) (int, bool) {
	if cause := context.Cause(ctx); cause != nil {
		return endedBySignal(cause)
	}
	return 0, false
}
