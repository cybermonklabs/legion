package specgraph

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"strings"
)

// History is the paths whose content differs between the trees of two commits, both paths of a
// move included, as `git diff --no-renames --name-only -z from to --` gives them with no setting of
// the clone or of the machine (GitHistory); ok is false only when the history does not hold one of
// the two commits, which the derivation reads as a tie that is not known, so no failing example
// counts across them. err is a fault of the history itself, such as a directory that is no clone: a
// history that could list nothing has not read two equal trees, and the derivation then gives no
// state. It reads trees, never ancestry: main's commits are squash commits, so the commit of a
// result on a branch is no ancestor of the main commit that holds its change.
type History interface {
	Changed(from, to string) (paths []string, ok bool, err error)
}

// FixtureHistory is the history of a scenario, in the form of its history.json:
//
//	{"commits":[{"id","parent","files":{"<repository path>":"<content label>"}}]}
//
// Commits is nil when it is not set, which check refuses, and an empty list for a history of no
// commit. Files is the whole tree of each commit, as a label of each path's content: a path that
// the commit does not hold is no key. It is set at each commit and has each label nonzero, as its
// tag input says for a map (members.go), and by its own rule each label has a letter or a digit
// (hasWord), since one of none names no content: two commits with "" or " " at a path would read
// as one content, and a change of a label to " " would read as a change of the file. By its own
// rule, a tree also holds at least one path, and each path is a repository path
// (FixtureHistory.check), since a test reads at least one file, two commits with no path would
// read as one tree, and no tie covers a path such as "./a". A path of one space is a repository
// path, as git holds one: a tree that holds a file " " lists it as such. Parent is for the reader,
// and Changed never reads it.
type FixtureHistory struct {
	Commits []FixtureCommit `json:"commits" input:"present: [] is no commit, so no result carries"`
}

// FixtureCommit is one commit of a FixtureHistory: its id, its parent and its tree.
type FixtureCommit struct {
	ID     string            `json:"id" input:"nonzero"`
	Parent string            `json:"parent" input:"zero: a commit with no parent; Changed never reads it"`
	Files  map[string]string `json:"files" input:"nonzero"`
}

// historyForm is the form of history.json, with the type of each value, as ReadFixtureHistory
// names it for a value of another type.
const historyForm = `{"commits":[{"id":"<commit>","parent":"<commit>","files":{"<repository path>":"<content label>"}}, …]}`

// ReadFixtureHistory reads a history.json. It refuses what decodeJSON refuses, a value of another
// type than historyForm needs named by its place among them, and what FixtureHistory.check
// refuses, a file that does not set commits among them.
func ReadFixtureHistory(r io.Reader) (FixtureHistory, error) {
	var h FixtureHistory
	if err := decodeJSONFrom(r, &h, historyForm); err != nil {
		return FixtureHistory{}, fmt.Errorf("specgraph: history: %w", err)
	}
	if err := h.check(); err != nil {
		return FixtureHistory{}, err
	}
	return h, nil
}

// check is the first fault of h that ReadFixtureHistory and the entry of the derivation refuse,
// or nil: no list of commits, a commit with no id, or with an id or a parent of no letter and no
// digit (hasWord), which names no commit, an id on two commits, and then a commit that sets no
// files, whose tree holds no path, or whose tree has a path that is no repository path
// (isRepoPath) or a content label of no letter and no digit (hasWord), which names no content,
// the least such path by its order being the one named. Each would read as a tree or a content
// that another such commit shares, or as a change that no tie covers, so a failing example would
// count across them (deriveChecks, step 7); and a history that does not set its commits is not one
// that knows that it has none.
func (h FixtureHistory) check() error {
	if h.Commits == nil {
		return errors.New("specgraph: history: it sets no commits; a history of no commit sets an empty list")
	}
	seen := map[string]bool{}
	for _, c := range h.Commits {
		switch {
		case c.ID == "":
			return errors.New("specgraph: history: a commit has no id")
		case !hasWord(c.ID):
			return fmt.Errorf("specgraph: history: commit %s: its id has no letter and no digit, so it names no commit", quoted(c.ID))
		case seen[c.ID]:
			return fmt.Errorf("specgraph: history: commit %s is in the history two times", quoted(c.ID))
		}
		if fault := nameFault(namedMember{"parent", "commit", c.Parent}); fault != "" {
			return fmt.Errorf("specgraph: history: commit %s: %s", quoted(c.ID), fault)
		}
		seen[c.ID] = true
	}
	for _, c := range h.Commits {
		switch {
		case c.Files == nil:
			return fmt.Errorf("specgraph: history: commit %s: it sets no files", quoted(c.ID))
		case len(c.Files) == 0:
			return fmt.Errorf("specgraph: history: commit %s: its files hold no path, and the tree of a commit holds at least one file", quoted(c.ID))
		}
		least, found := "", false
		for path, label := range c.Files {
			if (!hasWord(label) || !isRepoPath(path)) && (!found || path < least) {
				least, found = path, true
			}
		}
		switch {
		case !found:
		case !isRepoPath(least):
			return fmt.Errorf(`specgraph: history: commit %s: the path %s is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`, quoted(c.ID), quoted(least))
		default:
			return fmt.Errorf("specgraph: history: commit %s: the content label of %s has no letter and no digit, so it names no content", quoted(c.ID), quoted(least))
		}
	}
	return nil
}

