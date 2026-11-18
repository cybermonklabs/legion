package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// loggingGh is a stand-in real gh that writes one line to log for every process `legion gh` runs
// of it, its arguments, and then answers as ghStandInPrefix does, and any other call with
// "gh ran".
func loggingGh(t *testing.T) (stub, log string) {
	t.Helper()
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stub, log = filepath.Join(dir, "gh"), filepath.Join(dir, "processes.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>'" + log + "'\n" +
		strings.TrimPrefix(ghStandInPrefix(help), "#!/bin/sh\n") + "echo 'gh ran'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub, log
}

// processes is every gh process the stand-in logged since the last call, in order, and empties
// the log for the next.
func processes(t *testing.T, log string) []string {
	t.Helper()
	logged := readRecord(t, log)
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if logged == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(logged, "\n"), "\n")
}

// helpKeepEnv is the environment of a role whose gh keeps its help: a state directory, a gh
// directory and a data directory of its own.
func helpKeepEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{"XDG_STATE_HOME": t.TempDir(), "GH_CONFIG_DIR": t.TempDir(), "XDG_DATA_HOME": t.TempDir()}
}

// readHelps reads gh's own help and gh api's through a keep made the way one `legion gh` call
// makes it for the gh at the path gh, and answers what the two reads printed.
func readHelps(t *testing.T, gh string, env map[string]string) []string {
	t.Helper()
	keep := newGhHelpKeep(gh, env, os.Environ())
	var helps []string
	for _, path := range [][]string{nil, {"api"}} {
		help, err := keep.read(path)
		if err != nil {
			t.Fatalf("read the help of %q: %v", path, err)
		}
		helps = append(helps, help)
	}
	return helps
}

// ghTargetOfKept is ghTargetOf of argv with gh's help as help answers it, and the row every test
// of the resolver runs through it: the same argv is resolved twice more through a ghHelpKeep that
// help backs, first by a keep that keeps each help it reads, then by a new keep of the same state,
// which reads from the kept copies and from help only for a path whose help help refused, which
// is never kept. Each must give the target, the refusal text and the files sent that help gives
// directly.
func ghTargetOfKept(t *testing.T, argv []string, env map[string]string, help func([]string) (string, error), remotes func() ([]ghRemote, error), dirFor func(owner, name string) (string, error)) (ghTarget, error) {
	t.Helper()
	want, wantErr := ghTargetOf(argv, env, help, remotes, dirFor)
	// The gh the keep is keyed on, which it never runs: help answers in its place.
	gh := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	keepEnv := map[string]string{"XDG_STATE_HOME": t.TempDir(), "GH_CONFIG_DIR": t.TempDir(), "XDG_DATA_HOME": t.TempDir()}
	refused := map[string]bool{}
	for _, call := range []string{"the call that keeps gh's help", "the call that reads the kept copies"} {
		keep := newGhHelpKeep(gh, keepEnv, nil)
		if keep.dir == "" {
			t.Fatalf("a keep for %s keeps nothing", gh)
		}
		keep.fresh = func(path []string) (string, error) {
			key := strings.Join(path, " ")
			if call == "the call that reads the kept copies" && !refused[key] {
				t.Errorf("%s of %q read gh's help of %q, which the call before it kept", call, argv, path)
			}
			text, err := help(path)
			if err != nil {
				refused[key] = true
			}
			return text, err
		}
		target, err := ghTargetOf(argv, env, keep.read, remotes, dirFor)
		if !reflect.DeepEqual(target, want) || errorText(err) != errorText(wantErr) {
			t.Errorf("%s decided ghTargetOf(%q) = %+v, %q; gh's own help decides %+v, %q", call, argv, target, errorText(err), want, errorText(wantErr))
		}
	}
	return want, wantErr
}

