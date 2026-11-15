package workerbin

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// standInImageGit points imageGit at a stand-in for the image's git wrapper, a script in a new
// directory, until the test ends, and answers that directory.
func standInImageGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := filepath.Join(dir, "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\necho image-git\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := imageGit
	imageGit = git
	t.Cleanup(func() { imageGit = previous })
	return dir
}

// otherGitDir is a new directory that holds an executable git that is not the image's.
func otherGitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\necho another-git\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// entryNames is the names of what dir holds, in order.
func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// Oh My Pi's bash tool sources the operator's rc file and snapshots the PATH it leaves, so an rc
// that prepends its own directories (this devbox's dotfiles shims, with their own legion and gh)
// lands them ahead of the launcher directory. The prefix Oh My Pi runs before every command,
// `${PI_SHELL_PREFIX} <command>` in its persistent shell, puts the installed directory back in
// front, so legion and gh both resolve to its scripts, and running it again leaves PATH as it was.
func TestShellPrefixResolvesLegionsLauncherAheadOfAnRcsDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state dir")
	if err := Install(root, "/opt/legion", "/usr/bin:/bin"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	rc := t.TempDir()
	for _, name := range []string{"gh", "legion"} {
		if err := os.WriteFile(filepath.Join(rc, name), []byte("#!/bin/sh\nexit 97\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := strings.Join([]string{rc, LauncherDir(root), "/usr/bin", "/bin"}, ":")
	prefix := shellprefix.For(LauncherDir(root))
	script := "PATH=" + shellprefix.Literal(snapshot) + "\n" +
		prefix + " command -v legion\n" +
		prefix + " command -v gh\n" +
		"before=$PATH\n" +
		prefix + ` test "$PATH" = "$before" && echo stable`
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v: %s", err, out)
	}
	want := filepath.Join(LauncherDir(root), "legion") + "\n" + filepath.Join(LauncherDir(root), "gh") + "\nstable\n"
	if string(out) != want {
		t.Fatalf("resolved\n%s\nwant\n%s", out, want)
	}
}

// The worker image's gh (packages/daemon/docker/gh, COPY'd to /opt/legion/bin/gh) is the script
// Install writes for the image's legion, so a pod's gh and a pane's are one wrapper.
func TestTheImagesGhIsTheWrapperInstallWrites(t *testing.T) {
	root := t.TempDir()
	if err := Install(root, "/opt/legion/bin/legion", "/usr/bin:/bin"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(LauncherDir(root), "gh"))
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(filepath.Join("..", "..", "..", "docker", "gh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(image) != string(want) {
		t.Fatalf("packages/daemon/docker/gh = %q, want the wrapper Install writes for /opt/legion/bin/legion, %q", image, want)
	}
}

// Where the image's git leads path, Install puts two private 0700 scripts in a 0700 bin under root
// and nothing else: the launcher, which execs the installing legion, and gh, which execs its
// `legion gh` (the wrapper that picks the owner's gh directory).
func TestInstallInstallsThePrivateLauncherAndTheGhWrapper(t *testing.T) {
	image := standInImageGit(t)
	root := t.TempDir()
	if err := Install(root, "/opt/legion", image); err != nil {
		t.Fatalf("Install: %v", err)
	}
	bin := LauncherDir(root)
	if info, err := os.Stat(bin); err != nil {
		t.Fatalf("bin: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("bin mode = %o, want 0700", info.Mode().Perm())
	}
	for name, want := range map[string]string{
		"legion": "#!/bin/sh\nexec '/opt/legion' \"$@\"\n",
		"gh":     "#!/bin/sh\nexec '/opt/legion' gh \"$@\"\n",
	} {
		script := filepath.Join(bin, name)
		contents, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info, err := os.Stat(script); err != nil {
			t.Fatalf("%s stat: %v", name, err)
		} else if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %o, want 0700", name, info.Mode().Perm())
		}
		if string(contents) != want {
			t.Fatalf("%s = %q, want %q", name, contents, want)
		}
	}
	for dir, want := range map[string][]string{root: {"bin"}, bin: {"gh", "legion"}} {
		if names := entryNames(t, dir); !slices.Equal(names, want) {
			t.Fatalf("%s holds %q after Install, want %q", dir, names, want)
		}
	}
	if err := Install(root, "legion", image); err == nil || !strings.Contains(err.Error(), "not an absolute path") {
		t.Fatalf("Install with a relative executable = %v, want a refusal", err)
	}
}

// Where the first git on path is not the image's, the image's git later on path included, and
// where path holds no git at all, Install writes bin's git too: a 0700 script that execs the
// installing legion's `legion git --after` with bin's own absolute path, both single-quoted. bin
// then holds gh, git and legion.
func TestInstallWritesTheGitWrapperWhereTheImagesGitDoesNotLeadPath(t *testing.T) {
	image := standInImageGit(t)
	for name, path := range map[string]string{
		"another git ahead of the image's": otherGitDir(t) + ":" + image,
		"no git on path":                   t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state dir")
			if err := Install(root, "/opt/legion state/legion", path); err != nil {
				t.Fatalf("Install: %v", err)
			}
			bin := LauncherDir(root)
			script := filepath.Join(bin, "git")
			contents, err := os.ReadFile(script)
			if err != nil {
				t.Fatalf("git: %v", err)
			}
			if want := "#!/bin/sh\nexec '/opt/legion state/legion' git --after '" + bin + "' -- \"$@\"\n"; string(contents) != want {
				t.Fatalf("git = %q, want %q", contents, want)
			}
			if info, err := os.Stat(script); err != nil {
				t.Fatalf("git stat: %v", err)
			} else if info.Mode().Perm() != 0o700 {
				t.Fatalf("git mode = %o, want 0700", info.Mode().Perm())
			}
			if names := entryNames(t, bin); !slices.Equal(names, []string{"gh", "git", "legion"}) {
				t.Fatalf("bin holds %q after Install, want gh, git and legion", names)
			}
		})
	}
}

// Where the image's git is the first git on path, Install writes no git into bin and removes the
// one an earlier run wrote. So the git an agent's shell finds under Path is the image's, which is
// what the image's jj wrapper reads before a push.
func TestInstallWritesNoGitWrapperWhereTheImagesGitLeadsPath(t *testing.T) {
	image := standInImageGit(t)
	root := t.TempDir()
	stale := filepath.Join(LauncherDir(root), "git")
	if err := Install(root, "/opt/legion", otherGitDir(t)+":"+image); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the first Install wrote no git: %v", err)
	}
	path := image + ":" + otherGitDir(t)
	if err := Install(root, "/opt/legion", path); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("bin/git after an Install whose path the image's git leads: %v, want it removed", err)
	}
	if names := entryNames(t, LauncherDir(root)); !slices.Equal(names, []string{"gh", "legion"}) {
		t.Fatalf("bin holds %q after Install, want gh and legion", names)
	}
	shell := exec.Command("sh", "-c", "command -v git")
	shell.Env = []string{"PATH=" + Path(path, root)}
	out, err := shell.Output()
	if err != nil {
		t.Fatalf("command -v git: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), filepath.Join(image, "git"); got != want {
		t.Fatalf("command -v git under Path = %q, want the image's git %q", got, want)
	}
}

