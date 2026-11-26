package workspace

import (
	"context"
	"errors"
	"slices"
)

// CacheGitStdin runs git on bare repository gitDir as CacheGit does, writing stdin to its standard
// input. It is for git commands such as update-ref whose one operation takes a batch of refs.
func CacheGitStdin(ctx context.Context, run Runner, gitDir, stdin string, args ...string) (Result, error) {
	if run == nil {
		return Result{}, errors.New("workspace runner is required")
	}
	argv := slices.Concat(feedGit, []string{"--git-dir=" + gitDir}, args)
	result, err := run.Run(ctx, Command{Argv: argv, Env: isolatedGitConfig, Dir: gitDir, Stdin: stdin, Timeout: run.Timeout()})
	return checkedResult(argv, result, err)
}
