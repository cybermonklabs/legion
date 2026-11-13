package rescue

// What this package's own tests reach into, and why it is here rather than in the fixture.
//
// No test of this package may put a stand-in binary on PATH — the fixture runs the real git
// and jj of the machine, which is what makes its answers worth anything — so the only place a
// test can read the commands this check runs, with the arguments it runs them with, is the
// check itself, through the observer it calls before each one. And the one place a test can
// read the tree the check would commit for a worktree jj holds, without the commit and the push
// that follow it, is the code that builds it.

import (
	"context"
	"testing"
)

// ObserveCommands has observe called with the directory, the program and the arguments of every
// git and jj this package starts, for the rest of t.
func ObserveCommands(t testing.TB, observe func(dir, name string, args []string)) {
	t.Helper()
	was := observeCommand
	observeCommand = observe
	t.Cleanup(func() { observeCommand = was })
}

// TreeTheJJRouteBuilds is the tree the check builds for the files of the worktree at dir, which
// jj holds, by the route it takes there in place of the whole-tree git read: jj's own snapshot
// of the worktree, with what that read would have kept and the snapshot's tree lacks carried in
// from a copy of the worktree's index and git's own read of the files that index does not track
// (edits, snapshotTree, carried). It is that code itself, run as a check would run it, with ctx
// as its bound, env as the role's environment and head as the worktree's git HEAD, "" where it is
// unborn, as the check reads it. It answers the tree's id, or the error that would fail the
// workspace, and makes no commit and pushes nothing: a test compares the tree with the one the
// git read builds over the same worktree. What it writes is what the route writes, the snapshot
// of the worktree's working copy among it (rescue's package doc). dir must hold a .jj directory;
// the other read is no part of what this answers.
func TreeTheJJRouteBuilds(ctx context.Context, env []string, dir, head string) (string, error) {
	return runner{ctx: ctx, env: env}.snapshotTree(dir, head)
}
