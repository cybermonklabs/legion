package shim_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
)

// otherRole is a second workspace of the issue's clone, as another role of the issue works one:
// what it runs rewrites the working-copy commit of the workspace under test, which jj then finds
// stale where that rewrite changed its tree.
type otherRole struct {
	w   podWorkspace
	dir string
}

// newOtherRole adds the second workspace, on the parent of the working-copy commit of the
// workspace under test.
func newOtherRole(w podWorkspace) otherRole {
	w.t.Helper()
	dir := filepath.Join(w.t.TempDir(), "second")
	w.as("implementer", "jj", "workspace", "add", dir, "--name", "second", "-r", "@-")
	return otherRole{w: w, dir: dir}
}

// workingCopy is the revset of the working-copy commit of the workspace under test, which
// workspace.Provision names after its directory.
func (o otherRole) workingCopy() string { return filepath.Base(o.w.dir) + "@" }

// run runs one jj command in the other role's workspace, as reviewer.
func (o otherRole) run(argv ...string) string {
	o.w.t.Helper()
	return o.w.asIn(o.dir, "reviewer", argv...)
}

// change describes a change of name to body in the other role's workspace, and answers its commit.
func (o otherRole) change(name, body, message string) string {
	o.w.t.Helper()
	o.w.writeIn(o.dir, map[string]string{name: body})
	o.run("jj", "describe", "-m", message)
	return o.run("jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", "commit_id")
}

// rebase rebases the working-copy commit of the workspace under test onto onto.
func (o otherRole) rebase(onto string) {
	o.w.t.Helper()
	o.run("jj", "rebase", "-r", o.workingCopy(), "-d", onto)
}

// reauthor re-authors the working-copy commit of the workspace under test with no snapshot, which
// keeps its tree.
func (o otherRole) reauthor() {
	o.w.t.Helper()
	o.run("jj", "--ignore-working-copy", "metaedit", "--update-author", "-r", o.workingCopy())
}

