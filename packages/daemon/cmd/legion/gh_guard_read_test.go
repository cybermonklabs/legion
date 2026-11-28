package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// loggingGhWrapper is a directory holding a pane's `gh` wrapper, for the head of a test's PATH:
// the script `legion gh` accepts as the wrapper (ghWrapperScript), whose `legion` is a script
// that writes one line to log for every start of `legion gh`, its arguments, and then runs this
// test binary as `legion` with them. It answers the directory and the log.
func loggingGhWrapper(t *testing.T) (dir, log string) {
	t.Helper()
	dir, shims := t.TempDir(), t.TempDir()
	log = filepath.Join(shims, "starts.log")
	legion := filepath.Join(shims, "legion")
	if err := execfile.Write(legion, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+shellWord(log)+"\nexec "+shellWord(legionCopy(t))+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := execfile.Write(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexec "+shellWord(legion)+" gh \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, log
}

// A guarded call, run as a pane runs it, through its `gh` wrapper, starts `legion gh` once: the
// call itself. The guard of that `legion gh` runs its own visibility read of the call's repository
// with the real gh itself, routed as `legion gh` routes the read (ghVisibilityReadChild), where it
// used to run `gh` from PATH, the wrapper, and so a second `legion gh`, whose start cost a loaded
// pod about 0.6 s before its real gh ran (dispatch://INFRA-3562, R12). For a private repository of
// an owner the bundle holds, with no kept answer, the stand-in real gh still runs gh's help, the
// read and then the call. For a repository of an owner the bundle holds no directory for, a read
// that writes nothing and that `legion gh` runs with GH_CONFIG_DIR (R4 of dispatch://INFRA-3399),
// the read is refused before any gh runs and the guard reads the visibility with git's ls-remote
// instead, once: the real gh runs gh's help and the call alone. The stand-in check is no copy of
// the one the read rule was proven against and the deployment names a rule file (the fixture's),
// so the guard runs whole and reads the visibility of both.
func TestAGuardedGhCallStartsOneLegionGhAndReadsItsVisibilityWithTheRealGh(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	for _, tc := range []struct {
		name, slug string
		// read is whether the real gh runs the guard's REST read of slug, lsRemotes how many times
		// git's ls-remote runs.
		read      bool
		lsRemotes int
	}{
		{name: "a private repository of an owner the bundle holds", slug: "acme/widgets", read: true},
		{name: "a repository of an owner the bundle holds no directory for", slug: "other-owner/invented-repo", lsRemotes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, log := loggingGh(t)
			wrappers, starts := loggingGhWrapper(t)
			// The stand-in git answers ls-remote as git does for a public repository, and records
			// it; every other git, the one `legion gh` reads the working directory's configuration
			// and remotes with among them, is the real one.
			bin, lsRemote := t.TempDir(), filepath.Join(t.TempDir(), "ls-remote")
			git := "#!/bin/sh\nfor a; do if [ \"$a\" = ls-remote ]; then echo ls-remote >> " + shellWord(lsRemote) +
				"; printf '%s\\tHEAD\\n' " + strings.Repeat("1", 40) + "; exit 0; fi; done\nexec " + shellWord(fixturegit.CompiledGit(t)) + " \"$@\"\n"
			if err := execfile.Write(filepath.Join(bin, "git"), []byte(git), 0o700); err != nil {
				t.Fatal(err)
			}
			args := []string{"api", "repos/" + tc.slug, "--jq", ".full_name"}
			cmd := legionGhCommand(t, t.TempDir(), map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"PATH": wrappers + ":" + bin + ":" + os.Getenv("PATH"), "GH_STANDIN_VISIBILITY": "private",
				"XDG_STATE_HOME": t.TempDir(), "XDG_DATA_HOME": t.TempDir(), "HOME": t.TempDir(), testRoleStateEnv: t.TempDir(),
				"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses), leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
			}, args...)
			// The call starts at the pane's wrapper, as a role's `gh` does, so that its own start
			// is the first line of the log.
			wrapper := filepath.Join(wrappers, "gh")
			cmd.Path, cmd.Args = wrapper, append([]string{wrapper}, args...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil || stdout.String() != "gh ran\n" {
				t.Fatalf("gh %q through the wrapper = %v, stdout %q, stderr %q; want exit 0 and gh's answer", args, err, stdout.String(), stderr.String())
			}
			if got, want := processes(t, starts), []string{"gh " + strings.Join(args, " ")}; !slices.Equal(got, want) {
				t.Errorf("the call started `legion gh` %d times:\n%q\nwant once, the call itself:\n%q", len(got), got, want)
			}
			want := []string{"--help", "api --help", strings.Join(args, " ")}
			if tc.read {
				want = slices.Insert(want, 2, strings.Join(leakguard.VisibilityReadWithHeadersArgs(tc.slug), " "))
			}
			if got := processes(t, log); !slices.Equal(got, want) {
				t.Errorf("the real gh ran as\n%q\nwant\n%q", got, want)
			}
			if got := strings.Count(readRecord(t, lsRemote), "\n"); got != tc.lsRemotes {
				t.Errorf("git's ls-remote ran %d times, want %d", got, tc.lsRemotes)
			}
		})
	}
}

// The real gh the guard of `legion gh` runs its own visibility read with is handed the whole
// environment the `legion gh` it used to run the read through handed it. The shared check's own
// visibility read still goes that way: `legion gh` answers it with a guard that runs `gh` from
// PATH (answerVisibilityRead), so a second `legion gh` runs the real gh for it. The stand-in real
// gh writes its whole environment for the guard's REST read of the repository, and a guarded call
// on that repository, whose guard runs the read itself, gives it the same environment, under one
// environment of the two calls and in one working directory. Read off the code, that is the gh
// directory the repository routes to (ghroute.DirFor), the `to` owner's for a repository a
// configured rename moved and the owner's own for its sibling; the wrapper as GH_PATH; the real
// gh as LEGION_GH_RUN_BY_REAL_GH; cat as GH_PAGER; the names the guard gives each read of its own
// that gh's allowlist keeps (leakguard's ghReadEnv and promptsNothing): NO_COLOR=1,
// CLICOLOR_FORCE=0 and GIT_TERMINAL_PROMPT=0; and no PUBLIC_LEAK_CHECK_VISIBILITY_READ.
func TestTheGuardsOwnVisibilityReadHandsTheRealGhWhatTheInnerLegionGhDid(t *testing.T) {
	t.Parallel()
	root := pairRoutedBundle(t)
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	wrappers := ghWrapperDir(t)
	for _, tc := range []struct{ slug, owner string }{
		{"sjawhar/legion", "cybermonklabs"},
		{"sjawhar/legion-smoke", "sjawhar"},
	} {
		t.Run(tc.slug, func(t *testing.T) {
			read := strings.Join(leakguard.VisibilityReadWithHeadersArgs(tc.slug), " ")
			environ := filepath.Join(t.TempDir(), "environ")
			stub := filepath.Join(t.TempDir(), "real-gh")
			script := ghStandInHelp(help) +
				"if [ \"$*\" = " + shellWord(read) + " ]; then\n  env | LC_ALL=C sort > " + shellWord(environ) + "\n  echo private\n  exit 0\nfi\n" +
				"echo 'gh ran'\n"
			if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			env := map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": ghconfig.Dir(root, "cybermonklabs"), "LEGION_GH_OWNERS_DIR": root,
				"PATH": wrappers + ":" + os.Getenv("PATH"), "XDG_STATE_HOME": t.TempDir(), testRoleStateEnv: t.TempDir(),
				"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses), leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
			}
			// readBy is the environment the real gh wrote for one call of args under env, which
			// prints want.
			readBy := func(env map[string]string, want string, args ...string) []string {
				t.Helper()
				if err := os.Remove(environ); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				code, stdout, stderr := legionGhIn(t, dir, env, "", args...)
				if code != 0 || stdout != want {
					t.Fatalf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and %q", args, code, stdout, stderr, want)
				}
				written := readRecord(t, environ)
				if written == "" {
					t.Fatalf("legion gh %q ran no visibility read of %s with the real gh", args, tc.slug)
				}
				return strings.Split(strings.TrimSuffix(written, "\n"), "\n")
			}
			direct := readBy(env, "gh ran\n", "api", "repos/"+tc.slug, "--jq", ".full_name")
			shared := map[string]string{leakguard.VisibilityReadVar: tc.slug}
			maps.Copy(shared, env)
			inner := readBy(shared, "private\n", leakguard.VisibilityReadArgs(tc.slug)...)
			if !slices.Equal(direct, inner) {
				t.Errorf("the guard's own read handed the real gh\n%s\nthe read through `legion gh` handed it\n%s", strings.Join(direct, "\n"), strings.Join(inner, "\n"))
			}
			for _, want := range []string{
				"GH_CONFIG_DIR=" + ghconfig.Dir(root, tc.owner), "GH_PATH=" + filepath.Join(wrappers, "gh"),
				leakguard.RunByRealGhVar + "=" + stub, "GH_PAGER=cat",
				"NO_COLOR=1", "CLICOLOR_FORCE=0", "GIT_TERMINAL_PROMPT=0",
			} {
				if !slices.Contains(direct, want) {
					t.Errorf("the guard's own read handed the real gh no %s:\n%s", want, strings.Join(direct, "\n"))
				}
			}
			for _, entry := range direct {
				if strings.HasPrefix(entry, leakguard.VisibilityReadVar+"=") {
					t.Errorf("the guard's own read handed the real gh %s, which no read hands it", entry)
				}
			}
		})
	}
}
