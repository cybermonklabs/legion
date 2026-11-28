package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/ghroute/ghroutetest"
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
	if err := execfile.Write(stub, []byte(script), 0o700); err != nil {
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
// directory and a data directory of its own; and roleState, the role's own state volume, which
// the keep's copies are in (newGhHelpKeep), a directory of the test's own too.
func helpKeepEnv(t *testing.T) (env map[string]string, roleState string) {
	t.Helper()
	return map[string]string{"XDG_STATE_HOME": t.TempDir(), "GH_CONFIG_DIR": t.TempDir(), "XDG_DATA_HOME": t.TempDir()}, t.TempDir()
}

// readHelps reads gh's own help and gh api's through a keep made the way one `legion gh` call
// makes it for the gh at the path gh, with roleState as the role's state volume and env as the
// caller's environment, and answers what the two reads printed.
func readHelps(t *testing.T, gh, roleState string, env map[string]string) []string {
	t.Helper()
	caller := os.Environ()
	for name, value := range env {
		caller = append(caller, name+"="+value)
	}
	keep := newGhHelpKeep(context.Background(), gh, roleState, env, ghChildEnviron(caller, map[string]string{"GH_CONFIG_DIR": env["GH_CONFIG_DIR"]}))
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

// ghTargetOfKept is ghroute.TargetOf of argv with gh's help as help answers it, resolved twice
// more through a ghHelpKeep that help backs: first by a keep that keeps each help it reads, then
// by a new keep of the same state, which reads from the kept copies and from help only for a path
// whose help help refused, which is never kept. Each must give the target, the refusal text and
// the files sent that help gives directly. This package's own resolver calls run through it (the
// rename pair's routing rows, the guard rig's reads). internal/ghroute's tests resolve their rows
// there, through no keep, under gh's saved help or an edited help of it (ghroutetest.EditedHelps);
// the keep is held instead to answer every path of gh's saved help as that help does
// (TestGhHelpKeepAnswersEveryPathAsGhsSavedHelp), and every path of each edited help as that help
// does (TestGhHelpKeepAnswersEveryPathAsEachEditedHelp), and TargetOf reads gh's help only path by
// path, through its help argument.
func ghTargetOfKept(t *testing.T, argv []string, env map[string]string, help func([]string) (string, error), remotes func() ([]ghroute.Remote, error), dirFor func(owner, name string) (string, error)) (ghroute.Target, error) {
	t.Helper()
	want, wantErr := ghroute.TargetOf(argv, env, help, remotes, dirFor)
	// The gh the keep is keyed on, which it never runs: help answers in its place.
	gh := filepath.Join(t.TempDir(), "gh")
	if err := execfile.Write(gh, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	keepEnv, roleState := helpKeepEnv(t)
	refused := map[string]bool{}
	for _, call := range []string{"the call that keeps gh's help", "the call that reads the kept copies"} {
		keep := newGhHelpKeep(context.Background(), gh, roleState, keepEnv, nil)
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
		target, err := ghroute.TargetOf(argv, env, keep.read, remotes, dirFor)
		if !reflect.DeepEqual(target, want) || errorText(err) != errorText(wantErr) {
			t.Errorf("%s decided ghroute.TargetOf(%q) = %+v, %q; gh's own help decides %+v, %q", call, argv, target, errorText(err), want, errorText(wantErr))
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

// The keep answers every path of gh's saved help as that help does, and so decides every call
// ghroute.TargetOf resolves under that help as the help decides it, a call that reads the working
// directory's git remotes included, with no list of calls to keep up to date: TargetOf reads gh's
// help only through its help argument, a path at a time, and runs no gh itself (it calls no
// ghroute.ReadHelp). The help is gh 2.98.0's saved help (ghHelpDir) and each release's saved walk
// (ghHelpWalkDir), each directory read under a state of its own. Its paths are every file's, read
// as the path ghroutetest.SavedHelp reads it for (gh.txt gh's own help, else the path's words
// joined by "_"), help topics among them, and, where no file holds them, an unknown command, gh's
// default alias co, and a subcommand pr does not list, which gh answers with pr's help
// (savedHelpPaths). A walk saves no gh alias's help, so its saved help refuses co as gh refuses an
// unknown command; ghHelpDir holds the help gh printed for co. Two files are no path's help and
// are skipped: VERSION, `gh --version`'s first line, and ghHelpDir's reference.txt, gh's
// reference, which regenerate-gh-help.sh saves from `gh help reference` (the 2.98.0 walk's
// reference.txt, the help of the topic reference, holds the same text and is read). A first keep
// reads every path from gh, whose answer here is the saved help's, as nothing is kept yet, and
// keeps each help gh printed; a new keep of the same state reads each kept path from its copy,
// reading nothing from gh, and a refused path from gh again, since a refusal is never kept
// (readThroughBothKeeps). Each answer, its text or its error's, is the saved help's. A directory
// that yields no path fails, and so does a walk of no release.
func TestGhHelpKeepAnswersEveryPathAsGhsSavedHelp(t *testing.T) {
	t.Parallel()
	dirs := []string{ghHelpDir}
	releases, err := os.ReadDir(ghHelpWalkDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, release := range releases {
		if release.IsDir() {
			dirs = append(dirs, filepath.Join(ghHelpWalkDir, release.Name()))
		}
	}
	if len(dirs) == 1 {
		t.Fatalf("%s holds no release's walk", ghHelpWalkDir)
	}
	for _, dir := range dirs {
		t.Run(dir, func(t *testing.T) {
			paths, files := savedHelpPaths(t, dir)
			refused := readThroughBothKeeps(t, "gh's saved help in "+dir, paths, files, ghroutetest.SavedHelp(dir))
			t.Logf("%s: %d paths read through both keeps, %d of its files and %d no file holds; %d kept and read from their copies, %d refused and read from gh's help again",
				dir, len(paths), files, len(paths)-files, len(paths)-refused, refused)
		})
	}
}

// The keep answers every path of each edited help a resolver table's row is read under
// (ghroutetest.EditedHelps) as that help does, as it answers gh's saved help
// (TestGhHelpKeepAnswersEveryPathAsGhsSavedHelp), and so decides every call ghroute.TargetOf
// resolves under an edited help as that help decides it: internal/ghroute's tests resolve those
// rows through no keep. Each edited help is gh 2.98.0's saved help (ghHelpDir) with its edits
// (ghroutetest.EditedHelp), read under a state of its own for the paths the saved help's test
// reads in ghHelpDir and every path an edit names, through the two keeps that test reads them
// through, with the edited help as gh's answer. The help of each path an edit names differs from
// the saved help's, so each edited help is read for a help the saved help's test does not read.
// A registry of no edited help fails.
func TestGhHelpKeepAnswersEveryPathAsEachEditedHelp(t *testing.T) {
	t.Parallel()
	if len(ghroutetest.EditedHelps) == 0 {
		t.Fatal("ghroutetest.EditedHelps holds no edited help")
	}
	saved := ghroutetest.SavedHelp(ghHelpDir)
	for _, name := range slices.Sorted(maps.Keys(ghroutetest.EditedHelps)) {
		t.Run(name, func(t *testing.T) {
			edits := ghroutetest.EditedHelps[name]
			edited := ghroutetest.EditedHelp(saved, edits)
			paths, files := savedHelpPaths(t, ghHelpDir)
			for key := range edits {
				path := strings.Fields(key)
				if text, err := edited(path); err != nil {
					t.Errorf("gh's help of %q edited for %s is the error %q", path, name, err)
				} else if was, _ := saved(path); text == was {
					t.Errorf("gh's help of %q edited for %s is the saved help's: the edit changes nothing", path, name)
				}
				if !slices.ContainsFunc(paths, func(p []string) bool { return slices.Equal(p, path) }) {
					paths = append(paths, path)
				}
			}
			refused := readThroughBothKeeps(t, "gh's saved help in "+ghHelpDir+" edited for "+name, paths, files, edited)
			t.Logf("%s: %d paths of %s read through both keeps, %d of them a path an edit names; %d kept and read from their copies, %d refused and read from gh's help again",
				name, len(paths), ghHelpDir, len(edits), len(paths)-refused, refused)
		})
	}
}

// savedHelpPaths is every path TestGhHelpKeepAnswersEveryPathAsGhsSavedHelp reads in dir, a
// directory of gh's saved help, as that test says, and how many of them, the first, are a file's.
func savedHelpPaths(t *testing.T, dir string) (paths [][]string, files int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		switch name := entry.Name(); {
		case name == "VERSION" || dir == ghHelpDir && name == "reference.txt":
		case !entry.Type().IsRegular() || !strings.HasSuffix(name, ".txt"):
			t.Errorf("%s holds %s, which is no saved help", dir, name)
		case name == "gh.txt":
			paths = append(paths, nil)
		default:
			paths = append(paths, strings.Split(strings.TrimSuffix(name, ".txt"), "_"))
		}
	}
	files = len(paths)
	if files == 0 {
		t.Fatalf("%s holds no saved help", dir)
	}
	for _, path := range [][]string{{"frobnicate"}, {"co"}, {"pr", "frobnicate"}} {
		if !slices.ContainsFunc(paths, func(p []string) bool { return slices.Equal(p, path) }) {
			paths = append(paths, path)
		}
	}
	return paths, files
}

// readThroughBothKeeps reads every path of paths through two keeps of a state of its own, help
// answering in gh's place, as TestGhHelpKeepAnswersEveryPathAsGhsSavedHelp says: a first keep,
// which reads every path from gh and keeps each help gh printed, then a new keep of the same
// state, which reads each kept path from its copy and a refused path from gh again. Each answer,
// its text or its error's, must be help's; what names help in a failure. Each of the first files
// paths is a file's, which help must answer, and help must refuse one path at least. It answers
// how many paths help refused.
func readThroughBothKeeps(t *testing.T, what string, paths [][]string, files int, help func([]string) (string, error)) (refused int) {
	t.Helper()
	for i, path := range paths {
		if _, err := help(path); err != nil {
			refused++
			if i < files {
				t.Errorf("%s answers %q, a path a file holds, with the error %q", what, path, err)
			}
		}
	}
	if refused == 0 {
		t.Errorf("%s answers every path read, want one gh refuses", what)
	}
	// The gh the keep is keyed on, which it never runs: help answers in its place.
	gh := filepath.Join(t.TempDir(), "gh")
	if err := execfile.Write(gh, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env, roleState := helpKeepEnv(t)
	const first = "the keep that keeps each help it reads"
	for _, keeper := range []string{first, "the keep that reads the kept copies"} {
		keep := newGhHelpKeep(context.Background(), gh, roleState, env, nil)
		if keep.dir == "" {
			t.Fatalf("a keep for %s keeps nothing", gh)
		}
		fromGh := false
		keep.fresh = func(path []string) (string, error) {
			fromGh = true
			return help(path)
		}
		for _, path := range paths {
			want, wantErr := help(path)
			fromGh = false
			got, err := keep.read(path)
			if got != want || errorText(err) != errorText(wantErr) {
				at := 0
				for at < len(got) && at < len(want) && got[at] == want[at] {
					at++
				}
				t.Errorf("%s answered gh's help of %q otherwise than %s: from byte %d, %q and the error %q, where that help holds %q and the error %q",
					keeper, path, what, at, got[at:min(len(got), at+40)], errorText(err), want[at:min(len(want), at+40)], errorText(wantErr))
			}
			if wantFromGh := keeper == first || wantErr != nil; fromGh != wantFromGh {
				t.Errorf("%s read gh's help of %q from gh = %v, want %v: nothing is kept before the first keep reads, a refusal is never kept, and every other help is", keeper, path, fromGh, wantFromGh)
			}
		}
	}
	return refused
}

// The guard's own visibility reads run one gh process, the read itself, and read no gh help: the
// REST read and the GraphQL read, with the repository's token and with the caller's, and the
// shared check's own read, which `legion gh` answers through the guard's REST read. Each call has
// a role volume of its own (legionCommand) and no state directory, so no kept help could stand in
// for a help read.
func TestLegionGhReadsNoHelpForTheGuardsVisibilityReads(t *testing.T) {
	t.Parallel()
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

// A repeated `gh api` call that the guard checks whole runs one gh process, the call itself. The
// first call reads gh's help of gh and of gh api and the guard's read of the destination's
// visibility, and keeps both in the role's own state volume, which the two calls share
// (testRoleStateEnv): the helps (ghHelpKeep), and the private the stand-in answers, which the
// guard keeps there for two minutes (leakguard's Guard.keepVisibility). The second call, well
// inside those two minutes, reads no help and no visibility. The stand-in check is not the one
// the read rule was proven against and the deployment names a rule file (the fixture's), so the
// guard runs whole and reads the visibility on the first call.
func TestLegionGhRunsOneGhProcessForARepeatedAPICall(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	stub, log := loggingGh(t)
	env := map[string]string{
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"XDG_STATE_HOME": t.TempDir(), "XDG_DATA_HOME": t.TempDir(), "HOME": t.TempDir(), testRoleStateEnv: t.TempDir(),
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses), "GH_STANDIN_VISIBILITY": "private",
		leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
	}
	args := []string{"api", "repos/acme/widgets", "--jq", ".full_name"}
	read := strings.Join(leakguard.VisibilityReadWithHeadersArgs("acme/widgets"), " ")
	want := [][]string{
		{"--help", "api --help", read, strings.Join(args, " ")},
		{strings.Join(args, " ")},
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
	t.Parallel()
	stub, log := loggingGh(t)
	env, roleState := helpKeepEnv(t)
	first := readHelps(t, stub, roleState, env)
	if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
		t.Fatalf("the first call ran gh as %q, want both helps read", got)
	}
	if second := readHelps(t, stub, roleState, env); !slices.Equal(second, first) {
		t.Errorf("the second call read other helps than the first")
	}
	if got := processes(t, log); len(got) != 0 {
		t.Errorf("the second call ran gh as %q, want no gh process", got)
	}
	dir := filepath.Join(roleState, "legion-gh", "help")
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

// `legion gh` keeps gh's help in the role's own state volume: under ghRoleStateBase's
// legion-gh/help, here the directory the test hook names (testRoleStateEnv), one copy of gh's own
// help and one of gh api's, each a file of mode 0600 this user owns in a directory of mode 0700,
// and nothing under $XDG_STATE_HOME/legion-gh/help. The volume needs no XDG_STATE_HOME: with none
// set, the copies are kept in it all the same.
func TestGhHelpKeepIsInTheRoleVolume(t *testing.T) {
	t.Parallel()
	root, defaultDir := gitCredentialBundle(t)
	for _, tc := range []struct {
		name string
		// stateHome is whether the call is given XDG_STATE_HOME.
		stateHome bool
	}{
		{"with a state directory", true},
		{"with no state directory", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, _ := loggingGh(t)
			roleState, state := t.TempDir(), t.TempDir()
			env := map[string]string{
				"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
				"XDG_DATA_HOME": t.TempDir(), "HOME": t.TempDir(), testRoleStateEnv: roleState,
				"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), ghCheckPasses), leakguard.RulesFileVar: leakguardtest.WriteRules(t, t.TempDir()),
			}
			if tc.stateHome {
				env["XDG_STATE_HOME"] = state
			}
			args := []string{"api", "repos/acme/widgets", "--jq", ".full_name"}
			if code, stdout, stderr := legionGh(t, env, args...); code != 0 || stdout != "gh ran\n" {
				t.Fatalf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 0 and gh's answer", args, code, stdout, stderr)
			}
			dir := filepath.Join(roleState, "legion-gh", "help")
			if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || !ghOwnedBy(info, os.Getuid()) {
				t.Fatalf("the role volume's keep %s = %v, %v; want a directory of mode 0700 owned by %d", dir, info, err, os.Getuid())
			}
			var paths []string
			for _, name := range copyNames(t, dir) {
				path := filepath.Join(dir, name)
				if info := stat(t, path); !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ghOwnedBy(info, os.Getuid()) {
					t.Errorf("the copy %s is %v; want a file of mode 0600 owned by %d", name, info, os.Getuid())
				}
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var entry ghHelpEntry
				if err := json.Unmarshal(content, &entry); err != nil || entry.Help == "" {
					t.Errorf("the copy %s = %q, %v; want gh's help of a command path", name, content, err)
				}
				paths = append(paths, strings.Join(entry.Path, " "))
			}
			slices.Sort(paths)
			if !slices.Equal(paths, []string{"", "api"}) {
				t.Errorf("the role volume keeps the helps of %q, want gh's own and gh api's", paths)
			}
			if _, err := os.Lstat(filepath.Join(state, "legion-gh", "help")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("legion gh kept help under XDG_STATE_HOME as well (%v), want nothing there", err)
			}
		})
	}
}

// Where the role volume is there, a copy under $XDG_STATE_HOME/legion-gh/help is never read,
// though it holds gh's help under exactly this call's inputs: a keep with no role volume kept it
// there and reads it back with no gh, and a keep with the volume then reads both helps from gh
// again, keeps them in the volume under the same names, and reads them from there on the call
// after.
func TestGhHelpKeepReadsNoCopyInTheStateDirectoryWhereThereIsARoleVolume(t *testing.T) {
	t.Parallel()
	stub, log := loggingGh(t)
	env, roleState := helpKeepEnv(t)
	none := filepath.Join(t.TempDir(), "missing")
	planted := readHelps(t, stub, none, env)
	if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
		t.Fatalf("the keep with no role volume ran gh as %q, want both helps read", got)
	}
	readHelps(t, stub, none, env)
	if got := processes(t, log); len(got) != 0 {
		t.Fatalf("the keep with no role volume ran gh as %q on its second call, want the copies it kept under XDG_STATE_HOME read", got)
	}
	stateCopies := copyNames(t, filepath.Join(env["XDG_STATE_HOME"], "legion-gh", "help"))
	if got := readHelps(t, stub, roleState, env); !slices.Equal(got, planted) {
		t.Errorf("the keep with a role volume read other helps than gh printed before")
	}
	if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
		t.Errorf("the keep with a role volume ran gh as %q, want both helps read from gh and no copy under XDG_STATE_HOME read", got)
	}
	readHelps(t, stub, roleState, env)
	if got := processes(t, log); len(got) != 0 {
		t.Errorf("the second keep with a role volume ran gh as %q, want the copies kept in the volume read", got)
	}
	if got := copyNames(t, filepath.Join(roleState, "legion-gh", "help")); !slices.Equal(got, stateCopies) {
		t.Errorf("the role volume holds the copies %q, want those of the same inputs, %q", got, stateCopies)
	}
}

// With no role volume this user can write, as in a tmux pane, which has none, the copies are kept
// under $XDG_STATE_HOME/legion-gh/help as before there was one, and the call after reads them and
// runs no gh. The volume's place is a missing path, a file, or a directory this user cannot write
// (made only where the test does not run as root, which may write any), and nothing is made in it.
func TestGhHelpKeepIsInTheStateDirectoryWhereThereIsNoRoleVolume(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// base makes the role volume's place.
		base func(t *testing.T) string
	}{
		{"a missing path", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }},
		{"a file", func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "state")
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return file
		}},
		{"a directory this user cannot write", func(t *testing.T) string {
			if os.Geteuid() == 0 {
				t.Skip("root may write a directory of mode 0555")
			}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			return dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, log := loggingGh(t)
			env, _ := helpKeepEnv(t)
			base := tc.base(t)
			first := readHelps(t, stub, base, env)
			if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
				t.Fatalf("the first call ran gh as %q, want both helps read", got)
			}
			if second := readHelps(t, stub, base, env); !slices.Equal(second, first) {
				t.Errorf("the second call read other helps than the first")
			}
			if got := processes(t, log); len(got) != 0 {
				t.Errorf("the second call ran gh as %q, want the copies under XDG_STATE_HOME read", got)
			}
			if got := copyNames(t, filepath.Join(env["XDG_STATE_HOME"], "legion-gh", "help")); len(got) != 2 {
				t.Errorf("XDG_STATE_HOME/legion-gh/help holds the copies %q, want gh's own help and gh api's", got)
			}
			if _, err := os.Lstat(filepath.Join(base, "legion-gh")); err == nil {
				t.Errorf("a keep with %s as its role volume made legion-gh in it", tc.name)
			}
		})
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
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, stub string, env map[string]string)
	}{
		{"the binary's size", func(t *testing.T, stub string, _ map[string]string) {
			info := stat(t, stub)
			if err := execfile.Append(stub, []byte("# a later build\n")); err != nil {
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
			if err := execfile.Write(next, content, 0o700); err != nil {
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
		{"NO_COLOR set", func(_ *testing.T, _ string, env map[string]string) { env["NO_COLOR"] = "legion-help-keep" }},
		{"CLICOLOR_FORCE set", func(_ *testing.T, _ string, env map[string]string) { env["CLICOLOR_FORCE"] = "legion-help-keep" }},
		{"GH_REPO set", func(_ *testing.T, _ string, env map[string]string) { env["GH_REPO"] = "acme/legion-help-keep" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub, log := loggingGh(t)
			link := filepath.Join(t.TempDir(), "gh")
			if err := os.Symlink(stub, link); err != nil {
				t.Fatal(err)
			}
			env, roleState := helpKeepEnv(t)
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
			readHelps(t, link, roleState, env)
			readHelps(t, link, roleState, env)
			if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
				t.Fatalf("two calls ran gh as %q, want the helps read once", got)
			}
			tc.change(t, stub, env)
			readHelps(t, link, roleState, env)
			if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
				t.Errorf("after %s the next call ran gh as %q, want both helps read again", tc.name, got)
			}
			readHelps(t, link, roleState, env)
			if got := processes(t, log); len(got) != 0 {
				t.Errorf("after %s the second call ran gh as %q, want no gh process", tc.name, got)
			}
		})
	}
}

// A variable of the caller's that gh is never handed (ghChildEnviron) changes no help gh prints,
// so the copies kept without it still answer: the call after it is set runs no gh.
func TestGhHelpKeepKeysOnlyWhatGhIsHanded(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"GH_FORCE_TTY", "GH_PAGER", "GH_DEBUG", "GH_EDITOR", "GH_TELEMETRY"} {
		t.Run(name, func(t *testing.T) {
			stub, log := loggingGh(t)
			link := filepath.Join(t.TempDir(), "gh")
			if err := os.Symlink(stub, link); err != nil {
				t.Fatal(err)
			}
			env, roleState := helpKeepEnv(t)
			readHelps(t, link, roleState, env)
			if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
				t.Fatalf("the first call ran gh as %q, want both helps read", got)
			}
			env[name] = "1"
			readHelps(t, link, roleState, env)
			if got := processes(t, log); len(got) != 0 {
				t.Errorf("after %s was set the next call ran gh as %q, want the kept copies read", name, got)
			}
		})
	}
}

