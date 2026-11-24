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
		// want is the report, as a function of the commit's sha, and nil where the push passes.
		want func(head string) []string
	}{
		{
			name: "the message names a host",
			build: func(t *testing.T) string {
				branch(t, "message")
				write(t, "README.md", "one\ntwo\n")
				return commit(t, "moved the run to "+theHost+" this morning")
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " message: " + theClass}
			},
		},
		{
			name: "the author identity names a host",
			build: func(t *testing.T) string {
				branch(t, "author")
				write(t, "README.md", "one\nthree\n")
				return commit(t, "a message of no interest", "GIT_AUTHOR_EMAIL=builder@"+theHost)
			},
			want: func(head string) []string {
				return []string{"commit " + short(head) + " author: " + theClass}
			},
		},
		{
			name: "an added line of a tracked file names a host",
			build: func(t *testing.T) string {
				branch(t, "line")
				write(t, "README.md", "one\nthe runner answers at "+theHost+"\n")
				return commit(t, "a message of no interest")
			},
			want: func(string) []string { return []string{"README.md: " + theClass} },
		},
		{
			name: "the ref name names a host",
			build: func(t *testing.T) string {
				branch(t, "ref-name")
				write(t, "README.md", "one\nfour\n")
				return commit(t, "a message of no interest")
			},
			ref:    "refs/heads/legion/" + theHost,
			newRef: true,
			want:   func(string) []string { return []string{"the ref name: " + theClass} },
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
			if refused := refusal(t, err); !slices.Equal(refused.Lines, tc.want(head)) {
				t.Errorf("the report is %q, want %q", refused.Lines, tc.want(head))
			}
		})
	}
}