// The git Install reads on path leaves out bin itself, written as it is, with a trailing slash,
// or through a link: a daemon started from an agent's shell inherits a PATH that bin leads, and
// bin's own git there must not decide whether bin holds one.
func TestInstallGitWrapperSkipsItsOwnDirectory(t *testing.T) {
	image := standInImageGit(t)
	root := t.TempDir()
	bin := LauncherDir(root)
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "bin link")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	for _, own := range []string{bin, bin + "/", link} {
		if err := Install(root, "/opt/legion", otherGitDir(t)); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if _, err := os.Stat(filepath.Join(bin, "git")); err != nil {
			t.Fatalf("the first Install wrote no git: %v", err)
		}
		if err := Install(root, "/opt/legion", own+":"+image); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if _, err := os.Stat(filepath.Join(bin, "git")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("with %q first on path, bin/git after Install: %v, want it removed, since the git after bin is the image's", own, err)
		}
	}
}

// Path leads with root's bin exactly once, whatever PATH the daemon inherited: a daemon started
// from an agent's shell carries its predecessor's bin, and every other entry stays verbatim.
func TestPathLeadsWithTheLauncherDirectoryExactlyOnce(t *testing.T) {
	root := "/var/lib/legion"
	bin := LauncherDir(root)
	for path, want := range map[string]string{
		"/usr/local/bin:/usr/bin":                    bin + ":/usr/local/bin:/usr/bin",
		bin + ":/usr/local/bin:" + bin + ":/usr/bin": bin + ":/usr/local/bin:/usr/bin",
		"":               bin,
		"/usr/bin::/bin": bin + ":/usr/bin:/bin",
	} {
		if got := Path(path, root); got != want {
			t.Errorf("Path(%q) = %q, want %q", path, got, want)
		}
	}
}
