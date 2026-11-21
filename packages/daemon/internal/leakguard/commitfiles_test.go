package leakguard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

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
	head := commitFiles(t, fixture, repo, map[string]string{"README.md": "the runner answers at " + theHost + "\n"}, "the first commit")
	// The destination holds nothing: a ref the push creates, in a repository with no branch and
	// no tag that could say otherwise.
	destination := destinationAt(t, fixture, repo)
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	err := guard.Push(bound(t), PushRequest{
		Dir:     repo,
		Remote:  "origin",
		URL:     destination,
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: noSHA}},
	})
	wantRefused(t, err, []string{"README.md: " + theClass})
}

// A file is binary by its own content alone, whatever an attribute says: a file .gitattributes
// marks -diff or binary is read as text, the .gitattributes the pushed commit adds itself
// included, where git's own diff would write "Binary files ... differ" for it and no line of it
// would reach the rules. Every commit's files are read as blobs, which no attribute touches, a
// commit of one parent's and a merge's resolution alike (Guard.addedLines). A file that holds a
// zero byte in its first 8000 bytes adds no line, in a commit of one parent and in a merge's
// resolution alike, and a commit that adds a line to such a file adds none, although the zero byte
// is in no line it adds.
func TestAFileIsBinaryByItsOwnContentAlone(t *testing.T) {
	const named = "the runner answers at " + theHost + "\n"
	refused := []string{"notes.lock: " + theClass}
	for _, tc := range []struct {
		name              string
		held, side, added map[string]string
		// want is the report, and nil where the push passes.
		want []string
	}{
		{
			name:  "a file the commit's .gitattributes marks -diff",
			added: map[string]string{".gitattributes": "notes.lock -diff\n", "notes.lock": named},
			want:  refused,
		},
		{
			name:  "a file the commit's .gitattributes marks binary",
			added: map[string]string{".gitattributes": "notes.lock binary\n", "notes.lock": named},
			want:  refused,
		},
		{
			name:  "a file a merge's resolution adds, its .gitattributes marking it -diff",
			side:  map[string]string{},
			added: map[string]string{".gitattributes": "notes.lock -diff\n", "notes.lock": named},
			want:  refused,
		},
		{
			name:  "a file with a zero byte, in a commit of one parent",
			added: map[string]string{"notes.lock": "\x00\n" + named},
		},
		{
			name:  "a file with a zero byte, in a merge's resolution",
			side:  map[string]string{},
			added: map[string]string{"notes.lock": "\x00\n" + named},
		},
		{
			name:  "a line added to a file whose zero byte the commit does not add",
			held:  map[string]string{"notes.lock": "\x00\n"},
			added: map[string]string{"notes.lock": "\x00\n" + named},
		},
		// A file the destination holds, which .gitattributes marks -diff or binary, is compared
		// with the parent's when the commit changes it, where git's own diff of it would write
		// "Binary files ... differ" and no line: the line the commit adds is read all the same.
		{
			name:  "a line added to a file the destination holds, which .gitattributes marks -diff",
			held:  map[string]string{".gitattributes": "notes.lock -diff\n", "notes.lock": "a line of ordinary prose\n"},
			added: map[string]string{"notes.lock": "a line of ordinary prose\n" + named},
			want:  refused,
		},
		{
			name:  "a line added to a file the destination holds, which .gitattributes marks binary",
			held:  map[string]string{".gitattributes": "notes.lock binary\n", "notes.lock": "a line of ordinary prose\n"},
			added: map[string]string{"notes.lock": "a line of ordinary prose\n" + named},
			want:  refused,
		},
		// git reads a file as binary where a zero byte is in its first 8000 bytes, wherever in
		// them it is (xdiff-interface.c's buffer_is_binary), and as text where the first is past
		// them.
		{
			name:  "a file whose first zero byte is at 300, inside its first 8000 bytes",
			added: map[string]string{"notes.lock": named + strings.Repeat("a", 300-len(named)) + "\x00\n"},
		},
		{
			name:  "a file whose first zero byte is at 8000, past its first 8000 bytes",
			added: map[string]string{"notes.lock": named + strings.Repeat("a", 8000-len(named)) + "\x00\n"},
			want:  refused,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, tc.held, tc.side, tc.added, nil)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
			wantRefused(t, err, tc.want)
		})
	}
}

