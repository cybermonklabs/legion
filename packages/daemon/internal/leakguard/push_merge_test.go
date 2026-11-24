package leakguard

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// A merge is read through its combined diff, which prints "Binary files differ" for a file that
// holds a NUL byte, or that a -diff attribute or a binary diff driver covers, whatever --text says.
// Each case's merge resolves one file to a line naming a host, every parent already on the
// destination so the merge is the whole of what the push adds, and the rules must refuse it: a
// file holding a NUL byte, and one a -diff covers; the same file under a path holding a tab and a
// byte that is not UTF-8, which git quotes, and its control with a UTF-8 letter; a -diff file
// pushed under an inherited GIT_LITERAL_PATHSPECS=1; a binary file no parent holds; and an
// octopus. Each case's control shows the combined diff prints no line naming the host, and the
// text file, the control of the merge read itself, shows one.
//
// The last case is a merge that turns a submodule's entry of its first parent into a file whose
// blob is the id that entry named. The first parent's diff showed only "Subproject commit <id>",
// so nothing read the blob, and the merge keeps the "same" id: it is read whole all the same.
func TestAMergesBinaryFilesAreReadAsText(t *testing.T) {
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
	write(t, filepath.Join(repo, "README.md"), "one\n")
	fixture.Run(repo, "git", "add", "-A")
	fixture.Run(repo, "git", "commit", "--quiet", "-m", "the commit the destination holds")
	base := fixture.Run(repo, "git", "rev-parse", "HEAD")
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	fixture.Run(repo, "git", "push", "--quiet", "origin", "main")
	const named = "the runner answers at " + theHost + "\n"

	for n, tc := range []struct {
		name string
		// path is the file the merge resolves and the record the report names; lead is the
		// first line of every version of it, a NUL byte where the case is a binary file.
		path, lead string
		// hidden is whether the combined diff hides the file: false for the text control.
		hidden bool
		// fresh is whether the file is new in the merge: no parent holds it.
		fresh bool
		// sides is how many parents the merge has beside the first: 2 for an octopus.
		sides int
		// env is the environment the guard runs under, on top of the fixture's.
		env []string
	}{
		{name: "a merge that resolves a file holding a NUL byte", path: "m.bin", lead: "base\x00\n", hidden: true, sides: 1},
		{name: "a merge that resolves a file a -diff covers", path: "m.lock", lead: "base\n", hidden: true, sides: 1},
		{name: "a merge that resolves a text file", path: "m.txt", lead: "base\n", sides: 1},
		{name: "a merge that resolves a binary file whose path holds a tab and a byte that is not UTF-8", path: "q\t\xff.bin", lead: "base\x00\n", hidden: true, sides: 1},
		{name: "a merge that resolves a binary file whose path holds a tab and a UTF-8 letter", path: "q\té.bin", lead: "base\x00\n", hidden: true, sides: 1},
		{
			name: "a merge that resolves a file a -diff covers, pushed with literal pathspecs", path: "m.lock", lead: "base\n", hidden: true, sides: 1,
			env: []string{"GIT_LITERAL_PATHSPECS=1"},
		},
		{name: "a merge that adds a binary file no parent holds", path: "new.bin", lead: "a\x00b\n", hidden: true, fresh: true, sides: 1},
		{name: "an octopus merge that resolves a file holding a NUL byte", path: "m.bin", lead: "base\x00\n", hidden: true, sides: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := fmt.Sprintf("merge-%d", n)
			ours := branch + "-ours"
			// version commits a version of the case's file, or for a file new in the merge a file
			// of the parent's own, on a branch from fork.
			version := func(name, fork, text string) string {
				t.Helper()
				fixture.Run(repo, "git", "checkout", "--quiet", "-B", name, fork)
				if tc.fresh {
					write(t, filepath.Join(repo, name+".txt"), text)
				} else {
					write(t, filepath.Join(repo, tc.path), tc.lead+text)
				}
				fixture.Run(repo, "git", "add", "-A")
				fixture.Run(repo, "git", "commit", "--quiet", "-m", name)
				return fixture.Run(repo, "git", "rev-parse", "HEAD")
			}
			fixture.Run(repo, "git", "checkout", "--quiet", "-B", branch, base)
			// The -diff cases' attribute, committed with the file every side starts from.
			write(t, filepath.Join(repo, ".gitattributes"), "*.lock -diff\n")
			if !tc.fresh {
				write(t, filepath.Join(repo, tc.path), tc.lead)
			}
			fixture.Run(repo, "git", "add", "-A")
			fixture.Run(repo, "git", "commit", "--quiet", "-m", "the file every side starts from")
			fork := fixture.Run(repo, "git", "rev-parse", "HEAD")
			var sides, names []string
			for i := range tc.sides {
				name := fmt.Sprintf("%s-side%d", branch, i)
				names = append(names, name)
				sides = append(sides, version(name, fork, fmt.Sprintf("a line of side %d\n", i)))
			}
			tip := version(ours, fork, "a line of ours\n")
			// The destination holds every parent, on remote-tracking refs of this repository.
			fixture.Run(repo, append([]string{"git", "push", "--quiet", "origin", ours}, names...)...)
			// The merge's tree is ours with the file resolved to a line of the merge's own, or added;
			// commit-tree makes it a merge of every parent, a conflicting octopus included.
			write(t, filepath.Join(repo, tc.path), tc.lead+named)
			fixture.Run(repo, "git", "add", "-A")
			argv := []string{"git", "commit-tree", fixture.Run(repo, "git", "write-tree"), "-m", "the merge", "-p", tip}
			for _, side := range sides {
				argv = append(argv, "-p", side)
			}
			merge := fixture.Run(repo, argv...)
			fixture.Run(repo, "git", "reset", "--quiet", "--hard", merge)
			control := []string{"git", "diff-tree", "-r", "-c", "-p", "--text", "--no-commit-id", merge}
			out := fixture.Out(repo, control...)
			if prints := strings.Contains(out, theHost); prints == tc.hidden {
				t.Fatalf("%s prints the host: %t, want %t, so the case is not the one it names:\n%s", strings.Join(control, " "), prints, !tc.hidden, out)
			}
			ref := "refs/heads/" + ours
			err := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, tc.env...).Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: ref, LocalSHA: merge, RemoteRef: ref, RemoteSHA: tip}},
			})
			want := []string{tc.path + ": " + theClass}
			if refused := refusal(t, err); refused.By != byRules || !slices.Equal(refused.Lines, want) {
				t.Errorf("the refusal is by %q with the report %q, want this repository's own rules and %q", refused.By, refused.Lines, want)
			}
		})
	}

	t.Run("a merge that makes a file of a submodule's entry whose id is a blob naming a host", func(t *testing.T) {
		object := func(t *testing.T, content string) string {
			t.Helper()
			path := filepath.Join(t.TempDir(), "content")
			write(t, path, content)
			return strings.TrimSpace(fixture.Out(repo, "git", "hash-object", "-w", "--no-filters", path))
		}
		// tree is a tree of the one entry sub, of mode and id.
		tree := func(t *testing.T, mode, id string) string {
			t.Helper()
			index := fixture.With("GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index"))
			index.Run(repo, "git", "update-index", "--add", "--cacheinfo", mode+","+id+",sub")
			return strings.TrimSpace(index.Out(repo, "git", "write-tree"))
		}
		hidden := object(t, "base\x00\n"+named)
		ours := fixture.Run(repo, "git", "commit-tree", tree(t, "160000", hidden), "-m", "ours: sub a submodule's entry", "-p", base)
		side := fixture.Run(repo, "git", "commit-tree", tree(t, "100644", object(t, "base\x00\nside\n")), "-m", "the side: sub a file", "-p", base)
		merge := fixture.Run(repo, "git", "commit-tree", tree(t, "100644", hidden), "-m", "the merge", "-p", ours, "-p", side)
		fixture.Run(repo, "git", "branch", "--force", "gitlink-ours", ours)
		fixture.Run(repo, "git", "branch", "--force", "gitlink-side", side)
		fixture.Run(repo, "git", "push", "--quiet", "origin", "gitlink-ours", "gitlink-side")
		control := []string{"git", "diff-tree", "-r", "-c", "-p", "--text", "--no-commit-id", merge}
		if out := fixture.Out(repo, control...); strings.Contains(out, theHost) || !strings.Contains(out, "Binary files differ") {
			t.Fatalf("%s prints the host or no binary file, so the case is not the one it names:\n%s", strings.Join(control, " "), out)
		}
		err := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public).Push(bound(t), PushRequest{
			Dir:     repo,
			Remote:  "origin",
			URL:     "https://github.com/invented-org/invented-repo.git",
			Updates: []Update{{LocalRef: "refs/heads/gitlink-ours", LocalSHA: merge, RemoteRef: "refs/heads/gitlink-ours", RemoteSHA: ours}},
		})
		want := []string{"sub: " + theClass}
		if refused := refusal(t, err); refused.By != byRules || !slices.Equal(refused.Lines, want) {
			t.Errorf("the refusal is by %q with the report %q, want this repository's own rules and %q", refused.By, refused.Lines, want)
		}
	})
}

