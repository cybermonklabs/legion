package shim_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The two operation times an overlap's operations carry (JJ_OP_TIMESTAMP). The next jj command
// that loads the repository merges two operations that ran beside each other in the order of
// their times, and that order decides which commit of @'s change the merge leaves at @: the
// change's, whose tree the working copy did not record, or the snapshot's, which carries the
// author @ had (jj 0.46.0-sami.20261127-111037, measured on 2026-11-28).
const earlierOperation, laterOperation = "2001-02-03T04:05:06+00:00", "2001-02-03T04:05:07+00:00"

// overlapOrder is the time each of an overlap's two operations carries.
type overlapOrder struct{ snapshot, change string }

var (
	snapshotFirst = overlapOrder{snapshot: earlierOperation, change: laterOperation}
	changeFirst   = overlapOrder{snapshot: laterOperation, change: earlierOperation}
)

// overlapEdits are the edits a role leaves on disk with no jj command between them and the
// adoption: a changed tracked file and a new one.
var overlapEdits = map[string]string{"README.md": "smoke, edited by a role\n", "new.txt": "a file no jj command recorded\n"}

// overlappedChange is the case of a recording jj in which a jj command that snapshots the
// workspace overlaps the adoption's change, its metaedit or its `jj new`, as the daemon's
// workspace read could before the workspace lock and a role's background job still can. The first
// time the change runs, the real jj snapshots the workspace (`jj status`, under the previous role's
// identity), taking in the edits on disk that no jj command recorded, and then runs the change at
// the operation before that snapshot (--at-op), as a change that loaded the repository before the
// snapshot committed does: the two commit operations beside each other, each with the time order
// gives it. marker is a file of the test's own; the adoption's later rounds run the real jj alone.
func overlappedChange(marker string, order overlapOrder) string {
	return "*' metaedit '*|*' new '*)\nif [ ! -e " + shellQuoted(marker) + " ]; then\n: > " + shellQuoted(marker) + "\n" +
		"before=$(\"$REAL\" --ignore-working-copy op log -n1 --no-graph -T id) || exit 9\n" +
		"JJ_USER=implementer JJ_EMAIL=implementer@example.test JJ_OP_TIMESTAMP=" + order.snapshot + " \"$REAL\" status >/dev/null 2>&1 || exit 9\n" +
		"JJ_OP_TIMESTAMP=" + order.change + " exec \"$REAL\" --at-op \"$before\" \"$@\"\nfi ;;\n"
}

// snapshotHeldAcrossTheChange is the case of a recording jj in which a jj command that snapshots
// the workspace (`jj status`, over the many files writeMany writes) loads the repository before
// the adoption's metaedit and still holds jj's working-copy lock after it: the first time the
// metaedit runs, the snapshot starts in the background, and the metaedit runs once the snapshot
// holds the lock (lock exists), which jj takes only once it has loaded the repository. The
// snapshot writes done once it has ended. Each of the two carries the time order gives it; marker
// is a file of the test's own, and a later metaedit runs the real jj alone.
func snapshotHeldAcrossTheChange(lock, done, marker string, order overlapOrder) string {
	return "*' metaedit '*)\nif [ ! -e " + shellQuoted(marker) + " ]; then\n: > " + shellQuoted(marker) + "\n" +
		"( JJ_USER=implementer JJ_EMAIL=implementer@example.test JJ_OP_TIMESTAMP=" + order.snapshot + " \"$REAL\" status; : > " + shellQuoted(done) + " ) </dev/null >/dev/null 2>&1 &\n" +
		"while [ ! -e " + shellQuoted(lock) + " ]; do [ -e " + shellQuoted(done) + " ] && exit 9; sleep 0.01; done\n" +
		"JJ_OP_TIMESTAMP=" + order.change + " exec \"$REAL\" \"$@\"\nfi ;;\n"
}

// manyFiles is how many new files writeMany writes: enough that a snapshot of them holds jj's
// working-copy lock for seconds on a pod's volume (2,000 took 1.9 s, measured on 2026-11-28).
const manyFiles = 5000

