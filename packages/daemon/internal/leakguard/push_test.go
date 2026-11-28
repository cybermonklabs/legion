package leakguard

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
	"github.com/sjawhar/legion/daemon/internal/redact"
)

// A push's records are read from the commits it would add, out of a real repository: the
// message, the author identity, each added line of each file, and the ref name are each read,
// and a commit the destination already holds is not read at all. Every case here pushes one commit
// that names something private in one of those places, and the last one names nothing anywhere
// and is passed.
func TestAPushsRecordsAreReadFromItsCommits(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	// The fixture's own git runs under an environment that reaches no repository, remote,
	// credential or configuration but the test's (docs/testing.md).
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, message string, env ...string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.With(env...).Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	write(t, "README.md", "one\n")
	base := commit(t, "the commit the destination holds")
	// The push's destination holds the base commit, on a remote-tracking ref of this repository:
	// that is what makes it a commit no push adds.
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	// origin then names the destination by the URL each push below is made to, which is the URL
	// git would hand the hook for a push to origin.
	fixture.Run(repo, "git", "remote", "set-url", "origin", theURL)
	branch := func(t *testing.T, name string) {
		t.Helper()
		fixture.Run(repo, "git", "checkout", "--quiet", "-B", name, base)
	}

	for _, tc := range []struct {
		name string
		// build makes the case's commit on a branch of its own and answers its sha.
		build func(t *testing.T) string
		// ref is the ref the destination would hold, "refs/heads/main" where the case leaves it
		// empty.
		ref string
		// newRef is whether the destination holds that ref already: a new one has no sha of its
		// own to read the push's commits from, so they are read from the remote-tracking refs.
		newRef bool
		// want is the report, "<record>: <class>" a line, as a function of the commit's sha, and
		// nil where the push passes.
		want func(head string) []string
	}{
		{
			name: "the message names a host",
			build: func(t *testing.T) string {
				branch(t, "message")
				write(t, "README.md", "one\ntwo\n")
				return commit(t, "moved the run to "+leakguardtest.RulesHost+" this morning")
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " message: " + leakguardtest.RulesHostClass}
			},
		},
		{
			name: "the author identity names a host",
			build: func(t *testing.T) string {
				branch(t, "author")
				write(t, "README.md", "one\nthree\n")
				return commit(t, "a message of no interest", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " author: " + leakguardtest.RulesHostClass}
			},
		},
		{
			name: "an added line of a tracked file names a host",
			build: func(t *testing.T) string {
				branch(t, "line")
				write(t, "README.md", "one\nthe runner answers at "+leakguardtest.RulesHost+"\n")
				return commit(t, "a message of no interest")
			},
			want: func(string) []string { return []string{"README.md: " + leakguardtest.RulesHostClass} },
		},
		{
			name: "the ref name names a host",
			build: func(t *testing.T) string {
				branch(t, "ref-name")
				write(t, "README.md", "one\nfour\n")
				return commit(t, "a message of no interest")
			},
			ref:    "refs/heads/legion/" + leakguardtest.RulesHost,
			newRef: true,
			want:   func(string) []string { return []string{"the ref name: " + leakguardtest.RulesHostClass} },
		},
		{
			name: "a commit that names nothing",
			build: func(t *testing.T) string {
				branch(t, "clean")
				if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o700); err != nil {
					t.Fatal(err)
				}
				write(t, "docs/notes.md", "a line of ordinary prose\n")
				return commit(t, "a message of no interest")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := tc.build(t)
			ref, remoteSHA := tc.ref, base
			if ref == "" {
				ref = "refs/heads/main"
			}
			if tc.newRef {
				remoteSHA = noSHA
			}
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:    repo,
				Remote: "origin",
				URL:    "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{
					LocalRef:  ref,
					LocalSHA:  head,
					RemoteRef: ref,
					RemoteSHA: remoteSHA,
				}},
			})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("a push that names nothing: %v", err)
				}
				return
			}
			reports(t, refusal(t, err).Lines, tc.want(head)...)
		})
	}
}