// A merge whose combined diff prints a file as binary, and that publishes no text its first
// parent does not hold, passes: the combined diff lists a file whose mode differs from every
// parent's even where its blob is the first parent's, and lists a submodule's entry under a -diff
// attribute as binary too. The cases: one side edits a binary file and the other only makes it
// executable, and the merge keeps the edit under the new mode; ours' binary file made a symlink of
// the same blob; and a -diff submodule moved to a commit of another repository. Each case's
// control shows the combined diff prints the file as binary, so the case is the one it names, and
// nothing in any of them names anything. The refusal control is the first case with the file
// resolved to a line naming a host, which the rules refuse: a mode change does not stop a changed
// blob from being read.
func TestAMergeThatPublishesNoNewTextOfABinaryFilePasses(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	remote, repo := filepath.Join(root, "remote.git"), filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitBare(remote, "main")...)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	fixture.Run(repo, "git", "remote", "add", "origin", remote)
	// The submodule case's attribute, which diff-tree reads from the working tree.
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("sub -diff\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "content")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(fixture.Out(repo, "git", "hash-object", "-w", "--no-filters", path))
	}
	// commit is a commit whose tree holds one entry, mode and id at path, with parents.
	commit := func(t *testing.T, path string, entry [2]string, parents ...string) string {
		t.Helper()
		index := fixture.With("GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index"))
		index.Run(repo, "git", "update-index", "--add", "--cacheinfo", entry[0]+","+entry[1]+","+path)
		argv := []string{"git", "commit-tree", strings.TrimSpace(index.Out(repo, "git", "write-tree")), "-m", "a commit"}
		for _, parent := range parents {
			argv = append(argv, "-p", parent)
		}
		return fixture.Run(repo, argv...)
	}
	b0, b1, b2 := blob(t, "base\x00\n"), blob(t, "base\x00\nours\n"), blob(t, "base\x00\nside\n")
	named := blob(t, "base\x00\nthe runner answers at "+theHost+"\n")
	link := func(n int) string { return strings.Repeat(fmt.Sprint(n), 40) }
	for n, tc := range []struct {
		name string
		path string
		// fork, ours, side and merge are the file's mode and id in each commit: ours is the
		// merge's first parent and side its second.
		fork, ours, side, merge [2]string
		// want is the report, and nil where the push passes.
		want []string
	}{
		{
			name: "a merge that keeps ours' binary file and takes the other side's chmod", path: "f.bin",
			fork: [2]string{"100644", b0}, ours: [2]string{"100644", b1}, side: [2]string{"100755", b0}, merge: [2]string{"100755", b1},
		},
		{
			name: "a merge that makes ours' binary file a symlink of the same blob", path: "s.bin",
			fork: [2]string{"100644", b0}, ours: [2]string{"100644", b1}, side: [2]string{"100644", b2}, merge: [2]string{"120000", b1},
		},
		{
			name: "a merge that moves a -diff submodule", path: "sub",
			fork: [2]string{"160000", link(1)}, ours: [2]string{"160000", link(2)}, side: [2]string{"160000", link(3)}, merge: [2]string{"160000", link(4)},
		},
		{
			name: "control: a merge that takes the chmod and resolves the binary file to a line naming a host", path: "f.bin",
			fork: [2]string{"100644", b0}, ours: [2]string{"100644", b1}, side: [2]string{"100755", b0}, merge: [2]string{"100755", named},
			want: []string{"f.bin: " + theClass},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fork := commit(t, tc.path, tc.fork)
			ours := commit(t, tc.path, tc.ours, fork)
			side := commit(t, tc.path, tc.side, fork)
			merge := commit(t, tc.path, tc.merge, ours, side)
			branch := fmt.Sprintf("kept-%d", n)
			fixture.Run(repo, "git", "branch", "--force", branch, ours)
			fixture.Run(repo, "git", "branch", "--force", branch+"-side", side)
			// The destination holds both parents, on remote-tracking refs of this repository.
			fixture.Run(repo, "git", "push", "--quiet", "origin", branch, branch+"-side")
			control := []string{"git", "diff-tree", "-r", "-c", "-p", "--text", "--no-commit-id", merge}
			if out := fixture.Out(repo, control...); !strings.Contains(out, "Binary files differ") {
				t.Fatalf("%s prints no file as binary, so the case is not the one it names:\n%s", strings.Join(control, " "), out)
			}
			ref := "refs/heads/" + branch
			err := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public).Push(bound(t), PushRequest{
				Dir:     repo,
				Remote:  "origin",
				URL:     "https://github.com/invented-org/invented-repo.git",
				Updates: []Update{{LocalRef: ref, LocalSHA: merge, RemoteRef: ref, RemoteSHA: ours}},
			})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("a merge that publishes no new text: %v", err)
				}
				return
			}
			if refused := refusal(t, err); refused.By != byRules || !slices.Equal(refused.Lines, tc.want) {
				t.Errorf("the refusal is by %q with the report %q, want this repository's own rules and %q", refused.By, refused.Lines, tc.want)
			}
		})
	}
}

