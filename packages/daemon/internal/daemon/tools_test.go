package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/envlisting"
	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

func executable(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := execfile.Write(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// binary is an executable in dir that reads as a compiled program, as the real gh is: an ELF
// header, no `#!` line.
func binary(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := execfile.Write(path, []byte("\x7fELF\x02\x01\x01\x00"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// currentJJ is what the worker image's jj prints for `jj --version`.
const currentJJ = "jj 0.46.0-sami.20261125-112159-d393bdcb1572dc48587ede160608a79a62eeeddf"

// fakeJJ is an executable jj in dir whose `jj --version` prints version.
func fakeJJ(t *testing.T, dir, version string) string {
	t.Helper()
	path := filepath.Join(dir, "jj")
	if err := execfile.Write(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+version+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The jj the worker image pins merges two commits with more than one merge base as it merges any
// other two. m1 and m2 below are two merges of the same n commits, each of which changes f from
// the root's "c" to "b", so f is "b" on both sides and in every merge base, and a merge of m1 and
// m2 can only be a clean "b". That is the minimal case of jj-vcs/jj#6369, where jj merges such
// commits back to the root's "c" without a conflict to say so. K1 merges two children of a merge
// whose f is a conflict, one that resolves f to "R" and one that leaves it, and must give a clean
// "R". K4 merges two merges of the same two commits, which set f to "x" and "y", one resolved to
// "x" and one to "y", and must give a conflict at f, not one side's text or the root's "o". The
// pinned jj must carry the fork's fix of sjawhar/jj#4, which starts from jj-vcs/jj#10100. The test
// checks merge results and not the version: it is red at a jj that fails the minimal case, K1, K4
// or a VB1 subtest, as jj 0.45.1-sami and the cherry-pick of jj-vcs/jj#10100 alone do, and a fork
// release without the fork's later snapshot fixes passes it; the fork's own tests hold those. It
// runs the first compiled jj on PATH (fixturegit.CompiledJJ), which on CI is the one CI's
// daemon-go-race job installs at ARG JJ_TOOL of packages/daemon/docker/worker.Dockerfile, so a
// local run tests whichever jj is first there.
func TestPinnedJJKeepsWhatEverySideOfAMergeHolds(t *testing.T) {
	t.Parallel()
	jj := fixturegit.CompiledJJ(t)
	git := fixturegit.CompiledGit(t)
	home := t.TempDir()
	version := fixturegit.Isolated(t, home).Run(home, jj, "--version")
	t.Logf("%s is %s", jj, version)
	for _, tc := range []struct {
		name string
		n    int
	}{{"two merge bases", 2}, {"three merge bases", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newJJScratch(t, jj, git)
			root := s.root("c\n")
			var bases []string
			for i := 1; i <= tc.n; i++ {
				bases = append(bases, s.commit(fmt.Sprintf("x%d", i), "f", "b\n", root))
			}
			// Each merge keeps its description, so the next `jj new` leaves it in place rather than
			// abandoning it as an empty working-copy commit.
			merges := make([]string, 2)
			for i := range merges {
				s.runJJ(slices.Concat([]string{"new"}, bases, []string{"-m", fmt.Sprintf("m%d", i+1)})...)
				merges[i] = s.commitID("@")
			}
			listed := s.mergeBases(merges[0], merges[1])
			want := slices.Sorted(slices.Values(bases))
			if !slices.Equal(listed, want) {
				t.Fatalf("the fixture is wrong, not jj: git merge-base --all m1 m2 lists %v, want the %d commits x1 to x%d, %v", listed, tc.n, tc.n, want)
			}
			for i, merge := range merges {
				if held := s.show(merge); held != "b\n" {
					t.Fatalf("the fixture is wrong, not jj: m%d holds %q in f, want \"b\\n\", as each of its parents does", i+1, held)
				}
			}
			s.runJJ("new", merges[0], merges[1], "-m", "merge")
			if s.fConflicted("@") || s.show("@") != "b\n" {
				t.Errorf("%s merges m1 and m2, two merges of the same %d commits that each hold \"b\\n\" in f, into %s; want a clean one holding \"b\\n\". "+
					"This is jj-vcs/jj#6369, and the pinned jj must carry the fork's fix of sjawhar/jj#4, which starts from jj-vcs/jj#10100", version, tc.n, s.describe("@"))
			}
		})
	}
	// K1: a and b change f from the root's "o" to "a" and to "b", so m, their merge, holds a conflict
	// at f. r1 and r2 are children of m: r1 resolves f to "R", and r2 adds g and leaves f as m holds
	// it. m is the only merge base of r1 and r2, so m's conflict at f is the merge bases' own merge
	// there, which r2 leaves as it is.
	t.Run("K1", func(t *testing.T) {
		t.Parallel()
		s := newJJScratch(t, jj, git)
		root := s.root("o\n")
		a := s.commit("a", "f", "a\n", root)
		b := s.commit("b", "f", "b\n", root)
		s.runJJ("new", a, b, "-m", "m")
		m := s.commitID("@")
		if !s.fConflicted(m) {
			t.Fatal("the fixture is wrong, not jj: jj file list -r m reads f as clean, want a conflict at f, which a sets to \"a\\n\" and b to \"b\\n\"")
		}
		r1 := s.commit("r1", "f", "R\n", m)
		r2 := s.commit("r2", "g", "x\n", m)
		if listed := s.mergeBases(r1, r2); !slices.Equal(listed, []string{m}) {
			t.Fatalf("the fixture is wrong, not jj: git merge-base --all r1 r2 lists %v, want m alone, %s", listed, m)
		}
		s.runJJ("new", r1, r2, "-m", "merge")
		if s.fConflicted("@") || s.show("@") != "R\n" {
			t.Errorf("%s merges r1, which resolves m's conflict at f to \"R\", and r2, which leaves it, into %s; want a clean one holding \"R\\n\": "+
				"in sjawhar/jj#4's merge rules, where the merge bases' merge does not resolve and one side holds it, the merge takes the other side", version, s.describe("@"))
		}
	})
	// K4: A and B change f from the root's "o" to "x" and to "y", and P1 and P2 are two merges of A
	// and B, which resolve f to "x" and to "y". A and B are the merge bases of P1 and P2, their own
	// merge at f is a conflict, and P1 and P2 each change that conflict, and change it differently.
	t.Run("K4", func(t *testing.T) {
		t.Parallel()
		s := newJJScratch(t, jj, git)
		h := s.crissCross("o\n", "x\n", "y\n")
		listed := s.mergeBases(h.p1, h.p2)
		if want := slices.Sorted(slices.Values([]string{h.a, h.b})); !slices.Equal(listed, want) {
			t.Fatalf("the fixture is wrong, not jj: git merge-base --all P1 P2 lists %v, want A and B, %v", listed, want)
		}
		s.runJJ("new", h.p1, h.p2, "-m", "merge")
		if !s.fConflicted("@") {
			t.Errorf("%s merges P1 and P2, two merges of A and B, which hold \"x\" and \"y\" in f, where P1 resolves f to \"x\" and P2 to \"y\", into a clean commit holding %q in f; want a conflict at f: "+
				"in sjawhar/jj#4's merge rules, where the merge bases' merge of a text file does not resolve and both sides changed it, each differently, the merge merges the two sides against a virtual merge base, as git does, "+
				"and here both sides rewrite the same lines of it, so the merge is a conflict", version, s.show("@"))
		}
	})
	// VB1: in R5 of dispatch://INFRA-4149/spec the virtual merge base that sjawhar/jj#4's merge
	// builds, which holds the merge bases' sides between conflict marker lines as git writes them,
	// is a term of a conflict and never a resolved file, whatever jj operation later merges that
	// conflict, a rebase, an abandon and a squash among them. Each case builds a merge whose merge
	// bases' own merge at f does not resolve, changes it, or its parents, with ordinary commands, and
	// reads the commit those commands leave: f there is a conflict, or a text with no `<<<<<<<`
	// line. jj 0.45.1-sami leaves each of them a conflict, or "y", a parent's text.
	for _, tc := range []struct {
		name string
		// run builds a history, runs the commands and answers the revision whose f the case reads.
		run func(s *jjScratch) string
	}{
		{"a resolution that edits a line of the hunk the merge bases disagree on, rebased onto P1", func(s *jjScratch) string {
			h := s.block()
			s.runJJ("rebase", "-r", h.d, "-d", h.p1)
			return h.d
		}},
		{"a resolution that edits a line of the hunk the merge bases disagree on, after an abandon of P2", func(s *jjScratch) string {
			h := s.block()
			s.runJJ("abandon", h.p2)
			return h.d
		}},
		{"a resolution that edits a line of the hunk the merge bases disagree on, squashed into P1", func(s *jjScratch) string {
			h := s.block()
			s.runJJ("squash", "--from", h.d, "--into", h.p1, "--use-destination-message")
			return h.p1
		}},
		{"a kept conflict checked out with jj new and edited", func(s *jjScratch) string {
			h := s.k4Five()
			s.runJJ("rebase", "-r", h.d, "-d", h.p1)
			s.runJJ("new", h.d, "-m", "w")
			checkedOut, err := os.ReadFile(filepath.Join(s.repo, "f"))
			if err != nil {
				s.t.Fatal(err)
			}
			s.write("f", string(checkedOut)+"SEVEN\n")
			return s.commitID("@")
		}},
		{"one parent restored from the merge, then jj resolve --tool :ours", func(s *jjScratch) string {
			h := s.k4()
			s.runJJ("restore", "--from", h.d, "--into", h.p1, "f")
			s.resolve(h.d, ":ours")
			return h.d
		}},
		{"one parent rebased onto A, then jj resolve --tool :theirs", func(s *jjScratch) string {
			h := s.k4()
			s.runJJ("rebase", "-s", h.p2, "-d", h.a)
			s.resolve(h.d, ":theirs")
			return h.d
		}},
	} {
		t.Run("VB1 "+tc.name, func(t *testing.T) {
			t.Parallel()
			s := newJJScratch(t, jj, git)
			revision := tc.run(s)
			if s.fConflicted(revision) {
				return
			}
			held := s.show(revision)
			for line := range strings.Lines(held) {
				if strings.HasPrefix(line, "<<<<<<<") {
					t.Errorf("%s leaves a clean f holding a `<<<<<<<` line, the virtual merge base's text as a resolved file:\n%s"+
						"want a conflict at f, or a text with no `<<<<<<<` line: R5 of dispatch://INFRA-4149/spec keeps the virtual merge base a term of a conflict, never a resolved file, whatever jj operation later merges that conflict", version, held)
					break
				}
			}
		})
	}
}

// jjScratch is a colocated jj repository of a subtest's own, the jj and the git that run there, and
// the runner their commands run under. Its methods build and read the repository's commits, each
// running in that repository under that runner.
type jjScratch struct {
	t             *testing.T
	jj, git, repo string
	run           fixturegit.Runner
}

// newJJScratch is t's own jjScratch: a repository that jj makes, colocated, in a temporary
// directory of t's, under a runner isolated in another.
func newJJScratch(t *testing.T, jj, git string) *jjScratch {
	t.Helper()
	repo := t.TempDir()
	s := &jjScratch{t: t, jj: jj, git: git, repo: repo, run: fixturegit.Isolated(t, t.TempDir())}
	s.runJJ(fixturegit.JJInit(repo, true)[1:]...)
	return s
}

// runJJ runs the scratch's jj with args in its repository under its runner, and fails the test
// when jj fails.
func (s *jjScratch) runJJ(args ...string) {
	s.t.Helper()
	s.run.Run(s.repo, append([]string{s.jj}, args...)...)
}

// write sets a file of the working copy, which the next jj command snapshots into the working-copy
// commit.
func (s *jjScratch) write(name, content string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.repo, name), []byte(content), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

// root sets f to content in the working-copy commit, commits it as "root", and names that commit,
// the one each history of the test starts from.
func (s *jjScratch) root(content string) string {
	s.t.Helper()
	s.write("f", content)
	s.runJJ("commit", "-m", "root")
	return s.commitID("@-")
}

// commitID names a commit by its commit id, which in a colocated repository is git's id of the
// same commit, so git merge-base reads the ids jj prints.
func (s *jjScratch) commitID(revision string) string {
	s.t.Helper()
	return s.run.Out(s.repo, s.jj, "log", "-r", revision, "--no-graph", "-T", "commit_id")
}

// show is what jj file show prints of f at revision.
func (s *jjScratch) show(revision string) string {
	s.t.Helper()
	return s.run.Out(s.repo, s.jj, "file", "show", "-r", revision, "f")
}

// fConflicted is whether f holds a conflict at revision, as jj file list reads it: the template
// prints "conflict" for a conflicted f and nothing for a clean one, and jj exits 0 either way.
func (s *jjScratch) fConflicted(revision string) bool {
	s.t.Helper()
	return s.run.Out(s.repo, s.jj, "file", "list", "-r", revision, "-T", `if(conflict, "conflict")`, "f") == "conflict"
}

// describe is revision as the test's messages name it: a clean or a conflicted commit, by whether
// f holds a conflict there, and what jj file show prints of f.
func (s *jjScratch) describe(revision string) string {
	s.t.Helper()
	state := "clean"
	if s.fConflicted(revision) {
		state = "conflicted"
	}
	return fmt.Sprintf("a %s commit holding %q in f", state, s.show(revision))
}

// commit starts a commit on parents described as description, sets name to content in it, and
// names it. Reading its id snapshots the file into it, and the commit keeps its description, so
// the next `jj new` leaves it in place rather than abandoning it as an empty working-copy commit.
func (s *jjScratch) commit(description, name, content string, parents ...string) string {
	s.t.Helper()
	s.runJJ(slices.Concat([]string{"new"}, parents, []string{"-m", description})...)
	s.write(name, content)
	return s.commitID("@")
}

// mergeBases is the commits git merge-base --all lists for x and y, sorted.
func (s *jjScratch) mergeBases(x, y string) []string {
	s.t.Helper()
	return slices.Sorted(slices.Values(strings.Fields(s.run.Out(s.repo, s.git, "merge-base", "--all", x, y))))
}

// crissCrossHistory is a root commit and four commits on it, each named by its commit id: A and B,
// two children of the root, and P1 and P2, two merges of A and B, so A and B are the merge bases of
// P1 and P2.
type crissCrossHistory struct{ root, a, b, p1, p2 string }

// crissCross builds a crissCrossHistory whose root holds o in f, whose A and P1 hold a, and whose B
// and P2 hold b.
func (s *jjScratch) crissCross(o, a, b string) crissCrossHistory {
	s.t.Helper()
	var h crissCrossHistory
	h.root = s.root(o)
	h.a = s.commit("A", "f", a, h.root)
	h.b = s.commit("B", "f", b, h.root)
	h.p1 = s.commit("P1", "f", a, h.a, h.b)
	h.p2 = s.commit("P2", "f", b, h.a, h.b)
	return h
}

// vbHistory is a crissCrossHistory and d, a merge of P1 and P2, in which every commit but the root
// is named by its change id.
type vbHistory struct {
	crissCrossHistory
	d string
}

// vbBuild builds a crissCrossHistory of o, a and b, then d, a merge of P1 and P2 that holds d in f,
// and then starts a commit described as "elsewhere" on the root.
func (s *jjScratch) vbBuild(o, a, b, d string) vbHistory {
	s.t.Helper()
	h := vbHistory{crissCrossHistory: s.crissCross(o, a, b)}
	h.d = s.commit("d", "f", d, h.p1, h.p2)
	// The commands a case runs on the history rewrite commits and change their commit ids, so
	// each is named by its change id from here on.
	for _, id := range []*string{&h.a, &h.b, &h.p1, &h.p2, &h.d} {
		*id = s.run.Out(s.repo, s.jj, "log", "-r", *id, "--no-graph", "-T", "change_id")
	}
	s.runJJ("new", h.root, "-m", "elsewhere")
	return h
}

// block is the block history: o holds 1 to 6 in f, A sets line 1 to "a", B sets lines 1 to 3 to
// "b1 b2 b3", P1 and P2 are merges of A and B that keep A's text and B's, and d merges P1 and P2
// and edits "b2", a line inside the hunk where A and B disagree, to "B2".
func (s *jjScratch) block() vbHistory {
	s.t.Helper()
	return s.vbBuild("1\n2\n3\n4\n5\n6\n", "a\n2\n3\n4\n5\n6\n", "b1\nb2\nb3\n4\n5\n6\n", "b1\nB2\nb3\n4\n5\n6\n")
}

// k4Five is a K4 history with five more lines: o holds 1 to 6 in f, A sets line 1 to "x", B sets it
// to "y", P1 and P2 are merges of A and B that keep "x" and "y", and d is a merge of P1 and P2
// that keeps "y" and sets line 5, one of the five more lines, to "FIVE".
func (s *jjScratch) k4Five() vbHistory {
	s.t.Helper()
	return s.vbBuild("1\n2\n3\n4\n5\n6\n", "x\n2\n3\n4\n5\n6\n", "y\n2\n3\n4\n5\n6\n", "y\n2\n3\n4\nFIVE\n6\n")
}

// k4 is a K4 history with no more lines: o holds "o" in f, A sets it to "x", B sets it to "y", P1
// and P2 are merges of A and B that keep "x" and "y", and d is a merge of P1 and P2 that keeps "y".
func (s *jjScratch) k4() vbHistory {
	s.t.Helper()
	return s.vbBuild("o\n", "x\n", "y\n", "y\n")
}

// resolve runs jj resolve with a built-in tool at revision, where a conflict at f is left to
// resolve: at a revision that holds none, jj resolve fails for want of one, and there is nothing
// to do.
func (s *jjScratch) resolve(revision, tool string) {
	s.t.Helper()
	if out, err := s.run.Try(s.repo, s.jj, "resolve", "-r", revision, "--tool", tool); err != nil && s.fConflicted(revision) {
		s.t.Fatalf("jj resolve -r %s --tool %s in %s: %v\n%s", revision, tool, s.repo, err, out)
	}
}

// Boot resolves the binaries Legion itself runs or hands a pane — git, jj and the real gh a pane's
// `legion gh` runs — once, each by its LEGION_<TOOL>_PATH override or on the daemon's PATH, and
// names every missing one with its override.
func TestResolveToolsPrefersTheOverrideAndNamesEveryMissingTool(t *testing.T) {
	t.Parallel()
	onPath, elsewhere := t.TempDir(), t.TempDir()
	executable(t, onPath, "git")
	pinned := executable(t, elsewhere, "git")
	env := map[string]string{"PATH": onPath, "LEGION_GIT_PATH": pinned}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	_, err := resolveTools(lookup)
	if err == nil || !strings.Contains(err.Error(), "jj (set LEGION_JJ_PATH to an absolute executable path)") ||
		!strings.Contains(err.Error(), "gh (set LEGION_GH_PATH to an absolute executable path)") || strings.Contains(err.Error(), "git (") {
		t.Fatalf("resolveTools with no jj or gh = %v, want only jj and gh named with their overrides", err)
	}
	fakeJJ(t, onPath, currentJJ)
	gh := executable(t, elsewhere, "gh")
	env["LEGION_GH_PATH"] = gh
	tools, err := resolveTools(lookup)
	if err != nil {
		t.Fatalf("resolveTools: %v", err)
	}
	if tools["git"] != pinned || tools["jj"] != filepath.Join(onPath, "jj") || tools["gh"] != gh || len(tools) != 3 {
		t.Fatalf("resolveTools = %v, want the override for git and gh and PATH for jj", tools)
	}
	env["LEGION_JJ_PATH"] = "jj"
	if _, err := resolveTools(lookup); err == nil || !strings.Contains(err.Error(), "LEGION_JJ_PATH") {
		t.Fatalf("resolveTools with a relative override = %v, want it refused", err)
	}
}

// A configuration that puts a role on claude needs Claude Code, bun for its MCP server, and the
// built claude-envoy plugin: boot names every gap in one refusal, and takes each tool's override
// before PATH.
func TestResolveClaudeNamesEveryGap(t *testing.T) {
	t.Parallel()
	onPath, plugin := t.TempDir(), t.TempDir()
	env := map[string]string{"PATH": onPath}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	resolve := func() (launchReads, error) { return resolveClaude(config.Config{}, lookup, nil) }
	_, err := resolve()
	want := "worker_backends runs a role on claude, and boot is missing bun (set LEGION_BUN_PATH to an absolute executable path), " +
		"claude (set LEGION_CLAUDE_PATH to an absolute executable path), the claude-envoy plugin (set LEGION_CLAUDE_PLUGIN_DIR to its package directory)"
	if err == nil || err.Error() != want {
		t.Fatalf("resolveClaude with nothing = %v\nwant %s", err, want)
	}
	executable(t, onPath, "bun")
	pinned := executable(t, t.TempDir(), "claude")
	env["LEGION_CLAUDE_PATH"], env["LEGION_CLAUDE_PLUGIN_DIR"] = pinned, plugin
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "holds no dist/envoy-channel.js: build the plugin") {
		t.Fatalf("resolveClaude with an unbuilt plugin = %v", err)
	}
	for _, file := range []string{"dist/envoy-channel.js", "legion/.claude-plugin/plugin.json"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(plugin, file)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugin, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read, err := resolve()
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	read.log(slog.New(slog.NewTextHandler(&logged, nil)))
	for _, want := range []string{"claude=" + pinned, "bun=" + filepath.Join(onPath, "bun"), "plugin=" + plugin} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("resolveClaude logged %q, want %s", logged.String(), want)
		}
	}
	if read.launcher.Marker() != " claude-bridge -- " || read.invocation != "" {
		t.Errorf("resolveClaude = marker %q, invocation %q", read.launcher.Marker(), read.invocation)
	}
	env["LEGION_CLAUDE_PLUGIN_DIR"] = "relative"
	if _, err := resolve(); err == nil || !strings.Contains(err.Error(), "LEGION_CLAUDE_PLUGIN_DIR is not an absolute path") {
		t.Errorf("a relative plugin dir = %v", err)
	}
}

// The gh boot finds on PATH is every pane's real gh (LEGION_GH_PATH), which `legion gh` runs with
// the owner's gh directory. The real gh is a compiled program, so a script heading PATH is a
// wrapper — a machine's own gh shim, which routes to a token of its own, or Legion's own `legion
// gh` wrapper, which would run `legion gh` again — and boot refuses it, naming it and
// LEGION_GH_PATH, rather than taking the compiled gh behind it. LEGION_GH_PATH is the operator's
// own word, so a script it names is taken.
func TestResolveToolsRefusesAScriptAsTheRealGh(t *testing.T) {
	t.Parallel()
	shims, bin := t.TempDir(), t.TempDir()
	binary(t, bin, "git")
	fakeJJ(t, bin, currentJJ)
	real := binary(t, bin, "gh")
	shim := filepath.Join(shims, "gh")
	if err := execfile.Write(shim, []byte("#!/bin/bash\nexec knives gh -- \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PATH": shims + ":" + bin}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	_, err := resolveTools(lookup)
	if err == nil || !strings.Contains(err.Error(), "the first gh on PATH, "+shim+", is a script") || !strings.Contains(err.Error(), "LEGION_GH_PATH") {
		t.Fatalf("resolveTools with a script first on PATH = %v, want it refused naming %s and LEGION_GH_PATH", err, shim)
	}
	env["PATH"] = bin + ":" + shims
	if tools, err := resolveTools(lookup); err != nil || tools["gh"] != real {
		t.Fatalf("resolveTools with the compiled gh first on PATH = %v, %v; want %s", tools, err, real)
	}
	env["PATH"], env["LEGION_GH_PATH"] = shims+":"+bin, shim
	if tools, err := resolveTools(lookup); err != nil || tools["gh"] != shim {
		t.Fatalf("resolveTools with LEGION_GH_PATH naming a script = %v, %v; want %s", tools, err, shim)
	}
}

// Provisioning relies on jj 0.38's configuration layout (minimumJJ), so boot refuses a jj older
// than 0.38, or one whose version it cannot read, naming what it found and LEGION_JJ_PATH, whether
// the jj came from the override or from PATH; 0.38 itself and later releases, a build's suffix
// included, pass.
func TestResolveToolsRefusesAJJOlderThanTheLayoutProvisioningReliesOn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, version string
		override      bool
		refused       []string
	}{
		{"jj 0.37.0 on PATH", "jj 0.37.0", false, []string{"is 0.37.0, older than 0.38", "LEGION_JJ_PATH"}},
		{"jj 0.37.0 at the override", "jj 0.37.0-abc123", true, []string{"is 0.37.0-abc123, older than 0.38", "LEGION_JJ_PATH"}},
		{"a version it cannot read", "jujutsu, probably", false, []string{`printed "jujutsu, probably"`, "LEGION_JJ_PATH"}},
		{"jj 0.38.0", "jj 0.38.0", false, nil},
		{"the worker image's jj", currentJJ, false, nil},
		{"jj 1.0.0 at the override", "jj 1.0.0", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			onPath, elsewhere := t.TempDir(), t.TempDir()
			executable(t, onPath, "git")
			binary(t, onPath, "gh")
			env := map[string]string{"PATH": onPath}
			if tc.override {
				env["LEGION_JJ_PATH"] = fakeJJ(t, elsewhere, tc.version)
			} else {
				fakeJJ(t, onPath, tc.version)
			}
			tools, err := resolveTools(func(name string) (string, bool) { v, ok := env[name]; return v, ok })
			if tc.refused == nil {
				if err != nil {
					t.Fatalf("resolveTools with %s = %v, want it accepted", tc.version, err)
				}
				if tools["jj"] == "" {
					t.Fatalf("resolveTools with %s resolved no jj: %v", tc.version, tools)
				}
				return
			}
			if err == nil {
				t.Fatalf("resolveTools with %s = %v, want it refused", tc.version, tools)
			}
			for _, want := range tc.refused {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("resolveTools with %s = %v, want it to contain %q", tc.version, err, want)
				}
			}
		})
	}
}

// The jj --version boot runs starts without either raw NATS seed the daemon holds, as every
// command the daemon starts does (runtime.WithoutNATSSeeds).
func TestTheJJVersionCheckGetsNoNATSSeed(t *testing.T) {
	// Not parallel: it sets environment variables of the whole process.
	// Unique values, so that only a variable a seed reached can hold one (envlisting).
	paneSeed, daemonSeed := envlisting.Unique("pane-seed-"), envlisting.Unique("daemon-seed-")
	t.Setenv("NATS_NKEY_SEED", paneSeed)
	t.Setenv("NATS_DAEMON_NKEY_SEED", daemonSeed)
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	jj := filepath.Join(dir, "jj")
	if err := execfile.Write(jj, []byte("#!/bin/sh\nenv >"+dump+"\necho '"+currentJJ+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkJJVersion(jj); err != nil {
		t.Fatalf("checkJJVersion(%s) = %v, want it accepted", jj, err)
	}
	seen, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("jj --version never ran: %v", err)
	}
	vars := envlisting.Vars(string(seen))
	if name, leaked := envlisting.Carrying(vars, paneSeed, daemonSeed); leaked {
		t.Errorf("jj --version's environment carries a NATS seed in %s:\n%s", name, seen)
	}
	if vars["PATH"] == "" {
		t.Errorf("jj --version's environment is not the daemon's (no PATH):\n%s", seen)
	}
}