// writeMany writes manyFiles new files under many/ in the workspace, with no jj command between.
func (w podWorkspace) writeMany() {
	w.t.Helper()
	dir := filepath.Join(w.dir, "many")
	if err := os.Mkdir(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	for i := range manyFiles {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), fmt.Appendf(nil, "file %d\n", i), 0o644); err != nil {
			w.t.Fatal(err)
		}
	}
}

// fileIn is what revision holds at name, read without a snapshot, and whether it holds the file.
func (w podWorkspace) fileIn(revision, name string) (string, bool) {
	w.t.Helper()
	out, err := w.runner("tester").Try(w.dir, w.jj, "--ignore-working-copy", "file", "show", "-r", revision, "--", name)
	return strings.TrimSpace(out), err == nil
}

// requireAdopted holds R1 and R4 of dispatch://INFRA-3916 after an adoption that answered got:
// the role's next ordinary `jj status` exits 0 and reports no stale working copy; @ is undescribed
// and authored by the adoption's identity, tester; and each of edits is in @ and on disk, or in a
// commit the answer names. log is the shim's log.
func (w podWorkspace) requireAdopted(got shimwire.AdoptWorkingCopyResult, log *syncBuffer, edits map[string]string) {
	w.t.Helper()
	w.t.Logf("the shim answered %+v; its log:\n%s", got, log)
	if !got.OK {
		w.t.Fatalf("the shim answered %#v, want a successful adoption; log:\n%s", got, log)
	}
	status, err := w.try("tester", "jj", "status")
	if err != nil || strings.Contains(strings.ToLower(status), "stale") {
		w.t.Errorf("jj status after the adoption ended %v and printed:\n%s\nwant exit 0 and no stale working copy", err, status)
	}
	if at := w.as("tester", "jj", "--ignore-working-copy", "log", "-r", "@", "--no-graph", "-T", `description ++ "|" ++ author.name() ++ " " ++ author.email()`); at != "|tester tester@example.test" {
		w.t.Errorf("@ after the adoption is %q (description|author), want an undescribed commit authored by tester tester@example.test", at)
	}
	for name, body := range edits {
		inAt, _ := w.fileIn("@", name)
		onDisk, _ := os.ReadFile(filepath.Join(w.dir, name))
		if inAt == strings.TrimSpace(body) && string(onDisk) == body {
			continue
		}
		if !slices.ContainsFunc(got.EditsCommits, func(commit string) bool {
			held, ok := w.fileIn(commit, name)
			return ok && held == strings.TrimSpace(body)
		}) {
			w.t.Errorf("the edit of %s is neither in @ and on disk (@ holds %q, the disk %q) nor in a commit the answer names (%q); log:\n%s",
				name, inAt, onDisk, got.EditsCommits, log)
		}
	}
}

// merged is whether the repository's operation log holds a merge of two operations that ran
// beside each other: the mark that an overlap was made.
func (w podWorkspace) merged() bool {
	w.t.Helper()
	ops := w.as("tester", "jj", "--ignore-working-copy", "op", "log", "--no-graph", "-T", `description ++ "\n"`, "--color=never")
	return strings.Contains(ops, "reconcile divergent operations")
}

// requireGitHeadAtParent holds that git's HEAD in the workspace, which shares its tree with git,
// names @'s parent, as the adoption leaves it (R4 of dispatch://INFRA-3916: after `jj new` the
// new working-copy commit is one whose parent git's HEAD names).
func (w podWorkspace) requireGitHeadAtParent() {
	w.t.Helper()
	head := w.as("tester", "git", "rev-parse", "HEAD")
	if parent := w.as("tester", "jj", "--ignore-working-copy", "log", "-r", "@-", "--no-graph", "-T", "commit_id"); head != parent {
		w.t.Errorf("git's HEAD after the adoption is %s, want @'s parent %s", head, parent)
	}
}