// Every way a diff can still leave a file unread refuses the push rather than passing it. The
// stand-in git answers one read as git would if it misbehaved and runs the real git for every
// other command: a commit's diff that prints a file as binary despite --text; a merge's combined
// diff that prints a file as binary under no index line, so no blob to read it from, or with no
// raw line for it, so no mode to read it by; a read of a merge's two blobs that prints no diff at
// all; and one that prints them as binary. Each push's control, the same push with the real git,
// passes. The refusal names the commit and not the file, whose path is the push's own text, and
// here is the host.
func TestAFileADiffLeavesUnreadRefusesThePush(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	fixture.Run(repo, "git", "commit", "--allow-empty", "--quiet", "-m", "a commit of nothing in particular")
	head := fixture.Run(repo, "git", "rev-parse", "HEAD")
	fixture.Run(repo, "git", "checkout", "--quiet", "-b", "side")
	fixture.Run(repo, "git", "commit", "--allow-empty", "--quiet", "-m", "the side's commit of nothing")
	side := fixture.Run(repo, "git", "rev-parse", "HEAD")
	merge := fixture.Run(repo, "git", "commit-tree", head+"^{tree}", "-m", "a merge of nothing", "-p", head, "-p", side)
	ids := strings.Repeat("1", 40) + "," + strings.Repeat("2", 40) + ".." + strings.Repeat("3", 40)
	raw := "::100644 100644 100644 " + strings.Repeat("1", 40) + " " + strings.Repeat("2", 40) + " " + strings.Repeat("3", 40) + " MM\t" + theHost
	for _, tc := range []struct {
		name string
		// pushed is the commit the push moves the ref to.
		pushed string
		// match is the case pattern of the read the stand-in answers, and answer what it prints.
		match, answer string
		// says is the reason the setup failure gives.
		says string
	}{
		{
			name: "a commit's diff that prints a file as binary", pushed: head,
			match:  `*" diff-tree "*" --unified=0 "*`,
			answer: "diff --git a/" + theHost + " b/" + theHost + "\\nBinary files a/" + theHost + " and b/" + theHost + " differ\\n",
			says:   "git diff-tree printed a file of commit " + short(head) + " as binary despite --text",
		},
		{
			name: "a merge's combined diff that prints a file as binary under no index line", pushed: merge,
			match:  `*" diff-tree "*" -c --unified=0 "*`,
			answer: "diff --combined " + theHost + "\\nBinary files differ\\n",
			says:   "git diff-tree printed a file of commit " + short(merge) + " as binary despite --text",
		},
		{
			name: "a merge's combined diff that prints a file as binary with no raw line for it", pushed: merge,
			match:  `*" diff-tree "*" -c --unified=0 "*`,
			answer: "diff --combined " + theHost + "\\nindex " + ids + "\\nBinary files differ\\n",
			says:   "git diff-tree printed a file of commit " + short(merge) + " as binary despite --text",
		},
		{
			name: "a read of a merge's two blobs that prints no diff", pushed: merge,
			match:  `*" diff-tree "*" -c --unified=0 "*) printf '` + raw + `\n\ndiff --combined ` + theHost + `\nindex ` + ids + `\nBinary files differ\n'; exit 0 ;;` + "\n" + `*" diff --text "*`,
			answer: "",
			says:   "git diff read no diff of a file the combined diff of merge " + short(merge) + " printed as binary",
		},
		{
			name: "a read of a merge's two blobs that prints them as binary", pushed: merge,
			match:  `*" diff-tree "*" -c --unified=0 "*) printf '` + raw + `\n\ndiff --combined ` + theHost + `\nindex ` + ids + `\nBinary files differ\n'; exit 0 ;;` + "\n" + `*" diff --text "*`,
			answer: "diff --git a/1 b/3\\nBinary files a/1 and b/3 differ\\n",
			says:   "git diff printed a blob of merge " + short(merge) + " as binary despite --text",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standIn := t.TempDir()
			script := "#!/bin/sh\ncase \" $* \" in\n" + tc.match + ") printf '" + tc.answer + "'; exit 0 ;;\nesac\nexec '" + fixturegit.CompiledGit(t) + "' \"$@\"\n"
			if err := os.WriteFile(filepath.Join(standIn, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			push := func(t *testing.T, extra ...string) error {
				t.Helper()
				return guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, extra...).Push(bound(t), PushRequest{
					Dir:     repo,
					Remote:  "origin",
					URL:     "https://github.com/invented-org/invented-repo.git",
					Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: tc.pushed, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
				})
			}
			if err := push(t); err != nil {
				t.Fatalf("the control, the push with the real git: %v", err)
			}
			failed := setupFailure(t, push(t, "PATH="+standIn+string(os.PathListSeparator)+os.Getenv("PATH")))
			says(t, failed, "the push's commits", tc.says)
			if strings.Contains(failed.Error(), theHost) {
				t.Fatalf("the setup failure names the file's path: %v", failed)
			}
		})
	}
}
