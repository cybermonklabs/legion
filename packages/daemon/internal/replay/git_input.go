package replay

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
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
	gitMissingKind = "missing"
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
// GitHub has no such pull request in the repository; GitHub's timeline of the pull request does
// not read as one, or names a head it gives no commit of; the remote holds no head, or no base
// commit, the collector fetched; git failed; or the guard killed git past its memory cap or its
// time (gitGuard).
const (
	GitPullNotFound    = "pull-not-found"
	GitTimelineUnread  = "timeline-unread"
	GitHeadUnreachable = "head-unreachable"
	GitBaseUnreachable = "base-unreachable"
	GitError           = "git-error"
	GitMemoryCap       = "memory-cap"
	GitTimeout         = "timeout"
)

// gitReasons is every reason a missing line may give.
var gitReasons = []string{GitPullNotFound, GitTimelineUnread, GitHeadUnreachable, GitBaseUnreachable, GitError, GitMemoryCap, GitTimeout}

// githubReason says whether reason is one GitHub's reads give (collect-git's GitHub phase) rather
// than git's.
func githubReason(reason string) bool {
	return reason == GitPullNotFound || reason == GitTimelineUnread
}

// GitInput is a window's git/<owner>__<name>.jsonl: what git says of the pull requests of the
// window's issues on one repository and of the first-parent commits of its base branch, which
// collect-git writes (CollectGit) and a replay answers the reads of a head's merge state from.
// The file holds one JSON object per line, in this order:
//
//   - {"kind":"commit","sha","paths"}: each commit the window's merges file holds of the base
//     branch, in that file's order, then each base commit a read names (MergeStateRead.BaseSHA)
//     that the merges file does not hold, in the log's order, with the paths it changes against
//     its first parent, sorted (Commits);
//   - for each pull request, in ascending number, {"kind":"pull","pr","merge","heads"}, its merge
//     commit into the base branch where the merges file holds it, else "", and the count of its
//     heads, followed by one {"kind":"head","pr","sha","at","from",...} line for each of its heads,
//     by at then sha (Pulls);
//   - {"kind":"missing","pr","head","base","reason","detail"}: each input the collector could not
//     get, sorted by pr, head, base, reason and detail (Missing). A missing input is never written
//     as clean.
//
// ReadWindow refuses lines out of that order, a head line that does not follow its pull request's
// line, and a pull line whose count of heads differs from the head lines after it, so a file read
// is written back byte for byte (Window.Write).
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
// merge of it), and its heads, by At then SHA.
type GitPull struct {
	Number int
	Merge  string
	Heads  []GitHead
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
// did not read the head — the GitHub phase alone wrote it, or git could not — the four are empty,
// and the line names none of them.
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
// merge-tree --write-tree --no-messages --name-only <base> <head>): clean, or a conflict in the
// paths Paths, sorted.
type GitVerdict struct {
	Base     string   `json:"base"`
	Conflict bool     `json:"conflict"`
	Paths    []string `json:"paths"`
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
	i, found := slices.BinarySearchFunc(g.Pulls, number, func(p GitPull, n int) int { return cmp.Compare(p.Number, n) })
	if !found {
		return GitPull{}, false
	}
	return g.Pulls[i], true
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
		Kind  string `json:"kind"`
		PR    int    `json:"pr"`
		Merge string `json:"merge"`
		Heads int    `json:"heads"`
	}
	gitHeadLine struct {
		Kind      string        `json:"kind"`
		PR        int           `json:"pr"`
		SHA       string        `json:"sha"`
		At        time.Time     `json:"at"`
		From      string        `json:"from"`
		Tip       string        `json:"tip,omitempty"`
		MergeBase string        `json:"mergeBase,omitempty"`
		Paths     *[]string     `json:"paths,omitempty"`
		Verdicts  *[]GitVerdict `json:"verdicts,omitempty"`
	}
	gitMissingLine struct {
		Kind   string `json:"kind"`
		PR     int    `json:"pr"`
		Head   string `json:"head"`
		Base   string `json:"base"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
)

// lines is g as its file holds it, in the format's order (GitInput): the commits, the pull
// requests each with its heads, then the missing inputs, each as g holds them.
func (g GitInput) lines() ([]json.RawMessage, error) {
	var lines []json.RawMessage
	add := func(value any) error {
		line, err := json.Marshal(value)
		if err == nil {
			lines = append(lines, line)
		}
		return err
	}
	nonNil := func(paths []string) []string {
		if paths == nil {
			return []string{}
		}
		return paths
	}
	for _, commit := range g.Commits {
		if err := add(gitCommitLine{Kind: gitCommitKind, SHA: commit.SHA, Paths: nonNil(commit.Paths)}); err != nil {
			return nil, err
		}
	}
	for _, pull := range g.Pulls {
		if err := add(gitPullLine{Kind: gitPullKind, PR: pull.Number, Merge: pull.Merge, Heads: len(pull.Heads)}); err != nil {
			return nil, err
		}
		for _, head := range pull.Heads {
			line := gitHeadLine{Kind: gitHeadKind, PR: pull.Number, SHA: head.SHA, At: head.At.UTC(), From: head.From}
			if head.Read() {
				paths, verdicts := nonNil(head.Paths), make([]GitVerdict, 0, len(head.Verdicts))
				for _, verdict := range head.Verdicts {
					verdict.Paths = nonNil(verdict.Paths)
					verdicts = append(verdicts, verdict)
				}
				line.Tip, line.MergeBase, line.Paths, line.Verdicts = head.Tip, head.MergeBase, &paths, &verdicts
			}
			if err := add(line); err != nil {
				return nil, err
			}
		}
	}
	for _, missing := range g.Missing {
		if err := add(gitMissingLine{Kind: gitMissingKind, PR: missing.PR, Head: missing.Head, Base: missing.Base, Reason: missing.Reason,
			Detail: missing.Detail}); err != nil {
			return nil, err
		}
	}
	return lines, nil
}

// gitLineRead is one line of a git input as parseGitLine reads it: its kind and, by its kind, the
// commit; the pull request (no heads yet) with the count of heads its line gives; the head with
// its pull request's number in pull; or the missing input.
type gitLineRead struct {
	kind    string
	commit  GitCommit
	pull    GitPull
	heads   int
	head    GitHead
	missing GitMissing
}

// gitLineMembers is the members a line of each kind holds, and those a head line holds only where
// git read the head, all of them or none.
var (
	gitLineMembers = map[string][]string{
		gitCommitKind:  {"kind", "sha", "paths"},
		gitPullKind:    {"kind", "pr", "merge", "heads"},
		gitHeadKind:    {"kind", "pr", "sha", "at", "from"},
		gitMissingKind: {"kind", "pr", "head", "base", "reason", "detail"},
	}
	gitReadMembers = []string{"tip", "mergeBase", "paths", "verdicts"}
)

// parseGitLine reads line, one line of a git input, as its kind gives it (GitInput). It refuses a
// line of no kind the format holds, a member missing, null, of another JSON type or that its kind
// does not hold, a head line holding some of git's members but not all, a commit that is no whole
// commit id, a list of paths that is not sorted or holds a path twice, a pull request numbered
// below one, a time that is zero, a head of no source the format names, a clean verdict that names
// paths, and a missing input of no reason the format names.
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
	read := 0
	for _, member := range gitReadMembers {
		if _, held := members[member]; held {
			read++
		}
	}
	if kind == gitHeadKind && read == len(gitReadMembers) {
		want = append(slices.Clone(want), gitReadMembers...)
	} else if kind == gitHeadKind && read > 0 {
		return gitLineRead{}, fmt.Errorf("a head line holds some of %s and not all", strings.Join(gitReadMembers, ", "))
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
		}
		if err := checkCommit(fmt.Sprintf("the merge of #%d", l.PR), l.Merge, true); err != nil {
			return gitLineRead{}, err
		}
		parsed.pull, parsed.heads = GitPull{Number: l.PR, Merge: l.Merge}, l.Heads
	case gitHeadKind:
		var l gitHeadLine
		if err := decodeStrict(line, &l); err != nil {
			return gitLineRead{}, err
		}
		head := GitHead{SHA: l.SHA, At: l.At, From: l.From, Tip: l.Tip, MergeBase: l.MergeBase}
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
		if l.Paths != nil {
			for what, sha := range map[string]string{"tip": l.Tip, "merge base": l.MergeBase} {
				if err := checkCommit(fmt.Sprintf("the %s of the head %s", what, l.SHA), sha, false); err != nil {
					return gitLineRead{}, err
				}
			}
			if err := checkPaths("the paths of the head "+l.SHA, *l.Paths); err != nil {
				return gitLineRead{}, err
			}
			head.Paths, head.Verdicts = *l.Paths, *l.Verdicts
			if err := checkVerdicts(l.SHA, line, head.Verdicts); err != nil {
				return gitLineRead{}, err
			}
		}
		parsed.pull, parsed.head = GitPull{Number: l.PR}, head
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

// checkVerdicts refuses the verdicts of the head sha, read from line, where a verdict lacks a
// member or holds one no verdict holds, names no whole commit id, names paths unsorted or twice, or
// is clean and names paths.
func checkVerdicts(sha string, line []byte, verdicts []GitVerdict) error {
	var raw struct {
		Verdicts []map[string]json.RawMessage `json:"verdicts"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return err
	}
	for i, members := range raw.Verdicts {
		for _, name := range []string{"base", "conflict", "paths"} {
			if value, held := members[name]; !held || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("verdict %d of the head %s has no %s", i+1, sha, name)
			}
		}
	}
	for _, verdict := range verdicts {
		if err := checkCommit("a verdict's base of the head "+sha, verdict.Base, false); err != nil {
			return err
		}
		if err := checkPaths(fmt.Sprintf("the conflicting paths of the head %s at %s", sha, verdict.Base), verdict.Paths); err != nil {
			return err
		}
		if !verdict.Conflict && len(verdict.Paths) > 0 {
			return fmt.Errorf("the clean verdict of the head %s at %s names the paths %q", sha, verdict.Base, verdict.Paths)
		}
	}
	return nil
}