// A push the rules refuse shows, for each finding, the record, the class, the match and the line
// it stands on, and the line is the line of the message or of the file: a commit message's third
// line; added lines counted from hunk headers that start past the file's first line, two hunks of
// one file; a line of a merge's own resolution counted from its combined hunk, past the lines of
// the parents that the resolution does not hold; and the ref name, a line of its own. Each line is
// the one git and the engine wrote for a real push, and the message's third line holds the engine
// to counting a record's lines from 0.
func TestPushRefusalShowsMatch(t *testing.T) {
	const (
		host  = leakguardtest.RulesHost
		class = leakguardtest.RulesHostClass
	)
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, message string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	write(t, "README.md", "one\ntwo\nthree\nfour\nfive\n")
	write(t, "notes.md", "first\nsecond\nthird\nfourth\nfifth\n")
	base := commit(t, "the commit the destination holds")
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	branch := func(t *testing.T, name string) {
		t.Helper()
		fixture.Run(repo, "git", "checkout", "--quiet", "-B", name, base)
	}

	for _, tc := range []struct {
		name string
		// build makes the case's commits on a branch of their own and answers the head's sha.
		build func(t *testing.T) string
		// ref is a new ref the push creates, and empty for a push to main.
		ref string
		// want is the whole report, as a function of the head's sha.
		want func(head string) []string
	}{
		{
			name: "the commit message",
			build: func(t *testing.T) string {
				branch(t, "message")
				write(t, "README.md", "one\ntwo\nthree\nfour\nfive\nsix\n")
				return commit(t, "a subject of no interest\n\nmoved the run to "+host+" this morning")
			},
			want: func(head string) []string {
				return []string{leakguardtest.ReportLine("commit "+short(head)+" message", class, host+" ", 3, "moved the run to "+host+" this morning")}
			},
		},
		{
			name: "added lines, in hunks that start past the file's first line",
			build: func(t *testing.T) string {
				branch(t, "lines")
				write(t, "README.md", "one\ntwo\nthree\nan added line\nthe runner answers at "+host+"\nfour\nfive\nsix\nthe spare answers at "+host+"\n")
				return commit(t, "a message of no interest")
			},
			want: func(string) []string {
				return []string{
					leakguardtest.ReportLine("README.md", class, host, 5, "the runner answers at "+host),
					leakguardtest.ReportLine("README.md", class, host, 9, "the spare answers at "+host),
				}
			},
		},
		{
			name: "a line of a merge's own resolution",
			build: func(t *testing.T) string {
				branch(t, "side")
				write(t, "notes.md", "first\nsecond\nthird\nthe side's fourth\nfifth\n")
				commit(t, "the side's commit")
				branch(t, "merged")
				write(t, "notes.md", "the merged first\nsecond\nthird\nfourth\nfifth\n")
				commit(t, "the merged branch's commit")
				fixture.Run(repo, "git", "merge", "--quiet", "--no-ff", "--no-commit", "side")
				// The resolution replaces the second line, which both parents hold: its combined
				// hunk holds three lines of the parents' before the resolution's own.
				write(t, "notes.md", "the merged first\nthe resolution answers at "+host+"\nthird\nthe side's fourth\nfifth\n")
				head := commit(t, "the merge")
				if parents := fixture.Run(repo, "git", "rev-list", "--parents", "-n", "1", head); len(strings.Fields(parents)) != 3 {
					t.Fatalf("the fixture's head is %q, want a commit of two parents", parents)
				}
				return head
			},
			want: func(string) []string {
				return []string{leakguardtest.ReportLine("notes.md", class, host, 2, "the resolution answers at "+host)}
			},
		},
		{
			name: "the ref name",
			build: func(t *testing.T) string {
				branch(t, "ref-name")
				write(t, "README.md", "one\ntwo\nthree\nfour\nfive\nseven\n")
				return commit(t, "a message of no interest")
			},
			ref: "refs/heads/legion/" + host,
			want: func(string) []string {
				return []string{leakguardtest.ReportLine("the ref name", class, host, 1, "refs/heads/legion/"+host)}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := tc.build(t)
			update := Update{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: base}
			if tc.ref != "" {
				update = Update{LocalRef: tc.ref, LocalSHA: head, RemoteRef: tc.ref, RemoteSHA: noSHA}
			}
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{update},
			})
			if got, want := refusal(t, err).Lines, tc.want(head); !slices.Equal(got, want) {
				t.Errorf("the report is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// A push's paths are read as paths, through the push's own records. A path the path rule matches
// is reported once as the path, though the rule matches every record of the file's, beside the
// word rule's match in the path's own text. The fixture's path-scoped allowlist still clears its
// one vendored file, and not the same line in the file beside it.
func TestAPushReadsItsPathsAsPaths(t *testing.T) {
	const notes = "docs/" + leakguardtest.Word + "-notes.md"
	const line = "var " + leakguardtest.Word + " = require('./lantern')"
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, name, content string) {
		t.Helper()
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, message string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	write(t, "README.md", "one\n")
	base := commit(t, "the commit the destination holds")
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")

	for _, tc := range []struct {
		name  string
		files map[string]string
		// want is the whole report.
		want []string
	}{
		{
			name:  "a path the path rule matches",
			files: map[string]string{notes: "a line of ordinary prose\nand a second\n"},
			want: []string{
				notes + ": " + leakguardtest.WordPathClass + ": matched the path",
				leakguardtest.ReportLine(notes, leakguardtest.WordClass, "/"+leakguardtest.Word+"-", 1, notes),
			},
		},
		{
			name:  "the vendored file's allowlist",
			files: map[string]string{leakguardtest.AllowedPath: line + "\n", "vendor/glasswing/other.js": line + "\n"},
			want:  []string{leakguardtest.ReportLine("vendor/glasswing/other.js", leakguardtest.WordClass, " "+leakguardtest.Word+" ", 1, line)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture.Run(repo, "git", "checkout", "--quiet", "-B", strings.ReplaceAll(tc.name, " ", "-"), base)
			for name, content := range tc.files {
				write(t, name, content)
			}
			head := commit(t, "a message of no interest")
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: base}},
			})
			if got := refusal(t, err).Lines; !slices.Equal(got, tc.want) {
				t.Errorf("the report is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// git writes a tab after the name on a patch's "+++ b/<path>" line when the path holds a space,
// and quotes the name before that tab when it also holds a character git quotes (measured with
// git 2.47.3 in an INFRA issue pod on 2026-11-19: `+++ b/docs/a b.md\t`, `+++ "b/docs/two\nlines
// x.md"\t`). The tab is no part of the path. So an added line of a file whose path holds a space
// is named by that path in the report, with no tab after it, quoted name or not; and a rule file's
// path-scoped allowlist anchored on such a path clears that file's added lines, as it clears the
// added lines of a file whose path holds none.
func TestAPushNamesAFileWhosePathHoldsASpaceByItsPath(t *testing.T) {
	const (
		host  = leakguardtest.RulesHost
		class = leakguardtest.RulesHostClass
		plain = "docs/a b.md"
		quote = `docs/a "b" c.md`
	)
	const vendored = "vendor/glass wing/bundle.js"
	rules := strings.Replace(leakguardtest.Rules, "^vendor/glasswing/bundle[.]js$", "^vendor/glass wing/bundle[.]js$", 1)
	if rules == leakguardtest.Rules {
		t.Fatal("the fixture's allowlist path was not found, so this test would allow nothing")
	}
	rulesFile := filepath.Join(t.TempDir(), "rules.toml")
	if err := os.WriteFile(rulesFile, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, name, content string) {
		t.Helper()
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, message string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	write(t, "README.md", "one\n")
	base := commit(t, "the commit the destination holds")
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	write(t, plain, "the runner answers at "+host+"\n")
	write(t, quote, "the spare answers at "+host+"\n")
	write(t, vendored, "var "+leakguardtest.Word+" = require('./lantern')\n")
	head := commit(t, "a message of no interest")

	guard := New(Options{
		Check:      leakguardtest.Write(t, t.TempDir(), passes),
		Visibility: answers(Public, nil),
		Env:        append(fixtureenv.Isolated(t.TempDir()), RulesFileVar+"="+rulesFile),
	})
	err := guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: base}},
	})
	want := []string{
		leakguardtest.ReportLine(quote, class, host, 1, "the spare answers at "+host),
		leakguardtest.ReportLine(plain, class, host, 1, "the runner answers at "+host),
	}
	if got := refusal(t, err).Lines; !slices.Equal(got, want) {
		t.Errorf("the report is\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A push the destination already holds everything of adds no commit and so has no record but its
// ref name, which is what a push of an existing commit to a new name is.
func TestAPushThatAddsNoCommitReadsNoCommit(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("the file names "+leakguardtest.RulesHost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.Run(repo, "git", "add", "-A")
	fixture.Run(repo, "git", "commit", "--quiet", "-m", "a commit naming "+leakguardtest.RulesHost)
	head := fixture.Run(repo, "git", "rev-parse", "HEAD")
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	// The destination holds that commit at that ref already, so the push moves nothing and the
	// commit's own text is none of its business.
	err := guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: head}},
	})
	if err != nil {
		t.Fatalf("a push that adds no commit: %v", err)
	}
}

// A push that fast-forwards a branch far behind the destination's main reads only the commits
// of its own. The destination holds the branch at an old main, and the push is that branch based
// on today's main, so every main commit between the two tips stands between the branch's old tip
// and its new one; each is on the destination's main already, on a remote-tracking ref of this
// repository, so the push publishes nothing of it. One of those main commits has an author a rule
// refuses, as main's own commits once refused the push of a branch the destination held at a main
// of weeks before. The control is a commit of the push's own with that same author: it is
// refused, and the report names it and nothing of main's.
func TestAPushOverTheDestinationsMainReadsOnlyItsOwnCommits(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, message string, env ...string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.With(env...).Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	const branch = "refs/heads/legion/LEGION-1"
	write(t, "README.md", "one\n")
	old := commit(t, "the main the branch was cut from")
	// The destination holds main and the branch, both at that old main.
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main", "main:"+branch)
	// Main moves on at the destination, and this repository's origin/main moves with it, as a
	// fetch would leave it. The first of those commits has an author a rule refuses.
	write(t, "README.md", "one\ntwo\n")
	commit(t, "a main commit", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
	write(t, "README.md", "one\ntwo\nthree\n")
	commit(t, "a later main commit")
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	fixture.Run(repo, "git", "remote", "set-url", "origin", theURL)
	// The branch is based on today's main, and the destination still holds it at the old one.
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "legion/LEGION-1", "origin/main")
	push := func(t *testing.T, head string) error {
		t.Helper()
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
		return guard.Push(bound(t), PushRequest{
			Dir:     repo,
			Remote:  "origin",
			URL:     "https://github.com/invented-org/invented-repo.git",
			Updates: []Update{{LocalRef: branch, LocalSHA: head, RemoteRef: branch, RemoteSHA: old}},
		})
	}
	write(t, "work.md", "a line of the branch's own\n")
	own := commit(t, "the branch's own commit")
	if err := push(t, own); err != nil {
		t.Fatalf("a push over main's commits whose own commit names nothing: %v", err)
	}
	write(t, "work.md", "a line of the branch's own\nand another\n")
	named := commit(t, "a second commit of the branch's own", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
	// One line, the push's own commit, and none of main's.
	reports(t, refusal(t, push(t, named)).Lines, "commit "+short(named)+" author: "+leakguardtest.RulesHostClass)
}

// A root commit's own added lines are read. A root commit has no parent to diff against, so a
// diff-tree asked for one answers nothing at all, and the first push of a repository — the one
// push that is all root commit — would carry every line of every file past the rules. The
// repository here is the whole of what the push publishes, as a `legion workspace-init` clone
// pushed to a new remote is.
func TestARootCommitsAddedLinesAreRead(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("the runner answers at "+leakguardtest.RulesHost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.Run(repo, "git", "add", "-A")
	fixture.Run(repo, "git", "commit", "--quiet", "-m", "the first commit")
	head := fixture.Run(repo, "git", "rev-parse", "HEAD")
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	// The destination holds nothing: a ref the push creates, in a repository with no remote
	// whose tracking refs could say otherwise.
	err := guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	})
	reports(t, refusal(t, err).Lines, "README.md: "+leakguardtest.RulesHostClass)
}

// What a merge's own resolution writes is read. A merge's diff-tree against its first parent is
// empty unless the shape is asked for, so a line that stands in neither parent — the text a
// person wrote while resolving, which is text no other commit of the push carries — would reach
// the destination unread. The merge here is clean and its resolution adds a file of its own,
// which is the "evil merge" every rebase-free branch can hold.
func TestAMergesOwnResolutionIsRead(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	write("README.md", "one\n")
	base := commit("the commit the destination holds")
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "side")
	write("side.md", "a line of the side branch\n")
	commit("the side commit")
	fixture.Run(repo, "git", "checkout", "--quiet", "main")
	write("main.md", "a line of the main branch\n")
	commit("the main commit")
	// The merge itself is clean; what the resolution adds is this file, which stands in neither
	// parent and is the whole of what the merge commit publishes of its own.
	fixture.Run(repo, "git", "merge", "--quiet", "--no-ff", "--no-commit", "side")
	write("resolution.md", "the runner answers at "+leakguardtest.RulesHost+"\n")
	head := commit("the merge")
	if parents := fixture.Run(repo, "git", "rev-list", "--parents", "-n", "1", head); len(strings.Fields(parents)) != 3 {
		t.Fatalf("the fixture's head is %q, want a commit of two parents", parents)
	}
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	err := guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: base}},
	})
	reports(t, refusal(t, err).Lines, "resolution.md: "+leakguardtest.RulesHostClass)
}