// historyFault is the fault of h that the entry of the derivation refuses, or nil: no History, since
// a caller that gives none is not one that knows that it has none, and a nil value of any type that
// implements History, whose Changed could panic where a result carries, checked once for the value
// whatever its type; a FixtureHistory that FixtureHistory.check refuses; and a GitHistory with no
// Dir, with which git would read the clone of the working directory of the process. A caller with no
// history gives FixtureHistory{Commits: []FixtureCommit{}}, which holds neither commit of a carry,
// so no result carries.
func historyFault(h History) error {
	const none = "an Input with no history sets FixtureHistory{Commits: []FixtureCommit{}}"
	if h == nil {
		return errors.New("specgraph: it sets no History; " + none)
	}
	switch v := reflect.ValueOf(h); v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		if v.IsNil() {
			return fmt.Errorf("specgraph: history: a nil %T is no history; %s", h, none)
		}
	}
	switch h := h.(type) {
	case FixtureHistory:
		return h.check()
	case *FixtureHistory:
		return h.check()
	case GitHistory:
		return h.dirFault()
	case *GitHistory:
		return h.dirFault()
	}
	return nil
}

// Changed is the sorted paths whose label differs between the trees of from and to, or that one
// of the trees does not hold, so a move gives both of its paths; ok is false when the history
// holds no commit with one of the two ids. A FixtureHistory gives no error. Each call builds the
// tree of each commit by its id (trees) and reads the two trees there; the judge builds them once
// instead (fixtureTrees, newCheckJudge), so that its many calls take time that grows with the
// history and not with its square.
func (h FixtureHistory) Changed(from, to string) ([]string, bool, error) {
	return h.trees().Changed(from, to)
}

// fixtureTrees is the tree of each commit of a FixtureHistory by its id, whose ids are unique
// (FixtureHistory.check): the history as the judge reads it, each answer of Changed reading two
// trees by their ids.
type fixtureTrees map[string]map[string]string

// trees is the tree of each commit of h by its id.
func (h FixtureHistory) trees() fixtureTrees {
	trees := make(fixtureTrees, len(h.Commits))
	for _, c := range h.Commits {
		trees[c.ID] = c.Files
	}
	return trees
}

// Changed is FixtureHistory.Changed of the history whose trees t holds, the one body of both.
func (t fixtureTrees) Changed(from, to string) ([]string, bool, error) {
	a, okFrom := t[from]
	b, okTo := t[to]
	if !okFrom || !okTo {
		return nil, false, nil
	}
	return pathsChanged(a, b), true, nil
}

// pathsChanged is the sorted paths whose label differs between the trees a and b, or that one of
// them does not hold.
func pathsChanged(a, b map[string]string) []string {
	var changed []string
	for path, label := range a {
		if other, held := b[path]; !held || other != label {
			changed = append(changed, path)
		}
	}
	for path := range b {
		if _, held := a[path]; !held {
			changed = append(changed, path)
		}
	}
	slices.Sort(changed)
	return changed
}

// GitHistory is the history of the git clone at Dir.
type GitHistory struct {
	Dir string `input:"nonzero"`
}

// dirFault is the refusal of g with no Dir, or nil.
func (g GitHistory) dirFault() error {
	if g.Dir == "" {
		return errors.New("specgraph: history: a GitHistory sets no Dir, so git would read the clone of the working directory of the process")
	}
	return nil
}