// gitReading assembles a git input from its lines in the file's order (add), and is the input once
// every line is added (input): the first line out of the format's order, a head line that does not
// follow its pull request's line or comes past the count of heads that line gives, a commit or a
// head given twice, and a pull line whose count of heads its head lines fall short of, refuse it.
type gitReading struct {
	name  string
	in    GitInput
	lines int
	// heads is the count of heads the last pull line gives; missing whether a missing line came.
	heads   int
	missing bool
	commits map[string]bool
	err     error
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
	pulls := len(r.in.Pulls)
	switch line.kind {
	case gitCommitKind:
		switch {
		case pulls > 0 || r.missing:
			refuse("a commit line after a pull or a missing line")
			return
		case r.commits[line.commit.SHA]:
			refuse("the commit %s a second time", line.commit.SHA)
			return
		}
		if r.commits == nil {
			r.commits = make(map[string]bool)
		}
		r.commits[line.commit.SHA] = true
		r.in.Commits = append(r.in.Commits, line.commit)
	case gitPullKind:
		switch {
		case r.missing:
			refuse("a pull line after a missing line")
			return
		case pulls > 0 && len(r.in.Pulls[pulls-1].Heads) < r.heads:
			refuse("the pull line of #%d before the %d heads #%d counts", line.pull.Number, r.heads, r.in.Pulls[pulls-1].Number)
			return
		case pulls > 0 && r.in.Pulls[pulls-1].Number >= line.pull.Number:
			refuse("the pull line of #%d after that of #%d", line.pull.Number, r.in.Pulls[pulls-1].Number)
			return
		}
		r.in.Pulls = append(r.in.Pulls, line.pull)
		r.heads = line.heads
	case gitHeadKind:
		if pulls == 0 || r.in.Pulls[pulls-1].Number != line.pull.Number {
			refuse("a head line of #%d that does not follow its pull line", line.pull.Number)
			return
		}
		pull := &r.in.Pulls[pulls-1]
		switch n := len(pull.Heads); {
		case n == r.heads:
			refuse("a head of #%d past the %d its pull line counts", pull.Number, r.heads)
			return
		case slices.ContainsFunc(pull.Heads, func(h GitHead) bool { return h.SHA == line.head.SHA }):
			refuse("the head %s of #%d a second time", line.head.SHA, pull.Number)
			return
		case n > 0 && compareGitHeads(pull.Heads[n-1], line.head) >= 0:
			refuse("the head %s of #%d out of the order of at, then sha", line.head.SHA, pull.Number)
			return
		}
		pull.Heads = append(pull.Heads, line.head)
	case gitMissingKind:
		if pulls > 0 && len(r.in.Pulls[pulls-1].Heads) < r.heads {
			refuse("a missing line before the %d heads #%d counts", r.heads, r.in.Pulls[pulls-1].Number)
			return
		}
		if n := len(r.in.Missing); n > 0 && compareGitMissing(r.in.Missing[n-1], line.missing) >= 0 {
			refuse("a missing line out of the order of pr, head, base, reason and detail, or given twice")
			return
		}
		r.missing = true
		r.in.Missing = append(r.in.Missing, line.missing)
	}
}

// input is the input every line added gives, or the first line refused.
func (r *gitReading) input() (GitInput, error) {
	if r.err != nil {
		return GitInput{}, r.err
	}
	if pulls := len(r.in.Pulls); pulls > 0 && len(r.in.Pulls[pulls-1].Heads) < r.heads {
		return GitInput{}, fmt.Errorf("%s: #%d holds %d head lines, and its pull line counts %d", r.name, r.in.Pulls[pulls-1].Number,
			len(r.in.Pulls[pulls-1].Heads), r.heads)
	}
	return r.in, nil
}