// A push the destination already holds everything of adds no commit and so has no record but its
// ref name, which is what a push of an existing commit to a new name is.
func TestAPushThatAddsNoCommitReadsNoCommit(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("the file names "+theHost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.Run(repo, "git", "add", "-A")
	fixture.Run(repo, "git", "commit", "--quiet", "-m", "a commit naming "+theHost)
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
	commit(t, "a main commit", "GIT_AUTHOR_EMAIL=builder@"+theHost)
	write(t, "README.md", "one\ntwo\nthree\n")
	commit(t, "a later main commit")
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
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
	named := commit(t, "a second commit of the branch's own", "GIT_AUTHOR_EMAIL=builder@"+theHost)
	want := []string{"commit " + short(named) + " author: " + theClass}
	if refused := refusal(t, push(t, named)); !slices.Equal(refused.Lines, want) {
		t.Errorf("the report is %q, want %q: the push's own commit, and none of main's", refused.Lines, want)
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
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("the runner answers at "+theHost+"\n"), 0o600); err != nil {
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
	want := []string{"README.md: " + theClass}
	if refused := refusal(t, err); !slices.Equal(refused.Lines, want) {
		t.Errorf("the report is %q, want %q", refused.Lines, want)
	}
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
	write("resolution.md", "the runner answers at "+theHost+"\n")
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
	want := []string{"resolution.md: " + theClass}
	if refused := refusal(t, err); !slices.Equal(refused.Lines, want) {
		t.Errorf("the report is %q, want %q", refused.Lines, want)
	}
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
	write("main.md", "the runner answers at "+theHost+"\n")
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
	write("copy.md", "the runner answers at "+theHost+"\n")
	onTop := commit("a commit no remote-tracking ref holds")

	for _, tc := range []struct {
		name string
		head string
		// want is the report, and nil where the push passes.
		want []string
	}{
		{name: "the merge alone", head: merge},
		{
			name: "a commit of the same line on top of the merge",
			head: onTop,
			want: []string{"copy.md: " + theClass},
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
			if refused := refusal(t, err); !slices.Equal(refused.Lines, tc.want) {
				t.Errorf("the report is %q, want %q", refused.Lines, tc.want)
			}
		})
	}
}

// ParseUpdates reads git's own pre-push lines, which is how a `legion leak-check` standing in for
// a pre-push hook reads what the push would move, and refuses a line of another shape rather than
// guessing at it: a line of three fields, and a line whose sha is not the 40 or 64 lowercase hex
// digits git writes. A sha of other bytes is refused without being quoted, so a control byte in it
// never reaches the report, and one that begins with "-" never reaches a git as an option.
func TestParseUpdates(t *testing.T) {
	sha1, sha256 := strings.Repeat("a1", 20), strings.Repeat("b2", 32)
	lines := "refs/heads/main " + sha1 + " refs/heads/main " + sha1 + "\n\nrefs/heads/work " + sha256 + " refs/heads/work " + noSHA + "\n"
	updates, err := ParseUpdates(strings.NewReader(lines))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Update{
		{LocalRef: "refs/heads/main", LocalSHA: sha1, RemoteRef: "refs/heads/main", RemoteSHA: sha1},
		{LocalRef: "refs/heads/work", LocalSHA: sha256, RemoteRef: "refs/heads/work", RemoteSHA: noSHA},
	}
	if !slices.Equal(updates, want) {
		t.Errorf("read %v, want %v", updates, want)
	}
	if updates[0].Line() != "refs/heads/main "+sha1+" refs/heads/main "+sha1 {
		t.Errorf("the line is %q", updates[0].Line())
	}
	if _, err := ParseUpdates(strings.NewReader("refs/heads/main " + sha1 + "\n")); err == nil {
		t.Error("a line of three fields was read as a pre-push line")
	}
	for _, tc := range []struct{ name, local, remote string }{
		{"a local sha holding an escape byte", "\x1b[31m" + strings.Repeat("a", 35), noSHA},
		{"a local sha shaped as an option", "-x", noSHA},
		{"a remote sha shaped as an option", sha1, "--output=" + strings.Repeat("a", 31)},
		{"a short sha", "a1b2c3d", noSHA},
		{"a sha in capitals", strings.ToUpper(sha1), noSHA},
		{"a sha of 41 digits", sha1 + "a", noSHA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseUpdates(strings.NewReader("refs/heads/main " + tc.local + " refs/heads/main " + tc.remote + "\n"))
			if err == nil {
				t.Fatalf("the line was read as a pre-push line")
			}
			if want := "pre-push line 1 holds a sha that is not the 40 or 64 lowercase hex digits git writes"; err.Error() != want {
				t.Errorf("the refusal is %q, want %q, which quotes no field", err, want)
			}
		})
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

// A push to a public destination that moves a ref to a blob or a tree, or to a tag of one however
// many tags deep, is refused as a setup failure. git sends the object, and a rev-list given its
// sha bare lists no commit for it and exits 0, so the rules, which read a push's commits, would
// read nothing of it and pass the push: a blob or a file of a tree that names a host would reach
// the destination unread. The guard reads the sha as `<sha>^{commit}`, which git refuses for such
// an object, and that refusal is the setup failure, whether the ref is new or the destination
// holds it at a commit already. The shared check here is the stand-in that passes every push
// (leakguardtest.Passes), so each refusal is the guard's own. The controls show the read takes
// nothing from a push the rules can read: a tag chain that ends at a commit is read as that commit
// is, its added line refused by the rules and a clean one passed.
func TestAPushOfARefToNoCommitIsRefused(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(t *testing.T, name, content string) string {
		t.Helper()
		write(t, filepath.Join(repo, name), content)
		fixture.Run(repo, "git", "add", "-A")
		fixture.Run(repo, "git", "commit", "--quiet", "-m", "a message of no interest")
		return fixture.Run(repo, "git", "rev-parse", "HEAD")
	}
	base := commit(t, "README.md", "one\n")
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")

	const named, clean = "the runner answers at " + theHost + "\n", "a line of ordinary prose\n"
	// blob is a blob of content in the repository's object store, on no commit.
	blob := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "notes.md")
		write(t, path, content)
		return strings.TrimSpace(fixture.Out(repo, "git", "hash-object", "-w", "--no-filters", path))
	}
	// tree is a tree of one file, notes.md, holding content, on no commit. It is written from an
	// index of its own, so the repository's index is left as it was.
	tree := func(t *testing.T, content string) string {
		t.Helper()
		index := fixture.With("GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index"))
		index.Run(repo, "git", "update-index", "--add", "--cacheinfo", "100644,"+blob(t, content)+",notes.md")
		return strings.TrimSpace(index.Out(repo, "git", "write-tree"))
	}
	// annotated is an annotated tag named name on object, whose message names nothing, and its sha.
	annotated := func(t *testing.T, name, object string) string {
		t.Helper()
		fixture.Run(repo, "git", "-c", "advice.nestedTag=false", "tag", "-a", "-m", "a tag of no interest", name, object)
		return fixture.Run(repo, "git", "rev-parse", "refs/tags/"+name)
	}
	push := func(t *testing.T, name, local, remote string) error {
		t.Helper()
		ref := "refs/tags/" + name
		guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
		return guard.Push(bound(t), PushRequest{
			Dir:     repo,
			Remote:  "origin",
			URL:     "https://github.com/invented-org/invented-repo.git",
			Updates: []Update{{LocalRef: ref, LocalSHA: local, RemoteRef: ref, RemoteSHA: remote}},
		})
	}

	for _, tc := range []struct {
		name string
		// local is the sha the ref is pushed at, made under the tag name name.
		local func(t *testing.T, name string) string
		// remote is the sha the destination holds the ref at: none, for a ref the push creates.
		remote string
		// kind is the type of the object at the end of the ref's tags.
		kind string
	}{
		{
			name:   "a lightweight tag on a blob that names a host",
			local:  func(t *testing.T, _ string) string { return blob(t, named) },
			remote: noSHA,
			kind:   "blob",
		},
		{
			name:   "an annotated tag on a blob that names a host",
			local:  func(t *testing.T, name string) string { return annotated(t, name, blob(t, named)) },
			remote: noSHA,
			kind:   "blob",
		},
		{
			name:   "a lightweight tag on a tree whose file names a host",
			local:  func(t *testing.T, _ string) string { return tree(t, named) },
			remote: noSHA,
			kind:   "tree",
		},
		{
			name: "a tag of a tag on a blob that names a host",
			local: func(t *testing.T, name string) string {
				return annotated(t, name, annotated(t, name+"-inner", blob(t, named)))
			},
			remote: noSHA,
			kind:   "blob",
		},
		{
			name:   "a blob that names a host over a ref the destination holds at a commit",
			local:  func(t *testing.T, _ string) string { return blob(t, named) },
			remote: base,
			kind:   "blob",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := strings.ReplaceAll(tc.name, " ", "-")
			local := tc.local(t, name)
			if got := fixture.Run(repo, "git", "cat-file", "-t", local+"^{}"); got != tc.kind {
				t.Fatalf("the fixture's object is a %s once its tags are peeled, want a %s", got, tc.kind)
			}
			failed := setupFailure(t, push(t, name, local, tc.remote))
			says(t, failed, "the push's commits", "git rev-list exited 128: error: "+local+"^{commit}: expected commit type, but the object dereferences to "+tc.kind+" type")
			if strings.Contains(failed.Error(), theHost) {
				t.Fatalf("the setup failure holds the text it refused: %v", failed)
			}
			t.Log(failed)
		})
	}

	t.Run("a tag of a tag on a commit whose added line names a host", func(t *testing.T) {
		fixture.Run(repo, "git", "checkout", "--quiet", "-B", "named", base)
		head := commit(t, "chain.md", named)
		local := annotated(t, "named-outer", annotated(t, "named-inner", head))
		want := []string{"chain.md: " + theClass}
		if refused := refusal(t, push(t, "named-outer", local, noSHA)); refused.By != byRules || !slices.Equal(refused.Lines, want) {
			t.Errorf("the refusal is by %q with the report %q, want this repository's own rules and %q", refused.By, refused.Lines, want)
		}
	})
	t.Run("an annotated tag on a commit that names nothing", func(t *testing.T) {
		fixture.Run(repo, "git", "checkout", "--quiet", "-B", "clean", base)
		head := commit(t, "clean.md", clean)
		if err := push(t, "clean-tag", annotated(t, "clean-tag", head), noSHA); err != nil {
			t.Fatalf("a push of a tag on a commit that names nothing: %v", err)
		}
	})
}