// A forward merge, the default branch merged onto an older tip of a branch the destination
// holds, reads no commit the repository's remote-tracking refs hold. The default branch's commit
// here adds a line the rules refuse, and origin/main holds it: it was fetched from the
// destination, so the push of the merge does not publish it, whichever ref the push moves. A
// commit that adds the same line on top of the merge is on no remote-tracking ref, so it is read
// and refused, and it is the whole of the report.
func TestAForwardMergeReadsNoCommitARemoteTrackingRefHolds(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	write("README.md", "one\n")
	commit("the first commit")
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work")
	write("work.md", "a line of the branch\n")
	tip := commit("the branch's own commit")
	fixture.Run(repo, "git", "checkout", "--quiet", "main")
	write("main.md", "the runner answers at "+leakguardtest.RulesHost+"\n")
	commit("a commit of the default branch")
	// The destination holds both branches, and so does this repository's origin/: origin/work
	// is the old tip the push moves, and origin/main holds the default branch's commit.
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main", "work")
	fixture.Run(repo, "git", "remote", "set-url", "origin", theURL)
	fixture.Run(repo, "git", "checkout", "--quiet", "work")
	fixture.Run(repo, "git", "merge", "--quiet", "--no-ff", "-m", "merge main into work", "main")
	merge := fixture.Run(repo, "git", "rev-parse", "HEAD")
	if parents := fixture.Run(repo, "git", "rev-list", "--parents", "-n", "1", merge); len(strings.Fields(parents)) != 3 {
		t.Fatalf("the fixture's merge is %q, want a commit of two parents", parents)
	}
	write("copy.md", "the runner answers at "+leakguardtest.RulesHost+"\n")
	onTop := commit("a commit no remote-tracking ref holds")

	for _, tc := range []struct {
		name string
		head string
		// want is the report, "<record>: <class>" a line, and nil where the push passes.
		want []string
	}{
		{name: "the merge alone", head: merge},
		{
			name: "a commit of the same line on top of the merge",
			head: onTop,
			want: []string{"copy.md: " + leakguardtest.RulesHostClass},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: tc.head, RemoteRef: "refs/heads/work", RemoteSHA: tip}},
			})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("the push of the merge alone: %v", err)
				}
				return
			}
			reports(t, refusal(t, err).Lines, tc.want...)
		})
	}
}

