package fixtureenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pod's PATH leads with /opt/legion/bin, whose git runs Legion's leak guard on every push and
// whose jj refuses a push whose git is any other, so a test's push to a local bare repository
// under its own empty home is refused in a pod and passes on CI. Isolated takes every directory
// whose git is the image's git wrapper off the PATH it hands a command, Guard off the process's
// own PATH, and Live gives the process's PATH back to a live proof. A directory holding a link to
// the wrapper goes too; a test's own copy of the wrapper, which is no link, stays, and so does
// every other entry, in order.
func TestGuardAndIsolatedTakeLegionsWrapperDirectoriesOffPath(t *testing.T) {
	root := t.TempDir()
	dirs := map[string]string{}
	for _, name := range []string{"image", "linked", "copied", "tools"} {
		dirs[name] = filepath.Join(root, name)
		if err := os.Mkdir(dirs[name], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"image", "copied", "tools"} {
		if err := os.WriteFile(filepath.Join(dirs[name], "git"), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dirs["image"], "git"), filepath.Join(dirs["linked"], "git")); err != nil {
		t.Fatal(err)
	}
	keep, before, wrapper := guardedKeep, unguarded, imageGit
	t.Cleanup(func() { guardedKeep, unguarded, imageGit = keep, before, wrapper })
	imageGit = filepath.Join(dirs["image"], "git")
	pod := strings.Join([]string{dirs["image"], dirs["linked"], dirs["copied"], dirs["tools"]}, string(filepath.ListSeparator))
	want := strings.Join([]string{dirs["copied"], dirs["tools"]}, string(filepath.ListSeparator))
	t.Setenv("PATH", pod)

	var isolated []string
	for _, entry := range Isolated(t.TempDir()) {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			isolated = append(isolated, value)
		}
	}
	if len(isolated) != 1 || isolated[0] != want {
		t.Errorf("Isolated holds PATH %q, want one, %s", isolated, want)
	}

	// A process Guard has not run in yet, as a TestMain's is.
	guardedKeep, unguarded = nil, nil
	if err := Guard(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PATH"); got != want {
		t.Errorf("PATH=%s after Guard, want %s", got, want)
	}
	t.Run("a live proof", func(t *testing.T) {
		Live(t)
		if got := os.Getenv("PATH"); got != pod {
			t.Errorf("PATH=%s in the live proof, want the process's own %s", got, pod)
		}
	})
	RequireGuarded(t)
}
