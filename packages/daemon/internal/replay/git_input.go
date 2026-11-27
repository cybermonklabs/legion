package replay

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// commitSHA is a whole commit id as git and GitHub write it: 40 lower-case hex digits.
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// The kinds of a git input's lines, each line's "kind" (GitInput).
const (
	gitCommitKind  = "commit"
	gitPullKind    = "pull"
	gitHeadKind    = "head"
	gitReadingKind = "reading"
	gitMissingKind = "missing"
	gitDoneKind    = "done"
)

// Where a head of a pull request comes from (GitHead.From): a committed event of GitHub's timeline
// of the pull request, at its committer's time; a head_ref_force_pushed event's new tip, at the
// event's time; or a read of the pull request's merge state the window's log holds
// (MergeStateRead), at the read's time, for a head the timeline does not give.
const (
	GitHeadCommit    = "commit"
	GitHeadForcePush = "force-push"
	GitHeadRead      = "read"
)

// The reasons a git input gives for an input the collector could not get (GitMissing.Reason):
// GitHub has no such pull request in the repository; GitHub still refuses to read the pull request
// after the reader's tries, with its 422 of a pull request whose diff it cannot compute for now
// (tooManyFiles); GitHub's timeline of the pull request does not read as one, or names a head it
// gives no commit of; the remote holds no head, or no base commit, the collector fetched; git
// failed; or the guard killed git past its memory cap or its time (gitGuard).
const (
	GitPullNotFound    = "pull-not-found"
	GitPullUnread      = "pull-unread"
	GitTimelineUnread  = "timeline-unread"
	GitHeadUnreachable = "head-unreachable"
	GitBaseUnreachable = "base-unreachable"
	GitError           = "git-error"
	GitMemoryCap       = "memory-cap"
	GitTimeout         = "timeout"
)

// gitReasons is every reason a missing line may give.
var gitReasons = []string{GitPullNotFound, GitPullUnread, GitTimelineUnread, GitHeadUnreachable, GitBaseUnreachable, GitError, GitMemoryCap, GitTimeout}

// githubReason says whether reason is one GitHub's reads give (collect-git's GitHub phase) rather
// than git's. GitPullUnread is GitHub's: like GitPullNotFound it leaves the pull request no pull
// line and its reads unanswered, the git phase keeps it, and a run again reads the pull request
// again (githubHeld).
func githubReason(reason string) bool {
	return reason == GitPullNotFound || reason == GitPullUnread || reason == GitTimelineUnread
}

// GitInput is a window's git/<owner>__<name>.jsonl: what git says of the pull requests of the
// window's issues on one repository and of the first-parent commits of its base branch, which
// collect-git writes (CollectGit) and a replay answers the reads of a head's merge state from.
// The file holds one JSON object per line, in blocks, each ended by {"kind":"done","pr"}, the
// line of its pull request (0 for the commits' block), so that collect-git grows the file a block
// at a time and a block a stopped write left short is known by its missing done line. First comes
// the GitHub phase's block of each pull request, in ascending number:
//
//   - {"kind":"pull","pr","merge","base","baseChanges","heads"} where GitHub gave the pull request:
//     its merge commit where the merges file holds it, else "", its current base branch, its base
//     change times in ascending order, and the count of its heads, followed by one
//     {"kind":"head","pr","sha","at","from"} line for each head, by at then sha (Pulls);
//   - then each {"kind":"missing","pr","head","base","reason","detail"} of the pull request of
//     GitHub's reasons (githubReason), sorted by head, base, reason and detail.
//
// Then, where the file holds anything of git's, git's blocks:
//
//   - the commits' block: {"kind":"commit","sha","paths"}, each commit the window's merges file
//     holds of the base branch, in that file's order, then each base commit a read names
//     (MergeStateRead.BaseSHA) that the merges file does not hold, in the log's order, with the
//     paths it changes against its first parent, sorted (Commits); then each missing line of pr 0,
//     of a commit git did not give, sorted by base, reason and detail;
//   - then the block of each pull request of a pull line that holds any line of git's, in
//     ascending number: {"kind":"reading","pr","sha","tip","mergeBase","paths","verdicts"}, git's
//     reading of each head git read (GitHead), in the order of the pull request's head lines, then
//     each missing line of the pull request of git's reasons, sorted as above.
//
// A missing input is never written as clean. In memory a missing input of either kind is in
// Missing, sorted by pr, head, base, reason and detail.
//
// ReadWindow refuses lines out of that order, a block that holds no line but its done line or
// ends with no done line of its pull request, a head line that does not follow its pull request's
// line, a pull line whose count of heads differs from the head lines after it, a reading line of
// no head of its pull line or out of their order, and git's block of a pull request of no pull
// line, so a file read is written back byte for byte (Window.Write).
type GitInput struct {
	Commits []GitCommit
	Pulls   []GitPull
	Missing []GitMissing
}