// readCommits are the commits a push reads, each as a report names it (short), in the order it
// reads them: one per commit's author record, which every commit read has exactly one of.
func readCommits(t *testing.T, guard *Guard, req PushRequest) []string {
	t.Helper()
	var commits []string
	err := guard.pushRecords(bound(t), req, func(record Record) error {
		if commit, ok := strings.CutSuffix(record.Label, " author"); ok {
			commits = append(commits, strings.TrimPrefix(commit, "commit "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the push's records: %v", err)
	}
	return commits
}

// A push to a URL reads only the commits the remote with that URL does not hold. For `git push
// <url>` git hands a pre-push hook the URL where a remote's name goes, and no remote-tracking ref
// is filed under a URL, so the guard read every commit the destination already had as one the
// push added: a push of a branch of this repository to its URL ran 83.7 minutes in the guard and
// was refused over a commit already on main, where the same push to origin took 18 s. origin here
// names the destination by its https URL, and its main holds a commit whose author a rule refuses;
// the push's own commit names nothing. Each row names the destination as a push can: by origin's
// name, which is the control; by that URL; by the URL with a token in its userinfo; in git's scp
// syntax for ssh; and in other capitals, which GitHub reads as the same repository. In each, the
// push's own commit is the one commit read, and the push passes. A second remote, local, names a
// repository on this machine by a path that holds a space, and its main holds the same commits: a
// push to that path holds local's refs and reads its own commit alone, as git prints the path on
// one line. The last row pushes to a URL of a repository no remote names, whose tracking refs
// nobody has, so it reads every commit and is refused over main's.
func TestAPushToAURLReadsOnlyWhatTheRemoteWithThatURLDoesNotHold(t *testing.T) {
	home, repo := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(filepath.Dir(repo), fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, message string, env ...string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.With(env...).Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	write(t, "README.md", "one\n")
	first := commit(t, "the first main commit")
	write(t, "README.md", "one\ntwo\n")
	named := commit(t, "a main commit", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
	write(t, "README.md", "one\ntwo\nthree\n")
	later := commit(t, "a later main commit")
	// origin/main holds main, as a fetch from the destination leaves it; the ref is written rather
	// than fetched, so that no git of the test reaches GitHub. origin is configured as a blobless
	// clone's is, which `git remote -v` prints with its filter after the URL, `<url> (fetch)
	// [blob:none]`.
	fixture.Run(repo, "git", "remote", "add", "origin", theURL)
	fixture.Run(repo, "git", "config", "remote.origin.promisor", "true")
	fixture.Run(repo, "git", "config", "remote.origin.partialclonefilter", "blob:none")
	fixture.Run(repo, "git", "update-ref", "refs/remotes/origin/main", later)
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work")
	write(t, "work.md", "a line of the branch's own\n")
	own := commit(t, "the branch's own commit")
	const elsewhere = "https://github.com/invented-org/another-repo.git"
	spaced := filepath.Join(t.TempDir(), "a destination.git")
	fixture.Run(repo, "git", "remote", "add", "local", spaced)
	fixture.Run(repo, "git", "update-ref", "refs/remotes/local/main", later)
	// withToken is theURL with a token in its userinfo, built in pieces so that no line of this
	// file holds a URL with a credential, which the shared public leak check refuses.
	userinfo := "x-access-token:invented-token"
	withToken := "https://" + userinfo + "@github.com/" + theRepo + ".git"

	for _, tc := range []struct {
		name string
		// remote and url are what git hands the hook. For a push to a URL, remote is the URL as
		// typed and url the one git reaches, after any insteadOf rewrites it.
		remote, url string
		// reads are the commits the push reads, and refused is whether the push is refused.
		reads   []string
		refused bool
	}{
		{name: "origin by name", remote: "origin", url: theURL},
		{name: "the URL", remote: theURL, url: theURL},
		{name: "the URL with a token in its userinfo", remote: withToken, url: withToken},
		{name: "a URL an insteadOf rewrites to the destination's", remote: "gh:" + theRepo, url: theURL},
		{name: "the URL in git's scp syntax for ssh", remote: "git@github.com:" + theRepo + ".git", url: "git@github.com:" + theRepo + ".git"},
		{name: "the URL in other capitals", remote: "https://github.com/Invented-Org/Invented-Repo.git", url: "https://github.com/Invented-Org/Invented-Repo.git"},
		{name: "a local path holding a space, which local names", remote: spaced, url: spaced},
		{
			name: "a URL no remote names", remote: elsewhere, url: elsewhere,
			reads: []string{short(first), short(named), short(later), short(own)}, refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			req := PushRequest{
				Dir:     repo,
				Remote:  tc.remote,
				URL:     tc.url,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: own, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
			}
			want := tc.reads
			if want == nil {
				want = []string{short(own)}
			}
			if read := readCommits(t, guard, req); !slices.Equal(read, want) {
				t.Errorf("the push reads %d commits, %q, want %q", len(read), read, want)
			}
			err := guard.Push(bound(t), req)
			if !tc.refused {
				if err != nil {
					t.Errorf("the push of a commit that names nothing: %v", err)
				}
				return
			}
			reports(t, refusal(t, err).Lines, "commit "+short(named)+" author: "+leakguardtest.RulesHostClass)
		})
	}
}

// Only the destination's own remote-tracking refs say what it holds. A commit that another
// remote's tracking ref holds, here a commit of another repository that the push's branch is built
// on, is one the destination need not have, so the push reads it whether it names the destination
// by its remote's name or by its URL. That commit's author is one a rule refuses, and the report
// names it and nothing else. A guard that took every remote's tracking refs for the destination's
// would pass this push and publish the commit unread, and so would one that read the destination
// out of the path of a third remote's URL, which names another host.
func TestAPushReadsACommitOnlyAnotherRemoteHolds(t *testing.T) {
	home, repo := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(filepath.Dir(repo), fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, message string, env ...string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.With(env...).Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	write(t, "README.md", "one\n")
	base := commit(t, "the first main commit")
	fixture.Run(repo, "git", "remote", "add", "origin", theURL)
	fixture.Run(repo, "git", "update-ref", "refs/remotes/origin/main", base)
	// The other remote's side branch holds a commit the destination has never had.
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "side")
	write(t, "side.md", "a line of another repository\n")
	elsewhere := commit(t, "a commit of another repository", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
	fixture.Run(repo, "git", "remote", "add", "other", "https://github.com/invented-org/another-repo.git")
	fixture.Run(repo, "git", "update-ref", "refs/remotes/other/side", elsewhere)
	fixture.Run(repo, "git", "remote", "add", "lookalike", "https://example.invalid/a@github.com/"+theRepo+".git")
	fixture.Run(repo, "git", "update-ref", "refs/remotes/lookalike/side", elsewhere)
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work")
	write(t, "work.md", "a line of the branch's own\n")
	own := commit(t, "the branch's own commit")

	for _, tc := range []struct{ name, remote string }{
		{name: "origin by name", remote: "origin"},
		{name: "the URL", remote: theURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  tc.remote,
				URL:     theURL,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: own, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
			})
			reports(t, refusal(t, err).Lines, "commit "+short(elsewhere)+" author: "+leakguardtest.RulesHostClass)
		})
	}
}

// A remote whose URL only looks like the destination's is not held for a push to the destination.
// git and curl end a URL's authority at the first "/", "?" or "#", so each URL here reaches
// example.invalid, though GitHub's host and the destination's names stand after an "@" in its
// query or its fragment. A remote, look, has such a URL, and its main holds a commit of the other
// host whose author a rule refuses; the push's branch is built on it. A push to the destination's
// URL reads that commit, and the report names it.
func TestAPushHoldsNoRemoteWhoseURLOnlyLooksLikeTheDestinations(t *testing.T) {
	for _, url := range []string{
		"https://example.invalid?@github.com/" + theRepo + ".git",
		"https://example.invalid#@github.com/" + theRepo + ".git",
		"http://example.invalid?x=@github.com/" + theRepo + ".git",
	} {
		t.Run(url, func(t *testing.T) {
			home, repo := t.TempDir(), filepath.Join(t.TempDir(), "repo")
			fixture := fixturegit.Isolated(t, home)
			fixture.Run(filepath.Dir(repo), fixturegit.InitWorkTree(repo, "main")...)
			commit := func(t *testing.T, name, message string, env ...string) string {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, name), []byte(message+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				fixture.Run(repo, "git", "add", "-A")
				fixture.With(env...).Run(repo, "git", "commit", "--quiet", "-m", message)
				return fixture.Run(repo, "git", "rev-parse", "HEAD")
			}
			base := commit(t, "README.md", "the first main commit")
			fixture.Run(repo, "git", "remote", "add", "origin", theURL)
			fixture.Run(repo, "git", "update-ref", "refs/remotes/origin/main", base)
			elsewhere := commit(t, "side.md", "a commit of the other host", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
			fixture.Run(repo, "git", "remote", "add", "look", url)
			fixture.Run(repo, "git", "update-ref", "refs/remotes/look/main", elsewhere)
			own := commit(t, "work.md", "the branch's own commit")

			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  theURL,
				URL:     theURL,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: own, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
			})
			reports(t, refusal(t, err).Lines, "commit "+short(elsewhere)+" author: "+leakguardtest.RulesHostClass)
		})
	}
}

// A ref another remote files among the destination's remote's tracking refs is not one the
// destination holds. `--remotes=origin` takes every ref under refs/remotes/origin/, and three
// ordinary configurations file another repository's commits there, each with a real fetch: a
// remote named origin/private, whose refs git files under refs/remotes/origin/private/; a remote
// whose fetch refspec names a ref there (remote.mirror.fetch); and a remote that a legacy
// .git/remotes/legacy file defines, whose Pull line names a ref there, which `git fetch` reads
// although no configuration shows it. In each, a commit of the other repository whose author a
// rule refuses is read, whether the push names origin or its URL, and the report names it. A git
// built without the legacy file (git's WITH_BREAKING_CHANGES) fetches nothing from it, and the
// third row is skipped there, naming that git.
//
// The other remote is written to the configuration as `git remote add` writes it, its URL and its
// fetch refspec, rather than added with that command: from git 2.51, `git remote add` refuses a
// name that nests under another remote's (or that another nests under), but a remote such a git
// reads and fetches can still be named so, added by an older git or written with `git config`, as
// git 2.56.1 does for origin/private beside origin.
func TestAPushReadsACommitAnotherRemoteFiledAmongTheDestinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		// remote is the other remote's name, and refspec its fetch refspec, git's default for that
		// name where it is empty.
		remote, refspec string
		// remotesFile is whether the remote is defined by a .git/remotes file rather than by the
		// configuration.
		remotesFile bool
		// filed is the ref the fetch files the other repository's main at.
		filed string
	}{
		{name: "a remote named origin/private", remote: "origin/private", filed: "refs/remotes/origin/private/main"},
		{
			name: "a remote whose refspec names a ref among origin's", remote: "mirror",
			refspec: "+refs/heads/main:refs/remotes/origin/topic", filed: "refs/remotes/origin/topic",
		},
		{
			name: "a .git/remotes file whose Pull line names a ref among origin's", remote: "legacy", remotesFile: true,
			refspec: "+refs/heads/main:refs/remotes/origin/legacy", filed: "refs/remotes/origin/legacy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, root := t.TempDir(), t.TempDir()
			another, repo := filepath.Join(root, "another.git"), filepath.Join(root, "repo")
			fixture := fixturegit.Isolated(t, home)
			fixture.Run(root, fixturegit.InitBare(another, "main")...)
			fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
			commit := func(t *testing.T, name, message string, env ...string) string {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, name), []byte(message+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				fixture.Run(repo, "git", "add", "-A")
				fixture.With(env...).Run(repo, "git", "commit", "--quiet", "-m", message)
				return fixture.Run(repo, "git", "rev-parse", "HEAD")
			}
			base := commit(t, "README.md", "the first main commit")
			fixture.Run(repo, "git", "remote", "add", "origin", theURL)
			fixture.Run(repo, "git", "update-ref", "refs/remotes/origin/main", base)
			fixture.Run(repo, "git", "checkout", "--quiet", "-b", "side")
			elsewhere := commit(t, "side.md", "a commit of another repository", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
			fixture.Run(another, "git", "fetch", "--quiet", repo, "side:main")
			refspec := cmp.Or(tc.refspec, "+refs/heads/*:refs/remotes/"+tc.remote+"/*")
			if tc.remotesFile {
				remotes := filepath.Join(repo, ".git", "remotes")
				if err := os.MkdirAll(remotes, 0o700); err != nil {
					t.Fatal(err)
				}
				file := "URL: " + another + "\nPull: " + refspec + "\n"
				if err := os.WriteFile(filepath.Join(remotes, tc.remote), []byte(file), 0o600); err != nil {
					t.Fatal(err)
				}
				if out, err := fixture.Try(repo, "git", "fetch", "--quiet", tc.remote); err != nil {
					t.Skipf("%s fetches nothing from a .git/remotes file: %s", fixture.Run(repo, "git", "version"), out)
				}
			} else {
				fixture.Run(repo, "git", "config", "remote."+tc.remote+".url", another)
				fixture.Run(repo, "git", "config", "remote."+tc.remote+".fetch", refspec)
				fixture.Run(repo, "git", "fetch", "--quiet", tc.remote)
			}
			if filed := fixture.Run(repo, "git", "rev-parse", tc.filed); filed != elsewhere {
				t.Fatalf("%s is %s after the fetch from %s, want %s", tc.filed, filed, tc.remote, elsewhere)
			}
			fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work")
			own := commit(t, "work.md", "the branch's own commit")

			for _, remote := range []string{"origin", theURL} {
				guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
				err := guard.Push(bound(t), PushRequest{
					Dir:     repo,
					Remote:  remote,
					URL:     theURL,
					Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: own, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
				})
				want := "commit " + short(elsewhere) + " author: " + leakguardtest.RulesHostClass
				if refused := refusal(t, err); len(refused.Lines) != 1 || !strings.HasPrefix(refused.Lines[0], want+": ") {
					t.Errorf("the push to %s: the report is %q, want one line of %q", remote, refused.Lines, want)
				}
			}
		})
	}
}