// errorText is err's message, "" for none.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The guard's own visibility reads run one gh process, the read itself, and read no gh help: the
// REST read and the GraphQL read, with the repository's token and with the caller's, and the
// shared check's own read, which `legion gh` answers through the guard's REST read. No state
// directory is given, so no kept help could stand in for a help read.
func TestLegionGhReadsNoHelpForTheGuardsVisibilityReads(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	stub, log := loggingGh(t)
	const slug = "sjawhar/legion"
	rest := strings.Join(leakguard.VisibilityReadWithHeadersArgs(slug), " ")
	graphql := strings.Join(leakguard.VisibilityGraphQLArgs(slug), " ")
	for _, tc := range []struct {
		name  string
		args  []string
		token string
		want  []string
	}{
		{"the guard's REST read", leakguard.VisibilityReadWithHeadersArgs(slug), "", []string{rest}},
		{"the guard's GraphQL read", leakguard.VisibilityGraphQLArgs(slug), "", []string{graphql}},
		{"the guard's REST read under GH_TOKEN", leakguard.VisibilityReadWithHeadersArgs(slug), "GH_TOKEN", []string{rest}},
		{"the guard's GraphQL read under GITHUB_TOKEN", leakguard.VisibilityGraphQLArgs(slug), "GITHUB_TOKEN", []string{graphql}},
		{"the shared check's read", leakguard.VisibilityReadArgs(slug), "", []string{rest}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				leakguard.VisibilityReadVar: slug,
			}
			if tc.token != "" {
				env[tc.token] = "ghs_caller"
			}
			code, stdout, stderr := legionGh(t, env, tc.args...)
			if code != 0 || stdout != "public\n" {
				t.Fatalf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and the visibility", tc.args, code, stdout, stderr)
			}
			if got := processes(t, log); !slices.Equal(got, tc.want) {
				t.Errorf("legion gh %q ran gh as\n%q\nwant the read alone, no help:\n%q", tc.args, got, tc.want)
			}
		})
	}
}

// A repeated `gh api` call that the guard checks whole runs two gh processes: the guard's read
// of the destination's visibility, then the call. The first call reads gh's help of gh and of gh
// api once and keeps it; the second reads none, and the guard's read inside it reads none
// either. The stand-in check is not the one the read rule was proven against, so the guard runs
// whole and reads the visibility, and the stand-in answers private, which the guard reads again
// for every call.
func TestLegionGhRunsTwoGhProcessesForARepeatedAPICall(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	stub, log := loggingGh(t)
	env := map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"XDG_STATE_HOME": t.TempDir(), "XDG_DATA_HOME": t.TempDir(), "HOME": t.TempDir(),
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses), "GH_STANDIN_VISIBILITY": "private",
	}
	args := []string{"api", "repos/acme/widgets", "--jq", ".full_name"}
	read := strings.Join(leakguard.VisibilityReadWithHeadersArgs("acme/widgets"), " ")
	want := [][]string{
		{"--help", "api --help", read, strings.Join(args, " ")},
		{read, strings.Join(args, " ")},
	}
	for i, want := range want {
		code, stdout, stderr := legionGh(t, env, args...)
		if code != 0 || stdout != "gh ran\n" {
			t.Fatalf("legion gh %q, call %d = exit %d, stdout %q, stderr %q; want exit 0 and gh's answer", args, i+1, code, stdout, stderr)
		}
		if got := processes(t, log); !slices.Equal(got, want) {
			t.Errorf("legion gh %q, call %d, ran %d gh processes:\n%q\nwant %d:\n%q", args, i+1, len(got), got, len(want), want)
		}
	}
}

// A second call reads no help: its keep answers gh's own help and gh api's from the copies the
// first call kept, which are what gh printed, in files of mode 0600 owned by this user, in a
// directory of mode 0700.
func TestGhHelpKeepReadsEachHelpOnce(t *testing.T) {
	stub, log := loggingGh(t)
	env := helpKeepEnv(t)
	first := readHelps(t, stub, env)
	if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
		t.Fatalf("the first call ran gh as %q, want both helps read", got)
	}
	if second := readHelps(t, stub, env); !slices.Equal(second, first) {
		t.Errorf("the second call read other helps than the first")
	}
	if got := processes(t, log); len(got) != 0 {
		t.Errorf("the second call ran gh as %q, want no gh process", got)
	}
	dir := filepath.Join(env["XDG_STATE_HOME"], "legion-gh", "help")
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("the keep's directory = %v, %v; want a directory of mode 0700", info, err)
	}
	for _, name := range copyNames(t, dir) {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ghOwnedBy(info, os.Getuid()) {
			t.Errorf("the copy %s is %v, %v; want a file of mode 0600 owned by %d", name, info, err, os.Getuid())
		}
	}
}

