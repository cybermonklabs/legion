package shim

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// freshnessFixture is what the pinned jj printed for the freshness check's commands in one state
// of a workspace (testdata/freshness/<state>/), each file's comment line, which names the command
// and the jj that printed it, taken off: the working copy's record (`jj debug working-copy`), the
// head operation (`jj op log -n1`), and where captured, @'s commit dump (`jj debug object
// commit`) and the commit id that command named.
type freshnessFixture struct {
	workingCopy, head, commitID, commit string
}

func readFreshnessFixture(t *testing.T, state string) freshnessFixture {
	t.Helper()
	read := func(name string) (comment, body string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", "freshness", state, name))
		if errors.Is(err, os.ErrNotExist) {
			return "", ""
		}
		if err != nil {
			t.Fatal(err)
		}
		comment, body, ok := strings.Cut(string(data), "\n")
		if !ok || !strings.HasPrefix(comment, "# jj ") || !strings.Contains(comment, "printed by jj ") {
			t.Fatalf("testdata/freshness/%s/%s opens with %q, want a comment line naming the command and the jj that printed it", state, name, comment)
		}
		return comment, strings.TrimSpace(body)
	}
	var f freshnessFixture
	_, f.workingCopy = read("working-copy.txt")
	_, f.head = read("op-head.txt")
	comment, commit := read("commit.txt")
	f.commit = commit
	if commit != "" {
		// The comment names the command that printed the dump, and so @'s commit id.
		words := strings.Fields(comment)
		i := slices.Index(words, "commit")
		if i < 0 || i+1 >= len(words) {
			t.Fatalf("testdata/freshness/%s/commit.txt's comment %q names no commit", state, comment)
		}
		f.commitID = words[i+1]
	}
	return f
}

