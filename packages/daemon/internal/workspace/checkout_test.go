package workspace

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/execfile"
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
	run := newLocalRunner(t)
	pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
	req := provisionRequest(t)
	location, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}

	realTools := run.tools
	run.tools = jjTools(t, "*' bookmark set '*) echo 'the test fails jj bookmark set' >&2; exit 1 ;;\n")
	_, err = Provision(context.Background(), run, req)
	run.tools = realTools
	if err == nil || !strings.Contains(err.Error(), "bookmark set") {
		t.Fatalf("Provision whose jj bookmark set failed = %v, want that command's failure", err)
	}
	if listed := strings.TrimSpace(runSetup(t, location.Dir, "jj", "sparse", "list", "--ignore-working-copy", "-R", location.Dir)); listed != "" {
		t.Fatalf("the workspace whose bookmark failed holds the sparse patterns %q, want none", listed)
	}
	if _, err := os.Lstat(filepath.Join(location.Dir, ".jj", checkoutMarkerFile)); err != nil {
		t.Fatalf("after a provision that failed right after its jj workspace add, .jj/%s lstat = %v, want the marker there", checkoutMarkerFile, err)
	}
	requireCutShortCheckoutRedone(t, run, req)
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