// A change to anything gh's help is printed from makes the next call read the help from gh
// again, and the call after that reads it from the copy kept anew. The gh is reached through a
// link, as a gh on PATH often is, and each change to the binary is made to the file the link ends
// at: its size alone, its modification time alone, and another file renamed over it. The rest are
// an alias added to the gh directory's config.yml, config.yml rewritten in place with its size and
// its modification time kept, config.yml made unreadable, a hosts.yml written, an extension
// installed, and a variable that changes how gh prints.
func TestGhHelpKeepReadsAgainAfterItsInputsChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, stub string, env map[string]string)
	}{
		{"the binary's size", func(t *testing.T, stub string, _ map[string]string) {
			info := stat(t, stub)
			file, err := os.OpenFile(stub, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("# a later build\n"); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			chtimes(t, stub, info.ModTime())
		}},
		{"the binary's modification time", func(t *testing.T, stub string, _ map[string]string) {
			chtimes(t, stub, stat(t, stub).ModTime().Add(time.Minute))
		}},
		{"a binary renamed over it", func(t *testing.T, stub string, _ map[string]string) {
			content, err := os.ReadFile(stub)
			if err != nil {
				t.Fatal(err)
			}
			info := stat(t, stub)
			next := stub + ".next"
			if err := os.WriteFile(next, content, 0o700); err != nil {
				t.Fatal(err)
			}
			chtimes(t, next, info.ModTime())
			if err := os.Rename(next, stub); err != nil {
				t.Fatal(err)
			}
		}},
		{"an alias added", func(t *testing.T, _ string, env map[string]string) {
			if err := os.WriteFile(filepath.Join(env["GH_CONFIG_DIR"], "config.yml"), []byte("aliases:\n    pv: pr view\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"config.yml rewritten in place, its size and time kept", func(t *testing.T, _ string, env map[string]string) {
			config := filepath.Join(env["GH_CONFIG_DIR"], "config.yml")
			info := stat(t, config)
			content, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, bytes.ToUpper(content), 0o600); err != nil {
				t.Fatal(err)
			}
			chtimes(t, config, info.ModTime())
		}},
		{"config.yml made unreadable", func(t *testing.T, _ string, env map[string]string) {
			if err := os.Chmod(filepath.Join(env["GH_CONFIG_DIR"], "config.yml"), 0o000); err != nil {
				t.Fatal(err)
			}
		}},
		{"a hosts.yml written", func(t *testing.T, _ string, env map[string]string) {
			if err := os.WriteFile(filepath.Join(env["GH_CONFIG_DIR"], "hosts.yml"), []byte("github.com:\n    user: x-access-token\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"an extension installed", func(t *testing.T, _ string, env map[string]string) {
			if err := os.MkdirAll(filepath.Join(env["XDG_DATA_HOME"], "gh", "extensions", "gh-hello"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"GH_FORCE_TTY set", func(_ *testing.T, _ string, env map[string]string) { env["GH_FORCE_TTY"] = "1" }},
		{"CLICOLOR_FORCE set", func(_ *testing.T, _ string, env map[string]string) { env["CLICOLOR_FORCE"] = "1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, log := loggingGh(t)
			link := filepath.Join(t.TempDir(), "gh")
			if err := os.Symlink(stub, link); err != nil {
				t.Fatal(err)
			}
			env := helpKeepEnv(t)
			// The gh directory's config.yml and the extensions directory are set an hour back, so
			// a change to either changes its time even where the file system's clock has not
			// ticked since it was made.
			past := time.Now().Add(-time.Hour)
			config := filepath.Join(env["GH_CONFIG_DIR"], "config.yml")
			if err := os.WriteFile(config, []byte("git_protocol: https\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			extensions := filepath.Join(env["XDG_DATA_HOME"], "gh", "extensions")
			if err := os.MkdirAll(extensions, 0o700); err != nil {
				t.Fatal(err)
			}
			chtimes(t, config, past)
			chtimes(t, extensions, past)
			readHelps(t, link, env)
			readHelps(t, link, env)
			if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
				t.Fatalf("two calls ran gh as %q, want the helps read once", got)
			}
			tc.change(t, stub, env)
			readHelps(t, link, env)
			if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
				t.Errorf("after %s the next call ran gh as %q, want both helps read again", tc.name, got)
			}
			readHelps(t, link, env)
			if got := processes(t, log); len(got) != 0 {
				t.Errorf("after %s the second call ran gh as %q, want no gh process", tc.name, got)
			}
		})
	}
}

// A gh whose links end at a multiplexer, as mise's shim does, keeps nothing: the multiplexer picks
// the gh it runs on each run, so the file the link ends at stays the same while the gh behind it
// changes. Every call reads both helps from the gh the multiplexer picked, the call after another
// gh was picked too.
func TestGhHelpKeepKeepsNothingForAMultiplexer(t *testing.T) {
	stub, log := loggingGh(t)
	content, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(other, content, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	current, mise := filepath.Join(bin, "current"), filepath.Join(bin, "mise")
	if err := os.WriteFile(mise, []byte("#!/bin/sh\nexec \"$(cat '"+current+"')\" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "gh")
	if err := os.Symlink(mise, shim); err != nil {
		t.Fatal(err)
	}
	env := helpKeepEnv(t)
	for i, picked := range []string{stub, stub, other} {
		if err := os.WriteFile(current, []byte(picked), 0o600); err != nil {
			t.Fatal(err)
		}
		readHelps(t, shim, env)
		if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
			t.Errorf("call %d through a multiplexer ran gh as %q, want both helps read from the gh it picked", i+1, got)
		}
	}
	if _, err := os.Lstat(filepath.Join(env["XDG_STATE_HOME"], "legion-gh", "help")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a keep for a multiplexer made its directory: %v", err)
	}
}

// A kept copy that is not this user's alone is not read, and the help is read from gh: a copy
// its group or others may write, one in a directory others may write, a keep whose directory and
// copy another user owns, a copy alone another user owns (made only where the test runs as root,
// which alone can give a file away), a link in the copy's place and a link in the directory's.
// Where the directory is still this user's, the fresh read is kept again, as a file of mode 0600
// in a directory of mode 0700.
func TestGhHelpKeepIgnoresACopyThatIsNotTheUsersAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// tamper changes the kept copy in dir, or the keep the next call reads it with.
		tamper func(t *testing.T, dir string, keep *ghHelpKeep)
		// rekept is whether the fresh read is kept again as a private file.
		rekept bool
	}{
		{"a copy its group may write", func(t *testing.T, dir string, _ *ghHelpKeep) { chmodCopies(t, dir, 0o620) }, true},
		{"a copy others may write", func(t *testing.T, dir string, _ *ghHelpKeep) { chmodCopies(t, dir, 0o602) }, true},
		{"a directory others may write", func(t *testing.T, dir string, _ *ghHelpKeep) {
			if err := os.Chmod(dir, 0o777); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a keep another user owns", func(_ *testing.T, _ string, keep *ghHelpKeep) { keep.uid = os.Getuid() + 1 }, false},
		{"a copy alone another user owns", func(t *testing.T, dir string, _ *ghHelpKeep) {
			if os.Geteuid() != 0 {
				t.Skip("only root gives a file to another user")
			}
			for _, name := range copyNames(t, dir) {
				if err := os.Lchown(filepath.Join(dir, name), os.Getuid()+1, -1); err != nil {
					t.Fatal(err)
				}
			}
		}, true},
		{"a link in the copy's place", func(t *testing.T, dir string, _ *ghHelpKeep) {
			elsewhere := t.TempDir()
			for _, name := range copyNames(t, dir) {
				moved := filepath.Join(elsewhere, name)
				if err := os.Rename(filepath.Join(dir, name), moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
		}, true},
		{"a link in the directory's place", func(t *testing.T, dir string, _ *ghHelpKeep) {
			moved := filepath.Join(t.TempDir(), "help")
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, dir); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, log := loggingGh(t)
			env := helpKeepEnv(t)
			want, err := newGhHelpKeep(stub, env, os.Environ()).read(nil)
			if err != nil {
				t.Fatal(err)
			}
			processes(t, log)
			dir := filepath.Join(env["XDG_STATE_HOME"], "legion-gh", "help")
			keep := newGhHelpKeep(stub, env, os.Environ())
			tc.tamper(t, dir, keep)
			got, err := keep.read(nil)
			if err != nil || got != want {
				t.Errorf("the fresh read = %v and another help than gh's", err)
			}
			if ran := processes(t, log); !slices.Equal(ran, []string{"--help"}) {
				t.Errorf("with %s the call ran gh as %q, want gh's help read from gh", tc.name, ran)
			}
			if !tc.rekept {
				return
			}
			if info, err := os.Lstat(dir); err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("the directory kept into again = %v, %v; want mode 0700", info, err)
			}
			for _, name := range copyNames(t, dir) {
				info, err := os.Lstat(filepath.Join(dir, name))
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
					t.Errorf("the copy %s kept again = %v, %v; want a file of mode 0600", name, info, err)
				}
			}
		})
	}
}

// ghOpenPrivate refuses a link in the directory's place and a link in the copy's place where the
// open itself follows the link: under gVisor, the kernel every Legion pod runs, an open of a link
// to a directory with O_DIRECTORY|O_NOFOLLOW opens the directory the link names, where Linux
// answers ELOOP. Here both opens follow every link, so only ghOpenPrivate's own check of each path
// can refuse one; a directory and a copy that are no link are still opened.
func TestGhOpenPrivateRefusesALinkItsOpenFollowed(t *testing.T) {
	openDir, openAt := ghOpenDir, ghOpenAt
	ghOpenDir = func(dir string) (*os.File, error) { return os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY, 0) }
	ghOpenAt = func(parent int, name string) (int, error) {
		return unix.Openat(parent, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	}
	t.Cleanup(func() { ghOpenDir, ghOpenAt = openDir, openAt })
	// private makes a directory of mode 0700 holding a copy of mode 0600, both this user's alone.
	private := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "help")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "copy"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, tc := range []struct {
		name string
		// keep is the directory ghOpenPrivate opens the copy named "copy" in.
		keep func(t *testing.T) string
		want bool
	}{
		{"a directory and a copy that are no link", private, true},
		{"a link in the directory's place", func(t *testing.T) string {
			link := filepath.Join(t.TempDir(), "help")
			if err := os.Symlink(private(t), link); err != nil {
				t.Fatal(err)
			}
			return link
		}, false},
		{"a link in the copy's place", func(t *testing.T) string {
			dir := private(t)
			moved := filepath.Join(t.TempDir(), "copy")
			if err := os.Rename(filepath.Join(dir, "copy"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, filepath.Join(dir, "copy")); err != nil {
				t.Fatal(err)
			}
			return dir
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, opened := ghOpenPrivate(tc.keep(t), "copy", os.Getuid())
			if opened {
				file.Close()
			}
			if opened != tc.want {
				t.Errorf("with %s ghOpenPrivate opened the copy = %v, want %v", tc.name, opened, tc.want)
			}
		})
	}
}

// ghPrivate, the check a copy and its directory are each read under, holds a file owned by the
// user it is asked about with no permission for its group or others, and no file of another
// user's or one its group or others may read or write.
func TestGhPrivateIsTheUsersAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		uid  int
		perm os.FileMode
		want bool
	}{
		{"the user's own copy", os.Getuid(), 0o600, true},
		{"the user's own copy, read only", os.Getuid(), 0o400, true},
		{"another user's copy", os.Getuid() + 1, 0o600, false},
		{"a copy its group may read", os.Getuid(), 0o640, false},
		{"a copy others may write", os.Getuid(), 0o602, false},
	} {
		if err := os.Chmod(path, tc.perm); err != nil {
			t.Fatal(err)
		}
		if got := ghPrivate(stat(t, path), tc.uid); got != tc.want {
			t.Errorf("ghPrivate(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// copyNames is the name of every copy in the keep's directory dir.
func copyNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		t.Fatalf("%s holds no copy", dir)
	}
	return names
}

// chmodCopies gives every copy in dir the mode perm.
func chmodCopies(t *testing.T, dir string, perm os.FileMode) {
	t.Helper()
	for _, name := range copyNames(t, dir) {
		if err := os.Chmod(filepath.Join(dir, name), perm); err != nil {
			t.Fatal(err)
		}
	}
}

// stat is os.Lstat of path, failing the test where it cannot be read.
func stat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// chtimes sets path's access and modification times to at.
func chtimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}
