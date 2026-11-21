package leakguard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// recordRepo is a repository of the test's own whose origin, a bare repository, holds its first
// commit, base: what a public destination holds before a push.
type recordRepo struct {
	t       *testing.T
	git     fixturegit.Runner
	dir     string
	base    string
	pending []string
}

func newRecordRepo(t *testing.T) *recordRepo {
	t.Helper()
	root := t.TempDir()
	r := &recordRepo{t: t, git: fixturegit.Isolated(t, t.TempDir()), dir: filepath.Join(root, "repo")}
	remote := filepath.Join(root, "remote.git")
	r.git.Run(root, fixturegit.InitBare(remote, "main")...)
	r.git.Run(root, fixturegit.InitWorkTree(r.dir, "main")...)
	r.write("README.md", "one\n")
	r.base = r.commit("the commit the destination holds")
	r.git.Run(r.dir, "git", "remote", "add", "origin", remote)
	r.git.Run(r.dir, "git", "push", "--quiet", "origin", "main")
	return r
}

// write writes content to name, under the repository's work tree.
func (r *recordRepo) write(name, content string) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// commit commits the whole work tree and answers the commit.
func (r *recordRepo) commit(message string) string {
	r.t.Helper()
	r.git.Run(r.dir, "git", "add", "-A")
	r.git.Run(r.dir, "git", "commit", "--quiet", "--allow-empty", "-m", message)
	return r.rev("HEAD")
}

// rev is what name resolves to.
func (r *recordRepo) rev(name string) string {
	r.t.Helper()
	return r.git.Run(r.dir, "git", "rev-parse", name)
}

// blob is the blob of path in commit.
func (r *recordRepo) blob(commit, path string) string {
	r.t.Helper()
	return r.rev(commit + ":" + path)
}

// note writes text as the record's note on blob, as `git notes` writes one: an inexpert or a
// forging writer, whose fan-out the reader must read as well as its own.
func (r *recordRepo) note(blob, text string) {
	r.t.Helper()
	r.git.Run(r.dir, "git", "notes", "--ref="+NotesRef, "add", "--force", "-m", text, blob)
}

// push is a push of head to origin's main, over base, through guard: its answer, and what it
// said on Options.Out.
func (r *recordRepo) push(guard *Guard, head string) (string, error) {
	r.t.Helper()
	var out bytes.Buffer
	guard.opts.Out = &out
	err := guard.Push(bound(r.t), PushRequest{
		Dir:     r.dir,
		Remote:  "origin",
		URL:     "https://github.com/invented-org/invented-repo.git",
		Updates: []Update{{LocalRef: "refs/heads/main", LocalSHA: head, RemoteRef: "refs/heads/main", RemoteSHA: r.base}},
	})
	return out.String(), err
}

// lines is err's report, nil for a push that passed, failing the test for any other answer.
func (r *recordRepo) lines(err error) []string {
	r.t.Helper()
	if err == nil {
		return nil
	}
	return refusal(r.t, err).Lines
}

// withoutRecord is the report of a push of head with no record at all, which is today's: the ref
// is put aside for the push and put back after it.
func (r *recordRepo) withoutRecord(head string) []string {
	r.t.Helper()
	if tip, err := r.git.Try(r.dir, "git", "rev-parse", "--verify", "--quiet", NotesRef); err == nil {
		r.git.Run(r.dir, "git", "update-ref", "-d", NotesRef)
		defer r.git.Run(r.dir, "git", "update-ref", NotesRef, strings.TrimSpace(tip))
	}
	said, err := r.push(recordGuard(r.t), head)
	if said != "" {
		r.t.Fatalf("a push with no record said %q, want nothing", said)
	}
	return r.lines(err)
}

// recordGuard is a guard whose shared check passes everything and whose destinations are public,
// so that the rule pass is the whole of what a push meets.
func recordGuard(t *testing.T) *Guard {
	t.Helper()
	return guardWith(t, leakguardtest.Write(t, t.TempDir(), passes), Public)
}

