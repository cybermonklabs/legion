package rescue

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// The trailers of a rescue commit's message (rescueCommit), which tell a later put-back what to
// set the new workspace to: TrailerWorkingCopy names the working-copy commit, and each
// TrailerBookmark names one bookmark of the issue's own line and its commit, as
// `<name> <commit>`. A put-back reads them with `git log --format=%(trailers:key=<key>,valueonly)`.
const (
	TrailerWorkingCopy = "Legion-Working-Copy"
	TrailerBookmark    = "Legion-Bookmark"
)

// bookmark is one bookmark a rescue commit's trailers name, and a commit it is on.
type bookmark struct{ name, commit string }

// bookmarkTemplate is jj's listing of each local bookmark as `<name> <commit>`. A conflicted
// bookmark is on each of its added targets, so it gets one line for each of them.
const bookmarkTemplate = `if(remote, "", if(conflict, ` +
	`added_targets.map(|c| name ++ " " ++ c.commit_id() ++ "\n").join(""), ` +
	`if(normal_target, name ++ " " ++ normal_target.commit_id() ++ "\n")))`

// bookmarks is the bookmarks of listed — the local bookmarks of the workspace's own line
// (ownLine) — that a rescue commit's trailers name, in name order: the issue's own, own, always;
// each other that the remote has no branch of; and each other that is ahead of the remote's branch
// of its name, whose commit holds work that branch lacks. A bookmark is behind its branch when its
// commit is, or is an ancestor of, the branch's tip as the remote advertises it, where the clone
// holds that tip, or the clone's remote-tracking ref of the branch, which is what the pod last
// fetched of it. A bookmark whose branch has neither in the clone cannot be shown behind, and is
// named.
//
// A put-back sets only a bookmark that carries the issue's work or that the remote does not have.
// One the remote has and that carries nothing its branch lacks would only move the new pod's copy
// back, behind what that pod fetched, and `main` is one of them: at the remote's commit, or behind
// it where the remote's branch moved on after the pod last fetched it. The clone often lacks the
// remote's newer tip, so the remote-tracking ref stands in for it. Each bookmark is compared with
// its own branch alone: `main` ahead of the remote's main is the issue's work even where another
// branch of the remote, the issue's own among them, holds its commit. The push reads what the
// remote holds from the remote alone (check), and a remote-tracking ref read here changes nothing
// it pushes: a bookmark's commit is on the own line, so it is pushed when the remote lacks it, and
// a bookmark left out loses only a place at or behind what the pod fetched of that branch.
//
// Each bookmark is named at the commit that holds its work (standsFor), as the working copy is,
// and only then compared with the remote. `legion/<KEY>` on jj's empty working-copy commit with no
// description, where provisioning sets it and where a read that takes no snapshot still sees it
// while the role's edits are in no commit, is so named at the commit under it: the empty commit
// is in no push and on no remote, and a put-back could not find it.
func (r runner) bookmarks(clone, own string, listed []bookmark, remoteBranches map[string]string, held []string) ([]bookmark, error) {
	commits := make([]string, len(listed))
	for i, b := range listed {
		commits[i] = b.commit
	}
	stand, err := r.standsFor(clone, commits)
	if err != nil {
		return nil, err
	}
	mapped := make([]bookmark, len(listed))
	var tracking []string
	for i, b := range listed {
		mapped[i] = bookmark{name: b.name, commit: stand[b.commit]}
		if b.name != own && remoteBranches[b.name] != "" {
			tracking = append(tracking, "refs/remotes/origin/"+b.name)
		}
	}
	// The clone's remote-tracking ref of each branch those bookmarks name, by the branch's name;
	// one the clone lacks is not listed.
	fetched := map[string]string{}
	if len(tracking) > 0 {
		out, err := r.git(clone, nil, append([]string{"for-each-ref", "--format=%(refname) %(objectname)"}, tracking...)...)
		if err != nil {
			return nil, err
		}
		for line := range strings.Lines(out) {
			ref, sha, _ := strings.Cut(strings.TrimSpace(line), " ")
			fetched[strings.TrimPrefix(ref, "refs/remotes/origin/")] = sha
		}
	}
	var named []bookmark
	for _, b := range mapped {
		if tip := remoteBranches[b.name]; b.name != own && tip != "" {
			// What the clone holds of the remote's branch of the bookmark's name.
			var branch []string
			if slices.Contains(held, tip) {
				branch = append(branch, tip)
			}
			if sha := fetched[b.name]; sha != "" {
				branch = append(branch, sha)
			}
			if len(branch) > 0 {
				ahead, err := r.git(clone, nil, append([]string{"rev-list", "--max-count=1", b.commit, "--not"}, branch...)...)
				if err != nil {
					return nil, err
				}
				if ahead == "" {
					continue
				}
			}
		}
		named = append(named, b)
	}
	slices.SortFunc(named, func(a, b bookmark) int {
		return cmp.Or(strings.Compare(a.name, b.name), strings.Compare(a.commit, b.commit))
	})
	// Two added targets of a conflicted bookmark can stand for the same commit.
	return slices.Compact(named), nil
}

