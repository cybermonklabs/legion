package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/execfile"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// jjTools is testTools with a jj that first matches its arguments against arms, the arms of a sh
// `case " $* " in`, each ending `;;`, and runs every command no arm ends as the real jj.
func jjTools(t *testing.T, arms string) map[string]string {
	t.Helper()
	tools := testTools(t)
	wrapper := filepath.Join(t.TempDir(), "jj")
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" + arms + "esac\n" +
		"exec '" + tools["jj"] + "' \"$@\"\n"
	if err := execfile.Write(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tools["jj"] = wrapper
	return tools
}

// recordCheckoutMarker is the sh line that writes to record whether `.jj/legion-checkout` exists
// in the directory the command runs in: `present` or `absent`.
func recordCheckoutMarker(record string) string {
	return "  if [ -f .jj/" + checkoutMarkerFile + " ]; then printf 'present\\n' > '" + record + "'; else printf 'absent\\n' > '" + record + "'; fi\n"
}

// markerAtCheckoutTools is testTools with a jj that records at `sparse reset` whether
// `.jj/legion-checkout` exists (recordCheckoutMarker), then checks out as the real jj does.
func markerAtCheckoutTools(t *testing.T, record string) map[string]string {
	t.Helper()
	return jjTools(t, "*' sparse reset '*)\n"+recordCheckoutMarker(record)+";;\n")
}

// cutShortCheckoutTools is testTools with a jj that intercepts `sparse reset`: it records whether
// `.jj/legion-checkout` exists in record, copies the pre-staged partial directory into the
// workspace (simulating files written before a kill, including a truncated file), and exits 137.
func cutShortCheckoutTools(t *testing.T, record, partial string) map[string]string {
	t.Helper()
	return jjTools(t, "*' sparse reset '*)\n"+recordCheckoutMarker(record)+"  cp -RP '"+partial+"/.' .\n  exit 137\n;;\n")
}

// readCheckoutRecord is what the jj of markerAtCheckoutTools or cutShortCheckoutTools recorded at
// its `sparse reset`, failing the test when no `sparse reset` ran.
func readCheckoutRecord(t *testing.T, record string) string {
	t.Helper()
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("no `jj sparse reset` recorded .jj/%s: %v", checkoutMarkerFile, err)
	}
	return strings.TrimSpace(string(recorded))
}