// The freshness check reads what the pinned jj prints, captured from a workspace in each state
// (testdata/freshness), and decides as jj's own check does: a working copy whose recorded
// operation is the head is fresh after two reads; one whose operation is not the head is fresh
// where its recorded tree ids and labels are those of @'s commit, in order, and stale where they
// are not, with no read of the operation log. The labels are spelled two ways — `labels:
// Unlabeled` and `Labeled([…])` on the working copy's side, `conflict_labels: Resolved("")` and
// `Conflicted([…])` in the commit's dump — and compared as the string literals jj printed. The
// dump spreads each value over lines, and a label or a description that holds `"`, `TreeId("` or
// `root_tree:` is one string literal, never a field of its own. A case the check cannot read, a
// command that fails or prints what does not parse, is neither fresh nor stale: the check stops
// there and answers an error that names it.
func TestTheFreshnessCheckReadsEachTreeShapeJJPrints(t *testing.T) {
	const (
		workingCopy = "debug working-copy"
		head        = "op log -n1 --no-graph -T id"
		commitID    = "log -r @ --no-graph -T commit_id"
		dump        = "debug object commit"
	)
	for _, tc := range []struct {
		name  string
		state string
		// change rewrites the captured fixture for a row the capture has no state of.
		change func(f *freshnessFixture)
		// fail is the one command that fails, "" for none.
		fail  string
		fresh bool
		ran   []string
		// unread is what the error of a check that could not read the working copy says, nil for
		// a check that read it fresh or stale.
		unread    []string
		labels    int
		resolved  bool
		treeCount int
	}{
		{name: "the recorded operation is the head", state: "fresh",
			fresh: true, ran: []string{workingCopy, head}, labels: 1, resolved: true, treeCount: 1},
		{name: "a resolved, unlabeled working copy behind the head with its commit's tree and labels",
			state: "operation-behind", fresh: true, ran: []string{workingCopy, head, commitID, dump},
			labels: 1, resolved: true, treeCount: 1},
		{name: "a resolved working copy whose tree id is not its commit's",
			state: "stale", ran: []string{workingCopy, head, commitID, dump}, labels: 1, resolved: true, treeCount: 1},
		{name: "a conflicted working copy with its commit's three tree ids and labels",
			state: "conflicted", fresh: true, ran: []string{workingCopy, head, commitID, dump}, labels: 3, treeCount: 3},
		{name: "a conflicted working copy one of whose labels is not its commit's",
			state: "conflicted",
			change: func(f *freshnessFixture) {
				f.commit = strings.Replace(f.commit, `\"the other side\"`, `\"another side\"`, 1)
			},
			ran: []string{workingCopy, head, commitID, dump}, labels: 3, treeCount: 3},
		{name: `labels and a description that hold ", TreeId(" and root_tree:`,
			state: "labels-quoted", fresh: true, ran: []string{workingCopy, head, commitID, dump}, labels: 3, treeCount: 3},
		{name: "a working copy record that does not parse", state: "operation-behind",
			change: func(f *freshnessFixture) {
				f.workingCopy = "Type: \"local\"\nCurrent operation: an operation of another shape"
			},
			ran: []string{workingCopy}, unread: []string{"jj debug working-copy printed what does not parse", `its first line: "Type: \"local\""`}},
		{name: "a commit dump that does not parse", state: "operation-behind",
			change: func(f *freshnessFixture) {
				f.commit = strings.Replace(f.commit, "root_tree: Resolved(", "root_tree: Resolved[", 1)
			},
			ran: []string{workingCopy, head, commitID, dump}, unread: []string{"jj debug object commit", "printed what does not parse", `its first line: "Commit {"`}},
		{name: "a command of the check that fails", state: "operation-behind", fail: head,
			ran: []string{workingCopy, head}, unread: []string{"Command failed (exit 1): jj op log -n1 --no-graph -T id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readFreshnessFixture(t, tc.state)
			if tc.change != nil {
				tc.change(&f)
			}
			var ran []string
			read := func(args ...string) (string, error) {
				command := strings.Join(args, " ")
				if strings.HasPrefix(command, dump+" ") {
					if got := strings.TrimPrefix(command, dump+" "); got != f.commitID {
						t.Fatalf("the check dumped commit %s, want @'s, %s", got, f.commitID)
					}
					command = dump
				}
				ran = append(ran, command)
				if command == tc.fail {
					return "", errors.New("Command failed (exit 1): jj " + command)
				}
				switch command {
				case workingCopy:
					return f.workingCopy, nil
				case head:
					return f.head, nil
				case commitID:
					return f.commitID, nil
				case dump:
					return f.commit, nil
				}
				t.Fatalf("the check ran jj %s, which is none of its commands", command)
				return "", nil
			}
			got, err := readFreshness(read)
			if !slices.Equal(ran, tc.ran) {
				t.Errorf("the check ran %q, want %q", ran, tc.ran)
			}
			if tc.unread != nil {
				if err == nil {
					t.Fatalf("the check read the working copy fresh=%v, want an error saying %q", got.fresh, tc.unread)
				}
				for _, want := range tc.unread {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the check's error %q does not say %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("the check could not read the working copy: %v", err)
			}
			if got.fresh != tc.fresh {
				t.Errorf("the check read the working copy fresh=%v, want %v", got.fresh, tc.fresh)
			}
			recorded, tree, err := parseWorkingCopy(f.workingCopy)
			if err != nil || got.operation != recorded || got.head != f.head {
				t.Fatalf("the check answered the recorded operation %q and the head %q, want %q and %q (%v)", got.operation, got.head, recorded, f.head, err)
			}
			if len(tree.ids) != tc.treeCount || len(tree.labels) != tc.labels || tree.resolved != tc.resolved {
				t.Errorf("the working copy's record read as %d tree ids and %d labels (resolved %v), want %d and %d (resolved %v)",
					len(tree.ids), len(tree.labels), tree.resolved, tc.treeCount, tc.labels, tc.resolved)
			}
			if f.commit == "" || tc.change != nil {
				return
			}
			// Each label is one string literal and root_tree the one field of that name, whatever
			// the labels and the description hold.
			current, err := parseCommit(f.commit)
			if err != nil {
				t.Fatalf("parse the captured commit dump: %v", err)
			}
			if len(current.ids) != tc.treeCount || len(current.labels) != tc.labels || current.resolved != tc.resolved {
				t.Errorf("the commit's dump read as %d tree ids and %d labels (resolved %v), want %d and %d (resolved %v)",
					len(current.ids), len(current.labels), current.resolved, tc.treeCount, tc.labels, tc.resolved)
			}
			if tc.state != "stale" && !current.equal(tree) {
				t.Errorf("the commit's tree and labels %+v are not the working copy's %+v", current, tree)
			}
		})
	}
}