// A remote that fetches from one repository and pushes to another says nothing of what its push
// destination holds: its tracking refs hold what the repository it fetches from has. origin here
// fetches from another repository and pushes to the destination (remote.origin.pushurl), and its
// tracking ref holds a commit of that other repository, whose author a rule refuses. A push to
// origin by name reads that commit, and the report names it.
func TestAPushReadsWhatItsRemoteFetchedFromAnotherRepository(t *testing.T) {
	home, repo := t.TempDir(), filepath.Join(t.TempDir(), "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(filepath.Dir(repo), fixturegit.InitWorkTree(repo, "main")...)
	commit := func(t *testing.T, name, message string, env ...string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(message+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		fixture.Run(repo, "git", "add", "-A")
		fixture.With(env...).Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	elsewhere := commit(t, "side.md", "a commit of another repository", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
	fixture.Run(repo, "git", "remote", "add", "origin", "https://github.com/invented-org/another-repo.git")
	fixture.Run(repo, "git", "remote", "set-url", "--push", "origin", theURL)
	fixture.Run(repo, "git", "update-ref", "refs/remotes/origin/main", elsewhere)
	own := commit(t, "work.md", "the branch's own commit")

	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	err := guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     theURL,
		Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: own, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
	})
	reports(t, refusal(t, err).Lines, "commit "+short(elsewhere)+" author: "+leakguardtest.RulesHostClass)
}

// A push from a shallow repository whose commits run down to a commit with no parent there is
// refused at once, naming that commit and the fetch that ends the boundary, rather than read. A
// shallow commit's parents are not in the repository, so the walk stops at it, and where nothing
// the destination is taken to hold reaches it, the destination's own commits beneath it read as
// the push's: a scan of history the destination has published, which refuses the push over a
// commit the destination already holds. The repository here is a clone of the destination's main
// two commits deep, and its branch merges a base branch fetched three deep from a seed of the
// test's own. The destination holds main alone, and the base forks from main's m3, whose author a
// rule refuses, through s1, a commit of the seed's own, so the shallow commit three deep on the
// base's side is s1, which the destination does not hold. It has to be one the destination lacks:
// the destination is read for what it holds (destinationHolds), and a commit it holds stops the
// walk before any shallow commit beneath it, so a base that forked from m3 directly, its shallow
// commit m3, would read nothing past the boundary at all (R10 of dispatch://INFRA-3524's spec,
// under its plan's decision 18). A branch on origin/main alone, from the same clone, reads its own
// commit and passes, so a shallow clone pushes as before; and once a `git fetch --unshallow` from
// the seed, which holds s1's history, has run, the merge's push reads s1, the base's commits, the
// merge and its own commit, none of main's, and passes. The refusal names `git fetch --unshallow
// origin` as before, though the destination cannot deepen a commit it does not hold.
//
// The fetch the refusal names is from the push's own destination: the remote a push names, even
// one whose refs the guard does not hold (here because a remote named origin/extra can file refs
// among origin's), and for a push to a URL, that URL as the URL rule prints it
// (redact.URLCredentials): its authority, which holds the token, written as the mark and its path
// kept, so no part of the token is printed. The failure's own error is read, beneath the
// redaction a refusal's text gets.
func TestAPushPastAShallowBoundaryIsRefusedNamingTheFetch(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, seed, clone := filepath.Join(root, "remote.git"), filepath.Join(root, "seed"), filepath.Join(root, "clone")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(seed, "main")...)
	write := func(t *testing.T, repo, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, repo, message string, env ...string) string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		fixture.With(env...).Run(repo, "git", "commit", "--quiet", "-m", message)
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	// The seed: main is m1 to m7, and base forks from m3 through s1, a commit of the seed's own,
	// then b1, and merges m7 back in. The destination holds main alone.
	write(t, seed, "main.md", "1\n")
	commit(t, seed, "m1")
	write(t, seed, "main.md", "1\n2\n")
	commit(t, seed, "m2")
	write(t, seed, "main.md", "1\n2\n3\n")
	commit(t, seed, "m3", "GIT_AUTHOR_EMAIL=builder@"+leakguardtest.RulesHost)
	fixture.Run(seed, "git", "checkout", "--quiet", "-b", "base")
	write(t, seed, "seed.md", "a line the seed alone holds\n")
	s1 := commit(t, seed, "s1")
	write(t, seed, "base.md", "a line of the base branch\n")
	b1 := commit(t, seed, "b1")
	fixture.Run(seed, "git", "checkout", "--quiet", "main")
	for i := 4; i <= 7; i++ {
		write(t, seed, "main.md", strings.Repeat("x\n", i))
		commit(t, seed, "m"+strconv.Itoa(i))
	}
	fixture.Run(seed, "git", "checkout", "--quiet", "base")
	fixture.Run(seed, "git", "merge", "--quiet", "--no-ff", "-m", "b2 merges main", "main")
	b2 := fixture.Run(seed, "git", "rev-parse", "HEAD")
	fixture.Run(seed, "git", "push", "--quiet", remote, "main")

	// The clone holds main two deep, and the seed's base three deep at FETCH_HEAD, on no tracking
	// ref, as a fetch of a branch of another repository leaves it. Three deep from b2 is s1 on the
	// base's side and m6 on main's: both are shallow commits, m4 and m5 are not here, and the
	// destination does not hold s1.
	fixture.Run(root, "git", "clone", "--quiet", "--depth=2", "--branch=main", "file://"+remote, clone)
	fixture.Run(clone, "git", "fetch", "--quiet", "--depth=3", "file://"+seed, "base")
	if shallow := fixture.Run(clone, "git", "rev-parse", "--is-shallow-repository"); shallow != "true" {
		t.Fatalf("the clone's --is-shallow-repository is %q, want true", shallow)
	}
	fixture.Run(clone, "git", "checkout", "--quiet", "-b", "plain", "origin/main")
	write(t, clone, "plain.md", "a line of the plain branch\n")
	plain := commit(t, clone, "the plain branch's own commit")
	fixture.Run(clone, "git", "checkout", "--quiet", "-b", "work", "origin/main")
	fixture.Run(clone, "git", "merge", "--quiet", "--no-ff", "-m", "work merges base", "FETCH_HEAD")
	merge := fixture.Run(clone, "git", "rev-parse", "HEAD")
	write(t, clone, "work.md", "a line of the branch's own\n")
	own := commit(t, clone, "the branch's own commit")

	request := func(ref, head string) PushRequest {
		return PushRequest{
			Dir:     clone,
			Remote:  "origin",
			URL:     "file://" + remote,
			Updates: []Update{{LocalRef: ref, LocalSHA: head, RemoteRef: ref, RemoteSHA: noSHA}},
		}
	}
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	if read, want := readCommits(t, guard, request("refs/heads/plain", plain)), []string{short(plain)}; !slices.Equal(read, want) {
		t.Errorf("the plain branch's push reads %q, want only its own commit, %q", read, want)
	}
	if err := guard.Push(bound(t), request("refs/heads/plain", plain)); err != nil {
		t.Errorf("the plain branch's push from a shallow clone: %v", err)
	}

	err := guard.Push(bound(t), request("refs/heads/work", own))
	failed := setupFailure(t, err)
	for _, want := range []string{short(s1), "shallow", "`git fetch --unshallow origin`"} {
		if !strings.Contains(failed.Error(), want) {
			t.Errorf("the setup failure %q does not say %q", failed.Error(), want)
		}
	}

	fixture.Run(clone, "git", "config", "remote.origin/extra.url", "file://"+remote)
	fixture.Run(clone, "git", "config", "remote.origin/extra.fetch", "+refs/heads/*:refs/remotes/origin/extra/*")
	userinfo := "x-access-token:invented-token"
	withToken := "https://" + userinfo + "@github.com/" + theRepo + ".git"
	for _, tc := range []struct{ name, remote, url, fetch string }{
		{name: "a push to origin, which the guard does not hold", remote: "origin", url: "file://" + remote, fetch: "origin"},
		{name: "a push to a URL with a token in its userinfo", remote: withToken, url: withToken, fetch: "https://" + redact.URLWithCredentials + "/" + theRepo + ".git"},
	} {
		req := request("refs/heads/work", own)
		req.Remote, req.URL = tc.remote, tc.url
		said := setupFailure(t, guard.Push(bound(t), req)).Err.Error()
		if want := "`git fetch --unshallow " + tc.fetch + "`"; !strings.Contains(said, want) {
			t.Errorf("%s: the failure %q does not say %q", tc.name, said, want)
		}
		if strings.Contains(said, userinfo) {
			t.Errorf("%s: the failure %q holds the URL's userinfo", tc.name, said)
		}
	}
	fixture.Run(clone, "git", "config", "--remove-section", "remote.origin/extra")

	// The destination cannot deepen s1, which it does not hold, and the seed can.
	fixture.Run(clone, "git", "fetch", "--quiet", "--unshallow", "file://"+seed, "base")
	read := readCommits(t, guard, request("refs/heads/work", own))
	want := []string{short(s1), short(b1), short(b2), short(merge), short(own)}
	slices.Sort(read)
	slices.Sort(want)
	if !slices.Equal(read, want) {
		t.Errorf("after the unshallow, the push reads %q, want s1, the base's commits, the merge and its own, %q", read, want)
	}
	if err := guard.Push(bound(t), request("refs/heads/work", own)); err != nil {
		t.Errorf("the push after the unshallow: %v", err)
	}
}

// commitFiles writes files into repo's working tree, each path to its content, a directory a path
// names made where it is not there, commits the working tree with message, and answers the commit.
func commitFiles(t *testing.T, fixture fixturegit.Runner, repo string, files map[string]string, message string) string {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture.Run(repo, "git", "add", "-A")
	fixture.Run(repo, "git", "commit", "--quiet", "-m", message)
	return fixture.Run(repo, "git", "rev-parse", "HEAD")
}

// pushOf is a repository whose base, a commit of README.md alone, a destination holds on its
// main, and head, the commit on base that writes the files added, each path to its content, which
// the push adds. It answers too the URL the push is made to, the destination's path: a bare
// repository of the test's own, which a push by path wrote base to, so the repository has no
// remote and no remote-tracking ref. The destination names no repository on github.com, so the
// guard scans the push without reading a visibility (Guard.scanThrough), and its path is a URL
// the reads of what the destination holds reach under the test's GIT_ALLOW_PROTOCOL=file.
func pushOf(t *testing.T, added map[string]string) (repo, base, head, destination string) {
	t.Helper()
	home, root := t.TempDir(), t.TempDir()
	repo, destination = filepath.Join(root, "repo"), filepath.Join(t.TempDir(), "destination.git")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	fixture.Run(root, fixturegit.InitBare(destination, "main")...)
	base = commitFiles(t, fixture, repo, map[string]string{"README.md": "one\n"}, "the commit the destination holds")
	fixture.Run(repo, "git", "push", "--quiet", destination, base+":refs/heads/main")
	head = commitFiles(t, fixture, repo, added, "the commit the push adds")
	return repo, base, head, destination
}

// mainPush is the push of head to main, where the destination holds base, to remote at url.
func mainPush(repo, base, head, remote, url string) PushRequest {
	return PushRequest{
		Dir:     repo,
		Remote:  remote,
		URL:     url,
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: base}},
	}
}