// blobRecord is the PATH entry of a stand-in git that runs the real git for every call, and keeps
// in the file record the objects each cat-file --batch is asked for, one a line, each kept before
// git reads it, so that the asks of every batch are kept in the order they were made: each commit
// object a push reads (Guard.pushRecords), and what of a commit's files the guard reads. No batch,
// no file.
func blobRecord(t *testing.T) (path, record string) {
	t.Helper()
	bin := t.TempDir()
	record = filepath.Join(t.TempDir(), "blobs")
	writeProgram(t, bin, "git", `git=`+shQuote(fixturegit.CompiledGit(t))+` record=`+shQuote(record)+`
case " $* " in
*" cat-file --batch "*)
  while IFS= read -r id; do printf '%s\n' "$id" >>"$record"; printf '%s\n' "$id"; done | "$git" "$@"
  exit ;;
esac
exec "$git" "$@"
`)
	return pathWith(bin), record
}

// asked are the objects record holds (blobRecord), in the order they were asked for, and none
// where no batch was asked for any.
func asked(t *testing.T, record string) []string {
	t.Helper()
	blobs, err := os.ReadFile(record)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Fields(string(blobs))
}

// A commit of one parent reads its files bounded, as a merge does. A binary file adds no line,
// whatever its size, whether the commit's file is binary or the parent's file it replaces is. A
// text file above the bound refuses the push as a setup failure that names the file and its size,
// whether it is the commit's own or the parent's file the commit's is compared with: a file passed
// over, or read in part, would pass its lines unread. A rename or a mode change that leaves such a
// file as the parent has it adds no line, and nothing of the file is read: cat-file is asked for
// no blob, the commit object alone. A text file at the bound is read, a text file that replaces a
// binary one adds every line it has, and of a commit that changes a binary file and a text file,
// the text file alone adds a line. The bound here is 64 bytes, and the objects the guard asks for
// are kept (blobRecord).
func TestACommitsFileAboveTheBoundRefusesThePush(t *testing.T) {
	big := strings.Repeat("a line of ordinary prose\n", 4)
	named := "the runner answers at " + theHost + "\n"
	line := "at " + theHost
	atBound := line + strings.Repeat(" ", 63-len(line)) + "\n"
	const failed = "LEAK CHECK SETUP ERROR: the push to origin: what the push adds could not be read: "
	for _, tc := range []struct {
		name        string
		held, added map[string]string
		// edit is the change the commit makes to the working tree beyond added, nil for none.
		edit func(t *testing.T, repo string)
		// failure is the setup failure for the commit, "" where there is none.
		failure func(commit string) string
		// want is the report, and nil where the push passes or fails.
		want []string
		// readsNothing is whether the guard asks cat-file for no blob of the commit's, the commit
		// object alone.
		readsNothing bool
	}{
		{
			name:  "a binary file above the bound, which the commit changes beside a text file",
			held:  map[string]string{"big.bin": "\x00" + big, "notes.md": "one\n"},
			added: map[string]string{"big.bin": "\x00" + big + named, "notes.md": "one\ntwo\n"},
		},
		{
			name: "a text file above the bound, renamed whole",
			held: map[string]string{"big.md": big + named},
			edit: func(t *testing.T, repo string) {
				if err := os.Rename(filepath.Join(repo, "big.md"), filepath.Join(repo, "moved.md")); err != nil {
					t.Fatal(err)
				}
			},
			readsNothing: true,
		},
		{
			name: "a text file above the bound whose mode the commit changes",
			held: map[string]string{"big.md": big + named},
			edit: func(t *testing.T, repo string) {
				if err := os.Chmod(filepath.Join(repo, "big.md"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			readsNothing: true,
		},
		{
			name:  "a text file above the bound",
			added: map[string]string{"big.md": big},
			failure: func(commit string) string {
				return failed + "commit " + short(commit) + "'s file big.md is 100 bytes, more than the 64 bytes a commit's file is read up to"
			},
		},
		{
			name:  "the parent's text file above the bound, which the commit's is compared with",
			held:  map[string]string{"big.md": big},
			added: map[string]string{"big.md": "a line of the commit\n"},
			failure: func(commit string) string {
				return failed + "the parent of commit " + short(commit) + "'s file big.md is 100 bytes, more than the 64 bytes a commit's file is read up to"
			},
		},
		{
			name:  "the commit's text file above the bound, which the parent's file it is compared with is not",
			held:  map[string]string{"big.md": "a line of the parent\n"},
			added: map[string]string{"big.md": big},
			failure: func(commit string) string {
				return failed + "commit " + short(commit) + "'s file big.md is 100 bytes, more than the 64 bytes a commit's file is read up to"
			},
		},
		{
			name:  "a text file at the bound",
			added: map[string]string{"edge.md": atBound},
			want:  []string{"edge.md: " + theClass},
		},
		{
			name:  "a binary file and a text file the commit changes",
			held:  map[string]string{"image.bin": "\x00one\n", "notes.md": "one\n"},
			added: map[string]string{"image.bin": "\x00two\n" + named, "notes.md": "one\n" + named},
			want:  []string{"notes.md: " + theClass},
		},
		{
			name:  "a text file in place of a binary one",
			held:  map[string]string{"notes.lock": "\x00one\n"},
			added: map[string]string{"notes.lock": named},
			want:  []string{"notes.lock: " + theClass},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, tc.held, nil, tc.added, tc.edit)
			path, record := blobRecord(t)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, path)
			guard.blobBound = 64
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
			if read := asked(t, record); tc.readsNothing && !slices.Equal(read, []string{head}) {
				t.Errorf("the guard asked cat-file for %q, want the commit alone, %q", read, head)
			}
			if tc.failure != nil {
				wantSetupFailure(t, err, tc.failure(head))
				return
			}
			wantRefused(t, err, tc.want)
		})
	}
}