// GitCommit is a first-parent commit of the base branch and the paths it changes against its
// first parent (git diff-tree -r --no-commit-id --name-only <sha>^1 <sha>), sorted.
type GitCommit struct {
	SHA   string
	Paths []string
}

// GitPull is a pull request: its number, the commit GitHub records as its merge into the base
// branch where the window's merges file holds that commit ("" where the window's inputs hold no
// merge of it), the current base branch, the times of its base changes, and its heads.
type GitPull struct {
	Number      int
	Merge       string
	Base        string
	BaseChanges []time.Time
	Heads       []GitHead
}

// GitHead is a head of a pull request: its commit, the moment it stood from and where that moment
// comes from (GitHeadCommit, GitHeadForcePush, GitHeadRead). A committer's time only estimates
// when a commit was pushed.
//
// Where git read the head (Read), Tip is the base commit its verdicts run to: the first parent of
// the pull request's own merge commit where it merged in the window's inputs, else the last commit
// of the merges file. MergeBase is git merge-base <head> <tip>, Paths the paths the head changes
// since it (git diff --no-renames --name-only <mergeBase> <head>, sorted): the head's profile.
// Verdicts is git's verdict at each first-parent commit C of the merges file after the merge base
// up to the tip (git rev-list --first-parent <tip> ^<head>, oldest first) whose commit line shares
// a path with Paths: git's merge of the head with the base as it stands at C, so the first one
// that conflicts is the cause and each later one holds the earlier commits. A verdict git could
// not give is a missing line of the head and that base, and the verdicts stop before it. Where git
// read the head, its reading line holds the four; where it did not — the GitHub phase alone wrote
// it, or git could not — the four are empty, and the head has no reading line.
type GitHead struct {
	SHA       string
	At        time.Time
	From      string
	Tip       string
	MergeBase string
	Paths     []string
	Verdicts  []GitVerdict
}

// Read says whether git read the head: whether h holds its tip, merge base, paths and verdicts.
func (h GitHead) Read() bool { return h.Tip != "" }

// GitVerdict is git's verdict of merging a head with the base as it stands at the commit Base (git
// merge-tree --write-tree --no-messages --name-only <base> <head>): clean, or a conflict in
// PathCount distinct paths, 0 for a clean merge and at least 1 for a conflict. Which paths
// conflict is not kept: no read of the replay asks it, and a real repository's lists of them would
// hold most of the file.
type GitVerdict struct {
	Base      string `json:"base"`
	Conflict  bool   `json:"conflict"`
	PathCount int    `json:"pathCount"`
}

// GitMissing is an input the collector could not get: of the pull request PR (0 for a commit of
// the base branch), the head and the base commit it is of ("" for none), why (one of gitReasons)
// and what failed.
type GitMissing struct {
	PR     int
	Head   string
	Base   string
	Reason string
	Detail string
}

// compareGitMissing orders missing lines as a git input writes them: by pr, head, base, reason and
// detail.
func compareGitMissing(a, b GitMissing) int {
	return cmp.Or(cmp.Compare(a.PR, b.PR), strings.Compare(a.Head, b.Head), strings.Compare(a.Base, b.Base),
		strings.Compare(a.Reason, b.Reason), strings.Compare(a.Detail, b.Detail))
}

// compareGitHeads orders a pull request's heads as a git input writes them: by At, then SHA.
func compareGitHeads(a, b GitHead) int {
	return cmp.Or(a.At.Compare(b.At), strings.Compare(a.SHA, b.SHA))
}

// Commit is the commit line of sha, and whether g holds one.
func (g GitInput) Commit(sha string) (GitCommit, bool) {
	i := slices.IndexFunc(g.Commits, func(c GitCommit) bool { return c.SHA == sha })
	if i < 0 {
		return GitCommit{}, false
	}
	return g.Commits[i], true
}

// Pull is the pull request number with its heads, and whether g holds it.
func (g GitInput) Pull(number int) (GitPull, bool) {
	i, found := g.pullIndex(number)
	if !found {
		return GitPull{}, false
	}
	return g.Pulls[i], true
}

// pullIndex is the place in g.Pulls, which runs in ascending number, of the pull request number,
// and whether g holds it.
func (g GitInput) pullIndex(number int) (int, bool) {
	return slices.BinarySearchFunc(g.Pulls, number, func(p GitPull, n int) int { return cmp.Compare(p.Number, n) })
}

// holdsGit says whether g holds anything of git's, and so git's blocks (GitInput): a commit, a head
// git read, or a missing input of git's.
func (g GitInput) holdsGit() bool {
	return len(g.Commits) > 0 || slices.ContainsFunc(g.Missing, func(m GitMissing) bool { return !githubReason(m.Reason) }) ||
		slices.ContainsFunc(g.Pulls, func(p GitPull) bool { return slices.ContainsFunc(p.Heads, GitHead.Read) })
}

