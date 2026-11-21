package leakguard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
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
