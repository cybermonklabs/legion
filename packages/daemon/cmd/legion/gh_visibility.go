package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
)

// visibilityAnswerBound is the longest `legion gh` takes to answer the shared check's own
// visibility read (answerVisibilityRead). The installed check gives each of its two tries of that
// read 20 s (public_leak_check.py's vis_of), so the answer, or its refusal, comes before the check
// stops waiting, and no read made for it outlives the check's wait. It also leaves no room for the
// guard's wait for a near reset (Guard.Visibility), so the answer goes on to GraphQL instead.
const visibilityAnswerBound = 15 * time.Second

// answerVisibilityRead answers the shared public leak check's own visibility read of repo, exactly
// leakguard.VisibilityReadArgs with PUBLIC_LEAK_CHECK_VISIBILITY_READ naming repo, by the guard's
// one rule (Guard.Visibility): the REST read, and where GitHub refuses it for a spent rate limit,
// GraphQL with the repository's own token and then git's ls-remote with no credential, which can
// answer only public. The guard's reads come back through this command, which runs each with the
// repository's token (ghVisibilityRead). The real gh's read, which the check made before, answers
// nothing while the installation's REST limit is spent, and the check then refuses the push. So
// this is how a theorem-labs push gets through in a spent hour: the check reads the visibility of
// every theorem-labs repository itself, before the guard's own read runs.
//
// It prints what `gh api ... --jq .visibility` prints, the word public, private or internal and a
// newline, and exits 0. Where no read answers, it prints nothing on standard output, says why on
// standard error, and exits 1, which the check reads as an unknown visibility and refuses; the
// check prints its own refusal. As for the guard, no unauthenticated answer is private or internal.
// A signal that reached this command before or during the reads ends it with 128+<signal>, as it
// ends a call gh was never started for (runGhChild, endedBySignal).
func answerVisibilityRead(ctx context.Context, repo ghrepo.Repository, stdout, stderr io.Writer) int {
	if status, ok := signalled(ctx); ok {
		return status
	}
	bounded, cancel := context.WithTimeout(ctx, visibilityAnswerBound)
	defer cancel()
	visibility, err := leakguard.New(leakguard.Options{}).Visibility(bounded, repo.String())
	if status, ok := signalled(ctx); ok {
		return status
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
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
