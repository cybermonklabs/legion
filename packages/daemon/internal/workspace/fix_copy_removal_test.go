package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fix of an issue's task works a copy of its own beside the issue's workspace (FixDir), which
// the shim's adoption adds as a workspace of the same clone and which nothing but this removal
// takes away. So the removal of an issue's workspace takes that copy with it: its directory, the
// commits no one else holds, its registration in the clone and its git worktree entry. Left
// behind, the copy is a dead fix's work standing at the one name the next fix of the issue is
// given, and the next provisioning would find a workspace of an issue that has none.
//
// Here the copy holds a commit of its own that no branch holds, as a fix that never pushed leaves
// one, and the row reads the clone afterwards: no directory of either, no registered workspace of
// the copy's name, and no git worktree of the clone's.
func TestRemoveTakesTheFixsCopyWithTheWorkspace(t *testing.T) {
	run := newLocalRunner(t)
	workspace, err := Provision(context.Background(), run, provisionRequest(t))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	fix := FixDir(workspace.Dir)
	runSetup(t, workspace.Clone, "jj", "workspace", "add", fix, "--name", filepath.Base(fix), "-R", workspace.Clone)
	if err := os.WriteFile(filepath.Join(fix, "fix.txt"), []byte("fix: the failure\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t, fix, "jj", "commit", "-m", "fix: the failure", "-R", fix)
	if _, err := os.Stat(filepath.Join(fix, ".jj")); err != nil {
		t.Fatalf("the fix's copy was not made: %v", err)
	}

	if err := Remove(context.Background(), run, workspace, nil); err != nil {
		t.Fatalf("remove: %v", err)
	}

	for _, dir := range []string{workspace.Dir, removingPath(workspace.Dir), fix, removingPath(fix)} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is there after the removal (%v), want it gone with the workspace", dir, err)
		}
	}
	listed := runSetup(t, workspace.Clone, "jj", "workspace", "list", "-T", `name ++ "\n"`, "-R", workspace.Clone)
	for _, name := range []string{filepath.Base(workspace.Dir), filepath.Base(fix)} {
		if strings.Contains(listed, name) {
			t.Errorf("the clone still registers the workspace %s:\n%s", name, listed)
		}
	}
	if locks := gitWorktreeLocks(t, workspace.Clone); len(locks) != 0 {
		t.Errorf("the removal left git worktrees registered: %v", locks)
	}
}