// A jj command that snapshots the workspace while the adoption runs breaks neither R1 nor R4 of
// dispatch://INFRA-3916. The adoption's commands take no working-copy lock of jj's, so a snapshot
// that loaded the repository before the adoption's change commits its operation beside the
// change's, and the next load merges the two: @'s change is divergent, and @ is the change's
// commit, whose tree the working copy did not record, or the snapshot's, which carries the
// previous role's author; on the `jj new` branch @ is the described commit again or the new
// commit on the snapshot's, with git's HEAD left on the described commit as it was before the
// snapshot. On each branch and in each merge order the adoption answers success, git's HEAD names
// @'s parent, the role's next `jj status` finds the working copy fresh, @ is undescribed and
// authored by the adoption's identity, and each edit is in @ and on disk or in a commit the answer
// names. So it is where an earlier overlap left the working copy stale with the edits already
// committed in a divergent commit of @'s change: the update has nothing to snapshot, and the
// answer names that commit (bringUpToDate).
//
// The jj is the pinned one (fixturegit.PinnedJJ), run by a recording wrapper that also makes the
// overlap, with operation times that fix the merge's order.
func TestAnAdoptionOverlappedByASnapshotOfItsWorkspaceLeavesItFreshAndLosesNoEdit(t *testing.T) {
	jj := fixturegit.PinnedJJ(t)
	overlapping := func(order overlapOrder) func(podWorkspace, string) string {
		return func(_ podWorkspace, marker string) string { return overlappedChange(marker, order) }
	}
	for _, tc := range []struct {
		name string
		// described is whether the previous role left @ described, so the adoption runs `jj new`.
		described bool
		// arrange answers the cases of the shim's recording jj, once the edits are on disk; marker
		// is a file of the test's own a case may use.
		arrange func(w podWorkspace, marker string) string
		// named is whether the answer must name the commit that holds the edits.
		named bool
	}{
		{name: "metaedit, the snapshot's operation merged first", arrange: overlapping(snapshotFirst)},
		{name: "metaedit, the change's operation merged first", arrange: overlapping(changeFirst), named: true},
		{name: "jj new, the snapshot's operation merged first", described: true, arrange: overlapping(snapshotFirst)},
		{name: "jj new, the change's operation merged first", described: true, arrange: overlapping(changeFirst)},
		{name: "the edits already in a divergent commit before the update", named: true,
			arrange: func(w podWorkspace, _ string) string {
				// Another role's metaedit loaded the repository before a snapshot and committed
				// after it, merged first: @ is that metaedit's commit, and the snapshot's commit,
				// which holds the edits, is the other commit of @'s change.
				before := w.as("tester", "jj", "--ignore-working-copy", "op", "log", "-n1", "--no-graph", "-T", "id")
				w.runner("implementer").With("JJ_OP_TIMESTAMP="+laterOperation).Run(w.dir, w.jj, "status")
				w.runner("reviewer").With("JJ_OP_TIMESTAMP="+earlierOperation).
					Run(w.dir, w.jj, "--ignore-working-copy", "--at-op", before, "metaedit", "--update-author", "-r", "@")
				if status, err := w.try("tester", "jj", "status"); err == nil || !strings.Contains(status, "The working copy is stale") {
					w.t.Fatalf("jj status after the overlap ended %v and printed:\n%s\nwant the stale working copy every ordinary command meets", err, status)
				}
				return ""
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPodWorkspace(t, jj)
			w.as("implementer", "jj", "metaedit", "--update-author")
			if tc.described {
				w.write(map[string]string{"previous.txt": "the previous role's work\n"})
				w.as("implementer", "jj", "describe", "-m", "feat: the previous role's change")
			}
			w.write(overlapEdits)
			bin, _ := recordingJJ(t, jj, tc.arrange(w, filepath.Join(t.TempDir(), "overlapped")))

			got, shim := w.adoptWith("tester", bin, 60_000)
			if !w.merged() {
				t.Fatalf("the repository's operation log holds no merge of two operations: the overlap was not made; log:\n%s", shim.log)
			}
			if got.OK {
				w.requireGitHeadAtParent()
			}
			w.requireAdopted(got, shim.log, overlapEdits)
			if tc.named && len(got.EditsCommits) == 0 {
				t.Errorf("the answer names no commit, want the one that holds the edits; log:\n%s", shim.log)
			}
			for _, commit := range got.EditsCommits {
				if shim.log.count(commit) != 1 {
					t.Errorf("the shim's log does not name %s, which the answer names:\n%s", commit, shim.log)
				}
			}
		})
	}
}

