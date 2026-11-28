package workspace

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
// half of the tree and half of one file's bytes has its workspace removed and added again by the
// next provision, which checks out the whole tree.
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

	// Second provision with the real jj must remove the half-written workspace and check out whole.
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
// remove the workspace that checkout wrote into and add it again (workspace-reset), checking out
// markedCheckoutTree whole and leaving no marker.
func requireCutShortCheckoutRedone(t *testing.T, run *recordingRunner, req Request) {
	t.Helper()
	cutShortCheckout(t, run, req)

	var steps []string
	again := req
	again.Steps = NewSteps(newTestClock(), func(line string) { steps = append(steps, line) })
	working, err := Provision(context.Background(), run, again)
	if err != nil {
		t.Fatalf("the Provision after the cut-short checkout: %v", err)
	}
	if !slices.ContainsFunc(steps, func(line string) bool { return strings.HasPrefix(line, "step workspace-reset ") }) {
		t.Errorf("the provision after the cut-short checkout logged %q, want its workspace-reset", steps)
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
// a checkout's place ends) leaves the workspace marked, so the next provision adds it again, marked,
// for a checkout that is then cut short, and the one after that removes the workspace the cut-short
// checkout wrote into (workspace-reset) and adds it again, checking out the whole tree.
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
// workspace — leaves the workspace registered with no file, and marked. So the next provision adds
// it again, marked, for a checkout that is then cut short, and the one after that removes the
// workspace the cut-short checkout wrote into (workspace-reset) and adds it again, checking out the
// whole tree.
func TestProvisionMarksAWorkspaceWhoseRegistrationFailedAfterItsAdd(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
	req := provisionRequest(t)
	location, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}

	failBookmarkSet(t, run, req)
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

// stopAfterTheAdd provisions req's workspace with a jj whose `workspace add` adds the workspace and
// then plants a FIFO at its creation record, so the provision stops where RecordCreated refuses the
// FIFO: right after the add and the checkout marker, before the creation record and any bookmark.
func stopAfterTheAdd(t *testing.T, run *recordingRunner, req Request, location Workspace) {
	t.Helper()
	record := filepath.Join(location.Dir, ".jj", createdFile)
	realTools := run.tools
	run.tools = jjTools(t, "*' workspace add '*)\n  '"+realTools["jj"]+"' \"$@\" || exit\n  mkfifo '"+record+"' || exit\n  exit 0\n;;\n")
	_, err := Provision(context.Background(), run, req)
	run.tools = realTools
	if err == nil || !strings.Contains(err.Error(), record+" is a fifo") {
		t.Fatalf("Provision whose jj workspace add left a FIFO at the creation record = %v, want RecordCreated's refusal of it", err)
	}
}

// stopping is a provision TestProvisionAddsAgainAWorkspaceWhoseProvisionStoppedBeforeItsCheckoutEnded
// stops: its runner, its request and the workspace it provisions, and a directory outside that
// workspace, holding precious.txt, which a stop may link to from inside the workspace.
type stopping struct {
	run      *recordingRunner
	req      Request
	location Workspace
	outside  string
}

// stepNames is the name of each step that lines, a step clock's lines, ended, in order.
func stepNames(lines []string) []string {
	var names []string
	for _, line := range lines {
		if rest, ok := strings.CutPrefix(line, "step "); ok {
			name, _, _ := strings.Cut(rest, " ")
			names = append(names, name)
		}
	}
	return names
}

// requireWholeTree fails the test unless working holds tree and nothing else, checked out and
// unchanged: its sparse patterns are the whole tree (.), every entry below it but .jj and .git is a
// regular file of tree with the content tree gives it, every file of tree is there, and jj finds
// nothing changed.
func requireWholeTree(t *testing.T, working Workspace, tree map[string]string) {
	t.Helper()
	if listed := strings.TrimSpace(runSetup(t, working.Dir, "jj", "sparse", "list", "-R", working.Dir)); listed != "." {
		t.Errorf("the workspace's sparse patterns are %q, want the whole tree (.)", listed)
	}
	held := map[string]string{}
	err := filepath.WalkDir(working.Dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(working.Dir, path)
		if err != nil {
			return err
		}
		switch {
		case rel == ".":
			return nil
		case rel == ".jj" || rel == ".git":
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case entry.IsDir():
			return nil
		case !entry.Type().IsRegular():
			held[filepath.ToSlash(rel)] = entry.Type().String()
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		held[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(held, tree) {
		t.Errorf("the workspace holds %q, want %q", held, tree)
	}
	if diff := strings.TrimSpace(runSetup(t, working.Dir, "jj", "diff", "--summary", "-R", working.Dir)); diff != "" {
		t.Errorf("jj diff --summary = %q, want empty", diff)
	}
}

// A provision can stop anywhere from its `jj workspace add` to its checkout's end, on a failure or
// a kill of the daemon or the pod, and leaves the workspace marked (checkoutMarkerFile) with no
// sparse pattern wherever it stops. No role has started there, so the next provision removes the
// workspace (workspace-reset) and adds it again from nothing. Each row stops a first provision at
// one point of that window: right after the add and its marker, where RecordCreated refuses a FIFO
// planted at the creation record, the record then left a FIFO, gone, empty as a kill mid-write
// leaves it, or holding no instant; at its jj bookmark set, which only an issue GitHub has no
// branch of reaches; at its wait for a checkout's place, after its registration and its
// configuration; and in its checkout, cut short after writing half of one file, and a file and a
// link to a directory outside the workspace that the tree does not hold. Each stop is made for an
// issue whose branch GitHub has, whether or not the branch moves before the next provision, and
// for one GitHub has no branch of, whether or not GitHub makes one before the next provision.
//
// The next provision logs its workspace-reset between its checkout-check and its registration, and
// its workspace-delete after its configuration, once it has let the store lock go. It removes the
// old workspace whole: a file planted in its .jj is gone, its working-copy commit is abandoned,
// nothing is left renamed aside, and nothing outside it is removed through the link. It
// ends with the whole tree of GitHub's tip of the issue's branch, or of main for an issue with
// none, checked out on that commit and unchanged; with the issue's bookmark there, tracking
// origin's, or on the working-copy commit for an issue with no branch, as a first provision leaves
// it; with its creation recorded no earlier than it began; and with no marker. Its answer is waited
// for with no span of its own (testwait.Receive), so a provision that waits on the FIFO fails by
// name before the test binary's deadline.
func TestProvisionAddsAgainAWorkspaceWhoseProvisionStoppedBeforeItsCheckoutEnded(t *testing.T) {
	t.Parallel()
	// pushed is what pushRemoteBranch writes into the file it adds.
	const pushed = "pushed from another clone\n"
	afterTheAdd := func(damage func(t *testing.T, record string)) func(*testing.T, stopping) {
		return func(t *testing.T, s stopping) {
			stopAfterTheAdd(t, s.run, s.req, s.location)
			damage(t, filepath.Join(s.location.Dir, ".jj", createdFile))
		}
	}
	replacedWith := func(body string) func(*testing.T, string) {
		return func(t *testing.T, record string) {
			if err := os.Remove(record); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(record, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	stops := []struct {
		name string
		// branchless is a stop that only the provision of an issue GitHub has no branch of reaches.
		branchless bool
		stop       func(*testing.T, stopping)
	}{
		{name: "after its add, its creation record a FIFO", stop: afterTheAdd(func(*testing.T, string) {})},
		{name: "after its add, its creation record gone", stop: afterTheAdd(func(t *testing.T, record string) {
			if err := os.Remove(record); err != nil {
				t.Fatal(err)
			}
		})},
		{name: "after its add, its creation record empty", stop: afterTheAdd(replacedWith(""))},
		{name: "after its add, its creation record holding no instant", stop: afterTheAdd(replacedWith("not an instant\n"))},
		{name: "at its jj bookmark set", branchless: true, stop: func(t *testing.T, s stopping) { failBookmarkSet(t, s.run, s.req) }},
		{name: "at its wait for a checkout place", stop: func(t *testing.T, s stopping) {
			stopped := s.req
			stopped.CheckoutSlot = func(context.Context) (func(), error) { return nil, context.Canceled }
			if _, err := Provision(context.Background(), s.run, stopped); !errors.Is(err, context.Canceled) {
				t.Fatalf("Provision whose wait for a checkout's place ended = %v, want context.Canceled", err)
			}
		}},
		{name: "in its checkout", stop: func(t *testing.T, s stopping) {
			partial := t.TempDir()
			for rel, body := range map[string]string{"half.txt": "first half\n", "stray/extra.txt": "no file of the tree\n"} {
				full := filepath.Join(partial, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(s.outside, filepath.Join(partial, "escape")); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(t.TempDir(), "marker.txt")
			realTools := s.run.tools
			s.run.tools = cutShortCheckoutTools(t, record, partial)
			_, err := Provision(context.Background(), s.run, s.req)
			s.run.tools = realTools
			if err == nil {
				t.Fatal("the Provision whose checkout was cut short succeeded")
			}
			if got := readCheckoutRecord(t, record); got != "present" {
				t.Fatalf("the cut-short checkout recorded .jj/%s as %q, want present", checkoutMarkerFile, got)
			}
		}},
	}
	for _, branch := range []struct {
		name string
		// first is the file a commit on the issue's branch adds on GitHub before the first
		// provision, and retry the file one adds before the next; "" for none.
		first, retry string
	}{
		{"a branch on GitHub", "branch.txt", ""},
		{"a branch on GitHub that moves before the retry", "branch.txt", "moved.txt"},
		{"no branch on GitHub", "", ""},
		{"no branch on GitHub until the retry", "", "made.txt"},
	} {
		for _, stop := range stops {
			if stop.branchless && branch.first != "" {
				continue
			}
			t.Run(branch.name+", stopped "+stop.name, func(t *testing.T) {
				t.Parallel()
				run := newLocalRunner(t)
				pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
				req := provisionRequest(t)
				location, err := Location(req.StateDir, req.Repo, req.Issue)
				if err != nil {
					t.Fatal(err)
				}
				tree := maps.Clone(markedCheckoutTree)
				var tip string
				if branch.first != "" {
					tip = pushRemoteBranch(t, run.remote, location.Bookmark, branch.first)
					tree[branch.first] = pushed
				}
				outside := t.TempDir()
				precious := filepath.Join(outside, "precious.txt")
				if err := os.WriteFile(precious, []byte("keep\n"), 0o644); err != nil {
					t.Fatal(err)
				}

				stop.stop(t, stopping{run: run, req: req, location: location, outside: outside})
				if _, err := os.Lstat(filepath.Join(location.Dir, ".jj", checkoutMarkerFile)); err != nil {
					t.Fatalf("after the provision stopped %s, .jj/%s lstat = %v, want the marker there", stop.name, checkoutMarkerFile, err)
				}
				if listed := strings.TrimSpace(runSetup(t, location.Dir, "jj", "sparse", "list", "--ignore-working-copy", "-R", location.Dir)); listed != "" {
					t.Fatalf("after the provision stopped %s, the workspace holds the sparse patterns %q, want none", stop.name, listed)
				}
				// What only the old workspace holds: a file in its .jj, which jj never checks out
				// or snapshots, and its working-copy commit.
				planted := filepath.Join(location.Dir, ".jj", "old-workspace")
				if err := os.WriteFile(planted, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				oldCommit := commitOf(t, location.Clone, filepath.Base(location.Dir)+"@")
				if branch.retry != "" {
					tip = pushRemoteBranch(t, run.remote, location.Bookmark, branch.retry)
					tree[branch.retry] = pushed
				}

				var steps []string
				retry := req
				retry.Steps = NewSteps(newTestClock(), func(line string) { steps = append(steps, line) })
				retried := time.Now()
				var working Workspace
				done := make(chan error, 1)
				go func() {
					var err error
					working, err = Provision(context.Background(), run, retry)
					done <- err
				}()
				if err := testwait.Receive(t, "the Provision after the one stopped "+stop.name, done); err != nil {
					t.Fatalf("the Provision after the one stopped %s: %v", stop.name, err)
				}

				want := []string{"checkout-check", "workspace-reset", "workspace-registration", "configuration", "workspace-delete"}
				if names := stepNames(steps); len(names) < len(want) || !slices.Equal(names[len(names)-len(want):], want) {
					t.Errorf("the Provision after the stop logged the steps %q, want %q last", names, want)
				}
				if _, err := os.Lstat(planted); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("after the Provision, the old workspace's .jj/old-workspace lstat = %v, want ErrNotExist", err)
				}
				if _, err := os.Lstat(removingPath(location.Dir)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("after the Provision, %s lstat = %v, want ErrNotExist", removingPath(location.Dir), err)
				}
				visible := strings.Fields(runSetup(t, location.Clone, "jj", "log", "-r", "all()", "--no-graph", "-T", `commit_id ++ "\n"`, "--ignore-working-copy"))
				if slices.Contains(visible, oldCommit) {
					t.Errorf("after the Provision, the old workspace's working-copy commit %s is still visible", oldCommit)
				}
				if body, err := os.ReadFile(precious); err != nil || string(body) != "keep\n" {
					t.Errorf("after the Provision, %s = %q, %v; want it kept", precious, body, err)
				}

				requireWholeTree(t, working, tree)
				fromGitHub := tip != ""
				if !fromGitHub {
					tip = commitOf(t, location.Clone, "main@origin")
				}
				if parent := commitOf(t, working.Dir, "@-"); parent != tip {
					t.Errorf("after the Provision, @- is %s, want %s", parent, tip)
				}
				bookmarked := tip
				if !fromGitHub {
					bookmarked = commitOf(t, working.Dir, "@")
				}
				rows, err := readBookmark(context.Background(), run, working, working.Bookmark)
				if err != nil {
					t.Fatal(err)
				}
				if local := rows.local; !local.present || local.conflict || local.added[0] != bookmarked {
					t.Errorf("after the Provision, bookmark %s is %+v, want it on %s alone", working.Bookmark, local, bookmarked)
				}
				if origin := rows.origin; fromGitHub != (origin.present && origin.tracked && !origin.conflict && origin.added[0] == tip) {
					t.Errorf("after the Provision, %s@origin is %+v, want it tracked at %s: %t", working.Bookmark, origin, tip, fromGitHub)
				}
				if at, ok, err := Created(working.Dir); err != nil || !ok || at.Before(retried) {
					t.Errorf("Created after the Provision = %s, %t, %v; want a record no earlier than %s", at, ok, err, retried)
				}
				if _, err := os.Lstat(filepath.Join(working.Dir, ".jj", checkoutMarkerFile)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("after the Provision, .jj/%s lstat = %v, want ErrNotExist", checkoutMarkerFile, err)
				}
			})
		}
	}
}

// A provision stopped inside workspace-reset, after its rename of the old workspace aside and before
// that directory's delete ends (the delete's time follows what the cut-short checkout wrote), leaves
// removingPath beside a workspace that is registered but missing. The next provision adds the
// workspace again and checks out the whole tree; it also clears what the stopped reset left aside,
// as resetUnfinishedWorkspace's first step says, so a partial checkout of the issue's tree does not
// stay on the issue's volume until the issue is removed.
func TestProvisionClearsWhatAStoppedWorkspaceResetLeftAside(t *testing.T) {
	t.Parallel()
	run := newLocalRunner(t)
	pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
	req := provisionRequest(t)
	location, err := Location(req.StateDir, req.Repo, req.Issue)
	if err != nil {
		t.Fatal(err)
	}

	partial := t.TempDir()
	if err := os.WriteFile(filepath.Join(partial, "half.txt"), []byte("first half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	realTools := run.tools
	run.tools = cutShortCheckoutTools(t, filepath.Join(t.TempDir(), "marker.txt"), partial)
	if _, err := Provision(context.Background(), run, req); err == nil {
		t.Fatal("the Provision whose checkout was cut short succeeded")
	}
	run.tools = realTools
	// The state a kill right after workspace-reset's rename leaves.
	aside := removingPath(location.Dir)
	if err := os.Rename(location.Dir, aside); err != nil {
		t.Fatal(err)
	}

	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the Provision after a stopped workspace-reset: %v", err)
	}
	requireWholeTree(t, working, markedCheckoutTree)
	if _, err := os.Lstat(aside); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after the provision that followed a stopped workspace-reset, %s lstat = %v, want ErrNotExist", aside, err)
	}
}

// cutShortCheckout provisions req's workspace with a checkout cut short after writing the first
// half of half.txt (cutShortCheckoutTools), so the workspace is left registered, marked and holding
// no sparse pattern: the workspace a workspace-reset takes away.
func cutShortCheckout(t *testing.T, run *recordingRunner, req Request) {
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
}

// workspace-reset can itself stop, on a failure or a kill, at each of its own steps, and a
// provision stopped there must leave one after it that ends where a first provision ends. Each row
// makes a real marked workspace whose checkout was cut short (cutShortCheckout), for an issue whose
// branch GitHub has and for one GitHub has no branch of, whose bookmark the cut-short provision set
// on the workspace's working-copy commit. It then has the next provision's real workspace-reset stop
// at one of its own points, with a jj that fails one command:
//   - at its jj bookmark delete, which only the issue with no branch reaches, before the rename:
//     the workspace stays where it was, marked, with the bookmark on its working-copy commit;
//   - at its jj workspace forget, after the bookmark's deletion and the rename aside: the directory
//     is aside and the workspace still registered;
//   - after the reset ended and before its directory aside is deleted (workspace-delete), at the
//     registration's jj workspace add: the directory is aside and the workspace forgotten.
//
// The provision after the stop, with the real jj, deletes a directory left aside before it takes
// the store lock (workspace-delete, its first step) and adds the workspace again, or resets the
// workspace still marked (workspace-reset, then workspace-delete once the lock is let go). It ends
// with the whole tree of GitHub's tip of the issue's branch, or of main for an issue with none,
// checked out on that commit and unchanged, never on the old workspace's empty working-copy commit,
// which is abandoned; with the issue's bookmark alone on the working-copy commit for an issue with
// no branch, or on origin's tip tracking origin's; with its creation recorded no earlier than it
// began; and with no marker and nothing left aside.
func TestProvisionRecoversFromAStoppedWorkspaceReset(t *testing.T) {
	t.Parallel()
	// pushed is what pushRemoteBranch writes into the file it adds.
	const pushed = "pushed from another clone\n"
	failing := func(command string) string {
		return "*' " + command + " '*) echo 'the test fails jj " + command + "' >&2; exit 1 ;;\n"
	}
	stops := []struct {
		name string
		// command is the jj command that fails, and in the step it fails in.
		command, in string
		// branchless is a stop only the issue GitHub has no branch of reaches.
		branchless bool
		// aside is whether the stop leaves the workspace's directory renamed aside, and registered
		// whether it leaves the workspace registered in the shared clone.
		aside, registered bool
	}{
		{name: "at its jj bookmark delete", command: "bookmark delete", in: "workspace-reset", branchless: true, registered: true},
		{name: "at its jj workspace forget", command: "workspace forget", in: "workspace-reset", aside: true, registered: true},
		{name: "after it ended and before its workspace-delete", command: "workspace add", in: "workspace-registration", aside: true},
	}
	for _, branch := range []struct {
		name string
		// file is the file a commit on the issue's branch adds on GitHub; "" for no branch.
		file string
	}{
		{"a branch on GitHub", "branch.txt"},
		{"no branch on GitHub", ""},
	} {
		for _, stop := range stops {
			if stop.branchless && branch.file != "" {
				continue
			}
			t.Run(branch.name+", stopped "+stop.name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				run := newLocalRunner(t)
				pushTreeFiles(t, run.remote, "main", markedCheckoutTree)
				req := provisionRequest(t)
				location, err := Location(req.StateDir, req.Repo, req.Issue)
				if err != nil {
					t.Fatal(err)
				}
				aside, name := removingPath(location.Dir), filepath.Base(location.Dir)
				tree := maps.Clone(markedCheckoutTree)
				var tip string
				if branch.file != "" {
					tip = pushRemoteBranch(t, run.remote, location.Bookmark, branch.file)
					tree[branch.file] = pushed
				}
				cutShortCheckout(t, run, req)
				oldCommit := commitOf(t, location.Clone, name+"@")
				if branch.file == "" {
					if on := commitOf(t, location.Clone, `bookmarks(exact:"`+location.Bookmark+`")`); on != oldCommit {
						t.Fatalf("after the cut-short provision, bookmark %s is on %q, want the workspace's working-copy commit %s", location.Bookmark, on, oldCommit)
					}
				}

				// The next provision, its workspace-reset stopped.
				var stoppedSteps []string
				stopped := req
				stopped.Steps = NewSteps(newTestClock(), func(line string) { stoppedSteps = append(stoppedSteps, line) })
				realTools := run.tools
				run.tools = jjTools(t, failing(stop.command))
				_, err = Provision(ctx, run, stopped)
				run.tools = realTools
				if err == nil || !strings.Contains(err.Error(), stop.command) {
					t.Fatalf("the Provision whose jj %s failed = %v, want that command's failure", stop.command, err)
				}
				stopped.Steps.Fail()
				if names := stepNames(stoppedSteps); !slices.Contains(names, "workspace-reset") || names[len(names)-1] != stop.in {
					t.Fatalf("the stopped Provision logged the steps %q, want its workspace-reset and its failure in %s", names, stop.in)
				}
				_, dirErr := os.Lstat(location.Dir)
				_, asideErr := os.Lstat(aside)
				if (dirErr == nil) == stop.aside || (asideErr == nil) != stop.aside {
					t.Fatalf("after the stop, %s lstat = %v and %s lstat = %v; want the directory renamed aside: %t", location.Dir, dirErr, aside, asideErr, stop.aside)
				}
				listed := runSetup(t, location.Clone, "jj", "workspace", "list", "--ignore-working-copy", "-R", location.Clone)
				if registered := strings.Contains(listed, name+":"); registered != stop.registered {
					t.Fatalf("after the stop, the shared clone's workspaces are %q; want %s registered: %t", listed, name, stop.registered)
				}
				if !stop.aside {
					if _, err := os.Lstat(filepath.Join(location.Dir, ".jj", checkoutMarkerFile)); err != nil {
						t.Fatalf("after the stop, .jj/%s lstat = %v, want the marker there", checkoutMarkerFile, err)
					}
				}
				rows, err := readBookmark(ctx, run, location, location.Bookmark)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case branch.file != "" && (!rows.local.present || rows.local.conflict || rows.local.added[0] != tip):
					t.Fatalf("after the stop, bookmark %s is %+v, want it on origin's tip %s, where no reset deletes it", location.Bookmark, rows.local, tip)
				case branch.file == "" && stop.command == "bookmark delete" && (!rows.local.present || rows.local.conflict || rows.local.added[0] != oldCommit):
					t.Fatalf("after the stop, bookmark %s is %+v, want it still on the old working-copy commit %s", location.Bookmark, rows.local, oldCommit)
				case branch.file == "" && stop.command != "bookmark delete" && rows.local.present:
					t.Fatalf("after the stop, bookmark %s is %+v, want it deleted, as the reset deletes it before its rename", location.Bookmark, rows.local)
				}

				// The provision after the stop, with the real jj.
				var steps []string
				retry := req
				retry.Steps = NewSteps(newTestClock(), func(line string) { steps = append(steps, line) })
				retried := time.Now()
				working, err := Provision(ctx, run, retry)
				if err != nil {
					t.Fatalf("the Provision after the stopped workspace-reset: %v", err)
				}
				names := stepNames(steps)
				if stop.aside {
					if len(names) < 2 || !slices.Equal(names[:2], []string{"inputs", "workspace-delete"}) || slices.Contains(names, "workspace-reset") {
						t.Errorf("the Provision after the stop logged the steps %q, want workspace-delete first, before the store lock, and no workspace-reset", names)
					}
				} else if want := []string{"checkout-check", "workspace-reset", "workspace-registration", "configuration", "workspace-delete"}; len(names) < len(want) || !slices.Equal(names[len(names)-len(want):], want) {
					t.Errorf("the Provision after the stop logged the steps %q, want %q last", names, want)
				}

				requireWholeTree(t, working, tree)
				fromGitHub := tip != ""
				if !fromGitHub {
					tip = commitOf(t, location.Clone, "main@origin")
				}
				if parent := commitOf(t, working.Dir, "@-"); parent != tip {
					t.Errorf("after the Provision, @- is %s, want %s (the old working-copy commit is %s)", parent, tip, oldCommit)
				}
				visible := strings.Fields(runSetup(t, location.Clone, "jj", "log", "-r", "all()", "--no-graph", "-T", `commit_id ++ "\n"`, "--ignore-working-copy"))
				if slices.Contains(visible, oldCommit) {
					t.Errorf("after the Provision, the old workspace's working-copy commit %s is still visible", oldCommit)
				}
				bookmarked := tip
				if !fromGitHub {
					bookmarked = commitOf(t, working.Dir, "@")
				}
				rows, err = readBookmark(ctx, run, working, working.Bookmark)
				if err != nil {
					t.Fatal(err)
				}
				if local := rows.local; !local.present || local.conflict || local.added[0] != bookmarked {
					t.Errorf("after the Provision, bookmark %s is %+v, want it on %s alone", working.Bookmark, local, bookmarked)
				}
				if origin := rows.origin; fromGitHub != (origin.present && origin.tracked && !origin.conflict && origin.added[0] == tip) {
					t.Errorf("after the Provision, %s@origin is %+v, want it tracked at %s: %t", working.Bookmark, origin, tip, fromGitHub)
				}
				if at, ok, err := Created(working.Dir); err != nil || !ok || at.Before(retried) {
					t.Errorf("Created after the Provision = %s, %t, %v; want a record no earlier than %s", at, ok, err, retried)
				}
				if _, err := os.Lstat(filepath.Join(working.Dir, ".jj", checkoutMarkerFile)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("after the Provision, .jj/%s lstat = %v, want ErrNotExist", checkoutMarkerFile, err)
				}
				if _, err := os.Lstat(aside); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("after the Provision, %s lstat = %v, want ErrNotExist", aside, err)
				}
			})
		}
	}
}

// R3-4: a workspace that Legion finished checking out leaves no `.jj/legion-checkout`. When an
// agent later clears its sparse patterns and leaves an untracked file, the next provision writes
// no marker, removes nothing (no workspace-reset) and keeps the untracked file.
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
	var steps []string
	again := req
	again.Steps = NewSteps(newTestClock(), func(line string) { steps = append(steps, line) })
	_, err = Provision(context.Background(), run, again)
	run.tools = realTools
	if err != nil {
		t.Fatalf("second Provision: %v", err)
	}
	if slices.ContainsFunc(steps, func(line string) bool { return strings.HasPrefix(line, "step workspace-reset ") }) {
		t.Errorf("second Provision logged %q, want no workspace-reset of a workspace with no marker", steps)
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

// The checkout marker's helpers refuse a symlinked `.jj`: none writes, reads or removes a marker
// through it.
func TestProvisionCheckoutMarkerHelpersRefuseALinkedJJ(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	badWS := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(badWS, ".jj")); err != nil {
		t.Fatal(err)
	}
	if err := writeCheckoutMarker(badWS); !errors.Is(err, errLinkedJJ) {
		t.Errorf("writeCheckoutMarker over linked .jj = %v, want errLinkedJJ", err)
	}
	if _, err := HasCheckoutMarker(badWS); !errors.Is(err, errLinkedJJ) {
		t.Errorf("HasCheckoutMarker over linked .jj = %v, want errLinkedJJ", err)
	}
	if err := removeCheckoutMarker(badWS); !errors.Is(err, errLinkedJJ) {
		t.Errorf("removeCheckoutMarker over linked .jj = %v, want errLinkedJJ", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, checkoutMarkerFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("marker was written through linked .jj into %s (%v)", outside, err)
	}
}