// findable is workingCopy and bookmarks held to the rule every trailer keeps: a put-back must find
// each commit a trailer names, so the commit is an ancestor of one of parents or of a held commit,
// which the push carries or the remote holds. A working-copy commit that is neither is jj's @ as
// an empty merge with no description: holdingWork keeps a merge, and ownHeads drops the empty
// commit, so the merge is no tip. The rescue commit stands for that merge, since its parents are
// the merge's heads, and the working copy is named at its first parent. A bookmark that is neither
// is left out.
func (r runner) findable(clone string, parents, held []string, workingCopy string, bookmarks []bookmark) (string, []bookmark, error) {
	var named []string
	if workingCopy != "" {
		named = append(named, workingCopy)
	}
	for _, b := range bookmarks {
		named = append(named, b.commit)
	}
	lost, err := r.unpushed(clone, named, slices.Concat(parents, held))
	if err != nil {
		return "", nil, err
	}
	if slices.Contains(lost, workingCopy) {
		workingCopy = parents[0]
	}
	kept := slices.DeleteFunc(slices.Clone(bookmarks), func(b bookmark) bool { return slices.Contains(lost, b.commit) })
	return workingCopy, kept, nil
}

// parents is the rescue commit's parents: the tips, sorted, with the one that holds the working-copy
// commit first, else the first in sorted order; and then previous, the branch's old tip, when no
// tip holds it. No tip is held by previous, so `merge-base --independent` keeps previous exactly
// when no tip holds it.
func (r runner) parents(clone string, tips []string, workingCopy, previous string) ([]string, error) {
	parents := slices.Clone(tips)
	if workingCopy != "" {
		for i, tip := range parents {
			outside, err := r.git(clone, nil, "rev-list", "--count", workingCopy, "^"+tip)
			if err != nil {
				return nil, err
			}
			if outside == "0" {
				parents = append([]string{tip}, slices.Delete(parents, i, i+1)...)
				break
			}
		}
	}
	if previous != "" {
		independent, err := r.git(clone, nil, append([]string{"merge-base", "--independent", previous}, tips...)...)
		if err != nil {
			return nil, err
		}
		if slices.Contains(fields(independent), previous) {
			parents = append(parents, previous)
		}
	}
	return parents, nil
}