// pushTreeFiles commits every path->content entry in files onto branch on the bare remote.
func pushTreeFiles(t *testing.T, remote, branch string, files map[string]string) {
	t.Helper()
	scratch := t.TempDir()
	runSetup(t, scratch, "git", "clone", "--quiet", remote, "other")
	other := filepath.Join(scratch, "other")
	runSetup(t, other, "git", "checkout", "--quiet", branch)
	for rel, body := range files {
		full := filepath.Join(other, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runSetup(t, other, "git", "add", ".")
	runSetup(t, other, "git", "commit", "--quiet", "-m", "fixture files")
	runSetup(t, other, "git", "push", "--quiet", "origin", branch)
}

// R3-3: a tmux-runtime provision (FromGitHub) whose first checkout was cut short after writing
// half of the tree and half of one file's bytes clears the partial working copy and redoes the
// checkout whole on the next provision.
func TestProvisionRedoesACheckoutThatWasCutShort(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	tree := map[string]string{
		"alpha.txt":        "alpha content\n",
		"nested/beta.txt":  "beta content\n",
		"nested/gamma.txt": "gamma content\n",
		"half.txt":         "first half of the file\nsecond half of the file\n",
	}
	pushTreeFiles(t, run.remote, "main", tree)

	partial := t.TempDir()
	for rel, body := range map[string]string{
		"alpha.txt":       tree["alpha.txt"],
		"nested/beta.txt": tree["nested/beta.txt"],
		"half.txt":        "first half of the file\n",
	} {
		full := filepath.Join(partial, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	record := filepath.Join(t.TempDir(), "marker.txt")
	realTools := run.tools
	run.tools = cutShortCheckoutTools(t, record, partial)

	req := provisionRequest(t)
	_, err := Provision(context.Background(), run, req)
	run.tools = realTools
	if err == nil {
		t.Fatal("first Provision succeeded despite cut-short checkout stand-in")
	}
	if got := readCheckoutRecord(t, record); got != "present" {
		t.Errorf("the checkout stand-in recorded .jj/legion-checkout as %q, want present", got)
	}

	// Second provision with the real jj must clear the half-written working copy and check out whole.
	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	requireCheckedOut(t, working)
	for rel, want := range tree {
		got, err := os.ReadFile(filepath.Join(working.Dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s after second Provision: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	if diff := strings.TrimSpace(runSetup(t, working.Dir, "jj", "diff", "--summary", "-R", working.Dir)); diff != "" {
		t.Errorf("jj diff --summary = %q, want empty", diff)
	}
	if _, err := os.Lstat(filepath.Join(working.Dir, ".jj", checkoutMarkerFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after a finished checkout, .jj/legion-checkout lstat = %v, want ErrNotExist", err)
	}
}

// markedCheckoutTree is the tree the marked-workspace tests push to main; half.txt is the file
// whose first half a cut-short checkout leaves behind (requireCutShortCheckoutRedone).
var markedCheckoutTree = map[string]string{"alpha.txt": "alpha content\n", "half.txt": "first half\nsecond half\n"}

// requireCutShortCheckoutRedone provisions req's workspace, marked and holding no sparse pattern,
// twice more on run. The first provision's checkout writes half of half.txt and is cut short
// (cutShortCheckoutTools), and must find the marker there. The second, with the real jj, must
// clear what that checkout wrote (checkout-clear) and check out markedCheckoutTree whole, leaving
// no marker.
func requireCutShortCheckoutRedone(t *testing.T, run *recordingRunner, req Request) {
	t.Helper()
	partial := t.TempDir()
	if err := os.WriteFile(filepath.Join(partial, "half.txt"), []byte("first half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "marker.txt")
	realTools := run.tools
	run.tools = cutShortCheckoutTools(t, record, partial)
	_, err := Provision(context.Background(), run, req)
	run.tools = realTools
	if err == nil {
		t.Fatal("the Provision whose checkout was cut short succeeded")
	}
	if got := readCheckoutRecord(t, record); got != "present" {
		t.Fatalf("the cut-short checkout recorded .jj/%s as %q, want present", checkoutMarkerFile, got)
	}

	var steps []string
	again := req
	again.Steps = NewSteps(newTestClock(), func(line string) { steps = append(steps, line) })
	working, err := Provision(context.Background(), run, again)
	if err != nil {
		t.Fatalf("the Provision after the cut-short checkout: %v", err)
	}
	if !slices.ContainsFunc(steps, func(line string) bool { return strings.HasPrefix(line, "step checkout-clear ") }) {
		t.Errorf("the provision after the cut-short checkout logged %q, want its checkout-clear", steps)
	}
	requireCheckedOut(t, working)
	for rel, want := range markedCheckoutTree {
		if got, err := os.ReadFile(filepath.Join(working.Dir, rel)); err != nil || string(got) != want {
			t.Errorf("%s after the provision that redid the checkout = %q (%v), want %q", rel, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(working.Dir, ".jj", checkoutMarkerFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after a finished checkout, .jj/%s lstat = %v, want ErrNotExist", checkoutMarkerFile, err)
	}
}

// A freshly registered workspace is marked as soon as it is registered, before the provision's
// configuration: a provision stopped between the registration and the checkout (here, its wait for
// a checkout's place ends) leaves the workspace marked, so the next provision keeps the marker
// through a checkout that is then cut short, and the one after that clears what the cut-short
// checkout wrote (checkout-clear) before it checks out the whole tree.
func TestProvisionMarksAWorkspaceAsSoonAsItIsRegistered(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
	req := provisionRequest(t)
	location, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(location.Dir, ".jj", checkoutMarkerFile)

	// The step clock logs the registration's line as the configuration begins.
	var markedAtConfiguration bool
	stopped := req
	stopped.Steps = NewSteps(newTestClock(), func(line string) {
		if strings.HasPrefix(line, "step workspace-registration ") {
			_, err := os.Lstat(marker)
			markedAtConfiguration = err == nil
		}
	})
	stopped.CheckoutSlot = func(context.Context) (func(), error) { return nil, context.Canceled }
	if _, err := Provision(context.Background(), run, stopped); !errors.Is(err, context.Canceled) {
		t.Fatalf("Provision whose wait for a checkout's place ended = %v, want context.Canceled", err)
	}
	if !markedAtConfiguration {
		t.Errorf("the freshly registered workspace held no .jj/%s as its configuration began", checkoutMarkerFile)
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("after a provision stopped before its checkout, .jj/%s lstat = %v, want the marker there", checkoutMarkerFile, err)
	}
	requireCutShortCheckoutRedone(t, run, req)
}

// A provision that fails after its `jj workspace add` — here at the registration's last step, the
// `jj bookmark set` that puts the bookmark of an issue whose branch GitHub does not have on the new
// workspace — leaves the workspace registered with no file, and marked. So the next provision
// keeps the marker through a checkout that is then cut short, and the one after that clears what
// the cut-short checkout wrote (checkout-clear) before it checks out the whole tree.
func TestProvisionMarksAWorkspaceWhoseRegistrationFailedAfterItsAdd(t *testing.T) {
	t.Parallel()
	run, req, location := markedWithNoBookmark(t)
	if listed := strings.TrimSpace(runSetup(t, location.Dir, "jj", "sparse", "list", "--ignore-working-copy", "-R", location.Dir)); listed != "" {
		t.Fatalf("the workspace whose bookmark failed holds the sparse patterns %q, want none", listed)
	}
	if _, err := os.Lstat(filepath.Join(location.Dir, ".jj", checkoutMarkerFile)); err != nil {
		t.Fatalf("after a provision that failed right after its jj workspace add, .jj/%s lstat = %v, want the marker there", checkoutMarkerFile, err)
	}
	requireCutShortCheckoutRedone(t, run, req)
}

// A provision that fails at the registration's last step, the `jj bookmark set` that puts the
// bookmark of an issue whose branch GitHub does not have on the new workspace, leaves the workspace
// registered and marked with no bookmark. The provision after it reports the workspace on that
// bookmark (Workspace.Bookmark, workspace-init's last line), so it is there: on the workspace's
// working-copy commit, where a first provision that did not fail puts it.
func TestProvisionSetsTheBookmarkOfAWorkspaceWhoseRegistrationFailedAfterItsAdd(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
	req := provisionRequest(t)

	realTools := run.tools
	run.tools = jjTools(t, "*' bookmark set '*) echo 'the test fails jj bookmark set' >&2; exit 1 ;;\n")
	_, err := Provision(context.Background(), run, req)
	run.tools = realTools
	if err == nil || !strings.Contains(err.Error(), "bookmark set") {
		t.Fatalf("Provision whose jj bookmark set failed = %v, want that command's failure", err)
	}

	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the Provision after the failed jj bookmark set: %v", err)
	}
	requireCheckedOut(t, working)
	at := commitOf(t, working.Dir, "@")
	if got := commitOf(t, working.Dir, `bookmarks(exact:"`+working.Bookmark+`")`); got != at {
		t.Errorf("after the provision that followed a failed jj bookmark set, bookmark %s is on %q, want the workspace's working-copy commit %s", working.Bookmark, got, at)
	}
}

// failBookmarkSet provisions req's workspace with a jj whose `bookmark set` fails, the
// registration's last step for an issue whose branch GitHub does not have, so the provision stops
// right after the workspace is added, marked and its creation recorded.
func failBookmarkSet(t *testing.T, run *recordingRunner, req Request) {
	t.Helper()
	realTools := run.tools
	run.tools = jjTools(t, "*' bookmark set '*) echo 'the test fails jj bookmark set' >&2; exit 1 ;;\n")
	_, err := Provision(context.Background(), run, req)
	run.tools = realTools
	if err == nil || !strings.Contains(err.Error(), "bookmark set") {
		t.Fatalf("Provision whose jj bookmark set failed = %v, want that command's failure", err)
	}
}

// markedWithNoBookmark provisions an issue whose branch GitHub does not have, on a fresh remote
// whose main holds markedCheckoutTree, with a jj whose `bookmark set` fails (failBookmarkSet). It
// returns the runner, the request and the workspace that provision left registered and marked,
// added at main with no bookmark, its default branch named.
func markedWithNoBookmark(t *testing.T) (*recordingRunner, Request, Workspace) {
	t.Helper()
	run := newLocalRunner(t)
	pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
	req := provisionRequest(t)
	location, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	location.DefaultBranch = req.DefaultBranch
	failBookmarkSet(t, run, req)
	return run, req, location
}

// requireNothingFinished fails the test when a call of run after the first before set or tracked
// a bookmark or checked out, or when location no longer carries its checkout marker: what a
// refused retry of a marked workspace leaves.
func requireNothingFinished(t *testing.T, run *recordingRunner, before int, location Workspace) {
	t.Helper()
	for _, call := range run.Calls()[before:] {
		if commandWith(call.Argv, "jj", "bookmark", "set") || commandWith(call.Argv, "jj", "bookmark", "track") || commandWith(call.Argv, "jj", "sparse", "reset") {
			t.Errorf("the refused provision ran %q", call.Argv)
		}
	}
	if _, err := os.Lstat(filepath.Join(location.Dir, ".jj", checkoutMarkerFile)); err != nil {
		t.Errorf("after the refusal, .jj/%s lstat = %v, want the marker there", checkoutMarkerFile, err)
	}
}

// An issue whose branch GitHub has, here made at the default branch's head as the daemon makes it,
// starts its workspace on top of the branch, so the bookmark is on the workspace's @-. A provision
// whose first checkout is cut short leaves that workspace marked, and the provision that redoes
// the checkout finishes its registration without moving the bookmark onto @.
func TestProvisionKeepsTheBookmarkOfABranchWhoseFirstCheckoutWasCutShort(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
	req := provisionRequest(t)
	location, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(runSetup(t, run.remote, "git", "--git-dir="+run.remote, "rev-parse", "refs/heads/main"))
	runSetup(t, run.remote, "git", "--git-dir="+run.remote, "update-ref", "refs/heads/"+location.Bookmark, head)

	requireCutShortCheckoutRedone(t, run, req)
	bookmark := commitOf(t, location.Dir, `bookmarks(exact:"`+location.Bookmark+`")`)
	if parent := commitOf(t, location.Dir, "@-"); bookmark != head || parent != head {
		t.Errorf("after the provision that redid the cut-short checkout, bookmark %s is on %q and @- is %q, want both on origin's branch %s", location.Bookmark, bookmark, parent, head)
	}
}

// A provision stopped right after it marked a freshly added workspace (checkoutMarkerFile), before
// its creation record and its bookmark, leaves neither: here its jj bookmark set fails, and the
// creation record it wrote first is removed. The provision after it writes both: Created answers
// that provision's moment, and the bookmark is on the workspace's working-copy commit, where a
// first provision that did not stop puts it.
func TestProvisionRecordsTheCreationOfAWorkspaceWhoseRegistrationStoppedAtItsMarker(t *testing.T) {
	t.Parallel()
	run, req, location := markedWithNoBookmark(t)
	if err := os.Remove(filepath.Join(location.Dir, ".jj", createdFile)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Created(location.Dir); err != nil || ok {
		t.Fatalf("Created of the workspace whose record was removed = %v, %v; want no record", ok, err)
	}

	retried := time.Now()
	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the Provision after the registration stopped at its marker: %v", err)
	}
	requireRegistrationFinished(t, working, retried, "the provision that finished the registration")
}

// requireRegistrationFinished fails the test when working, which a provision that finished its
// registration answered, is not checked out whole, holds no creation record from retried on, or
// has its bookmark off its working-copy commit (requireBookmarkOnWorkingCopy). after names that
// provision.
func requireRegistrationFinished(t *testing.T, working Workspace, retried time.Time, after string) {
	t.Helper()
	requireCheckedOut(t, working)
	if at, ok, err := Created(working.Dir); err != nil || !ok || at.Before(retried) {
		t.Errorf("Created after %s = %s, %v, %v; want a record no earlier than %s", after, at, ok, err, retried)
	}
	requireBookmarkOnWorkingCopy(t, working, after)
}

// requireBookmarkOnWorkingCopy fails the test when working's bookmark is not on its working-copy
// commit, where a first provision of an issue whose branch GitHub does not have puts it. after
// names the provision that left it.
func requireBookmarkOnWorkingCopy(t *testing.T, working Workspace, after string) {
	t.Helper()
	at := commitOf(t, working.Dir, "@")
	if got := commitOf(t, working.Dir, "bookmarks("+exactName(working.Bookmark)+")"); got != at {
		t.Errorf("after %s, bookmark %s is on %q, want the workspace's working-copy commit %s", after, working.Bookmark, got, at)
	}
}

// A provision killed between RecordCreated's truncation of the creation record and its write
// leaves the record empty, and a stray write can leave one that holds no instant. Created refuses
// either, so the retry of the workspace, marked since its registration stopped (here at its jj
// bookmark set), replaces the record with its own moment and logs that it did, naming the refusal.
// It then finishes the registration and the checkout: Created answers that provision's moment, and
// the bookmark is on the workspace's working-copy commit.
func TestProvisionReplacesTheUnreadableCreationRecordOfAMarkedWorkspace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, record string }{
		{"an empty record", ""},
		{"a record that holds no instant", "not an instant\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run, req, location := markedWithNoBookmark(t)
			record := filepath.Join(location.Dir, ".jj", createdFile)
			if err := os.WriteFile(record, []byte(tc.record), 0o644); err != nil {
				t.Fatal(err)
			}
			refusal := fmt.Sprintf("the creation record %s holds %q, not an RFC 3339 instant", record, tc.record)
			if _, _, err := Created(location.Dir); err == nil || err.Error() != refusal {
				t.Fatalf("Created of the workspace whose record holds %q = %v, want %q", tc.record, err, refusal)
			}

			var logged []string
			retry := req
			retry.Log = func(line string) { logged = append(logged, line) }
			retried := time.Now()
			working, err := Provision(context.Background(), run, retry)
			if err != nil {
				t.Fatalf("the Provision of the marked workspace whose record holds %q: %v", tc.record, err)
			}
			replaced := "Workspace " + location.Dir + ": replaced the creation record it could not read with this provisioning's moment: " + refusal
			if !slices.Contains(logged, replaced) {
				t.Errorf("the Provision that replaced the record logged %q, want %q", logged, replaced)
			}
			requireRegistrationFinished(t, working, retried, "the provision that replaced the record")
		})
	}
}

// A FIFO at a marked workspace's creation record, which a role of the issue's volume could plant
// and hold open for reading, is refused rather than waited on or written to: Created refuses it as
// no regular file, and so does its replacement (RecordCreated, through rolefile.OpenRegular), so
// the retry fails at once, with the store lock held, naming the record and why it was not
// replaced, and keeps the marker and the FIFO, having set, tracked and checked out nothing and
// written nothing into the FIFO. The retry's answer is waited for with no span of its own
// (testwait.Receive), so a retry that waits on the FIFO fails by name before the test binary's
// deadline, and a slow provision passes.
func TestProvisionRefusesAMarkedWorkspaceWhoseCreationRecordIsAFIFO(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reader bool
	}{
		{"with no reader", false},
		{"with a reader holding it open", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run, req, location := markedWithNoBookmark(t)
			record := filepath.Join(location.Dir, ".jj", createdFile)
			if err := os.Remove(record); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(record, 0o644); err != nil {
				t.Fatal(err)
			}
			reader := -1
			if tc.reader {
				// O_NONBLOCK, since a read-only open of a FIFO otherwise waits for a writer.
				fd, err := unix.Open(record, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = unix.Close(fd) })
				reader = fd
			}

			before := len(run.Calls())
			done := make(chan error, 1)
			go func() {
				_, err := Provision(context.Background(), run, req)
				done <- err
			}()
			err := testwait.Receive(t, "the Provision of a marked workspace whose creation record is a FIFO "+tc.name, done)
			want := "the creation record " + record + " is not a regular file (p---------), and it could not be replaced: record the creation of workspace " + location.Dir + ": " + record + " is a fifo"
			if err == nil || err.Error() != want {
				t.Fatalf("the Provision of a marked workspace whose creation record is a FIFO %s = %v\nwant %q", tc.name, err, want)
			}
			requireNothingFinished(t, run, before, location)
			if info, err := os.Lstat(record); err != nil || info.Mode().Type() != fs.ModeNamedPipe {
				t.Errorf("after the refusal, the creation record lstat = %v, %v; want the FIFO there", info, err)
			}
			if tc.reader {
				written := make([]byte, createdRecordLimit)
				if n, err := unix.Read(reader, written); err == nil && n > 0 {
					t.Errorf("the refused Provision wrote %q into the FIFO at the creation record", written[:n])
				}
			}
		})
	}
}

// A provision that fails at the `jj bookmark set` of an issue whose branch GitHub does not have
// leaves the workspace marked, added at the default branch with no bookmark. When GitHub has the
// issue's branch by the next provision, that provision refuses the workspace by name, with the
// bookmark and origin's commit, before it sets, tracks or checks out anything, rather than
// checking it out with no bookmark where origin's branch is not.
func TestProvisionRefusesAWorkspaceWhoseBranchGitHubMadeAfterItsRegistrationFailed(t *testing.T) {
	t.Parallel()
	run, req, location := markedWithNoBookmark(t)
	branch := pushRemoteBranch(t, run.remote, location.Bookmark, "fixture.txt")

	before := len(run.Calls())
	_, err := Provision(context.Background(), run, req)
	want := "Bookmark " + location.Bookmark + " is not in the shared clone " + location.Clone + " while " + location.Bookmark + "@origin is at " + branch + ", and workspace " + location.Dir + " holds no checkout"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the Provision after GitHub made the branch = %v, want a refusal containing %q", err, want)
	}
	requireNothingFinished(t, run, before, location)
}

// A marked workspace, added at the default branch with no bookmark after its jj bookmark set
// failed, whose origin row concurrent fetches then conflicted, with no local bookmark (GitHub made
// the branch, one fetch saw it move and another saw it deleted), is refused as createWorkspace
// refuses that row, with its way out, before anything is set, tracked or checked out. The way out
// holds: the next provisioning's fetch sets the row to origin's branch as it is then, here none,
// and that provisioning finishes the registration. Provision's own fetch settles the row first,
// so the retry's check (checkExistingCheckout) is called directly.
func TestProvisionRefusesAMarkedWorkspaceWhoseOriginRowConcurrentFetchesConflicted(t *testing.T) {
	t.Parallel()
	run, req, location := markedWithNoBookmark(t)
	clone := location.Clone
	listed := pushRemoteBranch(t, run.remote, location.Bookmark, "fixture.txt")
	moved := conflictOriginRowWithDeletion(t, run, clone, location.Bookmark)

	before := len(run.Calls())
	_, _, err := checkExistingCheckout(context.Background(), run, location, req.Log)
	want := "Remote bookmark " + location.Bookmark + "@origin is conflicted (adds " + moved + "; removes " + listed + "), one side a deletion, which concurrent fetches leave; workspace " + location.Dir + " holds no checkout and was not checked out. Provision again: the next provisioning's fetch sets the row to origin's branch as it is then"
	if err == nil || err.Error() != want {
		t.Fatalf("the retry of the marked workspace on a conflicted origin row = %v\nwant %q", err, want)
	}
	requireNothingFinished(t, run, before, location)

	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	requireCheckedOut(t, working)
	requireBookmarkOnWorkingCopy(t, working, "provisioning again")
}

// A marked workspace, added at the default branch with no bookmark after its jj bookmark set
// failed, whose bookmark GitHub then had, tracked in the shared clone and deleted there with the
// deletion never pushed, is refused as createWorkspace refuses that deletion, before anything is
// set, tracked or checked out. Each of the three ways out, run exactly as the refusal writes it,
// provisions: restoring the bookmark, and cancelling the deletion, which leaves the row untracked,
// each start at origin's branch once the directory is deleted too, as the refusal says for this
// workspace added without the branch; and deleting the branch on GitHub starts at main, with the
// bookmark on the working-copy commit.
func TestProvisionRefusesAMarkedWorkspaceWhoseBookmarkWasDeletedInTheSharedClone(t *testing.T) {
	t.Parallel()
	for _, way := range []struct {
		name, command string
		deleteDir     bool
	}{
		{"restore", "jj bookmark set", true},
		{"cancel the deletion", "jj bookmark forget", true},
		{"delete the branch on GitHub", githubDelete, false},
	} {
		t.Run(way.name, func(t *testing.T) {
			t.Parallel()
			run, req, location := markedWithNoBookmark(t)
			clone := location.Clone
			pushed := pushRemoteBranch(t, run.remote, location.Bookmark, "fixture.txt")
			fromOrigin(t, run, clone, "jj", "git", "fetch", "-R", clone)
			runSetup(t, clone, "jj", "bookmark", "track", location.Bookmark+"@origin", "-R", clone)
			runSetup(t, clone, "jj", "bookmark", "delete", location.Bookmark, "-R", clone)

			before := len(run.Calls())
			_, err := Provision(context.Background(), run, req)
			want := "Bookmark " + location.Bookmark + " was deleted in the shared clone " + clone + " and the deletion never pushed, while " + location.Bookmark + "@origin is tracked at " + pushed + "; workspace " + location.Dir + " holds no checkout and was not checked out. " +
				"Restore it, then delete its directory, which holds no work: " + wayOut(clone, "bookmark", "set", jjString(location.Bookmark), "-r", originRow(location.Bookmark)) + ". " +
				"Cancel the deletion, then delete its directory, which holds no work, and the next provisioning adopts origin's branch: " + wayOut(clone, "bookmark", "forget", exactName(location.Bookmark)) + ". " +
				"Start from main instead: delete the branch on GitHub (the pull request's Delete branch button, or " + deleteOnGitHub(location) + "), and the next provisioning starts at main"
			if err == nil || err.Error() != want {
				t.Fatalf("the retry of the marked workspace whose bookmark was deleted = %v\nwant %q", err, want)
			}
			requireNothingFinished(t, run, before, location)

			command := lostWorkspace{run: run, req: req, clone: clone}.runWayOut(t, err.Error(), way.command, "")
			if way.deleteDir {
				if err := os.RemoveAll(location.Dir); err != nil {
					t.Fatal(err)
				}
			}
			working, err := Provision(context.Background(), run, req)
			if err != nil {
				t.Fatalf("provision after `%s`: %v", command, err)
			}
			bookmark, parent := commitOf(t, working.Dir, "bookmarks("+exactName(working.Bookmark)+")"), commitOf(t, working.Dir, "@-")
			if way.deleteDir {
				if parent != pushed || bookmark != pushed {
					t.Errorf("after `%s` and the directory's deletion, @- is %s and bookmark %s is on %s, want both on origin's branch %s", command, parent, working.Bookmark, bookmark, pushed)
				}
			} else if main := commitOf(t, clone, "main@origin"); parent != main || bookmark != commitOf(t, working.Dir, "@") {
				t.Errorf("after `%s`, @- is %s and bookmark %s is on %s, want @- on main's %s and the bookmark on the working-copy commit", command, parent, working.Bookmark, bookmark, main)
			}
		})
	}
}

// R3-4: a workspace that Legion finished checking out leaves no `.jj/legion-checkout`. When an
// agent later clears its sparse patterns and leaves an untracked file, the next provision writes
// no marker and keeps the untracked file.
func TestProvisionKeepsTheFilesOfAWorkspaceItDidNotCheckOut(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	pushRemoteBranch(t, run.remote, "main", "tracked.txt")
	req := provisionRequest(t)

	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(working.Dir, ".jj", checkoutMarkerFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after a finished checkout, .jj/legion-checkout lstat = %v, want ErrNotExist", err)
	}

	// An agent clears sparse patterns and writes an untracked file in the workspace.
	runSetup(t, working.Dir, "jj", "sparse", "set", "--clear", "-R", working.Dir)
	untracked := filepath.Join(working.Dir, "notes.txt")
	const wantNotes = "agent notes\n"
	if err := os.WriteFile(untracked, []byte(wantNotes), 0o644); err != nil {
		t.Fatal(err)
	}

	record := filepath.Join(t.TempDir(), "marker.txt")
	realTools := run.tools
	run.tools = markerAtCheckoutTools(t, record)
	_, err = Provision(context.Background(), run, req)
	run.tools = realTools
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if got := readCheckoutRecord(t, record); got != "absent" {
		t.Errorf("second Provision's checkout found .jj/%s %s, want absent: no marker for a workspace whose patterns an agent cleared", checkoutMarkerFile, got)
	}
	got, err := os.ReadFile(untracked)
	if err != nil {
		t.Fatalf("untracked file was removed: %v", err)
	}
	if string(got) != wantNotes {
		t.Errorf("untracked file = %q, want %q", got, wantNotes)
	}
	if listed := strings.TrimSpace(runSetup(t, working.Dir, "jj", "sparse", "list", "-R", working.Dir)); listed != "." {
		t.Errorf("jj sparse list = %q, want .", listed)
	}
	if _, err := os.Stat(filepath.Join(working.Dir, "tracked.txt")); err != nil {
		t.Errorf("tracked.txt missing after second Provision: %v", err)
	}
}

// R3-5: a checked-out workspace with a planted `.jj/legion-checkout` (left by a crash between the
// checkout's end and the marker's removal) has that marker removed on its next provision, so a
// later agent sparse-clear and untracked file are kept across the provision after that.
func TestProvisionRemovesTheMarkerOfAFinishedCheckout(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	pushRemoteBranch(t, run.remote, "main", "tracked.txt")
	req := provisionRequest(t)

	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("first Provision: %v", err)
	}
	marker := filepath.Join(working.Dir, ".jj", checkoutMarkerFile)
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Provisioning the already-checked-out workspace must remove the leftover marker.
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after provisioning a checked-out workspace, .jj/legion-checkout lstat = %v, want ErrNotExist", err)
	}

	// An agent clears sparse patterns and writes an untracked file; the next provision keeps it.
	runSetup(t, working.Dir, "jj", "sparse", "set", "--clear", "-R", working.Dir)
	untracked := filepath.Join(working.Dir, "notes.txt")
	const wantNotes = "keep me\n"
	if err := os.WriteFile(untracked, []byte(wantNotes), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("third Provision: %v", err)
	}
	got, err := os.ReadFile(untracked)
	if err != nil {
		t.Fatalf("untracked file was removed by the third Provision: %v", err)
	}
	if string(got) != wantNotes {
		t.Errorf("untracked file = %q, want %q", got, wantNotes)
	}
}

// clearWorkingCopy keeps `.jj` and `.git`, removes every other file, nested directory and link,
// follows no symlink outside the workspace directory, and refuses a symlinked workspace directory.
// The checkout marker helpers refuse a symlinked `.jj`.
func TestProvisionClearWorkingCopyFollowsNoLinkAndKeepsMetadata(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "precious.txt")
	if err := os.WriteFile(outsideFile, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".jj", checkoutMarkerFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".git"), []byte("gitdir: /example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "nested", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "nested", "dir", "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(ws, "nested", "escape-file")); err != nil {
		t.Fatal(err)
	}

	if err := clearWorkingCopy(ws); err != nil {
		t.Fatalf("clearWorkingCopy: %v", err)
	}
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != ".git,.jj" {
		t.Errorf("remaining entries = %q, want .git,.jj", names)
	}
	if _, err := os.Stat(filepath.Join(ws, ".jj", checkoutMarkerFile)); err != nil {
		t.Errorf("clearWorkingCopy removed .jj/%s: %v", checkoutMarkerFile, err)
	}
	if body, err := os.ReadFile(outsideFile); err != nil || string(body) != "keep\n" {
		t.Errorf("outside file changed: %q, %v", body, err)
	}

	// A symlinked workspace directory is refused without touching its target.
	linkedWS := filepath.Join(t.TempDir(), "linked-ws")
	if err := os.Symlink(outside, linkedWS); err != nil {
		t.Fatal(err)
	}
	if err := clearWorkingCopy(linkedWS); err == nil {
		t.Error("clearWorkingCopy over a symlinked workspace directory succeeded, want error")
	}
	if body, err := os.ReadFile(outsideFile); err != nil || string(body) != "keep\n" {
		t.Errorf("outside file changed through symlinked workspace: %q, %v", body, err)
	}

	// A symlinked .jj is refused by write, read and remove of the marker.
	badWS := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(badWS, ".jj")); err != nil {
		t.Fatal(err)
	}
	if err := writeCheckoutMarker(badWS); !errors.Is(err, errLinkedJJ) {
		t.Errorf("writeCheckoutMarker over linked .jj = %v, want errLinkedJJ", err)
	}
	if _, err := hasCheckoutMarker(badWS); !errors.Is(err, errLinkedJJ) {
		t.Errorf("hasCheckoutMarker over linked .jj = %v, want errLinkedJJ", err)
	}
	if err := removeCheckoutMarker(badWS); !errors.Is(err, errLinkedJJ) {
		t.Errorf("removeCheckoutMarker over linked .jj = %v, want errLinkedJJ", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, checkoutMarkerFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker was written through linked .jj into %s (%v)", outside, err)
	}
}