// theKey is the record key of guard, which a test forges a note under.
func theKey(t *testing.T, guard *Guard) string {
	t.Helper()
	engine, err := compiled()
	if err != nil {
		t.Fatal(err)
	}
	key, err := guard.recordKey(engine)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// covered is the count line a push with a record says.
func covered(covered, read int) string {
	return fmt.Sprintf("covered %d of the file versions the push adds, and the rules read the other %d\n", covered, read)
}

// The record covers what it read and found clean, and a push then reads none of its lines: the
// guard's batches hold no line of a recorded file. One more commit that adds a file naming a host
// is read and refused, and the record covers the rest of that push.
func TestAPushReadsNoLineOfAFileVersionTheRecordCovers(t *testing.T) {
	repo := newRecordRepo(t)
	for i := range 5 {
		repo.write(fmt.Sprintf("src/file%d.go", i), strings.Repeat(fmt.Sprintf("a line of file %d\n", i), 20))
	}
	head := repo.commit("the internal main")
	writer := recordGuard(t)
	summary, err := writer.WriteRecord(bound(t), repo.dir, []string{head})
	if err != nil {
		t.Fatal(err)
	}
	// README.md and the five files.
	if want := (RecordSummary{Recorded: 6}); summary != want {
		t.Fatalf("the writer answered %+v, want %+v", summary, want)
	}
	if again, err := writer.WriteRecord(bound(t), repo.dir, []string{head}); err != nil || again != (RecordSummary{Covered: 6}) {
		t.Fatalf("a second run answered %+v, %v, want every file version covered", again, err)
	}

	guard := recordGuard(t)
	var read []string
	guard.onBatch = func(batch []Record) {
		for _, record := range batch {
			read = append(read, record.Label)
		}
	}
	said, err := repo.push(guard, head)
	if err != nil {
		t.Fatalf("a push of recorded file versions: %v", err)
	}
	if !strings.HasSuffix(said, covered(5, 0)) {
		t.Errorf("the push said %q, want it to end %q", said, covered(5, 0))
	}
	// The added paths are records of their own, and each of the five is read once, as a path.
	if got := countPrefix(read, "src/"); got != 5 {
		t.Errorf("the rule pass read %d records of the recorded files, want their 5 paths alone: %q", got, read)
	}

	repo.write("src/planted.md", "the runner answers at "+theHost+"\n")
	planted := repo.commit("a commit no record covers")
	said, err = repo.push(recordGuard(t), planted)
	want := []string{"src/planted.md: " + theClass}
	if got := repo.lines(err); !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}
	if !strings.HasSuffix(said, covered(5, 1)) {
		t.Errorf("the push said %q, want it to end %q", said, covered(5, 1))
	}
}

// countPrefix is how many of labels start with prefix.
func countPrefix(labels []string, prefix string) int {
	n := 0
	for _, label := range labels {
		if strings.HasPrefix(label, prefix) {
			n++
		}
	}
	return n
}

// The writer records no file version a rule's content pattern matches a line of, whatever the
// line's place in the file.
func TestTheWriterRecordsNoFileVersionARuleMatches(t *testing.T) {
	repo := newRecordRepo(t)
	repo.write("clean.md", "nothing of ours\n")
	repo.write("named.md", "a first line\r\nthe runner answers at "+theHost+"\r\na last line")
	head := repo.commit("one clean file and one that names a host")
	summary, err := recordGuard(t).WriteRecord(bound(t), repo.dir, []string{head})
	if err != nil {
		t.Fatal(err)
	}
	if want := (RecordSummary{Recorded: 2, NotRecorded: 1}); summary != want {
		t.Fatalf("the writer answered %+v, want %+v", summary, want)
	}
	notes := repo.git.Run(repo.dir, "git", "notes", "--ref="+NotesRef, "list")
	if strings.Contains(notes, repo.blob(head, "named.md")) {
		t.Errorf("the record holds a note on the file version that names a host: %q", notes)
	}
}

