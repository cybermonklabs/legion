package fixtureenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A pod's PATH leads with /opt/legion/bin, whose git runs Legion's leak guard on every push and
// whose jj refuses a push whose git is any other, so a test's push to a local bare repository
// under its own empty home is refused in a pod and passes on CI. Guard takes every directory whose
// git is the image's git wrapper off the process's PATH, and Live gives that PATH back to a live
// proof. A directory holding a link to the wrapper goes too; a test's own copy of the wrapper,
// which is no link, stays, and so does every other entry, in order, behind the directory of the
// real git Guard puts first (realTools), which the copies here run.
func TestGuardTakesLegionsWrapperDirectoriesOffPath(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test runs a real git: %v", err)
	}
	if git, err = filepath.EvalSymlinks(git); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dirs := map[string]string{}
	for _, name := range []string{"image", "linked", "copied", "tools"} {
		dirs[name] = filepath.Join(root, name)
		if err := os.Mkdir(dirs[name], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"image": "#!/bin/sh\n", "copied": "#!/bin/sh\nexec '" + git + "' \"$@\"\n", "tools": "#!/bin/sh\nexec '" + git + "' \"$@\"\n"} {
		if err := os.WriteFile(filepath.Join(dirs[name], "git"), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dirs["image"], "git"), filepath.Join(dirs["linked"], "git")); err != nil {
		t.Fatal(err)
	}
	keep, before, wrapper := guardedKeep, unguarded, imageGit
	t.Cleanup(func() { guardedKeep, unguarded, imageGit = keep, before, wrapper })
	imageGit = filepath.Join(dirs["image"], "git")
	// The directory of the readlink and sh Guard's read of the real git runs, which a PATH of these
	// directories alone would lack.
	readlink, err := exec.LookPath("readlink")
	if err != nil {
		t.Fatal(err)
	}
	system := filepath.Dir(readlink)
	pod := strings.Join([]string{dirs["image"], dirs["linked"], dirs["copied"], dirs["tools"], system}, string(filepath.ListSeparator))
	want := strings.Join([]string{dirs["copied"], dirs["tools"], system}, string(filepath.ListSeparator))
	t.Setenv("PATH", pod)

	// A process Guard has not run in yet, as a TestMain's is.
	guardedKeep, unguarded = nil, nil
	if err := Guard(); err != nil {
		t.Fatal(err)
	}
	entries := filepath.SplitList(os.Getenv("PATH"))
	if len(entries) == 0 || strings.Join(entries[1:], string(filepath.ListSeparator)) != want {
		t.Fatalf("PATH=%s after Guard, want the real git's directory and then %s", os.Getenv("PATH"), want)
	}
	if linked, err := os.Readlink(filepath.Join(entries[0], "git")); err != nil || linked != git {
		t.Errorf("the first entry %s links git to %q (%v), want %s, the git the copies run", entries[0], linked, err, git)
	}
	t.Run("a live proof", func(t *testing.T) {
		Live(t)
		if got := os.Getenv("PATH"); got != pod {
			t.Errorf("PATH=%s in the live proof, want the process's own %s", got, pod)
		}
	})
	RequireGuarded(t)
}