// Each file a pushed commit adds is read as text, whatever git would otherwise make of it. A diff
// prints "Binary files … differ" and no line at all for a file that holds a NUL byte, or whose
// path a `-diff` attribute covers (from the commit's own .gitattributes, from .git/info/attributes
// or from core.attributesFile) or a diff driver with `binary = true`; the destination still gets
// the file, so a line in it that names a host would pass the rules unread. A driver's textconv
// and an external diff command would change the text read rather than drop it, and diff-tree runs
// neither unless asked: those cases hold the read to that. Each case's commit adds one file that
// names a host, with the working tree at that commit as a push leaves it, and the rules must
// refuse that file. Each case's control shows its mechanism is live, that the read the case is
// about (git's default, or the textconv or external diff turned on) prints no line naming the host.
func TestAPushedCommitsFilesAreReadAsText(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(repo, "README.md"), "one\n")
	fixture.Run(repo, "git", "add", "-A")
	fixture.Run(repo, "git", "commit", "--quiet", "-m", "the commit the destination holds")
	base := fixture.Run(repo, "git", "rev-parse", "HEAD")
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	const named = "the runner answers at " + theHost + "\n"
	// config sets key to value in the repository's own configuration for the rest of the case.
	config := func(t *testing.T, key, value string) {
		t.Helper()
		fixture.Run(repo, "git", "config", key, value)
		t.Cleanup(func() { fixture.Run(repo, "git", "config", "--unset", key) })
	}
	globalAttributes := filepath.Join(t.TempDir(), "attributes")
	write(t, globalAttributes, "*.af -diff\n")

	for _, tc := range []struct {
		name string
		// path is the file the commit adds and the record the report names.
		path, content string
		// setup writes what the case adds besides the file: attributes, configuration.
		setup func(t *testing.T)
		// env is the environment the guard and the control run under, on top of the fixture's.
		env []string
		// hides is the diff-tree option that engages the case's mechanism in the control: none
		// for a mechanism git applies by default.
		hides []string
	}{
		{name: "a file that holds a NUL byte", path: "nul.bin", content: "a\x00b\n" + named},
		{
			name: "a file a -diff in the commit's .gitattributes covers", path: "deps.lock", content: named,
			setup: func(t *testing.T) { write(t, filepath.Join(repo, ".gitattributes"), "*.lock -diff\n") },
		},
		{
			name: "a file a -diff in .git/info/attributes covers", path: "info.dat", content: named,
			setup: func(t *testing.T) {
				info := filepath.Join(repo, ".git", "info", "attributes")
				write(t, info, "*.dat -diff\n")
				t.Cleanup(func() { write(t, info, "") })
			},
		},
		{
			name: "a file a -diff in core.attributesFile covers", path: "g.af", content: named,
			setup: func(t *testing.T) { config(t, "core.attributesFile", globalAttributes) },
		},
		{
			name: "a file whose diff driver is binary", path: "b.bn", content: named,
			setup: func(t *testing.T) {
				write(t, filepath.Join(repo, ".gitattributes"), "*.bn diff=bn\n")
				config(t, "diff.bn.binary", "true")
			},
		},
		{
			name: "a file whose diff driver has a textconv", path: "conv.tc", content: named,
			setup: func(t *testing.T) {
				write(t, filepath.Join(repo, ".gitattributes"), "*.tc diff=tc\n")
				config(t, "diff.tc.textconv", "true")
			},
			hides: []string{"--textconv"},
		},
		{
			name: "a file whose diff driver has an external command", path: "ext.ex", content: named,
			setup: func(t *testing.T) {
				write(t, filepath.Join(repo, ".gitattributes"), "*.ex diff=ex\n")
				config(t, "diff.ex.command", "true")
			},
			hides: []string{"--ext-diff"},
		},
		{
			name: "a file under an external diff the environment names", path: "env.md", content: named,
			env:   []string{"GIT_EXTERNAL_DIFF=true"},
			hides: []string{"--ext-diff"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := strings.ReplaceAll(tc.name, " ", "-")
			fixture.Run(repo, "git", "checkout", "--quiet", "-B", branch, base)
			if tc.setup != nil {
				tc.setup(t)
			}
			write(t, filepath.Join(repo, tc.path), tc.content)
			fixture.Run(repo, "git", "add", "-A")
			fixture.Run(repo, "git", "commit", "--quiet", "-m", "a message of no interest")
			head := fixture.Run(repo, "git", "rev-parse", "HEAD")
			control := append(append([]string{"git", "diff-tree", "-r", "-p", "--no-commit-id"}, tc.hides...), head)
			if out := fixture.With(tc.env...).Out(repo, control...); strings.Contains(out, theHost) {
				t.Fatalf("%s prints the host, so the case's mechanism hides nothing and the case proves nothing:\n%s", strings.Join(control, " "), out)
			}
			ref := "refs/heads/" + branch
			err := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, tc.env...).Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: ref, LocalSHA: head, RemoteRef: ref, RemoteSHA: noSHA}},
			})
			want := []string{tc.path + ": " + theClass}
			if refused := refusal(t, err); refused.By != byRules || !slices.Equal(refused.Lines, want) {
				t.Errorf("the refusal is by %q with the report %q, want this repository's own rules and %q", refused.By, refused.Lines, want)
			}
		})
	}
}