// A note under this binary's key is the whole of what the reader trusts: forged on a file version
// that names a host, it makes the push pass, which is what shows the reader reads no line of a
// covered version. Without it the push is refused, and a note under any other key, or under the
// key of other rules, covers nothing.
func TestOnlyANoteUnderTheKeyOfTheseRulesCoversAFileVersion(t *testing.T) {
	repo := newRecordRepo(t)
	repo.write("notes.md", "the runner answers at "+theHost+"\n")
	head := repo.commit("a file that names a host")
	blob := repo.blob(head, "notes.md")
	refused := []string{"notes.md: " + theClass}
	otherRules := recordGuard(t)
	otherRules.keyRules = ruleFile + "\n# one more line\n"
	for _, tc := range []struct {
		name string
		// note is the note on the file's version, "" for none.
		note string
		want []string
	}{
		{name: "no note", want: refused},
		{name: "a note under this binary's key", note: "pass " + theKey(t, recordGuard(t))},
		{name: "a note under another key", note: "pass " + strings.Repeat("ab", 32), want: refused},
		{name: "a note under the key of other rules", note: "pass " + theKey(t, otherRules), want: refused},
		{name: "a note whose line is the key with something after it", note: "pass " + theKey(t, recordGuard(t)) + "0", want: refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo.git.Try(repo.dir, "git", "update-ref", "-d", NotesRef)
			if tc.note != "" {
				repo.note(blob, "pass "+strings.Repeat("cd", 32)+"\n"+tc.note)
			}
			_, err := repo.push(recordGuard(t), head)
			if got := repo.lines(err); !slices.Equal(got, tc.want) {
				t.Fatalf("the report is %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("the other rules' own reader", func(t *testing.T) {
		repo.git.Try(repo.dir, "git", "update-ref", "-d", NotesRef)
		repo.note(blob, "pass "+theKey(t, otherRules))
		if _, err := repo.push(otherRules, head); err != nil {
			t.Fatalf("a reader under the note's own key: %v", err)
		}
	})
}

// A record ref that names no commit covers nothing: the push reads every file version, says why,
// and is refused for what it names. A repository with no record ref says nothing at all.
func TestARecordThatCannotBeReadCoversNothing(t *testing.T) {
	repo := newRecordRepo(t)
	repo.write("notes.md", "the runner answers at "+theHost+"\n")
	head := repo.commit("a file that names a host")
	want := []string{"notes.md: " + theClass}

	said, err := repo.push(recordGuard(t), head)
	if got := repo.lines(err); !slices.Equal(got, want) {
		t.Fatalf("with no record, the report is %q, want %q", got, want)
	}
	if said != "" {
		t.Errorf("a push with no record said %q, want nothing", said)
	}

	// The ref names the file version itself, a blob, and no notes commit.
	repo.git.Run(repo.dir, "git", "update-ref", NotesRef, repo.blob(head, "notes.md"))
	said, err = repo.push(recordGuard(t), head)
	if got := repo.lines(err); !slices.Equal(got, want) {
		t.Fatalf("with a record that names a blob, the report is %q, want %q", got, want)
	}
	if !strings.Contains(said, "the blob record "+NotesRef+" could not be read, so the rules read every file version of the push") ||
		!strings.Contains(said, "which is no commit") {
		t.Errorf("the push said %q, want the line that the record could not be read", said)
	}
}

// A writer whose ref another writer moved after it read it records nothing and says to run again,
// and the other writer's record stands. That holds for a ref that was there and for one another
// writer made first.
func TestAWriteOfAMovedRecordIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		// before is whether the ref is there before the writer reads it.
		before bool
	}{
		{name: "a ref another writer moved", before: true},
		{name: "a ref another writer made", before: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRecordRepo(t)
			repo.write("clean.md", "nothing of ours\n")
			head := repo.commit("a clean file")
			if tc.before {
				repo.note(repo.blob(head, "README.md"), "pass "+strings.Repeat("ef", 32))
			}
			writer := recordGuard(t)
			var other string
			writer.beforeImport = func() {
				repo.note(repo.rev(repo.base+":README.md"), "pass "+strings.Repeat("12", 32)+"\n")
				other = repo.rev(NotesRef)
			}
			_, err := writer.WriteRecord(bound(t), repo.dir, []string{head})
			if !errors.Is(err, ErrRecordMoved) {
				t.Fatalf("the writer answered %v, want ErrRecordMoved", err)
			}
			if tip := repo.rev(NotesRef); tip != other {
				t.Errorf("the record is at %s, want the other writer's %s", tip, other)
			}
			if notes := repo.git.Run(repo.dir, "git", "notes", "--ref="+NotesRef, "list"); strings.Contains(notes, repo.blob(head, "clean.md")) {
				t.Errorf("the refused writer's note is in the record: %q", notes)
			}
		})
	}
}

