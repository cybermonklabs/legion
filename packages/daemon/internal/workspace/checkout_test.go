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
	"time"

	"github.com/sjawhar/legion/daemon/internal/execfile"
)

// cutShortCheckoutTools is testTools with a jj that intercepts `sparse reset`: it records whether
// `.jj/legion-checkout` exists in record, copies the pre-staged partial directory into the
// workspace (simulating files written before a kill, including a truncated file), and exits 137.
func cutShortCheckoutTools(t *testing.T, record, partial string) map[string]string {
	t.Helper()
	tools := testTools(t)
	wrapper := filepath.Join(t.TempDir(), "jj")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *' sparse reset '*)\n" +
		"  if [ -f .jj/" + checkoutMarkerFile + " ]; then printf 'present\\n' > '" + record + "'; else printf 'absent\\n' > '" + record + "'; fi\n" +
		"  cp -RP '" + partial + "/.' .\n" +
		"  exit 137\n" +
		";; esac\n" +
		"exec '" + tools["jj"] + "' \"$@\"\n"
	if err := execfile.Write(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	tools["jj"] = wrapper
	return tools
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

// checkoutObserverRunner wraps a Runner and records whether `.jj/legion-checkout` exists in the
// workspace's `.jj` directory at the moment `jj sparse reset` starts.
type checkoutObserverRunner struct {
	inner       Runner
	sawCheckout bool
	sawMarker   bool
	// err is the marker's lstat error at that moment, other than its absence.
	err error
}

func (r *checkoutObserverRunner) Timeout() time.Duration { return r.inner.Timeout() }

func (r *checkoutObserverRunner) Run(ctx context.Context, command Command) (Result, error) {
	if isCheckout(command.Argv) {
		r.sawCheckout = true
		switch _, err := os.Lstat(filepath.Join(command.Dir, ".jj", checkoutMarkerFile)); {
		case err == nil:
			r.sawMarker = true
		case !errors.Is(err, fs.ErrNotExist):
			r.err = err
		}
	}
	return r.inner.Run(ctx, command)
}

// checkoutRunner routes `jj sparse reset` to checkout and every other command to inner, so a test
// can swap the checkout binary without touching recordingRunner's private fields.
type checkoutRunner struct {
	inner    Runner
	checkout Runner
}

func (r *checkoutRunner) Timeout() time.Duration { return r.inner.Timeout() }

func (r *checkoutRunner) Run(ctx context.Context, command Command) (Result, error) {
	if isCheckout(command.Argv) {
		return r.checkout.Run(ctx, command)
	}
	return r.inner.Run(ctx, command)
}

// R3-3: a tmux-runtime provision (FromGitHub) whose first checkout was cut short after writing
// half of the tree and half of one file's bytes clears the partial working copy and redoes the
// checkout whole on the next provision.
func TestProvisionRedoesACheckoutThatWasCutShort(t *testing.T) {
	// Not parallel: newLocalRunner calls t.Setenv.
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
	first := &checkoutRunner{inner: run, checkout: NewRunner(testTimeout, cutShortCheckoutTools(t, record, partial))}

	req := provisionRequest(t)
	if _, err := Provision(context.Background(), first, req); err == nil {
		t.Fatal("first Provision succeeded despite cut-short checkout stand-in")
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read marker record: %v", err)
	}
	if got := strings.TrimSpace(string(recorded)); got != "present" {
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

// A freshly registered workspace is marked as soon as it is registered, before the provision's
// configuration: a provision stopped between the registration and the checkout (here, its wait for
// a checkout's place ends) leaves the workspace marked, so the next provision keeps the marker
// through a checkout that is then cut short, and the one after that clears what the cut-short
// checkout wrote (checkout-clear) before it checks out the whole tree.
func TestProvisionMarksAWorkspaceAsSoonAsItIsRegistered(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	tree := map[string]string{"alpha.txt": "alpha content\n", "half.txt": "first half\nsecond half\n"}
	pushTreeFiles(t, run.remote, "main", tree)
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

	partial := t.TempDir()
	if err := os.WriteFile(filepath.Join(partial, "half.txt"), []byte("first half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "marker.txt")
	cut := &checkoutRunner{inner: run, checkout: NewRunner(testTimeout, cutShortCheckoutTools(t, record, partial))}
	if _, err := Provision(context.Background(), cut, req); err == nil {
		t.Fatal("the second Provision succeeded despite its cut-short checkout stand-in")
	}
	if recorded, err := os.ReadFile(record); err != nil || strings.TrimSpace(string(recorded)) != "present" {
		t.Fatalf("the cut-short checkout recorded .jj/%s as %q (%v), want present", checkoutMarkerFile, recorded, err)
	}

	var steps []string
	again := req
	again.Steps = NewSteps(newTestClock(), func(line string) { steps = append(steps, line) })
	working, err := Provision(context.Background(), run, again)
	if err != nil {
		t.Fatalf("third Provision: %v", err)
	}
	if !slices.ContainsFunc(steps, func(line string) bool { return strings.HasPrefix(line, "step checkout-clear ") }) {
		t.Errorf("the provision after the cut-short checkout logged %q, want its checkout-clear", steps)
	}
	requireCheckedOut(t, working)
	for rel, want := range tree {
		if got, err := os.ReadFile(filepath.Join(working.Dir, rel)); err != nil || string(got) != want {
			t.Errorf("%s after the third Provision = %q (%v), want %q", rel, got, err, want)
		}
	}
	if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after a finished checkout, .jj/%s lstat = %v, want ErrNotExist", checkoutMarkerFile, err)
	}
}

// R3-4: a workspace that Legion finished checking out leaves no `.jj/legion-checkout`. When an
// agent later clears its sparse patterns and leaves an untracked file, the next provision writes
// no marker and keeps the untracked file.
func TestProvisionKeepsTheFilesOfAWorkspaceItDidNotCheckOut(t *testing.T) {
	// Not parallel: newLocalRunner calls t.Setenv.
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

	observer := &checkoutObserverRunner{inner: run}
	if _, err := Provision(context.Background(), observer, req); err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if !observer.sawCheckout {
		t.Fatal("second Provision ran no `jj sparse reset`")
	}
	if observer.err != nil {
		t.Fatalf("lstat of .jj/%s as the second Provision's checkout started: %v", checkoutMarkerFile, observer.err)
	}
	if observer.sawMarker {
		t.Error("second Provision wrote .jj/legion-checkout for a workspace whose patterns an agent cleared")
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
	// Not parallel: newLocalRunner calls t.Setenv.
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