// A gh whose links end at a multiplexer, as mise's shim does, keeps nothing: the multiplexer picks
// the gh it runs on each run, so the file the link ends at stays the same while the gh behind it
// changes. Every call reads both helps from the gh the multiplexer picked, the call after another
// gh was picked too.
func TestGhHelpKeepKeepsNothingForAMultiplexer(t *testing.T) {
	t.Parallel()
	stub, log := loggingGh(t)
	content, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "gh")
	if err := execfile.Write(other, content, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	current, mise := filepath.Join(bin, "current"), filepath.Join(bin, "mise")
	if err := execfile.Write(mise, []byte("#!/bin/sh\nexec \"$(cat '"+current+"')\" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(t.TempDir(), "gh")
	if err := os.Symlink(mise, shim); err != nil {
		t.Fatal(err)
	}
	env, roleState := helpKeepEnv(t)
	for i, picked := range []string{stub, stub, other} {
		if err := os.WriteFile(current, []byte(picked), 0o600); err != nil {
			t.Fatal(err)
		}
		readHelps(t, shim, roleState, env)
		if got := processes(t, log); !slices.Equal(got, []string{"--help", "api --help"}) {
			t.Errorf("call %d through a multiplexer ran gh as %q, want both helps read from the gh it picked", i+1, got)
		}
	}
	for _, base := range []string{roleState, env["XDG_STATE_HOME"]} {
		if _, err := os.Lstat(filepath.Join(base, "legion-gh", "help")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a keep for a multiplexer made its directory under %s: %v", base, err)
		}
	}
}

// A kept copy that is not this user's alone is not read, and the help is read from gh: a copy
// its group or others may write, one in a directory others may write, a keep whose directory and
// copy another user owns, a copy alone another user owns (made only where the test runs as root,
// which alone can give a file away), a link in the copy's place and a link in the directory's.
// Where the directory is still this user's, the fresh read is kept again, as a file of mode 0600
// in a directory of mode 0700.
func TestGhHelpKeepIgnoresACopyThatIsNotTheUsersAlone(t *testing.T) {
	t.Parallel()
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
			env, roleState := helpKeepEnv(t)
			want, err := newGhHelpKeep(context.Background(), stub, roleState, env, os.Environ()).read(nil)
			if err != nil {
				t.Fatal(err)
			}
			processes(t, log)
			dir := filepath.Join(roleState, "legion-gh", "help")
			keep := newGhHelpKeep(context.Background(), stub, roleState, env, os.Environ())
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
	// Not parallel: it sets the package variables ghOpenAt and ghOpenDir.
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
	t.Parallel()
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
