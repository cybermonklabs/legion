package specgraph

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// Tie is the files that a check's test reads (R18), as repository paths in three forms: a path,
// "<dir>/*" (the files directly in dir) and "<dir>/**" (every file under dir). Known is false when
// no tie could be derived, and Why says why (R18: "A tie that Dispatch cannot derive reads as
// unknown, not as none").
type Tie struct {
	Paths []string `input:"zero: a tie that is not known has no path; Input.check refuses a known tie with none, and a tie that is not known with one"`
	Known bool     `input:"zero: a tie that is not known, which carries no result (R18); Input.check refuses one that holds a path"`
	Why   string   `input:"zero: no reason is given; no rule reads it"`
}

// NewTie is the known tie of paths, sorted and unique. It refuses an entry in any other form, naming
// it: an entry is relative, its parts are separated by "/", none of them is empty, "." or "..",
// and none holds "\" or "*", except a last part "*" or "**" after a directory. It also refuses a
// tie of no path, which would read as none: a test reads at least its own file.
func NewTie(paths []string) (Tie, error) {
	if err := tiePathsFault(paths); err != nil {
		return Tie{}, err
	}
	return Tie{Paths: slices.Compact(slices.Sorted(slices.Values(paths))), Known: true}, nil
}

// tiePathsFault is the fault of paths that NewTie refuses, or nil. The entry of the derivation
// refuses a known Tie by it too, since a known tie with no path would cover no change.
func tiePathsFault(paths []string) error {
	if len(paths) == 0 {
		return errors.New("specgraph: a tie holds no path")
	}
	for _, entry := range paths {
		if !isTieEntry(entry) {
			return fmt.Errorf("specgraph: tie entry %s is not a repository path, <dir>/* or <dir>/**", quoted(entry))
		}
	}
	return nil
}

// CommitTies is the tie of each check at each commit, by commit and then by check id, since a tie
// is a fact of a commit (R18: "At each commit, Dispatch derives four ties"). A check that the map
// of a commit does not hold has a tie that is not known at that commit, and so has each check at a
// commit that it does not hold.
type CommitTies map[string]map[string]Tie

// fault is the first fault of ties, by commit and then by check, that ReadCommitTies and the entry
// of the derivation refuse, or nil: a commit or a check of no letter and no digit, which names none
// (hasWord), as the reader refuses such a commit of a subject, so the tie of a check would be under
// a name that no result reads and a carry would end nowhere; a commit whose map is nil, since a
// commit where no tie is known has {} or no key; a known tie that NewTie refuses, since a known tie
// with no path covers no change; and a tie that is not known and holds a path, which says two
// things that cannot both be true, so a caller that wrote the paths and left Known out would read
// as one with no tie (R18: "A tie that Dispatch cannot derive reads as unknown, not as none"). A
// Tie{} is a tie that is not known.
func (ties CommitTies) fault() error {
	for _, commit := range slices.Sorted(maps.Keys(ties)) {
		switch {
		case !hasWord(commit):
			return fmt.Errorf("commit %s: a commit of no letter and no digit names no commit", quoted(commit))
		case ties[commit] == nil:
			return fmt.Errorf("commit %s: it sets no map of the ties there, and a commit where no tie is known has an empty map or no key", quoted(commit))
		}
		for _, check := range slices.Sorted(maps.Keys(ties[commit])) {
			tie, fault := ties[commit][check], error(nil)
			switch {
			case !hasWord(check):
				fault = errors.New("a check of no letter and no digit names no check")
			case tie.Known:
				fault = tiePathsFault(tie.Paths)
			case len(tie.Paths) > 0:
				fault = errors.New("a tie that is not known holds no path, and a tie that holds one is known")
			}
			if fault != nil {
				return fmt.Errorf("commit %s: check %s: %w", quoted(commit), quoted(check), fault)
			}
		}
	}
	return nil
}

// commitTiesForm is the form of a scenario's ties.json, which ReadCommitTies reads.
const commitTiesForm = `{"<commit>":{"<check id>":["<path or pattern>", …]}}`

// ReadCommitTies reads ties in the form of a scenario's ties.json (commitTiesForm), each list of a
// check as the known tie of its entries, sorted and unique, as NewTie gives it. It reads the JSON
// with decodeJSON, the package's one reader of a JSON file of a format, so it refuses what that
// reader refuses, and names commitTiesForm and the place of a value of another type, such as an
// array in the place of a commit's object, which is ties by check alone; then it refuses what
// CommitTies.fault refuses, the rule of the entry of the derivation: a commit or a check of no
// letter and no digit, and a tie that NewTie refuses, naming the commit and the check.
func ReadCommitTies(r io.Reader) (CommitTies, error) {
	var paths map[string]map[string][]string
	if err := decodeJSONFrom(r, &paths, commitTiesForm); err != nil {
		return nil, err
	}
	ties := make(CommitTies, len(paths))
	for commit, byCheck := range paths {
		ties[commit] = make(map[string]Tie, len(byCheck))
		for check, entries := range byCheck {
			ties[commit][check] = Tie{Paths: slices.Compact(slices.Sorted(slices.Values(entries))), Known: true}
		}
	}
	if err := ties.fault(); err != nil {
		return nil, err
	}
	return ties, nil
}

// isRepoPath is whether path is a repository path: relative, with its parts separated by "/", and
// none of them empty, "." or "..". So "", "./a", "/a", "a/" and "a//b" are none: no tie entry
// covers such a path, and a tree of the history that held one would hold a change that no tie
// covers. Git paths can hold "*" and "\", so a path of the history may; a tie entry may not
// (isTieEntry). NewTie and the history of a scenario both read a path by it.
func isRepoPath(path string) bool {
	for part := range strings.SplitSeq(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// isTieEntry is whether entry has one of the three forms of a tie: a repository path (isRepoPath)
// none of whose parts holds "\" or "*", alone or followed by a last part "*" or "**".
func isTieEntry(entry string) bool {
	if i := strings.LastIndexByte(entry, '/'); i >= 0 && (entry[i+1:] == "*" || entry[i+1:] == "**") {
		entry = entry[:i]
	}
	return isRepoPath(entry) && !strings.ContainsAny(entry, `*\`)
}

// Covers is whether the tie holds path: an entry equal to it, a "<dir>/*" entry of the directory
// that holds it, or a "<dir>/**" entry of a directory above it. It is false when the tie is not
// known.
func (t Tie) Covers(path string) bool {
	if !t.Known {
		return false
	}
	for _, entry := range t.Paths {
		dir, last := entry, ""
		if i := strings.LastIndexByte(entry, '/'); i >= 0 {
			dir, last = entry[:i], entry[i+1:]
		}
		rest, under := strings.CutPrefix(path, dir+"/")
		switch {
		case last == "**" && under && rest != "":
			return true
		case last == "*" && under && rest != "" && !strings.Contains(rest, "/"):
			return true
		case entry == path:
			return true
		}
	}
	return false
}