// awaitLockFile waits until the file at lock exists, which is jj's working-copy lock held,
// failing the test where ended says the jj command meant to hold it ended first. It asks every
// millisecond, since on a fast machine that command holds the lock for a fraction of a second.
func awaitLockFile(t *testing.T, lock string, ended func() bool) {
	t.Helper()
	testwait.WithinPolling(t, testwait.Remaining(t), time.Millisecond, "jj status to take its working-copy lock "+lock, func() bool {
		if _, err := os.Stat(lock); err == nil {
			return true
		}
		if ended() {
			t.Fatalf("jj status ended before the test saw its working-copy lock %s exist", lock)
		}
		return false
	})
}

// newSeparateWorkspace is a jj repository whose git store is jj's own (`jj git init
// --no-colocate`), with README.md committed under an empty @, its commands run under a home of
// the test's own. In a workspace that shares its tree with git, as an issue pod's does, every
// jj command that snapshots holds jj's git_import_export.lock across its snapshot, and a change
// run with --ignore-working-copy takes that lock too, to write git's refs, so a change that
// loaded the repository before such a snapshot ends only once the snapshot has (measured on
// 2026-11-28). Here the change takes no git lock and ends while a snapshot that loaded the
// repository before it still holds jj's working-copy lock: what a pod's workspace comes to when
// the snapshot loads the repository before the change commits and takes its locks after.
func newSeparateWorkspace(t *testing.T, jj string) podWorkspace {
	t.Helper()
	root := t.TempDir()
	w := podWorkspace{t: t, home: filepath.Join(root, "home"), dir: filepath.Join(root, "repo"), jj: jj}
	for _, dir := range []string{w.home, w.dir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w.runner("implementer").Run(w.dir, append([]string{jj}, fixturegit.JJInit(w.dir, false)[1:]...)...)
	w.write(map[string]string{"README.md": "smoke\n"})
	w.as("implementer", "jj", "commit", "-m", "the default branch")
	return w
}

// The adoption waits for a snapshot that holds jj's working-copy lock, so the snapshot commits
// first. A `jj status` over many new files holds that lock while the adoption starts, in an issue
// pod's workspace: the adoption waits before its first jj command, says so in one line of the
// shim's log, then loads the repository after the snapshot, and runs every command with
// --ignore-working-copy and no `jj workspace update-stale`. Or the snapshot loaded the repository
// before the adoption's change and still holds the lock after it (newSeparateWorkspace): the
// adoption waits before the check after its change, which then reads the snapshot's operation,
// merged with the change's in either order. Either way R1 and R4 hold, as
// TestAnAdoptionOverlappedByASnapshotOfItsWorkspaceLeavesItFreshAndLosesNoEdit holds them.
func TestTheAdoptionWaitsForASnapshotThatHoldsJJsWorkingCopyLock(t *testing.T) {
	jj := fixturegit.PinnedJJ(t)
	edits := map[string]string{"many/f1.txt": "file 1\n"}
	for name, body := range overlapEdits {
		edits[name] = body
	}
	for _, tc := range []struct {
		name string
		// atTheCheck is whether the snapshot starts at the adoption's change rather than before
		// the adoption, with order its operation's time and the change's.
		atTheCheck bool
		order      overlapOrder
		// when is where the shim's line says the adoption waited.
		when string
	}{
		{name: "held when the adoption starts", when: "before its first jj command"},
		{name: "held at the check after the change, the snapshot's operation merged first", atTheCheck: true, order: snapshotFirst,
			when: "before it checks the working copy after its change"},
		{name: "held at the check after the change, the change's operation merged first", atTheCheck: true, order: changeFirst,
			when: "before it checks the working copy after its change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w podWorkspace
			if tc.atTheCheck {
				w = newSeparateWorkspace(t, jj)
			} else {
				w = newPodWorkspace(t, jj)
			}
			w.as("implementer", "jj", "metaedit", "--update-author")
			w.write(overlapEdits)
			w.writeMany()
			lock := filepath.Join(w.dir, ".jj", "working_copy", "working_copy.lock")
			done := filepath.Join(t.TempDir(), "snapshotted")
			var cases string
			if tc.atTheCheck {
				cases = snapshotHeldAcrossTheChange(lock, done, filepath.Join(t.TempDir(), "overlapped"), tc.order)
			}
			bin, log := recordingJJ(t, jj, cases)
			p, shim := w.startShim(bin)

			var snapshot *exec.Cmd
			var output bytes.Buffer
			ended := make(chan error, 1)
			if !tc.atTheCheck {
				snapshot = exec.Command(jj, "status")
				snapshot.Dir = w.dir
				snapshot.Env = w.runner("implementer").Env
				snapshot.Stdout, snapshot.Stderr = &output, &output
				if err := snapshot.Start(); err != nil {
					t.Fatalf("start jj status: %v", err)
				}
				go func() { ended <- snapshot.Wait() }()
				awaitLockFile(t, lock, func() bool { return len(ended) > 0 })
			}
			got := w.askAdoption(p, "tester", 120_000)
			if snapshot != nil {
				if err := testwait.Receive(t, "jj status to end", ended); err != nil {
					t.Fatalf("jj status: %v\n%s", err, output.String())
				}
			} else {
				testwait.Eventually(t, "the snapshot the wrapper started to end", func() bool {
					_, err := os.Stat(done)
					return err == nil
				})
			}

			if shim.log.count("the adoption waits for jj's working-copy lock "+lock+", which a jj command that snapshots the workspace holds, "+tc.when+":") != 1 {
				t.Errorf("the shim's log does not say once that the adoption waited for jj's working-copy lock %s; log:\n%s", tc.when, shim.log)
			}
			if tc.atTheCheck {
				if !w.merged() {
					t.Fatalf("the repository's operation log holds no merge of two operations: the overlap was not made; log:\n%s", shim.log)
				}
			} else {
				for _, command := range commandsRan(t, log) {
					if !strings.Contains(command, "--ignore-working-copy") {
						t.Errorf("the adoption ran jj %s without --ignore-working-copy after the snapshot it waited for", command)
					}
				}
			}
			w.requireAdopted(got, shim.log, edits)
		})
	}
}