// gitDiffArgs are the arguments of git that Changed runs before its two commits, each one there so
// that no setting of the clone or of the machine changes the answer (TestGitHistoryIsTheSameUnderEachSetting):
//
//   - --no-renames: with diff.renames, a move would give its new path alone;
//   - --ignore-submodules=none: ignore = all for a submodule in .gitmodules or in the config, or
//     diff.ignoreSubmodules, would leave out a moved submodule;
//   - --no-relative: with diff.relative, a Dir under the top of the clone would give only its own
//     paths, relative to it;
//   - --name-only -z: each path as the tree writes it, whatever core.quotePath, with no quote of a
//     letter that is not ASCII, a blank, a quote or a line feed;
//   - --end-of-options: an id that starts with "-" is a revision, never an option.
//
// It passes no flag for diff.external, a textconv of .gitattributes or color: git runs none of them
// for a list of names, which a row of the test holds. Changed runs git through command, which adds
// --no-replace-objects and runs git with no variable GIT_* of the environment.
var gitDiffArgs = []string{"diff", "--no-renames", "--ignore-submodules=none", "--no-relative",
	"--name-only", "-z", "--end-of-options"}

// Changed gives the paths that `git -C <Dir> --no-replace-objects <gitDiffArgs> <from> <to> --`
// lists, sorted, once the clone holds a commit of each of the two ids (holds). ok is false, and
// err nil, when it does not, as for a commit beyond the depth of a shallow clone. err names Dir and
// git's own reason when git cannot answer, as for a directory that is no clone: such a history has
// not read two equal trees.
// The check comes first because git diff outside a clone runs in its no-index mode, which reads
// two paths of the directory and not two commits, so only a clone is asked for a diff.
func (g GitHistory) Changed(from, to string) ([]string, bool, error) {
	fault := func(err error) error {
		return fmt.Errorf("specgraph: history: git cannot read the commits %s and %s of the clone at %s: %s",
			quoted(from), quoted(to), quoted(g.Dir), gitReason(err))
	}
	switch held, err := g.holds(from, to); {
	case err != nil:
		return nil, false, fault(err)
	case !held:
		return nil, false, nil
	}
	out, err := g.git("", slices.Concat(gitDiffArgs, []string{from, to, "--"})...)
	if err != nil {
		return nil, false, fault(err)
	}
	changed := strings.Split(string(bytes.TrimSuffix(out, []byte{0})), "\x00")
	if len(out) == 0 {
		changed = nil
	}
	slices.Sort(changed)
	return changed, true, nil
}

// holds is whether the clone at Dir holds a commit of each of ids, as `git cat-file --batch-check`
// answers for each "<id>^{commit}" on its input: "<object> <type> <size>" for one that it holds,
// and "<id>^{commit} missing" for one that it does not. An id with a line feed is the name of no
// object. git's own error, such as for a directory that is no clone, is err.
func (g GitHistory) holds(ids ...string) (bool, error) {
	var input strings.Builder
	for _, id := range ids {
		if strings.Contains(id, "\n") {
			return false, nil
		}
		input.WriteString(id)
		input.WriteString("^{commit}\n")
	}
	out, err := g.git(input.String(), "cat-file", "--batch-check")
	if err != nil {
		return false, err
	}
	answers := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(answers) != len(ids) {
		return false, fmt.Errorf("git cat-file --batch-check gave %d answers for %d commits", len(answers), len(ids))
	}
	for _, answer := range answers {
		if fields := strings.Fields(answer); len(fields) != 3 || fields[1] != "commit" {
			return false, nil
		}
	}
	return true, nil
}

// command is `git -C <Dir> --no-replace-objects <args>`, run with the environment that it would
// inherit (exec.Cmd.Environ) less each variable GIT_*. Each git command of this package runs
// through it, so no setting of the process changes which objects git reads:
//
//   - --no-replace-objects: a replace ref would put another commit, and its tree, in the place of
//     one, in what git diff, cat-file, ls-tree and read-tree read;
//   - no variable GIT_*: a variable such as GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE or
//     GIT_OBJECT_DIRECTORY would make git read another clone, another tree or another index than
//     the one that its command names, and GIT_CONFIG_* other settings.
func (g GitHistory) command(args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", g.Dir, "--no-replace-objects"}, args...)...)
	cmd.Env = slices.DeleteFunc(cmd.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") })
	return cmd
}

// git runs command(args) with stdin as its input, and gives its output.
func (g GitHistory) git(stdin string, args ...string) ([]byte, error) {
	cmd := g.command(args...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.Output()
}

// gitReason is the reason of err, an error of a run of git: the first line that git wrote on its
// stderr, as quoted prints it, or err itself when git wrote none or did not run.
func gitReason(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if line, _, _ := strings.Cut(strings.TrimSpace(string(exit.Stderr)), "\n"); line != "" {
			return quoted(line)
		}
	}
	return quoted(err.Error())
}