// A path rule refuses every line of a file at its path, whatever the text, so a recorded file
// version at such a path is read as if nothing covered it: the push is refused, and its report is
// what a push with no record reports, line for line.
func TestAPathRuleStillRefusesAFileTheRecordCovers(t *testing.T) {
	repo := newRecordRepo(t)
	// Built out of two pieces for the reason theHost is (TestTheReportMasksAPathComponent).
	path := "packages/" + "agen" + "t-c/notes.md"
	repo.write(path, "a first line of nothing\na second line of nothing\n")
	head := repo.commit("a file at a path the rules refuse")
	today := repo.withoutRecord(head)
	if len(today) == 0 {
		t.Fatal("the fixture's path is refused by no rule, so it proves nothing")
	}
	summary, err := recordGuard(t).WriteRecord(bound(t), repo.dir, []string{head})
	if err != nil || summary.Recorded != 2 {
		t.Fatalf("the writer answered %+v, %v, want the file's clean version recorded", summary, err)
	}
	said, err := repo.push(recordGuard(t), head)
	if got := repo.lines(err); !slices.Equal(got, today) {
		t.Fatalf("the report is %q, want today's %q", got, today)
	}
	if !strings.HasSuffix(said, covered(0, 1)) {
		t.Errorf("the push said %q, want it to end %q", said, covered(0, 1))
	}
}

// A merge's combined diff reads the lines of each result the merge's own resolution wrote. Where
// the record covers both parents' versions of a file and not the merge's, the merge's lines are
// read and refused as with no record; a covered result is not read, and the report is today's.
func TestAMergeReadsTheResultsTheRecordDoesNotCover(t *testing.T) {
	repo := newRecordRepo(t)
	repo.write("f.md", "one\n")
	repo.write("g.md", "one\n")
	repo.commit("the files both sides change")
	fork := repo.rev("HEAD")
	repo.git.Run(repo.dir, "git", "checkout", "--quiet", "-b", "side")
	repo.write("f.md", "one\nside\n")
	repo.write("g.md", "one\nside\n")
	side := repo.commit("the side's change")
	repo.git.Run(repo.dir, "git", "checkout", "--quiet", "main")
	repo.write("f.md", "main\none\n")
	repo.write("g.md", "main\none\n")
	repo.commit("the main change")
	merge := func(g string) string {
		repo.git.Run(repo.dir, "git", "checkout", "--quiet", "--detach", "main")
		repo.git.Try(repo.dir, "git", "merge", "--quiet", "--no-ff", "--no-commit", "side")
		repo.write("f.md", "main\none\nside\nresolved\n")
		repo.write("g.md", g)
		head := repo.commit("the merge")
		if parents := repo.git.Run(repo.dir, "git", "rev-list", "--parents", "-n", "1", head); len(strings.Fields(parents)) != 3 {
			t.Fatalf("the fixture's merge is %q, want a commit of two parents", parents)
		}
		return head
	}
	clean := merge("main\none\nside\nclean\n")
	if _, err := recordGuard(t).WriteRecord(bound(t), repo.dir, []string{clean, side, fork}); err != nil {
		t.Fatal(err)
	}
	named := merge("main\none\nside\nthe runner answers at " + theHost + "\n")
	today := repo.withoutRecord(named)
	if want := []string{"g.md: " + theClass}; !slices.Equal(today, want) {
		t.Fatalf("with no record, the report is %q, want %q", today, want)
	}
	said, err := repo.push(recordGuard(t), named)
	if got := repo.lines(err); !slices.Equal(got, today) {
		t.Fatalf("the report is %q, want today's %q", got, today)
	}
	// Both files hold one content on each side, so each side is one blob. Covered: the fork's,
	// the side's and the merge's f.md, which is the clean merge's. Read: the main side's, which
	// the writer was not given, and the merge's g.md.
	if !strings.HasSuffix(said, covered(3, 2)) {
		t.Errorf("the push said %q, want it to end %q", said, covered(3, 2))
	}
}

