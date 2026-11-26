package survivaltest

// reference.go reads the directory ../testdata/reference/prepare.sh writes (its README.md lists
// it): the first-parent chain of cybermonklabs/legion the reference replays, each commit's patch
// as git wrote it, the pull requests that chain merged and the issue each was attributed, and the
// code groups the reference counts by. The survival package's reference tests (build tag
// survivalref) and the code-survival run's live test read it, from the directory
// SURVIVAL_REF_DIR names. It reads the files one at a time and holds no patch it has handed on.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ReferenceDirEnv is the variable that names the directory prepare.sh wrote.
const ReferenceDirEnv = "SURVIVAL_REF_DIR"

// ReferenceRepo is the repository the reference replays: the repository of every pull request of
// lg/pulls.json, and the one an issue's repository must be for a pull request to be attributed it.
const ReferenceRepo = "cybermonklabs/legion"

// referenceSteps is how many steps prepare.sh runs, each of which records its line in steps/.
const referenceSteps = 5

// Reference is the directory prepare.sh wrote.
type Reference struct {
	Dir string
}

// OpenReference is the directory dir, which a reference test reads from SURVIVAL_REF_DIR in its
// own test file, since Dispatch's non-test files read no environment outside the settings table
// (textcheck's TestNoReaderBypassesTheSettingsTable). It fails t, naming the variable, where dir
// is empty or names a directory in which a step of prepare.sh has not finished: a reference test
// never passes over a directory it cannot read.
func OpenReference(t testing.TB, dir string) Reference {
	t.Helper()
	if dir == "" {
		t.Fatalf("%s is unset: set it to the directory testdata/reference/prepare.sh <dir> wrote (testdata/reference/README.md)", ReferenceDirEnv)
	}
	ref := Reference{Dir: dir}
	for step := 1; step <= referenceSteps; step++ {
		if _, err := os.Stat(ref.Path("steps", strconv.Itoa(step)+".txt")); err != nil {
			t.Fatalf("%s=%s holds no finished step %d of prepare.sh: %v", ReferenceDirEnv, dir, step, err)
		}
	}
	return ref
}

// Path is the file of the directory that the parts name, joined.
func (r Reference) Path(parts ...string) string {
	return filepath.Join(append([]string{r.Dir}, parts...)...)
}

// PatchPath is the file holding the patch of the chain's commit sha: its part of
// `git log --reverse --first-parent -p -U0 -M --format=%x01%H`, gzipped, without its \x01 line.
func (r Reference) PatchPath(sha string) string {
	return r.Path("lg", "git", sha+".diff.gz")
}

// ChainCommit is one commit of lg/commits.txt: its SHA, its committer time (in UTC), its parents
// (the first parent first) and its subject.
type ChainCommit struct {
	SHA         string
	CommittedAt time.Time
	Parents     []string
	Subject     string
}

// Chain is lg/commits.txt, the first-parent chain the reference replays, oldest first: each line
// git's `%H %cI %P %s`.
func (r Reference) Chain(t testing.TB) []ChainCommit {
	t.Helper()
	var chain []ChainCommit
	r.eachLine(t, r.Path("lg", "commits.txt"), func(line string) {
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 2 {
			t.Fatalf("survivaltest: lg/commits.txt line %q holds no SHA and time", line)
		}
		at, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			t.Fatalf("survivaltest: lg/commits.txt line %q: %v", line, err)
		}
		commit := ChainCommit{SHA: fields[0], CommittedAt: at.UTC()}
		rest := ""
		if len(fields) == 3 {
			rest = fields[2]
		}
		// %P writes the parents before the subject, one SHA each, separated by a space.
		for {
			parent, after, _ := strings.Cut(rest, " ")
			if !isSHA(parent) {
				break
			}
			commit.Parents = append(commit.Parents, parent)
			rest = after
		}
		commit.Subject = rest
		chain = append(chain, commit)
	})
	return chain
}

// Pull is one pull request of lg/pulls.json: its number, the commit it merged as (GitHub's
// merge_commit_sha, which GitHub answers for a pull request closed unmerged too), its head and
// base branches, the login that opened it, its title and body, and when it merged (zero for one
// closed unmerged).
type Pull struct {
	Number   int
	MergeSHA string
	HeadRef  string
	BaseRef  string
	Author   string
	Title    string
	Body     string
	MergedAt time.Time
}

// Merged says whether the pull request merged.
func (p Pull) Merged() bool { return !p.MergedAt.IsZero() }