// rescueCommit is the commit at the rescue branch's new tip. Its parents are parents, and its tree
// is the first parent's, so it adds no content that no parent holds: the guard's read of what the
// push adds finds nothing new in it. Its message is `Rescue of <dir> before its pod was replaced`,
// then the trailers, TrailerWorkingCopy and then a TrailerBookmark for each of bookmarks. Its
// identity is fixed and its date is the newest committer date of its parents (fixedIdentity), so
// a later check of the same disk makes the same commit.
func (r runner) rescueCommit(clone, dir string, parents []string, workingCopy string, bookmarks []bookmark) (string, error) {
	tree, err := r.git(clone, nil, "rev-parse", parents[0]+"^{tree}")
	if err != nil {
		return "", err
	}
	date, err := r.newestDate(clone, parents...)
	if err != nil {
		return "", err
	}
	var trailers []string
	if workingCopy != "" {
		trailers = append(trailers, TrailerWorkingCopy+": "+workingCopy)
	}
	for _, b := range bookmarks {
		trailers = append(trailers, TrailerBookmark+": "+b.name+" "+b.commit)
	}
	message := "Rescue of " + dir + " before its pod was replaced"
	if len(trailers) > 0 {
		message += "\n\n" + strings.Join(trailers, "\n")
	}
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	return r.git(clone, fixedIdentity(date), append(args, "-m", message)...)
}

// The author and committer of every commit this check makes.
const (
	rescuerName  = "Legion rescue"
	rescuerEmail = "rescue@invalid"
)

// noDate is the date of a commit this check makes with no parent: git's first second, in UTC.
const noDate = "@0 +0000"

// fixedIdentity is the environment a commit this check makes is written in: one identity, and date
// for both of its dates. It goes in the commit-tree call's own environment, which runner.command
// puts after the role's. Every role's environment sets the GIT_AUTHOR_* and GIT_COMMITTER_* names
// and emails of its own (runtime.GitIdentity.Env), and those outrank `-c user.*`, so a commit made
// through one role would differ from the same commit made through another. With all six fixed, a
// second check of the same disk, through any role, makes the same commits, finds them on the
// branch, and pushes nothing.
func fixedIdentity(date string) []string {
	return []string{
		"GIT_AUTHOR_NAME=" + rescuerName, "GIT_AUTHOR_EMAIL=" + rescuerEmail, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + rescuerName, "GIT_COMMITTER_EMAIL=" + rescuerEmail, "GIT_COMMITTER_DATE=" + date,
	}
}

// newestDate is the newest committer date of commits, as git reads a date back (`@<seconds>
// <zone>`), or noDate when there are none.
func (r runner) newestDate(dir string, commits ...string) (string, error) {
	if len(commits) == 0 {
		return noDate, nil
	}
	out, err := r.git(dir, nil, append([]string{"log", "--no-walk=unsorted", "--format=%ct %cz"}, commits...)...)
	if err != nil {
		return "", err
	}
	date, newest, found := noDate, int64(0), false
	for line := range strings.Lines(out) {
		seconds, zone, _ := strings.Cut(strings.TrimSpace(line), " ")
		at, err := strconv.ParseInt(seconds, 10, 64)
		if err != nil {
			return "", fmt.Errorf("git printed %q as a committer date: %w", line, err)
		}
		if !found || at > newest {
			date, newest, found = "@"+seconds+" "+zone, at, true
		}
	}
	return date, nil
}

// subjectLimit is the most bytes of a commit's subject a report carries (shimwire.RescueCommit).
const subjectLimit = 200

// subjects is each of commits with its subject, in the order given, cut at subjectLimit on a rune
// boundary. None is none: git log with no commit would read HEAD.
func (r runner) subjects(clone string, commits []string) ([]shimwire.RescueCommit, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	out, err := r.command(clone, nil, strings.NewReader(strings.Join(commits, "\n")+"\n"), "git",
		append(slices.Clone(gitPins), "log", "--no-walk=unsorted", "--stdin", "--format=%H%x09%s")...)
	if err != nil {
		return nil, err
	}
	listed := make([]shimwire.RescueCommit, 0, len(commits))
	for line := range strings.Lines(out) {
		id, subject, _ := strings.Cut(strings.TrimRight(line, "\n"), "\t")
		if len(subject) > subjectLimit {
			cut := subjectLimit
			for cut > 0 && !utf8.RuneStart(subject[cut]) {
				cut--
			}
			subject = subject[:cut]
		}
		listed = append(listed, shimwire.RescueCommit{ID: id, Subject: subject})
	}
	return listed, nil
}