// A commit that renames a file no record covers and changes one it covers reads the renamed
// file's diff as a rename, as a push with no record does: the rename's source is in the pathspec,
// so the lines the destination already holds under the old name are not read again as added.
func TestACoveredReadReadsARenameAsARename(t *testing.T) {
	repo := newRecordRepo(t)
	// The destination already holds this file and its line; only the push's own additions are
	// the push's business.
	repo.write("old.md", "the runner answers at "+theHost+"\n"+strings.Repeat("a line of the old file\n", 10))
	repo.write("kept.md", "one\n")
	repo.base = repo.commit("what the destination holds")
	repo.git.Run(repo.dir, "git", "push", "--quiet", "origin", "main")
	repo.write("kept.md", "one\ntwo\n")
	recorded := repo.commit("a version of kept.md the record covers")
	if _, err := recordGuard(t).WriteRecord(bound(t), repo.dir, []string{recorded}); err != nil {
		t.Fatal(err)
	}
	repo.git.Run(repo.dir, "git", "reset", "--quiet", "--hard", repo.base)
	repo.git.Run(repo.dir, "git", "mv", "old.md", "new name.md")
	repo.write("new name.md", "the runner answers at "+theHost+"\n"+strings.Repeat("a line of the old file\n", 10)+"one more line\n")
	repo.write("kept.md", "one\ntwo\n")
	head := repo.commit("a rename and a covered change")
	if status := repo.git.Run(repo.dir, "git", "diff-tree", "-r", "-M", "--no-commit-id", "--name-status", head); !strings.Contains("\n"+status, "\nR") {
		t.Fatalf("the fixture's commit holds no rename: %q", status)
	}
	if today := repo.withoutRecord(head); today != nil {
		t.Fatalf("with no record, the push is refused: %q", today)
	}
	said, err := repo.push(recordGuard(t), head)
	if err != nil {
		t.Fatalf("the push of a rename and a covered change: %v", err)
	}
	if !strings.HasSuffix(said, covered(1, 1)) {
		t.Errorf("the push said %q, want it to end %q", said, covered(1, 1))
	}
}

// A path that holds a space is named without the tab git ends its "+++" line with, and the record
// covers a file at such a path as it covers any other.
func TestAPathWithASpaceIsReadAsItsOwnPath(t *testing.T) {
	repo := newRecordRepo(t)
	repo.write("a clean file.md", "nothing of ours\n")
	recorded := repo.commit("a clean file at a path with a space")
	if _, err := recordGuard(t).WriteRecord(bound(t), repo.dir, []string{recorded}); err != nil {
		t.Fatal(err)
	}
	repo.write("a named file.md", "the runner answers at "+theHost+"\n")
	head := repo.commit("a file that names a host at a path with a space")
	guard := recordGuard(t)
	var read []string
	guard.onBatch = func(batch []Record) {
		for _, record := range batch {
			if record.Text != record.Label && strings.HasPrefix(record.Label, "a clean file.md") {
				read = append(read, record.Label)
			}
		}
	}
	said, err := repo.push(guard, head)
	want := []string{"a named file.md: " + theClass}
	if got := repo.lines(err); !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want %q", got, want)
	}
	if !strings.HasSuffix(said, covered(1, 1)) {
		t.Errorf("the push said %q, want it to end %q", said, covered(1, 1))
	}
	if len(read) > 0 {
		t.Errorf("the rule pass read a line of the covered file: %q", read)
	}
}