// The adoption runs its change at most three times in all. Where a jj command rewrites @ after each of
// its changes, here a metaedit under the previous role's identity right after each of the
// adoption's own, the check after the third change still finds @ authored by that role, and the
// adoption refuses, naming what that check found; it ran its change three times.
func TestAnAdoptionOverwrittenAfterEveryChangeRefusesNamingWhatItFound(t *testing.T) {
	jj := fixturegit.PinnedJJ(t)
	w := newPodWorkspace(t, jj)
	w.as("implementer", "jj", "metaedit", "--update-author")
	bin, log := recordingJJ(t, jj, "*' metaedit '*)\n\"$REAL\" \"$@\" || exit $?\n"+
		"JJ_USER=implementer JJ_EMAIL=implementer@example.test exec \"$REAL\" --ignore-working-copy metaedit --update-author -r @ ;;\n")

	got, shim := w.adoptWith("tester", bin, 60_000)
	if got.OK || !strings.Contains(got.Error, "ran its change 3 times") || !strings.Contains(got.Error, "found @ authored by implementer <implementer@example.test>") {
		t.Fatalf("the shim answered %#v, want a refusal after three changes naming @'s author; log:\n%s", got, shim.log)
	}
	if n := countLines(commandsRan(t, log), " metaedit "); n != 3 {
		t.Errorf("the adoption ran its metaedit %d time(s), want 3", n)
	}
}

// holdLock takes the exclusive flock of the file at path for the rest of the test, as another
// frame holds the workspace lock.
func holdLock(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock %s: %v", path, err)
	}
}