// A file no line can be taken from is never asked of cat-file, so git neither writes it nor, in a
// partial clone, fetches it (blobBatch): the parent's file of a commit whose own file is binary,
// and a merge parent's file once an earlier parent's file at that path holds every line of the
// merge's. Such a file the repository lacks refuses nothing, since no line of it would be read:
// the commit here is pushed from a repository its parent's file has been removed from, and
// passes. The objects the guard asks for are kept (blobRecord), each commit it reads among them.
func TestAFileNoLineCanBeTakenFromIsNotAskedFor(t *testing.T) {
	named := "the runner answers at " + theHost + "\n"
	for _, tc := range []struct {
		name              string
		held, side, added map[string]string
		// lacks is the file the repository is made to lack before the push, as <rev>:<path> with
		// HEAD the pushed commit, "" for none.
		lacks string
		// asks are the objects the guard asks cat-file for, in order, each as <rev>[:<path>] with
		// HEAD the pushed commit: each commit the push reads, then the blobs of its files.
		asks []string
	}{
		{
			name:  "the parent's file of a commit whose own file is binary, which the repository lacks",
			held:  map[string]string{"notes.lock": "a line of the parent\n"},
			added: map[string]string{"notes.lock": "\x00a line of the parent\n" + named},
			lacks: "HEAD^:notes.lock",
			asks:  []string{"HEAD", "HEAD:notes.lock"},
		},
		{
			name:  "a merge's second parent's file, where its first parent's holds every line of the merge's",
			held:  map[string]string{"notes.md": "one\ntwo\nthree\n"},
			side:  map[string]string{"notes.md": "a line the side branch writes\n"},
			added: map[string]string{"notes.md": "three\ntwo\n"},
			// The main commit and its file, which it adds, then the merge, the merge's file and its
			// first parent's.
			asks: []string{"HEAD^1", "HEAD^1:main.md", "HEAD", "HEAD:notes.md", "HEAD^1:notes.md"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, tc.held, tc.side, tc.added, nil)
			fixture := fixturegit.Isolated(t, t.TempDir())
			blob := func(at string) string {
				t.Helper()
				return fixture.Run(repo, "git", "rev-parse", strings.Replace(at, "HEAD", head, 1))
			}
			if tc.lacks != "" {
				lacked := blob(tc.lacks)
				remove(t, filepath.Join(repo, ".git", "objects", lacked[:2], lacked[2:]))
				if _, err := fixture.Try(repo, "git", "cat-file", "-e", lacked); err == nil {
					t.Fatalf("the repository still has %s, so this case proves nothing", tc.lacks)
				}
			}
			var want []string
			for _, at := range tc.asks {
				want = append(want, blob(at))
			}
			path, record := blobRecord(t)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, path)
			if err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination)); err != nil {
				t.Fatalf("the push = %v, want it passed", err)
			}
			if read := asked(t, record); !slices.Equal(read, want) {
				t.Errorf("the guard asked cat-file for %q, want %q", read, want)
			}
		})
	}
}