// v0Negotiation is what git 2.47.3 printed on standard error, exit 255, for `git -c
// protocol.version=0 fetch --negotiate-only --negotiation-tip=<sha> file://<bare>`: a fetch
// negotiation needs protocol v2, and a destination served on v0 or v1 makes git answer so.
var v0Negotiation = []string{"warning: --negotiate-only requires protocol v2", "fatal: the remote end hung up unexpectedly"}

// A new branch's first push is judged on what it adds to the destination, not on the
// destination's own history: its range is bounded by what the destination itself says it holds,
// its advertised refs and a fetch negotiation from the branch's commit (R10 of
// dispatch://INFRA-3524's spec). Each case pushes, through Guard.Push under the fixture rule file,
// a new branch work of a clone to a local bare destination whose main holds a commit of a seed's
// that adds a line the rules refuse, and the branch's own commit adds none. The push passes in
// four cases: the clone's remote-tracking refs are current (current); they are stale, since the
// clone took the destination's main with `git fetch <url> main`, which files no tracking ref, and
// branched from FETCH_HEAD (stale); the clone took the destination's main at T0 so, the
// destination then moved to a T1 the clone does not hold, and the branch starts from T0, so the
// advertised refs name nothing the clone holds and the negotiation finds T0 (moved); and the push
// names by URL a destination no remote of the clone names, the clone's origin being the seed
// (by URL).
//
// In the fifth case the clone's origin named a second bare repository while a push to it filed
// refs/remotes/origin/rehearsal at a commit that adds the refused line, and `git remote set-url`
// then named the destination, which does not hold that commit: the push of a branch on it is
// refused over it, since where the negotiation answers, no tracking ref bounds the range. In the
// sixth, the clone's configuration holds url.<an empty bare repository>.insteadOf the
// destination's URL, which the clone's own `git ls-remote` follows, and the push of the moved
// case passes all the same: the reads are made of the URL the hook was given, outside the clone's
// configuration. In the seventh the negotiation fails as against a destination served on protocol
// v0 (v0Negotiation), on a stand-in git first on the guard's PATH that answers `fetch
// --negotiate-only` so and runs every other command as the real git: the reads take no
// configuration of the caller's, so no protocol.version=0 of the caller's reaches them, and the
// row makes the v0 answer on the client's side, after checking that the real git under
// protocol.version=0 answers so. The destination moved to a T1 the clone does not hold, the branch
// starts from the clone's older main, at which its origin/main stands, and the push passes as it
// did before the reads, bounded by that tracking ref with the advertised commits beside it. In the
// eighth both reads fail, the destination a git:// URL on 127.0.0.1 nothing listens on, which the
// clone's origin names and whose tracking ref holds the destination's main: the push is judged as
// it was before the reads, bounded by the tracking refs, with the same text.
//
// In the ninth the push can write somewhere other than the hook's URL, once for each way the guard
// tells so (writesOnlyToItsURL): the wrapper's mark for `--receive-pack` in the guard's
// environment; a remote.origin.receivepack key naming the git-receive-pack of a second bare
// repository; a remote.origin.vcs key; an origin URL `ext::git %s <destination>`, which holds `::`
// and which the guard's GIT_ALLOW_PROTOCOL lets through; and a GIT_EXEC_PATH that is a symbolic
// link to git's own exec path, another path to ownExecPath, under which every git the guard runs
// works as under git's own. The destination holds a commit that adds the refused line, which the
// clone took with `git fetch <url> main`, as in the stale case, so that no tracking ref holds it,
// and the branch starts from it. The two reads would answer for the destination, which holds that
// commit, and pass the push; the guard makes neither, the tracking refs bound the range as they
// did before the reads, and the push is refused over that commit, as it was before them. In every
// case but the fifth and the ninth, a second commit of the branch that adds the refused line,
// which the destination does not hold, is refused, alone.
func TestANewBranchIsJudgedOnWhatItAddsToTheDestination(t *testing.T) {
	const class = leakguardtest.RulesHostClass
	refused := "the runner answers at " + leakguardtest.RulesHost + "\n"
	// rig is one case's destination: a bare repository whose main seed, a work tree of the rig's
	// own, moves, and root, the case's directory, which the clone is made in.
	type rig struct {
		fixture                 fixturegit.Runner
		root, destination, seed string
	}
	// move commits files on the seed's main, pushes it to the destination's main, and answers
	// the commit.
	move := func(t *testing.T, r rig, files map[string]string, message string) string {
		t.Helper()
		commit := commitFiles(t, r.fixture, r.seed, files, message)
		r.fixture.Run(r.seed, "git", "push", "--quiet", r.destination, "main")
		return commit
	}
	// held moves the destination's main to a commit that adds the refused line, and answers it.
	held := func(t *testing.T, r rig) string {
		t.Helper()
		return move(t, r, map[string]string{"held.md": refused}, "a commit of the destination's that adds the refused line")
	}
	// later moves the destination's main to a commit of no interest, and answers it.
	later := func(t *testing.T, r rig) string {
		t.Helper()
		return move(t, r, map[string]string{"later.md": "a later line\n"}, "a later commit of the destination's")
	}
	// cloned is a clone of the repository at from, whose origin names from.
	cloned := func(t *testing.T, r rig, from string) string {
		t.Helper()
		repo := filepath.Join(r.root, "repo")
		r.fixture.Run(r.root, "git", "clone", "--quiet", from, repo)
		return repo
	}
	// fetched is the destination's main, fetched into repo by the destination's URL, which files
	// it at FETCH_HEAD and at no remote-tracking ref.
	fetched := func(t *testing.T, r rig, repo string) string {
		t.Helper()
		r.fixture.Run(repo, "git", "fetch", "--quiet", r.destination, "main")
		return r.fixture.Run(repo, "git", "rev-parse", "FETCH_HEAD")
	}
	// trackedMain is the commit the clone's origin/main is at.
	trackedMain := func(r rig, repo string) string {
		return r.fixture.Run(repo, "git", "rev-parse", "refs/remotes/origin/main")
	}
	// unheld is the clone of the ninth case and the commit its branch starts at: a commit of the
	// destination's that adds the refused line, fetched by the destination's URL after the clone
	// was made, so that it is at FETCH_HEAD and at no remote-tracking ref.
	unheld := func(t *testing.T, r rig) (repo, start string) {
		t.Helper()
		repo = cloned(t, r, r.destination)
		held(t, r)
		return repo, fetched(t, r, repo)
	}
	// elsewhere is a second bare repository of the case's own, where a receive-pack of the
	// push's own choosing would write.
	elsewhere := func(t *testing.T, r rig) string {
		t.Helper()
		second := filepath.Join(r.root, "elsewhere.git")
		r.fixture.Run(r.root, fixturegit.InitBare(second, "main")...)
		return second
	}
	// heldRefused is the ninth case's report: the destination's commit that adds the refused line,
	// which no tracking ref holds, read as before the reads.
	heldRefused := []string{"held.md: " + class}
	for _, tc := range []struct {
		name string
		// build makes the case's clone, and answers it, the commit its new branch starts at, and
		// the remote and the URL git hands the hook for the push.
		build func(t *testing.T, r rig) (repo, start, remote, url string)
		// env is what the guard's environment holds beyond guardWith's, nil for nothing.
		env func(t *testing.T) []string
		// want is the report of the push of the branch's own commit, nil where it passes.
		want []string
		// second is whether a second commit of the branch, which adds the refused line, is
		// pushed too, and refused alone.
		second bool
	}{
		{
			name: "current",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				held(t, r)
				repo := cloned(t, r, r.destination)
				return repo, trackedMain(r, repo), "origin", r.destination
			},
			second: true,
		},
		{
			name: "stale",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				repo := cloned(t, r, r.destination)
				held(t, r)
				return repo, fetched(t, r, repo), "origin", r.destination
			},
			second: true,
		},
		{
			name: "moved",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				repo := cloned(t, r, r.destination)
				held(t, r)
				start := fetched(t, r, repo)
				later(t, r)
				return repo, start, "origin", r.destination
			},
			second: true,
		},
		{
			name: "by URL",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				held(t, r)
				repo := cloned(t, r, r.seed)
				return repo, trackedMain(r, repo), r.destination, r.destination
			},
			second: true,
		},
		{
			name: "a tracking ref names a commit the destination does not hold",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				held(t, r)
				repo := cloned(t, r, r.destination)
				rehearsal := filepath.Join(r.root, "rehearsal.git")
				r.fixture.Run(r.root, fixturegit.InitBare(rehearsal, "main")...)
				rehearsed := commitFiles(t, r.fixture, repo, map[string]string{"rehearsed.md": refused}, "a commit the destination does not hold")
				r.fixture.Run(repo, "git", "remote", "set-url", "origin", rehearsal)
				r.fixture.Run(repo, "git", "push", "--quiet", "origin", "HEAD:refs/heads/rehearsal")
				r.fixture.Run(repo, "git", "remote", "set-url", "origin", r.destination)
				if filed := r.fixture.Run(repo, "git", "rev-parse", "refs/remotes/origin/rehearsal"); filed != rehearsed {
					t.Fatalf("refs/remotes/origin/rehearsal is at %s, want the rehearsed commit %s: the case would prove nothing", filed, rehearsed)
				}
				return repo, rehearsed, "origin", r.destination
			},
			want: []string{"rehearsed.md: " + class},
		},
		{
			name: "an insteadOf of the clone",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				repo := cloned(t, r, r.destination)
				held(t, r)
				start := fetched(t, r, repo)
				later(t, r)
				empty := filepath.Join(r.root, "empty.git")
				r.fixture.Run(r.root, fixturegit.InitBare(empty, "main")...)
				r.fixture.Run(repo, "git", "config", "url."+empty+".insteadOf", r.destination)
				if listed := r.fixture.Run(repo, "git", "ls-remote", r.destination); listed != "" {
					t.Fatalf("git ls-remote of %s in the clone lists %q, want the empty repository's nothing: the case would prove nothing", r.destination, listed)
				}
				return repo, start, "origin", r.destination
			},
			second: true,
		},
		{
			name: "protocol v0",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				held(t, r)
				repo := cloned(t, r, r.destination)
				start := trackedMain(r, repo)
				later(t, r)
				out, err := r.fixture.Try(repo, "git", "-c", "protocol.version=0", "fetch", "--negotiate-only", "--negotiation-tip="+start, "file://"+r.destination)
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 255 || out != strings.Join(v0Negotiation, "\n")+"\n" {
					t.Fatalf("the real git's negotiation under protocol.version=0 answered %q (%v), want %q and exit 255: the stand-in answers as no git does", out, err, v0Negotiation)
				}
				return repo, start, "origin", r.destination
			},
			env: func(t *testing.T) []string {
				bin := t.TempDir()
				writeProgram(t, bin, "git", "case \" $* \" in\n*\" --negotiate-only \"*) "+printsToStderr(v0Negotiation...)+"  exit 255 ;;\nesac\n"+
					"exec "+shQuote(fixturegit.CompiledGit(t))+" \"$@\"\n")
				return []string{pathWith(bin)}
			},
			second: true,
		},
		{
			name: "both reads fail",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				held(t, r)
				repo := cloned(t, r, r.destination)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				url := fmt.Sprintf("git://%s/destination.git", listener.Addr())
				if err := listener.Close(); err != nil {
					t.Fatal(err)
				}
				r.fixture.Run(repo, "git", "remote", "set-url", "origin", url)
				return repo, trackedMain(r, repo), "origin", url
			},
			// git:// is let through, so the reads fail on a port nothing listens on rather than
			// on the transport.
			env:    func(*testing.T) []string { return []string{"GIT_ALLOW_PROTOCOL=file:git"} },
			second: true,
		},
		{
			name: "a push that can write elsewhere: the wrapper's mark",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				repo, start := unheld(t, r)
				return repo, start, "origin", r.destination
			},
			env:  func(*testing.T) []string { return []string{PushNamesReceivePackVar + "=1"} },
			want: heldRefused,
		},
		{
			name: "a push that can write elsewhere: remote.origin.receivepack",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				repo, start := unheld(t, r)
				r.fixture.Run(repo, "git", "config", "remote.origin.receivepack", "git-receive-pack "+shQuote(elsewhere(t, r))+" #")
				return repo, start, "origin", r.destination
			},
			want: heldRefused,
		},
		{
			name: "a push that can write elsewhere: remote.origin.vcs",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				repo, start := unheld(t, r)
				r.fixture.Run(repo, "git", "config", "remote.origin.vcs", "invented")
				return repo, start, "origin", r.destination
			},
			want: heldRefused,
		},
		{
			name: "a push that can write elsewhere: a URL with ::",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				repo, start := unheld(t, r)
				url := "ext::git %s " + r.destination
				r.fixture.Run(repo, "git", "remote", "set-url", "origin", url)
				return repo, start, "origin", url
			},
			// ext is let through, so that the reads, were they made, would answer for the
			// destination.
			env:  func(*testing.T) []string { return []string{"GIT_ALLOW_PROTOCOL=file:ext"} },
			want: heldRefused,
		},
		{
			name: "a push that can write elsewhere: a GIT_EXEC_PATH that is not git's own",
			build: func(t *testing.T, r rig) (string, string, string, string) {
				repo, start := unheld(t, r)
				return repo, start, "origin", r.destination
			},
			env: func(t *testing.T) []string {
				own := fixturegit.Isolated(t, t.TempDir()).Run(t.TempDir(), "git", "--exec-path")
				link := filepath.Join(t.TempDir(), "git-core")
				if err := os.Symlink(own, link); err != nil {
					t.Fatal(err)
				}
				return []string{"GIT_EXEC_PATH=" + link}
			},
			want: heldRefused,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			r := rig{
				fixture:     fixturegit.Isolated(t, t.TempDir()),
				root:        root,
				destination: filepath.Join(root, "destination.git"),
				seed:        filepath.Join(root, "seed"),
			}
			r.fixture.Run(root, fixturegit.InitBare(r.destination, "main")...)
			r.fixture.Run(root, fixturegit.InitWorkTree(r.seed, "main")...)
			move(t, r, map[string]string{"README.md": "one\n"}, "the destination's first commit")
			repo, start, remote, url := tc.build(t, r)
			r.fixture.Run(repo, "git", "checkout", "--quiet", "-b", "work", start)
			var env []string
			if tc.env != nil {
				env = tc.env(t)
			}
			push := func(t *testing.T, head string) error {
				t.Helper()
				guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, env...)
				return guard.Push(bound(t), PushRequest{
					Dir:     repo,
					Remote:  remote,
					URL:     url,
					Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: head, RemoteRef: "refs/heads/work", RemoteSHA: noSHA}},
				})
			}
			own := commitFiles(t, r.fixture, repo, map[string]string{"work.md": "a line of the branch's own\n"}, "the branch's own commit")
			if err := push(t, own); tc.want == nil {
				if err != nil {
					t.Errorf("the push of the branch's own commit, which adds no refused line: %v", err)
				}
			} else {
				reports(t, refusal(t, err).Lines, tc.want...)
			}
			if !tc.second {
				return
			}
			named := commitFiles(t, r.fixture, repo, map[string]string{"work.md": "a line of the branch's own\n" + refused}, "a second commit of the branch's own")
			reports(t, refusal(t, push(t, named)).Lines, "work.md: "+class)
		})
	}
}

