package workspace

import (
	"context"
	"errors"
	"slices"
)

// CacheGitHolding is CacheGit for a command on what the answering party of the cache's fetch
// wrote — a ref that fetch left in the clone — whose argv or stderr can name such a ref, which
// can echo a token the cache holds: its error names the command through the fetch's filter
// (fetchCredential.words) and prints git's stderr through that filter read once more by the
// fail-closed rule (fetchCredential.shown). held is every credential the caller holds: the
// repository cache's provisioning token, as its file stands now.
func CacheGitHolding(ctx context.Context, run Runner, gitDir string, held []string, args ...string) (Result, error) {
	if run == nil {
		return Result{}, errors.New("workspace runner is required")
	}
	return cacheGitWithin(ctx, run, gitDir, run.Timeout(), &fetchCredential{held: held}, args...)
}

// CacheGitStdinHolding runs git on bare repository gitDir as CacheGitHolding does, writing stdin
// to its standard input. It is for a git command such as update-ref whose one operation takes a
// batch of refs, which the cache's fetch wrote, on its standard input.
func CacheGitStdinHolding(ctx context.Context, run Runner, gitDir, stdin string, held []string, args ...string) (Result, error) {
	if run == nil {
		return Result{}, errors.New("workspace runner is required")
	}
	credential := fetchCredential{held: held}
	argv := slices.Concat(isolatedGit, []string{"--git-dir=" + gitDir}, args)
	result, err := run.Run(ctx, Command{Argv: argv, Env: isolatedGitConfig, Dir: gitDir, Stdin: stdin, Timeout: run.Timeout()})
	argv, result.Stderr = credential.words(argv), credential.shown(result.Stderr)
	return checkedResult(argv, result, err)
}