// A record of a file names the file, and the lines of a file are read from its blob alike in a
// commit of one parent and in a merge. The identities and the message come first, then the
// records of paths, then the records of lines, each in path order and a file's lines in the
// file's order, and a blank line is no record. Of a commit of one parent that changes a binary
// file beside a rename, the rename alone adds lines, and a submodule adds its path alone: it has
// no blob of this repository's to read a line from.
func TestAFilesRecordsCarryItsPath(t *testing.T) {
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
	tree := func() string {
		t.Helper()
		fixture.Run(repo, "git", "add", "-A")
		return fixture.Run(repo, "git", "write-tree")
	}
	write("README.md", "one\ntwo\nthree\nfour\n")
	base := fixture.Run(repo, "git", "commit-tree", tree(), "-m", "the base")
	write("README.md", "one\nthe second\nthree\nfour\nfive\n")
	write("new.md", "first\n\nthird\n")
	edit := fixture.Run(repo, "git", "commit-tree", tree(), "-p", base, "-m", "an edit")
	if err := os.Remove(filepath.Join(repo, "new.md")); err != nil {
		t.Fatal(err)
	}
	write("README.md", "one\ntwo\nthree\nfour\nfrom the side\n")
	side := fixture.Run(repo, "git", "commit-tree", tree(), "-p", base, "-m", "the side")
	// new.md is as the edit has it, so it is that parent's and none of the merge's own.
	write("README.md", "one\nthe second\nthree\nfour\nfive\nthe resolution's\nfrom the side\n")
	write("new.md", "first\n\nthird\n")
	write("merged.md", "only the merge\n")
	merge := fixture.Run(repo, "git", "commit-tree", tree(), "-p", edit, "-p", side, "-m", "the merge")
	// mixed, of one parent, changes a binary file and renames README.md with a line added.
	for _, name := range []string{"new.md", "merged.md"} {
		if err := os.Remove(filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "one\ntwo\nthree\nfour\n")
	write("image.bin", "\x00one\n")
	binaries := fixture.Run(repo, "git", "commit-tree", tree(), "-p", base, "-m", "a binary file")
	write("image.bin", "\x00two\nthree\n")
	if err := os.Rename(filepath.Join(repo, "README.md"), filepath.Join(repo, "NOTES.md")); err != nil {
		t.Fatal(err)
	}
	write("NOTES.md", "one\ntwo\nthree\nfour\nfive\n")
	mixed := fixture.Run(repo, "git", "commit-tree", tree(), "-p", binaries, "-m", "a rename beside a binary file")
	// submodule, of one parent, adds a submodule at vendor/lib, at the base's commit.
	fixture.Run(repo, "git", "read-tree", base)
	fixture.Run(repo, "git", "update-index", "--add", "--cacheinfo", "160000,"+base+",vendor/lib")
	submodule := fixture.Run(repo, "git", "commit-tree", fixture.Run(repo, "git", "write-tree"), "-p", base, "-m", "a submodule")

	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
	for _, tc := range []struct {
		name   string
		commit string
		want   []Record
	}{
		{
			name:   "a commit of one parent",
			commit: edit,
			want: []Record{
				{Label: "new.md", Text: "new.md"},
				{Label: "README.md", Text: "the second"},
				{Label: "README.md", Text: "five"},
				{Label: "new.md", Text: "first"},
				{Label: "new.md", Text: "third"},
			},
		},
		{
			name:   "a merge",
			commit: merge,
			want: []Record{
				{Label: "merged.md", Text: "merged.md"},
				{Label: "README.md", Text: "the resolution's"},
				{Label: "merged.md", Text: "only the merge"},
			},
		},
		{
			name:   "a commit of one parent that changes a binary file beside a rename",
			commit: mixed,
			want: []Record{
				{Label: "NOTES.md", Text: "NOTES.md"},
				{Label: "NOTES.md", Text: "five"},
			},
		},
		{
			name:   "a commit of one parent that adds a submodule",
			commit: submodule,
			want:   []Record{{Label: "vendor/lib", Text: "vendor/lib"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := bound(t)
			batch, err := guard.readBlobs(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			records, err := guard.commitRecords(ctx, repo, tc.commit, batch)
			if err = batch.close(err); err != nil {
				t.Fatalf("read the commit: %v", err)
			}
			commit := "commit " + short(tc.commit)
			identities := []string{commit + " author", commit + " committer", commit + " message"}
			if len(records) < len(identities) || !slices.Equal(labels(records[:len(identities)]), identities) {
				t.Fatalf("the records are %+v, want the identities and the message first", records)
			}
			if files := records[len(identities):]; !slices.Equal(files, tc.want) {
				t.Errorf("the records of files are %+v, want %+v", files, tc.want)
			}
		})
	}
}

// labels are the labels of records, in their order.
func labels(records []Record) []string {
	var of []string
	for _, record := range records {
		of = append(of, record.Label)
	}
	return of
}

// A commit's added lines are the lines of each file it changes that the file it is compared with
// does not hold: the parent's file at the same path for a file it modifies, and the file it is
// renamed from for one it renames. A line the commit adds that the parent's file already holds
// elsewhere is no line the destination lacks, and adds no record, whatever line of the file it
// lands at; the same line added to a file whose parent's file does not hold it is read. A rename
// that leaves its file as it is reads nothing, and leaves no other rename unread: a pure rename of
// c to a, beside a rename of a/x to q, where a/x is a file in the directory c's new path names,
// still reads the line q adds.
func TestACommitsLinesAreReadFromItsBlobs(t *testing.T) {
	named := "the runner answers at " + theHost + "\n"
	const prose = "one\ntwo\nthree\nfour\nfive\n"
	for _, tc := range []struct {
		name        string
		held, added map[string]string
		// edit is the change the commit makes to the working tree beyond added, nil for none.
		edit func(t *testing.T, repo string)
		// want is the report, and nil where the push passes.
		want []string
	}{
		{
			name:  "a line the parent's file holds elsewhere",
			held:  map[string]string{"notes.md": named + "a line of ordinary prose\n"},
			added: map[string]string{"notes.md": named + "a line of ordinary prose\n" + named},
		},
		{
			name:  "the same line added to a file whose parent's file does not hold it",
			held:  map[string]string{"notes.md": named, "other.md": "a line of ordinary prose\n"},
			added: map[string]string{"other.md": "a line of ordinary prose\n" + named},
			want:  []string{"other.md: " + theClass},
		},
		{
			name: "a pure rename c -> a beside a changed rename a/x -> q",
			held: map[string]string{"c": "a file renamed whole\n", "a/x": prose},
			edit: func(t *testing.T, repo string) {
				at := func(name string) string { return filepath.Join(repo, name) }
				if err := os.Rename(at("a/x"), at("q")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(at("a")); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(at("c"), at("a")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(at("q"), []byte(prose+named), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"q: " + theClass},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, base, head, destination := pushOf(t, tc.held, nil, tc.added, tc.edit)
			guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
			err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
			wantRefused(t, err, tc.want)
		})
	}
}

// movedFiles is how many files the commit TestACommitThatMovesThousandsOfFilesIsReadForItsLine
// moves: past the some 11,000 from which the read that wrote one pathspec per moved path could
// not start git, and above the 11,647 files of cybermonklabs/oh-my-pi.
const movedFiles = 12500

// A commit that moves thousands of files whole is read for the line it adds, and nothing of the
// files it moves is read. A commit's lines are read from its blobs, so what the read costs grows
// with the files it changes, and no git's command line grows with the files it moves: a read that
// wrote each moved path on the command line of its diff could not start git for such a commit
// (fork/exec: file name too long), and refused its push as a setup failure that named neither the
// commit nor the cause. The commit here moves movedFiles files from one directory to another and
// adds a line naming a host to a text file beside them: it is refused for that line, and cat-file
// is asked for the commit and that file's two blobs alone, the commit's and its parent's. The
// trees are written with plumbing, one blob for every moved file.
func TestACommitThatMovesThousandsOfFilesIsReadForItsLine(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	repo := filepath.Join(root, "repo")
	fixture := fixturegit.Isolated(t, home)
	fixture.Run(root, fixturegit.InitWorkTree(repo, "main")...)
	blob := func(name, content string) string {
		t.Helper()
		file := filepath.Join(root, name)
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return fixture.Run(repo, "git", "hash-object", "-w", file)
	}
	moved := blob("moved", "a line of ordinary prose\n")
	notes := blob("notes", "one\n")
	named := blob("named", "one\nthe runner answers at "+theHost+"\n")
	// tree is the tree of movedFiles files under dir, each the one moved blob, and of notes.md as
	// the blob notesAt, written from an index of its own.
	tree := func(dir, notesAt string) string {
		t.Helper()
		var entries strings.Builder
		for i := range movedFiles {
			fmt.Fprintf(&entries, "100644 %s\t%s/an-invented-component-%05d.ts\n", moved, dir, i)
		}
		fmt.Fprintf(&entries, "100644 %s\tnotes.md\n", notesAt)
		index := fixture.With("GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index"))
		index.Fed(repo, entries.String(), "git", "update-index", "--index-info")
		return index.Run(repo, "git", "write-tree")
	}
	base := fixture.Run(repo, "git", "commit-tree", tree("packages/an-invented-package/src", notes), "-m", "the commit the destination holds")
	head := fixture.Run(repo, "git", "commit-tree", tree("packages/an-invented-package/lib", named), "-p", base, "-m", "a move of every file")
	destination := destinationAt(t, fixture, repo, base+":refs/heads/main")
	path, record := blobRecord(t)
	guard := guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public, path)
	err := guard.Push(bound(t), mainPush(repo, base, head, "origin", destination))
	wantRefused(t, err, []string{"notes.md: " + theClass})
	if read := asked(t, record); !slices.Equal(read, []string{head, named, notes}) {
		t.Errorf("the guard asked cat-file for %d objects, %.80q, want the commit and notes.md's two blobs, %q", len(read), read, []string{head, named, notes})
	}
}
