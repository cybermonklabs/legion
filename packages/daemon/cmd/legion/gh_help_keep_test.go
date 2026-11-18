package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// loggingGh is a stand-in real gh that writes one line to log for every process `legion gh` runs
// of it, its arguments, and then answers as the real gh would: a help request with gh's saved
// help of that command (ghHelpDir), or of its nearest parent the directory holds, which is what
// gh prints for a subcommand it does not know (savedGhHelp); a visibility read made under
// PUBLIC_LEAK_CHECK_VISIBILITY_READ with visibility; and any other call with "gh ran".
func loggingGh(t *testing.T, visibility string) (stub, log string) {
	t.Helper()
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stub, log = filepath.Join(dir, "gh"), filepath.Join(dir, "processes.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >>'" + log + "'\n" +
		"for last; do :; done\n" +
		"if [ \"$last\" = --help ]; then\n" +
		"  path=$(printf '%s ' \"$@\" | sed -e 's/--help $//' -e 's/ $//' -e 's/ /_/g')\n" +
		"  while [ -n \"$path\" ] && [ ! -f '" + help + "'/\"$path\".txt ]; do\n" +
		"    case $path in *_*) path=${path%_*} ;; *) echo 'unknown command' >&2; exit 1 ;; esac\n" +
		"  done\n" +
		"  exec cat '" + help + "'/\"${path:-gh}\".txt\n" +
		"fi\n" +
		"if [ -n \"$" + leakguard.VisibilityReadVar + "\" ]; then\n  echo " + visibility + "\n  exit 0\nfi\n" +
		"echo 'gh ran'\n"
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
// makes it, and answers what the two reads printed.
func readHelps(t *testing.T, stub string, env map[string]string) []string {
	t.Helper()
	keep := newGhHelpKeep(stub, env)
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

// The guard's own visibility reads run one gh process, the read itself, and read no gh help: the
// REST read and the GraphQL read, with the repository's token and with the caller's, and the
// shared check's own read, which `legion gh` answers through the guard's REST read. No state
// directory is given, so no kept help could stand in for a help read.
func TestLegionGhReadsNoHelpForTheGuardsVisibilityReads(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	stub, log := loggingGh(t, "public")
	const slug = "sjawhar/legion"
	rest := strings.Join(leakguard.VisibilityReadWithHeadersArgs(slug), " ")
	for _, tc := range []struct {
		name  string
		args  []string
		token string
		want  []string
	}{
		{"the guard's REST read", leakguard.VisibilityReadWithHeadersArgs(slug), "", []string{rest}},
		{"the guard's GraphQL read", leakguard.VisibilityGraphQLArgs(slug), "", []string{strings.Join(leakguard.VisibilityGraphQLArgs(slug), " ")}},
		{"the guard's REST read under GH_TOKEN", leakguard.VisibilityReadWithHeadersArgs(slug), "GH_TOKEN", []string{rest}},
		{"the guard's GraphQL read under GITHUB_TOKEN", leakguard.VisibilityGraphQLArgs(slug), "GITHUB_TOKEN", []string{strings.Join(leakguard.VisibilityGraphQLArgs(slug), " ")}},
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
	stub, log := loggingGh(t, "private")
	env := map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"XDG_STATE_HOME": t.TempDir(), "XDG_DATA_HOME": t.TempDir(), "HOME": t.TempDir(),
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses),
	}
	args := []string{"api", "repos/acme/widgets", "--jq", ".full_name"}
	want := [][]string{
		{"--help", "api --help", strings.Join(leakguard.VisibilityReadWithHeadersArgs("acme/widgets"), " "), strings.Join(args, " ")},
		{strings.Join(leakguard.VisibilityReadWithHeadersArgs("acme/widgets"), " "), strings.Join(args, " ")},
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
	stub, log := loggingGh(t, "public")
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
	copies, err := os.ReadDir(dir)
	if err != nil || len(copies) != 2 {
		t.Fatalf("the keep holds %v, %v; want two copies", copies, err)
	}
	for _, entry := range copies {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if stat := info.Sys().(*syscall.Stat_t); !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || int(stat.Uid) != os.Getuid() {
			t.Errorf("the copy %s is %v owned by %d; want a file of mode 0600 owned by %d", entry.Name(), info.Mode(), stat.Uid, os.Getuid())
		}
	}
}

// A change to anything gh's help is printed from makes the next call read the help from gh
// again, and the call after that reads it from the copy kept anew: the gh binary's size alone,
// its modification time alone, gh replaced by a rename over it, an alias added to the gh
// directory's config.yml, config.yml made unreadable, a hosts.yml written, an extension
// installed, and a variable that changes how gh prints.
func TestGhHelpKeepReadsAgainAfterItsInputsChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, stub string, env map[string]string)
	}{
		{"the binary's size", func(t *testing.T, stub string, _ map[string]string) {
			info, err := os.Stat(stub)
			if err != nil {
				t.Fatal(err)
			}
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
			if err := os.Chtimes(stub, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
		{"the binary's modification time", func(t *testing.T, stub string, _ map[string]string) {
			info, err := os.Stat(stub)
			if err != nil {
				t.Fatal(err)
			}
			later := info.ModTime().Add(time.Minute)
			if err := os.Chtimes(stub, later, later); err != nil {
				t.Fatal(err)
			}
		}},
		{"a binary renamed over it", func(t *testing.T, stub string, _ map[string]string) {
			content, err := os.ReadFile(stub)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(stub)
			if err != nil {
				t.Fatal(err)
			}
			next := stub + ".next"
			if err := os.WriteFile(next, content, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(next, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(next, stub); err != nil {
				t.Fatal(err)
			}
		}},
		{"an alias added", func(t *testing.T, _ string, env map[string]string) {
			if err := os.WriteFile(filepath.Join(env["GH_CONFIG_DIR"], "config.yml"), []byte("aliases:\n    pv: pr view\n"), 0o600); err != nil {
				t.Fatal(err)
			}
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
			stub, log := loggingGh(t, "public")
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
			for _, path := range []string{config, extensions} {
				if err := os.Chtimes(path, past, past); err != nil {
					t.Fatal(err)
				}
			}
			readHelps(t, stub, env)
			readHelps(t, stub, env)
			if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
				t.Fatalf("two calls ran gh as %q, want the helps read once", got)
			}
			tc.change(t, stub, env)
			readHelps(t, stub, env)
			if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
				t.Errorf("after %s the next call ran gh as %q, want both helps read again", tc.name, got)
			}
			readHelps(t, stub, env)
			if got := processes(t, log); len(got) != 0 {
				t.Errorf("after %s the second call ran gh as %q, want no gh process", tc.name, got)
			}
		})
	}
}

// A kept copy that is not this user's alone is not read, and the help is read from gh: a copy
// its group or others may write, one in a directory others may write, a keep whose directory and
// copy another user owns, and a link in the copy's place. Where the directory is still this
// user's, the fresh read is kept again, as a file of mode 0600 in a directory of mode 0700. A
// test that is no superuser cannot make a file of another user's in its own directory: the
// copy's owner is read by the same check as the directory's (private), which
// TestGhHelpKeepPrivateIsTheUsersAlone reads on a copy.
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, log := loggingGh(t, "public")
			env := helpKeepEnv(t)
			want, err := newGhHelpKeep(stub, env).read(nil)
			if err != nil {
				t.Fatal(err)
			}
			processes(t, log)
			dir := filepath.Join(env["XDG_STATE_HOME"], "legion-gh", "help")
			keep := newGhHelpKeep(stub, env)
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

// private, the check kept applies to a copy and to its directory, holds a file of mode 0600
// owned by the keep's user, and no file of another user's or one its group or others may read or
// write.
func TestGhHelpKeepPrivateIsTheUsersAlone(t *testing.T) {
	stub, _ := loggingGh(t, "public")
	env := helpKeepEnv(t)
	keep := newGhHelpKeep(stub, env)
	if _, err := keep.read(nil); err != nil {
		t.Fatal(err)
	}
	names := copyNames(t, keep.dir)
	copied := filepath.Join(keep.dir, names[0])
	for _, tc := range []struct {
		name string
		uid  int
		perm os.FileMode
		want bool
	}{
		{"the user's own copy", os.Getuid(), 0o600, true},
		{"another user's copy", os.Getuid() + 1, 0o600, false},
		{"a copy its group may read", os.Getuid(), 0o640, false},
		{"a copy others may write", os.Getuid(), 0o602, false},
	} {
		if err := os.Chmod(copied, tc.perm); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(copied)
		if err != nil {
			t.Fatal(err)
		}
		checking := *keep
		checking.uid = tc.uid
		if got := checking.private(info); got != tc.want {
			t.Errorf("private(%s) = %v, want %v", tc.name, got, tc.want)
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

// What ghTargetOf decides is the same from gh's own help, from the call that keeps it and from
// the call that reads the kept copies: the same target, the same files sent and the same refusal
// with the same text, for a set of commands of each kind the resolver reads. The last call starts
// no gh at all.
func TestGhHelpKeepDecidesAsGhsOwnHelpDoes(t *testing.T) {
	stub, log := loggingGh(t, "public")
	env := helpKeepEnv(t)
	dirFor := func(owner, _ string) (string, error) {
		if owner == "acme" || owner == "sjawhar" {
			return filepath.Join("/owners", owner), nil
		}
		return "", errors.New(`the role's GitHub App is not installed on "` + owner + `"`)
	}
	for _, argv := range [][]string{
		{"api", "repos/sjawhar/legion-smoke", "--jq", ".full_name"},
		{"api", "repos/acme/widgets/issues", "-X", "POST", "-F", "body=@body.md"},
		{"api", "graphql", "-F", "owner=acme", "-f", "query=query { viewer { login } }"},
		{"pr", "create", "-R", "acme/widgets", "--fill"},
		{"pr", "create", "-R", "acme/widgets", "--title", "t", "--body-file", "body.md"},
		{"release", "create", "v1", "-R", "acme/widgets", "-F", "notes.md"},
		{"pr", "list"},
		{"pr", "view", "1", "-R", "acme/widgets", "--help=false"},
		{"issue", "transfer", "3", "sjawhar/legion", "-R", "sjawhar/legion-smoke"},
		{"repo", "list", "sjawhar"},
		{"search", "issues", "repo:acme/widgets", "bug"},
		{"repo", "clone", "acme/widgets", "--", "--depth", "1"},
		{"pr", "view", "--help"},
		{"help", "pr", "view"},
		{"pr"},
		{"--version"},
		{"pr", "view", "https://github.com/other/thing/pull/1"},
		{"pr", "view", "1", "--bogus"},
		{"issue", "lock", "1"},
		{"co", "1"},
		{"copilot"},
		{"nosuch"},
		{"pr", "nosuch"},
		{"api", "repos/acme/widgets", "repos/sjawhar/legion"},
		{"pr", "list", "-R", "acme/widgets", "-R", "sjawhar/legion"},
	} {
		fresh, freshErr := ghTargetOf(argv, env, func(path []string) (string, error) { return readGhHelp(stub, path) }, noGitRepository, dirFor)
		processes(t, log)
		for _, call := range []string{"the call that keeps", "the call that reads the copies"} {
			target, err := ghTargetOf(argv, env, newGhHelpKeep(stub, env).read, noGitRepository, dirFor)
			if !reflect.DeepEqual(target, fresh) || errorText(err) != errorText(freshErr) {
				t.Errorf("%s decided ghTargetOf(%q) = %+v, %q; gh's own help decides %+v, %q", call, argv, target, errorText(err), fresh, errorText(freshErr))
			}
			if ran := processes(t, log); call == "the call that reads the copies" && len(ran) != 0 {
				t.Errorf("%s of %q ran gh as %q, want no gh process", call, argv, ran)
			}
		}
	}
}

// errorText is err's message, "" for none.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
