package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/programbound"
)

// visibilityAnswerBound is the longest `legion gh` takes to answer the shared check's own
// visibility read (answerVisibilityRead), from the moment runGh starts. The installed check gives
// each of its two tries of that read 20 s (public_leak_check.py's vis_of), so the answer, or its
// refusal, comes before the check stops waiting, and no read made for it outlives the check's
// wait. The guard waits for a near rate-limit reset only where its caller's bound has room for
// the wait and the reads after it, and this bound has room for none. It is a variable so that
// this package's tests can give it their own deadline (programbound).
var visibilityAnswerBound = 15 * time.Second

func init() {
	programbound.Register(&visibilityAnswerBound)
}

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
// ls-remote with no credential, which can answer only public, and where this command refuses the
// REST read because the bundle holds no directory for the repository's owner, that same
// ls-remote. The guard's reads come back through this command, which runs each with the
// repository's token, or with the caller's GH_TOKEN or GITHUB_TOKEN where one is set
// (ghVisibilityRead). The real gh's read, which the check made before, answers nothing while the
// installation's REST limit is spent, and the check then refuses the push. So this is how an
// example-org push gets through in a spent hour: the check reads the visibility of every
// example-org repository itself, before the guard's own read runs.
//
// The read takes no kept answer and keeps none. The check keeps answers of its own, and reads each
// other remote of a push fresh: a remote it reads public widens the commits it counts as held,
// which it does not scan. For that read a false public is the leak, so a public the guard kept
// for a repository made private since must not answer it.
//
// It prints what `gh api ... --jq .visibility` prints, the word public, private or internal and a
// newline, and exits 0. Where no read answers, it prints nothing on standard output, says why on
// standard error, and exits 1, which the check reads as an unknown visibility and refuses; the
// check prints its own refusal. A signal that reached this command before or during the reads
// ends it with 128+<signal>, as it ends a call gh was never started for (runGhChild,
// endedBySignal).
func answerVisibilityRead(ctx context.Context, repo ghrepo.Repository, stdout, stderr io.Writer) int {
	if status, ok := signalled(ctx); ok {
		return status
	}
	bounded, cancel := context.WithTimeout(ctx, programbound.Of(&visibilityAnswerBound))
	defer cancel()
	visibility, err := leakguard.New(leakguard.Options{Curl: leakguardCurl}).FreshVisibility(bounded, repo.String())
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
