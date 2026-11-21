//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// A stop of print after each call by which it changes the file system leaves the spec files as they
// were before the run or as the run leaves them, byte for byte, and tree takes them; the next run of
// print ends as a run with no stop ends, and leaves the tree as a run with no stop leaves it, each
// entry that is no spec file the same inode, and nothing beside the directory (D5). Each change
// that print makes goes through one seam, fileSystem, so the test stops a run as a kill stops it:
// after the stop, each call changes nothing and fails. The run with no stop counts the calls. Then a
// run stops after each prefix of its calls: after 0 calls, after 1, and on to the whole run. In each
// run, the one with no stop, each stopped run and each next run, the test compares the tree before
// each call with the tree after the call before it, and at the end of the run, so a change made
// beside the seam is a fault on each path of print: the one with no stop, and the paths of a
// failure and of the clean-up that a stop and the next run take (removeNew, undoNew, finishOld,
// keep, cleanLeftovers, removeDirs).
//
// The shapes: a run that changes one file, several files, or adds a file; a run that moves a node
// out of a file into a new file; a run over a directory that holds entries that print does not own
// (a file, a subdirectory and a symbolic link); a run into a directory that does not exist, which
// the lock makes; a run where the system refuses each hard link, so print copies each file that it
// keeps; and a run that print refuses, because the system refuses the exchange, whose next run is
// refused too and leaves the tree as it was. No run of print removes a spec file: the files that it
// leaves hold each file that it read (specgraph.ReadLayout keeps each), and Layout.Place empties
// none, so the shape nearest to that one moves a node out of a file. Two controls must fail: the
// write of the head before this change, which renamed its files one at a time, and a change made
// beside the seam.
func TestAStopAfterEachCallOfPrintLeavesTheFilesBeforeOrAfter(t *testing.T) {
	t.Parallel()
	const limit = 4096
	others := func(t *testing.T, dir string) string {
		t.Helper()
		writeFile(t, filepath.Join(dir, "README"), []byte("The spec files of the daemon.\n"))
		if err := os.Mkdir(filepath.Join(dir, "notes"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "notes", "note.md"), []byte("A note.\n"))
		if err := os.Symlink("README", filepath.Join(dir, "readme-link")); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	refuseLinks := func(call stopCall) fileSystem {
		return callFS{func(op, path string, do func() error) error {
			return call(op, path, func() error {
				if op == "link" {
					return &os.LinkError{Op: "link", New: path, Err: syscall.EPERM}
				}
				return do()
			})
		}}
	}
	terms := map[string]string{"term-a": stopTerm("term-a", 60, 1), "term-b": stopTerm("term-b", 34, 1), "term-d": stopTerm("term-d", 50, 1)}
	for _, row := range []struct {
		name string
		run  func(t *testing.T) stopRun
		// fault is "" for a shape that must pass, and for a control the words of the fault
		// that it must find.
		fault string
	}{
		{"a run that changes one file", func(t *testing.T) stopRun {
			return newStopRun(t, stopTree(t, stopClaims(6), limit), stopClaims(6, "claim-ac"), []string{"claim-ac"}, limit, "claim-ac.yaml")
		}, ""},
		{"a run that changes several files", func(t *testing.T) stopRun {
			return newStopRun(t, stopTree(t, stopClaims(6), limit), stopClaims(6, "claim-aa", "claim-ac", "claim-ae"), []string{"claim-aa", "claim-ac", "claim-ae"}, limit,
				"claim-aa.yaml", "claim-ac.yaml", "claim-ae.yaml")
		}, ""},
		{"a run that adds a file", func(t *testing.T) stopRun {
			return newStopRun(t, stopTree(t, stopClaims(6), limit), stopClaims(7), []string{"claim-ag"}, limit, "claim-ag.yaml")
		}, ""},
		{"a run that moves a node out of a file into a new file", func(t *testing.T) stopRun {
			second := maps.Clone(terms)
			second["term-b"] = stopTerm("term-b", 48, 2)
			return newStopRun(t, stopTree(t, terms, limit), second, []string{"term-b"}, limit, "terms-2.yaml", "terms-3.yaml")
		}, ""},
		{"a run over a directory that holds entries that print does not own", func(t *testing.T) stopRun {
			return newStopRun(t, others(t, stopTree(t, stopClaims(6), limit)), stopClaims(6, "claim-ab", "claim-ad"), []string{"claim-ab", "claim-ad"}, limit,
				"claim-ab.yaml", "claim-ad.yaml")
		}, ""},
		{"a run into a directory that does not exist", func(t *testing.T) stopRun {
			return newStopRun(t, "", stopClaims(3), []string{"claim-aa", "claim-ab", "claim-ac"}, limit, "claim-aa.yaml", "claim-ab.yaml", "claim-ac.yaml")
		}, ""},
		// Each spec file has the mode 0640, so the copy that print makes of each file that it
		// keeps changes the mode that the copy was made with (0644), and a change of that mode
		// made beside the seam shows.
		{"a run where the system refuses each hard link", func(t *testing.T) stopRun {
			before := others(t, stopTree(t, stopClaims(6), limit))
			for name := range specFiles(t, before) {
				if err := os.Chmod(filepath.Join(before, name), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			r := newStopRun(t, before, stopClaims(6, "claim-ab", "claim-ad"), []string{"claim-ab", "claim-ad"}, limit, "claim-ab.yaml", "claim-ad.yaml")
			r.fsys = refuseLinks
			return r
		}, ""},
		{"a run that print refuses, because the system refuses the exchange", func(t *testing.T) stopRun {
			r := newStopRun(t, others(t, stopTree(t, stopClaims(6), limit)), stopClaims(6, "claim-ac"), []string{"claim-ac"}, limit)
			r.fsys = func(call stopCall) fileSystem {
				return callFS{func(op, path string, do func() error) error {
					return call(op, path, func() error {
						if op == "exchange" {
							return syscall.EXDEV
						}
						return do()
					})
				}}
			}
			r.next, r.code = r.fsys, 2
			return r
		}, ""},
		{"the control: the files renamed one at a time, as the head before this change wrote them", func(t *testing.T) stopRun {
			r := newStopRun(t, stopTree(t, stopClaims(6), limit), stopClaims(6, "claim-aa", "claim-ac", "claim-ae"), []string{"claim-aa", "claim-ac", "claim-ae"}, limit,
				"claim-aa.yaml", "claim-ac.yaml", "claim-ae.yaml")
			r.fsys = func(call stopCall) fileSystem { return renamesFS{callFS{call}} }
			return r
		}, "the spec files are neither as before the run nor as after it"},
		{"the control: a change beside the seam", func(t *testing.T) stopRun {
			r := newStopRun(t, stopTree(t, stopClaims(6), limit), stopClaims(6, "claim-ac"), []string{"claim-ac"}, limit, "claim-ac.yaml")
			r.fsys = func(call stopCall) fileSystem {
				return callFS{func(op, path string, do func() error) error {
					err := call(op, path, do)
					if op == "exchange" && err == nil {
						err = os.WriteFile(filepath.Join(path, "beside.txt"), []byte("A change beside the seam.\n"), 0o644)
					}
					return err
				}}
			}
			return r
		}, "the tree changed beside the seam"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			calls, faults := stopAtEachCall(t, row.run(t))
			if row.fault == "" {
				for _, fault := range faults {
					t.Error(fault)
				}
				t.Logf("%d calls of the seam, %d stops: %s", len(calls), len(calls)+1, strings.Join(calls, "; "))
				return
			}
			found := slices.IndexFunc(faults, func(fault string) bool { return strings.Contains(fault, row.fault) })
			if found < 0 {
				t.Fatalf("the control found no fault %q, so the test cannot see it; faults: %q", row.fault, faults)
			}
			t.Logf("the control fails, as it must: %s", faults[found])
		})
	}
}

// stagingNumber is the number in the name of the directory that a run builds beside its spec
// directory, which differs from run to run: the test names each call with "<inode>" in its place.
var stagingNumber = regexp.MustCompile(regexp.QuoteMeta(stagingInfix) + "[0-9]+")

// errStopped is the answer of each call of the seam after the stop of a run: the call changes
// nothing, as a kill of the run after the call before it leaves the files.
var errStopped = errors.New("the run stopped before this call")

// stopCall runs one call of the seam, do, of the method op on path, or answers an error in its
// place and changes nothing.
type stopCall = func(op, path string, do func() error) error

// stopRun is one run of print in TestAStopAfterEachCallOfPrintLeavesTheFilesBeforeOrAfter: before,
// the spec directory before the run ("" for a run into a directory that does not exist); the
// approved spec a at path, the ids of the run and the bound of one file; changes, each spec file
// that the run with no stop must change or add, so the run is the shape that its name says; fsys,
// the file system of the run, with each call made through call; next, the file system of the next
// run after a stop; and code, the exit status of a run with no stop, 0 or the 2 of a refused run.
type stopRun struct {
	before  string
	path    string
	a       specgraph.Approved
	ids     []string
	limit   int64
	changes []string
	fsys    func(call stopCall) fileSystem
	next    func(call stopCall) fileSystem
	code    int
}

// newStopRun is the run over the spec directory before that prints the ids from the documents
// second, each by its id, within limit, that must change the files changes and exit 0, and whose
// next run after a stop runs on the file system of the process.
func newStopRun(t *testing.T, before string, second map[string]string, ids []string, limit int64, changes ...string) stopRun {
	t.Helper()
	path := writeApproved(t, second)
	process := func(call stopCall) fileSystem { return callFS{call} }
	return stopRun{before: before, path: path, a: readApprovedFile(t, path), ids: ids, limit: limit, changes: changes, fsys: process, next: process}
}

// fresh is a new spec directory as before holds it, for one run, and root, the new directory above
// it that holds it alone: for a run into a directory that does not exist, root/repo/spec.
func (r stopRun) fresh(t *testing.T) (root, dir string) {
	t.Helper()
	if r.before == "" {
		root = t.TempDir()
		return root, filepath.Join(root, "repo", "spec")
	}
	dir = copyTree(t, r.before)
	return filepath.Dir(dir), dir
}

// print runs print over dir with fsys, and answers its exit status and its stderr. It waits for the
// lock of dir as the command does (printLockWait): a child that another test of the package starts
// holds a copy of each open file of this process, and so of the flock of a run that has ended,
// from its fork to its exec, and a run that waits no time for the lock fails then.
func (r stopRun) print(dir string, fsys fileSystem) (int, string) {
	var stderr strings.Builder
	code := printApproved(io.Discard, &stderr, fsys, r.path, r.a, r.ids, dir, printBounds{limit: r.limit, wait: printLockWait})
	return code, stderr.String()
}

// seamWatch watches one run of print for a change of the tree under root beside the seam: through
// call, it compares the tree before each call of the seam with the tree after the call before it,
// and end compares the tree at the end of the run with the tree after the last call. run names the
// run in each fault.
type seamWatch struct {
	t      *testing.T
	root   string
	run    string
	calls  int
	last   map[string]string
	faults []string
}

// watch is a seamWatch of the run named run over the tree under root, from now.
func watch(t *testing.T, root, run string) *seamWatch {
	return &seamWatch{t: t, root: root, run: run, last: snapshot(t, root)}
}

// call is next, a call of the seam, with the tree compared before it and read after it.
func (w *seamWatch) call(next stopCall) stopCall {
	return func(op, path string, do func() error) error {
		w.calls++
		if now := snapshot(w.t, w.root); !maps.Equal(now, w.last) {
			name, _ := filepath.Rel(w.root, path)
			w.faults = append(w.faults, fmt.Sprintf("%s: before call %d (%s %s), the tree changed beside the seam: %s", w.run, w.calls, op, name, changeOf(w.last, now)))
		}
		err := next(op, path, do)
		w.last = snapshot(w.t, w.root)
		return err
	}
}

// end answers each fault of the run, with a change of the tree after its last call.
func (w *seamWatch) end() []string {
	if now := snapshot(w.t, w.root); !maps.Equal(now, w.last) {
		w.faults = append(w.faults, fmt.Sprintf("%s: after the last call, %d, the tree changed beside the seam: %s", w.run, w.calls, changeOf(w.last, now)))
	}
	return w.faults
}

// stopAtEachCall runs r once with no stop, on a fresh spec directory, and records each call of the
// seam; then, for each n from 0 to the number of calls, it runs r on a fresh spec directory with a
// seam that stops after call n. It answers the calls, each its method and its path from the
// directory above the spec directory, and one line for each fault: a change of the tree beside the
// seam in any run, a run with no stop that does not exit r.code or is not the shape that r names,
// and after a stop, spec files that are neither as before the run nor as after it, files that tree
// refuses, and a next run of print that does not exit r.code or does not leave the tree as the run
// with no stop leaves it.
func stopAtEachCall(t *testing.T, r stopRun) ([]string, []string) {
	t.Helper()
	root, dir := r.fresh(t)
	var calls []string
	w := watch(t, root, "the run with no stop")
	code, stderr := r.print(dir, r.fsys(w.call(func(op, path string, do func() error) error {
		name, _ := filepath.Rel(root, path)
		calls = append(calls, op+" "+stagingNumber.ReplaceAllString(name, stagingInfix+"<inode>"))
		return do()
	})))
	faults := w.end()
	if code != r.code {
		return calls, append(faults, fmt.Sprintf("the run with no stop: exit %d, want %d, stderr %q", code, r.code, stderr))
	}
	before, after := specFilesOf(t, r.before), specFilesOf(t, dir)
	var changes []string
	for _, name := range slices.Sorted(maps.Keys(after)) {
		if text, held := before[name]; !held || text != after[name] {
			changes = append(changes, name)
		}
	}
	if !slices.Equal(changes, r.changes) {
		faults = append(faults, fmt.Sprintf("the run with no stop changed %q, want %q: the run is not the shape that its name says", changes, r.changes))
	}
	if !slices.ContainsFunc(calls, func(call string) bool { return strings.HasPrefix(call, "exchange ") }) {
		faults = append(faults, fmt.Sprintf("the run with no stop made no exchange: %q", calls))
	}
	want := entries(t, root)
	for n := 0; n <= len(calls); n++ {
		faults = append(faults, r.stopAfter(t, n, calls, before, after, want)...)
	}
	return calls, faults
}

// stopAfter runs r on a fresh spec directory with a seam that stops after call n of calls, the
// calls of the run with no stop, and answers each fault that the stopped run and the next run
// leave: before and after are the spec files before the run and after the run with no stop, and
// want each entry under the directory above the spec directory after it.
func (r stopRun) stopAfter(t *testing.T, n int, calls []string, before, after, want map[string]string) []string {
	t.Helper()
	root, dir := r.fresh(t)
	kept := keptInodes(t, dir)
	at := "stopped before the first call"
	if n > 0 {
		at = fmt.Sprintf("stopped after call %d of %d (%s)", n, len(calls), calls[n-1])
	}
	made := 0
	stopped := watch(t, root, "the run "+at)
	r.print(dir, r.fsys(stopped.call(func(op, path string, do func() error) error {
		if made == n {
			return errStopped
		}
		made++
		return do()
	})))
	faults := stopped.end()
	files := specFilesOf(t, dir)
	if !maps.Equal(files, before) && !maps.Equal(files, after) {
		return append(faults, fmt.Sprintf("%s, the spec files are neither as before the run nor as after it: %s", at, filesState(files, before, after)))
	}
	// tree refuses a directory that holds no spec file, as it did before a run into a directory
	// that does not exist.
	if len(files) > 0 {
		var stderr strings.Builder
		if code := run([]string{"tree", "--dir", dir}, io.Discard, &stderr); code != 0 {
			faults = append(faults, fmt.Sprintf("%s, tree refuses the files: exit %d, stderr %q", at, code, stderr.String()))
		}
	}
	next := watch(t, root, "the next run after the run "+at)
	code, stderr := r.print(dir, r.next(next.call(func(_, _ string, do func() error) error { return do() })))
	faults = append(faults, next.end()...)
	if code != r.code {
		return append(faults, fmt.Sprintf("%s, the next run of print: exit %d, want %d, stderr %q", at, code, r.code, stderr))
	}
	if got := entries(t, root); !maps.Equal(got, want) {
		faults = append(faults, fmt.Sprintf("%s, the next run of print leaves %q under %s, want %q, as a run with no stop leaves it", at, got, root, want))
	}
	for _, name := range slices.Sorted(maps.Keys(kept)) {
		if got := inodeOf(t, filepath.Join(dir, name)); got != kept[name] {
			faults = append(faults, fmt.Sprintf("%s, the next run of print leaves %s at inode %d, which was %d: print keeps an entry that is no spec file, and copies none", at, name, got, kept[name]))
		}
	}
	return faults
}

// renamesFS is the file system of the process with the write of the head before this change in
// place of the exchange: each spec file of the new directory renamed over its name in the spec
// directory, one call each.
type renamesFS struct{ callFS }

func (r renamesFS) exchange(a, b string) error {
	list, err := os.ReadDir(a)
	if err != nil {
		return err
	}
	for _, entry := range list {
		from, to := filepath.Join(a, entry.Name()), filepath.Join(b, entry.Name())
		if err := r.call("rename", to, func() error { return os.Rename(from, to) }); err != nil {
			return err
		}
	}
	return nil
}

// stopClaims is n statements, claim-aa, claim-ab and on, each with its check, by id: version 2 of
// each statement of second, and version 1 of each other.
func stopClaims(n int, second ...string) map[string]string {
	documents := map[string]string{}
	for i := range n {
		id := fmt.Sprintf("claim-a%c", 'a'+i)
		version := 1
		if slices.Contains(second, id) {
			version = 2
		}
		var b strings.Builder
		b.WriteString("---\ntype: requirement\nid: " + id + "\nkind: behavior\ntext:\n")
		for line := range 3 {
			fmt.Fprintf(&b, "  - The daemon keeps each claim of the group %s, line %d of version %d.\n", id[len("claim-"):], line+1, version)
		}
		documents[id] = b.String()
		documents[id+"-check"] = "---\ntype: check\nid: " + id + "-check\nserves: " + id + "\nsubject: graph\nprocedure: query\nclaim: The graph holds each claim.\n" +
			"procedure_text:\n  - Read each claim.\nfailing_example:\n  - A graph with no claims.\n"
	}
	return documents
}

// stopTerm is the document of the term id at version, with lines lines of definition: about 46
// bytes each.
func stopTerm(id string, lines, version int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntype: term\nid: %s\nname: %s\ndefinition:\n", id, strings.ReplaceAll(id, "-", " "))
	for line := range lines {
		fmt.Fprintf(&b, "  - Line %d of %s, version %d of the text.\n", line+1, id, version)
	}
	return b.String()
}

// stopTree prints each node of documents, by id, into a new spec directory within limit, and
// answers that directory.
func stopTree(t *testing.T, documents map[string]string, limit int64) string {
	t.Helper()
	path := writeApproved(t, documents)
	dir := filepath.Join(t.TempDir(), "spec")
	var stderr strings.Builder
	if code := printApproved(io.Discard, &stderr, osFileSystem{}, path, readApprovedFile(t, path), slices.Sorted(maps.Keys(documents)), dir, printBounds{limit: limit, wait: 0}); code != 0 {
		t.Fatalf("print of the tree before the run: exit %d, stderr %q", code, stderr.String())
	}
	return dir
}

// specFilesOf is specFiles of dir, or no file for a directory that does not exist.
func specFilesOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	if _, err := os.Lstat(dir); dir == "" || errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}
	}
	return specFiles(t, dir)
}