// The rule pass reads a push's records as git writes them and holds one batch of them at a time:
// no batch is past the bound but a batch of one record that is past it alone, a line longer than
// the bound is such a batch, and the report is the one a single batch of every record makes.
func TestTheRulePassHoldsOneBatchAtATime(t *testing.T) {
	const limit = 4 << 10
	repo := newRecordRepo(t)
	var many strings.Builder
	for i := range 2000 {
		if i%500 == 7 {
			fmt.Fprintf(&many, "line %d names %s\n", i, theHost)
			continue
		}
		fmt.Fprintf(&many, "line %d of a file of many lines\n", i)
	}
	repo.write("many.md", many.String())
	long := strings.Repeat("a long line of nothing ", 3*limit/23) + theHost
	if len(long) <= limit {
		t.Fatalf("the long line is %d bytes, not past the bound of %d", len(long), limit)
	}
	repo.write("long.md", "before\n"+long+"\nafter\n")
	head := repo.commit("a commit of many lines and one long one")

	whole := recordGuard(t)
	whole.batchLimit = 1 << 40
	_, err := repo.push(whole, head)
	want := repo.lines(err)
	if len(want) != 5 {
		t.Fatalf("the report of one batch is %q, want the five lines that name a host", want)
	}

	guard := recordGuard(t)
	guard.batchLimit = limit
	batches, alone := 0, false
	guard.onBatch = func(batch []Record) {
		batches++
		size := 0
		for _, record := range batch {
			size += len(record.Text) + recordHeader
			if record.Text == long && len(batch) == 1 {
				alone = true
			}
		}
		if size > limit && len(batch) > 1 {
			t.Errorf("a batch of %d records holds %d bytes, past the bound of %d", len(batch), size, limit)
		}
	}
	_, err = repo.push(guard, head)
	if got := repo.lines(err); !slices.Equal(got, want) {
		t.Fatalf("the report is %q, want the one batch's %q", got, want)
	}
	if !alone {
		t.Error("the line longer than the bound was not a batch of its own")
	}
	if batches < 2000*30/limit {
		t.Errorf("the push was scanned in %d batches, too few for the bound of %d", batches, limit)
	}
}

// The key is a sha256 of the record's tag, the rule file's sha256, the engine's version and Go's,
// and any one of the four changed is another key.
func TestTheRecordKeyChangesWithEachOfItsParts(t *testing.T) {
	base := recordKeyOf("rules", "v1", "go1")
	if len(base) != 64 || strings.ToLower(base) != base {
		t.Fatalf("the key %q is no lower-case hex sha256", base)
	}
	for _, other := range []string{recordKeyOf("rules ", "v1", "go1"), recordKeyOf("rules", "v2", "go1"), recordKeyOf("rules", "v1", "go2")} {
		if other == base {
			t.Errorf("a changed part left the key %q", base)
		}
	}
	if version := engineVersion(); version == "" {
		t.Error("the test binary's build names no version of the detection engine, so it has no key")
	}
	engine, err := compiled()
	if err != nil {
		t.Fatal(err)
	}
	if engine.detector.MaxDecodeDepth != 0 {
		t.Fatalf("the rules decode to depth %d; the record checks no decoded form", engine.detector.MaxDecodeDepth)
	}
}