// Pulls is every pull request of lg/pulls.json, in its order (by number), read one at a time.
func (r Reference) Pulls(t testing.TB) []Pull {
	t.Helper()
	file, err := os.Open(r.Path("lg", "pulls.json"))
	if err != nil {
		t.Fatalf("survivaltest: open lg/pulls.json: %v", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(file))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		t.Fatalf("survivaltest: lg/pulls.json starts with %v (%v), not a list", token, err)
	}
	var pulls []Pull
	for decoder.More() {
		var read struct {
			Number   int     `json:"number"`
			MergeSHA *string `json:"merge_commit_sha"`
			Head     struct {
				Ref string `json:"ref"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
			Title    string     `json:"title"`
			Body     *string    `json:"body"`
			MergedAt *time.Time `json:"merged_at"`
		}
		if err := decoder.Decode(&read); err != nil {
			t.Fatalf("survivaltest: read pull request %d of lg/pulls.json: %v", len(pulls)+1, err)
		}
		pull := Pull{Number: read.Number, HeadRef: read.Head.Ref, BaseRef: read.Base.Ref, Author: read.User.Login, Title: read.Title}
		if read.MergeSHA != nil {
			pull.MergeSHA = *read.MergeSHA
		}
		if read.Body != nil {
			pull.Body = *read.Body
		}
		if read.MergedAt != nil {
			pull.MergedAt = read.MergedAt.UTC()
		}
		pulls = append(pulls, pull)
	}
	return pulls
}

// PullIssue is one line of lg/pr_issue.tsv: a merged pull request's merge commit, its number, and
// the issue attribution's sources 3 and 4 give it, held to ReferenceRepo ("" for none).
type PullIssue struct {
	MergeSHA string
	Number   int
	Issue    string
}

// PullIssues is lg/pr_issue.tsv, one line for each merged pull request of lg/pulls.json.
func (r Reference) PullIssues(t testing.TB) []PullIssue {
	t.Helper()
	var issues []PullIssue
	r.eachLine(t, r.Path("lg", "pr_issue.tsv"), func(line string) {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			t.Fatalf("survivaltest: lg/pr_issue.tsv line %q holds %d fields, not 3", line, len(fields))
		}
		number, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("survivaltest: lg/pr_issue.tsv line %q: %v", line, err)
		}
		issues = append(issues, PullIssue{MergeSHA: fields[0], Number: number, Issue: fields[2]})
	})
	return issues
}

// Group is one code group, as a project's delivery settings name it and survival.Group holds it:
// its name, the head-branch prefixes and the author logins of its changes, and none of either for
// the group that takes every change no group before it took. This package imports no package of
// the module, so its callers convert.
type Group struct {
	Name           string
	BranchPrefixes []string
	AuthorLogins   []string
}

// ReferenceGroups is the code groups the reference counts by, in their order: the cohort of
// scripts/02_cohort.py and of lg/merge_keys.tsv's group column, which INFRA's delivery settings
// name. Legion is a change whose head branch starts legion/ or that one of Legion's two implement
// Apps opened; Other is every change after it. The answer is the caller's to change.
func ReferenceGroups() []Group {
	return []Group{
		{Name: "Legion", BranchPrefixes: []string{"legion/"},
			AuthorLogins: []string{"legion-implementer[bot]", "trajectory-s-legion-implementer[bot]"}},
		{Name: "Other"},
	}
}

// PatchReader reads gzipped patches one at a time into one buffer it reuses, so it holds the
// text of one patch, the last it read. The zero value is ready to use; it is not safe for
// concurrent use.
type PatchReader struct {
	gzip    *gzip.Reader
	file    *bufio.Reader
	text    bytes.Buffer
	largest int
	held    int
	total   int64
	reads   int
}

// Read is the text of the gzipped file at path, which stays the reader's until its next Read:
// the caller hands on nothing it keeps (survival.ParseDiff's answer shares no memory with it).
func (p *PatchReader) Read(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("survivaltest: open %s: %w", path, err)
	}
	defer file.Close()
	if p.file == nil {
		p.file = bufio.NewReaderSize(file, 1<<16)
	} else {
		p.file.Reset(file)
	}
	if p.gzip == nil {
		if p.gzip, err = gzip.NewReader(p.file); err != nil {
			return nil, fmt.Errorf("survivaltest: read %s: %w", path, err)
		}
	} else if err := p.gzip.Reset(p.file); err != nil {
		return nil, fmt.Errorf("survivaltest: read %s: %w", path, err)
	}
	p.text.Reset()
	if _, err := p.text.ReadFrom(p.gzip); err != nil {
		return nil, fmt.Errorf("survivaltest: read %s: %w", path, err)
	}
	if err := p.gzip.Close(); err != nil {
		return nil, fmt.Errorf("survivaltest: read %s: %w", path, err)
	}
	text := p.text.Bytes()
	p.largest = max(p.largest, len(text))
	p.held = max(p.held, p.text.Cap())
	p.total += int64(len(text))
	p.reads++
	return text, nil
}

// Largest is the longest text the reader has read, in bytes.
func (p *PatchReader) Largest() int { return p.largest }

// PeakHeld is the most patch bytes the reader has held at once: the capacity of its one buffer,
// which holds the text of the patch it read last and grows to the longest.
func (p *PatchReader) PeakHeld() int { return p.held }

// Total is the bytes of text the reader has read, over Reads files.
func (p *PatchReader) Total() int64 { return p.total }

// Reads is how many files the reader has read.
func (p *PatchReader) Reads() int { return p.reads }

// eachLine calls read with each line of the file at path, without its newline.
func (r Reference) eachLine(t testing.TB, path string, read func(line string)) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("survivaltest: open %s: %v", path, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for scanner.Scan() {
		read(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("survivaltest: read %s: %v", path, err)
	}
}

// isSHA says whether s is a full SHA-1 in lower-case hex.
func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range []byte(s) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
