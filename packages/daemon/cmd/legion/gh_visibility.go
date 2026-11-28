package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
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

// ghVisibilityReadChild is the real gh that one of the guard's own two visibility reads of repo
// runs (ghVisibilityRead's REST and GraphQL argv), and the whole environment it runs with: what
// `legion gh` does for such a read before it runs gh, written once for the two places that run
// that gh, so they never part. One is runGh's ghVisibilityCall mode, the read of a guard that
// runs `gh` from PATH, as the shared check's own read's guard does (answerVisibilityRead); the
// other is the guard of `legion gh` and of `legion leak-check`, which runs the read with the real
// gh itself (ghGuardRead), so no second `legion gh` starts for it. env is the read's environment
// by name and environ the same environment whole: for runGh this process's own, for ghGuardRead
// the one the guard would have started `gh` from PATH with (leakguard.Options.GhRead), which is
// the environment such a `legion gh` would have had. wrapper is the wrapper first on PATH, and
// LEGION_GH_PATH the real gh, each refused already where it is no such program (ghRunnable).
//
// Its token is that of the repository, routed by the one rule (ghConfigDirFor): a configured
// rename serves the repository from the `to` owner's directory, and the `from` owner's token
// cannot read it. The GraphQL read names its repository only in variables, so runGh reads its
// owner and name from the variable PUBLIC_LEAK_CHECK_VISIBILITY_READ, which ghVisibilityRead
// matched to the read's exact argv, and ghGuardRead from the repository the guard reads. Its argv
// names the host, `--hostname github.com`, so no GH_HOST sends it elsewhere.
//
// Under the caller's GH_TOKEN or GITHUB_TOKEN it keeps GH_CONFIG_DIR and is routed by nothing,
// because gh sends that token in place of any directory's: the bundle may hold no directory for
// the read's repository at all, and this routing would then refuse a read the caller's token can
// make, which is the second reason #4219 gives for running these reads under it
// (ghVisibilityRead).
//
// With no caller token, a read of a repository whose owner the bundle holds no directory for is
// refused with leakguard.OwnerNotServedError (ownerGHConfigDir), alone, and no gh runs: runGh
// prints it as its line, and ghGuardRead answers it to the guard. The guard reads either exactly
// so (leakguard.Guard.Visibility), and then reads the repository's visibility with no credential.
// A configured rename whose `to` owner the bundle holds no directory for is refused with that
// error inside a longer one, which the guard reads as the setup failure it is.
//
// The directory is judged by ghConfigShape before gh runs, and gh runs with GH_PATH the wrapper,
// leakguard.RunByRealGhVar the real gh, and no pager program (runGh says why), from the
// allowlist of environ (ghChildEnviron, withGitHubUseHTTPPath).
func ghVisibilityReadChild(env map[string]string, environ []string, wrapper string, repo ghrepo.Repository) (leakguard.GhReadChild, error) {
	dir := env["GH_CONFIG_DIR"]
	if ghCallerToken(env) == "" {
		var err error
		if dir, err = ghConfigDirFor(env["LEGION_GH_OWNERS_DIR"], strings.ToLower(repo.Owner()), repo.Name()); err != nil {
			return leakguard.GhReadChild{}, err
		}
	}
	if err := ghConfigShape(dir); err != nil {
		return leakguard.GhReadChild{}, err
	}
	gh := env["LEGION_GH_PATH"]
	childEnv, err := withGitHubUseHTTPPath(ghChildEnviron(environ, map[string]string{
		"GH_CONFIG_DIR": dir, "GH_PATH": wrapper, leakguard.RunByRealGhVar: gh, "GH_PAGER": "cat",
	}))
	if err != nil {
		return leakguard.GhReadChild{}, err
	}
	return leakguard.GhReadChild{Program: gh, Env: childEnv}, nil
}

// ghGuardRead is leakguard.Options.GhRead for the guard `legion gh` runs over a call and the one
// `legion leak-check` runs over a write, in a process whose environment, env, names
// LEGION_GH_PATH, and nil where it names none: the guard then runs `gh` from PATH, and a `legion
// gh` that read reaches refuses it as it refuses every call with no real gh named. For the read
// of slug it answers what a `legion gh` started for that read would have run:
// ghVisibilityReadChild of the environment the guard hands it, after the refusals runGh makes
// before it reads anything of a call (ghUnsetRefusal, ghRunnable). It checks no
// leakguard.RunByRealGhVar: the guard blanks it in the environment it hands, as for every child
// of its own, and the real gh is given it again. So the guard's read runs one gh process, the
// read itself, where it ran a `legion gh` first, whose start cost a loaded pod about 0.6 s before
// its real gh started (dispatch://INFRA-3562/artifact/measurement-md@v3, section 2).
func ghGuardRead(env map[string]string) func(slug string, environ []string) (leakguard.GhReadChild, error) {
	if env["LEGION_GH_PATH"] == "" {
		return nil
	}
	return func(slug string, environ []string) (leakguard.GhReadChild, error) {
		repo, err := ghrepo.Parse(leakguard.VisibilityReadVar, slug)
		if err != nil {
			return leakguard.GhReadChild{}, fmt.Errorf("the guard's visibility read names no repository GitHub could serve: %w", err)
		}
		readEnv := environmentOf(environ)
		if err := ghUnsetRefusal(readEnv); err != nil {
			return leakguard.GhReadChild{}, err
		}
		_, wrapper, err := ghRunnable(readEnv)
		if err != nil {
			return leakguard.GhReadChild{}, err
		}
		return ghVisibilityReadChild(readEnv, environ, wrapper, repo)
	}
}

// answerVisibilityRead answers the shared public leak check's own visibility read of repo
// (sharedCheckVisibilityRead) by the guard's one rule, read fresh (Guard.FreshVisibility): the
// REST read, and where GitHub refuses it for a rate limit, GraphQL with the same token or git's
// ls-remote with no credential, which can answer only public, and where this command refuses the
// REST read because the bundle holds no directory for the repository's owner, that same
// ls-remote. Its guard names no GhRead and runs `gh` from PATH, so the guard's reads come back
// through this command (ghVisibilityRead), which runs each with the repository's token, or with
// the caller's GH_TOKEN or GITHUB_TOKEN where one is set (ghVisibilityReadChild), in a second
// `legion gh`. The real gh's read, which the check made before, answers nothing while the
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
	visibility, err := leakguard.New(leakguard.Options{Curl: leakguardCurl, RoleState: ghRoleStateBase}).FreshVisibility(bounded, repo.String())
	if status, ok := signalled(ctx); ok {
		return status
	}
	if err != nil {
		printGhRefusal(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, visibility)
	return 0
}