// recordingJJ writes the jj the shim runs, in a directory of its own for the shim's PATH: a
// script that logs each command it is run with, as one line of the log, and then runs the real
// jj, after cases, the branches of a shell `case " $* " in … esac` that may do something else for
// a row's command first. It answers the directory and the log.
func recordingJJ(t *testing.T, jj, cases string) (bin, log string) {
	t.Helper()
	bin, log = t.TempDir(), filepath.Join(t.TempDir(), "jj-commands")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuoted(log) + "\nREAL=" + shellQuoted(jj) + "\n" +
		"case \" $* \" in\n" + cases + "esac\nexec \"$REAL\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "jj"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// commandsRan is each command the recording jj logged, one line each.
func commandsRan(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read what the shim's jj ran: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// rewriteAfterMetaedit is the case of a recording jj in which the real metaedit runs and then the
// other role's workspace rebases the working-copy commit onto onto, while the adoption is still
// running: the flagged commands take no working-copy lock, so nothing holds that rebase back.
func rewriteAfterMetaedit(o otherRole, onto string) string {
	return "*' metaedit '*)\n\"$REAL\" \"$@\"\nstatus=$?\ncd " + shellQuoted(o.dir) + " && \"$REAL\" rebase -r " +
		shellQuoted(o.workingCopy()) + " -d " + shellQuoted(onto) + " >/dev/null 2>&1 || exit 9\nexit $status ;;\n"
}

// firstWorkingCopyRead is the case of a recording jj in which the first `jj debug working-copy`,
// the freshness check's first command before the adoption's change, runs answer in place of the
// real jj, and every later one runs the real jj. marker is a file of the test's own.
func firstWorkingCopyRead(marker, answer string) string {
	return "*' debug working-copy '*)\nif [ ! -e " + shellQuoted(marker) + " ]; then\n: > " + shellQuoted(marker) + "\n" + answer + "\nfi ;;\n"
}

// snapshots is how many operations of the issue's repository snapshotted a working copy.
func (w podWorkspace) snapshots() int {
	w.t.Helper()
	log := w.as("tester", "jj", "--ignore-working-copy", "op", "log", "--no-graph", "-T", `description ++ "\n"`, "--color=never")
	return strings.Count(log+"\n", "snapshot working copy\n")
}

// countLines is how many of lines contain substr.
func countLines(lines []string, substr string) int {
	n := 0
	for _, line := range lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// A stale working copy no longer stops a phase task. In a workspace an issue pod provisions,
// another role's workspace rewrites the working-copy commit so that jj finds it stale, and a role
// changes a tracked file and writes a new one with no jj command between. The adoption reads the
// working copy stale with no snapshot, runs `jj workspace update-stale`, and answers success; the
// update leaves those edits in a commit of @'s change that is not checked out, and no longer on
// disk, so the answer names that commit, the shim's log line says so, and the role's next `jj
// status` finds the working copy fresh. The same holds where the rewrite lands during the
// adoption, after its change, and where the other role abandoned the working-copy commit, whose
// change the update writes the edits into. A refusal after the update, or an update cut off at
// its bound once it wrote, still names the commit. And a freshness check that cannot read jj's
// answer, or whose command fails, reads the working copy as stale, says so in one warning line,
// and the update then finds nothing to do.
//
// The jj is the pinned one (fixturegit.PinnedJJ): the check reads jj's debug output, which no
// release holds stable, so a bump that changes its shape fails here by name. The shim runs it
// through a recording wrapper, which also stands for the rows' other actors.
func TestAdoptWorkingCopyBringsAStaleWorkspaceUpToDateAndNamesTheCommitThatHoldsItsEdits(t *testing.T) {
	jj := fixturegit.PinnedJJ(t)
	edits := map[string]string{"README.md": "smoke, edited by a role\n", "new.txt": "a file no jj command recorded\n"}
	staleByRebase := func(w podWorkspace, _ string) string {
		o := newOtherRole(w)
		o.rebase(o.change("other.txt", "another role's file\n", "another role's change"))
		if status, err := w.try("tester", "jj", "status"); err == nil || !strings.Contains(status, "The working copy is stale") {
			w.t.Fatalf("jj status after the other role's rebase ended %v and printed:\n%s\nwant the stale working copy every ordinary command meets", err, status)
		}
		return ""
	}
	for _, tc := range []struct {
		name string
		// arrange sets the workspace up and answers the cases of the shim's recording jj; marker
		// is a file of the test's own a case may use.
		arrange func(w podWorkspace, marker string) string
		// edits is whether a tracked file is changed and a new file written after arrange, with
		// no jj command between.
		edits    bool
		budget   int
		ok       bool
		errorHas []string
		// held is whether the answer names one commit, which holds the edits.
		held bool
		// updates is how many times `jj workspace update-stale` ran, and afterChange whether it
		// ran after the adoption's change, at the freshness check that follows it.
		updates     int
		afterChange bool
		// warned is what the one warning line of the freshness check says, "" for no such line.
		warned string
	}{
		{name: "stale with edits", arrange: staleByRebase, edits: true, ok: true, held: true, updates: 1},
		{name: "stale, clean", arrange: staleByRebase, ok: true, updates: 1},
		{name: "stale, update cut off after it wrote",
			arrange: func(w podWorkspace, marker string) string {
				staleByRebase(w, marker)
				return "*' workspace update-stale '*) \"$REAL\" \"$@\"; exec sleep 60 ;;\n"
			},
			// The update's bound is the budget less its reserve, the smaller of 5 s and a quarter.
			edits: true, budget: 6_000, errorHas: []string{"Command timed out after 4.5 s", "workspace update-stale"}, held: true, updates: 1},
		{name: "stale, refused after the update",
			arrange: func(w podWorkspace, marker string) string {
				staleByRebase(w, marker)
				return "*' metaedit '*) echo \"Error: the author could not be set\" >&2; exit 1 ;;\n"
			},
			edits: true, errorHas: []string{"Command failed (exit 1)", "metaedit", "the author could not be set"}, held: true, updates: 1},
		{name: "stale during the adoption",
			arrange: func(w podWorkspace, _ string) string {
				o := newOtherRole(w)
				return rewriteAfterMetaedit(o, o.change("other.txt", "another role's file\n", "another role's change"))
			},
			ok: true, updates: 1, afterChange: true},
		{name: "an operation lands while a metaedit writes none",
			arrange: func(w podWorkspace, _ string) string {
				// @ already carries the adoption's identity, so its metaedit writes no operation.
				w.as("tester", "jj", "metaedit", "--update-author")
				o := newOtherRole(w)
				return rewriteAfterMetaedit(o, o.change("other.txt", "another role's file\n", "another role's change"))
			},
			ok: true, updates: 1, afterChange: true},
		{name: "stale, ws@ abandoned",
			arrange: func(w podWorkspace, _ string) string {
				w.write(map[string]string{"recorded.txt": "a change jj recorded\n"})
				w.as("implementer", "jj", "status")
				o := newOtherRole(w)
				o.run("jj", "abandon", o.workingCopy())
				return ""
			},
			edits: true, ok: true, held: true, updates: 1},
		{name: "output that does not parse",
			arrange: func(_ podWorkspace, marker string) string {
				return firstWorkingCopyRead(marker, "echo 'Current operation: of another shape'; exit 0")
			},
			ok: true, updates: 1, warned: `its first line: "Current operation: of another shape"`},
		{name: "a command of the check fails",
			arrange: func(_ podWorkspace, marker string) string {
				return firstWorkingCopyRead(marker, "echo 'Error: an injected failure of the check' >&2; exit 1")
			},
			ok: true, updates: 1, warned: "Error: an injected failure of the check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPodWorkspace(t, jj)
			cases := tc.arrange(w, filepath.Join(t.TempDir(), "answered"))
			if tc.edits {
				w.write(edits)
			}
			bin, log := recordingJJ(t, jj, cases)
			budget := tc.budget
			if budget == 0 {
				budget = 60_000
			}

			got, shim := w.adoptWith("tester", bin, budget)
			if got.OK != tc.ok {
				t.Fatalf("the shim answered %#v, want ok=%v; log:\n%s", got, tc.ok, shim.log)
			}
			for _, want := range tc.errorHas {
				if !strings.Contains(got.Error, want) {
					t.Errorf("the refusal %q does not say %q", got.Error, want)
				}
			}
			ran := commandsRan(t, log)
			if n := countLines(ran, "workspace update-stale"); n != tc.updates {
				t.Errorf("jj workspace update-stale ran %d time(s), want %d; jj ran:\n%s", n, tc.updates, strings.Join(ran, "\n"))
			}
			if tc.afterChange {
				change := slices.IndexFunc(ran, func(line string) bool { return strings.Contains(line, " metaedit ") })
				update := slices.IndexFunc(ran, func(line string) bool { return strings.Contains(line, "workspace update-stale") })
				if change < 0 || update < change {
					t.Errorf("jj workspace update-stale ran at %d and the metaedit at %d, want the update after the change, at the check that follows it; jj ran:\n%s",
						update, change, strings.Join(ran, "\n"))
				}
			}
			lines := strings.Split(shim.log.String(), "\n")
			warnings := countLines(lines, "warning: the freshness check")
			if tc.warned == "" && warnings != 0 || tc.warned != "" && (warnings != 1 || countLines(lines, tc.warned) != 1) {
				t.Errorf("the shim wrote %d warning line(s) of the freshness check, want %d saying %q; log:\n%s",
					warnings, min(len(tc.warned), 1), tc.warned, shim.log)
			}
			if tc.ok {
				// Read before the commit's content, so a working copy the adoption left stale is
				// named first, as the role's next command meets it.
				status, err := w.try("tester", "jj", "status")
				if err != nil || strings.Contains(strings.ToLower(status), "stale") {
					t.Errorf("jj status after the adoption ended %v and printed:\n%s\nwant exit 0 and no stale working copy", err, status)
				}
			}

			if !tc.held {
				if len(got.EditsCommits) != 0 {
					t.Errorf("the answer names %q, want no commit: no edit was left anywhere", got.EditsCommits)
				}
			} else {
				if len(got.EditsCommits) != 1 {
					t.Fatalf("the answer names %q, want the one commit that holds the edits; log:\n%s", got.EditsCommits, shim.log)
				}
				commit := got.EditsCommits[0]
				if countLines(lines, commit) != 1 || countLines(lines, "no longer on disk") != 1 {
					t.Errorf("the shim's log does not name %s in one line saying the edits are there and no longer on disk:\n%s", commit, shim.log)
				}
				for name, body := range edits {
					if got := w.as("tester", "jj", "--ignore-working-copy", "file", "show", "-r", commit, "--", name); got != strings.TrimSpace(body) {
						t.Errorf("commit %s holds %s as %q, want the edit %q that no jj command recorded", commit, name, got, strings.TrimSpace(body))
					}
				}
				if body, err := os.ReadFile(filepath.Join(w.dir, "README.md")); err != nil || string(body) == edits["README.md"] {
					t.Errorf("README.md is still on disk as the role edited it (%v): want the edit only in %s", err, commit)
				}
				if _, err := os.Stat(filepath.Join(w.dir, "new.txt")); !os.IsNotExist(err) {
					t.Errorf("new.txt is still on disk (%v): want it only in %s", err, commit)
				}
			}
		})
	}
}

// On a working copy that is fresh the adoption runs no `jj workspace update-stale`, which would
// snapshot it, and names no commit: every command it runs carries --ignore-working-copy, and the
// repository's log gains no snapshot of a working copy while an edit no jj command recorded is on
// disk throughout. Fresh is what jj's own check finds: the working copy's recorded operation is
// the head, or it is behind the head and the working-copy commit still has the tree and the
// conflict labels the working copy recorded — after another workspace re-authored that commit
// with no snapshot, rebased it onto a commit of the same tree, or re-authored a conflicted one —
// and after the adoption's own change, which keeps both.
func TestAdoptWorkingCopyOnAFreshWorkspaceRunsNoUpdateStale(t *testing.T) {
	jj := fixturegit.PinnedJJ(t)
	conflicted := func(w podWorkspace) otherRole {
		w.write(map[string]string{"README.md": "the workspace's side\n"})
		w.as("implementer", "jj", "status")
		o := newOtherRole(w)
		o.rebase(o.change("README.md", "another role's side\n", "another role's side"))
		// An ordinary command fails on the stale working copy; the update checks the conflict out.
		w.as("implementer", "jj", "workspace", "update-stale")
		if conflict := w.as("implementer", "jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", "conflict"); conflict != "true" {
			w.t.Fatalf("@ after the other role's rebase holds a conflict: %s, want true", conflict)
		}
		return o
	}
	for _, tc := range []struct {
		name    string
		arrange func(w podWorkspace)
	}{
		{name: "fresh", arrange: func(podWorkspace) {}},
		{name: "fresh, operation behind", arrange: func(w podWorkspace) { newOtherRole(w).reauthor() }},
		{name: "fresh, conflicted @, operations equal", arrange: func(w podWorkspace) { conflicted(w) }},
		{name: "conflicted @, operations differ", arrange: func(w podWorkspace) { conflicted(w).reauthor() }},
		{name: "rebased onto the same tree", arrange: func(w podWorkspace) {
			// The other role's working-copy commit is empty on the same parent: the same tree.
			newOtherRole(w).rebase("second@")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPodWorkspace(t, jj)
			tc.arrange(w)
			const unrecorded = "unrecorded.txt"
			w.write(map[string]string{unrecorded: "an edit no jj command recorded\n"})
			before := w.snapshots()
			bin, log := recordingJJ(t, jj, "")

			got, shim := w.adoptWith("tester", bin, 60_000)
			if !got.OK || len(got.EditsCommits) != 0 {
				t.Fatalf("the shim answered %#v, want a successful adoption that names no commit; log:\n%s", got, shim.log)
			}
			ran := commandsRan(t, log)
			if n := countLines(ran, "workspace update-stale"); n != 0 {
				t.Errorf("jj workspace update-stale ran %d time(s) on a fresh working copy; jj ran:\n%s", n, strings.Join(ran, "\n"))
			}
			for _, command := range ran {
				if !strings.Contains(command, "--ignore-working-copy") {
					t.Errorf("the adoption ran jj %s without --ignore-working-copy", command)
				}
			}
			if after := w.snapshots(); after != before {
				t.Errorf("the adoption snapshotted a working copy: %d snapshot operations before it, %d after", before, after)
			}
			if body, err := os.ReadFile(filepath.Join(w.dir, unrecorded)); err != nil || string(body) != "an edit no jj command recorded\n" {
				t.Errorf("the edit on disk after the adoption is %q (%v), want it kept", body, err)
			}
			if warnings := strings.Count(shim.log.String(), "warning: the freshness check"); warnings != 0 {
				t.Errorf("the shim wrote %d warning line(s) of the freshness check; log:\n%s", warnings, shim.log)
			}
			status, err := w.try("tester", "jj", "status")
			if err != nil || strings.Contains(strings.ToLower(status), "stale") {
				t.Fatalf("jj status after the adoption ended %v and printed:\n%s\nwant exit 0 and no stale working copy", err, status)
			}
		})
	}
}