// ParseUpdates reads git's own pre-push lines, which is how a `legion leak-check` standing in for
// a pre-push hook reads what the push would move, and refuses a line of another shape rather than
// guessing at it.
func TestParseUpdates(t *testing.T) {
	lines := "refs/heads/main abc refs/heads/main def\n\nrefs/heads/work 123 refs/heads/work " + noSHA + "\n"
	updates, err := ParseUpdates(strings.NewReader(lines))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Update{
		{LocalRef: "refs/heads/main", LocalSHA: "abc", RemoteRef: "refs/heads/main", RemoteSHA: "def"},
		{LocalRef: "refs/heads/work", LocalSHA: "123", RemoteRef: "refs/heads/work", RemoteSHA: noSHA},
	}
	if !slices.Equal(updates, want) {
		t.Errorf("read %v, want %v", updates, want)
	}
	if updates[0].Line() != "refs/heads/main abc refs/heads/main def" {
		t.Errorf("the line is %q", updates[0].Line())
	}
	if _, err := ParseUpdates(strings.NewReader("refs/heads/main abc\n")); err == nil {
		t.Error("a line of three fields was read as a pre-push line")
	}
}

// A git read of a push's records that fails quotes what git wrote on its standard error, never its
// standard output, which is the push's own content: a stand-in git that writes a megabyte of a
// diff's added lines to its standard output and its reason to its standard error is refused with
// the reason alone. A standard error past gitErrorBytes is cut to its end, which holds git's
// `fatal:` line, after the count of the bytes left out. Each stand-in is the one git first on PATH.
func TestAFailedGitReadQuotesItsCappedStandardErrorAlone(t *testing.T) {
	failingGit := func(t *testing.T, script string) *Guard {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))})
	}
	const content = "+the pushed line"
	t.Run("a megabyte of the push's content on standard output", func(t *testing.T) {
		guard := failingGit(t, "yes '"+content+"' | head -c 1048576\necho 'fatal: bad object HEAD' >&2")
		_, err := guard.git(bound(t), t.TempDir(), "diff-tree", "HEAD")
		if want := "git diff-tree exited 1: fatal: bad object HEAD"; err == nil || err.Error() != want {
			t.Errorf("git = %.200v, want %q", err, want)
		}
	})
	t.Run("a standard error past the cap, its reason last", func(t *testing.T) {
		const reason = "fatal: unable to read tree"
		guard := failingGit(t, "yes 'warning: refname is ambiguous' | head -n 4000 >&2\necho '"+reason+"' >&2")
		_, err := guard.git(bound(t), t.TempDir(), "diff-tree", "HEAD")
		if err == nil {
			t.Fatal("git = nil, want its failure")
		}
		said := strings.TrimPrefix(err.Error(), "git diff-tree exited 1: ")
		count, kept, found := strings.Cut(said, " bytes left out) …")
		if !found || !strings.HasPrefix(count, "(") || len(kept) > gitErrorBytes || !strings.HasSuffix(kept, "\n"+reason) {
			t.Errorf("git quoted %d bytes, from %q to %q, want the count of the bytes left out and at most the last %d bytes of its standard error, its reason included",
				len(said), said[:min(len(said), 40)], said[max(0, len(said)-40):], gitErrorBytes)
		}
	})
	// Two-byte characters then one byte put the cut on the second byte of a character.
	t.Run("a cut inside a character moves to the next one", func(t *testing.T) {
		said := gitSaid(strings.Repeat("é", gitErrorBytes) + "x")
		if want := "(4098 bytes left out) …" + strings.Repeat("é", gitErrorBytes/2-1) + "x"; said != want {
			t.Errorf("gitSaid = %d bytes ending %q, want %d bytes ending %q: whole characters after the count",
				len(said), said[max(0, len(said)-25):], len(want), want[len(want)-25:])
		}
	})
	// Bytes that start no character still leave gitErrorBytes of them quoted, never none.
	t.Run("a run that starts no character keeps the whole cap", func(t *testing.T) {
		said := gitSaid(strings.Repeat("\x80", gitErrorBytes+10))
		if want := "(10 bytes left out) …" + strings.Repeat("\x80", gitErrorBytes); said != want {
			t.Errorf("gitSaid = %d bytes, want %d: the count of 10 and the last %d bytes", len(said), len(want), gitErrorBytes)
		}
	})
}
