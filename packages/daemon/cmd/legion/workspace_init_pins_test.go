package main

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/repocache"
)

// gitConfigCall is one git invocation fakeGit recorded to WINIT_GIT_CONFIG_LOG: who started it
// ("jj" or "workspace-init"), the GIT_CONFIG_COUNT it was handed ("unset" when it had none), its
// GIT_CONFIG_KEY_<i>=GIT_CONFIG_VALUE_<i> pairs in order, and its argv.
type gitConfigCall struct {
	starter, count string
	pairs          []string
	argv           string
}

// gitConfigCalls is every git invocation fakeGit recorded to log, in order; nil when no git ran
// with WINIT_GIT_CONFIG_LOG set.
func gitConfigCalls(t *testing.T, log string) []gitConfigCall {
	t.Helper()
	var calls []gitConfigCall
	for _, line := range logLines(t, log) {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			t.Fatalf("fakeGit's line %q holds no starter, GIT_CONFIG_COUNT and argv", line)
		}
		call := gitConfigCall{starter: fields[0], count: fields[1], argv: fields[len(fields)-1]}
		if count, err := strconv.Atoi(call.count); err == nil && count >= 0 && count <= len(fields)-3 {
			call.pairs = fields[2 : 2+count]
			call.argv = strings.Join(fields[2+count:], "\t")
		}
		calls = append(calls, call)
	}
	return calls
}

// Every git a cache-backed provision starts, itself or through jj, reads the runner's pins last,
// after every other GIT_CONFIG_* pair it was handed (R4: workspace-init "keeps them for each git
// and jj that it adds"): no hook runs and no git starts its automatic maintenance. fakeGit records
// each invocation's pairs; jj starts the git the runner resolved, which is fakeGit, so the gits jj
// starts are recorded too. A pin dropped from pinnedGitConfig, or a git started past the runner,
// fails here.
func TestWorkspaceInitPinsEveryGitItStarts(t *testing.T) {
	t.Parallel()
	v := newIssueVolume(t).withRemote(t, "main")
	cache, _ := v.narrowedCacheTarServer(t)
	v.commitAddingAFile(t, v.env["WINIT_REMOTE"], "refs/heads/main", "refs/heads/legion/LEGION-42", "issue.txt", "issue\n")
	log := filepath.Join(t.TempDir(), "git-config.log")
	v.env["WINIT_GIT_CONFIG_LOG"] = log
	v.provisionEnv()
	v.env[cacheURLEnv] = cache
	v.env[repocache.CacheTokenFileEnv] = writeCacheToken(t, cmdCacheToken)

	code, stdout, stderr := v.run(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d:\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	calls := gitConfigCalls(t, log)
	// The clone's git init, its history check, the GitHub delta's ls-remote and fetch, and the
	// clone's configuration each start a git of their own.
	const fewest = 5
	if len(calls) < fewest {
		t.Fatalf("provision started %d gits, want at least %d (the clone's init, its history check, the GitHub delta, its configuration):\n%+v", len(calls), fewest, calls)
	}
	if !slices.ContainsFunc(calls, func(call gitConfigCall) bool { return call.starter == "jj" }) {
		t.Fatalf("no git jj started reached the volume's git, so jj's gits went unchecked:\n%+v", calls)
	}
	pins := []string{"core.hooksPath=/dev/null", "gc.auto=0", "maintenance.auto=false"}
	// The GitHub delta's ls-remote and fetch carry the one-shot credential helper's pairs, which
	// the pins must follow.
	if !slices.ContainsFunc(calls, func(call gitConfigCall) bool { return len(call.pairs) > len(pins) }) {
		t.Fatalf("no git carried a GIT_CONFIG_* pair of its own for the pins to follow:\n%+v", calls)
	}
	for _, call := range calls {
		if len(call.pairs) < len(pins) || !slices.Equal(call.pairs[len(call.pairs)-len(pins):], pins) {
			t.Errorf("git %s, started by %s, ran with GIT_CONFIG_COUNT=%s and pairs %q, want them to end with %q", call.argv, call.starter, call.count, call.pairs, pins)
		}
	}
}