// The adoption and the workspace read each hold the workspace lock (.jj/legion-workspace.lock)
// from before their first jj command until their last one has ended, across every role's shim of
// the workspace. A lock held past the adoption's whole budget refuses the adoption before it runs
// any jj command, with a reason that names the lock and the wait, and the shim's log says once
// that it waited. A workspace read sent to another role's shim while an adoption holds the lock
// says once that it waits, and runs its first jj command, the read's one snapshot, only once the
// adoption's last command has ended.
func TestTheWorkspaceLockKeepsTheAdoptionAndTheWorkspaceReadApart(t *testing.T) {
	t.Run("the lock held past the adoption's budget", func(t *testing.T) {
		// A workspace needs no more than its .jj directory for the lock, and the adoption runs no
		// jj at all.
		w := podWorkspace{t: t, home: t.TempDir(), dir: t.TempDir()}
		if err := os.Mkdir(filepath.Join(w.dir, ".jj"), 0o755); err != nil {
			t.Fatal(err)
		}
		lock := filepath.Join(w.dir, ".jj", "legion-workspace.lock")
		holdLock(t, lock)
		bin, log := recordingJJ(t, "jj-is-never-run", "")

		got, shim := w.adoptWith("tester", bin, 1_500)
		if got.OK || !strings.Contains(got.Error, "the workspace lock "+lock+" was still held") || !strings.Contains(got.Error, "its budget of 1.5s") {
			t.Fatalf("the shim answered %#v, want a refusal naming the workspace lock %s and the wait's bound; log:\n%s", got, lock, shim.log)
		}
		if ran, err := os.ReadFile(log); !os.IsNotExist(err) {
			t.Errorf("jj ran (%v):\n%s\nwant no jj command while the workspace lock is held", err, ran)
		}
		if n := shim.log.count("the adoption waits for the workspace lock " + lock); n != 1 {
			t.Errorf("the shim's log says %d time(s) that the adoption waits for the workspace lock, want once:\n%s", n, shim.log)
		}
	})

	t.Run("a workspace read waits for an adoption that holds it", func(t *testing.T) {
		jj := fixturegit.PinnedJJ(t)
		w := newPodWorkspace(t, jj)
		release := filepath.Join(t.TempDir(), "release")
		// The adoption's metaedit waits for the test, so the adoption holds the lock meanwhile.
		bin, log := recordingJJ(t, jj, "*' metaedit '*) while [ ! -e "+shellQuoted(release)+" ]; do sleep 0.01; done ;;\n")
		adopting, _ := w.startShim(bin)
		reading, reader := w.startShim(bin)

		adopting.send(t, shimwire.AdoptWorkingCopy{ID: "adopt", JJUser: "tester", JJEmail: "tester@example.test", TimeoutMs: 60_000})
		testwait.Until(t, "the adoption to reach its metaedit", func() (string, bool) {
			data, _ := os.ReadFile(log)
			return string(data), strings.Contains(string(data), " metaedit ")
		})
		reading.send(t, shimwire.WorkspaceWork{ID: "read", Issue: "WIDGETS-42", TimeoutMs: 60_000})
		reader.log.await(t, "the workspace read waits for the workspace lock "+filepath.Join(w.dir, ".jj", "legion-workspace.lock"), 1)
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		if got, ok := adopting.next(t).(shimwire.AdoptWorkingCopyResult); !ok || !got.OK {
			t.Fatalf("the adopting shim answered %#v, want a successful adoption", got)
		}
		if got, ok := reading.next(t).(shimwire.WorkspaceWorkResult); !ok || !got.OK {
			t.Fatalf("the reading shim answered %#v, want a successful read", got)
		}
		// Every command of the adoption carries --ignore-working-copy, and the read's first, its
		// snapshot, does not: nothing but the read's second command may follow that one.
		ran := commandsRan(t, log)
		first := slices.IndexFunc(ran, func(command string) bool { return !strings.Contains(command, "--ignore-working-copy") })
		if first < 0 {
			t.Fatalf("the read's snapshot never ran; jj ran:\n%s", strings.Join(ran, "\n"))
		}
		for _, command := range ran[first+1:] {
			if !strings.Contains(command, "working_copies()") {
				t.Errorf("jj ran %q after the read's first command, want only the read's own second command; jj ran:\n%s", command, strings.Join(ran, "\n"))
			}
		}
	})
}