// The lines of a git input as it writes them, one type of each kind, its members in the order the
// line holds them.
type (
	gitCommitLine struct {
		Kind  string   `json:"kind"`
		SHA   string   `json:"sha"`
		Paths []string `json:"paths"`
	}
	gitPullLine struct {
		Kind        string      `json:"kind"`
		PR          int         `json:"pr"`
		Merge       string      `json:"merge"`
		Base        string      `json:"base"`
		BaseChanges []time.Time `json:"baseChanges"`
		Heads       int         `json:"heads"`
	}
	gitHeadLine struct {
		Kind string    `json:"kind"`
		PR   int       `json:"pr"`
		SHA  string    `json:"sha"`
		At   time.Time `json:"at"`
		From string    `json:"from"`
	}
	gitReadingLine struct {
		Kind      string       `json:"kind"`
		PR        int          `json:"pr"`
		SHA       string       `json:"sha"`
		Tip       string       `json:"tip"`
		MergeBase string       `json:"mergeBase"`
		Paths     []string     `json:"paths"`
		Verdicts  []GitVerdict `json:"verdicts"`
	}
	gitMissingLine struct {
		Kind   string `json:"kind"`
		PR     int    `json:"pr"`
		Head   string `json:"head"`
		Base   string `json:"base"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	gitDoneLine struct {
		Kind string `json:"kind"`
		PR   int    `json:"pr"`
	}
)

// nonNil is list, or an empty list where it is nil, so that its line writes [] rather than null.
func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

// gitBlock is a block of a git input (GitInput): each of values as its line, then the done line of
// the pull request pr; none where values is empty, since a block holds a line but its done line.
func gitBlock(pr int, values []any) ([]json.RawMessage, error) {
	if len(values) == 0 {
		return nil, nil
	}
	lines := make([]json.RawMessage, 0, len(values)+1)
	for _, value := range append(values, gitDoneLine{Kind: gitDoneKind, PR: pr}) {
		line, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// missingLines is each of missing as its line, sorted by pr, head, base, reason and detail, each
// once.
func missingLines(missing []GitMissing) []any {
	sorted := slices.SortedFunc(slices.Values(missing), compareGitMissing)
	sorted = slices.CompactFunc(sorted, func(a, b GitMissing) bool { return compareGitMissing(a, b) == 0 })
	values := make([]any, len(sorted))
	for i, m := range sorted {
		values[i] = gitMissingLine{Kind: gitMissingKind, PR: m.PR, Head: m.Head, Base: m.Base, Reason: m.Reason, Detail: m.Detail}
	}
	return values
}

// githubBlock is the GitHub phase's block of the pull request number (GitInput): its pull line and
// head lines where pull is not nil, then a line of each of missing, its missing inputs of GitHub's.
func githubBlock(number int, pull *GitPull, missing []GitMissing) ([]json.RawMessage, error) {
	var values []any
	if pull != nil {
		changes := make([]time.Time, len(pull.BaseChanges))
		for i, at := range pull.BaseChanges {
			changes[i] = at.UTC()
		}
		values = append(values, gitPullLine{Kind: gitPullKind, PR: pull.Number, Merge: pull.Merge, Base: pull.Base, BaseChanges: changes,
			Heads: len(pull.Heads)})
		for _, head := range pull.Heads {
			values = append(values, gitHeadLine{Kind: gitHeadKind, PR: pull.Number, SHA: head.SHA, At: head.At.UTC(), From: head.From})
		}
	}
	return gitBlock(number, append(values, missingLines(missing)...))
}

// commitBlock is git's block of the commits (GitInput): a line of each of commits, then of each of
// missing, the missing inputs of commits. Git's readings rest on the commits, so it refuses to
// write a block that holds neither.
func commitBlock(commits []GitCommit, missing []GitMissing) ([]json.RawMessage, error) {
	values := make([]any, 0, len(commits)+len(missing))
	for _, commit := range commits {
		values = append(values, gitCommitLine{Kind: gitCommitKind, SHA: commit.SHA, Paths: nonNil(commit.Paths)})
	}
	values = append(values, missingLines(missing)...)
	if len(values) == 0 {
		return nil, errors.New("git's lines hold no commit and no missing commit, which git's readings rest on")
	}
	return gitBlock(0, values)
}

// readingBlock is git's block of pull (GitInput): the reading line of each head of pull git read,
// in pull's order, then a line of each of missing, the pull request's missing inputs of git's; none
// where it holds neither.
func readingBlock(pull GitPull, missing []GitMissing) ([]json.RawMessage, error) {
	var values []any
	for _, head := range pull.Heads {
		if head.Read() {
			values = append(values, gitReadingLine{Kind: gitReadingKind, PR: pull.Number, SHA: head.SHA, Tip: head.Tip, MergeBase: head.MergeBase,
				Paths: nonNil(head.Paths), Verdicts: nonNil(head.Verdicts)})
		}
	}
	return gitBlock(pull.Number, append(values, missingLines(missing)...))
}

// lines is g as its file holds it, in the format's order (GitInput): the GitHub phase's block of
// each pull request, then, where g holds anything of git's, the commits' block and git's block of
// each pull request, each as g holds it. It refuses a missing input of GitHub's of no pull request
// and one of git's of a pull request g holds no pull line of, which no block holds.
func (g GitInput) lines() ([]json.RawMessage, error) {
	github, git := make(map[int][]GitMissing), make(map[int][]GitMissing)
	for _, m := range g.Missing {
		if githubReason(m.Reason) {
			github[m.PR] = append(github[m.PR], m)
		} else {
			git[m.PR] = append(git[m.PR], m)
		}
	}
	pulls := slices.SortedFunc(slices.Values(g.Pulls), func(a, b GitPull) int { return cmp.Compare(a.Number, b.Number) })
	numbers := make([]int, 0, len(pulls)+len(github))
	for _, pull := range pulls {
		numbers = append(numbers, pull.Number)
	}
	numbers = slices.Compact(slices.Sorted(slices.Values(append(numbers, slices.Collect(maps.Keys(github))...))))
	var lines []json.RawMessage
	add := func(block []json.RawMessage, err error) error {
		lines = append(lines, block...)
		return err
	}
	for _, number := range numbers {
		if number < 1 {
			return nil, fmt.Errorf("a missing input of GitHub's of pr %d, which is no pull request", number)
		}
		var pull *GitPull
		if i, found := slices.BinarySearchFunc(pulls, number, func(p GitPull, n int) int { return cmp.Compare(p.Number, n) }); found {
			pull = &pulls[i]
		}
		if err := add(githubBlock(number, pull, github[number])); err != nil {
			return nil, err
		}
	}
	if !g.holdsGit() {
		return lines, nil
	}
	if err := add(commitBlock(g.Commits, git[0])); err != nil {
		return nil, err
	}
	delete(git, 0)
	for _, pull := range pulls {
		if err := add(readingBlock(pull, git[pull.Number])); err != nil {
			return nil, err
		}
		delete(git, pull.Number)
	}
	if len(git) > 0 {
		return nil, fmt.Errorf("a missing input of git's of #%d, which holds no pull line", slices.Min(slices.Collect(maps.Keys(git))))
	}
	return lines, nil
}

// gitLineRead is one line of a git input as parseGitLine reads it: its kind and, by its kind, the
// commit; the pull request (no heads yet) with the count of heads its line gives; the head, or
// git's reading of it, with its pull request's number in pull; the missing input; or, for a done
// line, its pull request's number in pull.
type gitLineRead struct {
	kind    string
	commit  GitCommit
	pull    GitPull
	heads   int
	head    GitHead
	missing GitMissing
}

// gitLineMembers is the members a line of each kind holds, and gitVerdictMembers those each
// verdict of a reading line holds.
var (
	gitLineMembers = map[string][]string{
		gitCommitKind:  {"kind", "sha", "paths"},
		gitPullKind:    {"kind", "pr", "merge", "base", "baseChanges", "heads"},
		gitHeadKind:    {"kind", "pr", "sha", "at", "from"},
		gitReadingKind: {"kind", "pr", "sha", "tip", "mergeBase", "paths", "verdicts"},
		gitMissingKind: {"kind", "pr", "head", "base", "reason", "detail"},
		gitDoneKind:    {"kind", "pr"},
	}
	gitVerdictMembers = []string{"base", "conflict", "pathCount"}
)

// parseGitLine reads line, one line of a git input, as its kind gives it (GitInput). It refuses a
// line of no kind the format holds, a member missing, null, of another JSON type or that its kind
// does not hold, a verdict that lacks a member or holds one no verdict holds (its conflicting
// paths among them), a commit that is no whole commit id, a list of paths that is not sorted or
// holds a path twice, a pull request numbered below one, a time that is zero, a head of no source
// the format names, a clean verdict that counts a conflicting path, a conflict that counts none,
// and a missing input of no reason the format names.
func parseGitLine(line []byte) (gitLineRead, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(line, &members); err != nil {
		return gitLineRead{}, err
	}
	var kind string
	if err := json.Unmarshal(members["kind"], &kind); err != nil {
		return gitLineRead{}, errors.New("a git line names no kind")
	}
	want, known := gitLineMembers[kind]
	if !known {
		return gitLineRead{}, fmt.Errorf("a git line of the kind %q, which the format holds none of", kind)
	}
	for name, value := range members {
		switch {
		case !slices.Contains(want, name):
			return gitLineRead{}, fmt.Errorf("a %s line holds %q, which no such line holds", kind, name)
		case bytes.Equal(bytes.TrimSpace(value), []byte("null")):
			return gitLineRead{}, fmt.Errorf("a %s line holds a null %s", kind, name)
		}
	}
	for _, name := range want {
		if _, held := members[name]; !held {
			return gitLineRead{}, fmt.Errorf("a %s line has no %s", kind, name)
		}
	}
	parsed := gitLineRead{kind: kind}
	switch kind {
	case gitCommitKind:
		var l gitCommitLine
		if err := decodeStrict(line, &l); err != nil {
			return gitLineRead{}, err
		}
		if err := checkCommit("the commit", l.SHA, false); err != nil {
			return gitLineRead{}, err
		}
		if err := checkPaths("the paths of "+l.SHA, l.Paths); err != nil {
			return gitLineRead{}, err
		}
		parsed.commit = GitCommit{SHA: l.SHA, Paths: l.Paths}
	case gitPullKind:
		var l gitPullLine
		if err := decodeStrict(line, &l); err != nil {
			return gitLineRead{}, err
		}
		switch {
		case l.PR < 1:
			return gitLineRead{}, fmt.Errorf("a pull line numbers its pull request %d", l.PR)
		case l.Heads < 0:
			return gitLineRead{}, fmt.Errorf("the pull line of #%d counts %d heads", l.PR, l.Heads)
		case l.Base == "":
			return gitLineRead{}, fmt.Errorf("the pull line of #%d names no base branch", l.PR)
		}
		if err := checkCommit(fmt.Sprintf("the merge of #%d", l.PR), l.Merge, true); err != nil {
			return gitLineRead{}, err
		}
		for i, at := range l.BaseChanges {
			if at.IsZero() || i > 0 && at.Before(l.BaseChanges[i-1]) {
				return gitLineRead{}, fmt.Errorf("the pull line of #%d has an invalid base change time", l.PR)
			}
		}
		parsed.pull, parsed.heads = GitPull{Number: l.PR, Merge: l.Merge, Base: l.Base, BaseChanges: l.BaseChanges}, l.Heads
	case gitHeadKind:
		var l gitHeadLine
		if err := decodeStrict(line, &l); err != nil {
			return gitLineRead{}, err
		}
		switch {
		case l.PR < 1:
			return gitLineRead{}, fmt.Errorf("a head line numbers its pull request %d", l.PR)
		case l.At.IsZero():
			return gitLineRead{}, fmt.Errorf("the head %s of #%d stands from no time", l.SHA, l.PR)
		case l.From != GitHeadCommit && l.From != GitHeadForcePush && l.From != GitHeadRead:
			return gitLineRead{}, fmt.Errorf("the head %s of #%d comes from %q, which is none of %s, %s and %s", l.SHA, l.PR, l.From,
				GitHeadCommit, GitHeadForcePush, GitHeadRead)
		}
		if err := checkCommit(fmt.Sprintf("a head of #%d", l.PR), l.SHA, false); err != nil {
			return gitLineRead{}, err
		}
		parsed.pull, parsed.head = GitPull{Number: l.PR}, GitHead{SHA: l.SHA, At: l.At, From: l.From}
	case gitReadingKind:
		var head string
		_ = json.Unmarshal(members["sha"], &head)
		if err := checkVerdictMembers(head, members["verdicts"]); err != nil {
			return gitLineRead{}, err
		}
		var l gitReadingLine
		if err := decodeStrict(line, &l); err != nil {
			return gitLineRead{}, err
		}
		if l.PR < 1 {
			return gitLineRead{}, fmt.Errorf("a reading line numbers its pull request %d", l.PR)
		}
		for _, commit := range []struct{ what, sha string }{{fmt.Sprintf("the head of a reading line of #%d", l.PR), l.SHA},
			{"the tip of the head " + l.SHA, l.Tip}, {"the merge base of the head " + l.SHA, l.MergeBase}} {
			if err := checkCommit(commit.what, commit.sha, false); err != nil {
				return gitLineRead{}, err
			}
		}
		if err := checkPaths("the paths of the head "+l.SHA, l.Paths); err != nil {
			return gitLineRead{}, err
		}
		if err := checkVerdicts(l.SHA, l.Verdicts); err != nil {
			return gitLineRead{}, err
		}
		parsed.pull, parsed.head = GitPull{Number: l.PR}, GitHead{SHA: l.SHA, Tip: l.Tip, MergeBase: l.MergeBase, Paths: l.Paths, Verdicts: l.Verdicts}
	case gitMissingKind:
		var l gitMissingLine
		if err := decodeStrict(line, &l); err != nil {
			return gitLineRead{}, err
		}
		if l.PR < 0 {
			return gitLineRead{}, fmt.Errorf("a missing line numbers its pull request %d", l.PR)
		}
		if !slices.Contains(gitReasons, l.Reason) {
			return gitLineRead{}, fmt.Errorf("a missing line gives the reason %q, which is none of %s", l.Reason, strings.Join(gitReasons, ", "))
		}
		for what, sha := range map[string]string{"head": l.Head, "base": l.Base} {
			if err := checkCommit("the missing input's "+what, sha, true); err != nil {
				return gitLineRead{}, err
			}
		}
		parsed.missing = GitMissing{PR: l.PR, Head: l.Head, Base: l.Base, Reason: l.Reason, Detail: l.Detail}
	case gitDoneKind:
		var l gitDoneLine
		if err := decodeStrict(line, &l); err != nil {
			return gitLineRead{}, err
		}
		if l.PR < 0 {
			return gitLineRead{}, fmt.Errorf("a done line numbers its pull request %d", l.PR)
		}
		parsed.pull = GitPull{Number: l.PR}
	}
	return parsed, nil
}

// checkCommit refuses sha, which what names, where it is no whole commit id, and where it is empty
// unless empty says it may be.
func checkCommit(what, sha string, empty bool) error {
	if (sha == "" && empty) || commitSHA.MatchString(sha) {
		return nil
	}
	return fmt.Errorf("%s is %q, no whole commit id", what, sha)
}

// checkPaths refuses paths, which what names, where a path is empty or they are not sorted, each
// once.
func checkPaths(what string, paths []string) error {
	for i, p := range paths {
		if p == "" || (i > 0 && paths[i-1] >= p) {
			return fmt.Errorf("%s are not each a path, sorted, each once: %q", what, paths)
		}
	}
	return nil
}

// checkVerdictMembers refuses verdicts, the verdicts member of a reading line of the head sha,
// where a verdict holds a member no verdict holds, the list of its conflicting paths among them,
// or lacks one; a member that is no list of objects is left to the line's decoding to refuse.
func checkVerdictMembers(sha string, verdicts json.RawMessage) error {
	var raw []map[string]json.RawMessage
	if json.Unmarshal(verdicts, &raw) != nil {
		return nil
	}
	for i, members := range raw {
		for _, name := range slices.Sorted(maps.Keys(members)) {
			if !slices.Contains(gitVerdictMembers, name) {
				return fmt.Errorf("verdict %d of the head %s holds %q, which no verdict holds: a verdict holds %s, and counts its conflicting paths in pathCount",
					i+1, sha, name, strings.Join(gitVerdictMembers, ", "))
			}
		}
		for _, name := range gitVerdictMembers {
			if value, held := members[name]; !held || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("verdict %d of the head %s has no %s", i+1, sha, name)
			}
		}
	}
	return nil
}

// checkVerdicts refuses the verdicts of the head sha where a verdict names no whole commit id, is
// clean and counts a conflicting path, or is a conflict and counts none.
func checkVerdicts(sha string, verdicts []GitVerdict) error {
	for _, verdict := range verdicts {
		if err := checkCommit("a verdict's base of the head "+sha, verdict.Base, false); err != nil {
			return err
		}
		switch {
		case !verdict.Conflict && verdict.PathCount != 0:
			return fmt.Errorf("the clean verdict of the head %s at %s counts %d conflicting paths, and a clean merge has none", sha, verdict.Base,
				verdict.PathCount)
		case verdict.Conflict && verdict.PathCount < 1:
			return fmt.Errorf("the conflict of the head %s at %s counts %d conflicting paths, and a conflict has at least one", sha, verdict.Base,
				verdict.PathCount)
		}
	}
	return nil
}

// gitReading assembles a git input from its lines in the file's order (add), and is the input once
// every line is added (input). The first line out of the format's order (GitInput) refuses it: a
// line of the GitHub phase's after git's lines began, a block that begins before the one before it
// ends or ends with another pull request's done line or none, a block of a pull request at or
// below the one before it, a head line that does not follow its pull request's line or comes past
// the count of heads that line gives, a commit or a head given twice, a reading line of no head of
// its pull request or before the reading of a later head, and a missing line out of its block's
// order.
type gitReading struct {
	name  string
	in    GitInput
	lines int
	// git says whether git's blocks began, with the commits' block, and commitsDone whether that
	// block ended; last is the pull request of the last block that ended, of the GitHub phase's
	// blocks, or of git's since the commits' block.
	git, commitsDone bool
	last             int
	// The block under way, where open: its pull request pr, the count of heads its pull line gives
	// (-1 where it holds none), the place after the last head a reading line named, and its last
	// missing input.
	open    bool
	pr      int
	heads   int
	read    int
	missing *GitMissing
	commits map[string]bool
	err     error
}

// begin opens the block of the pull request pr (0 for the commits' block).
func (r *gitReading) begin(pr int) {
	r.open, r.pr, r.heads, r.read, r.missing = true, pr, -1, 0, nil
}

// headsShort says whether the block under way holds a pull line and fewer head lines after it than
// that line counts.
func (r *gitReading) headsShort() bool {
	return r.open && r.heads >= 0 && len(r.in.Pulls[len(r.in.Pulls)-1].Heads) < r.heads
}

// add takes the next line of the input, read as parseGitLine reads it.
func (r *gitReading) add(line gitLineRead) {
	if r.err != nil {
		return
	}
	r.lines++
	refuse := func(format string, args ...any) {
		r.err = fmt.Errorf("%s line %d: %s", r.name, r.lines, fmt.Sprintf(format, args...))
	}
	number := line.pull.Number
	switch line.kind {
	case gitPullKind:
		switch {
		case r.git:
			refuse("the pull line of #%d after git's lines began", number)
		case r.open:
			refuse("the pull line of #%d inside the block of #%d", number, r.pr)
		case number <= r.last:
			refuse("the pull line of #%d after the block of #%d", number, r.last)
		default:
			r.begin(number)
			r.heads = line.heads
			r.in.Pulls = append(r.in.Pulls, line.pull)
		}
	case gitHeadKind:
		if r.git || !r.open || r.heads < 0 || r.pr != number || r.missing != nil {
			refuse("a head line of #%d that does not follow its pull line", number)
			return
		}
		pull := &r.in.Pulls[len(r.in.Pulls)-1]
		switch n := len(pull.Heads); {
		case n == r.heads:
			refuse("a head of #%d past the %d its pull line counts", number, r.heads)
		case slices.ContainsFunc(pull.Heads, func(h GitHead) bool { return h.SHA == line.head.SHA }):
			refuse("the head %s of #%d a second time", line.head.SHA, number)
		case n > 0 && compareGitHeads(pull.Heads[n-1], line.head) >= 0:
			refuse("the head %s of #%d out of the order of at, then sha", line.head.SHA, number)
		default:
			pull.Heads = append(pull.Heads, line.head)
		}
	case gitCommitKind:
		switch sha := line.commit.SHA; {
		case r.commitsDone:
			refuse("the commit %s after the commits' block", sha)
		case r.open && (!r.git || r.missing != nil):
			refuse("the commit %s inside the block of #%d or after a missing commit", sha, r.pr)
		case r.commits[sha]:
			refuse("the commit %s a second time", sha)
		default:
			if !r.git {
				r.git = true
				r.begin(0)
			}
			if r.commits == nil {
				r.commits = make(map[string]bool)
			}
			r.commits[sha] = true
			r.in.Commits = append(r.in.Commits, line.commit)
		}
	case gitReadingKind:
		i, held := r.in.pullIndex(number)
		switch {
		case !r.commitsDone:
			refuse("a reading line of #%d before the commits' block ends", number)
		case r.open && r.pr != number:
			refuse("a reading line of #%d inside the block of #%d", number, r.pr)
		case r.open && r.missing != nil:
			refuse("a reading line of #%d after a missing line of its block", number)
		case !r.open && number <= r.last:
			refuse("git's block of #%d after that of #%d", number, r.last)
		case !held:
			refuse("a reading line of #%d, which holds no pull line", number)
		default:
			pull := &r.in.Pulls[i]
			j := slices.IndexFunc(pull.Heads, func(h GitHead) bool { return h.SHA == line.head.SHA })
			if j < 0 {
				refuse("a reading line of the head %s, which no head line of #%d names", line.head.SHA, number)
				return
			}
			if !r.open {
				r.begin(number)
			}
			if j < r.read {
				refuse("the reading of the head %s of #%d out of the order of its head lines, or a second time", line.head.SHA, number)
				return
			}
			r.read = j + 1
			h := &pull.Heads[j]
			h.Tip, h.MergeBase, h.Paths, h.Verdicts = line.head.Tip, line.head.MergeBase, line.head.Paths, line.head.Verdicts
		}
	case gitMissingKind:
		m := line.missing
		if githubReason(m.Reason) {
			switch {
			case r.git:
				refuse("a missing line of GitHub's of #%d after git's lines began", m.PR)
			case m.PR < 1:
				refuse("a missing line of GitHub's of pr %d, which is no pull request", m.PR)
			case r.open && r.pr != m.PR:
				refuse("a missing line of #%d inside the block of #%d", m.PR, r.pr)
			case r.headsShort():
				refuse("a missing line before the %d heads #%d counts", r.heads, r.pr)
			case !r.open && m.PR <= r.last:
				refuse("the block of #%d after that of #%d", m.PR, r.last)
			}
		} else {
			_, held := r.in.pullIndex(m.PR)
			switch {
			case !r.git && (r.open || m.PR != 0):
				refuse("a missing line of git's of #%d before the commits' block", m.PR)
			case r.git && !r.commitsDone && m.PR != 0:
				refuse("a missing line of #%d before the commits' block ends", m.PR)
			case r.commitsDone && m.PR == 0:
				refuse("a missing commit after the commits' block")
			case r.commitsDone && r.open && r.pr != m.PR:
				refuse("a missing line of #%d inside the block of #%d", m.PR, r.pr)
			case r.commitsDone && !r.open && m.PR <= r.last:
				refuse("git's block of #%d after that of #%d", m.PR, r.last)
			case r.commitsDone && !held:
				refuse("a missing line of git's of #%d, which holds no pull line", m.PR)
			}
			if r.err == nil && !r.git {
				r.git = true
				r.begin(0)
			}
		}
		if r.err != nil {
			return
		}
		if !r.open {
			r.begin(m.PR)
		}
		if r.missing != nil && compareGitMissing(*r.missing, m) >= 0 {
			refuse("a missing line out of the order of head, base, reason and detail, or given twice")
			return
		}
		r.missing = &m
		r.in.Missing = append(r.in.Missing, m)
	case gitDoneKind:
		switch {
		case !r.open:
			refuse("the done line of #%d ends no block: a block holds a line but its done line", number)
		case r.pr != number:
			refuse("the done line of #%d ends the block of #%d", number, r.pr)
		case r.headsShort():
			refuse("#%d holds %d head lines, and its pull line counts %d", number, len(r.in.Pulls[len(r.in.Pulls)-1].Heads), r.heads)
		default:
			r.open, r.last = false, number
			if r.git && !r.commitsDone {
				r.commitsDone = true
			}
		}
	}
}

// input is the input every line added gives, its missing inputs sorted by pr, head, base, reason
// and detail; or the first line refused, or a last block that ends with no done line, as a write
// that stopped leaves it.
func (r *gitReading) input() (GitInput, error) {
	if r.err != nil {
		return GitInput{}, r.err
	}
	if r.open {
		return GitInput{}, fmt.Errorf("%s: the block of #%d ends with no done line, as a write that stopped leaves it; a run of collect-git cuts it off and goes on",
			r.name, r.pr)
	}
	slices.SortFunc(r.in.Missing, compareGitMissing)
	return r.in, nil
}

// gitBlockOf is the block line, a line of a git input but a done line, belongs to: git's or the
// GitHub phase's, and its pull request (0 for the commits' block).
func gitBlockOf(line gitLineRead) (git bool, pr int) {
	switch line.kind {
	case gitCommitKind:
		return true, 0
	case gitReadingKind:
		return true, line.pull.Number
	case gitMissingKind:
		return !githubReason(line.missing.Reason), line.missing.PR
	}
	return false, line.pull.Number
}

// gitFileEnd reads file, the git input of a window that window.json names name, to where a run of
// collect-git goes on from it: the end of its last done line, with the sha256 of its bytes up to
// there. What follows is what a write that stopped left of the block it was appending: a last line
// with no newline, and whole lines of that one block, which the run cuts off and writes again. It
// holds a line at a time. It refuses a whole line after the last done line that is no line of the
// format (parseGitLine), and whole lines there of more than one block, which no write that stopped
// leaves, as in a file of an older form, which a run must not cut away.
func gitFileEnd(file io.Reader, name string) (int64, string, error) {
	type block struct {
		git bool
		pr  int
	}
	buffered := bufio.NewReaderSize(file, inputBuffer)
	hash := sha256.New()
	sum := hex.EncodeToString(hash.Sum(nil))
	var at, end int64
	var tail *block
	var refused error
	for number := 1; ; number++ {
		line, err := buffered.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, "", err
		}
		if !bytes.HasSuffix(line, []byte("\n")) {
			break
		}
		at += int64(len(line))
		hash.Write(line)
		parsed, err := parseGitLine(bytes.TrimSpace(line))
		switch {
		case err == nil && parsed.kind == gitDoneKind:
			end, sum, tail, refused = at, hex.EncodeToString(hash.Sum(nil)), nil, nil
		case refused != nil:
		case err != nil:
			refused = fmt.Errorf("%s line %d: %w", name, number, err)
		default:
			git, pr := gitBlockOf(parsed)
			if tail == nil {
				tail = &block{git, pr}
			} else if *tail != (block{git, pr}) {
				refused = fmt.Errorf("%s line %d: lines of more than one block after the last done line, which no write that stopped leaves", name, number)
			}
		}
	}
	if refused != nil {
		return 0, "", fmt.Errorf("%w: so the file is no git file of the format (GitInput), as one of an older form is not; collect it again", refused)
	}
	return end, sum, nil
}