// keptInodes is the inode of each entry directly in dir that is no spec file and no symbolic link,
// by name: none for a directory that does not exist.
func keptInodes(t *testing.T, dir string) map[string]uint64 {
	t.Helper()
	kept := map[string]uint64{}
	list, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return kept
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range list {
		if spec := entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), specgraph.FileSuffix); !spec && entry.Type()&fs.ModeSymlink == 0 {
			kept[entry.Name()] = inodeOf(t, filepath.Join(dir, entry.Name()))
		}
	}
	return kept
}

// snapshot is each entry under root, by its path from root: its mode, inode, number of links, owner
// and group, and the hash of the text of a regular file or the target of a symbolic link. A change
// of any of them is a change of the tree.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	all := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		desc := fmt.Sprintf("%v inode %d links %d owner %d:%d", info.Mode(), st.Ino, st.Nlink, st.Uid, st.Gid)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			desc += " to " + target
		case info.Mode().IsRegular():
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			h := fnv.New64a()
			h.Write(content)
			desc += fmt.Sprintf(" text of %d bytes, hash %x", len(content), h.Sum64())
		}
		name, _ := filepath.Rel(root, path)
		all[name] = desc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// changeOf names the first entry, by its path, that differs between the snapshots was and now.
func changeOf(was, now map[string]string) string {
	names := maps.Clone(was)
	maps.Copy(names, now)
	for _, name := range slices.Sorted(maps.Keys(names)) {
		if was[name] != now[name] {
			return fmt.Sprintf("%s was %q and is %q", name, was[name], now[name])
		}
	}
	return "no entry"
}

// filesState is one word for each spec file that files, before or after holds: how files holds it.
func filesState(files, before, after map[string]string) string {
	names := map[string]bool{}
	for _, m := range []map[string]string{files, before, after} {
		for name := range m {
			names[name] = true
		}
	}
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(names)) {
		text, held := files[name]
		state := "neither"
		switch {
		case !held:
			state = "absent"
		case text == before[name] && text == after[name]:
			state = "unchanged"
		case text == before[name]:
			state = "as before"
		case text == after[name]:
			state = "as after"
		}
		parts = append(parts, name+" "+state)
	}
	return strings.Join(parts, ", ")
}
